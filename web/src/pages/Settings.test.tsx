// @vitest-environment jsdom
import { useMemo, useRef, useState } from 'react';
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { ConfirmProvider } from '../components/Confirm';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Settings from './Settings';
import { NavigationGuard, type BeforeLeave } from '../components/NavigationGuard';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });

function NavigationHarness() {
  const guard = useRef<BeforeLeave>();
  const [page, setPage] = useState('settings');
  const [error, setError] = useState('');
  const provider = useMemo(() => ({ register: (handler: BeforeLeave) => { guard.current = handler; return () => { guard.current = undefined; }; } }), []);
  return <NavigationGuard.Provider value={provider}><button onClick={async () => { try { await guard.current?.(); setPage('models'); } catch (e) { setError(String(e)); } }}>公开模型导航</button>{error && <p>{error}</p>}{page === 'settings' ? <Settings /> : <p>已到公开模型</p>}</NavigationGuard.Provider>;
}
const settings = { gateway_key: 'synthetic-admin', quota_refresh_seconds: 300, retention_days: 7, max_attempts: 3, retries_per_channel: 2, failure_cooldown_seconds: 10 };
const json = (value: unknown) => new Response(JSON.stringify(value));
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(json(url === '/api/settings' ? settings : { port: 2886, origin: 'http://127.0.0.1:2886' }))));
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('Settings navigation protection', () => {
  it('saves both pending targets before leaving, with the port before the administrator settings', async () => {
    const writes: string[] = [];
    let finishPort!: (response: Response) => void;
    vi.mocked(fetch).mockImplementation((url, init) => {
      if (init?.method === 'PUT') { writes.push(String(url)); return String(url).endsWith('/port') ? new Promise<Response>(resolve => { finishPort = resolve; }) : Promise.resolve(json({})); }
      return Promise.resolve(json(url === '/api/settings' ? settings : { port: 2886 }));
    });
    render(<NavigationHarness />);
    fireEvent.change(await screen.findByLabelText('额度刷新周期'), { target: { value: '123' } });
    await waitFor(() => expect(screen.getByLabelText('监听端口')).toHaveProperty('value', '2886'));
    fireEvent.change(screen.getByLabelText('监听端口'), { target: { value: '3886' } });
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' }));
    fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(writes).toEqual(['/api/runtime/port']));
    expect(screen.getByLabelText('额度刷新周期').matches(':disabled')).toBe(true);
    expect(screen.getByLabelText('监听端口')).toHaveProperty('disabled', true);
    expect(screen.queryByText('已到公开模型')).toBeNull();
    await act(async () => finishPort(json({ port: 3886 })));
    await screen.findByText('已到公开模型');
    expect(writes).toEqual(['/api/runtime/port', '/api/settings']);
  });

  it('keeps an invalid setting draft and retries a rejected exit save without leaving early', async () => {
    let reject = true;
    vi.mocked(fetch).mockImplementation((url, init) => init?.method === 'PUT'
      ? Promise.resolve(reject ? new Response(JSON.stringify({ error: '保存被拒绝' }), { status: 409 }) : json({}))
      : Promise.resolve(json(url === '/api/settings' ? settings : { port: 2886 })));
    render(<NavigationHarness />);
    const period = await screen.findByLabelText('额度刷新周期');
    fireEvent.change(period, { target: { value: '12' } });
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await screen.findByText('额度刷新周期须为 0（仅手动）或 30..86400 秒的整数。');
    expect(vi.mocked(fetch).mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
    fireEvent.change(period, { target: { value: '123' } });
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await screen.findByText('保存被拒绝'); expect(period).toHaveProperty('value', '123'); expect(screen.queryByText('已到公开模型')).toBeNull();
    reject = false;
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await screen.findByText('已到公开模型');
  });

  it('keeps the edited draft when leaving is cancelled, then allows confirmed discard', async () => {
    render(<NavigationHarness />);
    fireEvent.change(await screen.findByLabelText('额度刷新周期'), { target: { value: '123' } });
    const unload = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(unload); expect(unload.defaultPrevented).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByLabelText('额度刷新周期')).toHaveProperty('value', '123');
    expect(screen.queryByText('已到公开模型')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' }));
    fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
    await screen.findByText('已到公开模型');
  });

  it('blocks navigation during save and permits it after the saved response finishes', async () => {
    let finish!: (response: Response) => void;
    const fetcher = vi.mocked(fetch);
    fetcher.mockImplementation((url, init) => init?.method === 'PUT' ? new Promise<Response>(resolve => { finish = resolve; }) : Promise.resolve(json(url === '/api/settings' ? settings : { port: 2886 })));
    render(<NavigationHarness />);
    fireEvent.change(await screen.findByLabelText('额度刷新周期'), { target: { value: '123' } });
    fireEvent.click(screen.getAllByRole('button', { name: '保存设置' })[0]);
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' }));
    await waitFor(() => expect(screen.queryByText('已到公开模型')).toBeNull());
    expect(screen.queryByRole('dialog')).toBeNull(); expect(screen.queryByText('已到公开模型')).toBeNull();
    await act(async () => finish(json({})));
    await waitFor(() => expect(screen.getAllByRole('button', { name: '保存设置' })[0]).toHaveProperty('disabled', true));
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' }));
    await screen.findByText('已到公开模型'); expect(screen.queryByRole('dialog')).toBeNull();
  });
  it('protects an unsaved port and its pending save but permits leaving after saving without restarting', async () => {
    let finish!: (response: Response) => void;
    let savedPort = 2886;
    vi.mocked(fetch).mockImplementation((url, init) => {
      if (url === '/api/runtime/port' && init?.method === 'PUT') return new Promise<Response>(resolve => { finish = resolve; });
      return Promise.resolve(json(url === '/api/settings' ? settings : { port: savedPort, origin: 'http://127.0.0.1:2886' }));
    });
    render(<NavigationHarness />);
    const port = await screen.findByLabelText('监听端口');
    await waitFor(() => expect(port).toHaveProperty('value', '2886'));
    fireEvent.change(port, { target: { value: '3886' } });
    const unload = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(unload); expect(unload.defaultPrevented).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(port).toHaveProperty('value', '3886'); expect(screen.queryByText('已到公开模型')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '保存端口' }));
    expect(port).toHaveProperty('disabled', true);
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' }));
    await waitFor(() => expect(screen.queryByText('已到公开模型')).toBeNull());
    expect(screen.queryByRole('dialog')).toBeNull(); expect(screen.queryByText('已到公开模型')).toBeNull();
    await act(async () => { savedPort = 3886; finish(json({ port: savedPort, restart_required: true })); });
    await screen.findByText('已保存端口 3886，重启后生效。');
    expect(port).toHaveProperty('disabled', false);
    expect(port).toHaveProperty('value', '3886');
    const afterSave = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(afterSave); expect(afterSave.defaultPrevented).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: '公开模型导航' }));
    await screen.findByText('已到公开模型'); expect(screen.queryByRole('dialog')).toBeNull();
  });

});


