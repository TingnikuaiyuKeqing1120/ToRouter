import type { ModelConfig } from '../api';
import { inputModalityOptions, outputModalityOptions } from '../api';
import { Field, Input, Select } from './ui';
import { ThinkingEffortPicker } from './ThinkingEffortPicker';

export function ModelSpecsEditor({ value, onChange }: { value: ModelConfig; onChange: (value: ModelConfig) => void }) {
  function update<K extends keyof ModelConfig>(key: K, next: ModelConfig[K] | undefined) {
    const result = { ...value };
    if (next === undefined) delete result[key]; else result[key] = next;
    onChange(result);
  }
  function numberField(key: 'context_window' | 'max_output_tokens' | 'max_input_tokens', label: string) {
    return <Field label={label} hint="留空表示未知，0 表示已知为 0。"><Input aria-label={label} type="number" min={0} step={1} value={value[key] ?? ''} onChange={event => update(key, event.target.value === '' ? undefined : Number(event.target.value))} /></Field>;
  }
  function modalities(key: 'input_modalities' | 'output_modalities', label: string, options: string[]) {
    const selected = value[key];
    return <Field label={label}><label className="flex items-center gap-2 text-xs"><input className="checkbox" type="checkbox" aria-label={`设置${label}`} checked={selected !== undefined} onChange={event => update(key, event.target.checked ? [] : undefined)} />{selected === undefined ? '未知' : '已明确；不选表示空列表'}</label>{selected !== undefined && <div className="flex flex-wrap gap-3 pt-2">{[...new Set([...options, ...selected])].map(option => <label key={option} className="flex items-center gap-1.5 text-xs"><input className="checkbox" type="checkbox" aria-label={`${label} ${option}`} checked={selected.includes(option)} onChange={event => update(key, event.target.checked ? [...selected, option] : selected.filter(item => item !== option))} />{option}</label>)}</div>}</Field>;
  }
  const native = value.native_thinking;
  return <div className="space-y-4">
    <div className="form-grid">{numberField('context_window', '上下文总窗口')}{numberField('max_output_tokens', '最大输出 tokens')}{modalities('input_modalities', '输入模态', inputModalityOptions)}{modalities('output_modalities', '输出模态', outputModalityOptions)}</div>
    <div className="space-y-2"><label className="flex items-center gap-2 text-xs"><input className="checkbox" type="checkbox" aria-label="设置支持的思考选项" checked={value.thinking_efforts !== undefined} onChange={event => update('thinking_efforts', event.target.checked ? [] : undefined)} />支持的思考选项{value.thinking_efforts === undefined ? ' · 未知' : ' · 不选表示空列表'}</label>{value.thinking_efforts !== undefined && <ThinkingEffortPicker efforts={value.thinking_efforts} showDefault={false} onChange={next => update('thinking_efforts', next)} />}</div>
    <div className="form-grid"><Field label="支持思考"><Select aria-label="原生思考支持" value={native?.supported == null ? '' : String(native.supported)} onChange={event => update('native_thinking', { ...(native || { modes: [] }), supported: event.target.value === '' ? null : event.target.value === 'true' })}><option value="">未知</option><option value="true">支持</option><option value="false">不支持</option></Select></Field><Field label="思考是否必需"><Select aria-label="思考是否必需" value={native?.mandatory == null ? '' : String(native.mandatory)} onChange={event => update('native_thinking', { ...(native || { supported: null, modes: [] }), mandatory: event.target.value === '' ? null : event.target.value === 'true' })}><option value="">未知</option><option value="true">必需</option><option value="false">可关闭</option></Select></Field></div>
    <details className="advanced"><summary>其他规格与原生映射</summary><div className="space-y-4 pt-3">
      <div className="form-grid">{numberField('max_input_tokens', '最大输入 tokens')}<Field label="流式支持"><Select aria-label="流式支持" value={value.streaming === undefined ? '' : String(value.streaming)} onChange={event => update('streaming', event.target.value === '' ? undefined : event.target.value === 'true')}><option value="">未知</option><option value="true">支持</option><option value="false">不支持</option></Select></Field></div>
      <section className="space-y-2"><h3 className="text-xs font-medium text-[var(--color-text-secondary)]">原生映射与原厂默认资料（只读）</h3><pre tabIndex={0} aria-label="原生思考映射资料" className="overflow-auto rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3 text-xs font-mono">{JSON.stringify({ output_token_field: value.output_token_field ?? null, modes: native?.modes ?? null }, null, 2)}</pre></section>
    </div></details>
  </div>;
}
