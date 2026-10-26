"""Exercise the real packaging script with synthetic keys and a stub compiler."""
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[1]
POWERSHELL = shutil.which("pwsh") or shutil.which("powershell")


@unittest.skipUnless(POWERSHELL, "PowerShell required")
class PackagingTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.sandbox = Path(cls.temp.name).resolve()
        cls.tool = cls.sandbox / ("manifest.exe" if os.name == "nt" else "manifest")
        subprocess.run(["go", "build", "-o", str(cls.tool), "./scripts/release-manifest"], cwd=REPO, check=True, capture_output=True)

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    def setUp(self):
        self.root = Path(tempfile.mkdtemp(dir=self.sandbox))
        (self.root / "scripts").mkdir()
        shutil.copy2(REPO / "scripts/package-release.ps1", self.root / "scripts/package-release.ps1")
        for name in ["LICENSE", "THIRD_PARTY_NOTICES.md"]:
            (self.root / name).write_text("synthetic legal text\n")
        self.keys = self.root / "keys"
        self.other_keys = self.root / "other-keys"
        for dest in [self.keys, self.other_keys]:
            subprocess.run(["go", "run", "./scripts/release-keygen", "-dir", str(dest)], cwd=REPO, check=True, capture_output=True)
        self.public = (self.keys / "update-public.key").read_text().strip()
        self.wrong_public = (self.other_keys / "update-public.key").read_text().strip()
        self.output = self.root / "artifacts"
        self.marker = self.root / "build-started"

    def package(self, public=None, rotate_private_during_build=False):
        quote = lambda value: "'" + str(value).replace("'", "''") + "'"
        rotate_key = ""
        if rotate_private_during_build:
            rotate_key = ("Copy-Item -LiteralPath " + quote(self.other_keys / "update-private.key") +
                          " -Destination " + quote(self.keys / "update-private.key") + " -Force")
        # Keep Go's actual Ed25519 implementation; only replace the expensive app
        # compiler. The fake build records exactly which public key it received.
        script = f"""
$ErrorActionPreference = 'Stop'
function global:go {{
    if ($args[0] -ne 'run' -or $args[1] -ne './scripts/release-manifest') {{ throw 'Unexpected Go command' }}
    $toolArgs = @($args | Select-Object -Skip 2)
    & {quote(self.tool)} @toolArgs
    $global:LASTEXITCODE = $LASTEXITCODE
}}
function global:powershell {{
    $outputIndex = [Array]::IndexOf($args, '-OutputDir')
    $keyIndex = [Array]::IndexOf($args, '-UpdatePublicKey')
    [IO.File]::WriteAllText({quote(self.marker)}, $args[$keyIndex + 1])
    [IO.Directory]::CreateDirectory($args[$outputIndex + 1]) | Out-Null
    [IO.File]::WriteAllText((Join-Path $args[$outputIndex + 1] 'ToRouter.exe'), 'synthetic executable')
    {rotate_key}
    $global:LASTEXITCODE = 0
}}
& {quote(self.root / 'scripts/package-release.ps1')} -Version 1.2.3 -Repo 'example/project' -PrivateKeyFile {quote(self.keys / 'update-private.key')} -OutputDir {quote(self.output)}
"""
        if public is not None:
            script = script.rstrip() + " -PublicKey " + quote(public) + "\n"
        wrapper = self.root / "run.ps1"
        wrapper.write_text(script, encoding="utf-8")
        return subprocess.run([POWERSHELL, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", str(wrapper)], cwd=self.root, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=90)

    def test_mismatched_valid_public_key_rejected_before_build_or_outputs(self):
        result = self.package(self.wrong_public)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(self.marker.exists(), "mismatched keys started compilation")
        self.assertFalse(self.output.exists(), "mismatched keys created output artifacts")

    def test_matching_default_public_key_packages_signed_manifest(self):
        result = self.package()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.marker.read_text(), self.public)
        manifest = json.loads((self.output / "manifest.json").read_text())
        self.assertEqual(len(base64.b64decode(manifest["signature"])), 64)
        self.assertEqual(set(p.name for p in self.output.iterdir()), {
            "ToRouter.exe", "manifest.json", "LICENSE", "THIRD_PARTY_NOTICES.md", "SHA256SUMS.txt"})

    def test_changed_signing_key_is_rejected_against_key_injected_into_exe(self):
        result = self.package(rotate_private_during_build=True)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.marker.read_text(), self.public)
        self.assertFalse((self.output / "manifest.json").exists(), "final manifest accepted a different signing key")


if __name__ == "__main__":
    unittest.main()
