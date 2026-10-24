import test from 'node:test';
import assert from 'node:assert/strict';
import { MiniMaxCodeAuthPlugin, MiniMaxCodeGlobalAuthPlugin, _torouter } from './index.mjs';

const sessionID = '00000000-0000-4000-8000-000000000001';
const oauth = (delta = {}) => ({ type: 'oauth', access: 'fixture-access', refresh: 'fixture-refresh', expires: Date.now() + 3600000, uid: 'fixture-user', realUserID: 'fixture-user', sessionID, ...delta });
const request = delta => ({ model: 'fixture-model', max_tokens: 256, messages: [{ role: 'user', content: 'fixture prompt' }], ...delta });
const json = (value, status = 200) => Response.json(value, { status });
const message = delta => ({ type: 'message', id: 'fixture-message', role: 'assistant', content: [{ type: 'text', text: 'ok' }], stop_reason: 'end_turn', ...delta });
const sse = values => values.map(value => `event: ${value.type}\ndata: ${JSON.stringify(value)}\n\n`).join('');
const eventResponse = text => new Response(text, { headers: { 'content-type': 'text/event-stream' } });
const api = 'https://agent.minimax.cn/mavis/api/v1/llm/v1/messages';
const plugin = async (state, save = async () => {}) => {
  const hooks = await MiniMaxCodeAuthPlugin({ client: { auth: { set: save } } });
  const adapter = await hooks.auth.loader(async () => state);
  return { hooks, send: body => adapter.fetch(api, { method: 'POST', body: Buffer.from(JSON.stringify(body)) }) };
};

test('only reviewed hooks; CN/global config declares native Messages and no static models', async () => {
  for (const [factory, id, host] of [[MiniMaxCodeAuthPlugin, 'minimax-code', 'cn'], [MiniMaxCodeGlobalAuthPlugin, 'minimax-code-global', 'io']]) {
    const hooks = await factory({ client: {} });
    assert.deepEqual(Object.keys(hooks).sort(), ['auth', 'config', 'provider']);
    assert.equal(hooks.auth.methods.length, 1);
    assert.equal(hooks.auth.usage, undefined);
    assert.equal(hooks.auth.checkin, undefined);
    const config = {};
    await hooks.config(config);
    assert.equal(config.provider[id].npm, '@ai-sdk/anthropic');
    assert.equal(config.provider[id].api, `https://agent.minimax.${host}/mavis/api/v1/llm/v1`);
    assert.deepEqual(config.provider[id].models, {});
  }
});

test('exact body preserves instructions, signed reasoning, tool history/choice, extensions and unknown usage', async t => {
  const body = request({ system: [{ type: 'text', text: 'fixture instruction' }], temperature: 0.3, thinking: { type: 'adaptive' }, output_config: { effort: 'high' },
    future_parameter: { preserve: true }, tools: [{ name: 'lookup', input_schema: { type: 'object' }, strict: true }], tool_choice: { type: 'tool', name: 'lookup', disable_parallel_tool_use: true },
    messages: [{ role: 'user', content: 'first' }, { role: 'assistant', content: [{ type: 'thinking', thinking: 'fixture thought', signature: 'fixture-signature' }, { type: 'tool_use', id: 'fixture-call', name: 'lookup', input: { nested: 1 } }] },
      { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'fixture-call', content: [{ type: 'text', text: 'fixture result' }] }] }],
  });
  const original = JSON.stringify(body);
  const sessions = [];
  t.mock.method(globalThis, 'fetch', async (url, init) => {
    assert.equal(String(url), api);
    assert.equal(init.body, original);
    assert.equal(init.headers.get('authorization'), 'Bearer fixture-access');
    assert.equal(init.headers.get('x-api-key'), 'sk-xxx');
    sessions.push(init.headers.get('x-mavis-session-id'));
    return json(message({ usage: { cache_read_input_tokens: 5, future_counter: 8 } }));
  });
  for (let i = 0; i < 2; i++) {
    const { send } = await plugin(oauth());
    const result = await (await send(body)).json();
    assert.deepEqual(result.usage, { cache_read_input_tokens: 5, future_counter: 8 });
    assert.equal('input_tokens' in result.usage, false);
  }
  assert.deepEqual(sessions, [sessionID, sessionID]);
  assert.equal(JSON.stringify(body), original);
});

