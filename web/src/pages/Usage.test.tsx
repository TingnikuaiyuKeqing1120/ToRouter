// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import type { UsageGroup, UsageInputBucket, UsageMetrics, UsageSummary } from '../api';
import Usage from './Usage';
import { ConfirmProvider } from '../components/Confirm';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });
import type { BackfillRequest } from '../types/catalog-operations';
import type { UsageTrendSelection } from '../components/usage/UsageCharts';
import type { UsageMeasure } from '../components/usage/metrics';

vi.mock('../components/usage/UsageCharts', () => ({
  UsageTrend: ({ selection, onSelectionChange }: { selection: UsageTrendSelection; onSelectionChange: (next: UsageTrendSelection) => void }) => <section aria-label="用量趋势">
    <select aria-label="趋势维度" value={selection.dimension} onChange={event => onSelectionChange({ ...selection, dimension: event.target.value as UsageTrendSelection['dimension'] })}><option value="total">总体</option><option value="model">公开模型</option><option value="channel">渠道</option></select>
    <select aria-label="趋势指标" value={selection.measure} onChange={event => onSelectionChange({ ...selection, measure: event.target.value as UsageMeasure })}><option value="tokens">Token</option><option value="actual">实际支出</option><option value="official">等价API</option></select>
  </section>,
  UsageComposition: ({ title, measure, onMeasureChange }: { title: string; measure: UsageMeasure; onMeasureChange: (next: UsageMeasure) => void }) => <section aria-label={title}><select aria-label={title + '指标'} value={measure} onChange={event => onMeasureChange(event.target.value as UsageMeasure)}><option value="tokens">Token</option><option value="actual">实际支出</option><option value="official">等价API</option></select></section>,
  TokenComposition: ({ metrics }: { metrics: UsageMetrics }) => <div data-testid="analysis-tokens" data-input={metrics.input_tokens} data-read={metrics.cache_read_tokens} data-unknown={metrics.unknown_usage_requests} />,
  UsageInputDistribution: ({ buckets, unknownRequests }: { buckets: UsageInputBucket[]; unknownRequests: number }) => <div data-testid="analysis-distribution" data-values={JSON.stringify(buckets)} data-unknown={unknownRequests} />,
}));

