#!/usr/bin/env python3
"""Real-client acceptance for T010: text, tool round-trips, streaming, cancel
and fallback, driven by the clients real agents actually use.

A mock upstream proves the gateway's own logic; it says nothing about whether
Claude Code, Codex or the Gemini CLI can talk to it. This script covers both
sides, in two modes:

  self-check (default, no --client)
      Starts a real gateway process against mock upstreams and verifies, on
      the wire, every gateway-side capability the client checklist in
      docs/note/trial-acceptance.md requires: the base URL answers, the
      chat / responses / messages (and gemini) endpoints each serve their own
      format, SSE streams terminate, a client that vanishes mid-stream is
      recorded as 499, key auth accepts what each CLI sends and refuses
      everything else, and a session header pins a conversation. It is the
      precondition for the real run: until it is green, the real run measures
      the wrong thing.

  real (--client claude-code|codex|gemini)
      Attaches to an already running gateway — your channels, your upstream
      credentials, your accounts — reads the seam values (address, model list;
      D007 in docs/design/decisions.md), prints the exact values each CLI needs,
      issues one command per scenario and collects the gateway's usage rows as
      evidence. The client key is passed in with --client-key (the console
      shows a key in full exactly once at creation); nothing is printed in
      full.
      It never writes global client configuration: pointing a tool at the
      gateway is MMA's job (D007), this script only prints the values. A CLI
      that is not installed is skipped politely. Nothing passes on a guess —
      machine-checkable criteria are evaluated automatically and the rest
      stays in the manual_confirm slot for the human.

      --demo-gateway rehearses the whole real-mode pipeline against a private
      gateway with mock upstreams and test/fake_agent_cli.py as the client.
      Its evidence is stamped dry-run and is NOT acceptance evidence.

Windows notes (same as test/acceptance_routing.py, see docs/note/good-finds.md):
  * Run with PYTHONUTF8=1: captured CLI output and gateway logs are UTF-8 and
    the console code page would mangle them.
  * The binary in the repository root is NOT executed directly — on this
    machine the sandbox stalls executables launched from the workspace, which
    looks exactly like a product hang; it is built into a temp dir first.
  * -show-key prints the key and exits. The server is a second invocation.
  * Every /api/ route needs an admin session, so a 401 from /api/health is a
    perfectly good sign that the server is listening; non-GET /api/ calls also
    need the X-To-Router: 1 header.
  * The self-check port is auto-picked per run unless --port is given, so a
    stale listener cannot make a clean run fail.

Run:
  PYTHONUTF8=1 python test/acceptance_real_clients.py                  # self-check
  PYTHONUTF8=1 python test/acceptance_real_clients.py --client codex   # real
"""
import argparse
import http.client
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

NO_WINDOW = subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0
NEW_GROUP = subprocess.CREATE_NEW_PROCESS_GROUP if os.name == "nt" else 0

# Secrets that must never reach the evidence file or the console in full.
Secrets = []

class GatewayUnreachable(Exception):
    """The gateway stopped answering mid-run; reported, never swallowed."""


passed, failed = [], []


def check(name, cond, detail=""):
    (passed if cond else failed).append(name)
    print(("  PASS " if cond else "  FAIL ") + name + ((" :: " + str(detail)) if detail and not cond else ""))


# --------------------------------------------------------------------------
# shared plumbing
# --------------------------------------------------------------------------

def call(url, body=None, method="GET", headers=None, timeout=60):
    req = urllib.request.Request(url, data=json.dumps(body).encode() if body is not None else None, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()
    except Exception as e:
        raise GatewayUnreachable("cannot reach %s: %s: %s" % (url, type(e).__name__, e))


def call_sse(url, body, headers, timeout=90):
    """POST a streaming request and return (status, headers, full SSE text)."""
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        chunks = []
        while True:
            line = r.readline()
            if not line:
                break
            chunks.append(line.decode("utf-8", "replace"))
        return r.status, dict(r.headers), "".join(chunks)


def sse_events(text):
    """The JSON payload of every data: line, in order; [DONE] included."""
    out = []
    for line in text.splitlines():
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            out.append("[DONE]")
            continue
        try:
            out.append(json.loads(payload))
        except ValueError:
            pass
    return out


def usage_rows(origin, H, model=None, status=None, limit=200):
    q = "/api/logs?limit=%d" % limit
    if model:
        q += "&model=" + urllib.parse.quote(model)
    if status:
        q += "&status=%d" % status
    st, _, body = call(origin + q, headers=H)
    if st != 200:
        return []
    return sorted(json.loads(body), key=lambda r: r["id"])


def latest_usage_id(origin, H):
    rows = usage_rows(origin, H, limit=1)
    return rows[-1]["id"] if rows else 0


def new_usage(origin, H, marker, model=None):
    return [r for r in usage_rows(origin, H, model=model) if r["id"] > marker]


def wait_usage(origin, H, marker, pred, model=None, timeout=15):
    rows = []
    deadline = time.time() + timeout
    while time.time() < deadline:
        rows = new_usage(origin, H, marker, model=model)
        if pred(rows):
            break
        time.sleep(0.25)
    return rows


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def probe(url, timeout=5):
    """Reachability that reports instead of raising: (status, detail)."""
    try:
        st, _, _ = call(url, timeout=timeout)
        return st, ""
    except Exception as e:
        return None, "%s: %s" % (type(e).__name__, e)


def refused(url, body, method, headers, tries=3):
    """POST a request the gateway must refuse and read its refusal.

    A refused POST is racy on the wire: the auth layer answers before the
    request body is ever read, and Go closes a connection whose body it never
    consumed — the client then sees a reset instead of the 401 it was sent
    (measured on this machine: ~40% of attempts; docs/note/good-finds.md G015).
    Either outcome proves the request never reached the model API; only a 200
    would be a failure. A few tries usually surface the 401 itself.
    """
    detail = ""
    for _ in range(tries):
        try:
            st, _, text = call(url, body, method, headers)
            return st, text
        except Exception as e:  # urllib raises what it cannot classify
            detail = "%s: %s" % (type(e).__name__, e)
    return "reset", detail


# --------------------------------------------------------------------------
# mock upstream: one process serving every format, plus a slow trickle mode
# --------------------------------------------------------------------------

class Server(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        pass  # a client that vanished mid-stream is the point of the cancel check


def _chat_payload(stream):
    if not stream:
        return json.dumps({"id": "m", "object": "chat.completion", "model": "up",
                           "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"},
                                        "finish_reason": "stop"}],
                           "usage": {"prompt_tokens": 5, "completion_tokens": 2}}), None, None
    events = [
        {"id": "m", "object": "chat.completion.chunk", "model": "up",
         "choices": [{"index": 0, "delta": {"role": "assistant", "content": "ok"}, "finish_reason": None}]},
        {"id": "m", "object": "chat.completion.chunk", "model": "up",
         "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
         "usage": {"prompt_tokens": 5, "completion_tokens": 2}},
    ]
    return None, events, "[DONE]"


def _messages_payload(stream):
    if not stream:
        return json.dumps({"id": "m1", "type": "message", "role": "assistant", "model": "up",
                           "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
                           "usage": {"input_tokens": 7, "output_tokens": 2}}), None, None
    events = [
        ("message_start", {"type": "message_start", "message": {"id": "m1", "type": "message", "role": "assistant",
                                                                "model": "up", "content": [],
                                                                "usage": {"input_tokens": 7, "output_tokens": 0}}}),
        ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "ok"}}),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 2}}),
        ("message_stop", {"type": "message_stop"}),
    ]
    return None, events, None


def _responses_payload(stream):
    body = {"id": "resp_1", "object": "response", "model": "up", "status": "completed",
            "output": [{"type": "message", "role": "assistant",
                        "content": [{"type": "output_text", "text": "ok"}]}],
            "usage": {"input_tokens": 11, "output_tokens": 5, "total_tokens": 16}}
    if not stream:
        return json.dumps(body), None, None
    events = [
        {"type": "response.created", "response": dict(body, status="in_progress", output=[])},
        {"type": "response.output_text.delta", "delta": "ok"},
        {"type": "response.completed", "response": body},
    ]
    return None, events, None


def _gemini_payload(stream):
    def body(text="ok", finish=""):
        return {"candidates": [{"content": {"role": "model", "parts": [{"text": text}]}, "finishReason": finish}],
                "usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 2, "totalTokenCount": 7}}
    if not stream:
        return json.dumps(body(finish="STOP")), None, None
    return None, [body(), body(finish="STOP")], None


# Filler events for slow mode: a stream that keeps flowing long after a client
# has stopped reading, so an interruption can be observed mid-flight.
_FILLERS = {
    "chat": lambda: {"id": "m", "object": "chat.completion.chunk", "model": "up",
                     "choices": [{"index": 0, "delta": {"content": "."}, "finish_reason": None}]},
    "messages": ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                         "delta": {"type": "text_delta", "text": "."}}),
    "responses": lambda: {"type": "response.output_text.delta", "delta": "."},
    "gemini": lambda: {"candidates": [{"content": {"role": "model", "parts": [{"text": "."}]}}]},
}


