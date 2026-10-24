#!/usr/bin/env python3
"""T087 runtime status against a real EXE and disposable, local-only fixtures.

Run with --exe <CLI EXE> --evidence-file <receipt.json>. No build or real account
is involved. The mock only accepts Chat Completions; Gemini ingress must convert
to that same outgoing combination. --hold-seconds keeps a stable red fixture for
browser checks, ending early on --stop-file. Temporary UI credentials are written
only to <receipt>.fixture.json, never to stdout or the ordinary evidence receipt.

This does not prove real DeepSeek/Codex quota compatibility, an external client's
behavior, or Wails behavior. Authenticated quota refresh intentionally targets an
unsupported local credential: it must reject without contacting any provider.
"""

import argparse
import copy
from datetime import datetime, timezone
import hashlib
import http.cookiejar
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


class SyntheticUpstream:
    """Thread-safe responses and wire observations, keyed by model and key."""

    def __init__(self):
        self.lock = threading.Lock()
        self.responses = {}
        self.hits = []
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_POST(self):
                try:
                    length = int(self.headers.get('Content-Length', '0'))
                    if not 0 < length <= 1024 * 1024:
                        raise ValueError('invalid body size')
                    body = json.loads(self.rfile.read(length))
                    model = body.get('model', '')
                except (ValueError, TypeError):
                    self.send_error(400, 'invalid fixture request')
                    return
                key = self.headers.get('Authorization', '').removeprefix('Bearer ')
                with fixture.lock:
                    fixture.hits.append({'path': self.path, 'model': model, 'key': key})
                    status, retry_after = fixture.responses.get((model, key), (200, None))
                if self.path != '/v1/chat/completions':
                    status = 404
                if status == 200:
                    result = {'id': 'synthetic-chat', 'object': 'chat.completion',
                              'created': int(time.time()), 'model': model,
                              'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'fixture-ok'},
                                           'finish_reason': 'stop'}],
                              'usage': {'prompt_tokens': 5, 'completion_tokens': 2, 'total_tokens': 7}}
                else:
                    result = {'error': {'type': 'rate_limit_error', 'message': 'synthetic fixture failure'}}
                payload = json.dumps(result).encode('utf-8')
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(payload)))
                if status == 429 and retry_after is not None:
                    self.send_header('Retry-After', str(retry_after))
                self.end_headers()
                self.wfile.write(payload)

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.server.daemon_threads = True
        self.origin = f'http://127.0.0.1:{self.server.server_address[1]}'
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self):
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def set_response(self, model, key, status, retry_after=1):
        with self.lock:
            self.responses[(model, key)] = (status, retry_after)

    def observations(self):
        with self.lock:
            return copy.deepcopy(self.hits)


