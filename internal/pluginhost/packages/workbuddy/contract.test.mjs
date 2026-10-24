import test, { afterEach } from 'node:test'
import assert from 'node:assert/strict'
import { WorkBuddyAuthPlugin, WorkBuddyAIAuthPlugin, _internal } from './index.mjs'
import { validate, setSanitizer } from './contract.mjs'

const realFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = realFetch; setSanitizer(() => 'WorkBuddy request failed') })
const auth = extra => ({ type: 'oauth', access: 'synthetic-access', refresh: 'synthetic-refresh', uid: 'synthetic-user', sessionID: '11111111-1111-4111-8111-111111111111', expires: Date.now() + 3600_000, ...extra })
const chat = extra => ({ model: 'synthetic-model', stream: true, messages: [{ role: 'system', content: 'You are Claude Code, Anthropic official CLI. Codex CLI is an open source project led by OpenAI.' }, { role: 'user', content: 'hi' }], ...extra })
const chunk = (delta = {}, finish_reason = null) => ({ choices: [{ index: 0, delta, finish_reason }] })
const wire = (...events) => events.map(e => 'data: ' + (typeof e === 'string' ? e : JSON.stringify(e)) + '\n\n').join('')
const stream = text => new Response(text, { headers: { 'content-type': 'text/event-stream' } })
const env = data => new Response(JSON.stringify({ code: 0, data }))
async function loader(plugin = WorkBuddyAuthPlugin, a = auth(), client = {}) {
  const hooks = await plugin({ client })
  return hooks.auth.loader(async () => a)
}

test('host hooks are limited to config/provider/auth and one browser method per region', async () => {
  for (const [plugin, id, api] of [[WorkBuddyAuthPlugin, 'workbuddy', 'https://copilot.tencent.com/v2'], [WorkBuddyAIAuthPlugin, 'workbuddy-ai', 'https://www.workbuddy.ai/v2']]) {
    const hooks = await plugin({ client: {} })
    assert.deepEqual(Object.keys(hooks).sort(), ['auth', 'config', 'provider'])
    assert.equal(hooks.auth.methods.length, 1)
    const config = {}; await hooks.config(config)
    assert.equal(config.provider[id].api, api)
    assert.equal(config.provider[id].npm, '@ai-sdk/openai-compatible')
    assert.deepEqual(config.provider[id].models, {})
  }
})

test('request bytes, prompt, tools, history, controls are unchanged in one network send', async () => {
  const c = chat({ temperature: .25, max_tokens: 19, stop: ['END'], stream_options: { include_usage: true }, reasoning_effort: 'high', tool_choice: 'auto', tools: [{ type: 'function', function: { name: 'read', parameters: { type: 'object', properties: { path: { type: 'string', pattern: '^a+$' } }, additionalProperties: false, required: ['path'] } } }] })
  c.messages.push({ role: 'assistant', reasoning_content: 'prior thought', tool_calls: [{ id: 'call-A', type: 'function', function: { name: 'read', arguments: '{"path":"aaa"}' } }] }, { role: 'tool', tool_call_id: 'call-A', content: 'result' }, { role: 'user', content: 'next' })
  const text = JSON.stringify(c, null, 2), sent = []
  globalThis.fetch = async (url, init) => { sent.push({ url, ...init }); return stream(wire(chunk({ content: 'ok' }, 'stop'), '[DONE]')) }
  const l = await loader(WorkBuddyAuthPlugin, auth({ enterpriseId: 'synthetic-enterprise', domain: 'enterprise.example.test' }))
  const res = await l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: Buffer.from(text), headers: { Authorization: 'do-not-forward', 'x-api-key': 'do-not-forward' } })
  assert.equal(sent.length, 1)
  assert.equal(sent[0].body.toString(), text)
  assert.equal(sent[0].headers.get('Authorization'), 'Bearer synthetic-access')
  assert.equal(sent[0].headers.get('X-Domain'), 'enterprise.example.test')
  assert.equal(sent[0].headers.get('X-Enterprise-Id'), 'synthetic-enterprise')
  assert.equal(sent[0].headers.get('x-api-key'), null)
  assert.ok((await res.text()).includes('[DONE]'))
})

