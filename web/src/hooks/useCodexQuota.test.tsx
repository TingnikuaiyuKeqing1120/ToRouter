// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { quotaTargetKey, useCodexQuota, type QuotaTarget } from './useCodexQuota';

const response = (body: unknown) => new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
const snapshot = { ok: true, captured_at: '2026-10-05T00:00:00Z', buckets: [{ id: 'rate_limit', general: true, primary: { used_percent: 17 } }], cached: true, refresh_due: false, refresh_seconds: 300, next_refresh_at: new Date(Date.now() + 300_000).toISOString() };
const target: QuotaTarget = { channelID: 7, credentialID: 'personal / one', identity: 'account-a' };
function Harness({ targets = [target], active = true, networkEnabled = true }: { targets?: QuotaTarget[]; active?: boolean; networkEnabled?: boolean }) {
  const { observations, refreshQuota } = useCodexQuota(targets, active, networkEnabled);
  return <>{targets.map((item, index) => {
    const value = observations[quotaTargetKey(item)];
    return <section key={quotaTargetKey(item)} aria-label={`target ${index}`}><span data-testid={index ? `quota-${index}` : 'quota'}>{value?.snapshot?.buckets?.[0]?.primary?.used_percent ?? 'unknown'}</span><span>{value?.snapshot?.captured_at}</span><span data-testid={`error-${index}`}>{value?.error}</span><button disabled={value?.pending} onClick={() => refreshQuota(item)}>refresh {index}</button></section>;
  })}</>;
}
beforeEach(() => { Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' }); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it('reads saved snapshots but never refreshes the upstream when the plugin disallows networking', async () => {
  const fetcher = vi.fn((_url: string, _options?: RequestInit) => Promise.resolve(response({ ...snapshot, refresh_due: true })));
  vi.stubGlobal('fetch', fetcher);
  render(<Harness networkEnabled={false} />);
  await waitFor(() => expect(screen.getByTestId('quota').textContent).toBe('17'));
  fireEvent.click(screen.getByRole('button'));
  await act(async () => document.dispatchEvent(new Event('visibilitychange')));
  expect(fetcher.mock.calls.every(([, options]) => !options?.method)).toBe(true);
});

it('remount reads the durable cache without another upstream refresh', async () => {
  const fetcher = vi.fn().mockImplementation(() => Promise.resolve(response(snapshot)));
  vi.stubGlobal('fetch', fetcher);
  const first = render(<Harness />);
  await waitFor(() => expect(screen.getByTestId('quota').textContent).toBe('17'));
  first.unmount(); render(<Harness />);
  await waitFor(() => expect(screen.getByTestId('quota').textContent).toBe('17'));
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(fetcher.mock.calls.every(([, options]) => !options.method)).toBe(true);
  const query = new URL(String(fetcher.mock.calls[0][0]), 'http://localhost').searchParams;
  expect(query.get('channel_id')).toBe('7'); expect(query.get('credential_id')).toBe(target.credentialID);
});

it('paints stale data before refresh completes and retains timestamp on failure', async () => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockImplementation((_url, options) => options.method === 'POST' ? new Promise(resolve => { finish = resolve; }) : Promise.resolve(response({ ...snapshot, stale: true, refresh_due: true })));
  vi.stubGlobal('fetch', fetcher);
  render(<Harness />);
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(screen.getByTestId('quota').textContent).toBe('17');
  fireEvent.click(screen.getByRole('button')); expect(fetcher).toHaveBeenCalledTimes(2);
  await act(async () => finish(response({ ok: false, detail: 'rate limited' })));
  expect(screen.getByText('rate limited')).toBeTruthy();
  expect(screen.getByTestId('quota').textContent).toBe('17');
  expect(screen.getByText(snapshot.captured_at)).toBeTruthy();
});

it('manual-only mode does not schedule but manual refresh uses forced POST', async () => {
  const fetcher = vi.fn().mockImplementation(() => Promise.resolve(response({ ...snapshot, refresh_seconds: 0, next_refresh_at: undefined })));
  vi.stubGlobal('fetch', fetcher); render(<Harness />);
  await waitFor(() => expect(screen.getByTestId('quota').textContent).toBe('17'));
  fireEvent.click(screen.getByRole('button'));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(fetcher.mock.calls[1][0]).toContain('&force=1'); expect(fetcher.mock.calls[1][1].method).toBe('POST');
});

