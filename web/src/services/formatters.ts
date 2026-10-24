import type { Channel, Currency, ExchangeRate, UsageLog } from '../types';

export function parseModels(text: string): string[] {
  return [...new Set(text.split(/[,，\n]/).map(x => x.trim()).filter(Boolean))];
}

export const number = (value: number = 0) => new Intl.NumberFormat('zh-CN').format(value);

export const money = (value: number = 0, currency: Currency = 'USD') =>
  new Intl.NumberFormat(currency === 'CNY' ? 'zh-CN' : 'en-US', {
    style: 'currency',
    currency,
    minimumFractionDigits: 2,
    maximumFractionDigits: 6,
  }).format(value);

export function validExchangeRate(value: ExchangeRate | null): value is ExchangeRate {
  return !!value?.available && Number.isFinite(value.rate) && value.rate > 0;
}

export function dateRange(from: string, to: string): URLSearchParams {
  const params = new URLSearchParams();
  if (from) params.set('from', new Date(`${from}T00:00:00`).toISOString());
  if (to) params.set('to', new Date(`${to}T23:59:59.999`).toISOString());
  return params;
}

export function localDate(date = new Date()): string {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(
    date.getDate()
  ).padStart(2, '0')}`;
}

export function isFailure(log: UsageLog) {
  return !!log.error || log.status >= 400;
}

export function channelPayload(channel: Channel): Omit<Channel, 'has_key' | 'has_oauth' | 'model_configs' | 'model_catalog_refs'> {
  // has_key and has_oauth are read-only echoes of what the server holds; the
  // save endpoint refuses unknown fields, so both are stripped here.
  const { has_key: _hasKey, has_oauth: _hasOauth, associated_prices: _prices, associated_price_models: _priceModels, model_configs: _specs, model_catalog_refs: _refs, ...payload } = channel;
  const primaryKey = channel.credentials?.[0]?.api_key ?? channel.api_key ?? '';
  return { ...payload, api_key: primaryKey };
}
