"""Catalog integration acceptance using a real binary and isolated local upstreams.

No paid provider calls or existing user data. --exe selects an already-built unified executable.
Covers T084 global-only defaults and read-only public specifications as well as
existing catalog, billing and backfill behavior.
"""
import argparse
import http.cookiejar
import hashlib
import json
import math
import os
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Upstream:
    def __init__(self):
        self.fail = False
        self.usage = None
        self.requests = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
                owner.requests.append(body)
                self.send_response(429 if owner.fail else 200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                payload = {"error": {"message": "local retry"}} if owner.fail else {
                    "id": "local-test", "object": "chat.completion", "model": body.get("model"),
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
                    "usage": owner.usage if owner.usage is not None else {"prompt_tokens": 120, "completion_tokens": 8, "prompt_tokens_details": {"cached_tokens": 20}},
                }
                self.wfile.write(json.dumps(payload).encode())

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()


def price(channel, model, rate):
    return {"channel_id": channel, "model": model, "display_name": model, "origin": "manual",
            "selection": {"mode": "manual", "table": {"currency": "USD", "unit_tokens": 1000,
                "rates": {"input": rate, "output": rate * 2, "cache_read": 0, "cache_write": None}, "rules": []}}}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--exe", default="ToRouter.exe")
    parser.add_argument("--evidence-file", type=Path)
    parser.add_argument("--hold-seconds", type=int, default=0, help="Keep the isolated UI available after acceptance for browser verification")
    args = parser.parse_args()
    exe = str(Path(args.exe).resolve())
    upstreams = [Upstream(), Upstream()]
    with tempfile.TemporaryDirectory(prefix="torouter-catalog-") as directory:
        flags = subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0
        key_output = subprocess.run([exe, "-mode", "cli", "-data", directory, "-show-key"], capture_output=True, text=True, encoding="utf-8", timeout=30, creationflags=flags)
        match = re.search(r"tr_[A-Za-z0-9_-]+", key_output.stdout)
        assert key_output.returncode == 0 and match, "temporary admin key creation failed"
        admin_key = match.group(0)
        port = free_port()
        origin = f"http://127.0.0.1:{port}"
        proc = subprocess.Popen([exe, "-mode", "daemon", "-data", directory, "-port", str(port)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=flags)
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

        def call(path, body=None, method="GET", client=None, expected=200):
            headers = {"Content-Type": "application/json", "X-To-Router": "1"}
            if client:
                headers["Authorization"] = "Bearer " + client
            request = urllib.request.Request(origin + path, data=None if body is None else json.dumps(body).encode(), headers=headers, method=method)
            try:
                response = opener.open(request, timeout=20)
            except urllib.error.HTTPError as error:
                response = error
            assert response.code == expected, f"{method} {path} expected {expected}, got {response.code}"
            raw = response.read()
            return json.loads(raw) if raw else None

        try:
            for _ in range(100):
                try:
                    call("/api/auth/login", {"key": admin_key}, "POST")
                    break
                except (OSError, AssertionError):
                    assert proc.poll() is None, "isolated server exited"
                    time.sleep(.1)
            else:
                raise AssertionError("isolated server did not become ready")
            client = call("/api/client-keys", {"name": "catalog acceptance"}, "POST", expected=201)["key"]
            settings = call("/api/settings")
            settings.pop("prices", None)
            assert settings["request_defaults_enabled"] is False, "new installations silently enable request defaults"
            assert "config" not in settings, "old global capability/default container remains"
            settings["retries_per_channel"] = 1
            settings["max_attempts"] = 2
            call("/api/settings", settings, "PUT")
            channels = []
            for i, upstream in enumerate(upstreams):
                channels.append(call("/api/channels", {"name": f"local-{i}", "enabled": True,
                    "api_key": "isolated-test-key", "models": [f"up-{i}"], "endpoints": [{"format": "openai_chat_completions", "base_url": upstream.url}]}, "POST"))
                call("/api/model-specs", {"channel_id": channels[-1]["id"], "model": f"up-{i}", "specs": {
                    "output_token_field": "max_tokens" if i == 0 else "max_completion_tokens",
                    "context_window": 128000 if i == 0 else 1000000, "max_output_tokens": 0,
                    "input_modalities": [], "streaming": False}}, "PUT")
            model = {"id": "coding", "display_name": "Acceptance", "enabled": True, "fallback": True,
                "bindings": [{"channel_id": c["id"], "model": f"up-{i}", "priority": i, "enabled": True} for i, c in enumerate(channels)]}
            call("/api/models", model, "POST")
            specs = call("/v1/models/specs?api_format=openai_chat_completions", client=client)["data"][0]
            assert specs["context_length"] is None and len(specs["bindings"]) == 2, "mixed specifications were guessed"
            assert specs["max_output_tokens"] == 0 and specs["streaming"] is False and specs["modalities"]["input"] == [], "known zero/false/empty values became unknown"
            assert "isolated-test-key" not in json.dumps(specs) and upstreams[0].url not in json.dumps(specs), "specs leaked secrets or private URLs"
            admin_specs = call("/api/models/specs?api_format=openai_chat_completions")["data"][0]
            assert admin_specs == specs, "admin and public specs use different rules"
            call("/api/models/coding", {**model, "published_specs": {"context_window": 128000}}, "PUT", expected=400)
            spec_rows = call("/api/model-specs")
            row_b = next(row for row in spec_rows if row["channel_id"] == channels[1]["id"] and row["model"] == "up-1")
            row_b["specs"]["context_window"] = 128000
            payload_b = {key: row_b[key] for key in ["channel_id", "model", "specs"]}
            call("/api/model-specs", payload_b, "PUT")
            assert call("/v1/models/specs", client=client)["data"][0]["context_length"] == 128000, "consistent specs were not derived"
            del payload_b["specs"]["context_window"]
            call("/api/model-specs", payload_b, "PUT")
            assert call("/v1/models/specs", client=client)["data"][0]["context_length"] is None, "unknown binding was ignored"
            # A connection save must not erase separately-managed specifications.
            connection = next(row for row in call("/api/channels") if row["id"] == channels[0]["id"])
            connection.pop("model_configs", None)
            connection.pop("model_catalog_refs", None)
            connection.pop("associated_prices", None)
            connection.pop("has_key", None)
            connection.pop("has_oauth", None)
            connection["name"] = "connection edit preserves specs"
            call(f"/api/channels/{channels[0]['id']}", connection, "PUT")
            row_a = next(row for row in call("/api/model-specs") if row["channel_id"] == channels[0]["id"] and row["model"] == "up-0")
            assert row_a["specs"]["context_window"] == 128000, "connection save erased model specs"
            request = {"model": "coding", "messages": [{"role": "user", "content": "hi"}], "stream": False}
            call("/v1/chat/completions", request, "POST", client)
            assert "max_tokens" not in upstreams[0].requests[-1], "disabled defaults injected a budget"
            assert "max_completion_tokens" not in upstreams[0].requests[-1], "disabled defaults injected an alternate budget"
            history = call("/api/logs?model=coding")[0]
            assert history["actual_cost"] is None and history["official_value"] is None
            prices = [price(0, "coding", 1), price(channels[0]["id"], "up-0", 2), price(channels[1]["id"], "up-1", 4)]
            call("/api/prices", prices, "PUT")
            preview = call("/api/usage/backfill/preview", {"filter": {"model": "coding"}, "amounts": ["actual", "official"], "mode": "current"}, "POST")
            assert preview["calculable_amounts"] == 2
            applied = call("/api/usage/backfill/apply", {**preview["request"], "expected_resolution": preview["expected_resolution"]}, "POST")
            assert applied["applied_amounts"] == 2
            historical = call("/api/logs?model=coding")[0]
            assert historical["actual_pricing"]["method"] == "backfill_current"
            assert math.isclose(historical["actual_cost"], historical["official_value"] * 2)
            saved = (historical["actual_cost"], historical["official_value"])
            settings["request_defaults_enabled"] = True
            settings["request_defaults"] = {"max_output_tokens": 333, "temperature": 0.5}
            call("/api/settings", settings, "PUT")
            call("/v1/chat/completions", request, "POST", client)
            assert upstreams[0].requests[-1]["max_tokens"] == 333
            assert upstreams[0].requests[-1]["temperature"] == 0.5
            call("/v1/chat/completions", {**request, "max_tokens": 7, "temperature": 0, "reasoning": None}, "POST", client)
            last = upstreams[0].requests[-1]
            assert last["max_tokens"] == 7 and last["temperature"] == 0 and last["reasoning"] is None
            upstreams[0].fail = True
            call("/v1/chat/completions", request, "POST", client)
            assert upstreams[1].requests[-1]["max_completion_tokens"] == 333, "fallback did not reapply global defaults for its native field"
            assert "max_tokens" not in upstreams[1].requests[-1], "fallback inherited earlier candidate injection"
            settings["request_defaults_enabled"] = False
            call("/api/settings", settings, "PUT")
            call("/v1/chat/completions", request, "POST", client)
            assert "max_completion_tokens" not in upstreams[1].requests[-1] and "temperature" not in upstreams[1].requests[-1], "turning defaults off retained injection"
            current_prices = call("/api/prices")
            current_prices = [entry for entry in current_prices if not (entry["channel_id"] == 0 and entry["model"] == "coding")]
            current_prices.append(price(0, "coding", 3))
            call("/api/prices", current_prices, "PUT")
            loaded_price = next(entry for entry in call("/api/prices") if entry["channel_id"] == 0 and entry["model"] == "coding")
            assert loaded_price["selection"]["table"]["rates"]["input"] == 3
            logs = call("/api/logs?model=coding")
            retained = next(row for row in logs if row["id"] == historical["id"])
            assert (retained["actual_cost"], retained["official_value"]) == saved, "price edit recalculated historical amounts"
            catalog = call("/api/catalog/models?source=builtin")
            assert len(catalog) == 28 and len({row["provider"] for row in catalog}) == 13
            selected_ids = {row["id"] for row in catalog}
            assert {"gpt-6.1-sol", "gpt-5.6-terra", "step-5-preview", "step-3.7-flash", "kimi-k2.7-code"} <= selected_ids
            assert "doubao-seed-2-1-pro-260915" not in selected_ids
            draft = call("/api/catalog/prefill/preview", {"targets": [{"kind": "upstream", "channel_id": channels[0]["id"], "model": "up-0", "draft": {}, "ref": {"source": "builtin", "id": "gpt-6.1-sol"}}], "groups": ["capabilities"]}, "POST")
            assert draft["patches"][0]["config"]["max_output_tokens"] > 0
            assert len(call("/api/models")) == 1, "draft prefill persisted early"
            # Regional identity survives preview, save, reload and a later batch prefill.
            for suffix, currency, rate in [("china", "CNY", 3), ("overseas", "USD", .435)]:
                ref = {"source": "builtin", "id": "mimo-v2.6-pro", "offering_key": f"xiaomi/{suffix}/realtime"}
                alias = "regional-" + suffix
                preview = call("/api/catalog/prefill/preview", {"targets": [{"kind": "downstream", "model": alias, "draft": {}, "ref": ref}], "groups": ["price"], "price_mode": "manual"}, "POST")
                patch = preview["patches"][0]
                assert patch["catalog_model_ref"] == ref
                table = patch["price"]["selection"]["table"]
                assert table["currency"] == currency and table["rates"]["input"] == rate
                call("/api/models", {"id": alias, "enabled": True, "catalog_model_ref": ref,
                    "bindings": [{"channel_id": channels[1]["id"], "model": "up-1", "enabled": True}]}, "POST")
                call("/api/prices", call("/api/prices") + [patch["price"]], "PUT")
                saved_model = next(row for row in call("/api/models") if row["id"] == alias)
                assert saved_model["catalog_model_ref"] == ref
            batch = call("/api/catalog/prefill/preview", {"targets": [{"kind": "downstream", "model": "regional-" + region} for region in ["china", "overseas"]], "groups": ["price"], "force_overwrite": True}, "POST")
            assert [p["price"]["selection"]["ref"]["offering_key"] for p in batch["patches"]] == ["xiaomi/china/realtime", "xiaomi/overseas/realtime"]

            # Real catalog prices + controlled provider counters, including the 100k/1M
            # regression. No actual vendor request is needed to check normalization/billing.
            usage_upstream = Upstream()
            upstreams.append(usage_upstream)
            usage_channel = call("/api/channels", {"name": "cache acceptance", "enabled": True, "api_key": "isolated-test-key",
                "models": ["kimi-k3", "qwen3.8-max", "step-5-preview"], "endpoints": [{"format": "openai_chat_completions", "base_url": usage_upstream.url}]}, "POST")
            for model_id, offering, quote_key in [("kimi-k3", "moonshot/international/payg", "cache-1h"), ("qwen3.8-max", "qwen/beijing/payg", "cny-explicit-cache-token"), ("step-5-preview", "stepfun/china/standard", "standard-cny")]:
                prices = call("/api/prices")
                prices.append({"channel_id": usage_channel["id"], "model": model_id, "origin": "auto",
                    "selection": {"mode": "catalog", "ref": {"source": "builtin", "id": model_id, "offering_key": offering, "quote_key": quote_key}}})
                call("/api/prices", prices, "PUT")
            cases = [
                ("kimi-100k", "kimi-k3", 100000, 0, 100000, "cache_write_tokens", .6),
                ("kimi-1m", "kimi-k3", 1000000, 0, 1000000, "cache_write_tokens", 6),
                ("qwen-100k", "qwen3.8-max", 100000, 0, 100000, "cache_creation_input_tokens", 1.5),
                ("step-100k", "step-5-preview", 100000, 0, 0, None, .7),
                ("step-1m", "step-5-preview", 1000000, 0, 0, None, 7),
                ("step-hit", "step-5-preview", 100000, 80000, 0, None, .168),
            ]
            for alias, model_id, total, read, write, write_field, amount in cases:
                usage_upstream.usage = {"prompt_tokens": total, "completion_tokens": 0}
                if write_field:
                    usage_upstream.usage["prompt_tokens_details"] = {write_field: write}
                    if model_id == "qwen3.8-max":
                        usage_upstream.usage["prompt_tokens_details"].update({
                            "cache_creation": {"ephemeral_5m_input_tokens": write}, "cache_type": "ephemeral"})
                if read:
                    usage_upstream.usage["cached_tokens"] = read
                call("/api/models", {"id": alias, "enabled": True, "bindings": [{"channel_id": usage_channel["id"], "model": model_id, "enabled": True}]}, "POST")
                call("/v1/chat/completions", {"model": alias, "messages": [{"role": "user", "content": "hi"}]}, "POST", client)
                row = call("/api/logs?model=" + alias)[0]
                assert not row["estimated"]
                assert (row["input_tokens"], row["cache_read_tokens"], row["cache_write_tokens"]) == (total - read - write, read, write), alias
                assert math.isclose(row["actual_cost"], amount), (alias, row["actual_cost"], amount)
                assert row["actual_pricing"]["currency"] == ("USD" if model_id == "kimi-k3" else "CNY")
                assert row["official_value"] is None, "upstream cost leaked into unconfigured API value"
            print("PASS catalog real-process acceptance: global defaults off/on/explicit/fallback, derived mixed/unknown specs, management isolation, backfill, regional pricing, cache billing (Kimi/Qwen/Step 100k/1M)")
            if args.evidence_file:
                args.evidence_file.parent.mkdir(parents=True, exist_ok=True)
                args.evidence_file.write_text(json.dumps({
                    "status": "pass", "exe": exe,
                    "exe_sha256": hashlib.sha256(Path(exe).read_bytes()).hexdigest(),
                    "evidence_kind": "isolated real gateway process with controlled local upstreams",
                    "checks": ["defaults disabled on new store", "global defaults enabled and disabled", "explicit client values preserved",
                        "fallback native field isolation", "mixed and unknown specs", "known zero/false/empty specifications",
                        "admin/public projection parity", "removed declarations rejected", "connection/spec save isolation",
                        "channel/official pricing and historical backfill", "regional selection and batch", "Kimi/Qwen/Step cache usage and rates"],
                    "limits": ["no real provider or OAuth account calls", "browser interaction recorded separately"]
                }, ensure_ascii=False, indent=2)+"\n", encoding="utf-8")
            if args.hold_seconds > 0:
                print("UI_ORIGIN=" + origin, flush=True)
                until = time.monotonic() + args.hold_seconds
                while time.monotonic() < until:
                    time.sleep(.5)

        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
            for upstream in upstreams:
                upstream.close()


if __name__ == "__main__":
    main()
