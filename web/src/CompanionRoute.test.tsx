// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App from './App';
import { blankPrice } from './hooks/usePriceWorkbench';

const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const live = { server_time: '2026-10-06T09:00:00Z', session_id: 'test-server', sequence: 0, active_count: 0, active: [], recent: [], today: { records: 0, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, estimated_records: 0 } };
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}
function backend(save?: (options: RequestInit) => Promise<Response>) {
  const { key: _key, ...price } = blankPrice(0, 'CNY', 1);
  const row = { ...price, model: 'public-a' };
  return vi.fn().mockImplementation((url: string, options: RequestInit = {}) => {
    if (url === '/api/status') return Promise.resolve(response({ rows: [], problem_count: 0 }));
    if (url === '/api/auth/session') return Promise.resolve(response({ authenticated: true }));
    if (url === '/api/companion') return Promise.resolve(response(live));
    if (url === '/api/desktop/pet') return Promise.resolve(response({running:false}));
    if (url === '/api/companion/preferences') return Promise.resolve(response({theme:'dark',paused:false,revision:0}));
    if (url === '/api/prices') return options.method === 'PUT' ? save?.(options) ?? Promise.resolve(response(JSON.parse(String(options.body)))) : Promise.resolve(response([row]));
    if (url === '/api/channels') return Promise.resolve(response([]));
    if (url === '/api/models') return Promise.resolve(response([{ id: 'public-a', enabled: true, bindings: [], config: {} }]));
    if (url === '/api/exchange-rate') return Promise.resolve(response({ available: false }));
    return Promise.resolve(response({}, 404));
  });
}

