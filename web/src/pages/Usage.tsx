import { Fragment, useMemo, useState, type ReactNode } from 'react';
import { Activity, ArrowDownLeft, ArrowUpRight, BadgeDollarSign, CalendarDays, ChartNoAxesCombined, ChevronDown, ChevronRight, Download, LayoutDashboard, Layers3, ListTree, PencilLine, RefreshCw, Settings2, Wallet } from 'lucide-react';
import { errorMessage, localDate, number, type Channel, type Currency, type ExchangeRate, type UsageCostComponents, type UsageMetrics, type UsageSummary } from '../api';
import { useResource } from '../hooks';
import { ExchangeRatePanel } from '../components/ExchangeRatePanel';
import { HistoryBackfill } from '../components/HistoryBackfill';
import { Button, DateFields, Empty, ErrorBox, Input, Loading, PageHeader, Select } from '../components/ui';
import { UsageComposition, UsageInputDistribution, UsageTrend, TokenComposition, type UsageTrendSelection } from '../components/usage/UsageCharts';
import { compactNumber, convertAmounts, displayMoney, formatMetric, formatPercent, pricingValues, type UsageMeasure } from '../components/usage/metrics';
import { allTimeRange, calendarLabels, customRange, presetRange, recentLabels, usageQuery, type UsagePeriod, type UsageRange, type UsageTimeMode } from '../components/usage/time';
import './Usage.css';

type Section = 'overview' | 'details' | 'analysis' | 'other';
type DetailsSelection = { view: 'channel' | 'model'; search: string; expanded: Set<number> };
const sections = [
  { id: 'overview', label: '用量总览', icon: LayoutDashboard },
  { id: 'details', label: '用量明细', icon: ListTree },
  { id: 'analysis', label: '使用分析', icon: ChartNoAxesCombined },
  { id: 'other', label: '导出与汇率', icon: Settings2 },
] as const;
const costLabels: Record<keyof UsageCostComponents, string> = { input: '普通输入', output: '输出', cache_read: '缓存命中', cache_write: '缓存写入', unattributed: '未拆分', plan: '套餐月费' };

