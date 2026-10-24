// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { CatalogItem, CatalogPriceRef, PrefillRequest, Price, PriceTable } from '../api';
import { ConfirmProvider } from '../components/Confirm';
import Prices from './Prices';

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
const ref = (region = 'cn', quote = 'payg'): CatalogPriceRef => ({ source: 'builtin', id: 'model-a', offering_key: region, quote_key: quote });
const table = (change: Partial<PriceTable> = {}): PriceTable => ({ currency: 'CNY', unit_tokens: 1000000, rates: { input: null, output: null, cache_read: null, cache_write: null }, rules: [], ...change });
const row = (model: string, channel_id = 0, selection: Price['selection'] = { mode: 'manual', table: table() }): Price => ({ model, channel_id, display_name: model, origin: 'manual', selection });
function catalog(): CatalogItem[] {
  return [{ id: 'model-a', name: 'Alpha', provider: 'Vendor', source: 'builtin', context_length: null, max_input_tokens: null, max_output_tokens: null, modalities: {}, thinking: {}, offerings: [
    { key: 'cn', provider: 'Vendor', region: '中国', source_kind: 'original', quotes: [{ key: 'payg', name: '标准', source_kind: 'original', table: table({ rates: { input: 2, output: 0, cache_read: null, cache_write: 4 } }) }, { key: 'hosted', name: '托管', source_kind: 'hosted', table: table({ rates: { input: 1, output: 1, cache_read: null, cache_write: null } }) }] },
    { key: 'overseas', provider: 'Vendor', region: '海外', source_kind: 'official', quotes: [{ key: 'payg', name: '标准海外', source_kind: 'official', table: table({ currency: 'USD', rates: { input: 1, output: 2, cache_read: null, cache_write: null } }) }, { key: 'unknown', name: '未知报价', source_kind: 'original', table: table() }] },
  ] } as CatalogItem];
}
function setup(initial = [row('model-a')], options: { catalog?: CatalogItem[]; rate?: unknown; deferred?: boolean; rejectPreview?: boolean } = {}) {
  let prices = initial, complete: ((response: Response) => void) | undefined;
  const requests: PrefillRequest[] = [], catalogItems = options.catalog || catalog();
  const preview = (request: PrefillRequest) => ({ request, patches: request.targets.map(target => ({ target, config: {}, warnings: [], price: { ...row(target.model, target.channel_id), selection: { mode: 'catalog', ref: target.price_ref || ref() } } })), updated: request.targets.length, skipped: 0 });
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/prices') { if (init?.method === 'PUT') prices = JSON.parse(String(init.body)); return Promise.resolve(json(prices)); }
    if (url === '/api/models') return Promise.resolve(json([{ id: 'model-a', enabled: true }, { id: 'model-b', enabled: true }, { id: 'hidden', enabled: false }]));
    if (url === '/api/channels') return Promise.resolve(json([{ id: 7, name: 'Channel', enabled: true, models: ['model-a'], available_models: ['model-a'], endpoints: [] }]));
    if (url === '/api/catalog/models?source=all') return Promise.resolve(json(catalogItems));
    if (url === '/api/exchange-rate') return Promise.resolve(json(options.rate ?? { available: true, rate: 7, base: 'USD', quote: 'CNY' }));
    if (url === '/api/catalog/prefill/preview') {
      const request: PrefillRequest = JSON.parse(String(init?.body)); requests.push(request);
      if (request.targets.some(target => target.ref && 'quote_key' in target.ref)) return Promise.resolve(json({ error: 'model reference cannot contain a quote key' }, 400));
      if (options.rejectPreview) return Promise.resolve(json({ error: 'preview rejected' }, 409));
      return options.deferred ? new Promise<Response>(resolve => { complete = resolve; }) : Promise.resolve(json(preview(request)));
    }
    return Promise.resolve(json([]));
  });
  vi.stubGlobal('fetch', fetcher); render(<ConfirmProvider><Prices /></ConfirmProvider>);
  return { requests, fetcher, prices: () => prices, finish: async () => act(async () => complete?.(json(preview(requests[requests.length - 1])))) };
}
async function save() { fireEvent.click(screen.getByRole('button', { name: '保存价格' })); await waitFor(() => expect(screen.getByRole('button', { name: '保存价格' })).toHaveProperty('disabled', true)); }
beforeEach(() => {
  localStorage.clear(); history.replaceState(null, '', '/#/prices');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('matches only visible checked rows through price drafts and preserves hidden/channel prices', async () => {
  const backend = setup([row('model-a'), row('model-b'), row('model-a', 7)]);
  await screen.findByLabelText('选择价格 0:model-a');
  fireEvent.click(screen.getByLabelText('全选当前可见价格'));
  fireEvent.change(screen.getByLabelText('搜索价格'), { target: { value: 'model-a' } });
  fireEvent.click(screen.getByRole('button', { name: '一键匹配（1）' }));
  await screen.findByText('model-a：已匹配 中国 · 标准');
  expect(backend.requests).toHaveLength(1);
  expect(backend.requests[0]).toMatchObject({ groups: ['price'], force_overwrite: true, price_mode: 'catalog', targets: [{ model: 'model-a', channel_id: 0, price_draft: { mode: 'manual' } }] });
  expect(backend.requests[0].targets).toHaveLength(1);
  expect(backend.fetcher.mock.calls.some(([url]) => url === '/api/catalog/prefill/apply')).toBe(false);
  await save();
  expect(backend.prices().map(price => price.selection.mode)).toEqual(['catalog', 'manual', 'manual']);
});

it('does not overwrite a row edited while a preview is in flight', async () => {
  const backend = setup(undefined, { deferred: true }); await screen.findByLabelText('选择价格 0:model-a');
  fireEvent.click(screen.getByLabelText('选择价格 0:model-a')); fireEvent.click(screen.getByRole('button', { name: '一键匹配（1）' }));
  await waitFor(() => expect(backend.requests).toHaveLength(1));
  fireEvent.change(screen.getByLabelText('输入 0:model-a'), { target: { value: '9' } });
  await backend.finish(); await screen.findByText('model-a：编辑已变化，保持当前草稿');
  expect(screen.getByLabelText('输入 0:model-a')).toHaveProperty('value', '9');
  await save(); expect(backend.prices()[0].selection).toMatchObject({ mode: 'manual', table: { rates: { input: 9 } } });
});

it('invalidates pending results after leaving the old visible scope, even when returning to it', async () => {
  const backend = setup(undefined, { deferred: true }); await screen.findByLabelText('选择价格 0:model-a');
  fireEvent.click(screen.getByLabelText('选择价格 0:model-a')); fireEvent.click(screen.getByRole('button', { name: '一键匹配（1）' }));
  await waitFor(() => expect(backend.requests).toHaveLength(1));
  fireEvent.change(screen.getByLabelText('搜索价格'), { target: { value: 'other' } });
  fireEvent.change(screen.getByLabelText('搜索价格'), { target: { value: '' } });
  await backend.finish();
  expect(screen.queryByText('资料库 · 跟随更新')).toBeNull();
  expect(screen.getByRole('button', { name: '保存价格' })).toHaveProperty('disabled', true);
});

it('searches shared catalog by exact region/quote, excludes hosted official quotes, and supports keyboard dismissal', async () => {
  const backend = setup(); await screen.findByLabelText('搜索价格资料 0:model-a');
  const input = screen.getByRole('combobox', { name: '搜索价格资料 0:model-a' });
  fireEvent.focus(input); expect(screen.queryByRole('option', { name: /托管/ })).toBeNull();
  fireEvent.change(input, { target: { value: '海外' } });
  expect(within(screen.getByRole('listbox')).getAllByRole('option')).toHaveLength(2);
  expect(screen.getByRole('option', { name: /未知报价/ }).getAttribute('aria-disabled')).toBe('true');
  fireEvent.keyDown(input, { key: 'Escape' }); expect(screen.queryByRole('listbox')).toBeNull();
  fireEvent.keyDown(input, { key: 'ArrowDown' }); fireEvent.keyDown(input, { key: 'Enter' });
  await waitFor(() => expect(backend.requests).toHaveLength(1));
  expect(backend.requests[0].targets[0].price_ref).toEqual(ref('overseas'));
  expect(backend.fetcher.mock.calls.filter(([url]) => url === '/api/catalog/models?source=all')).toHaveLength(1);
  await screen.findByText('资料库 · 跟随更新');
});

it('detaches a complete catalog table on edit, converts actual million units and all conditional currency rates', async () => {
  const items = catalog(), source = items[0].offerings[0].quotes[0];
  source.table = table({ currency: 'USD', unit_tokens: 1000, rates: { input: 0.002, output: 0, cache_read: null, cache_write: 0.003 }, rules: [{ enabled: true, when: { input_range: { from_tokens: 100, to_tokens: null } }, action: { rate_changes: { input: 0.004 }, multiplier: 0.5 } }], uncovered_conditions: ['备注保留'] });
  const backend = setup([row('model-a', 0, { mode: 'catalog', ref: ref() })], { catalog: items });
  await waitFor(() => expect(screen.getByLabelText('输入 0:model-a')).toHaveProperty('value', '14'));
  expect(screen.getByLabelText('缓存写入 0:model-a')).toHaveProperty('value', '21');
  expect(screen.getByLabelText('输出 0:model-a')).toHaveProperty('value', '0');
  fireEvent.change(screen.getByLabelText('输入 0:model-a'), { target: { value: '28' } });
  expect(screen.getByText('已手填')).toBeTruthy(); await save();
  expect(backend.prices()[0].selection).toMatchObject({ mode: 'manual', table: { currency: 'CNY', unit_tokens: 1000, rates: { input: 0.028, output: 0, cache_write: 0.021 }, rules: [{ action: { rate_changes: { input: 0.028 }, multiplier: 0.5 } }], uncovered_conditions: ['备注保留'] } });
});

it('keeps original currency visible and blocks cross-currency editing without an exchange rate', async () => {
  setup([row('model-a', 0, { mode: 'manual', table: table({ currency: 'USD', unit_tokens: 1000, rates: { input: 0.002, output: null, cache_read: null, cache_write: null } }) })], { rate: { available: false } });
  await screen.findByText('原币种 USD / 1M Tokens');
  expect(screen.getByLabelText('输入 0:model-a')).toHaveProperty('value', '2');
  expect(screen.getByLabelText('输入 0:model-a')).toHaveProperty('disabled', true);
  fireEvent.change(screen.getByLabelText('页面币种'), { target: { value: 'USD' } });
  expect(screen.getByLabelText('输入 0:model-a')).toHaveProperty('disabled', false);
});

it('reorders visible slots with keyboard while leaving hidden prices in their existing slots', async () => {
  const backend = setup([row('model-a'), row('hidden'), row('model-b'), row('model-a', 7)]);
  await screen.findByLabelText('排序 0:model-a');
  fireEvent.keyDown(screen.getByLabelText('排序 0:model-a'), { key: 'ArrowDown', altKey: true });
  await save(); expect(backend.prices().map(price => `${price.channel_id}:${price.model}`)).toEqual(['0:model-b', '0:hidden', '0:model-a', '7:model-a']);
});

it('confirms replacing configured prices and cancelling keeps the entire draft intact', async () => {
  const backend = setup([row('model-a', 0, { mode: 'manual', table: table({ rates: { input: 0, output: null, cache_read: null, cache_write: null } }) })]);
  await screen.findByLabelText('选择价格 0:model-a'); fireEvent.click(screen.getByLabelText('选择价格 0:model-a'));
  fireEvent.click(screen.getByRole('button', { name: '一键匹配（1）' }));
  await screen.findByRole('dialog', { name: '替换所选价格？' }); fireEvent.click(screen.getByRole('button', { name: '取消' }));
  await waitFor(() => expect(screen.getByRole('button', { name: '一键匹配（1）' })).toHaveProperty('disabled', false));
  expect(backend.requests).toHaveLength(0); expect(screen.getByLabelText('输入 0:model-a')).toHaveProperty('value', '0');
});

it('skips selected blank rows without blocking valid rows or sending an invalid target', async () => {
  const backend = setup(); await screen.findByLabelText('选择价格 0:model-a');
  fireEvent.click(screen.getByRole('button', { name: '手工添加价格' }));
  fireEvent.click(screen.getByLabelText('全选当前可见价格'));
  fireEvent.click(screen.getByRole('button', { name: '一键匹配（2）' }));
  await screen.findByText('空白行：请先填写模型 ID');
  expect(backend.requests[0].targets.map(target => target.model)).toEqual(['model-a']);
  expect(screen.getByText('资料库 · 跟随更新')).toBeTruthy();
});

it('requires confirmation for an existing advanced draft and invalidates it only after successful replacement', async () => {
  const draft = row('model-a', 0, { mode: 'manual', table: table({ rates: { input: 5, output: null, cache_read: null, cache_write: null } }) });
  localStorage.setItem('to-router.price-advanced.drafts.v2', JSON.stringify({ '0|model-a': { key: 1, price: draft } }));
  const backend = setup(); await screen.findByLabelText('选择价格 0:model-a');
  fireEvent.click(screen.getByLabelText('选择价格 0:model-a')); fireEvent.click(screen.getByRole('button', { name: '一键匹配（1）' }));
  await screen.findByRole('dialog', { name: '替换所选价格？' });
  expect(localStorage.getItem('to-router.price-advanced.drafts.v2')).toContain('"input":5');
  fireEvent.click(screen.getByRole('button', { name: '确认匹配' }));
  await screen.findByText('资料库 · 跟随更新');
  expect(localStorage.getItem('to-router.price-advanced.drafts.v2')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '高级 0:model-a' }));
  expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '2');
  expect(backend.requests).toHaveLength(1);
});

