// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import Plugins from './Plugins';
import { ConfirmProvider } from '../components/Confirm';
import type { MarketCatalog, MarketItem, PluginList } from '../types/plugins';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const marketType = 'market:0123456789abcdef0123456789abcdef:fixture';
const item = (id: string, community = false): MarketItem => ({ id, name: id, package: `fixture-${id}`, description: `${id} 供应商`, icon: '', community, version: '2.0', installed: false, update_available: false, providers: ['fixture'], installed_providers: [] });
const provider = { id: 'fixture', name: 'Fixture Supplier', credential_type: marketType as `market:${string}`, api: '', npm: '', methods: [] };

it('finds an installed supplier by its provider name even when package metadata does not match', async () => {
  const supplier = { ...item('saved package'), providers: [], installed: true, installed_version: '1.0', installed_providers: [provider] };
  setup({ items: [supplier], refreshing: false }, undefined, url => url.startsWith('/api/plugins/market?q=') ? Promise.resolve(json({items:[supplier],refreshing:false})) : undefined);
  await screen.findByRole('article', { name: supplier.name });
  fireEvent.change(screen.getByRole('textbox', {name:'搜索插件'}), {target:{value:'Fixture Supplier'}});
  await waitFor(()=>expect(screen.getByRole('article', {name:supplier.name})).toBeTruthy());
});
function setup(catalog: MarketCatalog, plugins?: PluginList, override?: (url: string, init?: RequestInit) => Promise<Response> | undefined) {
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    const custom = override?.(url, init); if (custom) return custom;
    if (url === '/api/plugins') return Promise.resolve(json(plugins || { version: 'fixture', items: catalog.items.filter(value => value.installed).flatMap(value => value.installed_providers.map(provider => ({ id: provider.credential_type, credential_type: provider.credential_type, name: provider.name, description: '', package: value.package, icon: '', implementation: 'compatible', featured: false, version: value.installed_version || '', capabilities: [{ id: 'connect', enabled: true, available: true, affected_channel_ids: [] }] }))) }));
    if (url === '/api/plugins/market') return Promise.resolve(json(catalog));
    if (url === '/api/plugins/market/refresh') return Promise.resolve(json({}));
    throw new Error(`Unexpected request ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ConfirmProvider><Plugins /></ConfirmProvider>);
  return fetcher;
}
beforeEach(() => {
  history.replaceState(null, '', '/#/plugins');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

it('places community suppliers first in Other, searches providers and suppresses duplicate installed plugin cards', async () => {
  const community = { ...item('Z community', true), installed: true, installed_providers: [provider] };
  setup({ items: [item('A independent'), community], refreshing: false }, { version: 'fixture', items: [{ id: marketType, credential_type: marketType, name: 'Duplicate provider', description: '', package: community.package, icon: '', implementation: 'compatible', featured: false, version: '1.0', capabilities: [{ id: 'connect', enabled: true, available: true, affected_channel_ids: [] }] }] });
  const section = await screen.findByRole('region', { name: '其他' });
  await screen.findByRole('article', { name: community.name });
  expect(within(section).getAllByRole('article').map(card => card.getAttribute('aria-label'))).toEqual(['Z community', 'A independent']);
  expect(within(screen.getByRole('article', { name: community.name })).getByText('Magpie社区')).toBeTruthy();
  expect(screen.queryByRole('article', { name: 'Duplicate provider' })).toBeNull();
  expect(screen.getByRole('link', { name: '去接入' }).getAttribute('href')).toBe(`#/channels?plugin=${encodeURIComponent(marketType)}`);
  fireEvent.change(screen.getByRole('textbox', { name: '搜索插件' }), { target: { value: 'fixture' } });
  expect(within(section).getAllByRole('article')).toHaveLength(2);
  fireEvent.change(screen.getByRole('textbox', { name: '搜索插件' }), { target: { value: 'independent' } });
  expect(within(section).getAllByRole('article')).toHaveLength(1);
  fireEvent.click(screen.getByRole('tab', { name: '自动嗅探' }));
  expect(screen.queryByText('Magpie社区')).toBeNull();
});

