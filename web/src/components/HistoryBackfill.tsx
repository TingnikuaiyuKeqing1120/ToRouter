import { Fragment, useEffect, useMemo, useRef, useState } from 'react';
import { CheckCircle2, ChevronDown, ChevronRight, Download, SlidersHorizontal } from 'lucide-react';
import { api, dateRange, errorMessage, number, type Channel, type Currency, type PriceTable } from '../api';
import type { BackfillFilter, BackfillGroup, BackfillMetric, BackfillPreview, BackfillRequest } from '../types/catalog-operations';
import { displayCatalogTable } from '../services/catalogPriceDisplay';
import { PriceConditionsReadOnly } from './PriceConditions';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { Button, DateFields, ErrorBox, Field, Input, Modal, Select } from './ui';
import './HistoryBackfill.css';

const reasons: Record<string, string> = {
  price_unconfigured: '未配置价格', usage_unknown: '用量未知', upstream_identity_missing: '实际上游身份缺失',
  catalog_quote_missing: '引用报价缺失', catalog_model_missing: '引用模型缺失',
  original_reference_required: '等价价值需要原厂参考价', catalog_needs_refresh: '来源待刷新', price_table_invalid: '价表无效',
  'rate_missing:input': '输入单价缺失', 'rate_missing:output': '输出单价缺失',
  'rate_missing:cache_read': '读取价格缺失', 'rate_missing:cache_write': '写入价格缺失',
};
export function pricingReason(value: string) { return reasons[value] || value; }