it('keeps an unfinished advanced table in sync when the main amount is changed', async () => {
  const draft = row('model-a', 0, { mode: 'manual', table: table({ rules: [{ enabled: false, when: { input_range: { from_tokens: 1000, to_tokens: null } }, action: { multiplier: 0.5 } }] }) });
  localStorage.setItem('to-router.price-advanced.drafts.v2', JSON.stringify({ '0|model-a': { key: 1, price: draft } }));
  setup(); await screen.findByLabelText('输入 0:model-a');
  fireEvent.change(screen.getByLabelText('输入 0:model-a'), { target: { value: '3' } });
  const persisted = JSON.parse(localStorage.getItem('to-router.price-advanced.drafts.v2')!);
  expect(persisted['0|model-a'].price.selection.table).toMatchObject({ rates: { input: 3 }, rules: [{ enabled: false, action: { multiplier: 0.5 } }] });
});

it('reports preview failures per row and preserves drafts without replacing the table by a loading screen', async () => {
  const backend = setup([row('model-a'), row('model-b')], { rejectPreview: true });
  await screen.findByLabelText('选择价格 0:model-a'); fireEvent.click(screen.getByLabelText('全选当前可见价格'));
  fireEvent.click(screen.getByRole('button', { name: '一键匹配（2）' }));
  await screen.findByText('model-a：匹配失败，preview rejected');
  expect(screen.getByText('model-b：匹配失败，preview rejected')).toBeTruthy();
  expect(screen.getByLabelText('输入 0:model-a')).toBeTruthy();
  expect(backend.prices().every(price => price.selection.mode === 'manual')).toBe(true);
});