def _filler(kind):
    f = _FILLERS[kind]
    return f() if callable(f) else f


class Mock:
    """Configurable upstream. mode: ok | slow | 429

    One mock serves every format the gateway speaks, so a channel can point at
    it for any protocol; last_path / last_body record what the gateway
    actually called — that is how the endpoint checks prove routing instead of
    trusting the status code alone.
    """

    def __init__(self, mode="ok"):
        self.mode = mode
        self.hits = 0
        self.last_body = ""
        self.last_path = ""

    def serve(self):
        mock = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                mock.hits += 1
                n = int(self.headers.get("content-length", 0))
                mock.last_body = self.rfile.read(n).decode("utf-8", "replace")
                mock.last_path = self.path
                if mock.mode == "429":
                    self.send_response(429)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                streaming = "streamGenerateContent" in self.path
                if not streaming:
                    try:
                        streaming = json.loads(mock.last_body).get("stream") is True
                    except ValueError:
                        streaming = False
                if self.path.startswith("/v1beta/models/"):
                    kind, payload = "gemini", _gemini_payload(streaming)
                elif "/v1/messages" in self.path:
                    kind, payload = "messages", _messages_payload(streaming)
                elif "/v1/responses" in self.path:
                    kind, payload = "responses", _responses_payload(streaming)
                else:
                    kind, payload = "chat", _chat_payload(streaming)
                self.reply(kind, *payload)

            def reply(self, kind, body, events, done):
                if body is not None:
                    out = body.encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(out)))
                    self.end_headers()
                    self.wfile.write(out)
                    return
                events = list(events)
                if mock.mode == "slow":
                    events = [_filler(kind) for _ in range(80)] + events
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for item in events:
                    if mock.mode == "slow":
                        time.sleep(0.25)
                    if isinstance(item, tuple):
                        self.wfile.write(("event: %s\ndata: %s\n\n" % (item[0], json.dumps(item[1]))).encode())
                    else:
                        self.wfile.write(("data: %s\n\n" % json.dumps(item)).encode())
                    self.wfile.flush()
                if done:
                    self.wfile.write(("data: %s\n\n" % done).encode())
                    self.wfile.flush()

        return Server(("127.0.0.1", 0), H)


class Mocks:
    def __init__(self):
        self.servers, self.mocks = [], []

    def fresh(self, mode):
        m = Mock(mode)
        srv = m.serve()
        self.servers.append(srv)
        self.mocks.append(m)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        return m, "http://127.0.0.1:%d" % srv.server_address[1]

    def shutdown(self):
        for srv in self.servers:
            srv.shutdown()


def set_channel_enabled(origin, headers, cid, enabled):
    """Update through the console's write contract, excluding GET projections.

    An empty api_key preserves the stored credential under PUT semantics.
    """
    st, _, body = call(origin + "/api/channels", headers=headers)
    if st != 200:
        return st, body
    row = [c for c in json.loads(body) if c["id"] == cid]
    if not row:
        return 404, "channel not found"
    c = {k: v for k, v in row[0].items() if k not in (
        "has_key", "has_oauth", "associated_prices", "associated_price_models",
        "model_configs", "model_catalog_refs")}
    c["enabled"] = enabled
    st, _, body = call(origin + "/api/channels/%d" % cid, c, "PUT", headers)
    return st, body


class Gateway:
    """A real ToRouter daemon process on a private port with a throwaway
    data dir, an admin session and a client key. Same scaffolding as
    test/acceptance_routing.py (G009: -show-key is its own invocation)."""

    def __init__(self, port):
        self.configured_models = set()
        self.port = port
        self.origin = "http://127.0.0.1:%d" % port
        self.data = tempfile.mkdtemp(prefix="tr-trial-")
        self.exe = os.path.join(self.data, "ToRouter.exe")
        subprocess.run(["go", "build", "-tags", "production", "-trimpath", "-ldflags", "-s -w -H windowsgui" if os.name == "nt" else "-s -w", "-o", self.exe, "./cmd/torouter"],
                       check=True, creationflags=NO_WINDOW)
        got = subprocess.run([self.exe, "-mode", "cli", "-data", self.data, "-show-key"], capture_output=True,
                             text=True, encoding="utf-8", errors="replace", timeout=60,
                             creationflags=NO_WINDOW)
        self.admin_key = got.stdout.strip().splitlines()[-1].strip() if got.stdout.strip() else ""
        if not self.admin_key.startswith("tr_"):
            raise RuntimeError("no admin key: rc=%s out=%r err=%r"
                               % (got.returncode, got.stdout, got.stderr))
        print("admin key: " + self.admin_key[:8] + "...")
        self.proc = subprocess.Popen([self.exe, "-mode", "daemon", "-data", self.data, "-port", str(port)],
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                                     encoding="utf-8", errors="replace", creationflags=NO_WINDOW)
        self.wait_healthy()
        st, hdr, body = call(self.origin + "/api/auth/login",
                             {"key": self.admin_key, "remember": False}, "POST", {"X-To-Router": "1"})
        if st != 200:
            raise RuntimeError("login failed: %s %s" % (st, body))
        self.H = {"X-To-Router": "1", "Cookie": hdr.get("Set-Cookie", "").split(";")[0],
                  "Content-Type": "application/json"}
        st, _, body = call(self.origin + "/api/client-keys", {"name": "trial"}, "POST", self.H)
        if st != 201:
            raise RuntimeError("client key failed: %s %s" % (st, body))
        token = json.loads(body)["key"]
        self.G = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}

    def wait_healthy(self, timeout=30):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.proc.poll() is not None:
                raise RuntimeError("server exited early rc=%s: %s"
                                   % (self.proc.returncode, self.proc.stdout.read()))
            try:
                st, _, _ = call(self.origin + "/api/health", timeout=3)
                if st in (200, 401):
                    return
            except Exception:
                pass
            time.sleep(0.2)
        raise RuntimeError("server never became healthy")

    def channel(self, name, proto, url, models):
        st, _, b = call(self.origin + "/api/channels",
                        {"name": name, "api_key": "upstream-secret",
                         "endpoints": [{"format": formats[proto], "base_url": url}],
                         "models": models, "enabled": True}, "POST", self.H)
        if st != 200:
            print("  channel save failed:", st, b)
        return json.loads(b)["id"]

    def model(self, mid, bindings, fallback=True):
        editing = mid in self.configured_models
        endpoint = "/api/models/" + urllib.parse.quote(mid, safe="") if editing else "/api/models"
        st, _, b = call(self.origin + endpoint,
                        {"id": mid, "enabled": True, "fallback": fallback,
                         "bindings": [{"channel_id": c, "model": m, "priority": p, "enabled": True}
                                      for c, m, p in bindings]}, "PUT" if editing else "POST", self.H)
        assert st == 200, ("model save failed", st, b)
        self.configured_models.add(mid)

    def set_channel_enabled(self, cid, enabled):
        return set_channel_enabled(self.origin, self.H, cid, enabled)

    def stop(self):
        self.proc.terminate()
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        print("server exit code: %s" % self.proc.returncode)


formats = {"openai": "openai_chat_completions", "anthropic": "anthropic_messages",
           "gemini": "gemini_generate_content", "responses": "openai_responses"}


# --------------------------------------------------------------------------
# self-check mode: every gateway-side capability the client checklist needs
# --------------------------------------------------------------------------