def write_receipt(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')


def elapsed(timestamp):
    return datetime.fromisoformat(timestamp.replace('Z', '+00:00')) <= datetime.now(timezone.utc)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--exe', type=Path, required=True)
    parser.add_argument('--evidence-file', type=Path, required=True)
    parser.add_argument('--hold-seconds', type=int, default=0)
    parser.add_argument('--stop-file', type=Path)
    args = parser.parse_args()
    if args.hold_seconds < 0:
        parser.error('--hold-seconds must be nonnegative')
    exe = args.exe.resolve(strict=True)
    evidence_file = args.evidence_file.resolve()
    fixture_file = evidence_file.with_suffix('.fixture.json')
    if evidence_file.name.endswith('.fixture.json'):
        parser.error('--evidence-file must not end in .fixture.json')
    checks = []
    result = {'status': 'running', 'exe_sha256': hashlib.sha256(exe.read_bytes()).hexdigest(),
              'checks': checks, 'evidence_boundary':
              'real isolated app/API and synthetic loopback upstream; no real provider, account, user database or browser acceptance'}
    upstream = SyntheticUpstream()
    upstream.start()
    fixture_receipt = None
    try:
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            port = listener.getsockname()[1]
        origin = f'http://127.0.0.1:{port}'
        # Disable environment proxies for loopback, including custom user
        # proxy settings that could otherwise send synthetic traffic elsewhere.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                    urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        anonymous = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
        with tempfile.TemporaryDirectory(prefix='torouter-runtime-status-') as data:
            key_output = subprocess.run([str(exe), '-data', data, '-show-key'],
                    capture_output=True, text=True, encoding='utf-8', errors='replace',
                    creationflags=flags, check=True, timeout=60)
            match = re.search(r'tr_[A-Za-z0-9_-]+', key_output.stdout)
            if match is None:
                raise AssertionError('the isolated EXE did not return a test admin key')
            admin = match.group(0)
            proc = subprocess.Popen([str(exe), '-data', data, '-port', str(port)],
                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL, creationflags=flags)
            try:
                def call(path, body=None, method='GET', expected=200, session=True, management=True, headers=None):
                    request_headers = {'Content-Type': 'application/json'}
                    if management:
                        request_headers['X-To-Router'] = '1'
                    request_headers.update(headers or {})
                    request = urllib.request.Request(origin + path,
                            data=None if body is None else json.dumps(body).encode('utf-8'),
                            method=method, headers=request_headers)
                    try:
                        response = (opener if session else anonymous).open(request, timeout=15)
                    except urllib.error.HTTPError as error:
                        response = error
                    with response:
                        raw = response.read()
                        if response.status != expected:
                            # Do not include raw authentication bodies/headers in
                            # a failed receipt; route and status identify the case.
                            raise AssertionError(f'{method} {path}: HTTP {response.status}, expected {expected}')
                    return json.loads(raw) if raw else None

                def require(name, condition):
                    if not condition:
                        raise AssertionError(name)

                def passed(name):
                    checks.append(name)
                    print('PASS ' + name, flush=True)

                for _ in range(150):
                    try:
                        call('/api/auth/login', {'key': admin}, 'POST')
                        break
                    except OSError:
                        if proc.poll() is not None:
                            raise AssertionError('isolated process exited before login')
                        time.sleep(.1)
                else:
                    raise AssertionError('isolated process did not become ready')

                call('/api/status', expected=401, session=False)
                for path in ['/api/status/reset', '/api/status/refresh-quota']:
                    # Auth rejects before decoding. An unread request body on
                    # urllib's closing socket can hide that refusal behind a
                    # Windows TCP reset; these auth-only checks need no body.
                    call(path, None, 'POST', expected=401, session=False)
                    call(path, None, 'POST', expected=403, management=False)
                    call(path, {}, 'POST', expected=400)
                passed('runtime API requires an admin session and write header; malformed authenticated targets are rejected')

                settings = call('/api/settings')
                require('settings expose new cooldown field and retire old name',
                        'failure_cooldown_seconds' in settings and 'retry_after_max_seconds' not in settings)
                settings.pop('prices', None)
                settings.update({'failure_cooldown_seconds': 1, 'retries_per_channel': 1, 'max_attempts': 1})
                call('/api/settings', settings, 'PUT')
                saved = call('/api/settings')
                require('cooldown and retry settings round-trip',
                        saved['failure_cooldown_seconds'] == 1 and saved['retries_per_channel'] == 1
                        and 'retry_after_max_seconds' not in saved)
                passed('failure_cooldown_seconds=1 replaces the retired setting and retries_per_channel=1 round-trips')

                client_key = call('/api/client-keys', {'name': 'runtime fixture only'}, 'POST', expected=201)['key']
                client_headers = {'Authorization': 'Bearer ' + client_key}

                def create_channel(name, models, credentials):
                    body = {'name': name, 'enabled': True, 'models': models,
                            'credential_type': 'api_key', 'dispatch_mode': 'failover', 'chain_type': 'unidirectional',
                            'endpoints': [{'format': 'openai_chat_completions', 'base_url': upstream.origin + '/v1'}],
                            'credentials': [{'id': identity, 'label': label, 'api_key': key, 'enabled': True}
                                            for identity, label, key in credentials]}
                    return call('/api/channels', body, 'POST')

                def create_model(identity, channel_id, upstream_model):
                    call('/api/models', {'id': identity, 'display_name': identity, 'enabled': True, 'fallback': False,
                         'bindings': [{'channel_id': channel_id, 'model': upstream_model, 'priority': 0, 'enabled': True}]}, 'POST')

                main_channel = create_channel('Runtime A cooldown', ['unstable', 'healthy'],
                                                [('main', 'Main test credential', 'synthetic-main')])
                main_id = main_channel['id']
                create_model('runtime-unstable', main_id, 'unstable')
                create_model('runtime-healthy', main_id, 'healthy')
                main_target = {'channel_id': main_id, 'credential_id': 'main'}

                def turn(model='runtime-unstable', expected=200, gemini=False):
                    if gemini:
                        path = '/v1beta/models/' + urllib.parse.quote(model, safe='') + ':generateContent'
                        body = {'contents': [{'role': 'user', 'parts': [{'text': 'fixture request'}]}]}
                    else:
                        path = '/v1/chat/completions'
                        body = {'model': model, 'messages': [{'role': 'user', 'content': 'fixture request'}], 'stream': False}
                    return call(path, body, 'POST', expected=expected, session=False,
                                management=False, headers=client_headers)

                def snapshot():
                    current = call('/api/status')
                    rows = current['rows']
                    require('problem_count equals red credential rows', current['problem_count'] == sum(bool(row['red']) for row in rows))
                    order = [(not row['red'], row['channel_name'], row['channel_id'], row['credential_id']) for row in rows]
                    require('credential rows sort red first then by channel', order == sorted(order))
                    for row in rows:
                        models = [(not item['red'], item['model']) for item in row['models']]
                        require('model rows sort red first then by ID', models == sorted(models))
                    serialized = json.dumps(current)
                    require('status does not expose synthetic credentials',
                            all(key not in serialized for key in ['synthetic-main', 'synthetic-pool-a', 'synthetic-pool-b']))
                    return current

                def row(channel_id=main_id, credential_id='main'):
                    return next(item for item in snapshot()['rows']
                                if item['channel_id'] == channel_id and item['credential_id'] == credential_id)

                def model_status(model='unstable', channel_id=main_id, credential_id='main'):
                    return next(item for item in row(channel_id, credential_id)['models'] if item['model'] == model)

                def wait_cooldown(model='unstable', channel_id=main_id, credential_id='main'):
                    deadline = time.monotonic() + 8
                    while time.monotonic() < deadline:
                        status = model_status(model, channel_id, credential_id)
                        until = status.get('cooldown_until')
                        if until and elapsed(until):
                            # Re-read after the observed deadline. A snapshot
                            # captured just before expiry can arrive just after
                            # it and still correctly contain the former color.
                            return model_status(model, channel_id, credential_id)
                        time.sleep(.05)
                    raise AssertionError('synthetic cooldown did not expire within its bounded wait')

                before_refresh = len(upstream.observations())
                call('/api/status/refresh-quota', main_target, 'POST', expected=400)
                require('unsupported quota refresh does not query upstream', len(upstream.observations()) == before_refresh)
                require('local API channel does not advertise quota support', not row()['quota_supported'])
                call('/api/status/reset', {**main_target, 'model': 'absent-model'}, 'POST', expected=400)
                call('/api/status/reset', {**main_target, 'credential_id': 'absent-credential'}, 'POST', expected=404)
                passed('app mounts reset and quota hooks; unsupported local quota refresh rejects without a provider call')

                # Retry-After avoids long exponential waits while checking the
                # color/count boundary. The first cooldown leaves enough time
                # for both ingress requests and their accounting assertions.
                for count in range(1, 5):
                    upstream.set_response('unstable', 'synthetic-main', 429, 2 if count == 1 else 1)
                    before_hits = len(upstream.observations())
                    turn(expected=429)
                    require('one upstream call per configured attempt', len(upstream.observations()) == before_hits + 1)
                    current = model_status()
                    require('429 increments failure count and turns the combination red',
                            current['red'] and current['failures'] == count and current['last_request']['status'] == 429)
                    require('another model stays independent', not model_status('healthy')['red'] and model_status('healthy')['failures'] == 0)
                    if count == 1:
                        before_usage = call('/api/usage')
                        before_logs = call('/api/logs?limit=1000')
                        turn(expected=503)
                        turn(expected=503, gemini=True)
                        after_logs = call('/api/logs?limit=1000')
                        new_ids = {item['id'] for item in before_logs}
                        skipped = [item for item in after_logs if item['id'] not in new_ids]
                        require('both cooled ingress requests have explicit not_sent logs',
                                len(skipped) == 2 and all(item.get('not_sent') and item['status'] == 503 for item in skipped))
                        require('not_sent logs have no tokens or computed prices', all(
                            all(item[field] == 0 for field in ['input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens'])
                            and item.get('actual_cost') is None and item.get('official_value') is None for item in skipped))
                        require('skips neither reach upstream nor grow usage',
                                len(upstream.observations()) == before_hits + 1 and call('/api/usage') == before_usage)
                        require('ingress protocol does not fork the actual combination failure count', model_status()['failures'] == 1)
                        passed('/v1 and /v1beta share outbound cooldown; not_sent logs remain outside usage, tokens and prices')
                    expired = wait_cooldown()
                    require('expiry preserves failure count and applies 3/4 color boundary',
                            expired['failures'] == count and expired['red'] == (count >= 4))
                require('fourth expired failure remains visibly unavailable', row()['red'] and model_status()['reason'] == '连续不可用')
                passed('first through third cooldowns expire green without clearing counts; fourth expires red')

                upstream.set_response('unstable', 'synthetic-main', 200)
                response = turn(gemini=True)
                require('Gemini ingress is served through the Chat Completions fixture', bool(response.get('candidates')))
                recovered = model_status()
                require('real successful request clears persistent failure state',
                        not recovered['red'] and recovered['failures'] == 0 and recovered['last_request']['status'] == 200)
                passed('an expired persistent-red combination permits a real success that clears failures')

                for public, upstream_model in [('runtime-unstable', 'unstable'), ('runtime-healthy', 'healthy')]:
                    upstream.set_response(upstream_model, 'synthetic-main', 429, 10)
                    turn(public, expected=429)
                before_hits = len(upstream.observations())
                before_config = call('/api/channels')
                call('/api/status/reset', {**main_target, 'model': 'unstable'}, 'POST')
                require('model restore is scoped to the selected combination',
                        model_status()['failures'] == 0 and not model_status()['red'] and model_status('healthy')['red'])
                call('/api/status/reset', main_target, 'POST')
                require('credential restore clears its models without a probe',
                        not row()['red'] and all(item['failures'] == 0 for item in row()['models'])
                        and len(upstream.observations()) == before_hits)
                require('restore does not edit enabled/configuration state', call('/api/channels') == before_config)
                upstream.set_response('unstable', 'synthetic-main', 200)
                upstream.set_response('healthy', 'synthetic-main', 200)
                turn()
                passed('model and whole-credential manual restore are scoped, clear failure state, preserve configuration and send no probe')

                pool = create_channel('Runtime B pool', ['pool-model-a', 'pool-model-b'],
                        [('pool-a', 'Pool A test credential', 'synthetic-pool-a'),
                         ('pool-b', 'Pool B test credential', 'synthetic-pool-b')])
                pool_id = pool['id']
                create_model('runtime-pool-a', pool_id, 'pool-model-a')
                create_model('runtime-pool-b', pool_id, 'pool-model-b')
                upstream.set_response('pool-model-a', 'synthetic-pool-a', 429, 30)
                before_hits = len(upstream.observations())
                turn('runtime-pool-a', expected=429)
                turn('runtime-pool-a')
                turn('runtime-pool-b')
                recent = upstream.observations()[before_hits:]
                require('same-channel pool dispatch selects the actual expected credentials',
                        [(item['model'], item['key']) for item in recent] ==
                        [('pool-model-a', 'synthetic-pool-a'), ('pool-model-a', 'synthetic-pool-b'),
                         ('pool-model-b', 'synthetic-pool-a')])
                require('failed credential/model does not cool its sibling model or sibling credential',
                        row(pool_id, 'pool-a')['red'] and not row(pool_id, 'pool-b')['red']
                        and model_status('pool-model-a', pool_id, 'pool-a')['failures'] == 1
                        and model_status('pool-model-b', pool_id, 'pool-a')['failures'] == 0
                        and model_status('pool-model-a', pool_id, 'pool-b')['last_request']['status'] == 200)
                require('one channel with two credentials projects two rows',
                        len([item for item in snapshot()['rows'] if item['channel_id'] == pool_id]) == 2)
                passed('same-channel models and credentials retain independent state and latest-request metadata; table ordering/counts agree')

                if args.hold_seconds:
                    # Leave a durable red row beside a green sibling model.
                    # After arming, future calls can succeed without a channel
                    # edit; UI restore itself must remain a local operation.
                    for _ in range(4):
                        upstream.set_response('unstable', 'synthetic-main', 429, 1)
                        turn(expected=429)
                        wait_cooldown()
                    require('browser hold has an expired persistent-red fixture', model_status()['red'] and model_status()['failures'] == 4)
                    upstream.set_response('unstable', 'synthetic-main', 200)
                    turn('runtime-healthy')
                    fixture_receipt = {'active': True, 'temporary_local_fixture_only': True,
                         'origin': origin, 'admin_key': admin, 'client_key': client_key,
                         'upstream_origin': upstream.origin, 'main_target': main_target, 'pool_channel_id': pool_id,
                         'status_route': origin + '/#/status', 'stop_file': str(args.stop_file.resolve()) if args.stop_file else None,
                         'browser_checks': ['expand the durable red credential and compare its green sibling model',
                                            'restore a model and whole credential; check sidebar problem count',
                                            'settings show failure cooldown seconds; logs identify not_sent records'],
                         'recovery_request': {'method': 'POST', 'path': '/v1/chat/completions',
                            'body': {'model': 'runtime-unstable', 'messages': [{'role': 'user', 'content': 'fixture recovery'}]}}}
                    write_receipt(fixture_file, fixture_receipt)
                    result['ui_fixture_file'] = str(fixture_file)
                result.update({'status': 'passed', 'upstream_calls': len(upstream.observations()),
                               'final_status': snapshot(), 'final_usage': call('/api/usage')})
                write_receipt(evidence_file, result)
                print(json.dumps({'status': 'passed', 'checks': len(checks), 'evidence_file': str(evidence_file)}, ensure_ascii=False), flush=True)
                if args.hold_seconds:
                    print('UI_FIXTURE_FILE=' + str(fixture_file), flush=True)
                    until = time.monotonic() + args.hold_seconds
                    while time.monotonic() < until and not (args.stop_file and args.stop_file.exists()):
                        if proc.poll() is not None:
                            raise AssertionError('isolated process exited during UI hold')
                        time.sleep(.25)
            finally:
                if proc.poll() is None:
                    proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait(timeout=10)
                if fixture_receipt is not None:
                    fixture_receipt['active'] = False
                    fixture_receipt.pop('admin_key', None)
                    fixture_receipt.pop('client_key', None)
                    write_receipt(fixture_file, fixture_receipt)
    except Exception as error:
        result.update({'status': 'failed', 'failure': str(error), 'failure_type': type(error).__name__})
        write_receipt(evidence_file, result)
        raise
    finally:
        upstream.close()
    result['isolated_data_cleaned'] = True
    write_receipt(evidence_file, result)
    print('ISOLATED_DATA_CLEANED', flush=True)


if __name__ == '__main__':
    main()
