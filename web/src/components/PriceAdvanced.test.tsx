// @vitest-environment jsdom
import { ConfirmProvider } from './Confirm';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor } from '@testing-library/react';
import type { ExchangeRate, Price, PriceRule, PriceTable } from '../api';
import { PriceAdvanced } from './PriceAdvanced';

const exchange: ExchangeRate = { available: true, base: 'USD', quote: 'CNY', rate: 7, date: '2026-10-05', attempted_on: '2026-10-05', fetched_at: '2026-10-05T00:00:00Z', stale: false, error: '', source_url: 'https://frankfurter.dev/' };
const price = (change: Partial<PriceTable> = {}): Price => ({ channel_id: 0, model: 'public-a', display_name: 'Alpha', origin: 'manual', selection: { mode: 'manual', table: { currency: 'CNY', unit_tokens: 1000000, rates: { input: 1, output: 2, cache_read: null, cache_write: null }, rules: [], ...change } } });
const fullRates = { input: 0, output: null, cache_read: null, cache_write: null };
function table(row: Price) { if (row.selection.mode !== 'manual') throw new Error('expected manual price'); return row.selection.table; }
function editor(value: Price = price()) {
  const onDraft = vi.fn(), onApply = vi.fn().mockResolvedValue(undefined), onClose = vi.fn(), onDiscard = vi.fn();
  render(<PriceAdvanced value={value} rate={exchange} onDraft={onDraft} onApply={onApply} onClose={onClose} onDiscard={onDiscard} />);
  return { onDraft, onApply, onClose, onDiscard };
}
async function apply(callback: ReturnType<typeof editor>['onApply']) {
  fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
  await waitFor(() => expect(callback).toHaveBeenCalledOnce());
  await waitFor(() => expect(screen.getByRole('button', { name: '应用并保存' })).toHaveProperty('disabled', false));
  return table(callback.mock.calls[0][0]);
}
beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(JSON.stringify([]), { headers: { 'Content-Type': 'application/json' } }))));
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function(this: HTMLDialogElement) { this.setAttribute('open', ''); } });
  Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value: function(this: HTMLDialogElement) { this.removeAttribute('open'); } });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('simple price conditions', () => {
  it('edits independent rates with explicit unknown and free values without changing base prices', async () => {
    const callbacks = editor();
    expect(screen.getByLabelText('按时段计费')).toHaveProperty('checked', false);
    expect(screen.queryByLabelText('分段 1 开始')).toBeNull();
    fireEvent.click(screen.getByLabelText('按上下文长度计费'));
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.change(screen.getByLabelText('分段 1 计费方式'), { target: { value: 'fixed' } });
    fireEvent.change(screen.getByLabelText('分段 1 输入'), { target: { value: '0' } });
    fireEvent.change(screen.getByLabelText('分段 1 输出'), { target: { value: '' } });
    expect(screen.getByLabelText('分段 1 输出')).toHaveProperty('placeholder', '未知');
    const saved = await apply(callbacks.onApply);
    expect(saved.rates).toEqual({ input: 1, output: 2, cache_read: null, cache_write: null });
    expect(saved.rules).toEqual([{ enabled: true, when: { input_range: { from_tokens: 0, to_tokens: null } }, action: { rate_changes: fullRates } }]);
  });

  it('retains whole segments when toggled off and on, and deletes only the selected segment', async () => {
    const context: PriceRule = { id: 'context', enabled: true, when: { input_range: { from_tokens: 32001, to_tokens: null } }, action: { rate_changes: fullRates } };
    const night: PriceRule = { id: 'night', enabled: false, when: { time_window: { start: '22:00', end: '06:00', all_day: false, timezone: 'Asia/Shanghai' } }, action: { multiplier: 0.5 } };
    const callbacks = editor(price({ rules: [context, night] }));
    expect(screen.getByLabelText('按时段计费')).toHaveProperty('checked', false);
    fireEvent.click(screen.getByLabelText('按上下文长度计费'));
    expect(screen.queryByLabelText('分段 1 起始 Token')).toBeNull();
    expect(table(callbacks.onDraft.mock.lastCall![0]).rules).toEqual([{ ...context, enabled: false }, night]);
    fireEvent.click(screen.getByLabelText('按上下文长度计费'));
    expect(screen.getByLabelText('分段 1 起始 Token')).toHaveProperty('value', '32001');
    expect(screen.getByLabelText('分段 1 输入')).toHaveProperty('value', '0');
    fireEvent.click(screen.getByLabelText('按时段计费'));
    expect(screen.getByLabelText('分段 1 开始')).toHaveProperty('value', '22:00');
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.click(screen.getByRole('button', { name: '删除分段 2' }));
    fireEvent.click(screen.getByRole('button', { name: '删除分段 1' }));
    expect(screen.queryByLabelText('分段 1 起始 Token')).toBeNull();
    expect((await apply(callbacks.onApply)).rules).toEqual([{ ...night, enabled: true }]);
  });

  it('keeps inclusive thresholds and composite conditions', async () => {
    const callbacks = editor();
    fireEvent.click(screen.getByLabelText('按上下文长度计费'));
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.change(screen.getByLabelText('分段 1 终止 Token'), { target: { value: '32000' } });
    fireEvent.change(screen.getByLabelText('分段 1 倍率'), { target: { value: '2' } });
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.change(screen.getByLabelText('分段 2 起始 Token'), { target: { value: '32001' } });
    fireEvent.click(screen.getByLabelText('按时段计费'));
    fireEvent.change(screen.getByLabelText('分段 1 计费方式'), { target: { value: 'fixed' } });
    fireEvent.change(screen.getByLabelText('分段 1 时区'), { target: { value: 'UTC' } });
    fireEvent.change(screen.getByLabelText('分段 1 开始'), { target: { value: '09:00' } });
    fireEvent.change(screen.getByLabelText('分段 1 结束'), { target: { value: '10:00' } });
    fireEvent.change(screen.getByLabelText('分段 2 倍率'), { target: { value: '0.5' } });
    const saved = await apply(callbacks.onApply);
    expect(saved.rules[0].when.time_window).toEqual({ start: '09:00', end: '10:00', all_day: false, timezone: 'UTC' });
    expect(saved.rules[0].when.input_range).toEqual({ from_tokens: 0, to_tokens: 32000 });
    expect(saved.rules[1].when.input_range).toEqual({ from_tokens: 32001, to_tokens: null });
    expect(saved.rules[1].action.multiplier).toBe(0.5);
  });

  it('blocks a shared inclusive boundary, keeps the draft editable, and saves after starting the next range at 101', async () => {
    const callbacks = editor();
    fireEvent.click(screen.getByLabelText('按上下文长度计费'));
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.change(screen.getByLabelText('分段 1 终止 Token'), { target: { value: '100' } });
    fireEvent.change(screen.getByLabelText('分段 1 倍率'), { target: { value: '2' } });
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.change(screen.getByLabelText('分段 2 起始 Token'), { target: { value: '100' } });
    fireEvent.change(screen.getByLabelText('分段 2 倍率'), { target: { value: '3' } });
    expect(screen.getByRole('alert').textContent).toContain('分段 1 与分段 2 重叠');
    expect(screen.getByRole('button', { name: '应用并保存' })).toHaveProperty('disabled', true);
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
    expect(callbacks.onApply).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText('分段 2 计费方式'), { target: { value: 'fixed' } });
    expect(screen.getByLabelText('分段 2 输入')).toHaveProperty('value', '1');
    expect(screen.getByRole('button', { name: '删除分段 2' })).toHaveProperty('disabled', false);
    fireEvent.change(screen.getByLabelText('分段 2 计费方式'), { target: { value: 'multiplier' } });
    fireEvent.change(screen.getByLabelText('分段 2 倍率'), { target: { value: '3' } });
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '4' } });
    fireEvent.change(screen.getByLabelText('分段 2 起始 Token'), { target: { value: '101' } });
    expect(screen.queryByRole('alert')).toBeNull();
    const saved = await apply(callbacks.onApply);
    expect(saved.rates.input).toBe(4);
    expect(saved.rules.map(rule => [rule.when.input_range!.from_tokens, rule.when.input_range!.to_tokens, rule.action.multiplier])).toEqual([[0, 100, 2], [101, null, 3]]);
  });

  it('blocks cross-midnight and cross-timezone overlap while allowing touching time endpoints and all-day alone', async () => {
    const callbacks = editor();
    fireEvent.click(screen.getByLabelText('按时段计费'));
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.change(screen.getByLabelText('分段 2 开始'), { target: { value: '23:00' } });
    fireEvent.change(screen.getByLabelText('分段 2 结束'), { target: { value: '07:00' } });
    expect(screen.getByRole('alert').textContent).toContain('分段 1 与分段 2 重叠');
    expect(screen.getByRole('button', { name: '应用并保存' })).toHaveProperty('disabled', true);
    fireEvent.change(screen.getByLabelText('分段 2 开始'), { target: { value: '06:00' } });
    fireEvent.change(screen.getByLabelText('分段 2 结束'), { target: { value: '22:00' } });
    expect(screen.queryByRole('alert')).toBeNull();
    fireEvent.change(screen.getByLabelText('分段 2 时区'), { target: { value: 'UTC' } });
    expect(screen.getByRole('alert').textContent).toContain('分段 1 与分段 2 重叠');
    // Shanghai 22–06 is UTC 14–22; UTC 22–06 touches but does not overlap it.
    fireEvent.change(screen.getByLabelText('分段 2 开始'), { target: { value: '22:00' } });
    fireEvent.change(screen.getByLabelText('分段 2 结束'), { target: { value: '06:00' } });
    expect(screen.queryByRole('alert')).toBeNull();
    fireEvent.click(screen.getByLabelText('分段 1 全天'));
    expect(screen.getByRole('alert').textContent).toContain('分段 1 与分段 2 重叠');
    expect(screen.getByLabelText('分段 1 开始')).toHaveProperty('disabled', true);
    fireEvent.click(screen.getByRole('button', { name: '删除分段 2' }));
    expect(screen.queryByRole('alert')).toBeNull();
    const saved = await apply(callbacks.onApply);
    expect(saved.rules).toHaveLength(1);
    expect(saved.rules[0].when.time_window).toEqual({ start: '22:00', end: '06:00', all_day: true, timezone: 'Asia/Shanghai' });
  });

  it('allows saving disabled overlapping drafts and restores the error and values when the group is re-enabled', async () => {
    const callbacks = editor();
    fireEvent.click(screen.getByLabelText('按上下文长度计费'));
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.change(screen.getByLabelText('分段 1 倍率'), { target: { value: '2' } });
    fireEvent.click(screen.getByRole('button', { name: '新建分段' }));
    fireEvent.change(screen.getByLabelText('分段 2 倍率'), { target: { value: '3' } });
    expect(screen.getByRole('button', { name: '应用并保存' })).toHaveProperty('disabled', true);
    fireEvent.click(screen.getByLabelText('按上下文长度计费'));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByLabelText('分段 2 倍率')).toBeNull();
    const saved = await apply(callbacks.onApply);
    expect(saved.rules.map(rule => [rule.enabled, rule.action.multiplier])).toEqual([[false, 2], [false, 3]]);
    fireEvent.click(screen.getByLabelText('按上下文长度计费'));
    expect(screen.getByLabelText('分段 2 倍率')).toHaveProperty('value', '3');
    expect(screen.getByRole('alert').textContent).toContain('分段 1 与分段 2 重叠');
    expect(screen.getByRole('button', { name: '应用并保存' })).toHaveProperty('disabled', true);
    fireEvent.click(screen.getByRole('button', { name: '删除分段 2' }));
    expect(screen.getByRole('button', { name: '应用并保存' })).toHaveProperty('disabled', false);
  });

  it('preserves combined conditions, partial rate overrides, ordering and folded price notes when editing base rates', async () => {
    const rules: PriceRule[] = [
      { id: 'combo', enabled: true, when: { input_range: { from_tokens: 1000, to_tokens: 32000 }, time_window: { start: '22:00', end: '06:00', all_day: true, timezone: 'UTC' }, weekdays: [1, 3, 7], date_set: ['2026-10-01', '2026-10-07'], timezone: 'Asia/Shanghai' }, action: { multiplier: 0 } },
      { id: 'partial', enabled: false, when: { input_range: { from_tokens: 0, to_tokens: null } }, action: { rate_changes: { input: null, output: 0 } } },
    ];
    const callbacks = editor(price({ rules, uncovered_conditions: ['audio input', 'regional quota'] }));
    expect(screen.queryByLabelText('按时段计费')).toBeNull();
    expect(screen.queryByLabelText('未覆盖条件')).toBeNull();
    const details = screen.getByText('条件报价 · 2 项').closest('details');
    expect(details).toHaveProperty('open', false);
    fireEvent.click(screen.getByText('条件报价 · 2 项'));
    expect(screen.getByText(/星期一、三、日/)).toBeTruthy();
    expect(screen.getByText('audio input').closest('details')).toHaveProperty('open', false);
    fireEvent.click(screen.getByText('价格备注 · 2 项'));
    expect(screen.getByText('audio input').closest('details')).toHaveProperty('open', true);
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '5' } });
    const saved = await apply(callbacks.onApply);
    expect(saved.rules).toEqual(rules);
    expect(saved.uncovered_conditions).toEqual(['audio input', 'regional quota']);
    expect(saved.rates.input).toBe(5);
  });

  it.each([
    [{ enabled: true, when: { time_window: { start: '22:00', end: '06:00', all_day: false, timezone: 'UTC' } }, action: { multiplier: 0.5 } }, { enabled: true, when: { input_range: { from_tokens: 0, to_tokens: null } }, action: { multiplier: 2 } }],
    [{ enabled: true, when: { input_range: { from_tokens: 0, to_tokens: 10 } }, action: { multiplier: 2 } }, { enabled: false, when: { input_range: { from_tokens: 11, to_tokens: null } }, action: { multiplier: 3 } }],
    [{ enabled: true, when: { time_window: { start: '22:00', end: '06:00', all_day: false, timezone: 'Europe/Berlin' } }, action: { multiplier: 0.5 } }],
  ] as PriceRule[][])('preserves a nonstandard order, mixed segment enabled state or other timezone without exposing unsafe controls', async (...rules: PriceRule[]) => {
    const callbacks = editor(price({ rules }));
    expect(screen.queryByLabelText('按上下文长度计费')).toBeNull();
    fireEvent.change(screen.getByLabelText('基础 输出'), { target: { value: '3' } });
    expect((await apply(callbacks.onApply)).rules).toEqual(rules);
  });

  it('converts disabled partial fixed rules without filling inherited rates or changing multipliers and token units', async () => {
    const callbacks = editor(price({ currency: 'USD', unit_tokens: 1000, rules: [{ enabled: false, when: { input_range: { from_tokens: 0, to_tokens: null } }, action: { rate_changes: { input: 3, output: null, cache_read: 0 } } }, { enabled: true, when: {}, action: { multiplier: 0.5 } }] }));
    fireEvent.change(screen.getByLabelText('高级行币种'), { target: { value: 'CNY' } });
    const saved = await apply(callbacks.onApply);
    expect(saved).toMatchObject({ currency: 'CNY', unit_tokens: 1000, rates: { input: 7, output: 14 } });
    expect(saved.rules).toEqual([{ enabled: false, when: { input_range: { from_tokens: 0, to_tokens: null } }, action: { rate_changes: { input: 21, output: null, cache_read: 0 } } }, { enabled: true, when: {}, action: { multiplier: 0.5 } }]);
  });

  it('retains edits and unlocks controls after an apply failure so the user can retry', async () => {
    const callbacks = editor();
    callbacks.onApply.mockRejectedValueOnce(new Error('price save failed'));
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '0' } });
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
    await screen.findByText('price save failed');
    expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '0');
    expect(screen.getByLabelText('基础 输入').matches(':disabled')).toBe(false);
    expect(screen.getByLabelText('按时段计费').matches(':disabled')).toBe(false);
    expect(callbacks.onClose).not.toHaveBeenCalled(); expect(callbacks.onDiscard).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
    await waitFor(() => expect(callbacks.onApply).toHaveBeenCalledTimes(2));
    expect(table(callbacks.onApply.mock.calls[1][0]).rates.input).toBe(0);
  });
});

