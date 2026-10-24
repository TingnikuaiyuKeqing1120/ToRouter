// @vitest-environment jsdom
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import type { Channel } from '../api';
import Channels from './Channels';
import { enabledPlugins } from '../testFixtures/plugins';

// The picker owns file decoding and icon options in its own tests. This seam
// checks that the page persists its selection through the display API only.
vi.mock('../components/ChannelIconPicker', () => ({
  ChannelIconPicker: ({ name, value, onSelect, onClose }: { name: string; value?: string; onSelect: (value: string) => Promise<void>; onClose: () => void }) => <section role="dialog" aria-label={`测试图标选择器 ${name}`}>
    <output aria-label="当前图标">{value || 'auto'}</output>
    <button type="button" onClick={async () => { await onSelect('openai'); onClose(); }}>选择测试内置图标</button>
  </section>,
}));

const render = () => renderUI(<Channels />, { wrapper: ConfirmProvider });
const json = (body: unknown) => new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
const alpha: Channel = {
  id: 12, name: 'Alpha Personal', api_key: '', has_key: true, credential_type: 'api_key',
  enabled: true, base_url: 'https://fallback.example/v1/',
  endpoints: [{ format: 'openai_chat_completions', base_url: '' }, { format: 'openai_responses', base_url: 'https://responses.example/v1' }],
  models: Array.from({ length: 7 }, (_, index) => `alpha-model-${index + 1}`), available_models: ['catalog-only-model'],
};
const beta: Channel = {
  id: 34, name: 'Beta Work', api_key: '', has_key: true, credential_type: 'api_key',
  enabled: false, models: ['beta-model'],
  endpoints: [{ format: 'anthropic_messages', base_url: 'https://beta.example/v1' }],
};
const oauth: Channel = { ...beta, id: 61, name: 'OAuth Work', credential_type: 'copilot_oauth', enabled: true, credentials: [{ id: 'work', enabled: true, has_oauth: true, oauth_info: { provider: 'copilot', account_label: 'Work', status: 'active', expires_at: '', updated_at: '' } }] };
const apiPath = '/api/channels?credential_type=api_key';
const displayPath = '/api/channels/display';
const oauthPaths = ['codex_oauth', 'chatgpt_oauth', 'copilot_oauth', 'claude_oauth', 'antigravity_oauth', 'zen_free', 'devin_oauth', 'plugin:qoder', 'plugin:qoder-cn', 'plugin:workbuddy', 'plugin:workbuddy-ai', 'plugin:minimax-code', 'plugin:minimax-code-global'].map(type => `/api/channels?credential_type=${type}`);

