"""Export regressions use synthetic trees only; never export the working repository."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch


REPO = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("export_public", REPO / "scripts/export-public.py")
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)


class ExportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.sandbox = Path(self.temp.name).resolve()
        self.source = self.sandbox / "source"
        self.target = self.sandbox / "candidate"
        self.source.mkdir()
        for name in exporter.ROOT_FILES:
            self.write(self.source / name, "public fixture\n")
        self.write(self.source / "go.mod", "module torouter\n")
        self.write(self.source / ".gitignore", ".local/\nweb/node_modules/\nweb/dist/\n*.exe\n")
        for name in [*exporter.INCLUDE_DIRS, "web", "test"]:
            self.write(self.source / name / "fixture.txt", "source one\n")
        self.write(self.source / "scripts/export-public.py", "# old exporter fixture\n")
        shutil.copy2(REPO / "scripts/check-privacy.py", self.source / "scripts/check-privacy.py")
        self.write(self.source / "docs/generated/mcp-tools.json", "{}\n")
        self.root_patch = patch.object(exporter, "ROOT", self.source)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)

    def write(self, path, text):
        assert path.resolve().is_relative_to(self.sandbox)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def export(self, target=None):
        target = target or self.target
        # The old implementation is destructive: every tested target must be disposable.
        assert target.resolve().is_relative_to(self.sandbox)
        with contextlib.redirect_stdout(io.StringIO()):
            exporter.export(target)

    def snapshot(self):
        result = {}
        def linked(path):
            return path.is_symlink() or getattr(path.lstat(), "st_file_attributes", 0) & 0x400
        for root, directories, files in os.walk(self.sandbox, followlinks=False):
            for name in directories + files:
                path = Path(root) / name
                relative = path.relative_to(self.sandbox).as_posix()
                if linked(path):
                    result[relative] = ("link", os.readlink(path))
                elif path.is_dir():
                    result[relative] = ("directory",)
                else:
                    result[relative] = ("file", path.read_bytes())
            directories[:] = [name for name in directories if not linked(Path(root) / name)]
        return result

    def test_repeat_keeps_local_material_and_updates_owned_sources(self):
        self.export()
        self.write(self.target / ".local/release/update-private.key", "synthetic private bytes\n")
        private_bytes = (self.target / ".local/release/update-private.key").read_bytes()
        self.write(self.target / "ToRouter.exe", "synthetic binary\n")
        self.write(self.target / "web/node_modules/dependency.txt", "installed dependency\n")
        self.write(self.target / "web/dist/index.html", "built frontend\n")
        git_config = (self.target / ".git/config").read_bytes()
        self.write(self.source / "internal/fixture.txt", "source two\n")
        stale = self.source / "cmd/fixture.txt"
        assert stale.resolve().is_relative_to(self.sandbox)
        stale.unlink()
        self.export()
        self.assertEqual((self.target / ".local/release/update-private.key").read_bytes(), private_bytes)
        self.assertEqual((self.target / ".git/config").read_bytes(), git_config)
        for name in ["ToRouter.exe", "web/node_modules/dependency.txt", "web/dist/index.html"]:
            self.assertTrue((self.target / name).is_file(), name)
        self.assertEqual((self.target / "internal/fixture.txt").read_text(), "source two\n")
        self.assertFalse((self.target / "cmd/fixture.txt").exists())

    def test_root_ancestor_and_descendant_are_rejected_without_changes(self):
        for target in [self.source, self.sandbox, self.source / "nested"]:
            with self.subTest(target=target.name):
                before = self.snapshot()
                with self.assertRaises(ValueError):
                    self.export(target)
                self.assertEqual(before, self.snapshot())

    def test_unrelated_existing_directory_is_rejected_without_changes(self):
        self.write(self.target / "important.txt", "keep me\n")
        before = self.snapshot()
        with self.assertRaises(ValueError):
            self.export()
        self.assertEqual(before, self.snapshot())

    def test_legacy_candidate_can_be_adopted_without_removing_local_material(self):
        self.export()
        # Simulate a candidate produced by the exporter before ownership metadata existed.
        marker = self.target / ".local/public-export.json"
        if marker.exists():
            assert marker.resolve().is_relative_to(self.sandbox)
            marker.unlink()
        self.write(self.target / ".local/release/update-private.key", "synthetic private bytes\n")
        self.write(self.target / "internal/obsolete.go", "package obsolete\n")
        self.export()
        self.assertFalse((self.target / "internal/obsolete.go").exists())
        self.assertEqual((self.target / ".local/release/update-private.key").read_text(), "synthetic private bytes\n")

    def test_development_clone_is_not_a_legacy_candidate(self):
        self.export()
        marker = self.target / ".local/public-export.json"
        if marker.exists():
            assert marker.resolve().is_relative_to(self.sandbox)
            marker.unlink()
        self.write(self.target / "AGENTS.md", "private development instructions\n")
        before = self.snapshot()
        with self.assertRaises(ValueError):
            self.export()
        self.assertEqual(before, self.snapshot())

    def junction(self, link, target):
        assert link.resolve().is_relative_to(self.sandbox)
        assert target.resolve().is_relative_to(self.sandbox)
        if os.name == "nt":
            result = subprocess.run(["cmd", "/c", "mklink", "/J", str(link), str(target)], capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            link.symlink_to(target, target_is_directory=True)
        def unlink():
            assert link.absolute().is_relative_to(self.sandbox)
            if link.is_symlink():
                link.unlink()
            else:
                link.rmdir()
        self.addCleanup(unlink)

    def test_linked_target_ancestor_is_rejected_without_changes(self):
        outside = self.sandbox / "outside"
        outside.mkdir()
        self.junction(self.sandbox / "link", outside)
        before = self.snapshot()
        with self.assertRaises(ValueError):
            self.export(self.sandbox / "link/candidate")
        self.assertEqual(before, self.snapshot())

    def test_source_and_destination_links_in_public_paths_are_rejected(self):
        self.export()
        outside = self.sandbox / "outside"
        self.write(outside / "valuable.txt", "untouched\n")
        for parent in [self.source / "internal", self.target / "internal"]:
            with self.subTest(parent=parent.parent.name):
                if parent.parent == self.target:
                    self.write(self.source / "internal/escape/valuable.txt", "new public source\n")
                link = parent / "escape"
                self.junction(link, outside)
                before = self.snapshot()
                with self.assertRaises(ValueError):
                    self.export()
                self.assertEqual(before, self.snapshot())
                # Cleanup now so each side is exercised independently.
                self._cleanups.pop()[0]()

    def test_links_inside_preserved_dependencies_do_not_block_export(self):
        self.export()
        dependency = self.target / "web/node_modules"
        dependency.mkdir(parents=True)
        outside = self.sandbox / "outside"
        outside.mkdir()
        self.junction(dependency / "linked-package", outside)
        self.export()
        self.assertTrue((dependency / "linked-package").is_dir())

    def test_file_directory_conflict_fails_before_any_source_changes(self):
        self.export()
        conflict = self.target / "internal/fixture.txt"
        assert conflict.resolve().is_relative_to(self.sandbox)
        conflict.unlink()
        conflict.mkdir()
        self.write(self.source / "README.md", "updated readme\n")
        before = self.snapshot()
        with self.assertRaises(ValueError):
            self.export()
        self.assertEqual(before, self.snapshot())

    def test_invalid_inventory_cannot_claim_local_or_parent_files(self):
        self.export()
        self.write(self.sandbox / "valuable.txt", "outside candidate\n")
        self.write(self.target / ".local/release/update-private.key", "synthetic private bytes\n")
        for name in ["../valuable.txt", ".local/release/update-private.key", "internal/../../valuable.txt"]:
            with self.subTest(name=name):
                self.write(self.target / ".local/public-export.json", json.dumps({
                    "format": "to-router-public-export-v1", "files": [name]}))
                before = self.snapshot()
                with self.assertRaises(ValueError):
                    self.export()
                self.assertEqual(before, self.snapshot())

    def test_linked_inventory_parent_is_rejected_before_changes(self):
        self.export()
        local = self.target / ".local"
        assert local.resolve().is_relative_to(self.sandbox)
        local.rename(self.target / "saved-local")
        outside = self.sandbox / "outside"
        self.write(outside / "valuable.txt", "outside candidate\n")
        self.junction(local, outside)
        before = self.snapshot()
        with self.assertRaises(ValueError):
            self.export()
        self.assertEqual(before, self.snapshot())

    def test_unowned_file_collision_is_rejected_before_updates(self):
        self.export()
        self.write(self.target / "internal/future.go", "user's unowned source\n")
        self.write(self.source / "internal/future.go", "new public source\n")
        self.write(self.source / "README.md", "new readme\n")
        before = self.snapshot()
        with self.assertRaises(ValueError):
            self.export()
        self.assertEqual(before, self.snapshot())

    def test_hardlinked_destination_is_rejected_before_any_changes(self):
        self.export()
        outside = self.sandbox / "outside/valuable.txt"
        self.write(outside, "valuable original\n")
        managed = self.target / "internal/fixture.txt"
        assert managed.resolve().is_relative_to(self.sandbox)
        assert outside.resolve().is_relative_to(self.sandbox)
        managed.unlink()
        os.link(outside, managed)
        self.write(self.source / "internal/fixture.txt", "updated public source\n")
        self.write(self.source / "README.md", "updated readme\n")
        before = self.snapshot()
        with self.assertRaises(ValueError):
            self.export()
        self.assertEqual(before, self.snapshot())

    def test_privacy_audit_still_rejects_secrets(self):
        self.write(self.source / "internal/secret.txt", "-----BEGIN " + "PRIVATE KEY-----\n")
        with self.assertRaises(SystemExit) as result:
            self.export()
        self.assertNotEqual(result.exception.code, 0)


if __name__ == "__main__":
    unittest.main()