describe('price workbench catalog responsibility', () => {
  it('keeps the exact quote separate from the strict model reference when previewing an existing link', async () => {
    const ref = { source: 'builtin', id: 'origin', offering_key: 'china', quote_key: '24h' };
    const linked: Price = { ...price(), selection: { mode: 'catalog', ref } };
    const requests: any[] = [];
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url.endsWith('/prefill/preview')) { const request = JSON.parse(String(init?.body)); requests.push(request); return Promise.resolve(new Response(JSON.stringify({ request, patches: [], updated: 0, skipped: 1 }))); }
      return Promise.resolve(new Response('[]'));
    }));
    editor(linked);
    fireEvent.click(screen.getByText('更换资料来源'));
    fireEvent.click(screen.getByRole('button', { name: '预览预填' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0].targets[0].ref).toEqual({ source: 'builtin', id: 'origin', offering_key: 'china' });
    expect(requests[0].targets[0].price_ref).toEqual(ref);
  });
  it('allows directly editing a linked table and detaches only the changed draft without losing conditions', async () => {
    const source = price({ currency: 'USD', unit_tokens: 1000, rules: [{ id: 'night', enabled: true, when: { weekdays: [1], timezone: 'UTC' }, action: { multiplier: 0 } }], uncovered_conditions: ['audio not priced'] });
    const originalTable = structuredClone(table(source));
    const linked: Price = { ...source, selection: { mode: 'catalog', ref: { source: 'builtin', id: 'origin', offering_key: 'china', quote_key: 'standard' } } };
    const onDraft = vi.fn(), onApply = vi.fn().mockResolvedValue(undefined);
    render(<PriceAdvanced value={linked} rate={exchange} resolvedTable={originalTable} onDraft={onDraft} onApply={onApply} onClose={vi.fn()} onDiscard={vi.fn()} />);
    expect(screen.queryByRole('button', { name: '复制为手填价' })).toBeNull();
    expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '1');
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '0' } });
    expect(table(onDraft.mock.lastCall![0])).toEqual({ ...originalTable, rates: { ...originalTable.rates, input: 0 } });
    expect(originalTable.rates.input).toBe(1);
    expect(onApply).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
    await waitFor(() => expect(onApply).toHaveBeenCalledOnce());
    expect(onApply.mock.calls[0][0].selection.mode).toBe('manual');
  });

  it('prefills only a complete price selection in the advanced draft before explicit save', async () => {
    const selected: Price = { ...price(), selection: { mode: 'catalog', ref: { source: 'builtin', id: 'origin', offering_key: 'china', quote_key: '24h' } } };
    const fetcher = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/catalog/prefill/preview') { const request = JSON.parse(String(init?.body)); return Promise.resolve(new Response(JSON.stringify({ request, patches: [{ target: request.targets[0], config: {}, price: selected, warnings: [] }], updated: 1, skipped: 0 }))); }
      return Promise.resolve(new Response(JSON.stringify([])));
    });
    vi.stubGlobal('fetch', fetcher);
    const callbacks = editor();
    expect(screen.queryByRole('checkbox', { name: '预填能力规格' })).toBeNull();
    expect(screen.queryByRole('checkbox', { name: '预填请求默认' })).toBeNull();
    expect(screen.queryByLabelText('价格用途')).toBeNull();
    fireEvent.click(screen.getByText('更换资料来源'));
    fireEvent.click(screen.getByRole('button', { name: '预览预填' }));
    fireEvent.click(await screen.findByRole('button', { name: '填入当前草稿' }));
    const request = JSON.parse(String(fetcher.mock.calls.find(([url]) => url === '/api/catalog/prefill/preview')![1]?.body));
    expect(request.groups).toEqual(['price']); expect(request.targets[0]).not.toHaveProperty('price_role');
    expect(callbacks.onApply).not.toHaveBeenCalled(); expect(callbacks.onDraft.mock.lastCall![0].selection).toEqual(selected.selection);
    expect(fetcher.mock.calls.some(([url]) => url === '/api/catalog/prefill/apply')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: '应用并保存' }));
    await waitFor(() => expect(callbacks.onApply).toHaveBeenCalledOnce());
    expect(callbacks.onApply.mock.calls[0][0].selection).toEqual(selected.selection); expect(callbacks.onApply.mock.calls[0][0]).not.toHaveProperty('role');
  });
});