function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
function setupFetch(channelResponse: (url: string, init?: RequestInit) => Promise<Response> = () => Promise.resolve(json([alpha, beta])), displayResponse: (init?: RequestInit) => Promise<Response> = init => {
  if (init?.method) throw new Error(`Unexpected display write: ${init.method}`);
  return Promise.resolve(json({ sort_mode: 'name', order: [], icons: {} }));
}) {
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/key-discovery/sources') return Promise.resolve(json({ sources: [{ id: 'source-one', name: 'Source One', icon: 'generic', default_hint: '默认位置', files: ['config.json'], enabled: true, authorized: true }] }));
    if (url === '/api/plugins' && !init?.method) return Promise.resolve(json(enabledPlugins));
    if (url === '/api/quotas') return Promise.resolve(json({ items: [], refresh_seconds: 0 }));
    if (url === '/api/settings') return Promise.resolve(json({ oauth_auto_open: false, request_defaults_enabled: false, request_defaults: {} }));
    if (url === displayPath) return displayResponse(init);
    if (url.startsWith('/api/channels')) return channelResponse(url, init);
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
const row = (name = alpha.name) => screen.getByRole('article', { name: `渠道 ${name}` });
const search = () => screen.getByRole('textbox', { name: '搜索渠道或模型' });
async function loaded() {
  await screen.findByRole('article', { name: `渠道 ${alpha.name}` });
  await waitFor(() => expect(screen.getByRole('button', { name: '刷新渠道' })).toHaveProperty('disabled', false));
  await waitFor(() => expect(screen.getByRole('button', { name: `更换 ${alpha.name} 图标` })).toHaveProperty('disabled', false));
}
function modelDetails(article: HTMLElement) {
  const details = within(article).getByLabelText(`查看 ${alpha.name} 全部模型`).closest('details');
  expect(details).not.toBeNull();
  return details as HTMLDetailsElement;
}
function assertManagementProtected(article: HTMLElement, disabled: boolean, enabled = true) {
  for (const name of [`编辑 ${alpha.name}`, `测试渠道 ${alpha.name}`]) {
    expect(within(article).getByRole('button', { name })).toHaveProperty('disabled', disabled);
  }
  expect(within(article).getByRole('switch', { name: `${enabled ? '停用' : '启用'} ${alpha.name}` })).toHaveProperty('disabled', disabled);
  const more = within(article).getByLabelText(`更多操作 ${alpha.name}`).closest('details')!;
  expect(within(more).getByRole('button', { name: `删除 ${alpha.name}`, hidden: true })).toHaveProperty('disabled', disabled);
  expect(within(article).getByRole('button', { name: `复制 ${alpha.name} 上游地址` })).toHaveProperty('disabled', false);
}
function assertDisplayProtected(article: HTMLElement, disabled: boolean) {
  expect(within(article).getByRole('button', { name: `更换 ${alpha.name} 图标` })).toHaveProperty('disabled', disabled);
  expect(within(article).getByRole('button', { name: `拖动排序 ${alpha.name}` })).toHaveProperty('disabled', disabled || screen.getAllByRole('article').length < 2);
}
function assertProtected(article: HTMLElement, disabled: boolean, enabled = true) {
  assertManagementProtected(article, disabled, enabled);
  assertDisplayProtected(article, disabled);
}
const displayedIDs = () => screen.getAllByRole('article').map(article => Number(article.dataset.channelId));
const displayWrites = (fetcher: ReturnType<typeof setupFetch>) => fetcher.mock.calls.filter(([url, init]) => url === displayPath && init?.method === 'PUT').map(([, init]) => JSON.parse(String(init?.body)));
function move(name: string, direction: '上移' | '下移') {
  const summary = within(row(name)).getByLabelText(`更多操作 ${name}`);
  if (!summary.closest('details')!.open) fireEvent.click(summary);
  fireEvent.click(within(row(name)).getByRole('button', { name: `${direction} ${name}` }));
}
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

beforeEach(() => {
  vi.stubGlobal('PointerEvent', class extends MouseEvent {
    pointerId: number; isPrimary = true; pointerType = 'mouse';
    constructor(type: string, init: PointerEventInit = {}) { super(type, init); this.pointerId = init.pointerId ?? 1; }
  });
  vi.stubGlobal('requestAnimationFrame', vi.fn(() => 1));
  vi.stubGlobal('cancelAnimationFrame', vi.fn());
  sessionStorage.clear(); localStorage.clear(); history.replaceState(null, '', '/');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: vi.fn().mockResolvedValue(undefined) } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('API key channel display preferences', () => {
  it.each(['关闭弹窗', '取消', 'Escape'])('consumes a restored editor once and respects %s through refresh and explicit reopening', async close => {
    sessionStorage.setItem('to-router.channels.v1', JSON.stringify({ view: 'api_key', editor: { channelID: alpha.id, reauth: false } }));
    setupFetch();
    const page = render();
    const dialog = await screen.findByRole('dialog', { name: '编辑渠道' });
    if (close === 'Escape') fireEvent(dialog, new Event('cancel', { cancelable: true }));
    else fireEvent.click(within(dialog).getByRole('button', { name: close }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    page.rerender(<Channels />);
    fireEvent.click(screen.getByRole('button', { name: '刷新渠道' }));
    await loaded();
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(JSON.parse(sessionStorage.getItem('to-router.channels.v1')!).editor).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: `编辑 ${alpha.name}` }));
    expect(screen.getByRole('dialog', { name: '编辑渠道' })).toBeTruthy();
  });

  it('opens a linked channel once across credential views and keeps its editor closed after dismissal', async () => {
    history.replaceState(null, '', '/#/channels?channel_id=12');
    const fetcher = setupFetch();
    render();
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByDisplayValue(alpha.name)).toBeTruthy();
    expect(fetcher.mock.calls.some(([url]) => url === '/api/channels')).toBe(true);
    fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByRole('article', { name: `渠道 ${alpha.name}` })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '刷新渠道' }));
    await loaded();
    expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: `编辑 ${alpha.name}` }));
    expect(screen.getByRole('dialog', { name: '编辑渠道' })).toBeTruthy();
  });
  it('uses persisted manual order and appends newly discovered channels by name', async () => {
    const aardvark = { ...alpha, id: 56, name: 'Aardvark New' }, zebra = { ...beta, id: 78, name: 'Zebra New' };
    setupFetch(() => Promise.resolve(json([zebra, alpha, aardvark, beta])), () => Promise.resolve(json({ sort_mode: 'manual', order: [34, 12], icons: {} })));
    render(); await loaded();
    expect(displayedIDs()).toEqual([34, 12, 56, 78]);
    expect(within(row(beta.name)).getByRole('button', { name: `上移 ${beta.name}`, hidden: true })).toHaveProperty('disabled', true);
    expect(within(row(zebra.name)).getByRole('button', { name: `下移 ${zebra.name}`, hidden: true })).toHaveProperty('disabled', true);
  });

  it('waits for a successful move and preserves expanded rows and manual order across mode switches', async () => {
    const pending = deferred<Response>();
    let writes = 0;
    let preferences = { sort_mode: 'name', order: [] as number[], icons: {} };
    const fetcher = setupFetch(undefined, init => {
      if (!init?.method) return Promise.resolve(json(preferences));
      preferences = { ...preferences, ...JSON.parse(String(init.body)) };
      if (++writes === 1) return pending.promise;
      return Promise.resolve(json(preferences));
    });
    const mounted = render(); await loaded();
    const article = row(), details = modelDetails(article);
    fireEvent.click(details.querySelector('summary')!);
    move(alpha.name, '下移');
    expect(displayWrites(fetcher)).toEqual([{ sort_mode: 'manual', order: [34, 12] }]);
    expect(displayedIDs()).toEqual([12, 34]);
    assertProtected(article, true);
    await act(async () => pending.resolve(json({ sort_mode: 'manual', order: [34, 12], icons: {} })));
    await waitFor(() => expect(displayedIDs()).toEqual([34, 12]));
    expect(row()).toBe(article);
    expect(modelDetails(article)).toBe(details);
    expect(details.open).toBe(true);
    assertProtected(article, false);
    fireEvent.click(screen.getByRole('button', { name: '按名称' }));
    await waitFor(() => expect(displayedIDs()).toEqual([12, 34]));
    expect(preferences.order).toEqual([34, 12]);
    expect(screen.getByRole('button', { name: '按名称' }).getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(screen.getByRole('button', { name: '手动排列' }));
    await waitFor(() => expect(displayedIDs()).toEqual([34, 12]));
    expect(displayWrites(fetcher)).toEqual([{ sort_mode: 'manual', order: [34, 12] }, { sort_mode: 'name' }, { sort_mode: 'manual' }]);
    expect(row()).toBe(article);
    expect(details.open).toBe(true);
    expect(fetcher.mock.calls.filter(([url]) => url === apiPath)).toHaveLength(1);
    expect(fetcher.mock.calls.filter(([, init]) => init?.method && init.method !== 'GET').map(([url]) => url)).toEqual([displayPath, displayPath, displayPath]);
    mounted.unmount();
  });

  it('keeps manual mode after a failed switch and restores its sequence after reloading in name mode', async () => {
    const failed = deferred<Response>();
    let writes = 0;
    let preferences = { sort_mode: 'manual', order: [34, 12], icons: { '12': 'openai' } };
    const fetcher = setupFetch(undefined, init => {
      if (!init?.method) return Promise.resolve(json(preferences));
      if (++writes === 1) return failed.promise;
      preferences = { ...preferences, ...JSON.parse(String(init.body)) };
      return Promise.resolve(json(preferences));
    });
    const mounted = render(); await loaded();
    fireEvent.click(screen.getByRole('button', { name: '按名称' }));
    expect(displayedIDs()).toEqual([34, 12]);
    expect(screen.getByRole('button', { name: '手动排列' }).getAttribute('aria-pressed')).toBe('true');
    await act(async () => failed.reject(new Error('mode save failed')));
    expect((await screen.findByRole('alert')).textContent).toContain('未能切换排序方式');
    expect(displayedIDs()).toEqual([34, 12]);
    fireEvent.click(screen.getByRole('button', { name: '按名称' }));
    await waitFor(() => expect(displayedIDs()).toEqual([12, 34]));
    mounted.unmount(); render(); await loaded();
    expect(screen.getByRole('button', { name: '按名称' }).getAttribute('aria-pressed')).toBe('true');
    expect(displayedIDs()).toEqual([12, 34]);
    fireEvent.click(screen.getByRole('button', { name: '手动排列' }));
    await waitFor(() => expect(displayedIDs()).toEqual([34, 12]));
    expect(preferences.icons).toEqual({ '12': 'openai' });
    expect(displayWrites(fetcher)).toEqual([{ sort_mode: 'name' }, { sort_mode: 'name' }, { sort_mode: 'manual' }]);
  });

  it.each(['button', 'drag'] as const)('reorders only matching channel slots using %s while hidden channels keep their positions', async method => {
    const groupAlpha = { ...alpha, models: [...alpha.models, 'selected-group'] };
    const gamma = { ...alpha, id: 56, name: 'Gamma Group', models: ['selected-group'] };
    const delta = { ...beta, id: 78, name: 'Delta Hidden' };
    const fetcher = setupFetch(() => Promise.resolve(json([delta, gamma, beta, groupAlpha])), init => Promise.resolve(json(init?.method ? { sort_mode: 'manual', order: [56, 34, 12, 78], icons: {} } : { sort_mode: 'manual', order: [12, 34, 56, 78], icons: {} })));
    render(); await loaded();
    fireEvent.change(search(), { target: { value: 'selected-group' } });
    expect(displayedIDs()).toEqual([12, 56]);
    if (method === 'button') move(gamma.name, '上移');
    else {
      const handle = within(row(gamma.name)).getByRole('button', { name: `拖动排序 ${gamma.name}` });
      const release = pointerMove(handle, row());
      expect(row().style.getPropertyValue('--sortable-y')).not.toBe('0px');
      expect(displayWrites(fetcher)).toEqual([]);
      release();
    }
    await waitFor(() => expect(displayWrites(fetcher)).toEqual([{ sort_mode: 'manual', order: [56, 34, 12, 78] }]));
    await waitFor(() => expect(displayedIDs()).toEqual([56, 12]));
    expect(search()).toHaveProperty('value', 'selected-group');
    fireEvent.click(screen.getByRole('button', { name: '清除搜索' }));
    expect(displayedIDs()).toEqual([56, 34, 12, 78]);
  });

  it('rolls pointer preview back on save failure and cancels when search changes', async () => {
    const failed = deferred<Response>();
    const fetcher = setupFetch(undefined, init => init?.method ? failed.promise : Promise.resolve(json({ sort_mode: 'name', order: [], icons: {} })));
    render(); await loaded();
    const article = row(), handle = within(article).getByRole('button', { name: `拖动排序 ${alpha.name}` });
    pointerMove(handle, row(beta.name));
    fireEvent.change(search(), { target: { value: alpha.name } });
    fireEvent.pointerUp(window, { pointerId: 1, clientX: 50, clientY: 175 });
    expect(displayWrites(fetcher)).toEqual([]);
    fireEvent.change(search(), { target: { value: '' } });
    const finish = pointerMove(handle, row(beta.name));
    expect(row(beta.name).style.getPropertyValue('--sortable-y')).toBe('-100px');
    finish();
    expect(displayWrites(fetcher)).toEqual([{ sort_mode: 'manual', order: [34, 12] }]);
    await act(async () => failed.reject(new Error('pointer save rejected')));
    expect((await screen.findByRole('alert')).textContent).toContain('pointer save rejected');
    expect(displayedIDs()).toEqual([12, 34]);
    expect(article.style.getPropertyValue('--sortable-y')).toBe('');
    expect(row(beta.name).style.getPropertyValue('--sortable-y')).toBe('');
  });

  it('starts only from a handle and cancels pointer gestures without writes', async () => {
    const fetcher = setupFetch(undefined, init => Promise.resolve(json({ sort_mode: init?.method ? 'manual' : 'name', order: init?.method ? [34, 12] : [], icons: {} })));
    render(); await loaded();
    const article = row(), handle = within(article).getByRole('button', { name: `拖动排序 ${alpha.name}` });
    expect(article.draggable).toBe(false); expect(handle.draggable).toBe(false);
    measureSortable(article.parentElement!);
    fireEvent.pointerDown(article, { pointerId: 1, button: 0, clientX: 50, clientY: 50 });
    fireEvent.pointerMove(window, { pointerId: 1, clientX: 50, clientY: 175 });
    fireEvent.pointerUp(window, { pointerId: 1, clientX: 50, clientY: 175 });
    expect(displayWrites(fetcher)).toEqual([]);
    const release = pointerMove(handle, row(beta.name));
    fireEvent.pointerUp(window, { pointerId: 2, clientX: 50, clientY: 175 });
    expect(displayWrites(fetcher)).toEqual([]);
    fireEvent.keyDown(window, { key: 'Escape' }); release();
    expect(displayWrites(fetcher)).toEqual([]);
    const finish = pointerMove(handle, row(beta.name));
    finish();
    await waitFor(() => expect(displayWrites(fetcher)).toEqual([{ sort_mode: 'manual', order: [34, 12] }]));
  });

  it('blocks display changes while preferences are loading or unavailable, keeps channel management usable, and recovers on retry', async () => {
    const first = deferred<Response>(), retry = deferred<Response>();
    let gets = 0;
    const fetcher = setupFetch(undefined, () => ++gets === 1 ? first.promise : retry.promise);
    render();
    const article = await screen.findByRole('article', { name: `渠道 ${alpha.name}` });
    assertManagementProtected(article, false);
    assertDisplayProtected(article, true);
    await act(async () => first.reject(new Error('display unavailable')));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('display unavailable');
    expect(alert.textContent).toContain('显示设置未加载');
    expect(alert.textContent).toContain('暂按名称展示');
    assertManagementProtected(article, false);
    assertDisplayProtected(article, true);
    fireEvent.click(within(alert).getByRole('button', { name: '重试' }));
    assertManagementProtected(article, false);
    assertDisplayProtected(article, true);
    await act(async () => retry.resolve(json({ sort_mode: 'manual', order: [34, 12], icons: {} })));
    await loaded();
    expect(displayedIDs()).toEqual([34, 12]);
    expect(screen.queryByRole('alert')).toBeNull();
    assertProtected(article, false);
    expect(displayWrites(fetcher)).toEqual([]);
  });

  it('keeps the previous order on a failed save and can retry the move without updating channel configuration', async () => {
    const failed = deferred<Response>();
    let writes = 0;
    const fetcher = setupFetch(undefined, init => !init?.method ? Promise.resolve(json({ sort_mode: 'name', order: [], icons: {} })) : ++writes === 1 ? failed.promise : Promise.resolve(json({ sort_mode: 'manual', order: [34, 12], icons: {} })));
    render(); await loaded();
    const article = row();
    move(alpha.name, '下移');
    await act(async () => failed.reject(new Error('display save failed')));
    expect((await screen.findByRole('alert')).textContent).toContain('display save failed');
    expect(displayedIDs()).toEqual([12, 34]);
    expect(row()).toBe(article);
    move(alpha.name, '下移');
    await waitFor(() => expect(displayedIDs()).toEqual([34, 12]));
    expect(displayWrites(fetcher)).toEqual([{ sort_mode: 'manual', order: [34, 12] }, { sort_mode: 'manual', order: [34, 12] }]);
    expect(fetcher.mock.calls.filter(([, init]) => init?.method && init.method !== 'GET').every(([url]) => url === displayPath)).toBe(true);
  });

  it('retains the saved order when refreshing display preferences fails and labels it as the previous settings', async () => {
    const failed = deferred<Response>(), retry = deferred<Response>();
    let reads = 0;
    const fetcher = setupFetch(undefined, () => ++reads === 1 ? Promise.resolve(json({ sort_mode: 'manual', order: [34, 12], icons: { '12': 'openai' } })) : reads === 2 ? failed.promise : retry.promise);
    render(); await loaded();
    const article = row(), details = modelDetails(article);
    fireEvent.click(details.querySelector('summary')!);
    fireEvent.click(screen.getByRole('button', { name: '刷新渠道' }));
    await act(async () => failed.reject(new Error('display refresh unavailable')));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('显示设置刷新失败，保留上次设置');
    expect(alert.textContent).toContain('display refresh unavailable');
    expect(alert.textContent).not.toContain('暂按名称展示');
    expect(displayedIDs()).toEqual([34, 12]);
    expect(row()).toBe(article);
    expect(details.open).toBe(true);
    assertManagementProtected(article, false);
    assertDisplayProtected(article, true);
    fireEvent.click(within(alert).getByRole('button', { name: '重试' }));
    await act(async () => retry.resolve(json({ sort_mode: 'manual', order: [34, 12], icons: { '12': 'openai' } })));
    await loaded();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(displayedIDs()).toEqual([34, 12]);
    expect(details.open).toBe(true);
    expect(displayWrites(fetcher)).toEqual([]);
  });

  it('reloads display preferences after deleting their last ordered channel without changing the selected mode', async () => {
    let deleted = false;
    const fetcher = setupFetch((_url, init) => {
      if (init?.method === 'DELETE') { deleted = true; return Promise.resolve(json({})); }
      return Promise.resolve(json(deleted ? [beta] : [alpha, beta]));
    }, init => {
      if (init?.method) throw new Error(`Unexpected display write: ${init.method}`);
      return Promise.resolve(json(deleted ? { sort_mode: 'manual', order: [], icons: {} } : { sort_mode: 'manual', order: [12], icons: { '12': 'openai' } }));
    });
    render(); await loaded();
    expect(screen.getByText('手动排列')).toBeTruthy();
    fireEvent.click(within(row()).getByLabelText(`更多操作 ${alpha.name}`));
    fireEvent.click(within(row()).getByRole('button', { name: `删除 ${alpha.name}` }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '删除渠道？' })).getByRole('button', { name: '确认删除' }));
    await waitFor(() => expect(screen.queryByRole('article', { name: `渠道 ${alpha.name}` })).toBeNull());
    expect(screen.queryByRole('article', { name: `渠道 ${alpha.name}` })).toBeNull();
    expect(displayedIDs()).toEqual([34]);
    expect(screen.getByRole('button', { name: '手动排列' }).getAttribute('aria-pressed')).toBe('true');
    expect(fetcher.mock.calls.filter(([url, init]) => url === displayPath && !init?.method)).toHaveLength(2);
    expect(displayWrites(fetcher)).toEqual([]);
    expect(fetcher.mock.calls.filter(([, init]) => init?.method && init.method !== 'GET').map(([url]) => url)).toEqual(['/api/channels/12']);
  });

  it('persists the picker selection as an icon-only display patch and gives the saved value back to the picker', async () => {
    const fetcher = setupFetch(undefined, init => Promise.resolve(json({ sort_mode: 'name', order: [], icons: init?.method ? { '12': 'openai' } : {} })));
    render(); await loaded();
    const article = row();
    fireEvent.click(within(article).getByRole('button', { name: `更换 ${alpha.name} 图标` }));
    const picker = await screen.findByRole('dialog', { name: `测试图标选择器 ${alpha.name}` });
    fireEvent.click(within(picker).getByRole('button', { name: '选择测试内置图标' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(displayWrites(fetcher)).toEqual([{ icon: { channel_id: 12, value: 'openai' } }]);
    expect(row()).toBe(article);
    expect(fetcher.mock.calls.filter(([url]) => url === apiPath)).toHaveLength(1);
    expect(fetcher.mock.calls.filter(([, init]) => init?.method && init.method !== 'GET').map(([url]) => url)).toEqual([displayPath]);
    fireEvent.click(within(article).getByRole('button', { name: `更换 ${alpha.name} 图标` }));
    expect(within(await screen.findByRole('dialog', { name: `测试图标选择器 ${alpha.name}` })).getByLabelText('当前图标').textContent).toBe('openai');
  });

  it('keeps ordinary enable changes separate from display preferences', async () => {
    let current = alpha;
    const fetcher = setupFetch((_url, init) => {
      if (init?.method === 'PUT') { current = { ...alpha, enabled: false }; return Promise.resolve(json(current)); }
      return Promise.resolve(json([current, beta]));
    });
    render(); await loaded();
    fireEvent.click(within(row()).getByRole('switch', { name: `停用 ${alpha.name}` }));
    await screen.findByRole('switch', { name: `启用 ${alpha.name}` });
    await waitFor(() => expect(screen.getByRole('button', { name: `编辑 ${alpha.name}` })).toHaveProperty('disabled', false));
    expect(displayWrites(fetcher)).toEqual([]);
    expect(fetcher.mock.calls.filter(([, init]) => init?.method && init.method !== 'GET').map(([url]) => url)).toEqual(['/api/channels/12']);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('saves an icon from the editor independently while keeping its unsaved connection draft', async () => {
    const fetcher = setupFetch(undefined, init => Promise.resolve(json({ sort_mode: 'name', order: [], icons: init?.method ? { '12': 'openai' } : {} })));
    render(); await loaded();
    fireEvent.click(within(row()).getByRole('button', { name: `编辑 ${alpha.name}` }));
    const editor = screen.getByRole('dialog', { name: '编辑渠道' });
    fireEvent.change(within(editor).getByLabelText('渠道名称'), { target: { value: 'Unsaved connection name' } });
    fireEvent.click(within(editor).getByRole('button', { name: '更换渠道图标' }));
    const picker = await screen.findByRole('dialog', { name: `测试图标选择器 ${alpha.name}` });
    fireEvent.click(within(picker).getByRole('button', { name: '选择测试内置图标' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: `测试图标选择器 ${alpha.name}` })).toBeNull());
    expect(screen.getByRole('dialog', { name: '编辑渠道' })).toBe(editor);
    expect(within(editor).getByLabelText('渠道名称')).toHaveProperty('value', 'Unsaved connection name');
    expect(displayWrites(fetcher)).toEqual([{ icon: { channel_id: 12, value: 'openai' } }]);
    expect(fetcher.mock.calls.filter(([, init]) => init?.method && init.method !== 'GET').map(([url]) => url)).toEqual([displayPath]);
    fireEvent.click(within(editor).getByRole('button', { name: '取消' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '放弃更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    fireEvent.click(within(row()).getByRole('button', { name: `更换 ${alpha.name} 图标` }));
    expect(within(await screen.findByRole('dialog', { name: `测试图标选择器 ${alpha.name}` })).getByLabelText('当前图标').textContent).toBe('openai');
  });
});
describe('API key channel list interactions', () => {
  it('labels channel IDs clearly, sorts by name then ID, and exposes no channel priority', async () => {
    setupFetch(() => Promise.resolve(json([beta, { ...alpha, id: 19 }, alpha])));
    render();
    await screen.findByRole('article', { name: `渠道 ${beta.name}` });
    const articles = screen.getAllByRole('article');
    expect(displayedIDs()).toEqual([12, 19, 34]);
    for (const article of articles) expect(within(article).queryByText(/^ID \d+$/)).toBeNull();
    expect(articles.map(article => within(article).getByRole('heading').textContent)).toEqual([alpha.name, alpha.name, beta.name]);
    expect(screen.queryByText('#12')).toBeNull();
    expect(screen.queryByText(/优先级/)).toBeNull();
  });

  it.each(['  ALPHA PERSONAL  ', '  12  ', '  OPENAI  ', '  RESPONSES.EXAMPLE  ', '  FALLBACK.EXAMPLE  ', '  ALPHA-MODEL-7  '])('finds a channel using the trimmed, case-insensitive query %s', async query => {
    setupFetch(); render(); await loaded();
    fireEvent.change(search(), { target: { value: query } });
    expect(row()).toBeTruthy();
    expect(screen.getByRole('heading', { name: '1 个匹配 · 共 2 个渠道' })).toBeTruthy();
    expect(screen.queryByRole('article', { name: `渠道 ${beta.name}` })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '清除搜索' }));
    expect(search()).toHaveProperty('value', '');
    expect(screen.getAllByRole('article')).toHaveLength(2);
    expect(screen.getByRole('heading', { name: '2 个渠道 · 1 个启用' })).toBeTruthy();
  });

  it('distinguishes an unmatched search from an empty list, searches enabled models, and restores search on route return', async () => {
    setupFetch(); const mounted = render(); await loaded();
    fireEvent.change(search(), { target: { value: 'catalog-only-model' } });
    expect(screen.getByText('未找到匹配渠道')).toBeTruthy();
    expect(screen.queryByText('连接你的第一个渠道')).toBeNull();
    expect(screen.queryByRole('article')).toBeNull();
    expect(screen.getByRole('heading', { name: '0 个匹配 · 共 2 个渠道' })).toBeTruthy();
    fireEvent.change(search(), { target: { value: '  Alpha  ' } });
    mounted.unmount();
    render(); await loaded();
    expect(search()).toHaveProperty('value', '  Alpha  ');
    expect(screen.queryByRole('article', { name: `渠道 ${beta.name}` })).toBeNull();
    fireEvent.change(search(), { target: { value: '   ' } });
    expect(screen.getAllByRole('article')).toHaveLength(2);
    expect(screen.queryByText('未找到匹配渠道')).toBeNull();
  });

  it('shows initial loading, retains the same row, search and expanded models during refresh, then adopts the fresh list', async () => {
    const first = deferred<Response>(), refresh = deferred<Response>();
    let calls = 0;
    setupFetch(() => ++calls === 1 ? first.promise : refresh.promise);
    render();
    expect(screen.getByText('正在加载数据…')).toBeTruthy();
    expect(screen.queryByRole('article')).toBeNull();
    await act(async () => first.resolve(json([alpha, beta]))); await loaded();
    fireEvent.change(search(), { target: { value: 'Alpha' } });
    const article = row(), details = modelDetails(article);
    expect(details.open).toBe(false);
    fireEvent.click(details.querySelector('summary')!);
    expect(details.open).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: '刷新渠道' }));
    expect(screen.getByText('正在刷新…')).toBeTruthy();
    expect(row()).toBe(article);
    expect(details.open).toBe(true);
    expect(search()).toHaveProperty('value', 'Alpha');
    assertProtected(article, true);
    await act(async () => refresh.resolve(json([{ ...alpha, enabled: false }, beta])));
    await waitFor(() => expect(screen.queryByText('正在刷新…')).toBeNull());
    expect(row()).toBe(article);
    expect(details.open).toBe(true);
    expect(search()).toHaveProperty('value', 'Alpha');
    expect(within(article).getByRole('switch', { name: `启用 ${alpha.name}` }).getAttribute('aria-checked')).toBe('false');
    assertProtected(article, false, false);
  });

  it('keeps the last list read-only after a failed refresh, permits copying its effective address, and recovers on retry', async () => {
    const failed = deferred<Response>(), retry = deferred<Response>();
    let calls = 0;
    const fetcher = setupFetch(() => ++calls === 1 ? Promise.resolve(json([alpha, beta])) : calls === 2 ? failed.promise : retry.promise);
    render(); await loaded();
    const article = row();
    fireEvent.click(screen.getByRole('button', { name: '刷新渠道' }));
    await act(async () => failed.reject(new Error('refresh unavailable')));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', expect.stringContaining('refresh unavailable'));
    expect(row()).toBe(article);
    assertProtected(article, true);
    for (const name of [`编辑 ${alpha.name}`, `测试渠道 ${alpha.name}`]) fireEvent.click(within(article).getByRole('button', { name }));
    fireEvent.click(within(article).getByRole('switch', { name: `停用 ${alpha.name}` }));
    fireEvent.click(within(article).getByRole('button', { name: `删除 ${alpha.name}`, hidden: true }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(fetcher.mock.calls.some(([, init]) => init?.method && init.method !== 'GET')).toBe(false);
    fireEvent.click(within(article).getByRole('button', { name: `复制 ${alpha.name} 上游地址` }));
    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledWith('https://fallback.example/v1'));
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(row()).toBe(article);
    assertProtected(article, true);
    await act(async () => retry.resolve(json([alpha, beta])));
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
    assertProtected(article, false);
  });

  it('labels the preferred effective address separately from protocols and copies each channel destination accurately', async () => {
    setupFetch(); render(); await loaded();
    const article = row();
    const address = within(article).getByText('首选地址').parentElement!;
    expect(within(address).getByText('https://fallback.example/v1')).toBeTruthy();
    const others = within(address).getByText('另 1 个地址');
    expect(others.getAttribute('title')).toBe('OpenAI Chat Completions：https://fallback.example/v1\nOpenAI Responses：https://responses.example/v1');
    expect(within(address).queryByText('OpenAI Chat Completions')).toBeNull();
    expect(within(article).getByText('OpenAI Chat Completions')).toBeTruthy();
    expect(within(article).getByText('OpenAI Responses')).toBeTruthy();
    fireEvent.click(within(article).getByRole('button', { name: `复制 ${alpha.name} 上游地址` }));
    fireEvent.click(within(row(beta.name)).getByRole('button', { name: `复制 ${beta.name} 上游地址` }));
    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenNthCalledWith(2, 'https://beta.example/v1'));
    expect(navigator.clipboard.writeText).toHaveBeenNthCalledWith(1, 'https://fallback.example/v1');
    const details = modelDetails(article);
    fireEvent.click(details.querySelector('summary')!);
    expect(details.open).toBe(true);
    for (const model of alpha.models) expect(within(details).getByText(model)).toBeTruthy();
  });

  it('puts deletion inside more actions and waits for the existing confirmation before writing', async () => {
    const fetcher = setupFetch((_url, init) => Promise.resolve(json(init?.method === 'DELETE' ? {} : [alpha, beta])));
    render(); await loaded();
    const article = row();
    expect(within(article).getByRole('button', { name: `编辑 ${alpha.name}` })).toBeTruthy();
    expect(within(article).getByRole('button', { name: `测试渠道 ${alpha.name}` })).toBeTruthy();
    const summary = within(article).getByLabelText(`更多操作 ${alpha.name}`);
    const more = summary.closest('details')!;
    expect(more.open).toBe(false);
    expect(within(more).getByRole('button', { name: `删除 ${alpha.name}`, hidden: true })).toBeTruthy();
    fireEvent.click(summary);
    expect(more.open).toBe(true);
    fireEvent.click(within(more).getByRole('button', { name: `删除 ${alpha.name}` }));
    const dialog = await screen.findByRole('dialog', { name: '删除渠道？' });
    expect(fetcher.mock.calls.some(([, init]) => init?.method === 'DELETE')).toBe(false);
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    expect(fetcher.mock.calls.some(([, init]) => init?.method === 'DELETE')).toBe(false);
    fireEvent.click(summary);
    fireEvent.click(within(more).getByRole('button', { name: `删除 ${alpha.name}` }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '删除渠道？' })).getByRole('button', { name: '确认删除' }));
    await waitFor(() => expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'DELETE')).toHaveLength(1));
    expect(fetcher.mock.calls.find(([, init]) => init?.method === 'DELETE')![0]).toBe('/api/channels/12');
  });

  it('reuses only the matching view snapshot while switching and ignores late responses', async () => {
    const firstOAuth = deferred<Response>(), secondAPI = deferred<Response>(), lateOAuth = deferred<Response>(), thirdAPI = deferred<Response>();
    let apiCalls = 0, oauthCalls = 0;
    const fetcher = setupFetch(url => {
      if (url === apiPath) return ++apiCalls === 1 ? Promise.resolve(json([alpha, beta])) : apiCalls === 2 ? secondAPI.promise : thirdAPI.promise;
      if (url === '/api/channels?credential_type=copilot_oauth') return ++oauthCalls === 1 ? firstOAuth.promise : lateOAuth.promise;
      if (oauthPaths.includes(url)) return Promise.resolve(json([]));
      throw new Error(`Unexpected channel request: ${url}`);
    });
    render(); await loaded();
    fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    expect(screen.queryByRole('article', { name: `渠道 ${alpha.name}` })).toBeNull();
    expect(screen.queryByRole('region', { name: `渠道 ${oauth.name}` })).toBeNull();
    await act(async () => firstOAuth.resolve(json([oauth])));
    fireEvent.click(await screen.findByRole('tab', { name: 'Copilot' }));
    await screen.findByRole('region', { name: `渠道 ${oauth.name}` });
    fireEvent.click(screen.getByRole('button', { name: 'API Key 渠道' }));
    expect(screen.getAllByRole('article')).toHaveLength(2);
    expect(screen.queryByText('正在加载数据…')).toBeNull();
    assertProtected(row(), true);
    expect(screen.queryByText(oauth.name)).toBeNull();
    await act(async () => secondAPI.resolve(json([alpha, beta]))); await loaded();
    fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    expect(screen.getByRole('region', { name: `渠道 ${oauth.name}` })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'API Key 渠道' }));
    const retainedAPI = row();
    expect(screen.getAllByRole('article')).toHaveLength(2);
    await act(async () => lateOAuth.resolve(json([oauth])));
    expect(row()).toBe(retainedAPI);
    expect(screen.queryByRole('region', { name: `渠道 ${oauth.name}` })).toBeNull();
    await act(async () => thirdAPI.resolve(json([alpha, beta]))); await loaded();
    expect(screen.queryByText(oauth.name)).toBeNull();
    expect(fetcher.mock.calls.filter(([url]) => url.startsWith('/api/channels?credential_type=')).map(([url]) => url)).toEqual([
      apiPath, ...oauthPaths, apiPath, ...oauthPaths, apiPath,
    ]);
  });
});


