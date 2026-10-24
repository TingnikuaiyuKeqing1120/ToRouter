import { describe, expect, it } from 'vitest';
import { allTimeRange, customRange, presetRange, usageQuery } from './time';

describe('usage time ranges', () => {
  it('queries all retained history without invented date bounds or a sampling limit', () => {
    expect(Object.fromEntries(new URLSearchParams(usageQuery(allTimeRange(), 'Asia/Shanghai')))).toEqual({ timezone: 'Asia/Shanghai' });
  });
  it('starts natural day and month at local midnight and retains the current end time', () => {
    const now = new Date(2026, 9, 18, 14, 23, 45, 678);
    const today = presetRange('calendar', 'day', now);
    const month = presetRange('calendar', 'month', now);
    expect(today.from).toBe(new Date(2026, 9, 18).toISOString());
    expect(month.from).toBe(new Date(2026, 9, 1).toISOString());
    expect(today.to).toBe(now.toISOString());
    expect(month.to).toBe(now.toISOString());
    expect(now.getHours()).toBe(14);
  });

  it.each([
    [new Date(2026, 9, 18, 14), new Date(2026, 9, 12)],
    [new Date(2026, 9, 19, 14), new Date(2026, 9, 19)],
    [new Date(2026, 9, 21, 14), new Date(2026, 9, 19)],
    [new Date(2027, 0, 1, 14), new Date(2026, 11, 28)],
  ])('starts the natural week on Monday for %s', (now, monday) => {
    const range = presetRange('calendar', 'week', now);
    expect(range.from).toBe(monday.toISOString());
    expect(new Date(range.from).getDay()).toBe(1);
    expect(range.to).toBe(now.toISOString());
  });

  it.each([['day', 1, '近24小时'], ['week', 7, '近7天'], ['month', 30, '近30天']] as const)('uses an exact rolling duration for recent %s', (period, days, label) => {
    const now = new Date('2026-03-08T12:34:56.789-04:00');
    const range = presetRange('recent', period, now);
    expect(Date.parse(range.to) - Date.parse(range.from)).toBe(days * 24 * 60 * 60 * 1000);
    expect(range.to).toBe(now.toISOString());
    expect(range.label).toBe(label);
  });

  it('uses complete local dates, including the end day, for custom ranges', () => {
    const range = customRange('2026-10-02', '2026-10-04');
    expect(range.from).toBe(new Date(2026, 9, 2, 0, 0, 0, 0).toISOString());
    expect(range.to).toBe(new Date(2026, 9, 4, 23, 59, 59, 999).toISOString());
    expect(range.label).toBe('2026-10-02 — 2026-10-04');
    const sameDay = customRange('2026-10-02', '2026-10-02');
    expect(Date.parse(sameDay.to)).toBeGreaterThan(Date.parse(sameDay.from));
  });

  it.each([['', '2026-10-01'], ['2026-02-30', '2026-03-01'], ['2026-13-01', '2026-13-02'], ['2026-1-01', '2026-10-01'], ['2026-10-03', '2026-10-02']])('rejects invalid or inverted custom dates %s / %s', (from, to) => {
    expect(() => customRange(from, to)).toThrow();
  });

  it('keeps the full committed range and timezone in one query without a sampling limit', () => {
    const range = customRange('2026-10-02', '2026-10-04');
    const params = new URLSearchParams(usageQuery(range, 'America/New_York'));
    expect(Object.fromEntries(params)).toEqual({ from: range.from, to: range.to, timezone: 'America/New_York' });
    expect(params.has('limit')).toBe(false);
  });
});
