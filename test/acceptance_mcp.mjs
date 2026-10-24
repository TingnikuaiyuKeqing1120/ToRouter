// Cross-SDK acceptance against the built Windows CLI and its real HTTP server.
// Install @modelcontextprotocol/sdk@1.32.1 in a temporary prefix, then pass it
// with --sdk-root. Only a fresh data directory and a local synthetic provider
// are used. Credentials stay in memory and are never included in the report.
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { createRequire } from 'node:module';
import { createServer } from 'node:http';
import { mkdtemp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { parseArgs } from 'node:util';
import { randomUUID, createHash } from 'node:crypto';

const { values } = parseArgs({ options: { binaries: { type: 'string' }, 'sdk-root': { type: 'string' }, report: { type: 'string' }, 'keep-data': { type: 'boolean' } } });
assert(values.binaries && values['sdk-root'] && values.report, 'requires --binaries, --sdk-root and --report');
const requireSDK = createRequire(join(resolve(values['sdk-root']), 'package.json'));
const load = name => import(pathToFileURL(requireSDK.resolve(`@modelcontextprotocol/sdk/${name}.js`)).href);
const [{ Client }, { StreamableHTTPClientTransport }, { StdioClientTransport }] = await Promise.all([load('client/index'), load('client/streamableHttp'), load('client/stdio')]);
const sdkPath = requireSDK.resolve('@modelcontextprotocol/sdk/client/index.js');
const sdkPackage = JSON.parse(await readFile(resolve(sdkPath, '../../../../package.json'), 'utf8'));
const exe = join(resolve(values.binaries), 'ToRouter.exe');
const dataDir = await mkdtemp(join(tmpdir(), 'torouter-mcp-'));
const report = { client: `@modelcontextprotocol/sdk@${sdkPackage.version}`, binary_sha256: createHash('sha256').update(await readFile(exe)).digest('hex'), checks: [], success: false };
const clients = [];
let gateway;
let provider;
const delay = ms => new Promise(r => setTimeout(r, ms));
const record = name => { report.checks.push(name); console.log(`PASS ${name}`); };
async function bind(server) { await new Promise(r => server.listen(0, '127.0.0.1', r)); return server.address().port; }
async function close(server) { if (server?.listening) await new Promise(r => server.close(r)); }
async function tool(client, name, args = {}, expected = 'ok') {
  let result;
  try { result = await client.callTool({ name, arguments: args }); }
  catch (error) { if (expected === 'denied') return; throw new Error(`${name} transport failed`, { cause: error }); }
  if (expected === 'denied') { assert(result.isError, `${name}: expected denial`); return; }
  assert(!result.isError, `${name}: unexpected tool error`);
  const out = result.structuredContent || JSON.parse(result.content.find(v => v.type === 'text').text);
  if (expected === 'failed') { assert.equal(out.status, 'failed', `${name}: expected failure receipt`); return out; }
  if (out.status && out.request_id) { assert.equal(out.status, 'completed', `${name}: unexpected operation outcome`); return out.result; }
  return out;
}
const write = args => ({ ...args, request_id: randomUUID() });
try {
  const portProbe = createServer();
  const port = await bind(portProbe); await close(portProbe);
  const origin = `http://127.0.0.1:${port}`;
  const admin = execFileSync(exe, ['-data', dataDir, '-show-key'], { encoding: 'utf8', windowsHide: true }).trim();
  gateway = spawn(exe, ['-data', dataDir, '-port', `${port}`], { windowsHide: true, stdio: ['ignore', 'ignore', 'ignore'] });
  for (let i = 0; i < 100; i++) {
    try { if ((await fetch(origin + '/api/health')).status === 401) break; } catch {}
    assert(gateway.exitCode === null, 'isolated gateway exited');
    await delay(100);
  }
  const login = await fetch(origin + '/api/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-To-Router': '1' }, body: JSON.stringify({ key: admin }) });
  assert.equal(login.status, 200, 'admin login');
  const cookie = login.headers.get('set-cookie').split(';')[0];
  async function api(path, method = 'GET', body) {
    const res = await fetch(origin + '/api' + path, { method, headers: { Cookie: cookie, 'X-To-Router': '1', 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
    assert(res.ok, `admin endpoint ${path}: ${res.status}`);
    return res.status === 204 ? null : await res.json();
  }
  async function httpClient(token) {
    const client = new Client({ name: 'torouter-exe-http-acceptance', version: '1' });
    await client.connect(new StreamableHTTPClientTransport(new URL(origin + '/mcp'), { requestInit: { headers: { Authorization: `Bearer ${token}` } } }));
    clients.push(client); return client;
  }
  const managerConnection = await api('/mcp/connections', 'POST', { name: 'HTTP test', permissions: ['read', 'manage'] });
  const fullConnection = await api('/mcp/connections', 'POST', { name: 'Maintenance test', permissions: ['read', 'manage', 'maintain', 'sensitive'] });
  const manager = await httpClient(managerConnection.token);
  const full = await httpClient(fullConnection.token);
  assert.equal((await fetch(origin + '/mcp')).status, 401);
  const limited = await manager.listTools(), all = await full.listTools();
  assert(limited.tools.some(t => t.name === 'create_channel') && !limited.tools.some(t => t.name === 'delete_channel'));
  assert(!limited.tools.some(t => t.name === 'read_credential_key'));
  assert(all.tools.length > limited.tools.length);
  assert((await manager.readResource({ uri: 'torouter://help' })).contents[0].text.includes('MCP控制台'));
  report.tools = { ordinary: limited.tools.length, full: all.tools.length };
  record('HTTP initialization, permission-filtered tools and help');

  let providerCalls = 0;
  provider = createServer(async (req, res) => {
    for await (const _ of req) { /* drain */ }
    providerCalls++;
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ id: 'synthetic-chat', object: 'chat.completion', model: 'test-model', choices: [{ index: 0, message: { role: 'assistant', content: 'local test' }, finish_reason: 'stop' }], usage: { prompt_tokens: 20, completion_tokens: 5, total_tokens: 25 } }));
  });
  const upstreamURL = `http://127.0.0.1:${await bind(provider)}/v1`;
  const createArgs = write({ channel: { name: 'MCP synthetic', base_url: upstreamURL, api_key: 'synthetic-only-secret', endpoints: [{ format: 'openai_chat_completions', base_url: upstreamURL, state: '' }], enabled: true, models: ['test-model'] } });
  const created = await tool(manager, 'create_channel', createArgs);
  const replay = await tool(manager, 'create_channel', createArgs);
  assert.equal(created.channel.id, replay.channel.id);
  const ref = { kind: 'channel', channel_id: created.channel.id };
  const readChannel = async (client = manager) => (await tool(client, 'get_entities', { refs: [ref] })).items[0];
  const initial = await readChannel();
  assert(!JSON.stringify(initial).includes('synthetic-only-secret'));
  await tool(manager, 'update_channel', write({ channel_id: ref.channel_id, expected_version: initial.version, patch: { name: 'Updated through HTTP' } }));
  await tool(manager, 'update_channel', write({ channel_id: ref.channel_id, expected_version: initial.version, patch: { name: 'stale' } }), 'failed');
  const current = await readChannel();
  await tool(manager, 'update_channel', write({ channel_id: ref.channel_id, expected_version: current.version, patch: { base_url: 'https://receiver.invalid', endpoints: [{ format: 'openai_chat_completions', base_url: 'https://receiver.invalid', state: '' }] } }), 'failed');
  assert.equal((await readChannel()).version, current.version);
  record('exact patch, redaction, destination refusal and identical request replay');

  for (const id of ['test-a', 'test-b']) await tool(manager, 'create_public_model', write({ model: { id, display_name: id, enabled: true, fallback: true, sort_order: 0, bindings: [{ channel_id: ref.channel_id, model: 'test-model', priority: 0, enabled: true }], tags: [] } }));
  const models = (await tool(manager, 'search_entities', { kind: 'public_model' })).items;
  const edits = models.map(m => ({ id: m.ref.id, expected_version: m.version, patch: { display_name: 'Bulk ' + m.ref.id } }));
  await tool(manager, 'update_public_models', write({ items: [edits[0], { ...edits[1], expected_version: 'stale' }] }), 'failed');
  const unchanged = (await tool(manager, 'search_entities', { kind: 'public_model' })).items;
  assert.deepEqual(unchanged.map(m => m.version), models.map(m => m.version));
  await tool(manager, 'update_public_models', write({ items: edits }));
  record('public model atomic bulk rejects stale item without partial writes');

  const stdio = new Client({ name: 'torouter-exe-stdio-acceptance', version: '1' });
  const stdioTransport = new StdioClientTransport({ command: exe, args: ['mcp'], env: { TOROUTER_MCP_URL: origin + '/mcp', TOROUTER_MCP_TOKEN: managerConnection.token }, stderr: 'pipe' });
  let stdioErrors = ''; stdioTransport.stderr.on('data', b => { stdioErrors += b.toString(); });
  await stdio.connect(stdioTransport); clients.push(stdio);
  assert.equal((await stdio.listTools()).tools.length, limited.tools.length);
  const viaStdio = await readChannel(stdio);
  const stdioArgs = write({ channel_id: ref.channel_id, expected_version: viaStdio.version, patch: { name: 'Updated through stdio' } });
  await tool(stdio, 'update_channel', stdioArgs);
  assert.equal((await readChannel()).data.name, 'Updated through stdio');
  await tool(stdio, 'update_channel', stdioArgs);
  const receipt = await tool(stdio, 'get_operation_receipt', { request_id: stdioArgs.request_id });
  assert(receipt, 'receipt unavailable');
  const nextModels = (await tool(stdio, 'search_entities', { kind: 'public_model' })).items;
  await tool(stdio, 'update_public_models', write({ items: nextModels.map(m => ({ id: m.ref.id, expected_version: m.version, patch: { display_name: m.ref.id } })) }));
  await tool(stdio, 'update_channel', write({ ...stdioArgs, patch: { name: 'stale via stdio' } }), 'failed');
  assert.equal(stdioErrors, '', 'stdio wrote unexpected diagnostics');
  record('built CLI stdio read, write, bulk, conflict and receipt share HTTP state');

  const inferenceKey = (await tool(full, 'create_client_key', write({ name: 'synthetic inference', scope: { model_mode: 'all', model_ids: [], tag_rules: {} } }))).client_key;
  const secret = await tool(full, 'read_client_key', { id: inferenceKey.id });
  const response = await fetch(origin + '/v1/chat/completions', { method: 'POST', headers: { Authorization: `Bearer ${secret.key}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ model: 'test-a', messages: [{ role: 'user', content: 'synthetic' }], stream: false }) });
  assert.equal(response.status, 200, 'synthetic inference'); await response.json();
  let logs;
  for (let i = 0; i < 30; i++) { logs = await tool(full, 'query_usage_logs', { filter: {}, limit: 10 }); if (logs.items.length) break; await delay(100); }
  assert.equal(logs.items.length, 1);
  const before = await tool(full, 'get_usage_log', { id: logs.items[0].id });
  const preview = await tool(full, 'preview_amount_correction', write({ usage_ids: [before.id], reason: 'isolated acceptance', actual: { amount: 1.25, currency: 'USD', basis: { note: 'synthetic' } } }));
  assert.equal((await tool(full, 'get_usage_log', { id: before.id })).actual_cost, before.actual_cost);
  const change = await tool(full, 'apply_amount_correction', write({ preview_id: preview.preview_id }));
  const after = await tool(full, 'get_usage_log', { id: before.id });
  assert.equal(after.actual_cost, 1.25);
  for (const field of ['id', 'time', 'channel_id', 'model', 'input_tokens', 'output_tokens', 'official_value']) assert.deepEqual(after[field], before[field], `immutable ${field}`);
  const revert = await tool(full, 'preview_amount_revert', write({ change_id: change.id, reason: 'isolated acceptance revert' }));
  await tool(full, 'apply_amount_correction', write({ preview_id: revert.preview_id }));
  assert.equal((await tool(full, 'get_usage_log', { id: before.id })).actual_cost, before.actual_cost);
  assert.equal(providerCalls, 1, 'management caused unexpected model calls');
  record('synthetic request, historical amount preview/apply/conditional revert preserve facts');

  const exportData = await tool(manager, 'export_usage_csv', write({ filter: {} }));
  assert((await manager.readResource({ uri: exportData.uri })).contents.length > 0);
  const otherClient = await httpClient((await api('/mcp/connections', 'POST', { name: 'Other', permissions: ['read'] })).token);
  await assert.rejects(() => otherClient.readResource({ uri: exportData.uri }));
  record('export resource is bound to its management connection');

  const deletionArgs = { channel_id: ref.channel_id, expected_version: (await readChannel()).version };
  const deletion = await tool(full, 'delete_channel', write(deletionArgs));
  assert.equal(deletion.status, 'preview'); await readChannel();
  await tool(full, 'delete_channel', write({ ...deletionArgs, preview_id: deletion.preview_id }));
  assert.equal((await tool(full, 'search_entities', { kind: 'channel' })).total, 0);
  record('maintenance preview has no effect until explicit apply');

  const rotated = await api(`/mcp/connections/${managerConnection.connection.id}/rotate`, 'POST', {});
  await assert.rejects(() => manager.listTools());
  const newClient = await httpClient(rotated.token);
  await api(`/mcp/connections/${rotated.connection.id}`, 'PATCH', { enabled: false });
  await assert.rejects(() => newClient.listTools());
  await api(`/mcp/connections/${rotated.connection.id}`, 'PATCH', { enabled: true });
  await newClient.listTools();
  await api(`/mcp/connections/${rotated.connection.id}`, 'DELETE');
  await assert.rejects(() => newClient.listTools());
  record('rotation, disabling, re-enabling and revocation affect live clients');
  report.success = true;
} catch (error) {
  report.failure = error.message;
  process.exitCode = 1;
  console.error(error.message);
} finally {
  for (const client of clients) { try { await client.close(); } catch {} }
  gateway?.kill();
  if (gateway && gateway.exitCode === null) await new Promise(r => gateway.once('exit', r));
  await close(provider);
  if (values['keep-data']) report.data_directory = dataDir;
  else await rm(dataDir, { recursive: true, force: true });
  await mkdir(resolve(values.report, '..'), { recursive: true });
  await writeFile(values.report, JSON.stringify(report, null, 2) + '\n');
}
