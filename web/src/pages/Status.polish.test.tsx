// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import { RuntimeStatusProvider } from '../hooks/useRuntimeStatus';
import type { RuntimeCredentialStatus } from '../api';
import Status from './Status';

const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const row: RuntimeCredentialStatus = { channel_id: 1, channel_name: 'Main', credential_id: 'key-1', credential_name: '工作凭据', red: true, reason: '连续不可用', failures: 4, quota_supported: true, models: [
  { model: 'alpha', name: '模型 Alpha', upstream: 'Main', red: true, reason: '冷却中', failures: 4 },
  { model: 'beta', name: '模型 Beta', upstream: 'Main', red: true, reason: '冷却中', failures: 2 },
  { model: 'healthy', name: '模型 Healthy', upstream: 'Main', red: false, reason: '', failures: 0 },
] };
const snapshot = { rows: [row, { ...row, channel_id: 2, channel_name: 'Secondary', credential_name: '备用凭据', red: false, failures: 0, models: [], quota_supported: false }], problem_count: 1 };
let fetcher: ReturnType<typeof vi.fn>;
beforeEach(() => {
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  fetcher = vi.fn((url: string, _init?: RequestInit) => Promise.resolve(response(url === '/api/status' ? snapshot : { ok: true })));
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });
function mount() { return render(<ConfirmProvider><RuntimeStatusProvider><Status /></RuntimeStatusProvider></ConfirmProvider>); }
const search = (value: string) => fireEvent.change(screen.getByRole('textbox', { name: '搜索运行状态' }), { target: { value } });

it('filters channels and credentials locally and keeps result counts with only problems', async () => {
  mount(); await screen.findByRole('table', { name: '渠道凭据运行状态' });
  expect(screen.getByText('显示 2 / 2 个凭据 · 异常 1 个')).toBeTruthy();
  expect(screen.getByText(/每 5 秒自动刷新 · 更新于/)).toBeTruthy();
  search('备用凭据');
  expect(screen.getByText('显示 1 / 2 个凭据 · 异常 0 个')).toBeTruthy();
  expect(screen.queryByRole('button', { name: '展开 Main / 工作凭据 详情' })).toBeNull();
  search('Main'); fireEvent.click(screen.getByRole('checkbox', { name: '仅异常' }));
  expect(screen.getByText('显示 1 / 2 个凭据 · 异常 1 个')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '清除筛选' }));
  expect(screen.getByText('显示 2 / 2 个凭据 · 异常 1 个')).toBeTruthy();
  expect(fetcher).toHaveBeenCalledTimes(1);
});

it('shows model matches and preserves the whole credential reset scope through model filtering', async () => {
  mount(); await screen.findByRole('table', { name: '渠道凭据运行状态' });
  search('alpha');
  expect(screen.getByText('匹配模型：模型 Alpha (alpha)')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '展开 Main / 工作凭据 详情' }));
  const table = screen.getByRole('table', { name: 'Main / 工作凭据 模型状态' });
  expect(within(table).getByText('模型 Alpha')).toBeTruthy();
  expect(within(table).queryByText('模型 Beta')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '恢复整个凭据' }));
  const dialog = await screen.findByRole('dialog');
  expect(within(dialog).getByText(/整个凭据，含未显示模型/)).toBeTruthy();
  fireEvent.click(within(dialog).getByRole('button', { name: '清除并允许重试' }));
  await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url === '/api/status/reset')).toBe(true));
  expect(JSON.parse(String(fetcher.mock.calls.find(([url]) => url === '/api/status/reset')![1]?.body))).toEqual({ channel_id: 1, credential_id: 'key-1' });
});

it('hides reset actions without failures, anomalies or active cooldown, including expired cooldown', async () => {
  const clean = { ...row, red: false, reason: '', failures: 0, quota_supported: false, models: [{ ...row.models[2], cooldown_until: '2000-01-01T00:00:00Z' }] };
  fetcher.mockResolvedValue(response({ rows: [clean], problem_count: 0 }));
  mount(); fireEvent.click(await screen.findByRole('button', { name: '展开 Main / 工作凭据 详情' }));
  expect(screen.queryByRole('button', { name: '恢复整个凭据' })).toBeNull();
  expect(screen.queryByRole('button', { name: '恢复模型 healthy' })).toBeNull();
  expect(screen.queryByRole('button', { name: '刷新额度' })).toBeNull();
});

it.each([{ failures: 1 }, { cooldown_until: '2099-01-01T00:00:00Z' }])('offers reset for a green model with clearable state %j', async extra => {
  const model = { ...row.models[2], ...extra };
  fetcher.mockResolvedValue(response({ rows: [{ ...row, red: false, failures: 0, models: [model] }], problem_count: 0 }));
  mount(); fireEvent.click(await screen.findByRole('button', { name: '展开 Main / 工作凭据 详情' }));
  expect(screen.getByRole('button', { name: '恢复模型 healthy' })).toBeTruthy();
  expect(screen.getByRole('button', { name: '恢复整个凭据' })).toBeTruthy();
});

it('retains the expanded snapshot and successful update time after refresh failure and retries in place', async () => {
  mount(); fireEvent.click(await screen.findByRole('button', { name: '展开 Main / 工作凭据 详情' }));
  const timestamp = screen.getByText(/每 5 秒自动刷新 · 更新于/).textContent;
  let finish!: (value: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
  fireEvent.click(screen.getByRole('button', { name: '刷新状态' }));
  expect(screen.getByRole('table', { name: 'Main / 工作凭据 模型状态' })).toBeTruthy();
  await act(async () => finish(response({ error: '状态读取失败' }, 503)));
  await screen.findByText('状态读取失败（当前保留上次状态）');
  expect(screen.getByText(/每 5 秒自动刷新 · 更新于/).textContent).toBe(timestamp);
  expect(screen.getByRole('table', { name: 'Main / 工作凭据 模型状态' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '重试' }));
  await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
  expect(screen.getByRole('table', { name: 'Main / 工作凭据 模型状态' })).toBeTruthy();
});

it('retries a failed model action with the same target while retaining search and expansion', async () => {
  fetcher.mockImplementation((url, _init) => Promise.resolve(url === '/api/status/reset' ? response({ error: '清除失败' }, 503) : response(snapshot)));
  mount(); await screen.findByRole('table', { name: '渠道凭据运行状态' }); search('alpha');
  fireEvent.click(screen.getByRole('button', { name: '展开 Main / 工作凭据 详情' }));
  fireEvent.click(screen.getByRole('button', { name: '恢复模型 alpha' }));
  fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '清除并允许重试' }));
  await screen.findByText('清除失败');
  expect(screen.getByRole('textbox', { name: '搜索运行状态' })).toHaveProperty('value', 'alpha');
  expect(screen.getByRole('table', { name: 'Main / 工作凭据 模型状态' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '重试' }));
  fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '清除并允许重试' }));
  await waitFor(() => expect(fetcher.mock.calls.filter(([url]) => url === '/api/status/reset')).toHaveLength(2));
  const targets = fetcher.mock.calls.filter(([url]) => url === '/api/status/reset').map(([, init]) => JSON.parse(String(init?.body)));
  expect(targets).toEqual([{ channel_id: 1, credential_id: 'key-1', model: 'alpha' }, { channel_id: 1, credential_id: 'key-1', model: 'alpha' }]);
});
