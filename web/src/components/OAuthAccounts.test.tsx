// @vitest-environment jsdom
import { ConfirmProvider } from './Confirm';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Channel } from '../api';
import { OAuthAccounts } from './OAuthAccounts';
import { Modal } from './ui';
import { NavigationGuard, type BeforeLeave } from './NavigationGuard';
import * as runtimeStatus from '../hooks/useRuntimeStatus';
import { enabledPlugins } from '../testFixtures/plugins';

// File decoding and the builtin/upload choices belong to ChannelIconPicker's
// tests; this seam verifies OAuth navigation persists the selected icon.
vi.mock('./ChannelIconPicker', () => ({
  ChannelIconPicker: ({ name, onSelect, onClose }: { name: string; onSelect: (value: string) => Promise<void>; onClose: () => void }) => <section role="dialog" aria-label={`测试图标选择器 ${name}`}><button onClick={async () => { await onSelect('openai'); onClose(); }}>选择测试图标</button></section>,
}));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const oauth = (label: string) => ({ provider: 'codex', account_label: label, status: 'active' as const, expires_at: '', updated_at: '2026-10-17T00:00:00Z' });
const channel = (id: number, credentials = ['one', 'two']): Channel => ({
  id, name: `Channel ${id}`, api_key: '', has_key: false, has_oauth: true, enabled: true, models: ['model-a'], endpoints: [], credential_type: 'codex_oauth',
  credentials: credentials.map(credential => ({ id: credential, label: 'Same label', enabled: true, has_oauth: true, oauth_info: oauth(`${credential}@example.invalid`) })),
});
const providers = [
  { type: 'copilot_oauth' as const, title: 'GitHub Copilot', note: 'Copilot', official: true },
  { type: 'chatgpt_oauth' as const, title: 'ChatGPT 官方', note: 'ChatGPT', official: true },
  { type: 'codex_oauth' as const, title: 'Codex 社区', note: '非官方', official: false },
  { type: 'plugin:workbuddy' as const, title: 'WorkBuddy', note: '兼容接入', official: false },
  { type: 'plugin:workbuddy-ai' as const, title: 'WorkBuddy AI', note: '兼容接入', official: false },
  { type: 'plugin:minimax-code' as const, title: 'MiniMax Code', note: '兼容接入', official: false },
  { type: 'plugin:minimax-code-global' as const, title: 'MiniMax Code Global', note: '兼容接入', official: false },
];
const quota = (used: number) => ({ ok: true, captured_at: '2026-10-17T00:00:00Z', refresh_seconds: 0, buckets: [{ id: 'rate_limit', general: true, primary: { used_percent: used, window_seconds: 18000 } }] });
function setup(rows: Channel[] = [], override?: (url: string, init?: RequestInit) => Promise<Response> | undefined) {
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    const custom = override?.(url, init); if (custom) return custom;
    if (url === '/api/settings/quota-refresh' && !init?.method) return Promise.resolve(json({ quota_refresh_seconds: 300 }));
    if (url.startsWith('/api/oauth/codex/usage')) {
      const params = new URL(url, 'http://localhost').searchParams;
      const used = Number(params.get('channel_id')) === 8 ? 80 : params.get('credential_id') === 'two' ? 60 : 10;
      return Promise.resolve(json(quota(used)));
    }
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  const callbacks = { onFlowStarted: vi.fn(), onReauthorize: vi.fn(), onEdit: vi.fn(), onAdded: vi.fn(), onCredentialOrderSaved: vi.fn(), onTest: vi.fn(), onToggle: vi.fn(), onDelete: vi.fn() };
  const pluginAccess = Object.fromEntries(providers.map(provider => [provider.type, { allowed: true, enabled: true, pluginID: enabledPlugins.items.find(plugin => plugin.credential_type === provider.type)?.id }]));
  const props = { pluginAccess, pools: providers.map(provider => ({ provider, channels: provider.type === 'codex_oauth' ? rows : [] })), loading: false, flows: {}, busy: null, ...callbacks };
  const view = render(<OAuthAccounts {...props} />);
  return { ...view, props, callbacks, fetcher };
}
const chooseCodex = () => fireEvent.click(screen.getByRole('tab', { name: /Codex/ }));
const manage = () => { fireEvent.click(screen.getByRole('button', { name: '管理接入方式' })); return screen.getByRole('dialog', { name: '管理接入方式' }); };
const quotaCalls = (fetcher: ReturnType<typeof setup>['fetcher']) => fetcher.mock.calls.filter(([url]) => url.startsWith('/api/oauth/codex/usage'));

it('opens the shared credential dialog in the selected second channel without starting authorization', () => {
  const view = setup([channel(7), channel(8)]); chooseCodex();
  expect(screen.queryByRole('button', { name: '导入凭证' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '向 Channel 8 添加账号 / 工作区' }));
  expect(screen.getByRole('dialog', { name: '向「Channel 8」添加账号 / 工作区' })).toBeTruthy();
  fireEvent.click(screen.getByRole('tab', { name: '粘贴凭证' }));
  expect(screen.getByLabelText('凭证文件')).toBeTruthy();
  expect(screen.queryByLabelText('添加到')).toBeNull();
  expect(view.fetcher.mock.calls.some(([url]) => url.includes('/oauth-transfer') || url.includes('/start'))).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  expect(screen.queryByRole('dialog', { name: /向「Channel 8」/ })).toBeNull();
});

it('exports precisely one selected credential from its batch toolbar and hides account identity in the export dialog', async () => {
  const view = setup([channel(7)], url => url === '/api/oauth-transfer/export' ? Promise.resolve(json({ file: '{"encrypted":"synthetic"}', filename: 'credentials.json', count: 1 })) : undefined);
  vi.stubGlobal('URL', class extends URL { static createObjectURL = vi.fn(() => 'blob:synthetic'); static revokeObjectURL = vi.fn(); });
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
  chooseCodex();
  const account = within(screen.getByRole('region', { name: '渠道 Channel 7' }));
  fireEvent.click(account.getAllByRole('checkbox', { name: '选择凭证 Same label' })[1]);
  fireEvent.click(account.getByRole('button', { name: '导出凭证' }));
  const dialog = within(screen.getByRole('dialog', { name: '导出凭证' }));
  expect(dialog.getByText('Same label')).toBeTruthy();
  expect(dialog.queryByText('two@example.invalid')).toBeNull();
  fireEvent.click(dialog.getByRole('button', { name: '下载凭证' }));
  await dialog.findByText(/已下载 1 个凭证/);
  const sent = view.fetcher.mock.calls.find(([url]) => url === '/api/oauth-transfer/export')!;
  expect(JSON.parse(String(sent[1]!.body))).toEqual({ refs: [{ channel_id: 7, credential_id: 'two' }] });
  expect(HTMLAnchorElement.prototype.click).toHaveBeenCalledTimes(1);
});

it('offers neither import nor encrypted export for public Zen Free credentials', async () => {
  const view = setup([]);
  const zen: Channel = { ...channel(9, ['public']), credential_type: 'zen_free', has_oauth: false, has_key: true, credentials: [{ id: 'public', label: '公共凭证', enabled: true, has_key: true }] };
  view.rerender(<OAuthAccounts {...view.props} initialProvider="zen_free" pools={[...view.props.pools, { provider: { type: 'zen_free', title: 'Zen Free', note: '公共接入', official: false }, channels: [zen] }]} />);
  await waitFor(() => expect(screen.getByRole('tab', { name: 'Zen Free' }).getAttribute('aria-selected')).toBe('true'));
  expect(screen.queryByRole('button', { name: '导入凭证' })).toBeNull();
  fireEvent.click(screen.getByRole('checkbox', { name: '选择凭证 公共凭证' }));
  expect(screen.queryByRole('button', { name: '导出凭证' })).toBeNull();
});

