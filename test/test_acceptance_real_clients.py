"""Synthetic-only privacy regressions; no gateway, user keys, or model calls."""
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import acceptance_real_clients as acceptance


class OutputPrivacyTests(unittest.TestCase):
    def setUp(self):
        self.old_secrets = acceptance.Secrets
        acceptance.Secrets = []

    def tearDown(self):
        acceptance.Secrets = self.old_secrets

    def test_redact_honors_explicit_secrets_and_overlapping_values(self):
        acceptance.Secrets = ["synthetic", "synthetic-long-secret"]
        self.assertEqual(acceptance.redact("synthetic-long-secret"), "tr_***REDACTED***")
        self.assertNotIn("q7Z!", acceptance.redact("error q7Z!", "q7Z!"))

    def test_env_summary_masks_short_values_and_secrets_in_urls(self):
        acceptance.Secrets = ["q7Z!"]
        result = acceptance.redact_env({"API_KEY": "q7Z!", "TOKEN": "x",
                                        "BASE_URL": "http://localhost/v1",
                                        "AUTH_URL": "http://localhost/q7Z!"})
        self.assertEqual(result["BASE_URL"], "http://localhost/v1")
        self.assertEqual(result["API_KEY"], "tr_***REDACTED***")
        self.assertEqual(result["TOKEN"], "tr_***REDACTED***")
        self.assertNotIn("q7Z!", json.dumps(result))

    def test_issue_passes_original_arguments_and_environment(self):
        key = "synthetic-client-secret"
        acceptance.Secrets = [key]
        cmd = acceptance.build_cmd(["unused", "--key={key}"], "", "", "", key)
        with patch.object(acceptance.subprocess, "run", return_value=
                          subprocess.CompletedProcess([], 0, "answer", "")) as run:
            result = acceptance.issue(cmd, {"API_KEY": key}, 5, exe=["actual-cli"])
        self.assertEqual(run.call_args.args[0], ["actual-cli", "--key=" + key])
        self.assertEqual(run.call_args.kwargs["env"]["API_KEY"], key)
        self.assertEqual(result["command"], ["actual-cli", "--key=" + key])

    def test_real_pipeline_redacts_console_and_json_but_preserves_calls(self):
        for key in ("q7Z!", "synthetic-client-key-that-is-long"):
            with self.subTest(key_length=len(key)), tempfile.TemporaryDirectory() as tmp:
                admin = "synthetic-admin-secret"
                template = Path(tmp, "commands.json")
                template.write_text(json.dumps({"codex": {
                    "text": ["codex", "--key={key}", "--admin=" + admin],
                    "stream": ["codex", "--key", "{key}"]}}), encoding="utf-8")
                args = SimpleNamespace(
                    url="http://127.0.0.1:1", admin_key=admin, client_key=key,
                    client="codex", cmd_file=str(template), cli_path=sys.executable,
                    model="synthetic-model", demo=True, yes=True,
                    evidence_dir=tmp, scenario=["text", "cancel"],
                    cancel_model="", timeout=5, stream_seconds=0,
                    first_output_timeout=1, started_at="synthetic-time")
                observed = []

                def fake_call(url, *unused, **kwargs):
                    payload = {
                        "/api/auth/login": (200, {}),
                        "/api/connection": (200, {}),
                        "/api/models": (200, [{"id": args.model, "enabled": True,
                                               "bindings": []}]),
                        "/api/client-keys": (201, {"key": key,
                                                    "client_key": {"id": 7, "name": "trial"}}),
                    }
                    status, body = payload["/api/" + url.split("/api/", 1)[1]]
                    return status, {}, json.dumps(body).encode()

                def fake_issue(cmd, env, *unused, **kwargs):
                    observed.append((list(cmd), dict(env)))
                    return {"command": cmd, "env": env, "exit_code": 0,
                            "stdout": "answer " + key, "stderr": "warning " + admin,
                            "error": "synthetic failure " + key + " " + admin,
                            "streamed": True}

                rows = [[{"id": 1, "status": 200}],
                        [{"id": 2, "status": 499, "error": "cancel " + key + " " + admin}]]
                output = io.StringIO()
                with patch.dict(os.environ, {}, clear=True), \
                        patch.object(acceptance, "probe", return_value=(200, "ok")), \
                        patch.object(acceptance, "call", side_effect=fake_call), \
                        patch.object(acceptance, "latest_usage_id", return_value=0), \
                        patch.object(acceptance, "wait_usage", side_effect=rows), \
                        patch.object(acceptance, "issue", side_effect=fake_issue), \
                        contextlib.redirect_stdout(output):
                    self.assertEqual(acceptance.real_run(args), 0)
                evidence = list(Path(tmp).glob("codex-*.json"))
                self.assertEqual(len(evidence), 1)
                raw_json = evidence[0].read_text(encoding="utf-8")
                for secret in (key, admin):
                    self.assertNotIn(secret, output.getvalue())
                    self.assertNotIn(secret, raw_json)
                self.assertIn("--key=" + key, observed[0][0])
                self.assertIn(key, observed[1][0])
                self.assertTrue(all(env["TO_ROUTER_API_KEY"] == key for _, env in observed))
                doc = json.loads(raw_json)
                self.assertEqual(doc["summary"]["auto_pass"], 2)
                self.assertIn("REDACTED", doc["scenarios"][0]["runs"][0]["command"][1])
                self.assertIn("REDACTED", doc["scenarios"][1]["usage_rows"][0]["error"])

    def test_json_boundary_preserves_structure_without_mutating_raw_run(self):
        acceptance.Secrets = ["q7Z!"]
        original = {"runs": [{"command": ["cli", "q7Z!"], "error": None}],
                    "long": "a" * 5000 + "q7Z!"}
        with tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stdout(io.StringIO()):
            path = Path(tmp, "evidence.json")
            acceptance.write_evidence(str(path), original)
            saved = json.loads(path.read_text(encoding="utf-8"))
        self.assertEqual(original["runs"][0]["command"][1], "q7Z!")
        self.assertIsNone(saved["runs"][0]["error"])
        self.assertGreater(len(saved["long"]), 5000)
        self.assertNotIn("q7Z!", json.dumps(saved))


if __name__ == "__main__":
    unittest.main()