it('treats a never-fetched manual-only cache as empty and allows its first manual read', async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn().mockImplementation((_url, init) => Promise.resolve(response(init.method === 'POST'
    ? { ...snapshot, refresh_seconds: 0, next_refresh_at: undefined }
    : { ok: false, cached: true, refresh_due: false, refresh_seconds: 0 })));
  vi.stubGlobal('fetch', fetcher);
  await act(async () => { render(<Harness />); });
  expect(screen.getByTestId('quota').textContent).toBe('unknown');
  expect(screen.getByTestId('error-0').textContent).toBe('');
  expect(screen.getByRole('button')).toHaveProperty('disabled', false);
  await act(async () => vi.advanceTimersByTimeAsync(120_000));
  expect(fetcher).toHaveBeenCalledTimes(1);
  await act(async () => fireEvent.click(screen.getByRole('button')));
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(fetcher.mock.calls[1][1].method).toBe('POST');
  expect(screen.getByTestId('quota').textContent).toBe('17');
});

it('suspends hidden-page refresh and catches up when visible', async () => {
  vi.useFakeTimers();
  let calls = 0;
  const fetcher = vi.fn().mockImplementation((_url, options) => {
    if (options.method === 'POST') calls++;
    return Promise.resolve(response({ ...snapshot, refresh_due: options.method !== 'POST' && Date.now() >= due, next_refresh_at: new Date(Date.now() + 30_000).toISOString(), refresh_seconds: 30 }));
  });
  const due = Date.now() + 30_000;
  vi.stubGlobal('fetch', fetcher);
  await act(async () => { render(<Harness />); });
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
  act(() => document.dispatchEvent(new Event('visibilitychange')));
  await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
  expect(fetcher).toHaveBeenCalledTimes(1); expect(calls).toBe(0);
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
  await act(async () => document.dispatchEvent(new Event('visibilitychange')));
  expect(calls).toBe(1);
  expect(screen.getByTestId('quota').textContent).toBe('17');
});

it('a visible page refreshes at the persisted deadline and settings event rechecks cache', async () => {
  vi.useFakeTimers();
  const due = Date.now() + 30_000;
  const fetcher = vi.fn().mockImplementation((_url, options) => Promise.resolve(response({ ...snapshot, refresh_due: options.method !== 'POST' && Date.now() >= due, next_refresh_at: new Date(Date.now() + 30_000).toISOString(), refresh_seconds: 30 })));
  vi.stubGlobal('fetch', fetcher);
  await act(async () => { render(<Harness />); });
  await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
  expect(fetcher.mock.calls.filter(([, opts]) => opts.method === 'POST')).toHaveLength(1);
  await act(async () => window.dispatchEvent(new Event('quota-settings-changed')));
  expect(fetcher.mock.calls.length).toBeGreaterThan(3);
});

it('observes each credential and channel pair independently even when credential IDs match', async () => {
  const targets = [target, { ...target, credentialID: 'second' }, { ...target, channelID: 8 }];
  const fetcher = vi.fn().mockImplementation((url: string) => {
    const query = new URL(url, 'http://localhost').searchParams;
    const used = query.get('channel_id') === '8' ? 83 : query.get('credential_id') === 'second' ? 42 : 17;
    return Promise.resolve(response({ ...snapshot, buckets: [{ id: 'rate_limit', general: true, primary: { used_percent: used } }] }));
  });
  vi.stubGlobal('fetch', fetcher); render(<Harness targets={targets} />);
  await waitFor(() => expect(screen.getByTestId('quota-2').textContent).toBe('83'));
  expect(screen.getByTestId('quota').textContent).toBe('17');
  expect(screen.getByTestId('quota-1').textContent).toBe('42');
  fireEvent.click(screen.getByRole('button', { name: 'refresh 1' }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
  const [url, init] = fetcher.mock.calls[3];
  expect(new URL(url, 'http://localhost').searchParams.get('credential_id')).toBe('second');
  expect(init.method).toBe('POST');
  expect(screen.getByTestId('quota').textContent).toBe('17');
});

it('schedules each credential at its own persisted deadline', async () => {
  vi.useFakeTimers();
  const start = Date.now();
  const second = { ...target, credentialID: 'second' };
  const fetcher = vi.fn().mockImplementation((url: string) => {
    const credential = new URL(url, 'http://localhost').searchParams.get('credential_id');
    return Promise.resolve(response({ ...snapshot, refresh_seconds: 30, next_refresh_at: new Date(Date.now() === start ? start + (credential === 'second' ? 60_000 : 30_000) : Date.now() + 300_000).toISOString() }));
  });
  vi.stubGlobal('fetch', fetcher);
  await act(async () => { render(<Harness targets={[target, second]} />); });
  expect(fetcher).toHaveBeenCalledTimes(2);
  await act(async () => vi.advanceTimersByTimeAsync(30_000));
  expect(fetcher).toHaveBeenCalledTimes(3);
  expect(new URL(fetcher.mock.calls[2][0], 'http://localhost').searchParams.get('credential_id')).toBe(target.credentialID);
  await act(async () => vi.advanceTimersByTimeAsync(30_000));
  expect(fetcher).toHaveBeenCalledTimes(4);
  expect(new URL(fetcher.mock.calls[3][0], 'http://localhost').searchParams.get('credential_id')).toBe('second');
});

it('pauses on access-mode changes, ignores late completions, and rereads when restored', async () => {
  vi.useFakeTimers();
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
    .mockImplementation(() => Promise.resolve(response(snapshot)));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<Harness />);
  expect(fetcher).toHaveBeenCalledTimes(1);
  view.rerender(<Harness active={false} />);
  await act(async () => finish(response({ ...snapshot, refresh_due: true })));
  expect(screen.getByTestId('quota').textContent).toBe('unknown');
  fireEvent.click(screen.getByRole('button'));
  await act(async () => { await vi.advanceTimersByTimeAsync(600_000); window.dispatchEvent(new Event('quota-settings-changed')); });
  expect(fetcher).toHaveBeenCalledTimes(1);
  await act(async () => view.rerender(<Harness />));
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(screen.getByTestId('quota').textContent).toBe('17');
});

it('does not reuse the old identity snapshot or apply its delayed result after reauthorization', async () => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
    .mockImplementation(() => Promise.resolve(response({ ...snapshot, buckets: [] })));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<Harness />);
  view.rerender(<Harness targets={[{ ...target, identity: 'replacement-account' }]} />);
  await waitFor(() => expect(screen.getByText(snapshot.captured_at)).toBeTruthy());
  await act(async () => finish(response(snapshot)));
  expect(screen.getByTestId('quota').textContent).toBe('unknown');
  expect(fetcher).toHaveBeenCalledTimes(2);
});

