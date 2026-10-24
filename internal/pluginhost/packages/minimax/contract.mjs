// MiniMax uses native Anthropic Messages. Validation must not become a
// whitelist that silently strips future native parameters or content blocks.
const object = value => value && typeof value === 'object' && !Array.isArray(value);
let redact = () => 'MiniMax upstream request failed';
export function setSanitizer(fn) { redact = fn; }
export function sanitize(message) { return redact(String(message)); }

export function validate(request) {
  if (!object(request) || typeof request.model !== 'string' || !request.model.trim() ||
      !Array.isArray(request.messages) || !request.messages.length) throw new Error('MiniMax requires a model and Messages history');
  if (!Number.isSafeInteger(request.max_tokens) || request.max_tokens <= 0) throw new Error('MiniMax requires a positive max_tokens');
  if (request.stream != null && typeof request.stream !== 'boolean') throw new Error('MiniMax stream must be a boolean');
  for (const message of request.messages) {
    if (!object(message) || !['user', 'assistant'].includes(message.role) ||
        !(typeof message.content === 'string' || Array.isArray(message.content))) throw new Error('MiniMax requires native Messages roles and content');
  }
  if (request.tools != null && !Array.isArray(request.tools)) throw new Error('MiniMax tools must be an array');
  if (request.tool_choice != null && !object(request.tool_choice)) throw new Error('MiniMax tool_choice must use the native Messages object');
}

function errorBody(message) {
  // Arbitrary upstream text may echo credentials, prompts, or account details.
  // Keep only our own diagnosis; setSanitizer also applies the host policy.
  return { type: 'error', error: { type: 'api_error', message: sanitize(message) } };
}

export async function answer(response, renewed, streaming) {
  const headers = new Headers(response.headers);
  headers.delete('content-length');
  headers.delete('content-encoding');
  headers.set('X-Magpie-Sign-In', renewed && response.ok ? 'renewed' : 'kept');
  if (!response.ok) {
    await response.body?.cancel();
    headers.set('content-type', 'application/json');
    return new Response(JSON.stringify(errorBody(`MiniMax upstream HTTP ${response.status}`)), { status: response.status, headers });
  }
  if (!streaming) {
    let body;
    try { body = await response.json(); } catch { throw new Error('MiniMax returned invalid JSON'); }
    if (body?.type === 'error') {
      headers.set('content-type', 'application/json');
      return new Response(JSON.stringify(errorBody('MiniMax upstream response error')), { status: 502, headers });
    }
    if (body?.type !== 'message' || typeof body.stop_reason !== 'string' || !body.stop_reason) throw new Error('MiniMax response has no terminal stop reason');
    headers.set('content-type', 'application/json');
    // Unknown and absent usage fields retain their original meaning.
    return new Response(JSON.stringify(body), { status: response.status, headers });
  }
  if (!response.headers.get('content-type')?.toLowerCase().includes('text/event-stream') || !response.body) {
    await response.body?.cancel();
    throw new Error('MiniMax did not return an event stream');
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder('utf-8', { fatal: true });
  const encoder = new TextEncoder();
  let buffer = '', terminal = false, failed = false;
  const frame = raw => {
    const lines = raw.split(/\r?\n|\r/);
    const event = lines.findLast(line => line.startsWith('event:'))?.slice(6).trim();
    const data = lines.filter(line => line.startsWith('data:')).map(line => line.slice(5).replace(/^ /, '')).join('\n');
    if (!data) return raw;
    let value;
    try { value = JSON.parse(data); } catch { throw new Error('MiniMax returned malformed stream data'); }
    if (event === 'error' || value?.type === 'error' || object(value?.error)) {
      failed = true;
      return `event: error\ndata: ${JSON.stringify(errorBody('MiniMax upstream stream error'))}\n\n`;
    }
    if (failed) throw new Error('MiniMax returned data after a stream error');
    if (value?.type === 'message_stop') terminal = true;
    return raw;
  };
  const body = new ReadableStream({
    async pull(controller) {
      try {
        for (;;) {
          const { value, done } = await reader.read();
          buffer += decoder.decode(value, { stream: !done });
          if (buffer.length > 16 * 1024 * 1024) throw new Error('MiniMax stream frame exceeds limit');
          let boundary, emitted = false;
          while ((boundary = /\r?\n\r?\n|\r\r/.exec(buffer))) {
            const end = boundary.index + boundary[0].length;
            controller.enqueue(encoder.encode(frame(buffer.slice(0, end))));
            emitted = true;
            buffer = buffer.slice(end);
          }
          if (done) {
            // Partial frames, including a partial message_stop, are never success.
            if (buffer.trim() || (!terminal && !failed)) throw new Error('MiniMax stream ended before its terminal event');
            controller.close();
            return;
          }
          // A network chunk may contain only part of an SSE frame. Read until
          // this pull can yield a frame, without waiting for another consumer pull.
          if (emitted) return;
        }
      } catch (error) {
        await reader.cancel().catch(() => {});
        const safe = new Set(['MiniMax stream frame exceeds limit', 'MiniMax stream ended before its terminal event', 'MiniMax returned malformed stream data']);
        controller.error(new Error(sanitize(safe.has(error?.message) ? error.message : 'MiniMax stream transport failed')));
      }
    },
    cancel(reason) { return reader.cancel(reason); },
  });
  return new Response(body, { status: response.status, headers });
}