describe('price draft prefill protection', () => {
  it('sends the unsaved 123 draft, preserves it without force, and only fills a forced result into the draft', async () => {
    const requests: any[] = [];
    const fetcher = vi.fn((url: string, init?: RequestInit) => {
      if (!url.endsWith('/prefill/preview')) return Promise.resolve(new Response('[]'));
      const request = JSON.parse(String(init?.body)); requests.push(request);
      const selected = request.force_overwrite ? price().selection : request.targets[0].price_draft || price().selection;
      return Promise.resolve(new Response(JSON.stringify({ request, updated: 1, skipped: 0, patches: [{ target: request.targets[0], config: {}, price: { ...price(), selection: selected }, warnings: [] }] })));
    });
    vi.stubGlobal('fetch', fetcher);
    const callbacks = editor();
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '123' } });
    fireEvent.click(screen.getByText('更换资料来源'));
    fireEvent.click(screen.getByRole('button', { name: '一键填入当前草稿' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    await screen.findByRole('button', { name: '一键填入当前草稿' });
    expect(requests[0].targets[0].price_draft.table.rates.input).toBe(123);
    expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '123');
    fireEvent.click(screen.getByLabelText('强制覆盖所选分组'));
    fireEvent.click(screen.getByRole('button', { name: '一键填入当前草稿' }));
    await waitFor(() => expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '1'));
    expect(requests[1].force_overwrite).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: '放弃草稿' }));
    expect(callbacks.onDiscard).toHaveBeenCalledOnce();
    expect(callbacks.onApply).not.toHaveBeenCalled();
    expect(fetcher.mock.calls.filter(([url]) => url.endsWith('/apply') || url.endsWith('/prices'))).toHaveLength(0);
  });

  it('does not apply an in-flight quick fill after the price draft changes', async () => {
    let finish!: (response: Response) => void;
    vi.stubGlobal('fetch', vi.fn((url: string) => url.endsWith('/prefill/preview') ? new Promise<Response>(resolve => { finish = resolve; }) : Promise.resolve(new Response('[]'))));
    const callbacks = editor();
    fireEvent.click(screen.getByText('更换资料来源'));
    fireEvent.click(screen.getByRole('button', { name: '一键填入当前草稿' }));
    fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '123' } });
    await act(async () => finish(new Response(JSON.stringify({ patches: [{ config: {}, price: price(), warnings: [] }], updated: 1, skipped: 0 }))));
    expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '123');
    expect(callbacks.onDraft).toHaveBeenCalledOnce();
    expect(callbacks.onApply).not.toHaveBeenCalled();
  });
});
