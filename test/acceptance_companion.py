#!/usr/bin/env python3
"""Real local-process companion acceptance; upstreams are deterministic fixtures.

Build normally first, then run:
  python -X utf8 test/acceptance_companion.py --exe PATH
  python -X utf8 test/acceptance_companion.py --exe PATH --preview --evidence-dir DIR

Preview leaves only this script's copied executable, isolated database and
loopback fixture alive. POST JSON {"mode":"work"} to controller /scenario;
modes are idle/work/surf/retry/error/done. /scenario cancels previous fixture
requests. POST {} to /stop exits and cleans up. No browser is opened. Preview
login is saved only in fixture-admin.json, never printed. No real user data,
vendor account, key, or provider compatibility is exercised.
"""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid

NO_WINDOW = getattr(subprocess, "CREATE_NO_WINDOW", 0)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def json_bytes(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def call(origin, path, body=None, method="GET", headers=None, timeout=5):
    request = urllib.request.Request(origin + path, method=method,
                                    data=json_bytes(body) if body is not None else None,
                                    headers=headers or {})
    if body is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, dict(response.headers), response.read()
    except urllib.error.HTTPError as error:
        return error.code, dict(error.headers), error.read()


class Gateway:
    def __init__(self, executable, root):
        self.data = root / "data"
        self.data.mkdir()
        self.exe = root / executable.name
        shutil.copy2(executable, self.exe)
        # The same executable exposes a quiet, redirected key-reading CLI mode.
        result = subprocess.run([str(self.exe), "-mode", "cli", "-data", str(self.data), "-show-key"],
                                capture_output=True, text=True, encoding="utf-8", errors="replace",
                                timeout=20, creationflags=NO_WINDOW)
        self.admin = result.stdout.strip()
        require(result.returncode == 0 and self.admin.startswith("tr_"), "isolated key initialization failed")
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            self.port = listener.getsockname()[1]
        self.origin = f"http://127.0.0.1:{self.port}"
        self.output = (root / "process-output.log").open("wb")
        argv = [str(self.exe), "-mode", "daemon", "-data", str(self.data), "-port", str(self.port)]
        environment = os.environ.copy()
        environment["WEBVIEW2_USER_DATA_FOLDER"] = str(root / "webview-profile")
        self.process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=self.output,
                                        stderr=subprocess.STDOUT, creationflags=NO_WINDOW, env=environment)
        self.headers = {}
        self.client_key = ""

    def ready(self):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            require(self.process.poll() is None, "isolated gateway exited before readiness")
            try:
                status, _, _ = call(self.origin, "/api/health", timeout=.5)
                if status == 401:
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.1)
        else:
            raise AssertionError("isolated gateway did not become ready")
        status, headers, _ = call(self.origin, "/api/auth/login", {"key": self.admin}, "POST", {"X-To-Router": "1"})
        require(status == 200, "real login failed")
        self.headers = {"Cookie": headers.get("Set-Cookie", "").split(";")[0], "X-To-Router": "1"}
        connection = self.request("/api/connection")
        require(connection.get("origin") == self.origin and connection.get("port") == self.port,
                "ready process reports incorrect isolated origin")
        status, _, body = call(self.origin, "/api/client-keys", {"name": "companion-local-fixture"}, "POST", self.headers)
        require(status == 201, "real client key creation failed")
        self.client_key = json.loads(body)["key"]

    def request(self, path, body=None, method="GET"):
        status, _, raw = call(self.origin, path, body, method, self.headers)
        require(status == 200, "management request failed: " + path)
        return json.loads(raw)

    def configure(self, upstream):
        for model, format_name in (("fixture-chat", "openai_chat_completions"), ("fixture-responses", "openai_responses")):
            channel = self.request("/api/channels", {
                "name": "local-companion-" + model, "api_key": "local-fixture-only", "models": [model],
                "endpoints": [{"format": format_name, "base_url": upstream}], "enabled": True,
            }, "POST")
            self.request("/api/models", {
                "id": model, "enabled": True, "fallback": False,
                "bindings": [{"channel_id": channel["id"], "model": model, "enabled": True}],
            }, "POST")

    def snapshot(self):
        return self.request("/api/companion")

    def wait(self, predicate, message, timeout=8):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            snapshot = self.snapshot()
            found = predicate(snapshot)
            if found:
                return found
            time.sleep(.04)
        raise AssertionError(message)

    def close(self):
        # Only the Popen handle for our copied executable is ever terminated.
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        self.output.close()