it('keeps cached cards visible while installing and reloads installed state without starting authorization', async () => {
  const supplier = item('supplier', true), catalog = { items: [supplier], refreshing: false };
  let finish!: (value: Response) => void;
  const fetcher = setup(catalog, undefined, (url, init) => url === '/api/plugins/market/install' && init?.method === 'POST' ? new Promise(resolve => { finish = resolve; }) : undefined);
  const card = await screen.findByRole('article', { name: supplier.name });
  fireEvent.click(within(card).getByRole('button', { name: '安装' }));
  expect(screen.getByRole('article', { name: supplier.name })).toBe(card);
  expect(within(card).getByRole('button', { name: '正在安装…' })).toHaveProperty('disabled', true);
  expect(fetcher).toHaveBeenCalledWith('/api/plugins/market/install', expect.objectContaining({ method: 'POST', body: JSON.stringify({ package: supplier.package }) }));
  supplier.installed = true; supplier.installed_version = '2.0'; supplier.installed_providers = [provider];
  await act(async () => finish(json(supplier)));
  await within(card).findByRole('link', { name: '去接入' });
  expect(within(card).getByText('已安装 2.0')).toBeTruthy();
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(fetcher.mock.calls.some(([url]) => url.includes('/auth/'))).toBe(false);
});

it('retains the installed version after update failure and only removes activation after a short confirmation', async () => {
  const supplier = { ...item('supplier'), installed: true, installed_version: '1.0', update_available: true, installed_providers: [provider] };
  const catalog = { items: [supplier], refreshing: false };
  const fetcher = setup(catalog, undefined, (url, init) => {
    if (url === '/api/plugins/market/install') return Promise.resolve(json({ error: '下载失败，保留原版本' }, 500));
    if (url === `/api/plugins/market/${supplier.id}` && init?.method === 'DELETE') { supplier.installed = false; supplier.installed_providers = []; return Promise.resolve(json({ ok: true })); }
  });
  const card = await screen.findByRole('article', { name: supplier.name });
  fireEvent.click(within(card).getByRole('button', { name: '更新' }));
  await screen.findByRole('alert');
  expect(within(card).getByText('已安装 1.0')).toBeTruthy();
  fireEvent.click(within(card).getByRole('button', { name: '卸载 supplier' }));
  const dialog = await screen.findByRole('dialog', { name: '卸载 supplier？' });
  expect(within(dialog).getByText(/渠道、账号与配置会保留/)).toBeTruthy();
  fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
  await waitFor(() => expect(within(card).getByRole('button', { name: '卸载 supplier' })).toHaveProperty('disabled', false));
  expect(fetcher.mock.calls.some(([, init]) => init?.method === 'DELETE')).toBe(false);
  fireEvent.click(within(card).getByRole('button', { name: '卸载 supplier' }));
  fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '卸载' }));
  await within(card).findByRole('button', { name: '安装' });
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'DELETE')).toHaveLength(1);
});

it('keeps the cached supplier visible when its background catalogue update fails', async () => {
  vi.useFakeTimers();
  let reads = 0;
  setup({ items: [item('cached supplier')], refreshing: true }, undefined, url => {
    if (url === '/api/plugins/market' && ++reads > 1) return Promise.resolve(json({ error: '目录更新失败' }, 500));
  });
  await act(async () => {});
  const card = screen.getByRole('article', { name: 'cached supplier' });
  await act(async () => { vi.advanceTimersByTime(2000); });
  expect(screen.getByRole('alert').textContent).toContain('目录更新失败');
  expect(screen.getByRole('article', { name: 'cached supplier' })).toBe(card);
  expect(screen.getByRole('button', { name: '刷新' }).getAttribute('aria-busy')).toBe('false');
  expect(screen.getByRole('button', { name: '刷新' }).querySelector('.animate-spin')).toBeNull();
  await act(async () => { vi.advanceTimersByTime(6000); });
  expect(reads).toBe(2);
});

