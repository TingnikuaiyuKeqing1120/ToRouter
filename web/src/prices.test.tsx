// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { ConfirmProvider } from './components/Confirm';
import { type Price, type PriceTable, type PriceRule } from './api';
import { blankPrice } from './hooks/usePriceWorkbench';
import Prices from './pages/Prices';
import App from './App';
import Models from './pages/Models';
import Settings from './pages/Settings';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });
function showAllPrices() { const all = screen.queryByRole('checkbox', { name: '查看全部模型' }) as HTMLInputElement | null; if (all && !all.checked) fireEvent.click(all); }
function renderAllPrices() { const result = render(<Prices />); showAllPrices(); return result; }
async function recoverPricePage() { cleanup(); await act(async () => { renderAllPrices(); }); showAllPrices(); }
async function confirmPriceSave() { fireEvent.click(await screen.findByRole('button', { name: '确认保存' })); }
async function channelTab() { fireEvent.click(screen.getByRole('button', { name: '渠道价格（非套餐）' })); await screen.findByRole('button', { name: /选择渠道 ·/ }); }
const response = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
function price(model: string, change: Partial<Price> = {}, tableChange: Partial<PriceTable> = {}): Price {
  const { key: _key, ...row } = blankPrice(0, 'CNY', 1);
  if (row.selection.mode !== 'manual') throw new Error('blank price must be manual');
  return { ...row, model, selection: { mode: 'manual', table: { ...row.selection.table, ...tableChange } }, ...change };
}
function table(row: Price): PriceTable { if (row.selection.mode !== 'manual') throw new Error('expected manual price'); return row.selection.table; }
const rates = (change: Partial<PriceTable['rates']> = {}): PriceTable['rates'] => ({ input: null, output: null, cache_read: null, cache_write: null, ...change });
const overnightRule = (): PriceRule => ({ enabled: false, when: { time_window: { start: '22:00', end: '06:00', all_day: false, timezone: 'Asia/Shanghai' } }, action: { multiplier: 0.5 } });
function addRule(condition: 'context' | 'time') {
  const toggle = screen.getByLabelText(condition === 'context' ? '按上下文长度计费' : '按时段计费') as HTMLInputElement;
  if (!toggle.checked) fireEvent.click(toggle);
  fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
}
function fixedRuleRate(segment: string, label: string, value: string) {
  fireEvent.change(screen.getByLabelText(`${segment} ${label.replace('缓存输入', '缓存读').replace('缓存输出', '缓存写')}`), { target: { value } });
}
const channel = { id: 7, name: 'Vendor', models: ['up-a'], available_models: ['up-a', 'up-b'], api_key: '', has_key: true, enabled: true, endpoints: [{ format: 'openai_chat_completions', base_url: 'https://vendor.example' }] };
const rate = { available: true, base: 'USD', quote: 'CNY', rate: 7, date: '2026-10-05', attempted_on: '2026-10-05', fetched_at: '2026-10-05T00:00:00Z', stale: false, error: '', source_url: 'https://frankfurter.dev/' };
const model = { id: 'public-a', display_name: 'Alpha', enabled: true, fallback: true, config: {}, bindings: [{ channel_id: 7, model: 'up-a', priority: 0, enabled: true }] };
function backend(initial: Price[] = [], exchange: unknown = rate) {
  let rows = initial, catalogue = [model], channels = [channel];
  const fetcher = vi.fn().mockImplementation((url: string, options: RequestInit = {}) => {
    if (url === '/api/prices') { if (options.method === 'PUT') rows = JSON.parse(String(options.body)); return Promise.resolve(response(rows)); }
    if (url === '/api/channels') return Promise.resolve(response(channels));
    if (url === '/api/models') return Promise.resolve(response(catalogue));
    if (url === '/api/catalog/models?source=all') return Promise.resolve(response([]));
    if (url.startsWith('/api/models/') && options.method === 'PUT') { catalogue = [JSON.parse(String(options.body))]; return Promise.resolve(response(catalogue[0])); }
    if (url === '/api/exchange-rate') return Promise.resolve(response(exchange));
    if (url === '/api/auth/session') return Promise.resolve(response({}));
    if (url.startsWith('/api/logs')) return Promise.resolve(response([]));
    if (url === '/api/settings') return Promise.resolve(response({ gateway_key: 'admin', prices: rows, retention_days: 30, max_attempts: 3, retries_per_channel: 5, failure_cooldown_seconds: 10, max_tokens_default: 4096 }));
    if (url === '/api/runtime/port') return Promise.resolve(response({ port: 17878, origin: 'http://127.0.0.1:17878' }));
    return Promise.resolve(response({}, 404));
  });
  return { fetcher, rows: () => rows, catalogue: (next: typeof catalogue) => { catalogue = next; }, channels: (next: typeof channels) => { channels = next; } };
}
const puts = (b: ReturnType<typeof backend>) => b.fetcher.mock.calls.filter(call => call[0] === '/api/prices' && call[1].method === 'PUT').map(call => JSON.parse(String(call[1].body)) as Price[]);
beforeEach(() => { localStorage.clear(); sessionStorage.clear(); window.location.hash = '#/prices'; Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function(this: HTMLDialogElement) { this.setAttribute('open', ''); } }); Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value: function(this: HTMLDialogElement) { this.removeAttribute('open'); } }); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('compact price workbench', () => {
  it('shows enabled models by default, preserves disabled prices on pull/save and offers all model management', async () => {
    const b = backend([price('public-a'), price('public-off', { origin: 'auto' }), price('up-off', { channel_id: 7, origin: 'auto' })]);
    b.catalogue([model, { ...model, id: 'public-off', enabled: false }]);
    b.channels([{ ...channel, enabled: false, models: ['up-a'], available_models: ['up-a', 'up-off', 'discovered-only'] }]);
    vi.stubGlobal('fetch', b.fetcher); render(<Prices />);
    await screen.findByLabelText('模型 ID 0:public-a');
    expect(screen.queryByLabelText('模型 ID 0:public-off')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '拉取当前模型' })).toHaveProperty('disabled', false));
    fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '4' } });
    fireEvent.click(screen.getByRole('button', { name: '保存价格' }));
    await waitFor(() => expect(b.rows().map(row => row.model)).toEqual(['public-a', 'public-off', 'up-off']));
    fireEvent.click(screen.getByRole('checkbox', { name: '查看全部模型' }));
    expect(screen.getByLabelText('模型 ID 0:public-off')).toBeTruthy();
    expect(screen.getByText('未启用 / 已移除')).toBeTruthy();
    fireEvent.click(screen.getByRole('checkbox', { name: '查看全部模型' }));
    fireEvent.click(screen.getByRole('button', { name: '渠道价格（非套餐）' }));
    await screen.findByRole('button', { name: /选择渠道 ·/ });
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' }));
    await screen.findByLabelText('模型 ID 7:up-a');
    expect(screen.queryByLabelText('模型 ID 7:up-off')).toBeNull();
    expect(screen.queryByLabelText('模型 ID 7:discovered-only')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '保存价格' }));
    await waitFor(() => expect(b.rows().map(row => row.model)).toEqual(['public-a', 'public-off', 'up-off', 'up-a']));
    fireEvent.click(screen.getByRole('checkbox', { name: '查看全部模型' }));
    expect(screen.getByLabelText('模型 ID 7:up-off')).toBeTruthy();
  });
  it('adds only enabled public model candidates even when viewing all saved prices', async () => {
    const b = backend(); b.catalogue([model, { ...model, id: 'public-off', enabled: false }]);
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByText('暂无模型');
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' }));
    await screen.findByLabelText('模型 ID 0:public-a');
    expect(screen.queryByLabelText('模型 ID 0:public-off')).toBeNull();
  });
  it('keeps a manually added or renamed price draft visible in the enabled default view', async () => {
    const b = backend([price('public-a')]); vi.stubGlobal('fetch', b.fetcher); render(<Prices />);
    await screen.findByLabelText('模型 ID 0:public-a');
    fireEvent.change(screen.getByLabelText('模型 ID 0:public-a'), { target: { value: 'changed-id' } });
    expect(screen.getByLabelText('模型 ID 0:changed-id')).toHaveProperty('value', 'changed-id');
    fireEvent.click(screen.getByRole('button', { name: '手工添加价格' }));
    expect(screen.getAllByPlaceholderText('模型 ID')).toHaveLength(2);
    expect(screen.getByRole('checkbox', { name: '查看全部模型' })).toHaveProperty('checked', false);
  });

  it('opens an encoded channel target independently of the persisted channel selection', async () => {
    history.replaceState(null, '', '/#/prices?channel_id=7&model=vendor%2Fa%2Bb');
    localStorage.setItem('to-router.price-channels.v1', JSON.stringify([8]));
    const b = backend([price('vendor/a+b', { channel_id: 7 }), price('other', { channel_id: 7 }), price('vendor/a+b', { channel_id: 8 })]);
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices();
    await screen.findByLabelText('模型 ID 7:vendor/a+b');
    expect(screen.queryByLabelText('模型 ID 8:vendor/a+b')).toBeNull(); expect(screen.queryByLabelText('模型 ID 7:other')).toBeNull();
    expect(screen.getByLabelText('搜索价格')).toHaveProperty('value', 'vendor/a+b'); expect(puts(b)).toHaveLength(0);
  });
  it('preserves a public reference deep link through App and defaults new focused rows to that model', async () => {
    history.replaceState(null, '', '/#/prices?channel_id=0&model=target%2Fpublic');
    const b = backend(); vi.stubGlobal('fetch', b.fetcher); render(<App />);
    await screen.findByText('暂无模型');
    expect(window.location.hash).toBe('#/prices?channel_id=0&model=target%2Fpublic');
    expect(screen.getByLabelText('搜索价格')).toHaveProperty('value', 'target/public');
    fireEvent.click(screen.getByRole('button', { name: '手工添加价格' }));
    expect(await screen.findByLabelText('模型 ID 0:target/public')).toHaveProperty('value', 'target/public');
  });
  it('waits for a price save while retaining the target query for hash navigation', async () => {
    const b = backend([price('public-a')]); const normal = b.fetcher.getMockImplementation()!;
    b.channels([{ ...channel, models: ['vendor/a+b'] }]);
    b.fetcher.mockImplementation((url, options) => url === '/api/model-specs' ? Promise.resolve(response([{ channel_id: 7, channel_name: 'Vendor', model: 'vendor/a+b', specs: {} }, { channel_id: 8, channel_name: 'Other', model: 'other', specs: {} }])) : normal(url, options));
    vi.stubGlobal('fetch', b.fetcher); render(<App />); await screen.findByLabelText('输入 0:public-a');
    fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '3' } });
    await act(async () => { window.location.hash = '#/model-specs?channel_id=7&model=vendor%2Fa%2Bb'; window.dispatchEvent(new HashChangeEvent('hashchange')); });
    await confirmPriceSave(); await screen.findByRole('button', { name: '编辑规格 7:vendor/a+b' });
    expect(screen.queryByRole('button', { name: '编辑规格 8:other' })).toBeNull();
    expect(window.location.hash).toBe('#/model-specs?channel_id=7&model=vendor%2Fa%2Bb'); expect(table(b.rows()[0]).rates.input).toBe(3);
  });
  it('imports official names and only enabled channel models without discovering upstream models', async () => {
    const b = backend(); b.channels([{ ...channel, available_models: ['up-a', 'up-b', 'up-c', 'up-d'] }]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByText('暂无模型');
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' })); await screen.findByLabelText('模型 ID 0:public-a');
    expect((screen.getByLabelText('显示名 0:public-a') as HTMLInputElement).value).toBe('Alpha');
    await channelTab();
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' })); await screen.findByLabelText('模型 ID 7:up-a');
    for (const disabled of ['up-b', 'up-c', 'up-d']) expect(screen.queryByLabelText(`模型 ID 7:${disabled}`)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '保存价格' })); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(puts(b)[0][0]).toMatchObject({ channel_id: 0, model: 'public-a', origin: 'auto', selection: { mode: 'manual', table: { rates: { input: null } } } });
    expect(b.fetcher.mock.calls.some(call => /discover|oauth\/models/.test(call[0]))).toBe(false);
    expect(JSON.parse(localStorage.getItem('to-router.price-channels.v1')!)).toEqual([7]);
  });
  it('persists a channel subset through remount and imports only its enabled models', async () => {
    const b = backend(); b.channels([channel, { ...channel, id: 8, name: 'Other', models: ['other-model'], available_models: [] }]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByText('暂无模型');
    await channelTab();
    fireEvent.click(screen.getByText(/选择渠道 ·/)); fireEvent.click(screen.getByLabelText('选择渠道 Other')); fireEvent.click(screen.getByRole('button', { name: '完成选择' })); fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' }));
    await screen.findByLabelText('模型 ID 7:up-a'); expect(screen.queryByLabelText('模型 ID 8:other-model')).toBeNull();
    cleanup(); renderAllPrices(); await screen.findByText('暂无模型'); await channelTab();
    fireEvent.click(screen.getByText(/选择渠道 ·/)); expect(screen.getByLabelText('选择渠道 Vendor').getAttribute('aria-pressed')).toBe('true'); expect(screen.getByLabelText('选择渠道 Other').getAttribute('aria-pressed')).toBe('false');
    expect(JSON.parse(localStorage.getItem('to-router.price-channels.v1')!)).toEqual([7]);
  });
  it('imports enabled manual model IDs across selected channels and deduplicates within each channel', async () => {
    const b = backend(); b.channels([
      { ...channel, models: ['up-a', 'up-a', 'enabled-manual'] },
      { ...channel, id: 8, name: 'Other', enabled: false, models: ['up-a', 'other-model', 'other-model'], available_models: ['up-a', 'other-model', 'off-other'] },
    ]);
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByText('暂无模型');
    await channelTab();
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' })); await screen.findByLabelText('模型 ID 7:enabled-manual');
    expect(screen.getAllByPlaceholderText('模型 ID').map(input => input.getAttribute('aria-label'))).toEqual(['模型 ID 7:up-a', '模型 ID 7:enabled-manual', '模型 ID 8:up-a', '模型 ID 8:other-model']);
    fireEvent.click(screen.getByRole('button', { name: '保存价格' })); await waitFor(() => expect(b.rows()).toHaveLength(4));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存价格' })).toHaveProperty('disabled', true));
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' })); await waitFor(() => expect(screen.getByRole('button', { name: '拉取当前模型' })).toHaveProperty('disabled', false));
    expect(screen.getAllByPlaceholderText('模型 ID')).toHaveLength(4); expect(puts(b)).toHaveLength(1);
    expect(screen.getByRole('button', { name: '保存价格' })).toHaveProperty('disabled', true);
    expect(b.fetcher.mock.calls.some(call => /discover|oauth\/models/.test(call[0]))).toBe(false);
  });
  it('pulls no disabled models from an empty enabled list and preserves configured, manual and unselected rows after saving', async () => {
    const b = backend([
      price('up-b', { channel_id: 7, origin: 'auto' }),
      price('off-zero', { channel_id: 7, origin: 'auto' }, { rates: rates({ input: 0 }) }),
      price('off-rule', { channel_id: 7, origin: 'auto' }, { rules: [overnightRule()] }),
      price('off-unit', { channel_id: 7, origin: 'auto' }, { unit_tokens: 1000 }),
      price('off-manual', { channel_id: 7, display_name: 'Saved Manual' }),
      price('unselected-empty', { channel_id: 8, origin: 'auto' }),
    ]);
    b.channels([{ ...channel, models: [], available_models: [] }, { ...channel, id: 8, name: 'Other', models: [] }]);
    localStorage.setItem('to-router.price-channels.v1', JSON.stringify([7]));
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByText('暂无模型');
    fireEvent.click(screen.getByRole('button', { name: '渠道价格（非套餐）' })); await screen.findByLabelText('模型 ID 7:up-b');
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' })); await waitFor(() => expect(screen.queryByLabelText('模型 ID 7:up-b')).toBeNull());
    expect(screen.queryByLabelText('模型 ID 7:up-a')).toBeNull(); expect(screen.getByLabelText('输入 7:off-zero')).toHaveProperty('value', '0');
    expect(screen.getByLabelText('模型 ID 7:off-rule')).toBeTruthy(); expect(screen.getByLabelText('模型 ID 7:off-unit')).toBeTruthy();
    expect(screen.getByLabelText('显示名 7:off-manual')).toHaveProperty('value', 'Saved Manual');
    fireEvent.click(screen.getByRole('button', { name: '保存价格' })); await waitFor(() => expect(b.rows().map(row => `${row.channel_id}:${row.model}`)).toEqual(['7:off-zero', '7:off-rule', '7:off-unit', '7:off-manual', '8:unselected-empty']));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存价格' })).toHaveProperty('disabled', true));
    expect(localStorage.getItem('to-router.price-workbench.draft.v2')).toBeNull();
    cleanup(); renderAllPrices(); await screen.findByText('暂无模型'); fireEvent.click(screen.getByRole('button', { name: '渠道价格（非套餐）' })); await screen.findByLabelText('输入 7:off-zero');
    expect(screen.queryByLabelText('模型 ID 7:up-b')).toBeNull(); expect(screen.getByLabelText('输入 7:off-zero')).toHaveProperty('value', '0');
    expect(screen.getByLabelText('显示名 7:off-manual')).toHaveProperty('value', 'Saved Manual');
    fireEvent.click(screen.getByRole('button', { name: '高级 7:off-rule' })); expect(screen.getByLabelText('按时段计费')).toHaveProperty('checked', false);
    expect(table(b.rows().find(row => row.model === 'off-rule')!).rules).toEqual([overnightRule()]);
    expect(table(b.rows().find(row => row.model === 'off-unit')!).unit_tokens).toBe(1000); expect(puts(b)).toHaveLength(1);
  });
  it('reloads only absent unpriced auto rows while preserving zero, rules and manual rows', async () => {
    const b = backend([price('gone-empty', { origin: 'auto' }), price('gone-zero', { origin: 'auto' }, { rates: rates({ input: 0 }) }), price('gone-rule', { origin: 'auto' }, { rules: [overnightRule()] }), price('public-a', { display_name: 'Manual Name' })]);
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('模型 ID 0:gone-empty');
    fireEvent.click(screen.getByRole('button', { name: '拉取当前模型' })); await waitFor(() => expect(screen.queryByLabelText('模型 ID 0:gone-empty')).toBeNull());
    expect(screen.getByLabelText('输入 0:gone-zero')).toHaveProperty('value', '0'); expect(screen.getByLabelText('模型 ID 0:gone-rule')).toBeTruthy();
    expect(screen.getByLabelText('显示名 0:public-a')).toHaveProperty('value', 'Manual Name');
    fireEvent.click(screen.getByRole('button', { name: '保存价格' })); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(puts(b)[0].find(row => row.model === 'public-a')!.origin).toBe('manual');
  });
  it('searches name and ID and persists manual unfinished rows without sending empty IDs', async () => {
    const b = backend([price('public-a', { display_name: 'Alpha' })]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('模型 ID 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '手工添加价格' })); const empty = screen.getAllByPlaceholderText('模型 ID').find(input => (input as HTMLInputElement).value === '')!;
    const label = empty.getAttribute('aria-label')!.replace('模型 ID', '显示名'); fireEvent.change(screen.getByLabelText(label), { target: { value: 'Draft Name' } });
    fireEvent.click(screen.getByRole('button', { name: '保存价格' })); expect(puts(b)).toHaveLength(0);
    cleanup(); renderAllPrices(); await screen.findByDisplayValue('Draft Name');
    fireEvent.change(screen.getByLabelText('搜索价格'), { target: { value: 'Alpha' } }); expect(screen.getByLabelText('模型 ID 0:public-a')).toBeTruthy(); expect(screen.queryByDisplayValue('Draft Name')).toBeNull();
  });
  it('switches display currency without rewriting prices, then converts only an edited row including fixed rules', async () => {
    const b = backend([price('public-a', {}, { currency: 'USD', rates: rates({ input: 1, output: 2 }), rules: [{ enabled: true, when: { input_range: { from_tokens: 0, to_tokens: null } }, action: { rate_changes: { input: 3, output: null, cache_read: 0, cache_write: null } } }] })]);
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a'); await waitFor(() => expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '7'));
    fireEvent.change(screen.getByLabelText('页面币种'), { target: { value: 'USD' } }); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '1'); expect(puts(b)).toHaveLength(0);
    fireEvent.change(screen.getByLabelText('页面币种'), { target: { value: 'CNY' } }); fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '14' } });
    fireEvent.click(screen.getByRole('button', { name: '保存价格' })); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(table(puts(b)[0][0])).toMatchObject({ currency: 'CNY', rates: { input: 14, output: 14 } }); expect(table(puts(b)[0][0]).rules[0].action.rate_changes).toMatchObject({ input: 21, cache_read: 0, output: null });
  });
  it('shows native advanced currency and converts only when explicitly choosing CNY or USD', async () => {
    const b = backend([price('public-a', {}, { currency: 'USD', rates: rates({ input: 1, output: 2 }) })]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
    expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '1'); expect(screen.getByLabelText('高级行币种')).toHaveProperty('value', 'USD');
    expect(screen.getAllByRole('option').filter(option => option.closest('select') === screen.getByLabelText('高级行币种')).map(option => option.textContent)).toEqual(['人民币（CNY）', '美元（USD）']);
    fireEvent.change(screen.getByLabelText('高级行币种'), { target: { value: 'CNY' } }); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '7');
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '14' } }); fireEvent.change(screen.getByLabelText('高级行币种'), { target: { value: 'USD' } });
    expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '2'); fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(table(puts(b)[0][0])).toMatchObject({ currency: 'USD', rates: { input: 2, output: 2 } }); expect(puts(b)[0][0]).not.toHaveProperty('currency_override');
  });
  it.each(['CNY', 'USD'] as const)('syncs an ordinary field edit into a saved %s advanced draft without losing other draft values', async source => {
    const b = backend([price('public-a', {}, { currency: source, rates: rates({ input: 1, output: 2 }) })]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); fireEvent.change(screen.getByLabelText('基础 输出'), { target: { value: '9' } });
    addRule('context'); fireEvent.change(screen.getByLabelText('分段 1 终止 Token'), { target: { value: '32000' } }); fireEvent.change(screen.getByLabelText('分段 1 计费方式'), { target: { value: 'fixed' } }); fixedRuleRate('分段 1', '输入', '4'); fixedRuleRate('分段 1', '缓存输入', '0');
    addRule('context'); fireEvent.change(screen.getByLabelText('分段 2 起始 Token'), { target: { value: '32001' } }); fireEvent.change(screen.getByLabelText('分段 2 计费方式'), { target: { value: 'fixed' } }); fixedRuleRate('分段 2', '输出', '5'); await recoverPricePage();
    fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '14' } }); fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
    const factor = source === 'USD' ? 7 : 1;
    expect(screen.getByLabelText('高级行币种')).toHaveProperty('value', 'CNY'); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '14'); expect(screen.getByLabelText('基础 输出')).toHaveProperty('value', String(9 * factor));
    expect(screen.getByLabelText('分段 1 输入')).toHaveProperty('value', String(4 * factor)); expect(screen.getByLabelText('分段 1 缓存读')).toHaveProperty('value', '0'); expect(screen.getByLabelText('分段 2 输出')).toHaveProperty('value', String(5 * factor));
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(1)); expect(table(puts(b)[0][0])).toMatchObject({ currency: 'CNY', rates: { input: 14, output: 9 * factor }, rules: [{ enabled: true, when: { input_range: { from_tokens: 0, to_tokens: 32000 } } }, { enabled: true, when: { input_range: { from_tokens: 32001, to_tokens: null } } }] });
    expect(table(puts(b)[0][0]).rules[0].action.rate_changes).toEqual({ input: 4 * factor, output: 9 * factor, cache_read: 0, cache_write: null }); expect(table(puts(b)[0][0]).rules[1].action.rate_changes).toEqual({ input: factor, output: 5 * factor, cache_read: null, cache_write: null });
  });
  it('requires a successful initial load before writes and retries the cache read', async () => {
    const b = backend([price('public-a', {}, { rates: rates({ input: 1 }) })]); const normal = b.fetcher.getMockImplementation()!; let fail = true;
    b.fetcher.mockImplementation((url, options) => url === '/api/prices' && !options.method && fail ? Promise.resolve(response({ error: { message: 'prices unavailable' } }, 503)) : normal(url, options));
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByText('prices unavailable');
    expect(screen.getByRole('button', { name: '手工添加价格' })).toHaveProperty('disabled', true); expect(screen.getByRole('button', { name: '保存价格' })).toHaveProperty('disabled', true); expect(puts(b)).toHaveLength(0);
    fail = false; fireEvent.click(screen.getByRole('button', { name: '重试' })); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '手工添加价格' })); const empty = screen.getAllByPlaceholderText('模型 ID').find(input => (input as HTMLInputElement).value === '')!;
    fireEvent.change(empty, { target: { value: 'manual-new' } }); fireEvent.click(screen.getByRole('button', { name: '保存价格' })); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(puts(b)[0].map(row => row.model)).toEqual(['public-a', 'manual-new']);
    expect(b.fetcher.mock.calls.filter(call => call[0] === '/api/prices' && !call[1].method)).toHaveLength(2);
  });
  it('keeps base prices and rule switches independent with explicit ranges and overnight windows', async () => {
    const b = backend([price('public-a')]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
    expect(screen.queryByLabelText('分段 1 起始 Token')).toBeNull(); expect(screen.queryByLabelText('分段 1 开始')).toBeNull(); expect(screen.queryByLabelText('分档方式')).toBeNull();
    fireEvent.change(screen.getByLabelText('计价单位'), { target: { value: '1000' } }); addRule('context');
    fireEvent.change(screen.getByLabelText('分段 1 终止 Token'), { target: { value: '32000' } }); addRule('context');
    fireEvent.change(screen.getByLabelText('分段 2 起始 Token'), { target: { value: '32001' } }); fireEvent.change(screen.getByLabelText('分段 2 计费方式'), { target: { value: 'fixed' } }); fixedRuleRate('分段 2', '输入', '0');
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(table(puts(b)[0][0])).toMatchObject({ unit_tokens: 1000, rates: { input: null }, rules: [{ enabled: true }, { enabled: true }] });
    expect(table(puts(b)[0][0]).rules.slice(0, 2).map(rule => [rule.when.input_range!.from_tokens, rule.when.input_range!.to_tokens])).toEqual([[0, 32000], [32001, null]]);
    expect(table(puts(b)[0][0]).rules[1].action.rate_changes).toEqual({ input: 0, output: null, cache_read: null, cache_write: null });
    expect(screen.getByText(/项条件/)).toBeTruthy(); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '');
    expect(screen.getAllByRole('row')).toHaveLength(2);
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
    fireEvent.click(screen.getByLabelText('按上下文长度计费')); fireEvent.click(screen.getByLabelText('按时段计费'));
    expect(screen.queryByLabelText('分段 1 终止 Token')).toBeNull(); expect(screen.queryByLabelText('分段 1 开始')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(2));
    expect(table(puts(b)[1][0]).rules).toEqual(table(puts(b)[0][0]).rules.map(rule => ({ ...rule, enabled: false })));
    expect(screen.queryByText('有')).toBeNull(); expect(screen.getByText('无')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); fireEvent.click(screen.getByLabelText('按上下文长度计费')); expect(screen.getByLabelText('分段 1 终止 Token')).toHaveProperty('value', '32000'); expect(screen.getByLabelText('分段 2 起始 Token')).toHaveProperty('value', '32001'); expect(screen.getByLabelText('分段 2 输入')).toHaveProperty('value', '0');
  });
  it('restores a manual row without automatically opening its saved advanced draft', async () => {
    const b = backend(); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByText('暂无模型');
    fireEvent.click(screen.getByRole('button', { name: '手工添加价格' })); fireEvent.click(screen.getByRole('button', { name: '高级 0:草稿1' }));
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '2' } }); await recoverPricePage();
    cleanup(); await act(async () => { renderAllPrices(); }); showAllPrices();
    expect(screen.getByLabelText('模型 ID 0:草稿1')).toHaveProperty('value', '');
    expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '高级 0:草稿1' })); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '2');
  });
  it('keeps advanced drafts across remount without auto-opening and displays the page currency separately', async () => {
    const b = backend([price('public-a', {}, { currency: 'USD', rates: rates({ input: 1 }) })]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); addRule('context');
    cleanup(); await act(async () => { renderAllPrices(); }); showAllPrices(); expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); expect(screen.getByLabelText('按上下文长度计费')).toHaveProperty('checked', true); expect(screen.getByLabelText('分段 1 起始 Token')).toHaveProperty('value', '0'); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '1');
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(table(puts(b)[0][0]).currency).toBe('USD'); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '7');
    fireEvent.change(screen.getByLabelText('页面币种'), { target: { value: 'USD' } }); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '1');
  });
  it('keeps independent advanced drafts for two rows and applies or discards only the current row', async () => {
    const b = backend([price('public-a'), price('public-b')]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); addRule('context'); await recoverPricePage();
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-b' })); fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '5' } }); await recoverPricePage();
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); expect(screen.getByLabelText('按上下文长度计费')).toHaveProperty('checked', true); expect(screen.getByLabelText('分段 1 起始 Token')).toHaveProperty('value', '0'); fireEvent.click(screen.getByRole('button', { name: '放弃草稿' }));
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-b' })); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '5'); await recoverPricePage();
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); expect(screen.getByLabelText('按上下文长度计费')).toHaveProperty('checked', false); addRule('context'); fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-b' })); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '5');
  });
  it.each([true, false])('keeps an advanced draft when a row gains or changes its model ID and display name (empty=%s)', async empty => {
    const b = backend(empty ? [] : [price('public-a')]); vi.stubGlobal('fetch', b.fetcher); renderAllPrices();
    if (empty) { await screen.findByText('暂无模型'); fireEvent.click(screen.getByRole('button', { name: '手工添加价格' })); }
    else await screen.findByLabelText('模型 ID 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: empty ? '高级 0:草稿1' : '高级 0:public-a' }));
    addRule('context'); fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '2' } }); await recoverPricePage();
    fireEvent.change(screen.getByLabelText(empty ? '模型 ID 0:草稿1' : '模型 ID 0:public-a'), { target: { value: 'renamed-id' } });
    fireEvent.change(screen.getByLabelText('显示名 0:renamed-id'), { target: { value: 'Renamed Model' } });
    fireEvent.click(screen.getByRole('button', { name: '高级 0:renamed-id' })); expect(screen.getByRole('heading', { name: '高级价格 · Renamed Model' })).toBeTruthy(); expect(screen.getByLabelText('按上下文长度计费')).toHaveProperty('checked', true); expect(screen.getByLabelText('分段 1 起始 Token')).toHaveProperty('value', '0'); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '2');
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(1)); expect(puts(b)[0][0]).toMatchObject({ model: 'renamed-id', display_name: 'Renamed Model', selection: { mode: 'manual', table: { rates: { input: 2 }, rules: [{ enabled: true, when: { input_range: { from_tokens: 0, to_tokens: null } } }] } } });
  });
  it('locks advanced controls during a held save and keeps the applied final value', async () => {
    const b = backend([price('public-a', {}, { rates: rates({ input: 1 }) })]); const normal = b.fetcher.getMockImplementation()!; let finish!: (value: Response) => void;
    b.fetcher.mockImplementation((url, options) => url === '/api/prices' && options.method === 'PUT' ? new Promise(resolve => { finish = resolve; }) : normal(url, options));
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a'); fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '2' } }); addRule('time'); fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(screen.getByLabelText('基础 输入').matches(':disabled')).toBe(true); expect(screen.getByLabelText('按上下文长度计费').matches(':disabled')).toBe(true); expect(screen.getByRole('button', { name: '新建分段' }).matches(':disabled')).toBe(true);
    await act(async () => { finish(response(puts(b)[0])); }); await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull()); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '2');
  });
  it('keeps native edits available without FX and rejects priced conversion without changing the currency label', async () => {
    const b = backend([price('public-a', {}, { currency: 'USD', rates: rates({ input: 1 }) })], { ...rate, available: false, rate: 0 }); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '1'); expect(screen.getByLabelText('基础 输入').matches(':disabled')).toBe(false);
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '2' } }); fireEvent.change(screen.getByLabelText('高级行币种'), { target: { value: 'CNY' } });
    expect(await screen.findByText('请先刷新汇率')).toBeTruthy(); expect(screen.getByLabelText('高级行币种')).toHaveProperty('value', 'USD'); expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '2');
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(1)); expect(table(puts(b)[0][0])).toMatchObject({ currency: 'USD', rates: { input: 2 } });
  });
  it('allows currency selection for an entirely unpriced row without FX', async () => {
    const b = backend([price('public-a')], { ...rate, available: false, rate: 0 }); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); fireEvent.change(screen.getByLabelText('高级行币种'), { target: { value: 'USD' } }); expect(screen.getByLabelText('高级行币种')).toHaveProperty('value', 'USD'); expect(screen.queryByText('请先刷新汇率')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' })); await waitFor(() => expect(puts(b)).toHaveLength(1)); expect(table(puts(b)[0][0])).toMatchObject({ currency: 'USD', rates: { input: null, output: null } });
  });
  it('blocks cross-currency editing without an exchange rate while keeping native amounts', async () => {
    const b = backend([price('public-a', {}, { currency: 'USD', rates: rates({ input: 1 }) })], { ...rate, available: false, rate: 0 }); vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('disabled', true); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '1'); expect(puts(b)).toHaveLength(0);
  });
});