it('shows the newly supported compatibility account tabs', () => {
  setup([]);
  for (const title of ['WorkBuddy', 'WorkBuddy AI', 'MiniMax Code', 'MiniMax Code Global']) expect(screen.getByRole('tab', { name: title })).toBeTruthy();
});

function measureSortable(list: HTMLElement, horizontal = false) {
  const items = Array.from(list.children).filter((node): node is HTMLElement => node instanceof HTMLElement && node.hasAttribute('data-sortable-item'));
  const rect = (index: number, length = 1) => ({ x: horizontal ? index * 100 : 0, y: horizontal ? 0 : index * 100, left: horizontal ? index * 100 : 0, top: horizontal ? 0 : index * 100, right: horizontal ? (index + length) * 100 : 300, bottom: horizontal ? 100 : (index + length) * 100, width: horizontal ? length * 100 : 300, height: horizontal ? 100 : length * 100, toJSON() {} });
  vi.spyOn(list, 'getBoundingClientRect').mockImplementation(() => rect(0, items.length) as DOMRect);
  items.forEach((item, index) => vi.spyOn(item, 'getBoundingClientRect').mockImplementation(() => rect(index) as DOMRect));
}
function pointerMove(handle: HTMLElement, target: HTMLElement, horizontal = false) {
  const source = handle.closest<HTMLElement>('[data-sortable-item]')!;
  const list = source.parentElement!;
  measureSortable(list, horizontal);
  const from = source.getBoundingClientRect(), to = target.getBoundingClientRect();
  const start = { clientX: horizontal ? from.left + 50 : 50, clientY: horizontal ? 50 : from.top + 50, pointerId: 1, button: 0 };
  const end = { ...start, clientX: horizontal ? to.left + (to.left < from.left ? 25 : 75) : 50, clientY: horizontal ? 50 : to.top + (to.top < from.top ? 25 : 75) };
  fireEvent.pointerDown(handle, start);
  fireEvent.pointerMove(window, end);
  return () => fireEvent.pointerUp(window, end);
}

it.each(['mouse', 'touch'])('moves only the grabbed member by %s into an empty collapsed channel, and cancellation never saves', async pointerType => {
  let finish!: (value: Response) => void;
  const source = { ...channel(7), generation: 4 }, target = { ...channel(8, []), generation: 9 };
  source.credentials!.forEach((credential, index) => { credential.label = `Note ${index + 1}`; });
  const view = setup([source, target], (url, init) => url === '/api/channels/7/credentials/move' && init?.method === 'POST' ? new Promise(resolve => { finish = resolve; }) : undefined);
  chooseCodex(); await screen.findByText('剩余 90%');
  fireEvent.click(screen.getByRole('button', { name: '收起 Channel 8 凭证区' }));
  const destination = screen.getByRole('region', { name: '渠道 Channel 8' });
  vi.spyOn(destination, 'getBoundingClientRect').mockReturnValue({ x: 0, y: 300, left: 0, top: 300, right: 300, bottom: 400, width: 300, height: 100, toJSON() {} } as DOMRect);
  const list = screen.getByLabelText('Channel 7 凭证列表'); measureSortable(list);
  const handle = screen.getByRole('button', { name: '拖动凭证 Note 1' });
  const press = { clientX: 50, clientY: 50, pointerId: 1, button: 0, pointerType };
  const over = { ...press, clientY: 350 };
  fireEvent.pointerDown(handle, press); fireEvent.pointerMove(window, over);
  expect(destination.classList.contains('is-drop-target')).toBe(true);
  expect(within(destination).getByText('Note 1 · 移到「Channel 8」')).toBeTruthy();
  fireEvent.keyDown(window, { key: 'Escape' }); fireEvent.pointerUp(window, over);
  expect(view.fetcher.mock.calls.some(([url]) => url.endsWith('/credentials/move'))).toBe(false);
  expect(destination.classList.contains('is-drop-target')).toBe(false);
  fireEvent.pointerDown(handle, press); fireEvent.pointerMove(window, { ...press, clientX: 700, clientY: 650 }); fireEvent.pointerUp(window, { ...press, clientX: 700, clientY: 650 });
  expect(view.fetcher.mock.calls.some(([url]) => url.endsWith('/credentials/move'))).toBe(false);
  fireEvent.click(screen.getByRole('checkbox', { name: '选择凭证 Note 2' }));
  fireEvent.pointerDown(handle, press); fireEvent.pointerMove(window, over); fireEvent.pointerUp(window, over); fireEvent.pointerUp(window, over);
  const moves = view.fetcher.mock.calls.filter(([url]) => url.endsWith('/credentials/move'));
  expect(moves).toHaveLength(1);
  expect(JSON.parse(String(moves[0][1]?.body))).toEqual({ target_channel_id: 8, credential_ids: ['one'], expected_generation: 4, expected_target_generation: 9 });
  expect(screen.getByRole('article', { name: '凭证 Note 1' })).toBeTruthy();
  expect(view.callbacks.onAdded).not.toHaveBeenCalled();
  await act(async () => finish(json({ source: { ...source, credentials: source.credentials!.slice(1) }, target: { ...target, credentials: source.credentials!.slice(0, 1) } })));
  expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1);
  view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'codex_oauth' ? [{ ...source, credentials: source.credentials!.slice(1) }, { ...target, credentials: source.credentials!.slice(0, 1) }] : pool.channels }))} />);
  expect(within(screen.getByRole('region', { name: '渠道 Channel 7' })).queryByRole('article', { name: '凭证 Note 1' })).toBeNull();
});

it('moves exact members through the keyboard-accessible menu and preserves both lists after a failed save', async () => {
  const source = { ...channel(7), generation: 4 }, target = { ...channel(8, []), generation: 9 };
  const other = { ...channel(9, []), credential_type: 'copilot_oauth' as const };
  let attempts = 0;
  const view = setup([source, target, other], url => url.endsWith('/credentials/move') ? Promise.resolve(++attempts === 1 ? json({ error: 'synthetic internal details' }, 409) : json({ source, target })) : undefined);
  chooseCodex();
  const origin = within(screen.getByRole('region', { name: '渠道 Channel 7' }));
  const summary = origin.getAllByLabelText('更多账号管理 Same label')[1];
  summary.focus(); fireEvent.click(summary);
  fireEvent.click(within(summary.closest('details')!).getByRole('button', { name: '移到其他渠道' }));
  const dialog = within(screen.getByRole('dialog', { name: '移到其他渠道' }));
  expect(dialog.queryByRole('option', { name: 'Channel 9' })).toBeNull();
  fireEvent.change(dialog.getByLabelText('移动目标渠道'), { target: { value: '8' } });
  fireEvent.click(dialog.getByRole('button', { name: '确认移动' }));
  await dialog.findByRole('alert');
  expect(origin.getAllByRole('article')).toHaveLength(2);
  expect(screen.queryByText('synthetic internal details')).toBeNull(); expect(view.callbacks.onAdded).not.toHaveBeenCalled();
  fireEvent.click(dialog.getByRole('button', { name: '确认移动' }));
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1));
  const moves = view.fetcher.mock.calls.filter(([url]) => url.endsWith('/credentials/move'));
  expect(moves).toHaveLength(2); expect(moves[0][1]?.body).toBe(moves[1][1]?.body);
  expect(JSON.parse(String(moves[0][1]?.body))).toEqual({ target_channel_id: 8, credential_ids: ['two'], expected_generation: 4, expected_target_generation: 9 });
});

