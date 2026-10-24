import importlib.util
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'scripts' / 'check-privacy.py'
spec = importlib.util.spec_from_file_location('privacy_check', SCRIPT)
privacy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(privacy)


class PrivacyCheckTests(unittest.TestCase):
    def test_attribution_exception_is_exact_and_keeps_secret_checks(self):
        with tempfile.TemporaryDirectory(prefix='torouter-notice-test-') as folder:
            root = Path(folder)
            subprocess.run(['git', 'init'], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            notice = root / 'web/public/third-party/licenses/fixture/LICENSE'
            notice.parent.mkdir(parents=True)
            legal = b'Copyright Example Contributor <public-author' + b'@gmail.com>\n'
            notice.write_bytes(legal)
            manifest = root / privacy.NOTICE_MANIFEST
            def inventory(data, path='licenses/fixture/LICENSE'):
                manifest.write_text(json.dumps({'schema': 1, 'components': [{
                    'name': 'synthetic-upstream', 'version': 'test-fixture',
                    'source': 'https://github.com/example/synthetic-upstream',
                    'notices': [{'path': path, 'sha256': hashlib.sha256(data).hexdigest()}],
                }]}))
            def check():
                return subprocess.run([sys.executable, str(SCRIPT)], cwd=root, capture_output=True, text=True)
            inventory(legal)
            good = check()
            self.assertEqual(good.returncode, 0, good.stdout)
            self.assertIn('Retained 1 public email attribution', good.stdout)
            inventory(legal, '../licenses/fixture/LICENSE')
            self.assertEqual(check().returncode, 1)
            inventory(b'outdated legal text')
            self.assertEqual(check().returncode, 1)
            secret = legal + ('tr_mcp_' + 'a' * 40).encode()
            notice.write_bytes(secret)
            inventory(secret)
            guarded = check()
            self.assertEqual(guarded.returncode, 1)
            self.assertIn('token-candidate', guarded.stdout)
            self.assertNotIn(legal.decode().strip(), guarded.stdout)

    def test_staged_legal_text_cannot_use_an_unstaged_attribution_inventory(self):
        with tempfile.TemporaryDirectory(prefix='torouter-notice-index-') as folder:
            root = Path(folder)
            def git(*args):
                subprocess.run(['git', *args], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            git('init')
            path = 'licenses/fixture/AUTHORS'
            notice = root / privacy.NOTICE_ROOT / path
            notice.parent.mkdir(parents=True)
            manifest = root / privacy.NOTICE_MANIFEST
            def write_manifest(data):
                manifest.write_text(json.dumps({'schema': 1, 'components': [{
                    'name': 'synthetic-upstream', 'version': 'test-fixture',
                    'source': 'https://github.com/example/synthetic-upstream',
                    'notices': [{'path': path, 'sha256': hashlib.sha256(data).hexdigest()}],
                }]}))
            notice.write_bytes(b'original')
            write_manifest(b'original')
            git('add', 'web/public/third-party')
            revised = b'Example Contributor <public-author' + b'@gmail.com>'
            notice.write_bytes(revised)
            git('add', str(notice.relative_to(root)))
            write_manifest(revised)
            normal = subprocess.run([sys.executable, str(SCRIPT)], cwd=root, capture_output=True, text=True)
            staged = subprocess.run([sys.executable, str(SCRIPT), '--staged'], cwd=root, capture_output=True, text=True)
            self.assertEqual(normal.returncode, 0, normal.stdout)
            self.assertEqual(staged.returncode, 1, staged.stdout)
            self.assertIn('personal-email-candidate', staged.stdout)

    def test_paths_without_confusing_api_routes_or_explicit_fixtures(self):
        value = 'C:' + '\\Users\\' + 'private-profile\\app.exe'
        self.assertTrue(privacy.findings(value.encode()))
        self.assertTrue(privacy.findings(value.replace('\\', '/').encode()))
        self.assertFalse(privacy.findings(b'https://api.example.com/v1/users/me/balance'))
        self.assertFalse(privacy.findings(('C:' + '\\Users\\example-user\\app.exe').encode()))

    def test_secret_candidates_report_categories_and_lines_only(self):
        token = 'tr_' + 'mcp_' + 'a' * 40
        self.assertEqual(privacy.findings(('first\n' + token).encode()), [('token-candidate', 2)])
        self.assertNotIn(token, str(privacy.findings(token.encode())))
        self.assertNotIn(token, privacy.safe_location('docs/' + token + '.txt'))
        self.assertEqual(privacy.findings(('tr_' + 'b' * 43).encode()), [('token-candidate', 1)])

    def test_staged_data_is_checked_even_when_working_copy_is_clean(self):
        with tempfile.TemporaryDirectory(prefix='torouter-privacy-test-') as folder:
            def git(*args):
                subprocess.run(['git', *args], cwd=folder, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            git('init')
            p = Path(folder) / 'sample.txt'
            value = 'C:' + '\\Users\\' + 'private-profile\\config.json'
            p.write_text(value)
            git('add', 'sample.txt')
            p.write_text('safe placeholder')
            normal = subprocess.run([sys.executable, str(SCRIPT)], cwd=folder, capture_output=True, text=True)
            staged = subprocess.run([sys.executable, str(SCRIPT), '--staged'], cwd=folder, capture_output=True, text=True)
            self.assertEqual(normal.returncode, 0, normal.stdout)
            self.assertEqual(staged.returncode, 1, staged.stdout)
            self.assertNotIn(value, staged.stdout + staged.stderr)


if __name__ == '__main__':
    unittest.main()