test('known unsupported controls refuse before refresh or inference; no prompt defaults', async () => {
  const l = await loader(WorkBuddyAuthPlugin, auth({ expires: 1 }))
  let calls = 0; globalThis.fetch = async () => { calls++; throw new Error('must not fetch') }
  for (const extra of [{ stream: false }, { max_completion_tokens: 4 }, { messages: [{ role: 'developer', content: 'keep role' }] }, { tool_choice: 'required' }, { tool_choice: { type: 'function', function: { name: 'x' } } }, { tools: [{ type: 'function', function: { name: 'has.dot' } }] }, { tools: [{ type: 'function', function: { name: 'read', strict: true } }] }, { tools: [{ type: 'function', function: { name: 'read', parameters: { anyOf: [{ type: 'object' }] } } }] }]) {
    await assert.rejects(l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat(extra)) }))
  }
  assert.equal(calls, 0)
  const noSystem = chat({ messages: [{ role: 'user', content: 'no default instruction please' }] })
  assert.doesNotThrow(() => validate(noSystem))
})

test('network errors and 400 never replay inference or ask billing', async () => {
  const l = await loader()
  let calls = 0
  globalThis.fetch = async () => { calls++; throw new Error('synthetic network failure') }
  await assert.rejects(l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) }))
  assert.equal(calls, 1)
  globalThis.fetch = async () => { calls++; return new Response('{"code":11153,"msg":"must end with a user message"}', { status: 400 }) }
  assert.equal((await l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) })).status, 400)
  assert.equal(calls, 2)
})

test('rotated tokens wait for durable ACK and persistence failure prevents inference', async () => {
  let ack, sawSave
  const saved = new Promise(resolve => { sawSave = resolve })
  const client = { auth: { set: async ({ body }) => { assert.equal(body.access, 'synthetic-new'); assert.equal(body.sessionID, auth().sessionID); sawSave(); await new Promise(resolve => { ack = resolve }) } } }
  const sends = []
  globalThis.fetch = async (url, init) => { sends.push({ url, init }); return url.endsWith('/refresh') ? env({ accessToken: 'synthetic-new', refreshToken: 'synthetic-new-refresh', expiresIn: 3600 }) : stream(wire(chunk({}, 'stop'), '[DONE]')) }
  const l = await loader(WorkBuddyAuthPlugin, auth({ expires: 1 }), client)
  const pending = l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) })
  await saved
  assert.equal(sends.length, 1)
  ack(); const res = await pending; await res.text()
  assert.equal(sends.length, 2)
  assert.equal(sends[1].init.headers.get('Authorization'), 'Bearer synthetic-new')
  const failing = await loader(WorkBuddyAuthPlugin, auth({ expires: 1 }), { auth: { set: async () => { throw new Error('durable save refused') } } })
  await assert.rejects(failing.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) }), /durable save refused/)
  assert.equal(sends.length, 3)
})

test('live model failure never becomes a static list; enterprise headers and dynamic domain stay on fixed origin', async () => {
  const hooks = await WorkBuddyAIAuthPlugin({ client: {} })
  globalThis.fetch = async (url, init) => {
    assert.equal(url, 'https://www.workbuddy.ai/v3/config')
    assert.equal(init.headers.get('X-Tenant-Id'), 'synthetic-enterprise')
    assert.equal(init.headers.get('X-Domain'), 'tenant.example.test')
    return env({ agents: [{ name: 'cli', models: ['synthetic-model'] }], models: [{ id: 'synthetic-model', maxInputTokens: 123, supportsImages: true }] })
  }
  const models = await hooks.provider.models({ id: 'workbuddy-ai' }, { auth: auth({ enterpriseId: 'synthetic-enterprise', domain: 'tenant.example.test' }) })
  assert.equal(models['synthetic-model'].limit.context, 123)
  globalThis.fetch = async () => new Response('failure', { status: 503 })
  await assert.rejects(hooks.provider.models({ id: 'workbuddy-ai', models: { fallback: {} } }, { auth: auth() }))
  globalThis.fetch = async () => env({})
  await assert.rejects(hooks.provider.models({ id: 'workbuddy-ai', models: { fallback: {} } }, { auth: auth() }), /no live models/)
})

