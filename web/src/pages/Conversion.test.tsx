// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import type { RequestDiagnostic, UsageLog } from '../types';
import Settings from './Settings';
import Logs from './Logs';

const json = (value: unknown) => new Response(JSON.stringify(value));
const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
const originalScroll = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollIntoView');
const settings = { gateway_key: 'synthetic-admin', retention_days: 7, max_attempts: 3, retries_per_channel: 2, failure_cooldown_seconds: 10 };
const diagnostic: RequestDiagnostic = {
  code: 'reasoning_rejected', source: 'upstream', phase: 'upstream_response', message: '上游拒绝了请求中的思考状态。',
  facts: ['上游明确拒绝了该附件。'], possible_causes: ['最近切换的目标可能不接受原附件。'],
  actions_taken: ['稳定优先，未剥离或重试。'], steps: [{ action: '核对本次与上次的目标。', expected: '切回后恢复可支持兼容性判断。' }],
  settings_path: '#/settings?section=conversion', request_id: 'synthetic-request-7',
};
const log: UsageLog = {
  id: 7, time: '2026-10-19T08:00:00Z', channel_id: 1, channel_name: 'Synthetic',
  model: 'example-model', upstream_model: 'example-upstream', inbound: 'openai_chat_completions', outbound: 'anthropic_messages',
  input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0,
  estimated: true, duration_ms: 50, ttft_ms: 0, status: 400, error: 'synthetic raw error', diagnostic,
  degraded: 'thinking', attempts: [{ attempt_index: 1, channel_id: 1, channel_name: 'Synthetic', upstream_model: 'example-upstream', status_code: 400, duration_ms: 50 }],
};
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  history.replaceState(null, '', '#/settings');
  Object.defineProperty(navigator, 'clipboard', { configurable: true, get: () => undefined });
});
afterEach(() => {
  cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); history.replaceState(null, '', '#/');
  if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
  else Reflect.deleteProperty(navigator, 'clipboard');
  if (originalScroll) Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', originalScroll);
  else Reflect.deleteProperty(HTMLElement.prototype, 'scrollIntoView');
});

async function openLog(value: UsageLog = log) {
  vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(json(url.startsWith('/api/logs') ? [value] : []))));
  render(<Logs />);
  fireEvent.click(await screen.findByRole('button', { name: '查看请求 7 详情' }));
  return within(screen.getByRole('dialog'));
}

describe('protocol conversion settings', () => {
  it('locates the conversion section after loading and saves the chosen policy through the unified form', async () => {
    history.replaceState(null, '', '#/settings?section=conversion');
    const scroll = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: scroll });
    const fetcher = vi.fn((url: string, options?: RequestInit) => Promise.resolve(json(options?.method === 'PUT' ? {} : url === '/api/settings' ? settings : { port: 2886 })));
    vi.stubGlobal('fetch', fetcher);
    render(<ConfirmProvider><Settings /></ConfirmProvider>);
    const stable = await screen.findByRole('button', { name: '稳定优先' });
    expect(stable.getAttribute('aria-pressed')).toBe('true');
    // Finding the form does not guarantee its passive location effect has run.
    await waitFor(() => {
      expect(scroll).toHaveBeenCalledWith({ block: 'start' });
      expect(document.activeElement).toBe(screen.getByRole('region', { name: '协议转换' }));
    });
    expect(fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: '可用优先' }));
    const unload = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(unload);
    expect(unload.defaultPrevented).toBe(true);
    fireEvent.click(screen.getAllByRole('button', { name: '保存设置' })[0]);
    await waitFor(() => expect(fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true));
    const body = JSON.parse(String(fetcher.mock.calls.find(([, init]) => init?.method === 'PUT')![1]!.body));
    expect(body.reasoning_policy).toBe('availability');
    expect(body.gateway_key).toBe(settings.gateway_key);
    await waitFor(() => expect(screen.getAllByRole('button', { name: '保存设置' })[0]).toHaveProperty('disabled', true));
    const saved = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(saved);
    expect(saved.defaultPrevented).toBe(false);
  });

  it('loads availability without changing or automatically saving it', async () => {
    const fetcher = vi.fn((url: string) => Promise.resolve(json(url === '/api/settings' ? { ...settings, reasoning_policy: 'availability' } : { port: 2886 })));
    vi.stubGlobal('fetch', fetcher);
    render(<ConfirmProvider><Settings /></ConfirmProvider>);
    expect((await screen.findByRole('button', { name: '可用优先' })).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getAllByRole('button', { name: '保存设置' })[0]).toHaveProperty('disabled', true);
  });
});