def self_check(args):
    port = args.port or free_port()
    origin = "http://127.0.0.1:%d" % port
    gw, mocks = None, Mocks()
    try:
        # A stale listener on the chosen port must not look like a product
        # failure: the gateway fails fast and loudly, so pick another port.
        for _ in range(3):
            try:
                gw = Gateway(port)
                break
            except RuntimeError as e:
                if "exited early" not in str(e):
                    raise
                port = free_port()
                origin = "http://127.0.0.1:%d" % port
        if gw is None:
            print("FATAL: could not start a gateway on a private port")
            return 1
        H, G = gw.H, gw.G
        print("gateway: " + origin)

        def fresh(mode):
            return mocks.fresh(mode)

        USER = [{"role": "user", "content": "hi"}]
        chat_body = {"model": "chat", "messages": USER}

        # The chat channel comes first: C1 asserts the model is visible to
        # clients before any scenario touches it.
        chat_mock, chat_url = fresh("ok")
        c_chat = gw.channel("chan-chat", "openai", chat_url, ["chat"])
        gw.model("chat", [(c_chat, "chat", 0)])

        # ---- C1: the base URL answers and lists the downstream models.
        # OpenAI-compatible clients probe it before anything else.
        st, _, body = call(origin + "/v1/models", headers=G)
        check("C1 GET /v1/models on the base URL answers 200", st == 200, body[:200])
        listed = [m.get("id") for m in json.loads(body).get("data", [])] if st == 200 else []
        check("C1 the downstream model is listed for clients", "chat" in listed, listed)

        # ---- C2: key auth — what each CLI sends must pass, nothing else may.
        # Claude Code sends x-api-key, the Gemini CLI sends x-goog-api-key,
        # OpenAI-compatible clients send Bearer; the admin key must not work.
        # A body-less GET is the deterministic form of these checks (G015).
        for label, headers in (("no key", None), ("a wrong key", {"Authorization": "Bearer tr_bogus"}),
                               ("the admin key", {"Authorization": "Bearer " + gw.admin_key})):
            st, _, body = call(origin + "/v1/models", headers=headers)
            check("C2 %s is refused on the model API" % label, st == 401, (st, body[:120]))
        st, detail = refused(origin + "/v1/chat/completions", chat_body, "POST",
                             {"Authorization": "Bearer tr_bogus"})
        check("C2 a wrong key never reaches a chat POST", st in (401, "reset"), (st, detail[:120]))
        ck = G["Authorization"][7:]
        check("C2 x-api-key is accepted (Claude Code)",
              call(origin + "/v1/chat/completions", chat_body, "POST", {"x-api-key": ck})[0] == 200)
        check("C2 x-goog-api-key is accepted (Gemini CLI)",
              call(origin + "/v1/chat/completions", chat_body, "POST", {"x-goog-api-key": ck})[0] == 200)

        # ---- C3: the chat endpoint serves its own format.
        marker = latest_usage_id(origin, H)
        st, _, body = call(origin + "/v1/chat/completions", chat_body, "POST", G)
        check("C3 chat completions answers 200", st == 200, body[:200])
        ok = st == 200 and bool(json.loads(body).get("choices", [{}])[0].get("message", {}).get("content"))
        check("C3 the answer body is chat-shaped", ok, body[:200])
        check("C3 the gateway called the chat endpoint",
              chat_mock.last_path == "/v1/chat/completions", chat_mock.last_path)
        rows = wait_usage(origin, H, marker, lambda rs: bool(rs), model="chat")
        r = rows[-1] if rows else {}
        check("C3 a usage row landed", bool(rows), rows)
        check("C3 the row names both formats",
              r.get("inbound") == "openai_chat_completions" and r.get("outbound") == "openai_chat_completions", r)
        check("C3 the row carries real tokens, not an estimate",
              r.get("status") == 200 and r.get("input_tokens", 0) > 0
              and r.get("output_tokens", 0) > 0 and not r.get("estimated"), r)

        # ---- C4: the messages endpoint serves Anthropic Messages.
        msg_mock, msg_url = fresh("ok")
        c_msg = gw.channel("chan-messages", "anthropic", msg_url, ["messages"])
        gw.model("messages", [(c_msg, "messages", 0)])
        marker = latest_usage_id(origin, H)
        st, _, body = call(origin + "/v1/messages",
                           {"model": "messages", "max_tokens": 64, "messages": USER}, "POST", G)
        check("C4 messages answers 200", st == 200, body[:200])
        ok = st == 200 and bool(json.loads(body).get("content", [{}])[0].get("text"))
        check("C4 the answer body is messages-shaped", ok, body[:200])
        check("C4 the gateway called the messages endpoint",
              msg_mock.last_path == "/v1/messages", msg_mock.last_path)
        rows = wait_usage(origin, H, marker, lambda rs: bool(rs), model="messages")
        r = rows[-1] if rows else {}
        check("C4 a usage row landed", bool(rows), rows)
        check("C4 the row names both formats",
              r.get("inbound") == "anthropic_messages" and r.get("outbound") == "anthropic_messages", r)
        check("C4 the row carries real tokens, not an estimate",
              r.get("status") == 200 and r.get("input_tokens", 0) > 0
              and r.get("output_tokens", 0) > 0 and not r.get("estimated"), r)

        # ---- C5: the responses endpoint serves OpenAI Responses, usage included
        # (its schema differs from chat completions — T024).
        resp_mock, resp_url = fresh("ok")
        c_resp = gw.channel("chan-responses", "responses", resp_url, ["responses"])
        gw.model("responses", [(c_resp, "responses", 0)])
        marker = latest_usage_id(origin, H)
        st, _, body = call(origin + "/v1/responses",
                           {"model": "responses", "input": "hi"}, "POST", G)
        check("C5 responses answers 200", st == 200, body[:200])
        check("C5 the gateway called the responses endpoint",
              resp_mock.last_path == "/v1/responses", resp_mock.last_path)
        rows = wait_usage(origin, H, marker, lambda rs: bool(rs), model="responses")
        r = rows[-1] if rows else {}
        check("C5 a usage row landed", bool(rows), rows)
        check("C5 the responses usage schema was parsed (T024)",
              r.get("status") == 200 and r.get("input_tokens") == 11
              and r.get("output_tokens") == 5 and not r.get("estimated"), r)

        # ---- C6: the gemini endpoint serves generateContent. The Gemini CLI
        # speaks it, so it belongs on the checklist next to the other three.
        gem_mock, gem_url = fresh("ok")
        c_gem = gw.channel("chan-gemini", "gemini", gem_url, ["gemini"])
        gw.model("gemini", [(c_gem, "gemini", 0)])
        marker = latest_usage_id(origin, H)
        st, _, body = call(origin + "/v1beta/models/gemini:generateContent",
                           {"contents": [{"role": "user", "parts": [{"text": "hi"}]}]}, "POST", G)
        check("C6 generateContent answers 200", st == 200, body[:200])
        ok = st == 200 and bool(json.loads(body).get("candidates", [{}])[0].get("content", {}).get("parts", [{}])[0].get("text"))
        check("C6 the answer body is gemini-shaped", ok, body[:200])
        check("C6 the gateway called the gemini endpoint",
              gem_mock.last_path == "/v1beta/models/gemini:generateContent", gem_mock.last_path)
        rows = wait_usage(origin, H, marker, lambda rs: bool(rs), model="gemini")
        r = rows[-1] if rows else {}
        check("C6 the row names both formats",
              r.get("inbound") == "gemini_generate_content" and r.get("outbound") == "gemini_generate_content", r)

        # ---- C7: SSE streams terminate in each format's own terminator, and
        # the usage row proves the stream was consumed to the end. One model
        # per format so each request is served by the endpoint that speaks it
        # instead of being converted by a higher-priority binding.
        chat_stream, chat_stream_url = fresh("ok")
        c_s_chat = gw.channel("chan-stream-chat", "openai", chat_stream_url, ["st-chat"])
        gw.model("st-chat", [(c_s_chat, "st-chat", 0)])
        msg_stream, msg_stream_url = fresh("ok")
        c_s_msg = gw.channel("chan-stream-messages", "anthropic", msg_stream_url, ["st-messages"])
        gw.model("st-messages", [(c_s_msg, "st-messages", 0)])
        resp_stream, resp_stream_url = fresh("ok")
        c_s_resp = gw.channel("chan-stream-responses", "responses", resp_stream_url, ["st-responses"])
        gw.model("st-responses", [(c_s_resp, "st-responses", 0)])
        gem_stream, gem_stream_url = fresh("ok")
        c_s_gem = gw.channel("chan-stream-gemini", "gemini", gem_stream_url, ["st-gemini"])
        gw.model("st-gemini", [(c_s_gem, "st-gemini", 0)])

        marker = latest_usage_id(origin, H)
        st, hdr, text = call_sse(origin + "/v1/chat/completions",
                                 {"model": "st-chat", "messages": USER, "stream": True}, G)
        events = sse_events(text)
        check("C7 chat stream answers 200 with an event stream",
              st == 200 and "text/event-stream" in hdr.get("Content-Type", ""), (st, hdr.get("Content-Type")))
        check("C7 chat stream terminates with [DONE]",
              bool(events) and events[-1] == "[DONE]", text[-200:])
        check("C7 chat stream reports the finish reason",
              any(e.get("choices", [{}])[0].get("finish_reason") == "stop" for e in events if e != "[DONE]"), events)

        st, hdr, text = call_sse(origin + "/v1/messages",
                                 {"model": "st-messages", "max_tokens": 64, "messages": USER, "stream": True}, G)
        events = sse_events(text)
        check("C7 messages stream terminates with message_stop",
              st == 200 and any(e.get("type") == "message_stop" for e in events), text[-200:])

        st, hdr, text = call_sse(origin + "/v1/responses",
                                 {"model": "st-responses", "input": "hi", "stream": True}, G)
        events = sse_events(text)
        check("C7 responses stream terminates with response.completed",
              st == 200 and any(e.get("type") == "response.completed" for e in events), text[-200:])

        st, hdr, text = call_sse(origin + "/v1beta/models/st-gemini:streamGenerateContent",
                                 {"contents": [{"role": "user", "parts": [{"text": "hi"}]}]}, G)
        events = sse_events(text)
        check("C7 gemini stream terminates with a finish reason",
              st == 200 and any((e.get("candidates") or [{}])[0].get("finishReason") for e in events), text[-200:])

        rows = wait_usage(origin, H, marker, lambda rs: len(rs) >= 4, timeout=25)
        check("C7 one usage row per stream", len(rows) >= 4, len(rows))
        check("C7 every streamed row is 200 with output tokens",
              bool(rows) and all(r["status"] == 200 and r["output_tokens"] > 0 and not r.get("estimated")
                                 for r in rows),
              [(r["model"], r["status"], r["output_tokens"], r.get("estimated")) for r in rows])
        check("C7 each stream kept its own inbound format",
              {r.get("inbound") for r in rows} >= {"openai_chat_completions", "anthropic_messages",
                                                   "openai_responses", "gemini_generate_content"},
              sorted({r.get("inbound") for r in rows}))

        # ---- C8: a client that vanishes mid-stream is recorded as 499, not
        # swallowed. The gateway must not retry a committed stream either.
        cancel_mock, cancel_url = fresh("slow")
        c_cancel = gw.channel("chan-cancel", "openai", cancel_url, ["cancel"])
        gw.model("cancel", [(c_cancel, "cancel", 0)])
        marker = latest_usage_id(origin, H)
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
        conn.request("POST", "/v1/chat/completions",
                     body=json.dumps({"model": "cancel", "messages": USER, "stream": True}).encode(),
                     headers={**G, "Content-Type": "application/json"})
        resp = conn.getresponse()
        check("C8 the stream started", resp.status == 200, resp.status)
        resp.read(128)  # let a few chunks flow, then vanish like Ctrl+C would
        conn.close()
        rows = wait_usage(origin, H, marker,
                          lambda rs: any(r["status"] == 499 for r in rs), model="cancel", timeout=25)
        cancelled = [r for r in rows if r["status"] == 499]
        check("C8 the interruption is recorded as 499", len(cancelled) == 1,
              [(r["status"], r.get("error")) for r in rows])
        check("C8 the row says the client cancelled",
              bool(cancelled) and "cancel" in (cancelled[0].get("error") or "").lower(), cancelled)
        check("C8 nothing was resent after the disconnect",
              len([r for r in rows if r["model"] == "cancel"]) == 1,
              [(r["status"], r.get("error")) for r in rows])

        # ---- C9: session affinity from a header. Reorder two equal-priority
        # bindings after the first turn: new requests must use the new head,
        # while the existing session must remain on its original channel.
        pin_a, url_a = fresh("ok")
        pin_b, url_b = fresh("ok")
        c_pa = gw.channel("chan-aff-a", "openai", url_a, ["affinity"])
        c_pb = gw.channel("chan-aff-b", "openai", url_b, ["affinity"])
        gw.model("affinity", [(c_pa, "affinity", 0), (c_pb, "affinity", 0)])

        def turn(with_carrier, header=True):
            msgs = list(USER)
            if with_carrier:
                msgs = msgs + [{"role": "assistant", "content": "prev", "reasoning_content": "earlier"}]
            h = dict(G)
            if header:
                h["x-session-id"] = "trial-session-1"
            before_a, before_b = pin_a.hits, pin_b.hits
            st, _, body = call(origin + "/v1/chat/completions",
                               {"model": "affinity", "messages": msgs}, "POST", h)
            if st != 200:
                print("FATAL: affinity turn failed: %d %s" % (st, body))
                raise SystemExit(1)
            return st, body, (pin_a.hits - before_a, pin_b.hits - before_b)

        st, _, (a1, b1) = turn(False)
        check("C9 first turn routed once", a1 + b1 == 1, (a1, b1))
        pinned = "A" if a1 == 1 else ("B" if b1 == 1 else "")
        expected = (1, 0) if pinned == "A" else (0, 1)
        gw.model("affinity", [(c_pb, "affinity", 0), (c_pa, "affinity", 0)])
        check("C9 a new request follows the reordered head", turn(False, header=False)[2] == (0, 1))
        deltas = []
        for _ in range(4):
            deltas.append(turn(False)[2])
        check("C9 later turns keep the first channel",
              all(d == expected for d in deltas), (pinned, deltas))
        st, _, d = turn(True)
        check("C9 carried reasoning prefers the pinned channel", d == expected, (pinned, d))
        original = c_pa if pinned == "A" else c_pb
        spare = c_pb if pinned == "A" else c_pa
        moved = (0, 1) if pinned == "A" else (1, 0)
        try:
            st, body = gw.set_channel_enabled(original, False)
            check("C9 the soft-pinned channel is disabled", st == 200, (st, body))
            if st != 200:
                return 1
            st, _, d = turn(True)
            check("C9 carried reasoning succeeds on the remaining channel",
                  st == 200 and d == moved, (st, d))
        finally:
            st, body = gw.set_channel_enabled(original, True)
            check("C9 the soft-pinned channel is restored", st == 200, (st, body))
        # Make the recovered source the preferred candidate: staying on the
        # spare now proves a successful transfer updated the session pin.
        gw.model("affinity", [(original, "affinity", 0), (spare, "affinity", 1)])
        st, _, d = turn(True)
        check("C9 the transferred session remains repinned after source recovery",
              st == 200 and d == moved, (st, d))
        st, _, body = call(origin + "/v1/chat/completions",
                           {"model": "affinity", "messages": USER}, "POST", G)
        check("C9 a request with no session header still succeeds", st == 200, body[:200])

        # External Responses history is hard state: only the source account
        # and endpoint can continue the response it produced (D003).
        hard_a, hard_url_a = fresh("ok")
        hard_b, hard_url_b = fresh("ok")
        c_ha = gw.channel("chan-history-a", "responses", hard_url_a, ["history"])
        c_hb = gw.channel("chan-history-b", "responses", hard_url_b, ["history"])
        gw.model("history", [(c_ha, "history", 0), (c_hb, "history", 1)])

        def history_turn(previous_id=None):
            request = {"model": "history", "input": "continue" if previous_id else "hi"}
            if previous_id:
                request["previous_response_id"] = previous_id
            before = (hard_a.hits, hard_b.hits)
            st, _, body = call(origin + "/v1/responses", request, "POST",
                               {**G, "x-session-id": "trial-hard-session"})
            return st, body, (hard_a.hits - before[0], hard_b.hits - before[1])

        st, body, d = history_turn()
        previous_id = json.loads(body).get("id") if st == 200 else None
        check("C9 hard history is created on its source",
              st == 200 and bool(previous_id) and d == (1, 0), (st, d, body[:200]))
        if not previous_id:
            return 1
        gw.model("history", [(c_hb, "history", 0), (c_ha, "history", 1)])
        st, body, d = history_turn(previous_id)
        check("C9 hard history stays on its source after reordering",
              st == 200 and d == (1, 0), (st, d, body[:200]))
        try:
            st, body = gw.set_channel_enabled(c_ha, False)
            check("C9 the hard history source is disabled", st == 200, (st, body))
            if st != 200:
                return 1
            st, body, d = history_turn(previous_id)
            err = json.loads(body).get("error", {}) if st != 200 else {}
            check("C9 unavailable hard history refuses instead of silently moving",
                  st == 503 and err.get("recovery", {}).get("code") == "state_affinity_unavailable",
                  (st, err))
            check("C9 unavailable hard history reaches no upstream", d == (0, 0), d)
        finally:
            st, body = gw.set_channel_enabled(c_ha, True)
            check("C9 the hard history source is restored", st == 200, (st, body))
        st, body, d = history_turn(previous_id)
        check("C9 recovered hard history succeeds on its original source",
              st == 200 and d == (1, 0), (st, d, body[:200]))
        check("C9 the source receives its original response reference",
              json.loads(hard_a.last_body).get("previous_response_id") == previous_id,
              hard_a.last_body)

        print("\nself-check: %d passed, %d failed" % (len(passed), len(failed)))
        if failed:
            print("FAILED: " + ", ".join(failed))
        return 1 if failed else 0
    finally:
        if gw is not None:
            gw.stop()
        mocks.shutdown()


