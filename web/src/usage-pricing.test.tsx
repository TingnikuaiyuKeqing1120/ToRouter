// @vitest-environment jsdom
import { ConfirmProvider } from './components/Confirm';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render as renderUI, screen, within } from '@testing-library/react';
import type { UsageMetrics, UsageSummary } from './api';
import Usage from './pages/Usage';
import Logs from './pages/Logs';
vi.mock('recharts', () => ({ ResponsiveContainer: ({ children }: { children: React.ReactNode }) => <div>{children}</div>, AreaChart: ({ data, children }: { data: unknown; children: React.ReactNode }) => <svg data-testid="chart-data" data-values={JSON.stringify(data)}>{children}</svg>, Area: () => null, PieChart: ({ children }: { children: React.ReactNode }) => <div>{children}</div>, Pie: () => null, Cell: () => null, BarChart: ({ children }: { children: React.ReactNode }) => <div>{children}</div>, Bar: () => null, CartesianGrid: () => null, Tooltip: () => null, XAxis: () => null, YAxis: () => null }));
const response = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'Content-Type': 'application/json' } });
const pricingMetrics: UsageMetrics = { requests: 2, failures: 0, input_tokens: 10, plain_input_tokens: 10, cache_read_tokens: 0, cache_write_tokens: 0, output_tokens: 20, known_usage_requests: 1, unknown_usage_requests: 1, official_values: { USD: 0, CNY: 0 }, actual_costs: { USD: 0, CNY: 7 }, official_priced_requests: 1, actual_priced_requests: 1 };
function usageSummary(): UsageSummary {
  const zero = { input: 0, output: 0, cache_read: 0, cache_write: 0, unattributed: 0 };
  return { ...pricingMetrics, daily: [{ ...pricingMetrics, date: '2026-10-05', requests: 1, unknown_usage_requests: 0 }, { ...pricingMetrics, date: '2026-10-06', requests: 1, input_tokens: 0, plain_input_tokens: 0, output_tokens: 0, known_usage_requests: 0, unknown_usage_requests: 1, official_values: { USD: 0, CNY: 0 }, actual_costs: { USD: 0, CNY: 0 }, official_priced_requests: 0, actual_priced_requests: 0 }], by_channel: [], by_model: [], input_distribution: [{ label: '0–4K', from_tokens: 0, to_tokens: 4000, requests: 1 }], timezone: Intl.DateTimeFormat().resolvedOptions().timeZone, actual_components: { USD: { ...zero }, CNY: { ...zero, unattributed: 7 } }, official_components: { USD: { ...zero }, CNY: { ...zero } } };
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
describe('current pricing consumers', () => {
  it('shows compact CNY and USD amounts while retaining precise native and tiny amounts', async () => {
    const data = { ...usageSummary(), actual_costs: { CNY: 32.67858, USD: 0 }, official_values: { CNY: 0, USD: 0.000000123456789 } };
    const rate = { available: true, base: 'USD', quote: 'CNY', rate: 7, date: '2026-10-17', stale: false };
    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) => Promise.resolve(response(url.startsWith('/api/usage?') ? data : url === '/api/channels' ? [] : rate))));
    render(<Usage />);
    const actual = await screen.findByRole('region', { name: '实际支出' });
    expect(within(actual).getByText('¥32.68').getAttribute('title')).toContain('¥32.67858 · 原币 ¥32.67858 CNY');
    const official = screen.getByRole('region', { name: '等价API' });
    expect(within(official).getByText('≈ ¥8.6e-7')).toBeTruthy();
    expect(within(official).getByText('原币 $1.2e-7 USD').getAttribute('title')).toBe('$1.23456789e-7 USD');
    fireEvent.change(screen.getByLabelText('显示币种'), { target: { value: 'USD' } });
    expect(within(actual).getByText('≈ $4.67').getAttribute('title')).toContain('原币 ¥32.67858 CNY');
    expect(within(actual).getByText('原币 ¥32.68 CNY').getAttribute('title')).toBe('¥32.67858 CNY');
    expect(within(official).getByText('$1.2e-7').getAttribute('title')).toContain('$1.23456789e-7');
    expect(within(official).queryByText('$0.00', { selector: '.usage-card-value > span' })).toBeNull();
    expect(within(actual).getByText('按量已计价 1 / 2')).toBeTruthy();
  });
  it('shows independent native amounts, leaves unavailable values blank for a complete period', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) => Promise.resolve(response(url.startsWith('/api/usage?') ? usageSummary() : url === '/api/channels' ? [] : { available: false, rate: 0 }))));
    render(<Usage />); const official = await screen.findByRole('region', { name: '等价API' });
    expect(within(official).getByText('¥0.00', { selector: '.usage-card-value > span' }).getAttribute('title')).toContain('原币 ¥0.00 CNY');
    expect(within(screen.getByRole('region', { name: '实际支出' })).getByText('¥7.00', { selector: '.usage-card-value > span' }).getAttribute('title')).toContain('原币 ¥7.00 CNY');
    expect(screen.queryByRole('alert')).toBeNull();
    fireEvent.click(within(screen.getByRole('region', { name: '用量趋势' })).getByRole('button', { name: '实际支出' }));
    expect(JSON.parse(screen.getByTestId('chart-data').getAttribute('data-values')!).map((day: { amount: unknown }) => day.amount)).toEqual([7, null]);
    fireEvent.click(within(screen.getByRole('region', { name: '用量趋势' })).getByRole('button', { name: '等价API' }));
    expect(JSON.parse(screen.getByTestId('chart-data').getAttribute('data-values')!).map((day: { amount: unknown }) => day.amount)).toEqual([0, null]);
    const navigation = screen.getByRole('navigation', { name: '用量页导航' });
    fireEvent.click(within(navigation).getByRole('button', { name: '导出与汇率' }));
    fireEvent.change(screen.getByRole('combobox', { name: '显示币种' }), { target: { value: 'USD' } });
    fireEvent.click(within(navigation).getByRole('button', { name: '用量总览' }));
    expect(within(screen.getByRole('region', { name: '等价API' })).getByText('$0.00', { selector: '.usage-card-value > span' }).getAttribute('title')).toContain('原币 $0.00 USD');
    const actual = screen.getByRole('region', { name: '实际支出' });
    expect(within(actual).getByText('原币 ¥7.00 CNY · 待汇率换算')).toBeTruthy();
    expect(actual.querySelector('.usage-card-value')?.firstElementChild?.textContent).toBe('—');
    expect(screen.queryByText(/旧版/)).toBeNull();
  });
  it('renders only current official/actual amounts and distinguishes null from zero', async () => {
    const base = { time: '2026-10-05T00:00:00Z', channel_id: 7, model: 'public', upstream_model: 'up', inbound: 'openai_responses', outbound: 'openai_responses', input_tokens: 10, output_tokens: 20, status: 200, error: '', duration_ms: 50, ttft_ms: 10 };
    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) => Promise.resolve(response(url.startsWith('/api/logs?') ? [{ ...base, id: 1, official_value: 0, official_currency: 'USD', actual_cost: null, currency: '' }, { ...base, id: 2, official_value: 3, official_currency: 'USD', actual_cost: 7, currency: 'CNY' }] : []))));
    render(<Logs />); await screen.findByRole('button', { name: '查看请求 1 详情' });
    const rows = screen.getAllByRole('row'); expect(within(rows[1]).getAllByRole('cell').slice(3, 5).map(cell => cell.textContent)).toEqual(['$0.00 USD', '—']);
    expect(within(rows[2]).getAllByRole('cell').slice(3, 5).map(cell => cell.textContent)).toEqual(['$3.00 USD', '¥7.00 CNY']); expect(screen.queryByRole('columnheader', { name: '旧版费用' })).toBeNull();
  });
});
