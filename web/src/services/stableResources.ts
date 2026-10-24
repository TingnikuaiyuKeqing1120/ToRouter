import { api } from './client';
import { validExchangeRate } from './formatters';
import type { CatalogSourceStatus, ExchangeRate } from '../types';

// Explicitly limited to shared catalog metadata and FX; never cache keys,
// channels, usage, or other authorized/user-specific collections here.
const paths = ['/catalog/models?source=all', '/catalog/sources', '/exchange-rate'] as const;
export type StableResourcePath = typeof paths[number];
type Snapshot = { data?: unknown; json?: string; revision: number; pending?: Promise<unknown> };
const snapshots = new Map<StableResourcePath, Snapshot>();
// A force read or confirmed write can supersede a request without finishing
// it. Track every live transport until settlement so session cleanup also
// cancels requests no longer referenced by a snapshot's pending promise.
const inFlight = new Set<AbortController>();
function snapshot(path: StableResourcePath) {
  let entry = snapshots.get(path);
  if (!entry) { entry = { revision: 0 }; snapshots.set(path, entry); }
  return entry;
}
export function isStableResourcePath(path: string): path is StableResourcePath {
  return (paths as readonly string[]).includes(path);
}
export function stableResourceData<T>(path: StableResourcePath): T | undefined {
  return snapshot(path).data as T | undefined;
}
function accept<T>(path: StableResourcePath, entry: Snapshot, value: T): T {
  if (path === '/catalog/sources') value = [...value as CatalogSourceStatus[]].sort((a, b) => a.priority - b.priority) as T;
  if (path === '/exchange-rate' && validExchangeRate(entry.data as ExchangeRate) && !validExchangeRate(value as ExchangeRate)) {
    const failed = value as ExchangeRate, previous = entry.data as ExchangeRate;
    value = { ...previous, stale: true, error: failed?.error || '汇率读取失败，保留上次结果', attempted_on: failed?.attempted_on || previous.attempted_on } as T;
  }
  const json = JSON.stringify(value);
  if (entry.data !== undefined && entry.json === json) return entry.data as T;
  entry.data = value; entry.json = json;
  return value;
}
export function publishStableResource<T>(path: StableResourcePath, value: T | undefined): T | undefined {
  const entry = snapshot(path); entry.revision++; entry.pending = undefined;
  if (value === undefined) { entry.data = undefined; entry.json = undefined; return undefined; }
  return accept(path, entry, value);
}
export function readStableResource<T>(path: StableResourcePath, force = false): Promise<T> {
  const entry = snapshot(path);
  if (entry.pending && !force) return entry.pending as Promise<T>;
  const revision = ++entry.revision;
  const abort = new AbortController(); inFlight.add(abort);
  const request = api<T>(path, { signal: abort.signal }).then(value => {
    if (path !== '/exchange-rate' && !Array.isArray(value)) throw new Error(path === '/catalog/sources' ? '来源配置响应无效' : '模型资料响应无效');
    if (entry.revision !== revision) return (entry.data ?? value) as T;
    return accept(path, entry, value);
  }).finally(() => { inFlight.delete(abort); if (entry.revision === revision) entry.pending = undefined; });
  entry.pending = request;
  return request;
}
export function clearStableResources() {
  // Invalidate promises too: an old session must not repopulate this cache.
  for (const entry of snapshots.values()) { entry.revision++; entry.pending = undefined; entry.data = undefined; entry.json = undefined; }
  for (const abort of inFlight) abort.abort();
  inFlight.clear();
}
