// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Plugins from './Plugins';
import { ConfirmProvider } from '../components/Confirm';
import type { PluginCapability, PluginInfo, PluginList } from '../types/plugins';

let pluginFetcher: ReturnType<typeof vi.fn>;
const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const connect: PluginCapability = { id: 'connect', enabled: true, available: true, affected_channel_ids: [] };
function plugin(id: string, changes: Partial<PluginInfo> = {}): PluginInfo {
  return { id, name: id, description: `${id} 直连能力`, icon: id, implementation: 'native', featured: true, version: '1.0', capabilities: [{ ...connect }], ...changes };
}
const snapshot: PluginList = { version: 'v1', items: [plugin('Codex'), plugin('Claude 订阅'), plugin('Antigravity', { featured: false })] };
function mount() { return render(<ConfirmProvider><Plugins /></ConfirmProvider>); }
beforeEach(() => {
  history.replaceState(null, '', '/#/plugins');
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  pluginFetcher = vi.fn(() => Promise.resolve(response(snapshot)));
  vi.stubGlobal('fetch', (path: string, options?: RequestInit) => path.startsWith('/api/plugins/market') ? Promise.resolve(response({ items: [], refreshing: false })) : pluginFetcher(path, options));
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('plugin library', () => {
  it.each(['workbuddy', 'workbuddy-ai', 'minimax-code', 'minimax-code-global'] as const)('jumps from %s to its channel pool', async provider => {
    pluginFetcher.mockResolvedValueOnce(response({ version: 'providers', items: [plugin(provider, { credential_type: `plugin:${provider}` })] }));
    mount();
    expect((await screen.findByRole('link', { name: '去接入' })).getAttribute('href')).toBe(`#/channels?plugin=${provider}`);
  });

  it('installs the fixed compatibility layer without enabling or logging in automatically', async () => {
    const initial: PluginList = { version: 'before', compatible: { installed: false }, items: [plugin('qoder', { implementation: 'compatible', capabilities: [{ ...connect, enabled: false, available: false, unavailable_reason: '需要 Node.js 24' }] })] };
    const installed: PluginList = { ...initial, version: 'after', compatible: { installed: true, version: 'v24.13.0' }, items: initial.items.map(p => ({ ...p, capabilities: [{ ...connect, enabled: false }] })) };
    pluginFetcher.mockResolvedValueOnce(response(initial)).mockResolvedValueOnce(response(installed)).mockResolvedValueOnce(response(installed));
    mount();
    fireEvent.click(await screen.findByRole('button', { name: '安装兼容层' }));
    const modal = screen.getByRole('dialog');
    expect(within(modal).getByRole('link', { name: 'Node.js 官网' }).getAttribute('href')).toBe('https://nodejs.org/en/download');
    fireEvent.click(within(modal).getByRole('button', { name: '检测并安装' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(pluginFetcher).toHaveBeenCalledTimes(3);
    const [path, options] = pluginFetcher.mock.calls[1];
    expect(String(path)).toContain('/plugins/compatible/install');
    expect(JSON.parse(String(options?.body))).toEqual({ runtime_path: '' });
    expect(screen.getByRole('switch', { name: 'qoder 反代直连' })).toHaveProperty('checked', false);
    expect(screen.getByRole('switch', { name: 'qoder 反代直连' })).toHaveProperty('disabled', false);
    expect(screen.queryByRole('link', { name: '去接入' })).toBeNull();
  });

  it('groups by featured, uses implementation only as badge, and exposes channel links', async () => {
    mount();
    const featured = await screen.findByRole('region', { name: '精选' });
    expect(within(featured).getAllByRole('article').map(item => item.getAttribute('aria-label'))).toEqual(['Codex', 'Claude 订阅']);
    expect(within(screen.getByRole('region', { name: '其他' })).getByRole('article', { name: 'Antigravity' })).toBeTruthy();
    expect(screen.getAllByText('原生')).toHaveLength(3);
    expect(screen.queryByText(/默认启用|预装|选装/)).toBeNull();
    expect(screen.queryByRole('button', { name: /安装|扫描|嗅探/ })).toBeNull();
    expect(within(screen.getByRole('article', { name: 'Claude 订阅' })).getByRole('link', { name: '去接入' }).getAttribute('href')).toBe('#/channels?plugin=Claude%20%E8%AE%A2%E9%98%85');
    expect(pluginFetcher).toHaveBeenCalledTimes(1);
  });

  it('preserves independent category queries and data without new requests or authentication', async () => {
    mount(); await screen.findByRole('article', { name: 'Codex' });
    fireEvent.change(screen.getByRole('textbox', { name: '搜索插件' }), { target: { value: 'Codex' } });
    fireEvent.click(screen.getByRole('tab', { name: '自动嗅探' }));
    expect(screen.getByRole('textbox', { name: '搜索插件' })).toHaveProperty('value', '');
    fireEvent.change(screen.getByRole('textbox', { name: '搜索插件' }), { target: { value: 'saved discovery query' } });
    expect(screen.getByText('暂无自动嗅探插件')).toBeTruthy();
    expect(screen.getByRole('region', { name: '精选' })).toBeTruthy();
    expect(screen.getByRole('region', { name: '其他' })).toBeTruthy();
    expect(screen.queryByRole('link', { name: '开始嗅探' })).toBeNull();
    fireEvent.click(screen.getByRole('tab', { name: '反代直连' }));
    expect(screen.getByRole('textbox', { name: '搜索插件' })).toHaveProperty('value', 'Codex');
    expect(screen.getAllByRole('article')).toHaveLength(1);
    expect(screen.getByRole('article', { name: 'Codex' })).toBeTruthy();
    fireEvent.click(screen.getByRole('tab', { name: '自动嗅探' }));
    expect(screen.getByRole('textbox', { name: '搜索插件' })).toHaveProperty('value', 'saved discovery query');
    expect(pluginFetcher).toHaveBeenCalledTimes(1);
    expect(window.location.hash).toBe('#/plugins');
  });

  it('keeps background refresh inside the fixed button without adding layout rows', async () => {
    let finish!: (value: Response) => void;
    pluginFetcher.mockResolvedValueOnce(response(snapshot)).mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
    const { container } = mount();
    const card = await screen.findByRole('article', { name: 'Codex' });
    const page = container.querySelector('.plugins-page')!;
    const children = Array.from(page.children), paragraphs = page.querySelectorAll('p').length;
    const refresh = screen.getByRole('button', { name: '刷新' });
    fireEvent.click(refresh);
    expect(refresh.getAttribute('aria-busy')).toBe('true');
    expect(refresh.querySelector('.animate-spin')).toBeTruthy();
    expect(Array.from(page.children)).toEqual(children);
    expect(page.querySelectorAll('p')).toHaveLength(paragraphs);
    expect(screen.queryByText('正在刷新插件状态…')).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();
    expect(screen.getByRole('article', { name: 'Codex' })).toBe(card);
    await act(async () => finish(response({ error: '后台核对失败' }, 500)));
    expect(screen.getByRole('alert').textContent).toContain('后台核对失败');
    expect(screen.getByRole('article', { name: 'Codex' })).toBe(card);
    expect(screen.getByRole('switch', { name: 'Codex 反代直连' })).toHaveProperty('disabled', true);
  });

  it('waits for save success and sends exact capability, boolean, and expected version', async () => {
    const initial = { version: 'r1', items: [plugin('sample/id', { capabilities: [{ ...connect, enabled: false }] })] };
    let finish!: (value: Response) => void;
    pluginFetcher.mockResolvedValueOnce(response(initial)).mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
    mount();
    const toggle = await screen.findByRole('switch', { name: 'sample/id 反代直连' });
    fireEvent.click(toggle);
    expect(toggle).toHaveProperty('checked', false);
    expect(toggle).toHaveProperty('disabled', true);
    expect(pluginFetcher).toHaveBeenLastCalledWith('/api/plugins/sample%2Fid/capabilities/connect', expect.objectContaining({ method: 'PUT', body: JSON.stringify({ enabled: true, expected_version: 'r1' }) }));
    await act(async () => finish(response({ version: 'r2', items: [plugin('sample/id')] })));
    expect(toggle).toHaveProperty('checked', true);
    expect(toggle).toHaveProperty('disabled', false);
    expect(screen.getByRole('link', { name: '去接入' })).toBeTruthy();
    fireEvent.click(toggle);
    await waitFor(() => expect(pluginFetcher).toHaveBeenLastCalledWith('/api/plugins/sample%2Fid/capabilities/connect', expect.objectContaining({ body: JSON.stringify({ enabled: false, expected_version: 'r2' }) })));
  });

  it('confirms affected channels and preserves data on cancel before allowing disable', async () => {
    const initial: PluginList = { version: 'v1', items: [plugin('Codex', { capabilities: [{ ...connect, affected_channel_ids: [1, 2] }] })] };
    pluginFetcher.mockResolvedValueOnce(response(initial)).mockResolvedValueOnce(response({ version: 'v2', items: [plugin('Codex', { capabilities: [{ ...connect, enabled: false, affected_channel_ids: [1, 2] }] })] }));
    mount(); const toggle = await screen.findByRole('switch', { name: 'Codex 反代直连' });
    fireEvent.click(toggle);
    const dialog = await screen.findByRole('dialog', { name: '停用 Codex？' });
    expect(within(dialog).getByText(/2 个渠道.*数据会保留/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    await waitFor(() => expect(toggle).toHaveProperty('disabled', false));
    expect(toggle).toHaveProperty('checked', true);
    expect(pluginFetcher).toHaveBeenCalledTimes(1);
    fireEvent.click(toggle);
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '确认停用' }));
    await waitFor(() => expect(toggle).toHaveProperty('checked', false));
    expect(pluginFetcher).toHaveBeenLastCalledWith('/api/plugins/Codex/capabilities/connect', expect.objectContaining({ method: 'PUT', body: JSON.stringify({ enabled: false, expected_version: 'v1' }) }));
    expect(screen.queryByRole('link', { name: '去接入' })).toBeNull();
  });

  it.each([403, 409, 500])('retains old enabled value after %s save error and blocks writes until a successful reread', async status => {
    pluginFetcher.mockResolvedValueOnce(response(snapshot)).mockResolvedValueOnce(response({ error: '操作被拒绝' }, status)).mockResolvedValueOnce(response(snapshot));
    mount(); const toggle = await screen.findByRole('switch', { name: 'Codex 反代直连' });
    fireEvent.click(toggle);
    await screen.findByRole('alert');
    expect(toggle).toHaveProperty('checked', true);
    expect(toggle).toHaveProperty('disabled', true);
    expect(screen.getAllByRole('article')).toHaveLength(3);
    if (status === 409) expect(screen.getByText(/插件状态已变化/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => expect(toggle).toHaveProperty('disabled', false));
    expect(pluginFetcher).toHaveBeenCalledTimes(3);
  });

  it('never clears loaded cards during refresh or read error, including tab changes', async () => {
    let finish!: (value: Response) => void;
    pluginFetcher.mockResolvedValueOnce(response(snapshot)).mockImplementationOnce(() => new Promise(resolve => { finish = resolve; })).mockResolvedValueOnce(response(snapshot));
    mount(); const card = await screen.findByRole('article', { name: 'Codex' });
    fireEvent.click(screen.getByRole('button', { name: '刷新' }));
    expect(screen.getByRole('article', { name: 'Codex' })).toBe(card);
    expect(screen.getByRole('switch', { name: 'Codex 反代直连' })).toHaveProperty('disabled', true);
    await act(async () => finish(response({ error: '无读取权限' }, 403)));
    expect(screen.getByRole('alert').textContent).toContain('无读取权限');
    expect(screen.getByRole('article', { name: 'Codex' })).toBe(card);
    fireEvent.click(screen.getByRole('tab', { name: '自动嗅探' }));
    fireEvent.click(screen.getByRole('tab', { name: '反代直连' }));
    expect(screen.getByRole('switch', { name: 'Codex 反代直连' })).toHaveProperty('disabled', true);
    expect(pluginFetcher).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => expect(screen.getByRole('switch', { name: 'Codex 反代直连' })).toHaveProperty('disabled', false));
  });

  it('shows unavailable reasons and only allows discovery navigation for enabled available capabilities', async () => {
    history.replaceState(null, '', '/#/plugins?category=discover_api_key&plugin=source%2Fone');
    pluginFetcher.mockResolvedValue(response({ version: 'v1', items: [
      plugin('source/one', { implementation: 'compatible', capabilities: [{ ...connect, id: 'discover_api_key' }] }),
      plugin('Unavailable', { capabilities: [{ ...connect, id: 'discover_api_key', available: false, unavailable_reason: '当前系统不支持' }] }),
    ] }));
    mount();
    const target = await screen.findByRole('article', { name: 'source/one' });
    expect(screen.getByRole('tab', { name: '自动嗅探' }).getAttribute('aria-selected')).toBe('true');
    expect(document.activeElement).toBe(target);
    expect(target.getAttribute('aria-current')).toBe('true');
    expect(within(target).getByText('兼容')).toBeTruthy();
    expect(screen.getByRole('link', { name: '开始嗅探' }).getAttribute('href')).toBe('#/channels?discover=1&source=source%2Fone');
    expect(screen.getByText('当前系统不支持')).toBeTruthy();
    const unavailable = screen.getByRole('switch', { name: 'Unavailable 自动嗅探' });
    // An unavailable but previously enabled capability must still be stoppable.
    expect(unavailable).toHaveProperty('disabled', false);
    fireEvent.click(unavailable);
    await waitFor(() => expect(pluginFetcher).toHaveBeenCalledTimes(2));
    expect(JSON.parse(String(pluginFetcher.mock.calls[1][1]?.body))).toEqual({ enabled: false, expected_version: 'v1' });
  });

  it('aborts stale reads and does not reuse a prior authenticated page snapshot after remount', async () => {
    let finish!: (value: Response) => void;
    pluginFetcher.mockResolvedValueOnce(response(snapshot)).mockImplementationOnce(() => new Promise(resolve => { finish = resolve; })).mockResolvedValueOnce(response({ items: [], version: 'new-session' }));
    const first = mount(); await screen.findByRole('article', { name: 'Codex' });
    fireEvent.click(screen.getByRole('button', { name: '刷新' }));
    const signal = pluginFetcher.mock.calls[1][1]?.signal;
    first.unmount(); mount();
    expect(signal?.aborted).toBe(true);
    await screen.findByText('暂无反代直连插件');
    await act(async () => finish(response(snapshot)));
    expect(screen.queryByRole('article')).toBeNull();
  });
});
