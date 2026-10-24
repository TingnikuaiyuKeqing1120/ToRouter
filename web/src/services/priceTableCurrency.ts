import { validExchangeRate, type Currency, type ExchangeRate, type Price, type PriceRates, type PriceTable } from '../api';

const fields = ['input', 'output', 'cache_read', 'cache_write'] as const;

export function convertTable(table: PriceTable, target: Currency, rate: ExchangeRate | null): PriceTable {
  if (table.currency === target) return structuredClone(table);
  const hasAmounts = fields.some(key => table.rates[key] != null) || table.rules.some(rule => Object.values(rule.action.rate_changes || {}).some(value => value != null));
  if (hasAmounts && !validExchangeRate(rate)) throw new Error('请先刷新汇率');
  const multiplier = validExchangeRate(rate) ? table.currency === 'USD' ? rate.rate : 1 / rate.rate : 1;
  const convert = (rates: Partial<PriceRates>) => Object.fromEntries(Object.entries(rates).map(([key, value]) => [key, value == null ? null : Number((value * multiplier).toPrecision(14))]));
  return { ...table, currency: target, rates: convert(table.rates) as PriceRates, rules: table.rules.map(rule => ({ ...rule, action: rule.action.rate_changes ? { ...rule.action, rate_changes: convert(rule.action.rate_changes) } : rule.action })) };
}

export function convertPrice(price: Price, target: Currency, rate: ExchangeRate | null): Price {
  return price.selection.mode === 'catalog' ? price : { ...price, selection: { mode: 'manual', table: convertTable(price.selection.table, target, rate) } };
}
