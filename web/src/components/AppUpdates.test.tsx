// @vitest-environment jsdom
import { StrictMode, useMemo, useRef, useState } from 'react';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AppUpdateArrow, AppUpdatePanel, AppUpdatesProvider } from './AppUpdates';
import { NavigationCancelled, NavigationGuard, type BeforeLeave } from './NavigationGuard';
import { ConfirmProvider } from './Confirm';
import Settings from '../pages/Settings';

const current = { enabled: true, current_version: '1.0.0', state: 'idle' };
const available = { ...current, state: 'available', available: { version: '1.1.0', notes: '更新一\n<script>plain text</script>', os: 'windows', arch: 'amd64', url: 'https://example.invalid/private-download', sha256: 'synthetic-digest', size: 256 } };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
function deferred() { let resolve!: (response: Response) => void; return { promise: new Promise<Response>(done => { resolve = done; }), resolve: (response: Response) => resolve(response) }; }
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json(available))));
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
function Updates({ guard = async () => {}, intent = () => {} }: { guard?: () => Promise<void>; intent?: (active: boolean) => void }) {
  return <AppUpdatesProvider beforeInstall={guard} onUpdateIntent={intent}><strong>to-router<AppUpdateArrow /></strong><AppUpdatePanel /></AppUpdatesProvider>;
}

