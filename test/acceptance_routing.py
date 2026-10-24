#!/usr/bin/env python3
"""Isolated real-process acceptance for the routing-policy work (T013-T017).

Builds the console binary into a temp directory, starts it against a throwaway
data directory and a private port, points it at local mock upstreams, then
verifies on the wire:

  S1  same-channel retries happen before any cross-channel failover (T016)
  S2  a stateful request with no pin is refused (409) and no upstream is
      contacted; a client that names its session is pinned from turn one and
      every later turn lands on the same channel
  S3  turning fallback off really stops after the first binding (T015)
  S4  a stripped reasoning carrier is reported by header, and neither the
      reasoning nor the answer is lost (T014)
  S5  retries_per_channel round-trips through the console API (T016)
  S6  a cross-protocol tool call to Gemini 3 carries the provider documented
      dummy signature, and a 2.x target carries none

Notes for whoever runs this next:
  * The binary in the repository root is NOT executed directly. On this machine
    the sandbox stalls executables launched from the workspace, which looks
    exactly like a product hang; building into the temp dir avoids it.
  * -show-key prints the key and exits. The server is a second invocation.
  * Every /api/ route needs an admin session, so a 401 from /api/health is a
    perfectly good sign that the server is listening.
  * The breaker is per channel and opens after three consecutive failures, so
    observed retry counts are min(retries, 3), not retries. Scenarios that
    assert raw counts therefore use a brand-new channel each time.

Run:  PYTHONUTF8=1 python test/acceptance_routing.py [--port 19890]
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

passed, failed = [], []


def check(name, cond, detail=""):
    (passed if cond else failed).append(name)
    print(("  PASS " if cond else "  FAIL ") + name + ((" :: " + str(detail)) if detail and not cond else ""))


class Mock:
    """Configurable upstream. mode: 429 | ok | thinking"""

    def __init__(self, mode):
        self.mode = mode
        self.hits = 0
        self.last_body = ""

    def serve(self):
        mock = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                mock.hits += 1
                n = int(self.headers.get("content-length", 0))
                mock.last_body = self.rfile.read(n).decode("utf-8", "replace")
                if mock.mode == "429":
                    self.send_response(429)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                if mock.mode == "thinking":
                    out = json.dumps({
                        "id": "m", "type": "message", "role": "assistant", "model": "c",
                        "content": [
                            {"type": "thinking", "thinking": "hidden reasoning", "signature": "sig"},
                            {"type": "text", "text": "answer"}],
                        "stop_reason": "end_turn",
                        "usage": {"input_tokens": 3, "output_tokens": 4}}).encode()
                elif "/v1beta/models/" in self.path:
                    out = json.dumps({"candidates": [{"content": {"role": "model", "parts": [{"text": "ok"}]},
                                                      "finishReason": "STOP"}],
                                      "usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 2}}).encode()
                else:
                    out = json.dumps({"choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"},
                                                   "finish_reason": "stop"}],
                                      "usage": {"prompt_tokens": 5, "completion_tokens": 2}}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)

        return ThreadingHTTPServer(("127.0.0.1", 0), H)


def call(url, body=None, method="GET", headers=None, timeout=60):
    req = urllib.request.Request(url, data=json.dumps(body).encode() if body is not None else None, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=19890)
    args = ap.parse_args()
    origin = "http://127.0.0.1:%d" % args.port

    data = tempfile.mkdtemp(prefix="tr-accept-")
    exe = os.path.join(data, "ToRouter.exe")
    subprocess.run(["go", "build", "-tags", "production", "-trimpath", "-ldflags", "-s -w -H windowsgui" if os.name == "nt" else "-s -w", "-o", exe, "./cmd/torouter"], check=True)

    servers, mocks = [], []

    def fresh(mode):
        m = Mock(mode)
        srv = m.serve()
        servers.append(srv)
        mocks.append(m)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        return m, "http://127.0.0.1:%d" % srv.server_address[1]

    def total_hits():
        return sum(m.hits for m in mocks)

    got = subprocess.run([exe, "-mode", "cli", "-data", data, "-show-key"], capture_output=True, text=True,
                         encoding="utf-8", errors="replace", timeout=60,
                         creationflags=subprocess.CREATE_NO_WINDOW)
    key = got.stdout.strip().splitlines()[-1].strip() if got.stdout.strip() else ""
    if not key.startswith("tr_"):
        print("FATAL: no admin key: rc=%s out=%r err=%r" % (got.returncode, got.stdout, got.stderr))
        return 1
    print("admin key: " + key[:8] + "...")

    proc = subprocess.Popen([exe, "-mode", "daemon", "-data", data, "-port", str(args.port)],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                            encoding="utf-8", errors="replace", creationflags=subprocess.CREATE_NO_WINDOW)
    try:
        for _ in range(100):
            if proc.poll() is not None:
                print("FATAL: server exited early rc=%s" % proc.returncode)
                print(proc.stdout.read())
                return 1
            try:
                st, _, _ = call(origin + "/api/health", timeout=3)
                if st in (200, 401):
                    break
            except urllib.error.HTTPError as e:
                if e.code in (200, 401):
                    break
                time.sleep(0.2)
            except Exception:
                time.sleep(0.2)
        else:
            print("FATAL: server never became healthy")
            return 1

        st, hdr, body = call(origin + "/api/auth/login", {"key": key, "remember": False}, "POST", {"X-To-Router": "1"})
        cookie = hdr.get("Set-Cookie", "").split(";")[0]
        H = {"X-To-Router": "1", "Cookie": cookie, "Content-Type": "application/json"}
        check("login", st == 200, body)

        st, _, body = call(origin + "/api/client-keys", {"name": "accept"}, "POST", H)
        ck = json.loads(body)["key"]
        G = {"Authorization": "Bearer " + ck, "Content-Type": "application/json"}

        formats = {"openai": "openai_chat_completions", "anthropic": "anthropic_messages",
                   "gemini": "gemini_generate_content", "responses": "openai_responses"}

        def channel(name, proto, url, models):
            st, _, b = call(origin + "/api/channels",
                            {"name": name, "api_key": "upstream-secret",
                             "endpoints": [{"format": formats[proto], "base_url": url}],
                             "models": models, "enabled": True}, "POST", H)
            if st != 200:
                print("  channel save failed:", st, b)
            return json.loads(b)["id"]

        def raw_channel(name, endpoints, models):
            st, _, b = call(origin + "/api/channels",
                            {"name": name, "api_key": "upstream-secret",
                             "endpoints": endpoints, "models": models, "enabled": True}, "POST", H)
            if st != 200:
                print("  channel save failed:", st, b)
            return json.loads(b)["id"]

        configured_models = set()

        def model(mid, bindings, fallback=True):
            editing = mid in configured_models
            endpoint = "/api/models/" + urllib.parse.quote(mid, safe="") if editing else "/api/models"
            st, _, b = call(origin + endpoint,
                            {"id": mid, "enabled": True, "fallback": fallback,
                             "bindings": [{"channel_id": c, "model": m, "priority": p, "enabled": True}
                                          for c, m, p in bindings]}, "PUT" if editing else "POST", H)
            assert st == 200, ("model save failed", st, b)
            configured_models.add(mid)

        def chat(body):
            return call(origin + "/v1/chat/completions", body, "POST", G)

        USER = [{"role": "user", "content": "hi"}]

        # ---- S1: same-channel retries, then failover
        badA, urlA = fresh("429")
        goodA, urlGA = fresh("ok")
        a = channel("badA", "openai", urlA, ["test"])
        b = channel("goodA", "openai", urlGA + "/v1", ["test"])
        model("test", [(a, "test", 0), (b, "test", 1)])
        before = badA.hits
        st, _, body = chat({"model": "test", "messages": USER})
        check("S1 final status 200", st == 200, body)
        check("S1 first channel retried before failover", badA.hits - before >= 3, badA.hits - before)
        check("S1 exactly one failover happened", goodA.hits == 1, goodA.hits)

        # ---- S2: stateful request with no pin is refused, upstream untouched
        badB, urlB = fresh("429")
        goodB, urlGB = fresh("ok")
        c1 = channel("badB", "openai", urlB, ["test"])
        c2 = channel("goodB", "openai", urlGB + "/v1", ["test"])
        model("test", [(c1, "test", 0), (c2, "test", 1)])
        before = badB.hits + goodB.hits
        st, _, body = chat({"model": "test", "messages": [
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "prev", "reasoning_content": "earlier"}]})
        err = json.loads(body).get("error", {})
        check("S2 refused with 409", st == 409, st)
        check("S2 no upstream contacted", badB.hits + goodB.hits == before, badB.hits + goodB.hits - before)
        check("S2 recovery code", err.get("recovery", {}).get("code") == "state_affinity_unknown", err)
        check("S2 tells how to fix",
              set(err.get("recovery", {}).get("actions", [])) >= {"use_single_binding", "handoff", "start_fresh_session"}, err)

        # ---- S2b: a client that names its session is pinned on turn one, and
        # every later turn lands on the same channel.
        pinA, urlPA = fresh("ok")
        pinB, urlPB = fresh("ok")
        c3 = channel("pinA", "openai", urlPA + "/v1", ["pin"])
        c4 = channel("pinB", "openai", urlPB + "/v1", ["pin"])
        model("pin", [(c3, "pin", 0), (c4, "pin", 1)])

        def pinned_turn(with_state):
            beforeA, beforeB = pinA.hits, pinB.hits
            msg = {"model": "pin", "metadata": {"user_id": "user_hash_session_accept1"},
                   "messages": [{"role": "user", "content": "hi"}]}
            if with_state:
                msg["messages"].append({"role": "assistant", "content": "prev", "reasoning_content": "earlier"})
            st, _, b = chat(msg)
            if st != 200:
                print("FATAL: pinned turn failed: %d %s" % (st, b))
                raise SystemExit(1)
            return pinA.hits - beforeA, pinB.hits - beforeB

        hitA, hitB = pinned_turn(False)
        check("S2b first turn routed once", hitA + hitB == 1, (hitA, hitB))
        ok = True
        for i in range(4):
            hitA, hitB = pinned_turn(True)
            if hitA + hitB != 1:
                ok = False
                break
        check("S2b stateful turns routed once", ok, (hitA, hitB))
        check("S2b all five turns on one channel", pinA.hits == 5 or pinB.hits == 5, (pinA.hits, pinB.hits))

        # ---- S3: fallback off stops after the first binding
        badC, urlC = fresh("429")
        goodC, urlGC = fresh("ok")
        c3 = channel("badC", "openai", urlC, ["test"])
        c4 = channel("goodC", "openai", urlGC + "/v1", ["test"])
        model("test", [(c3, "test", 0), (c4, "test", 1)], fallback=False)
        st, _, body = chat({"model": "test", "messages": USER})
        err = json.loads(body).get("error", {})
        check("S3 no second channel", goodC.hits == 0, goodC.hits)
        check("S3 recovery code fallback_disabled", err.get("recovery", {}).get("code") == "fallback_disabled", err)

        # ---- S4: stripped reasoning is reported
        thinker, urlT = fresh("thinking")
        th = channel("thinker", "anthropic", urlT, ["test"])
        model("stripped", [(th, "test", 0)])
        st, h, body = chat({"model": "stripped", "messages": USER})
        check("S4 missing budget is refused with client hint",
              st == 400 and b"Configure max_tokens explicitly in the client request" in body, body)
        check("S4 missing budget does not reach upstream", thinker.hits == 0, thinker.hits)
        # T084: the client supplies the required target budget; no implicit gateway default.
        st, h, body = chat({"model": "stripped", "messages": USER, "max_tokens": 64})
        check("S4 status 200", st == 200, body)
        check("S4 degraded header", "thinking" in h.get("X-To-Router-Degraded", ""), h.get("X-To-Router-Degraded"))
        check("S4 reasoning not leaked", b"hidden reasoning" not in body, body)
        check("S4 answer kept", b"answer" in body, body)

        # ---- S5: retries_per_channel round-trips
        st, _, body = call(origin + "/api/settings", headers=H)
        s = json.loads(body)
        s.pop("prices", None)
        s["retries_per_channel"] = 2
        st, _, body = call(origin + "/api/settings", s, "PUT", H)
        check("S5 settings saved", st == 200, body)
        st, _, body = call(origin + "/api/settings", headers=H)
        check("S5 retries_per_channel persisted", json.loads(body).get("retries_per_channel") == 2, body[:200])
        badD, urlD = fresh("429")
        goodD, urlGD = fresh("ok")
        c5 = channel("badD", "openai", urlD, ["test"])
        c6 = channel("goodD", "openai", urlGD + "/v1", ["test"])
        model("test", [(c5, "test", 0), (c6, "test", 1)])
        before = badD.hits
        st, _, body = chat({"model": "test", "messages": USER})
        check("S5 configured retry count honored", badD.hits - before == 2, badD.hits - before)
        s["retries_per_channel"] = 5
        call(origin + "/api/settings", s, "PUT", H)

        # ---- S6: cross-protocol tool call to Gemini 3 carries the documented
        # bypass signature instead of being refused; 2.x carries none.
        gm_mock, urlG = fresh("ok")
        gm = channel("gem", "gemini", urlG, ["gemini-3-pro", "gemini-2.5-flash"])
        model("g3", [(gm, "gemini-3-pro", 0)])
        tools = [{"role": "user", "content": "hi"},
                 {"role": "assistant", "tool_calls": [{"id": "c1", "type": "function",
                                                       "function": {"name": "f", "arguments": "{}"}}]}]
        st, _, body = chat({"model": "g3", "messages": tools})
        check("S6 gemini 3 routes", st == 200, (st, body[:200]))
        check("S6 upstream received the bypass signature",
              "skip_thought_signature_validator" in gm_mock.last_body, gm_mock.last_body[:200])
        # And it must sit on the part, not inside functionCall.
        try:
            wire = json.loads(gm_mock.last_body)
            parts = [p for c in wire.get("contents", []) for p in c.get("parts", [])]
            call_parts = [p for p in parts if "functionCall" in p]
            on_part = any("thoughtSignature" in p for p in call_parts)
            in_call = any("thoughtSignature" in p.get("functionCall", {}) for p in call_parts)
            check("S6 signature is on the part, not inside functionCall", on_part and not in_call,
                  gm_mock.last_body[:300])
        except Exception as e:
            check("S6 signature placement", False, e)
        model("g2", [(gm, "gemini-2.5-flash", 0)])
        st, _, body = chat({"model": "g2", "messages": tools})
        check("S6 gemini 2.x routes", st == 200, (st, body[:200]))
        check("S6 no bypass for 2.x", "skip_thought_signature_validator" not in gm_mock.last_body,
              gm_mock.last_body[:200])

        # ---- S7: T019/P0 a channel that only declares Responses produces no
        # candidate for a chat request. The body must never reach /responses.
        resp_only, urlRO = fresh("ok")
        ronly = raw_channel("responses-only", [{"format": formats["responses"], "base_url": urlRO}],
                            ["resp-only"])
        model("resp-only", [(ronly, "resp-only", 0)])
        st, _, body = chat({"model": "resp-only", "messages": USER})
        err = json.loads(body).get("error", {})
        check("S7 responses-only channel is not a chat candidate", st == 503, (st, body[:200]))
        check("S7 nothing sent upstream", resp_only.hits == 0, resp_only.hits)
        check("S7 recovery names the fix",
              err.get("recovery", {}).get("code") == "no_usable_endpoint", err)

        # ---- S8: T019/P0 the endpoint array is display order: a channel that
        # declares both Responses and Messages serves a chat request from its
        # Messages endpoint whichever of the two comes first.
        resp_mock, urlRM = fresh("ok")
        msg_mock, urlMM = fresh("thinking")

        def mixed(name, first, model_id):
            endpoints = [{"format": formats["responses"], "base_url": urlRM},
                         {"format": formats["anthropic"], "base_url": urlMM}]
            if first == "anthropic":
                endpoints.reverse()
            return raw_channel(name, endpoints, [model_id])

        model("mixed", [(mixed("mixedA", "responses", "mixed"), "mixed", 0)])
        st, _, body = chat({"model": "mixed", "messages": USER, "max_tokens": 64})
        check("S8 chat request served", st == 200, (st, body[:200]))
        check("S8 responses endpoint untouched", resp_mock.hits == 0, resp_mock.hits)
        check("S8 messages endpoint used", msg_mock.hits == 1, msg_mock.hits)
        model("mixed2", [(mixed("mixedB", "anthropic", "mixed2"), "mixed2", 0)])
        st, _, body = chat({"model": "mixed2", "messages": USER, "max_tokens": 64})
        check("S8 reversed array order picks the same endpoint",
              st == 200 and msg_mock.hits == 2 and resp_mock.hits == 0,
              (st, msg_mock.hits, resp_mock.hits, body[:200]))

        print("\n%d passed, %d failed" % (len(passed), len(failed)))
        if failed:
            print("FAILED: " + ", ".join(failed))
        return 1 if failed else 0
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        for srv in servers:
            srv.shutdown()
        print("server exit code: %s" % proc.returncode)


if __name__ == "__main__":
    sys.exit(main())