it('opens the existing editor from the name and focuses its credential section from the count', async () => {
  setupFetch(); render(); await loaded();
  fireEvent.click(within(row()).getByRole('button', { name: `编辑渠道名称 ${alpha.name}` }));
  let dialog = screen.getByRole('dialog');
  expect(within(dialog).getByDisplayValue(alpha.name)).toBeTruthy();
  fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
  fireEvent.click(within(row()).getByRole('button', { name: `配置 ${alpha.name} 凭据` }));
  dialog = screen.getByRole('dialog');
  expect(within(dialog).getByRole('region', { name: '凭据池与调度策略' })).toBe(document.activeElement);
  expect(within(dialog).getByDisplayValue(alpha.name)).toBeTruthy();
});


it('retains confirmed credential order and clears pointer pending when the follow-up channel read fails, then retries', async () => {
  localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ active: 'copilot_oauth' }));
  const saving = deferred<Response>(), failedRead = deferred<Response>();
  const original: Channel = { ...oauth, generation: 42, credentials: [
    { ...oauth.credentials![0], id: 'first', label: 'First' },
    { ...oauth.credentials![0], id: 'second', label: 'Second' },
  ] };
  let persisted = original, reads = 0, writes = 0;
  const fetcher = setupFetch((url, init) => {
    if (url === '/api/channels/61/credential-order' && init?.method === 'PUT') {
      const ids = JSON.parse(String(init.body)).credential_ids as string[];
      persisted = { ...persisted, credentials: ids.map(id => persisted.credentials!.find(credential => credential.id === id)!) };
      return ++writes === 1 ? saving.promise : Promise.resolve(json({ ok: true }));
    }
    if (url === '/api/channels?credential_type=copilot_oauth') return ++reads === 2 ? failedRead.promise : Promise.resolve(json([persisted]));
    if (url === apiPath) return Promise.resolve(json([alpha, beta]));
    return Promise.resolve(json([]));
  });
  render(); await loaded();
  fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
  const first = await screen.findByRole('article', { name: '凭证 First' });
  await waitFor(() => expect(screen.getByRole('button', { name: '拖动凭证 Second' })).toHaveProperty('disabled', false));
  const list = screen.getByLabelText(`${oauth.name} 凭证列表`);
  const ids = () => Array.from(list.children).map(item => (item as HTMLElement).dataset.credentialId);
  const finish = pointerMove(screen.getByRole('button', { name: '拖动凭证 Second' }), first); finish();
  expect(list.querySelector('[data-sortable-state="pending"]')).not.toBeNull();
  expect(ids()).toEqual(['first', 'second']);
  await act(async () => saving.resolve(json({ ok: true })));
  await waitFor(() => expect(ids()).toEqual(['second', 'first']));
  expect(list.querySelector('[data-sortable-state="pending"]')).toBeNull();
  expect(list.hasAttribute('data-sortable-active')).toBe(false);
  expect(screen.getByText('凭证调用顺序已保存')).toBeTruthy();
  await act(async () => failedRead.reject(new Error('channel refresh unavailable')));
  const alert = await screen.findByRole('alert');
  expect(alert.textContent).toContain('刷新失败，已保留当前数据');
  expect(alert.textContent).toContain('channel refresh unavailable');
  expect(alert.textContent).not.toContain('保存失败');
  expect(ids()).toEqual(['second', 'first']);
  expect(list.querySelector('[data-sortable-state="pending"]')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '重试' }));
  await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
  await waitFor(() => expect(screen.getByRole('button', { name: '拖动凭证 First' })).toHaveProperty('disabled', false));
  pointerMove(screen.getByRole('button', { name: '拖动凭证 First' }), screen.getByRole('article', { name: '凭证 Second' }))();
  await waitFor(() => expect(ids()).toEqual(['first', 'second']));
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT').map(([, init]) => JSON.parse(String(init?.body)))).toEqual([
    { credential_ids: ['second', 'first'] }, { credential_ids: ['first', 'second'] },
  ]);
  expect(list.querySelector('[data-sortable-state="pending"]')).toBeNull();
});


