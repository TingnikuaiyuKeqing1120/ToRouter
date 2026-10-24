"""Management boundaries against a real executable and a disposable database.

No real provider calls. Optional UI hold ends on the supplied stop file, allowing
normal process/database cleanup after browser checks without killing this runner.
"""
import argparse
import copy
import hashlib
import http.cookiejar
import json
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def price(channel, model, value=None):
    return {'channel_id': channel, 'model': model, 'origin': 'manual',
            'selection': {'mode': 'manual', 'table': {'currency': 'USD', 'unit_tokens': 1000000,
            'rates': {'input': value, 'output': value, 'cache_read': None, 'cache_write': None}, 'rules': []}}}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--exe', type=Path, required=True)
    parser.add_argument('--evidence-file', type=Path, required=True)
    parser.add_argument('--hold-seconds', type=int, default=0)
    parser.add_argument('--stop-file', type=Path)
    args = parser.parse_args()
    exe = args.exe.resolve(strict=True)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    origin = f'http://127.0.0.1:{port}'
    with tempfile.TemporaryDirectory(prefix='torouter-management-') as data:
        flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
        key_output = subprocess.run([str(exe), '-data', data, '-show-key'], capture_output=True,
                                    text=True, encoding='utf-8', creationflags=flags, check=True)
        admin = re.search(r'tr_[A-Za-z0-9_-]+', key_output.stdout).group(0)
        proc = subprocess.Popen([str(exe), '-data', data, '-port', str(port)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=flags)
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

        def call(path, body=None, method='GET', expected=200):
            request = urllib.request.Request(origin + path, data=None if body is None else json.dumps(body).encode(),
                                             method=method, headers={'Content-Type': 'application/json', 'X-To-Router': '1'})
            try:
                response = opener.open(request, timeout=15)
            except urllib.error.HTTPError as error:
                response = error
            raw = response.read()
            assert response.status == expected, (path, response.status, raw[:500])
            return json.loads(raw) if raw else None

        def model(identity, upstream='up', name='', tags=None):
            return {'id': identity, 'display_name': name, 'enabled': True, 'fallback': True, 'tags': tags or [],
                    'bindings': [{'channel_id': channel['id'], 'model': upstream, 'priority': 0, 'enabled': True}]}

        checks = []
        try:
            for _ in range(100):
                try:
                    call('/api/auth/login', {'key': admin}, 'POST')
                    break
                except OSError:
                    assert proc.poll() is None, 'isolated process exited'
                    time.sleep(.1)
            else:
                raise AssertionError('isolated process did not become ready')
            channel_body = {'name': 'isolated acceptance', 'enabled': True, 'api_key': 'synthetic-acceptance-key',
                            'models': ['up', 'other', 'draft'], 'tags': ['billing:payg'],
                            'endpoints': [{'format': 'openai_chat_completions', 'base_url': 'http://127.0.0.1:9'}]}
            channel = call('/api/channels', channel_body, 'POST')
            cid = channel['id']
            catalog = call('/api/catalog/models?source=builtin')
            source_a = next(item for item in catalog if item['id'] == 'gpt-6.1-sol')
            source_b = next(item for item in catalog if item['id'] == 'claude-sonnet-5-5')

            def ref(item):
                return {'source': item['source'], 'id': item['id'], 'offering_key': item['offerings'][0]['key']}

            ref_a, ref_b = ref(source_a), ref(source_b)
            specs = {'thinking_efforts': [], 'native_thinking': {'supported': True, 'modes': [
                {'id': 'reasoning', 'api_format': 'openai_chat_completions', 'control': 'effort', 'efforts': ['low', 'high']}]}}
            call('/api/model-specs', {'channel_id': cid, 'model': 'up', 'specs': specs, 'catalog_model_ref': ref_a}, 'PUT')
            spec_before = next(row for row in call('/api/model-specs') if row['channel_id'] == cid and row['model'] == 'up')
            call('/api/models', model('thinking-empty'), 'POST')
            assert next(row for row in call('/api/models/specs')['data'] if row['id'] == 'thinking-empty')['thinking']['options'] == []
            checks.append('known empty thinking projection')

            call('/api/models', model('rename:source', name='Source'), 'POST')
            call('/api/models', model('rename:target', 'other', 'Existing target'), 'POST')
            call('/api/prices', [price(0, 'rename:source', 8), price(cid, 'up', 11)], 'PUT')
            before_models = call('/api/models')
            call('/api/models', model('rename:target', name='Must not overwrite'), 'POST', expected=409)
            call('/api/models/rename%3Asource/rename', model('rename:target', name='Must not overwrite'), 'POST', expected=409)
            assert call('/api/models') == before_models, 'rejected create/rename changed models'
            call('/api/models/rename%3Asource/rename', model('rename:complete', 'other', 'Renamed'), 'POST')
            renamed = next(row for row in call('/api/models') if row['id'] == 'rename:complete')
            assert renamed['display_name'] == 'Renamed' and renamed['bindings'][0]['model'] == 'other'
            saved_prices = call('/api/prices')
            assert not any(row['id'] == 'rename:source' for row in call('/api/models'))
            for identity in ['rename:source', 'rename:complete']:
                matching = [p for p in saved_prices if p['channel_id'] == 0 and p['model'] == identity]
                assert len(matching) == 1 and matching[0]['selection']['table']['rates']['input'] == 8
                assert 'role' not in matching[0]
            checks.append('encoded rename rejects collisions, saves whole model, copies official price and retains old prices')

            stale_settings = call('/api/settings')
            latest_prices = copy.deepcopy(saved_prices)
            next(p for p in latest_prices if p['channel_id'] == cid and p['model'] == 'up')['selection']['table']['rates']['input'] = 99
            call('/api/prices', latest_prices, 'PUT')
            call('/api/settings', stale_settings, 'PUT', expected=400)
            clean_settings = {k: v for k, v in stale_settings.items() if k != 'prices'}
            clean_settings['retention_days'] = 31
            call('/api/settings', clean_settings, 'PUT')
            call(f'/api/channels/{cid}', {**channel_body, 'id': cid, 'associated_prices': []}, 'PUT', expected=400)
            call('/api/models/thinking-empty', {**model('thinking-empty'), 'associated_prices': []}, 'PUT', expected=400)
            assert call('/api/prices') == latest_prices
            assert call('/api/settings')['retention_days'] == 31
            checks.append('ordinary settings/channel/model saves cannot write prices')

            prefill = {'targets': [{'kind': 'upstream', 'channel_id': cid, 'model': 'up', 'ref': ref_b,
                        'price_ref': {**ref_b, 'quote_key': source_b['offerings'][0]['quotes'][0]['key']}}],
                       'groups': ['price'], 'price_mode': 'manual', 'force_overwrite': False}
            result = call('/api/catalog/prefill/apply', prefill, 'POST')
            spec_after = next(row for row in call('/api/model-specs') if row['channel_id'] == cid and row['model'] == 'up')
            assert spec_after['specs'] == spec_before['specs'] and spec_after['catalog_model_ref'] == ref_a
            assert result['updated'] == 0 and call('/api/prices') == latest_prices
            checks.append('price-only prefill leaves protected prices and specification association unchanged')

            draft = price(cid, 'draft')
            draft['selection']['table'].update({'unit_tokens': 37, 'uncovered_conditions': ['keep note'],
                'rules': [{'id': 'disabled-rule', 'enabled': False, 'when': {}, 'action': {}, 'draft': {'keep': 'draft'}}]})
            call('/api/prices', call('/api/prices') + [draft], 'PUT')
            before = call('/api/prices')
            prefill['targets'][0]['model'] = 'draft'
            call('/api/catalog/prefill/apply', prefill, 'POST')
            assert call('/api/prices') == before
            prefill['force_overwrite'] = True
            call('/api/catalog/prefill/apply', prefill, 'POST')
            assert next(p for p in call('/api/prices') if p['channel_id'] == cid and p['model'] == 'draft')['selection']['table']['rates']['input'] is not None
            checks.append('non-force preserves unfinished price rules/unit/note; explicit force replaces')

            draft_request = {'targets': [{'kind': 'downstream', 'model': 'independent-price', 'ref': ref_a,
                              'price_draft': price(0, 'independent-price', 123)['selection']}],
                             'groups': ['price'], 'price_mode': 'manual'}
            before_prices, before_models = call('/api/prices'), call('/api/models')
            preview = call('/api/catalog/prefill/preview', draft_request, 'POST')
            assert preview['patches'][0].get('price') is None
            draft_request['force_overwrite'] = True
            preview = call('/api/catalog/prefill/preview', draft_request, 'POST')
            assert preview['patches'][0].get('price') is not None
            call('/api/catalog/prefill/apply', draft_request, 'POST', expected=400)
            assert call('/api/prices') == before_prices and call('/api/models') == before_models
            checks.append('independent current price draft is protected, preview-only and never creates routing entities')

            call('/api/tags/billing%3Apayg?force=true', method='DELETE', expected=400)
            assert next(row for row in call('/api/channels') if row['id'] == cid)['tags'] == ['billing:payg']
            call('/api/tags', {'id': 'custom:audit', 'group_id': 'billing', 'name': 'Audit'}, 'POST', expected=201)
            call(f'/api/channels/{cid}', {**channel_body, 'id': cid, 'tags': ['billing:payg', 'custom:audit']}, 'PUT')
            call('/api/models/thinking-empty', model('thinking-empty', tags=['custom:audit']), 'PUT')
            call('/api/tags/custom%3Aaudit', method='DELETE', expected=409)
            assert 'custom:audit' in next(row for row in call('/api/channels') if row['id'] == cid)['tags']
            call('/api/tags/custom%3Aaudit?force=true', method='DELETE')
            assert next(row for row in call('/api/channels') if row['id'] == cid)['tags'] == ['billing:payg']
            assert next(row for row in call('/api/models') if row['id'] == 'thinking-empty')['tags'] == []
            assert not any(row['id'] == 'custom:audit' for row in call('/api/tags'))
            checks.append('encoded tag rejection keeps references; confirmed deletion detaches both owners')

            # Stable fixtures for the optional browser portion; do not save UI drafts automatically.
            call('/api/models', {**model('gpt-6.1-sol'), 'catalog_model_ref': ref_a}, 'POST')
            call('/api/prices', call('/api/prices') + [price(0, 'gpt-6.1-sol')], 'PUT')
            connection = call('/api/connection')
            assert connection['origin'] == origin and connection['port'] == port
            checks.append('connection contract reports actual custom port')
            result = {'status': 'passed', 'exe_sha256': hashlib.sha256(exe.read_bytes()).hexdigest(),
                      'checks': checks, 'evidence_boundary': 'isolated real process and synthetic configuration; no provider calls'}
            args.evidence_file.parent.mkdir(parents=True, exist_ok=True)
            args.evidence_file.write_text(json.dumps(result, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
            print(json.dumps(result, ensure_ascii=False, indent=2), flush=True)
            if args.hold_seconds:
                print('UI_ORIGIN='+origin, flush=True)
                until = time.monotonic()+args.hold_seconds
                while time.monotonic() < until and not (args.stop_file and args.stop_file.exists()):
                    time.sleep(.25)
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
    print('ISOLATED_DATA_CLEANED', flush=True)


if __name__ == '__main__':
    main()