test('native request validation rejects wrong protocol shapes before networking and preserves new fields', async t => {
  let calls = 0;
  t.mock.method(globalThis, 'fetch', async () => { calls++; return json(message()); });
  const { send } = await plugin(oauth());
  for (const body of [request({ max_tokens: undefined }), request({ messages: [{ role: 'system', content: 'bad placement' }] }), request({ tool_choice: 'required' }), request({ stream: 'true' })]) {
    assert.throws(() => _torouter.validate(body));
    await assert.rejects(send(body));
  }
  assert.equal(calls, 0);
  _torouter.validate(request({ future_parameter: { arbitrary: true } }));
});

test('401 inference is sent once and arbitrary HTTP errors never expose upstream secrets', async t => {
  let calls = 0;
  t.mock.method(globalThis, 'fetch', async () => { calls++; return new Response('fixture-secret fixture-user@private.invalid fixture prompt', { status: 401 }); });
  const { send } = await plugin(oauth());
  const response = await send(request());
  assert.equal(response.status, 401);
  const body = await response.text();
  assert.doesNotMatch(body, /fixture-secret|private\.invalid|fixture prompt/);
  assert.equal(calls, 1);
});

test('rotation waits for durable whole-state save before inference and preserves stable account/session', async t => {
  const order = [];
  const old = oauth({ expires: Date.now() + 1000, extraState: { future: true } });
  t.mock.method(globalThis, 'fetch', async (url, init) => {
    if (String(url).endsWith('/oauth2/token')) {
      order.push('refresh');
      const form = new URLSearchParams(init.body);
      assert.equal(form.get('grant_type'), 'refresh_token');
      assert.equal(form.get('refresh_token'), old.refresh);
      return json({ access_token: 'fixture-new-access', refresh_token: 'fixture-new-refresh', expires_in: 3600 });
    }
    order.push('inference');
    assert.equal(init.headers.get('authorization'), 'Bearer fixture-new-access');
    assert.equal(init.headers.get('x-mavis-session-id'), sessionID);
    return json(message());
  });
  const { send } = await plugin(old, async input => {
    order.push('save-start');
    assert.equal(input.path.id, 'minimax-code');
    assert.deepEqual(input.body.extraState, old.extraState);
    assert.equal(input.body.uid, old.uid);
    assert.equal(input.body.sessionID, sessionID);
    assert.equal(input.body.refresh, 'fixture-new-refresh');
    await new Promise(resolve => setTimeout(resolve, 5));
    order.push('save-durable');
  });
  await (await send(request())).text();
  assert.deepEqual(order, ['refresh', 'save-start', 'save-durable', 'inference']);
});

test('failed refresh or failed durable save blocks inference; explicit refresh does not return a second save delta', async t => {
  const calls = [];
  let mode = 'save-failed';
  t.mock.method(globalThis, 'fetch', async url => {
    calls.push(String(url));
    assert.match(String(url), /\/oauth2\/token$/);
    if (mode === 'refresh-failed') return json({ error: 'invalid_grant', error_description: 'fixture-secret' }, 400);
    return json({ access_token: 'fixture-new-access', refresh_token: 'fixture-new-refresh', expires_in: 3600 });
  });
  const old = oauth({ expires: Date.now() + 1000 });
  const failed = await plugin(old, async () => { throw new Error('fixture-secret persistence failed'); });
  await assert.rejects(failed.send(request()), error => !error.message.includes('fixture-secret'));
  mode = 'refresh-failed';
  await assert.rejects(failed.send(request()), /signed out/);
  mode = 'success';
  let saved = 0;
  const success = await plugin(old, async () => { saved++; });
  assert.equal(await success.hooks.auth.refresh(old), undefined);
  assert.equal(saved, 1);
  assert.equal(calls.length, 3);
});

