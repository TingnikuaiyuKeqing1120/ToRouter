import type { CodexQuotaResponse } from '../api';
import type { CredentialAutoResetDraft, CredentialOperationsConfig, CredentialResetRule } from '../types/credential-operations';

export function codexGeneralWindows(quota?: CodexQuotaResponse): number[] {
  const general = quota?.buckets?.find(bucket => bucket.general === true);
  return [...new Set([general?.primary?.window_seconds, general?.secondary?.window_seconds].filter((value): value is number => value != null && Number.isFinite(value) && value > 0))].sort((a, b) => b - a);
}
export function codexHasFiveHourWindow(quota?: CodexQuotaResponse): boolean { return codexGeneralWindows(quota).includes(18000); }
export function createAutoResetDraft(value: CredentialOperationsConfig['auto_reset']): CredentialAutoResetDraft {
  return { enabled: value.enabled, rules: value.rules.map(rule => ({ ...rule, remaining: rule.remaining_below != null, remaining_below: rule.remaining_below == null ? '' : String(rule.remaining_below), expires: rule.expires_within_days != null, expires_within_days: rule.expires_within_days == null ? '' : String(rule.expires_within_days) })) };
}
export function validateAutomaticWarmup(value: CredentialOperationsConfig['warmup'], models: string[], hasFiveHourWindow: boolean): { value?: CredentialOperationsConfig['warmup']; error?: string } {
  if (value.enabled && !hasFiveHourWindow) return { error: '上游未明确提供 5 小时额度窗口，暂不支持自动预热。' };
  if (value.enabled && !value.model) return { error: '请选择一个已启用的预热模型。' };
  if (value.model && !models.includes(value.model)) return { error: '原预热模型已停用，请重新选择，或清空模型。' };
  if (value.daily_times.some(time => !/^([01]\d|2[0-3]):[0-5]\d$/.test(time))) return { error: '请填写有效的每日预热时刻，或删除空时刻。' };
  if (value.enabled && value.mode === 'daily' && !value.daily_times.length) return { error: '每日定时至少需要一个预热时刻。' };
  return { value: { ...value, daily_times: [...new Set(value.daily_times)].sort() } };
}
export function validateAutomaticReset(draft: CredentialAutoResetDraft): { value?: CredentialOperationsConfig['auto_reset']; error?: string } {
  if (draft.enabled && !draft.rules.length) return { error: '自动重置至少需要一组规则。' };
  const rules: CredentialResetRule[] = [];
  for (let index = 0; index < draft.rules.length; index++) {
    const rule = draft.rules[index];
    if (!rule.remaining && !rule.expires) return { error: `第 ${index + 1} 组至少选择一个条件。` };
    const remaining = rule.remaining ? Number(rule.remaining_below) : null, expires = rule.expires ? Number(rule.expires_within_days) : null;
    if (remaining != null && (!rule.remaining_below.trim() || !Number.isFinite(remaining) || remaining < 0 || remaining > 100 || rule.window_seconds <= 0)) return { error: `第 ${index + 1} 组请选择额度窗口，并填写 0–100% 的剩余比例。` };
    if (expires != null && (!rule.expires_within_days.trim() || !Number.isFinite(expires) || expires < 0 || expires > 3650)) return { error: `第 ${index + 1} 组请填写 0–3650 天的重置卡到期条件。` };
    rules.push({ window_seconds: rule.window_seconds, remaining_below: remaining, expires_within_days: expires, join: rule.join });
  }
  return { value: { enabled: draft.enabled, rules } };
}
