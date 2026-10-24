// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { MarketAuthorization } from './MarketAuthorization';
import type { MarketAuthMethod } from '../types/plugins';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const base = { packageID: 'fixture-package', providerID: 'supplier', onDone: vi.fn(), onStarted: vi.fn() };
const apiMethod: MarketAuthMethod = { index: 7, type: 'api', label: 'API 接入', prompts: [
  { type: 'select', key: 'region', label: '区域', options: [{ label: '国际', value: 'global' }, { label: '企业', value: 'enterprise' }] },
  { type: 'text', key: 'tenant', label: '企业名称', condition: { key: 'region', op: 'eq', value: 'enterprise' } },
  { type: 'text', key: 'key', label: 'API Key' },
] };
beforeEach(() => { base.onDone.mockClear(); base.onStarted.mockClear(); vi.spyOn(window, 'open').mockReturnValue(null); });
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

it('uses declared method index and conditional inputs without sending hidden values', async () => {
  const fetcher = vi.fn(() => Promise.resolve(json({ flow_id: 'api-flow', status: 'success', channel_id: 42 })));
  vi.stubGlobal('fetch', fetcher);
  render(<MarketAuthorization {...base} methods={[apiMethod]} />);
  expect(screen.queryByLabelText('企业名称')).toBeNull();
  fireEvent.change(screen.getByLabelText('区域'), { target: { value: 'enterprise' } });
  fireEvent.change(screen.getByLabelText('企业名称'), { target: { value: 'Synthetic tenant' } });
  fireEvent.change(screen.getByLabelText('区域'), { target: { value: 'global' } });
  fireEvent.change(screen.getByLabelText('API Key'), { target: { value: 'synthetic-key' } });
  expect(screen.getByLabelText('API Key')).toHaveProperty('type', 'password');
  fireEvent.click(screen.getByRole('button', { name: '连接' }));
  await waitFor(() => expect(base.onDone).toHaveBeenCalledWith(expect.objectContaining({ channel_id: 42 })));
  expect(fetcher).toHaveBeenCalledWith('/api/plugins/market/fixture-package/auth/supplier/start', expect.objectContaining({ method: 'POST', body: JSON.stringify({ method: 7, inputs: { region: 'global', key: 'synthetic-key' } }) }));
  expect(window.open).not.toHaveBeenCalled();
});

it('shows plugin prompts for the selected method and completes a code flow for the exact credential', async () => {
  const methods: MarketAuthMethod[] = [apiMethod, { index: 12, type: 'oauth', label: '浏览器登录', prompts: [{ type: 'text', key: 'workspace', label: '工作区' }] }];
  const fetcher = vi.fn((url: string, _init?: RequestInit) => Promise.resolve(json(url.endsWith('/settings') ? { oauth_auto_open: false } : url.endsWith('/complete') ? { flow_id: 'code-flow', status: 'success', channel_id: 9 } : { flow_id: 'code-flow', status: 'pending', method: 'code', url: 'https://example.invalid/login', instructions: '复制登录后的授权码', state: { secret: 'never-render-this' } })));
  vi.stubGlobal('fetch', fetcher);
  render(<MarketAuthorization {...base} methods={methods} channelID={9} credentialID="member-two" />);
  fireEvent.change(screen.getByLabelText('接入方式'), { target: { value: '12' } });
  expect(screen.queryByLabelText('API Key')).toBeNull();
  fireEvent.change(screen.getByLabelText('工作区'), { target: { value: 'fixture-workspace' } });
  fireEvent.click(screen.getByRole('button', { name: '开始授权' }));
  await screen.findByLabelText('授权码');
  expect(screen.getByRole('link', { name: '打开授权页面' }).getAttribute('href')).toBe('https://example.invalid/login');
  expect(screen.queryByText(/never-render-this/)).toBeNull();
  expect(JSON.parse(String(fetcher.mock.calls.find(([url]) => url.endsWith('/start'))?.[1]?.body))).toEqual({ method: 12, inputs: { workspace: 'fixture-workspace' }, channel_id: 9, credential_id: 'member-two' });
  fireEvent.change(screen.getByLabelText('授权码'), { target: { value: 'fixture-code' } });
  fireEvent.click(screen.getByRole('button', { name: '完成授权' }));
  await waitFor(() => expect(base.onDone).toHaveBeenCalledOnce());
  expect(fetcher).toHaveBeenCalledWith('/api/plugins/market/auth/code-flow/complete', expect.objectContaining({ method: 'POST', body: JSON.stringify({ code: 'fixture-code' }) }));
});

