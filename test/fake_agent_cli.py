#!/usr/bin/env python3
"""A stand-in agent CLI, for rehearsing test/acceptance_real_clients.py.

It implements the smallest slice of Claude Code's print mode that the real
mode drives: -p/--print with a prompt, --model, and (ignored) extra flags.
It reads ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN from the environment, sends
one *streaming* Anthropic Messages request per invocation with a stable
metadata.user_id — so the gateway's session affinity sees a client-shaped
session — and prints the answer as it arrives, then exits 0.

Why it exists: the real-mode pipeline (command templates, environment
injection, output capture, usage-row collection, cancel, channel failover)
should be exercised before the first real run, on machines where no agent CLI
is installed. It is NOT acceptance evidence — no real account, no real
upstream, no real agent behaviour. Acceptance evidence comes from a real CLI
(see docs/note/trial-acceptance.md).

Run:  PYTHONUTF8=1 python test/fake_agent_cli.py -p "hi" --model demo
"""
import json
import os
import sys
import urllib.error
import urllib.request


def parse(argv):
    prompt, model = "hi", "demo"
    i = 0
    while i < len(argv):
        a = argv[i]
        if a in ("-p", "--print") and i + 1 < len(argv):
            prompt = argv[i + 1]
            i += 2
        elif a == "--model" and i + 1 < len(argv):
            model = argv[i + 1]
            i += 2
        else:
            i += 1
    return prompt, model


def main():
    prompt, model = parse(sys.argv[1:])
    base = os.environ.get("ANTHROPIC_BASE_URL", "http://127.0.0.1:17878").rstrip("/")
    key = os.environ.get("ANTHROPIC_AUTH_TOKEN") or os.environ.get("ANTHROPIC_API_KEY") or ""
    body = {"model": model, "max_tokens": 64, "stream": True,
            "metadata": {"user_id": "user_fake_session_trial"},
            "messages": [{"role": "user", "content": prompt}]}
    req = urllib.request.Request(base + "/v1/messages", data=json.dumps(body).encode(),
                                 method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("x-api-key", key)
    req.add_header("anthropic-version", "2023-06-01")
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            for raw in r:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                try:
                    event = json.loads(line[5:].strip())
                except ValueError:
                    continue
                if event.get("type") == "content_block_delta":
                    text = event.get("delta", {}).get("text", "")
                    if text:
                        print(text, end="", flush=True)
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:500]
        print("[fake-agent-cli] upstream HTTP %s: %s" % (e.code, detail), file=sys.stderr)
        return 1
    except Exception as e:  # noqa: BLE001 - a rehearsal shim reports and exits
        print("[fake-agent-cli] request failed: %s" % e, file=sys.stderr)
        return 1
    print()
    print("[fake-agent-cli] done")
    return 0


if __name__ == "__main__":
    sys.exit(main())
