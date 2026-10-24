import type { CatalogItem, CatalogOffering, CatalogQuote, Currency, ExchangeRate, PriceRule, PriceTable } from '../api';
import { convertTable } from './priceTableCurrency';

export const catalogRateFields = ['input', 'output', 'cache_read', 'cache_write'] as const;
export const catalogRateLabels = ['输入', '输出', '缓存读', '缓存写'];
type QuoteEntry = { offering: CatalogOffering; quote: CatalogQuote };

export function displayCatalogTable(table: PriceTable, currency: Currency, rate: ExchangeRate | null) {
  let converted: PriceTable;
  let missingFx = false;
  try { converted = convertTable(table, currency, rate); }
  catch { converted = structuredClone(table); missingFx = true; }
  const validUnit = Number.isFinite(converted.unit_tokens) && converted.unit_tokens > 0;
  const factor = validUnit ? 1000000 / converted.unit_tokens : 1;
  const amount = (value: number | null) => value == null ? null : Number((value * factor).toPrecision(14));
  return {
    table: { ...converted, unit_tokens: validUnit ? 1000000 : converted.unit_tokens, rates: Object.fromEntries(catalogRateFields.map(field => [field, amount(converted.rates[field])])) as PriceTable['rates'], rules: converted.rules.map(rule => ({ ...rule, action: { ...rule.action, ...(rule.action.rate_changes ? { rate_changes: Object.fromEntries(Object.entries(rule.action.rate_changes).map(([field, value]) => [field, amount(value)])) } : {}) } })) },
    missingFx, validUnit,
  };
}

export function selectCatalogQuote(item: CatalogItem, currency: Currency, rate: ExchangeRate | null) {
  const entries: QuoteEntry[] = item.offerings.flatMap(offering => offering.quotes.map(quote => ({ offering, quote })));
  const original = entries.filter(entry => entry.offering.source_kind === 'original' && entry.quote.source_kind === 'original');
  if (original.length) {
    const score = ({ offering }: QuoteEntry) => (/^(standard|paid-standard)$/.test(offering.service_tier || '') ? 2 : 0) + (offering.region === 'global' ? 1 : 0);
    const entry = [...original].sort((a, b) => score(b) - score(a))[0];
    return { entry, entries, label: '标准基价' };
  }
  // Compare complete input/output base tables, never individual fields or conditional discounts.
  const candidates = entries.filter(({ offering }) => !/batch|flex|priority|promo|discount/i.test(offering.service_tier || ''));
  const comparable = candidates.flatMap(entry => {
    const display = displayCatalogTable(entry.quote.table, currency, rate);
    const { input, output } = display.table.rates;
    return display.validUnit && !display.missingFx && input != null && output != null ? [{ entry, total: input + output }] : [];
  });
  const canCompareAll = candidates.length > 1 && comparable.length === candidates.length;
  const entry = canCompareAll ? comparable.sort((a, b) => a.total - b.total)[0].entry : candidates[0] || entries[0];
  return { entry, entries, label: canCompareAll ? '候选最低基价（输入＋输出）' : '来源基价' };
}

export function catalogRuleConditions(rule: PriceRule) {
  const when = rule.when, labels: string[] = [];
  if (when.input_range) {
    const range = when.input_range;
    labels.push(`输入长度 ${range.from_tokens.toLocaleString('en-US')}–${range.to_tokens == null ? '不限' : range.to_tokens.toLocaleString('en-US')} Tokens（含边界）`);
  }
  if (when.time_window) {
    const window = when.time_window;
    labels.push(`${window.all_day ? '全天' : `${window.start}–${window.end}${window.start > window.end ? '（跨夜）' : ''}`} ${window.timezone}（左含右不含）`);
  }
  const weekdays = ['一', '二', '三', '四', '五', '六', '日'];
  if (when.weekdays?.length) labels.push(`星期${when.weekdays.map(day => weekdays[day - 1] || day).join('、')} · ${when.timezone || 'UTC'}`);
  if (when.date_set?.length) labels.push(`日期 ${when.date_set.join('、')} · ${when.timezone || 'UTC'}`);
  return labels.length ? labels.join('；') : '所有请求';
}

export function catalogAmount(value: number | null | undefined) {
  if (value == null) return '未知';
  if (value === 0) return '0';
  if (Math.abs(value) >= 0.01) {
    const rounded = Number(value.toFixed(2));
    return String(rounded);
  }
  return String(Number(value.toPrecision(4)));
}

export function catalogQuoteScope({ offering, quote }: QuoteEntry) {
  const source = ({ original: '原厂', hosted: '托管', third_party: '第三方', aggregator_summary: '目录摘要' } as Record<string, string>)[quote.source_kind] || quote.source_kind;
  const quoteName = quote.name || quote.key;
  return `${offering.provider} · ${offering.region || '地区未指定'} · ${offering.service_tier || '档位未指定'} · ${source}${quoteName === 'token' || quoteName === 'standard' ? '' : ` · ${quoteName}`}`;
}