it('preserves a disabled supplier after reinstall and enables or disables it with the latest plugin version', async () => {
  const supplier = { ...item('supplier', true), installed: false, installed_version: '', installed_providers: [] as MarketItem['installed_providers'] };
  const plugins: PluginList = { version: 'v1', items: [{ id: marketType, credential_type: marketType, name: provider.name, description: '', package: supplier.package, icon: '', implementation: 'compatible', featured: false, version: '1', capabilities: [{ id: 'connect', enabled: false, available: false, affected_channel_ids: [] }] }] };
  const fetcher = setup({ items: [supplier], refreshing: false }, plugins, (url, init) => {
    if (url === '/api/plugins/market/install') { plugins.version = 'v2'; supplier.installed = true; supplier.installed_providers = [provider]; supplier.installed_version = '2'; plugins.items[0].capabilities[0].available = true; return Promise.resolve(json(supplier)); }
    if (init?.method === 'PUT') {
      const body = JSON.parse(String(init.body));
      expect(body.expected_version).toBe(plugins.version);
      plugins.items[0].capabilities[0].enabled = body.enabled;
      plugins.version = body.enabled ? 'v3' : 'v4';
      return Promise.resolve(json(plugins));
    }
  });
  const card = await screen.findByRole('article', { name: supplier.name });
  expect(within(card).queryByRole('switch')).toBeNull();
  fireEvent.click(within(card).getByRole('button', { name: '安装' }));
  await within(card).findByText('已安装 2');
  const toggle = within(card).getByRole('switch', { name: 'Fixture Supplier 反代直连' });
  expect(toggle).toHaveProperty('checked', false);
  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).toHaveProperty('checked', true));
  expect(within(card).getByRole('link', { name: '去接入' })).toBeTruthy();
  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).toHaveProperty('checked', false));
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT').map(([, init]) => JSON.parse(String(init?.body)))).toEqual([{ enabled: true, expected_version: 'v2' }, { enabled: false, expected_version: 'v3' }]);
});

it('controls each provider of a multi-region package independently', async () => {
  const second = { ...provider, id: 'fixture-cn', name: 'Fixture CN', credential_type: `${marketType}-cn` as `market:${string}` };
  const supplier = { ...item('supplier', true), installed: true, installed_providers: [provider, second] };
  const plugins: PluginList = { version: 'v1', items: [provider, second].map(value => ({ id: value.credential_type, credential_type: value.credential_type, name: value.name, description: '', package: supplier.package, icon: '', implementation: 'compatible', featured: false, version: '1', capabilities: [{ id: 'connect', enabled: true, available: true, affected_channel_ids: [] }] })) };
  const fetcher = setup({ items: [supplier], refreshing: false }, plugins, (url, init) => {
    if (init?.method !== 'PUT') return;
    expect(url).toBe(`/api/plugins/${encodeURIComponent(second.credential_type)}/capabilities/connect`);
    plugins.items[1].capabilities[0].enabled = false;
    plugins.version = 'v2'; return Promise.resolve(json(plugins));
  });
  const card = await screen.findByRole('article', { name: supplier.name });
  expect(within(card).getAllByRole('link', { name: '去接入' })).toHaveLength(2);
  fireEvent.click(within(card).getByRole('switch', { name: 'Fixture CN 反代直连' }));
  await waitFor(() => expect(within(card).getByRole('switch', { name: 'Fixture CN 反代直连' })).toHaveProperty('checked', false));
  expect(within(card).getByRole('switch', { name: 'Fixture Supplier 反代直连' })).toHaveProperty('checked', true);
  expect(within(card).getAllByRole('link', { name: '去接入' })).toHaveLength(1);
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
});

