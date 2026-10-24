import type { CredentialOperationsConfig } from './credential-operations';

export type Protocol = 'openai' | 'anthropic' | 'gemini';

/** One API format a channel serves, with the base URL it lives at. */
export interface Endpoint {
  format: EndpointFormat;
  base_url: string;
  /** Whether this endpoint keeps server-side conversation state. */
  state?: '' | 'stateless' | 'server_state';
}

export type EndpointFormat = 'openai_chat_completions' | 'openai_responses' | 'anthropic_messages' | 'gemini_generate_content';

export const endpointFormats: EndpointFormat[] = [
  'openai_chat_completions',
  'openai_responses',
  'anthropic_messages',
  'gemini_generate_content'
];

export const endpointLabels: Record<EndpointFormat, string> = {
  openai_chat_completions: 'OpenAI Chat Completions',
  openai_responses: 'OpenAI Responses',
  anthropic_messages: 'Anthropic Messages',
  gemini_generate_content: 'Gemini generateContent',
};

export const endpointFamilies: Record<EndpointFormat, string> = {
  openai_chat_completions: 'OpenAI',
  openai_responses: 'OpenAI',
  anthropic_messages: 'Anthropic',
  gemini_generate_content: 'Gemini',
};

/** A usage row records the formats its request actually used. Rows written
 * before formats had full names hold the wire family instead, and "openai" once
 * covered both OpenAI facades — those are labelled as legacy rather than
 * guessed back into one of them. */
const usageFormatLabels: Record<string, string> = {
  openai_chat_completions: 'OpenAI Chat',
  openai_responses: 'OpenAI Responses',
  anthropic_messages: 'Anthropic Messages',
  gemini_generate_content: 'Gemini',
  openai: 'OpenAI（旧）',
  anthropic: 'Anthropic（旧）',
  gemini: 'Gemini（旧）',
};

export function usageFormatLabel(format: string): string {
  return usageFormatLabels[format] || format;
}

export type StaticCredentialType = 'api_key' | 'copilot_oauth' | 'chatgpt_oauth' | 'codex_oauth' | 'claude_oauth' | 'antigravity_oauth' | 'devin_oauth' | 'zen_free' | 'plugin:qoder' | 'plugin:qoder-cn' | 'plugin:workbuddy' | 'plugin:workbuddy-ai' | 'plugin:minimax-code' | 'plugin:minimax-code-global';

export type MarketCredentialType = `market:${string}`;
export type CredentialType = StaticCredentialType | MarketCredentialType;

export const credentialTypes: CredentialType[] = ['api_key', 'copilot_oauth', 'chatgpt_oauth', 'codex_oauth', 'claude_oauth', 'antigravity_oauth', 'devin_oauth', 'zen_free', 'plugin:qoder', 'plugin:qoder-cn', 'plugin:workbuddy', 'plugin:workbuddy-ai', 'plugin:minimax-code', 'plugin:minimax-code-global'];

export const credentialLabels: Record<CredentialType, string> = {
  api_key: 'API 密钥',
  copilot_oauth: 'GitHub Copilot（设备码授权）',
  chatgpt_oauth: 'ChatGPT 套餐（官方 SIWC 授权）',
  codex_oauth: 'Codex 后端（社区授权）',
  claude_oauth: 'Claude 订阅',
  antigravity_oauth: 'Antigravity',
  devin_oauth: 'Devin',
  zen_free: 'OpenCode Zen Free（公共访问）',
  'plugin:qoder': 'Qoder（国际版）',
  'plugin:qoder-cn': 'Qoder CN（中国版）',
  'plugin:workbuddy': 'WorkBuddy',
  'plugin:workbuddy-ai': 'WorkBuddy AI（国际版）',
  'plugin:minimax-code': 'MiniMax Code（中国版）',
  'plugin:minimax-code-global': 'MiniMax Code（国际版）',
};

/** Copy that explains each track's provenance, because two of the three are not
 * OpenAI-official and the operator should see that before authorizing. */
