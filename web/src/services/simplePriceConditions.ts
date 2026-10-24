import type { PriceInputRange, PriceRule, PriceTable, PriceTimeWindow } from '../api';

const fields = ['input', 'output', 'cache_read', 'cache_write'] as const;
export type SimplePriceKind = 'time' | 'context' | 'composite';

export function isSupportedRule(rule: PriceRule): boolean {
  const when = rule.when;
  if (!when) return false;
  // If date_set exists, it's a complex catalog condition
  if (when.date_set && when.date_set.length > 0) return false;
  if (!when.time_window && !when.input_range) return false;
  if (when.time_window && !['UTC', 'Asia/Shanghai'].includes(when.time_window.timezone)) return false;

  const action = rule.action;
  if (!action) return false;
  const fullRates = action.rate_changes && Object.keys(action.rate_changes).length === 4 && fields.every(field => field in action.rate_changes!);
  if (!fullRates && action.multiplier == null) return false;
  return true;
}

export function simplePriceKind(rule: PriceRule): SimplePriceKind | null {
  if (!isSupportedRule(rule)) return null;
  const hasTime = !!rule.when.time_window;
  const hasContext = !!rule.when.input_range;
  if (hasTime && hasContext) return 'composite';
  if (hasTime) return 'time';
  if (hasContext) return 'context';
  return null;
}

export function simplePriceRank(rule: PriceRule): number {
  return rule.when.input_range ? 1 : rule.action.rate_changes ? 0 : 2;
}

export function hasSimplePriceConditions(table: PriceTable): boolean {
  if (!table.rules || table.rules.length === 0) return true;
  if (table.rules.some(rule => !simplePriceKind(rule))) return false;
  if (table.rules.some((rule, index) => index > 0 && simplePriceRank(rule) < simplePriceRank(table.rules[index - 1]))) return false;
  return (['time', 'context'] as const).every(kind => {
    const matching = table.rules.filter(rule => {
      const k = simplePriceKind(rule);
      return k === kind || k === 'composite';
    });
    return new Set(matching.map(rule => rule.enabled)).size <= 1;
  });
}

export function timeSegments(window: PriceTimeWindow): [number, number][] {
  if (window.all_day) return [[0, 1440]];
  const minute = (time: string) => {
    if (!/^\d{2}:\d{2}$/.test(time)) return NaN;
    const [hours, minutes] = time.split(':').map(Number);
    return hours < 24 && minutes < 60 ? hours * 60 + minutes : NaN;
  };
  const offset = window.timezone === 'Asia/Shanghai' ? 480 : 0;
  const start = (minute(window.start) - offset + 1440) % 1440;
  const end = (minute(window.end) - offset + 1440) % 1440;
  if (!Number.isFinite(start) || !Number.isFinite(end) || start === end) return [];
  return start < end ? [[start, end]] : [[start, 1440], [0, end]];
}

export function isRangeOverlap(a?: PriceInputRange | null, b?: PriceInputRange | null): boolean {
  if (!a || !b) return true;
  const aTo = a.to_tokens ?? Infinity;
  const bTo = b.to_tokens ?? Infinity;
  return a.from_tokens <= bTo && b.from_tokens <= aTo;
}

export function isWeekdaysOverlap(a?: number[] | null, b?: number[] | null): boolean {
  const daysA = (a && a.length > 0) ? a : [1, 2, 3, 4, 5, 6, 7];
  const daysB = (b && b.length > 0) ? b : [1, 2, 3, 4, 5, 6, 7];
  return daysA.some(d => daysB.includes(d));
}

export function isTimeWindowOverlap(a?: PriceTimeWindow | null, b?: PriceTimeWindow | null): boolean {
  if (!a || !b) return true;
  const segsA = timeSegments(a);
  const segsB = timeSegments(b);
  if (segsA.length === 0 || segsB.length === 0) return true;
  return segsA.some(([s1, e1]) => segsB.some(([s2, e2]) => s1 < e2 && s2 < e1));
}

export function simplePriceTableError(table: PriceTable): string {
  if (!hasSimplePriceConditions(table) || !table.rules) return '';
  const rules = table.rules;
  for (let i = 0; i < rules.length; i++) {
    for (let j = i + 1; j < rules.length; j++) {
      const first = rules[i], second = rules[j];
      if (!first.enabled || !second.enabled) continue;

      const hasRangeA = !!first.when.input_range;
      const hasRangeB = !!second.when.input_range;
      const hasTimeA = !!first.when.time_window;
      const hasTimeB = !!second.when.time_window;

      // Pure time and pure context conditions are orthogonal and do not conflict
      if ((hasTimeA && !hasRangeA && hasRangeB && !hasTimeB) ||
          (hasRangeA && !hasTimeA && hasTimeB && !hasRangeB)) {
        continue;
      }

      const contextOverlap = isRangeOverlap(first.when.input_range, second.when.input_range);
      const weekOverlap = isWeekdaysOverlap(first.when.weekdays, second.when.weekdays);
      const timeOverlap = isTimeWindowOverlap(first.when.time_window, second.when.time_window);

      if (contextOverlap && weekOverlap && timeOverlap) {
        const bothTime = !!(first.when.time_window && second.when.time_window);
        const bothContext = !!(first.when.input_range && second.when.input_range);
        if (bothContext && !bothTime) {
          return `分段 ${i + 1} 与分段 ${j + 1} 重叠；两端都包含，请调整边界后保存。`;
        }
        if (bothTime && !bothContext) {
          return `分段 ${i + 1} 与分段 ${j + 1} 重叠；请调整时段后保存。`;
        }
        return `分段 ${i + 1} 与分段 ${j + 1} 重叠；时段、星期与上下文条件同时存在冲突，请调整后保存。`;
      }
    }
  }
  return '';
}
