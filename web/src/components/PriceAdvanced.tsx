import { useState } from 'react';
import { errorMessage, type CatalogItem, type Currency, type ExchangeRate, type Price, type PriceRates, type PriceTable } from '../api';
import { priceFields } from '../hooks/usePriceWorkbench';
import { convertTable } from '../services/priceTableCurrency';
import { simplePriceTableError } from '../services/simplePriceConditions';
import { PrefillControls } from './CatalogPrefill';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { PriceConditionsEditor } from './PriceConditions';
import { Button, ErrorBox, Field, Input, Modal, Select } from './ui';

export { convertTable, convertPrice } from '../services/priceTableCurrency';
const labels = ['输入', '输出', '缓存命中', '缓存写入'];
export const emptyRates = (): PriceRates => ({ input: null, output: null, cache_read: null, cache_write: null });
export const emptyTable = (currency: Currency = 'CNY'): PriceTable => ({ currency, unit_tokens: 1000000, rates: emptyRates(), rules: [] });

function Rates({ value, onChange, prefix }: { value: PriceRates; onChange: (rates: PriceRates) => void; prefix: string }) {
  return <div className="price-amounts">{priceFields.map((key, i) => <Field key={key} label={labels[i]}><Input aria-label={`${prefix} ${labels[i]}`} type="number" min={0} step="any" placeholder="未知" value={value[key] ?? ''} onChange={e => onChange({ ...value, [key]: e.target.value === '' ? null : Number(e.target.value) })} /></Field>)}</div>;
}

export function PriceTableEditor({ value, onChange, rate = null, compact = false }: { value: PriceTable; onChange: (table: PriceTable) => void; rate?: ExchangeRate | null; compact?: boolean }) {
  const [error, setError] = useState('');
  const patch = (change: Partial<PriceTable>) => onChange({ ...value, ...change });
  return <div className="space-y-4">
    {error && <ErrorBox message={error} />}
    <div className="form-grid">
      <Field label="币种"><Select aria-label="高级行币种" value={value.currency} onChange={e => { try { onChange(convertTable(value, e.target.value as Currency, rate)); setError(''); } catch (e) { setError(errorMessage(e)); } }}><option value="CNY">人民币（CNY）</option><option value="USD">美元（USD）</option></Select></Field>
      <Field label="每 X Tokens"><Select aria-label="计价单位" value={[1000, 1000000, 100000000].includes(value.unit_tokens) ? value.unit_tokens : 'custom'} onChange={e => patch({ unit_tokens: e.target.value === 'custom' ? 1 : Number(e.target.value) })}><option value={1000}>1K</option><option value={1000000}>1M</option><option value={100000000}>1亿</option><option value="custom">自定义</option></Select>{![1000, 1000000, 100000000].includes(value.unit_tokens) && <Input aria-label="自定义 Token 单位" type="number" min={1} value={value.unit_tokens} onChange={e => patch({ unit_tokens: Number(e.target.value) })} />}</Field>
    </div>
    <Rates prefix="基础" value={value.rates} onChange={rates => patch({ rates })} />
    <PriceConditionsEditor value={value} onChange={onChange} compact={compact} />
  </div>;
}

export function PriceAdvanced({ value, savedValue = value, rate, resolvedTable, catalogItems = [], onDraft, onApply, onClose, onDiscard }: { value: Price; savedValue?: Price; rate: ExchangeRate | null; resolvedTable?: PriceTable; catalogItems?: CatalogItem[]; onDraft: (price: Price) => void; onApply: (price: Price) => Promise<void>; onClose: () => void; onDiscard: () => void }) {
  const [form, setForm] = useState(value), [error, setError] = useState(''), [busy, setBusy] = useState(false);
  const change = (table: PriceTable) => { const next: Price = { ...form, selection: { mode: 'manual', table } }; setForm(next); onDraft(next); };
  const conditionError = form.selection.mode === 'manual' ? simplePriceTableError(form.selection.table) : '';
  async function save() {
    if (conditionError) { setError(conditionError); return false; }
    setBusy(true);
    try { await onApply(form); return true; } catch (e) { setError(errorMessage(e)); return false; } finally { setBusy(false); }
  }
  const canLeave = useUnsavedChanges({ dirty: JSON.stringify(form) !== JSON.stringify(savedValue), busy, save, discard: onDiscard });
  async function close() { if (await canLeave()) onClose(); }
  function displayedTable() {
    if (form.selection.mode === 'manual') return form.selection.table;
    const ref = form.selection.ref;
    const item = catalogItems.find(row => row.source === ref.source && row.id === ref.id);
    if (item) {
      if (item.needs_refresh) return undefined;
      const offering = item.offerings.find(row => row.key === ref.offering_key);
      const quote = offering?.quotes.find(row => row.key === ref.quote_key);
      if (form.channel_id === 0 && !['original', 'official', 'original_vendor'].includes(quote?.source_kind || offering?.source_kind || '')) return undefined;
      return quote?.table;
    }
    return value.selection.mode === 'catalog' && JSON.stringify(ref) === JSON.stringify(value.selection.ref) ? resolvedTable : undefined;
  }
  const table = displayedTable();
  return <Modal title={`高级价格 · ${value.display_name || value.model || '新模型'}`} description={value.channel_id === 0 ? '官方 API 价' : '渠道价'} onClose={close} busy={busy} wide>
    <div className="space-y-5">
      {error && <ErrorBox message={error} />}
      <fieldset disabled={busy} className="space-y-4">
        <p className="text-sm text-[var(--color-text-secondary)]">{form.selection.mode === 'catalog' ? '跟随资料库更新；修改下方价格或条件后独立保存。' : '按填写的价格计费。空白表示未知，0 表示免费。'}</p>
        {table ? <PriceTableEditor value={table} rate={rate} onChange={change} /> : <ErrorBox message="关联报价暂不可用，请在下方重新匹配资料来源。" />}
        <details className="price-source-details">
          <summary>更换资料来源</summary>
          <div className="pt-3"><PrefillControls targets={[{ kind: form.channel_id === 0 ? 'downstream' : 'upstream', channel_id: form.channel_id || undefined, model: form.model, price_draft: form.selection, ...(form.selection.mode === 'catalog' ? { ref: { source: form.selection.ref.source, id: form.selection.ref.id, offering_key: form.selection.ref.offering_key }, price_ref: form.selection.ref } : {}) }]} allowedGroups={['price']} onDraft={patch => { if (patch.price) { const next = { ...form, selection: patch.price.selection }; setForm(next); onDraft(next); } }} /></div>
        </details>
      </fieldset>
      <div className="modal-footer"><Button variant="secondary" disabled={busy} onClick={onDiscard}>放弃草稿</Button><Button disabled={busy || !!conditionError} onClick={() => void save()}>{busy ? '保存中…' : '应用并保存'}</Button></div>
    </div>
  </Modal>;
}
