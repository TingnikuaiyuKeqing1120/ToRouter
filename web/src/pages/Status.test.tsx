// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Status from './Status';
import { RuntimeStatusProvider } from '../hooks/useRuntimeStatus';
import { RuntimeStatusIndicator } from '../components/RuntimeStatusIndicator';
import { ConfirmProvider } from '../components/Confirm';
import type { RuntimeCredentialStatus, RuntimeStatusSnapshot } from '../api';

const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const row: RuntimeCredentialStatus = { channel_id: 1, channel_name: 'Zulu', credential_id: 'key-1', credential_name: '工作凭据', red: true, reason: '连续不可用', failures: 4, quota_supported: true, models: [
  { model: 'a', name: '模型 A', upstream: 'https://upstream.test/v1', red: false, reason: '', failures: 0 },
  { model: 'z', name: '模型 Z', upstream: 'https://upstream.test/v1', red: true, reason: '连续不可用', failures: 4, last_request: { at: '2026-10-16T08:00:00Z', status: 503, duration_ms: 45, error: '繁忙' } },
  { model: 'b', name: '模型 B', upstream: 'https://upstream.test/v1', red: true, reason: '冷却中', failures: 2 },
] };
const snapshot: RuntimeStatusSnapshot = { rows: [{ ...row, channel_id: 2, channel_name: 'Alpha', credential_name: '备用', red: false, failures: 0, quota_supported: false, models: [] }, row], problem_count: 1 };
function mount() { return render(<ConfirmProvider><RuntimeStatusProvider><RuntimeStatusIndicator /><Status /></RuntimeStatusProvider></ConfirmProvider>); }
beforeEach(() => {
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(response(snapshot))));
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('runtime status', () => {
  it('shares one load, sorts red credentials and models first, and keeps both tables at four columns', async () => {
    mount();
    const table = await screen.findByRole('table', { name: '渠道凭据运行状态' });
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(within(table).getAllByRole('columnheader')).toHaveLength(4);
    expect(within(table).getAllByRole('row')[1].textContent).toContain('Zulu');
    expect(screen.getByLabelText('1 个异常凭据').textContent).toBe('1');
    fireEvent.click(screen.getByRole('button', { name: '展开 Zulu / 工作凭据 详情' }));
    const models = screen.getByRole('table', { name: 'Zulu / 工作凭据 模型状态' });
    expect(within(models).getAllByRole('columnheader')).toHaveLength(4);
    expect(within(models).getAllByRole('row').slice(1).map(item => item.querySelector('td')?.textContent)).toEqual(['模型 Bb', '模型 Zz', '模型 Aa']);
    expect(within(models).getByText('503 · 45 ms')).toBeTruthy();
    expect(within(models).getByText('繁忙')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '展开 Alpha / 备用 详情' }));
    expect(screen.getAllByRole('button', { name: '刷新额度' })).toHaveLength(1);
  });

  it('cancels safely and retains red state and expanded detail after a failed reset', async () => {
    const fetcher = vi.fn((url: string, _options?: RequestInit) => Promise.resolve(url === '/api/status' ? response(snapshot) : response({ error: '恢复失败' }, 503)));
    vi.stubGlobal('fetch', fetcher); mount();
    fireEvent.click(await screen.findByRole('button', { name: '展开 Zulu / 工作凭据 详情' }));
    fireEvent.click(screen.getByRole('button', { name: '恢复整个凭据' }));
    expect(await screen.findByText(/清除「Zulu \/ 工作凭据（整个凭据，含未显示模型）.*异常状态并允许重试，不改变启停/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '恢复整个凭据' })).toHaveProperty('disabled', false));
    expect(fetcher).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole('button', { name: '恢复模型 z' }));
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '清除并允许重试' }));
    await screen.findByText('恢复失败');
    expect(screen.getByLabelText('1 个异常凭据')).toBeTruthy();
    expect(screen.getByRole('table', { name: 'Zulu / 工作凭据 模型状态' })).toBeTruthy();
    expect(JSON.parse(String(fetcher.mock.calls[1][1]?.body))).toEqual({ channel_id: 1, credential_id: 'key-1', model: 'z' });
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it('refreshes only supported quota and reloads after a confirmed whole credential reset', async () => {
    let current = snapshot;
    const fetcher = vi.fn((url: string, _options?: RequestInit) => {
      if (url === '/api/status/reset') current = { rows: [{ ...row, red: false, failures: 0, models: [] }], problem_count: 0 };
      return Promise.resolve(response(url === '/api/status' ? current : { ok: true }));
    });
    vi.stubGlobal('fetch', fetcher); mount();
    fireEvent.click(await screen.findByRole('button', { name: '展开 Zulu / 工作凭据 详情' }));
    fireEvent.click(screen.getByRole('button', { name: '刷新额度' }));
    await waitFor(() => expect(fetcher.mock.calls.filter(([url]) => url === '/api/status')).toHaveLength(2));
    expect(JSON.parse(String(fetcher.mock.calls.find(([url]) => url === '/api/status/refresh-quota')![1]?.body))).toEqual({ channel_id: 1, credential_id: 'key-1' });
    fireEvent.click(screen.getByRole('button', { name: '恢复整个凭据' }));
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '清除并允许重试' }));
    await screen.findByLabelText('未发现异常');
    expect(JSON.parse(String(fetcher.mock.calls.find(([url]) => url === '/api/status/reset')![1]?.body))).toEqual({ channel_id: 1, credential_id: 'key-1' });
    expect(screen.queryByLabelText('1 个异常凭据')).toBeNull();
  });

  it('polls once per interval, pauses hidden, and rejects an old in-flight snapshot after manual refresh', async () => {
    mount(); await screen.findByLabelText('1 个异常凭据');
    vi.useFakeTimers();
    const hidden = vi.spyOn(document, 'hidden', 'get');
    await act(async () => { document.dispatchEvent(new Event('visibilitychange')); });
    const count = vi.mocked(fetch).mock.calls.length;
    await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
    expect(fetch).toHaveBeenCalledTimes(count + 1);
    hidden.mockReturnValue(true);
    await act(async () => document.dispatchEvent(new Event('visibilitychange')));
    await act(async () => { await vi.advanceTimersByTimeAsync(15000); });
    expect(fetch).toHaveBeenCalledTimes(count + 1);
    let finish!: (value: Response) => void;
    vi.mocked(fetch).mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
    hidden.mockReturnValue(false);
    await act(async () => document.dispatchEvent(new Event('visibilitychange')));
    const pendingOptions = vi.mocked(fetch).mock.calls[vi.mocked(fetch).mock.calls.length - 1][1];
    vi.mocked(fetch).mockResolvedValue(response({ rows: [], problem_count: 0 }));
    await act(async () => document.dispatchEvent(new Event('visibilitychange')));
    expect(pendingOptions?.signal?.aborted).toBe(true);
    await act(async () => finish(response(snapshot)));
    expect(screen.getByLabelText('未发现异常')).toBeTruthy();
    expect(screen.queryByLabelText('1 个异常凭据')).toBeNull();
  });
});
