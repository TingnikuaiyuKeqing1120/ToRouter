export interface QuotaTarget { channel_id: number; credential_id: string }
export interface QuotaSnapshot extends QuotaTarget {
  channel_name: string;
  credential_name: string;
  kind: 'balance' | 'windows';
  enabled: boolean;
  balances: { currency: string; total: string }[];
  windows: { id: string; label: string; remaining_percent: number | null; resets_at?: string }[];
  checked_at?: string;
  expires_at?: string;
  next_refresh_at?: string;
  error?: string;
  stale: boolean;
}
export interface QuotaList { items: QuotaSnapshot[]; refresh_seconds: number }
