import { validExchangeRate, type Currency, type CurrencyCosts, type ExchangeRate, type PricingAggregate, type UsageMetrics } from '../../api';

export type UsageMeasure = 'tokens' | 'actual' | 'official';
export const measureLabels: Record<UsageMeasure, string> = { tokens: 'Token', actual: '实际支出', official: '等价API' };
export const compactNumber = (value: number) => new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 2 }).format(value);
export const formatPercent = (numerator: number, denominator: number) => denominator > 0 ? `${(numerator / denominator * 100).toFixed(1)}%` : '—';
export function displayMoney(value: number, currency: Currency): string {
  if (!Number.isFinite(value)) return '—';
  const amount = Math.abs(value);
  // Keep tiny nonzero amounts visible without filling a compact card with leading zeros.
  if (amount > 0 && amount < 0.000001) return `${currency === 'CNY' ? '¥' : '$'}${Number(value.toPrecision(2))}`;
  return new Intl.NumberFormat(currency === 'CNY' ? 'zh-CN' : 'en-US', {
    style: 'currency', currency,
    ...(amount > 0 && amount < 0.01 ? { maximumSignificantDigits: 2 } : { minimumFractionDigits: 2, maximumFractionDigits: 2 }),
  }).format(value);
}
export function pricingValues(value: PricingAggregate, measure: 'actual' | 'official') {
  return measure === 'actual'
    ? { amounts: value.actual_costs, count: (value.actual_priced_requests || 0) + Math.max(0, (value.plan_charges || 0) - (value.unknown_plan_charges || 0)) }
    : { amounts: value.official_values, count: value.official_priced_requests || 0 };
}
export function unpricedRequests(value: UsageMetrics, measure: 'actual' | 'official') {
  return Math.max(0, value.requests - (measure === 'actual'
    ? (value.actual_priced_requests || 0) + (value.plan_requests || 0)
    : value.official_priced_requests || 0));
}
export function convertAmounts(amounts: CurrencyCosts | undefined, currency: Currency, rate: ExchangeRate | null): number | null {
  if (!amounts) return null;
  const other = currency === 'CNY' ? 'USD' : 'CNY';
  if (!amounts[other]) return amounts[currency] || 0;
  if (!validExchangeRate(rate)) return null;
  return (amounts[currency] || 0) + amounts[other] * (currency === 'CNY' ? rate.rate : 1 / rate.rate);
}
export function metricValue(value: UsageMetrics, measure: UsageMeasure, currency: Currency, rate: ExchangeRate | null): number | null {
  if (measure === 'tokens') return value.known_usage_requests > 0 || value.requests === 0 ? value.input_tokens + value.output_tokens : null;
  const record = pricingValues(value, measure);
  const unknownPlan = measure === 'actual' && (value.unknown_plan_charges || 0) > 0;
  return record.count > 0 || (value.requests === 0 && !unknownPlan) ? convertAmounts(record.amounts, currency, rate) : null;
}
export function formatMetric(value: number | null, measure: UsageMeasure, currency: Currency, compact = false): string {
  if (value == null || !Number.isFinite(value)) return '—';
  if (measure === 'tokens') return compact ? compactNumber(value) : new Intl.NumberFormat('zh-CN').format(value);
  if (compact) return displayMoney(value, currency);
  const normalized = Number(value.toPrecision(12));
  if (value !== 0 && Math.abs(value) < 0.000001) return `${currency === 'CNY' ? '¥' : '$'}${normalized}`;
  return new Intl.NumberFormat(currency === 'CNY' ? 'zh-CN' : 'en-US', {
    style: 'currency', currency, minimumFractionDigits: 2, maximumFractionDigits: 20,
  }).format(normalized);
}