class Plan:
    def __init__(self, mode="chat", preview=False):
        self.id = uuid.uuid4().hex
        self.mode, self.preview = mode, preview
        self.attempts = 0
        self.expected = bytearray()
        self.cancelled = threading.Event()
        self.input_sent = threading.Event()
        self.text_sent = threading.Event()
        self.usage_sent = threading.Event()
        self.attempt_two = threading.Event()
        self.headers_gate = threading.Event()
        self.text_gate = threading.Event()
        self.usage_gate = threading.Event()
        self.end_gate = threading.Event()
        if mode != "retry":
            self.headers_gate.set()

    def release(self):
        for gate in (self.headers_gate, self.text_gate, self.usage_gate, self.end_gate):
            gate.set()

    def cancel(self):
        self.cancelled.set()
        self.release()


class Job:
    def __init__(self, gateway, plan):
        self.gateway, self.plan = gateway, plan
        self.done = threading.Event()
        self.socket = None
        self.status, self.body, self.error = None, b"", None
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def run(self):
        connection = http.client.HTTPConnection("127.0.0.1", self.gateway.port, timeout=45)
        native = self.plan.mode in ("failed", "incomplete")
        body = {"model": "fixture-responses" if native else "fixture-chat", "stream": True}
        marker = "fixture:" + self.plan.id
        if native:
            body["input"] = marker
        else:
            body["messages"] = [{"role": "user", "content": marker}]
        try:
            connection.request("POST", "/v1/responses" if native else "/v1/chat/completions", json_bytes(body),
                               {"Content-Type": "application/json", "Authorization": "Bearer " + self.gateway.client_key})
            self.socket = connection.sock
            if self.plan.cancelled.is_set():
                self.cancel()
            response = connection.getresponse()
            self.status = response.status
            self.body = response.read()
        except (OSError, http.client.HTTPException) as error:
            self.error = type(error).__name__  # No raw diagnostic text or credentials.
        finally:
            connection.close()
            self.socket = None
            self.done.set()

    def cancel(self):
        sock = self.socket
        if sock is not None:
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            sock.close()
        self.plan.cancel()

    def finished(self):
        require(self.done.wait(10), "fixture client did not finish")
        require(self.error is None and self.status == 200, "fixture client response failed")
        require(self.body == bytes(self.plan.expected), "native stream bytes differ from local fixture")
        return {"bytes": len(self.body), "sha256": hashlib.sha256(self.body).hexdigest()}