describe('price persistence and navigation', () => {
  it('autosaves dirty values every 30 seconds and performs no clean writes', async () => {
    vi.useFakeTimers(); const b = backend([price('public-a')]); vi.stubGlobal('fetch', b.fetcher); await act(async () => { renderAllPrices(); }); showAllPrices();
    await act(async () => { await vi.advanceTimersByTimeAsync(30000); }); expect(puts(b)).toHaveLength(0);
    fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '0' } });
    await act(async () => { await vi.advanceTimersByTimeAsync(29999); }); expect(puts(b)).toHaveLength(0);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); }); expect(puts(b)).toHaveLength(1); expect(table(puts(b)[0][0]).rates.input).toBe(0);
    await act(async () => { await vi.advanceTimersByTimeAsync(30000); }); expect(puts(b)).toHaveLength(1);
  });
  it('does not overwrite edits made during a save and flushes the latest state', async () => {
    const b = backend([price('public-a')]); const normal = b.fetcher.getMockImplementation()!; let finish!: (value: Response) => void; let first = true;
    b.fetcher.mockImplementation((url, options) => url === '/api/prices' && options.method === 'PUT' && first ? (first = false, new Promise(resolve => { finish = resolve; })) : normal(url, options));
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices(); await screen.findByLabelText('输入 0:public-a');
    fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '1' } }); fireEvent.click(screen.getByRole('button', { name: '保存价格' }));
    await waitFor(() => expect(puts(b)).toHaveLength(1)); fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '2' } });
    await act(async () => { finish(response(puts(b)[0])); }); await waitFor(() => expect(puts(b)).toHaveLength(2));
    expect(table(puts(b)[1][0]).rates.input).toBe(2); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '2');
  });
  it('keeps an advanced draft on cancelled navigation and discards it only on an explicit choice', async () => {
    const b = backend([price('public-a')]); vi.stubGlobal('fetch', b.fetcher); render(<App />); await screen.findByLabelText('输入 0:public-a');
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '3' } });
    fireEvent.click(screen.getAllByRole('button', { name: '请求日志' })[0]);
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
    expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '3'); expect(puts(b)).toHaveLength(0);
    fireEvent.click(screen.getAllByRole('button', { name: '请求日志' })[0]);
    fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
    await screen.findByRole('heading', { name: '请求日志' });
    expect(localStorage.getItem('to-router.price-advanced.drafts.v2')).toBeNull(); expect(puts(b)).toHaveLength(0);
  });
  it('waits for sidebar saves, keeps edits on failure and retries before leaving', async () => {
    const b = backend([price('public-a')]); const normal = b.fetcher.getMockImplementation()!; let reject = true;
    b.fetcher.mockImplementation((url, options) => url === '/api/prices' && options.method === 'PUT' && reject ? Promise.resolve(response({ error: { message: 'price save failed' } }, 500)) : normal(url, options));
    vi.stubGlobal('fetch', b.fetcher); render(<App />); await screen.findByLabelText('输入 0:public-a');
    fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '3' } }); fireEvent.click(screen.getAllByRole('button', { name: '请求日志' })[0]);
    await confirmPriceSave(); await screen.findAllByText('price save failed'); expect(window.location.hash).toBe('#/prices'); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '3');
    reject = false; fireEvent.click(screen.getAllByRole('button', { name: '请求日志' })[0]); await confirmPriceSave(); await screen.findByRole('heading', { name: '请求日志' });
    expect(window.location.hash).toBe('#/logs'); expect(table(b.rows()[0]).rates.input).toBe(3);
  });
  it('waits for a held navigation save before replacing the price page', async () => {
    const b = backend([price('public-a')]); const normal = b.fetcher.getMockImplementation()!; let finish!: (value: Response) => void;
    b.fetcher.mockImplementation((url, options) => url === '/api/prices' && options.method === 'PUT' ? new Promise(resolve => { finish = resolve; }) : normal(url, options));
    vi.stubGlobal('fetch', b.fetcher); render(<App />); await screen.findByLabelText('输入 0:public-a'); fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '6' } });
    fireEvent.click(screen.getAllByRole('button', { name: '请求日志' })[0]); await confirmPriceSave(); await waitFor(() => expect(puts(b)).toHaveLength(1));
    expect(screen.getByRole('heading', { name: '价格配置' })).toBeTruthy(); expect(window.location.hash).toBe('#/prices');
    await act(async () => { finish(response(puts(b)[0])); }); await screen.findByRole('heading', { name: '请求日志' }); expect(window.location.hash).toBe('#/logs'); expect(table(puts(b)[0][0]).rates.input).toBe(6);
  });
  it('flushes before a hash/back navigation instead of losing the price component', async () => {
    const b = backend([price('public-a')]); vi.stubGlobal('fetch', b.fetcher); render(<App />); await screen.findByLabelText('输入 0:public-a');
    fireEvent.change(screen.getByLabelText('输出 0:public-a'), { target: { value: '4' } });
    await act(async () => { window.location.hash = '#/logs'; window.dispatchEvent(new HashChangeEvent('hashchange')); });
    await confirmPriceSave(); await screen.findByRole('heading', { name: '请求日志' }); expect(table(b.rows()[0]).rates.output).toBe(4);
  });
});

