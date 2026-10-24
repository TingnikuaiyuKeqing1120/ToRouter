"""Local-only T111 UI fixture: real EXE writes, controlled catalog/FX reads.

The supplied upstream must be an isolated test process. No browser automation.
Control file: delay (seconds), fail (bool), copies (catalog size), label, rate.
"""
import argparse
import copy
import json
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--upstream-state', type=Path, required=True)
    parser.add_argument('--control', type=Path, required=True)
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--stop', type=Path, required=True)
    parser.add_argument('--events', type=Path, required=True)
    args = parser.parse_args()
    session = json.loads(args.upstream_state.read_text(encoding='utf-8'))
    assert session['synthetic'] and session['origin'].startswith('http://127.0.0.1:')
    upstream = session['origin']
    events, lock = [], threading.Lock()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            self.proxy()

        do_HEAD = do_POST = do_PUT = do_DELETE = do_PATCH = do_GET

        def proxy(self):
            control = json.loads(args.control.read_text(encoding='utf-8'))
            resource = self.path.split('?')[0] in ['/api/catalog/models', '/api/catalog/sources', '/api/exchange-rate'] and self.command == 'GET'
            event = {'path': self.path, 'method': self.command, 'start': time.time(), 'resource_fixture': resource}
            if resource:
                time.sleep(min(max(control.get('delay', 0), 0), 30))
            headers = {key: value for key, value in self.headers.items() if key.lower() not in ['host', 'connection', 'content-length', 'accept-encoding']}
            if 'Origin' in headers:
                headers['Origin'] = upstream
            if 'Referer' in headers:
                headers['Referer'] = upstream + '/'
            raw = self.rfile.read(int(self.headers.get('Content-Length', '0')))
            if resource and control.get('fail'):
                status, result, outgoing = 503, b'{"error":"controlled refresh failure"}', {'Content-Type': 'application/json'}
            elif self.path == '/api/exchange-rate' and self.command == 'GET':
                status, outgoing = 200, {'Content-Type': 'application/json'}
                result = json.dumps({'available': True, 'rate': control.get('rate', 7), 'base': 'USD', 'quote': 'CNY', 'stale': False, 'date': '2026-10-18', 'source_name': 'local UI fixture'}).encode()
            else:
                request = urllib.request.Request(upstream + self.path, method=self.command, headers=headers, data=raw or None)
                try:
                    response = opener.open(request, timeout=35)
                except urllib.error.HTTPError as error:
                    response = error
                status, result, outgoing = response.status, response.read(), dict(response.headers)
                if self.path.startswith('/api/catalog/models') and status == 200 and control.get('copies'):
                    originals = json.loads(result)
                    fixture = []
                    for index in range(control['copies']):
                        item = copy.deepcopy(originals[index % len(originals)])
                        item['id'] = 'ui-fixture-' + str(index).zfill(4)
                        item['name'] = f"{control.get('label', '本地验收模型')} {index:04}"
                        fixture.append(item)
                    result = json.dumps(fixture, ensure_ascii=False).encode()
            self.send_response(status)
            for key, value in outgoing.items():
                if key.lower() not in ['content-length', 'transfer-encoding', 'connection', 'content-encoding']:
                    self.send_header(key, value)
            self.send_header('Content-Length', str(len(result)))
            self.end_headers()
            try:
                self.wfile.write(result)
            except (BrokenPipeError, ConnectionResetError):
                pass
            event.update(end=time.time(), status=status)
            with lock:
                events.append(event)

    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    origin = f'http://127.0.0.1:{server.server_port}'
    args.state.write_text(json.dumps({'synthetic': True, 'origin': origin, 'upstream': upstream}), encoding='utf-8')
    print(origin, flush=True)
    try:
        while not args.stop.exists():
            time.sleep(.2)
    finally:
        server.shutdown()
        server.server_close()
        args.events.write_text(json.dumps(events, indent=2), encoding='utf-8')


if __name__ == '__main__':
    main()
