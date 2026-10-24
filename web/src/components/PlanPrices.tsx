import { useEffect, useRef, useState } from 'react';
import { Save, Settings2, Trash2 } from 'lucide-react';
import { api, errorMessage, validExchangeRate, type Channel, type Currency, type ExchangeRate } from '../api';
import type { PlanCharge, PlanPriceConfig, PlanPricesResponse, PriceChannelOption } from '../types/planPricing';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { useConfirm } from './Confirm';
import { ProviderIcon } from './ProviderIcons';
import { Button, Empty, ErrorBox, Input, Loading, Modal } from './ui';

export function localMonth(date = new Date()) { return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}`; }
function localDateTime(value: string) { const date = new Date(value); if (!Number.isFinite(date.getTime())) return ''; return `${localMonth(date)}-${String(date.getDate()).padStart(2, '0')}T${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`; }
function blankConfig(id: number, currency: Currency): PlanPriceConfig { return { channel_id: id, default_amount: null, currency, first_payment_at: new Date().toISOString(), timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC' }; }
const keyOf = (id: number, month: string) => `${id}/${month}`;
const amountOf = (raw: string) => raw.trim() === '' ? null : /^(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$/.test(raw) ? Number(raw) : NaN;
const conciseAmount = (amount: number) => Number(amount.toPrecision(14));

function PlanMoneyInput({ label, amount, source, currency, rate, rateLoading, disabled, onChange }: { label: string; amount: number | null; source: Currency; currency: Currency; rate: ExchangeRate | null; rateLoading: boolean; disabled: boolean; onChange: (amount: number | null) => void }) {
  const multiplier = source === currency ? 1 : validExchangeRate(rate) ? source === 'USD' ? rate.rate : 1 / rate.rate : null;
  const [edit, setEdit] = useState<{ raw: string; currency: Currency; multiplier: number } | null>(null);
  const active = edit?.currency === currency ? edit : null;
  const editingMultiplier = active?.multiplier ?? multiplier;
  const converted = amount == null || !Number.isFinite(amount) ? null : multiplier == null || multiplier === 1 ? amount : conciseAmount(amount * multiplier);
  const fullValue = converted == null ? '' : String(converted);
  const value = active ? active.raw : converted == null ? '' : multiplier == null || multiplier === 1 ? fullValue : String(Number(Math.abs(converted) >= 1e-6 ? converted.toFixed(6) : converted.toPrecision(6)));
  useEffect(() => { setEdit(null); }, [currency]);
  // Invalid text is kept visible, but reset/discard must still restore the saved amount.
  useEffect(() => { setEdit(current => current && Number.isNaN(amountOf(current.raw)) && (amount == null || Number.isFinite(amount)) ? null : current); }, [amount]);
  return <><div className="plan-money"><Input aria-label={label} type="text" inputMode="decimal" placeholder="未知" title={source !== currency && editingMultiplier != null && fullValue ? `${active?.raw ?? fullValue} ${currency}/月` : undefined} disabled={disabled || editingMultiplier == null} value={value}
    onFocus={() => { if (editingMultiplier != null) setEdit({ raw: active?.raw ?? fullValue, currency, multiplier: editingMultiplier }); }}
    onBlur={() => { if (!Number.isNaN(amountOf(value))) setEdit(null); }}
    onChange={event => {
      const snapshot = active || (multiplier == null ? null : { raw: value, currency, multiplier });
      if (!snapshot) return;
      const raw = event.target.value, parsed = amountOf(raw);
      setEdit({ ...snapshot, raw });
      onChange(parsed == null ? null : snapshot.multiplier === 1 ? parsed : conciseAmount(parsed / snapshot.multiplier));
    }} /><span>{editingMultiplier == null ? source : currency}/月</span></div>
    {source !== currency && <span className="table-sub">{editingMultiplier == null && !rateLoading ? `汇率不可用 · 原币 ${source} ${amount == null ? '未知' : Number.isFinite(amount) ? amount : '待填写'}，切回 ${source} 编辑` : `原币 ${source} ${amount == null ? '未知' : Number.isFinite(amount) ? amount : '待填写'}`}</span>}</>;
}

export function PlanPrices({ channels, selected, search, revision = 0, onHistoryChannels, currency = 'CNY', rate = null, rateLoading = false }: { channels: Channel[]; selected: number[]; search: string; revision?: number; onHistoryChannels?: (channels: PriceChannelOption[]) => void; currency?: Currency; rate?: ExchangeRate | null; rateLoading?: boolean }) {
  const [month, setMonth] = useState(localMonth), [data, setData] = useState<PlanPricesResponse | null>(null), [loading, setLoading] = useState(true), [error, setError] = useState(''), [busy, setBusy] = useState(false);
  const [configs, setConfigs] = useState<Record<number, PlanPriceConfig>>({}), [charges, setCharges] = useState<Record<string, PlanCharge>>({});
  const [editing, setEditing] = useState<number | null>(null), [retry, setRetry] = useState(0);
  const request = useRef(0), live = useRef(true), pending = useRef(false);
  const reportHistory = useRef(onHistoryChannels); reportHistory.current = onHistoryChannels;
  const confirm = useConfirm();
  useEffect(() => { live.current = true; return () => { live.current = false; request.current++; }; }, []);
  useEffect(() => {
    const version = ++request.current; setLoading(true); setError(''); setData(current => current?.month === month ? current : null);
    void api<PlanPricesResponse>(`/plan-prices?month=${month}`).then(result => { if (live.current && version === request.current) { if (!result || result.month !== month || !Array.isArray(result.prices) || !Array.isArray(result.charges) || !Array.isArray(result.scheduled)) throw new Error('套餐价格响应无效，请重试'); setData(result); reportHistory.current?.(result.charges.map(charge => ({ id: charge.channel_id, name: charge.channel_name || `渠道 ${charge.channel_id}` }))); } }).catch(cause => { if (live.current && version === request.current) setError(errorMessage(cause)); }).finally(() => { if (live.current && version === request.current) setLoading(false); });
  }, [month, revision, retry]);
  const dirty = !!Object.keys(configs).length || !!Object.keys(charges).length;
  async function save() {
    if (pending.current) return false;
    pending.current = true; setBusy(true); setError('');
    try {
      for (const config of Object.values(configs)) {
        if (config.default_amount != null && (!Number.isFinite(config.default_amount) || config.default_amount < 0)) throw new Error('月费金额必须为空或非负数');
        if (!Number.isFinite(new Date(config.first_payment_at).getTime())) throw new Error('请填写首次支付时间');
      }
      for (const charge of Object.values(charges)) {
        if (charge.amount != null && (!Number.isFinite(charge.amount) || charge.amount < 0)) throw new Error('本期金额必须为空或非负数');
        if (!Number.isFinite(new Date(charge.paid_at).getTime())) throw new Error('请填写本期支付时间');
      }
      const combined = new Set<string>();
      for (const [id, config] of Object.entries(configs)) {
        const charge = charges[keyOf(Number(id), month)] || Object.values(charges).find(value => value.channel_id === Number(id));
        await api(`/plan-prices/${id}`, { method: 'PUT', body: JSON.stringify({ ...config, ...(charge ? { charge: { month: charge.month, amount: charge.amount, currency: charge.currency, paid_at: charge.paid_at } } : {}) }) });
        if (!live.current) return false;
        setData(current => current ? { ...current, prices: [...current.prices.filter(value => value.channel_id !== config.channel_id), config], charges: charge && charge.month === current.month ? [...current.charges.filter(value => value.channel_id !== charge.channel_id), charge] : current.charges } : current);
        setConfigs(current => { const next = { ...current }; delete next[Number(id)]; return next; });
        if (charge) { const key = keyOf(charge.channel_id, charge.month); combined.add(key); setCharges(current => { const next = { ...current }; delete next[key]; return next; }); }
      }
      for (const [key, charge] of Object.entries(charges)) {
        if (combined.has(key)) continue;
        await api(`/plan-prices/${charge.channel_id}/charges/${charge.month}`, { method: 'PUT', body: JSON.stringify({ amount: charge.amount, currency: charge.currency, paid_at: charge.paid_at }) });
        if (!live.current) return false;
        setData(current => current && current.month === charge.month ? { ...current, charges: [...current.charges.filter(value => value.channel_id !== charge.channel_id), charge] } : current);
        setCharges(current => { const next = { ...current }; delete next[key]; return next; });
      }
      if (live.current) setRetry(value => value + 1);
      return true;
    } catch (cause) { if (live.current) setError(errorMessage(cause)); return false; }
    finally { pending.current = false; if (live.current) setBusy(false); }
  }
  useUnsavedChanges({ dirty, busy, save, discard: () => { setConfigs({}); setCharges({}); setEditing(null); } });
  const configured = (id: number) => configs[id] || data?.prices.find(config => config.channel_id === id);
  const actual = (id: number) => data?.charges.find(charge => charge.channel_id === id);
  function period(id: number): PlanCharge | undefined {
    if (charges[keyOf(id, month)]) return charges[keyOf(id, month)];
    if (actual(id)) return actual(id);
    const config = configs[id], savedConfig = data?.prices.find(value => value.channel_id === id);
    // A changed payment schedule needs the server's authoritative projection first.
    if (config && (!savedConfig || config.first_payment_at !== savedConfig.first_payment_at || config.timezone !== savedConfig.timezone)) return;
    const scheduled = data?.scheduled.find(charge => charge.channel_id === id && charge.month === month);
    return scheduled && config ? { ...scheduled, amount: config.default_amount, currency: config.currency } : scheduled;
  }
  const planChannels = channels.filter(channel => channel.tags?.includes('billing:plan'));
  const independentOnly = channels.filter(channel => !channel.tags?.includes('billing:plan') && Object.values(channel.model_tags || {}).some(tags => tags.includes('billing:plan')));
  const ids = [...new Set([...planChannels.map(channel => channel.id), ...(data?.charges || []).map(charge => charge.channel_id)])].filter(id => selected.includes(id));
  const visible = ids.filter(id => (channels.find(channel => channel.id === id)?.name || actual(id)?.channel_name || String(id)).toLowerCase().includes(search.toLowerCase()));
  function patchConfig(id: number, patch: Partial<PlanPriceConfig>) { setConfigs(current => ({ ...current, [id]: { ...(configured(id) || blankConfig(id, currency)), ...patch } })); }
  function patchPeriod(id: number, patch: Partial<PlanCharge>) { const current = period(id); if (current) setCharges(drafts => ({ ...drafts, [keyOf(id, month)]: { ...current, ...patch } })); }
  async function remove(id: number) {
    if (busy || !await confirm({ title: '停止自动记账？', description: '移除该渠道的自动月费配置，已有历史账单和套餐声明保留。', confirmLabel: '停止自动记账' })) return;
    setBusy(true); pending.current = true; setError('');
    try { await api(`/plan-prices/${id}`, { method: 'DELETE' }); if (live.current) { setConfigs(current => { const next = { ...current }; delete next[id]; return next; }); setRetry(value => value + 1); } }
    catch (cause) { if (live.current) setError(errorMessage(cause)); }
    finally { pending.current = false; if (live.current) setBusy(false); }
  }
  const editingConfig = editing == null ? undefined : configured(editing) || blankConfig(editing, currency);
  return <div className="plan-prices">
    <div className="price-selection-bar plan-selection-bar"><label>账单月份<Input type="month" aria-label="账单月份" value={month} disabled={busy} onChange={event => { if (/^\d{4}-\d{2}$/.test(event.target.value)) setMonth(event.target.value); }} /></label><span>{visible.length} 个渠道 · 每渠道一笔合计</span><Button disabled={busy || !dirty} onClick={() => void save()}><Save size={15} />{busy ? '保存中…' : '保存套餐价格'}</Button></div>
    {error && <div className="p-3"><ErrorBox message={error} retry={() => setRetry(value => value + 1)} /></div>}
    {!!independentOnly.length && <p className="plan-price-note">{independentOnly.map(channel => channel.name).join('、')}有独立套餐模型；请先通过“套餐声明”声明渠道并配置月费。</p>}
    {loading && !data ? <Loading /> : !data ? null : !visible.length ? <Empty title={!selected.length ? '未选择展示渠道' : search.trim() ? '没有匹配的套餐账单' : '暂无套餐账单'} description={!selected.length ? '点击“选择渠道”调整展示范围。' : search.trim() ? '调整名称或 ID 搜索条件。' : '通过套餐声明选择渠道，再设置月费与首次支付时间。'} /> : <div className="table-scroll"><table className="data-table plan-price-table"><thead><tr><th>渠道</th><th>默认月费</th><th>本期金额</th><th>支付日期</th><th>操作</th></tr></thead><tbody>{visible.map(id => {
      const channel = channels.find(channel => channel.id === id), config = configured(id), charge = period(id), recorded = actual(id), declared = !!channel?.tags?.includes('billing:plan');
      return <tr key={id}><td><div className="plan-channel-name"><ProviderIcon name={channel?.name || recorded?.channel_name} size={20} /><strong>{channel?.name || recorded?.channel_name || `渠道 ${id}`}</strong></div>{!declared && <span className="table-sub">历史账单 · {channel ? '已退出套餐' : '渠道已删除'}</span>}{declared && !channel?.enabled && <span className="table-sub">调用已停用 · 月费继续记账</span>}</td>
        <td>{declared ? <PlanMoneyInput label={`默认月费 ${id}`} amount={config?.default_amount ?? null} source={config?.currency || currency} currency={currency} rate={rate} rateLoading={rateLoading} disabled={busy} onChange={amount => patchConfig(id, { default_amount: amount })} /> : <span>—</span>}{declared && !config && <span className="table-sub">尚未设置自动月费</span>}</td>
        <td>{charge ? <><PlanMoneyInput key={keyOf(id, month)} label={`本期金额 ${id}`} amount={charge.amount} source={charge.currency} currency={currency} rate={rate} rateLoading={rateLoading} disabled={busy} onChange={amount => patchPeriod(id, { amount })} /><span className="table-sub">{recorded ? '已记录' : '计划金额'}{new Date(charge.paid_at) > new Date() ? ' · 未到支付时间' : ''}</span></> : <span className="table-sub">{configs[id] || !config ? '请先保存支付设置' : '本月无账单'}</span>}</td>
        <td>{charge ? <Input aria-label={`支付日期 ${id}`} type="datetime-local" disabled={busy} value={localDateTime(charge.paid_at)} onChange={event => { if (event.target.value) patchPeriod(id, { paid_at: new Date(event.target.value).toISOString() }); }} /> : '—'}</td>
        <td><div className="price-row-actions">{declared && <Button variant="ghost" aria-label={`支付设置 ${id}`} title="首次支付时间与时区" disabled={busy} onClick={() => setEditing(id)}><Settings2 size={17} /></Button>}{config && <Button variant="ghost" aria-label={`停止自动记账 ${id}`} title="停止自动记账，保留历史" disabled={busy} onClick={() => void remove(id)}><Trash2 size={16} /></Button>}</div></td></tr>;
    })}</tbody></table></div>}
    <p className="plan-price-note">金额按 {currency}/月展示，保存保留原币。支付时间按本机时区展示；月费到期计入支出，不分摊到模型或请求。</p><div className="price-status" role="status">{busy ? '保存中…' : dirty ? '有未保存的套餐修改' : '套餐价格已保存'}</div>
    {editing != null && editingConfig && <Modal title="支付设置" description={channels.find(channel => channel.id === editing)?.name} onClose={() => setEditing(null)}><div className="plan-payment-settings"><label>首次支付时间（本机时区）<Input aria-label="首次支付时间" type="datetime-local" disabled={busy} value={localDateTime(editingConfig.first_payment_at)} onChange={event => { if (event.target.value) patchConfig(editing, { first_payment_at: new Date(event.target.value).toISOString() }); }} /></label><p>每月按此日期记账，月底取当月最后一天。</p><p>记账时区：{editingConfig.timezone}</p></div><div className="modal-footer"><Button onClick={() => { if (!configured(editing)) patchConfig(editing, editingConfig); setEditing(null); }}>完成设置</Button></div></Modal>}
  </div>;
}