it('keeps batch moving separate from dragging and sends selected members in source order', async () => {
  const source = { ...channel(7, ['one', 'two', 'three']), generation: 6 }, target = { ...channel(8, []), generation: 10 };
  const view = setup([source, target], url => url.endsWith('/credentials/move') ? Promise.resolve(json({ source, target })) : undefined);
  chooseCodex(); const origin = within(screen.getByRole('region', { name: '渠道 Channel 7' }));
  fireEvent.click(origin.getAllByRole('checkbox', { name: '选择凭证 Same label' })[2]);
  fireEvent.click(origin.getAllByRole('checkbox', { name: '选择凭证 Same label' })[0]);
  fireEvent.click(origin.getByRole('button', { name: '移动选中凭证' }));
  const dialog = within(screen.getByRole('dialog', { name: '移到其他渠道' }));
  fireEvent.change(dialog.getByLabelText('移动目标渠道'), { target: { value: '8' } });
  fireEvent.click(dialog.getByRole('button', { name: '确认移动' }));
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1));
  const move = view.fetcher.mock.calls.find(([url]) => url.endsWith('/credentials/move'))!;
  expect(JSON.parse(String(move[1]?.body))).toEqual({ target_channel_id: 8, credential_ids: ['one', 'three'], expected_generation: 6, expected_target_generation: 10 });
});

beforeEach(() => {
  vi.stubGlobal('PointerEvent', class extends MouseEvent {
    pointerId: number; isPrimary = true; pointerType = 'mouse';
    constructor(type: string, init: PointerEventInit = {}) { super(type, init); this.pointerId = init.pointerId ?? 1; this.pointerType = init.pointerType || 'mouse'; }
  });
  vi.stubGlobal('requestAnimationFrame', vi.fn(() => 1));
  vi.stubGlobal('cancelAnimationFrame', vi.fn());
  localStorage.clear(); sessionStorage.clear();
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('allows canceling an existing authorization after its plugin disables without resuming polling', async () => {
  const view = setup([channel(7)], url => url.startsWith('/api/oauth/codex/cancel?') ? Promise.resolve(json({ ok: true })) : undefined);
  view.rerender(<OAuthAccounts {...view.props} pluginAccess={{ ...view.props.pluginAccess, codex_oauth: { allowed: false, enabled: false, pluginID: 'codex', message: '接入插件未启用，账号与配置已保留' } }} initialProvider="codex_oauth" flows={{ 'channel:7': 'fixture-flow' }} />);
  fireEvent.click(screen.getByRole('button', { name: '向 Channel 7 添加账号 / 工作区' }));
  fireEvent.click(screen.getByRole('button', { name: '取消授权' }));
  await waitFor(() => expect(view.fetcher).toHaveBeenCalledWith('/api/oauth/codex/cancel?flow=fixture-flow', expect.objectContaining({ method: 'POST' })));
  expect(view.callbacks.onFlowStarted).toHaveBeenCalledWith('channel:7', null);
  expect(view.fetcher.mock.calls.some(([url]) => /\/status|\/start/.test(url))).toBe(false);
});

it('keeps credentials with equal labels isolated within and across channels', async () => {
  const rows = [channel(7), channel(8, ['one'])].map(row => ({ ...row, credentials: row.credentials!.map(credential => ({ ...credential, oauth_info: oauth('same-account@example.invalid') })) }));
  const { fetcher } = setup(rows); chooseCodex();
  await screen.findByText('剩余 90%');
  expect(screen.getByText('剩余 40%')).toBeTruthy(); expect(screen.getByText('剩余 20%')).toBeTruthy();
  const first = within(screen.getByRole('region', { name: '渠道 Channel 7' })).getAllByRole('article', { name: '凭证 Same label' });
  expect(within(first[0]).getByText('剩余 90%')).toBeTruthy(); expect(within(first[1]).getByText('剩余 40%')).toBeTruthy();
  expect(within(screen.getByRole('region', { name: '渠道 Channel 8' })).getByText('剩余 20%')).toBeTruthy();
  expect(quotaCalls(fetcher)).toHaveLength(3);
  expect(quotaCalls(fetcher).map(([url]) => { const p = new URL(url, 'http://localhost').searchParams; return [p.get('channel_id'), p.get('credential_id')]; })).toEqual([['7', 'one'], ['7', 'two'], ['8', 'one']]);
});

it('uses the explicit general bucket and keeps an extra rate_limit bucket in details', async () => {
  const warning = vi.spyOn(console, 'error').mockImplementation(() => {});
  setup([channel(7, ['one'])], url => url.startsWith('/api/oauth/codex/usage') ? Promise.resolve(json({
    ...quota(11), buckets: [
      { id: 'vendor-main', general: true, name: 'Vendor root', primary: { used_percent: 11, window_seconds: 18000 } },
      { id: 'rate_limit', name: 'Named extra', primary: { used_percent: 99, window_seconds: 3600 } },
      { id: 'rate_limit', name: 'Second extra', primary: { used_percent: 80, window_seconds: 86400 } },
    ],
  })) : undefined);
  chooseCodex();
  const row = await screen.findByRole('article', { name: '凭证 Same label' });
  await within(row).findByText('剩余 89%');
  expect(within(row).queryByText('剩余 1%')).toBeNull();
  expect(within(row).queryByText('Named extra')).toBeNull();
  fireEvent.click(within(row).getByRole('button', { name: '额度详情 Same label' }));
  const dialog = screen.getByRole('dialog', { name: '凭证详情 · Same label' });
  expect(within(dialog).getByText('Named extra')).toBeTruthy();
  expect(within(dialog).getByText('剩余 1%')).toBeTruthy();
  expect(within(dialog).getByText('Second extra')).toBeTruthy();
  expect(within(dialog).getByText('剩余 20%')).toBeTruthy();
  expect(warning.mock.calls.some(args => args.some(value => /same key|unique.*key/i.test(String(value))))).toBe(false);
});

it('persists access-method icons and keeps the reused picker out of channel writes', async () => {
  const { fetcher, unmount } = setup();
  fireEvent.click(screen.getByRole('tab', { name: /Copilot/ }));
  expect(screen.queryByRole('button', { name: '更换 Codex 社区 图标' })).toBeNull();
  const codexTab = screen.getByRole('tab', { name: /Codex/ });
  fireEvent.click(codexTab.querySelector('.oauth-main-provider-icon')!);
  expect(codexTab.getAttribute('aria-selected')).toBe('true');
  expect(screen.queryByRole('dialog')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '更换 Codex 社区 图标' }));
  fireEvent.click(screen.getByRole('button', { name: '选择测试图标' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).icons.codex_oauth).toBe('openai');
  unmount(); setup();
  expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).icons.codex_oauth).toBe('openai');
  expect(fetcher.mock.calls.some(([, init]) => init?.method)).toBe(false);
});

it('keeps management available when every access method is hidden and restores without changing channel enablement', async () => {
  const { fetcher, callbacks } = setup([channel(7)]);
  const dialog = manage();
  for (const provider of providers) fireEvent.click(within(dialog).getByLabelText(`显示 ${provider.title}`));
  expect(screen.queryAllByRole('tab')).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
  expect(screen.getByRole('button', { name: '管理接入方式' })).toBeTruthy();
  const reopened = manage();
  fireEvent.click(within(reopened).getByLabelText('显示 Codex 社区'));
  fireEvent.click(within(reopened).getByRole('button', { name: '关闭弹窗' }));
  await screen.findByRole('switch', { name: '停用 Channel 7' });
  expect(screen.getByRole('tab', { name: /Codex/ }).getAttribute('aria-selected')).toBe('true');
  expect(callbacks.onToggle).not.toHaveBeenCalled();
  expect(fetcher.mock.calls.some(([url, init]) => url.startsWith('/api/channels') && init?.method === 'PUT')).toBe(false);
});

