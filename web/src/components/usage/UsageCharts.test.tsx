// @vitest-environment jsdom
import { useState, type ComponentProps, type ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import type { ExchangeRate, UsageDay, UsageGroup, UsageMetrics, UsageSummary } from '../../api';
import { TokenComposition, UsageComposition, UsageInputDistribution, UsageTrend, type UsageTrendSelection } from './UsageCharts';
import type { UsageMeasure } from './metrics';

vi.mock('recharts', () => ({
  ResponsiveContainer: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  AreaChart: ({ data, children }: { data: unknown; children: ReactNode }) => <div data-testid="trend" data-values={JSON.stringify(data)}>{children}</div>,
  Area: ({ dataKey, name, stroke, type, connectNulls }: { dataKey: string; name: string; stroke: string; type: string; connectNulls: boolean }) => <span data-testid="series" data-key={dataKey} data-name={name} data-color={stroke} data-type={type} data-connect={String(connectNulls)} />,
  PieChart: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  Pie: ({ data }: { data: unknown }) => <div data-testid="pie" data-values={JSON.stringify(data)} />,
  BarChart: ({ data, children }: { data: unknown; children: ReactNode }) => <div data-testid="histogram" data-values={JSON.stringify(data)}>{children}</div>,
  Tooltip: ({ formatter }: { formatter: (value: number, name: string) => unknown }) => <span data-testid="tooltip" data-formatted={JSON.stringify(formatter(0.000000123456789, 'sample'))} />,
  Bar: () => null, Cell: () => null, CartesianGrid: () => null, XAxis: () => null, YAxis: () => null,
}));
afterEach(cleanup);
const metrics = (overrides: Partial<UsageMetrics> = {}): UsageMetrics => ({ requests: 1, failures: 0, input_tokens: 60, plain_input_tokens: 20, cache_write_tokens: 10, cache_read_tokens: 30, output_tokens: 40, known_usage_requests: 1, unknown_usage_requests: 0, actual_costs: { USD: 1, CNY: 0 }, actual_priced_requests: 1, official_values: { USD: 2, CNY: 0 }, official_priced_requests: 1, ...overrides });
const day = (date: string, overrides: Partial<UsageMetrics> = {}): UsageDay => ({ ...metrics(overrides), date });
const group = (name: string, overrides: Partial<UsageGroup> = {}): UsageGroup => ({ ...metrics(), name, daily: [day('2026-10-01')], input_distribution: [], ...overrides });
const components = { input: 0, output: 0, cache_read: 0, cache_write: 0, unattributed: 0 };
const summary = (overrides: Partial<UsageSummary> = {}): UsageSummary => ({ ...metrics(), daily: [day('2026-10-01')], by_model: [], by_channel: [], input_distribution: [], timezone: 'Asia/Shanghai', actual_components: { USD: components, CNY: components }, official_components: { USD: components, CNY: components }, ...overrides });
const rate: ExchangeRate = { available: true, base: 'USD', quote: 'CNY', rate: 7, date: '2026-10-01', attempted_on: '', fetched_at: '', stale: false, error: '', source_url: '' };
const values = (id: string) => JSON.parse(screen.getByTestId(id).getAttribute('data-values')!);
const setMeasure = (label: string) => fireEvent.click(screen.getByRole('button', { name: label }));

function TrendHarness(props: Omit<ComponentProps<typeof UsageTrend>, 'selection' | 'onSelectionChange'>) {
  const [selection, setSelection] = useState<UsageTrendSelection>({ dimension: 'total', measure: 'tokens' });
  return <UsageTrend {...props} selection={selection} onSelectionChange={setSelection} />;
}
function CompositionHarness(props: Omit<ComponentProps<typeof UsageComposition>, 'measure' | 'onMeasureChange'>) {
  const [measure, setMeasure] = useState<UsageMeasure>('tokens');
  return <UsageComposition {...props} measure={measure} onMeasureChange={setMeasure} />;
}

describe('UsageTrend', () => {
  it('shows paid monthly fees in channel trends and composition even without any requests', () => {
    const paid = metrics({ requests: 0, known_usage_requests: 0, actual_priced_requests: 0, plan_charges: 1, plan_costs: { USD: 20, CNY: 0 }, actual_costs: { USD: 20, CNY: 0 } });
    const channel = group('Monthly plan', { ...paid, channel_id: 4, daily: [day('2026-10-01', paid)] });
    const data = summary({ ...paid, daily: channel.daily, by_channel: [channel] });
    const { unmount } = render(<UsageTrend data={data} currency="USD" rate={null} selection={{ dimension: 'channel', measure: 'actual' }} onSelectionChange={vi.fn()} />);
    expect(values('trend')[0].group0).toBe(20);
    expect(screen.queryByText('所选期间暂无用量记录')).toBeNull();
    expect(screen.queryByText(/次请求未计价/)).toBeNull();
    unmount();
    render(<UsageComposition groups={[channel]} title="渠道构成" currency="USD" rate={null} measure="actual" onMeasureChange={vi.fn()} />);
    expect(values('pie')[0].value).toBe(20);
    expect(screen.queryByText('所选期间暂无用量记录')).toBeNull();
  });
  it('uses the parent selection and reports changes without resetting other fields', () => {
    const onSelectionChange = vi.fn();
    const data = summary();
    const { rerender } = render(<UsageTrend data={data} currency="USD" rate={null} selection={{ dimension: 'channel', measure: 'actual' }} onSelectionChange={onSelectionChange} />);
    expect(screen.getByRole('button', { name: '渠道' }).getAttribute('aria-pressed')).toBe('true');
    setMeasure('等价API');
    expect(onSelectionChange).toHaveBeenLastCalledWith({ dimension: 'channel', measure: 'official' });
    expect(screen.getByRole('button', { name: '实际支出' }).getAttribute('aria-pressed')).toBe('true');
    setMeasure('公开模型');
    expect(onSelectionChange).toHaveBeenLastCalledWith({ dimension: 'model', measure: 'actual' });
    rerender(<UsageTrend data={data} currency="USD" rate={null} selection={{ dimension: 'model', measure: 'official' }} onSelectionChange={onSelectionChange} />);
    expect(screen.getByRole('button', { name: '公开模型' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: '等价API' }).getAttribute('aria-pressed')).toBe('true');
  });
  it('retains precise tiny amounts in the expanded trend table', () => {
    render(<UsageTrend data={summary({ daily: [day('2026-10-01', { actual_costs: { USD: 0.000000123456789, CNY: 0 } })] })} currency="USD" rate={null} selection={{ dimension: 'total', measure: 'actual' }} onSelectionChange={vi.fn()} />);
    expect(within(screen.getByRole('table')).getByText('$1.23456789e-7')).toBeTruthy();
    expect(screen.getByText('每日实际支出（USD）')).toBeTruthy();
    expect(JSON.parse(screen.getByTestId('tooltip').getAttribute('data-formatted')!)).toEqual(['$1.23456789e-7 USD', 'sample']);
  });
  it('uses mutually exclusive token parts, fills inactive dates and preserves active unknown dates', () => {
    render(<TrendHarness data={summary({ requests: 2, unknown_usage_requests: 1, daily: [day('2026-10-01'), day('2026-10-03', { known_usage_requests: 0, unknown_usage_requests: 1 })] })} currency="USD" rate={null} />);
    expect(values('trend')).toEqual([
      { date: '2026-10-01', input: 30, read: 30, output: 40 },
      { date: '2026-10-02', input: 0, read: 0, output: 0 },
      { date: '2026-10-03', input: null, read: null, output: null },
    ]);
    expect(screen.getByText(/1 次请求用量未知/)).toBeTruthy();
    expect(screen.getByRole('table')).toBeTruthy();
    expect(screen.getAllByTestId('series').every(series => series.dataset.type === 'linear' && series.dataset.connect === 'false')).toBe(true);
  });
  it('switches to converted single-line money and distinguishes free zero from unpriced', () => {
    render(<TrendHarness data={summary({ daily: [day('2026-10-01', { actual_costs: { USD: 1, CNY: 7 } }), day('2026-10-02', { actual_costs: { USD: 0, CNY: 0 } }), day('2026-10-03', { actual_priced_requests: 0 })] })} currency="USD" rate={rate} />);
    setMeasure('实际支出');
    expect(values('trend').map((row: { amount: number | null }) => row.amount)).toEqual([2, 0, null]);
    expect(screen.getAllByTestId('series')).toHaveLength(1);
    expect(screen.getByText('$0.00')).toBeTruthy();
    setMeasure('等价API');
    expect(values('trend').map((row: { amount: number }) => row.amount)).toEqual([2, 2, 2]);
  });
  it('keeps Top5 plus other totals and identities stable when the ranking changes', () => {
    const groups = Array.from({ length: 7 }, (_, i) => group(`model.${i}[public]`, { input_tokens: (i + 1) * 100, output_tokens: 0, actual_costs: { USD: 7 - i, CNY: 0 }, daily: [day('2026-10-01', { input_tokens: (i + 1) * 100, output_tokens: 0, actual_costs: { USD: 7 - i, CNY: 0 } })] }));
    render(<TrendHarness data={summary({ by_model: groups })} currency="USD" rate={null} />);
    setMeasure('公开模型');
    const tokenRows = values('trend');
    expect(Object.values(tokenRows[0]).filter(value => typeof value === 'number').reduce((a, b) => Number(a) + Number(b), 0)).toBe(2800);
    const tokenColors = Object.fromEntries(screen.getAllByTestId('series').map(series => [series.dataset.name, series.dataset.color]));
    expect(tokenRows[0].other).toBe(300);
    expect(screen.getAllByTestId('series')[0].dataset.name).toBe('model.6[public]');
    setMeasure('实际支出');
    const moneyRows = values('trend');
    expect(Object.values(moneyRows[0]).filter(value => typeof value === 'number').reduce((a, b) => Number(a) + Number(b), 0)).toBe(28);
    expect(moneyRows[0].other).toBe(3);
    expect(screen.getAllByTestId('series')[0].dataset.name).toBe('model.0[public]');
    screen.getAllByTestId('series').forEach(series => { if (tokenColors[series.dataset.name!]) expect(series.dataset.color).toBe(tokenColors[series.dataset.name!]); });
  });
  it('does not turn unknown other-series points into zero when inactive groups are present', () => {
    const groups = Array.from({ length: 7 }, (_, i) => group(`m${i}`, i === 6 ? { known_usage_requests: 0, unknown_usage_requests: 1, daily: [day('2026-10-01', { known_usage_requests: 0, unknown_usage_requests: 1 })] } : i === 5 ? { input_tokens: 0, output_tokens: 0, daily: [] } : {}));
    render(<TrendHarness data={summary({ by_model: groups })} currency="USD" rate={null} />);
    setMeasure('公开模型');
    expect(values('trend')[0].other).toBeNull();
  });
  it('retains known usage in other when another active member is unknown', () => {
    const leading = Array.from({ length: 5 }, (_, i) => group(`top${i}`, { input_tokens: 100, output_tokens: 0, daily: [day('2026-10-01', { input_tokens: 100, output_tokens: 0 })] }));
    const known = group('known-tail', { input_tokens: 20, output_tokens: 0, daily: [day('2026-10-01', { input_tokens: 20, output_tokens: 0 })] });
    const unknown = group('unknown-tail', { known_usage_requests: 0, unknown_usage_requests: 1, daily: [day('2026-10-01', { known_usage_requests: 0, unknown_usage_requests: 1 })] });
    render(<TrendHarness data={summary({ by_model: [...leading, known, unknown] })} currency="USD" rate={null} />);
    setMeasure('公开模型');
    expect(values('trend')[0].other).toBe(20);
    expect(Object.values(values('trend')[0]).filter(value => typeof value === 'number').reduce((sum, value) => Number(sum) + Number(value), 0)).toBe(520);
  });
  it('treats an active known free zero as evidence even when other also includes unknown usage', () => {
    const leading = Array.from({ length: 5 }, (_, i) => group(`top${i}`));
    const free = group('free-tail', { actual_costs: { USD: 0, CNY: 0 }, daily: [day('2026-10-01', { actual_costs: { USD: 0, CNY: 0 } })] });
    const unknown = group('unknown-tail', { actual_priced_requests: 0, daily: [day('2026-10-01', { actual_priced_requests: 0 })] });
    render(<TrendHarness data={summary({ by_model: [...leading, free, unknown] })} currency="USD" rate={null} />);
    setMeasure('公开模型'); setMeasure('实际支出');
    expect(values('trend')[0].other).toBe(0);
  });
  it('handles empty, unknown, and zero-token periods distinctly', () => {
    const { rerender } = render(<TrendHarness data={summary({ requests: 0, daily: [] })} currency="USD" rate={null} />);
    expect(screen.getByText('所选期间暂无用量记录')).toBeTruthy();
    rerender(<TrendHarness data={summary({ known_usage_requests: 0, daily: [day('2026-10-01', { known_usage_requests: 0 })] })} currency="USD" rate={null} />);
    expect(screen.getByText('所选期间用量未知')).toBeTruthy();
    rerender(<TrendHarness data={summary({ daily: [day('2026-10-01', { input_tokens: 0, output_tokens: 0, plain_input_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0 })] })} currency="USD" rate={null} />);
    expect(values('trend')[0]).toEqual({ date: '2026-10-01', input: 0, read: 0, output: 0 });
  });
  it('blocks multi-currency money totals without a valid rate', () => {
    render(<TrendHarness data={summary({ actual_costs: { USD: 1, CNY: 7 } })} currency="USD" rate={null} />);
    setMeasure('实际支出');
    expect(screen.getByText('暂不能合计不同币种')).toBeTruthy();
    expect(screen.queryByTestId('trend')).toBeNull();
    expect(screen.queryByRole('table')).toBeNull();
  });
});

describe('UsageComposition', () => {
  it('shortens token totals while preserving exact values in titles', () => {
    render(<UsageComposition title="模型构成" groups={[group('large', { input_tokens: 112124081, output_tokens: 0 })]} currency="USD" rate={null} measure="tokens" onMeasureChange={vi.fn()} />);
    const displays = screen.getAllByText('1.12亿');
    expect(displays).toHaveLength(2);
    displays.forEach(display => expect(display.getAttribute('title')).toBe('112,124,081 Token'));
    expect(screen.getByText('Token', { selector: '.usage-chart-unit' })).toBeTruthy();
  });
  it('uses the parent measure and reports requested changes', () => {
    const onMeasureChange = vi.fn();
    render(<UsageComposition groups={[group('model')]} title="模型构成" currency="USD" rate={null} measure="actual" onMeasureChange={onMeasureChange} />);
    setMeasure('Token');
    expect(onMeasureChange).toHaveBeenCalledWith('tokens');
    expect(screen.getByRole('button', { name: '实际支出' }).getAttribute('aria-pressed')).toBe('true');
  });
  it('shows compact values with precise titles and keeps tiny costs distinct from free', () => {
    const groups = [group('tiny', { actual_costs: { USD: 0.000000123456789, CNY: 0 } }), group('free', { actual_costs: { USD: 0, CNY: 0 } }), group('unknown', { actual_priced_requests: 0 })];
    render(<UsageComposition title="模型构成" groups={groups} currency="USD" rate={null} measure="actual" onMeasureChange={vi.fn()} />);
    const list = within(screen.getByRole('list', { name: '模型构成数值' }));
    expect(list.getByText('$1.2e-7').getAttribute('title')).toBe('$1.23456789e-7 USD');
    expect(list.getByText('$0.00')).toBeTruthy();
    expect(list.getByText('未知')).toBeTruthy();
    expect(values('pie')).toHaveLength(1);
    expect(JSON.parse(screen.getByTestId('tooltip').getAttribute('data-formatted')!)).toEqual(['$1.23456789e-7 USD', 'sample']);
  });
  it('preserves Top5 plus other amount totals and excludes unknown amounts from percentages', () => {
    const groups = Array.from({ length: 8 }, (_, i) => group(`m${i}`, { actual_costs: { USD: i + 1, CNY: 0 }, actual_priced_requests: i === 7 ? 0 : 1 }));
    render(<CompositionHarness title="模型构成" groups={groups} currency="USD" rate={null} />);
    setMeasure('实际支出');
    const entries = values('pie');
    expect(entries).toHaveLength(6);
    expect(entries.reduce((sum: number, entry: { value: number }) => sum + entry.value, 0)).toBe(28);
    expect(entries.find((entry: { name: string }) => entry.name === '其他').value).toBe(3);
    expect(screen.getByText(/1 次请求未计价/)).toBeTruthy();
  });
  it('does not draw a pie for free or unknown totals and keeps readable native zeros', () => {
    const { rerender } = render(<CompositionHarness title="渠道构成" groups={[group('免费', { actual_costs: { USD: 0, CNY: 0 } })]} currency="USD" rate={null} />);
    setMeasure('实际支出');
    expect(screen.queryByTestId('pie')).toBeNull();
    expect(screen.getByText('已记录总量为 0，无比例可展示')).toBeTruthy();
    expect(within(screen.getByRole('list', { name: '渠道构成数值' })).getByText('$0.00')).toBeTruthy();
    rerender(<CompositionHarness title="渠道构成" groups={[group('未知', { actual_priced_requests: 0 })]} currency="USD" rate={null} />);
    expect(screen.getByText('尚无已计价金额')).toBeTruthy();
    expect(screen.queryByTestId('pie')).toBeNull();
  });
  it('does not add different currencies without a rate', () => {
    render(<CompositionHarness title="渠道构成" groups={[group('a'), group('b', { actual_costs: { USD: 0, CNY: 7 } })]} currency="USD" rate={null} />);
    setMeasure('实际支出');
    expect(screen.getByText('暂不能合计不同币种')).toBeTruthy();
    expect(screen.queryByTestId('pie')).toBeNull();
  });
});

describe('input analysis', () => {
  it('uses exact input ranges and counts, without including unknown requests', () => {
    const buckets = [{ label: '0–4K', from_tokens: 0, to_tokens: 4000, requests: 3 }, { label: '128K+', from_tokens: 128000, to_tokens: null, requests: 1 }];
    render(<UsageInputDistribution buckets={buckets} unknownRequests={2} />);
    expect(values('histogram')).toEqual(buckets);
    expect(screen.getByText(/有用量记录 4 次请求 · 用量未知 2 次/)).toBeTruthy();
    expect(screen.getByText('0–4,000（不含） Token')).toBeTruthy();
    expect(screen.getByText('128,000–以上 Token')).toBeTruthy();
  });
  it('renders accessible exclusive token composition and unknown coverage', () => {
    render(<TokenComposition metrics={metrics({ unknown_usage_requests: 2 })} />);
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('未命中输入（含缓存写入） 30.0%');
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('缓存命中 30.0%');
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('输出 40.0%');
    expect(screen.getByText(/2 次请求用量未知/)).toBeTruthy();
  });
});
