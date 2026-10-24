"""Run a built EXE against a local synthetic Codex service and disposable data.

All provider endpoints are loopback. UI/API actions use the actual EXE; the mock
only supplies upstream responses. No real account, token, or reset credit is used.
"""
import argparse
from contextlib import closing
from datetime import datetime, timedelta, timezone
import hashlib
import http.client
from http.cookiejar import CookieJar
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
import urllib.request
import urllib.error
from urllib.parse import urlsplit


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('exe', 'state-file', 'stop-file', 'control-file'):
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--workbench', action='store_true', help='Include weekly-only and reauthorization states plus a legacy schedule for load migration.')
    parser.add_argument('--model-count', type=int, default=3, help='Synthetic API channel catalogue size for editor rendering checks (3..2000).')
    parser.add_argument('--port', type=int, default=0, help='Optional local console port; 0 chooses an available port.')
    args = parser.parse_args()
    if not 3 <= args.model_count <= 2000:
        parser.error('model-count must be 3..2000')
    if not 0 <= args.port <= 65535:
        parser.error('port must be 0..65535')
    if args.stop_file.exists():
        parser.error('stop file already exists')
    exe = args.exe.resolve(strict=True)
    args.state_file.parent.mkdir(parents=True, exist_ok=True)
    flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
    calls, lock = [], threading.Lock()
    started = time.time()

    class Upstream(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def handle_request(self):
            body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
            control = json.loads(args.control_file.read_text()) if args.control_file.exists() else {}
            path = urlsplit(self.path).path
            payload = json.loads(body) if body else {}
            with lock:
                calls.append({'path': path, 'method': self.command,
                              'account': self.headers.get('ChatGPT-Account-ID', ''),
                              'request_id': payload.get('redeem_request_id'),
                              'model': payload.get('model'), 'at': time.time()})
                args.state_file.with_suffix('.calls.json').write_text(json.dumps(calls, indent=2), encoding='utf-8')
            time.sleep(min(10, max(0, control.get('delay_seconds', 0))))
            now = int(time.time())
            count = control.get('card_count', 3)
            if path == '/usage':
                result = {'plan_type': 'plus', 'rate_limit_reset_credits': {'available_count': count},
                          'rate_limit': {'allowed': not control.get('quota_blocked', False), 'limit_reached': bool(control.get('quota_blocked', False)),
                            'primary_window': {'used_percent': control.get('used_percent', 24), 'limit_window_seconds': 18000, 'reset_at': now + 7200},
                            'secondary_window': {'used_percent': 61, 'limit_window_seconds': 604800, 'reset_at': now + 345600}}}
                code = control.get('read_status', 200)
                if args.workbench and self.headers.get('ChatGPT-Account-ID') == 'account-1-2':
                    result['rate_limit'].pop('primary_window')
            elif path == '/credits':
                result = {'available_count': count, 'credits': [
                    {'id': f'synthetic-card-{i}', 'type': 'rate_limit', 'status': 'available',
                     'granted_at': now - 86400, 'expires_at': now + (i + 1) * 86400} for i in range(count)]}
                code = control.get('cards_status', 200)
            elif path == '/credits/consume':
                result, code = {}, control.get('consume_status', 200)
            elif path == '/responses':
                result, code = None, control.get('warmup_status', 200)
            else:
                result, code = {'error': 'synthetic upstream: unsupported endpoint'}, 404
            raw = (b'data: {"type":"response.completed","response":{"status":"completed"}}\n\n'
                   if result is None else json.dumps(result).encode())
            self.send_response(code)
            self.send_header('Content-Type', 'text/event-stream' if result is None else 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            try:
                self.wfile.write(raw)
            except (ConnectionResetError, BrokenPipeError):
                pass

        do_GET = do_POST = handle_request

    upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    upstream_origin = f'http://127.0.0.1:{upstream.server_port}'
    process = None
    server_log = args.state_file.with_suffix('.server.log').open('w', encoding='utf-8')
    temporary = tempfile.TemporaryDirectory(prefix='torouter-t102-', ignore_cleanup_errors=True)
    try:
        with temporary as data:
            subprocess.run([str(exe), '-data', data, '-show-key'], stdout=subprocess.DEVNULL,
                           stderr=subprocess.PIPE, creationflags=flags, check=True)
            sealer = Path(data) / 'seal.go'
            sealer.write_text('''package main
import ("crypto/aes";"crypto/cipher";"crypto/rand";"encoding/base64";"encoding/json";"os")
func main(){key,e:=os.ReadFile(os.Args[1]);if e!=nil{panic(e)};block,e:=aes.NewCipher(key);if e!=nil{panic(e)};a,e:=cipher.NewGCM(block);if e!=nil{panic(e)};v:=map[string]string{};for _,s:=range os.Args[2:]{n:=make([]byte,a.NonceSize());if _,e=rand.Read(n);e!=nil{panic(e)};v[s]=base64.StdEncoding.EncodeToString(a.Seal(n,n,[]byte(s),nil))};json.NewEncoder(os.Stdout).Encode(v)}''', encoding='utf-8')
            sealed = json.loads(subprocess.run(['go', 'run', str(sealer), str(Path(data)/'master.key'), '', 'synthetic-access', 'synthetic-refresh'],
                                capture_output=True, text=True, check=True, creationflags=flags).stdout)
            stamp = lambda value: value.isoformat().replace('+00:00', 'Z')
            now = datetime.now(timezone.utc)
            channels, records = [], {}
            definitions = [(1, '日常编程', 'codex_oauth', 'codex', 4), (2, '长任务备用', 'codex_oauth', 'codex', 1),
                           (3, '团队 Copilot', 'copilot_oauth', 'copilot', 1), (4, 'ChatGPT 工作区', 'chatgpt_oauth', 'chatgpt', 1),
                           (5, 'DeepSeek 日常', 'api_key', '', 2), (6, '备用 API', 'api_key', '', 1)]
            for channel_id, name, kind, provider, count in definitions:
                members = []
                for i in range(count):
                    cid = f'demo_{i+1}'
                    member = {'id': cid, 'label': f'凭证 {i+1}', 'enabled': True}
                    if provider:
                        member['oauth'] = {'provider': provider, 'client_id': 'synthetic-preview',
                            'account': f'account-{channel_id}-{i}', 'subject': f'subject-{channel_id}-{i}',
                            'account_label': f'preview{channel_id}{i+1}@example.invalid', 'status': 'active',
                            'access_token': sealed['synthetic-access'], 'refresh_token': sealed['synthetic-refresh'],
                            'github_token': sealed['synthetic-access'], 'id_token': sealed[''],
                            'expires_at': stamp(now+timedelta(days=2)), 'updated_at': stamp(now)}
                    else:
                        member['api_key'] = sealed['synthetic-access']
                    if args.workbench and channel_id == 1 and i == 1:
                        member['operations'] = {'warmup': {'enabled': False, 'mode': 'daily', 'model': 'coding-model', 'daily_time': '09:00'},
                                                'auto_reset': {'enabled': False, 'rules': []}}
                    if args.workbench and channel_id == 1 and i == 3:
                        member['oauth']['status'] = 'reauth_required'
                        member['oauth']['last_error'] = 'Refresh Token 已失效，需要重新登录'
                    members.append(member)
                    if provider == 'codex':
                        identity = hashlib.sha256(json.dumps([kind, provider, 'synthetic-preview', member['oauth']['account'], member['oauth']['subject']], separators=(',', ':')).encode()).hexdigest()
                        records[f'codex_quota_v2:{channel_id}:{cid}'] = {'generation': 1, 'identity': identity, 'last_attempt_at': stamp(now), 'snapshot': {
                            'captured_at': stamp(now), 'plan_type': 'plus', 'reset_credit_count': 3,
                            'buckets': [{'id': 'rate_limit', 'general': True, 'allowed': True, 'limit_reached': False,
                                'primary': {'used_percent': 24+i*8, 'window_seconds': 18000, 'reset_at': int(now.timestamp()+7200)},
                                'secondary': {'used_percent': 61, 'window_seconds': 604800, 'reset_at': int(now.timestamp()+345600)}}]}}
                        if args.workbench and channel_id == 1 and i == 2:
                            records[f'codex_quota_v2:{channel_id}:{cid}']['snapshot']['buckets'][0].pop('primary')
                channels.append({'id': channel_id, 'generation': 1, 'name': name, 'credential_type': kind, 'enabled': True,
                    'models': ['coding-model', 'review-model'], 'available_models': ['coding-model', 'review-model', 'long-context-model'],
                    'endpoints': [{'format': 'openai_responses' if provider in ('codex', 'chatgpt') else 'openai_chat_completions', 'base_url': upstream_origin}],
                    'credentials': members, 'dispatch_mode': 'failover', 'chain_type': 'unidirectional'})
                if args.workbench and channel_id == 2:
                    channels[-1]['enabled'] = False
                if channel_id == 5 and args.model_count > 3:
                    catalogue = ['coding-model', 'review-model', 'long-context-model'] + [f'catalogue-model-{i:04}' for i in range(3, args.model_count)]
                    channels[-1]['available_models'] = catalogue
                    channels[-1]['models'] = catalogue[:args.model_count // 2]
            with closing(sqlite3.connect(Path(data)/'router.db')) as db:
                for channel in channels:
                    db.execute('INSERT INTO channels(id,data) VALUES(?,?)', (channel['id'], json.dumps(channel, ensure_ascii=False)))
                for key, record in records.items():
                    db.execute('INSERT INTO meta(key,value) VALUES(?,?)', (key, json.dumps(record)))
                db.execute("UPDATE settings SET data=json_set(data,'$.quota_refresh_seconds',0) WHERE id=1")
                db.commit()
            port = args.port or free_port()
            origin = f'http://127.0.0.1:{port}'
            env = dict(os.environ, TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS='127.0.0.1',
                TOROUTER_CODEX_BACKEND_URL=upstream_origin, TOROUTER_CODEX_USAGE_URL=upstream_origin+'/usage',
                TOROUTER_CODEX_RESET_CREDITS_URL=upstream_origin+'/credits', TOROUTER_CODEX_TOKEN_URL=upstream_origin+'/token',
                TOROUTER_CODEX_MODELS_URL=upstream_origin+'/models', TOROUTER_CODEX_ISSUER_URL=upstream_origin,
                TOROUTER_CODEX_FALLBACK_URL=upstream_origin, TOROUTER_CODEX_AUTHORIZE_URL=upstream_origin+'/authorize',
                TOROUTER_CODEX_DEVICE_CODE_URL=upstream_origin+'/device', TOROUTER_CODEX_DEVICE_TOKEN_URL=upstream_origin+'/token')
            process = subprocess.Popen([str(exe), '-data', data, '-port', str(port)], env=env,
                stdin=subprocess.DEVNULL, stdout=server_log, stderr=server_log, creationflags=flags)
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(CookieJar()))
            for _ in range(100):
                try:
                    with opener.open(origin, timeout=1) as response:
                        assert response.status == 200
                    break
                except OSError:
                    assert process.poll() is None, 'EXE exited'
                    time.sleep(.1)
            else:
                raise RuntimeError('EXE not ready')
            state = {'status': 'ready', 'synthetic': True, 'workbench': args.workbench, 'origin': origin, 'upstream_origin': upstream_origin,
                     'data_dir': data, 'pid': process.pid, 'exe_sha256': hashlib.sha256(exe.read_bytes()).hexdigest()}
            args.state_file.write_text(json.dumps(state, indent=2), encoding='utf-8')
            print('UI_ORIGIN='+origin, flush=True)
            while not args.stop_file.exists():
                assert process.poll() is None, 'EXE exited'
                time.sleep(.25)
            for path in ('/api/auth/local', '/api/shutdown'):
                req = urllib.request.Request(origin+path, data=b'{}', method='POST', headers={'X-To-Router': '1', 'Content-Type': 'application/json'})
                try:
                    with opener.open(req, timeout=10) as response:
                        response.read()
                except (ConnectionResetError, http.client.RemoteDisconnected):
                    if path != '/api/shutdown':
                        raise
                    # The service can close its socket before the shutdown reply is flushed.
                    # Accept this only when the real child process then exits successfully.
            assert process.wait(timeout=15) == 0, 'EXE shutdown failed'
    finally:
        if process and process.poll() is None:
            process.terminate()
            process.wait(timeout=10)
        upstream.shutdown()
        upstream.server_close()
        server_log.close()
        # Retry after the process closes Windows-held database files.
        temporary.cleanup()
        print(json.dumps({'calls': len(calls), 'seconds': round(time.time()-started, 1)}), flush=True)


if __name__ == '__main__':
    main()
