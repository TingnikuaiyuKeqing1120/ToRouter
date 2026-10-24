import { useState } from 'react';
import type { PriceRates, PriceRule, PriceTable } from '../api';
import { catalogAmount, catalogRateFields, catalogRateLabels, catalogRuleConditions } from '../services/catalogPriceDisplay';
import { hasSimplePriceConditions, simplePriceRank, simplePriceTableError } from '../services/simplePriceConditions';
import { Button, ErrorBox, Field, Input, Select } from './ui';

export function PriceConditionsReadOnly({ value, summary = '条件报价', notice }: { value: PriceTable; summary?: string; notice?: string }) {
  if (!value.rules.length && !value.uncovered_conditions?.length) return null;
  return <>{!!value.rules.length && <details><summary>{summary} · {value.rules.length} 项</summary><div className="space-y-2 pt-2 text-xs">
    {notice && <p className="text-[var(--color-text-muted)]">{notice}</p>}
    {value.rules.map((rule, index) => <div key={index} className="border-l border-[var(--color-border)] pl-2">
      <p>{catalogRuleConditions(rule)}{!rule.enabled ? ' · 未启用' : ''}</p>
      {rule.action.rate_changes && <p>{catalogRateFields.filter(field => field in rule.action.rate_changes!).map((field, i) => <span key={field}>{i > 0 && ' / '}{catalogRateLabels[catalogRateFields.indexOf(field)]} {catalogAmount(rule.action.rate_changes![field])}</span>)} · {value.currency} / {value.unit_tokens} Tokens</p>}
      {rule.action.multiplier != null && <p>价格 × {rule.action.multiplier}</p>}
    </div>)}
  </div></details>}{!!value.uncovered_conditions?.length && <details><summary>价格备注 · {value.uncovered_conditions.length} 项</summary><div className="space-y-2 pt-2 text-xs">{value.uncovered_conditions.map((condition, index) => <p key={index}>{condition}</p>)}</div></details>}</>;
}

function IndependentRates({ value, onChange, prefix }: { value: PriceRates; onChange: (rates: PriceRates) => void; prefix: string }) {
  return <div className="price-amounts">{catalogRateFields.map((field, index) => <Field key={field} label={catalogRateLabels[index]}><Input aria-label={`${prefix} ${catalogRateLabels[index]}`} type="number" min={0} step="any" placeholder="未知" value={value[field] ?? ''} onChange={e => onChange({ ...value, [field]: e.target.value === '' ? null : Number(e.target.value) })} /></Field>)}</div>;
}

