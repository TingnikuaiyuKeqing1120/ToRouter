"""T073 acceptance against a real binary and a controlled local upstream.

Only disposable data and synthetic keys are used. Optional hold exposes the same
fixtures for browser acceptance. Evidence never contains credentials or prompts.
"""
import argparse
import copy
import hashlib
import http.cookiejar
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


FORMATS = ['openai_chat_completions', 'openai_responses',
           'anthropic_messages', 'gemini_generate_content']


def scope(models=None, rules=None):
    return {'model_mode': 'all' if models is None else 'selected',
            'model_ids': [] if models is None else models, 'tag_rules': rules or {}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--exe', type=Path, required=True)
    parser.add_argument('--evidence-file', type=Path, required=True)
    parser.add_argument('--state-file', type=Path)
    parser.add_argument('--stop-file', type=Path)
    parser.add_argument('--hold-seconds', type=int, default=0)
    args = parser.parse_args()
    exe = args.exe.resolve(strict=True)
    if args.stop_file and args.stop_file.exists():
        parser.error('stop file already exists')
    upstream_calls, failed_routes, checks = [], set(), []
    upstream_lock = threading.Lock()

    class Upstream(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))))
            route = urllib.parse.urlsplit(self.path).path.strip('/').split('/')[0]
            with upstream_lock:
                upstream_calls.append({'route': route, 'path': self.path, 'model': body.get('model')})
            status = 429 if route in failed_routes else 200
            if status == 429:
                response = {'error': {'message': 'synthetic rate limit'}}
            elif ':generateContent' in self.path:
                response = {'candidates': [{'content': {'role': 'model', 'parts': [{'text': 'ok'}]},
                                             'finishReason': 'STOP'}],
                            'usageMetadata': {'promptTokenCount': 12, 'candidatesTokenCount': 2,
                                              'totalTokenCount': 14}}
            elif self.path.endswith('/messages'):
                response = {'id': 'synthetic-message', 'type': 'message', 'role': 'assistant',
                            'model': body.get('model'), 'content': [{'type': 'text', 'text': 'ok'}],
                            'stop_reason': 'end_turn', 'usage': {'input_tokens': 12, 'output_tokens': 2}}
            elif self.path.endswith('/responses'):
                response = {'id': 'synthetic-response', 'object': 'response', 'status': 'completed',
                            'model': body.get('model'), 'output': [{'type': 'message', 'role': 'assistant',
                            'content': [{'type': 'output_text', 'text': 'ok', 'annotations': []}]}],
                            'usage': {'input_tokens': 12, 'output_tokens': 2, 'total_tokens': 14}}
            else:
                response = {'id': 'synthetic-chat', 'object': 'chat.completion', 'model': body.get('model'),
                            'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'ok'},
                                         'finish_reason': 'stop'}],
                            'usage': {'prompt_tokens': 12, 'completion_tokens': 2, 'total_tokens': 14}}
            raw = json.dumps(response).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    upstream_thread = threading.Thread(target=upstream.serve_forever, daemon=True)
    upstream_thread.start()
    upstream_origin = f'http://127.0.0.1:{upstream.server_port}'
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    origin = f'http://127.0.0.1:{port}'
    flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
    proc = None
    with tempfile.TemporaryDirectory(prefix='torouter-scope-') as directory:
        try:
            proc = subprocess.Popen([str(exe), '-data', directory, '-port', str(port)],
                                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL, creationflags=flags)
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

            def call(path, body=None, method='GET', client=None, expected=200):
                headers = {'Content-Type': 'application/json', 'X-To-Router': '1'}
                if client:
                    headers['Authorization'] = 'Bearer ' + client
                request = urllib.request.Request(origin + path, method=method, headers=headers,
                    data=None if body is None else json.dumps(body).encode())
                try:
                    response = opener.open(request, timeout=20)
                except urllib.error.HTTPError as error:
                    response = error
                raw = response.read()
                accepted = expected if isinstance(expected, (tuple, list)) else [expected]
                assert response.status in accepted, (method, path, response.status, raw[:500])
                return json.loads(raw) if raw else None

            for _ in range(150):
                try:
                    call('/api/auth/local', {}, 'POST')
                    break
                except OSError:
                    assert proc.poll() is None, 'isolated gateway exited'
                    time.sleep(.1)
            else:
                raise AssertionError('isolated gateway did not become ready')

            settings = call('/api/settings')
            settings.pop('prices', None)
            settings.update({'retries_per_channel': 1, 'max_attempts': 4})
            call('/api/settings', settings, 'PUT')
            call('/api/tag-groups', {'id': 'purpose', 'name': '用途'}, 'POST', expected=201)
            for name in ['code', 'writing']:
                call('/api/tags', {'id': 'purpose:' + name, 'group_id': 'purpose',
                    'name': '编程' if name == 'code' else '写作', 'color': 'sky'}, 'POST', expected=201)

            channels, channel_bodies = {}, {}
            for route, label, tags, context in [
                ('paid', '按量 API · 仅授权时可用', ['billing:payg', 'privacy:zdr'], 64000),
                ('free', '免费 API · 默认跟随测试', ['billing:free', 'privacy:zdr'], 128000),
                ('unknown', '套餐 API · 隐私未知', ['billing:plan'], 96000),
            ]:
                body = {'name': label, 'enabled': True, 'api_key': 'synthetic-only-key',
                        'models': ['native', 'secondary'], 'available_models': ['native', 'secondary', 'disabled'],
                        'tags': tags, 'model_tags': {'secondary': ['privacy:log_no_train', 'purpose:writing']},
                        'endpoints': [{'format': f, 'base_url': upstream_origin + '/' + route} for f in FORMATS]}
                channel_bodies[route] = body
                channels[route] = call('/api/channels', body, 'POST')['id']
                call('/api/model-specs', {'channel_id': channels[route], 'model': 'native',
                    'specs': {'context_window': context, 'max_output_tokens': 8192,
                              'input_modalities': ['text'], 'output_modalities': ['text'], 'streaming': True}}, 'PUT')

            def public(identity, routes):
                return {'id': identity, 'display_name': {'coding': '日常编程', 'paid-only': '按量专用',
                        'unknown': '隐私未知测试'}.get(identity, identity), 'enabled': True, 'fallback': True,
                        'bindings': [{'channel_id': channels[route], 'model': 'native', 'priority': i,
                                      'enabled': True} for i, route in enumerate(routes)]}

            for identity, routes in [('coding', ['paid', 'free']), ('paid-only', ['paid']), ('unknown', ['unknown'])]:
                call('/api/models', public(identity, routes), 'POST')
            default = call('/api/client-keys', {'name': '全部权限 · 原有行为'}, 'POST', expected=201)
            assert default['client_key']['scope'] == scope()
            limited_scope = scope(['coding'], {'billing': ['billing:free'], 'privacy': ['privacy:zdr']})
            limited = call('/api/client-keys', {'name': '免费且 ZDR', 'scope': limited_scope}, 'POST', expected=201)
            kid, token = limited['client_key']['id'], limited['key']
            checks.append('default key remains unrestricted; explicit selected scope persists')

            call('/v1/models', expected=401)
            assert [m['id'] for m in call('/v1/models', client=token)['data']] == ['coding']
            client_specs = call('/v1/models/specs', client=token)['data']
            assert len(client_specs) == 1 and client_specs[0]['context_length'] == 128000
            assert len(client_specs[0]['bindings']) == 1
            assert channel_bodies['paid']['name'] not in json.dumps(client_specs, ensure_ascii=False)
            complete_specs = call('/api/models/specs')['data']
            assert next(m for m in complete_specs if m['id'] == 'coding')['context_length'] is None
            preview = call('/api/client-keys/preview', {'scope': limited_scope}, 'POST')
            assert preview['allowed_models'] == 1
            assert any(not b['allowed'] for m in preview['models'] if m['model_id'] == 'coding' for b in m['bindings'])
            checks.append('discovery and specifications reflect key scope; admin sees all; preview explains rejected binding')

            calls = [('/v1/chat/completions', {'model': 'coding', 'messages': [{'role': 'user', 'content': 'hi'}]}),
                     ('/v1/responses', {'model': 'coding', 'input': 'hi', 'store': False}),
                     ('/v1/messages', {'model': 'coding', 'max_tokens': 8, 'messages': [{'role': 'user', 'content': 'hi'}]}),
                     ('/v1beta/models/coding:generateContent', {'contents': [{'role': 'user', 'parts': [{'text': 'hi'}]}]})]
            for path, body in calls:
                call(path, body, 'POST', client=token)
            assert len(upstream_calls) == 4 and {r['route'] for r in upstream_calls} == {'free'}
            checks.append('four protocol requests select only permitted upstream despite paid priority')

            before = len(upstream_calls)
            call('/v1/chat/completions', {**calls[0][1], 'model': 'paid-only'}, 'POST', token, 403)
            assert len(upstream_calls) == before
            call(f'/api/client-keys/{kid}', {'name': '暂不允许模型', 'scope': scope([])}, 'PUT')
            call(calls[0][0], calls[0][1], 'POST', token, 403)
            assert call('/v1/models', client=token)['data'] == [] and len(upstream_calls) == before
            call(f'/api/client-keys/{kid}', {'name': '限制但空选', 'scope': scope(['coding'], {'billing': []})}, 'PUT')
            call(calls[0][0], calls[0][1], 'POST', token, 403)
            assert len(upstream_calls) == before
            assert call(f'/api/client-keys/{kid}/key')['key'] == token
            checks.append('selected empty and restricted empty deny without upstream calls; editing retains original key')

            unknown_scope = scope(['unknown'], {'billing': ['billing:plan'], 'privacy': ['privacy:zdr']})
            call(f'/api/client-keys/{kid}', {'name': '未知隔离', 'scope': unknown_scope}, 'PUT')
            call(calls[0][0], {**calls[0][1], 'model': 'unknown'}, 'POST', token, 403)
            unknown_scope['tag_rules']['privacy'] = ['privacy:unknown']
            call(f'/api/client-keys/{kid}', {'name': '显式允许未知', 'scope': unknown_scope}, 'PUT')
            call(calls[0][0], {**calls[0][1], 'model': 'unknown'}, 'POST', token)
            assert upstream_calls[-1]['route'] == 'unknown'
            checks.append('unknown only matches explicit unknown or no group restriction')

            call(f'/api/client-keys/{kid}', {'name': '免费且 ZDR', 'scope': limited_scope}, 'PUT')
            changed = copy.deepcopy(channel_bodies['free'])
            changed['model_tags']['native'] = ['privacy:unknown']
            call(f"/api/channels/{channels['free']}", {**changed, 'id': channels['free']}, 'PUT')
            call(calls[0][0], calls[0][1], 'POST', token, 403)
            changed['model_tags']['native'] = ['privacy:zdr']
            changed['tags'] = ['billing:free', 'privacy:train']
            call(f"/api/channels/{channels['free']}", {**changed, 'id': channels['free']}, 'PUT')
            call(calls[0][0], calls[0][1], 'POST', token)
            changed['model_tags'].pop('native')
            call(f"/api/channels/{channels['free']}", {**changed, 'id': channels['free']}, 'PUT')
            call(calls[0][0], calls[0][1], 'POST', token, 403)
            call(f"/api/channels/{channels['free']}", {**channel_bodies['free'], 'id': channels['free']}, 'PUT')
            call(calls[0][0], calls[0][1], 'POST', token)
            checks.append('model override and explicit unknown stay independent; restoring inherit follows current channel')

            custom_scope = scope(['coding'], {'purpose': ['purpose:code']})
            changed = copy.deepcopy(channel_bodies['free'])
            changed['model_tags']['native'] = ['purpose:code', 'purpose:writing']
            call(f"/api/channels/{channels['free']}", {**changed, 'id': channels['free']}, 'PUT')
            custom = call('/api/client-keys', {'name': '自定义用途筛选', 'scope': custom_scope}, 'POST', expected=201)
            call(calls[0][0], calls[0][1], 'POST', custom['key'])
            references = call('/api/tags/references?id=purpose%3Acode')
            assert custom['client_key']['name'] in references['client_key_names']
            call('/api/tags/purpose%3Acode?force=true', method='DELETE', expected=409)
            invalid = copy.deepcopy(changed)
            invalid['tags'] = ['billing:free', 'billing:payg']
            call(f"/api/channels/{channels['free']}", {**invalid, 'id': channels['free']}, 'PUT', expected=400)
            checks.append('custom groups support OR; key references block forced deletion; conflicting billing rejected')

            rename_key = call('/api/client-keys', {'name': '改名与删除引用', 'scope': scope(['paid-only'])}, 'POST', expected=201)
            call('/api/models/paid-only/rename', public('paid-renamed', ['paid']), 'POST')
            updated_key = next(k for k in call('/api/client-keys') if k['id'] == rename_key['client_key']['id'])
            assert updated_key['scope']['model_ids'] == ['paid-renamed']
            call('/api/models/paid-renamed', method='DELETE', expected=(200, 204))
            updated_key = next(k for k in call('/api/client-keys') if k['id'] == rename_key['client_key']['id'])
            assert updated_key['scope']['model_mode'] == 'selected' and updated_key['scope']['model_ids'] == []
            call(calls[0][0], calls[0][1], 'POST', rename_key['key'], 403)
            checks.append('public model rename updates key references atomically; deletion never changes selected scope to all')

            call('/api/models', public('overwrite-removed', ['paid']), 'POST')
            overwrite_key = call('/api/client-keys', {'name': '覆盖导入引用',
                'scope': scope(['coding', 'overwrite-removed'])}, 'POST', expected=201)
            call('/api/models/import', {'mode': 'overwrite', 'models': [public('coding', ['paid', 'free']),
                public('unknown', ['unknown'])], 'prices': []}, 'POST')
            updated_key = next(k for k in call('/api/client-keys') if k['id'] == overwrite_key['client_key']['id'])
            assert updated_key['scope']['model_ids'] == ['coding']
            call('/api/models', public('overwrite-removed', ['paid']), 'POST')
            call(calls[0][0], {**calls[0][1], 'model': 'overwrite-removed'}, 'POST', overwrite_key['key'], 403)
            call('/api/models/overwrite-removed', method='DELETE', expected=(200, 204))
            checks.append('overwrite import preserves retained permissions and removes deleted IDs; recreating old ID does not reauthorize it')

            call('/api/tag-groups', {'id': 'cleanup', 'name': '可删除的未知组'}, 'POST', expected=201)
            with_unknown = copy.deepcopy(changed)
            with_unknown['tags'].append('cleanup:unknown')
            with_unknown['model_tags']['native'].append('cleanup:unknown')
            call(f"/api/channels/{channels['free']}", {**with_unknown, 'id': channels['free']}, 'PUT')
            call('/api/models/coding', {**public('coding', ['paid', 'free']), 'tags': ['cleanup:unknown']}, 'PUT')
            call('/api/tag-groups/cleanup', method='DELETE')
            saved_channel = next(c for c in call('/api/channels') if c['id'] == channels['free'])
            assert 'cleanup:unknown' not in saved_channel.get('tags', [])
            assert 'cleanup:unknown' not in saved_channel['model_tags']['native']
            assert 'cleanup:unknown' not in next(m for m in call('/api/models') if m['id'] == 'coding')['tags']
            checks.append('unknown-only group deletion cleans ordinary references without leaving an undeletable group')

            # Opaque continuation ownership cannot be established by excluding
            # other domains from a key scope. No session/prompt-cache ID is used.
            for route in ['paid', 'free']:
                body = copy.deepcopy(changed if route == 'free' else channel_bodies[route])
                for endpoint in body['endpoints']:
                    if endpoint['format'] == 'openai_responses':
                        endpoint['state'] = 'server_state'
                call(f"/api/channels/{channels[route]}", {**body, 'id': channels[route]}, 'PUT')
            call('/api/models', public('stateful', ['paid', 'free']), 'POST')
            state_key = call('/api/client-keys', {'name': '状态续接边界', 'scope': scope(['stateful'])}, 'POST', expected=201)
            call('/v1/responses', {'model': 'stateful', 'input': 'hi', 'store': True}, 'POST', state_key['key'])
            assert upstream_calls[-1]['route'] == 'paid'
            continuation = {'model': 'stateful', 'input': 'continue', 'previous_response_id': 'synthetic-response'}
            before = len(upstream_calls)
            call('/v1/responses', continuation, 'POST', state_key['key'], 409)
            call(f"/api/client-keys/{state_key['client_key']['id']}", {'name': '状态续接边界',
                'scope': scope(['stateful'], {'billing': ['billing:free']})}, 'PUT')
            call('/v1/responses', continuation, 'POST', state_key['key'], 409)
            assert len(upstream_calls) == before
            checks.append('narrowing scope cannot forward unowned native continuation to a different provider')

            paid_before = sum(r['route'] == 'paid' for r in upstream_calls)
            failed_routes.add('free')
            call(calls[0][0], calls[0][1], 'POST', token, (429, 503))
            assert sum(r['route'] == 'paid' for r in upstream_calls) == paid_before
            failed_routes.clear()
            checks.append('failure on permitted path never falls back to a forbidden paid upstream')
            logs = call('/api/logs?limit=100')
            local_denials = [entry for entry in logs if entry.get('status') == 403 and entry.get('not_sent')]
            assert local_denials and all(not entry.get('input_tokens') and not entry.get('output_tokens')
                and not entry.get('actual_cost') and not entry.get('official_value') for entry in local_denials)
            checks.append('permission failures logged as not sent with no billed usage')

            expected_keys = {k['id']: k['scope'] for k in call('/api/client-keys')}
            proc.terminate()
            proc.wait(timeout=10)
            proc = subprocess.Popen([str(exe), '-data', directory, '-port', str(port)],
                                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL, creationflags=flags)
            for _ in range(150):
                try:
                    call('/api/auth/local', {}, 'POST')
                    break
                except OSError:
                    assert proc.poll() is None, 'isolated gateway exited on reopen'
                    time.sleep(.1)
            else:
                raise AssertionError('isolated gateway did not reopen')
            assert {k['id']: k['scope'] for k in call('/api/client-keys')} == expected_keys
            assert call(f'/api/client-keys/{kid}/key')['key'] == token
            assert [m['id'] for m in call('/v1/models', client=token)['data']] == ['coding']
            checks.append('reopening stored database preserves scope and original encrypted key')

            result = {'status': 'passed', 'exe_sha256': hashlib.sha256(exe.read_bytes()).hexdigest(),
                      'checks': checks, 'upstream_calls': upstream_calls,
                      'boundary': 'real isolated gateway and controlled loopback upstream; no vendor/account validation'}
            args.evidence_file.parent.mkdir(parents=True, exist_ok=True)
            args.evidence_file.write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
            print(json.dumps({'status': 'passed', 'checks': len(checks), 'upstream_calls': len(upstream_calls)}), flush=True)
            if args.state_file:
                args.state_file.parent.mkdir(parents=True, exist_ok=True)
                args.state_file.write_text(json.dumps({'synthetic': True, 'origin': origin, 'pid': proc.pid,
                    'data_directory': directory, 'channel_ids': channels, 'limited_key_id': kid}, indent=2), encoding='utf-8')
            if args.hold_seconds:
                print('UI_ORIGIN=' + origin, flush=True)
                until = time.monotonic() + args.hold_seconds
                while time.monotonic() < until and not (args.stop_file and args.stop_file.exists()):
                    time.sleep(.25)
        finally:
            if proc and proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
            upstream.shutdown()
            upstream.server_close()
            upstream_thread.join(timeout=5)
    print('ISOLATED_DATA_CLEANED', flush=True)


if __name__ == '__main__':
    main()
