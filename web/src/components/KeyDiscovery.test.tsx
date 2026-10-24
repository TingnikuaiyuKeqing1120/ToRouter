// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { KeyDiscovery } from './KeyDiscovery';

// File decoding and icon catalog behavior are covered by the picker tests.
vi.mock('./ChannelIconPicker', () => ({ ChannelIconPicker: ({ name, baseUrl, value, onSelect, onClose }: { name: string; baseUrl: string; value: string; onSelect: (value: string) => Promise<void>; onClose: () => void }) => <section role="dialog" aria-label="测试图标选择器"><output>{name}|{baseUrl}|{value}</output><button onClick={async () => { await onSelect('openai'); onClose(); }}>选择测试图标</button></section> }));
vi.mock('./ProviderIcons', () => ({ ProviderIcon: ({ name, baseUrl, override }: { name: string; baseUrl?: string; override?: string }) => <span data-testid="provider-icon" data-name={name} data-url={baseUrl} data-override={override} /> }));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const sources = [
  { id: 'source-one', name: 'Source One', icon: 'claudecode', enabled: true },
  { id: 'source-two', name: 'Source Two', icon: 'codex', enabled: true },
  { id: 'source-disabled', name: 'Source Three', icon: 'opencode', enabled: false },
];
const target = { id: 7, name: 'Saved channel', version: 'version-7' };
const row = (id: string, extra = {}) => ({ id, masked_key: `masked-${id}`, sources: [{ id: 'source-one', name: 'Source One', icon: 'claudecode' }, { id: 'source-two', name: 'Source Two', icon: 'codex' }], status: 'new', disabled: false, formats: ['openai_responses'], targets: [], credential_type: 'api_key', ...extra });
const group = (id: string, rows = [row(id)], extra = {}) => ({ id, url_display: `https://${id}.example/v1`, formats: ['openai_responses'], rows, existing: false, target_count: 0, copy_only: false, targets: [], display_icon: 'generic', ...extra });
const groups = [
  group('new', [row('new-a'), row('new-b')]),
  group('saved', [row('saved-new', { targets: [target] }), row('saved-existing', { status: 'exists' })], { existing: true, target_count: 1, targets: [target], display_icon: 'deepseek' }),
  group('ambiguous', [row('copy-oauth', { credential_type: 'codex_oauth', masked_key: 'OAuth ••••synthetic' })], { existing: true, target_count: 2, copy_only: true, targets: [target, { ...target, id: 8 }] }),
  group('all-existing', [row('old', { status: 'exists', disabled: true })], { existing: true, target_count: 1, targets: [target] }),
];
const preview = (id = 'session-one', value = groups, status = 'complete') => ({ id, expires_at: '2026-10-24T00:00:00Z', sources: sources.slice(0, 2).map(source => ({ id: source.id, name: source.name, status, diagnostics: [] as { code: string; message: string }[] })), groups: value });
function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
function setup(override?: (url: string, init?: RequestInit) => Promise<Response> | undefined, initialSourceID?: string) {
  const fetcher = vi.fn((url: string, init?: RequestInit): Promise<Response> => {
    const custom = override?.(url, init); if (custom) return custom;
    if (url === '/api/key-discovery/sources') return Promise.resolve(json({ sources }));
    if (url === '/api/key-discovery/sessions') return Promise.resolve(json(preview('session-one', [], 'pending')));
    if (url.endsWith('/scan')) return Promise.resolve(json(preview()));
    if (init?.method === 'DELETE') return Promise.resolve(json({}));
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  const onClose = vi.fn(), onChanged = vi.fn();
  const view = render(<KeyDiscovery initialSourceID={initialSourceID} onClose={onClose} onChanged={onChanged} />);
  return { ...view, fetcher, onClose, onChanged };
}
const choose = (name = 'Source One') => screen.getByRole('checkbox', { name: `选择来源 ${name}` });
const start = () => fireEvent.click(screen.getAllByRole('button', { name: '开始扫描' })[0]);
const selected = (id: string) => screen.getByRole('checkbox', { name: `选择凭证 masked-${id}` });
const add = (count: number) => screen.getByRole('button', { name: `创建渠道并添加 (${count})` });
async function scanDone() {
  await screen.findByRole('checkbox', { name: '选择凭证 masked-new-a' });
  await waitFor(() => expect(add(3)).toHaveProperty('disabled', false));
}

beforeEach(() => {
  localStorage.clear(); sessionStorage.clear();
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: vi.fn().mockResolvedValue(undefined) } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('binds a local scan and its import to the explicitly chosen second channel', async () => {
  const locked = { id: 8, name: 'Second pool', version: 'second-version' };
  const scoped = () => ({ ...preview(), target: locked, groups: [group('scoped', [row('scoped', { credential_type: 'codex_oauth', targets: [target, locked] })], { targets: [target, locked], target_count: 2 })] });
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url.endsWith('/sources')) return Promise.resolve(json({ sources: [{ id: 'source-codex', name: 'Codex', enabled: true, icon: 'codex' }, { id: 'source-claude', name: 'Claude', enabled: true, icon: 'claude' }] }));
    if (url === '/api/key-discovery/sessions') return Promise.resolve(json({ ...scoped(), groups: [] }));
    if (url.endsWith('/scan')) return Promise.resolve(json(scoped()));
    if (url.endsWith('/import')) return Promise.resolve(json({ results: [{ channel_id: 8, added: 1, skipped: 0, status: 'added' }], preview: { ...scoped(), groups: [] } }));
    if (init?.method === 'DELETE') return Promise.resolve(json({}));
    throw new Error('unexpected fixture request');
  });
  vi.stubGlobal('fetch', fetcher);
  render(<KeyDiscovery initialSourceID="source-codex" credentialType="codex_oauth" targetChannelID={8} onClose={vi.fn()} />);
  await screen.findByText('Codex'); expect(screen.queryByText('Claude')).toBeNull(); start();
  await screen.findByRole('button', { name: '添加到当前渠道 (1)' });
  expect(screen.queryByLabelText(/渠道名称/)).toBeNull();
  expect(screen.getByText('添加到「Second pool」')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '添加到当前渠道 (1)' }));
  await screen.findByText('已追加 1 个凭证');
  const begin = fetcher.mock.calls.find(([url]) => url === '/api/key-discovery/sessions')!;
  expect(JSON.parse(String(begin[1]?.body))).toEqual({ source_ids: ['source-codex'], credential_type: 'codex_oauth', target_channel_id: 8 });
  const imported = fetcher.mock.calls.find(([url]) => url.endsWith('/import'))!;
  expect(JSON.parse(String(imported[1]?.body))).toEqual({ items: [{ candidate_id: 'scoped', target_channel_id: 8, expected_version: 'second-version', display_icon: '' }] });
});

