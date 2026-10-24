import type { ModelDefaults, ThinkingDefault } from '../api';
import { Field, Input, Select } from './ui';

export function ModelDefaultsEditor({ value, onChange }: { value: ModelDefaults; onChange: (value: ModelDefaults) => void }) {
  function update<K extends keyof ModelDefaults>(key: K, next: ModelDefaults[K] | undefined) {
    const result = { ...value };
    if (next === undefined) delete result[key]; else result[key] = next;
    onChange(result);
  }
  function thinking(patch: Partial<ThinkingDefault>) { update('thinking', { ...value.thinking!, ...patch }); }
  return <div className="form-grid">
    <Field label="默认最大输出 Tokens" hint="留空不补充；0 原样保存。"><Input aria-label="默认最大输出" type="number" min={0} step={1} value={value.max_output_tokens ?? ''} onChange={event => update('max_output_tokens', event.target.value === '' ? undefined : Number(event.target.value))} /></Field>
    <Field label="默认 temperature" hint="客户端明确温度优先，包括 0。"><Input aria-label="默认 temperature" type="number" step="any" value={value.temperature ?? ''} onChange={event => update('temperature', event.target.value === '' ? undefined : Number(event.target.value))} /></Field>
    <Field label="默认思考模式" hint="仅在候选已有适用映射时注入。"><Select aria-label="默认思考模式" value={value.thinking?.mode || ''} onChange={event => update('thinking', event.target.value ? { mode: event.target.value } : undefined)}><option value="">不补充</option><option value="effort">档位</option><option value="budget">预算</option><option value="enabled">开关</option><option value="adaptive">自适应</option>{value.thinking?.mode && !['effort', 'budget', 'enabled', 'adaptive'].includes(value.thinking.mode) && <option value={value.thinking.mode}>{value.thinking.mode}</option>}</Select></Field>
    {value.thinking?.mode === 'effort' && <Field label="默认思考档位"><Input aria-label="默认思考档位" placeholder="例如 high" value={value.thinking.effort || ''} onChange={event => thinking({ effort: event.target.value })} /></Field>}
    {value.thinking?.mode === 'budget' && <Field label="默认思考预算 (Tokens)"><Input aria-label="默认思考预算" type="number" min={0} step={1} value={value.thinking.budget ?? ''} onChange={event => thinking({ budget: event.target.value === '' ? undefined : Number(event.target.value) })} /></Field>}
    {value.thinking?.mode === 'enabled' && <Field label="默认思考开关"><Select aria-label="默认思考开关" value={value.thinking.enabled == null ? '' : String(value.thinking.enabled)} onChange={event => thinking({ enabled: event.target.value === '' ? undefined : event.target.value === 'true' })}><option value="">不补充</option><option value="true">开启</option><option value="false">关闭</option></Select></Field>}
  </div>;
}
