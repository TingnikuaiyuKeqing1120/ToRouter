// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useResource } from './hooks';
import { clearStableResources, publishStableResource } from '../services/stableResources';

const response = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'Content-Type': 'application/json' } });
function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
beforeEach(clearStableResources);

describe('useResource successful snapshot identity', () => {
  it('keeps data and loadedPath together through pending, failure and successful retry', async () => {
    const first = deferred<Response>(), failed = deferred<Response>(), retry = deferred<Response>();
    const fetcher = vi.fn().mockReturnValueOnce(first.promise).mockReturnValueOnce(failed.promise).mockReturnValueOnce(retry.promise);
    vi.stubGlobal('fetch', fetcher);
    const { result, rerender } = renderHook(({ path }) => useResource<{ count: number }>(path), { initialProps: { path: '/usage?range=first' } });
    expect(result.current.loadedPath).toBe('');
    expect(result.current.data).toBeNull();
    await act(async () => first.resolve(response({ count: 4 })));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.loadedPath).toBe('/usage?range=first');
    expect(result.current.data).toEqual({ count: 4 });
    rerender({ path: '/usage?range=next' });
    expect(result.current.loading).toBe(true);
    expect(result.current.loadedPath).toBe('/usage?range=first');
    expect(result.current.data).toEqual({ count: 4 });
    await act(async () => failed.reject(new Error('network unavailable')));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBe('network unavailable');
    expect(result.current.loadedPath).toBe('/usage?range=first');
    expect(result.current.data).toEqual({ count: 4 });
    act(() => result.current.reload());
    expect(result.current.loading).toBe(true);
    expect(result.current.error).toBe('');
    expect(result.current.loadedPath).toBe('/usage?range=first');
    await act(async () => retry.resolve(response({ count: 9 })));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.loadedPath).toBe('/usage?range=next');
    expect(result.current.data).toEqual({ count: 9 });
    expect(fetcher.mock.calls.map(call => call[0])).toEqual(['/api/usage?range=first', '/api/usage?range=next', '/api/usage?range=next']);
  });

  it('ignores an older success arriving after a newer successful request, even when transport ignores abort', async () => {
    const old = deferred<Response>(), current = deferred<Response>();
    const fetcher = vi.fn().mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise);
    vi.stubGlobal('fetch', fetcher);
    const { result, rerender } = renderHook(({ path }) => useResource<string>(path), { initialProps: { path: '/old' } });
    rerender({ path: '/current' });
    expect((fetcher.mock.calls[0][1] as RequestInit).signal?.aborted).toBe(true);
    await act(async () => current.resolve(response('current data')));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.loadedPath).toBe('/current');
    expect(result.current.data).toBe('current data');
    await act(async () => old.resolve(response('outdated data')));
    expect(result.current.loadedPath).toBe('/current');
    expect(result.current.data).toBe('current data');
    expect(result.current.error).toBe('');
  });

  it('ignores an obsolete failure without clearing the active loading state', async () => {
    const old = deferred<Response>(), current = deferred<Response>();
    vi.stubGlobal('fetch', vi.fn().mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise));
    const { result, rerender } = renderHook(({ path }) => useResource<string>(path), { initialProps: { path: '/old' } });
    rerender({ path: '/current' });
    await act(async () => old.reject(new Error('obsolete failure')));
    expect(result.current.error).toBe('');
    expect(result.current.loading).toBe(true);
    expect(result.current.loadedPath).toBe('');
    await act(async () => current.resolve(response('current data')));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.data).toBe('current data');
    expect(result.current.loadedPath).toBe('/current');
  });
});

it('restores FX immediately, silently compares identical data, and keeps manual reload visible', async () => {
  const initial = { available: true, rate: 7 };
  publishStableResource('/exchange-rate', initial);
  const silent = deferred<Response>(), manual = deferred<Response>();
  vi.stubGlobal('fetch', vi.fn().mockReturnValueOnce(silent.promise).mockReturnValueOnce(manual.promise));
  const { result } = renderHook(() => useResource<typeof initial>('/exchange-rate'));
  expect(result.current.data).toBe(initial); expect(result.current.loading).toBe(false);
  await act(async () => silent.resolve(response(initial)));
  expect(result.current.data).toBe(initial); expect(result.current.loading).toBe(false);
  act(() => result.current.reload()); expect(result.current.loading).toBe(true);
  await act(async () => manual.reject(new Error('offline')));
  expect(result.current.data).toBe(initial); expect(result.current.error).toBe('offline'); expect(result.current.loading).toBe(false);
});

it('shares FX in-flight reads across consumers without caching ordinary authorized resources', async () => {
  const pending = deferred<Response>();
  const fetcher = vi.fn().mockReturnValueOnce(pending.promise).mockResolvedValue(response([{ id: 'channel' }]));
  vi.stubGlobal('fetch', fetcher);
  const a = renderHook(() => useResource('/exchange-rate')), b = renderHook(() => useResource('/exchange-rate'));
  expect(fetcher).toHaveBeenCalledTimes(1);
  a.unmount(); await act(async () => pending.resolve(response({ available: true, rate: 7 })));
  expect(b.result.current.data).toEqual({ available: true, rate: 7 });
  const ordinary = renderHook(() => useResource('/channels')); await waitFor(() => expect(ordinary.result.current.loading).toBe(false)); ordinary.unmount();
  const fresh = renderHook(() => useResource('/channels')); expect(fresh.result.current.data).toBeNull();
});

it('ignores a delayed shared-resource success after the hook switches paths', async () => {
  const old = deferred<Response>(), current = deferred<Response>();
  vi.stubGlobal('fetch', vi.fn().mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise));
  const { result, rerender } = renderHook(({ path }) => useResource(path), { initialProps: { path: '/exchange-rate' } });
  rerender({ path: '/channels' });
  await act(async () => current.resolve(response(['current channel'])));
  await act(async () => old.resolve(response({ available: true, rate: 7 })));
  expect(result.current.data).toEqual(['current channel']); expect(result.current.loadedPath).toBe('/channels');
});
