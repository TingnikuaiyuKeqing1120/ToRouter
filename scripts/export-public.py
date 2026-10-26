"""
Synchronize public sources into an identified public repository candidate.
Exclude private documents, history and local artifacts from the source;
preserve the candidate's Git history and local build/key material.
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
INVENTORY = Path(".local/public-export.json")

# Exact root files allowed in public repository
ROOT_FILES = [
    "README.md",
    "README_en.md",
    "LICENSE",
    "THIRD_PARTY_NOTICES.md",
    "go.mod",
    "go.sum",
    "build.ps1",
    "wails.json",
    ".gitignore",
    ".gitattributes",
]

# Entire directories to include
INCLUDE_DIRS = [
    "cmd",
    "internal",
    "third_party",
    "scripts",
]

# Specifically allowed docs files
ALLOWED_DOCS = [
    "docs/assets/readme",
    "docs/generated/mcp-tools.json",
    "docs/protocol-refs/provider-profile-sources.json",
]


def ignore_patterns(path, names):
    ignored = set()
    for name in names:
        if name in ("node_modules", "dist", ".vite", "__pycache__", ".local", ".git"):
            ignored.add(name)
        elif name.endswith((".exe", ".db", ".sqlite", ".sqlite3", ".log", ".syso~", ".tsbuildinfo")):
            ignored.add(name)
        elif name in ("master.key", "previous.exe"):
            ignored.add(name)
    return ignored


def check_path(path: Path):
    """Reject links before resolving them, including Windows directory junctions."""
    for item in [*reversed(path.parents), path]:
        try:
            info = item.lstat()
        except FileNotFoundError:
            continue
        if item.is_symlink() or getattr(info, "st_file_attributes", 0) & 0x400:
            raise ValueError("Export paths must not traverse symbolic links or reparse points")


def public_path(relative: Path):
    if any(ignore_patterns("", [part]) for part in relative.parts):
        return False
    name = relative.as_posix()
    return (name in ROOT_FILES or
            (len(relative.parts) > 1 and relative.parts[0] in [*INCLUDE_DIRS, "web", "test"]) or
            any(name == allowed or name.startswith(allowed + "/") for allowed in ALLOWED_DOCS))


def source_files(root: Path):
    """Inventory only the public selection; never enter preserved local directories."""
    files = set()

    def collect(path):
        check_path(path)
        if path.is_dir():
            for child in sorted(path.iterdir()):
                if not ignore_patterns(path, [child.name]):
                    collect(child)
        elif path.is_file():
            files.add(path.relative_to(root).as_posix())
        elif path.exists():
            raise ValueError("Public sources must be regular files or directories")

    for relative in [*ROOT_FILES, *INCLUDE_DIRS, "web", "test", *ALLOWED_DOCS]:
        collect(root / relative)
    return files


def previous_files(dest: Path):
    marker = dest / INVENTORY
    check_path(marker)
    if marker.exists():
        try:
            data = json.loads(marker.read_text(encoding="utf-8"))
            names = data["files"]
            if data["format"] != "to-router-public-export-v1" or not isinstance(names, list):
                raise ValueError()
            for name in names:
                path = Path(name)
                if (not isinstance(name, str) or not path.parts or path.is_absolute() or
                        ".." in path.parts or "\\" in name or ":" in name or
                        path.as_posix() != name or not public_path(path)):
                    raise ValueError()
            return set(names)
        except (OSError, ValueError, KeyError, TypeError):
            raise ValueError("Invalid public export inventory; destination was not changed") from None
    if not dest.exists() or not any(dest.iterdir()):
        return set()

    # One-time adoption of the exact old public layout, never a development clone
    # or an arbitrary populated directory. Local build/key material stays unowned.
    required = [dest / "go.mod", dest / "scripts/export-public.py"]
    for path in required:
        check_path(path)
    if (not (dest / ".git").exists() or not all(path.is_file() for path in required) or
            (dest / "go.mod").read_text(encoding="utf-8").splitlines()[:1] != ["module torouter"]):
        raise ValueError("Nonempty destination is not an identified public export candidate")
    root_names = {*ROOT_FILES, *INCLUDE_DIRS, "web", "test", "docs"}
    for child in dest.iterdir():
        if child.name not in root_names and not ignore_patterns(dest, [child.name]):
            raise ValueError("Destination contains files outside the public candidate layout")
    docs = dest / "docs"
    check_path(docs)
    if docs.exists():
        def check_docs(path):
            check_path(path)
            name = path.relative_to(dest).as_posix()
            if any(name == allowed or name.startswith(allowed + "/") for allowed in ALLOWED_DOCS):
                return
            if not path.is_dir() or not any(allowed.startswith(name + "/") for allowed in ALLOWED_DOCS):
                raise ValueError("Destination contains private or unknown documentation")
            for child in path.iterdir():
                check_docs(child)
        check_docs(docs)
    return source_files(dest)


def export(dest_dir: Path):
    # All validation precedes mkdir, copy and delete. Do not resolve away a link
    # before checking it, or a seemingly separate target can alias the source.
    dest_dir = Path(os.path.abspath(dest_dir))
    check_path(dest_dir)
    dest_dir = dest_dir.resolve()
    root = ROOT.resolve()
    if dest_dir.is_relative_to(root) or root.is_relative_to(dest_dir):
        raise ValueError("Destination must be separate from the source and its ancestors")
    if dest_dir.exists() and not dest_dir.is_dir():
        raise ValueError("Destination must be a directory")
    files = source_files(root)
    previous = previous_files(dest_dir)
    for name in files | previous:
        path = dest_dir / name
        check_path(path)
        if path.exists() and not path.is_file():
            raise ValueError("Public source path conflicts with a destination directory")
        if name in files and path.exists() and path.stat().st_nlink > 1:
            raise ValueError("Public source destination must not be a hard-linked file")
        for parent in path.parents:
            if parent == dest_dir:
                break
            if parent.exists() and not parent.is_dir():
                raise ValueError("Public source parent conflicts with a destination file")
        if name in files and name not in previous and path.exists():
            raise ValueError("New public source conflicts with an unowned destination file")
    marker = dest_dir / INVENTORY
    if marker.exists() and not marker.is_file():
        raise ValueError("Public export inventory must be a regular file")
    if marker.parent.exists() and not marker.parent.is_dir():
        raise ValueError("Public export inventory parent must be a directory")

    print(f"==> Exporting to: {dest_dir}")
    dest_dir.mkdir(parents=True, exist_ok=True)
    for name in sorted(previous - files):
        path = dest_dir / name
        path.unlink(missing_ok=True)
        # Empty source folders may go; folders containing dependencies/artifacts stay.
        for parent in path.parents:
            if parent == dest_dir:
                break
            try:
                parent.rmdir()
            except OSError:
                break
    for name in sorted(files):
        dst = dest_dir / name
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(root / name, dst)
    marker.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=marker.parent, delete=False) as output:
        temporary = Path(output.name)
        output.write(json.dumps({"format": "to-router-public-export-v1", "files": sorted(files)}, indent=2) + "\n")
    try:
        temporary.replace(marker)
    finally:
        temporary.unlink(missing_ok=True)

    # Ensure git is initialized in destination so git-based checks can run
    git_dir = dest_dir / ".git"
    if not git_dir.exists():
        print("==> Initializing empty Git repository in destination...")
        subprocess.run(["git", "init"], cwd=str(dest_dir), check=True)

    # 6. Verify with check-privacy.py on destination
    print("\n==> Running privacy audit on exported tree...")
    check_script = dest_dir / "scripts" / "check-privacy.py"
    if check_script.exists():
        res = subprocess.run([sys.executable, str(check_script)], cwd=str(dest_dir), capture_output=True, text=True)
        print(res.stdout)
        if res.stderr:
            print("STDERR:", res.stderr)
        if res.returncode != 0:
            print("ERROR: Privacy audit failed on exported tree!")
            sys.exit(res.returncode)
    else:
        print("Warning: check-privacy.py not found in exported scripts.")

    print(f"\n[SUCCESS] Public repository successfully exported to {dest_dir}!")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", "-t", type=Path, required=True, help="Destination directory for public repo")
    args = parser.parse_args()
    try:
        export(args.target)
    except (ValueError, OSError) as error:
        parser.exit(1, f"Export refused: {error}\n")


if __name__ == "__main__":
    main()
