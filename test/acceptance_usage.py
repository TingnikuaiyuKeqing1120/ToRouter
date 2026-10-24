"""Full-period usage against an isolated executable and synthetic SQLite history."""
import argparse
from contextlib import closing
from datetime import datetime, timezone
import hashlib
import http.cookiejar
import json
from pathlib import Path
import re
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--exe', type=Path, required=True)
    parser.add_argument('--evidence-file', type=Path, required=True)
    args = parser.parse_args()
    exe = args.exe.resolve(strict=True)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    origin = f'http://127.0.0.1:{port}'
    flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
    with tempfile.TemporaryDirectory(prefix='torouter-usage-') as data:
        key_result = subprocess.run([str(exe), '-data', data, '-show-key'], capture_output=True,
                                    text=True, encoding='utf-8', creationflags=flags, check=True)
        admin = re.search(r'tr_[A-Za-z0-9_-]+', key_result.stdout).group(0)
        proc = subprocess.Popen([str(exe), '-data', data, '-port', str(port)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=flags)
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

        def call(path, body=None):
            request = urllib.request.Request(origin+path, data=None if body is None else json.dumps(body).encode(),
                                             headers={'Content-Type': 'application/json', 'X-To-Router': '1'})
            with opener.open(request, timeout=30) as response:
                return json.load(response)

        def digest(db):
            value = hashlib.sha256()
            for identity, raw in db.execute('SELECT id,data FROM usage_logs ORDER BY id'):
                value.update(f'{identity}:{raw}\n'.encode())
            return value.hexdigest()

        try:
            for _ in range(100):
                try:
                    call('/api/auth/login', {'key': admin})
                    break
                except OSError:
                    assert proc.poll() is None, 'isolated process exited'
                    time.sleep(.1)
            else:
                raise AssertionError('isolated process did not become ready')

            def millis(value):
                return int(datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()*1000)

            start, until = millis('2026-10-01T00:00:00Z'), millis('2026-11-01T00:00:00Z')
            middle = millis('2026-10-16T12:00:00Z')
            base = {'channel_id': 8, 'channel_name': 'Main channel', 'model': 'main', 'status': 200,
                    'input_tokens': 4, 'output_tokens': 3, 'cache_read_tokens': 2, 'cache_write_tokens': 1,
                    'official_value': 1.5, 'official_currency': 'CNY', 'actual_cost': .25, 'currency': 'USD'}

            def row(stamp, **changes):
                record = {**base, **changes, 'time': datetime.fromtimestamp(stamp/1000, timezone.utc).isoformat()}
                return (stamp, record['channel_id'], record['model'], record['status'], json.dumps(record))

            bulk = 12000
            fixtures = [row(start, channel_id=7, channel_name='Early channel', model='old-model',
                            input_tokens=100, cache_read_tokens=20, cache_write_tokens=10, output_tokens=40,
                            official_value=3, official_currency='USD', actual_cost=5, currency='CNY')]
            fixtures.extend(row(middle) for _ in range(bulk))
            fixtures.extend([
                row(until-1, channel_id=7, channel_name='Early channel', model='old-model', status=503,
                    input_tokens=30, cache_read_tokens=0, cache_write_tokens=0, output_tokens=11,
                    official_value=None, actual_cost=0, currency='CNY'),
                row(start, not_sent=True, input_tokens=999999, actual_cost=999999),
                row(start-1, input_tokens=999999), row(until, input_tokens=999999),
            ])
            with closing(sqlite3.connect(Path(data)/'router.db', timeout=10)) as db:
                db.executemany('INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,?,?,?,?)', fixtures)
                db.commit()
                before = digest(db)

            query = '/api/usage?from=2026-10-01&to=2026-10-31'
            began = time.monotonic()
            summary = call(query+'&limit=1')
            elapsed = time.monotonic()-began
            assert summary['requests'] == bulk+2, summary
            assert summary['failures'] == 1, summary
            assert summary['input_tokens'] == 7*bulk+160, summary
            assert summary['output_tokens'] == 3*bulk+51, summary
            assert summary['official_values'] == {'USD': 3, 'CNY': 1.5*bulk}, summary
            assert summary['actual_costs'] == {'USD': .25*bulk, 'CNY': 5}, summary
            assert summary['official_priced_requests'] == bulk+1, summary
            assert summary['actual_priced_requests'] == bulk+2, summary
            assert not {'sample_limit', 'possibly_truncated'} & summary.keys()
            checks = ['all 12002 matching records, token categories, currencies and zero/unknown pricing included']
            assert [item['date'] for item in summary['daily']] == ['2026-10-01', '2026-10-16', '2026-10-31']
            assert [item['requests'] for item in summary['daily']] == [1, bulk, 1]
            for key in ('daily', 'by_channel', 'by_model'):
                assert sum(item['requests'] for item in summary[key]) == bulk+2, key
                for metric in ('official_values', 'actual_costs'):
                    for currency in ('USD', 'CNY'):
                        assert sum(item[metric][currency] for item in summary[key]) == summary[metric][currency], (key, metric, currency)
            assert next(item for item in summary['by_model'] if item['name'] == 'old-model')['requests'] == 2
            checks.append('daily/channel/model groups reconcile, including the oldest history')
            filtered = call(query+'&channel_id=7&model=old-model&status=503')
            assert filtered['requests'] == 1 and filtered['actual_priced_requests'] == 1
            assert filtered['actual_costs'] == {'USD': 0, 'CNY': 0} and filtered['official_priced_requests'] == 0
            checks.append('combined filters and inclusive end date preserve free versus unpriced')
            assert len(call('/api/logs?limit=100000')) == 10000
            assert len(call('/api/logs?limit=1')) == 1
            checks.append('log display limits remain independent from full-period statistics')
            empty = call('/api/usage?from=2026-12-01&to=2026-12-31')
            assert empty['requests'] == 0 and all(empty[key] == [] for key in ('daily', 'by_channel', 'by_model'))
            assert empty['official_values'] == empty['actual_costs'] == {'USD': 0, 'CNY': 0}
            checks.append('empty period retains the expected response shape')
            assert call(query) == summary
            with closing(sqlite3.connect(Path(data)/'router.db', timeout=10)) as db:
                assert digest(db) == before
            checks.append('repeat reads agree and leave historical records untouched')
            evidence = {'status': 'passed', 'exe_sha256': hashlib.sha256(exe.read_bytes()).hexdigest(),
                        'matching_records': bulk+2, 'query_seconds': round(elapsed, 3), 'checks': checks,
                        'evidence_boundary': 'isolated executable and synthetic history; no user data or provider calls'}
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
    print('ISOLATED_DATA_CLEANED', flush=True)


if __name__ == '__main__':
    main()
