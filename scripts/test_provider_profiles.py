import importlib.util
import json
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("check-provider-profiles.py")
SPEC = importlib.util.spec_from_file_location("check_provider_profiles", SCRIPT)
CHECKER = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(CHECKER)


def source(path="src/profile.go", content=b"profile"):
    import hashlib

    return {
        "repo": "example/provider",
        "tracking_ref": "main",
        "baseline_commit": "1" * 40,
        "files": [{"path": path, "sha256": hashlib.sha256(content).hexdigest()}],
    }


def manifest(providers=None):
    return {"schema_version": 1, "providers": providers or [
        {"id": "codex", "implementation": "internal/codex/profile.go", "sources": [source()]}
    ]}


def fake_api(commit="2" * 40, file_values=None):
    if file_values is None:
        file_values = {"src/profile.go": b"profile"}

    def get(url, timeout=CHECKER.TIMEOUT_SECONDS):
        if "/commits/" in url:
            return json.dumps({"sha": commit}).encode()
        path = url.split(commit + "/", 1)[1]
        if path not in file_values:
            raise CHECKER.UpstreamFileMissing(url)
        value = file_values[path]
        if isinstance(value, Exception):
            raise value
        return value

    return get


class ProviderProfileCheckerTests(unittest.TestCase):
    def test_same_content_at_new_commit_is_unchanged(self):
        with patch.object(CHECKER, "get_url_bytes", side_effect=fake_api(commit="2" * 40)):
            lines, status = CHECKER.check_upstream(manifest())
        self.assertEqual(status, 0)
        self.assertTrue(all(line.startswith("UNCHANGED") for line in lines))

    def test_content_change_is_reported(self):
        with patch.object(CHECKER, "get_url_bytes", side_effect=fake_api(file_values={"src/profile.go": b"updated"})):
            lines, status = CHECKER.check_upstream(manifest())
        self.assertEqual(status, 2)
        self.assertTrue(any(line.startswith("CHANGED") for line in lines))

    def test_deleted_file_is_changed(self):
        with patch.object(CHECKER, "get_url_bytes", side_effect=fake_api(file_values={})):
            lines, status = CHECKER.check_upstream(manifest())
        self.assertEqual(status, 2)
        self.assertIn("CHANGED", lines[0])
        self.assertIn("missing at", lines[0])

    def test_network_failure_is_unavailable(self):
        with patch.object(CHECKER, "get_url_bytes", side_effect=CHECKER.UpstreamUnavailable("network error")):
            lines, status = CHECKER.check_upstream(manifest())
        self.assertEqual(status, 1)
        self.assertTrue(all(line.startswith("UNAVAILABLE") for line in lines))

    def test_mixed_changed_and_unavailable_returns_error(self):
        src = source()
        src["files"].append({"path": "src/other.go", "sha256": "0" * 64})
        values = {"src/profile.go": b"changed", "src/other.go": CHECKER.UpstreamUnavailable("rate limited")}
        with patch.object(CHECKER, "get_url_bytes", side_effect=fake_api(file_values=values)):
            lines, status = CHECKER.check_upstream(manifest([{
                "id": "codex", "implementation": "internal/codex/profile.go", "sources": [src]
            }]))
        self.assertEqual(status, 1)
        self.assertTrue(any(line.startswith("CHANGED") for line in lines))
        self.assertTrue(any(line.startswith("UNAVAILABLE") for line in lines))

    def test_duplicate_provider_id_is_invalid(self):
        data = manifest()
        data["providers"].append(data["providers"][0])
        with self.assertRaises(CHECKER.ManifestError):
            CHECKER.validate_manifest(data)

    def test_provider_filter_limits_upstream_requests(self):
        data = manifest([
            {"id": "codex", "implementation": "internal/codex/profile.go", "sources": [source()]},
            {"id": "claude", "implementation": "internal/claude/profile.go", "sources": [source()]},
        ])
        with patch.object(CHECKER, "get_url_bytes", side_effect=fake_api()) as fetch:
            lines, status = CHECKER.check_upstream(data, {"claude"})
        self.assertEqual(status, 0)
        self.assertEqual(len(lines), 1)
        self.assertIn("claude", lines[0])
        self.assertEqual(fetch.call_count, 2)

    def test_shared_repository_ref_is_queried_once(self):
        data = manifest([
            {"id": "codex", "implementation": "internal/codex/profile.go", "sources": [source()]},
            {"id": "claude", "implementation": "internal/claude/profile.go", "sources": [source()]},
        ])
        with patch.object(CHECKER, "get_url_bytes", side_effect=fake_api()) as fetch:
            lines, status = CHECKER.check_upstream(data)
        self.assertEqual(status, 0)
        self.assertEqual(len(lines), 2)
        commit_calls = [call for call in fetch.call_args_list if "/commits/" in call.args[0]]
        self.assertEqual(len(commit_calls), 1)


if __name__ == "__main__":
    unittest.main()