# --------------------------------------------------------------------------
# real mode: drive the installed CLI, collect the gateway's evidence
# --------------------------------------------------------------------------

SCENARIOS = ["text", "tools", "stream", "cancel", "fallback"]

CRITERIA = {
    "text": "退出码 0、客户端回答了非空内容、网关落了一条 200 用量行",
    "tools": "两轮都成功、各落 200 用量行、且第二轮与第一轮落在同一渠道（会话亲和）",
    "stream": "退出码 0、200 用量行且有真实输出 token；答案逐段到达（人工确认）",
    "cancel": "网关卡到一条 499 用量行，记录这次客户端中断",
    "fallback": "停用第一渠道后零命中，CLI 成功且输出非空，其他绑定渠道有 200 用量行",
}

PROMPTS = {
    "text": "只回答两个字：收到。",
    "tools_1": "在当前目录新建 trial-tool.txt，内容写 hello，完成后只回复 Done。",
    "tools_2": "读取 trial-tool.txt，原样告诉我文件内容。",
    "stream": "从 1 数到 40，每行一个数字。",
    "fallback": "只回答两个字：收到。",
}

# Per-client wiring. The values (address / key / model id) come from the
# console API at run time; the command templates below are built in and are
# NOT verified against your installed version — override them with --cmd-file
# (JSON: {"claude-code": {"text": ["claude", ...]}}). Nothing here writes a
# global config file: writing client configuration is MMA's job (D007).
CLIENTS = {
    "claude-code": {
        "bin": "claude",
        "label": "Claude Code — Anthropic Messages 入站",
        "inbound": "anthropic_messages",
        "base_url": "网关根地址（CLI 自行拼接 /v1/messages）",
        "env": [("ANTHROPIC_BASE_URL", "{origin}"), ("ANTHROPIC_AUTH_TOKEN", "{key}")],
        "install": "npm install -g @anthropic-ai/claude-code，然后跑一次 claude 完成登录",
        "cmd": {
            "text": ["claude", "-p", "{prompt}", "--model", "{model}"],
            "tools": ["claude", "-p", "{prompt}", "--model", "{model}",
                      "--allowedTools", "Read,Write,Edit,Bash"],
            "stream": ["claude", "-p", "{prompt}", "--model", "{model}",
                       "--output-format", "stream-json", "--verbose"],
        },
    },
    "codex": {
        "bin": "codex",
        "label": "Codex CLI — OpenAI Responses 入站",
        "inbound": "openai_responses",
        "base_url": "网关根地址 + /v1",
        "env": [("TO_ROUTER_API_KEY", "{key}")],
        "install": "npm install -g @openai/codex；首次使用需登录或配置 provider",
        "cmd": {
            "text": ["codex", "exec", "--skip-git-repo-check", "-m", "{model}",
                     "-c", "model_provider=\"torouter\"",
                     "-c", "model_providers.torouter={name=\"to-router\", base_url=\"{origin}/v1\", "
                           "env_key=\"TO_ROUTER_API_KEY\"}",
                     "{prompt}"],
            "tools": ["codex", "exec", "--skip-git-repo-check", "-m", "{model}",
                      "-c", "model_provider=\"torouter\"",
                      "-c", "model_providers.torouter={name=\"to-router\", base_url=\"{origin}/v1\", "
                            "env_key=\"TO_ROUTER_API_KEY\"}",
                      "{prompt}"],
            "stream": ["codex", "exec", "--skip-git-repo-check", "-m", "{model}", "--json",
                       "-c", "model_provider=\"torouter\"",
                       "-c", "model_providers.torouter={name=\"to-router\", base_url=\"{origin}/v1\", "
                             "env_key=\"TO_ROUTER_API_KEY\"}",
                       "{prompt}"],
        },
    },
    "gemini": {
        "bin": "gemini",
        "label": "Gemini CLI — generateContent 入站",
        "inbound": "gemini_generate_content",
        "base_url": "网关根地址（若 CLI 要求显式 /v1beta，以 CLI 文档为准）",
        "env": [("GEMINI_API_KEY", "{key}"), ("GOOGLE_GEMINI_BASE_URL", "{origin}")],
        "install": "npm install -g @google/gemini-cli，然后跑一次 gemini 完成登录",
        "cmd": {
            "text": ["gemini", "-p", "{prompt}", "--model", "{model}"],
            "tools": ["gemini", "-p", "{prompt}", "--model", "{model}"],
            "stream": ["gemini", "-p", "{prompt}", "--model", "{model}"],
        },
    },
}


