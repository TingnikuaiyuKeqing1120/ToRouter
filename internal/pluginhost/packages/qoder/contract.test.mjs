import test from 'node:test';
import assert from 'node:assert/strict';
import { validate, setSanitizer } from './contract.mjs';
import { _internal as q } from './index.mjs';

const model = { config: { max_input_tokens: 10000 }, context: 20000, defaultWindow: 10000, windows: [10000, 20000], thinks: true, alwaysThinks: false, efforts: ['low', 'high'], defaultEffort: 'low' };
const base = () => ({ model: 'synthetic', messages: [{ role: 'system', content: 'Keep my instructions' }, { role: 'user', content: 'hi' }] });
function wire(chunks, done = true) {
  const text = chunks.map(c => `data: ${JSON.stringify({ statusCodeValue: 200, body: JSON.stringify(c) })}\n\n`).join('') +
    (done ? `data: ${JSON.stringify({ body: '[DONE]' })}\n\n` : '');
  return new Response(text).body;
}
const chunk = (delta, reason = null) => ({ choices: [{ index: 0, delta, finish_reason: reason }] });
test('request controls are preserved or explicitly refused before network', () => {
  const c = base();
  validate(c);
  for (const extra of [{ temperature: .1 }, { tool_choice: 'required' }, { stop: ['x'] }, { parallel_tool_calls: false }]) {
    assert.throws(() => validate({ ...c, ...extra }));
  }
  assert.throws(() => validate({ ...c, tools: [{ type: 'function', function: { name: 'x', strict: true } }] }));
  assert.doesNotThrow(() => validate({ ...c, tools: [{ type: 'function', function: { name: 'x', strict: false } }] }));
  assert.throws(() => validate({ ...c, messages: [...c.messages, { role: 'tool', tool_call_id: 'orphan', content: 'result' }] }));
  assert.throws(() => q.qoderBody({ ...c, reasoning_effort: 'medium' }, model));
  const body = q.qoderBody({ ...c, max_tokens: 40 }, model);
  assert.equal(body.system[0].text, 'Keep my instructions');
  assert.equal(body.parameters.max_tokens, 40);
  assert.equal(body.parameters.context_length, 10000);
  assert.equal(q.qoderBody({ ...c, messages: [{ role: 'user', content: 'long'.repeat(40000) }] }, model).parameters.context_length, 10000);
});
test('complete tool history and plaintext thought retain their identity and value', () => {
  const c = base();
  c.messages.push({ role: 'assistant', reasoning_content: 'synthetic thought', tool_calls: [{ id: 'call-stable', type: 'function', function: { name: 'read', arguments: '{"path":"synthetic"}' } }] });
  c.messages.push({ role: 'tool', tool_call_id: 'call-stable', content: 'result' });
  const result = q.qoderBody(c, model);
  assert.equal(result.messages[2].reasoning_content, 'synthetic thought');
  assert.equal(result.messages[2].tool_calls[0].id, result.messages[3].tool_call_id);
  c.messages[3].content = [{ type: 'image_url', image_url: { url: 'https://example.invalid/image' } }];
  assert.throws(() => validate(c));
});
test('abrupt EOF never becomes a successful stop', async () => {
  const events = [];
  for await (const e of q.events(wire([chunk({ content: 'partial' })], false))) events.push(e);
  assert.ok(events.some(e => e.text === 'partial'));
  assert.ok(events.some(e => e.error?.status === 502));
  assert.ok(!events.some(e => e.stop));
  const res = await q.answer(new Response(wire([chunk({ content: 'partial' })], false)), { model: 'synthetic', stream: false });
  assert.equal(res.status, 502);
});
test('interleaved native tools retain IDs, independent arguments and exact unknown usage', async () => {
  const data = [
    chunk({ tool_calls: [{ index: 0, id: 'call-A', function: { name: 'a', arguments: '{"x":' } }, { index: 1, id: 'call-B', function: { name: 'b', arguments: '{"y":' } }] }),
    chunk({ tool_calls: [{ index: 0, function: { arguments: '1}' } }, { index: 1, function: { arguments: '2}' } }] }, 'tool_calls'),
    { choices: [], usage: { completion_tokens: 7, prompt_tokens_details: { cached_tokens: 0 } } },
  ];
  const res = await q.answer(new Response(wire(data)), { model: 'synthetic' });
  const body = await res.json();
  assert.deepEqual(body.choices[0].message.tool_calls.map(t => [t.id, t.function.name, t.function.arguments]), [['call-A', 'a', '{"x":1}'], ['call-B', 'b', '{"y":2}']]);
  assert.deepEqual(body.usage, { completion_tokens: 7, prompt_tokens_details: { cached_tokens: 0 } });
});
test('refusals survive and unknown state or empty terminal is not successful', async () => {
  const res = await q.answer(new Response(wire([chunk({ refusal: 'synthetic refusal' }, 'content_filter')])), { model: 'synthetic' });
  const body = await res.json();
  assert.equal(body.choices[0].message.refusal, 'synthetic refusal');
  assert.equal(body.choices[0].finish_reason, 'content_filter');
  await assert.rejects(async () => { for await (const _ of q.events(wire([chunk({ reasoning_details: [{ data: 'opaque' }] })]))) {} });
  const events = [];
  for await (const e of q.events(wire([]))) events.push(e);
  assert.ok(events.some(e => e.error));
  assert.ok(!events.some(e => e.stop));
});
test('both HTTP errors and in-stream errors use the host credential redactor', async () => {
  setSanitizer(message => message.replaceAll('synthetic-private-token', '[redacted]'));
  const error = `data: ${JSON.stringify({ statusCodeValue: 502, body: 'failed synthetic-private-token' })}\n\n`;
  const http = await q.answer(new Response(error), { model: 'synthetic' });
  assert.equal(http.status, 502);
  assert.ok(!(await http.text()).includes('synthetic-private-token'));
  const before = `data: ${JSON.stringify({ body: JSON.stringify(chunk({ content: 'before failure' })) })}\n\n`;
  const streaming = await q.answer(new Response(before + error), { model: 'synthetic', stream: true });
  const text = await streaming.text();
  assert.ok(text.includes('before failure'));
  assert.ok(text.includes('[redacted]'));
  assert.ok(!text.includes('synthetic-private-token'));
  assert.ok(!text.includes('[DONE]'));
});