it('saves only the global quota refresh field, announces the change, and retains a failed draft', async () => {
  let fail = true;
  const changed = vi.fn(); window.addEventListener('quota-settings-changed', changed);
  try {
    const { fetcher } = setup([], (url, init) => url === '/api/settings/quota-refresh' && init?.method === 'PUT'
      ? Promise.resolve(fail ? json({ error: { message: '保存失败' } }, 500) : json({ quota_refresh_seconds: 60 })) : undefined);
    chooseCodex();
    fireEvent.click(screen.getByText('刷新周期'));
    const input = await screen.findByLabelText('额度刷新周期（秒）');
    await waitFor(() => expect(input).toHaveProperty('value', '300'));
    fireEvent.change(input, { target: { value: '60' } });
    fireEvent.click(screen.getByRole('button', { name: '保存刷新周期' }));
    await screen.findByText(/保存失败，填写已保留/);
    expect(input).toHaveProperty('value', '60'); expect(changed).not.toHaveBeenCalled();
    fail = false; fireEvent.click(screen.getByRole('button', { name: '保存刷新周期' }));
    await waitFor(() => expect(changed).toHaveBeenCalledTimes(1));
    const writes = fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
    expect(writes).toHaveLength(2);
    for (const [url, init] of writes) { expect(url).toBe('/api/settings/quota-refresh'); expect(JSON.parse(String(init?.body))).toEqual({ quota_refresh_seconds: 60 }); }
  } finally { window.removeEventListener('quota-settings-changed', changed); }
});

it('retains the refresh draft on rejected navigation save, then saves the narrow setting before leaving', async () => {
  let fail = true, leave: BeforeLeave | undefined, left = false;
  const view = setup([], (url, init) => url === '/api/settings/quota-refresh' && init?.method === 'PUT'
    ? Promise.resolve(fail ? json({ error: 'refresh save rejected' }, 409) : json({ quota_refresh_seconds: 60 })) : undefined);
  view.unmount();
  render(<NavigationGuard.Provider value={{ register: handler => { leave = handler; return () => {}; } }}><button onClick={() => void leave?.().then(() => { left = true; }).catch(() => {})}>导航离开</button><OAuthAccounts {...view.props} /></NavigationGuard.Provider>);
  chooseCodex(); fireEvent.click(screen.getByText('刷新周期'));
  const input = await screen.findByLabelText('额度刷新周期（秒）'); await waitFor(() => expect(input).toHaveProperty('value', '300'));
  fireEvent.change(input, { target: { value: '60' } });
  fireEvent.click(screen.getByText('导航离开')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('保存失败，填写已保留：refresh save rejected'); expect(left).toBe(false); expect(input).toHaveProperty('value', '60');
  fail = false; fireEvent.click(screen.getByText('导航离开')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(left).toBe(true));
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT'); expect(writes).toHaveLength(2);
  expect(writes.every(([url, init]) => url === '/api/settings/quota-refresh' && JSON.parse(String(init?.body)).quota_refresh_seconds === 60)).toBe(true);
});

it('protects the refresh draft when changing access method by mouse or keyboard', async () => {
  let fail = true;
  setup([], (url, init) => url === '/api/settings/quota-refresh' && init?.method === 'PUT'
    ? Promise.resolve(fail ? json({ error: 'refresh save rejected' }, 409) : json({ quota_refresh_seconds: 60 })) : undefined);
  chooseCodex(); fireEvent.click(screen.getByText('刷新周期'));
  const input = await screen.findByLabelText('额度刷新周期（秒）'); await waitFor(() => expect(input).toHaveProperty('value', '300'));
  fireEvent.change(input, { target: { value: '60' } });
  fireEvent.click(screen.getByRole('tab', { name: /Copilot/ }));
  fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull()); expect(input).toHaveProperty('value', '60');
  fireEvent.keyDown(screen.getByRole('tab', { name: /Codex/ }), { key: 'ArrowLeft' });
  fireEvent.click(await screen.findByRole('button', { name: '确认保存' })); await screen.findByText('保存失败，填写已保留：refresh save rejected');
  expect(input).toHaveProperty('value', '60'); expect(screen.getByRole('tab', { name: /Codex/ }).getAttribute('aria-selected')).toBe('true');
  fail = false; fireEvent.click(screen.getByRole('tab', { name: /Copilot/ })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(screen.getByRole('tab', { name: /Copilot/ }).getAttribute('aria-selected')).toBe('true'));
});

it('keeps the current provider and its unsaved refresh setting mounted when search hides all its channels', async () => {
  const view = setup([channel(7)]);
  chooseCodex(); fireEvent.click(screen.getByText('刷新周期'));
  const input = await screen.findByLabelText('额度刷新周期（秒）');
  await waitFor(() => expect(input).toHaveProperty('value', '300'));
  fireEvent.change(input, { target: { value: '60' } });
  view.rerender(<OAuthAccounts {...view.props} search="unmatched channel" />);
  expect(screen.getByRole('tab', { name: /Codex/ }).getAttribute('aria-selected')).toBe('true');
  expect(screen.getByLabelText('额度刷新周期（秒）')).toBe(input);
  expect(input).toHaveProperty('value', '60');
  expect(screen.queryByRole('region', { name: '渠道 Channel 7' })).toBeNull();
  expect(screen.getByText('未找到匹配渠道，试试其他名称、模型或地址。')).toBeTruthy();
});

it('preserves hidden channel order when reordering search results', () => {
  localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ active: 'codex_oauth', accounts: { codex_oauth: [9, 8, 7] } }));
  const rows = [{ ...channel(7), name: 'Match first' }, { ...channel(8), name: 'Other' }, { ...channel(9), name: 'Match last' }];
  const view = setup(rows);
  view.rerender(<OAuthAccounts {...view.props} search="match" />);
  fireEvent.click(screen.getByRole('button', { name: '下移账号 Match last', hidden: true }));
  expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).accounts.codex_oauth).toEqual([7, 8, 9]);
  view.rerender(<OAuthAccounts {...view.props} />);
  expect(screen.getAllByRole('region', { name: /^渠道 / }).map(region => region.getAttribute('aria-label'))).toEqual(['渠道 Match first', '渠道 Other', '渠道 Match last']);
});