const fields = ['input', 'output', 'cache_write', 'cache_read'] as const;
const labels = ['输入价格', '输出价格', '写入价格', '读取价格'];
const metricLabels = { actual: '实际支出', official: '等价 API 价值' };
type ScopeFields = { from: string; to: string; channel: string; actual: string; official: string };
const emptyScope: ScopeFields = { from: '', to: '', channel: '', actual: '', official: '' };
type PriceDraft = { table: PriceTable; edited: boolean };
const emptyTable = (currency: Currency): PriceTable => ({ currency, unit_tokens: 1000000, rates: { input: null, output: null, cache_write: null, cache_read: null }, rules: [] });
const hasPrice = (table: PriceTable) => fields.some(field => table.rates[field] !== null) || table.rules.length > 0;
const collator = new Intl.Collator('zh-CN', { numeric: true, sensitivity: 'base' });
function time(value: string) { const date = new Date(value); return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN', { hour12: false }); }

function scopeFilter(scope: ScopeFields, metric: BackfillMetric): BackfillFilter {
  const dates = dateRange(scope.from, scope.to);
  return {
    from: dates.get('from') || undefined, to: dates.get('to') || undefined,
    channel_id: scope.channel ? Number(scope.channel) : undefined,
    ...(metric === 'actual' ? { upstream_model: scope.actual.trim() || undefined } : { model: scope.official.trim() || undefined }),
  };
}
function priceSource(group: BackfillGroup) {
  if (group.resolution.layer === 'manual') return '历史临时价';
  return group.metric === 'actual' ? '上游渠道价' : '官方 API 参考价';
}

export function HistoryBackfill({ channels, onClose, onApplied }: { channels: Channel[]; onClose: () => void; onApplied: () => void }) {
  const [metric, setMetric] = useState<BackfillMetric>('actual');
  const [scope, setScope] = useState<ScopeFields>(emptyScope);
  const [scopeDraft, setScopeDraft] = useState<ScopeFields>(emptyScope);
  const [scopeError, setScopeError] = useState('');
  const [refresh, setRefresh] = useState(0);
  const [loaded, setLoaded] = useState<{ key: string; value: BackfillPreview } | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState('');
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [drafts, setDrafts] = useState<Record<string, PriceDraft>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [historicalChannels, setHistoricalChannels] = useState<Record<number, string>>({});
  const filter = useMemo(() => scopeFilter(scope, metric), [scope, metric]);
  const scopeKey = JSON.stringify({ filter, metric });
  // Both views share one read snapshot; switching a view never clears the list.
  const readRequest = useMemo<BackfillRequest>(() => ({
    filter: { from: filter.from, to: filter.to, channel_id: filter.channel_id },
    amounts: ['actual', 'official'], mode: 'current',
  }), [filter.from, filter.to, filter.channel_id]);
  const readKey = JSON.stringify(readRequest.filter);
  const data = loaded?.key === readKey ? loaded.value : null;
  const pendingScope = scopeDraft.from !== scope.from || scopeDraft.to !== scope.to || scopeDraft.channel !== scope.channel || scopeDraft[metric].trim() !== scope[metric].trim();

  useEffect(() => { setNotice(''); }, [scopeKey]);
  useEffect(() => {
    let cancelled = false;
    setLoading(true); setError('');
    api<BackfillPreview>('/usage/backfill/preview', { method: 'POST', body: JSON.stringify(readRequest) })
      .then(value => {
        if (cancelled) return;
        setLoaded({ key: readKey, value });
        setHistoricalChannels(old => ({ ...old, ...Object.fromEntries(value.groups.filter(group => group.channel_id > 0).map(group => [group.channel_id, group.channel_name])) }));
      })
      .catch(e => { if (!cancelled) setError(`未能加载缺口：${errorMessage(e)}`); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [readRequest, readKey, refresh]);

  const channelName = (id: number, historicalName = '') => historicalName || channels.find(channel => channel.id === id)?.name || (id > 0 ? `历史渠道 ${id}` : '渠道未记录');
  const channelOptions = [...new Set([...channels.map(channel => channel.id), ...Object.keys(historicalChannels).map(Number), ...(scopeDraft.channel ? [Number(scopeDraft.channel)] : [])])]
    .map(id => ({ id, name: channelName(id, historicalChannels[id]) }))
    .sort((a, b) => collator.compare(a.name, b.name) || a.id - b.id);
  const inView = (group: BackfillGroup) => group.metric === metric && (!scope[metric] || group.model === scope[metric]);
  const groups = (data?.groups || []).filter(inView).sort((a, b) => (metric === 'actual' ? collator.compare(channelName(a.channel_id, a.channel_name), channelName(b.channel_id, b.channel_name)) : 0)
    || collator.compare(a.model || a.public_models?.join('、') || '', b.model || b.public_models?.join('、') || '') || a.channel_id - b.channel_id || a.key.localeCompare(b.key));
  const missing = groups.reduce((total, group) => total + group.records, 0);
  const calculable = groups.reduce((total, group) => total + group.calculable_amounts, 0);
  const filled = groups.filter(group => drafts[group.key] && hasPrice(drafts[group.key].table));
  const invalidPrices = filled.some(group => fields.some(field => { const value = drafts[group.key].table.rates[field]; return value !== null && (!Number.isFinite(value) || value < 0); }));
  const available = !!data && !loading && !busy && !pendingScope && !error;
  const dirty = Object.values(drafts).some(draft => draft.edited);

  const canLeave = useUnsavedChanges({ dirty, busy: !!busy, save: apply, prompt: {
    title: '应用本次填写？', description: '将按当前范围使用已填写的价格补算历史缺失金额。价格配置不会改变；未完成的填写会继续保留。', confirmLabel: '放弃填写', saveLabel: '确认应用',
  } });

  function rowTable(group: BackfillGroup) { return drafts[group.key]?.table || emptyTable(group.resolution.table?.currency || 'CNY'); }
  function editPrice(group: BackfillGroup, update: (table: PriceTable) => PriceTable) {
    setDrafts(old => ({ ...old, [group.key]: { table: update(old[group.key]?.table || emptyTable(group.resolution.table?.currency || 'CNY')), edited: true } }));
    setNotice('');
  }
  function applyScope() {
    if (scopeDraft.from && scopeDraft.to && scopeDraft.from > scopeDraft.to) { setScopeError('开始日期不能晚于结束日期'); return; }
    setScopeError(''); setScope({ ...scopeDraft, actual: scopeDraft.actual.trim(), official: scopeDraft.official.trim() });
  }

  async function prefill() {
    if (!available || busyRef.current) return;
    busyRef.current = true; setBusy('prefill'); setError(''); setNotice('');
    try {
      const fresh = await api<BackfillPreview>('/usage/backfill/preview', { method: 'POST', body: JSON.stringify(readRequest) });
      setLoaded({ key: readKey, value: fresh });
      const quoted = fresh.groups.filter(group => inView(group) && group.resolution.table && !drafts[group.key]?.edited);
      setDrafts(old => {
        const next = { ...old };
        for (const group of quoted) {
          const table = group.resolution.table!;
          next[group.key] = { table: displayCatalogTable(table, table.currency, null).table, edited: false };
        }
        return next;
      });
      setNotice(`已预填 ${number(quoted.length)} 项当前价格，手填内容已保留。`);
    } catch (e) { setError(`预填失败：${errorMessage(e)}`); }
    finally { busyRef.current = false; setBusy(''); }
  }

  async function apply() {
    if (!available || busyRef.current || !filled.length || invalidPrices) { setError('请检查当前范围与已填写价格后再应用。'); return false; }
    busyRef.current = true; setBusy('apply'); setError(''); setNotice('');
    try {
      const request: BackfillRequest = { filter, amounts: [metric], mode: 'manual', prices: filled.map(group => ({ key: group.key, table: drafts[group.key].table })) };
      const preview = await api<BackfillPreview>('/usage/backfill/preview', { method: 'POST', body: JSON.stringify(request) });
      if (!preview.calculable_amounts) {
        const detail = Object.keys(preview.missing_reasons).map(pricingReason).join('、');
        setNotice(`当前没有可补算的记录${detail ? `：${detail}` : ''}。`);
        setRefresh(value => value + 1);
        return false;
      }
      const result = await api<BackfillPreview>('/usage/backfill/apply', { method: 'POST', body: JSON.stringify({ ...preview.request, expected_resolution: preview.expected_resolution }) });
      setNotice(`已补算 ${number(result.applied_amounts)} 条${metricLabels[metric]}。`);
      const appliedKeys = new Set(result.groups.filter(group => group.calculable_amounts > 0 && group.calculable_amounts === group.records).map(group => group.key));
      setDrafts(old => Object.fromEntries(Object.entries(old).filter(([key]) => !appliedKeys.has(key))));
      onApplied(); setRefresh(value => value + 1);
      const remaining = Object.entries(drafts).some(([key, draft]) => draft.edited && !appliedKeys.has(key));
      if (remaining) setNotice('当前范围已应用，仍有其他未完成的填写；请处理后再关闭，或选择放弃填写。');
      return !remaining;
    } catch (e) {
      const message = errorMessage(e);
      setError(message.includes('configuration or source changed') ? '价格或记录已变化，请刷新缺口后重试。填写内容已保留。' : `补算未完成：${message}`);
      return false;
    } finally { busyRef.current = false; setBusy(''); }
  }

  const scopeSummary = [scope.from || scope.to ? `${scope.from || '最早'} — ${scope.to || '至今'}` : '全部时间', scope.channel ? channelName(Number(scope.channel), historicalChannels[Number(scope.channel)]) : '全部渠道', scope[metric] || '全部模型'].join(' · ');
  const columns = metric === 'actual' ? 7 : 6;

  return <Modal title="补算未计价" description="补齐历史缺失金额，已有金额保持不变。" busy={!!busy} onClose={() => void canLeave().then(allowed => { if (allowed) onClose(); })} className="backfill-modal" wide>
    <div className="backfill-panel">
      <div className="backfill-toolbar">
        <div className="backfill-segmented" role="group" aria-label="补算金额类型">
          {(['actual', 'official'] as const).map(value => <button key={value} type="button" aria-pressed={metric === value} disabled={!!busy} onClick={() => { setMetric(value); setScopeError(''); }}>{metricLabels[value]}</button>)}
        </div>
        <Button variant="secondary" disabled={!available || !groups.length} onClick={() => void prefill()}><Download size={15} />{busy === 'prefill' ? '正在预填…' : '预填当前价格'}</Button>
      </div>
      <div className="backfill-scope-line"><span>{scopeSummary}</span>{pendingScope && <strong>筛选待应用</strong>}</div>
      <div className="backfill-scroll" aria-busy={loading}>
      {error && <ErrorBox message={error} retry={() => setRefresh(value => value + 1)} />}
      {notice && <p className="backfill-notice" role="status"><CheckCircle2 size={16} />{notice}</p>}
      <div className="backfill-list-heading"><h3>未计价缺口 <span>{data ? `${number(missing)} 条` : '—'}</span></h3><span role={loading ? 'status' : undefined}>{loading ? '正在检查未计价记录…' : data ? `当前价格可补 ${number(calculable)} 条` : ''}</span></div>
      {data && <>
        {groups.length ? <div className="backfill-table-scroll"><table className="backfill-table" aria-label="未计价缺口">
          <thead><tr>{metric === 'actual' && <th>渠道</th>}<th>{metric === 'actual' ? '上游模型 ID' : '公开模型 ID'}</th>{labels.map(label => <th key={label}>{label}</th>)}<th>详情</th></tr></thead>
          <tbody>{groups.map(group => {
            const table = rowTable(group), open = expanded.has(group.key), name = group.model || (group.public_models?.length ? `上游未记录 · ${group.public_models.join('、')}` : '模型 ID 未记录');
            const rowName = metric === 'actual' ? `${channelName(group.channel_id, group.channel_name)} · ${name}` : name;
            return <Fragment key={group.key}><tr>
              {metric === 'actual' && <td><span className="backfill-channel">{channelName(group.channel_id, group.channel_name)}</span><small>#{group.channel_id || '—'}</small></td>}
              <td className="backfill-model"><span title={name}>{name}</span><div className="backfill-price-unit"><select aria-label={`${rowName} 币种`} title="填写币种，不换算数值" disabled={!!busy} value={table.currency} onChange={e => editPrice(group, value => ({ ...value, currency: e.target.value as Currency }))}><option value="CNY">CNY</option><option value="USD">USD</option></select><small>/ 百万 Token</small></div></td>
              {fields.map((field, index) => <td key={field}><Input aria-label={`${rowName} ${labels[index]}`} type="number" min={0} step="any" placeholder="未填" disabled={!!busy} value={table.rates[field] ?? ''} onChange={e => editPrice(group, value => ({ ...value, rates: { ...value.rates, [field]: e.target.value === '' ? null : Number(e.target.value) } }))} /></td>)}
              <td><button type="button" className="backfill-detail-button" aria-label={`${rowName} 详情`} aria-expanded={open} onClick={() => setExpanded(old => { const next = new Set(old); next.has(group.key) ? next.delete(group.key) : next.add(group.key); return next; })}>{open ? <ChevronDown size={18} /> : <ChevronRight size={18} />}</button></td>
            </tr>{open && <tr className="backfill-detail"><td colSpan={columns}>
              <dl><div><dt>待计价请求</dt><dd>{number(group.records)} 条</dd></div><div><dt>当前可补算</dt><dd>{number(group.calculable_amounts)} 条</dd></div><div><dt>当前价格来源</dt><dd>{priceSource(group)}</dd></div>
                <div><dt>请求时间</dt><dd>{time(group.first_time)} — {time(group.last_time)}</dd></div>
                {!!group.public_models?.length && <div><dt>公开模型</dt><dd>{group.public_models.join('、')}</dd></div>}
                <div><dt>未计价原因</dt><dd>{Object.entries(group.missing_reasons).map(([reason, count]) => `${pricingReason(reason)}（${number(count)} 条）`).join('；') || '当前价格已可计算'}</dd></div>
              </dl>
              {(drafts[group.key]?.table || group.resolution.table) && <PriceConditionsReadOnly value={drafts[group.key]?.table || group.resolution.table!} />}
            </td></tr>}</Fragment>;
          })}</tbody>
        </table></div> : <div className="backfill-empty"><CheckCircle2 size={24} /><p>当前范围没有未计价记录</p></div>}
      </>}
      {invalidPrices && <p className="backfill-validation" role="alert">价格需为大于或等于 0 的有效数字。</p>}
      <details className="backfill-range"><summary><SlidersHorizontal size={15} />调整范围<span>按需筛选</span><ChevronDown size={15} /></summary>
        <fieldset disabled={!!busy}>
          <div className="form-grid"><DateFields from={scopeDraft.from} to={scopeDraft.to} setFrom={from => setScopeDraft(old => ({ ...old, from }))} setTo={to => setScopeDraft(old => ({ ...old, to }))} />
            <Field label="渠道"><Select aria-label="补算渠道范围" value={scopeDraft.channel} onChange={e => setScopeDraft(old => ({ ...old, channel: e.target.value }))}><option value="">全部渠道</option>{channelOptions.map(channel => <option key={channel.id} value={channel.id}>{channel.name} · #{channel.id}</option>)}</Select></Field>
            <Field label={metric === 'actual' ? '上游模型 ID' : '公开模型 ID'}><Input aria-label="补算模型范围" placeholder="全部模型" value={scopeDraft[metric]} onChange={e => setScopeDraft(old => ({ ...old, [metric]: e.target.value }))} /></Field>
          </div>{scopeError && <p role="alert" className="backfill-validation">{scopeError}</p>}
          <div className="backfill-range-actions"><Button variant="secondary" onClick={applyScope}>应用范围</Button><Button variant="ghost" onClick={() => { setScope(emptyScope); setScopeDraft(emptyScope); setScopeError(''); }}>重置范围</Button></div>
        </fieldset>
      </details>
      </div>
    </div>
    <div className="backfill-footer"><span>{filled.length ? `已填写 ${number(filled.length)} 项 · 仅补${metricLabels[metric]}` : '预填或填写价格后应用'}</span><div>
      <Button disabled={!available || !filled.length || invalidPrices} onClick={() => void apply()}>{busy === 'apply' ? '正在应用…' : '应用'}</Button>
    </div></div>
  </Modal>;
}
