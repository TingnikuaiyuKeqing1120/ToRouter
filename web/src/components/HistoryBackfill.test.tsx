// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { PriceTable } from '../api';
import type { BackfillGroup, BackfillMetric, BackfillPreview, BackfillRequest } from '../types/catalog-operations';
import { ConfirmProvider } from './Confirm';
import { HistoryBackfill } from './HistoryBackfill';

const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const table: PriceTable = {
  currency: 'USD', unit_tokens: 1000, rates: { input: 1, output: 0, cache_write: 2, cache_read: null },
  rules: [
    { id: 'long', enabled: true, when: { input_range: { from_tokens: 1001, to_tokens: null } }, action: { rate_changes: { input: 3, cache_read: 0 }, multiplier: 2 } },
    { id: 'night', enabled: false, when: { time_window: { start: '22:00', end: '06:00', all_day: false, timezone: 'Asia/Shanghai' }, weekdays: [1, 3] }, action: { multiplier: .5 } },
  ], uncovered_conditions: ['Holiday promotion'],
};
function group(key: string, metric: BackfillMetric = 'actual', model = 'model-a', channel = 7, name = '渠道 A'): BackfillGroup {
  return {
    key, metric, channel_id: metric === 'actual' ? channel : 0, channel_name: metric === 'actual' ? name : '', model,
    public_models: metric === 'actual' ? ['public-a', 'public-b'] : [], records: 2, calculable_amounts: 2,
    missing_reasons: { price_unconfigured: 2 }, first_time: '2026-10-01T00:00:00Z', last_time: '2026-10-17T00:00:00Z',
    resolution: { status: 'resolved', layer: metric === 'actual' ? 'channel' : 'official', channel_id: channel, model, table },
  };
}
const actualA = group('actual-a'), actualB = group('actual-b', 'actual', 'model-b', 9, '渠道 B'), official = group('official-a', 'official', 'public-a');
function preview(request: BackfillRequest, groups: BackfillGroup[] = [actualA]): BackfillPreview {
  return {
    request: { ...request, max_usage_id: 81 }, examined: 4, calculable_amounts: groups.reduce((sum, item) => sum + item.calculable_amounts, 0),
    existing_amounts: 3, missing_amounts: groups.reduce((sum, item) => sum + item.records, 0), missing_reasons: { usage_unknown: 1 }, groups,
    expected_resolution: groups.map(item => ({ key: item.key, resolution: item.resolution })), applied_amounts: 0,
  };
}
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done; }); return { promise, resolve }; }
function setupFetch(getGroups: (request: BackfillRequest) => BackfillGroup[] = () => [actualA], applyStatus = 200, applyMessage = '') {
  const fetcher = vi.fn().mockImplementation((url: string, options: RequestInit) => {
    const request = JSON.parse(String(options.body)) as BackfillRequest;
    const groups = getGroups(request).filter(item => request.amounts.includes(item.metric)
      && (!request.filter.upstream_model || item.metric !== 'actual' || item.model === request.filter.upstream_model)
      && (!request.filter.model || item.metric !== 'official' || item.model === request.filter.model));
    if (url.endsWith('/preview')) return Promise.resolve(response(preview(request, groups)));
    if (url.endsWith('/apply')) return Promise.resolve(applyStatus === 200 ? response({ ...preview(request, groups), applied_amounts: 2 }) : response({ error: { message: applyMessage } }, applyStatus));
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher); return fetcher;
}
function requests(fetcher: ReturnType<typeof setupFetch>, endpoint = 'preview'): BackfillRequest[] { return fetcher.mock.calls.filter(call => String(call[0]).endsWith('/' + endpoint)).map(call => JSON.parse(String(call[1].body))); }
function mount() { const onApplied = vi.fn(), onClose = vi.fn(); render(<ConfirmProvider><HistoryBackfill channels={[]} onApplied={onApplied} onClose={onClose} /></ConfirmProvider>); return { onApplied, onClose }; }
async function loaded() { await waitFor(() => expect(screen.getByRole('button', { name: '预填当前价格' })).toHaveProperty('disabled', false)); }
const rowInput = (name: string, label = '输入价格') => screen.getByRole('spinbutton', { name: `${name} ${label}` });
const edit = (name: string, value: string, label = '输入价格') => fireEvent.change(rowInput(name, label), { target: { value } });
async function prefill() { fireEvent.click(screen.getByRole('button', { name: '预填当前价格' })); await screen.findByText(/已预填/); }
function openScope() { const details = screen.getByText('调整范围').closest('details')!; if (!details.open) fireEvent.click(screen.getByText('调整范围')); }
async function applyScope(model: string) { openScope(); fireEvent.change(screen.getByLabelText('补算模型范围'), { target: { value: model } }); fireEvent.click(screen.getByRole('button', { name: '应用范围' })); await loaded(); }

beforeEach(() => {
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function(this: HTMLDialogElement) { this.setAttribute('open', ''); } });
  Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value: function(this: HTMLDialogElement) { this.removeAttribute('open'); } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('historical pricing workflow', () => {
  it('applies the current manual backfill before an accepted exit without changing price configuration', async () => {
    const fetcher = setupFetch(); const { onClose, onApplied } = mount(); await loaded();
    edit('渠道 A · model-a', '2');
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    const confirmation = await screen.findByRole('dialog', { name: '应用本次填写？' });
    expect(within(confirmation).queryByRole('button', { name: '继续编辑' })).toBeNull();
    expect(within(confirmation).getByText(/当前范围/)).toBeTruthy();
    fireEvent.click(within(confirmation).getByRole('button', { name: '确认应用' }));
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(onApplied).toHaveBeenCalledOnce(); expect(requests(fetcher, 'apply')[0]).toMatchObject({ mode: 'manual', amounts: ['actual'], prices: [{ key: actualA.key }] });
    expect(fetcher.mock.calls.every(([url]) => url.includes('/usage/backfill/'))).toBe(true);
  });

  it('keeps manual backfill drafts after an exit application conflict', async () => {
    setupFetch(() => [actualA], 409, 'configuration or source changed'); const { onClose } = mount(); await loaded();
    edit('渠道 A · model-a', '2'); fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    fireEvent.click(await screen.findByRole('button', { name: '确认应用' }));
    await screen.findByText('价格或记录已变化，请刷新缺口后重试。填写内容已保留。');
    expect(onClose).not.toHaveBeenCalled(); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '2');
  });

  it('retains edits in another metric when only the current metric is applied on exit', async () => {
    setupFetch(() => [actualA, official]); const { onClose } = mount(); await loaded();
    edit('渠道 A · model-a', '2'); fireEvent.click(screen.getByRole('button', { name: '等价 API 价值' })); edit('public-a', '3');
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' })); fireEvent.click(await screen.findByRole('button', { name: '确认应用' }));
    await screen.findByText('当前范围已应用，仍有其他未完成的填写；请处理后再关闭，或选择放弃填写。');
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '实际支出' })); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '2');
  });

  it('automatically previews all history, sorts channel/model rows, and orders the four inline prices', async () => {
    const a10 = group('a10', 'actual', 'model-10'), a2 = group('a2', 'actual', 'model-2');
    const fetcher = setupFetch(() => [actualB, a10, a2]); const { onApplied } = mount(); await loaded();
    expect(requests(fetcher)).toEqual([{ filter: {}, amounts: ['actual', 'official'], mode: 'current' }]);
    expect(requests(fetcher, 'apply')).toEqual([]); expect(onApplied).not.toHaveBeenCalled();
    const list = screen.getByRole('table', { name: '未计价缺口' });
    expect(within(list).getAllByRole('columnheader').map(item => item.textContent)).toEqual(['渠道', '上游模型 ID', '输入价格', '输出价格', '写入价格', '读取价格', '详情']);
    expect(within(list).getAllByRole('row').slice(1).map(row => within(row).getAllByRole('spinbutton')[0].getAttribute('aria-label'))).toEqual(['渠道 A · model-2 输入价格', '渠道 A · model-10 输入价格', '渠道 B · model-b 输入价格']);
    expect(rowInput('渠道 A · model-2')).toHaveProperty('value', '');
    expect(screen.getByText('调整范围').closest('details')).toHaveProperty('open', false);
    expect(screen.queryByRole('button', { name: '预览补算' })).toBeNull();
  });

  it('only commits after prefill and Apply, re-previewing manual prices with the fixed snapshot and expected quotes', async () => {
    let applied = false; const fetcher = setupFetch(() => applied ? [] : [actualA]); const { onApplied } = mount(); await loaded();
    onApplied.mockImplementation(() => { applied = true; });
    expect(screen.getByRole('button', { name: '应用' })).toHaveProperty('disabled', true);
    expect(screen.queryByRole('button', { name: '应用当前价格' })).toBeNull();
    expect(screen.queryByRole('button', { name: '应用填写价格' })).toBeNull();
    await prefill(); expect(requests(fetcher, 'apply')).toEqual([]); edit('渠道 A · model-a', '0');
    fireEvent.click(screen.getByRole('button', { name: '应用' })); await screen.findByText('当前范围没有未计价记录');
    expect(requests(fetcher)).toHaveLength(4);
    const request = requests(fetcher)[2];
    expect(request).toEqual({ filter: {}, amounts: ['actual'], mode: 'manual', prices: [{ key: 'actual-a', table: { ...table, unit_tokens: 1000000, rates: { input: 0, output: 0, cache_write: 2000, cache_read: null }, rules: [{ ...table.rules[0], action: { rate_changes: { input: 3000, cache_read: 0 }, multiplier: 2 } }, table.rules[1]] } }] });
    expect(requests(fetcher, 'apply')).toEqual([{ ...request, max_usage_id: 81, expected_resolution: preview(request).expected_resolution }]);
    expect(onApplied).toHaveBeenCalledOnce();
  });

  it('submits different manual prices per group, preserves zero and null, and never writes live prices', async () => {
    const fetcher = setupFetch(() => [actualB, actualA]); mount(); await loaded();
    edit('渠道 A · model-a', '0'); edit('渠道 A · model-a', '12', '输出价格'); edit('渠道 A · model-a', '', '输出价格');
    edit('渠道 B · model-b', '7'); edit('渠道 B · model-b', '3', '写入价格');
    fireEvent.click(screen.getByRole('button', { name: '应用' })); await waitFor(() => expect(requests(fetcher, 'apply')).toHaveLength(1));
    const request = requests(fetcher).find(item => item.mode === 'manual')!;
    expect(request.amounts).toEqual(['actual']);
    expect(request.prices).toEqual([
      { key: 'actual-a', table: { currency: 'USD', unit_tokens: 1000000, rates: { input: 0, output: null, cache_write: null, cache_read: null }, rules: [] } },
      { key: 'actual-b', table: { currency: 'USD', unit_tokens: 1000000, rates: { input: 7, output: null, cache_write: 3, cache_read: null }, rules: [] } },
    ]);
    expect(requests(fetcher, 'apply')[0]).toEqual({ ...request, max_usage_id: 81, expected_resolution: preview(request, [actualB, actualA]).expected_resolution });
    expect(fetcher.mock.calls.every(call => ['/api/usage/backfill/preview', '/api/usage/backfill/apply'].includes(call[0]))).toBe(true);
  });

  it('shows one disabled submitting action until the async apply actually completes', async () => {
    const pending = deferred<Response>(); let application: BackfillRequest | undefined;
    const fetcher = vi.fn().mockImplementation((url: string, options: RequestInit) => {
      const request = JSON.parse(String(options.body)) as BackfillRequest;
      if (url.endsWith('/apply')) { application = request; return pending.promise; }
      return Promise.resolve(response(preview(request)));
    });
    vi.stubGlobal('fetch', fetcher); const { onApplied } = mount(); await loaded(); edit('渠道 A · model-a', '0');
    fireEvent.click(screen.getByRole('button', { name: '应用' }));
    const button = await screen.findByRole('button', { name: '正在应用…' });
    expect(button).toHaveProperty('disabled', true); expect(screen.getByRole('button', { name: '预填当前价格' })).toHaveProperty('disabled', true);
    expect(onApplied).not.toHaveBeenCalled(); fireEvent.click(button); expect(requests(fetcher, 'apply')).toHaveLength(1);
    expect(application).toMatchObject({ amounts: ['actual'], mode: 'manual', max_usage_id: 81 });
    await act(async () => pending.resolve(response({ ...preview(application!), applied_amounts: 2 })));
    await waitFor(() => expect(onApplied).toHaveBeenCalledOnce()); await loaded(); expect(screen.getByRole('button', { name: '应用' })).toHaveProperty('disabled', true);
  });

  it('refreshes quotes on prefill, normalizes all rule rates to 1M, and retains full conditions and notes through edits', async () => {
    let quoted = structuredClone(table); const fetcher = setupFetch(() => [{ ...actualA, resolution: { ...actualA.resolution, table: quoted } }]); mount(); await loaded();
    quoted = { ...quoted, rates: { ...quoted.rates, input: 4 } }; await prefill();
    expect(requests(fetcher)).toHaveLength(2); expect(requests(fetcher, 'apply')).toEqual([]);
    expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '4000'); expect(rowInput('渠道 A · model-a', '输出价格')).toHaveProperty('value', '0'); expect(rowInput('渠道 A · model-a', '读取价格')).toHaveProperty('value', '');
    fireEvent.click(screen.getByRole('button', { name: '渠道 A · model-a 详情' }));
    expect(screen.getByText('条件报价 · 2 项').closest('details')).toHaveProperty('open', false); expect(screen.getByText('Holiday promotion').closest('details')).toHaveProperty('open', false);
    fireEvent.click(screen.getByText('价格备注 · 1 项')); expect(screen.getByText('Holiday promotion').closest('details')).toHaveProperty('open', true);
    edit('渠道 A · model-a', '0'); await prefill(); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '0');
    fireEvent.click(screen.getByRole('button', { name: '应用' })); await waitFor(() => expect(requests(fetcher, 'apply')).toHaveLength(1));
    expect(requests(fetcher, 'apply')[0].prices![0].table).toEqual({ ...table, unit_tokens: 1000000, rates: { input: 0, output: 0, cache_write: 2000, cache_read: null }, rules: [{ ...table.rules[0], action: { rate_changes: { input: 3000, cache_read: 0 }, multiplier: 2 } }, table.rules[1]] });
  });

  it('preserves hand-edited zero before first prefill and refreshes only unedited groups on repeated prefill', async () => {
    let amount = 1; const fetcher = setupFetch(() => [actualA, { ...actualB, resolution: { ...actualB.resolution, table: { ...table, rates: { ...table.rates, input: amount } } } }]);
    mount(); await loaded(); edit('渠道 A · model-a', '0'); await prefill();
    expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '0'); expect(rowInput('渠道 B · model-b')).toHaveProperty('value', '1000');
    amount = 5; await prefill(); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '0'); expect(rowInput('渠道 B · model-b')).toHaveProperty('value', '5000'); expect(requests(fetcher, 'apply')).toEqual([]);
  });

  it('switches locally without clearing the table, keeps per-amount counts and drafts, and applies only the visible amount', async () => {
    const fetcher = setupFetch(() => [actualA, { ...official, records: 7, calculable_amounts: 3 }]); mount(); await loaded(); edit('渠道 A · model-a', '11');
    const list = screen.getByRole('table', { name: '未计价缺口' });
    expect(screen.getByRole('heading', { name: '未计价缺口 2 条' })).toBeTruthy(); expect(screen.getByText('当前价格可补 2 条')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '等价 API 价值' }));
    expect(screen.getByRole('table', { name: '未计价缺口' })).toBe(list); expect(screen.queryByText('正在检查未计价记录…')).toBeNull(); expect(requests(fetcher)).toHaveLength(1);
    expect(screen.getByRole('heading', { name: '未计价缺口 7 条' })).toBeTruthy(); expect(screen.getByText('当前价格可补 3 条')).toBeTruthy();
    expect(screen.queryByRole('columnheader', { name: '渠道' })).toBeNull(); expect(screen.getByRole('columnheader', { name: '公开模型 ID' })).toBeTruthy();
    expect(rowInput('public-a')).toHaveProperty('value', '');
    await prefill(); expect(requests(fetcher).slice(-1)[0]).toEqual({ filter: {}, amounts: ['actual', 'official'], mode: 'current' }); edit('public-a', '22');
    fireEvent.click(screen.getByRole('button', { name: '实际支出' })); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '11');
    fireEvent.click(screen.getByRole('button', { name: '等价 API 价值' })); expect(rowInput('public-a')).toHaveProperty('value', '22'); expect(requests(fetcher)).toHaveLength(2);
    fireEvent.click(screen.getByRole('button', { name: '应用' })); await waitFor(() => expect(requests(fetcher, 'apply')).toHaveLength(1));
    expect(requests(fetcher, 'apply')[0].amounts).toEqual(['official']); expect(requests(fetcher, 'apply')[0].prices?.map(item => item.key)).toEqual(['official-a']);
    fireEvent.click(screen.getByRole('button', { name: '实际支出' })); await loaded(); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '11');
  });

  it('prefills and applies only groups in the committed filter, preserving drafts hidden by the filter', async () => {
    const fetcher = setupFetch(() => [actualA, actualB, official]); mount(); await loaded(); edit('渠道 A · model-a', '11');
    const list = screen.getByRole('table', { name: '未计价缺口' }); await applyScope('model-b');
    expect(requests(fetcher)).toHaveLength(1); expect(screen.getByRole('table', { name: '未计价缺口' })).toBe(list);
    expect(screen.queryByRole('spinbutton', { name: '渠道 A · model-a 输入价格' })).toBeNull(); await prefill();
    expect(requests(fetcher).slice(-1)[0]).toEqual({ filter: {}, amounts: ['actual', 'official'], mode: 'current' }); expect(screen.getByText(/已预填 1 项/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '等价 API 价值' })); expect(rowInput('public-a')).toHaveProperty('value', '');
    fireEvent.click(screen.getByRole('button', { name: '实际支出' })); edit('渠道 B · model-b', '22');
    fireEvent.click(screen.getByRole('button', { name: '应用' })); await waitFor(() => expect(requests(fetcher, 'apply')).toHaveLength(1));
    expect(requests(fetcher, 'apply')[0].filter).toEqual({ upstream_model: 'model-b' }); expect(requests(fetcher, 'apply')[0].prices?.map(item => item.key)).toEqual(['actual-b']);
    await loaded(); fireEvent.click(screen.getByRole('button', { name: '重置范围' })); await loaded(); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '11');
  });

  it('uses public model filters for official amounts and commits date and channel filters explicitly', async () => {
    const hidden = group('official-hidden', 'official', 'public-z');
    const fetcher = setupFetch(() => [actualA, official, hidden]); mount(); await loaded(); openScope();
    fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: '2026-10-02' } }); fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '2026-10-04' } }); fireEvent.change(screen.getByLabelText('补算渠道范围'), { target: { value: '7' } }); expect(requests(fetcher)).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: '应用范围' })); await loaded();
    expect(requests(fetcher).slice(-1)[0]?.filter).toEqual({ from: new Date(2026, 9, 2).toISOString(), to: new Date(2026, 9, 4, 23, 59, 59, 999).toISOString(), channel_id: 7 });
    fireEvent.click(screen.getByRole('button', { name: '等价 API 价值' })); await applyScope('public-a'); expect(requests(fetcher)).toHaveLength(2); await prefill();
    const sharedFilter = { from: new Date(2026, 9, 2).toISOString(), to: new Date(2026, 9, 4, 23, 59, 59, 999).toISOString(), channel_id: 7 };
    expect(requests(fetcher).slice(-1)[0]).toEqual({ filter: sharedFilter, amounts: ['actual', 'official'], mode: 'current' }); expect(screen.getByText(/已预填 1 项/)).toBeTruthy();
    await applyScope(''); expect(rowInput('public-z')).toHaveProperty('value', ''); expect(rowInput('public-a')).toHaveProperty('value', '1000'); await applyScope('public-a');
    fireEvent.click(screen.getByRole('button', { name: '应用' })); await waitFor(() => expect(requests(fetcher, 'apply')).toHaveLength(1));
    expect(requests(fetcher, 'apply')[0]).toMatchObject({ filter: { ...sharedFilter, model: 'public-a' }, amounts: ['official'], mode: 'manual' }); expect(requests(fetcher, 'apply')[0].filter).not.toHaveProperty('upstream_model'); expect(requests(fetcher, 'apply')[0].prices?.map(item => item.key)).toEqual(['official-a']);
  });

  it('disables prefill and both apply actions while scope changes are uncommitted and retains the previous draft', async () => {
    const fetcher = setupFetch(); mount(); await loaded(); edit('渠道 A · model-a', '3'); openScope(); fireEvent.change(screen.getByLabelText('补算模型范围'), { target: { value: 'new-model' } }); expect(screen.getByText('筛选待应用')).toBeTruthy();
    for (const label of ['预填当前价格', '应用']) { const button = screen.getByRole('button', { name: label }); expect(button).toHaveProperty('disabled', true); fireEvent.click(button); }
    expect(requests(fetcher)).toHaveLength(1); expect(requests(fetcher, 'apply')).toEqual([]); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '3');
    fireEvent.change(screen.getByLabelText('补算模型范围'), { target: { value: '' } }); await loaded(); expect(screen.getByRole('button', { name: '应用' })).toHaveProperty('disabled', false);
  });

  it('rejects reversed dates and negative prices before making any mutation request', async () => {
    const fetcher = setupFetch(); mount(); await loaded(); openScope(); fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: '2026-10-20' } }); fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '2026-10-01' } });
    fireEvent.click(screen.getByRole('button', { name: '应用范围' })); expect(screen.getByRole('alert').textContent).toBe('开始日期不能晚于结束日期'); expect(requests(fetcher)).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: '重置范围' })); edit('渠道 A · model-a', '-1'); expect(screen.getByRole('alert').textContent).toContain('大于或等于 0'); expect(screen.getByRole('button', { name: '应用' })).toHaveProperty('disabled', true); expect(requests(fetcher, 'apply')).toEqual([]);
  });

  it('uses the selected amount when a pending shared snapshot arrives without requesting it again', async () => {
    const late = deferred<Response>(); const fetcher = vi.fn().mockReturnValue(late.promise);
    vi.stubGlobal('fetch', fetcher); mount(); fireEvent.click(screen.getByRole('button', { name: '等价 API 价值' }));
    expect(fetcher).toHaveBeenCalledOnce();
    await act(async () => late.resolve(response(preview({ filter: {}, amounts: ['actual', 'official'], mode: 'current' }, [actualA, official])))); await loaded();
    expect(screen.getByRole('button', { name: '等价 API 价值' }).getAttribute('aria-pressed')).toBe('true'); expect(rowInput('public-a')).toHaveProperty('value', ''); expect(screen.queryByRole('spinbutton', { name: '渠道 A · model-a 输入价格' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '实际支出' })); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', ''); expect(fetcher).toHaveBeenCalledOnce();
  });

  it.each(['date', 'channel'])('ignores a late response from an obsolete shared %s range', async field => {
    const pending: { request: BackfillRequest; result: ReturnType<typeof deferred<Response>> }[] = [];
    const fetcher = vi.fn().mockImplementation((_url: string, options: RequestInit) => {
      const request = JSON.parse(String(options.body)) as BackfillRequest;
      if (!pending.length) { const result = deferred<Response>(); pending.push({ request, result }); result.resolve(response(preview(request, [actualA, actualB, official]))); return result.promise; }
      const result = deferred<Response>(); pending.push({ request, result }); return result.promise;
    });
    vi.stubGlobal('fetch', fetcher); mount(); await loaded(); openScope();
    const label = field === 'date' ? '开始日期' : '补算渠道范围';
    fireEvent.change(screen.getByLabelText(label), { target: { value: field === 'date' ? '2026-10-02' : '7' } }); fireEvent.click(screen.getByRole('button', { name: '应用范围' }));
    await waitFor(() => expect(pending).toHaveLength(2));
    fireEvent.change(screen.getByLabelText(label), { target: { value: field === 'date' ? '2026-10-03' : '9' } }); fireEvent.click(screen.getByRole('button', { name: '应用范围' }));
    await waitFor(() => expect(pending).toHaveLength(3));
    fireEvent.click(screen.getByRole('button', { name: '等价 API 价值' }));
    await act(async () => pending[2].result.resolve(response(preview(pending[2].request, [{ ...official, model: 'new-range' }])))); await loaded(); edit('new-range', '8');
    await act(async () => pending[1].result.resolve(response(preview(pending[1].request, [{ ...official, model: 'stale-range' }]))));
    expect(rowInput('new-range')).toHaveProperty('value', '8'); expect(screen.queryByRole('spinbutton', { name: 'stale-range 输入价格' })).toBeNull(); expect(requests(fetcher)).toHaveLength(3);
    expect(pending[2].request.amounts).toEqual(['actual', 'official']);
    expect(pending[2].request.filter).toEqual(field === 'date' ? { from: new Date(2026, 9, 3).toISOString() } : { channel_id: 9 });
  });

  it.each([[409, 'configuration or source changed; preview again', '请刷新缺口后重试'], [503, 'storage unavailable', 'storage unavailable']])('retains drafts after apply fails with %s and permits refresh', async (status, message, expected) => {
    const fetcher = setupFetch(undefined, status, message); const { onApplied } = mount(); await loaded(); edit('渠道 A · model-a', '0'); fireEvent.click(screen.getByRole('button', { name: '应用' })); expect((await screen.findByRole('alert')).textContent).toContain(expected);
    expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '0'); expect(onApplied).not.toHaveBeenCalled(); expect(requests(fetcher, 'apply')).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: '重试' })); await loaded(); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '0'); expect(screen.getByRole('button', { name: '应用' })).toHaveProperty('disabled', false);
  });

  it('does not apply a preview with no calculable records and preserves the draft', async () => {
    let canCalculate = true; const fetcher = setupFetch(() => [{ ...actualA, calculable_amounts: canCalculate ? 2 : 0 }]); const { onApplied } = mount(); await loaded(); edit('渠道 A · model-a', '4'); canCalculate = false;
    fireEvent.click(screen.getByRole('button', { name: '应用' })); await screen.findByText(/当前没有可补算的记录/); expect(requests(fetcher, 'apply')).toEqual([]); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '4'); expect(onApplied).not.toHaveBeenCalled();
  });

  it('retains prices, rules and notes when apply fills only part of a group, then completes the remainder', async () => {
    let applied = 0;
    const fetcher = vi.fn().mockImplementation((url: string, options: RequestInit) => {
      const request = JSON.parse(String(options.body)) as BackfillRequest;
      const records = applied ? 1 : 2;
      const calculable = request.mode === 'manual' ? 1 : applied ? 0 : 2;
      const result = preview(request, [{ ...actualA, records, calculable_amounts: calculable }]);
      if (url.endsWith('/apply')) { applied++; return Promise.resolve(response({ ...result, applied_amounts: 1 })); }
      return Promise.resolve(response(result));
    });
    vi.stubGlobal('fetch', fetcher); const { onApplied } = mount(); await loaded(); await prefill();
    edit('渠道 A · model-a', '', '输出价格'); edit('渠道 A · model-a', '2000');
    fireEvent.click(screen.getByRole('button', { name: '应用' }));
    await waitFor(() => expect(onApplied).toHaveBeenCalledOnce()); await loaded();
    expect(screen.getByText('1 条')).toBeTruthy();
    expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '2000');
    expect(rowInput('渠道 A · model-a', '输出价格')).toHaveProperty('value', '');
    fireEvent.click(screen.getByRole('button', { name: '渠道 A · model-a 详情' }));
    expect(screen.getByText('条件报价 · 2 项')).toBeTruthy(); expect(screen.getByText('价格备注 · 1 项')).toBeTruthy();
    edit('渠道 A · model-a', '5000', '输出价格');
    fireEvent.click(screen.getByRole('button', { name: '应用' }));
    await waitFor(() => expect(onApplied).toHaveBeenCalledTimes(2));
    const first = requests(fetcher, 'apply')[0].prices![0].table, second = requests(fetcher, 'apply')[1].prices![0].table;
    expect(first.rates).toEqual({ input: 2000, output: null, cache_write: 2000, cache_read: null });
    expect(second).toEqual({ ...first, rates: { ...first.rates, output: 5000 } });
    expect(second.rules).toEqual([{ ...table.rules[0], action: { rate_changes: { input: 3000, cache_read: 0 }, multiplier: 2 } }, table.rules[1]]);
    expect(second.uncovered_conditions).toEqual(['Holiday promotion']);
  });

  it('confirms edited draft disposal in the app and keeps drafts when cancellation is chosen', async () => {
    setupFetch(); const nativeConfirm = vi.spyOn(window, 'confirm'); const { onClose } = mount(); await loaded(); edit('渠道 A · model-a', '0'); fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' })); const confirm = await screen.findByRole('dialog', { name: '应用本次填写？' }); expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(within(confirm).getByRole('button', { name: '关闭弹窗' })); await waitFor(() => expect(screen.queryByRole('dialog', { name: '应用本次填写？' })).toBeNull()); expect(rowInput('渠道 A · model-a')).toHaveProperty('value', '0'); expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' })); fireEvent.click(await screen.findByRole('button', { name: '放弃填写' })); await waitFor(() => expect(onClose).toHaveBeenCalledOnce()); expect(nativeConfirm).not.toHaveBeenCalled();
  });

  it('closes a dialog containing only prefills without discard confirmation', async () => {
    setupFetch(); const { onClose } = mount(); await loaded(); await prefill(); fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' })); await waitFor(() => expect(onClose).toHaveBeenCalledOnce()); expect(screen.queryByRole('dialog', { name: '应用本次填写？' })).toBeNull();
  });
});