test('stream bytes, interleaved tools, terminal and unknown usage are preserved across arbitrary chunks', async t => {
  const text = sse([{ type: 'message_start', message: { usage: { input_tokens: 7 } } },
    { type: 'content_block_start', index: 0, content_block: { type: 'tool_use', id: 'fixture-a', name: 'a', input: {} } },
    { type: 'content_block_start', index: 1, content_block: { type: 'tool_use', id: 'fixture-b', name: 'b', input: {} } },
    { type: 'content_block_delta', index: 1, delta: { type: 'input_json_delta', partial_json: '{"b":2}' } },
    { type: 'content_block_delta', index: 0, delta: { type: 'input_json_delta', partial_json: '{"a":1}' } },
    { type: 'message_delta', delta: { stop_reason: 'tool_use' }, usage: { future_counter: 3 } }, { type: 'message_stop' }]);
  t.mock.method(globalThis, 'fetch', async () => {
    const bytes = new TextEncoder().encode(text);
    let i = 0;
    return new Response(new ReadableStream({ pull(controller) { if (i >= bytes.length) controller.close(); else { controller.enqueue(bytes.slice(i, i + 13)); i += 13; } } }), { headers: { 'content-type': 'text/event-stream' } });
  });
  const { send } = await plugin(oauth());
  assert.equal(await (await send(request({ stream: true }))).text(), text);
});

test('truncation, incomplete terminal, malformed data and transport errors cannot become successful EOF', async t => {
  let text;
  t.mock.method(globalThis, 'fetch', async () => eventResponse(text));
  const { send } = await plugin(oauth());
  for (const broken of [sse([{ type: 'message_start', message: {} }]), 'event: message_stop\ndata: {"type":"message_stop"}', 'data: broken\n\n']) {
    text = broken;
    await assert.rejects((await send(request({ stream: true }))).text());
  }
  t.mock.method(globalThis, 'fetch', async () => new Response(new ReadableStream({ pull(controller) { controller.error(new Error('fixture-secret raw transport')); } }), { headers: { 'content-type': 'text/event-stream' } }));
  await assert.rejects((await send(request({ stream: true }))).text(), error => !error.message.includes('fixture-secret'));
});

test('SSE and JSON application errors are safe, and non-stream success requires a real stop reason', async t => {
  let response;
  t.mock.method(globalThis, 'fetch', async () => response);
  const { send } = await plugin(oauth());
  response = eventResponse(sse([{ type: 'error', error: { message: 'fixture-secret fixture-user@private.invalid' } }]));
  const streamed = await (await send(request({ stream: true }))).text();
  assert.match(streamed, /event: error/);
  assert.doesNotMatch(streamed, /fixture-secret|private\.invalid|message_stop/);
  response = eventResponse('event: error\ndata: {"message":"fixture-secret"}\n\n');
  assert.doesNotMatch(await (await send(request({ stream: true }))).text(), /fixture-secret/);
  response = eventResponse(sse([{ type: 'error', error: { message: 'fixture-secret' } }, { type: 'message_stop' }]));
  await assert.rejects((await send(request({ stream: true }))).text());
  response = json({ type: 'error', error: { message: 'fixture-secret' } });
  const failure = await send(request());
  assert.equal(failure.status, 502);
  assert.doesNotMatch(await failure.text(), /fixture-secret/);
  response = json(message({ stop_reason: null }));
  await assert.rejects(send(request()));
});

test('live model failure or empty reply never falls back, and successful capabilities come from live reply', async t => {
  const { hooks } = await plugin(oauth());
  let response;
  let calls = 0;
  t.mock.method(globalThis, 'fetch', async url => { calls++; assert.equal(new URL(url).pathname, '/mavis/api/v1/models'); return response; });
  response = json({ error: 'fixture-secret' }, 503);
  await assert.rejects(hooks.provider.models({ id: 'minimax-code', models: { stale: {} } }, { auth: oauth() }));
  response = json({ providers: [] });
  await assert.rejects(hooks.provider.models({ id: 'minimax-code', models: { stale: {} } }, { auth: oauth() }));
  response = json({ providers: [{ providerId: 'minimax', config: { models: { 'fixture-live': { name: 'Live', limit: { context: 8192, output: 512 }, modalities: { input: ['text', 'image'] } } } } }] });
  const models = await hooks.provider.models({ id: 'minimax-code', models: {} }, { auth: oauth() });
  assert.deepEqual(Object.keys(models), ['fixture-live']);
  assert.equal(models['fixture-live'].capabilities.input.image, true);
  assert.equal(models['fixture-live'].limit.context, 8192);
  assert.equal(calls, 3);
});