it('requires an explicit discard before hiding the access method containing the refresh draft', async () => {
  const view = setup(); chooseCodex(); fireEvent.click(screen.getByText('刷新周期'));
  const input = await screen.findByLabelText('额度刷新周期（秒）'); await waitFor(() => expect(input).toHaveProperty('value', '300'));
  fireEvent.change(input, { target: { value: '60' } });
  const manager = manage(), visibility = within(manager).getByRole('switch', { name: '显示 Codex 社区' });
  fireEvent.click(visibility);
  fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
  expect(visibility.getAttribute('aria-checked')).toBe('true'); expect(input).toHaveProperty('value', '60');
  fireEvent.click(visibility); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
  await waitFor(() => expect(visibility.getAttribute('aria-checked')).toBe('false'));
  expect(screen.queryByLabelText('额度刷新周期（秒）')).toBeNull(); expect(view.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
});

it('does not display a late quota completion in a different access method and rereads on return', async () => {
  let finish!: (value: Response) => void;
  let calls = 0;
  const { fetcher } = setup([channel(7, ['one'])], url => url.startsWith('/api/oauth/codex/usage') && ++calls === 1 ? new Promise(resolve => { finish = resolve; }) : undefined);
  chooseCodex();
  await waitFor(() => expect(quotaCalls(fetcher)).toHaveLength(1));
  fireEvent.click(screen.getByRole('tab', { name: /Copilot/ }));
  await act(async () => finish(json(quota(99))));
  expect(screen.queryByText('剩余 1%')).toBeNull();
  chooseCodex();
  await screen.findByText('剩余 90%');
  expect(quotaCalls(fetcher)).toHaveLength(2);
  expect(screen.queryByText('剩余 1%')).toBeNull();
});


it('shows user remarks and channel-local fallback names without exposing copied account identities', async () => {
  const first = channel(7, ['one', 'two', 'three']);
  first.credentials![0].label = 'one@example.invalid';
  first.credentials![1].label = '';
  first.credentials![2].label = '工作凭据';
  const single = channel(8, ['single']);
  single.credentials![0].label = '';
  const { fetcher } = setup([first, single]); chooseCodex();
  await screen.findAllByText('剩余 90%');
  for (const name of ['凭据 1', '凭据 2', '工作凭据', '默认凭据']) {
    expect(screen.getByRole('article', { name: `凭证 ${name}` })).toBeTruthy();
  }
  for (const credential of [...first.credentials!, ...single.credentials!]) {
    const account = credential.oauth_info!.account_label!;
    expect(screen.queryByText(account)).toBeNull();
    expect(document.querySelector(`[title="${account}"]`)).toBeNull();
    expect([...document.querySelectorAll('[aria-label]')].some(element => element.getAttribute('aria-label')?.includes(account))).toBe(false);
  }
  expect(quotaCalls(fetcher)).toHaveLength(4);
  expect(fetcher.mock.calls.some(([, init]) => init?.method)).toBe(false);
});

it('reveals the account only on request and hides it again after closing credential details', async () => {
  const row = channel(7, ['one']); row.credentials![0].label = 'one@example.invalid';
  row.credentials![0].oauth_info!.expires_at = '2020-01-01T00:00:00Z';
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
  const { fetcher } = setup([row]); chooseCodex(); await screen.findByText('剩余 90%');
  expect(screen.queryByText('ID 7')).toBeNull();
  expect(screen.queryByText('one')).toBeNull();
  const open = () => fireEvent.click(screen.getByRole('button', { name: '额度详情 默认凭据' }));
  open();
  const dialog = screen.getByRole('dialog', { name: '凭证详情 · 默认凭据' });
  expect(within(dialog).queryByText('one@example.invalid')).toBeNull();
  const diagnostics = within(dialog).getByText('诊断信息').closest('details')!;
  expect(diagnostics.open).toBe(false);
  expect(within(dialog).getByText(/当前访问令牌到期/).closest('details')).toBe(diagnostics);
  expect(within(dialog).getByText('由系统按需续期；需要重新授权时会另行提示。')).toBeTruthy();
  fireEvent.click(within(dialog).getByText('诊断信息'));
  fireEvent.click(within(diagnostics).getByRole('button', { name: '复制渠道 ID' }));
  fireEvent.click(within(diagnostics).getByRole('button', { name: '复制凭证 ID' }));
  await waitFor(() => expect(writeText.mock.calls).toEqual([['7'], ['one']]));
  expect(fetcher.mock.calls.some(([, init]) => init?.method)).toBe(false);
  expect(within(dialog).queryByText('需重新授权')).toBeNull();
  fireEvent.click(within(dialog).getByRole('button', { name: '显示账号', expanded: false }));
  expect(within(dialog).getByText('one@example.invalid')).toBeTruthy();
  fireEvent.click(within(dialog).getByRole('button', { name: '隐藏账号', expanded: true }));
  expect(within(dialog).queryByText('one@example.invalid')).toBeNull();
  fireEvent.click(within(dialog).getByRole('button', { name: '显示账号' }));
  fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
  open();
  expect(screen.queryByText('one@example.invalid')).toBeNull();
  expect(screen.getByRole('button', { name: '显示账号', expanded: false })).toBeTruthy();
  expect(screen.getByText('诊断信息').closest('details')!.open).toBe(false);
});

it('closes menus on outside pointer or keyboard focus while preserving internal interaction and Escape focus', async () => {
  setup([channel(7), channel(8)]); chooseCodex(); await screen.findByText('剩余 90%');
  const summary = screen.getByLabelText('更多操作 Channel 7');
  const menu = summary.closest('details')!;
  const open = () => { fireEvent.click(summary); expect(menu.open).toBe(true); };
  open();
  fireEvent.pointerDown(menu.querySelector('div')!);
  fireEvent.click(menu.querySelector('div')!);
  expect(menu.open).toBe(true);
  const action = within(menu).getByRole('button', { name: '获取模型 Channel 7' });
  act(() => action.focus()); expect(menu.open).toBe(true);
  fireEvent.keyDown(action, { key: 'Escape' });
  expect(menu.open).toBe(false); expect(document.activeElement).toBe(summary);
  open(); fireEvent.pointerDown(document.body); expect(menu.open).toBe(false);
  open(); fireEvent.pointerDown(document.body, { pointerType: 'touch' }); expect(menu.open).toBe(false);
  open(); act(() => screen.getByRole('button', { name: '管理接入方式' }).focus()); expect(menu.open).toBe(false);
  open();
  const second = screen.getByLabelText('更多操作 Channel 8');
  fireEvent.click(second);
  await waitFor(() => expect(menu.open).toBe(false));
  expect(second.closest('details')!.open).toBe(true);
});

it('closes only enabled menu actions, runs each once, and gives a modal a visible focus return target', async () => {
  const view = setup([channel(7), channel(8)], (url, init) => url === '/api/channels/7/oauth/models' && init?.method === 'POST' ? Promise.resolve(json({ ok: true, fetched: 2 })) : undefined);
  view.rerender(<><OAuthAccounts {...view.props} />{null}</>);
  chooseCodex(); await screen.findByText('剩余 90%');
  const summary = screen.getByLabelText('更多操作 Channel 7'), menu = summary.closest('details')!;
  fireEvent.click(summary);
  fireEvent.click(within(menu).getByRole('button', { name: '上移账号 Channel 7' }));
  expect(menu.open).toBe(true);
  fireEvent.click(within(menu).getByRole('button', { name: '获取模型 Channel 7' }));
  expect(menu.open).toBe(false); expect(document.activeElement).toBe(summary);
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1));
  expect(view.fetcher.mock.calls.filter(([url]) => url === '/api/channels/7/oauth/models')).toHaveLength(1);
  for (const [action, callback] of [['重新授权', view.callbacks.onReauthorize], ['删除', view.callbacks.onDelete]] as const) {
    callback.mockImplementation(() => view.rerender(<><OAuthAccounts {...view.props} /><Modal title="操作确认" initialFocus="button" onClose={() => view.rerender(<><OAuthAccounts {...view.props} />{null}</>)}>确认操作</Modal></>));
    fireEvent.click(summary);
    const button = within(menu).getByRole('button', { name: `${action} Channel 7` });
    act(() => button.focus()); fireEvent.click(button);
    expect(callback).toHaveBeenCalledTimes(1); expect(menu.open).toBe(false);
    const dialog = screen.getByRole('dialog', { name: '操作确认' });
    expect(dialog.contains(document.activeElement)).toBe(true);
    fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
    expect(document.activeElement).toBe(summary);
  }
});


it('opens existing channel configuration from the name or credential count and expands only enabled models', async () => {
  const source = { ...channel(7), available_models: ['catalog-only'] };
  const view = setup([source]); chooseCodex();
  fireEvent.click(screen.getByRole('button', { name: '编辑渠道名称 Channel 7' }));
  expect(view.callbacks.onEdit).toHaveBeenLastCalledWith(source);
  fireEvent.click(screen.getByRole('button', { name: '配置 Channel 7 凭证' }));
  expect(view.callbacks.onEdit).toHaveBeenLastCalledWith(source, 'credentials');
  const summary = screen.getByLabelText('查看 Channel 7 全部模型');
  fireEvent.click(summary);
  expect(summary.closest('details')!.open).toBe(true);
  expect(within(summary.closest('details')!).getByText('model-a')).toBeTruthy();
  expect(screen.queryByText('catalog-only')).toBeNull();
});

