#!/usr/bin/env python3
"""Reproduce the slow tray shutdown.

Starts the server, optionally holds a keep-alive connection the way a browser
does, asks it to shut down through the console API, and measures how long the
process takes to actually exit and release the port.

  variant A: no client connection open at shutdown time
  variant B: one keep-alive connection left open (browser tab closed but the
             browser process still alive)

Run: PYTHONUTF8=1 python test/repro_shutdown.py
"""
import http.client
import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

PORT = 19901


def call(origin, path, body=None, method="GET", headers=None, timeout=30):
    req = urllib.request.Request(origin + path, data=json.dumps(body).encode() if body is not None else None, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()


def wait_port(origin, timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        try:
            st, _, _ = call(origin, "/api/health", timeout=2)
            if st in (200, 401):
                return True
        except Exception:
            time.sleep(0.1)
    return False


def run_variant(name, hold_connection):
    data = tempfile.mkdtemp(prefix="tr-shut-")
    exe = os.path.join(data, "ToRouter.exe")
    subprocess.run(["go", "build", "-tags", "production", "-trimpath", "-ldflags", "-s -w -H windowsgui" if os.name == "nt" else "-s -w", "-o", exe, "./cmd/torouter"], check=True)
    got = subprocess.run([exe, "-mode", "cli", "-data", data, "-show-key"], capture_output=True, text=True, timeout=60)
    key = got.stdout.strip()
    origin = "http://127.0.0.1:%d" % PORT
    proc = subprocess.Popen([exe, "-mode", "daemon", "-data", data, "-port", str(PORT)],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    try:
        if not wait_port(origin):
            print("%s: FATAL server never came up" % name)
            return
        st, hdr, body = call(origin, "/api/auth/login", {"key": key, "remember": False}, "POST", {"X-To-Router": "1"})
        cookie = hdr.get("Set-Cookie", "").split(";")[0]
        H = {"X-To-Router": "1", "Cookie": cookie, "Content-Type": "application/json"}

        keep = None
        if hold_connection:
            keep = http.client.HTTPConnection("127.0.0.1", PORT, timeout=30)
            keep.request("GET", "/api/health")
            keep.getresponse().read()
            print("%s: keep-alive connection open (browser-style)" % name)

        started = time.time()
        st, _, _ = call(origin, "/api/shutdown", method="POST", headers=H, timeout=30)
        responded = time.time() - started
        # Wait for the process to actually be gone.
        while proc.poll() is None and time.time() - started < 40:
            time.sleep(0.05)
        exited = time.time() - started
        # And for the port to be free again.
        freed = None
        while time.time() - started < 40:
            s = socket.socket()
            s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                s.bind(("127.0.0.1", PORT))
                s.close()
                freed = time.time() - started
                break
            except OSError:
                s.close()
                time.sleep(0.05)
        print("%s: HTTP responded in %.2fs, process exited in %.2fs (rc=%s), port free in %s"
              % (name, responded, exited, proc.returncode,
                 ("%.2fs" % freed) if freed else ">40s"))
        if keep:
            keep.close()
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()


if __name__ == "__main__":
    run_variant("A no-connection", False)
    time.sleep(1)
    run_variant("B keep-alive  ", True)
