"""
Export public repository source tree to a clean destination directory.
Enforces the whitelist of public sources, removes private documents,
history, and local artifacts, and verifies privacy compliance.
"""
import argparse
import os
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# Exact root files allowed in public repository
ROOT_FILES = [
    "README.md",
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


def export(dest_dir: Path):
    dest_dir = dest_dir.resolve()
    print(f"==> Exporting to: {dest_dir}")
    if dest_dir.exists():
        # Check if empty or clean
        existing = list(dest_dir.iterdir())
        if existing:
            print(f"Warning: Destination {dest_dir} contains {len(existing)} items. Cleaning...")
            for item in existing:
                if item.name == ".git":
                    continue
                if item.is_dir():
                    shutil.rmtree(item)
                else:
                    item.unlink()
    else:
        dest_dir.mkdir(parents=True, exist_ok=True)

    # 1. Copy root files
    print("==> Copying root files...")
    for filename in ROOT_FILES:
        src = ROOT / filename
        if src.exists():
            shutil.copy2(src, dest_dir / filename)
            print(f"  + {filename}")
        else:
            print(f"  ! Warning: {filename} does not exist in root")

    # 2. Copy standard directories
    for dir_name in INCLUDE_DIRS:
        src = ROOT / dir_name
        dst = dest_dir / dir_name
        if src.exists():
            print(f"==> Copying {dir_name}/...")
            shutil.copytree(src, dst, ignore=ignore_patterns, dirs_exist_ok=True)

    # 3. Copy web (clean copy)
    print("==> Copying web/...")
    src_web = ROOT / "web"
    dst_web = dest_dir / "web"
    shutil.copytree(src_web, dst_web, ignore=ignore_patterns, dirs_exist_ok=True)

    # 4. Copy test (clean copy)
    print("==> Copying test/...")
    src_test = ROOT / "test"
    dst_test = dest_dir / "test"
    shutil.copytree(src_test, dst_test, ignore=ignore_patterns, dirs_exist_ok=True)

    # 5. Copy allowed docs files
    print("==> Copying allowed docs files...")
    for rel_path in ALLOWED_DOCS:
        src = ROOT / rel_path
        dst = dest_dir / rel_path
        if src.is_dir():
            shutil.copytree(src, dst, ignore=ignore_patterns, dirs_exist_ok=True)
            print(f"  + {rel_path}/")
        elif src.is_file():
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(src, dst)
            print(f"  + {rel_path}")

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
    export(args.target)


if __name__ == "__main__":
    main()