it('writes the entire credential order once and waits for reread instead of optimistic reordering', async () => {
  let finish!: (value: Response) => void;
  const source = channel(7, ['one', 'two', 'three']);
  source.credentials!.forEach((credential, index) => { credential.label = `Note ${index + 1}`; });
  const view = setup([source], (url, init) => url === '/api/channels/7/credential-order' && init?.method === 'PUT' ? new Promise(resolve => { finish = resolve; }) : undefined);
  chooseCodex(); await within(screen.getByRole('article', { name: '凭证 Note 1' })).findByText('剩余 90%');
  const ids = () => screen.getAllByRole('article').map(row => row.dataset.credentialId);
  fireEvent.click(screen.getByRole('button', { name: '下移凭证 Note 1' }));
  fireEvent.click(screen.getByRole('button', { name: '下移凭证 Note 1' }));
  expect(ids()).toEqual(['one', 'two', 'three']);
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
  expect(writes).toHaveLength(1);
  expect(JSON.parse(String(writes[0][1]!.body))).toEqual({ credential_ids: ['two', 'one', 'three'] });
  expect(screen.getByRole('switch', { name: '停用凭证 Note 2' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('button', { name: '编辑 Channel 7' })).toHaveProperty('disabled', true);
  await act(async () => finish(json({ ok: true })));
  expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1);
  expect(ids()).toEqual(['one', 'two', 'three']);
  const changed = { ...source, credentials: [source.credentials![1], source.credentials![0], source.credentials![2]] };
  view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'codex_oauth' ? [changed] : [] }))} />);
  expect(ids()).toEqual(['two', 'one', 'three']);
});

it('preserves failed order and enablement, encodes the exact credential ID, and uses narrow bodies', async () => {
  const source = channel(7, ['one/space ?', 'two']);
  const view = setup([source], (url, init) => init?.method === 'PUT' && url.startsWith('/api/channels/7/') ? Promise.resolve(json({ error: { message: '拒绝写入' } }, 409)) : undefined);
  chooseCodex(); await screen.findByText('剩余 90%');
  const rows = screen.getAllByRole('article');
  fireEvent.click(within(rows[0]).getByRole('button', { name: '下移凭证 Same label' }));
  await screen.findByRole('alert');
  expect(screen.getAllByRole('article').map(row => row.dataset.credentialId)).toEqual(['one/space ?', 'two']);
  expect(view.callbacks.onAdded).not.toHaveBeenCalled();
  fireEvent.click(within(rows[0]).getByRole('switch', { name: '停用凭证 Same label' }));
  await waitFor(() => expect(within(rows[0]).getByRole('switch', { name: '停用凭证 Same label' })).toHaveProperty('disabled', false));
  expect(within(rows[0]).getByRole('switch', { name: '停用凭证 Same label' }).getAttribute('aria-checked')).toBe('true');
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
  expect(writes[1][0]).toBe('/api/channels/7/credentials/one%2Fspace%20%3F/enabled');
  expect(JSON.parse(String(writes[1][1]!.body))).toEqual({ enabled: false });
  expect(view.callbacks.onAdded).not.toHaveBeenCalled();
});

it('moves credentials by drag and keyboard without changing channel display preferences', async () => {
  const source = channel(7, ['one', 'two']);
  source.credentials![0].label = 'First'; source.credentials![1].label = 'Second';
  const view = setup([source], (url, init) => url === '/api/channels/7/credential-order' && init?.method === 'PUT' ? Promise.resolve(json({ ok: true })) : undefined);
  chooseCodex(); await screen.findByText('剩余 90%');
  const preferences = localStorage.getItem('to-router.oauth-display.v1');
  const release = pointerMove(screen.getByRole('button', { name: '拖动凭证 Second' }), screen.getByRole('article', { name: '凭证 First' }));
  expect(screen.getByRole('article', { name: '凭证 First' }).style.getPropertyValue('--sortable-y')).not.toBe('0px');
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
  release();
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1));
  fireEvent.keyDown(screen.getByRole('button', { name: '拖动凭证 Second' }), { key: 'ArrowUp' });
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(2));
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
  expect(writes).toHaveLength(2);
  for (const [, init] of writes) expect(JSON.parse(String(init!.body))).toEqual({ credential_ids: ['two', 'one'] });
  expect(localStorage.getItem('to-router.oauth-display.v1')).toBe(preferences);
});

it.each(['tab', 'icon'] as const)('previews horizontal access-method sorting and preserves hidden slots without a trailing %s click', async trailing => {
  localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ hidden: { chatgpt_oauth: true }, active: 'codex_oauth' }));
  const view = setup();
  await screen.findByRole('tab', { name: 'Codex 社区' });
  const tab = screen.getByRole('tab', { name: 'Codex 社区' });
  const finish = pointerMove(screen.getByRole('button', { name: '拖动接入方式 GitHub Copilot' }), tab.parentElement!, true);
  expect(tab.parentElement!.style.getPropertyValue('--sortable-x')).toBe('100px');
  expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).platforms).toEqual([]);
  finish();
  await waitFor(() => expect(screen.getAllByRole('tab').map(item => item.textContent)).toEqual(['GitHub Copilot0', 'Codex 社区0', 'WorkBuddy0', 'WorkBuddy AI0', 'MiniMax Code0', 'MiniMax Code Global0']));
  expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).platforms).toEqual(['copilot_oauth', 'chatgpt_oauth', 'codex_oauth', 'plugin:workbuddy', 'plugin:workbuddy-ai', 'plugin:minimax-code', 'plugin:minimax-code-global']);
  fireEvent.click(trailing === 'tab' ? screen.getByRole('tab', { name: 'GitHub Copilot' }) : screen.getByRole('button', { name: '更换 Codex 社区 图标' }), { detail: 1 });
  expect(screen.getByRole('tab', { name: 'Codex 社区' }).getAttribute('aria-selected')).toBe('true');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(view.fetcher.mock.calls.some(([, init]) => init?.method)).toBe(false);
});

it('keeps OAuth channel display sorting in browser preferences and preserves its keyboard path', async () => {
  const view = setup([channel(7, ['one']), channel(8, ['one'])]); chooseCodex();
  await screen.findByText('剩余 90%');
  const first = screen.getByRole('region', { name: '渠道 Channel 7' });
  const second = screen.getByRole('region', { name: '渠道 Channel 8' });
  const finish = pointerMove(screen.getByRole('button', { name: '拖动渠道 Channel 8' }), first);
  expect(first.style.getPropertyValue('--sortable-y')).toBe('100px'); finish();
  await waitFor(() => expect(Array.from(second.parentElement!.children).map(item => item.getAttribute('aria-label'))).toEqual(['渠道 Channel 8', '渠道 Channel 7']));
  expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).accounts.codex_oauth).toEqual([8, 7]);
  fireEvent.keyDown(screen.getByRole('button', { name: '拖动渠道 Channel 7' }), { key: 'ArrowUp' });
  expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).accounts.codex_oauth).toEqual([7, 8]);
  expect(view.fetcher.mock.calls.some(([, init]) => init?.method)).toBe(false);
});

it('cancels credential pointer sorting when generation or exact account identity changes', async () => {
  const source = { ...channel(7), generation: 4 };
  source.credentials![0].label = 'First'; source.credentials![1].label = 'Second';
  const view = setup([source]); chooseCodex(); await screen.findByText('剩余 90%');
  const finish = pointerMove(screen.getByRole('button', { name: '拖动凭证 Second' }), screen.getByRole('article', { name: '凭证 First' }));
  const replace = (next: Channel) => view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'codex_oauth' ? [next] : [] }))} />);
  replace({ ...source, generation: 5 }); finish();
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
  const release = pointerMove(screen.getByRole('button', { name: '拖动凭证 Second' }), screen.getByRole('article', { name: '凭证 First' }));
  replace({ ...source, generation: 5, credentials: source.credentials!.map((credential, index) => index ? { ...credential, oauth_info: oauth('replacement@example.invalid') } : credential) }); release();
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
  expect(screen.getByRole('article', { name: '凭证 First' }).style.getPropertyValue('--sortable-y')).toBe('');
});

