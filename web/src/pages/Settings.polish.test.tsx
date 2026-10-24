// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import Settings from './Settings';
import { restartConsole } from '../restart';
vi.mock('../restart', () => ({ restartConsole: vi.fn() }));

const settings = { gateway_key: 'synthetic-admin', prices: [], quota_refresh_seconds: 300, retention_days: 7, max_attempts: 3, retries_per_channel: 2, failure_cooldown_seconds: 10, request_defaults_enabled: false, request_defaults: { max_output_tokens: 0 } };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
let fetcher: ReturnType<typeof vi.fn>;
const writes = () => fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
const mount = () => render(<ConfirmProvider><Settings /></ConfirmProvider>);
const saveButton = () => screen.getAllByRole('button', { name: '保存设置' })[0];
async function ready() { const period = await screen.findByLabelText('额度刷新周期'); await waitFor(() => expect(screen.getByLabelText('监听端口')).toHaveProperty('value', '2886')); return period; }
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  fetcher = vi.fn((url: string, init?: RequestInit) => Promise.resolve(json(init?.method === 'PUT' ? url.endsWith('/port') ? { port: JSON.parse(String(init.body)).port } : {} : url === '/api/settings' ? settings : { port: 2886, origin: 'http://127.0.0.1:2886' })));
  vi.stubGlobal('fetch', fetcher); vi.mocked(restartConsole).mockReset(); vi.mocked(restartConsole).mockRejectedValue(new Error('isolated restart stopped'));
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('Settings polish', () => {
  it('saves a port-only draft from the main button and reports both target states', async () => {
    mount(); await ready(); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } });
    expect(screen.getByText('运行与登录设置已保存 · 端口有未保存更改')).toBeTruthy(); fireEvent.click(saveButton());
    await screen.findByText('运行与登录设置已保存 · 端口已保存，等待重启生效');
    expect(writes().map(([url]) => url)).toEqual(['/api/runtime/port']); expect(saveButton()).toHaveProperty('disabled', true);
  });
  it('retains the first saved target when the second is rejected and retries only the pending target', async () => {
    const base = fetcher.getMockImplementation()!; let rejected = true;
    fetcher.mockImplementation((url, init) => url === '/api/settings' && init?.method === 'PUT' && rejected ? Promise.resolve(json({ error: 'settings rejected' }, 409)) : base(url, init));
    mount(); const period = await ready(); fireEvent.change(period, { target: { value: '123' } }); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } });
    fireEvent.click(saveButton()); await screen.findByText('settings rejected');
    expect(writes().map(([url]) => url)).toEqual(['/api/runtime/port', '/api/settings']);
    expect(period).toHaveProperty('value', '123'); expect(screen.getByLabelText('监听端口')).toHaveProperty('value', '3886');
    expect(screen.getByText('运行与登录设置有未保存更改 · 端口已保存，等待重启生效')).toBeTruthy();
    rejected = false; fireEvent.click(saveButton()); await waitFor(() => expect(saveButton()).toHaveProperty('disabled', true));
    expect(writes().map(([url]) => url)).toEqual(['/api/runtime/port', '/api/settings', '/api/settings']);
  });
  it('does not save administrator settings when the first port write is rejected', async () => {
    const base = fetcher.getMockImplementation()!; let rejected = true;
    fetcher.mockImplementation((url, init) => url.endsWith('/port') && init?.method === 'PUT' && rejected ? Promise.resolve(json({ error: 'port rejected' }, 409)) : base(url, init));
    mount(); fireEvent.change(await ready(), { target: { value: '123' } }); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } });
    fireEvent.click(saveButton()); await screen.findByText('port rejected'); expect(writes()).toHaveLength(1);
    expect(screen.getByText('运行与登录设置有未保存更改 · 端口有未保存更改')).toBeTruthy();
    rejected = false; fireEvent.click(saveButton()); await waitFor(() => expect(saveButton()).toHaveProperty('disabled', true));
    expect(writes().map(([url]) => url)).toEqual(['/api/runtime/port', '/api/runtime/port', '/api/settings']);
  });
  it('retains a new settings draft while a post-save read completes late', async () => {
    const base = fetcher.getMockImplementation()!; let reads = 0; let complete!: (response: Response) => void;
    fetcher.mockImplementation((url, init) => url === '/api/settings' && !init?.method && ++reads > 1 ? new Promise<Response>(resolve => { complete = resolve; }) : base(url, init));
    mount(); const period = await ready(); fireEvent.change(period, { target: { value: '123' } }); fireEvent.click(saveButton());
    await waitFor(() => expect(saveButton()).toHaveProperty('disabled', true)); fireEvent.change(screen.getByLabelText('额度刷新周期'), { target: { value: '456' } });
    await act(async () => complete(json(settings))); expect(screen.getByLabelText('额度刷新周期')).toHaveProperty('value', '456');
    fireEvent.click(saveButton()); await waitFor(() => expect(writes()).toHaveLength(2)); expect(JSON.parse(writes()[1][1].body).quota_refresh_seconds).toBe(456);
  });
  it('retains a new port draft when its post-save read returns an older port', async () => {
    const base = fetcher.getMockImplementation()!; let reads = 0; let complete!: (response: Response) => void;
    fetcher.mockImplementation((url, init) => url.endsWith('/port') && !init?.method && ++reads > 1 ? new Promise<Response>(resolve => { complete = resolve; }) : base(url, init));
    mount(); await ready(); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } }); fireEvent.click(screen.getByRole('button', { name: '保存端口' }));
    await screen.findByText('已保存端口 3886，重启后生效。'); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '4886' } });
    await act(async () => complete(json({ port: 2886, origin: 'http://127.0.0.1:2886' })));
    expect(screen.getByLabelText('监听端口')).toHaveProperty('value', '4886'); expect(screen.getByText('运行与登录设置已保存 · 端口有未保存更改')).toBeTruthy();
  });
  it('saves both drafts before presenting a restart confirmation for the saved port', async () => {
    mount(); fireEvent.change(await ready(), { target: { value: '123' } }); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } });
    fireEvent.click(screen.getByRole('button', { name: '重启服务' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    const dialog = await screen.findByRole('dialog', { name: '重启服务？' }); expect(within(dialog).getByText(/已保存端口 3886/)).toBeTruthy();
    expect(writes().map(([url]) => url)).toEqual(['/api/runtime/port', '/api/settings']); expect(restartConsole).not.toHaveBeenCalled();
    expect(screen.getAllByRole('dialog')).toHaveLength(1); fireEvent.click(within(dialog).getByRole('button', { name: '确认重启' })); await waitFor(() => expect(restartConsole).toHaveBeenCalledOnce());
  });
  it('discards drafts before restart and uses the previously saved port', async () => {
    mount(); fireEvent.change(await ready(), { target: { value: '123' } }); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } });
    fireEvent.click(screen.getByRole('button', { name: '重启服务' })); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
    const dialog = await screen.findByRole('dialog', { name: '重启服务？' }); expect(within(dialog).getByText(/已保存端口 2886/)).toBeTruthy();
    expect(screen.getByLabelText('额度刷新周期')).toHaveProperty('value', '300'); expect(screen.getByLabelText('监听端口')).toHaveProperty('value', '2886'); expect(writes()).toHaveLength(0);
    fireEvent.click(within(dialog).getByRole('button', { name: '确认重启' })); await waitFor(() => expect(restartConsole).toHaveBeenCalledOnce());
  });
  it('keeps drafts when the save decision is closed and cancels the later restart confirmation', async () => {
    mount(); fireEvent.change(await ready(), { target: { value: '123' } }); fireEvent.click(screen.getByRole('button', { name: '重启服务' }));
    const decision = await screen.findByRole('dialog', { name: '保存更改？' }); fireEvent.click(screen.getByRole('button', { name: '重启服务' })); expect(screen.getAllByRole('dialog')).toHaveLength(1);
    fireEvent.click(within(decision).getByRole('button', { name: '关闭弹窗' })); await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByLabelText('额度刷新周期')).toHaveProperty('value', '123'); expect(writes()).toHaveLength(0); expect(restartConsole).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '重启服务' })); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '重启服务？' })).getByRole('button', { name: '取消' })); expect(restartConsole).not.toHaveBeenCalled();
  });
  it('keeps invalid port and settings drafts and never reaches restart', async () => {
    mount(); const period = await ready(); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '65536' } });
    fireEvent.click(screen.getByRole('button', { name: '重启服务' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await screen.findByText('端口必须是 1..65535 的整数。'); expect(screen.getByLabelText('监听端口')).toHaveProperty('value', '65536'); expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } }); fireEvent.change(period, { target: { value: '12' } });
    fireEvent.click(screen.getByRole('button', { name: '重启服务' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await screen.findByText('额度刷新周期须为 0（仅手动）或 30..86400 秒的整数。'); expect(writes()).toHaveLength(0); expect(restartConsole).not.toHaveBeenCalled();
  });
  it('does not restart after a rejected save and permits retry', async () => {
    const base = fetcher.getMockImplementation()!; fetcher.mockImplementation((url, init) => init?.method === 'PUT' ? Promise.resolve(json({ error: 'save rejected' }, 409)) : base(url, init));
    mount(); fireEvent.change(await ready(), { target: { value: '123' } }); fireEvent.click(screen.getByRole('button', { name: '重启服务' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await screen.findByText('save rejected'); expect(screen.getByLabelText('额度刷新周期')).toHaveProperty('value', '123'); expect(screen.queryByRole('dialog')).toBeNull(); expect(restartConsole).not.toHaveBeenCalled();
  });
  it('preserves key-change security and stops restart after expiring the session', async () => {
    const expired = vi.fn(); window.addEventListener('auth-expired', expired);
    try {
      mount(); await ready(); fireEvent.change(screen.getByLabelText(/管理员登录密钥/), { target: { value: 'new-synthetic-admin' } }); fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } });
      fireEvent.click(screen.getByRole('button', { name: '重启服务' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
      await waitFor(() => expect(expired).toHaveBeenCalledOnce()); expect(writes().map(([url]) => url)).toEqual(['/api/runtime/port', '/api/settings']); expect(restartConsole).not.toHaveBeenCalled(); expect(screen.queryByRole('dialog')).toBeNull();
      expect(fetcher.mock.calls.filter(([url, init]) => url === '/api/settings' && !init?.method)).toHaveLength(1); expect(JSON.parse(writes()[1][1].body)).not.toHaveProperty('prices');
    } finally { window.removeEventListener('auth-expired', expired); }
  });
  it('blocks duplicate saves and duplicate restart requests while each operation is pending', async () => {
    const base = fetcher.getMockImplementation()!; let complete!: (value: Response) => void;
    fetcher.mockImplementation((url, init) => init?.method === 'PUT' ? new Promise<Response>(resolve => { complete = resolve; }) : base(url, init));
    mount(); fireEvent.change(await ready(), { target: { value: '123' } }); fireEvent.click(saveButton()); fireEvent.submit(document.getElementById('settings-form')!); expect(writes()).toHaveLength(1);
    await act(async () => complete(json({}))); await waitFor(() => expect(saveButton()).toHaveProperty('disabled', true));
    let reject!: (reason: Error) => void; vi.mocked(restartConsole).mockImplementation(() => new Promise<string>((_, no) => { reject = no; }));
    fireEvent.click(screen.getByRole('button', { name: '重启服务' })); const confirm = await screen.findByRole('button', { name: '确认重启' }); fireEvent.click(confirm); fireEvent.click(confirm);
    expect(restartConsole).toHaveBeenCalledOnce();
    const unload = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(unload); expect(unload.defaultPrevented).toBe(false);
    await act(async () => reject(new Error('isolated failure'))); await screen.findByText('isolated failure');
  });
  it('recovers independent read failures without enabling controls for unknown values', async () => {
    let failed = true; const base = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => failed && !init?.method ? Promise.resolve(json({ error: 'read failed' }, 500)) : base(url, init));
    mount(); await screen.findByText('读取运行与登录设置失败：read failed'); await screen.findByText('读取监听端口失败：read failed'); expect(screen.getByRole('button', { name: '重启服务' })).toHaveProperty('disabled', true);
    failed = false; for (const retry of screen.getAllByRole('button', { name: '重试' })) fireEvent.click(retry); await ready(); expect(screen.getByRole('button', { name: '重启服务' })).toHaveProperty('disabled', false);
    expect(screen.getByLabelText('启用请求兜底参数')).toHaveProperty('checked', false); expect(screen.getByLabelText('默认最大输出')).toHaveProperty('value', '0');
  });
  it('applies the same draft decision before shutdown and does not execute when cancelled', async () => {
    mount(); fireEvent.change(await ready(), { target: { value: '123' } }); fireEvent.click(screen.getByRole('button', { name: '退出服务' })); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
    const dialog = await screen.findByRole('dialog', { name: '退出服务？' }); fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    expect(fetcher.mock.calls.some(([url]) => url === '/api/shutdown')).toBe(false); expect(writes()).toHaveLength(0);
  });
});
