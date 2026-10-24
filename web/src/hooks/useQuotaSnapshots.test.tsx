// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { useQuotaSnapshots } from './useQuotaSnapshots';
import type { QuotaSnapshot, QuotaTarget } from '../types/quota';
const target = { channel_id: 7, credential_id: 'synthetic / primary' };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
function snapshot(patch: Partial<QuotaSnapshot> = {}): QuotaSnapshot {
  return { ...target, channel_name: '示例渠道', credential_name: '主凭证', kind: 'balance', enabled: true, balances: [{ currency: 'CNY', total: '12.00' }], windows: [], stale: false, checked_at: new Date().toISOString(), next_refresh_at: new Date(Date.now() + 60_000).toISOString(), ...patch };
}
function View({ targets = [target], active = true, label = 'quota' }: { targets?: QuotaTarget[]; active?: boolean; label?: string }) {
  const quotas = useQuotaSnapshots(targets, active);
  return <section aria-label={label}><output>{quotas.items[0]?.balances[0]?.total || 'unknown'}</output><p>{quotas.error}</p><p>{quotas.items[0]?.error ? 'snapshot failed' : ''}</p><button onClick={() => quotas.refresh(target)}>refresh {label}</button></section>;
}
beforeEach(() => { vi.spyOn(document, 'hidden', 'get').mockReturnValue(false); Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' }); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });
it('reads cached data without querying vendor before it is due and remount reuses durable cache', async () => {
  const fetcher = vi.fn((_url: string, _options?: RequestInit) => Promise.resolve(json({ items: [snapshot()], refresh_seconds: 60 })));
  vi.stubGlobal('fetch', fetcher);
  const first = render(<View />); await screen.findByText('12.00'); first.unmount();
  render(<View />); await screen.findByText('12.00');
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(fetcher.mock.calls.every(([, options]) => !options?.method)).toBe(true);
});
it('shows stale cache while exact concurrent consumers join one vendor refresh', async () => {
  let finish!: (value: Response) => void;
  const cached = snapshot({ stale: true, next_refresh_at: new Date(0).toISOString() });
  const fetcher = vi.fn((url: string) => url.endsWith('/refresh') ? new Promise<Response>(done => { finish = done; }) : Promise.resolve(json({ items: [cached], refresh_seconds: 60 })));
  vi.stubGlobal('fetch', fetcher); render(<><View /><View label="second" /></>);
  await waitFor(() => expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(1));
  expect(screen.getAllByText('12.00')).toHaveLength(2);
  const write = fetcher.mock.calls.find(([url]) => url.endsWith('/refresh'))! as unknown as [string, RequestInit];
  expect(JSON.parse(String(write[1].body))).toEqual({ ...target, force: false });
  await act(async () => finish(json(snapshot({ balances: [{ currency: 'CNY', total: '11.00' }] }))));
  expect(screen.getAllByText('11.00')).toHaveLength(2);
});
it('only refreshes selected enabled targets and stops when hidden or inactive', async () => {
  vi.useFakeTimers(); let hidden = false;
  vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
  const items = [snapshot({ next_refresh_at: new Date(0).toISOString() }), snapshot({ credential_id: 'unselected', next_refresh_at: new Date(0).toISOString() }), snapshot({ credential_id: 'disabled', enabled: false })];
  const fetcher = vi.fn((url: string) => Promise.resolve(json(url.endsWith('/refresh') ? snapshot() : { items, refresh_seconds: 60 })));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<View targets={[target, { ...target, credential_id: 'disabled' }]} />);
  await act(async () => {});
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(1);
  hidden = true; fireEvent(document, new Event('visibilitychange'));
  await act(async () => vi.advanceTimersByTimeAsync(120_000));
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(1);
  view.rerender(<View active={false} />); hidden = false; fireEvent(document, new Event('visibilitychange'));
  await act(async () => vi.advanceTimersByTimeAsync(120_000));
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(1);
});
it('manual-only mode never auto refreshes and retains old value with a visible transport failure', async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn((url: string) => Promise.resolve(url.endsWith('/refresh') ? json({ error: 'synthetic refresh failed' }, 503) : json({ items: [snapshot({ stale: true })], refresh_seconds: 0 })));
  vi.stubGlobal('fetch', fetcher); render(<View />); await act(async () => {});
  await act(async () => vi.advanceTimersByTimeAsync(120_000));
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(0);
  fireEvent.click(screen.getByRole('button')); await act(async () => {});
  expect(screen.getByText('12.00')).toBeTruthy(); expect(screen.getByText('synthetic refresh failed')).toBeTruthy();
  await act(async () => vi.advanceTimersByTimeAsync(15_000));
  expect(screen.getByText('snapshot failed')).toBeTruthy();
  const write = fetcher.mock.calls.find(([url]) => url.endsWith('/refresh'))! as unknown as [string, RequestInit];
  expect(JSON.parse(String(write[1].body))).toEqual({ ...target, force: true });
});
it('does not apply a late refresh after target selection changes', async () => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn((url: string) => url.endsWith('/refresh') ? new Promise<Response>(done => { finish = done; }) : Promise.resolve(json({ items: [snapshot()], refresh_seconds: 0 })));
  vi.stubGlobal('fetch', fetcher); const view = render(<View />); await screen.findByText('12.00');
  fireEvent.click(screen.getByRole('button')); await waitFor(() => expect(finish).toBeTypeOf('function'));
  view.rerender(<View targets={[]} />); await act(async () => {});
  await act(async () => finish(json(snapshot({ balances: [{ currency: 'CNY', total: '99.00' }] }))));
  expect(screen.queryByText('99.00')).toBeNull();
});
it('backs off an error snapshot even when backend could not supply timestamps', async () => {
  vi.useFakeTimers();
  const failed = snapshot({ error: 'synthetic canceled observation', checked_at: undefined, next_refresh_at: undefined, stale: true });
  const fetcher = vi.fn((url: string) => Promise.resolve(json(url.endsWith('/refresh') ? failed : { items: [failed], refresh_seconds: 60 })));
  vi.stubGlobal('fetch', fetcher); render(<View />); await act(async () => {});
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(1);
  await act(async () => vi.advanceTimersByTimeAsync(59_000));
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(1);
  await act(async () => vi.advanceTimersByTimeAsync(1000));
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(2);
});
it('caps vendor concurrency across consumers and discards queued work after hiding', async () => {
  let hidden = false; vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
  const items = Array.from({ length: 8 }, (_, i) => snapshot({ credential_id: `fixture-${i}`, next_refresh_at: new Date(0).toISOString() }));
  const complete: (() => void)[] = [];
  const fetcher = vi.fn((url: string, options: RequestInit = {}) => url.endsWith('/refresh') ? new Promise<Response>(done => {
    const target = JSON.parse(String(options.body));
    complete.push(() => done(json(snapshot({ credential_id: target.credential_id }))));
  }) : Promise.resolve(json({ items, refresh_seconds: 60 })));
  vi.stubGlobal('fetch', fetcher); render(<><View targets={items.slice(0, 4)} /><View label="second" targets={items.slice(4)} /></>);
  await waitFor(() => expect(complete).toHaveLength(3));
  hidden = true; fireEvent(document, new Event('visibilitychange'));
  await act(async () => { for (const finish of complete) finish(); });
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/refresh'))).toHaveLength(3);
});
it('does not rewind a completed manual refresh when an older cache read arrives late', async () => {
  let finish!: (value: Response) => void; let reads = 0;
  const cached = { items: [snapshot()], refresh_seconds: 0 };
  const fetcher = vi.fn((url: string) => url.endsWith('/refresh') ? Promise.resolve(json(snapshot({ balances: [{ currency: 'CNY', total: '11.00' }] }))) : ++reads === 1 ? Promise.resolve(json(cached)) : new Promise<Response>(done => { finish = done; }));
  vi.stubGlobal('fetch', fetcher); render(<View />); await screen.findByText('12.00');
  fireEvent(window, new Event('quota-settings-changed')); await waitFor(() => expect(finish).toBeTypeOf('function'));
  fireEvent.click(screen.getByRole('button')); await screen.findByText('11.00');
  await act(async () => finish(json(cached)));
  expect(screen.getByText('11.00')).toBeTruthy(); expect(screen.queryByText('12.00')).toBeNull();
});
