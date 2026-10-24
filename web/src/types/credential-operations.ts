export type CredentialOperationKind = 'warmup' | 'reset';
export type CredentialOperationState = 'running' | 'succeeded' | 'failed' | 'unknown' | 'acknowledged';
export interface CredentialOperationReceipt {
  request_id: string;
  state: CredentialOperationState;
  detail: string;
  at: string;
}
export interface CredentialResetRule {
  window_seconds: number;
  remaining_below: number | null;
  expires_within_days: number | null;
  join: 'and' | 'or';
}
export interface CredentialOperationsConfig {
  warmup: { enabled: boolean; mode: 'reset' | 'daily'; model: string; daily_times: string[] };
  auto_reset: { enabled: boolean; rules: CredentialResetRule[] };
}
export type CredentialOperationsPatch =
  | { warmup: CredentialOperationsConfig['warmup']; auto_reset?: never; warmup_model?: never }
  | { auto_reset: CredentialOperationsConfig['auto_reset']; warmup?: never; warmup_model?: never }
  | { warmup_model: string; warmup?: never; auto_reset?: never };
export interface CredentialResetRuleDraft {
  window_seconds: number;
  remaining: boolean;
  remaining_below: string;
  expires: boolean;
  expires_within_days: string;
  join: 'and' | 'or';
}
export interface CredentialAutoResetDraft {
  enabled: boolean;
  rules: CredentialResetRuleDraft[];
}
export interface CredentialResetCards {
  captured_at: string;
  available_count: number | null;
  credits: { id: string; type: string; status: string; granted_at?: number; expires_at?: number; redeemed_at?: number; available: boolean }[];
}
export interface CredentialOperationsReport {
  supported: boolean;
  channel_generation: number;
  identity: string;
  config: CredentialOperationsConfig;
  reset_cards?: CredentialResetCards;
  reset_cards_error?: string;
  last_warmup?: CredentialOperationReceipt;
  last_reset?: CredentialOperationReceipt;
  time_zone: string;
}