describe('shared app updates', () => {
  it('shares one cached GET across entries and displays notes as plain text without download internals', async () => {
    render(<Updates />);
    fireEvent.click(await screen.findByRole('button', { name: '发现新版本 1.1.0，查看更新' }));
    const dialog = screen.getByRole('dialog', { name: '软件更新' });
    expect(within(dialog).getByText('更新一 <script>plain text</script>')).toBeTruthy();
    expect(dialog.querySelector('script')).toBeNull();
    expect(screen.queryByText('synthetic-digest')).toBeNull();
    expect(screen.queryByText('https://example.invalid/private-download')).toBeNull();
    expect(fetch).toHaveBeenCalledTimes(1);
  });
  it('does not treat an unchecked or disabled build as current, and only manually checks upstream', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(json(current)).mockResolvedValueOnce(json({ ...current, last_checked: '2026-10-24T00:00:00Z' }));
    const view = render(<Updates />);
    await screen.findByText('尚未检查更新。');
    expect(screen.queryByRole('button', { name: /发现新版本/ })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '检查更新' }));
    await screen.findByText('当前已是最新版本。');
    expect(vi.mocked(fetch).mock.calls[1][1]?.method).toBe('POST');
    expect(vi.mocked(fetch).mock.calls[1][0]).toBe('/api/app-updates/check');
    view.unmount();
    vi.mocked(fetch).mockResolvedValueOnce(json({ ...current, enabled: false, state: 'disabled' }));
    render(<Updates />);
    await screen.findByText('此构建暂未启用自动更新。');
    expect(screen.getByRole('button', { name: '检查更新' })).toHaveProperty('disabled', true);
  });
  it('settles the leave guard before a single install request and keeps progress until restart', async () => {
    const leave = deferred(); const download = deferred(); const intent = vi.fn();
    const guard = vi.fn(async () => { await leave.promise; });
    vi.mocked(fetch).mockImplementation((url) => String(url).endsWith('/install') ? download.promise : Promise.resolve(json(available)));
    render(<Updates guard={guard} intent={intent} />);
    fireEvent.click(await screen.findByRole('button', { name: /发现新版本/ }));
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    expect(guard).toHaveBeenCalledTimes(1);
    expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/install'))).toHaveLength(0);
    await act(async () => leave.resolve(json({})));
    await waitFor(() => expect(intent).toHaveBeenCalledWith(true));
    expect(screen.getByRole('button', { name: '更新并重启中…' })).toHaveProperty('disabled', true);
    fireEvent.click(screen.getByRole('button', { name: '更新并重启中…' }));
    const calls = vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/install'));
    expect(calls).toHaveLength(1); expect(JSON.parse(String(calls[0][1]?.body))).toEqual({ version: '1.1.0', sha256: available.available.sha256 });
    await act(async () => download.resolve(json({ requested: true, port: 2886, version: '1.1.0' })));
    expect(screen.getByRole('button', { name: '更新并重启中…' })).toHaveProperty('disabled', true);
  });
  it('cancels without installation and permits retry after a download rejection', async () => {
    const guard = vi.fn().mockRejectedValueOnce(new NavigationCancelled()).mockResolvedValue(undefined);
    vi.mocked(fetch).mockImplementation((url) => Promise.resolve(String(url).endsWith('/install') ? json({ error: '下载失败，原程序继续运行' }, 502) : json(available)));
    const intent = vi.fn(); render(<Updates guard={guard} intent={intent} />);
    fireEvent.click(await screen.findByRole('button', { name: /发现新版本/ }));
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '更新并重启' })).toHaveProperty('disabled', false));
    expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/install'))).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    await screen.findAllByText('下载失败，原程序继续运行');
    expect(intent).toHaveBeenLastCalledWith(false);
    expect(screen.getByRole('button', { name: '更新并重启' })).toHaveProperty('disabled', false);
  });
  it('unlocks an acknowledged install after live status reports helper launch failure, allowing close and explicit retry', async () => {
    vi.useFakeTimers();let failed = false;
    vi.mocked(fetch).mockImplementation((url) => {
      if (String(url).endsWith('/install')) { failed = true; return Promise.resolve(json({ requested: true, port: 2886, version: '1.1.0' }, 202)); }
      return Promise.resolve(json(failed ? { ...available, state: 'error', error: 'update file operation failed' } : available));
    });
    const intent = vi.fn();const guard = vi.fn(async () => {});render(<Updates guard={guard} intent={intent} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    fireEvent.click(screen.getByRole('button', { name: /发现新版本/ }));
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(screen.getByRole('button', { name: '更新并重启中…' })).toHaveProperty('disabled', true);
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByRole('button', { name: '更新并重启' })).toHaveProperty('disabled', false);
    expect(intent).toHaveBeenLastCalledWith(false);
    expect(screen.getAllByText('update file operation failed')).toHaveLength(2);
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/install'))).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: /发现新版本/ }));
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(guard).toHaveBeenCalledTimes(2);
    expect(intent).toHaveBeenLastCalledWith(true);
    const calls = vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/install'));
    expect(calls).toHaveLength(2);
    expect(calls.map(([, init]) => JSON.parse(String(init?.body)))).toEqual([
      { version: '1.1.0', sha256: available.available.sha256 },
      { version: '1.1.0', sha256: available.available.sha256 },
    ]);
  });
  it('binds an install to the displayed digest and uses the refreshed digest after a same-version refusal', async () => {
    const replacement = { ...available, available: { ...available.available, sha256: 'replacement-synthetic-digest', notes: '同版本重新发行' } };
    let checked = false;
    const requests: unknown[] = [];
    vi.mocked(fetch).mockImplementation((url, init) => {
      if (String(url).endsWith('/install')) {
        requests.push(JSON.parse(String(init?.body)));
        return Promise.resolve(checked ? json({ requested: true, port: 2886, version: '1.1.0' }, 202) : json({ error: '发行文件已变化，请重新检查更新。' }, 409));
      }
      if (String(url).endsWith('/check')) checked = true;
      return Promise.resolve(json(checked ? replacement : available));
    });
    render(<Updates />);
    fireEvent.click(await screen.findByRole('button', { name: /发现新版本/ }));
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    await screen.findAllByText('发行文件已变化，请重新检查更新。');
    expect(requests).toEqual([{ version: '1.1.0', sha256: available.available.sha256 }]);
    fireEvent.click(within(screen.getByRole('dialog', { name: '软件更新' })).getByRole('button', { name: '检查更新' }));
    await screen.findByText('同版本重新发行');
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[1]).toEqual({ version: '1.1.0', sha256: replacement.available.sha256 });
    expect(screen.queryByText(replacement.available.sha256)).toBeNull();
  });
  it('keeps an acknowledged install locked across a normal connection loss without resubmitting', async () => {
    vi.useFakeTimers();let accepted = false;
    vi.mocked(fetch).mockImplementation((url) => {
      if (String(url).endsWith('/install')) { accepted = true; return Promise.resolve(json({ requested: true, port: 2886, version: '1.1.0' }, 202)); }
      return accepted ? Promise.reject(new TypeError('network unavailable')) : Promise.resolve(json(available));
    });
    const intent = vi.fn();render(<Updates intent={intent} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    fireEvent.click(screen.getByRole('button', { name: /发现新版本/ }));
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(screen.getByRole('button', { name: '更新并重启中…' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: '关闭弹窗' })).toHaveProperty('disabled', true);
    expect(intent).toHaveBeenLastCalledWith(true);
    expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/install'))).toHaveLength(1);
  });
  it('displays a failed view reopen as a nonfatal result', async () => {
    vi.mocked(fetch).mockResolvedValue(json({ ...current, last_result: { version: '1.0.0', state: 'updated', executable_restored: false, data_restored: false, view_error: 'view launch failed' } }));
    render(<Updates />);
    expect((await screen.findByText('程序已恢复运行，但暂未打开视图：view launch failed。可从托盘重新打开。')).getAttribute('role')).toBe('status');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.getByRole('button', { name: '检查更新' })).toHaveProperty('disabled', false);
  });
  it.each([
    { state: 'rolled_back', executable_restored: true, data_restored: true, summary: '更新 1.1.0 失败，已恢复旧版程序，当前运行版本 1.0.0。', data: '数据已恢复到本次更新前的快照；更新尝试期间的写入不会保留。', error: 'new version failed readiness' },
    { state: 'rolled_back', executable_restored: false, data_restored: false, summary: '更新 1.1.0 失败，已重新启动原版本 1.0.0。', data: '本次未执行数据快照回退。', error: 'update preparation failed' },
    { state: 'failed', executable_restored: false, data_restored: false, summary: '最近更新 1.1.0 未完成。当前运行版本 1.0.0。', data: '', error: 'update file operation failed' },
    { state: 'updated', executable_restored: false, data_restored: false, summary: '最近已成功更新至 1.1.0。当前运行版本 1.0.0。', data: '', error: '' },
  ])('shows the recent $state result consistently in settings and the modal without blocking retries', async ({ state, executable_restored, data_restored, summary, data, error }) => {
    vi.mocked(fetch).mockResolvedValue(json({ ...available, last_result: { version: '1.1.0', state, executable_restored, data_restored, error } }));
    render(<Updates />);
    await screen.findByRole('button', { name: /发现新版本/ });
    const panel = within(screen.getByRole('region', { name: '关于与更新' })).getByRole('region', { name: '最近更新结果' });
    expect(within(panel).getByText(summary)).toBeTruthy();
    if (data) expect(within(panel).getByText(data)).toBeTruthy();
    if (error) expect(within(panel).getByText(`原因：${error}`)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: /发现新版本/ }));
    const dialog = screen.getByRole('dialog', { name: '软件更新' });
    const result = within(dialog).getByRole('region', { name: '最近更新结果' });
    expect(result.textContent).toBe(panel.textContent);
    expect(within(dialog).getByRole('button', { name: '更新并重启' })).toHaveProperty('disabled', false);
    expect(within(dialog).getByRole('button', { name: '关闭弹窗' })).toHaveProperty('disabled', false);
    expect(document.querySelector('.toast')).toBeNull();
  });
  it('aborts on auth expiration and ignores an old response', async () => {
    const request = deferred();
    vi.mocked(fetch).mockReturnValueOnce(request.promise);
    render(<Updates />);
    act(() => window.dispatchEvent(new Event('auth-expired')));
    expect(vi.mocked(fetch).mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => request.resolve(json(available)));
    expect(screen.queryByRole('button', { name: /发现新版本/ })).toBeNull();
  });
  it('does not install when saving a draft expires the session', async () => {
    const guard = async () => { window.dispatchEvent(new Event('auth-expired')); };
    render(<Updates guard={guard} />);
    fireEvent.click(await screen.findByRole('button', { name: /发现新版本/ }));
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: /发现新版本/ })).toBeNull());
    expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/install'))).toHaveLength(0);
  });
  it('refreshes only cached status in a visible sidebar and stops idle polling in a hidden tab', async () => {
    vi.useFakeTimers();let visible = 'visible';vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visible as DocumentVisibilityState);
    vi.mocked(fetch).mockResolvedValueOnce(json(current)).mockResolvedValue(json(available));
    render(<AppUpdatesProvider beforeInstall={async () => {}} onUpdateIntent={() => {}}><AppUpdateArrow /></AppUpdatesProvider>);
    await act(async () => { await vi.advanceTimersByTimeAsync(60000); });
    expect(screen.getByRole('button', { name: /发现新版本/ })).toBeTruthy();
    expect(vi.mocked(fetch).mock.calls.every(([, init]) => init?.method === 'GET')).toBe(true);
    const count = vi.mocked(fetch).mock.calls.length; visible = 'hidden';
    await act(async () => { await vi.advanceTimersByTimeAsync(120000); });
    expect(fetch).toHaveBeenCalledTimes(count);
  });
  it('survives strict effect re-mount and retains the successful status when a consumer re-mounts', async () => {
    function View() { const [visible, show] = useState(true); return <AppUpdatesProvider beforeInstall={async () => {}} onUpdateIntent={() => {}}><AppUpdateArrow /><button onClick={() => show(value => !value)}>切换面板</button>{visible && <AppUpdatePanel />}</AppUpdatesProvider>; }
    render(<StrictMode><View /></StrictMode>);
    await screen.findByRole('button', { name: /发现新版本/ });
    const count = vi.mocked(fetch).mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: '切换面板' }));
    fireEvent.click(screen.getByRole('button', { name: '切换面板' }));
    expect(screen.getByText('当前版本 1.0.0')).toBeTruthy();
    expect(fetch).toHaveBeenCalledTimes(count);
  });
});