test('browser authorization checks region URL, stable identity and required fields without using nickname or phone', async () => {
  for (const [plugin, origin, login] of [[WorkBuddyAuthPlugin, 'https://copilot.tencent.com', 'https://www.codebuddy.cn/login'], [WorkBuddyAIAuthPlugin, 'https://www.workbuddy.ai', 'https://www.codebuddy.ai/login']]) {
    const asked = []
    globalThis.fetch = async (url, init) => {
      const u = new URL(url); assert.equal(u.origin, origin); asked.push(u.pathname)
      if (u.pathname.endsWith('/state')) return env({ state: 'synthetic-state', authUrl: login })
      if (u.pathname.endsWith('/token')) return env({ accessToken: 'synthetic-access', refreshToken: 'synthetic-refresh', expiresIn: 3600, domain: 'tenant.example.test' })
      return env({ uid: 'synthetic-user', type: 'ultimate', enterpriseId: 'synthetic-enterprise', nickname: 'do-not-use', phoneNumber: 'do-not-use' })
    }
    const hooks = await plugin({ client: {} }), flow = await hooks.auth.methods[0].authorize()
    assert.equal(flow.method, 'auto'); assert.ok(flow.url.startsWith(login))
    const a = await flow.callback()
    assert.equal(a.type, 'success'); assert.equal(a.uid, 'synthetic-user'); assert.equal(a.enterpriseId, 'synthetic-enterprise')
    assert.match(a.sessionID, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    assert.ok(!JSON.stringify(a).includes('do-not-use'))
    assert.deepEqual(asked, ['/v2/plugin/auth/state', '/v2/plugin/auth/token', '/v2/plugin/login/account'])
  }
  globalThis.fetch = async () => env({ state: 'synthetic-state', authUrl: 'https://unverified.example.test/login' })
  const hooks = await WorkBuddyAuthPlugin({ client: {} })
  await assert.rejects(hooks.auth.methods[0].authorize(), /unverified/)
})

test('unresolved enterprise ownership does not silently sign into personal account', async () => {
  globalThis.fetch = async url => {
    const p = new URL(url).pathname
    return p.endsWith('/state') ? env({ state: 'synthetic-state', authUrl: 'https://www.codebuddy.cn/login' }) : p.endsWith('/token') ? env({ accessToken: 'synthetic-access', refreshToken: 'synthetic-refresh', expiresIn: 3600 }) : p.endsWith('/account') ? env({ uid: 'synthetic-user', type: 'ultimate' }) : env({ accounts: [] })
  }
  const hooks = await WorkBuddyAuthPlugin({ client: {} }), flow = await hooks.auth.methods[0].authorize()
  assert.equal((await flow.callback()).type, 'failed')
})

test('stream preserves interleaved tools, refusal, terminal and unknown usage exactly', async () => {
  const events = [chunk({ tool_calls: [{ index: 0, id: 'call-A', function: { name: 'a', arguments: '{"x":' } }, { index: 1, id: 'call-B', function: { name: 'b', arguments: '{"y":' } }] }), chunk({ tool_calls: [{ index: 1, function: { arguments: '2}' } }, { index: 0, function: { arguments: '1}' } }] }, 'tool_calls'), { choices: [], usage: { completion_tokens: 7 } }, '[DONE]']
  const raw = wire(...events)
  assert.equal(await _internal.guardedStream(stream(raw)).text(), raw)
  const refused = wire(chunk({ refusal: 'synthetic refusal' }, 'content_filter'), '[DONE]')
  assert.equal(await _internal.guardedStream(stream(refused)).text(), refused)
})

test('abrupt EOF, empty DONE and SSE errors never get a synthesized success', async () => {
  for (const raw of [wire(chunk({ content: 'partial' })), wire('[DONE]'), 'data: {"choices":']) {
    const text = await _internal.guardedStream(stream(raw)).text()
    assert.ok(text.includes('"error"')); assert.ok(!text.includes('[DONE]'))
  }
  const noMarker = wire(chunk({ content: 'ok' }, 'stop'))
  assert.equal(await _internal.guardedStream(stream(noMarker)).text(), noMarker)
})

test('HTTP and SSE errors redact known credentials, no raw untrusted headers or diagnostics', async () => {
  setSanitizer(message => message.replaceAll('synthetic-secret', '[redacted]'))
  globalThis.fetch = async () => new Response('synthetic-secret denied', { status: 403, statusText: 'synthetic-secret', headers: { 'set-cookie': 'synthetic-secret', 'x-debug': 'synthetic-secret' } })
  const l = await loader()
  const res = await l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) })
  assert.equal(res.status, 403); assert.equal(res.headers.get('set-cookie'), null)
  assert.ok(!(await res.text()).includes('synthetic-secret'))
  const raw = wire(chunk({ content: 'partial' }), { error: { message: 'synthetic-secret broke' } }, '[DONE]')
  const text = await _internal.guardedStream(stream(raw)).text()
  assert.ok(text.includes('partial')); assert.ok(text.includes('[redacted]')); assert.ok(!text.includes('synthetic-secret')); assert.ok(!text.includes('[DONE]'))
})