describe('model names and settings isolation', () => {
  it('saves, displays and searches the downstream display name', async () => {
    const b = backend(); vi.stubGlobal('fetch', b.fetcher); render(<Models />); fireEvent.click(await screen.findByRole('button', { name: '编辑模型 public-a' }));
    fireEvent.change(screen.getByLabelText('模型显示名'), { target: { value: 'Friendly Model' } }); fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await screen.findByRole('heading', { name: 'Friendly Model' }); fireEvent.change(screen.getByLabelText('搜索模型'), { target: { value: 'Friendly' } });
    expect(screen.getByRole('button', { name: '编辑模型 public-a' })).toBeTruthy();
  });
  it('omits price snapshots and preserves quota manual mode when saving system settings', async () => {
    const b = backend([price('public-a', {}, { rates: rates({ input: 1 }) })]); vi.stubGlobal('fetch', b.fetcher); render(<Settings />);
    fireEvent.change(await screen.findByLabelText('额度刷新周期'), { target: { value: '0' } }); fireEvent.click(screen.getAllByRole('button', { name: '保存设置' })[0]);
    await waitFor(() => expect(b.fetcher.mock.calls.some(call => call[0] === '/api/settings' && call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(String(b.fetcher.mock.calls.find(call => call[0] === '/api/settings' && call[1].method === 'PUT')![1].body)); expect(put).not.toHaveProperty('prices'); expect(put.quota_refresh_seconds).toBe(0);
  });
});

describe('price workbench reference protections', () => {
  const ref = { source: 'vendor', id: 'public-a', offering_key: 'standard', quote_key: 'token' };
  function linked(source?: PriceTable) {
    const linkedPrice = price('public-a', { selection: { mode: 'catalog', ref } });
    const b = backend([linkedPrice]), normal = b.fetcher.getMockImplementation()!;
    b.fetcher.mockImplementation((url, options) => url === '/api/catalog/models?source=all' ? Promise.resolve(response(source ? [{ source: ref.source, id: ref.id, offerings: [{ key: ref.offering_key, quotes: [{ key: ref.quote_key, source_kind: 'original', table: source }] }] }] : [])) : normal(url, options));
    vi.stubGlobal('fetch', b.fetcher); renderAllPrices();
    return { b, linkedPrice };
  }
  it('directly edits quoted prices per million and retains complete conditions through base and currency edits', async () => {
    const source = table(price('source', {}, { currency: 'USD', unit_tokens: 1000, rates: { input: 1, output: 0, cache_read: null, cache_write: 2 }, rules: [{ id: 'partial', enabled: true, when: { weekdays: [1, 3], timezone: 'UTC' }, action: { rate_changes: { input: 3, output: null, cache_read: 0 } } }], uncovered_conditions: ['audio input'] }));
    const { b } = linked(source);
    await waitFor(() => expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('disabled', false));
    fireEvent.change(screen.getByLabelText('页面币种'), { target: { value: 'USD' } });
    expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('disabled', false);
    expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '1000');
    expect(screen.getByLabelText('输出 0:public-a')).toHaveProperty('value', '0');
    expect(screen.getByLabelText('缓存命中 0:public-a')).toHaveProperty('value', '');
    expect(screen.getByText(/项条件/)).toBeTruthy(); expect(screen.queryByLabelText('基础 输入')).toBeNull(); expect(puts(b)).toHaveLength(0);
    expect(screen.queryByRole('button', { name: '复制为手填价' })).toBeNull();
    fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '3000' } });
    const copied = JSON.parse(localStorage.getItem('to-router.price-workbench.draft.v2')!)[0];
    expect(table(copied)).toEqual({ ...source, rates: { ...source.rates, input: 3 } }); expect(table(copied)).not.toBe(source);
    expect(b.rows()[0].selection).toEqual({ mode: 'catalog', ref });
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
    expect(screen.getByText('条件报价 · 1 项').closest('details')).toHaveProperty('open', false);
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '4' } });
    expect(JSON.parse(localStorage.getItem('to-router.price-advanced.drafts.v2')!)['0|public-a'].price.selection.table.rules).toEqual(source.rules);
    fireEvent.change(screen.getByLabelText('高级行币种'), { target: { value: 'CNY' } });
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
    await waitFor(() => expect(puts(b)).toHaveLength(1));
    const saved = table(b.rows()[0]);
    expect(saved.rates.input).toBe(28);
    expect(saved.rules).toEqual([{ ...source.rules[0], action: { rate_changes: { input: 21, output: null, cache_read: 0 } } }]);
    expect(saved.unit_tokens).toBe(1000); expect(saved.uncovered_conditions).toEqual(['audio input']);
    expect(source.rates.input).toBe(1); expect(source.rules[0].action.rate_changes!.input).toBe(3);
  });
  it('keeps unresolved references configured and cannot copy unknown source into zero rates', async () => {
    const { b, linkedPrice } = linked();
    await screen.findByText('资料库 · 报价缺失 / 需刷新');
    expect(screen.queryByRole('button', { name: '复制为手填价' })).toBeNull();
    expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('disabled', true);
    expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '');
    expect(screen.queryByLabelText('基础 输入')).toBeNull(); expect(puts(b)).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
    expect(screen.queryByLabelText('基础 输入')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(b.rows()[0]).toEqual(linkedPrice); expect(puts(b)).toHaveLength(0);
  });
});