export const credentialNotes: Record<CredentialType, string> = {
  api_key: '传统上游 API 密钥，适用于所有自定义上游。',
  copilot_oauth: 'GitHub 设备码授权换取 Copilot 令牌；走 GitHub 无文档接口，额度消耗快于官方客户端。',
  chatgpt_oauth: 'OpenAI 官方 Sign in with ChatGPT；仅 Responses 端点、无服务端状态、部分参数被剥离。',
  codex_oauth: '复用 Codex CLI 的公共客户端身份，非 OpenAI 官方支持路径；后端能力面未验证，请求按原样透传。',
  claude_oauth: '独立授权 Claude 订阅，使用固定原生请求身份；支持范围以实际账号为准。',
  antigravity_oauth: '独立 Google 授权并关联项目；模型权限取决于账号资格。',
  devin_oauth: '独立授权 Devin 订阅。支持文本和函数工具；会话失效时重新授权。',
  zen_free: '使用公共凭证，无需登录。模型目录与免费范围由上游提供。',
  'plugin:qoder': '使用兼容插件独立授权 Qoder 国际版；不读取原工具登录。支持范围以实际账号为准。',
  'plugin:qoder-cn': '使用兼容插件独立授权 Qoder 中国版；不读取原工具登录，不支持企业 VPC。',
  'plugin:workbuddy': '使用兼容插件独立授权 WorkBuddy；不读取原工具登录。',
  'plugin:workbuddy-ai': '使用兼容插件独立授权 WorkBuddy AI 国际版；不读取原工具登录。',
  'plugin:minimax-code': '使用兼容插件独立授权 MiniMax Code 中国版；不读取原工具登录。',
  'plugin:minimax-code-global': '使用兼容插件独立授权 MiniMax Code 国际版；不读取原工具登录。',
};

export interface CodexQuotaWindow {
  used_percent?: number | null;
  window_seconds?: number | null;
  reset_at?: number | null;
}

export interface CodexQuotaBucket {
  id: string;
  general?: boolean;
  name?: string;
  allowed?: boolean | null;
  limit_reached?: boolean | null;
  primary?: CodexQuotaWindow | null;
  secondary?: CodexQuotaWindow | null;
}

export interface CodexQuotaResponse {
  ok: boolean;
  cached?: boolean;
  stale?: boolean;
  refresh_due?: boolean;
  refresh_seconds?: number;
  next_refresh_at?: string;
  last_attempt_at?: string;
  captured_at?: string;
  plan_type?: string;
  reset_credit_count?: number | null;
  buckets?: CodexQuotaBucket[];
  detail?: string;
}

export interface OAuthInfo {
  provider: string;
  account_label?: string;
  api_base?: string;
  status?: 'active' | 'unsubscribed' | 'reauth_required';
  status_reason?: string;
  expires_at: string;
  updated_at: string;
}

/** The console-facing shape of an authorization flow: what start returns and
 * what status polls until it ends. */
export type OAuthStart =
  | { flow: string; method: 'browser'; authorization_url: string; redirect_uri?: string; callback_path?: string; paste_hint?: string; instructions?: string; non_official?: string }
  | { flow: string; method: 'device'; user_code: string; verification_uri: string; expires: string; interval: number };

export type OAuthStatus =
  | { status: 'pending'; expires?: string; interval?: number }
  | { status: 'complete'; channel_id: number; already_linked?: boolean; message?: string; degraded?: string[] }
  | { status: 'cancelled'; message?: string }
  | { status: 'error'; code: string; message: string; existing_channel_id?: number; existing_credential_id?: string };

/** The console route each credential type authorizes through. */
export const oauthRoutes: Record<Exclude<CredentialType, 'api_key'>, string> = {
  copilot_oauth: '/oauth/copilot',
  chatgpt_oauth: '/oauth/chatgpt',
  codex_oauth: '/oauth/codex',
  claude_oauth: '/oauth/claude',
  antigravity_oauth: '/oauth/antigravity',
  devin_oauth: '/oauth/devin',
  zen_free: '', // Public access has no OAuth authorization route.
  'plugin:qoder': '/oauth/qoder',
  'plugin:qoder-cn': '/oauth/qoder-cn',
  'plugin:workbuddy': '/oauth/workbuddy',
  'plugin:workbuddy-ai': '/oauth/workbuddy-ai',
  'plugin:minimax-code': '/oauth/minimax-code',
  'plugin:minimax-code-global': '/oauth/minimax-code-global',
};