def redact(text, *secrets, limit=4000):
    out = text or ""
    for s in sorted({s for s in [*Secrets, *secrets] if s}, key=len, reverse=True):
        out = out.replace(s, "tr_***REDACTED***")
    return out if limit is None else out[:limit]


def redact_env(env):
    """Keep URL settings after redaction; never expose other environment values."""
    out = {}
    for k, v in (env or {}).items():
        if k.upper().endswith("URL"):
            out[k] = redact(v)
        else:
            out[k] = "tr_***REDACTED***"
    return out


def build_cmd(template, prompt, model, origin, client_key):
    out = []
    for part in template:
        out.append(part.replace("{prompt}", prompt).replace("{model}", model)
                   .replace("{origin}", origin).replace("{key}", client_key))
    return out


def build_env(spec, origin, client_key):
    return {k: v.replace("{origin}", origin).replace("{key}", client_key) for k, v in spec}


def _as_text(value):
    if value is None:
        return ""
    if isinstance(value, bytes):
        return value.decode("utf-8", "replace")
    return value


def issue(cmd, env_extra, timeout, cwd=None, interrupt=False, stream_seconds=3.0, exe=None,
          first_output_timeout=120):
    """Run one client command. interrupt=True waits for the first byte of
    output, lets the stream flow for stream_seconds, then sends the console's
    Ctrl+Break (falling back to terminate for a CLI that ignored it) and
    records which method actually stopped it.

    exe overrides the template's executable with the binary that was actually
    detected, so --cli-path can drive a stand-in for rehearsal."""
    if exe:
        cmd = list(exe) + list(cmd)[1:]
    env = dict(os.environ)
    env.update(env_extra)
    env["PYTHONUTF8"] = "1"
    t0 = time.time()
    run = {"command": cmd, "env": env_extra, "cwd": cwd, "interrupted": interrupt}
    if not interrupt:
        try:
            p = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8",
                               errors="replace", timeout=timeout, env=env, cwd=cwd,
                               creationflags=NO_WINDOW)
            run.update(exit_code=p.returncode, stdout=p.stdout, stderr=p.stderr, error=None)
        except subprocess.TimeoutExpired as e:
            run.update(exit_code=None, stdout=_as_text(e.stdout), stderr=_as_text(e.stderr),
                       error="timeout after %ss" % timeout)
        except FileNotFoundError as e:
            run.update(exit_code=None, stdout="", stderr=str(e), error="cli not found: %s" % e)
        run["duration_ms"] = int((time.time() - t0) * 1000)
        return run
    try:
        # No CREATE_NO_WINDOW here: a console-less child cannot receive the
        # console's Ctrl+Break at all (verified — the event goes nowhere and
        # the client streams on). Inheriting the console is what makes the
        # interruption behave like the user's own Ctrl+C; terminate() below is
        # the fallback for a CLI that survives it.
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                env=env, cwd=cwd, creationflags=NEW_GROUP)
    except FileNotFoundError as e:
        run.update(exit_code=None, stdout="", stderr=str(e), error="cli not found: %s" % e,
                   duration_ms=0, streamed=False, interrupt_method="none")
        return run
    buf = []

    def pump():
        # read1, not readline: a streaming client prints tokens without
        # newlines, and waiting for a newline would wait for the whole answer
        # before the interrupt is even sent.
        while True:
            try:
                chunk = proc.stdout.read1(65536)
            except Exception:
                break
            if not chunk:
                break
            buf.append(chunk)

    reader = threading.Thread(target=pump, daemon=True)
    reader.start()
    streamed = False
    deadline = time.time() + first_output_timeout
    while not buf and proc.poll() is None and time.time() < deadline:
        time.sleep(0.05)
    if buf:
        streamed = True
        time.sleep(stream_seconds)
    method = "ctr_break" if os.name == "nt" else "sigint_group"
    try:
        if os.name == "nt":
            proc.send_signal(signal.CTRL_BREAK_EVENT)
        else:
            os.killpg(os.getpgid(proc.pid), signal.SIGINT)
    except Exception:
        method = "signal_failed"
    try:
        proc.wait(timeout=20)
    except subprocess.TimeoutExpired:
        method = "terminate"
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
    reader.join(timeout=5)
    text = b"".join(buf).decode("utf-8", "replace")
    run.update(exit_code=proc.returncode, stdout=text, stderr="",
               error=None if streamed else "the client produced no output within %ss; "
                                           "it was interrupted anyway" % first_output_timeout,
               streamed=streamed, interrupt_method=method)
    run["duration_ms"] = int((time.time() - t0) * 1000)
    return run


