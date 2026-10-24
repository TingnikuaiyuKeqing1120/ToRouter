import { useEffect, useMemo, useRef, useState } from 'react';
import { ArrowDown, ArrowUp, BookOpen, Check, Copy, ExternalLink, GripVertical, RefreshCw, Save, Search, X } from 'lucide-react';
import { api, errorMessage, validExchangeRate, type CatalogItem, type CatalogSourceStatus, type Currency, type ExchangeRate } from '../api';
import { CatalogPrice } from '../components/CatalogPrice';
import { catalogEntries, catalogEntryKey } from '../services/catalogEntries';
import { useResource } from '../hooks';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { useSortableList } from '../hooks/useSortableList';
import { Badge, Button, Empty, ErrorBox, Input, Loading, PageHeader, Select, useToast } from '../components/ui';
import { catalogSessionCache, catalogView } from './Catalog.cache';
import { publishStableResource, readStableResource } from '../services/stableResources';
import './Catalog.css';

const sourceOrder = (rows: CatalogSourceStatus[]) => [...rows].sort((a, b) => a.priority - b.priority);
const preferences = (rows: CatalogSourceStatus[]) => JSON.stringify(rows.map(row => [row.source, row.prefill_enabled, row.priority]).sort(([a], [b]) => String(a).localeCompare(String(b))));
function mergeSourceDraft(remote: CatalogSourceStatus[], draft: CatalogSourceStatus[]) {
  const bySource = new Map(draft.map(row => [row.source, row]));
  return sourceOrder(remote.map(row => { const old = bySource.get(row.source); return old ? { ...row, prefill_enabled: old.prefill_enabled, priority: old.priority } : row; }));
}
function formatTokens(n?: number | null) {
  if (n == null || n <= 0) return '未指定';
  if (n >= 1000000) return `${Number((n / 1000000).toFixed(1))}M`;
  if (n >= 1000) return `${Number((n / 1000).toFixed(1))}K`;
  return String(n);
}
function thinkingTier(thinking: CatalogItem['thinking']) {
  if (thinking?.supported == null) return '未知';
  if (!thinking.supported) return '不支持思考';
  const modes = thinking.modes || [], efforts = modes.find(mode => mode.efforts?.some(Boolean))?.efforts?.filter(Boolean);
  if (efforts?.length) return efforts.join(' · ');
  if (modes.some(mode => mode.control === 'budget')) return '由思考预算控制';
  if (modes.some(mode => mode.control === 'adaptive')) return '模型动态自适应';
  if (modes.some(mode => mode.control === 'enabled')) return '由思考开关控制';
  return '支持思考';
}