it('shows stale catalog references as unavailable and never detaches a guessed table', async () => {
  const items = catalog(); items[0].needs_refresh = true;
  setup([row('model-a', 0, { mode: 'catalog', ref: ref() })], { catalog: items });
  await screen.findByText('资料库 · 报价缺失 / 需刷新');
  expect(screen.getByLabelText('输入 0:model-a')).toHaveProperty('disabled', true);
  fireEvent.focus(screen.getByLabelText('搜索价格资料 0:model-a'));
  expect(within(screen.getByRole('listbox')).getAllByRole('option').every(option => option.getAttribute('aria-disabled') === 'true')).toBe(true);
});

it('direct main edits supersede an unavailable advanced reference instead of restoring it on save', async () => {
  const draft = row('model-a', 0, { mode: 'catalog', ref: ref('cn', 'removed') });
  localStorage.setItem('to-router.price-advanced.drafts.v2', JSON.stringify({ '0|model-a': { key: 1, price: draft } }));
  const backend = setup([row('model-a', 0, { mode: 'manual', table: table({ rates: { input: 1, output: null, cache_read: null, cache_write: null } }) })]);
  await screen.findByLabelText('输入 0:model-a');
  fireEvent.change(screen.getByLabelText('输入 0:model-a'), { target: { value: '3' } });
  expect(localStorage.getItem('to-router.price-advanced.drafts.v2')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '高级 0:model-a' }));
  expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '3');
  fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(backend.prices()[0].selection).toMatchObject({ mode: 'manual', table: { rates: { input: 3 } } });
});