def print_pointing(client, origin, client_key, key_info, model, bindings):
    print("=" * 74)
    print("指向网关所需的值（地址与模型来自控制台 API；写入客户端配置归 MMA，见 D007）")
    print("  网关地址   : %s" % origin)
    print("  客户端 Key : %s…（完整值不打印；证据文件只留 id/name/前缀）" % redact(client_key[:8], client_key))
    print("  下游模型 ID: %s" % model)
    print("  入站格式   : %s（%s）" % (client["inbound"], client["label"]))
    print("  baseURL 语义: %s" % client["base_url"])
    if key_info.get("client_key", {}).get("name"):
        print("  客户端 Key 记录: id=%s name=%s（控制台「客户端 Key」页可吊销）"
              % (key_info["client_key"].get("id"), key_info["client_key"].get("name")))
    print("  建议环境变量（本脚本按进程注入，不写全局配置）:")
    for k, v in redact_env(build_env(client["env"], origin, client_key)).items():
        print("    %s=%s" % (k, v))
    if bindings:
        print("  该模型的绑定（路由顺序，优先级升序）:")
        for b in bindings:
            print("    - 渠道 %s（%s）上游模型 %s 优先级 %s" %
                  (b.get("channel_name"), b.get("protocol"), b.get("model"), b.get("priority")))
    print("=" * 74)


def real_run(args):
    global Secrets
    Secrets = [args.client_key or os.environ.get("TO_ROUTER_CLIENT_KEY", ""),
               args.admin_key or os.environ.get("TO_ROUTER_ADMIN_KEY", "")]
    origin = args.url.rstrip("/")
    print("== 目标网关: %s" % redact(origin))
    st, detail = probe(origin + "/api/health")
    if st not in (200, 401):
        print("FATAL: %s/api/health 无应答（status=%s %s）。先启动网关，"
              "步骤见 docs/note/trial-acceptance.md 第 2 节。" % (redact(origin), st, redact(str(detail))))
        return 2

    admin_key = args.admin_key or os.environ.get("TO_ROUTER_ADMIN_KEY", "")
    if not admin_key and sys.stdin.isatty():
        import getpass
        admin_key = getpass.getpass("管理员密钥（托盘菜单「复制管理员密钥」或 -show-key）: ").strip()
    if not admin_key:
        print("FATAL: 控制台 API 需要管理员密钥：--admin-key、环境变量 TO_ROUTER_ADMIN_KEY，或交互输入。")
        return 2
    Secrets.append(admin_key)
    st, hdr, body = call(origin + "/api/auth/login", {"key": admin_key, "remember": False},
                         "POST", {"X-To-Router": "1"})
    if st != 200:
        print("FATAL: 管理员登录失败（%s）。检查密钥后重试。" % st)
        return 2
    H = {"X-To-Router": "1", "Cookie": hdr.get("Set-Cookie", "").split(";")[0],
         "Content-Type": "application/json"}

    # The seam values come from the gateway's own API (D007): the address and
    # the model list are read here, never assembled by hand, so a port change
    # cannot invalidate them. The client key is the one value the gateway
    # shows exactly once at creation time, so the run uses a key the operator
    # already holds (--client-key); the rehearsal creates a throwaway one.
    st, _, body = call(origin + "/api/connection", headers=H)
    conn = json.loads(body) if st == 200 else {}
    if conn.get("port"):
        origin = "http://127.0.0.1:%d" % conn["port"]
    client = CLIENTS[args.client]
    cmds = dict(client["cmd"])
    if args.cmd_file:
        with open(args.cmd_file, encoding="utf-8") as f:
            cmds.update(json.load(f).get(args.client, {}))

    cli_path = args.cli_path or shutil.which(client["bin"])
    if not cli_path:
        print("SKIP: 未检测到 %s（PATH 里没有 %s）。安装：%s" % (client["bin"], client["bin"], client["install"]))
        print("      装好并登录后重跑同一命令即可；本脚本不改任何全局客户端配置。")
        doc = evidence_doc(args, origin, None, {"client_key": {"id": None, "name": None}}, cli_path,
                           [{"scenario": "all", "criteria": "CLI 未安装，未执行任何场景",
                             "runs": [], "usage_rows": [], "auto_pass": None,
                             "auto_detail": "cli_not_installed: %s" % client["bin"],
                             "manual_confirm": None,
                             "manual_hint": "安装并登录 %s 后重跑" % client["bin"],
                             "skipped": True}], client, cmds, [])
        doc["summary"]["needs_human"] = 1
        path = os.path.join(args.evidence_dir, "%s-%s.json"
                            % (args.client, time.strftime("%Y%m%d-%H%M%S")))
        write_evidence(path, doc)
        return 0

    st, _, body = call(origin + "/api/models", headers=H)
    if st != 200:
        print("FATAL: 读取下游模型失败（%s）。" % st)
        return 2
    all_models = json.loads(body)
    enabled = [m for m in all_models if m.get("enabled")]
    model = args.model
    if not model:
        if len(enabled) == 1:
            model = enabled[0]["id"]
        else:
            print("FATAL: 请用 --model 指定下游模型 ID。已启用的模型:")
            for m in enabled:
                print("  - %s（绑定 %d 个渠道）" % (m["id"], len(m.get("bindings", []))))
            return 2
    chosen = [m for m in all_models if m["id"] == model]
    if not chosen or not chosen[0].get("enabled"):
        print("FATAL: 下游模型 %s 不存在或未启用。" % model)
        return 2
    bindings = sorted([b for b in chosen[0].get("bindings", [])
                       if b.get("enabled") and b.get("channel_exists")],
                      key=lambda b: b.get("priority", 0))

    # A .py stand-in cannot be CreateProcess'd on Windows; run it with this
    # interpreter. Real CLIs are executed as detected.
    cli_cmd = [cli_path]
    if cli_path.lower().endswith((".py", ".pyw")):
        cli_cmd = [sys.executable, cli_path]

    # The client key comes last of the prerequisites: a CLI that is not even
    # installed is reported without asking for one.
    if args.demo:
        st, _, body = call(origin + "/api/client-keys", {"name": "trial-demo"}, "POST", H)
        if st != 201:
            print("FATAL: 创建一次性客户端 Key 失败（%s %s）。" % (st, redact(_as_text(body))))
            return 2
        key_info = json.loads(body)
        client_key = key_info["key"]
    else:
        client_key = args.client_key or os.environ.get("TO_ROUTER_CLIENT_KEY", "")
        if not client_key:
            print("FATAL: 需要一个客户端 API Key。到控制台「设置 · 客户端 Key」新建一个"
                  "（创建时完整值只显示一次），然后用 --client-key 传入，"
                  "或设环境变量 TO_ROUTER_CLIENT_KEY。")
            print("      该 Key 就是之后要填进 MMA / 客户端配置的那个；管理员密钥不能调模型接口。")
            return 2
        key_info = {"client_key": {"id": None, "name": None}}
    Secrets = [client_key, admin_key]

    print_pointing(client, origin, client_key, key_info, model, bindings)
    if not args.yes and not args.demo:
        print("\n真实验收会驱动真实 CLI 与真实上游。先确认自检模式全绿：")
        print("  PYTHONUTF8=1 python test/acceptance_real_clients.py")
        print("确认已跑过后加 --yes 继续（docs/note/trial-acceptance.md 第 3 节）。")
        return 2
    if args.demo:
        print("\n[演练模式] 使用隔离网关 + mock 上游 + %s 作为客户端替身；"
              "产出的证据标记 dry-run，不作为验收证据。" % os.path.basename(cli_path))

    evidence_dir = args.evidence_dir
    os.makedirs(os.path.join(evidence_dir, "prompts"), exist_ok=True)
    prompts = {}
    for name, text in PROMPTS.items():
        path = os.path.join(evidence_dir, "prompts", "%s.txt" % name)
        with open(path, "w", encoding="utf-8") as f:
            f.write(text)
        prompts[name] = path

    # Which built-in template each scenario runs: cancel reuses the streaming
    # command, fallback reuses the plain text one.
    TEMPLATE_OF = {"text": "text", "tools": "tools", "stream": "stream",
                   "cancel": "stream", "fallback": "text"}
    env_extra = build_env(client["env"], origin, client_key)

    def show(cmd):
        """The command as it actually runs: the template's executable swapped
        for the binary that was detected (or given via --cli-path)."""
        return redact(" ".join(cli_cmd + list(cmd)[1:]))

    records = []
    for name in args.scenario:
        print("\n---- 场景 %s：%s" % (name, CRITERIA[name]))
        prompt_key = {"text": "text", "tools": "tools_1", "stream": "stream",
                      "cancel": "stream", "fallback": "fallback"}[name]
        model_for = args.cancel_model if name == "cancel" and args.cancel_model else model
        template = cmds[TEMPLATE_OF[name]]
        runs = []
        rows = []
        lifecycle_error = ""

        if name == "fallback" and len(bindings) >= 2:
            # Exclude the first channel before sending; this checks disabled
            # candidate selection, not retry after an upstream request failed.
            # Restore its original enabled state even when the command fails.
            first = bindings[0]
            st, _, body = call(origin + "/api/channels", headers=H)
            row = [c for c in json.loads(body) if c["id"] == first.get("channel_id")] if st == 200 else []
            if not row:
                lifecycle_error = "读不到第一渠道（HTTP %s），未执行停用演练" % st
            else:
                was_enabled = row[0].get("enabled", False)
                try:
                    st, b2 = set_channel_enabled(origin, H, first["channel_id"], False)
                    if st != 200:
                        lifecycle_error = "停用第一渠道失败（%s %s）" % (st, redact(_as_text(b2)))
                    else:
                        print("  [fallback] 已停用第一渠道 %s（id=%s）"
                              % (first.get("channel_name"), first.get("channel_id")))
                        marker = latest_usage_id(origin, H)
                        prompt = PROMPTS[prompt_key]
                        cmd = build_cmd(template, prompt, model_for, origin, client_key)
                        print("  $ %s" % show(cmd))
                        runs.append(issue(cmd, env_extra, args.timeout, exe=cli_cmd))
                        rows = wait_usage(origin, H, marker, lambda rs: bool(rs),
                                          model=model_for, timeout=20)
                finally:
                    st, b2 = set_channel_enabled(origin, H, first["channel_id"], was_enabled)
                    if st != 200:
                        lifecycle_error += ("；" if lifecycle_error else "") + (
                            "恢复第一渠道原启停状态失败（%s %s）" % (st, redact(_as_text(b2))))
                    else:
                        print("  [fallback] 已恢复第一渠道 %s 原启停状态（enabled=%s）"
                              % (first.get("channel_name"), was_enabled))

        if name == "tools":
            marker = latest_usage_id(origin, H)
            for key in ("tools_1", "tools_2"):
                cmd = build_cmd(template, PROMPTS[key], model_for, origin, client_key)
                print("  $ %s" % show(cmd))
                runs.append(issue(cmd, env_extra, args.timeout, exe=cli_cmd))
            rows = wait_usage(origin, H, marker, lambda rs: len(rs) >= 2,
                              model=model_for, timeout=25)
        elif name == "cancel":
            marker = latest_usage_id(origin, H)
            cmd = build_cmd(template, PROMPTS[prompt_key], model_for, origin, client_key)
            print("  $ %s   （流式中途发送 Ctrl+Break）" % show(cmd))
            runs.append(issue(cmd, env_extra, args.timeout, interrupt=True,
                              stream_seconds=args.stream_seconds, exe=cli_cmd,
                              first_output_timeout=args.first_output_timeout))
            rows = wait_usage(origin, H, marker, lambda rs: any(r["status"] == 499 for r in rs),
                              model=model_for, timeout=25)
        elif name != "fallback" or len(bindings) < 2:
            marker = latest_usage_id(origin, H)
            cmd = build_cmd(template, PROMPTS[prompt_key], model_for, origin, client_key)
            print("  $ %s" % show(cmd))
            runs.append(issue(cmd, env_extra, args.timeout, exe=cli_cmd))
            rows = wait_usage(origin, H, marker, lambda rs: bool(rs),
                              model=model_for, timeout=20)

        channel_of = {}
        for r in rows:
            channel_of[r.get("channel_id")] = channel_of.get(r.get("channel_id"), 0) + 1
        auto, detail = auto_eval(name, runs, rows, channel_of, bindings)
        if lifecycle_error:
            auto, detail = False, lifecycle_error
        record = {"scenario": name, "criteria": CRITERIA[name],
                  "prompt_files": [prompts[prompt_key]], "runs": [
                      {"command": [redact(p, limit=None) for p in r["command"]], "env": redact_env(r.get("env")),
                       "exit_code": r.get("exit_code"), "duration_ms": r.get("duration_ms"),
                       "interrupted": r.get("interrupted", False),
                       "interrupt_method": r.get("interrupt_method"),
                       "streamed_before_interrupt": r.get("streamed"),
                       "stdout_tail": redact(r.get("stdout"), *Secrets),
                       "stderr_tail": redact(r.get("stderr"), *Secrets),
                       "error": redact(r["error"]) if r.get("error") is not None else None} for r in runs],
                  "usage_rows": [usage_summary(r) for r in rows],
                  "auto_pass": auto, "auto_detail": detail,
                  "manual_confirm": None,
                  "manual_hint": MANUAL_HINTS[name]}
        records.append(record)
        print("  自动判定: %s" % ("PASS" if auto else ("FAIL" if auto is False else "无法自动判定")))
        print("  依据: %s" % redact(detail))
        print("  待人工确认: %s" % MANUAL_HINTS[name])

    doc = evidence_doc(args, origin, model, key_info, cli_path, records, client, cmds, bindings)
    path = os.path.join(evidence_dir, "%s-%s.json"
                        % (args.client, time.strftime("%Y%m%d-%H%M%S")))
    write_evidence(path, doc)
    print("\n证据文件: %s" % os.path.abspath(path))
    print("把该文件回填给维护者登记（docs/note/trial-acceptance.md 第 6 节）；"
          "manual_confirm 留空表示有待人工确认的项。")
    return 1 if doc["summary"]["auto_fail"] else 0