describe('request diagnosis', () => {
  it('separates evidence, possibilities, actions and steps while retaining folded errors, losses and attempts', async () => {
    const dialog = await openLog();
    const region = within(dialog.getByRole('region', { name: '请求诊断' }));
    for (const heading of ['已确认', '可能原因', '网关处理', '建议与验证']) expect(region.getByRole('heading', { name: heading })).toBeTruthy();
    expect(region.getByText('上游 · 上游响应')).toBeTruthy();
    expect(region.queryByText('upstream_response')).toBeNull();
    expect(region.getByText('结果判断：切回后恢复可支持兼容性判断。')).toBeTruthy();
    expect(region.getByRole('link', { name: '打开协议转换设置' }).getAttribute('href')).toBe('#/settings?section=conversion');
    expect(dialog.getByText('synthetic raw error').closest('details')?.open).toBe(false);
    expect(dialog.getByRole('region', { name: '转换损失原因' })).toBeTruthy();
    expect(dialog.getByRole('region', { name: '尝试与切换轨迹' })).toBeTruthy();
  });

  it('copies only diagnostic fields and necessary request metadata, with success feedback', async () => {
    const write = vi.fn().mockResolvedValue(undefined);
    vi.spyOn(navigator, 'clipboard', 'get').mockReturnValue({ writeText: write } as unknown as Clipboard);
    const extended = { ...log, request_body: 'synthetic-private-prompt', api_key: 'synthetic-secret', credential_id: 'synthetic-credential', diagnostic: { ...diagnostic, ciphertext: 'synthetic-ciphertext', steps: [{ action: '检查目标', expected: '根据结果判断', secret: 'synthetic-step-secret' }] } };
    const dialog = await openLog(extended);
    fireEvent.click(dialog.getByRole('button', { name: '复制排查信息' }));
    await dialog.findByRole('status');
    expect(dialog.getByText('排查信息已复制')).toBeTruthy();
    const copied = JSON.parse(write.mock.calls[0][0]);
    expect(copied.request).toEqual({ id: 7, request_id: diagnostic.request_id, time: log.time, status: 400, channel_id: 1, model: log.model, upstream_model: log.upstream_model, inbound: log.inbound, outbound: log.outbound });
    expect(copied.diagnostic).toEqual({ ...diagnostic, steps: [{ action: '检查目标', expected: '根据结果判断' }] });
    for (const secret of ['synthetic-private-prompt', 'synthetic-secret', 'synthetic-credential', 'synthetic-ciphertext', 'synthetic-step-secret', 'synthetic raw error']) expect(write.mock.calls[0][0]).not.toContain(secret);
  });

  it('reports clipboard rejection and permits retrying the same safe summary', async () => {
    const write = vi.fn().mockRejectedValueOnce(new Error('denied')).mockResolvedValueOnce(undefined);
    vi.spyOn(navigator, 'clipboard', 'get').mockReturnValue({ writeText: write } as unknown as Clipboard);
    const dialog = await openLog();
    fireEvent.click(dialog.getByRole('button', { name: '复制排查信息' }));
    await dialog.findByRole('alert');
    expect(dialog.getByText('复制失败，请允许剪贴板访问后重试。')).toBeTruthy();
    fireEvent.click(dialog.getByRole('button', { name: '复制排查信息' }));
    await dialog.findByText('排查信息已复制');
    expect(write).toHaveBeenCalledTimes(2);
    expect(dialog.queryByRole('alert')).toBeNull();
  });

  it('uses unknown labels and hides absent evidence blocks or unsafe settings URLs', async () => {
    const dialog = await openLog({ ...log, diagnostic: { code: 'unknown', source: 'unrecognized_source', phase: 'internal_phase', message: '', settings_path: 'https://example.invalid/?secret=hidden' } });
    const region = within(dialog.getByRole('region', { name: '请求诊断' }));
    expect(region.getByText('来源未明 · 阶段未明')).toBeTruthy();
    expect(region.getByText('原因尚不明确，请结合已记录的证据排查。')).toBeTruthy();
    for (const heading of ['已确认', '可能原因', '网关处理', '建议与验证']) expect(region.queryByRole('heading', { name: heading })).toBeNull();
    expect(region.queryByRole('link')).toBeNull();
    expect(region.queryByText('internal_phase')).toBeNull();
  });

  it('keeps logs without a diagnostic usable without fabricated evidence', async () => {
    const dialog = await openLog({ ...log, diagnostic: undefined });
    expect(dialog.queryByRole('region', { name: '请求诊断' })).toBeNull();
    expect(dialog.getByText('synthetic raw error')).toBeTruthy();
    expect(dialog.getByRole('region', { name: '尝试与切换轨迹' })).toBeTruthy();
  });

  it('queries the correlated request from a deep link and lets users clear it through the existing query flow', async () => {
    history.replaceState(null, '', '#/logs?request_id=synthetic%2Frequest%2B7');
    const fetcher = vi.fn((url: string) => Promise.resolve(json(url.startsWith('/api/logs') ? [{ ...log, request_id: 'synthetic/request+7' }] : [])));
    vi.stubGlobal('fetch', fetcher);
    render(<Logs />);
    fireEvent.click(await screen.findByRole('button', { name: '查看请求 7 详情' }));
    const requestCalls = () => fetcher.mock.calls.filter(([url]) => url.startsWith('/api/logs'));
    expect(new URLSearchParams(requestCalls()[0][0].split('?')[1]).get('request_id')).toBe('synthetic/request+7');
    expect(screen.getByLabelText('已应用查询范围').textContent).toContain('关联请求：synthetic/request+7');
    const link = within(screen.getByRole('dialog')).getByRole('link', { name: '查看同请求全部尝试' });
    expect(link.getAttribute('href')).toBe('#/logs?request_id=synthetic%2Frequest%2B7');
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '关闭' }));
    fireEvent.click(screen.getByRole('button', { name: '清除关联请求筛选' }));
    expect(screen.getByText('筛选已更改，等待查询。')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '查询日志' }));
    await waitFor(() => expect(requestCalls()).toHaveLength(2));
    expect(new URLSearchParams(requestCalls()[1][0].split('?')[1]).has('request_id')).toBe(false);
    expect(screen.queryByLabelText('关联请求筛选')).toBeNull();
  });
});
