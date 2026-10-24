// Private, versioned stdio bridge for verified OpenCode-style provider hooks.
// The Go parent owns package verification, credentials, permissions and routing.
// This process is trusted extension code, NOT an OS sandbox.
import { createInterface } from 'node:readline';
import { AsyncLocalStorage } from 'node:async_hooks';
import { once } from 'node:events';
import { pathToFileURL } from 'node:url';

const protocol = 1;
const maxLine = 24 * 1024 * 1024;
const maxState = 1024 * 1024;
const jobs = new AsyncLocalStorage();
const originalFetch = globalThis.fetch;
const callbacks = new Map();
const pending = new Map();
const cancelledReplies = new Set();
let sequence = 0;
let hooks, providerID, auth, descriptor, allowedMethods, contract;
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
  if (!plain(value) || value.type !== 'oauth' || Buffer.byteLength(JSON.stringify(value)) > maxState) {
    throw new Error('invalid OAuth plugin state');
  }
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
  redact(auth, true);
  redact(jobs.getStore()?.input);
  message = message.replace(/[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+/g, '[redacted]');
  message = message.replace(/(?:Bearer\s+|(?:access_token|refresh_token|session_token|authorization)\s*[=:]\s*)[^\s,;"<>]+/gi, '[redacted]');
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
  // Never let a redirect carry credentials past the approved origin/path.
  const response = await originalFetch(new Request(request, { redirect: 'manual' }));
  if (response.status >= 300 && response.status < 400) {
    await response.body?.cancel();
    throw new Error('plugin network redirect refused');
  }
  return response;
};

async function saveState(next, mode) {
  const value = checkState(next);
  // A returned rotation must still be persisted after cancellation. The parent
  // validates identity/version and acknowledges durable storage before use.
  const result = await ask('auth', { provider_id: providerID, mode, value });
  auth = checkState(result);
  return { data: {} };
}
function clientAPI() {
  return new Proxy({
    auth: {
      async set(input) {
        if (input?.path?.id !== providerID) throw new Error('cross-provider auth write refused');
        return saveState(input.body, 'replace');
      },
    },
  }, {
    get(target, property) {
      if (property in target) return target[property];
      throw new Error(`unsupported plugin SDK method: ${String(property)}`);
    },
  });
}
async function initialize(input) {
  if (hooks) throw new Error('plugin worker is already initialized');
  if (input.protocol !== protocol || typeof input.module_path !== 'string' || typeof input.export_name !== 'string' ||
      typeof input.provider_id !== 'string' || !Array.isArray(input.methods)) throw new Error('invalid plugin initialization');
  providerID = input.provider_id;
  allowedMethods = new Set(input.methods);
  if (!allowedMethods.size || [...allowedMethods].some(n => !Number.isInteger(n) || n < 0)) throw new Error('no verified authorization method');
  auth = input.auth == null ? undefined : checkState(input.auth);
  const module = await import(pathToFileURL(input.module_path).href);
  if (typeof module[input.export_name] !== 'function') throw new Error('verified plugin entry is missing');
  const loaded = await module[input.export_name]({ client: clientAPI() });
  contract = module._torouter;
  if (typeof contract?.setSanitizer === 'function') contract.setSanitizer(message => safeError({ message }));
  if (!plain(loaded)) throw new Error('invalid plugin hooks');
  for (const key of Object.keys(loaded)) {
    if (!['auth', 'provider', 'config'].includes(key)) throw new Error(`unsupported plugin hook: ${key}`);
  }
  if (loaded.auth?.provider !== providerID || loaded.provider?.id !== providerID ||
      typeof loaded.auth?.loader !== 'function' || typeof loaded.provider?.models !== 'function' ||
      !Array.isArray(loaded.auth?.methods)) throw new Error('plugin does not declare the verified provider contract');
  const config = { provider: {} };
  if (loaded.config) await loaded.config(config);
  const provider = config.provider?.[providerID];
  if (!plain(provider) || typeof provider.api !== 'string' || typeof provider.npm !== 'string') throw new Error('missing provider protocol declaration');
  const methods = [...allowedMethods].map(index => {
    const method = loaded.auth.methods[index];
    if (method?.type !== 'oauth' || typeof method.authorize !== 'function' || method.prompts?.length) {
      throw new Error('unverified authorization prompts or method');
    }
    return { index, label: String(method.label ?? providerID).slice(0, 160) };
  });
  hooks = loaded;
  descriptor = { id: providerID, api: provider.api, npm: provider.npm, methods, refresh: typeof hooks.auth.refresh === 'function' };
  return descriptor;
}
async function operation(message) {
  const input = message.input ?? {};
  if (message.method === 'init') return initialize(input);
  if (!hooks) throw new Error('plugin worker is not initialized');
  switch (message.method) {
    case 'validate': {
      jobAccess();
      if (typeof contract?.validate !== 'function') throw new Error('verified request contract is missing');
      contract.validate(input);
      return { valid: true };
    }
    case 'authorize': {
      jobAccess();
      if (!allowedMethods.has(input.method)) throw new Error('authorization method is not verified');
      const result = await hooks.auth.methods[input.method].authorize();
      if (!result || typeof result.callback !== 'function' || !['auto', 'code'].includes(result.method)) {
        throw new Error('unsupported authorization callback');
      }
      const url = new URL(result.url);
      if (url.protocol !== 'https:' || url.username || url.password) throw new Error('invalid authorization URL');
      const handle = `flow-${++sequence}`;
      callbacks.set(handle, result);
      if (result.user_code != null && !/^[A-Za-z0-9-]{1,64}$/.test(result.user_code)) throw new Error('invalid device verification code');
      return { flow: handle, url: url.href, method: result.method, user_code: result.user_code ?? '', instructions: String(result.instructions ?? '').slice(0, 2048) };
    }
    case 'complete': {
      jobAccess();
      const flow = callbacks.get(input.flow);
      if (!flow) throw new Error('unknown authorization flow');
      callbacks.delete(input.flow);
      const result = await flow.callback(input.code);
      jobAccess(); // New consent revoked in flight must not become a saved account.
      if (result?.type !== 'success') throw new Error('provider did not complete authorization');
      return result; // Private reply: the parent validates and stores consent.
    }
    case 'refresh': {
      jobAccess();
      if (!auth || typeof hooks.auth.refresh !== 'function') throw new Error('refresh is unavailable');
      const delta = await hooks.auth.refresh(copy(auth));
      if (delta !== undefined) {
        if (!plain(delta)) throw new Error('invalid refresh result');
        await saveState({ ...auth, ...delta }, 'merge');
      }
      return { changed: delta !== undefined };
    }
    case 'models': {
      jobAccess();
      if (!auth) throw new Error('no authorized account');
      // Do not let a failed live query quietly return the package's snapshot.
      const result = await hooks.provider.models({ id: providerID, models: {} }, { auth: copy(auth) });
      jobAccess();
      if (!plain(result) || Object.keys(result).length === 0) throw new Error('provider returned no live models');
      return result;
    }
    case 'fetch': {
      const job = jobAccess();
      if (!auth) throw new Error('no authorized account');
      if (typeof input.body !== 'string' || typeof input.url !== 'string') throw new Error('invalid inference input');
      const loader = await hooks.auth.loader(async () => copy(auth));
      if (typeof loader?.fetch !== 'function') throw new Error('provider does not supply its request adapter');
      jobAccess();
      const response = await loader.fetch(input.url, {
        method: 'POST', headers: { 'Content-Type': 'application/json', ...(input.headers ?? {}) },
        body: Buffer.from(input.body, 'base64'), signal: job.controller.signal,
      });
      if (!(response instanceof Response)) throw new Error('provider returned no HTTP response');
      await ask('head', { status: response.status, content_type: response.headers.get('content-type') ?? '', retry_after: response.headers.get('retry-after') ?? '', sign_in: response.headers.get('x-magpie-sign-in') ?? '' });
      const reader = response.body?.getReader();
      try {
        if (reader) for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          for (let i = 0; i < value.byteLength; i += 65536) {
            jobAccess();
            // Parent ACK only after consuming a chunk provides backpressure.
            await ask('chunk', { data: Buffer.from(value.subarray(i, i + 65536)).toString('base64') });
          }
        }
      } finally {
        await reader?.cancel().catch(() => {});
      }
      return { end: true }; // Transport end; no synthesized model terminal.
    }
    default: throw new Error('unsupported plugin operation');
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
  callbacks.clear();
  // A live rotation cannot be committed after the sole parent has gone.
  // The parent records this as unknown outcome, never auto-replays inference.
  process.exit();
});