/** "Adding an account" and "re-authorizing one" are different flows: an
 * oaiapp_* registration is bound to one user and workspace, so a new account
 * registers a new client while a re-authorization reuses the channel's. */
export type OAuthOperation = 'reauth' | 'add_credential';

export const oauthStatusLabels: Record<string, string> = {
  active: '凭证有效',
  unsubscribed: '无订阅',
  reauth_required: '需重新授权',
};

export const oauthErrorHints: Record<string, string> = {
  consent_missing_scope: '授权时未同意 ChatGPT 额度使用权限；请重新授权并在确认页同意全部权限。',
  access_denied: '已在授权页面拒绝授权。',
  expired_token: '授权码已过期，请重新开始。',
  state_mismatch: '回调链接与本次授权不匹配，已拒绝；请重新授权。',
  client_id_mismatch: '回调返回的应用注册与本次不一致，已拒绝。',
  unsubscribed: '该账号没有生效的 Copilot 订阅。',
  deviceauth_disabled: '该服务器未启用设备码登录，请改用浏览器授权。',
};

export interface ChannelDiagnostic {
  source: string;
  code: string;
  message: string;
  suggestion: string;
  upstream_status?: number;
  raw?: string;
  truncated?: boolean;
}

export interface FormatResult {
  format: EndpointFormat;
  credential_id?: string;
  credential_label?: string;
  ok: boolean;
  models?: { id: string; display_name?: string }[];
  duration_ms: number;
  upstream_status?: number;
  diagnostic?: ChannelDiagnostic;
}

export type DiscoverResult = { duration_ms: number; catalogue?: string[] } & (
  { ok: true; models: { id: string; display_name?: string }[] } |
  { ok: false; diagnostic: ChannelDiagnostic }
) & { formats?: FormatResult[] };

export interface ProbeResult {
  ok: boolean;
  model: string;
  duration_ms: number;
  upstream_status?: number;
  diagnostic?: ChannelDiagnostic;
  formats?: FormatResult[];
}

export function discoverFormats(result: DiscoverResult): (Omit<FormatResult, 'format'> & { format?: EndpointFormat })[] {
  if (result.formats) return result.formats;
  return result.ok
    ? [{ ok: true, models: result.models, duration_ms: result.duration_ms }]
    : [{ ok: false, models: [], duration_ms: result.duration_ms, diagnostic: result.diagnostic }];
}

export type Currency = 'USD' | 'CNY';
export type CurrencyCosts = Record<Currency, number>;

export interface ExchangeRate {
  available: boolean;
  base: 'USD';
  quote: 'CNY';
  rate: number;
  date: string;
  attempted_on: string;
  fetched_at: string;
  stale: boolean;
  error: string;
  source_url: string;
  source_id?: string;
  source_name?: string;
  checked_at?: string;
}

export interface ExchangeSourcesConfig {
  sources: string[];
  available: { id: string; name: string; url: string }[];
}

export type PriceRates = {
  input: number | null;
  output: number | null;
  cache_read: number | null;
  cache_write: number | null;
};