class Fixture:
    def __init__(self, gateway):
        self.gateway = gateway
        self.plans = {}
        self.jobs = []
        self.lock = threading.Lock()
        self.stop = threading.Event()
        self.preview_generation = 0
        self.scenario_lock = threading.Lock()
        self.scenario = "idle"
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def reply(self, status, value):
                raw = json_bytes(value)
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_GET(self):
                if self.path == "/state":
                    self.reply(200, {"mode": outer.scenario, "gateway_origin": outer.gateway.origin,
                                     "jobs": len(outer.jobs), "active_fixture_jobs": sum(not job.done.is_set() for job in outer.jobs)})
                else:
                    self.reply(404, {"error": "fixture endpoint not found"})

            def do_POST(self):
                size = int(self.headers.get("Content-Length", "0"))
                if size > 1024 * 1024:
                    self.reply(413, {"error": "fixture body too large"})
                    return
                try:
                    payload = json.loads(self.rfile.read(size))
                except (ValueError, TypeError):
                    self.reply(400, {"error": "JSON required"})
                    return
                if self.path in ("/scenario", "/stop"):
                    if not self.headers.get("Content-Type", "").startswith("application/json"):
                        self.reply(415, {"error": "application/json required"})
                        return
                    if self.path == "/stop":
                        self.reply(200, {"stopping": True})
                        outer.stop.set()
                        return
                    mode = payload.get("mode")
                    if mode not in ("idle", "work", "surf", "retry", "error", "done"):
                        self.reply(400, {"error": "unknown scenario"})
                        return
                    outer.select_scenario(mode)
                    self.reply(200, {"mode": mode, "driven_by": "real local gateway requests"})
                    return
                marker = payload.get("input")
                if not isinstance(marker, str):
                    marker = next((m.get("content", "") for m in payload.get("messages", []) if m.get("role") == "user"), "")
                with outer.lock:
                    plan = outer.plans.get(marker.removeprefix("fixture:")) if isinstance(marker, str) else None
                    if plan:
                        plan.attempts += 1
                        attempt = plan.attempts
                if plan is None:
                    self.reply(400, {"error": "unknown fixture request"})
                    return
                try:
                    outer.serve_stream(self, plan, attempt)
                except (OSError, TimeoutError):
                    pass  # Expected when the fixture caller cancels/reset its request.

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.origin = f"http://127.0.0.1:{self.server.server_port}"

    def start(self, mode="chat", preview=False):
        plan = Plan(mode, preview)
        with self.lock:
            self.plans[plan.id] = plan
        job = Job(self.gateway, plan)
        self.jobs.append(job)
        return plan, job

    @staticmethod
    def send(handler, plan, data, event=None):
        raw = (("event: " + event + "\n") if event else "").encode()
        raw += b"data: " + (data if isinstance(data, bytes) else json_bytes(data)) + b"\n\n"
        handler.wfile.write(raw)
        handler.wfile.flush()
        plan.expected.extend(raw)

    def serve_stream(self, handler, plan, attempt):
        if plan.mode == "retry" and attempt == 1:
            raw = json_bytes({"error": {"message": "local fixture retry", "type": "server_error"}})
            handler.send_response(503)
            handler.send_header("Retry-After", "1")
            handler.send_header("Content-Length", str(len(raw)))
            handler.end_headers()
            handler.wfile.write(raw)
            return
        if plan.mode == "retry":
            plan.attempt_two.set()
            if plan.preview:
                if plan.cancelled.wait(25):
                    return
                plan.headers_gate.set()
        if not plan.headers_gate.wait(40) or plan.cancelled.is_set():
            return
        handler.send_response(200)
        handler.send_header("Content-Type", "text/event-stream")
        handler.send_header("Cache-Control", "no-cache")
        handler.end_headers()
        if plan.mode in ("failed", "incomplete"):
            kind = "response." + plan.mode
            response = {"id": "resp_local_fixture", "status": plan.mode, "usage": {"input_tokens": 7, "output_tokens": 11}}
            if plan.mode == "incomplete":
                response["incomplete_details"] = {"reason": "max_output_tokens"}
            else:
                response["error"] = {"code": "server_error", "message": "local fixture failure"}
            self.send(handler, plan, {"type": kind, "response": response}, kind)
            return
        self.send(handler, plan, {"id": "chat_local_fixture", "choices": [], "usage": {"prompt_tokens": 7}})
        plan.input_sent.set()
        if plan.preview:
            if plan.cancelled.wait(.2):
                return
            plan.text_gate.set()
        if not plan.text_gate.wait(40) or plan.cancelled.is_set():
            return
        count = 32 if plan.preview and self.scenario in ("work", "surf") else 3
        for _ in range(count):
            self.send(handler, plan, {"id": "chat_local_fixture", "choices": [{"index": 0, "delta": {"content": "local fixture output " * 3}}]})
            if plan.cancelled.wait(1 if count == 32 else .08):
                return
        plan.text_sent.set()
        if plan.preview:
            plan.usage_gate.set()
        if not plan.usage_gate.wait(40) or plan.cancelled.is_set():
            return
        self.send(handler, plan, {"id": "chat_local_fixture", "choices": [], "usage": {"prompt_tokens": 7, "completion_tokens": 11}})
        plan.usage_sent.set()
        if plan.preview:
            if plan.cancelled.wait(.3):
                return
            plan.end_gate.set()
        if not plan.end_gate.wait(40) or plan.cancelled.is_set():
            return
        self.send(handler, plan, {"id": "chat_local_fixture", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
        self.send(handler, plan, b"[DONE]")

    def cancel_jobs(self):
        for job in self.jobs:
            if not job.done.is_set():
                job.cancel()

    def select_scenario(self, mode):
        with self.scenario_lock:
            self.preview_generation += 1
            generation = self.preview_generation
            self.cancel_jobs()
            self.scenario = mode
            if mode == "idle":
                return
            if mode == "error":
                def repeat_errors():
                    while not self.stop.is_set():
                        with self.scenario_lock:
                            if generation != self.preview_generation:
                                return
                            self.start("incomplete", True)
                        if self.stop.wait(2):
                            break
                threading.Thread(target=repeat_errors, daemon=True).start()
                return
            for _ in range(2 if mode == "surf" else 1):
                self.start("retry" if mode == "retry" else "chat", True)

    def close(self):
        self.stop.set()
        with self.scenario_lock:
            self.preview_generation += 1
            self.cancel_jobs()
        self.server.shutdown()
        self.server.server_close()
        for job in self.jobs:
            job.thread.join(timeout=2)


def active_entry(gateway, message, predicate=lambda entry: True):
    return gateway.wait(lambda snapshot: next((entry for entry in snapshot["active"] if predicate(entry)), None), message)


def completed_once(gateway, start_sequence, count=1, outcome="success"):
    snapshot = gateway.wait(lambda value: value if value["active_count"] == 0 and value["sequence"] >= start_sequence + count else None,
                            "request did not leave active lifecycle")
    fresh = [entry for entry in snapshot["recent"] if entry["sequence"] > start_sequence]
    require(snapshot["sequence"] == start_sequence + count and len(fresh) == count, "request emitted duplicate/missing completion events")
    require(len({entry["id"] for entry in fresh}) == count, "completion identifiers duplicated")
    require(all(entry["outcome"] == outcome for entry in fresh), "incorrect lifecycle outcome")
    return fresh


def run_acceptance(gateway, fixture):
    results = {}
    status, _, _ = call(gateway.origin, "/api/companion")
    require(status == 401, "companion GET accepted unauthenticated caller")
    status, _, _ = call(gateway.origin, "/api/companion/open", {}, "POST", {"Cookie": gateway.headers["Cookie"]})
    require(status == 403, "companion POST accepted missing management header")
    results["authentication"] = {"unauthenticated_get": 401, "post_without_management_header": 403}
    initial = gateway.snapshot()
    require(initial["active_count"] == 0 and initial["sequence"] == 0 and initial["today"]["records"] == 0, "fixture database is not fresh")

    sequence = initial["sequence"]
    plan, job = fixture.start()
    require(plan.input_sent.wait(5), "input-only SSE not delivered")
    input_only = active_entry(gateway, "input-only request not visible", lambda entry: entry["phase"] == "streaming")
    require(input_only["output_tokens"] is None and input_only["tokens_per_second"] is None, "input-only usage invented output tokens/speed")
    plan.text_gate.set()
    require(plan.text_sent.wait(5), "text SSE not delivered")
    estimated = active_entry(gateway, "text estimate/speed not visible",
                             lambda entry: entry["output_tokens_source"] == "estimated" and (entry["tokens_per_second"] or 0) > 0)
    require(estimated["output_tokens"] > 11, "fixture text did not produce calibration-sized estimate")
    plan.usage_gate.set()
    require(plan.usage_sent.wait(5), "usage SSE not delivered")
    reported = active_entry(gateway, "reported usage not visible", lambda entry: entry["output_tokens_source"] == "reported")
    require(reported["output_tokens"] == 11, "reported output did not calibrate estimate to exact value")
    plan.end_gate.set()
    wire = job.finished()
    finished = completed_once(gateway, sequence)[0]
    require(finished["output_tokens"] == 11 and finished["output_tokens_source"] == "reported", "final usage was lost")
    results["stream_lifecycle"] = {"input_only_output": None, "estimated_output": estimated["output_tokens"],
                                   "estimated_tok_s": estimated["tokens_per_second"], "reported_output": 11, "completion_events": 1, **wire}

    sequence = gateway.snapshot()["sequence"]
    pairs = [fixture.start(), fixture.start()]
    for item, _ in pairs:
        require(item.input_sent.wait(5), "parallel fixture did not start")
    concurrent = gateway.wait(lambda value: value if value["active_count"] >= 2 else None, "two active requests not visible")
    for item, _ in pairs:
        item.release()
    wires = [client.finished() for _, client in pairs]
    completed_once(gateway, sequence, 2)
    results["concurrency"] = {"active_count": concurrent["active_count"], "completion_events": 2, "streams": wires}

    sequence = gateway.snapshot()["sequence"]
    plan, job = fixture.start("retry")
    retrying = active_entry(gateway, "retry phase not visible", lambda entry: entry["phase"] == "retrying")
    require(plan.attempt_two.wait(5), "second upstream attempt was not made")
    second = active_entry(gateway, "attempt two not visible", lambda entry: entry["phase"] == "retrying" and entry["attempt"] == 2)
    plan.release()
    wire = job.finished()
    completed_once(gateway, sequence)
    require(plan.attempts == 2, "retry fixture received incorrect upstream attempt count")
    results["retry"] = {"phase": retrying["phase"], "attempt": second["attempt"], "upstream_attempts": plan.attempts, "completion_events": 1, **wire}

    for mode in ("failed", "incomplete"):
        sequence = gateway.snapshot()["sequence"]
        plan, job = fixture.start(mode)
        wire = job.finished()
        terminal = completed_once(gateway, sequence, outcome="error")[0]
        require(terminal["outcome"] != "success", "HTTP 200 native terminal was mistaken for success")
        results["response_" + mode] = {"http_status": job.status, "outcome": terminal["outcome"], "error_code": terminal["error_code"], **wire}

    sequence = gateway.snapshot()["sequence"]
    plan, job = fixture.start()
    require(plan.input_sent.wait(5), "cancellation fixture did not start")
    active_entry(gateway, "cancellation fixture not visible")
    job.cancel()
    cancelled = completed_once(gateway, sequence, outcome="cancelled")[0]
    require(cancelled["outcome"] != "success", "cancelled request emitted success")
    results["cancellation"] = {"outcome": cancelled["outcome"], "completion_events": 1}

    # Usage persistence and the API's 2-second cache refresh are asynchronous.
    expected_attempts = sum(item.attempts for item in fixture.plans.values())
    today = gateway.wait(lambda value: value["today"] if value["today"]["records"] == expected_attempts else None,
                         "today records do not equal actual upstream attempts", timeout=10)
    require(expected_attempts == 8, "fixture produced unexpected number of attempts")
    require(gateway.snapshot()["sequence"] == 7, "logical completion count changed after requests ended")
    results["today"] = {"records": today["records"], "upstream_attempts": expected_attempts, "logical_requests": 7}
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--exe", type=Path, required=True)
    parser.add_argument("--preview", "--serve", action="store_true", dest="preview")
    parser.add_argument("--evidence-dir", type=Path)
    args = parser.parse_args()
    executable = args.exe.resolve(strict=True)
    require(executable.is_file(), "--exe must name a built executable")
    summary = {"evidence": "real local gateway process and local upstream fixtures; no real-provider compatibility claim"}
    gateway = fixture = None
    with tempfile.TemporaryDirectory(prefix="torouter-companion-acceptance-", ignore_cleanup_errors=True) as temporary:
        root = Path(temporary)
        try:
            gateway = Gateway(executable, root)
            gateway.ready()
            fixture = Fixture(gateway)
            gateway.configure(fixture.origin)
            summary["cases"] = run_acceptance(gateway, fixture)
            summary["ok"] = True
            if args.evidence_dir or args.preview:
                evidence = (args.evidence_dir or root / "evidence").resolve()
                evidence.mkdir(parents=True, exist_ok=True)
                (evidence / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
            print(json.dumps(summary, ensure_ascii=False, sort_keys=True), flush=True)
            if args.preview:
                # The only saved key belongs to this run's disposable fixture.
                admin_file = evidence / "fixture-admin.json"
                admin_file.write_text(json.dumps({"origin": gateway.origin, "key": gateway.admin}) + "\n", encoding="utf-8")
                manifest = {"origin": gateway.origin, "pet": gateway.origin + "/#/pet", "management": gateway.origin + "/#/companion", "controller": fixture.origin,
                            "data_directory": str(gateway.data), "process_id": gateway.process.pid,
                            "scenario_endpoint": fixture.origin + "/scenario", "stop_endpoint": fixture.origin + "/stop",
                            "modes": ["idle", "work", "surf", "retry", "error", "done"],
                            "login_fixture_file": str(admin_file), "fixture_scope": "disposable local process/data only",
                            "request_duration_seconds": {"work": 33, "surf": 33, "retry": 26, "done": 1},
                            "error": "real response.incomplete requests repeat every 2 seconds until another scenario or stop"}
                manifest_path = evidence / "manifest.json"
                manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
                print(json.dumps({"preview": manifest, "manifest_file": str(manifest_path)}, ensure_ascii=False), flush=True)
                while not fixture.stop.wait(.5):
                    require(gateway.process.poll() is None, "preview gateway exited")
        except KeyboardInterrupt:
            pass
        except Exception as error:
            # Never print subprocess logs, HTTP response bodies or raw exceptions
            # from libraries; assertion strings above contain only fixed labels.
            summary["ok"] = False
            summary["error_type"] = type(error).__name__
            summary["error"] = str(error) if isinstance(error, AssertionError) else "local fixture setup/request failed"
            print(json.dumps(summary, ensure_ascii=False, sort_keys=True), flush=True)
            return 1
        finally:
            if fixture:
                fixture.close()
            if gateway:
                gateway.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