it('creates channels for all compatible selected groups regardless of zero, one or multiple existing URL targets', async () => {
  const value = [group('zero'), group('one', [row('one')], { existing: true, target_count: 1, targets: [target] }), group('multiple', [row('multiple')], { existing: true, target_count: 2, targets: [target, { ...target, id: 8 }] })];
  const view = setup(url => url.endsWith('/scan') ? Promise.resolve(json(preview('session-one', value))) : url.endsWith('/import') ? Promise.resolve(json({ results: [], preview: preview('session-one', []) })) : undefined);
  await screen.findByText('Source One'); start(); await screen.findByRole('button', { name: '创建渠道并添加 (3)' });
  expect(screen.queryByRole('button', { name: '复制凭证与信息' })).toBeNull();
  fireEvent.click(add(3));
  await waitFor(() => expect(view.fetcher.mock.calls.some(([url]) => url.endsWith('/import'))).toBe(true));
  const body = JSON.parse(String(view.fetcher.mock.calls.find(([url]) => url.endsWith('/import'))![1]?.body));
  expect(body.items.map((item: { target_channel_id: number; enabled: boolean }) => [item.target_channel_id, item.enabled])).toEqual([[0, true], [0, true], [0, true]]);
});

it('shows only tool choices, defaults to enabled tools, and never scans or grants scopes on opening', async () => {
  const { fetcher } = setup();
  await screen.findByText('Source One');
  expect(screen.getByRole('dialog', { name: '发现凭证' })).toBeTruthy();
  expect(choose()).toHaveProperty('checked', true);
  expect(choose('Source Two')).toHaveProperty('checked', true);
  expect(choose('Source Three')).toHaveProperty('checked', false);
  expect(choose('Source Three')).toHaveProperty('disabled', false);
  expect(screen.queryByRole('textbox')).toBeNull();
  expect(screen.queryByRole('link')).toBeNull();
  expect(screen.queryByText(/目录|授权|路径/)).toBeNull();
  expect(screen.getAllByRole('button', { name: '开始扫描' })).toHaveLength(2);
  expect(fetcher.mock.calls.map(([url]) => url)).toEqual(['/api/key-discovery/sources']);
});