export interface PriceInputRange { from_tokens: number; to_tokens: number | null }
export interface PriceTimeWindow { start: string; end: string; all_day: boolean; timezone: string }
export interface PriceRuleWhen { input_range?: PriceInputRange; time_window?: PriceTimeWindow; weekdays?: number[]; date_set?: string[]; timezone?: string }
export interface PriceRuleAction { rate_changes?: Partial<PriceRates>; multiplier?: number | null }
export interface PriceRule { draft?: unknown; id?: string; enabled: boolean; when: PriceRuleWhen; action: PriceRuleAction }
export interface PriceTable { currency: Currency; unit_tokens: number; rates: PriceRates; rules: PriceRule[]; uncovered_conditions?: string[] }
export interface CatalogModelRef { source: string; id: string; offering_key?: string }
export interface CatalogPriceRef extends CatalogModelRef { offering_key: string; quote_key: string }
export type PriceSelection = { mode: 'manual'; table: PriceTable; ref?: never } | { mode: 'catalog'; ref: CatalogPriceRef; table?: never };
export interface Price { channel_id: number; model: string; display_name: string; origin: 'auto' | 'manual'; selection: PriceSelection }
export interface PriceResolution { source_kind?: string; source_url?: string; verified_at?: string; status: 'resolved' | 'unconfigured' | 'missing'; missing_reason?: string; layer: string; channel_id: number; model: string; table?: PriceTable; ref?: CatalogPriceRef; fetched_at?: string }
export interface UsagePricingBasis { method: string; calculated_at: string; resolution: PriceResolution; rates: PriceRates; currency: Currency; unit_tokens: number; matched_rules: string[]; missing_reason?: string; note?: string }
export interface ThinkingDefault { mode: string; effort?: string; budget?: number; enabled?: boolean }
export interface ModelDefaults { max_output_tokens?: number; temperature?: number; thinking?: ThinkingDefault }
export interface NativeThinkingMode { id: string; api_format: EndpointFormat; control: 'effort' | 'budget' | 'enabled' | 'adaptive'; output_token_field?: string; efforts?: string[] | null; budget_min?: number; budget_max?: number; default_effort?: string; disabled_efforts?: string[] | null }
export interface NativeThinking { supported: boolean | null; mandatory?: boolean | null; modes: NativeThinkingMode[] }
export interface ModelConfig {
  max_output_tokens?: number;
  context_window?: number;
  max_input_tokens?: number;
  output_limit?: number;
  streaming?: boolean;
  input_modalities?: string[];
  output_modalities?: string[];
  thinking_efforts?: string[];
  native_thinking?: NativeThinking;
  output_token_field?: string;
}

export interface Binding {
  channel_id: number;
  model: string;
  priority: number;
  enabled: boolean;
}

export interface DownstreamModel {
  associated_prices?: Price[];
  id: string;
  display_name?: string;
  enabled: boolean;
  fallback: boolean;
  catalog_model_ref?: CatalogModelRef;
  bindings: Binding[];
  sort_order?: number;
  tags?: string[];
  effective_tags?: string[];
  effective_tag_details?: EffectiveTagDetail[];
}

export type TagGroupType = 'builtin' | 'user';

export interface TagGroup {
  id: string;
  name: string;
  type: TagGroupType;
  description: string;
  sort_order: number;
}

export interface Tag {
  id: string;
  group_id: string;
  name: string;
  color: string;
  icon?: string;
  description: string;
  sort_order: number;
}

export interface TagGroupWithTags extends TagGroup {
  tags: Tag[];
}

export interface TagReferences {
  tag_id: string;
  channel_names: string[];
  downstream_models: string[];
  client_key_names: string[];
}

export interface EffectiveTagDetail {
  tag_id: string;
  sources: string[];
}

export interface EffectiveBinding extends Binding {
  channel_exists: boolean;
  channel_name?: string;
  protocol?: Protocol;
  effective?: { config: ModelConfig; sources: Record<string, string> };
}

export interface DownstreamModelView extends DownstreamModel {
  bindings: EffectiveBinding[];
}

export const numericConfigFields: { key: keyof ModelConfig; label: string }[] = [
  { key: 'max_output_tokens', label: '最大输出 tokens' },
  { key: 'context_window', label: '上下文总窗口' },
  { key: 'max_input_tokens', label: '最大输入 tokens' },
  { key: 'output_limit', label: '输出上限' },
];

export const inputModalityOptions = ['text', 'image', 'audio', 'video', 'file'];
export const outputModalityOptions = ['text', 'image', 'audio', 'video'];

const allConfigLabels: Record<string, string> = {
  ...Object.fromEntries(numericConfigFields.map(f => [f.key as string, f.label])),
  context_length: '上下文总窗口',
  output_limit: '原生输出限制资料',
  streaming: '流式支持',
  thinking_supported: '思考支持',
  thinking_mandatory: '思考是否必需',
  thinking_modes: '原生思考映射资料',
  thinking_options: '支持的思考选项',
  input_modalities: '输入模态',
  output_modalities: '输出模态',
  thinking_effort: '默认思考档位',
  thinking_efforts: '支持的思考档位',
};

