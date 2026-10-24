import json, http.server, threading

captured = {}

class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('content-length', 0))
        body = self.rfile.read(n)
        captured[self.path] = {'body': body.decode('utf-8', 'replace'), 'headers': dict(self.headers)}
        if self.path == '/v1/chat/completions':
            if b'"stream":true' in body:
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                for t in ['Hello', ' from', ' mock']:
                    self.wfile.write(('data: ' + json.dumps({'choices': [{'delta': {'content': t}, 'finish_reason': None}]}) + '\n\n').encode())
                    self.wfile.flush()
                self.wfile.write(('data: ' + json.dumps({'choices': [{'delta': {}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 12, 'completion_tokens': 3}}) + '\n\n').encode())
                self.wfile.write(b'data: [DONE]\n\n')
            else:
                out = json.dumps({'choices': [{'message': {'role': 'assistant', 'content': 'mock says hi'}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 12, 'completion_tokens': 3}}).encode()
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(out)))
                self.end_headers()
                self.wfile.write(out)
        elif self.path == '/v1/messages':
            if b'"stream":true' in body:
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                self.wfile.write(b'event: message_start\ndata: {"type":"message_start","message":{"id":"ms1","type":"message","role":"assistant","model":"claude-mock","content":[],"usage":{"input_tokens":7,"output_tokens":0}}}\n\n')
                self.wfile.flush()
                self.wfile.write(b'event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n')
                self.wfile.flush()
                for t in ['stream', ' works', ' too']:
                    self.wfile.write(('event: content_block_delta\ndata: ' + json.dumps({'type': 'content_block_delta', 'index': 0, 'delta': {'type': 'text_delta', 'text': t}}) + '\n\n').encode())
                    self.wfile.flush()
                self.wfile.write(b'event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n')
                self.wfile.write(b'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}\n\n')
                self.wfile.write(b'event: message_stop\ndata: {"type":"message_stop"}\n\n')
                return
            else:
                out = json.dumps({'id': 'm1', 'type': 'message', 'role': 'assistant', 'model': 'claude-mock', 'content': [{'type': 'text', 'text': 'anthropic mock'}], 'stop_reason': 'end_turn', 'usage': {'input_tokens': 7, 'output_tokens': 2}}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(out)))
            self.end_headers()
            self.wfile.write(out)
        elif self.path.startswith('/v1beta/models/'):
            out = json.dumps({'candidates': [{'content': {'role': 'model', 'parts': [{'text': 'gemini mock'}]}, 'finishReason': 'STOP'}], 'usageMetadata': {'promptTokenCount': 5, 'candidatesTokenCount': 2}}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(out)))
            self.end_headers()
            self.wfile.write(out)
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, *a):
        pass

def report(port):
    class R(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            out = json.dumps(captured).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(out)))
            self.end_headers()
            self.wfile.write(out)
        def log_message(self, *a):
            pass
    http.server.HTTPServer(('127.0.0.1', port), R).serve_forever()

threading.Thread(target=report, args=(19998,), daemon=True).start()
http.server.HTTPServer(('127.0.0.1', 19999), H).serve_forever()
