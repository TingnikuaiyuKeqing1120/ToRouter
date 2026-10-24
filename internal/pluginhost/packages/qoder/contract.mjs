// to-router's reviewed request boundary. Do not quietly lower unsupported
// controls into prompt text or invent historical tool calls.
function object(v) { return v && typeof v === 'object' && !Array.isArray(v); }
let redact = () => 'Qoder request failed';
export function setSanitizer(fn) { redact = fn; }
export function sanitize(message) { return redact(String(message ?? 'Qoder request failed')); }
function fields(value, allowed, where) {
  if (!object(value)) throw new Error(`${where} must be an object`);
  for (const key of Object.keys(value)) {
    if (value[key] != null && !allowed.split(' ').includes(key)) {
      throw new Error(`Qoder cannot preserve ${where}.${key}`);
    }
  }
}
export function validate(chat) {
  fields(chat, 'model messages tools tool_choice stream stream_options max_tokens max_completion_tokens reasoning_effort context_length', 'request');
  if (typeof chat.model !== 'string' || !chat.model.trim() || !Array.isArray(chat.messages) || !chat.messages.length) throw new Error('model and messages are required');
  if (chat.stream != null && typeof chat.stream !== 'boolean') throw new Error('stream must be a boolean');
  if (chat.stream_options != null) {
    fields(chat.stream_options, 'include_usage', 'stream_options');
    if (chat.stream_options.include_usage != null && typeof chat.stream_options.include_usage !== 'boolean') throw new Error('include_usage must be a boolean');
  }
  if (chat.tool_choice != null && !['auto', 'none'].includes(chat.tool_choice)) throw new Error('Qoder cannot enforce the selected tool_choice');
  if (chat.max_tokens != null && chat.max_completion_tokens != null) throw new Error('use only one output token limit');
  for (const key of ['max_tokens', 'max_completion_tokens', 'context_length']) {
    if (chat[key] != null && (!Number.isSafeInteger(chat[key]) || chat[key] <= 0)) throw new Error(`${key} must be a positive integer`);
  }
  if (chat.reasoning_effort != null && !['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].includes(chat.reasoning_effort)) throw new Error('unsupported reasoning_effort');
  if (chat.tools != null && !Array.isArray(chat.tools)) throw new Error('tools must be an array');
  for (const t of chat.tools ?? []) {
    fields(t, 'type function', 'tool');
    if (t.type !== 'function') throw new Error('only function tools are supported');
    fields(t.function, 'name description parameters strict', 'tool.function');
    if (t.function.strict != null && t.function.strict !== false) throw new Error('Qoder cannot enforce a strict tool schema');
    if (typeof t.function.name !== 'string' || !t.function.name.trim() ||
      t.function.description != null && typeof t.function.description !== 'string' ||
      t.function.parameters != null && !object(t.function.parameters)) throw new Error('invalid function tool');
  }
  let leading = true;
  const pending = new Set(), seen = new Set();
  for (const m of chat.messages) {
    fields(m, 'role content tool_calls tool_call_id reasoning_content', 'message');
    if (!['system', 'user', 'assistant', 'tool'].includes(m.role)) throw new Error('unsupported message role');
    if (m.role === 'system') {
      if (!leading) throw new Error('Qoder cannot preserve a system instruction inside conversation history');
    } else leading = false;
    if (m.reasoning_content != null && (m.role !== 'assistant' || typeof m.reasoning_content !== 'string')) throw new Error('invalid historical reasoning_content');
    if (m.tool_call_id != null && m.role !== 'tool' || m.tool_calls != null && m.role !== 'assistant') throw new Error('tool fields on an incompatible message role');
    if (m.content != null && typeof m.content !== 'string') {
      if (!Array.isArray(m.content)) throw new Error('invalid message content');
      for (const part of m.content) {
        if (part?.type === 'text') {
          fields(part, 'type text', 'content');
          if (typeof part.text !== 'string') throw new Error('invalid text content');
        } else if (part?.type === 'image_url' && m.role === 'user') {
          fields(part, 'type image_url', 'content');
          fields(part.image_url, 'url', 'image_url');
          if (typeof part.image_url.url !== 'string' || !part.image_url.url) throw new Error('invalid image URL');
        } else throw new Error('Qoder cannot preserve this content type or role');
      }
    }
    if (m.role === 'tool') {
      if (!pending.delete(m.tool_call_id)) throw new Error('tool result has no matching unresolved call');
    } else if (pending.size) throw new Error('unresolved tool calls in history');
    if (m.tool_calls != null && !Array.isArray(m.tool_calls)) throw new Error('invalid historical tools');
    for (const call of m.tool_calls ?? []) {
      fields(call, 'id type function', 'tool_call');
      fields(call.function, 'name arguments', 'tool_call.function');
      if (call.type !== 'function' || typeof call.id !== 'string' || !call.id || seen.has(call.id) ||
          typeof call.function.name !== 'string' || !call.function.name || typeof call.function.arguments !== 'string') throw new Error('invalid historical tool call');
      try { JSON.parse(call.function.arguments); } catch { throw new Error('incomplete historical tool arguments'); }
      pending.add(call.id); seen.add(call.id);
    }
  }
  if (pending.size) throw new Error('tool results are required before continuing');
}