export function configLabel(field: string): string {
  return allConfigLabels[field] || field;
}

export type DispatchMode = 'failover' | 'round_robin';
export type ChainType = 'unidirectional' | 'cyclic';

export interface CredentialItem {
  operations?: CredentialOperationsConfig;
  id: string;
  label?: string;
  api_key?: string;
  has_key?: boolean;
  masked_key?: string;
  has_oauth?: boolean;
  oauth?: OAuthInfo;
  oauth_info?: OAuthInfo;
  enabled: boolean;
  source_channel_id?: number;
}

export interface Channel {
  generation?: number;
  associated_prices?: Price[];
  associated_price_models?: string[];
  id: number;
  name: string;
  api_key: string;
  has_key: boolean;
  has_oauth?: boolean;
  models: string[];
  available_models?: string[];
  manual_models?: string[];
  enabled: boolean;
  endpoints: Endpoint[];
  base_url?: string;
  preferred_endpoint?: EndpointFormat;
  dispatch_mode?: DispatchMode;
  chain_type?: ChainType;
  credentials?: CredentialItem[];
  model_configs?: Record<string, ModelConfig>;
  model_catalog_refs?: Record<string, CatalogModelRef>;
  model_evidence?: Record<string, { source: string; verified_at?: string; api_format: string }>;
  credential_type?: CredentialType;
  model_formats?: Record<string, EndpointFormat[]> | null;
  oauth?: OAuthInfo;
  tags?: string[];
  model_tags?: Record<string, string[]>;
}

export interface Settings {
  default_launch_mode?: 'web' | 'desktop' | 'cli';
  gateway_key: string;
  prices: Price[];
  retention_days: number;
  max_attempts: number;
  retries_per_channel: number;
  failure_cooldown_seconds: number;
  request_defaults_enabled: boolean;
  request_defaults: ModelDefaults;
  oauth_auto_open?: boolean;
  quota_refresh_seconds?: number;
  reasoning_policy?: 'preserve' | 'availability';
}

export interface ClientKey {
  id: number;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at: string | null;
  scope: ClientKeyScope;
}

export interface ClientKeyScope {
  model_mode: 'all' | 'selected';
  model_ids: string[];
  tag_rules: Record<string, string[]>;
}

export interface ClientKeyPreview {
  models: { model_id: string; allowed: boolean; reason: string; bindings: { channel_id: number; channel_name: string; model_id: string; allowed: boolean; reasons: string[] }[] }[];
  allowed_models: number;
  total_models: number;
}

export interface CreatedClientKey {
  client_key: ClientKey;
  key: string;
}

export interface AttemptRecord {
  attempt_index: number;
  channel_id: number;
  channel_name: string;
  credential_id?: string;
  credential_mask_key?: string;
  upstream_model: string;
  status_code: number;
  duration_ms: number;
  error_reason?: string;
  action_taken?: string;
}

export interface RequestDiagnostic {
  code: string;
  source: string;
  phase: string;
  message: string;
  facts?: string[];
  possible_causes?: string[];
  steps?: { action: string; expected?: string }[];
  actions_taken?: string[];
  settings_path?: string;
  request_id?: string;
}

export interface UsageLog {
  not_sent?: boolean;
  request_id?: string;
  id: number;
  time: string;
  channel_id: number;
  channel_name: string;
  client_key_name?: string;
  model: string;
  upstream_model: string;
  inbound: string;
  outbound: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  estimated: boolean;
  currency?: Currency | '';
  official_value?: number | null;
  official_currency?: Currency | '';
  actual_cost?: number | null;
  actual_pricing?: UsagePricingBasis;
  official_pricing?: UsagePricingBasis;
  duration_ms: number;
  ttft_ms: number;
  status: number;
  error: string;
  stateful?: boolean;
  degraded?: string;
  attempts?: AttemptRecord[];
  diagnostic?: RequestDiagnostic;
}

