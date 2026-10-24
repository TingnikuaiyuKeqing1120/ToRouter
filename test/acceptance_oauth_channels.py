"""T136 acceptance against a real executable and disposable synthetic data.

Exercises channel creation, bound imports, atomic moves, discovery and reopen.
No real account, source configuration or provider endpoint is accessed.
"""
import argparse
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import http.cookiejar
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid


KINDS = [
    'codex_oauth', 'copilot_oauth', 'chatgpt_oauth', 'claude_oauth',
    'antigravity_oauth', 'devin_oauth', 'zen_free', 'plugin:qoder',
    'plugin:qoder-cn', 'plugin:workbuddy', 'plugin:workbuddy-ai',
    'plugin:minimax-code', 'plugin:minimax-code-global',
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--exe', type=Path, required=True)
    parser.add_argument('--evidence-dir', type=Path, required=True)
    parser.add_argument('--hold-seconds', type=int, default=0)
    parser.add_argument('--stop-file', type=Path)
    args = parser.parse_args()
    exe = args.exe.resolve(strict=True)
    root = args.evidence_dir.resolve()
    root.mkdir(parents=True, exist_ok=True)
    # A fresh random child is always used; existing data is never opened.
    run = root / ('synthetic-' + uuid.uuid4().hex[:12])
    data, source = run / 'data', run / 'codex-source'
    data.mkdir(parents=True)
    source.mkdir()
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        port = probe.getsockname()[1]
    origin = f'http://127.0.0.1:{port}'
    class SyntheticProvider(BaseHTTPRequestHandler):
        def log_message(self, *unused):
            pass

        def do_GET(self):
            value = {'rate_limit': {'allowed': True, 'primary_window': {
                'used_percent': 0, 'limit_window_seconds': 18000,
                'reset_at': int(time.time()) + 18000,
            }}}
            raw = json.dumps(value).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    provider = ThreadingHTTPServer(('127.0.0.1', 0), SyntheticProvider)
    provider_thread = threading.Thread(target=provider.serve_forever, daemon=True)
    provider_thread.start()
    provider_origin = f'http://127.0.0.1:{provider.server_port}'
    env = dict(os.environ, CODEX_HOME=str(source),
               TOROUTER_CODEX_USAGE_URL=provider_origin + '/usage',
               TOROUTER_CODEX_RESET_CREDITS_URL=provider_origin + '/credits',
               TOROUTER_CODEX_TOKEN_URL=provider_origin + '/token',
               TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS='127.0.0.1')
    flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}),
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
    )
    process = None
    log = (run / 'process.log').open('wb')
    report = {'success': False, 'checks': [], 'synthetic': True,
              'binary_sha256': hashlib.sha256(exe.read_bytes()).hexdigest(),
              'evidence_boundary': 'real local EXE and SQLite; synthetic credentials; no provider login'}

    def call(path, body=None, method='GET', expected=200):
        request = urllib.request.Request(
            origin + '/api' + path,
            data=None if body is None else json.dumps(body).encode(),
            method=method,
            headers={'Content-Type': 'application/json', 'X-To-Router': '1'},
        )
        try:
            response = opener.open(request, timeout=20)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            assert response.status == expected, (path, response.status, raw[:300])
            return json.loads(raw) if raw else None

    def start():
        nonlocal process
        process = subprocess.Popen(
            [str(exe), '-mode', 'daemon', '-data', str(data), '-port', str(port)],
            env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=log,
            creationflags=flags,
        )
        deadline = time.monotonic() + 25
        while time.monotonic() < deadline:
            try:
                call('/auth/local', {}, 'POST')
                return
            except OSError:
                assert process.poll() is None, 'isolated process exited during startup'
                time.sleep(.1)
        raise AssertionError('isolated process did not become ready')

    def stop():
        nonlocal process
        if process is None or process.poll() is not None:
            return
        try:
            call('/shutdown', {}, 'POST')
            process.wait(timeout=20)
        except (OSError, subprocess.TimeoutExpired):
            process.terminate()
            process.wait(timeout=10)

    def record(name):
        report['checks'].append(name)
        print('PASS ' + name, flush=True)

    def channel(cid):
        return next(row for row in call('/channels') if row['id'] == cid)

    def create(name, kind):
        return call('/channels', {'name': name, 'credential_type': kind, 'enabled': True}, 'POST')

    def prepare(target, material):
        return call('/oauth-transfer/preview', {
            'credential_type': 'codex_oauth', 'target_channel_id': target,
            'file': json.dumps(material),
        }, 'POST')

    def commit(preview, target):
        version = preview.get('target', {}).get('version', '')
        items = [{'candidate_id': row['id'], 'target_channel_id': target,
                  'expected_version': version or next(
                      (v['version'] for v in row['targets'] if v['id'] == target), '')}
                 for row in preview['rows'] if row['status'] == 'new']
        assert items, 'synthetic material was not accepted'
        return call(f"/oauth-transfer/sessions/{preview['id']}/import", {'items': items}, 'POST')

    def move(source_channel, target_channel, ids, expected=200):
        return call(f"/channels/{source_channel['id']}/credentials/move", {
            'target_channel_id': target_channel['id'], 'credential_ids': ids,
            'expected_generation': source_channel['generation'],
            'expected_target_generation': target_channel['generation'],
        }, 'POST', expected)

    try:
        start()
        created = [create('Synthetic ' + kind, kind) for kind in KINDS]
        assert len({row['id'] for row in created}) == len(KINDS)
        for row in created:
            assert row['enabled'] and not row['models']
            if row['credential_type'] != 'zen_free':
                assert not row.get('credentials') and not row.get('oauth')
        public = create('Synthetic second public channel', 'zen_free')
        assert public['id'] != created[6]['id']
        record('13 built-in entries create independently; empty accounts and multiple public channels persist')

        src = created[0]
        dst = create('Synthetic Codex Plus', 'codex_oauth')
        material = {'access_token': 'synthetic-t136-access-only', 'account_id': 'synthetic-t136-workspace'}
        preview = prepare(src['id'], material)
        assert preview['target']['id'] == src['id']
        original_items = [{'candidate_id': row['id'], 'target_channel_id': dst['id'],
                           'expected_version': preview['target']['version']} for row in preview['rows']]
        call(f"/oauth-transfer/sessions/{preview['id']}/import", {'items': original_items}, 'POST', 409)
        assert not channel(src['id']).get('credentials') and not channel(dst['id']).get('credentials')
        result = commit(preview, src['id'])
        assert sum(item['added'] for item in result['results']) == 1
        src, dst = channel(src['id']), channel(dst['id'])
        member = src['credentials'][0]
        assert member['has_oauth'] and not member.get('oauth')
        record('bound access-only preview rejects target substitution and saves only to the requested channel')

        wrong = created[3]
        move(src, wrong, [member['id']], 400)
        assert channel(src['id'])['credentials'] == src['credentials']
        move(src, dst, [member['id']])
        moved = channel(dst['id'])
        assert not channel(src['id']).get('credentials')
        assert moved['credentials'][0]['id'] == member['id']
        assert moved['credentials'][0]['enabled'] == member['enabled']
        move(src, dst, [member['id']], 409)
        assert len(channel(dst['id'])['credentials']) == 1
        record('same-type move preserves member identity, empties source, and rejects stale or incompatible moves atomically')

        # A complete native JSON remains local: populated access and account prevent enrichment calls.
        exp = int(time.time()) + 3600
        payload = {'sub': 'synthetic-t136-user', 'exp': exp,
                   'https://api.openai.com/auth': {'chatgpt_account_id': 'synthetic-t136-scan-workspace'}}
        encoded = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip('=')
        token = 'e30.' + encoded + '.synthetic'
        (source / 'auth.json').write_text(json.dumps({'tokens': {
            'access_token': token, 'id_token': token, 'refresh_token': 'synthetic-t136-refresh',
            'account_id': 'synthetic-t136-scan-workspace',
        }}), encoding='utf-8')
        scoped = call('/key-discovery/sessions', {
            'source_ids': ['source-codex'], 'target_channel_id': src['id'],
            'credential_type': 'codex_oauth',
        }, 'POST')
        scanned = call(f"/key-discovery/sessions/{scoped['id']}/scan", {'source_id': 'source-codex'}, 'POST')
        assert scanned['target']['id'] == src['id']
        selected = [row for group in scanned['groups'] for row in group['rows']]
        assert selected and all(row['credential_type'] == 'codex_oauth' for row in selected)
        call(f"/key-discovery/sessions/{scoped['id']}/import", {'items': [
            {'candidate_id': row['id'], 'target_channel_id': src['id'],
             'expected_version': scanned['target']['version']} for row in selected]}, 'POST')
        assert len(channel(src['id'])['credentials']) == 1
        record('channel-scoped official-tool discovery reads only the synthetic Codex source and appends to the bound empty channel')

        url = 'https://t136.example.invalid/v1'
        api_body = {'enabled': True, 'credential_type': 'api_key', 'models': [],
                    'endpoints': [{'format': 'openai_responses', 'base_url': url}]}
        for n in range(2):
            call('/channels', {**api_body, 'name': f'Synthetic existing URL {n}',
                              'api_key': f'synthetic-existing-key-{n}'}, 'POST')
        (source / 'auth.json').write_text(json.dumps({'OPENAI_API_KEY': 'synthetic-t136-discovered-key'}), encoding='utf-8')
        (source / 'config.toml').write_text(
            'model_provider = "synthetic"\n[model_providers.synthetic]\n'
            f'base_url = "{url}"\nwire_api = "responses"\nrequires_openai_auth = true\n', encoding='utf-8')
        global_preview = call('/key-discovery/sessions', {'source_ids': ['source-codex']}, 'POST')
        found = call(f"/key-discovery/sessions/{global_preview['id']}/scan", {'source_id': 'source-codex'}, 'POST')
        candidates = [row for group in found['groups'] for row in group['rows']]
        assert len(candidates) == 1 and all(not group['copy_only'] for group in found['groups'])
        old_count = len(call('/channels'))
        imported = call(f"/key-discovery/sessions/{found['id']}/import", {'items': [
            {'candidate_id': candidates[0]['id'], 'target_channel_id': 0,
             'expected_version': '', 'name': 'Synthetic newly discovered'}]}, 'POST')
        assert sum(row['added'] for row in imported['results']) == 1
        assert len(call('/channels')) == old_count + 1
        added = channel(imported['results'][0]['channel_id'])
        assert added['enabled'] and added['name'] == 'Synthetic newly discovered'
        record('global discovery creates a default-enabled new channel despite two existing channels with the same URL')

        expected_channels = call('/channels')
        stop()
        with sqlite3.connect(data / 'router.db') as database:
            raw = '\n'.join(row[0] for row in database.execute('SELECT data FROM channels'))
            assert 'synthetic-t136-access-only' not in raw and 'synthetic-t136-refresh' not in raw
        start()
        assert call('/channels') == expected_channels
        record('full channel and member state survives process shutdown and reopen; secrets are sealed in SQLite')
        report['success'] = True
        report['origin'] = origin
        (run / 'ui-state.json').write_text(json.dumps({
            'synthetic': True, 'origin': origin, 'source_channel_id': src['id'],
            'target_channel_id': dst['id'], 'codex_source': str(source),
        }), encoding='utf-8')
        (root / 'latest-run.txt').write_text(str(run), encoding='utf-8')
        if args.hold_seconds:
            print('READY ' + origin, flush=True)
            until = time.monotonic() + args.hold_seconds
            while time.monotonic() < until and not (args.stop_file and args.stop_file.exists()):
                assert process.poll() is None, 'isolated process exited during UI hold'
                time.sleep(.25)
    finally:
        stop()
        provider.shutdown()
        provider.server_close()
        provider_thread.join(timeout=5)
        log.close()
        (run / 'report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(json.dumps({'success': report['success'], 'checks': len(report['checks'])}), flush=True)


if __name__ == '__main__':
    main()
