import type { Currency } from './index';

export interface PlanPriceConfig {
  channel_id: number;
  default_amount: number | null;
  currency: Currency;
  first_payment_at: string;
  timezone: string;
}
export interface PlanCharge {
  channel_id: number;
  channel_name: string;
  month: string;
  amount: number | null;
  currency: Currency;
  paid_at: string;
}
export interface PlanPricesResponse { month: string; prices: PlanPriceConfig[]; charges: PlanCharge[]; scheduled: PlanCharge[] }
/** Display identity only; historical channels do not carry channel credentials/config. */
export interface PriceChannelOption {
  id: number;
  name: string;
  base_url?: string;
  endpoints?: { base_url: string }[];
  deleted?: boolean;
}
export type BillingTag = 'billing:free' | 'billing:plan' | 'billing:payg' | 'billing:unknown';
