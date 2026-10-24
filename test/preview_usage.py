"""Disposable, deterministic usage data for browser acceptance; no provider calls."""
import argparse
import hashlib
from contextlib import closing
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.request


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--exe', type=Path, required=True)
    parser.add_argument('--state-file', type=Path, required=True)
    parser.add_argument('--stop-file', type=Path, required=True)
    parser.add_argument('--as-of', help='Fixed ISO timestamp for comparable before/after fixtures')
    args = parser.parse_args()
    exe = args.exe.resolve(strict=True)
    flags = getattr(subprocess, 'CREATE_NO_WINDOW', 0)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    origin = 'http://127.0.0.1:' + str(port)
    with tempfile.TemporaryDirectory(prefix='torouter-usage-ui-') as data:
        subprocess.run([str(exe), '-data', data, '-show-key'], capture_output=True, check=True, creationflags=flags)
        local = timezone(timedelta(hours=8))
        now = datetime.fromisoformat(args.as_of).astimezone(local) if args.as_of else datetime.now(local)
        today = now.replace(hour=0, minute=0, second=0, microsecond=0)
        profiles = [('coding', 1, '主力 API', 42000, .78, 1800), ('chat', 2, '日常对话', 6500, .40, 950),
                    ('reasoning', 1, '主力 API', 96000, .88, 4200), ('quick', 3, '轻量服务', 1800, .20, 320),
                    ('review', 2, '日常对话', 26000, .65, 1500), ('translate', 3, '轻量服务', 9200, .10, 700),
                    ('long-context', 4, '长上下文', 180000, .91, 2400)]
        rows = []
        for offset in range(30):
            stamp = today - timedelta(days=29-offset) + timedelta(hours=10)
            if stamp > now:
                stamp = now - timedelta(minutes=5)
            for index, (model, channel, name, input_size, hit, output_size) in enumerate(profiles):
                count = 5 + ((offset*13 + index*7) % 27)
                for serial in range(count):
                    total = input_size + ((serial*977 + offset*37) % max(1, input_size//4))
                    read = int(total*hit)
                    write = int((total-read)*.15)
                    plain = total-read-write
                    output = output_size + serial*13
                    actual_rates = {'input': .8, 'output': 3.2, 'cache_read': .08, 'cache_write': 1}
                    official_rates = {'input': 2, 'output': 8, 'cache_read': .2, 'cache_write': 2.5}
                    quantities = {'input': plain, 'output': output, 'cache_read': read, 'cache_write': write}
                    record = {'time': stamp.isoformat(), 'channel_id': channel, 'channel_name': name, 'model': model,
                              'upstream_model': model+'-upstream', 'input_tokens': plain, 'output_tokens': output,
                              'cache_read_tokens': read, 'cache_write_tokens': write, 'estimated': False,
                              'status': 503 if serial == 0 and offset % 9 == 0 else 200, 'duration_ms': 1350+serial*40,
                              'ttft_ms': 280, 'currency': 'CNY', 'official_currency': 'USD',
                              'actual_cost': sum(quantities[key]*actual_rates[key]/1_000_000 for key in quantities),
                              'official_value': sum(quantities[key]*official_rates[key]/1_000_000 for key in quantities),
                              'actual_pricing': {'method': 'calculated', 'currency': 'CNY', 'unit_tokens': 1_000_000, 'rates': actual_rates},
                              'official_pricing': {'method': 'calculated', 'currency': 'USD', 'unit_tokens': 1_000_000, 'rates': official_rates}}
                    if model == 'quick':
                        record['actual_cost'] = 0
                        record['actual_pricing']['rates'] = dict.fromkeys(actual_rates, 0)
                    if model == 'long-context' and serial == 0:
                        record['actual_cost'] = None
                        record['actual_pricing'] = None
                    rows.append((int(stamp.timestamp()*1000), channel, model, record['status'], json.dumps(record, ensure_ascii=False)))
        missing = {'time': now.isoformat(), 'channel_id': 5, 'channel_name': '待补全渠道', 'model': 'usage-unreported',
                   'estimated': True, 'status': 502, 'input_tokens': 0, 'output_tokens': 0, 'cache_read_tokens': 0,
                   'cache_write_tokens': 0, 'actual_cost': None, 'official_value': None}
        rows.append((int(now.timestamp()*1000), 5, 'usage-unreported', 502, json.dumps(missing, ensure_ascii=False)))
        with closing(sqlite3.connect(Path(data)/'router.db')) as db:
            db.executemany('INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,?,?,?,?)', rows)
            rate = {'available': True, 'base': 'USD', 'quote': 'CNY', 'rate': 7.2, 'date': now.date().isoformat(),
                    'attempted_on': now.date().isoformat(), 'checked_at': now.isoformat(), 'fetched_at': now.isoformat(),
                    'stale': False, 'error': '', 'source_name': '隔离验收汇率', 'source_id': 'fixture'}
            db.execute("INSERT OR REPLACE INTO meta(key,value) VALUES('exchange_usd_cny_v1',?)", (json.dumps(rate),))
            db.commit()
        proc = subprocess.Popen([str(exe), '-data', data, '-port', str(port)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=flags)
        try:
            for _ in range(100):
                try:
                    with urllib.request.urlopen(origin, timeout=1) as response:
                        assert response.status == 200
                    break
                except OSError:
                    assert proc.poll() is None, 'preview process exited'
                    time.sleep(.1)
            else:
                raise AssertionError('preview did not become ready')
            fixture_hash = hashlib.sha256(json.dumps(rows, ensure_ascii=False).encode('utf-8')).hexdigest()
            state = {'origin': origin, 'data_dir': data, 'pid': proc.pid, 'records': len(rows), 'synthetic': True,
                     'as_of': now.isoformat(), 'fixture_sha256': fixture_hash}
            args.state_file.parent.mkdir(parents=True, exist_ok=True)
            args.state_file.write_text(json.dumps(state, ensure_ascii=False, indent=2), encoding='utf-8')
            print('UI_ORIGIN='+origin, flush=True)
            deadline = time.monotonic()+1800
            while time.monotonic() < deadline and not args.stop_file.exists():
                time.sleep(.25)
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
    print('ISOLATED_UI_DATA_CLEANED', flush=True)


if __name__ == '__main__':
    main()