it('does not restore an old credential pool when an external read replaced its IDs before the order save completed', async () => {
  localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ active: 'copilot_oauth' }));
  const saving = deferred<Response>();
  const original: Channel = { ...oauth, generation: 42, credentials: [
    { ...oauth.credentials![0], id: 'first', label: 'First' },
    { ...oauth.credentials![0], id: 'second', label: 'Second' },
  ] };
  const external: Channel = { ...original, name: 'Externally edited', generation: 77, enabled: false, credentials: [
    { ...original.credentials![0], label: 'Retained first' },
    { ...original.credentials![1], id: 'replacement', label: 'Replacement' },
  ] };
  let reads = 0;
  setupFetch((url, init) => {
    if (url === '/api/channels/61/credential-order' && init?.method === 'PUT') return saving.promise;
    if (url === '/api/channels?credential_type=copilot_oauth') return ++reads === 1 ? Promise.resolve(json([original])) : reads === 2 ? Promise.resolve(json([external])) : Promise.reject(new Error('confirm read unavailable'));
    if (url === apiPath) return Promise.resolve(json([alpha, beta]));
    return Promise.resolve(json([]));
  });
  render(); await loaded(); fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
  const first = await screen.findByRole('article', { name: '凭证 First' });
  await waitFor(() => expect(screen.getByRole('button', { name: '拖动凭证 Second' })).toHaveProperty('disabled', false));
  pointerMove(screen.getByRole('button', { name: '拖动凭证 Second' }), first)();
  fireEvent.click(screen.getByRole('button', { name: '刷新渠道' }));
  await screen.findByRole('article', { name: '凭证 Replacement' });
  await act(async () => saving.resolve(json({ ok: true })));
  await screen.findByRole('alert');
  const list = screen.getByLabelText('Externally edited 凭证列表');
  expect(Array.from(list.children).map(item => (item as HTMLElement).dataset.credentialId)).toEqual(['first', 'replacement']);
  expect(screen.getByRole('article', { name: '凭证 Retained first' })).toBeTruthy();
  expect(screen.getByRole('switch', { name: '启用 Externally edited' }).getAttribute('aria-checked')).toBe('false');
  expect(screen.queryByRole('article', { name: '凭证 Second' })).toBeNull();
  expect(list.querySelector('[data-sortable-state="pending"]')).toBeNull();
});