test('file-follow states and arbitrary inference URLs reject without network', async () => {
  let calls = 0; globalThis.fetch = async () => { calls++; throw new Error('unexpected network') }
  const l = await loader(WorkBuddyAuthPlugin, auth({ source: 'desktop' }))
  await assert.rejects(l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) }))
  const valid = await loader()
  await assert.rejects(valid.fetch('https://unverified.example.test/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) }))
  await assert.rejects(valid.fetch('https://copilot.tencent.com/v2/chat/completions?extra=1', { method: 'POST', body: JSON.stringify(chat()) }))
  assert.equal(calls, 0)
})

test('personal browser authorization succeeds with a generic account label and missing identity fails', async () => {
  let missing = false
  globalThis.fetch = async url => {
    const p = new URL(url).pathname
    return p.endsWith('/state') ? env({ state: 'synthetic-state', authUrl: 'https://www.codebuddy.cn/login' }) : p.endsWith('/token') ? env({ accessToken: 'synthetic-access', refreshToken: 'synthetic-refresh', expiresIn: 3600 }) : env({ uid: missing ? '' : 'synthetic-personal', type: 'personal', nickname: 'do-not-use' })
  }
  const hooks = await WorkBuddyAuthPlugin({ client: {} })
  let flow = await hooks.auth.methods[0].authorize(), result = await flow.callback()
  assert.equal(result.type, 'success'); assert.equal(result.accountId, 'WorkBuddy'); assert.equal(result.enterpriseId, undefined)
  missing = true
  flow = await hooks.auth.methods[0].authorize(); result = await flow.callback()
  assert.equal(result.type, 'failed')
})

test('persisted session identity survives module reload, tool steps and token rotation without cross-account collisions', async () => {
  const sent = []
  globalThis.fetch = async (_url, init) => { sent.push(new Headers(init.headers)); return stream(wire(chunk({ content: 'ok' }, 'stop'), '[DONE]')) }
  const restarted = await import(new URL('./index.mjs?synthetic-worker-restart=1', import.meta.url))
  const stable = auth(), original = chat()
  const steps = chat({ messages: [...original.messages, { role: 'assistant', tool_calls: [{ id: 'call-stable', type: 'function', function: { name: 'read', arguments: '{}' } }] }, { role: 'tool', tool_call_id: 'call-stable', content: 'result' }] })
  const nextTurn = chat({ messages: [...steps.messages, { role: 'assistant', content: 'answered' }, { role: 'user', content: 'new user turn' }] })
  const first = await loader(WorkBuddyAuthPlugin, stable)
  const second = await loader(restarted.WorkBuddyAuthPlugin, { ...stable, access: 'synthetic-rotated-access', refresh: 'synthetic-rotated-refresh' })
  const other = await loader(restarted.WorkBuddyAuthPlugin, auth({ uid: 'synthetic-other-user', sessionID: '22222222-2222-4222-8222-222222222222' }))
  for (const [l, c] of [[first, original], [second, steps], [second, nextTurn], [other, original]]) {
    const res = await l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(c) }); await res.text()
  }
  const conv = h => h.get('X-Conversation-ID'), turn = h => h.get('X-Conversation-Request-ID')
  assert.equal(conv(sent[0]), conv(sent[1])); assert.equal(turn(sent[0]), turn(sent[1]))
  assert.equal(conv(sent[0]), conv(sent[2])); assert.notEqual(turn(sent[0]), turn(sent[2]))
  assert.notEqual(conv(sent[0]), conv(sent[3])); assert.notEqual(turn(sent[0]), turn(sent[3]))
  assert.notEqual(sent[0].get('X-Request-ID'), sent[1].get('X-Request-ID'))
  assert.throws(() => _internal.assertAuth(_internal.SITES.workbuddy, auth({ sessionID: undefined })), /session identity/)
  assert.throws(() => _internal.assertAuth(_internal.SITES.workbuddy, auth({ sessionID: 'not-a-uuid' })), /session identity/)
})