it('remembers source selection and keeps an explicit deep-linked source selected without scanning', async () => {
  localStorage.setItem('to-router.discovery-sources.v1', JSON.stringify(['source-two']));
  const view = setup(); await screen.findByText('Source One');
  expect(choose()).toHaveProperty('checked', false);
  expect(choose('Source Two')).toHaveProperty('checked', true);
  fireEvent.click(choose());
  expect(JSON.parse(localStorage.getItem('to-router.discovery-sources.v1')!)).toEqual(['source-two', 'source-one']);
  view.unmount();
  const linked = setup(undefined, 'source-disabled');
  await screen.findByText('Source Three');
  expect(choose('Source Three')).toHaveProperty('checked', true);
  expect(choose()).toHaveProperty('checked', false);
  expect(linked.fetcher.mock.calls).toHaveLength(1);
});

it('enables an unchecked disabled tool inline before selecting it without per-source authorization or scanning', async () => {
  const view = setup(url => {
    if (url === '/api/plugins') return Promise.resolve(json({ version: 'plugins-v1', items: [] }));
    if (url === '/api/plugins/source-disabled/capabilities/discover_api_key') return Promise.resolve(json({ version: 'plugins-v2', items: [] }));
  });
  await screen.findByText('Source Three');
  fireEvent.click(choose('Source Three'));
  await waitFor(() => expect(choose('Source Three')).toHaveProperty('checked', true));
  expect(view.fetcher).toHaveBeenCalledWith('/api/plugins/source-disabled/capabilities/discover_api_key', expect.objectContaining({ method: 'PUT', body: '{"enabled":true,"expected_version":"plugins-v1"}' }));
  fireEvent.click(choose('Source Three'));
  expect(choose('Source Three')).toHaveProperty('checked', false);
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
  expect(view.fetcher.mock.calls.some(([url]) => url.includes('/scopes') || url.includes('/sessions'))).toBe(false);
});

it('keeps a disabled source unchecked if inline enablement fails and can retry', async () => {
  let fail = true;
  const view = setup(url => {
    if (url === '/api/plugins') return Promise.resolve(json({ version: 'current-version', items: [] }));
    if (url.includes('/capabilities/')) return Promise.resolve(fail ? json({ error: 'synthetic internal path' }, 500) : json({}));
  });
  await screen.findByText('Source Three'); fireEvent.click(choose('Source Three'));
  await screen.findByText('启用工具未完成，请重试。');
  expect(choose('Source Three')).toHaveProperty('checked', false);
  expect(screen.queryByText('synthetic internal path')).toBeNull();
  fail = false; fireEvent.click(choose('Source Three'));
  await waitFor(() => expect(choose('Source Three')).toHaveProperty('checked', true));
  expect(view.fetcher.mock.calls.some(([url]) => url.endsWith('/scan'))).toBe(false);
});