it('focuses the exact credential from a quota deep link without selecting the first credential', async () => {
  history.replaceState(null, '', '#/channels?channel_id=12&credential_id=fixture-secondary');
  const channel = { ...alpha, credentials: [{ id: 'fixture-primary', label: '工作凭证', enabled: true, has_key: true }, { id: 'fixture-secondary', label: '备用凭证', enabled: true, has_key: true }] };
  setupFetch(() => Promise.resolve(json([channel])));
  render();
  await screen.findByRole('dialog');
  await waitFor(() => expect((document.activeElement as HTMLElement)?.dataset.credentialId).toBe('fixture-secondary'));
});

it('opens the discovery modal from its deep link and preselects the source without scanning', async () => {
  history.replaceState(null, '', '/#/channels?discover=1&source=source-one');
  const fetcher = setupFetch();
  render();
  await screen.findByRole('dialog', { name: '发现凭证' });
  expect(await screen.findByRole('checkbox', { name: '选择来源 Source One' })).toHaveProperty('checked', true);
  expect(fetcher.mock.calls.some(([url]) => url === '/api/key-discovery/sessions')).toBe(false);
  expect(fetcher.mock.calls.some(([url]) => String(url).endsWith('/scan'))).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
});

it('displays supported API key balances per credential and original currency without an account total', async () => {
  const underlying = setupFetch();
  const base = { channel_id: alpha.id, channel_name: alpha.name, kind: 'balance', enabled: true, windows: [], stale: false, checked_at: new Date().toISOString() };
  const items = [{ ...base, credential_id: 'fixture-primary', credential_name: '工作凭证', balances: [{ currency: 'CNY', total: '0.00' }, { currency: 'USD', total: '1.25' }] }, { ...base, credential_id: 'fixture-secondary', credential_name: '备用凭证', balances: [], checked_at: undefined, stale: true }];
  const fetcher = vi.fn((url: string, init?: RequestInit) => url === '/api/quotas' ? Promise.resolve(json({ items, refresh_seconds: 0 })) : underlying(url, init));
  vi.stubGlobal('fetch', fetcher); render(); await loaded();
  const section = within(row()).getByRole('region', { name: '凭证余额' });
  expect(within(section).queryByText('0.00')).toBeNull();
  fireEvent.click(within(section).getByRole('button', { name: /凭证余额/ }));
  expect(await within(section).findByText('0.00')).toBeTruthy();
  expect(within(section).getByText('1.25')).toBeTruthy(); expect(within(section).getByText('余额 —')).toBeTruthy();
  expect(within(section).getByText('工作凭证')).toBeTruthy(); expect(within(section).getByText('备用凭证')).toBeTruthy();
  expect(fetcher.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false);
  expect(within(row(beta.name)).queryByRole('region', { name: '凭证余额' })).toBeNull();
});