export function PriceConditionsEditor({ value, onChange, compact = false }: { value: PriceTable; onChange: (table: PriceTable) => void; compact?: boolean }) {
  if (!hasSimplePriceConditions(value)) return <PriceConditionsReadOnly value={value} summary={compact ? '条件计费' : '条件报价'} notice="已有条件按原报价保留，仅可查看。" />;
  
  const hasTimeRules = value.rules.some(r => r.enabled && !!r.when.time_window);
  const hasContextRules = value.rules.some(r => r.enabled && !!r.when.input_range);
  const [enableTime, setEnableTime] = useState(hasTimeRules);
  const [enableContext, setEnableContext] = useState(hasContextRules);

  const overlapError = simplePriceTableError(value);
  const update = (rules: PriceRule[]) => onChange({ ...value, rules: [...rules].sort((a, b) => simplePriceRank(a) - simplePriceRank(b)) });
  const patch = (index: number, change: Partial<PriceRule>) => update(value.rules.map((rule, i) => i === index ? { ...rule, ...change } : rule));

  const timeEnabled = value.rules.length > 0 ? value.rules.some(r => r.enabled && !!r.when.time_window) : enableTime;
  const contextEnabled = value.rules.length > 0 ? value.rules.some(r => r.enabled && !!r.when.input_range) : enableContext;
  const anyEnabled = timeEnabled || contextEnabled;

  const handleToggleTime = (checked: boolean) => {
    setEnableTime(checked);
    if (!checked) {
      if (contextEnabled) {
        update(value.rules.map(r => {
          const nextWhen = { ...r.when };
          delete nextWhen.time_window;
          delete nextWhen.weekdays;
          delete nextWhen.timezone;
          return { ...r, when: nextWhen };
        }));
      } else {
        update(value.rules.map(r => ({ ...r, enabled: false })));
      }
    } else {
      if (value.rules.length > 0) {
        update(value.rules.map(r => {
          if (r.when.time_window) {
            return { ...r, enabled: true };
          }
          if (r.enabled) {
            return {
              ...r,
              when: {
                ...r.when,
                time_window: { start: '22:00', end: '06:00', all_day: false, timezone: 'Asia/Shanghai' },
                weekdays: r.when.weekdays || [1, 2, 3, 4, 5, 6, 7],
                timezone: r.when.timezone || 'Asia/Shanghai',
              }
            };
          }
          return r;
        }));
      }
    }
  };

  const handleToggleContext = (checked: boolean) => {
    setEnableContext(checked);
    if (!checked) {
      if (timeEnabled) {
        update(value.rules.map(r => {
          const nextWhen = { ...r.when };
          delete nextWhen.input_range;
          return { ...r, when: nextWhen };
        }));
      } else {
        update(value.rules.map(r => ({ ...r, enabled: false })));
      }
    } else {
      if (value.rules.length > 0) {
        update(value.rules.map(r => {
          if (r.when.input_range) {
            return { ...r, enabled: true };
          }
          if (r.enabled) {
            return {
              ...r,
              when: {
                ...r.when,
                input_range: { from_tokens: 0, to_tokens: null },
              }
            };
          }
          return r;
        }));
      }
    }
  };

  const handleCreateSegment = () => {
    const when: PriceRule['when'] = {};
    if (timeEnabled) {
      when.time_window = { start: '22:00', end: '06:00', all_day: false, timezone: 'Asia/Shanghai' };
      when.weekdays = [1, 2, 3, 4, 5, 6, 7];
      when.timezone = 'Asia/Shanghai';
    }
    if (contextEnabled) {
      when.input_range = { from_tokens: 0, to_tokens: null };
    }
    const newRule: PriceRule = {
      enabled: true,
      when,
      action: { multiplier: 1 },
    };
    update([...value.rules, newRule]);
  };

  const activeRules = value.rules.map((rule, index) => ({ rule, index })).filter(({ rule }) => rule.enabled);

  const controls = (
    <div className="space-y-4">
      <div className="bg-[var(--color-bg-subtle)] p-3 rounded border border-[var(--color-border)] space-y-2">
        <div className="text-xs font-medium text-[var(--color-text-muted)]">高级计费分段条件支持</div>
        <div className="flex flex-wrap items-center gap-6">
          <label className="flex items-center gap-2 cursor-pointer text-sm">
            <input
              type="checkbox"
              aria-label="按时段计费"
              checked={timeEnabled}
              onChange={e => handleToggleTime(e.target.checked)}
            />
            <span>启用按时段</span>
          </label>
          <label className="flex items-center gap-2 cursor-pointer text-sm">
            <input
              type="checkbox"
              aria-label="按上下文长度计费"
              checked={contextEnabled}
              onChange={e => handleToggleContext(e.target.checked)}
            />
            <span>启用按上下文长度</span>
          </label>
        </div>
      </div>

      {!anyEnabled ? (
        <p className="text-xs text-[var(--color-text-muted)]">勾选上方条件以配置闲时打折、长上下文分档等高级计费策略。</p>
      ) : (
        <div className="space-y-3">
          {activeRules.length === 0 ? (
            <p className="text-xs text-[var(--color-text-muted)]">暂无计费分段，点击下方【+ 新建分段】添加分段。</p>
          ) : (
            activeRules.map(({ rule, index }, activeIdx) => {
              const prefix = `分段 ${activeIdx + 1}`;
              const weekdays = rule.when.weekdays ?? [1, 2, 3, 4, 5, 6, 7];
              const hasWorkday = weekdays.some(d => d >= 1 && d <= 5);
              const hasWeekend = weekdays.some(d => d === 6 || d === 7);

              const toggleWeekdayPart = (part: 'workday' | 'weekend', checked: boolean) => {
                let nextDays: number[] = [];
                if (part === 'workday') {
                  if (checked) {
                    nextDays = [...weekdays.filter(d => d > 5), 1, 2, 3, 4, 5].sort();
                  } else {
                    nextDays = weekdays.filter(d => d > 5);
                  }
                } else {
                  if (checked) {
                    nextDays = [...weekdays.filter(d => d <= 5), 6, 7].sort();
                  } else {
                    nextDays = weekdays.filter(d => d <= 5);
                  }
                }
                if (nextDays.length === 0) return;
                const tz = rule.when.time_window?.timezone || rule.when.timezone || 'Asia/Shanghai';
                patch(index, { when: { ...rule.when, weekdays: nextDays, timezone: tz } });
              };

              return (
                <div key={index} className="price-rule space-y-3 p-3 border border-[var(--color-border)] rounded bg-[var(--color-surface-1)]">
                  <div className="flex items-center justify-between">
                    <span className="font-semibold text-sm text-[var(--color-text-primary)]">{prefix}</span>
                    <Button variant="ghost" aria-label={`删除${prefix}`} onClick={() => update(value.rules.filter((_, i) => i !== index))}>删除</Button>
                  </div>

                  {/* 上下文长度匹配条件（若启用上下文则展示） */}
                  {contextEnabled && (
                    <div className="space-y-2 pt-1 border-t border-[var(--color-border-subtle)]">
                      <div className="text-xs font-medium text-[var(--color-text-muted)]">上下文 Token 区间（两端包含，整次请求同档）</div>
                      <div className="form-grid">
                        <Field label="起始 Token（含）">
                          <Input
                            aria-label={`${prefix} 起始 Token`}
                            type="number"
                            min={0}
                            step={1}
                            value={rule.when.input_range?.from_tokens ?? 0}
                            onChange={e => patch(index, {
                              when: {
                                ...rule.when,
                                input_range: {
                                  from_tokens: Number(e.target.value),
                                  to_tokens: rule.when.input_range?.to_tokens ?? null,
                                }
                              }
                            })}
                          />
                        </Field>
                        <Field label="终止 Token（含）">
                          <Input
                            aria-label={`${prefix} 终止 Token`}
                            type="number"
                            min={0}
                            step={1}
                            placeholder="不限"
                            value={rule.when.input_range?.to_tokens ?? ''}
                            onChange={e => patch(index, {
                              when: {
                                ...rule.when,
                                input_range: {
                                  from_tokens: rule.when.input_range?.from_tokens ?? 0,
                                  to_tokens: e.target.value === '' ? null : Number(e.target.value),
                                }
                              }
                            })}
                          />
                        </Field>
                      </div>
                    </div>
                  )}

                  {/* 时段匹配条件（若启用时段则展示） */}
                  {timeEnabled && (
                    <div className="space-y-2 pt-1 border-t border-[var(--color-border-subtle)]">
                      <div className="text-xs font-medium text-[var(--color-text-muted)]">时段与适用日期</div>
                      <div className="form-grid">
                        <Field label="开始">
                          <Input
                            aria-label={`${prefix} 开始`}
                            type="time"
                            disabled={rule.when.time_window?.all_day}
                            value={rule.when.time_window?.start ?? '22:00'}
                            onChange={e => patch(index, {
                              when: {
                                ...rule.when,
                                time_window: {
                                  start: e.target.value,
                                  end: rule.when.time_window?.end ?? '06:00',
                                  all_day: !!rule.when.time_window?.all_day,
                                  timezone: rule.when.time_window?.timezone ?? rule.when.timezone ?? 'Asia/Shanghai',
                                }
                              }
                            })}
                          />
                        </Field>
                        <Field label="结束">
                          <Input
                            aria-label={`${prefix} 结束`}
                            type="time"
                            disabled={rule.when.time_window?.all_day}
                            value={rule.when.time_window?.end ?? '06:00'}
                            onChange={e => patch(index, {
                              when: {
                                ...rule.when,
                                time_window: {
                                  start: rule.when.time_window?.start ?? '22:00',
                                  end: e.target.value,
                                  all_day: !!rule.when.time_window?.all_day,
                                  timezone: rule.when.time_window?.timezone ?? rule.when.timezone ?? 'Asia/Shanghai',
                                }
                              }
                            })}
                          />
                        </Field>
                        <label className="flex items-center gap-1.5 pt-6 text-sm cursor-pointer">
                          <input
                            type="checkbox"
                            aria-label={`${prefix} 全天`}
                            checked={!!rule.when.time_window?.all_day}
                            onChange={e => patch(index, {
                              when: {
                                ...rule.when,
                                time_window: {
                                  start: rule.when.time_window?.start ?? '22:00',
                                  end: rule.when.time_window?.end ?? '06:00',
                                  all_day: e.target.checked,
                                  timezone: rule.when.time_window?.timezone ?? rule.when.timezone ?? 'Asia/Shanghai',
                                }
                              }
                            })}
                          />
                          全天
                        </label>
                        <Field label="时区">
                          <Select
                            aria-label={`${prefix} 时区`}
                            value={rule.when.time_window?.timezone ?? rule.when.timezone ?? 'Asia/Shanghai'}
                            onChange={e => {
                              const tz = e.target.value;
                              patch(index, {
                                when: {
                                  ...rule.when,
                                  time_window: {
                                    start: rule.when.time_window?.start ?? '22:00',
                                    end: rule.when.time_window?.end ?? '06:00',
                                    all_day: !!rule.when.time_window?.all_day,
                                    timezone: tz,
                                  },
                                  timezone: tz,
                                }
                              });
                            }}
                          >
                            <option value="Asia/Shanghai">上海时间</option>
                            <option value="UTC">UTC</option>
                          </Select>
                        </Field>
                      </div>

                      <div className="flex items-center gap-4 text-xs pt-1">
                        <span className="text-[var(--color-text-muted)]">生效日期：</span>
                        <label className="flex items-center gap-1 cursor-pointer">
                          <input
                            type="checkbox"
                            aria-label={`${prefix} 工作日`}
                            checked={hasWorkday}
                            onChange={e => toggleWeekdayPart('workday', e.target.checked)}
                          />
                          工作日（周一至周五）
                        </label>
                        <label className="flex items-center gap-1 cursor-pointer">
                          <input
                            type="checkbox"
                            aria-label={`${prefix} 周末`}
                            checked={hasWeekend}
                            onChange={e => toggleWeekdayPart('weekend', e.target.checked)}
                          />
                          周末（周六与周日）
                        </label>
                      </div>
                    </div>
                  )}

                  {/* 价格配置 / 费率计算 */}
                  <div className="pt-1 border-t border-[var(--color-border-subtle)] space-y-2">
                    <div className="text-xs font-semibold text-[var(--color-text-primary)]">价格配置</div>
                    <Select
                      aria-label={`${prefix} 计费方式`}
                      value={rule.action.rate_changes ? 'fixed' : 'multiplier'}
                      onChange={e => patch(index, { action: e.target.value === 'fixed' ? { rate_changes: { ...value.rates } } : { multiplier: 1 } })}
                    >
                      <option value="multiplier">倍率（如 0.5 表示半价）</option>
                      <option value="fixed">独立价格</option>
                    </Select>

                    {rule.action.rate_changes ? (
                      <IndependentRates prefix={prefix} value={rule.action.rate_changes as PriceRates} onChange={rates => patch(index, { action: { rate_changes: rates } })} />
                    ) : (
                      <Field label="倍率">
                        <Input
                          aria-label={`${prefix} 倍率`}
                          type="number"
                          min={0}
                          step="any"
                          value={rule.action.multiplier ?? ''}
                          onChange={e => patch(index, { action: { multiplier: e.target.value === '' ? null : Number(e.target.value) } })}
                        />
                      </Field>
                    )}
                  </div>
                </div>
              );
            })
          )}

          <div className="pt-1">
            <Button
              variant="secondary"
              aria-label="新建分段"
              onClick={handleCreateSegment}
            >
              + 新建分段
            </Button>
          </div>
        </div>
      )}

      {!!value.uncovered_conditions?.length && (
        <details>
          <summary>价格备注 · {value.uncovered_conditions.length} 项</summary>
          {value.uncovered_conditions.map((condition, index) => <p className="text-xs pt-2" key={index}>{condition}</p>)}
        </details>
      )}
    </div>
  );

  return (
    <>
      {overlapError && <ErrorBox message={overlapError} />}
      {compact ? (
        <details>
          <summary>条件计费 · {value.rules.length} 项</summary>
          <div className="pt-3">{controls}</div>
        </details>
      ) : (
        controls
      )}
    </>
  );
}