MANUAL_HINTS = {
    "text": "回答内容是否切题、语气是否正常（自动判定只看退出码、非空输出与 200 用量行）",
    "tools": "CLI 输出里是否真的发生了工具调用（新建并读回 trial-tool.txt）",
    "stream": "答案是否逐段到达而非一次性出现；中途有没有断流或错误事件",
    "cancel": "（如你自己重跑过）Ctrl+C 后 CLI 是否立即退出、没有继续计费",
    "fallback": "控制台日志页是否能看到同一请求落在第二渠道、且没有落在已停用的第一渠道",
}


def usage_summary(r):
    return {"id": r.get("id"), "model": r.get("model"), "upstream_model": r.get("upstream_model"),
            "channel_id": r.get("channel_id"), "channel_name": r.get("channel_name"),
            "inbound": r.get("inbound"), "outbound": r.get("outbound"),
            "status": r.get("status"), "error": r.get("error"),
            "input_tokens": r.get("input_tokens"), "output_tokens": r.get("output_tokens"),
            "estimated": r.get("estimated"), "degraded": r.get("degraded"),
            "ttft_ms": r.get("ttft_ms"), "duration_ms": r.get("duration_ms")}


def auto_eval(name, runs, rows, channel_of, bindings):
    ok_rows = [r for r in rows if r.get("status") == 200]
    if name == "text":
        missing = []
        if not runs or runs[-1].get("exit_code") != 0:
            missing.append("退出码 %s" % (runs[-1].get("exit_code") if runs else None))
        if runs and not (runs[-1].get("stdout") or "").strip():
            missing.append("客户端输出为空")
        if not ok_rows:
            missing.append("没有 200 用量行")
        return (not missing), ("；".join(missing) if missing else
                              "退出码 0、输出非空、%d 条 200 用量行" % len(ok_rows))
    if name == "tools":
        if len(runs) < 2:
            return None, "两轮命令没有都执行"
        if len(runs) < 2 or runs[0].get("exit_code") != 0 or runs[1].get("exit_code") != 0:
            return False, "有一轮退出码非 0（%s, %s）" % (runs[0].get("exit_code"), runs[1].get("exit_code"))
        if len(ok_rows) < 2:
            return False, "只有 %d 条 200 用量行，期望两轮各至少一条" % len(ok_rows)
        channels = {r.get("channel_id") for r in ok_rows}
        return (len(channels) == 1), ("两轮都 200；命中渠道 %s（亲和%s）"
                                      % (sorted(channels), "命中" if len(channels) == 1 else "未命中"))
    if name == "stream":
        if not runs or runs[-1].get("exit_code") != 0:
            return False, "退出码 %s" % (runs[-1].get("exit_code") if runs else None)
        if not ok_rows:
            return False, "没有 200 用量行"
        if not any(r.get("output_tokens", 0) > 0 for r in ok_rows):
            return False, "用量行没有输出 token，流可能被截断"
        return True, "退出码 0、%d 条 200 用量行且有输出 token" % len(ok_rows)
    if name == "cancel":
        cancelled = [r for r in rows if r.get("status") == 499]
        if not cancelled:
            return False, ("没有 499 用量行；客户端是否真的被中断、是否使用了流式调用？"
                           "（本次 streamed=%s）" % (runs[-1].get("streamed") if runs else None))
        return True, "%d 条 499 用量行（%s）" % (len(cancelled), cancelled[0].get("error"))
    if name == "fallback":
        if len(bindings) < 2:
            return None, "该模型只有一个绑定，无法观察 fallback；换一个多绑定模型"
        first_id = bindings[0].get("channel_id")
        other = {b.get("channel_id") for b in bindings if b.get("channel_id") != first_id}
        on_first = channel_of.get(first_id, 0)
        on_other_ok = len([r for r in ok_rows if r.get("channel_id") in other])
        if not runs or runs[-1].get("exit_code") != 0:
            return False, "退出码 %s" % (runs[-1].get("exit_code") if runs else None)
        if not (runs[-1].get("stdout") or "").strip():
            return False, "客户端输出为空"
        if on_first == 0 and on_other_ok > 0:
            return True, "退出码 0、输出非空、第一渠道 0 命中，其他渠道 %d 条 200 用量行" % on_other_ok
        return False, "第一渠道 %d 命中、其他渠道 %d 条 200 用量行（期望 0 与 >0）" % (on_first, on_other_ok)
    return None, "未知场景"


