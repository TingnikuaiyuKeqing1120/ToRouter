import { expect, it } from 'vitest';
import { codexGeneralWindows, codexHasFiveHourWindow, validateAutomaticWarmup } from './credentialAutomation';

it('requires an explicitly provided five-hour general window and does not infer it from other buckets or missing data', () => {
  expect(codexHasFiveHourWindow()).toBe(false);
  expect(codexHasFiveHourWindow({ ok: true, buckets: [{ id: 'rate_limit', general: true, primary: { window_seconds: null }, secondary: { window_seconds: 604800 } }, { id: 'extra', primary: { window_seconds: 18000 } }] })).toBe(false);
  const known = { ok: true, buckets: [{ id: 'general', general: true, primary: { window_seconds: 604800 }, secondary: { window_seconds: 18000 } }] };
  expect(codexHasFiveHourWindow(known)).toBe(true);
  expect(codexGeneralWindows(known)).toEqual([604800, 18000]);
});

it.each(['reset', 'daily'] as const)('gates %s automatic warmup by five-hour eligibility but allows an existing plan to be disabled', mode => {
  const value = { enabled: true, mode, model: 'model', daily_times: ['11:00'] };
  expect(validateAutomaticWarmup(value, ['model'], false).error).toBe('上游未明确提供 5 小时额度窗口，暂不支持自动预热。');
  expect(validateAutomaticWarmup({ ...value, enabled: false }, ['model'], false).value?.enabled).toBe(false);
});

it('normalizes explicit daily times without inventing a default time', () => {
  expect(validateAutomaticWarmup({ enabled: false, mode: 'daily', model: '', daily_times: [] }, [], false).value?.daily_times).toEqual([]);
  expect(validateAutomaticWarmup({ enabled: true, mode: 'daily', model: 'model', daily_times: [] }, ['model'], true).error).toBe('每日定时至少需要一个预热时刻。');
  expect(validateAutomaticWarmup({ enabled: true, mode: 'daily', model: 'model', daily_times: ['18:00', '07:10', '18:00'] }, ['model'], true).value?.daily_times).toEqual(['07:10', '18:00']);
});