test('CN/global device OAuth creates independent complete state with signed stable identity lookup', async t => {
  for (const [factory, region, identityHost] of [[MiniMaxCodeAuthPlugin, 'cn', 'agent.minimaxi.com'], [MiniMaxCodeGlobalAuthPlugin, 'io', 'agent.minimax.io']]) {
    const calls = [];
    t.mock.method(globalThis, 'fetch', async (input, init) => {
      const url = new URL(input);
      calls.push(url.pathname);
      if (url.pathname === '/oauth2/device/code') {
        assert.equal(url.host, `account.minimax.${region}`);
        assert.equal(init.method, 'POST');
        const form = new URLSearchParams(init.body);
        assert.equal(form.get('client_id'), 'mcode-public');
        assert.equal(form.get('code_challenge_method'), 'S256');
        return json({ user_code: 'fixture-code', verification_uri: `https://account.minimax.${region}/fixture-device`, expired_in: 600, interval: 1 });
      }
      if (url.pathname === '/oauth2/token') {
        const form = new URLSearchParams(init.body);
        assert.equal(form.get('user_code'), 'fixture-code');
        assert.equal(form.get('grant_type'), 'urn:ietf:params:oauth:grant-type:device_code');
        assert.ok(form.get('code_verifier'));
        return json({ access_token: 'fixture-access', refresh_token: 'fixture-refresh', expires_in: 3600 });
      }
      assert.equal(url.host, identityHost);
      assert.equal(url.pathname, '/v1/api/user/info');
      assert.equal(init.method, 'GET');
      assert.ok(init.headers.yy);
      assert.ok(init.headers['x-signature']);
      return json({ data: { userInfo: { realUserID: 'fixture-user', userEmail: 'fixture-user@private.invalid', name: 'private display' } } });
    });
    const hooks = await factory({ client: {} });
    const flow = await hooks.auth.methods[0].authorize();
    assert.equal(flow.method, 'auto');
    assert.equal(flow.user_code, 'fixture-code');
    const state = await flow.callback();
    assert.equal(state.type, 'success');
    assert.equal(state.uid, 'fixture-user');
    assert.equal(state.realUserID, state.uid);
    assert.equal(state.accountId, state.uid);
    assert.match(state.sessionID, /^[\da-f-]{36}$/);
    assert.equal(state.email, undefined);
    assert.equal(state.name, undefined);
    assert.deepEqual(calls, ['/oauth2/device/code', '/oauth2/token', '/v1/api/user/info']);
  }
});

test('authorization rejects unapproved verification URLs and absent stable identity', async t => {
  let verification = 'https://unapproved.invalid/device';
  t.mock.method(globalThis, 'fetch', async url => {
    if (String(url).endsWith('/oauth2/device/code')) return json({ user_code: 'fixture-code', expired_in: 600, interval: 1, verification_uri: verification });
    if (String(url).endsWith('/oauth2/token')) return json({ access_token: 'fixture-access', refresh_token: 'fixture-refresh', expires_in: 3600 });
    return json({ data: { userInfo: { userEmail: 'fixture-user@private.invalid' } } });
  });
  const hooks = await MiniMaxCodeAuthPlugin({ client: {} });
  await assert.rejects(hooks.auth.methods[0].authorize());
  verification = 'https://account.minimax.cn/fixture-device';
  const flow = await hooks.auth.methods[0].authorize();
  await assert.rejects(flow.callback(), /no user id/);
});

test('inference signal and response reader cancellation reach the upstream', async t => {
  const controller = new AbortController();
  let cancelled = false;
  t.mock.method(globalThis, 'fetch', async (_url, init) => {
    assert.equal(init.signal, controller.signal);
    return new Response(new ReadableStream({ cancel() { cancelled = true; } }), { headers: { 'content-type': 'text/event-stream' } });
  });
  const hooks = await MiniMaxCodeAuthPlugin({ client: {} });
  const loader = await hooks.auth.loader(async () => oauth());
  const response = await loader.fetch(api, { body: JSON.stringify(request({ stream: true })), signal: controller.signal });
  await response.body.cancel('fixture cancel');
  assert.equal(cancelled, true);
});
