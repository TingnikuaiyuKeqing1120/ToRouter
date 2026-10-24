// @vitest-environment jsdom
import { beforeEach, afterEach, expect, it, vi } from 'vitest';
import { clearStableResources, isStableResourcePath, publishStableResource, readStableResource, stableResourceData } from './stableResources';

const json = (value: unknown) => new Response(JSON.stringify(value));
const path = '/catalog/models?source=all';
beforeEach(clearStableResources);
afterEach(() => vi.unstubAllGlobals());

it('only recognizes the three explicitly shared metadata paths', () => {
  for (const value of [path, '/catalog/sources', '/exchange-rate']) expect(isStableResourcePath(value)).toBe(true);
  for (const value of ['/client-keys', '/channels', '/usage', '/catalog/models?source=builtin']) expect(isStableResourcePath(value)).toBe(false);
});

it('deduplicates pending reads and keeps references for unchanged nested data', async () => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn(() => new Promise<Response>(resolve => { finish = resolve; }));
  vi.stubGlobal('fetch', fetcher);
  const first = readStableResource(path), second = readStableResource(path);
  expect(first).toBe(second); expect(fetcher).toHaveBeenCalledTimes(1);
  finish(json([{ id: 'one', offerings: [{ key: 'regional' }] }]));
  const rows = await first;
  fetcher.mockImplementation(() => Promise.resolve(json(rows)));
  expect(await readStableResource(path)).toBe(rows);
  fetcher.mockImplementation(() => Promise.resolve(json([{ id: 'two' }])));
  expect(await readStableResource(path)).not.toBe(rows);
});

it('does not let a read older than a server-confirmed write replace the cache', async () => {
  let finish!: (value: Response) => void;
  vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(resolve => { finish = resolve; })));
  const pending = readStableResource('/catalog/sources');
  const saved = publishStableResource('/catalog/sources', [{ priority: 1, source: 'new' }]);
  finish(json([{ priority: 9, source: 'old' }]));
  expect(await pending).toBe(saved);
  expect(stableResourceData('/catalog/sources')).toBe(saved);
});

it('preserves last valid FX honestly as stale when the server reports unavailable', async () => {
  const previous = { available: true, base: 'USD', quote: 'CNY', rate: 7, date: '2026-10-17', source_url: 'original', fetched_at: 'old' };
  publishStableResource('/exchange-rate', previous);
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ available: false, rate: 0, date: '2026-10-18', source_url: 'failed', attempted_on: '2026-10-18', error: 'source offline' }))));
  expect(await readStableResource('/exchange-rate')).toEqual({ ...previous, stale: true, attempted_on: '2026-10-18', error: 'source offline' });
});

it('clearing a session prevents an old in-flight read from repopulating it', async () => {
  let finish!: (value: Response) => void;
  vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(resolve => { finish = resolve; })));
  const pending = readStableResource(path);
  clearStableResources(); finish(json([{ id: 'old session' }])); await pending;
  expect(stableResourceData(path)).toBeUndefined();
});

it('cancels every old-session transport so delayed 401s cannot expire the new session', async () => {
  const pending: { finish: (value: Response) => void; signal: AbortSignal }[] = [];
  // Deliberately ignore transport cancellation: the real client must also
  // suppress auth-expired if a response still arrives after its signal aborts.
  vi.stubGlobal('fetch', vi.fn((_url: string, init: RequestInit) => new Promise<Response>(finish => pending.push({ finish, signal: init.signal! }))));
  const events = vi.spyOn(window, 'dispatchEvent');
  try {
    const old = readStableResource(path);
    const forced = readStableResource(path, true);
    const beforeWrite = readStableResource('/catalog/sources');
    publishStableResource('/catalog/sources', [{ source: 'saved', priority: 1 }]);
    const settled = Promise.allSettled([old, forced, beforeWrite]);
    clearStableResources();
    expect(pending.slice(0, 3).every(request => request.signal.aborted)).toBe(true);

    const current = readStableResource(path);
    expect(pending[3].signal.aborted).toBe(false);
    pending[3].finish(json([{ id: 'new session' }]));
    const currentRows = await current;
    for (const request of pending.slice(0, 3)) request.finish(new Response(JSON.stringify({ error: 'old session expired' }), { status: 401 }));
    const results = await settled;
    expect(results.every(result => result.status === 'rejected' && result.reason.status === 401)).toBe(true);
    expect(events).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'auth-expired' }));
    expect(stableResourceData(path)).toBe(currentRows);

    // Current-session failures retain the normal client authentication guard.
    const liveFailure = readStableResource('/exchange-rate');
    pending[4].finish(new Response('{}', { status: 401 }));
    await expect(liveFailure).rejects.toMatchObject({ status: 401 });
    expect(events).toHaveBeenCalledWith(expect.objectContaining({ type: 'auth-expired' }));
  } finally { events.mockRestore(); }
});