it('defaults failure cooldown to ten seconds and refuses an out-of-range draft', async () => {
  vi.mocked(fetch).mockImplementation((url) => Promise.resolve(json(url === '/api/settings' ? { ...settings, failure_cooldown_seconds: undefined } : { port: 2886 })));
  render(<Settings />);
  const cooldown = await screen.findByLabelText(/失败冷却时间/);
  expect(cooldown).toHaveProperty('value', '10');
  fireEvent.change(cooldown, { target: { value: '301' } });
  fireEvent.submit(document.getElementById('settings-form')!);
  await screen.findByText('失败冷却时间须为 1..300 秒的整数。');
  expect(vi.mocked(fetch).mock.calls.some(([, options]) => options?.method === 'PUT')).toBe(false);
});

it('opens license notices at the real daemon origin through the native browser bridge without losing drafts', async () => {
  const previousDesktop = Object.getOwnPropertyDescriptor(window, '__TO_ROUTER_DESKTOP__');
  const previousRuntime = window.runtime;
  const open = vi.fn();
  Object.defineProperty(window, '__TO_ROUTER_DESKTOP__', { configurable: true, value: { pet: false, origin: 'http://127.0.0.1:2886' } });
  window.runtime = { BrowserOpenURL: open };
  try {
    render(<Settings />);
    const period = await screen.findByLabelText('额度刷新周期');
    fireEvent.change(period, { target: { value: '123' } });
    const link = screen.getByRole('link', { name: '第三方声明与许可证' });
    expect(link.getAttribute('href')).toBe('http://127.0.0.1:2886/third-party/notices.html');
    fireEvent.click(link);
    expect(open).toHaveBeenCalledWith('http://127.0.0.1:2886/third-party/notices.html');
    expect(period).toHaveProperty('value', '123');
    expect(vi.mocked(fetch).mock.calls.some(([, options]) => options?.method === 'PUT')).toBe(false);
    window.runtime = {};
    fireEvent.click(link);
    expect(screen.getByRole('alert').textContent).toContain('暂时无法打开浏览器');
  } finally {
    if (previousDesktop) Object.defineProperty(window, '__TO_ROUTER_DESKTOP__', previousDesktop);
    else Reflect.deleteProperty(window, '__TO_ROUTER_DESKTOP__');
    window.runtime = previousRuntime;
  }
});