it('treats a table containing only notes as existing configuration before replacement', async () => {
  const original = row('model-a', 0, { mode: 'manual', table: table({ uncovered_conditions: ['活动期间需核对'] }) });
  const backend = setup([original]);
  await screen.findByLabelText('选择价格 0:model-a');
  fireEvent.click(screen.getByLabelText('选择价格 0:model-a'));
  fireEvent.click(screen.getByRole('button', { name: '一键匹配（1）' }));
  await screen.findByRole('dialog', { name: '替换所选价格？' });
  expect(backend.requests).toHaveLength(0);
  fireEvent.click(screen.getByRole('button', { name: '取消' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(backend.prices()).toEqual([original]);
});

it('keeps both prices unchanged when the advanced draft needs an unavailable exchange rate', async () => {
  const draft = row('model-a', 0, { mode: 'manual', table: table({ currency: 'USD', rates: { input: 9, output: null, cache_read: null, cache_write: null } }) });
  localStorage.setItem('to-router.price-advanced.drafts.v2', JSON.stringify({ '0|model-a': { key: 1, price: draft } }));
  const original = row('model-a', 0, { mode: 'manual', table: table({ rates: { input: 1, output: null, cache_read: null, cache_write: null } }) });
  const backend = setup([original], { rate: { available: false } });
  await screen.findByLabelText('输入 0:model-a');
  fireEvent.change(screen.getByLabelText('输入 0:model-a'), { target: { value: '3' } });
  await screen.findByText('请先刷新汇率');
  expect(screen.getByLabelText('输入 0:model-a')).toHaveProperty('value', '1');
  expect(backend.prices()).toEqual([original]);
  expect(JSON.parse(localStorage.getItem('to-router.price-advanced.drafts.v2')!)['0|model-a'].price).toEqual(draft);
});
