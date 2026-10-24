import { api } from './client';
import type { QuotaList, QuotaSnapshot, QuotaTarget } from '../types/quota';

export const quotaKey = (target: QuotaTarget) => JSON.stringify([target.channel_id, target.credential_id]);
let reading: Promise<QuotaList> | undefined;
const refreshing = new Map<string, { promise: Promise<QuotaSnapshot | undefined>; consumers: (() => boolean)[] }>();
let activeRefreshes = 0;
const waiting: (() => void)[] = [];

async function acquireRefresh() {
  if (activeRefreshes >= 3) await new Promise<void>(resolve => waiting.push(resolve));
  else activeRefreshes++;
}
function releaseRefresh() {
  const next = waiting.shift();
  if (next) next();
  else activeRefreshes--;
}

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 25_000);
  try { return await api<T>(path, { ...options, signal: controller.signal }); }
  finally { clearTimeout(timeout); }
}
// Join concurrent consumers without keeping account data after a read settles.
export function readQuotas() {
  if (!reading) reading = request<QuotaList>('/quotas').finally(() => { reading = undefined; });
  return reading;
}
export function refreshQuota(target: QuotaTarget, force: boolean, allowed: () => boolean = () => true) {
  const key = quotaKey(target);
  const existing = refreshing.get(key);
  if (existing) { existing.consumers.push(allowed); return existing.promise; }
  const consumers = [allowed];
  const operation = (async () => {
    await acquireRefresh();
    try {
      if (!consumers.some(consumer => consumer())) return undefined;
      return await request<QuotaSnapshot>('/quotas/refresh', { method: 'POST', body: JSON.stringify({ channel_id: target.channel_id, credential_id: target.credential_id, force }) });
    } finally { releaseRefresh(); }
  })().finally(() => refreshing.delete(key));
  refreshing.set(key, { promise: operation, consumers });
  return operation;
}