const metrics: UsageMetrics = { requests: 4, failures: 1, input_tokens: 300, plain_input_tokens: 120, cache_read_tokens: 150, cache_write_tokens: 30, output_tokens: 100, known_usage_requests: 3, unknown_usage_requests: 1, actual_priced_requests: 3, actual_costs: { USD: 1, CNY: 7 }, official_priced_requests: 3, official_values: { USD: 4, CNY: 0 } };
const buckets: UsageInputBucket[] = [{ label: '0–4K', from_tokens: 0, to_tokens: 4000, requests: 3 }];
const alpha: UsageGroup = { ...metrics, name: 'alpha-public', requests: 3, unknown_usage_requests: 0, daily: [{ ...metrics, date: '2026-10-14', requests: 3, unknown_usage_requests: 0 }], input_distribution: buckets };
const beta: UsageGroup = { ...metrics, name: 'beta-public', requests: 1, failures: 0, input_tokens: 0, plain_input_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, output_tokens: 0, known_usage_requests: 0, unknown_usage_requests: 1, actual_priced_requests: 0, actual_costs: { USD: 0, CNY: 0 }, official_priced_requests: 0, official_values: { USD: 0, CNY: 0 }, daily: [], input_distribution: [] };
const summary: UsageSummary = { ...metrics, daily: [{ ...metrics, date: '2026-10-14' }], by_model: [alpha, beta], by_channel: [{ ...metrics, name: '主渠道', channel_id: 7, models: [alpha, beta], daily: [{ ...metrics, date: '2026-10-14' }], input_distribution: buckets }], input_distribution: buckets, timezone: Intl.DateTimeFormat().resolvedOptions().timeZone, actual_components: { USD: { input: .1, output: .2, cache_read: .3, cache_write: .4, unattributed: 0 }, CNY: { input: 2, output: 2, cache_read: 2, cache_write: 1, unattributed: 0 } }, official_components: { USD: { input: 1, output: 1, cache_read: 1, cache_write: 1, unattributed: 0 }, CNY: { input: 0, output: 0, cache_read: 0, cache_write: 0, unattributed: 0 } } };
const rate = { available: true, base: 'USD', quote: 'CNY', rate: 7, date: '2026-10-14', attempted_on: '', fetched_at: '', stale: false, error: '', source_url: '' };
const response = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
function setupFetch(exportResponse: () => Response = () => new Response('time,model\n2026-10-14,alpha-public', { headers: { 'Content-Type': 'text/csv' } }), data: UsageSummary = summary, usageResponse?: (url: string) => Promise<Response>) {
  const fetcher = vi.fn().mockImplementation((url: string, options?: RequestInit) => {
    if (url.startsWith('/api/usage/export?')) return Promise.resolve(exportResponse());
    if (url.startsWith('/api/usage?')) return usageResponse ? usageResponse(url) : Promise.resolve(response(data));
    if (url === '/api/exchange-rate') return Promise.resolve(response(rate));
    if (url === '/api/channels') return Promise.resolve(response([]));
    if (url === '/api/usage/backfill/preview') {
      const request = JSON.parse(String(options?.body)) as BackfillRequest;
      return Promise.resolve(response({ request: { ...request, max_usage_id: 81 }, examined: 4, calculable_amounts: 0, existing_amounts: 1, missing_amounts: 3, missing_reasons: { price_unconfigured: 3 }, groups: [], expected_resolution: [], applied_amounts: 0 }));
    }
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
function usageCalls(fetcher: ReturnType<typeof setupFetch>): string[] { return fetcher.mock.calls.map(call => String(call[0])).filter(url => url.startsWith('/api/usage?')); }
function params(url: string) { return new URL(url, 'http://localhost').searchParams; }
function navigate(name: string) { fireEvent.click(within(screen.getByRole('navigation', { name: '用量页导航' })).getByRole('button', { name })); }
async function loaded() { await screen.findByRole('region', { name: '请求数' }); await waitFor(() => expect(screen.getByRole('button', { name: '刷新数据' }).hasAttribute('disabled')).toBe(false)); }
function editCustom(from: string, to: string) {
  fireEvent.click(screen.getByRole('button', { name: '自定义' }));
  fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: from } });
  fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: to } });
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date(2026, 9, 18, 14, 30, 45, 678));
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function(this: HTMLDialogElement) { this.setAttribute('open', ''); } });
  Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value: function(this: HTMLDialogElement) { this.removeAttribute('open'); } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('usage workspace interactions', () => {
  it('shares all-time history across sections, refreshes it and exports without date limits', async () => {
    const fetcher = setupFetch();
    const NativeURL = URL;
    vi.stubGlobal('URL', class extends NativeURL { static createObjectURL = vi.fn().mockReturnValue('blob:all-usage'); static revokeObjectURL = vi.fn(); });
    const downloads: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function(this: HTMLAnchorElement) { downloads.push(this.download); });
    render(<Usage />); await loaded();
    editCustom('2026-10-03', '2026-10-08');
    fireEvent.click(screen.getByRole('button', { name: '全部' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(2)); await loaded();
    const expected = { timezone: Intl.DateTimeFormat().resolvedOptions().timeZone };
    expect(Object.fromEntries(params(usageCalls(fetcher)[1]))).toEqual(expected);
    expect(screen.getByRole('button', { name: '全部' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.queryByLabelText('开始日期')).toBeNull();
    expect(screen.queryByText('日期待应用')).toBeNull();
    for (const name of ['用量明细', '使用分析', '导出与汇率', '用量总览']) navigate(name);
    expect(usageCalls(fetcher)).toHaveLength(2);
    fireEvent.click(screen.getByRole('button', { name: '刷新数据' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(3)); await loaded();
    expect(Object.fromEntries(params(usageCalls(fetcher)[2]))).toEqual(expected);
    navigate('导出与汇率');
    fireEvent.click(screen.getByRole('button', { name: '导出 CSV' }));
    await screen.findByText('明细已导出');
    const exported = fetcher.mock.calls.find(call => String(call[0]).startsWith('/api/usage/export?'))!;
    expect(Object.fromEntries(params(String(exported[0])))).toEqual(expected);
    expect(downloads).toEqual(['torouter-usage-all.csv']);
    fireEvent.click(screen.getByRole('button', { name: '自定义' }));
    expect(screen.getByLabelText('开始日期')).toHaveProperty('value', '2026-10-03');
    fireEvent.click(screen.getByRole('button', { name: '应用' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(4));
    expect(params(usageCalls(fetcher)[3]).get('from')).toBe(new Date(2026, 9, 3).toISOString());
    expect(screen.getByRole('button', { name: '全部' }).getAttribute('aria-pressed')).toBe('false');
    await waitFor(() => expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:all-usage'), { timeout: 1600 });
  });

  it('labels retained all-time results correctly when changing back to a bounded range', async () => {
    const pending = deferred<Response>();
    let calls = 0;
    setupFetch(undefined, summary, () => ++calls < 3 ? Promise.resolve(response(summary)) : pending.promise);
    render(<Usage />); await loaded();
    fireEvent.click(screen.getByRole('button', { name: '全部' }));
    await waitFor(() => expect(calls).toBe(2)); await loaded();
    fireEvent.click(screen.getByRole('button', { name: '自然周期' }));
    await waitFor(() => expect(screen.getByRole('status').textContent).toContain('暂显示 全部历史数据 的上次结果'));
    expect(document.body.textContent).not.toContain('NaN');
    await act(async () => pending.resolve(response(summary)));
    await loaded();
    expect(screen.getByText('本月至今')).toBeTruthy();
  });

  it('includes monthly payments in the overview but keeps model money unallocated and warns of missing monthly configuration', async () => {
    const planMetrics = { ...metrics, requests: 2, failures: 0, plan_requests: 2, actual_priced_requests: 0, actual_costs: { USD: 0, CNY: 0 } };
    const planModel = { ...alpha, ...planMetrics, name: 'plan-model' };
    setupFetch(undefined, { ...summary, ...planMetrics, plan_charges: 1, plan_costs: { USD: 0, CNY: 20 }, actual_costs: { USD: 0, CNY: 20 }, unconfigured_plan_channels: 1, by_model: [planModel], actual_components: { ...summary.actual_components, CNY: { input: 0, output: 0, cache_read: 0, cache_write: 0, unattributed: 0, plan: 20 } } });
    render(<Usage />); await loaded();
    const actual = within(screen.getByRole('region', { name: '实际支出' }));
    expect(actual.getByText('¥20.00', { selector: '.usage-card-value span' })).toBeTruthy();
    expect(actual.getByText('按量已计价 0 / 0')).toBeTruthy();
    expect(actual.getByText('套餐内 2 次')).toBeTruthy();
    expect(actual.getByRole('link', { name: /1 个套餐渠道尚未配置月费/ }).getAttribute('href')).toBe('#/prices?view=plan');
    navigate('用量明细');
    fireEvent.click(screen.getByRole('button', { name: '按公开模型' }));
    expect(screen.getByText('套餐内')).toBeTruthy();
  });
  it('retains independent chart selections across sections and while refreshing', async () => {
    let pending: ReturnType<typeof deferred<Response>> | null = null;
    setupFetch(undefined, summary, () => pending?.promise ?? Promise.resolve(response(summary)));
    render(<Usage />); await loaded();
    fireEvent.change(screen.getByLabelText('趋势维度'), { target: { value: 'channel' } });
    fireEvent.change(screen.getByLabelText('趋势指标'), { target: { value: 'actual' } });
    fireEvent.change(screen.getByLabelText('公开模型构成指标'), { target: { value: 'official' } });
    fireEvent.change(screen.getByLabelText('渠道构成指标'), { target: { value: 'actual' } });
    navigate('用量明细'); navigate('用量总览');
    const assertSelections = () => {
      expect(screen.getByLabelText('趋势维度')).toHaveProperty('value', 'channel');
      expect(screen.getByLabelText('趋势指标')).toHaveProperty('value', 'actual');
      expect(screen.getByLabelText('公开模型构成指标')).toHaveProperty('value', 'official');
      expect(screen.getByLabelText('渠道构成指标')).toHaveProperty('value', 'actual');
    };
    assertSelections();
    pending = deferred<Response>();
    vi.setSystemTime(new Date(2026, 9, 18, 14, 31));
    fireEvent.click(screen.getByRole('button', { name: '刷新数据' }));
    expect(screen.getByRole('status').textContent).toContain('正在更新');
    assertSelections();
    await act(async () => pending!.resolve(response(summary)));
    await waitFor(() => expect(screen.queryByRole('status')).toBeNull());
    assertSelections();
  });

  it('retains the analysis model and detail dimension, search and channel expansion', async () => {
    setupFetch(); render(<Usage />); await loaded();
    navigate('使用分析');
    fireEvent.change(screen.getByLabelText('分析公开模型'), { target: { value: 'alpha-public' } });
    navigate('导出与汇率'); navigate('使用分析');
    expect(screen.getByLabelText('分析公开模型')).toHaveProperty('value', 'alpha-public');
    expect(screen.getByTestId('analysis-tokens').dataset.unknown).toBe('0');
    navigate('用量明细');
    fireEvent.click(screen.getByRole('button', { name: '展开渠道 主渠道' }));
    fireEvent.change(screen.getByLabelText('搜索渠道'), { target: { value: '主渠道' } });
    navigate('用量总览'); navigate('用量明细');
    expect(screen.getByLabelText('搜索渠道')).toHaveProperty('value', '主渠道');
    expect(screen.getByRole('button', { name: '展开渠道 主渠道' }).getAttribute('aria-expanded')).toBe('true');
    expect(screen.getByRole('cell', { name: 'alpha-public' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '按公开模型' }));
    fireEvent.change(screen.getByLabelText('搜索公开模型'), { target: { value: 'beta-public' } });
    navigate('使用分析'); navigate('用量明细');
    expect(screen.getByRole('button', { name: '按公开模型' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByLabelText('搜索公开模型')).toHaveProperty('value', 'beta-public');
    expect(screen.queryByRole('cell', { name: 'alpha-public' })).toBeNull();
    expect(screen.getByRole('cell', { name: 'beta-public' })).toBeTruthy();
  });

  it('labels retained data with its successful range during pending and failed updates, then binds the new result', async () => {
    let next = deferred<Response>();
    let calls = 0;
    const fetcher = setupFetch(undefined, summary, () => ++calls === 1 ? Promise.resolve(response(summary)) : next.promise);
    render(<Usage />); await loaded();
    fireEvent.click(screen.getByRole('button', { name: '今日' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(2));
    expect(screen.getByText('今日至今')).toBeTruthy();
    expect(screen.getByRole('status').textContent).toBe('正在更新 · 暂显示 2026-10-01 — 2026-10-18 的上次结果');
    expect(screen.getByRole('region', { name: '请求数' }).textContent).toBe('请求数4成功 3失败 1');
    await act(async () => next.reject(new TypeError('Failed to fetch')));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', '用量加载失败，请重试。重试');
    expect(screen.getByText('Failed to fetch')).toBeTruthy();
    expect(screen.getByRole('status').textContent).toBe('更新未完成 · 暂显示 2026-10-01 — 2026-10-18 的上次结果');
    expect(screen.getByRole('region', { name: '请求数' }).textContent).toContain('请求数4');
    next = deferred<Response>();
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(screen.getByRole('status').textContent).toContain('2026-10-01 — 2026-10-18');
    await act(async () => next.resolve(response({ ...summary, requests: 9, failures: 0 })));
    await waitFor(() => expect(screen.getByRole('region', { name: '请求数' }).textContent).toBe('请求数9成功 9失败 0'));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();
    expect(within(screen.getByRole('region', { name: '统计时间范围' })).getByText('2026-10-18 — 2026-10-18')).toBeTruthy();
    expect(params(usageCalls(fetcher)[2]).get('from')).toBe(new Date(2026, 9, 18).toISOString());
    navigate('导出与汇率');
    expect(screen.getByText('9 条已发送记录')).toBeTruthy();
  });

  it('keeps a selected model with no records in the new period instead of falling back to totals', async () => {
    let calls = 0;
    setupFetch(undefined, summary, () => Promise.resolve(response(++calls === 1 ? summary : { ...summary, by_model: [beta] })));
    render(<Usage />); await loaded(); navigate('使用分析');
    fireEvent.change(screen.getByLabelText('分析公开模型'), { target: { value: 'alpha-public' } });
    fireEvent.click(screen.getByRole('button', { name: '今日' }));
    expect(await screen.findByText('所选模型暂无记录')).toBeTruthy();
    expect(screen.getByLabelText('分析公开模型')).toHaveProperty('value', 'alpha-public');
    expect(screen.getByRole('option', { name: 'alpha-public（本期间无记录）' })).toBeTruthy();
    expect(screen.queryByTestId('analysis-tokens')).toBeNull();
    expect(screen.queryByTestId('analysis-distribution')).toBeNull();
    navigate('用量总览'); navigate('使用分析');
    expect(screen.getByText('所选模型暂无记录')).toBeTruthy();
  });

  it('keeps the current committed range across all four sections without refetching', async () => {
    const fetcher = setupFetch(); render(<Usage />); await loaded();
    editCustom('2026-10-02', '2026-10-04');
    fireEvent.click(screen.getByRole('button', { name: '应用' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(2));
    await loaded();
    const query = params(usageCalls(fetcher)[1]);
    expect(query.get('from')).toBe(new Date(2026, 9, 2).toISOString());
    expect(query.get('to')).toBe(new Date(2026, 9, 4, 23, 59, 59, 999).toISOString());
    expect(query.get('timezone')).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
    for (const name of ['用量明细', '使用分析', '导出与汇率', '用量总览']) {
      navigate(name);
      expect(within(screen.getByRole('navigation', { name: '用量页导航' })).getByRole('button', { name }).getAttribute('aria-current')).toBe('page');
      expect(screen.getByLabelText('开始日期')).toHaveProperty('value', '2026-10-02');
      expect(screen.getByLabelText('结束日期')).toHaveProperty('value', '2026-10-04');
      expect(usageCalls(fetcher)).toHaveLength(2);
    }
    expect(screen.getByRole('region', { name: '请求数' })).toHaveProperty('textContent', '请求数4成功 3失败 1');
  });

  it('requests natural and recent periods, starting the week on Monday', async () => {
    const fetcher = setupFetch(); render(<Usage />); await loaded();
    expect(params(usageCalls(fetcher)[0]).get('from')).toBe(new Date(2026, 9, 1).toISOString());
    fireEvent.click(screen.getByRole('button', { name: '本周' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(2)); await loaded();
    expect(params(usageCalls(fetcher)[1]).get('from')).toBe(new Date(2026, 9, 12).toISOString());
    fireEvent.click(screen.getByRole('button', { name: '今日' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(3)); await loaded();
    expect(params(usageCalls(fetcher)[2]).get('from')).toBe(new Date(2026, 9, 18).toISOString());
    fireEvent.click(screen.getByRole('button', { name: '最近一段' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(4)); await loaded();
    for (const [label, days, count] of [['近24小时', 1, 4], ['近7天', 7, 5], ['近30天', 30, 6]] as const) {
      fireEvent.click(screen.getByRole('button', { name: label }));
      await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(count)); await loaded();
      const query = params(usageCalls(fetcher)[count - 1]);
      expect(Date.parse(query.get('to')!) - Date.parse(query.get('from')!)).toBe(days * 86400000);
    }
  });

  it('shows custom fields only when selected and waits for Apply before requesting dates', async () => {
    const fetcher = setupFetch(); render(<Usage />); await loaded();
    expect(screen.queryByLabelText('开始日期')).toBeNull();
    expect(screen.queryByRole('button', { name: '应用' })).toBeNull();
    editCustom('2026-10-03', '2026-10-08');
    expect(usageCalls(fetcher)).toHaveLength(1);
    expect(screen.getByText('日期待应用')).toBeTruthy();
    expect(screen.getByText('本月至今')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '应用' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(2)); await loaded();
    expect(params(usageCalls(fetcher)[1]).get('from')).toBe(new Date(2026, 9, 3).toISOString());
    expect(params(usageCalls(fetcher)[1]).get('to')).toBe(new Date(2026, 9, 8, 23, 59, 59, 999).toISOString());
    expect(screen.queryByText('日期待应用')).toBeNull();
    expect(screen.getByRole('button', { name: '应用' }).hasAttribute('disabled')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: '自然周期' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(3)); await loaded();
    expect(screen.queryByLabelText('开始日期')).toBeNull();
  });

  it('expands actual cost parts and channel model details, then switches the detail dimension', async () => {
    setupFetch(); render(<Usage />); await loaded();
    const actual = screen.getByRole('region', { name: '实际支出' });
    const costParts = within(actual).getByText('费用构成').closest('details')!;
    expect(costParts.open).toBe(false);
    fireEvent.click(within(actual).getByText('费用构成'));
    expect(costParts.open).toBe(true);
    expect(within(costParts).getByText('普通输入').nextElementSibling?.textContent).toBe('¥2.70');
    expect(within(costParts).getByText('缓存写入').nextElementSibling?.textContent).toBe('¥3.80');
    navigate('用量明细');
    expect(screen.queryByRole('cell', { name: 'alpha-public' })).toBeNull();
    const toggle = screen.getByRole('button', { name: '展开渠道 主渠道' });
    fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    const modelRow = screen.getByRole('cell', { name: 'alpha-public' }).closest('tr')!;
    expect(within(modelRow).getAllByRole('cell').slice(1, 6).map(cell => cell.textContent)).toEqual(['3', '300', '100', '150', '30']);
    fireEvent.click(toggle);
    expect(screen.queryByRole('cell', { name: 'alpha-public' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '按公开模型' }));
    expect(screen.getByRole('cell', { name: 'alpha-public' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: '展开渠道 主渠道' })).toBeNull();
    fireEvent.change(screen.getByLabelText('搜索公开模型'), { target: { value: 'beta-public' } });
    expect(screen.queryByRole('cell', { name: 'alpha-public' })).toBeNull();
    expect(screen.getByRole('cell', { name: 'beta-public' })).toBeTruthy();
  });

  it('leaves wholly unpriced cards unknown without presenting zero cost components', async () => {
    const zero = { input: 0, output: 0, cache_read: 0, cache_write: 0, unattributed: 0 };
    setupFetch(undefined, { ...summary, actual_priced_requests: 0, official_priced_requests: 0, actual_costs: { USD: 0, CNY: 0 }, official_values: { USD: 0, CNY: 0 }, actual_components: { USD: zero, CNY: zero }, official_components: { USD: zero, CNY: zero } });
    render(<Usage />); await loaded();
    for (const title of ['实际支出', '等价API']) {
      const card = screen.getByRole('region', { name: title });
      expect(card.querySelector('.usage-card-value')?.firstElementChild?.textContent).toBe('—');
      expect(within(card).getByText('暂无已计价记录')).toBeTruthy();
      expect(within(card).queryByText('费用构成')).toBeNull();
      expect(within(card).queryByText('¥0.00')).toBeNull();
    }
  });

  it('uses the selected public model for workload and input-distribution analysis', async () => {
    const fetcher = setupFetch(); render(<Usage />); await loaded(); navigate('使用分析');
    expect(screen.getByTestId('analysis-tokens').dataset.unknown).toBe('1');
    fireEvent.change(screen.getByRole('combobox', { name: '分析公开模型' }), { target: { value: 'alpha-public' } });
    expect(screen.getByTestId('analysis-tokens').dataset.input).toBe('300');
    expect(screen.getByTestId('analysis-tokens').dataset.read).toBe('150');
    expect(screen.getByTestId('analysis-tokens').dataset.unknown).toBe('0');
    expect(JSON.parse(screen.getByTestId('analysis-distribution').dataset.values!)).toEqual(buckets);
    fireEvent.change(screen.getByRole('combobox', { name: '分析公开模型' }), { target: { value: 'beta-public' } });
    expect(screen.getByTestId('analysis-tokens').dataset.input).toBe('0');
    expect(screen.getByTestId('analysis-tokens').dataset.unknown).toBe('1');
    expect(JSON.parse(screen.getByTestId('analysis-distribution').dataset.values!)).toEqual([]);
    expect(screen.getByTestId('analysis-distribution').dataset.unknown).toBe('1');
    expect(usageCalls(fetcher)).toHaveLength(1);
  });

  it('exports the same complete committed range and timezone, exposes failures, and supports retry', async () => {
    let attempts = 0;
    const fetcher = setupFetch(() => ++attempts === 1 ? response({ error: { message: '导出暂不可用' } }, 503) : new Response('time,model\n2026-10-14,alpha-public', { headers: { 'Content-Type': 'text/csv' } }));
    const createURL = vi.fn().mockReturnValue('blob:usage-download');
    const revokeURL = vi.fn();
    const NativeURL = URL;
    vi.stubGlobal('URL', class extends NativeURL { static createObjectURL = createURL; static revokeObjectURL = revokeURL; });
    const downloads: { href: string; name: string }[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function(this: HTMLAnchorElement) { downloads.push({ href: this.href, name: this.download }); });
    render(<Usage />); await loaded();
    editCustom('2026-10-02', '2026-10-04'); fireEvent.click(screen.getByRole('button', { name: '应用' }));
    await waitFor(() => expect(usageCalls(fetcher)).toHaveLength(2)); await loaded();
    const committed = params(usageCalls(fetcher)[1]);
    fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '2026-10-10' } });
    expect(screen.getByText('日期待应用')).toBeTruthy();
    navigate('导出与汇率'); fireEvent.click(screen.getByRole('button', { name: '导出 CSV' }));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', '用量导出失败，请重试。重试');
    expect(screen.getByText('导出暂不可用')).toBeTruthy();
    expect(downloads).toEqual([]);
    const first = fetcher.mock.calls.find(call => String(call[0]).startsWith('/api/usage/export?'))!;
    expect(Object.fromEntries(params(String(first[0])))).toEqual(Object.fromEntries(committed));
    expect(params(String(first[0])).has('limit')).toBe(false);
    expect(new Headers(first[1].headers).get('X-To-Router')).toBe('1');
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByText('明细已导出')).toHaveProperty('role', 'status');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(downloads).toEqual([{ href: 'blob:usage-download', name: 'torouter-usage-2026-10-02-2026-10-04.csv' }]);
    expect(createURL).toHaveBeenCalledOnce();
    const exports = fetcher.mock.calls.filter(call => String(call[0]).startsWith('/api/usage/export?'));
    expect(exports).toHaveLength(2);
    expect(Object.fromEntries(params(String(exports[1][0])))).toEqual(Object.fromEntries(committed));
    await waitFor(() => expect(revokeURL).toHaveBeenCalledWith('blob:usage-download'), { timeout: 1600 });
  });

  it('opens backfill for all history and only previews before closing', async () => {
    const fetcher = setupFetch(); render(<Usage />); await loaded();
    fireEvent.click(screen.getByRole('button', { name: '补算未计价' }));
    const dialog = await screen.findByRole('dialog', { name: '补算未计价' });
    await within(dialog).findByText('当前范围没有未计价记录');
    expect(within(dialog).getByText('调整范围').closest('details')).toHaveProperty('open', false);
    fireEvent.click(within(dialog).getByText('调整范围'));
    expect(within(dialog).getByLabelText('开始日期')).toHaveProperty('value', '');
    expect(within(dialog).getByLabelText('结束日期')).toHaveProperty('value', '');
    const backfill = fetcher.mock.calls.filter(call => String(call[0]).includes('/usage/backfill/'));
    expect(backfill).toHaveLength(1);
    expect(backfill[0][0]).toBe('/api/usage/backfill/preview');
    expect(JSON.parse(String(backfill[0][1]?.body))).toEqual({ filter: {}, amounts: ['actual', 'official'], mode: 'current' });
    fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(fetcher.mock.calls.some(call => String(call[0]).endsWith('/usage/backfill/apply'))).toBe(false);
    expect(usageCalls(fetcher)).toHaveLength(1);
  });
});

