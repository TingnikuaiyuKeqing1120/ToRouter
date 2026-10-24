"""Isolated OAuth UI fixture: real EXE GETs, synthetic delayed quota refresh.

Uses no real credentials. The local preview proxy blocks authentication and
provider actions; quota POSTs update synthetic snapshots instead of contacting
an upstream. Other requests use the built EXE, including refresh-period writes.
"""
import argparse
from contextlib import closing
from datetime import datetime, timedelta, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
from urllib.parse import parse_qs, urlsplit
import urllib.error
import urllib.request


def free_port():
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        return listener.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--exe', type=Path, required=True)
    parser.add_argument('--state-file', type=Path, required=True)
    parser.add_argument('--stop-file', type=Path, required=True)
    parser.add_argument('--control-file', type=Path, required=True)
    parser.add_argument('--privacy-cases', action='store_true', help='Seed unnamed, account-copied, and expired access-token display cases')
    args = parser.parse_args()
    if args.stop_file.exists():
        parser.error('stop file already exists')
    exe = args.exe.resolve(strict=True)
    flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
    port, proxy_port = free_port(), free_port()
    while proxy_port == port:
        proxy_port = free_port()
    backend = f'http://127.0.0.1:{port}'
    origin = f'http://127.0.0.1:{proxy_port}'
    now = datetime.now(timezone.utc).replace(microsecond=0)
    stamp = lambda value: value.isoformat().replace('+00:00', 'Z')
    with tempfile.TemporaryDirectory(prefix='torouter-oauth-ui-') as data:
        subprocess.run([str(exe), '-data', data, '-show-key'], stdout=subprocess.DEVNULL,
                       stderr=subprocess.PIPE, creationflags=flags, check=True)
        # Use Go's standard-library AES-GCM for the same sealed empty fields as
        # the real store. Only this disposable directory's key is read.
        sealer = Path(data) / 'seal_empty.go'
        sealer.write_text('''package main
import ("crypto/aes"; "crypto/cipher"; "crypto/rand"; "encoding/base64"; "fmt"; "os")
func main() {
  key, err := os.ReadFile(os.Args[1]); if err != nil { panic(err) }
  block, err := aes.NewCipher(key); if err != nil { panic(err) }
  aead, err := cipher.NewGCM(block); if err != nil { panic(err) }
  nonce := make([]byte, aead.NonceSize()); if _, err = rand.Read(nonce); err != nil { panic(err) }
  fmt.Print(base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, nil, nil)))
}
''', encoding='utf-8')
        sealed_empty = subprocess.run(['go','run',str(sealer),str(Path(data)/'master.key')],
            capture_output=True,text=True,check=True,creationflags=flags).stdout.strip()
        database = Path(data) / 'router.db'
        channels, records = [], {}
        definitions = [(1, '日常编程', 'codex_oauth', 'codex', 4),
                       (2, '长任务备用', 'codex_oauth', 'codex', 1),
                       (3, '团队 Copilot', 'copilot_oauth', 'copilot', 2),
                       (4, 'ChatGPT 工作区', 'chatgpt_oauth', 'chatgpt', 1)]
        for channel_id, name, kind, provider, count in definitions:
            members = []
            for index in range(count):
                credential_id = f'demo_{index + 1}'
                oauth = {'provider':provider, 'client_id':'synthetic-preview',
                         'account':f'account-{channel_id}-{index}', 'subject':f'subject-{channel_id}-{index}',
                         'account_label':f'preview{channel_id}{index + 1}@example.invalid',
                         'status':'active', 'expires_at':stamp(now + timedelta(days=1)), 'updated_at':stamp(now)}
                oauth.update(dict.fromkeys(('github_token','refresh_token','id_token','access_token'),sealed_empty))
                members.append({'id':credential_id, 'label':['个人主号','日常备用','临时账号','长任务账号'][index],
                                'enabled':index != 2, 'oauth':oauth})
                if args.privacy_cases and channel_id == 1:
                    if index == 1:
                        members[-1]['label'] = ''
                    elif index == 2:
                        members[-1]['label'] = oauth['account_label']
                    elif index == 3:
                        oauth['expires_at'] = stamp(now - timedelta(hours=1))
                if provider == 'codex':
                    identity_parts = [kind, provider, 'synthetic-preview', oauth['account'], oauth['subject']]
                    identity = hashlib.sha256(json.dumps(identity_parts, separators=(',',':')).encode()).hexdigest()
                    used = [18,62,100,39][index]
                    snapshot = {'captured_at':stamp(now), 'plan_type':'plus' if index < 2 else 'pro', 'buckets':[
                        {'id':'rate_limit', 'general':True, 'allowed':used < 100, 'limit_reached':used == 100,
                         'primary':{'used_percent':used,'window_seconds':18000,'reset_at':int((now+timedelta(hours=2)).timestamp())},
                         'secondary':{'used_percent':used//2,'window_seconds':604800,'reset_at':int((now+timedelta(days=4)).timestamp())}},
                        {'id':'code_review_rate_limit', 'allowed':True,'limit_reached':False,
                         'primary':{'used_percent':7+index*13,'window_seconds':604800,'reset_at':int((now+timedelta(days=3)).timestamp())}}]}
                    records[f'codex_quota_v2:{channel_id}:{credential_id}'] = {
                        'generation':1,'identity':identity,'snapshot':snapshot,'last_attempt_at':stamp(now)}
            channels.append({'id':channel_id,'generation':1,'name':name,'credential_type':kind,
                'enabled':True,'models':['coding-model','review-model'],
                'available_models':['coding-model','review-model','long-context-model'],
                'endpoints':[{'format':'openai_responses' if provider != 'copilot' else 'openai_chat_completions',
                              'base_url':'http://127.0.0.1:9'}],
                'credentials':members,'dispatch_mode':'failover','chain_type':'unidirectional'})
        with closing(sqlite3.connect(database)) as db:
            for channel in channels:
                db.execute('INSERT INTO channels(id,data) VALUES(?,?)', (channel['id'],json.dumps(channel,ensure_ascii=False)))
            for key, record in records.items():
                db.execute('INSERT INTO meta(key,value) VALUES(?,?)', (key,json.dumps(record)))
            # Cached GETs are safe; no automatic provider refresh is requested.
            db.execute("UPDATE settings SET data=json_set(data,'$.quota_refresh_seconds',0) WHERE id=1")
            db.commit()
        server_log = args.state_file.with_suffix('.server.log').open('w',encoding='utf-8')
        process = subprocess.Popen([str(exe), '-data', data, '-port',str(port)], stdin=subprocess.DEVNULL,
                                   stdout=server_log, stderr=server_log, creationflags=flags)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        writes = []
        write_lock = threading.Lock()

        class PreviewHandler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def respond(self, status, body):
                raw = json.dumps(body, ensure_ascii=False).encode()
                self.send_response(status)
                self.send_header('Content-Type','application/json; charset=utf-8')
                self.send_header('Content-Length',str(len(raw)))
                self.send_header('Cache-Control','no-store')
                self.end_headers()
                self.wfile.write(raw)

            def handle_request(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                path = urlsplit(self.path)
                if self.command not in ('GET','HEAD'):
                    with write_lock:
                        writes.append({'method':self.command,'path':path.path,
                                       'query':path.query if path.path == '/api/oauth/codex/usage' else ''})
                if path.path == '/api/oauth/codex/usage' and self.command == 'POST':
                    query = parse_qs(path.query)
                    key = f"codex_quota_v2:{query.get('channel_id',[''])[0]}:{query.get('credential_id',[''])[0]}"
                    if key not in records:
                        self.respond(404, {'error':'synthetic credential not found'})
                        return
                    control = json.loads(args.control_file.read_text()) if args.control_file.exists() else {}
                    time.sleep(min(5, max(0, float(control.get('delay_seconds', 2)))))
                    if control.get('fail', False):
                        self.respond(503, {'error':'合成验收：额度刷新暂不可用'})
                        return
                    updated = datetime.now(timezone.utc).replace(microsecond=0)
                    with closing(sqlite3.connect(database,timeout=10)) as db:
                        record = json.loads(db.execute('SELECT value FROM meta WHERE key=?',(key,)).fetchone()[0])
                        record['snapshot']['captured_at'] = stamp(updated)
                        record['last_attempt_at'] = stamp(updated)
                        record['snapshot']['buckets'][0]['primary']['used_percent'] = 24
                        db.execute('UPDATE meta SET value=? WHERE key=?',(json.dumps(record),key))
                        db.commit()
                    request_method, request_body = 'GET', None
                else:
                    if path.path.startswith('/api/oauth/') and path.path != '/api/oauth/codex/usage':
                        self.respond(409, {'error':'合成预览不连接授权服务'})
                        return
                    if path.path.endswith('/probe') or path.path.endswith('/oauth/models'):
                        self.respond(409, {'error':'合成预览不调用上游'})
                        return
                    request_method, request_body = self.command, body if body else None
                headers = {key:value for key,value in self.headers.items()
                           if key.lower() not in ('host','connection','content-length','accept-encoding')}
                if 'Origin' in headers:
                    headers['Origin'] = backend
                if 'Referer' in headers:
                    headers['Referer'] = backend+'/'
                request = urllib.request.Request(backend+self.path, data=request_body, method=request_method, headers=headers)
                try:
                    response = opener.open(request,timeout=25)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    raw = response.read()
                    self.send_response(response.status)
                    for key,value in response.headers.items():
                        if key.lower() not in ('connection','transfer-encoding','content-length'):
                            self.send_header(key,value)
                    self.send_header('Content-Length',str(len(raw)))
                    self.end_headers()
                    self.wfile.write(raw)

            do_GET = do_HEAD = do_POST = do_PUT = do_DELETE = handle_request

        server = None
        try:
            for _ in range(100):
                try:
                    with opener.open(backend,timeout=1) as response:
                        assert response.status == 200
                    break
                except OSError:
                    assert process.poll() is None, 'isolated EXE exited'
                    time.sleep(.1)
            else:
                raise RuntimeError('isolated EXE not ready')
            server = ThreadingHTTPServer(('127.0.0.1',proxy_port),PreviewHandler)
            threading.Thread(target=server.serve_forever,daemon=True).start()
            state = {'status':'ready','synthetic':True,'origin':origin,'backend_origin':backend,
                     'data_dir':data,'pid':process.pid,'channels':len(channels),
                     'quota_refresh':'local proxy simulation; GET and settings use real EXE',
                     'exe_sha256':hashlib.sha256(exe.read_bytes()).hexdigest()}
            args.state_file.parent.mkdir(parents=True,exist_ok=True)
            args.state_file.write_text(json.dumps(state,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')
            print('UI_ORIGIN='+origin,flush=True)
            while not args.stop_file.exists():
                assert process.poll() is None, 'isolated EXE exited'
                time.sleep(.25)
        finally:
            if server:
                server.shutdown()
                server.server_close()
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=10)
            server_log.close()
            args.state_file.with_suffix('.requests.json').write_text(json.dumps(writes,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')


if __name__ == '__main__':
    main()