it('forces an immediate catalogue refresh and retries it after failure without waiting for the background TTL', async () => {
  const catalog: MarketCatalog = { items: [item('cached supplier')], refreshing: false };
  let attempts = 0;
  const fetcher = setup(catalog, undefined, (url, init) => {
    if (url !== '/api/plugins/market/refresh') return;
    expect(init?.method).toBe('POST');
    if (++attempts === 1) return Promise.resolve(json({ error: '暂时离线' }, 500));
    catalog.items = [item('fresh supplier')];
    return Promise.resolve(json({}));
  });
  const card = await screen.findByRole('article', { name: 'cached supplier' });
  expect(fetcher.mock.calls.some(([url]) => url.endsWith('/refresh'))).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: '刷新' }));
  await screen.findByRole('alert');
  expect(screen.getByRole('article', { name: 'cached supplier' })).toBe(card);
  fireEvent.click(screen.getByRole('button', { name: '重试' }));
  await screen.findByRole('article', { name: 'fresh supplier' });
  expect(attempts).toBe(2);
  const marketCalls = fetcher.mock.calls.filter(([url]) => url.startsWith('/api/plugins/market')).map(([url]) => url);
  expect(marketCalls).toEqual(['/api/plugins/market', '/api/plugins/market/refresh', '/api/plugins/market/refresh', '/api/plugins/market']);
});

it('debounces remote supplier search and aborts stale results while keeping successful catalogue data', async () => {
  vi.useFakeTimers();
  let finishOld!: (value: Response) => void;
  const fetcher = setup({ items: [item('cached supplier')], refreshing: false }, undefined, url => {
    if (url === '/api/plugins/market?q=remote') return new Promise(resolve => { finishOld = resolve; });
    if (url === '/api/plugins/market?q=remote-new') return Promise.resolve(json({ items: [item('remote-new supplier')], refreshing: false }));
  });
  await act(async () => {});
  const search = screen.getByRole('textbox', { name: '搜索插件' });
  fireEvent.change(search, { target: { value: 'rem' } });
  await act(async () => { vi.advanceTimersByTime(150); });
  expect(fetcher.mock.calls.some(([url]) => url.includes('?q='))).toBe(false);
  fireEvent.change(search, { target: { value: 'remote' } });
  await act(async () => { vi.advanceTimersByTime(300); });
  const oldSignal = fetcher.mock.calls.find(([url]) => url === '/api/plugins/market?q=remote')?.[1]?.signal;
  fireEvent.change(search, { target: { value: 'remote-new' } });
  await act(async () => { vi.advanceTimersByTime(300); });
  expect(oldSignal?.aborted).toBe(true);
  expect(screen.getByRole('article', { name: 'remote-new supplier' })).toBeTruthy();
  await act(async () => finishOld(json({ items: [item('remote obsolete')], refreshing: false })));
  expect(screen.getByRole('article', { name: 'remote-new supplier' })).toBeTruthy();
  expect(screen.queryByRole('article', { name: 'remote obsolete' })).toBeNull();
});

it('keeps manual update available for GitHub suppliers whose manifest version is unchanged', async () => {
  const supplier = { ...item('github supplier'), package: 'github:fixture/supplier', installed: true, installed_version: '2.0', update_available: false, installed_providers: [provider] };
  const fetcher = setup({ items: [supplier], refreshing: false }, undefined, url => url === '/api/plugins/market/install' ? Promise.resolve(json(supplier)) : undefined);
  const card = await screen.findByRole('article', { name: supplier.name });
  fireEvent.click(within(card).getByRole('button', { name: '更新' }));
  await waitFor(() => expect(within(card).getByRole('button', { name: '更新' })).toHaveProperty('disabled', false));
  expect(fetcher).toHaveBeenCalledWith('/api/plugins/market/install', expect.objectContaining({ method: 'POST', body: JSON.stringify({ package: 'github:fixture/supplier' }) }));
  expect(within(card).getByText('已安装 2.0')).toBeTruthy();
});