it('rolls failed credential pointer order back without changing display preferences', async () => {
  const source = channel(7); source.credentials![0].label = 'First'; source.credentials![1].label = 'Second';
  const view = setup([source], (url, init) => url.endsWith('/credential-order') && init?.method === 'PUT' ? Promise.resolve(json({ error: { message: 'order rejected' } }, 409)) : undefined);
  chooseCodex(); await screen.findByText('剩余 90%');
  const saved = localStorage.getItem('to-router.oauth-display.v1');
  const first = screen.getByRole('article', { name: '凭证 First' });
  const finish = pointerMove(screen.getByRole('button', { name: '拖动凭证 Second' }), first);
  expect(first.style.getPropertyValue('--sortable-y')).toBe('100px'); finish();
  expect((await screen.findByRole('alert')).textContent).toContain('order rejected');
  await waitFor(() => expect(first.style.getPropertyValue('--sortable-y')).toBe(''));
  expect(screen.getAllByRole('article').map(item => item.dataset.credentialId)).toEqual(['one', 'two']);
  expect(view.callbacks.onAdded).not.toHaveBeenCalled();
  expect(localStorage.getItem('to-router.oauth-display.v1')).toBe(saved);
});

it('uses the existing runtime observation rather than authorization as availability and retains old rows on reload', async () => {
  const source = channel(7, ['one']);
  const runtime = vi.spyOn(runtimeStatus, 'useOptionalRuntimeStatus').mockReturnValue({ data: { problem_count: 1, rows: [{ channel_id: 7, channel_name: 'Channel 7', credential_id: 'one', credential_name: 'Same label', red: true, reason: '上游限流', failures: 1, quota_supported: true, models: [] }] }, error: '', loading: false, updatedAt: null, reload: vi.fn() });
  const view = setup([source]); chooseCodex(); await screen.findByText('剩余 90%');
  const row = screen.getByRole('article', { name: '凭证 Same label' });
  expect(within(row).getByText('上游限流')).toBeTruthy();
  expect(within(row).queryByText('已授权')).toBeNull();
  view.rerender(<OAuthAccounts {...view.props} loading />);
  expect(screen.getByRole('article', { name: '凭证 Same label' })).toBe(row);
  expect(within(row).getByText('剩余 90%')).toBeTruthy();
  runtime.mockReturnValue({ data: null, error: '', loading: false, updatedAt: null, reload: vi.fn() });
  view.rerender(<OAuthAccounts {...view.props} />);
  expect(within(row).getByText('状态未读取')).toBeTruthy();
  expect(view.fetcher.mock.calls.some(([url]) => url === '/api/status')).toBe(false);
});


it('enables the exact credential with a narrow write and adopts only the channel reread', async () => {
  const source = channel(7, ['one']); source.credentials![0].enabled = false;
  const view = setup([source], (url, init) => url === '/api/channels/7/credentials/one/enabled' && init?.method === 'PUT' ? Promise.resolve(json({ ok: true })) : undefined);
  chooseCodex();
  fireEvent.click(screen.getByRole('switch', { name: '启用凭证 Same label' }));
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1));
  expect(screen.getByRole('switch', { name: '启用凭证 Same label' }).getAttribute('aria-checked')).toBe('false');
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
  expect(writes).toHaveLength(1);
  expect(JSON.parse(String(writes[0][1]!.body))).toEqual({ enabled: true });
  const changed = { ...source, credentials: [{ ...source.credentials![0], enabled: true }] };
  view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'codex_oauth' ? [changed] : [] }))} />);
  expect(screen.getByRole('switch', { name: '停用凭证 Same label' }).getAttribute('aria-checked')).toBe('true');
});

it('shows authorization problems separately and marks retained runtime status after a read failure', async () => {
  const source = channel(7, ['one', 'two']);
  source.credentials![0].label = 'Expired'; source.credentials![0].oauth_info!.status = 'reauth_required';
  source.credentials![1].label = 'Observed';
  vi.spyOn(runtimeStatus, 'useOptionalRuntimeStatus').mockReturnValue({ data: { problem_count: 0, rows: source.credentials!.map(credential => ({ channel_id: 7, channel_name: 'Channel 7', credential_id: credential.id, credential_name: credential.label!, red: false, reason: '', failures: 0, quota_supported: true, models: [] })) }, error: '读取失败', loading: false, updatedAt: null, reload: vi.fn() });
  setup([source]); chooseCodex();
  expect(within(screen.getByRole('article', { name: '凭证 Expired' })).getByText('需重新授权')).toBeTruthy();
  const observed = within(screen.getByRole('article', { name: '凭证 Observed' }));
  expect(observed.getByText('未发现异常').className).toContain('is-neutral');
  expect(observed.getByText('上次状态')).toBeTruthy();
});

it('uses quota reset-card counts without per-row operation reads and exposes no Codex-only tools on Copilot', async () => {
  const source = channel(7, ['one']);
  const view = setup([source], url => url.startsWith('/api/oauth/codex/usage') ? Promise.resolve(json({ ...quota(10), reset_credit_count: 0 })) : undefined);
  chooseCodex();
  const count = await screen.findByRole('button', { name: '重置卡 Same label' });
  await waitFor(() => expect(count.textContent).toBe('重置卡 0'));
  expect(view.fetcher.mock.calls.some(([url]) => url.includes('/operations'))).toBe(false);
  const copilot = { ...source, credential_type: 'copilot_oauth' as const };
  view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'copilot_oauth' ? [copilot] : [] }))} />);
  fireEvent.click(screen.getByRole('tab', { name: /Copilot/ }));
  expect(screen.queryByRole('button', { name: '重置卡 Same label' })).toBeNull();
  expect(screen.queryByRole('button', { name: '预热 Same label' })).toBeNull();
  expect(screen.queryByLabelText('更多凭证操作 Same label')).toBeNull();
});
it('remembers each collapsed channel locally and receives quota without unmounting its rows', async () => {
  let finish!: (value: Response) => void;
  const view = setup([channel(7, ['one']), channel(8, ['one'])], url => url.startsWith('/api/oauth/codex/usage') && url.includes('channel_id=7') ? new Promise(resolve => { finish = resolve; }) : undefined);
  chooseCodex();
  const region = screen.getByRole('region', { name: '渠道 Channel 7' });
  const row = within(region).getByRole('article', { name: '凭证 Same label' });
  fireEvent.click(within(region).getByRole('button', { name: '收起 Channel 7 凭证区' }));
  const panel = document.getElementById('oauth-credentials-7')!;
  expect(panel.hidden).toBe(true);
  expect(row.isConnected).toBe(true);
  expect(screen.getByRole('button', { name: '收起 Channel 8 凭证区' })).toBeTruthy();
  await act(async () => finish(json(quota(23))));
  expect(within(row).getByText('剩余 77%')).toBeTruthy();
  expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).collapsedAccounts).toEqual({ 7: true });
  expect(view.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
  view.unmount();
  setup([channel(7, ['one']), channel(8, ['one'])]);
  expect(screen.getByRole('button', { name: '展开 Channel 7 凭证区' }).getAttribute('aria-expanded')).toBe('false');
  fireEvent.click(screen.getByRole('button', { name: '展开 Channel 7 凭证区' }));
  expect(document.getElementById('oauth-credentials-7')!.hidden).toBe(false);
});