it('retains independent snapshots and errors through transport failures and refresh requests', async () => {
  let count = 0;
  const fetcher = vi.fn().mockImplementation(() => ++count === 1 ? Promise.resolve(response(snapshot)) : Promise.reject(new Error('offline')));
  vi.stubGlobal('fetch', fetcher); render(<Harness />);
  await waitFor(() => expect(screen.getByTestId('quota').textContent).toBe('17'));
  fireEvent.click(screen.getByRole('button'));
  await screen.findByText('offline');
  expect(screen.getByTestId('quota').textContent).toBe('17');
  expect(screen.getByText(snapshot.captured_at)).toBeTruthy();
});

it('applies a refresh-period change after the pending request settles instead of restoring its old timer', async () => {
  vi.useFakeTimers();
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
    .mockImplementation(() => Promise.resolve(response({ ...snapshot, refresh_seconds: 0, next_refresh_at: undefined })));
  vi.stubGlobal('fetch', fetcher); render(<Harness />);
  act(() => window.dispatchEvent(new Event('quota-settings-changed')));
  expect(fetcher).toHaveBeenCalledTimes(1);
  await act(async () => finish(response({ ...snapshot, refresh_seconds: 30, next_refresh_at: new Date(Date.now() + 30_000).toISOString() })));
  expect(fetcher).toHaveBeenCalledTimes(2);
  await act(async () => vi.advanceTimersByTimeAsync(120_000));
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(fetcher.mock.calls.every(([, init]) => !init.method)).toBe(true);
});

it('discards the removed credential snapshot before the same identifier is added again', async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response(snapshot))
    .mockImplementation(() => Promise.resolve(response({ ok: false, detail: 'new identity has no quota' })));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<Harness />);
  await waitFor(() => expect(screen.getByTestId('quota').textContent).toBe('17'));
  view.rerender(<Harness targets={[]} />);
  view.rerender(<Harness />);
  await screen.findByText('new identity has no quota');
  expect(screen.getByTestId('quota').textContent).toBe('unknown');
  expect(fetcher).toHaveBeenCalledTimes(2);
});

it.each([0, 30])('uses failure refresh metadata to retry at the deadline with period %s', async period => {
  vi.useFakeTimers();
  const fetcher = vi.fn().mockImplementation((_url, init) => Promise.resolve(response(init.method === 'POST'
    ? { ok: false, detail: 'credential refresh timed out', refresh_seconds: period, next_refresh_at: period ? new Date(Date.now() + 30_000).toISOString() : undefined }
    : { ...snapshot, refresh_seconds: period, next_refresh_at: period ? new Date(Date.now() + 300_000).toISOString() : undefined })));
  vi.stubGlobal('fetch', fetcher);
  await act(async () => { render(<Harness />); });
  await act(async () => fireEvent.click(screen.getByRole('button')));
  expect(screen.getByTestId('error-0').textContent).toBe('credential refresh timed out');
  expect(screen.getByTestId('quota').textContent).toBe('17');
  expect(fetcher).toHaveBeenCalledTimes(2);
  await act(async () => vi.advanceTimersByTimeAsync(29_000));
  expect(fetcher).toHaveBeenCalledTimes(2);
  await act(async () => vi.advanceTimersByTimeAsync(1_000));
  expect(fetcher).toHaveBeenCalledTimes(period ? 3 : 2);
  if (period) expect(fetcher.mock.calls[2][1].method).toBeUndefined();
});