it('switches to scan progress, scans each selected source once, then defaults to selecting new credentials', async () => {
  const first = deferred<Response>(), second = deferred<Response>();
  let calls = 0;
  const view = setup(url => url.endsWith('/scan') ? (++calls === 1 ? first.promise : second.promise) : undefined);
  await screen.findByText('Source One'); start();
  expect(screen.queryByRole('region', { name: '扫描来源' })).toBeNull();
  expect(screen.getByText(/正在扫描/)).toBeTruthy();
  await waitFor(() => expect(calls).toBe(1));
  await act(async () => first.resolve(json(preview())));
  expect(screen.queryByText('https://new.example/v1')).toBeNull();
  await waitFor(() => expect(calls).toBe(2));
  await act(async () => second.resolve(json(preview()))); await scanDone();
  expect(view.fetcher).toHaveBeenCalledWith('/api/key-discovery/sessions', expect.objectContaining({ method: 'POST', body: '{"source_ids":["source-one","source-two"]}' }));
  expect(view.fetcher.mock.calls.filter(([url]) => url.endsWith('/scan')).map(([, init]) => JSON.parse(String(init?.body)).source_id)).toEqual(['source-one', 'source-two']);
  expect(selected('new-a')).toHaveProperty('checked', true);
  expect(selected('new-b')).toHaveProperty('checked', true);
  expect(screen.getByRole('checkbox', { name: '选择凭证 OAuth ••••synthetic' })).toHaveProperty('checked', true);
  expect(screen.queryByRole('combobox')).toBeNull();
  expect(screen.queryByText('masked-saved-existing')).toBeNull();
  expect(screen.queryByText('https://all-existing.example/v1')).toBeNull();
  const card = within(screen.getByRole('article', { name: '凭证组 https://new.example/v1' }));
  expect(card.getAllByLabelText('来源 Source One')[0].getAttribute('title')).toBe('Source One');
  expect(card.getAllByLabelText('来源 Source Two')[0].getAttribute('title')).toBe('Source Two');
  expect(card.queryByText('Source One')).toBeNull();
  const icon = within(card.getByRole('button', { name: '更换 https://new.example/v1 图标' })).getByTestId('provider-icon');
  expect(icon.dataset.url).toBe('https://new.example/v1'); expect(icon.dataset.override).toBe(''); expect(icon.dataset.name).toBe('');
});

it('derives group selection from rows and creates new enabled channels even for a URL with an existing target', async () => {
  const view = setup(url => url.endsWith('/import') ? Promise.resolve(json({ results: [{ candidate_id: 'new-a', channel_id: 21, added: 1, skipped: 0, status: 'added' }], preview: preview('session-one', []) })) : undefined);
  await screen.findByText('Source One'); start(); await scanDone();
  const newURL = screen.getByRole('checkbox', { name: '选择凭证组 https://new.example/v1' });
  const savedURL = screen.getByRole('checkbox', { name: '选择凭证组 https://saved.example/v1' });
  expect(savedURL).toHaveProperty('checked', true); expect(savedURL).toHaveProperty('disabled', false);
  fireEvent.click(selected('new-b')); expect(newURL).toHaveProperty('checked', false); expect(newURL).toHaveProperty('indeterminate', true);
  fireEvent.click(newURL); expect(selected('new-b')).toHaveProperty('checked', true);
  fireEvent.click(newURL); expect(selected('new-a')).toHaveProperty('checked', false); expect(newURL).toHaveProperty('indeterminate', false);
  fireEvent.click(selected('new-a'));
  fireEvent.change(screen.getByLabelText('渠道名称 https://new.example/v1'), { target: { value: 'Fresh pool' } });
  fireEvent.click(screen.getByRole('button', { name: '更换 https://new.example/v1 图标' }));
  fireEvent.click(screen.getByRole('button', { name: '选择测试图标' }));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: '测试图标选择器' })).toBeNull());
  fireEvent.click(add(2)); await screen.findByText(/已创建 1 个渠道，添加 1 个凭证/);
  expect(view.onChanged).toHaveBeenCalledTimes(1); expect(view.onClose).not.toHaveBeenCalled();
  const sent = JSON.parse(String(view.fetcher.mock.calls.find(([url]) => url.endsWith('/import'))![1]?.body));
  expect(sent.items).toEqual([
    { candidate_id: 'new-a', target_channel_id: 0, expected_version: '', enabled: true, display_icon: 'openai', name: 'Fresh pool' },
    { candidate_id: 'saved-new', target_channel_id: 0, expected_version: '', enabled: true, display_icon: 'deepseek' },
  ]);
  expect(screen.queryByRole('checkbox', { name: /启用渠道/ })).toBeNull();
});

it('selects all new rows including disabled source credentials, but excludes copy-only cards from the add count', async () => {
  setup(url => url.endsWith('/scan') ? Promise.resolve(json(preview('session-one', [...groups, group('blocked', [row('blocked', { disabled: true })])]))) : undefined);
  await screen.findByText('Source One'); start(); await screen.findByRole('button', { name: '创建渠道并添加 (4)' });
  expect(selected('blocked')).toHaveProperty('disabled', false); expect(selected('blocked')).toHaveProperty('checked', true);
  expect(screen.getByText('已选 4 个可添加凭证 · 1 个需复制')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '清空' }));
  expect(add(0)).toHaveProperty('disabled', true);
  expect(selected('new-a')).toHaveProperty('checked', false);
  expect(screen.getByRole('checkbox', { name: '选择凭证组 https://new.example/v1' })).toHaveProperty('checked', false);
  fireEvent.click(screen.getByRole('button', { name: '全选' }));
  expect(add(4)).toHaveProperty('disabled', false);
  expect(selected('blocked')).toHaveProperty('checked', true);
});

