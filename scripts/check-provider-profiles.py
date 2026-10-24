#!/usr/bin/env python3
"""Check pinned provider profile sources, optionally against their upstream refs."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "docs" / "protocol-refs" / "provider-profile-sources.json"
MAX_RESPONSE_BYTES = 1024 * 1024
TIMEOUT_SECONDS = 8
SHA_RE = re.compile(r"^[0-9a-f]{64}$")
REV_RE = re.compile(r"^[0-9a-f]{40}$")
REPO_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")


class ManifestError(ValueError):
    pass


class UpstreamUnavailable(Exception):
    pass


class UpstreamFileMissing(Exception):
    """A tracked source path disappeared or was renamed upstream."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ManifestError(message)


def load_manifest(path: Path = MANIFEST) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ManifestError(f"cannot read or parse manifest ({type(exc).__name__})") from exc
    validate_manifest(data)
    return data


def validate_manifest(data: Any) -> None:
    _require(isinstance(data, dict) and data.get("schema_version") == 1,
             "schema_version must be 1")
    providers = data.get("providers")
    _require(isinstance(providers, list) and providers, "providers must be a non-empty list")
    seen: set[str] = set()
    for provider in providers:
        _require(isinstance(provider, dict), "each provider must be an object")
        pid = provider.get("id")
        _require(isinstance(pid, str) and re.fullmatch(r"[a-z][a-z0-9-]*", pid) is not None,
                 "provider id must be a lowercase identifier")
        _require(pid not in seen, f"duplicate provider id: {pid}")
        seen.add(pid)
        impl = provider.get("implementation")
        _require(isinstance(impl, str) and impl.startswith("internal/") and ".." not in Path(impl).parts,
                 f"{pid}: invalid implementation path")
        sources = provider.get("sources")
        _require(isinstance(sources, list) and sources, f"{pid}: sources must be non-empty")
        for source in sources:
            _require(isinstance(source, dict), f"{pid}: source must be an object")
            repo, ref, revision = source.get("repo"), source.get("tracking_ref"), source.get("baseline_commit")
            _require(isinstance(repo, str) and REPO_RE.fullmatch(repo) is not None,
                     f"{pid}: invalid public repository identifier")
            _require(isinstance(ref, str) and re.fullmatch(r"[A-Za-z0-9._/-]+", ref) is not None,
                     f"{pid}: invalid tracking_ref")
            _require(isinstance(revision, str) and REV_RE.fullmatch(revision) is not None,
                     f"{pid}: baseline_commit must be a full 40-character SHA")
            files = source.get("files")
            _require(isinstance(files, list) and files, f"{pid}: source files must be non-empty")
            for item in files:
                _require(isinstance(item, dict), f"{pid}: file entry must be an object")
                file_path, digest = item.get("path"), item.get("sha256")
                parts = file_path.split("/") if isinstance(file_path, str) else []
                _require(parts and all(part not in ("", ".", "..") for part in parts)
                         and "\\" not in file_path and ":" not in file_path,
                         f"{pid}: invalid source file path")
                _require(isinstance(digest, str) and SHA_RE.fullmatch(digest) is not None,
                         f"{pid}: invalid SHA256 for {file_path}")


def get_url_bytes(url: str, timeout: int = TIMEOUT_SECONDS) -> bytes:
    request = urllib.request.Request(url, headers={"User-Agent": "to-router-profile-check/1"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = response.read(MAX_RESPONSE_BYTES + 1)
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            raise UpstreamFileMissing(url) from exc
        raise UpstreamUnavailable(f"HTTP {exc.code}") from exc
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        raise UpstreamUnavailable(type(exc).__name__) from exc
    if len(body) > MAX_RESPONSE_BYTES:
        raise UpstreamUnavailable("response exceeds 1 MiB limit")
    return body


def _api_commit(repo: str, ref: str) -> str:
    url = f"https://api.github.com/repos/{repo}/commits/{ref}"
    try:
        payload = json.loads(get_url_bytes(url).decode("utf-8"))
        sha = payload.get("sha") if isinstance(payload, dict) else None
        if not isinstance(sha, str) or REV_RE.fullmatch(sha) is None:
            raise UpstreamUnavailable("invalid commit response")
        return sha
    except UpstreamFileMissing as exc:
        raise UpstreamUnavailable("tracking ref not found") from exc
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise UpstreamUnavailable("invalid commit response") from exc


def check_upstream(data: dict[str, Any], provider_ids: set[str] | None = None) -> tuple[list[str], int]:
    commit_cache: dict[tuple[str, str], str | Exception] = {}
    outcomes: list[str] = []
    changed = False
    unavailable = False
    for provider in data["providers"]:
        if provider_ids is not None and provider["id"] not in provider_ids:
            continue
        for source in provider["sources"]:
            key = (source["repo"], source["tracking_ref"])
            if key not in commit_cache:
                try:
                    commit_cache[key] = _api_commit(*key)
                except UpstreamUnavailable as exc:
                    commit_cache[key] = exc
            commit = commit_cache[key]
            for item in source["files"]:
                label = f"{provider['id']} {source['repo']}:{item['path']}"
                if isinstance(commit, Exception):
                    outcomes.append(f"UNAVAILABLE {label} ({commit})")
                    unavailable = True
                    continue
                url = f"https://raw.githubusercontent.com/{source['repo']}/{commit}/{item['path']}"
                try:
                    digest = hashlib.sha256(get_url_bytes(url)).hexdigest()
                except UpstreamFileMissing:
                    outcomes.append(f"CHANGED {label} (missing at {commit})")
                    changed = True
                    continue
                except UpstreamUnavailable as exc:
                    outcomes.append(f"UNAVAILABLE {label} ({exc})")
                    unavailable = True
                    continue
                if digest == item["sha256"]:
                    outcomes.append(f"UNCHANGED {label} sha256={digest}")
                else:
                    outcomes.append(f"CHANGED {label} sha256={item['sha256']} current={digest}")
                    changed = True
    return outcomes, (1 if unavailable else 2 if changed else 0)


def offline_check(data: dict[str, Any], provider_ids: set[str] | None = None) -> tuple[list[str], int]:
    outcomes: list[str] = []
    missing = False
    for provider in data["providers"]:
        if provider_ids is not None and provider["id"] not in provider_ids:
            continue
        path = ROOT / provider["implementation"]
        if path.is_file():
            outcomes.append(f"OK {provider['id']} implementation={provider['implementation']}")
        else:
            outcomes.append(f"MISSING {provider['id']} implementation={provider['implementation']}")
            missing = True
        for source in provider["sources"]:
            for item in source["files"]:
                outcomes.append(f"PINNED {provider['id']} {source['repo']}:{item['path']} sha256={item['sha256']}")
    return outcomes, (1 if missing else 0)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Validate maintained provider-profile source pins.")
    parser.add_argument("--check-upstream", action="store_true", help="query public GitHub refs and compare pinned files")
    parser.add_argument("--provider", action="append", help="check only this provider id (repeatable)")
    parser.add_argument("--manifest", type=Path, default=MANIFEST, help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    try:
        data = load_manifest(args.manifest)
        known = {provider["id"] for provider in data["providers"]}
        requested = set(args.provider) if args.provider else None
        unknown = (requested or set()) - known
        if unknown:
            raise ManifestError("unknown provider id: " + ", ".join(sorted(unknown)))
        lines, status = check_upstream(data, requested) if args.check_upstream else offline_check(data, requested)
        for line in lines:
            print(line)
        return status
    except ManifestError as exc:
        print(f"ERROR {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
