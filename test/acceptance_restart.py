#!/usr/bin/env python3
"""Windows real-process restart acceptance, using only disposable local data.

Builds one unified executable into Temp, or copies --bin-dir's ToRouter.exe. A 200
restart response is only acceptance; success also requires parent exit, an
identified live successor, fresh admin auth and the correct /connection port.
Local mock streaming proves drain/persistence, not real-provider compatibility.
No admin/client keys or restart challenge are printed or read from command lines.

Run: PYTHONUTF8=1 python test/acceptance_restart.py [--bin-dir PATH]
Use --only terminal-port stream for a targeted run; --keep-evidence retains the
isolated result/log/data files. Cleanup never targets a user's running instance.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
import traceback
import urllib.error
import urllib.request

REPO = Path(__file__).resolve().parents[1]
NO_WINDOW = getattr(subprocess, "CREATE_NO_WINDOW", 0)
CASES = ("terminal-port", "saved-port", "tray", "stream", "bind-failure")


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def call(origin, path, body=None, method="GET", headers=None, timeout=3):
    request = urllib.request.Request(origin + path, method=method,
                                    data=json.dumps(body).encode() if body is not None else None,
                                    headers=headers or {})
    if body is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, dict(response.headers), response.read()
    except urllib.error.HTTPError as error:
        return error.code, dict(error.headers), error.read()


def powershell(script):
    result = subprocess.run(["powershell", "-NoProfile", "-NonInteractive", "-Command", script],
                            capture_output=True, text=True, encoding="utf-8", errors="replace",
                            timeout=15, creationflags=NO_WINDOW)
    require(result.returncode == 0, "process ownership query/cleanup failed")
    return result.stdout.strip()


def owned_processes(exe, data, terminate=False):
    # Both paths came from this run's unique Temp directory. Get-CimInstance
    # checks them again in the same PowerShell invocation before any Stop-Process.
    # ExecutablePath is always Windows-canonical (backslashes) regardless of how
    # the process was launched; under MSYS Python the incoming path may carry
    # forward slashes, so normalize before the -eq comparison.
    quote = lambda value: "'" + str(value).replace("'", "''") + "'"
    script = "$targetExe=(" + quote(exe) + " -replace '/','\\'); $targetData=" + quote(data) + "; "
    script += ("$owned=@(Get-CimInstance Win32_Process -Filter \"Name='ToRouter.exe'\" "
               "| Where-Object { $_.ExecutablePath -eq $targetExe -and $_.CommandLine -and "
               "$_.CommandLine.IndexOf($targetData, [StringComparison]::OrdinalIgnoreCase) -ge 0 -and "
               "$_.CommandLine -match '(?:^|\\s)-mode\\s+daemon(?:\\s|$)' }); ")
    if terminate:
        script += "$owned | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }; "
    script += "ConvertTo-Json -Compress -InputObject @($owned | Select-Object ProcessId,ParentProcessId)"
    return json.loads(powershell(script) or "[]")


class Gateway:
    def __init__(self, binaries, data, tray, timeout):
        self.data, self.timeout = data, timeout
        self.exe = binaries / "ToRouter.exe"
        self.port = free_port()
        self.ports = {self.port}
        self.parent = None
        self.process_output = None
        self.headers = {}
        self.origin = f"http://127.0.0.1:{self.port}"
        got = subprocess.run([str(self.exe), "-mode", "cli", "-data", str(data), "-show-key"],
                             capture_output=True, text=True, encoding="utf-8", errors="replace",
                             timeout=15, creationflags=NO_WINDOW)
        self.admin = got.stdout.strip()
        require(got.returncode == 0 and self.admin.startswith("tr_"), "isolated admin initialization failed")
        # CLI is only a view; the owning process always runs daemon mode.
        argv = [str(self.exe), "-mode", "daemon", "-data", str(data), "-port", str(self.port)]
        if tray:
            argv.append("-autostart")
        self.process_output = (data / "parent-output.log").open("wb")
        self.parent = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=self.process_output,
                                       stderr=subprocess.STDOUT, creationflags=NO_WINDOW)

    def ready(self, port, starting=False):
        origin = f"http://127.0.0.1:{port}"
        deadline = time.monotonic() + self.timeout
        while time.monotonic() < deadline:
            if starting and self.parent.poll() is not None:
                raise AssertionError(f"initial process exited: {self.parent.returncode}")
            try:
                status, _, _ = call(origin, "/api/health", timeout=0.5)
                if status != 401:
                    time.sleep(0.1)
                    continue
                status, headers, _ = call(origin, "/api/auth/login", {"key": self.admin}, "POST", {"X-To-Router": "1"})
                require(status == 200, "admin reauthentication failed")
                auth = {"X-To-Router": "1", "Cookie": headers.get("Set-Cookie", "").split(";")[0]}
                status, _, body = call(origin, "/api/health", headers=auth)
                require(status == 200 and json.loads(body).get("status") == "ok", "authenticated health check failed")
                status, _, body = call(origin, "/api/connection", headers=auth)
                require(status == 200, "connection metadata unavailable")
                connection = json.loads(body)
                require(connection.get("port") == port and connection.get("origin") == origin,
                        "ready process reports the wrong connection port/origin")
                self.origin, self.headers, self.port = origin, auth, port
                return
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                time.sleep(0.1)
        raise AssertionError(f"service did not become authenticated/ready on port {port}")

    def request(self, path, body=None, method="GET"):
        return call(self.origin, path, body, method, self.headers)

    def save_port(self, port):
        self.ports.add(port)
        status, _, body = self.request("/api/runtime/port", {"port": port}, "PUT")
        require(status == 200 and json.loads(body).get("restart_required") is True, "port preference was not accepted")

    def restart(self):
        status, _, body = self.request("/api/restart", {}, "POST")
        require(status == 200 and json.loads(body).get("ok") is True, "restart request was not accepted")

    def successor(self, port):
        code = self.parent.wait(timeout=self.timeout)
        require(code == 0, f"parent failed during restart: exit={code}")
        children = owned_processes(self.exe, self.data)
        require(len(children) == 1 and children[0]["ProcessId"] != self.parent.pid, "no unique live successor for isolated exe/data")
        require(children[0]["ParentProcessId"] == self.parent.pid, "successor is not the launched parent's child")
        self.ready(port)
        after = owned_processes(self.exe, self.data)
        require(after == children, "successor changed or exited during readiness checks")
        return {"parent_pid": self.parent.pid, "parent_exit": code,
                "child_pid": children[0]["ProcessId"], "ready_port": port}

    def logs(self):
        return (self.data / "to-router.log").read_text(encoding="utf-8", errors="replace")

    def database(self):
        paths = list(self.data.glob("*.db"))
        require(len(paths) == 1, "isolated database not uniquely identifiable")
        return sqlite3.connect(paths[0])

    def cleanup(self):
        # Administrative shutdown first; exact temporary executable+data match
        # bounds the fallback and excludes every unrelated user instance.
        matches = owned_processes(self.exe, self.data)
        if matches:
            for port in self.ports:
                try:
                    call(f"http://127.0.0.1:{port}", "/api/shutdown", {}, "POST", self.headers, timeout=1)
                except (urllib.error.URLError, TimeoutError, ConnectionError):
                    pass
            time.sleep(0.7)
        remaining = owned_processes(self.exe, self.data)
        if remaining:
            owned_processes(self.exe, self.data, terminate=True)
        if self.parent is not None:
            self.parent.wait(timeout=5)
        if self.process_output is not None:
            self.process_output.close()
        require(not owned_processes(self.exe, self.data), "isolated process remained after cleanup")


@contextmanager
def gateway(binaries, data, tray, timeout):
    instance = Gateway(binaries, data, tray, timeout)
    try:
        instance.ready(instance.port, starting=True)
        yield instance
    finally:
        instance.cleanup()


class DelayedStream:
    def __init__(self):
        self.release = threading.Event()
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(b'data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}\n\n')
                self.wfile.flush()
                if not outer.release.wait(10):
                    return
                try:
                    self.wfile.write(b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}\n\ndata: [DONE]\n\n')
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


def scenario(name, binaries, data, timeout):
    with gateway(binaries, data, name == "tray", timeout) as service:
        original_port = service.port
        if name in ("terminal-port", "tray"):
            with service.database() as database:
                row = database.execute("SELECT data FROM runtime WHERE id=1").fetchone()
            require(row is None or not json.loads(row[0]).get("port"), "scenario unexpectedly has a saved port")
            service.restart()
            result = service.successor(original_port)
            if name == "tray":
                log = service.logs()
                require(log.count("silent start") >= 2 and "console opened on start" not in log,
                        "tray start/restart was not logged as silent")
                result["silent_start_count"] = log.count("silent start")
            return result
        if name == "saved-port":
            new_port = free_port()
            while new_port == original_port:
                new_port = free_port()
            service.save_port(new_port)
            service.restart()
            result = service.successor(new_port)
            with socket.socket() as sock:
                sock.settimeout(0.3)
                require(sock.connect_ex(("127.0.0.1", original_port)) != 0, "old port remained listening")
            result["original_port"] = original_port
            return result
        if name == "bind-failure":
            blocker = ThreadingHTTPServer(("127.0.0.1", 0), Blocker)
            blocker.daemon_threads = True
            threading.Thread(target=blocker.serve_forever, daemon=True).start()
            try:
                service.save_port(blocker.server_port)
                service.restart()
                code = service.parent.wait(timeout=timeout)
                require(code != 0, "parent falsely reported successful relaunch onto occupied port")
                require("restart failed" in service.logs(), "relaunch failure was not recorded in to-router.log")
                require(not owned_processes(service.exe, service.data), "failed restart left a child running")
                return {"parent_exit": code, "restart_request_accepted": True, "failure_logged": True}
            finally:
                blocker.shutdown()
                blocker.server_close()
        return stream_scenario(service, original_port)


class Blocker(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        self.send_response(503)
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_POST = do_GET


def stream_scenario(service, port):
    upstream = DelayedStream()
    first_received = threading.Event()
    try:
        status, _, body = service.request("/api/client-keys", {"name": "restart-acceptance"}, "POST")
        require(status == 201, "isolated client key creation failed")
        client_key = json.loads(body)["key"]
        status, _, body = service.request("/api/channels", {
            "name": "local-delayed-stream", "api_key": "isolated-mock-key", "models": ["restart-stream"],
            "endpoints": [{"format": "openai_chat_completions", "base_url": f"http://127.0.0.1:{upstream.server.server_port}"}],
            "enabled": True}, "POST")
        require(status == 200, "local mock channel creation failed")
        channel_id = json.loads(body)["id"]
        status, _, _ = service.request("/api/models", {"id": "restart-stream", "enabled": True, "fallback": False,
                "bindings": [{"channel_id": channel_id, "model": "restart-stream", "enabled": True}]}, "POST")
        require(status == 200, "local mock model creation failed")

        def read_stream():
            request = urllib.request.Request(service.origin + "/v1/chat/completions", method="POST",
                data=json.dumps({"model": "restart-stream", "stream": True, "messages": [{"role": "user", "content": "hi"}]}).encode(),
                headers={"Authorization": "Bearer " + client_key, "Content-Type": "application/json"})
            with urllib.request.urlopen(request, timeout=12) as response:
                first = response.readline()
                if b'"hi"' in first:
                    first_received.set()
                return response.status, first + response.read()

        with ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(read_stream)
            try:
                require(first_received.wait(5), "client did not receive a first stream chunk")
                service.restart()
                # Past the 400ms API handoff delay, but below the shutdown grace:
                # the stream remains blocked while actual drain has begun.
                time.sleep(0.8)
                require(service.parent.poll() is None and not future.done(), "restart closed the in-flight stream before release")
            finally:
                upstream.release.set()
            status, stream = future.result(timeout=10)
            require(status == 200 and b"data: [DONE]" in stream, "client did not receive complete stream through restart")
        service.parent.wait(timeout=service.timeout)
        with service.database() as database:
            database_rows = database.execute("SELECT id,data FROM usage_logs WHERE model='restart-stream'").fetchall()
        require(len(database_rows) == 1, "completed in-flight stream was not persisted across parent exit")
        persisted = json.loads(database_rows[0][1])
        require(persisted["status"] == 200 and not persisted.get("error") and
                persisted["input_tokens"] == 7 and persisted["output_tokens"] == 3,
                "durable stream row does not match the complete response")
        result = service.successor(port)
        status, _, body = service.request("/api/logs?limit=10&model=restart-stream")
        require(status == 200, "successor cannot read persisted usage logs")
        rows = json.loads(body)
        require(len(rows) == 1, "successor did not expose exactly one usage row")
        row = rows[0]
        require(row["status"] == 200 and not row.get("error") and row["input_tokens"] == 7 and row["output_tokens"] == 3,
                "persisted stream status/tokens do not match completed upstream usage")
        require(row["id"] == database_rows[0][0], "successor's usage API row differs from the durable row")
        result.update({"stream_complete": True, "usage_rows": 1, "input_tokens": 7, "output_tokens": 3})
        return result
    finally:
        upstream.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--bin-dir", type=Path)
    parser.add_argument("--only", nargs="+", choices=CASES)
    parser.add_argument("--timeout", type=float, default=30)
    parser.add_argument("--keep-evidence", action="store_true")
    args = parser.parse_args()
    require(os.name == "nt", "this acceptance script targets the Windows terminal/tray shells")
    require(args.timeout > 0, "timeout must be positive")
    root = Path(tempfile.mkdtemp(prefix="torouter-restart-acceptance-")).resolve()
    binaries = root / "bin"
    binaries.mkdir()
    results = {}
    try:
        output = binaries / "ToRouter.exe"
        if args.bin_dir:
            shutil.copy2(args.bin_dir.resolve(strict=True) / "ToRouter.exe", output)
        else:
            built = subprocess.run(["go", "build", "-tags", "production", "-trimpath", "-ldflags", "-s -w -H windowsgui", "-o", str(output), "./cmd/torouter"],
                                   cwd=REPO, capture_output=True, timeout=120, creationflags=NO_WINDOW)
            require(built.returncode == 0, "isolated unified executable compilation failed")
        print("Binaries prepared in unique Temp directory; no user executable overwritten.", flush=True)
        for name in args.only or CASES:
            try:
                result = scenario(name, binaries, root / name, args.timeout)
                # scenario's context manager verifies cleanup before returning.
                result["cleanup_verified"] = True
                results[name] = {"passed": True, **result}
                print("PASS " + name + " " + json.dumps(result, sort_keys=True), flush=True)
            except Exception as error:
                # Deliberately omit raw HTTP bodies, output/command lines and keys.
                frames = traceback.extract_tb(error.__traceback__)
                location = " > ".join(f"{frame.name}:{frame.lineno}" for frame in frames if Path(frame.filename).resolve() == Path(__file__).resolve())
                detail = str(error) if isinstance(error, (AssertionError, subprocess.TimeoutExpired)) else type(error).__name__ + " at " + location
                results[name] = {"passed": False, "reason": detail}
                print("FAIL " + name + " " + detail, flush=True)
        (root / "result.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
        passed = sum(result["passed"] for result in results.values())
        print(f"{passed}/{len(results)} restart scenarios passed.", flush=True)
        if args.keep_evidence:
            print("Evidence directory: " + str(root), flush=True)
        return 0 if passed == len(results) else 1
    finally:
        if not args.keep_evidence:
            # root was created by this invocation; verify its absolute boundary.
            require(root.parent == Path(tempfile.gettempdir()).resolve() and root.name.startswith("torouter-restart-acceptance-"),
                    "refusing cleanup outside this run's temporary root")
            shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