def evidence_doc(args, origin, model, key_info, cli_path, records, client, cmds, bindings):
    if key_info.get("key"):
        key_source = "demo-created (throwaway)"
    elif any(r.get("skipped") for r in records):
        key_source = "not-supplied (run skipped before a key was needed)"
    else:
        key_source = "operator-supplied (--client-key)"
    auto_pass = len([r for r in records if r.get("auto_pass") is True])
    auto_fail = len([r for r in records if r.get("auto_pass") is False])
    pending = len([r for r in records if r.get("auto_pass") is None])
    return {
        "schema": "to-router/trial-evidence/1",
        "mode": "dry-run" if args.demo else "real",
        "generated_by": "test/acceptance_real_clients.py",
        "started_at": args.started_at,
        "finished_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "gateway": {"origin": origin, "model": model,
                    "bindings": [{"channel_id": b.get("channel_id"),
                                  "channel_name": b.get("channel_name"),
                                  "upstream_model": b.get("model"),
                                  "priority": b.get("priority")} for b in bindings]},
        "client": {"name": args.client, "label": client["label"], "inbound": client["inbound"],
                   "bin": client["bin"], "cli_path": cli_path},
        "client_key": {"source": key_source,
                       "id": key_info.get("client_key", {}).get("id"),
                       "name": key_info.get("client_key", {}).get("name"),
                       "prefix": redact((key_info.get("key") or "")[:8], key_info.get("key")) + "…（已脱敏）"},
        "invocation_templates": {"source": "cmd-file" if args.cmd_file else "builtin",
                                 "note": "内置命令模板未针对你的 CLI 版本验证；参数不符时用 --cmd-file 覆盖"},
        "self_check_precondition": {"confirmed_by_user": bool(args.yes),
                                    "command": "PYTHONUTF8=1 python test/acceptance_real_clients.py"},
        "scenarios": records,
        "summary": {"scenarios": len(records), "auto_pass": auto_pass,
                    "auto_fail": auto_fail, "needs_human": pending,
                    "manual_confirm_pending": len(records)},
        "notes": [
            "auto_pass 只覆盖可机检判据；manual_confirm 需人工填写 true/false 才算该场景闭环。",
            "本文件不含任何完整密钥；客户端 Key 仅留 id/name/前缀。",
        ],
    }


def write_evidence(path, doc):
    # Sanitize every string at the output boundary, including usage errors and
    # metadata. Keep the original run data intact for execution and evaluation.
    def sanitized(value):
        if isinstance(value, str):
            return redact(value, limit=None)
        if isinstance(value, list):
            return [sanitized(item) for item in value]
        if isinstance(value, dict):
            return {redact(k, limit=None): sanitized(v) for k, v in value.items()}
        return value

    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        json.dump(sanitized(doc), f, ensure_ascii=False, indent=2)
    print("evidence: " + os.path.abspath(path))


# --------------------------------------------------------------------------
# demo mode: rehearse the real-mode pipeline end to end
# --------------------------------------------------------------------------

def demo_run(args):
    port = args.port or free_port()
    gw, mocks = None, Mocks()
    try:
        gw = Gateway(port)
        m_a, url_a = mocks.fresh("ok")
        m_b, url_b = mocks.fresh("ok")
        m_s, url_s = mocks.fresh("slow")
        # Two bindings so fallback has a second channel; a second model on a
        # slow upstream so cancel can be interrupted mid-stream.
        c_a = gw.channel("demo-a", "anthropic", url_a, ["demo-model", "demo-cancel"])
        c_b = gw.channel("demo-b", "anthropic", url_b, ["demo-model"])
        c_s = gw.channel("demo-slow", "anthropic", url_s, ["demo-cancel"])
        gw.model("demo-model", [(c_a, "demo-model", 0), (c_b, "demo-model", 1)])
        gw.model("demo-cancel", [(c_s, "demo-cancel", 0)])
        args.url = gw.origin
        args.admin_key = gw.admin_key
        args.client = args.client or "claude-code"
        args.model = args.model or "demo-model"
        args.cancel_model = args.cancel_model or "demo-cancel"
        args.cli_path = args.cli_path or os.path.join(
            os.path.dirname(os.path.abspath(__file__)), "fake_agent_cli.py")
        args.demo = True
        return real_run(args)
    finally:
        if gw is not None:
            gw.stop()
        mocks.shutdown()


# --------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(description="T010 real-client acceptance: self-check and real modes.")
    ap.add_argument("--client", choices=sorted(CLIENTS),
                    help="real mode: which agent CLI to drive (default: self-check mode)")
    ap.add_argument("--url", default="http://127.0.0.1:17878",
                    help="gateway origin for real mode (default %(default)s)")
    ap.add_argument("--admin-key", default=os.environ.get("TO_ROUTER_ADMIN_KEY", ""),
                    help="admin key; falls back to TO_ROUTER_ADMIN_KEY, then a prompt")
    ap.add_argument("--client-key", default=os.environ.get("TO_ROUTER_CLIENT_KEY", ""),
                    help="client API key the CLI authenticates with; create it in the "
                         "console (shown once) or set TO_ROUTER_CLIENT_KEY. The rehearsal "
                         "mode creates a throwaway one instead")
    ap.add_argument("--model", default="", help="downstream model id the client should ask for")
    ap.add_argument("--cancel-model", default="",
                    help="model used by the cancel scenario (default: --model)")
    ap.add_argument("--cli-path", default="", help="explicit CLI path, skipping PATH detection")
    ap.add_argument("--cmd-file", default="",
                    help="JSON overriding the built-in command templates, e.g. "
                         '{"codex": {"text": ["codex", "exec", "{prompt}"]}}')
    ap.add_argument("--scenario", default=",".join(SCENARIOS),
                    help="comma subset of: " + ",".join(SCENARIOS))
    ap.add_argument("--timeout", type=int, default=300, help="per-command timeout in seconds")
    ap.add_argument("--stream-seconds", type=float, default=3.0,
                    help="how long a stream runs before the cancel scenario interrupts it")
    ap.add_argument("--first-output-timeout", type=float, default=120,
                    help="seconds to wait for the client's first output byte before "
                         "interrupting anyway (default %(default)s)")
    ap.add_argument("--evidence-dir", default=".local/trial-evidence",
                    help="where evidence JSON and prompt files land (default %(default)s)")
    ap.add_argument("--yes", action="store_true",
                    help="confirm the self-check precondition ran green before a real run")
    ap.add_argument("--port", type=int, default=0,
                    help="self-check/demo port (default: an auto-picked free port)")
    ap.add_argument("--demo-gateway", action="store_true",
                    help="rehearsal: private gateway + mock upstreams + fake client; "
                         "evidence is stamped dry-run and is not acceptance evidence")
    args = ap.parse_args()
    args.started_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    args.scenario = [s.strip() for s in args.scenario.split(",") if s.strip()]
    unknown = [s for s in args.scenario if s not in SCENARIOS]
    if unknown:
        print("FATAL: unknown scenario(s): %s" % ", ".join(unknown))
        return 2
    args.demo = False

    try:
        if args.demo_gateway:
            return demo_run(args)
        if args.client:
            return real_run(args)
        return self_check(args)
    except GatewayUnreachable as e:
        print("FATAL: %s" % redact(str(e)))
        return 2


if __name__ == "__main__":
    sys.exit(main())
