#!/usr/bin/env python3
"""Check a built console exe serves the current dist from isolated local data.

This proves embedded frontend freshness over HTTP, not browser rendering or
compatibility with real upstream accounts. Build first with build.ps1 -OutputDir.
"""
import argparse
import hashlib
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--exe", type=Path, required=True)
    parser.add_argument("--dist", type=Path, default=Path(__file__).resolve().parents[1] / "web" / "dist")
    args = parser.parse_args()
    exe = args.exe.resolve(strict=True)
    dist = args.dist.resolve(strict=True)
    assets = {file.relative_to(dist).as_posix(): file.read_bytes() for file in dist.rglob("*") if file.is_file()}
    assert "index.html" in assets, "dist/index.html missing"
    javascript = b"\n".join(body for name, body in assets.items() if name.endswith(".js"))
    for marker in ("转换损失原因", "单渠道尝试次数", "服务已恢复"):
        assert marker.encode("utf-8") in javascript, f"current frontend marker missing: {marker}"
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    origin = f"http://127.0.0.1:{port}"
    with tempfile.TemporaryDirectory(prefix="torouter-assets-data-", ignore_cleanup_errors=True) as data:
        process = subprocess.Popen([str(exe), "-port", str(port), "-data", data],
                                   stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
        try:
            deadline = time.monotonic() + 20
            while True:
                if process.poll() is not None:
                    raise RuntimeError(f"isolated process exited before serving assets: {process.returncode}")
                try:
                    with urllib.request.urlopen(origin, timeout=1) as response:
                        assert response.read() == assets["index.html"], "embedded index differs from current dist"
                    break
                except (urllib.error.URLError, TimeoutError):
                    if time.monotonic() >= deadline:
                        raise RuntimeError("isolated process did not become ready")
                    time.sleep(0.1)
            for name, expected in sorted(assets.items()):
                with urllib.request.urlopen(f"{origin}/{name}", timeout=3) as response:
                    actual = response.read()
                assert actual == expected, f"embedded asset differs: {name}"
                print(f"PASS embedded {name} sha256={hashlib.sha256(actual).hexdigest()}")
            print(f"PASS {len(assets)} current assets served by isolated built exe; all 3 UI markers present")
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


if __name__ == "__main__":
    main()