it('selects credentials by exact channel and current identity and exposes partial channel selection', async () => {
  const source = channel(7, ['one', 'two']);
  const other = channel(8, ['one']);
  const view = setup([source, other]); chooseCodex();
  const region = within(screen.getByRole('region', { name: '渠道 Channel 7' }));
  const rows = region.getAllByRole('article');
  const all = region.getByRole('checkbox', { name: '全选 Channel 7 凭证' }) as HTMLInputElement;
  fireEvent.click(within(rows[0]).getByRole('checkbox', { name: '选择凭证 Same label' }));
  expect(all.indeterminate).toBe(true);
  expect(region.getByText('已选 1 / 2 个凭证')).toBeTruthy();
  expect(within(screen.getByRole('region', { name: '渠道 Channel 8' })).getByRole('checkbox', { name: '选择凭证 Same label' })).toHaveProperty('checked', false);
  fireEvent.click(all);
  expect(all.indeterminate).toBe(false); expect(all.checked).toBe(true);
  fireEvent.click(all);
  expect(region.queryByText(/已选/)).toBeNull();
  fireEvent.click(within(rows[0]).getByRole('checkbox', { name: '选择凭证 Same label' }));
  const changed = { ...source, credentials: source.credentials!.map(credential => ({ ...credential, oauth_info: { ...credential.oauth_info!, updated_at: '2026-10-18T01:00:00Z' } })) };
  view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'codex_oauth' ? [changed, other] : [] }))} />);
  expect(region.queryByText(/已选/)).toBeNull();
  expect(view.fetcher.mock.calls.some(([, init]) => !!init?.method)).toBe(false);
});

it('shows disabled and authorization reasons under short statuses and reads automatic warmup from the channel projection', () => {
  const source = channel(7, ['one', 'two']);
  source.credentials![0].label = 'Disabled'; source.credentials![0].enabled = false;
  source.credentials![1].label = 'Expired'; source.credentials![1].oauth_info!.status = 'reauth_required';
  source.credentials![1].oauth_info!.status_reason = '授权已撤销';
  source.credentials![0].operations = { warmup: { enabled: true, mode: 'daily', model: 'model-a', daily_times: ['08:00'] }, auto_reset: { enabled: false, rules: [] } };
  const view = setup([source]); chooseCodex();
  const disabled = within(screen.getByRole('article', { name: '凭证 Disabled' }));
  expect(disabled.getAllByText('已停用').length).toBeGreaterThan(0);
  expect(disabled.getByText('凭证停用')).toBeTruthy(); expect(disabled.getByText('自动预热：开')).toBeTruthy();
  const expired = within(screen.getByRole('article', { name: '凭证 Expired' }));
  expect(expired.getByText('不可用')).toBeTruthy(); expect(expired.getByText('授权已撤销')).toBeTruthy(); expect(expired.getByText('自动预热：关')).toBeTruthy();
  view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'codex_oauth' ? [{ ...source, enabled: false }] : [] }))} />);
  expect(expired.getByText('渠道停用')).toBeTruthy();
  expect(expired.queryByText('不可用')).toBeNull();
});

it('shows only an explicitly reported weekly general window and preserves unknown quota and card counts', async () => {
  const view = setup([channel(7, ['one'])], url => url.startsWith('/api/oauth/codex/usage') ? Promise.resolve(json({ ...quota(10), reset_credit_count: null, buckets: [{ id: 'general', general: true, primary: { window_seconds: 604800, used_percent: null } }, { id: 'extra', primary: { window_seconds: 18000 } }] })) : undefined);
  chooseCodex();
  await screen.findByText('仅7天额度');
  expect(screen.getAllByText('剩余未知')).toHaveLength(2);
  const cards = screen.getByRole('button', { name: '重置卡 Same label' });
  expect(cards.textContent).toBe('重置卡 未知'); expect(cards.classList.contains('has-cards')).toBe(false); expect(cards).toHaveProperty('disabled', false);
  expect(view.fetcher.mock.calls.some(([url]) => url.includes('/operations'))).toBe(false);
});

it('emphasizes available reset cards without claiming which windows reset', async () => {
  setup([channel(7, ['one'])], url => url.startsWith('/api/oauth/codex/usage') ? Promise.resolve(json({ ...quota(10), reset_credit_count: 3 })) : undefined);
  chooseCodex();
  const cards = screen.getByRole('button', { name: '重置卡 Same label' });
  await waitFor(() => expect(cards.classList.contains('has-cards')).toBe(true));
  expect(cards.textContent).toBe('重置卡 3');
  expect(cards.querySelector('svg')).toBeTruthy();
  expect(screen.queryByText(/重置5h|5h\+7d/)).toBeNull();
});
it('keeps selection after batch enablement and uses only its returned generation for the next enablement and configuration', async () => {
  let persisted: Channel = { ...channel(7, ['one', 'two', 'three']), generation: 4 };
  persisted.credentials!.forEach((credential, index) => { credential.label = `Note ${index + 1}`; });
  const view = setup([persisted], (url, init) => {
    if (init?.method === 'PUT' && url.endsWith('/enabled')) {
      const id = decodeURIComponent(url.match(/\/credentials\/([^/]+)\//)![1]), body = JSON.parse(String(init.body));
      expect(body.expected_generation).toBe(persisted.generation);
      persisted = { ...persisted, generation: persisted.generation! + 1, credentials: persisted.credentials!.map(credential => credential.id === id ? { ...credential, enabled: body.enabled } : credential) };
      return Promise.resolve(json({ ok: true, generation: persisted.generation }));
    }
    if (url.endsWith('/operations')) {
      const id = decodeURIComponent(url.match(/\/credentials\/([^/]+)\//)![1]);
      return Promise.resolve(json({ supported: true, identity: `account:${id}`, channel_generation: persisted.generation, config: { warmup: { enabled: false, mode: 'reset', model: '', daily_times: [] }, auto_reset: { enabled: false, rules: [] } }, time_zone: 'Asia/Shanghai' }));
    }
  });
  chooseCodex();
  fireEvent.click(screen.getByRole('checkbox', { name: '选择凭证 Note 1' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '选择凭证 Note 2' }));
  fireEvent.click(screen.getByRole('button', { name: '停用' }));
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1));
  const reread = () => view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'codex_oauth' ? [persisted] : [] }))} />);
  reread();
  expect(screen.getByRole('checkbox', { name: '选择凭证 Note 1' })).toHaveProperty('checked', true);
  expect(screen.getByText('已选 2 / 3 个凭证')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '启用' }));
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(2));
  expect(persisted.generation).toBe(8);
  reread();
  fireEvent.click(screen.getByRole('button', { name: '自动重置配置' }));
  fireEvent.click(screen.getByRole('button', { name: '应用到选中凭证' }));
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(3));
  const enabledWrites = view.fetcher.mock.calls.filter(([url, init]) => init?.method === 'PUT' && url.endsWith('/enabled'));
  expect(enabledWrites.map(([, init]) => JSON.parse(String(init!.body)).expected_generation)).toEqual([4, 5, 6, 7]);
  const configWrites = view.fetcher.mock.calls.filter(([url, init]) => init?.method === 'PUT' && url.endsWith('/operations'));
  expect(configWrites).toHaveLength(2);
});

it('does not adopt an external generation reread after its own batch enablement', async () => {
  const source = { ...channel(7, ['one', 'two', 'three']), generation: 4 };
  source.credentials!.forEach((credential, index) => { credential.label = `Note ${index + 1}`; });
  let savedGeneration = 4;
  const view = setup([source], (url, init) => init?.method === 'PUT' && url.endsWith('/enabled') ? Promise.resolve(json({ ok: true, generation: ++savedGeneration })) : undefined);
  chooseCodex();
  fireEvent.click(screen.getByRole('checkbox', { name: '选择凭证 Note 1' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '选择凭证 Note 2' }));
  fireEvent.click(screen.getByRole('button', { name: '停用' }));
  await waitFor(() => expect(view.callbacks.onAdded).toHaveBeenCalledTimes(1));
  const external = { ...source, generation: 99, credentials: source.credentials!.map((credential, index) => ({ ...credential, enabled: index === 2 })) };
  view.rerender(<OAuthAccounts {...view.props} pools={view.props.pools.map(pool => ({ ...pool, channels: pool.provider.type === 'codex_oauth' ? [external] : [] }))} />);
  fireEvent.click(screen.getByRole('button', { name: '启用' }));
  await screen.findAllByText(/渠道或凭证已变化/);
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(2);
  expect(savedGeneration).toBe(6);
});
