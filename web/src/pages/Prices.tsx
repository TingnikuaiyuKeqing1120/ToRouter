import { useEffect, useMemo, useRef, useState } from 'react';
import { Download, GripVertical, Loader2, MoreHorizontal, Plus, Save, Search, Sparkles, Trash2, X } from 'lucide-react';
import { api, errorMessage, type Channel, type Currency, type DownstreamModelView, type ExchangeRate, type Price, type CatalogItem, type CatalogPriceRef, type PrefillPreview, type PrefillTarget, type TagGroupWithTags } from '../api';
import { managementTarget } from '../services/managementLinks';
import { normalizeLegacyPriceRows } from '../services/priceDraftMigration';
import { enabledChannelModels } from '../services/modelCandidates';
import { ChannelPicker } from '../components/ChannelPicker';
import { PlanDeclaration } from '../components/PlanDeclaration';
import { PlanPrices } from '../components/PlanPrices';
import { groupTagIDs } from '../services/tagAssignment';
import type { PriceChannelOption } from '../types/planPricing';
import { ExchangeRatePanel } from '../components/ExchangeRatePanel';
import { PriceAdvanced, convertTable } from '../components/PriceAdvanced';
import { blankPrice, configured, priceFields, usePriceWorkbench, type PriceRow } from '../hooks/usePriceWorkbench';
import { useResource } from '../hooks';
import { useSortableList } from '../hooks/useSortableList';
import { useConfirm } from '../components/Confirm';
import { PriceCatalogPicker, priceCatalogOptions, usablePriceTable } from '../components/PriceCatalogPicker';
import { Button, Empty, ErrorBox, Input, Loading, PageHeader, Select } from '../components/ui';
import './Prices.css';

