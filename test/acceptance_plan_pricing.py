"""T053 isolated binary acceptance. Synthetic accounts and local upstream only."""
import argparse
import concurrent.futures
import datetime as dt
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
    checks, calls = [], []
    local_zone = dt.timezone(dt.timedelta(hours=8))
    now = dt.datetime.now(local_zone)
    current = now.strftime('%Y-%m')
    start = now.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    previous = (start - dt.timedelta(days=1)).replace(day=1)
    first = (previous - dt.timedelta(days=1)).replace(day=1)

    class Upstream(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))))
            calls.append({'path': self.path, 'model': body.get('model')})
            raw = json.dumps({'id': 'synthetic-plan-usage', 'object': 'chat.completion',
                'model': body.get('model'), 'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'ok'}, 'finish_reason': 'stop'}],
                'usage': {'prompt_tokens': 12, 'completion_tokens': 2, 'total_tokens': 14}}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    origin = f'http://127.0.0.1:{port}'
    proc = None
    with tempfile.TemporaryDirectory(prefix='torouter-plan-') as directory:
        try:
            def boot():
                nonlocal proc
                proc = subprocess.Popen([str(exe), '-data', directory, '-port', str(port)],
                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                    creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))

            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

            def call(path, body=None, method='GET', key=None, expected=200):
                headers = {'Content-Type': 'application/json', 'X-To-Router': '1'}
                if key:
                    headers['Authorization'] = 'Bearer ' + key
                request = urllib.request.Request(origin + path, method=method, headers=headers,
                    data=None if body is None else json.dumps(body).encode())
                try:
                    response = opener.open(request, timeout=25)
                except urllib.error.HTTPError as error:
                    response = error
                raw = response.read()
                statuses = expected if isinstance(expected, (list, tuple)) else [expected]
                assert response.status in statuses, (path, response.status, raw[:300])
                return json.loads(raw) if raw else None

            def ready():
                for _ in range(150):
                    try:
                        call('/api/auth/local', {}, 'POST')
                        return
                    except OSError:
                        assert proc.poll() is None, 'isolated service exited'
                        time.sleep(.1)
                raise AssertionError('service did not start')

            def usage(**filters):
                return call('/api/usage?' + urllib.parse.urlencode({'timezone': 'Asia/Shanghai', **filters}))

            boot()
            ready()
            bodies, ids = {}, {}
            for name, label, tags in [('plan', '开发套餐', ['billing:plan', 'privacy:zdr']),
                                      ('api', '按量 API', ['billing:payg', 'privacy:zdr']),
                                      ('empty', '未配置月费', ['billing:plan']),
                                      ('history', '历史套餐', ['billing:plan'])]:
                body = {'name': label, 'enabled': True, 'api_key': 'synthetic-only-key',
                    'models': ['included', 'metered'], 'available_models': ['included', 'metered', 'disabled'],
                    'tags': tags, 'model_tags': {'metered': ['billing:payg']},
                    'endpoints': [{'format': 'openai_chat_completions', 'base_url': f'http://127.0.0.1:{upstream.server_port}/{name}'}]}
                bodies[name] = body
                ids[name] = call('/api/channels', body, 'POST')['id']

            def plan_config(amount=20, at=first, currency='USD'):
                return {'default_amount': amount, 'currency': currency, 'first_payment_at': at.isoformat(), 'timezone': 'Asia/Shanghai'}

            def plan_month(month=current):
                return call('/api/plan-prices?month=' + month)

            call(f"/api/plan-prices/{ids['plan']}", plan_config(), 'PUT')
            for month in [first.strftime('%Y-%m'), previous.strftime('%Y-%m'), current]:
                assert next(c for c in plan_month(month)['charges'] if c['channel_id'] == ids['plan'])['amount'] == 20
            total = usage()
            assert total['requests'] == 0 and total['actual_costs']['USD'] == 60
            assert total['plan_charges'] == 3 and total['unconfigured_plan_channels'] == 2
            assert total['by_model'] == []
            checks.append('monthly catch-up counts fees without requests; unconfigured channels are explicit')

            with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
                assert all(value['plan_charges'] == 3 for value in pool.map(lambda _: usage(), range(12)))
            call(f"/api/plan-prices/{ids['plan']}", plan_config(30), 'PUT')
            assert usage()['actual_costs']['USD'] == 60
            call(f"/api/plan-prices/{ids['plan']}/charges/{current}", {'amount': 15, 'currency': 'USD', 'paid_at': start.isoformat()}, 'PUT')
            assert usage()['actual_costs']['USD'] == 55
            assert next(c for c in plan_month(first.strftime('%Y-%m'))['charges'] if c['channel_id'] == ids['plan'])['amount'] == 20
            checks.append('concurrent reads deduplicate; changing default freezes history; period override changes one month')

            next_month = (start + dt.timedelta(days=32)).replace(day=1).strftime('%Y-%m')
            projection = plan_month(next_month)
            scheduled = next(row for row in projection['scheduled'] if row['channel_id'] == ids['plan'])
            assert scheduled['amount'] == 30 and scheduled['currency'] == 'USD'
            assert not any(row['channel_id'] == ids['plan'] for row in projection['charges'])
            assert usage()['plan_charges'] == 3
            checks.append('future payment preview comes from the server calendar without writing a charge')

            before = plan_month()
            call(f"/api/plan-prices/{ids['plan']}", {**plan_config(90), 'charge': {'month': current, 'amount': -1, 'currency': 'USD', 'paid_at': start.isoformat()}}, 'PUT', expected=400)
            assert plan_month() == before
            call('/api/channels/billing', {'changes': [{'channel_id': ids['api'], 'billing_tag': 'billing:plan'}, {'channel_id': 999999, 'billing_tag': 'billing:plan'}]}, 'PUT', expected=404)
            assert 'billing:payg' in next(c for c in call('/api/channels') if c['id'] == ids['api'])['tags']
            checks.append('combined price and period save, and batch declarations, roll back completely on failure')

            def price(channel_id, model):
                return {'channel_id': channel_id, 'model': model, 'display_name': model, 'origin': 'manual',
                    'selection': {'mode': 'manual', 'table': {'currency': 'USD', 'unit_tokens': 1000000,
                    'rates': {'input': 100000, 'output': 100000, 'cache_read': 0, 'cache_write': 0}, 'rules': []}}}

            for public_id, model in [('plan-model', 'included'), ('payg-model', 'metered')]:
                call('/api/models', {'id': public_id, 'display_name': public_id, 'enabled': True,
                    'bindings': [{'channel_id': ids['plan'], 'model': model, 'enabled': True}]}, 'POST')
            price_rows = [price(ids['plan'], 'included'), price(ids['plan'], 'metered'), price(0, 'plan-model'), price(0, 'payg-model')]
            call('/api/prices', price_rows, 'PUT')
            token = call('/api/client-keys', {'name': 'Synthetic plan acceptance'}, 'POST', expected=201)['key']

            def request(model):
                return call('/v1/chat/completions', {'model': model, 'messages': [{'role': 'user', 'content': 'hi'}]}, 'POST', key=token)

            request('plan-model')
            request('payg-model')
            logs = call('/api/logs')
            included = next(row for row in logs if row['model'] == 'plan-model')
            paid = next(row for row in logs if row['model'] == 'payg-model')
            assert included.get('actual_cost') is None and included['actual_pricing']['resolution']['layer'] == 'plan'
            assert included['official_value'] == 1.4 and paid['actual_cost'] == 1.4
            assert usage()['actual_costs']['USD'] == 56.4
            assert usage()['requests'] == 2 and usage()['plan_requests'] == 1
            assert usage(model='plan-model')['actual_costs']['USD'] == 0 and usage(model='plan-model')['plan_charges'] == 0
            checks.append('real requests freeze plan identity; independent payg model charges normally; API value remains independent')
            for path, key in [('/api/models/specs', None), ('/v1/models/specs', token)]:
                specs = call(path, key=key)['data']
                plan_spec = next(row for row in specs if row['id'] == 'plan-model')['bindings'][0]['payg']
                payg_spec = next(row for row in specs if row['id'] == 'payg-model')['bindings'][0]['payg']
                assert plan_spec['layer'] == 'plan' and plan_spec['table'] is None
                assert payg_spec['layer'] == 'channel' and payg_spec['table']['rates']['input'] == 100000
            assert len(call('/api/prices')) == 4
            checks.append('admin and client specifications reflect plan billing while preserving stored token price tables')

            actual_preview = call('/api/usage/backfill/preview', {'filter': {}, 'amounts': ['actual'], 'mode': 'current'}, 'POST')
            assert not actual_preview['groups'], actual_preview
            call('/api/channels/billing', {'changes': [{'channel_id': ids['plan'], 'billing_tag': 'billing:payg'}]}, 'PUT')
            after = next(row for row in call('/api/logs') if row['id'] == included['id'])
            assert after.get('actual_cost') is None and after['actual_pricing']['resolution']['layer'] == 'plan'
            assert not call('/api/usage/backfill/preview', {'filter': {}, 'amounts': ['actual'], 'mode': 'current'}, 'POST')['groups']
            call('/api/channels/billing', {'changes': [{'channel_id': ids['plan'], 'billing_tag': 'billing:plan'}]}, 'PUT')
            channel = next(row for row in call('/api/channels') if row['id'] == ids['plan'])
            assert channel['model_tags']['metered'] == ['billing:payg'] and 'privacy:zdr' in channel['tags']
            checks.append('plan usage stays out of actual backfill after tag changes; declarations preserve independent model and privacy tags')

            call('/api/prices', [row for row in price_rows if not (row['channel_id'] == 0 and row['model'] == 'plan-model')], 'PUT')
            request('plan-model')
            call('/api/prices', price_rows, 'PUT')
            preview = call('/api/usage/backfill/preview', {'filter': {}, 'amounts': ['official'], 'mode': 'current'}, 'POST')
            assert preview['calculable_amounts'] == 1, preview
            call('/api/usage/backfill/apply', {**preview['request'], 'expected_resolution': preview['expected_resolution']}, 'POST')
            fixed = next(row for row in call('/api/logs') if row['id'] == max(v['id'] for v in call('/api/logs')))
            assert fixed['official_value'] == 1.4 and fixed.get('actual_cost') is None
            assert fixed['actual_pricing']['resolution']['layer'] == 'plan'
            checks.append('official backfill succeeds independently and preserves plan actual-billing snapshot')

            filtered = usage(channel_id=ids['plan'], **{'from': start.isoformat(), 'to': now.isoformat()})
            assert filtered['plan_charges'] == 1 and filtered['plan_costs']['USD'] == 15
            assert usage(status=200)['plan_charges'] == 0
            call(f"/api/plan-prices/{ids['plan']}/charges/2099-01", {'amount': 99, 'currency': 'USD', 'paid_at': '2099-01-01T00:00:00+08:00'}, 'PUT')
            assert usage()['plan_charges'] == 3
            checks.append('payment-date and channel filters include exact fee scope; future and request-only filters do not count monthly fees')

            call(f"/api/plan-prices/{ids['history']}", plan_config(7, start, 'CNY'), 'PUT')
            call(f"/api/channels/{ids['history']}", {**bodies['history'], 'id': ids['history'], 'enabled': False}, 'PUT')
            assert next(c for c in plan_month()['charges'] if c['channel_id'] == ids['history'])['amount'] == 7
            future_payment = dt.datetime.now(local_zone) + dt.timedelta(seconds=3)
            call(f"/api/plan-prices/{ids['history']}/charges/2099-02", {'amount': 999, 'currency': 'CNY', 'paid_at': future_payment.isoformat()}, 'PUT')
            call(f"/api/channels/{ids['history']}", method='DELETE')
            assert next(c for c in plan_month()['charges'] if c['channel_id'] == ids['history'])['channel_name'] == '历史套餐'
            call(f"/api/plan-prices/{ids['history']}/charges/{current}", {'amount': 8, 'currency': 'CNY', 'paid_at': start.isoformat()}, 'PUT')
            assert usage(channel_id=ids['history'])['actual_costs']['CNY'] == 8
            checks.append('disabling does not cancel monthly fees; deleting channels retains named history and permits explicit correction')
            time.sleep(max(0, (future_payment - dt.datetime.now(local_zone)).total_seconds()) + .1)
            assert usage(channel_id=ids['history'])['actual_costs']['CNY'] == 8
            assert not any(row['channel_id'] == ids['history'] for row in plan_month('2099-02')['charges'])
            checks.append('deleting a channel cancels unoccurred scheduled charges before their payment time')

            snapshot = usage()
            proc.terminate()
            proc.wait(timeout=15)
            boot()
            ready()
            reopened = usage()
            assert snapshot['plan_charges'] == reopened['plan_charges'] and snapshot['actual_costs'] == reopened['actual_costs']
            checks.append('database reopen preserves monthly records and does not duplicate fees')

            evidence = {'synthetic': True, 'exe': str(exe), 'sha256': hashlib.sha256(exe.read_bytes()).hexdigest(),
                'checks_passed': len(checks), 'checks': checks, 'controlled_upstream_requests': len(calls),
                'month': current, 'summary': {key: reopened.get(key) for key in ['requests', 'plan_requests', 'plan_charges', 'actual_costs', 'plan_costs', 'unconfigured_plan_channels']}}
            args.evidence_file.parent.mkdir(parents=True, exist_ok=True)
            args.evidence_file.write_text(json.dumps(evidence, ensure_ascii=False, indent=2), encoding='utf-8')
            print(json.dumps({'checks_passed': len(checks), 'evidence_file': str(args.evidence_file)}, ensure_ascii=False), flush=True)
            if args.state_file:
                args.state_file.write_text(json.dumps({'synthetic': True, 'origin': origin, 'pid': proc.pid,
                    'data_directory': directory, 'channel_ids': ids, 'month': current}, ensure_ascii=False, indent=2), encoding='utf-8')
            if args.hold_seconds:
                print('ISOLATED_UI_READY ' + origin, flush=True)
                deadline = time.monotonic() + args.hold_seconds
                while time.monotonic() < deadline and not (args.stop_file and args.stop_file.exists()):
                    assert proc.poll() is None
                    time.sleep(.25)
        finally:
            if proc is not None and proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=15)
            upstream.shutdown()
            upstream.server_close()
    print('ISOLATED_DATA_CLEANED', flush=True)


if __name__ == '__main__':
    main()
