import { Plus, Trash2 } from 'lucide-react';
import type { CredentialAutoResetDraft, CredentialOperationsConfig, CredentialResetRuleDraft } from '../types/credential-operations';
import { Button, Field, Input, Select } from './ui';
import './CodexCredentialOperations.css';

function windowName(seconds: number) { return seconds % 86400 === 0 ? `${seconds / 86400} 天` : seconds % 3600 === 0 ? `${seconds / 3600} 小时` : `${seconds / 60} 分钟`; }

export function CodexAutomaticWarmupForm({ value, onChange, models, hasFiveHourWindow, timeZone, disabled = false }: {
  value: CredentialOperationsConfig['warmup']; onChange: (value: CredentialOperationsConfig['warmup']) => void; models: string[]; hasFiveHourWindow: boolean; timeZone?: string; disabled?: boolean;
}) {
  return <section className="codex-operations-config" aria-label="自动预热配置">
    <label className="codex-operations-check"><input type="checkbox" className="checkbox" aria-label="自动预热" checked={value.enabled} disabled={disabled || !hasFiveHourWindow && !value.enabled} onChange={event => onChange({ ...value, enabled: event.target.checked })} />启用自动预热</label>
    {!hasFiveHourWindow && <p className="codex-operations-muted">上游未明确提供 5 小时额度窗口，暂不支持自动预热。</p>}
    <Field label="预热模型"><Select aria-label="预热模型" value={value.model} disabled={disabled} onChange={event => onChange({ ...value, model: event.target.value })}><option value="">选择已启用的模型</option>{value.model && !models.includes(value.model) && <option value={value.model} disabled>{value.model}（已停用）</option>}{models.map(model => <option key={model} value={model}>{model}</option>)}</Select>{!models.length && <span>尚未启用模型，请先编辑渠道。</span>}</Field>
    <fieldset disabled={disabled} className="codex-operations-modes"><legend>自动预热方式</legend><label><input type="radio" name="warmup-mode" value="reset" checked={value.mode === 'reset'} onChange={() => onChange({ ...value, mode: 'reset', daily_times: value.daily_times.filter(Boolean) })} />跟随额度自然重置</label><label><input type="radio" name="warmup-mode" value="daily" checked={value.mode === 'daily'} onChange={() => onChange({ ...value, mode: 'daily' })} />每日定时</label></fieldset>
    {value.mode === 'reset' && <p className="codex-operations-muted">5小时窗口到期后预热一次，启动下一窗口；长周期额度耗尽时等待。</p>}
    {value.mode === 'daily' && <fieldset className="codex-daily-times" disabled={disabled}><legend>每日预热时刻</legend><p className="codex-operations-muted">网关时区：{timeZone || '未知'}</p>{!value.daily_times.length && <p className="codex-operations-muted">尚未设置时刻。</p>}{value.daily_times.map((time, index) => <div className="codex-daily-time" key={index}><Input type="time" aria-label={`每日预热时刻 ${index + 1}`} value={time} onChange={event => onChange({ ...value, daily_times: value.daily_times.map((item, position) => position === index ? event.target.value : item) })} /><Button variant="ghost" aria-label={`删除预热时刻 ${index + 1}`} onClick={() => onChange({ ...value, daily_times: value.daily_times.filter((_, position) => position !== index) })}><Trash2 size={14} />删除</Button></div>)}<Button variant="secondary" onClick={() => onChange({ ...value, daily_times: [...value.daily_times, ''] })}><Plus size={14} />添加预热时刻</Button></fieldset>}
  </section>;
}

export function CodexAutomaticResetForm({ value, onChange, windows, disabled = false }: {
  value: CredentialAutoResetDraft; onChange: (value: CredentialAutoResetDraft) => void; windows: number[]; disabled?: boolean;
}) {
  function changeRule(index: number, patch: Partial<CredentialResetRuleDraft>) { onChange({ ...value, rules: value.rules.map((rule, position) => position === index ? { ...rule, ...patch } : rule) }); }
  return <section className="codex-operations-config" aria-label="自动重置配置">
    <label className="codex-operations-check"><input type="checkbox" className="checkbox" aria-label="自动重置" checked={value.enabled} disabled={disabled} onChange={event => onChange({ ...value, enabled: event.target.checked })} />启用自动重置</label>
    <p className="codex-operations-muted">任意一组满足时使用一张；条件连续满足时只执行一次，恢复不满足后可再次触发。</p>
    {value.rules.map((rule, index) => <fieldset key={index} disabled={disabled} className="codex-reset-rule"><legend>规则 {index + 1}</legend><div className="codex-operations-section-heading"><label className="codex-operations-check"><input type="checkbox" className="checkbox" aria-label={`规则 ${index + 1} 额度条件`} checked={rule.remaining} onChange={event => changeRule(index, { remaining: event.target.checked })} />额度剩余</label><Button variant="ghost" aria-label={`删除规则 ${index + 1}`} onClick={() => onChange({ ...value, rules: value.rules.filter((_, position) => position !== index) })}><Trash2 size={14} />删除</Button></div>
      <div className="codex-reset-rule-condition"><Select aria-label={`规则 ${index + 1} 额度窗口`} disabled={!rule.remaining || disabled} value={rule.window_seconds} onChange={event => changeRule(index, { window_seconds: Number(event.target.value) })}>{!rule.window_seconds && <option value={0}>窗口未知</option>}{rule.window_seconds > 0 && !windows.includes(rule.window_seconds) && <option value={rule.window_seconds}>{windowName(rule.window_seconds)}（当前未提供）</option>}{windows.map(seconds => <option key={seconds} value={seconds}>{windowName(seconds)}</option>)}</Select><span>低于</span><Input type="number" min={0} max={100} step="any" aria-label={`规则 ${index + 1} 剩余比例`} disabled={!rule.remaining || disabled} value={rule.remaining_below} onChange={event => changeRule(index, { remaining_below: event.target.value })} /><span>%</span></div>
      <Select aria-label={`规则 ${index + 1} 条件关系`} disabled={disabled || !rule.remaining || !rule.expires} value={rule.join} onChange={event => changeRule(index, { join: event.target.value as 'and' | 'or' })}><option value="and">且 · 同时满足</option><option value="or">或 · 满足任一</option></Select>
      <label className="codex-operations-check"><input type="checkbox" className="checkbox" aria-label={`规则 ${index + 1} 到期条件`} checked={rule.expires} onChange={event => changeRule(index, { expires: event.target.checked })} />重置卡即将到期</label><div className="codex-reset-rule-expiry"><span>到期不足</span><Input type="number" min={0} max={3650} step="any" aria-label={`规则 ${index + 1} 卡到期天数`} disabled={!rule.expires || disabled} value={rule.expires_within_days} onChange={event => changeRule(index, { expires_within_days: event.target.value })} /><span>天</span></div>
      {rule.remaining && rule.expires && rule.join === 'or' && <p className="codex-operations-muted">卡即将到期时，即使额度充足也会使用。</p>}
    </fieldset>)}
    <Button variant="secondary" disabled={disabled || value.rules.length >= 32} onClick={() => onChange({ ...value, rules: [...value.rules, { window_seconds: Math.max(0, ...windows), remaining: false, remaining_below: '', expires: false, expires_within_days: '', join: 'and' }] })}><Plus size={14} />添加规则</Button>
  </section>;
}