beforeEach(() => {
  sessionStorage.clear(); localStorage.clear();
  history.replaceState(null, '', '#/companion');
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
  vi.stubGlobal('requestAnimationFrame', vi.fn(() => 1));
  vi.stubGlobal('cancelAnimationFrame', vi.fn());
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function(this: HTMLDialogElement) { this.setAttribute('open', ''); } });
  Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value: function(this: HTMLDialogElement) { this.removeAttribute('open'); } });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('authenticated companion route', () => {
  it('checks the session before mounting pet management and preserves its destination', async () => {
    const route = 'companion';
    history.replaceState(null, '', `#/${route}`);
    const restore = deferred<Response>();
    const fallback = backend();
    const fetcher = vi.fn().mockImplementation((url: string, options?: RequestInit) => url === '/api/auth/session' ? restore.promise : fallback(url, options));
    vi.stubGlobal('fetch', fetcher);
    render(<App />);
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(fetcher.mock.calls[0][0]).toBe('/api/auth/session');
    expect(screen.queryByRole('heading', {name:'桌宠管理'})).toBeNull();
    expect(window.location.hash).toBe('#/companion');
    await act(async () => restore.resolve(response({ authenticated: true })));
    await screen.findByRole('heading', {name:'桌宠管理'});
    expect(window.location.hash).toBe('#/companion');
    await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url === '/api/desktop/pet')).toBe(true));
    fireEvent.click(within(screen.getByRole('navigation', {name:'主导航'})).getByRole('button', { name: '上游接入' }));
    await screen.findByRole('heading', { name: '上游接入' }, { timeout: 4000 });
    expect(window.location.hash).toBe('#/channels');
  });

  it('falls back to login when local restoration is unavailable and returns to management after successful login', async () => {
    const fetcher = vi.fn().mockImplementation((url: string) => Promise.resolve(response(url === '/api/companion' ? live : {}, url === '/api/auth/session' ? 401 : url === '/api/auth/local' ? 403 : 200)));
    vi.stubGlobal('fetch', fetcher);
    render(<App />);
    await screen.findByLabelText('管理员密钥');
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual(['/api/auth/session', '/api/auth/local']);
    expect(screen.queryByRole('heading', {name:'桌宠管理'})).toBeNull();
    expect(window.location.hash).toBe('#/companion');
    fireEvent.change(screen.getByLabelText('管理员密钥'), { target: { value: 'test-key' } });
    fireEvent.click(screen.getByRole('button', { name: '进入控制台' }));
    await screen.findByRole('heading', {name:'桌宠管理'});
    expect(window.location.hash).toBe('#/companion');
    expect(fetcher.mock.calls.map(([url]) => url)).toContain('/api/auth/login');
  });

  it('aborts an in-flight pet state poll during session restoration and ignores its late result', async () => {
    let expired = false;
    const polling = deferred<Response>();
    const fetcher = vi.fn().mockImplementation((url: string) => url === '/api/desktop/pet' ? polling.promise : Promise.resolve(response({}, url === '/api/auth/local' ? 403 : url === '/api/auth/session' && expired ? 401 : 200)));
    vi.stubGlobal('fetch', fetcher);
    render(<App />);
    await screen.findByRole('heading', {name:'桌宠管理'});
    await act(async () => { expired = true; window.dispatchEvent(new Event('auth-expired')); });
    await screen.findByLabelText('管理员密钥');
    expect(screen.queryByRole('heading', {name:'桌宠管理'})).toBeNull();
    const poll = fetcher.mock.calls.find(([url]) => url === '/api/desktop/pet');
    expect(poll).toBeTruthy();
    expect((poll as unknown[])[1]).toMatchObject({ signal: expect.any(AbortSignal) });
    expect(((poll as unknown[])[1] as RequestInit).signal?.aborted).toBe(true);
    await act(async () => polling.resolve(response({running:true})));
    expect(screen.queryByRole('heading', {name:'桌宠管理'})).toBeNull();
    expect(window.location.hash).toBe('#/companion');
  });

  it('retains the price page, input and hash while management hash navigation saves, including a failed save', async () => {
    history.replaceState(null, '', '#/prices');
    const saving = deferred<Response>();
    let attempt = 0;
    const fetcher = backend(options => ++attempt === 1 ? saving.promise : Promise.resolve(response(JSON.parse(String(options.body)))));
    vi.stubGlobal('fetch', fetcher);
    render(<App />);
    const input = await screen.findByLabelText('输入 0:public-a');
    fireEvent.change(input, { target: { value: '6' } });
    await act(async () => { window.location.hash = '#/companion'; window.dispatchEvent(new HashChangeEvent('hashchange')); });
    fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(attempt).toBe(1));
    expect(window.location.hash).toBe('#/prices');
    expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '6');
    expect(screen.queryByRole('heading', {name:'桌宠管理'})).toBeNull();
    await act(async () => saving.resolve(response({ error: 'save failed' }, 503)));
    await waitFor(() => expect(screen.getAllByRole('alert').some(alert => alert.textContent?.includes('save failed'))).toBe(true));
    expect(window.location.hash).toBe('#/prices');
    expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '6');
    expect(screen.queryByRole('heading', {name:'桌宠管理'})).toBeNull();
    fireEvent.click(screen.getByRole('link', { name: '桌面小宠' }));
    fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await screen.findByRole('heading', {name:'桌宠管理'});
    expect(window.location.hash).toBe('#/companion');
    const puts = fetcher.mock.calls.filter(([url, options]) => url === '/api/prices' && options.method === 'PUT');
    expect(JSON.parse(String(puts[1][1].body))[0].selection.table.rates.input).toBe(6);
  });

  it('routes the management command through the price save guard without demo wording or early unmount', async () => {
    history.replaceState(null, '', '#/prices');
    const saving = deferred<Response>();
    const fetcher = backend(() => saving.promise);
    vi.stubGlobal('fetch', fetcher);
    render(<App />);
    fireEvent.change(await screen.findByLabelText('输出 0:public-a'), { target: { value: '4' } });
    fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    const command = await screen.findByRole('button', { name: /桌宠管理 开启或关闭桌面小宠/ });
    expect(command.textContent).not.toContain('演示');
    fireEvent.click(command);
    fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(fetcher.mock.calls.some(([url, options]) => url === '/api/prices' && options.method === 'PUT')).toBe(true));
    expect(window.location.hash).toBe('#/prices');
    expect(screen.getByLabelText('输出 0:public-a')).toHaveProperty('value', '4');
    expect(screen.queryByRole('heading', {name:'桌宠管理'})).toBeNull();
    const put = fetcher.mock.calls.find(([url, options]) => url === '/api/prices' && options.method === 'PUT')!;
    await act(async () => saving.resolve(response(JSON.parse(String(put[1].body)))));
    await screen.findByRole('heading', {name:'桌宠管理'});
    expect(window.location.hash).toBe('#/companion');
  });

  it.each(['pet', 'floating-pet'])('mounts floating pet on direct %s route after session check', async route => {
    history.replaceState(null, '', `#/${route}`);
    const restore = deferred<Response>();
    const fallback = backend();
    const fetcher = vi.fn().mockImplementation((url: string, options?: RequestInit) => url === '/api/auth/session' ? restore.promise : fallback(url, options));
    vi.stubGlobal('fetch', fetcher);
    render(<App />);
    expect(screen.queryByRole('button', { name: '摸摸蓝鲸和 Clawd' })).toBeNull();
    await act(async () => restore.resolve(response({ authenticated: true })));
    await screen.findByRole('button', { name: '摸摸蓝鲸和 Clawd' });
    expect(window.location.hash).toBe('#/pet');
  });
});


it('routes runtime status sidebar and command navigation through the existing save guard', async () => {
  history.replaceState(null, '', '#/prices');
  const saving = deferred<Response>();
  const fetcher = backend(() => saving.promise);
  vi.stubGlobal('fetch', fetcher);
  render(<App />);
  fireEvent.change(await screen.findByLabelText('输出 0:public-a'), { target: { value: '4' } });
  fireEvent.click(within(screen.getByRole('navigation', { name: '主导航' })).getByRole('button', { name: /运行状态/ }));
  fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(fetcher.mock.calls.some(([url, options]) => url === '/api/prices' && options.method === 'PUT')).toBe(true));
  expect(window.location.hash).toBe('#/prices');
  await act(async () => saving.resolve(response({ error: 'save failed' }, 503)));
  await waitFor(() => expect(screen.getAllByRole('alert').some(alert => alert.textContent?.includes('save failed'))).toBe(true));
  expect(screen.getByLabelText('输出 0:public-a')).toHaveProperty('value', '4');
  fetcher.mockImplementation(backend());
  fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
  fireEvent.change(screen.getByPlaceholderText(/搜索功能/), { target: { value: 'runtime status' } });
  fireEvent.click(await screen.findByRole('button', { name: /运行状态 RUNTIME STATUS/ }));
  fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByRole('heading', { name: '运行状态' });
  expect(window.location.hash).toBe('#/status');
  expect(screen.getByText('RUNTIME STATUS')).toBeTruthy();
});
