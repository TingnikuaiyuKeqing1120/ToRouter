"""T092 backfill acceptance against an isolated EXE and synthetic SQLite data.

Default: exercise real HTTP preview/apply and verify the persisted history.
With --state-file and --stop-file: only prepare the same untouched fixture and
hold it for browser checks until the stop file appears. No provider is called.
"""
import argparse
import copy
from contextlib import closing
from datetime import datetime, timedelta, timezone
import hashlib
import http.cookiejar
import json
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def table(rate, conditional=False):
    result = {'currency': 'USD', 'unit_tokens': 1,
              'rates': dict.fromkeys(('input', 'output', 'cache_read', 'cache_write'), rate), 'rules': []}
    if conditional:
        result['rules'] = [{'id': 'historical-night', 'enabled': True,
                            'when': {'time_window': {'start': '22:00', 'end': '06:00', 'timezone': 'UTC'}},
                            'action': {'multiplier': 2}},
                           {'id': 'keep-disabled-draft', 'enabled': False, 'when': {}, 'action': {},
                            'draft': {'note': 'retain for local edits', 'one': 1.0, 'thousand': 1e3}}]
        result['uncovered_conditions'] = ['synthetic historical condition note']
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--exe', type=Path, required=True)
    parser.add_argument('--evidence-file', type=Path)
    parser.add_argument('--state-file', type=Path)
    parser.add_argument('--stop-file', type=Path)
    args = parser.parse_args()
    if bool(args.state_file) != bool(args.stop_file):
        parser.error('--state-file and --stop-file must be supplied together')
    if args.stop_file and args.stop_file.exists():
        parser.error('stop file already exists; choose a new stop-file path')
    exe = args.exe.resolve(strict=True)
    flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    origin = f'http://127.0.0.1:{port}'
    with tempfile.TemporaryDirectory(prefix='torouter-backfill-') as data:
        # Initialize only this disposable directory; never display its key.
        subprocess.run([str(exe), '-data', data, '-show-key'], stdout=subprocess.DEVNULL,
                       stderr=subprocess.PIPE, creationflags=flags, check=True)
        proc = subprocess.Popen([str(exe), '-data', data, '-port', str(port)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=flags)
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

        def call(path, body=None, method='GET', expected=200):
            request = urllib.request.Request(origin + path,
                data=None if body is None else json.dumps(body).encode(), method=method,
                headers={'Content-Type': 'application/json', 'X-To-Router': '1'})
            try:
                response = opener.open(request, timeout=30)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                raw = response.read()
                assert response.status == expected, (path, response.status, raw[:500])
                return json.loads(raw) if raw else None

        database = Path(data) / 'router.db'

        def history():
            with closing(sqlite3.connect(database, timeout=10)) as db:
                return {identity: json.loads(raw) for identity, raw in db.execute('SELECT id,data FROM usage_logs ORDER BY id')}

        def history_digest():
            return hashlib.sha256(json.dumps(history(), sort_keys=True).encode()).hexdigest()

        try:
            for _ in range(100):
                try:
                    call('/api/auth/local', {}, 'POST')
                    break
                except OSError:
                    assert proc.poll() is None, 'isolated process exited'
                    time.sleep(.1)
            else:
                raise AssertionError('isolated process did not become ready')

            channel_models = [('Alpha API', ['vendor/shared-v2']),
                              ('Beta API', ['vendor/shared-v2', 'vendor/manual-b']),
                              ('Gamma Manual', ['vendor/manual-a', 'vendor/unselected'])]
            channels = []
            for name, models in channel_models:
                channels.append(call('/api/channels', {'name': name, 'models': models, 'available_models': models,
                    'enabled': True, 'api_key': 'synthetic-local-only',
                    'endpoints': [{'format': 'openai_chat_completions', 'base_url': 'http://127.0.0.1:9'}]}, 'POST'))
            alpha, beta, gamma = [channel['id'] for channel in channels]
            bindings = {'public-main': [(alpha, 'vendor/shared-v2'), (beta, 'vendor/shared-v2')],
                        'public-alias': [(alpha, 'vendor/shared-v2')],
                        'public-manual-a': [(gamma, 'vendor/manual-a')],
                        'public-manual-b': [(beta, 'vendor/manual-b')],
                        'public-unselected': [(gamma, 'vendor/unselected')]}
            for model, entries in bindings.items():
                call('/api/models', {'id': model, 'display_name': model, 'enabled': True,
                    'bindings': [{'channel_id': channel, 'model': upstream, 'priority': 0, 'enabled': True}
                                 for channel, upstream in entries]}, 'POST')
            prices = [{'channel_id': channel, 'model': model, 'origin': 'manual',
                       'selection': {'mode': 'manual', 'table': table(rate, conditional=True)}}
                      for channel, model, rate in [(alpha, 'vendor/shared-v2', 1),
                          (beta, 'vendor/shared-v2', 5), (0, 'public-main', 3),
                          (0, 'public-alias', 3)]]
            call('/api/prices', prices, 'PUT')
            saved_prices = call('/api/prices')
            at = (datetime.now(timezone.utc) - timedelta(days=1)).replace(hour=23, minute=0, second=0, microsecond=0)
            names = {channel['id']: channel['name'] for channel in channels}
            fixture_ids = {}

            def insert(label, channel=alpha, model='public-main', upstream='vendor/shared-v2', **changes):
                record = {'time': at.isoformat(), 'channel_id': channel, 'channel_name': names[channel],
                          'model': model, 'upstream_model': upstream, 'status': 200, 'estimated': False,
                          'input_tokens': 2, 'output_tokens': 1, 'cache_read_tokens': 0, 'cache_write_tokens': 0,
                          'actual_cost': None, 'official_value': None, **changes}
                stamp = int(datetime.fromisoformat(record['time']).timestamp()*1000)
                with closing(sqlite3.connect(database, timeout=10)) as db:
                    cursor = db.execute('INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,?,?,?,?)',
                        (stamp, channel, model, record['status'], json.dumps(record, ensure_ascii=False)))
                    fixture_ids[label] = cursor.lastrowid
                    db.commit()

            insert('current-a')
            insert('current-alias', model='public-alias')
            insert('current-beta', channel=beta)
            insert('known-zero', actual_cost=0, currency='USD', official_value=0, official_currency='USD')
            insert('unknown-current', estimated=True)
            insert('manual-a', channel=gamma, model='public-manual-a', upstream='vendor/manual-a')
            insert('manual-b', channel=beta, model='public-manual-b', upstream='vendor/manual-b')
            insert('unknown-manual', channel=gamma, model='public-manual-a', upstream='vendor/manual-a', estimated=True)
            insert('unselected', channel=gamma, model='public-unselected', upstream='vendor/unselected')
            insert('before-time', time=(at-timedelta(days=1)).isoformat())
            insert('after-time', time=(at+timedelta(hours=1)).isoformat())
            insert('not-sent', not_sent=True)
            insert('unknown-identity-a', channel=gamma, model='public-identity-a', upstream='')
            insert('unknown-identity-b', channel=gamma, model='public-identity-b', upstream='')
            with closing(sqlite3.connect(database, timeout=10)) as db:
                rate = {'available': True, 'base': 'USD', 'quote': 'CNY', 'rate': 7.2,
                        'date': datetime.now(timezone.utc).date().isoformat(),
                        'attempted_on': datetime.now(timezone.utc).date().isoformat(),
                        'checked_at': datetime.now(timezone.utc).isoformat(),
                        'fetched_at': datetime.now(timezone.utc).isoformat(), 'stale': False,
                        'error': '', 'source_name': 'Isolated fixture', 'source_id': 'fixture'}
                db.execute("INSERT OR REPLACE INTO meta(key,value) VALUES('exchange_usd_cny_v1',?)", (json.dumps(rate),))
                db.commit()

            fixture_hash = history_digest()
            if args.state_file:
                state = {'status': 'ready', 'origin': origin, 'data_dir': data, 'pid': proc.pid,
                         'records': len(fixture_ids), 'fixture_ids': fixture_ids, 'fixture_sha256': fixture_hash,
                         'channels': [{'id': c['id'], 'name': c['name']} for c in channels],
                         'filter_time': at.isoformat(), 'synthetic': True,
                         'exe_sha256': hashlib.sha256(exe.read_bytes()).hexdigest()}
                args.state_file.parent.mkdir(parents=True, exist_ok=True)
                args.state_file.write_text(json.dumps(state, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
                print('UI_ORIGIN='+origin, flush=True)
                while not args.stop_file.exists():
                    assert proc.poll() is None, 'isolated preview process exited'
                    time.sleep(.25)
            else:
                checks = []

                def preview(request):
                    return call('/api/usage/backfill/preview', request, 'POST')

                def apply_request(result):
                    payload = {**result['request'], 'expected_resolution': result['expected_resolution']}
                    # Browser JSON.parse/stringify drops redundant .0 on numbers;
                    # formatting-only changes in disabled drafts must not conflict.
                    return json.loads(json.dumps(payload), parse_float=lambda number:
                                      int(number.split('.')[0]) if number.endswith('.0') else float(number))

                def group(result, metric, model, channel=0):
                    return next(g for g in result['groups']
                                if g['metric'] == metric and g['model'] == model and g['channel_id'] == channel)

                def amount(label, metric):
                    return history()[fixture_ids[label]].get('actual_cost' if metric == 'actual' else 'official_value')

                full = preview({})
                shared = group(full, 'actual', 'vendor/shared-v2', alpha)
                assert shared['records'] == 5 and shared['calculable_amounts'] == 4, shared
                assert shared['public_models'] == ['public-alias', 'public-main'], shared
                assert shared['missing_reasons'] == {'usage_unknown': 1}, shared
                assert group(full, 'actual', 'vendor/shared-v2', beta)['records'] == 1
                assert group(full, 'official', 'public-main')['records'] == 5
                unknown_identity = [g for g in full['groups'] if g['metric'] == 'actual' and g['model'] == '']
                assert len(unknown_identity) == 2 and len({g['key'] for g in unknown_identity}) == 2
                assert history_digest() == fixture_hash, 'preview changed history'
                checks.append('actual groups merge aliases, official groups merge channels; unknown identities stay distinct; preview is read-only')

                filtered_request = {'filter': {'from': at.isoformat(), 'to': at.isoformat(),
                                    'channel_id': alpha, 'model': 'public-main'}, 'amounts': ['actual']}
                filtered = preview(filtered_request)
                assert filtered['examined'] == 3 and filtered['existing_amounts'] == 1
                assert filtered['calculable_amounts'] == 1 and filtered['missing_amounts'] == 1
                assert len(filtered['groups']) == 1 and filtered['groups'][0]['records'] == 2
                # Upstream ID is never accepted as the public model filter.
                assert preview({**filtered_request, 'filter': {**filtered_request['filter'], 'model': 'vendor/shared-v2'}})['examined'] == 0

                changed = copy.deepcopy(saved_prices)
                next(p for p in changed if p['channel_id'] == alpha)['selection']['table']['rates']['input'] = 9
                call('/api/prices', changed, 'PUT')
                call('/api/usage/backfill/apply', apply_request(filtered), 'POST', expected=409)
                assert history_digest() == fixture_hash, 'conflict partially wrote history'
                call('/api/prices', saved_prices, 'PUT')
                result = call('/api/usage/backfill/apply', apply_request(filtered), 'POST')
                assert result['applied_amounts'] == 1 and amount('current-a', 'actual') == 6
                assert amount('current-alias', 'actual') is None and amount('current-beta', 'actual') is None
                assert amount('before-time', 'actual') is None and amount('after-time', 'actual') is None
                assert amount('current-a', 'official') is None
                assert call('/api/usage/backfill/apply', apply_request(filtered), 'POST')['applied_amounts'] == 0
                checks.append('combined inclusive time/channel/public-model/amount filters, 409 conflict and repeated apply preserve scope')

                for metric in ('actual', 'official'):
                    current = preview({'amounts': [metric]})
                    call('/api/usage/backfill/apply', apply_request(current), 'POST')
                rows = history()
                basis = rows[fixture_ids['current-alias']]['actual_pricing']
                assert basis['matched_rules'] == ['historical-night']
                assert basis['resolution']['table'] == next(p for p in saved_prices if p['channel_id'] == alpha)['selection']['table']
                assert amount('current-alias', 'actual') == 6 and amount('current-beta', 'actual') == 30
                assert amount('current-a', 'official') == 18 and amount('current-beta', 'official') == 18
                checks.append('current prices use historical identities/time and persist full rules, disabled drafts and notes')

                for metric, rates in [('actual', (7, 11)), ('official', (13, 17))]:
                    frozen = {'max_usage_id': full['request']['max_usage_id']}
                    gaps = preview({**frozen, 'amounts': [metric]})
                    a = group(gaps, metric, 'vendor/manual-a' if metric == 'actual' else 'public-manual-a', gamma if metric == 'actual' else 0)
                    b = group(gaps, metric, 'vendor/manual-b' if metric == 'actual' else 'public-manual-b', beta if metric == 'actual' else 0)
                    manual = preview({**frozen, 'mode': 'manual', 'amounts': [metric], 'prices': [
                        {'key': a['key'], 'table': table(rates[0])}, {'key': b['key'], 'table': table(rates[1])}]})
                    assert manual['examined'] == 3 and manual['calculable_amounts'] == 2
                    assert manual['missing_reasons'] == {'usage_unknown': 1}
                    req = apply_request(manual)
                    before = history_digest()
                    invalids = []
                    duplicate = copy.deepcopy(req); duplicate['prices'].append(copy.deepcopy(duplicate['prices'][0])); invalids.append(duplicate)
                    foreign = copy.deepcopy(req); foreign['prices'].append({'key': 'outside-range', 'table': table(1)}); invalids.append(foreign)
                    bad_table = copy.deepcopy(req); bad_table['prices'][1]['table']['unit_tokens'] = 0; invalids.append(bad_table)
                    wrong_metric = copy.deepcopy(req); wrong_metric['amounts'] = ['official' if metric == 'actual' else 'actual']; invalids.append(wrong_metric)
                    for invalid in invalids:
                        call('/api/usage/backfill/apply', invalid, 'POST', expected=400)
                        assert history_digest() == before, 'invalid batch partially wrote history'
                    changed_draft = copy.deepcopy(req); changed_draft['prices'][1]['table']['rates']['input'] = 99
                    call('/api/usage/backfill/apply', changed_draft, 'POST', expected=409)
                    assert history_digest() == before, 'manual conflict partially wrote history'
                    if metric == 'actual':
                        insert('post-preview', channel=gamma, model='public-manual-a', upstream='vendor/manual-a')
                    result = call('/api/usage/backfill/apply', req, 'POST')
                    assert result['applied_amounts'] == 2
                    assert amount('manual-a', metric) == 3*rates[0] and amount('manual-b', metric) == 3*rates[1]
                    assert amount('unknown-manual', metric) is None and amount('unselected', metric) is None
                    if metric == 'actual':
                        assert amount('post-preview', metric) is None, 'apply adopted a new record'
                        assert amount('manual-a', 'official') is None, 'manual actual wrote official value'
                    assert call('/api/usage/backfill/apply', req, 'POST')['applied_amounts'] == 0
                checks.append('per-row manual actual/official prices, duplicate/invalid/foreign keys, 409 conflicts and atomic rollback verified by SQLite')
                checks.append('new records excluded by max_usage_id; selected batches are idempotent and leave unselected rows/metrics null')
                for metric in ('actual', 'official'):
                    assert amount('known-zero', metric) == 0
                    assert amount('unknown-current', metric) is None and amount('unknown-manual', metric) is None
                    assert amount('not-sent', metric) is None
                    assert amount('post-preview', metric) is None
                assert call('/api/prices') == saved_prices, 'backfill changed price configuration'
                checks.append('known zero, unknown usage and not-sent exclusions preserved; price configuration unchanged')
                evidence = {'status': 'passed', 'exe_sha256': hashlib.sha256(exe.read_bytes()).hexdigest(),
                    'fixture_sha256': fixture_hash, 'fixture_records': len(fixture_ids), 'checks': checks,
                    'evidence_boundary': 'isolated real executable, HTTP API and synthetic SQLite history; no user data or provider calls'}
                if args.evidence_file:
                    args.evidence_file.parent.mkdir(parents=True, exist_ok=True)
                    args.evidence_file.write_text(json.dumps(evidence, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
                print(json.dumps(evidence, ensure_ascii=False, indent=2), flush=True)
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
    print('ISOLATED_BACKFILL_DATA_CLEANED', flush=True)


if __name__ == '__main__':
    main()