test('data after DONE and duplicate DONE explicitly fail when the stream is read to EOF', async () => {
  setSanitizer(message => message)
  for (const after of [chunk({ content: 'late' }), '[DONE]']) {
    const text = await _internal.guardedStream(stream(wire(chunk({ content: 'done' }, 'stop'), '[DONE]', after))).text()
    assert.ok(text.includes('"error"')); assert.ok(text.includes('after its end marker'))
    assert.equal(text.split('data: [DONE]').length - 1, 1)
    assert.ok(!text.includes('late'))
  }
})

test('control failures cannot echo unknown tokens, phone numbers or response text before credentials are saved', async () => {
  setSanitizer(message => message) // Deliberately no host knowledge of new material.
  const privateText = 'synthetic-new-unknown-token phone=13900000000'
  const hooks = await WorkBuddyAuthPlugin({ client: {} })
  globalThis.fetch = async () => new Response(JSON.stringify({ code: 12345, msg: privateText }), { status: 403, statusText: privateText })
  await assert.rejects(hooks.auth.methods[0].authorize(), e => e.message.includes('HTTP 403, code 12345') && !e.message.includes(privateText) && !e.message.includes('13900000000'))
  globalThis.fetch = async () => { throw new Error(privateText) }
  await assert.rejects(hooks.auth.methods[0].authorize(), e => e.message === 'WorkBuddy control request failed')
  globalThis.fetch = async url => new URL(url).pathname.endsWith('/state') ? env({ state: 'synthetic-state', authUrl: 'https://www.codebuddy.cn/login' }) : new Response(JSON.stringify({ code: 12346, message: privateText }), { status: 401 })
  const flow = await hooks.auth.methods[0].authorize(), result = await flow.callback()
  assert.equal(result.type, 'failed'); assert.ok(result.error.includes('HTTP 401, code 12346')); assert.ok(!result.error.includes(privateText)); assert.ok(!result.error.includes('13900000000'))
  globalThis.fetch = async () => new Response(privateText, { status: 502 })
  await assert.rejects(hooks.auth.refresh(auth()), e => e.message === 'WorkBuddy control request refused (HTTP 502, code 0)')
})

test('429 and 503 preserve bounded Retry-After HTTP dates and delay seconds for gateway parsing', async () => {
  const l = await loader()
  for (const status of [429, 503]) {
    for (const retry of ['Wed, 21 Oct 2026 07:28:00 GMT', 'Wednesday, 21-Oct-26 07:28:00 GMT', 'Wed Oct 21 07:28:00 2026', '120']) {
      globalThis.fetch = async () => new Response('{"error":"synthetic busy"}', { status, headers: { 'Retry-After': retry } })
      const res = await l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) })
      assert.equal(res.status, status); assert.equal(res.headers.get('Retry-After'), retry)
      await res.text()
    }
  }
  globalThis.fetch = async () => new Response('synthetic busy', { status: 429, headers: { 'Retry-After': '1'.repeat(129) } })
  const res = await l.fetch('https://copilot.tencent.com/v2/chat/completions', { method: 'POST', body: JSON.stringify(chat()) })
  assert.equal(res.headers.get('Retry-After'), null)
  await res.text()
})