describe('settings update integration', () => {
  it('saves the draft before install without overwriting the tray launch preference', async () => {
    const writes: string[] = [];
    vi.mocked(fetch).mockImplementation((url, init) => {
      if (init?.method === 'PUT') { writes.push(String(url)); expect(JSON.parse(String(init.body))).not.toHaveProperty('default_launch_mode'); return Promise.resolve(json({})); }
      if (String(url).endsWith('/install')) { writes.push(String(url)); return Promise.resolve(json({ requested: true, port: 2886 })); }
      if (url === '/api/settings') return Promise.resolve(json({ gateway_key: 'synthetic-admin', default_launch_mode: 'web', quota_refresh_seconds: 300, retention_days: 7, max_attempts: 3, retries_per_channel: 2, failure_cooldown_seconds: 10 }));
      return Promise.resolve(json(String(url).includes('/app-updates') ? available : { port: 2886 }));
    });
    function Harness() {
      const handlers = useRef<BeforeLeave[]>([]);
      const guard = useMemo(() => ({ register: (handler: BeforeLeave) => { handlers.current.push(handler); return () => { handlers.current = handlers.current.filter(value => value !== handler); }; } }), []);
      return <ConfirmProvider><AppUpdatesProvider beforeInstall={async () => { for (const handler of handlers.current) await handler(); }} onUpdateIntent={() => {}}><NavigationGuard.Provider value={guard}><Settings /></NavigationGuard.Provider></AppUpdatesProvider></ConfirmProvider>;
    }
    render(<Harness />);
    fireEvent.change(await screen.findByLabelText('额度刷新周期'), { target: { value: '123' } });
    fireEvent.click(screen.getByRole('button', { name: '版本与说明' }));
    fireEvent.click(screen.getByRole('button', { name: '更新并重启' }));
    fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(writes).toEqual(['/api/settings', '/api/app-updates/install']));
  });
});