function hasTokens(value: UsageMetrics) { return value.known_usage_requests > 0 || value.requests === 0; }
function tokenText(value: UsageMetrics, amount: number) { return hasTokens(value) ? number(amount) : '—'; }
function rangeDates(value: Pick<UsageRange, 'from' | 'to'>) {
  if (!value.from && !value.to) return '全部历史数据';
  return localDate(new Date(value.from)) + ' — ' + localDate(new Date(value.to));
}
function rangeName(value: UsageRange) {
  return ({ 今日: '今日至今', 本周: '本周至今', 本月: '本月至今' } as Record<string, string>)[value.label]
    || (value.label.includes(' — ') ? '自定义' : value.label);
}
function Tokens({ metrics, value, compact = false }: { metrics: UsageMetrics; value: number; compact?: boolean }) {
  return <span title={hasTokens(metrics) ? number(value) + ' Token' : '未提供用量'}>{hasTokens(metrics) ? compact ? compactNumber(value) : number(value) : '—'}</span>;
}
function nativeText(value: UsageMetrics, measure: 'actual' | 'official', currency: Currency, compact = false) {
  const { amounts, count } = pricingValues(value, measure);
  if (!count || !amounts) return measure === 'actual' && value.plan_requests ? '套餐内 · 月费未分摊到请求' : '未计价';
  const present = (['CNY', 'USD'] as const).filter(key => amounts[key] > 0);
  return (present.length ? present : [currency]).map(key => formatMetric(amounts[key] || 0, measure, key, compact) + ' ' + key).join(' · ');
}
function Amount({ value, measure, currency, rate, native = false }: { value: UsageMetrics; measure: 'actual' | 'official'; currency: Currency; rate: ExchangeRate | null; native?: boolean }) {
  const { amounts, count } = pricingValues(value, measure);
  const converted = count ? convertAmounts(amounts, currency, rate) : null;
  const other = currency === 'CNY' ? 'USD' : 'CNY';
  const planOnly = measure === 'actual' && !count && (value.plan_requests || 0) > 0;
  const precise = converted == null ? nativeText(value, measure, currency) : formatMetric(converted, measure, currency) + ' · 原币 ' + nativeText(value, measure, currency);
  return <>
    <span title={precise}>{converted == null ? planOnly && !native ? '套餐内' : '—' : (amounts?.[other] ? '≈ ' : '') + displayMoney(converted, currency)}</span>
    {native && (!count || !!amounts?.[other] || converted == null) && <span className="usage-native-amount" title={nativeText(value, measure, currency)}>
      {!count ? planOnly ? '月费按支付日期计入' : '暂无已计价记录' : '原币 ' + nativeText(value, measure, currency, true)}{count > 0 && converted == null && ' · 待汇率换算'}
    </span>}
  </>;
}
function MetricCard({ title, hint, icon, value, children, className = '' }: { title: string; hint?: string; icon: ReactNode; value: ReactNode; children: ReactNode; className?: string }) {
  return <section className={'usage-metric-card ' + className} aria-label={title}>
    <div className="usage-card-heading"><h2>{title}{hint && <small>{hint}</small>}</h2><span className="usage-card-icon" aria-hidden="true">{icon}</span></div>
    <div className="usage-card-value">{value}</div><div className="usage-card-detail">{children}</div>
  </section>;
}
function CostParts({ components, currency, rate, pricedRequests }: { components: UsageSummary['actual_components']; currency: Currency; rate: ExchangeRate | null; pricedRequests: number }) {
  if (pricedRequests === 0) return null;
  const fields = ['input', 'output', 'cache_read', 'cache_write', 'plan', 'unattributed'] as const;
  return <details className="usage-cost-parts"><summary>费用构成 <ChevronDown size={13} /></summary><dl>{fields.map(field => {
    if ((field === 'unattributed' || field === 'plan') && !components.CNY[field] && !components.USD[field]) return null;
    const total = convertAmounts({ CNY: components.CNY[field] || 0, USD: components.USD[field] || 0 }, currency, rate);
    return <div key={field}><dt>{costLabels[field]}</dt><dd>{formatMetric(total, 'actual', currency)}</dd></div>;
  })}</dl></details>;
}
function SummaryCards({ data, currency, rate }: { data: UsageSummary; currency: Currency; rate: ExchangeRate | null }) {
  const ratio = hasTokens(data) && data.output_tokens > 0 ? (data.input_tokens / data.output_tokens).toFixed(1) + ' : 1' : '—';
  return <div className="usage-summary-grid">
    <MetricCard title="请求数" icon={<Activity size={18} />} value={number(data.requests)}>
      <span>成功 {number(data.requests - data.failures)}</span><span className={data.failures ? 'usage-warning-text' : ''}>失败 {number(data.failures)}</span>
    </MetricCard>
    <MetricCard title="总 Token" icon={<Layers3 size={18} />} value={<Tokens metrics={data} value={data.input_tokens + data.output_tokens} compact />}>
      <span>输入 + 输出，包含缓存</span>{data.unknown_usage_requests > 0 && <span className="usage-warning-text">{number(data.unknown_usage_requests)} 次未提供用量</span>}
    </MetricCard>
    <MetricCard title="实际支出" hint="按量 + 套餐" icon={<Wallet size={18} />} className="usage-money-card" value={<Amount value={data} measure="actual" currency={currency} rate={rate} native />}>
      <span title="按量请求计价；套餐月费独立按支付日期计入，不分摊。">按量已计价 {number(data.actual_priced_requests || 0)} / {number(Math.max(0, data.requests - (data.plan_requests || 0)))}</span>
      {!!data.plan_requests && <span>套餐内 {number(data.plan_requests)} 次</span>}
      {!!data.plan_charges && <span>月费 {number(data.plan_charges)} 笔</span>}
      {!!data.unknown_plan_charges && <span className="usage-warning-text">{number(data.unknown_plan_charges)} 笔月费金额未知</span>}
      {!!data.unconfigured_plan_channels && <a className="usage-warning-text" href="#/prices?view=plan">{number(data.unconfigured_plan_channels)} 个套餐渠道尚未配置月费 ↗</a>}
      <CostParts components={data.actual_components} currency={currency} rate={rate} pricedRequests={pricingValues(data, 'actual').count} />
    </MetricCard>
    <MetricCard title="输入 Token" icon={<ArrowDownLeft size={18} />} value={<Tokens metrics={data} value={data.input_tokens} compact />}>
      <span>未命中 <Tokens metrics={data} value={data.plain_input_tokens + data.cache_write_tokens} compact /></span><span>命中 <Tokens metrics={data} value={data.cache_read_tokens} compact /></span>
      <span className="usage-card-highlight">缓存命中率 <strong>{hasTokens(data) ? formatPercent(data.cache_read_tokens, data.input_tokens) : '—'}</strong></span>
    </MetricCard>
    <MetricCard title="输出 Token" icon={<ArrowUpRight size={18} />} value={<Tokens metrics={data} value={data.output_tokens} compact />}>
      <span>模型生成的输出用量</span><span className="usage-card-highlight">输入 : 输出 <strong>{ratio}</strong></span>
    </MetricCard>
    <MetricCard title="等价API" hint="标价" icon={<BadgeDollarSign size={18} />} className="usage-money-card" value={<Amount value={data} measure="official" currency={currency} rate={rate} native />}>
      <span title="按公开模型标价计算。">已计价 {number(data.official_priced_requests || 0)} / {number(data.requests)}</span>
      <CostParts components={data.official_components} currency={currency} rate={rate} pricedRequests={data.official_priced_requests || 0} />
    </MetricCard>
  </div>;
}
function DetailNumbers({ row, currency, rate }: { row: UsageMetrics; currency: Currency; rate: ExchangeRate | null }) {
  return <><td>{number(row.requests)}</td><td>{tokenText(row, row.input_tokens)}</td><td>{tokenText(row, row.output_tokens)}</td><td>{tokenText(row, row.cache_read_tokens)}</td><td>{tokenText(row, row.cache_write_tokens)}</td><td>{hasTokens(row) ? formatPercent(row.cache_read_tokens, row.input_tokens) : '—'}</td><td><Amount value={row} measure="actual" currency={currency} rate={rate} /></td><td><Amount value={row} measure="official" currency={currency} rate={rate} /></td></>;
}
function UsageDetails({ data, currency, rate, selection, onChange }: { data: UsageSummary; currency: Currency; rate: ExchangeRate | null; selection: DetailsSelection; onChange: (next: DetailsSelection) => void }) {
  const { view, search, expanded } = selection;
  const groups = (view === 'channel' ? data.by_channel : data.by_model).filter(group => (group.name + ' ' + (group.channel_id ?? '')).toLocaleLowerCase().includes(search.trim().toLocaleLowerCase())).slice().sort((a, b) => b.requests - a.requests || a.name.localeCompare(b.name));
  return <section className="panel usage-detail-panel" aria-label="用量明细">
    <div className="usage-section-heading"><div><h2>用量明细</h2><p>按当前时间范围汇总 · 金额单位 {currency}</p></div>
      <div className="usage-segmented" role="group" aria-label="明细维度"><button aria-pressed={view === 'channel'} onClick={() => onChange({ ...selection, view: 'channel', search: '' })}>按渠道</button><button aria-pressed={view === 'model'} onClick={() => onChange({ ...selection, view: 'model', search: '' })}>按公开模型</button></div>
    </div>
    <div className="usage-detail-search"><Input aria-label={view === 'channel' ? '搜索渠道' : '搜索公开模型'} placeholder={view === 'channel' ? '搜索渠道名称或 ID' : '搜索公开模型 ID'} value={search} onChange={e => onChange({ ...selection, search: e.target.value })} /><span>{number(groups.length)} {view === 'channel' ? '个渠道' : '个模型'}</span></div>
    {!groups.length ? <Empty title={search ? '没有匹配的记录' : '暂无用量记录'} description={search ? '试试其他名称或 ID。' : '所选期间没有用量记录。'} /> : <div className="usage-detail-scroll" tabIndex={0} aria-label="用量明细表，可横向滚动"><table className="data-table usage-detail-table"><thead><tr><th scope="col">{view === 'channel' ? '渠道 / 公开模型' : '公开模型'}</th><th scope="col">请求</th><th scope="col" title="普通输入、缓存写入与缓存命中的合计">输入</th><th scope="col">输出</th><th scope="col">缓存命中</th><th scope="col">缓存写入</th><th scope="col">命中率</th><th scope="col">实际支出</th><th scope="col">等价API</th></tr></thead><tbody>{groups.map(group => {
      const id = group.channel_id ?? 0, open = view === 'channel' && expanded.has(id);
      return <Fragment key={view === 'channel' ? 'channel-' + id : group.name}>
        <tr><td>{view === 'channel' ? <button className="usage-channel-toggle" aria-label={'展开渠道 ' + group.name} aria-expanded={open} onClick={() => { const next = new Set(expanded); if (next.has(id)) next.delete(id); else next.add(id); onChange({ ...selection, expanded: next }); }}>{open ? <ChevronDown size={16} /> : <ChevronRight size={16} />}<span>{group.name}<small>#{id} · {number(group.models?.length || 0)} 个公开模型</small></span></button> : <span className="usage-model-name">{group.name}</span>}</td><DetailNumbers row={group} currency={currency} rate={rate} /></tr>
        {open && (group.models || []).slice().sort((a, b) => b.requests - a.requests || a.name.localeCompare(b.name)).map(model => <tr className="usage-model-child" key={id + '-' + model.name}><td><span>{model.name}</span></td><DetailNumbers row={model} currency={currency} rate={rate} /></tr>)}
      </Fragment>;
    })}</tbody></table></div>}
    <p className="usage-table-note">输入包含缓存。渠道实际支出含支付日落在期间内的月费，月费不分摊到公开模型。金额为已计价部分；“套餐内”表示按套餐使用，“—”表示未计价或暂不可换算。</p>
  </section>;
}
function UsageAnalysis({ data, model, onModelChange }: { data: UsageSummary; model: string; onModelChange: (model: string) => void }) {
  const models = data.by_model.slice().sort((a, b) => a.name.localeCompare(b.name));
  const metrics = model ? models.find(item => item.name === model) : data;
  return <div className="usage-analysis">
    <div className="usage-section-heading usage-analysis-heading"><div><h2>使用分析</h2><p>输入、输出与缓存的工作负载</p></div><Select aria-label="分析公开模型" value={model} onChange={e => onModelChange(e.target.value)}><option value="">总体</option>{model && !metrics && <option value={model}>{model}（本期间无记录）</option>}{models.map(item => <option key={item.name} value={item.name}>{item.name}</option>)}</Select></div>
    {!metrics ? <Empty title="所选模型暂无记录" description={model + ' 在当前期间没有调用，可调整时间或选择其他模型。'} /> : <>
      <div className="usage-workload-stats"><div><span>缓存命中率</span><strong>{hasTokens(metrics) ? formatPercent(metrics.cache_read_tokens, metrics.input_tokens) : '—'}</strong><small>命中 Token / 总输入</small></div><div><span>输入 : 输出</span><strong>{hasTokens(metrics) && metrics.output_tokens > 0 ? (metrics.input_tokens / metrics.output_tokens).toFixed(1) + ' : 1' : '—'}</strong><small>输入包含缓存读写</small></div><div><span>有用量记录</span><strong>{number(metrics.known_usage_requests)}</strong><small>另有 {number(metrics.unknown_usage_requests)} 次未提供</small></div></div>
      <TokenComposition metrics={metrics} /><UsageInputDistribution buckets={metrics.input_distribution} unknownRequests={metrics.unknown_usage_requests} />
    </>}
  </div>;
}
function Failure({ message, detail, retry }: { message: string; detail: string; retry: () => void }) {
  return <div className="usage-failure"><ErrorBox message={message} retry={retry} /><details><summary>查看错误详情</summary><p>{detail}</p></details></div>;
}

export default function Usage() {
  const [section, setSection] = useState<Section>('overview');
  const [mode, setMode] = useState<UsageTimeMode>('calendar');
  const [period, setPeriod] = useState<UsagePeriod>('month');
  const [range, setRange] = useState(() => presetRange('calendar', 'month'));
  const [from, setFrom] = useState(() => localDate(new Date(range.from)));
  const [to, setTo] = useState(() => localDate(new Date(range.to)));
  const [rangeError, setRangeError] = useState('');
  const [currency, setCurrency] = useState<Currency>('CNY');
  const [trend, setTrend] = useState<UsageTrendSelection>({ dimension: 'total', measure: 'tokens' });
  const [composition, setComposition] = useState<{ model: UsageMeasure; channel: UsageMeasure }>({ model: 'tokens', channel: 'tokens' });
  const [details, setDetails] = useState<DetailsSelection>({ view: 'channel', search: '', expanded: new Set() });
  const [analysisModel, setAnalysisModel] = useState('');
  const [backfill, setBackfill] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [exportResult, setExportResult] = useState<{ query: string; error: string } | null>(null);
  const timezone = useMemo(() => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', []);
  const query = usageQuery(range, timezone);
  const path = '/usage?' + query;
  const { data, loadedPath, loading, error, reload } = useResource<UsageSummary>(path);
  const rate = useResource<ExchangeRate>('/exchange-rate');
  const channels = useResource<Channel[]>('/channels');
  const loadedRange = useMemo(() => {
    if (!loadedPath) return null;
    const params = new URLSearchParams(loadedPath.split('?')[1]);
    return { from: params.get('from') || '', to: params.get('to') || '' };
  }, [loadedPath]);
  const updating = loading || (!error && loadedPath !== path);
  const draftRange = useMemo(() => { try { return customRange(from, to); } catch { return null; } }, [from, to]);
  const pendingDates = mode === 'custom' && (!draftRange || draftRange.from !== range.from || draftRange.to !== range.to);

  function choosePreset(nextMode: 'calendar' | 'recent', nextPeriod: UsagePeriod) {
    const next = presetRange(nextMode, nextPeriod);
    setMode(nextMode); setPeriod(nextPeriod); setRange(next);
    setFrom(localDate(new Date(next.from))); setTo(localDate(new Date(next.to))); setRangeError('');
  }
  function refresh() { if (mode === 'custom' || mode === 'all') reload(); else choosePreset(mode, period); }
  async function exportCSV() {
    if (exporting) return;
    setExporting(true); setExportResult(null);
    try {
      const response = await fetch('/api/usage/export?' + query, { headers: { 'X-To-Router': '1' } });
      if (!response.ok) { const body = await response.json().catch(() => null); throw new Error(body?.error?.message || '导出失败，请重试。'); }
      const objectURL = URL.createObjectURL(await response.blob());
      const link = document.createElement('a'); link.href = objectURL; link.download = 'torouter-usage-' + (range.from ? localDate(new Date(range.from)) + '-' + localDate(new Date(range.to)) : 'all') + '.csv';
      document.body.appendChild(link); link.click(); link.remove();
      setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
      setExportResult({ query, error: '' });
    } catch (e) { setExportResult({ query, error: errorMessage(e) }); }
    finally { setExporting(false); }
  }

  return <div className="usage-page">
    <PageHeader eyebrow="USAGE OVERVIEW" title="用量概览" actions={<><Button variant="secondary" onClick={() => setBackfill(true)}><PencilLine size={16} />补算未计价</Button><Button variant="secondary" onClick={refresh} disabled={updating}><RefreshCw size={16} className={updating ? 'animate-spin' : ''} />刷新数据</Button></>} />
    <div className="usage-navigation">
      <nav className="usage-tabs" aria-label="用量页导航">{sections.map(item => <button key={item.id} aria-current={section === item.id ? 'page' : undefined} onClick={() => setSection(item.id)}><item.icon size={17} /><span>{item.label}</span></button>)}</nav>
      <label className="usage-currency"><span>显示币种</span><Select aria-label="显示币种" value={currency} onChange={e => setCurrency(e.target.value as Currency)}><option value="CNY">CNY · 人民币</option><option value="USD">USD · 美元</option></Select></label>
    </div>
    <section className="usage-timebar" aria-label="统计时间范围">
      <div className="usage-timebar-line">
        <span className="usage-time-label"><CalendarDays size={16} />时间范围</span>
        <div className="usage-segmented" role="group" aria-label="时间方式"><button aria-pressed={mode === 'calendar'} onClick={() => choosePreset('calendar', period)}>自然周期</button><button aria-pressed={mode === 'recent'} onClick={() => choosePreset('recent', period)}>最近一段</button></div>
        {(mode === 'calendar' || mode === 'recent') && <div className="usage-segmented usage-periods" role="group" aria-label="快捷时间范围">{(['day', 'week', 'month'] as const).map(item => <button key={item} aria-pressed={period === item} onClick={() => choosePreset(mode, item)}>{(mode === 'calendar' ? calendarLabels : recentLabels)[item]}</button>)}</div>}
        <Button variant={mode === 'custom' ? 'secondary' : 'ghost'} aria-pressed={mode === 'custom'} onClick={() => { setMode('custom'); setRangeError(''); }}>自定义</Button>
        <Button variant={mode === 'all' ? 'secondary' : 'ghost'} aria-pressed={mode === 'all'} onClick={() => { setMode('all'); setRange(allTimeRange()); setRangeError(''); }}>全部</Button>
      </div>
      <div className="usage-range-current"><strong>{rangeName(range)}</strong><span>{rangeDates(range)}</span><span title={timezone}>本地时间</span>{pendingDates && <span className="usage-pending-dates" role="status">日期待应用</span>}</div>
      {mode === 'custom' && <form className="usage-custom-range" onSubmit={e => { e.preventDefault(); try { setRange(customRange(from, to)); setRangeError(''); } catch (e) { setRangeError(errorMessage(e)); } }}><DateFields from={from} to={to} setFrom={setFrom} setTo={setTo} /><Button type="submit" variant="secondary" disabled={!pendingDates}>应用</Button></form>}
      {rangeError && <p role="alert" className="usage-warning-text">{rangeError}</p>}
    </section>
    {error && <Failure message="用量加载失败，请重试。" detail={error} retry={reload} />}
    {data && (updating || error) && <p className="usage-update-status" role="status">{updating ? <RefreshCw size={14} className="animate-spin" /> : null}{updating ? '正在更新' : '更新未完成'} · 暂显示 {loadedRange ? rangeDates(loadedRange) : '上次查询'} 的上次结果</p>}
    <div className="usage-pane" aria-busy={updating}>
      {section === 'other' ? <div className="usage-other">
        <section className="panel"><div className="usage-section-heading"><div><h2>导出用量明细</h2><p>带走当前期间的全部记录，继续分析。</p></div><Download size={20} /></div><div className="usage-other-body">
          <div className="usage-export-summary"><span>{rangeDates(range)}</span><span>{data && !error && !updating && loadedPath === path ? number(data.requests) + ' 条已发送记录' : '按当前已应用时间范围'}</span></div>
          <Button variant="secondary" onClick={() => void exportCSV()} disabled={exporting}><Download size={16} />{exporting ? '正在导出…' : '导出 CSV'}</Button>
          <p className="usage-small-note">导出请求记录，保留四类 Token、双口径金额、原币与计价依据。未计价及套餐内的请求金额留空；月费记录在套餐价格页查看，不混入请求明细。</p>
          {exportResult?.query === query && (exportResult.error ? <Failure message="用量导出失败，请重试。" detail={exportResult.error} retry={() => void exportCSV()} /> : <p role="status" className="usage-success-text">明细已导出</p>)}
        </div></section>
        <section className="panel"><div className="usage-section-heading"><div><h2>汇率参考</h2><p>币种可在页内导航旁切换；显示换算不改历史金额。</p></div></div><div className="usage-other-body"><ExchangeRatePanel data={rate.data} loading={rate.loading} error={rate.error} reload={rate.reload} /></div></section>
      </div> : !data ? updating ? <Loading /> : null : <>
        {section === 'overview' && <div className="usage-overview">
          <SummaryCards data={data} currency={currency} rate={rate.data} />
          <UsageTrend data={data} currency={currency} rate={rate.data} selection={trend} onSelectionChange={setTrend} />
          <div className="usage-two-panels"><UsageComposition groups={data.by_model} title="公开模型构成" currency={currency} rate={rate.data} measure={composition.model} onMeasureChange={model => setComposition(old => ({ ...old, model }))} /><UsageComposition groups={data.by_channel} title="渠道构成" currency={currency} rate={rate.data} measure={composition.channel} onMeasureChange={channel => setComposition(old => ({ ...old, channel }))} /></div>
        </div>}
        {section === 'details' && <UsageDetails data={data} currency={currency} rate={rate.data} selection={details} onChange={setDetails} />}
        {section === 'analysis' && <UsageAnalysis data={data} model={analysisModel} onModelChange={setAnalysisModel} />}
      </>}
    </div>
    {backfill && <HistoryBackfill channels={channels.data || []} onClose={() => setBackfill(false)} onApplied={reload} />}
  </div>;
}