it('requests raw material only on explicit copy and puts its structured JSON on the clipboard without rendering it', async () => {
  const payload = { items: [{ credential_type: 'codex_oauth', url: 'https://ambiguous.example/v1', formats: ['openai_responses'], sources: [{ id: 'source-one', name: 'Source One', icon: 'codex' }], filename: 'auth.json', content: '{"access_token":"synthetic-material"}' }] };
  const view = setup(url => url.endsWith('/copy') ? Promise.resolve(json(payload)) : undefined);
  await screen.findByText('Source One'); start(); await scanDone();
  expect(view.fetcher.mock.calls.some(([url]) => url.endsWith('/copy'))).toBe(false);
  expect(screen.getByText('此来源材料暂不可直接添加，可显式复制。')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '复制凭证与信息' }));
  await screen.findByRole('button', { name: '已复制' });
  expect(view.fetcher).toHaveBeenCalledWith('/api/key-discovery/sessions/session-one/copy', expect.objectContaining({ method: 'POST', body: '{"candidate_ids":["copy-oauth"]}' }));
  expect(navigator.clipboard.writeText).toHaveBeenCalledWith(JSON.stringify(payload, null, 2));
  expect(document.body.textContent).not.toContain('synthetic-material');
  expect(view.fetcher.mock.calls.some(([url]) => url.endsWith('/import'))).toBe(false);
});

it('returns to tool selection and scans into a fresh session while preserving chosen tools', async () => {
  let sessions = 0;
  const view = setup(url => {
    if (url === '/api/key-discovery/sessions') return Promise.resolve(json(preview(`session-${++sessions}`, [], 'pending')));
    if (url.endsWith('/scan')) return Promise.resolve(json(preview(`session-${sessions}`)));
  }, 'source-one');
  await screen.findByText('Source One'); start(); await scanDone();
  fireEvent.click(screen.getByRole('button', { name: '返回上一步' }));
  expect(choose()).toHaveProperty('checked', true); expect(choose('Source Two')).toHaveProperty('checked', false);
  expect(screen.queryByRole('region', { name: '发现结果' })).toBeNull();
  expect(view.fetcher).toHaveBeenCalledWith('/api/key-discovery/sessions/session-1', expect.objectContaining({ method: 'DELETE' }));
  start(); await scanDone(); expect(sessions).toBe(2);
});

it('aborts a cancelled scan, ignores its late result and does not overwrite a newer session', async () => {
  const late = deferred<Response>(); let sessions = 0, scanSignal: AbortSignal | null | undefined;
  const view = setup((url, init) => {
    if (url === '/api/key-discovery/sessions') return Promise.resolve(json(preview(`session-${++sessions}`, [], 'pending')));
    if (url.includes('/session-1/scan')) { scanSignal = init?.signal; return late.promise; }
    if (url.endsWith('/scan')) return Promise.resolve(json(preview('session-2')));
  }, 'source-one');
  await screen.findByText('Source One'); start();
  await waitFor(() => expect(scanSignal).toBeTruthy());
  fireEvent.click(screen.getByRole('button', { name: '取消扫描' }));
  expect(scanSignal?.aborted).toBe(true); expect(choose()).toBeTruthy();
  start(); await scanDone();
  await act(async () => late.resolve(json(preview('session-1', [group('late')]))));
  expect(screen.queryByText('https://late.example/v1')).toBeNull();
  expect(selected('new-a')).toHaveProperty('checked', true);
  expect(add(3)).toHaveProperty('disabled', false);
  expect(view.fetcher).toHaveBeenCalledWith('/api/key-discovery/sessions/session-1', expect.objectContaining({ method: 'DELETE' }));
});