export interface PricingAggregate {
  official_values?: CurrencyCosts;
  actual_costs?: CurrencyCosts;
  official_priced_requests?: number;
  actual_priced_requests?: number;
  plan_requests?: number;
  plan_costs?: CurrencyCosts;
  plan_charges?: number;
  unknown_plan_charges?: number;
}

export interface UsageMetrics extends PricingAggregate {
  requests: number;
  input_tokens: number;
  output_tokens: number;
  plain_input_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  known_usage_requests: number;
  unknown_usage_requests: number;
  failures: number;
}
export interface UsageDay extends UsageMetrics { date: string }
export interface UsageInputBucket { label: string; from_tokens: number; to_tokens: number | null; requests: number }
export interface UsageGroup extends UsageMetrics {
  name: string;
  channel_id?: number;
  daily: UsageDay[];
  input_distribution: UsageInputBucket[];
  models?: UsageGroup[];
}
export interface UsageCostComponents { input: number; output: number; cache_read: number; cache_write: number; unattributed: number; plan?: number }
export interface UsageSummary extends UsageMetrics {
  unconfigured_plan_channels?: number;
  daily: UsageDay[];
  by_channel: UsageGroup[];
  by_model: UsageGroup[];
  input_distribution: UsageInputBucket[];
  timezone: string;
  actual_components: Record<Currency, UsageCostComponents>;
  official_components: Record<Currency, UsageCostComponents>;
}

export interface CatalogModalities {
  input: string[];
  output: string[];
}

export type CatalogThinking = NativeThinking;
export interface CatalogQuote { key: string; name?: string; table: PriceTable; source_kind: 'original' | 'hosted' | 'third_party' | string; source_url?: string; verified_at?: string; fetched_at?: string; notes?: string[] }
export interface CatalogOffering { key: string; provider: string; region?: string; service_tier?: string; source_kind: string; quotes: CatalogQuote[] }
export interface CatalogItem {
  id: string; name: string; provider: string; source: string;
  original_id?: string; aliases?: string[];
  context_length: number | null; max_input_tokens: number | null; max_output_tokens: number | null;
  api_formats?: EndpointFormat[]; output_token_field?: string;
  modalities: CatalogModalities; thinking: CatalogThinking; defaults?: ModelDefaults;
  offerings: CatalogOffering[]; doc_url?: string; pricing_url?: string; verified_at?: string;
  fetched_at?: string; needs_refresh?: boolean; notes?: string[];
}
export interface CatalogSourceStatus { source: string; name: string; enabled: boolean; prefill_enabled: boolean; priority: number; item_count: number; last_fetched_at?: string | null; last_error?: string }

export interface UpstreamModelSpecs { channel_id: number; channel_name: string; model: string; specs: ModelConfig; catalog_model_ref?: CatalogModelRef }
export interface PublicModelSpecs {
  id: string; name: string; context_length: number | null; max_input_tokens: number | null; max_output_tokens: number | null; output_limit: number | null; streaming: boolean | null;
  modalities: { input: string[] | null; output: string[] | null };
  thinking: { supported: boolean | null; mandatory: boolean | null; modes: NativeThinkingMode[] | null; options: string[] | null };
  differences?: { field: string; bindings: { name: string; model: string; known: boolean; value: unknown }[] }[];
  warnings?: string[]; recommendation?: string;
}

export interface RuntimeModelStatus {
  model: string;
  name: string;
  upstream: string;
  red: boolean;
  reason: string;
  failures: number;
  cooldown_until?: string;
  last_request?: { at: string; status: number; duration_ms: number; error?: string };
}
export interface RuntimeCredentialStatus {
  channel_id: number;
  channel_name: string;
  credential_id: string;
  credential_name: string;
  red: boolean;
  reason: string;
  failures: number;
  cooldown_until?: string;
  quota_supported: boolean;
  models: RuntimeModelStatus[];
}
export interface RuntimeStatusSnapshot {
  rows: RuntimeCredentialStatus[];
  problem_count: number;
}