it('polls automatic authorization and ignores a late success after cancellation', async () => {
  vi.useFakeTimers();
  let finish!: (value: Response) => void;
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url.endsWith('/settings')) return Promise.resolve(json({ oauth_auto_open: false }));
    if (url.endsWith('/start')) return Promise.resolve(json({ flow_id: 'auto-flow', status: 'pending', method: 'auto' }));
    if (init?.method === 'DELETE') return Promise.resolve(json({ flow_id: 'auto-flow', status: 'cancelled' }));
    return new Promise<Response>(resolve => { finish = resolve; });
  });
  vi.stubGlobal('fetch', fetcher);
  render(<MarketAuthorization {...base} methods={[{ index: 0, type: 'oauth', label: '浏览器登录' }]} />);
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: '开始授权' })); });
  expect(screen.getByRole('status').textContent).toContain('等待授权');
  await act(async () => { vi.advanceTimersByTime(2000); });
  expect(fetcher).toHaveBeenCalledWith('/api/plugins/market/auth/auto-flow', expect.anything());
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: '取消授权' })); });
  await act(async () => finish(json({ flow_id: 'auto-flow', status: 'success', channel_id: 42 })));
  expect(base.onDone).not.toHaveBeenCalled();
  expect(screen.getByRole('button', { name: '开始授权' })).toBeTruthy();
  expect(fetcher).toHaveBeenCalledWith('/api/plugins/market/auth/auto-flow', expect.objectContaining({ method: 'DELETE' }));
});

it('keeps input drafts on a start failure and never exposes an unsafe authorization link', async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(json({ error: '接入失败' }, 500)).mockResolvedValueOnce(json({ flow_id: 'safe-flow', status: 'pending', method: 'code', url: 'javascript:alert(1)' }));
  vi.stubGlobal('fetch', fetcher);
  render(<MarketAuthorization {...base} methods={[{ index: 0, type: 'api', label: 'API 接入' }]} />);
  fireEvent.change(screen.getByLabelText('API Key'), { target: { value: 'synthetic-key' } });
  fireEvent.click(screen.getByRole('button', { name: '连接' }));
  await screen.findByRole('alert');
  expect(screen.getByLabelText('API Key')).toHaveProperty('value', 'synthetic-key');
  fireEvent.click(screen.getByRole('button', { name: '连接' }));
  await screen.findByLabelText('授权码');
  expect(screen.queryByRole('link')).toBeNull();
});

it('uses custom API prompts without adding a redundant key input', async () => {
  const fetcher = vi.fn(() => Promise.resolve(json({ status: 'success', channel_id: 7, flow_id: 'custom-api' })));
  vi.stubGlobal('fetch', fetcher);
  render(<MarketAuthorization {...base} methods={[{ index: 3, type: 'api', label: '令牌接入', prompts: [{ key: 'token', type: 'text', label: '访问令牌' }] }]} />);
  expect(screen.queryByLabelText('API Key')).toBeNull();
  fireEvent.change(screen.getByLabelText('访问令牌'), { target: { value: 'synthetic-token' } });
  fireEvent.click(screen.getByRole('button', { name: '连接' }));
  await waitFor(() => expect(base.onDone).toHaveBeenCalledOnce());
  expect(fetcher).toHaveBeenCalledWith('/api/plugins/market/fixture-package/auth/supplier/start', expect.objectContaining({ body: JSON.stringify({ method: 3, inputs: { token: 'synthetic-token' } }) }));
});

it('resumes polling when submitting a code starts asynchronous completion', async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn((url: string) => Promise.resolve(json(url.endsWith('/settings') ? { oauth_auto_open: false } : url.endsWith('/start') ? { flow_id: 'async-code', status: 'pending', method: 'code' } : url.endsWith('/complete') ? { flow_id: 'async-code', status: 'pending', method: 'auto' } : { flow_id: 'async-code', status: 'success', channel_id: 8 })));
  vi.stubGlobal('fetch', fetcher);
  render(<MarketAuthorization {...base} methods={[{ index: 0, type: 'oauth', label: '登录' }]} />);
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: '开始授权' })); });
  fireEvent.change(screen.getByLabelText('授权码'), { target: { value: 'synthetic-code' } });
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: '完成授权' })); });
  expect(screen.queryByLabelText('授权码')).toBeNull();
  expect(base.onDone).not.toHaveBeenCalled();
  await act(async () => { vi.advanceTimersByTime(2000); });
  expect(base.onDone).toHaveBeenCalledWith(expect.objectContaining({ status: 'success', channel_id: 8 }));
});