it('deletes a session whose begin response arrives after returning to the previous step', async () => {
  const late = deferred<Response>();
  const view = setup(url => url === '/api/key-discovery/sessions' ? late.promise : undefined);
  await screen.findByText('Source One'); start();
  fireEvent.click(screen.getByRole('button', { name: '返回上一步' }));
  await act(async () => late.resolve(json(preview('late-begin', []))));
  expect(choose()).toBeTruthy();
  expect(view.fetcher).toHaveBeenCalledWith('/api/key-discovery/sessions/late-begin', expect.objectContaining({ method: 'DELETE' }));
  expect(view.fetcher.mock.calls.some(([url]) => url.endsWith('/scan'))).toBe(false);
});

it('retains selected rows after an import failure and retries the same automatic destination', async () => {
  let fail = true;
  const view = setup(url => url.endsWith('/import') ? Promise.resolve(fail ? json({ error: 'synthetic private error' }, 500) : json({ results: [{ candidate_id: 'new-a', added: 3, skipped: 0, status: 'added' }], preview: preview('session-one', []) })) : undefined);
  await screen.findByText('Source One'); start(); await scanDone();
  fireEvent.click(add(3)); await screen.findByText('添加未完成，所选凭证已保留，请重试。');
  expect(view.onChanged).not.toHaveBeenCalled();
  expect(selected('new-a')).toHaveProperty('checked', true);
  expect(screen.queryByText('synthetic private error')).toBeNull();
  fail = false; fireEvent.click(add(3)); await screen.findByText(/添加 3 个凭证/);
  expect(view.onChanged).toHaveBeenCalledTimes(1);
  const writes = view.fetcher.mock.calls.filter(([url]) => url.endsWith('/import'));
  expect(writes).toHaveLength(2); expect(writes[0][1]?.body).toEqual(writes[1][1]?.body);
});

it('clears an invalid preview after an import conflict and requires a new scan', async () => {
  setup(url => url.endsWith('/import') ? Promise.resolve(json({ error: 'conflict' }, 409)) : undefined);
  await screen.findByText('Source One'); start(); await scanDone();
  fireEvent.click(add(3)); await screen.findByText('预览或目标已变化，请返回上一步重新扫描。');
  expect(add(0)).toHaveProperty('disabled', true);
  expect(screen.queryByText('masked-new-a')).toBeNull();
});

it('closes promptly during a scan and ignores completion after unmount', async () => {
  const late = deferred<Response>(); let signal: AbortSignal | null | undefined;
  const view = setup((url, init) => url.endsWith('/scan') ? (signal = init?.signal, late.promise) : undefined);
  await screen.findByText('Source One'); start(); await waitFor(() => expect(signal).toBeTruthy());
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  expect(view.onClose).toHaveBeenCalledTimes(1); expect(signal?.aborted).toBe(true);
  view.unmount();
  await act(async () => late.resolve(json(preview())));
  expect(view.fetcher).toHaveBeenCalledWith('/api/key-discovery/sessions/session-one', expect.objectContaining({ method: 'DELETE' }));
});

it('keeps a failed source notice after another source succeeds and does not render parser error details', async () => {
  setup((url, init) => {
    if (url.endsWith('/scan') && JSON.parse(String(init?.body)).source_id === 'source-one') return Promise.resolve(json({ error: 'synthetic private parser detail' }, 500));
    if (url.endsWith('/scan')) return Promise.resolve(json({ ...preview(), sources: preview().sources.map(source => ({ ...source, diagnostics: null })) }));
  });
  await screen.findByText('Source One'); start(); await scanDone();
  expect(screen.getByText('Source One：扫描失败，请返回重试')).toBeTruthy();
  expect(document.body.textContent).not.toContain('synthetic private parser detail');
});

it('treats a missing tool configuration as an ordinary concise empty result', async () => {
  setup(url => url.endsWith('/scan') ? Promise.resolve(json({ ...preview('session-one', []), sources: [{ id: 'source-one', name: 'Source One', status: 'complete', diagnostics: [{ code: 'not_found', message: 'Unread synthetic path' }] }] })) : undefined, 'source-one');
  await screen.findByText('Source One'); start();
  await screen.findByText('未发现新增凭证。');
  expect(screen.queryByText('Source One：未找到配置')).toBeNull();
  expect(screen.getByText('未发现新增凭证。')).toBeTruthy();
  expect(screen.queryByText('Unread synthetic path')).toBeNull();
  expect(add(0)).toHaveProperty('disabled', true);
});
