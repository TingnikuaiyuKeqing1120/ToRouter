// Supplier-only OpenCode/Magpie bridge. Executes user-installed trusted code.
// Loading and provider semantics follow Magpie b437e4a; unrelated hooks are ignored.
// The Go parent owns package verification, credentials, permissions and routing.
// This process is trusted extension code, NOT an OS sandbox.
import { createInterface } from 'node:readline';
import { AsyncLocalStorage } from 'node:async_hooks';
import { once } from 'node:events';
import { pathToFileURL } from 'node:url';
import fs from 'node:fs';
import path from 'node:path';

const maxLine = 24 * 1024 * 1024;
const maxState = 1024 * 1024;
const jobs = new AsyncLocalStorage();
const originalFetch = globalThis.fetch;
const pending = new Map();
const cancelledReplies = new Set();
const privateValues = new Set();
let sequence = 0;
let hooks = [], providerID, auth, config = { provider: {} }, flow;
let loader, snapshot, loaderVersion = 0;
let active;
let closing = false;

// No plugin log can enter protocol stdout or echo secrets to stderr.
for (const method of ['log', 'warn', 'error', 'info', 'debug', 'dir', 'trace']) {
  console[method] = () => {};
}
function copy(value) {
  return value === undefined ? undefined : JSON.parse(JSON.stringify(value));
}
function plain(value) {
  return value != null && typeof value === 'object' && !Array.isArray(value);
}
function checkState(value) {
  if (!plain(value) || !['oauth', 'api'].includes(value.type) || Buffer.byteLength(JSON.stringify(value)) > maxState) {
    throw new Error('invalid supplier auth state');
  }
  const remember = v => {
    if (typeof v === 'string' && v) privateValues.add(v);
    else if (plain(v) || Array.isArray(v)) Object.values(v).forEach(remember);
  };
  for (const [key, v] of Object.entries(value)) if (key !== 'type') remember(v);
  return copy(value);
}
function safeError(error) {
  if (String(error?.code ?? '').startsWith('ERR_MODULE_') || error?.code === 'ERR_UNKNOWN_FILE_EXTENSION') {
    return 'verified plugin module could not be loaded';
  }
  let message = String(error?.message ?? 'plugin operation failed');
  const redact = (value, privateState = false) => {
    if (typeof value === 'string' && value.length >= (privateState ? 1 : 4)) message = message.split(value).join('[redacted]');
    else if (plain(value) || Array.isArray(value)) for (const v of Object.values(value)) redact(v, privateState);
  };
  if (auth) for (const [key, value] of Object.entries(auth)) if (key !== 'type') redact(value, true);
  for (const value of privateValues) redact(value, true);
  redact(jobs.getStore()?.input);
  message = message.replace(/[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+/g, '[redacted]');
  message = message.replace(/(?:Bearer\s+|(?:access_token|refresh_token|session_token|authorization)\s*[=:]\s*)[^\s,;"<>]+/gi, '[redacted]');
  message = message.replace(/(["']?(?:access(?:_token)?|refresh(?:_token)?|id_token|token|session(?:_token)?|api[_-]?key|authorization|cookie|password|secret)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;<>]+)/gi, '$1[redacted]');
  for (const name of ['USERPROFILE', 'HOME']) {
    if (process.env[name]) message = message.split(process.env[name]).join('[local path]');
  }
  return message.slice(0, 1024);
}
async function emit(value) {
  const line = JSON.stringify(value) + '\n';
  if (Buffer.byteLength(line) > maxLine) throw new Error('plugin message exceeds limit');
  if (!process.stdout.write(line)) await once(process.stdout, 'drain');
}
function ask(event, payload) {
  const job = jobs.getStore();
  if (!job || closing) return Promise.reject(new Error('plugin callback outside an operation'));
  const id = ++sequence;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject, event, operationID: job.id });
    emit({ event, event_id: id, operation_id: job.id, payload }).catch(error => {
      pending.delete(id); reject(error);
    });
  });
}
function jobAccess() {
  const job = jobs.getStore();
  if (!job || job.cancelled || closing) throw new Error('plugin operation cancelled');
  return job;
}

// This is an integration guard for the verified packages' fetch calls, not
// isolation from arbitrary Node/Bun APIs. Existing control requests retain
// their own signal so an already-issued rotation can finish and be committed.
globalThis.fetch = async (input, init) => {
  jobAccess();
  const request = new Request(input, init);
  const url = new URL(request.url);
  if (url.protocol !== 'https:' && url.protocol !== 'http:') throw new Error('unsupported network protocol');
  if (url.username || url.password) throw new Error('credentials in request URL are not supported');
  await ask('network', { origin: url.origin, path: url.pathname, method: request.method });
  jobAccess();
  // Preserve the package's Fetch redirect contract, including manual cookie
  // flows. This callback is an integration check, not a network sandbox.
  return originalFetch(request);
};

async function saveState(next, mode = 'replace') {
  const value = checkState(next);
  const result = await ask('auth', { provider_id: providerID, mode, value });
  auth = checkState(result);
  loader = undefined;
  snapshot = undefined;
  loaderVersion++;
  return { data: true };
}
function providerAccess(input) {
  const id = input?.path?.id ?? input?.id ?? input?.providerID ?? providerID;
  if (id !== providerID) throw new Error('cross-provider auth access refused');
}
function noopSDK() {
  return new Proxy(async () => ({ data: undefined }), {
    get: (_, key) => key === 'then' ? undefined : noopSDK(),
  });
}
function clientAPI() {
  const known = {
    auth: {
      set: async input => { providerAccess(input); return saveState(input.body ?? input.auth); },
      get: async input => { providerAccess(input); return { data: copy(auth) }; },
      remove: async () => { throw new Error('supplier cannot remove a managed account'); },
    },
    config: { get: async () => ({ data: copy(config) }) },
    app: { log: async () => ({ data: true }) },
    tui: { showToast: async () => ({ data: true }) },
  };
  return new Proxy(known, { get: (target, key) => key in target ? target[key] : key === 'then' ? undefined : noopSDK() });
}
function entryPaths(target) {
  if (!fs.statSync(target).isDirectory()) return [target];
  let pkg = {};
  const manifest = path.join(target, 'package.json');
  if (fs.existsSync(manifest)) pkg = JSON.parse(fs.readFileSync(manifest, 'utf8'));
  const pick = v => typeof v === 'string' ? v : v?.import ?? v?.default;
  const names = [pick(pkg.exports?.['./server']), pkg.main, pick(pkg.exports?.['.']), typeof pkg.exports === 'string' ? pkg.exports : undefined,
    ...['index.ts', 'index.tsx', 'index.js', 'index.mjs', 'index.cjs']];
  return [...new Set(names.filter(v => typeof v === 'string').map(v => path.resolve(target, v)).filter(v => fs.existsSync(v)))];
}
function serverExports(module) {
  const d = module.default;
  if (plain(d) && ('server' in d || 'id' in d || 'tui' in d)) return typeof d.server === 'function' ? [d.server] : [];
  const out = new Set();
  const add = value => {
    if (typeof value === 'function') out.add(value);
    else if (Array.isArray(value)) value.forEach(add);
    else if (typeof value?.server === 'function') out.add(value.server);
  };
  Object.values(module).forEach(add);
  return [...out];
}
const validID = id => typeof id === 'string' && /^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$/.test(id);
function selected() {
  if (!providerID) throw new Error('select a supplier before this operation');
  return hooks.find(h => h.auth?.provider === providerID)?.auth;
}
function protocolFormat(npm) {
  if (npm === '@ai-sdk/openai-compatible') return 'openai_chat_completions';
  if (npm === '@ai-sdk/openai') return 'openai_responses';
  if (npm === '@ai-sdk/anthropic' || npm === '@ai-sdk/google-vertex/anthropic') return 'anthropic_messages';
  if (npm === '@ai-sdk/google') return 'gemini_generate_content';
  return '';
}
function modelRecord(id, m, p, loaderBase) {
  const npm = m.api?.npm ?? m.provider?.npm ?? p.npm ?? '';
  const ownAPI = m.api?.url || m.provider?.api;
  const api = loaderBase && (!ownAPI || ownAPI === p.api) ? loaderBase : ownAPI || p.api || '';
  return { id, name: m.name ?? id, api, npm, format: protocolFormat(npm), api_id: m.api?.id ?? m.id ?? id };
}
function providerInfo(id) {
  const cfg = config.provider?.[id] ?? {};
  const models = {};
  for (const [key, m] of Object.entries(cfg.models ?? {})) {
    if (!plain(m) || m.disabled) continue;
    models[key] = { ...copy(m), id: key, providerID: id,
      api: { id: m.id ?? key, url: m.provider?.api ?? cfg.api ?? '', npm: m.provider?.npm ?? cfg.npm ?? '' },
      options: copy(m.options ?? {}), headers: copy(m.headers ?? {}),
      cost: copy(m.cost ?? { input: 0, output: 0, cache: { read: 0, write: 0 } }),
      limit: copy(m.limit ?? { context: 0, output: 0 }),
      capabilities: { temperature: true, toolcall: true, input: { text: true }, output: { text: true } }, variants: copy(m.variants ?? {}) };
  }
  return { id, name: cfg.name ?? id, source: 'config', env: cfg.env ?? [], options: copy(cfg.options ?? {}), npm: cfg.npm ?? '', api: cfg.api ?? '', models };
}
function methodRecords(a) {
  return (a?.methods ?? []).flatMap((m, index) => {
    if (!['api', 'oauth'].includes(m?.type)) return [];
    return [{ index, type: m.type, label: String(m.label ?? m.type).slice(0, 160),
      prompts: (m.prompts ?? []).map(p => ({ type: p.type, key: p.key, label: p.message ?? p.label ?? p.key,
        placeholder: p.placeholder ?? '', options: copy(p.options), condition: copy(p.when) })) }];
  });
}
async function initialize(input) {
  if (hooks.length) throw new Error('supplier worker is already initialized');
  if (typeof input.module_path !== 'string') throw new Error('missing supplier module');
  providerID = input.provider_id || undefined;
  auth = input.auth == null ? undefined : checkState(input.auth);
  const target = path.resolve(input.module_path);
  const directory = fs.statSync(target).isDirectory() ? target : path.dirname(target);
  const context = { client: clientAPI(), directory, worktree: directory,
    project: { id: 'to-router', worktree: directory, time: { created: Date.now() } },
    serverUrl: new URL('http://127.0.0.1:4096'), $: globalThis.Bun?.$,
    experimental_workspace: { register() {} }, magpie: { option: () => false, options: () => ({}) } };
  let exports = [], entryError;
  for (const file of entryPaths(target)) {
    try { exports = serverExports(await import(pathToFileURL(file).href)); if (exports.length) break; }
    catch (error) { entryError = error; }
  }
  if (!exports.length) {
    const missing = String(entryError?.message ?? '').match(/Cannot find (?:package|module) ['"]([^'"]+)['"]/i)?.[1];
    const dependency = missing && !path.isAbsolute(missing) && /^[A-Za-z0-9@._/-]{1,200}$/.test(missing) ? `: missing dependency ${missing}` : '';
    throw new Error(entryError ? `supplier entry module could not be loaded${dependency}` : 'package exports no OpenCode server function');
  }
  for (const fn of exports) {
    const h = await fn(context, {});
    if (plain(h)) hooks.push(h);
  }
  for (const h of hooks) if (typeof h.config === 'function') await h.config(config);
  const ids = new Set();
  for (const h of hooks) {
    if (validID(h.auth?.provider)) ids.add(h.auth.provider);
    if (validID(h.provider?.id)) ids.add(h.provider.id);
  }
  const providers = [...ids].map(id => {
    const p = providerInfo(id);
    const a = hooks.find(h => h.auth?.provider === id)?.auth;
    return { id, name: p.name, api: p.api, npm: p.npm, methods: methodRecords(a),
      models: Object.entries(p.models).map(([id, m]) => modelRecord(id, m, p)) };
  }).filter(p => p.methods.length && (p.models.some(m => m.format) ||
    hooks.some(h => h.provider?.id === p.id && typeof h.provider.models === 'function')));
  if (!providers.length) throw new Error('插件没有可用的供应商模型接口，当前兼容层无法接入');
  if (providerID && !providers.some(p => p.id === providerID)) throw new Error('package does not export selected supplier');
  return providers;
}
function applicable(p, inputs) {
  if (p.when) {
    if (!(p.when.key in inputs)) return false;
    const equal = inputs[p.when.key] === p.when.value;
    if (p.when.op === 'eq' ? !equal : equal) return false;
  }
  return typeof p.condition !== 'function' || p.condition(inputs);
}
function successfulState(result, inputs = {}, key) {
  if (result?.type !== 'success') throw new Error('supplier did not complete authorization');
  if (result.provider && result.provider !== providerID) throw new Error('supplier returned another provider identity');
  const { type, provider, ...fields } = result;
  if ('refresh' in fields || 'access' in fields) return checkState({ ...fields, type: 'oauth' });
  if ('key' in fields || key) {
    const apiKey = fields.key ?? key;
    if (typeof apiKey !== 'string' || !apiKey.trim()) throw new Error('supplier API authorization returned no key');
    return checkState({ ...fields, type: 'api', key: apiKey, metadata: { ...inputs, ...(fields.metadata ?? {}) } });
  }
  throw new Error('supplier authorization returned no usable credential');
}
function consentState(state) {
  // Consent stays private in this process while the parent discovers models.
  // The parent's foreground consent transaction remains the storage boundary.
  auth = checkState(state);
  loader = snapshot = undefined;
  loaderVersion++;
  return copy(auth);
}
async function authorize(input) {
  jobAccess();
  const m = selected()?.methods?.[input.method];
  if (!m || !['api', 'oauth'].includes(m.type)) throw new Error('unknown supplier authorization method');
  const inputs = plain(input.inputs) ? input.inputs : {};
  for (const p of m.prompts ?? []) {
    if (!applicable(p, inputs)) continue;
    const value = inputs[p.key] ?? '';
    if (typeof p.validate === 'function' && await p.validate(value)) throw new Error(`invalid authorization input: ${p.key}`);
    if (p.type === 'select' && !(p.options ?? []).some(o => o.value === value)) throw new Error(`invalid authorization choice: ${p.key}`);
  }
  if (m.type === 'api') {
    const key = inputs.key ?? inputs.token ?? inputs.apiKey;
    const r = typeof m.authorize === 'function' ? await m.authorize(inputs) : { type: 'success', key };
    jobAccess();
    return { method: 'api', state: consentState(successfulState(r, inputs, key)) };
  }
  const result = await m.authorize(m.prompts?.length ? inputs : undefined);
  if (typeof result?.callback !== 'function' || !['auto', 'code'].includes(result.method)) throw new Error('supplier authorization callback is invalid');
  let url = '';
  if (result.url) {
    const value = new URL(result.url);
    if (!['https:', 'http:'].includes(value.protocol) || value.username || value.password) throw new Error('supplier authorization URL is invalid');
    url = value.href;
  }
  flow = result;
  return { url, instructions: String(result.instructions ?? '').slice(0, 8192), method: result.method };
}
async function fresh() {
  const a = selected();
  if (auth?.type !== 'oauth' || typeof a?.refresh !== 'function') return;
  const lead = Number.isFinite(a.refreshLead) ? Math.max(0, a.refreshLead) : 300000;
  if (!(auth.expires > 0 && auth.expires <= Date.now() + lead)) return;
  const delta = await a.refresh(copy(auth), providerInfo(providerID));
  if (delta !== undefined) {
    if (!plain(delta)) throw new Error('invalid supplier refresh result');
    await saveState({ ...auth, ...delta }, 'merge');
  }
}
async function info(live = false) {
  const p = providerInfo(providerID);
  if (live) for (const h of hooks) {
    if (h.provider?.id !== providerID || typeof h.provider.models !== 'function') continue;
    const models = await h.provider.models(copy(p), { auth: copy(auth) });
    if (!plain(models)) throw new Error('supplier returned an invalid model catalog');
    p.models = models;
  }
  for (const [key, m] of Object.entries(config.provider?.[providerID]?.models ?? {})) if (m.disabled) delete p.models[key];
  return p;
}
async function options(p) {
  if (loader) return loader;
  const version = loaderVersion;
  const a = selected();
  const out = typeof a?.loader === 'function' && auth ? await a.loader(async () => copy(auth), copy(p)) ?? {} : {};
  const value = { ...p.options, ...out };
  if (!value.apiKey && auth?.type === 'api') value.apiKey = auth.key;
  if (version === loaderVersion) loader = value;
  return value;
}
function bodyOf(value) {
  if (!value) return undefined;
  const bytes = Buffer.from(value, 'base64');
  try { return new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes); }
  catch { return bytes; }
}
async function inference(input) {
  const job = jobAccess();
  if (!auth) throw new Error('supplier account is not authorized');
  await fresh();
  const p = snapshot ?? await info(true);
  snapshot = p;
  const o = await options(p);
  jobAccess();
  const m = p.models[input.model_id];
  if (!m) throw new Error('model is absent from supplier catalog');
  const model = modelRecord(input.model_id, m, p, o.baseURL);
  if (!model.format || input.format && model.format !== input.format) throw new Error('request protocol does not match supplier model declaration');
  const npm = m?.api?.npm ?? m?.provider?.npm ?? p.npm;
  const headers = new Headers();
  if (o.apiKey) headers.set(npm === '@ai-sdk/anthropic' || npm === '@ai-sdk/google-vertex/anthropic' ? 'x-api-key' : npm === '@ai-sdk/google' ? 'x-goog-api-key' : 'authorization', npm === '@ai-sdk/anthropic' || npm === '@ai-sdk/google' || npm === '@ai-sdk/google-vertex/anthropic' ? o.apiKey : `Bearer ${o.apiKey}`);
  for (const bag of [o.headers, m?.headers, input.headers]) for (const [k, v] of Object.entries(bag ?? {})) headers.set(k, String(v));
  if (!headers.has('content-type') && input.body) headers.set('content-type', 'application/json');
  if (model.format === 'anthropic_messages' && !headers.has('anthropic-version')) headers.set('anthropic-version', '2023-06-01');
  let body = bodyOf(input.body);
  let parsed;
  if (typeof body === 'string') {
    try { parsed = JSON.parse(body); } catch {}
    if (plain(parsed) && 'model' in parsed) body = JSON.stringify({ ...parsed, model: model.api_id });
  }
  const base = model.api || o.baseURL;
  if (!base) throw new Error('supplier model declares no API endpoint');
  const url = new URL(base);
  const prefix = url.pathname.replace(/\/+$/, '');
  if (model.format === 'gemini_generate_content') {
    const streaming = parsed?.stream === true || input.url?.includes(':streamGenerateContent');
    url.pathname = `${prefix}/models/${model.api_id}:${streaming ? 'streamGenerateContent' : 'generateContent'}`;
    if (streaming) url.searchParams.set('alt', 'sse');
  } else url.pathname = prefix + ({ openai_chat_completions: '/chat/completions', openai_responses: '/responses', anthropic_messages: '/messages' })[model.format];
  const f = typeof o.fetch === 'function' ? o.fetch : fetch;
  const response = await f(url.href, { method: input.method ?? 'POST', headers, body, signal: job.controller.signal });
  if (!response || typeof response.status !== 'number' || !response.headers) throw new Error('supplier returned no HTTP response');
  const reader = response.body?.getReader();
  try {
    jobAccess();
    await ask('head', { status: response.status, headers: Object.fromEntries(response.headers) });
    if (reader) for (;;) {
      jobAccess();
      const { value, done } = await reader.read();
      if (done) break;
      for (let offset = 0; offset < value.byteLength; offset += 65536) {
        jobAccess();
        await ask('chunk', { data: Buffer.from(value.subarray(offset, offset + 65536)).toString('base64') });
      }
    }
    return { end: true };
  } finally { await reader?.cancel().catch(() => {}); }
}
async function operation(message) {
  const input = message.input ?? {};
  if (message.method === 'init') return initialize(input);
  selected();
  switch (message.method) {
    case 'authorize': return authorize(input);
    case 'complete': {
      jobAccess();
      if (!flow) throw new Error('supplier authorization is not started');
      const pendingFlow = flow; flow = undefined;
      const r = pendingFlow.method === 'code' ? await pendingFlow.callback(input.code ?? '') : await pendingFlow.callback();
      jobAccess();
      return consentState(successfulState(r));
    }
    case 'models': {
      jobAccess();
      if (!auth) throw new Error('supplier account is not authorized');
      await fresh();
      const p = await info(true);
      snapshot = p;
      // Loader-supplied account URLs override the provider-wide URL, but a
      // model's explicit protocol and API endpoint always remain its own.
      const o = await options(p);
      jobAccess();
      return Object.entries(p.models).map(([id, m]) => modelRecord(id, m, p, o.baseURL));
    }
    case 'fetch': return inference(input);
    case 'refresh': {
      jobAccess();
      const a = selected();
      if (!auth || typeof a?.refresh !== 'function') throw new Error('supplier refresh is unavailable');
      const delta = await a.refresh(copy(auth), providerInfo(providerID));
      if (delta !== undefined) await saveState({ ...auth, ...delta }, 'merge');
      return { changed: delta !== undefined };
    }
    default: throw new Error('unsupported supplier operation');
  }
}

async function dispatch(message) {
  if (!plain(message)) throw new Error('invalid protocol message');
  if (Number.isSafeInteger(message.reply_to)) {
    const waiter = pending.get(message.reply_to);
    if (!waiter) {
      if (cancelledReplies.delete(message.reply_to)) return;
      throw new Error('unknown callback reply');
    }
    pending.delete(message.reply_to);
    if (message.error) waiter.reject(new Error(String(message.error).slice(0, 256)));
    else waiter.resolve(message.result);
    return;
  }
  if (message.method === 'cancel') {
    if (active?.id === message.operation_id) {
      active.cancelled = true;
      active.controller.abort();
      for (const [id, waiter] of pending) {
        if (waiter.operationID === active.id && waiter.event !== 'auth') {
          pending.delete(id); cancelledReplies.add(id);
          waiter.reject(new Error('plugin operation cancelled'));
        }
      }
    }
    return;
  }
  if (typeof message.id !== 'string' || !message.id || typeof message.method !== 'string') throw new Error('invalid operation');
  if (active) {
    await emit({ id: message.id, error: 'plugin worker is busy' });
    return;
  }
  const job = { id: message.id, input: message.input, cancelled: false, controller: new AbortController() };
  active = job;
  void jobs.run(job, async () => {
    try {
      const result = await operation(message);
      await emit({ id: message.id, result: result ?? null });
    } catch (error) {
      await emit({ id: message.id, error: safeError(error) });
    } finally {
      if (active === job) active = undefined;
    }
  });
}
const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
lines.on('line', line => {
  if (Buffer.byteLength(line) > maxLine) {
    closing = true; process.exitCode = 1; lines.close(); return;
  }
  try {
    void dispatch(JSON.parse(line)).catch(() => { closing = true; process.exitCode = 1; lines.close(); });
  } catch { closing = true; process.exitCode = 1; lines.close(); }
});
lines.on('close', () => {
  closing = true;
  active?.controller.abort();
  for (const waiter of pending.values()) waiter.reject(new Error('parent disconnected'));
  pending.clear();
  flow = undefined;
  // A live rotation cannot be committed after the sole parent has gone.
  // The parent records this as unknown outcome, never auto-replays inference.
  process.exit();
});
