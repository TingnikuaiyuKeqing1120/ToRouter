import { localDate } from '../../api';

export type UsagePeriod = 'day' | 'week' | 'month';
export type UsageTimeMode = 'calendar' | 'recent' | 'custom' | 'all';
export interface UsageRange { from: string; to: string; label: string }
export const calendarLabels: Record<UsagePeriod, string> = { day: '今日', week: '本周', month: '本月' };
export const recentLabels: Record<UsagePeriod, string> = { day: '近24小时', week: '近7天', month: '近30天' };
export function allTimeRange(): UsageRange { return { from: '', to: '', label: '全部时间' }; }
export function presetRange(mode: 'calendar' | 'recent', period: UsagePeriod, now = new Date()): UsageRange {
  const from = new Date(now);
  if (mode === 'calendar') {
    from.setHours(0, 0, 0, 0);
    if (period === 'week') from.setDate(from.getDate() - (from.getDay() + 6) % 7);
    if (period === 'month') from.setDate(1);
  } else {
    from.setTime(now.getTime() - ({ day: 1, week: 7, month: 30 }[period]) * 24 * 60 * 60 * 1000);
  }
  return { from: from.toISOString(), to: now.toISOString(), label: (mode === 'calendar' ? calendarLabels : recentLabels)[period] };
}
export function customRange(from: string, to: string): UsageRange {
  const start = new Date(`${from}T00:00:00`), end = new Date(`${to}T23:59:59.999`);
  if (!/^\d{4}-\d{2}-\d{2}$/.test(from) || !/^\d{4}-\d{2}-\d{2}$/.test(to) || !Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || localDate(start) !== from || localDate(end) !== to) throw new Error('请选择有效的开始和结束日期。');
  if (start > end) throw new Error('开始日期不能晚于结束日期。');
  return { from: start.toISOString(), to: end.toISOString(), label: `${from} — ${to}` };
}
export function usageQuery(range: UsageRange, timezone: string): string {
  const params = new URLSearchParams({ timezone });
  if (range.from) params.set('from', range.from);
  if (range.to) params.set('to', range.to);
  return params.toString();
}