const selectionKey = 'to-router.price-channels.v1', advancedKey = 'to-router.price-advanced.drafts.v2';
function selectedChannels(): number[] | null { try { const value = JSON.parse(localStorage.getItem(selectionKey) || 'null'); return Array.isArray(value) ? value.filter(id => Number.isInteger(id) && id > 0) : null; } catch { return null; } }
function identity(row: Pick<Price, 'channel_id' | 'model'>) { return `${row.channel_id}|${row.model.trim()}`; }
type AdvancedDraft = { key: number; price: Price };
function advancedDrafts(): Record<string, AdvancedDraft> {
  try {
    const raw = localStorage.getItem(advancedKey) || '{}', drafts: Record<string, AdvancedDraft> = JSON.parse(raw);
    if (!Object.values(drafts).some(draft => draft?.price && Object.prototype.hasOwnProperty.call(draft.price, 'role'))) return drafts;
    if (!localStorage.getItem('to-router.price-advanced.drafts.pre-official.v2')) localStorage.setItem('to-router.price-advanced.drafts.pre-official.v2', raw);
    const rows = normalizeLegacyPriceRows(Object.values(drafts).map(draft => ({ ...draft.price, key: draft.key })));
    const next = Object.fromEntries(rows.map(({ key, ...price }) => [draftIdentity(price, key), { key, price }]));
    writeAdvancedDrafts(next); return next;
  } catch { return {}; }
}
function draftIdentity(row: Pick<Price, 'channel_id' | 'model'>, key: number) { return row.model.trim() ? identity(row) : `draft:${key}`; }
function writeAdvancedDrafts(drafts: Record<string, AdvancedDraft>) { try { if (Object.keys(drafts).length) localStorage.setItem(advancedKey, JSON.stringify(drafts)); else localStorage.removeItem(advancedKey); return true; } catch { return false; } }
const tableFields = ['input', 'output', 'cache_write', 'cache_read'] as const;
const fieldLabels = ['输入', '输出', '缓存写入', '缓存命中'];
const sameRef = (a: CatalogPriceRef, b: CatalogPriceRef) => a.source === b.source && a.id === b.id && a.offering_key === b.offering_key && a.quote_key === b.quote_key;
const numberPerMillion = (value: number | null | undefined, unit: number) => value == null ? '' : Number((value * 1000000 / unit).toPrecision(14));
export default function Prices() {
  const work = usePriceWorkbench();
  const confirm = useConfirm();
  const [target] = useState(managementTarget);
  const [initialPlan] = useState(() => new URLSearchParams(window.location.hash.split('?')[1] || '').get('view') === 'plan');
  const catalog = useResource<CatalogItem[]>('/catalog/models?source=all');
  const groups = useResource<TagGroupWithTags[]>('/tag-groups');
  const channels = useResource<Channel[]>('/channels'), rate = useResource<ExchangeRate>('/exchange-rate');
  const models = useResource<DownstreamModelView[]>('/models');
  const [showAll, setShowAll] = useState(!!target.model);
  const [edited, setEdited] = useState<Set<number>>(() => new Set());
  const [view, setView] = useState<'official' | 'channel' | 'plan'>(initialPlan ? 'plan' : target.channelID != null && target.channelID > 0 ? 'channel' : 'official'), [currency, setCurrency] = useState<Currency>('CNY'), [searches, setSearches] = useState({ official: target.channelID ? '' : target.model, channel: target.channelID ? target.model : '', plan: '' });
  const search = searches[view], setSearch = (value: string) => setSearches(current => ({ ...current, [view]: value }));
  const [picker, setPicker] = useState(false), [declaration, setDeclaration] = useState(false), [planVisited, setPlanVisited] = useState(initialPlan), [planRevision, setPlanRevision] = useState(0);
  const [selected, setSelected] = useState<number[]>(() => target.channelID != null && target.channelID > 0 ? [target.channelID] : selectedChannels() || []), [pulling, setPulling] = useState(false);
  const [planSelected, setPlanSelected] = useState<number[] | null>(initialPlan && target.channelID != null && target.channelID > 0 ? [target.channelID] : null);
  const [historyChannels, setHistoryChannels] = useState<Record<number, string>>({});
  const channelOptions: PriceChannelOption[] = (channels.data || []).map(channel => ({ id: channel.id, name: channel.name, base_url: channel.base_url || channel.endpoints?.[0]?.base_url }));
  const planOptions: PriceChannelOption[] = [...channelOptions, ...Object.entries(historyChannels).filter(([id]) => !channelOptions.some(channel => channel.id === Number(id))).map(([id, name]) => ({ id: Number(id), name, deleted: true }))];
  const planScope = planSelected ?? planOptions.map(channel => channel.id);
  const shownChannels = view === 'plan' ? planScope : selected;
  const pickerOptions = view === 'plan' ? planOptions : channelOptions;
  function reportHistory(found: PriceChannelOption[]) {
    setHistoryChannels(current => {
      if (found.every(channel => current[channel.id] === channel.name)) return current;
      return { ...current, ...Object.fromEntries(found.map(channel => [channel.id, channel.name])) };
    });
  }
  const seeded = useRef(target.channelID != null || selectedChannels() !== null);
  const [advanced, setAdvanced] = useState<{ key: number; price: Price; savedValue: Price } | null>(null);
  const [checked, setChecked] = useState<Set<number>>(() => new Set()), [matching, setMatching] = useState(false), [matchResult, setMatchResult] = useState<string[]>([]);
  const alive = useRef(true), latestRows = useRef(work.rows); latestRows.current = work.rows;
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const catalogOptions = useMemo(() => priceCatalogOptions(catalog.data || [], view === 'official'), [catalog.data, view]);
  useEffect(() => { if (channels.data && !seeded.current) { seeded.current = true; setSelected(channels.data.map(row => row.id)); } }, [channels.data]);
  useEffect(() => { if (seeded.current) try { localStorage.setItem(selectionKey, JSON.stringify(selected)); } catch { /* selection remains usable */ } }, [selected]);
  function patch(key: number, change: Partial<Price>) {
    setEdited(keys => new Set(keys).add(key));
    work.update(rows => rows.map(row => {
      if (row.key !== key) return row;
      const next = { ...row, ...change }, drafts = advancedDrafts(), oldIdentity = draftIdentity(row, key), nextIdentity = draftIdentity(next, key);
      if (drafts[oldIdentity] && (oldIdentity !== nextIdentity || change.display_name !== undefined)) {
        const draft = drafts[oldIdentity]; delete drafts[oldIdentity];
        drafts[nextIdentity] = { ...draft, price: { ...draft.price, channel_id: next.channel_id, model: next.model, display_name: next.display_name } };
        writeAdvancedDrafts(drafts);
      }
      return next;
    }));
  }
  function openAdvanced(row: PriceRow) {
    const draft = advancedDrafts()[draftIdentity(row, row.key)];
    setAdvanced({ key: row.key, price: draft?.price || row, savedValue: row });
  }
  function draftAdvanced(price: Price) { if (!advanced) return; writeAdvancedDrafts({ ...advancedDrafts(), [draftIdentity(advanced.price, advanced.key)]: { key: advanced.key, price } }); }
  function discardAdvanced() { if (advanced) { const drafts = advancedDrafts(); delete drafts[draftIdentity(advanced.price, advanced.key)]; writeAdvancedDrafts(drafts); } setAdvanced(null); }
  async function applyAdvanced(price: Price) {
    if (!advanced) return;
    if (!price.model.trim()) throw new Error('请先填写模型 ID，再保存高级价格。');
    const original = work.rows.find(row => row.key === advanced.key);
    patch(advanced.key, price);
    try { await work.flush(); discardAdvanced(); }
    catch (error) {
      // Failed advanced saves stay in their own draft; discarding them must not
      // leave the failed value in the main table's automatic save queue.
      if (original) work.update(rows => rows.map(row => row.key === advanced.key ? original : row));
      throw error;
    }
  }
  function tab(next: typeof view) { if (next === view) return; setView(next); if (next === 'plan') setPlanVisited(true); }
  function add() {
    const channel_id = view === 'official' ? 0 : selected.find(id => channels.data?.some(channel => channel.id === id)) || channels.data?.[0]?.id;
    if (channel_id == null) { work.setError('请选择渠道'); return; }
    if (channel_id && !selected.includes(channel_id)) setSelected(ids => [...ids, channel_id]);
    const key = work.nextKey();
    setEdited(keys => new Set(keys).add(key));
    work.update(rows => [...rows, { ...blankPrice(channel_id, currency, key), ...(target.model ? { model: target.model, display_name: target.model } : {}) }]);
  }
  async function pull() {
    const version = scopeVersion.current.version;
    setPulling(true); work.setError('');
    try {
      const current = view === 'official' ? await api<DownstreamModelView[]>('/models') : await api<Channel[]>('/channels');
      if (!alive.current || version !== scopeVersion.current.version) return;
      if (view === 'official') models.setData(current as DownstreamModelView[]); else channels.setData(current as Channel[]);
      const catalogue: { channel_id: number; model: string; display_name: string }[] = view === 'official'
        ? (current as DownstreamModelView[]).filter(model => model.enabled).map(model => ({ channel_id: 0, model: model.id, display_name: model.display_name || model.id }))
        : (current as Channel[]).filter(channel => selected.includes(channel.id)).flatMap(channel => enabledChannelModels(channel).filter(model => !isPlan(channel, model)).map(model => ({ channel_id: channel.id, model, display_name: model })));
      const incoming = new Map(catalogue.map(row => [identity(row), row]));
      const existing = new Set(view === 'official'
        ? (current as DownstreamModelView[]).map(model => `0|${model.id}`)
        : (current as Channel[]).flatMap(channel => [...channel.models, ...(channel.available_models || [])].map(model => `${channel.id}|${model}`)));
      work.update(rows => {
        // Only discard generated empty rows whose source has actually gone.
        // Disabled models, configured/manual rows and unselected channels remain.
        const next = rows.filter(row => !(row.origin === 'auto' && (view === 'official' ? row.channel_id === 0 : selected.includes(row.channel_id)) && !existing.has(`${row.channel_id}|${row.model}`) && !configured(row)));
        const present = new Set(next.map(identity));
        const merged = next.map(row => row.origin === 'auto' && incoming.has(identity(row)) ? { ...row, display_name: incoming.get(identity(row))!.display_name } : row);
        for (const entry of catalogue) if (!present.has(identity(entry))) { merged.push({ ...blankPrice(entry.channel_id, currency, work.nextKey(), 'auto'), ...entry }); present.add(identity(entry)); }
        return merged;
      });
    } catch (e) { if (alive.current && version === scopeVersion.current.version) work.setError(errorMessage(e)); } finally { if (alive.current) setPulling(false); }
  }
  function sourceTable(row: PriceRow) {
    if (row.selection.mode === 'manual') return row.selection.table;
    const ref = row.selection.ref;
    const option = catalogOptions.find(option => sameRef(option.ref, ref));
    return option && !option.disabled ? option.quote.table : undefined;
  }
  function displayed(row: PriceRow) {
    const table = sourceTable(row);
    if (!usablePriceTable(table)) return { table: undefined, editable: false, converted: false };
    try { return { table: convertTable(table, currency, rate.data), editable: true, converted: true }; } catch { return { table, editable: false, converted: false }; }
  }
  function amount(row: PriceRow, field: typeof priceFields[number], raw: string) {
    try {
      const source = sourceTable(row); if (!usablePriceTable(source)) return;
      const table = convertTable(source, currency, rate.data);
      const value = raw === '' ? null : Number(raw) * table.unit_tokens / 1000000;
      const price: Price = { ...row, selection: { mode: 'manual', table: { ...table, rates: { ...table.rates, [field]: value } } } };
      const id = draftIdentity(row, row.key), drafts = advancedDrafts(), draft = drafts[id];
      if (draft) {
        const draftTable = sourceTable({ ...draft.price, key: row.key });
        if (usablePriceTable(draftTable)) {
          const converted = convertTable(draftTable, currency, rate.data);
          drafts[id] = { ...draft, price: { ...draft.price, selection: { mode: 'manual', table: { ...converted, rates: { ...converted.rates, [field]: raw === '' ? null : Number(raw) * converted.unit_tokens / 1000000 } } } } };
        } else {
          // A stale reference has no independent editable table to retain. The
          // user's direct edit supersedes it rather than letting it shadow this price.
          delete drafts[id];
        }
        if (!writeAdvancedDrafts(drafts)) throw new Error('无法同步高级草稿，当前价格未改动。请先处理高级草稿后重试。');
      }
      // Both conversions and draft persistence must succeed before changing the
      // main table; otherwise an older advanced draft could restore another price.
      patch(row.key, price);
      work.setError('');
    } catch (e) { work.setError(errorMessage(e)); }
  }
  const enabled = new Set([
    ...(models.data || []).filter(model => model.enabled).map(model => `0|${model.id}`),
    ...(channels.data || []).flatMap(channel => enabledChannelModels(channel).map(model => `${channel.id}|${model}`)),
  ]);
  const isEnabled = (row: PriceRow) => enabled.has(`${row.channel_id}|${row.model}`);
  const candidatesLoading = view === 'official' ? models.loading : channels.loading;
  const candidatesError = view === 'official' ? models.error : channels.error;
  function isPlan(channel: Channel, model: string) {
    const billing = groups.data?.find(group => group.id === 'billing');
    const ids = billing ? groupTagIDs(billing) : new Set(['billing:free', 'billing:plan', 'billing:payg', 'billing:unknown']);
    const own = (channel.model_tags?.[model] || []).filter(tag => ids.has(tag));
    return (own.length ? own : channel.tags || []).includes('billing:plan');
  }
  const visible = work.rows.filter(row => (view === 'official' ? row.channel_id === 0 : view === 'channel' && row.channel_id > 0 && selected.includes(row.channel_id) && !channels.data?.some(channel => channel.id === row.channel_id && isPlan(channel, row.model))) && (showAll || edited.has(row.key) || isEnabled(row)) && `${row.model} ${row.display_name} ${channels.data?.find(channel => channel.id === row.channel_id)?.name || ''}`.toLowerCase().includes(search.toLowerCase()));
  const missingFx = visible.some(row => sourceTable(row) && !displayed(row).editable);
  const visibleChecked = visible.filter(row => checked.has(row.key));
  const scope = JSON.stringify([view, search, selected, showAll, visible.map(row => row.key)]);
  const scopeVersion = useRef({ scope, version: 0 });
  if (scopeVersion.current.scope !== scope) scopeVersion.current = { scope, version: scopeVersion.current.version + 1 };
  useEffect(() => { setMatchResult([]); }, [scope]);
  async function matchRows(rows: PriceRow[], ref?: CatalogPriceRef) {
    if (matching || !rows.length) return;
    setMatching(true); work.setError('');
    const version = scopeVersion.current.version;
    const snapshots = new Map(rows.map(row => [row.key, JSON.stringify(row)]));
    try {
      const draftsBefore = advancedDrafts();
      if (rows.some(row => configured(row) || !!draftsBefore[draftIdentity(row, row.key)]) && !await confirm({ title: '替换所选价格？', description: `将替换所选 ${rows.length} 项中的已有价格及高级草稿，匹配后跟随资料库更新。未能精确匹配的项目保持原值。`, confirmLabel: '确认匹配' })) return;
      if (!alive.current || version !== scopeVersion.current.version) return;
      const results: string[] = [], identities = new Map<string, number>();
      rows.forEach(row => identities.set(identity(row), (identities.get(identity(row)) || 0) + 1));
      const eligible = rows.filter(row => {
        if (!row.model.trim()) { results.push('空白行：请先填写模型 ID'); return false; }
        if (identities.get(identity(row))! > 1) { results.push(`${row.model}：存在重复价格，请先修正模型 ID`); return false; }
        return true;
      });
      for (let start = 0; start < eligible.length; start += 1000) {
        if (!alive.current || version !== scopeVersion.current.version) return;
        const batch = eligible.slice(start, start + 1000);
        const targets: PrefillTarget[] = batch.map(row => {
          const chosen = ref || (row.selection.mode === 'catalog' ? row.selection.ref : undefined);
          return { kind: row.channel_id === 0 ? 'downstream' : 'upstream', model: row.model.trim(), channel_id: row.channel_id, price_draft: row.selection, ...(chosen ? { ref: { source: chosen.source, id: chosen.id, offering_key: chosen.offering_key }, price_ref: chosen } : {}) };
        });
        let preview: PrefillPreview;
        try {
          preview = await api<PrefillPreview>('/catalog/prefill/preview', { method: 'POST', body: JSON.stringify({ targets, groups: ['price'], force_overwrite: true, price_mode: 'catalog' }) });
          if (!Array.isArray(preview?.patches)) throw new Error('价格匹配响应无效，请重试');
        } catch (error) {
          results.push(...batch.map(row => `${row.display_name || row.model}：匹配失败，${errorMessage(error)}`));
          continue;
        }
        if (!alive.current || version !== scopeVersion.current.version) return;
        const patches = new Map<number, Partial<Price>>();
        for (const row of batch) {
          const candidate = preview.patches?.find(patch => patch.target.model === row.model.trim() && (patch.target.channel_id || 0) === row.channel_id);
          const selection = candidate?.price?.selection;
          const option = selection?.mode === 'catalog' ? catalogOptions.find(option => sameRef(option.ref, selection.ref)) : undefined;
          const current = latestRows.current.find(current => current.key === row.key);
          if (!current || JSON.stringify(current) !== snapshots.get(row.key)) { results.push(`${row.display_name || row.model}：编辑已变化，保持当前草稿`); continue; }
          if (selection?.mode === 'catalog' && option && !option.disabled) {
            patches.set(row.key, { selection }); results.push(`${row.display_name || row.model}：已匹配 ${option.offering.region || '全域'} · ${option.quote.name || option.quote.key}`);
          } else results.push(`${row.display_name || row.model || '空白行'}：${candidate?.warnings?.join('；') || '未找到唯一有效报价，请选择精确资料'}`);
        }
        if (patches.size) {
          setEdited(keys => new Set([...keys, ...patches.keys()]));
          const drafts = advancedDrafts();
          work.update(current => current.map(row => {
            if (!patches.has(row.key) || JSON.stringify(row) !== snapshots.get(row.key)) return row;
            delete drafts[draftIdentity(row, row.key)];
            return { ...row, ...patches.get(row.key) };
          }));
          writeAdvancedDrafts(drafts);
        }
      }
      if (alive.current && version === scopeVersion.current.version) setMatchResult(results);
    } catch (error) { if (alive.current && version === scopeVersion.current.version) work.setError(errorMessage(error)); }
    finally { if (alive.current) setMatching(false); }
  }
  async function removeRows(rows: PriceRow[]) {
    const keys = new Set(rows.map(row => row.key));
    if (!keys.size || !await confirm({ title: '删除价格？', description: `仅删除当前选定的 ${keys.size} 项价格，隐藏项目保持原值。`, confirmLabel: '删除价格' })) return;
    const drafts = advancedDrafts(); rows.forEach(row => delete drafts[draftIdentity(row, row.key)]); writeAdvancedDrafts(drafts);
    work.update(current => current.filter(row => !keys.has(row.key)));
    setChecked(current => new Set([...current].filter(key => !keys.has(key))));
  }
  function reorder(ids: string[]) {
    const keys = new Set(visible.map(row => row.key)), byKey = new Map(work.rows.map(row => [String(row.key), row])); let position = 0;
    work.update(rows => rows.map(row => keys.has(row.key) ? byKey.get(ids[position++]) || row : row));
  }
  const sortable = useSortableList({ ids: visible.map(row => String(row.key)), revision: scope, disabled: matching || work.busy, onCommit: reorder });
  function move(key: number, direction: number) { const ids = visible.map(row => String(row.key)), from = ids.indexOf(String(key)), to = from + direction; if (to >= 0 && to < ids.length) { [ids[from], ids[to]] = [ids[to], ids[from]]; reorder(ids); } }
  return <>
    <PageHeader eyebrow="" title="价格配置" description="维护官方 API 参照价、渠道单价与套餐月费。" actions={view !== 'plan' && <Button variant="secondary" title="保存" aria-label="保存价格" disabled={!work.ready || work.loading || work.busy || !work.dirty} onClick={() => void work.flush().catch(() => {})}><Save size={17} />{work.busy ? '保存中…' : '保存'}</Button>} />
    <section className={`panel price-workbench${view === 'plan' ? ' price-workbench-plan' : ''}`}>
      <div className="price-toolbar">
        <div className="segmented" aria-label="价格范围"><button className={view === 'official' ? 'active' : ''} aria-pressed={view === 'official'} onClick={() => void tab('official')}>官方 API 价</button><button className={view === 'channel' ? 'active' : ''} aria-pressed={view === 'channel'} onClick={() => void tab('channel')}>渠道价格（非套餐）</button><button className={view === 'plan' ? 'active' : ''} aria-pressed={view === 'plan'} onClick={() => tab('plan')}>套餐价格</button></div>
        <div className="price-currency"><Select aria-label="页面币种" value={currency} onChange={e => setCurrency(e.target.value as Currency)}><option value="CNY">CNY</option><option value="USD">USD</option></Select><span>{view === 'plan' ? '/ 每月' : '/ 1M Tokens'}</span></div>
        {view !== 'plan' && <><Button className="price-pull" variant="secondary" aria-busy={pulling} disabled={pulling || work.loading || !work.ready} onClick={() => void pull()}>{pulling ? <Loader2 size={16} className="animate-spin" /> : <Download size={16} />}拉取当前模型</Button>
        <Button className="price-add" title="添加价格" aria-label="手工添加价格" disabled={work.loading || !work.ready} onClick={add}><Plus size={17} />添加价格</Button></>}
      </div>
      <div className="price-scope-toolbar"><div className="search-field"><Search size={16} /><Input aria-label="搜索价格" placeholder="搜索名称 / ID / 渠道" value={search} onChange={e => setSearch(e.target.value)} />{search && <Button variant="ghost" aria-label="清除价格搜索" onClick={() => setSearch('')}><X size={16} /></Button>}</div>{view !== 'official' && <><Button className="price-channel-trigger" variant="secondary" onClick={() => setPicker(true)}>选择渠道 · 已选 {shownChannels.filter(id => pickerOptions.some(row => row.id === id)).length}</Button>{view === 'plan' && <Button variant="secondary" onClick={() => setDeclaration(true)} disabled={channels.loading || !!channels.error}>套餐声明</Button>}</>}</div>
      {view === 'plan' && channels.error && <ErrorBox message={channels.error} retry={channels.reload} />}
      {planVisited && <div hidden={view !== 'plan'}><PlanPrices channels={channels.data || []} selected={planScope} search={searches.plan} currency={currency} rate={rate.data} rateLoading={rate.loading} revision={planRevision} onHistoryChannels={reportHistory} /></div>}
      <div hidden={view === 'plan'}>
      <div className="price-selection-bar"><span>当前 {visible.length} 项 · 已选 {visibleChecked.length}</span><Button disabled={matching || !visibleChecked.length || !catalog.data || catalog.loading} onClick={() => void matchRows(visibleChecked)}><Sparkles size={15} />{matching ? '匹配中…' : `一键匹配（${visibleChecked.length}）`}</Button><label className="price-disabled-toggle"><input type="checkbox" aria-label="查看全部模型" className="checkbox" checked={showAll} onChange={event => setShowAll(event.target.checked)} />查看已停用</label><Button variant="ghost" className="price-delete-selected" disabled={!visibleChecked.length || matching} onClick={() => void removeRows(visibleChecked)}>删除所选<Trash2 size={15} /></Button></div>
      {candidatesError && <div className="p-3"><ErrorBox message={candidatesError} retry={view === 'official' ? models.reload : channels.reload} /></div>}
      {catalog.error && <div className="p-3"><ErrorBox message={`价格资料读取失败：${catalog.error}`} retry={catalog.reload} /></div>}
      {work.error && <div className="p-3"><ErrorBox message={work.error} retry={() => void (!work.ready ? work.retryLoad() : work.flush()).catch(() => {})} /></div>}
      {missingFx && !rate.loading && <div className="price-fx-warning">缺少可用汇率，相关行保留原币种展示，读取汇率后可编辑。<Button variant="ghost" onClick={rate.reload}>读取汇率</Button></div>}
      {!!matchResult.length && <details className="price-match-result" open><summary>匹配结果 · {matchResult.length} 项</summary><ul>{matchResult.map((result, i) => <li key={i}>{result}</li>)}</ul></details>}
      {work.loading || !showAll && candidatesLoading ? <Loading /> : !visible.length ? <Empty title="暂无模型" description={search ? '没有符合搜索的价格，清除搜索后重试。' : target.model ? `未配置 ${target.model} 的价格，可使用上方添加价格。` : '拉取当前已启用模型，或添加一项价格。'} /> : <div className="table-scroll"><table className="data-table price-workbench-table" {...sortable.listProps}><thead><tr><th className="price-select-cell"><div className="price-select-controls"><input aria-label="全选当前可见价格" type="checkbox" className="checkbox" checked={visible.length > 0 && visibleChecked.length === visible.length} ref={node => { if (node) node.indeterminate = visibleChecked.length > 0 && visibleChecked.length < visible.length; }} onChange={e => { const select = e.target.checked; setChecked(keys => { const next = new Set(keys); visible.forEach(row => select ? next.add(row.key) : next.delete(row.key)); return next; }); }} /><span aria-hidden="true" /></div></th><th>名称</th><th>模型 ID</th>{fieldLabels.map(label => <th key={label}>{label}</th>)}<th>条件计费</th><th>资料搜索</th><th>高级 / 删除</th></tr></thead>{visible.map((row, index) => {
        const display = displayed(row), label = `${row.channel_id}:${row.model || `草稿${row.key}`}`;
        const ruleCount = sourceTable(row)?.rules.filter(rule => rule.enabled).length || 0;
        return <tbody key={row.key} className="price-model-group" {...sortable.itemProps(String(row.key))}><tr>
          <td className="price-select-cell"><div className="price-select-controls"><input aria-label={`选择价格 ${label}`} type="checkbox" className="checkbox" checked={checked.has(row.key)} onChange={event => setChecked(keys => { const next = new Set(keys); if (event.target.checked) next.add(row.key); else next.delete(row.key); return next; })} /><Button variant="ghost" title="拖动排序；Alt + 上下箭头移动" aria-label={`排序 ${label}`} {...sortable.handleProps(String(row.key))} onKeyDown={event => { if (event.altKey && ['ArrowUp', 'ArrowDown'].includes(event.key)) { event.preventDefault(); move(row.key, event.key === 'ArrowUp' ? -1 : 1); } }}><GripVertical size={15} /></Button></div></td>
          <td><Input aria-label={`显示名 ${label}`} title={row.origin === 'auto' ? '自动行名称随目录更新' : ''} value={row.display_name} placeholder={row.model || '显示名'} onChange={e => patch(row.key, { display_name: e.target.value })} /><span className="table-sub">{row.selection.mode === 'catalog' ? sourceTable(row) ? '资料库 · 跟随更新' : '资料库 · 报价缺失 / 需刷新' : '已手填'}</span></td>
          <td>{row.channel_id > 0 && <span className="table-sub">{channels.data?.find(channel => channel.id === row.channel_id)?.name || row.channel_id}</span>}{row.model && !isEnabled(row) && !candidatesLoading && !candidatesError && <span className="table-sub">未启用 / 已移除</span>}<Input aria-label={`模型 ID ${label}`} value={row.model} placeholder="模型 ID" onChange={e => patch(row.key, { model: e.target.value, origin: 'manual' })} />{display.table && !display.converted && <span className="table-sub">原币种 {display.table.currency} / 1M Tokens</span>}</td>
          {tableFields.map((field, i) => <td key={field}><Input aria-label={`${fieldLabels[i]} ${label}`} type="number" min={0} step="any" placeholder="—" disabled={!display.editable} value={numberPerMillion(display.table?.rates[field], display.table?.unit_tokens || 1000000)} onChange={e => { if (!e.target.validity.badInput) amount(row, field, e.target.value); }} /></td>)}
          <td className="price-rule-summary">{ruleCount ? `${ruleCount} 项条件` : '无'}{!!sourceTable(row)?.uncovered_conditions?.length && <span className="table-sub">含未覆盖条件</span>}</td>
          <td><PriceCatalogPicker items={catalog.data || []} choices={catalogOptions} official={row.channel_id === 0} model={row.model} label={label} value={row.selection.mode === 'catalog' ? row.selection.ref : undefined} disabled={matching || !row.model.trim() || catalog.loading || !!catalog.error} onSelect={ref => void matchRows([row], ref)} /></td>
          <td><div className="price-row-actions"><Button variant="ghost" title="高级" aria-label={`高级 ${label}`} disabled={matching} onClick={() => openAdvanced(row)}><MoreHorizontal size={17} /></Button><Button variant="ghost" title="删除" aria-label={`删除 ${label}`} disabled={matching} onClick={() => void removeRows([row])}><Trash2 size={15} /></Button></div><span className="sr-only">可见位置 {index + 1}</span></td>
        </tr></tbody>;
      })}</table></div>}
      <div className="price-status" role="status">{work.busy ? '保存中…' : work.dirty ? '已存草稿 · 30 秒自动保存' : '已保存'}</div>
      </div>
    </section>
    <div className="mt-4"><ExchangeRatePanel data={rate.data} loading={rate.loading} error={rate.error} reload={rate.reload} /></div>
    {picker && <ChannelPicker channels={pickerOptions} selected={shownChannels} onChange={view === 'plan' ? setPlanSelected : setSelected} onClose={() => setPicker(false)} />}
    {declaration && <PlanDeclaration channels={channels.data || []} onSaved={() => { channels.reload(); setPlanRevision(value => value + 1); }} onClose={() => setDeclaration(false)} />}
    {advanced && <PriceAdvanced key={advanced.key} value={advanced.price} savedValue={advanced.savedValue} resolvedTable={sourceTable({ ...advanced.price, key: advanced.key })} catalogItems={catalog.data || []} rate={rate.data} onDraft={draftAdvanced} onClose={() => setAdvanced(null)} onDiscard={discardAdvanced} onApply={applyAdvanced} />}
  </>;
}
