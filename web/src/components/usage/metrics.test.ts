import { describe, expect, it } from 'vitest';
import { displayMoney, formatMetric, metricValue, pricingValues, unpricedRequests } from './metrics';
import type { UsageMetrics } from '../../api';

describe('usage number display', () => {
  it('counts known monthly charges as money without treating them as priced requests', () => {
    const value = { requests: 5, actual_priced_requests: 1, plan_requests: 3, plan_charges: 4, unknown_plan_charges: 1, actual_costs: { USD: 20, CNY: 0 } } as UsageMetrics;
    expect(pricingValues(value, 'actual').count).toBe(4);
    expect(unpricedRequests(value, 'actual')).toBe(1);
    expect(metricValue(value, 'actual', 'USD', null)).toBe(20);
  });
  it('preserves unknown monthly cost with no requests and does not allocate monthly cost to plan-only model usage', () => {
    const value = { requests: 0, actual_priced_requests: 0, plan_charges: 1, unknown_plan_charges: 1, actual_costs: { USD: 0, CNY: 0 } } as UsageMetrics;
    expect(metricValue(value, 'actual', 'USD', null)).toBeNull();
    expect(metricValue({ ...value, requests: 2, plan_requests: 2, plan_charges: 0, unknown_plan_charges: 0 }, 'actual', 'USD', null)).toBeNull();
    expect(metricValue({ ...value, unknown_plan_charges: 0 }, 'actual', 'USD', null)).toBe(0);
  });
  it('uses readable summary precision and currency symbols', () => {
    expect(displayMoney(32.67858, 'CNY')).toBe('¥32.68');
    expect(displayMoney(0, 'USD')).toBe('$0.00');
    expect(displayMoney(0.00123456, 'USD')).toBe('$0.0012');
    expect(formatMetric(112124081, 'tokens', 'USD', true)).toBe('1.12亿');
    expect(formatMetric(112124081, 'tokens', 'USD')).toBe('112,124,081');
  });
  it.each([0.000001, 0.000000123456789, 1e-24, Number.MIN_VALUE, -0.000000123456789])('never rounds nonzero %s into free zero', value => {
    for (const currency of ['USD', 'CNY'] as const) {
      const displayed = displayMoney(value, currency).slice(1);
      const precise = formatMetric(value, 'actual', currency).slice(1);
      expect(Number(displayed)).not.toBe(0);
      expect(Number(precise)).toBe(value);
      expect(formatMetric(value, 'actual', currency, true)).toBe(displayMoney(value, currency));
    }
  });
  it('preserves detailed amount precision and unknown states', () => {
    expect(formatMetric(32.678581234567, 'actual', 'CNY')).toBe('¥32.6785812346');
    expect(formatMetric(0.1 + 0.2, 'actual', 'CNY')).toBe('¥0.30');
    expect(formatMetric(0.00000149, 'actual', 'USD')).toBe('$0.00000149');
    expect(formatMetric(0.00000123456789, 'actual', 'USD')).toBe('$0.00000123456789');
    expect(formatMetric(null, 'actual', 'USD', true)).toBe('—');
    expect(displayMoney(Number.NaN, 'USD')).toBe('—');
    expect(formatMetric(Number.POSITIVE_INFINITY, 'actual', 'USD')).toBe('—');
  });
});