export default function Catalog() {
  const toast = useToast(), rate = useResource<ExchangeRate>('/exchange-rate');
  const [rawOpen, setRawOpen] = useState<Set<string>>(() => new Set());
  const [items, setItems] = useState<CatalogItem[]>(() => catalogSessionCache.models ?? []), [modelsReady, setModelsReady] = useState(() => catalogSessionCache.models !== undefined), [modelsLoading, setModelsLoading] = useState(() => catalogSessionCache.models === undefined), [modelError, setModelError] = useState('');
  const [sources, renderSources] = useState<CatalogSourceStatus[]>(() => catalogSessionCache.sources ?? []), [sourceReady, setSourceReady] = useState(() => catalogSessionCache.sources !== undefined), [sourceLoading, setSourceLoading] = useState(() => catalogSessionCache.sources === undefined), [sourceError, setSourceError] = useState('');
  const [baseline, renderBaseline] = useState<CatalogSourceStatus[]>(() => catalogSessionCache.sources ?? []), sourceDraft = useRef(sources), sourceBaseline = useRef(baseline);
  const [savingSources, setSavingSources] = useState(false), saving = useRef(false), sourceRevision = useRef(0), sourceRead = useRef(0), modelRead = useRef(0), live = useRef(true);
  const [currency, setCurrency] = useState<Currency>(catalogView.currency), [tab, setTab] = useState(catalogView.tab), [externalSubSource, setExternalSubSource] = useState(catalogView.externalSubSource);
  const [selectedProvider, setSelectedProvider] = useState(catalogView.provider), [search, setSearch] = useState(catalogView.search);
  const filterKey = JSON.stringify([tab, externalSubSource, selectedProvider, search]);
  const [pagination, setPagination] = useState(() => ({ key: filterKey, page: catalogView.page }));
  useEffect(() => { setPagination(current => current.key === filterKey ? current : { key: filterKey, page: 1 }); }, [filterKey]);
  const [pullSource, setPullSource] = useState<'openrouter' | 'litellm'>('openrouter'), [pulling, setPulling] = useState(''), [pullError, setPullError] = useState('');
  const [pullingOffer, setPullingOffer] = useState(''), [offerErrors, setOfferErrors] = useState<Record<string, string>>({}), [copiedId, setCopiedId] = useState<string | null>(null), copyTimer = useRef<ReturnType<typeof setTimeout>>();
  const sourceDirty = preferences(sources) !== preferences(baseline);
  function setSources(rows: CatalogSourceStatus[], edited = false) { sourceDraft.current = rows; renderSources(rows); if (edited) sourceRevision.current++; }
  function setBaseline(rows: CatalogSourceStatus[]) { sourceBaseline.current = rows; renderBaseline(rows); }
  async function loadModels(background = false) {
    const request = ++modelRead.current; if (!background || catalogSessionCache.models === undefined) setModelsLoading(true); setModelError('');
    try { const rows = await readStableResource<CatalogItem[]>('/catalog/models?source=all', !background); if (live.current && request === modelRead.current) { setItems(rows); setModelsReady(true); } }
    catch (error) { if (live.current && request === modelRead.current) setModelError(errorMessage(error)); }
    finally { if (live.current && request === modelRead.current) setModelsLoading(false); }
  }
  async function loadSources(background = false) {
    const request = ++sourceRead.current; if (!background || catalogSessionCache.sources === undefined) setSourceLoading(true); setSourceError('');
    try {
      const rows = await readStableResource<CatalogSourceStatus[]>('/catalog/sources', !background);
      if (!live.current || request !== sourceRead.current) return;
      if (!Array.isArray(rows)) throw new Error('来源配置响应无效');
      const dirty = preferences(sourceDraft.current) !== preferences(sourceBaseline.current);
      const next = dirty ? mergeSourceDraft(rows, sourceDraft.current) : rows;
      setSources(JSON.stringify(next) === JSON.stringify(sourceDraft.current) ? sourceDraft.current : next); setBaseline(rows); setSourceReady(true);
    } catch (error) { if (live.current && request === sourceRead.current) setSourceError(errorMessage(error)); }
    finally { if (live.current && request === sourceRead.current) setSourceLoading(false); }
  }
  useEffect(() => {
    live.current = true; void loadModels(true); void loadSources(true);
    return () => { live.current = false; sourceRead.current++; modelRead.current++; if (copyTimer.current) clearTimeout(copyTimer.current); };
  }, []);
  async function saveSources() {
    if (saving.current || !sourceReady) return false;
    const snapshot = sourceDraft.current, revision = sourceRevision.current;
    if (snapshot.some(row => !Number.isInteger(row.priority) || row.priority < 0)) { setSourceError('推荐优先级必须为非负整数'); return false; }
    saving.current = true; setSavingSources(true); setSourceError(''); sourceRead.current++; setSourceLoading(false);
    try {
      const rows = await api<CatalogSourceStatus[]>('/catalog/sources', { method: 'PUT', body: JSON.stringify(snapshot) });
      if (!Array.isArray(rows)) throw new Error('来源保存响应无效');
      if (!live.current) return false;
      sourceRead.current++; setSourceLoading(false);
      const ordered = publishStableResource('/catalog/sources', sourceOrder(rows))!; setBaseline(ordered); setSources(revision === sourceRevision.current ? ordered : mergeSourceDraft(ordered, sourceDraft.current)); toast('已保存预填来源偏好'); return true;
    } catch (error) { if (live.current) setSourceError(errorMessage(error)); return false; }
    finally { saving.current = false; if (live.current) setSavingSources(false); }
  }
  useUnsavedChanges({ dirty: () => preferences(sourceDraft.current) !== preferences(sourceBaseline.current), busy: () => saving.current, save: saveSources, discard: () => setSources(sourceBaseline.current, true) });
  function updateSource(source: string, change: Partial<CatalogSourceStatus>) { setSources(sourceOrder(sourceDraft.current.map(row => row.source === source ? { ...row, ...change } : row)), true); }
  function reorderSources(ids: string[]) { const byId = new Map(sourceDraft.current.map(row => [row.source, row])); setSources(ids.map((id, index) => ({ ...byId.get(id)!, priority: index + 1 })), true); }
  const sortable = useSortableList({ ids: sources.map(row => row.source), disabled: savingSources || !sourceReady, revision: preferences(sources), onCommit: reorderSources });
  function moveSource(index: number, direction: number) { const ids = sources.map(row => row.source), next = index + direction; if (next < 0 || next >= ids.length) return; [ids[index], ids[next]] = [ids[next], ids[index]]; reorderSources(ids); }
  async function handlePull() {
    if (pulling) return;
    const requestedSource = pullSource; setPulling(requestedSource); setPullError('');
    try {
      const result = await api<{ source: string; count: number }>('/catalog/fetch', { method: 'POST', body: JSON.stringify({ source: requestedSource }) });
      if (!live.current) return;
      toast(`拉取成功：已收录 ${result.count} 款 ${result.source} 模型`); await Promise.all([loadModels(), loadSources()]);
    } catch (error) { if (live.current) setPullError(errorMessage(error)); }
    finally { if (live.current) setPulling(''); }
  }
  async function fetchOfferings(item: CatalogItem) {
    if (pullingOffer) return;
    const key = `${item.source}:${item.id}`; setPullingOffer(key); setOfferErrors(errors => ({ ...errors, [key]: '' }));
    try {
      await api('/catalog/offerings/fetch', { method: 'POST', body: JSON.stringify({ source: item.source, id: item.id }) });
      if (!live.current) return;
      await Promise.all([loadModels(), loadSources()]); if (live.current) toast('已更新该模型供应商报价');
    } catch (error) { if (live.current) setOfferErrors(errors => ({ ...errors, [key]: errorMessage(error) })); }
    finally { if (live.current) setPullingOffer(''); }
  }
  async function copyModelId(id: string) {
    try { await navigator.clipboard.writeText(id); if (!live.current) return; setCopiedId(id); if (copyTimer.current) clearTimeout(copyTimer.current); copyTimer.current = setTimeout(() => setCopiedId(null), 2000); toast(`已复制模型 ID: ${id}`); }
    catch { if (live.current) toast('复制失败，请手动选择复制'); }
  }
  const entries = useMemo(() => catalogEntries(items), [items]);
  const originals = useMemo(() => new Map(items.map(item => [`${item.source}:${item.id}`, item])), [items]);
  const providers = useMemo(() => [...new Set(entries.map(({ item }) => item.provider).filter(Boolean))].sort(), [entries]);
  const filteredItems = useMemo(() => {
    const query = search.trim().toLowerCase();
    return entries.filter(({ item }) => {
      if (tab === 'builtin' && item.source !== 'builtin' || tab === 'external' && item.source === 'builtin') return false;
      if (tab !== 'builtin' && externalSubSource !== 'all' && item.source !== 'builtin' && item.source !== externalSubSource) return false;
      if (selectedProvider !== 'all' && item.provider !== selectedProvider) return false;
      return !query || `${item.id} ${item.name} ${item.provider} ${item.offerings.map(offering => offering.region || '').join(' ')}`.toLowerCase().includes(query);
    });
  }, [entries, tab, externalSubSource, selectedProvider, search]);
  const pageCount = Math.max(1, Math.ceil(filteredItems.length / 36));
  const page = pagination.key === filterKey ? Math.min(pagination.page, pageCount) : 1;
  const visibleItems = filteredItems.slice((page - 1) * 36, page * 36);
  function changePage(nextPage: number) {
    setPagination({ key: filterKey, page: nextPage });
    if (typeof window !== 'undefined' && typeof window.scrollTo === 'function') {
      try {
        window.scrollTo({ top: 0, behavior: 'smooth' });
      } catch {
        window.scrollTo(0, 0);
      }
    }
  }
  useEffect(() => { Object.assign(catalogView, { page, search, tab, externalSubSource, provider: selectedProvider, currency }); }, [page, search, tab, externalSubSource, selectedProvider, currency]);
  const hasFilters = !!search.trim() || tab !== 'all' || externalSubSource !== 'all' || selectedProvider !== 'all';
  function clearFilters() { setSearch(''); setTab('all'); setExternalSubSource('all'); setSelectedProvider('all'); }
  const pullName = pullSource === 'openrouter' ? 'OpenRouter' : 'LiteLLM';
  return <div className="catalog-page">
    <PageHeader eyebrow="" title="模型数据库" description="查询模型规格、地区与报价，维护预填来源偏好。" actions={<div className="catalog-page-actions"><a className="catalog-model-link" href="#/models">公开模型 <ExternalLink size={14} /></a><Select aria-label="拉取来源" value={pullSource} onChange={event => setPullSource(event.target.value as typeof pullSource)}><option value="openrouter">OpenRouter</option><option value="litellm">LiteLLM</option></Select><Button variant="secondary" onClick={() => void handlePull()} disabled={!!pulling}><RefreshCw size={15} className={pulling ? 'animate-spin' : ''} />{pulling ? `正在拉取 ${pulling === 'openrouter' ? 'OpenRouter' : 'LiteLLM'}…` : `立即拉取 ${pullName}`}</Button></div>} />
    {pullError && <ErrorBox message={`拉取失败：${pullError}`} retry={() => void handlePull()} />}
    <details className="panel catalog-source-panel"><summary><BookOpen size={16} /><strong>来源与预填管理</strong><span>{sources.length} 个来源{sourceDirty ? ' · 未保存' : ''}{sourceError ? ' · 配置异常' : sourceLoading ? ' · 读取中' : ''}</span></summary><div className="catalog-source-content">
      <div className="catalog-source-heading"><p>靠前的来源优先推荐；只影响预填匹配，关闭参与预填仍可浏览资料。</p><Button variant="secondary" disabled={!sourceReady || !sourceDirty || savingSources} onClick={() => void saveSources()}><Save size={15} />{savingSources ? '保存中…' : '保存来源偏好'}</Button></div>
      {sourceError && <ErrorBox message={sourceError} retry={() => void (sourceReady && sourceDirty ? saveSources() : loadSources())} />}
      {!sourceReady && sourceLoading ? <Loading /> : <div className="catalog-source-list" {...sortable.listProps}>{sources.map((source, index) => <div className="catalog-source-row" key={source.source} {...sortable.itemProps(source.source)}>
        <Button variant="ghost" aria-label={`拖动来源 ${source.name}`} title="拖动修改预填推荐顺序" disabled={savingSources} {...sortable.handleProps(source.source)}><GripVertical size={16} /></Button><div className="catalog-source-name"><strong>{source.name}</strong><span>{source.item_count} 款{source.last_fetched_at ? ` · 更新于 ${new Date(source.last_fetched_at).toLocaleDateString()}` : ''}</span>{source.last_error && <span className="catalog-source-last-error">上次拉取：{source.last_error}</span>}</div>
        <label><input type="checkbox" className="checkbox" aria-label={`${source.name} 参与预填`} checked={source.prefill_enabled} disabled={savingSources} onChange={event => updateSource(source.source, { prefill_enabled: event.target.checked })} />参与预填</label><label className="catalog-priority">优先级<Input type="number" min={0} step={1} aria-label={`${source.name} 推荐优先级`} value={source.priority} disabled={savingSources} onChange={event => updateSource(source.source, { priority: Number(event.target.value) })} /></label><div className="catalog-source-move"><Button variant="ghost" title="上移" aria-label={`上移来源 ${source.name}`} disabled={savingSources || index === 0} onClick={() => moveSource(index, -1)}><ArrowUp size={15} /></Button><Button variant="ghost" title="下移" aria-label={`下移来源 ${source.name}`} disabled={savingSources || index === sources.length - 1} onClick={() => moveSource(index, 1)}><ArrowDown size={15} /></Button></div>
      </div>)}</div>}
      <div className="catalog-source-status" role="status">{sourceLoading && sourceReady ? '正在更新来源状态，保留当前填写…' : sourceDirty ? '来源偏好未保存' : sourceReady ? '来源偏好已保存' : '来源配置尚未读取'}</div>
    </div></details>
    <section className="panel catalog-filters" aria-label="模型资料筛选"><div className="catalog-filter-top"><div className="segmented" aria-label="数据库来源">{(['all', 'builtin', 'external'] as const).map(value => <button key={value} aria-pressed={tab === value} className={tab === value ? 'active' : ''} onClick={() => setTab(value)}>{value === 'all' ? `全部 (${entries.length})` : value === 'builtin' ? `精选静态库 (官方) (${entries.filter(entry => entry.item.source === 'builtin').length})` : `外部数据库 (${entries.filter(entry => entry.item.source !== 'builtin').length})`}</button>)}</div><div className="catalog-currency"><Select aria-label="页面币种" value={currency} onChange={event => setCurrency(event.target.value as Currency)}><option value="CNY">CNY</option><option value="USD">USD</option></Select><span>/ 1M Tokens</span></div></div>
      <div className="catalog-filter-controls"><div className="catalog-search"><Search size={16} /><Input aria-label="搜索模型数据库" placeholder="搜索模型 ID、名称或厂商 / 地区" value={search} onChange={event => setSearch(event.target.value)} />{search && <Button variant="ghost" aria-label="清除模型搜索" onClick={() => setSearch('')}><X size={15} /></Button>}</div><Select aria-label="筛选厂商" value={selectedProvider} onChange={event => setSelectedProvider(event.target.value)}><option value="all">全部厂商 ({providers.length})</option>{providers.map(provider => <option key={provider} value={provider}>{provider}</option>)}</Select>{tab !== 'builtin' && <Select aria-label="外部信源" value={externalSubSource} onChange={event => setExternalSubSource(event.target.value as typeof externalSubSource)}><option value="all">全部外部来源</option><option value="openrouter">OpenRouter</option><option value="litellm">LiteLLM</option></Select>}</div>
      <div className="catalog-filter-status"><span aria-live="polite">显示 {filteredItems.length} / {entries.length} 项资料</span><Button variant="ghost" disabled={!hasFilters} onClick={clearFilters}>清除筛选</Button><span className="catalog-fx-status">{validExchangeRate(rate.data) ? `1 USD ≈ ${rate.data.rate} CNY${rate.data.stale ? '（旧值）' : rate.error ? '（上次结果）' : ''}` : rate.loading ? '正在读取汇率…' : '汇率不可用，跨币种报价保留原币'}</span>{(!validExchangeRate(rate.data) || rate.error) && !rate.loading && <Button variant="ghost" onClick={rate.reload}>读取汇率</Button>}</div>
    </section>
    <div className="catalog-model-status" role="status">{modelsLoading && modelsReady ? '正在刷新模型资料，保留当前结果…' : pulling ? '正在拉取来源资料…' : ''}</div>
    {modelError && <ErrorBox message={`${modelsReady ? '模型刷新失败，显示上次资料：' : '模型资料读取失败：'}${modelError}`} retry={() => void loadModels()} />}
    {filteredItems.length > 36 && <nav className="catalog-pagination" aria-label="模型资料分页"><span>第 {page} / {pageCount} 页 · {(page - 1) * 36 + 1}–{Math.min(page * 36, filteredItems.length)} 项</span><Button variant="ghost" disabled={page === 1} onClick={() => changePage(page - 1)}>上一页</Button><Button variant="ghost" disabled={page === pageCount} onClick={() => changePage(page + 1)}>下一页</Button></nav>}
    {!modelsReady && modelsLoading ? <Loading /> : !modelsReady ? null : !filteredItems.length ? <Empty title={items.length ? '未找到匹配的模型规格' : '模型数据库为空'} description={items.length ? '调整来源、厂商或搜索条件，或清除全部筛选。' : `可拉取 ${pullName} 模型资料。`} action={items.length ? <Button variant="ghost" onClick={clearFilters}>重置筛选</Button> : <Button variant="secondary" onClick={() => void handlePull()} disabled={!!pulling}>{`立即拉取 ${pullName} 数据库`}</Button>} /> : <><div className="catalog-grid">{visibleItems.map(({ item, ref }) => {
      const modelKey = `${item.source}:${item.id}`, original = originals.get(modelKey) || item;
      return <article key={catalogEntryKey(ref)} className="group catalog-card"><div className="catalog-card-heading"><div><div className="catalog-card-source"><span>{item.provider}</span><Badge tone={item.source === 'builtin' ? 'blue' : 'neutral'}>{item.source === 'builtin' ? '官方精选' : item.source === 'openrouter' ? 'OpenRouter' : item.source === 'litellm' ? 'LiteLLM' : item.source}</Badge></div><h3>{item.name || item.id}</h3><p className="catalog-model-id">{item.id}</p></div><Button variant="ghost" title="复制模型 ID" aria-label="复制模型 ID" onClick={() => void copyModelId(item.id)}>{copiedId === item.id ? <Check size={16} /> : <Copy size={16} />}</Button></div>
        <dl className="catalog-specs"><div><dt>上下文窗口</dt><dd>{formatTokens(item.context_length)}</dd></div><div><dt>最大输出</dt><dd>{formatTokens(item.max_output_tokens)}</dd></div></dl><div className="catalog-capabilities">{item.modalities.input?.filter(modality => modality !== 'text').map(modality => <Badge key={modality}>{modality === 'image' ? '视觉输入' : modality === 'audio' ? '音频输入' : modality === 'video' ? '视频输入' : `${modality} 输入`}</Badge>)}<span className="catalog-thinking">思考档位：{thinkingTier(item.thinking)}</span></div>
        {item.needs_refresh && <p className="catalog-refresh-needed">待刷新，无法确认的字段不参与自动预填。</p>}
        <CatalogPrice item={item} currency={currency} rate={rate.data} rateLoading={rate.loading} />
        <div className="catalog-card-actions">{item.source === 'openrouter' && <Button variant="secondary" disabled={!!pullingOffer} onClick={() => void fetchOfferings(item)}>{pullingOffer === modelKey ? '正在拉取报价…' : '拉取该模型供应商报价'}</Button>}{item.doc_url && <a href={item.doc_url} target="_blank" rel="noreferrer">官方文档 <ExternalLink size={13} /></a>}</div>
        {offerErrors[modelKey] && <ErrorBox message={`报价更新失败：${offerErrors[modelKey]}`} retry={() => void fetchOfferings(item)} />}
        <details className="catalog-technical"><summary>更多详细信息与技术来源</summary><div>{item.max_input_tokens != null && <p>原厂独立最大输入：{formatTokens(item.max_input_tokens)}</p>}{item.thinking?.modes?.map(mode => <p key={`${mode.api_format}/${mode.id}`}>思考模式：{mode.id} · {mode.api_format} · {mode.control}</p>)}{item.notes?.map(note => <p key={note}>{note}</p>)}{item.verified_at && <p>核实日期：{item.verified_at}</p>}{item.fetched_at && <p>抓取日期：{item.fetched_at}</p>}{ref.offering_key && <p>地区报价标识：{ref.offering_key}</p>}<details className="catalog-raw" onToggle={event => { const open = event.currentTarget.open, key = catalogEntryKey(ref); setRawOpen(current => { const next = new Set(current); if (open) next.add(key); else next.delete(key); return next; }); }}><summary>原始模型资料 JSON</summary>{rawOpen.has(catalogEntryKey(ref)) && <pre>{JSON.stringify(original, null, 2)}</pre>}</details></div></details>
      </article>;
    })}</div>{filteredItems.length > 36 && <nav className="catalog-pagination catalog-pagination-bottom" aria-label="模型资料底部分页"><span>第 {page} / {pageCount} 页</span><Button variant="ghost" aria-label="上一页（底部）" disabled={page === 1} onClick={() => changePage(page - 1)}>上一页</Button><Button variant="ghost" aria-label="下一页（底部）" disabled={page === pageCount} onClick={() => changePage(page + 1)}>下一页</Button></nav>}</>}
  </div>;
}
