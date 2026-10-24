import { Area, AreaChart, Bar, BarChart, CartesianGrid, Cell, Pie, PieChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import type { Currency, ExchangeRate, UsageDay, UsageGroup, UsageInputBucket, UsageMetrics, UsageSummary } from '../../api';
import { Empty } from '../ui';
import { compactNumber, formatMetric, formatPercent, measureLabels, metricValue, pricingValues, unpricedRequests, type UsageMeasure } from './metrics';
import './usage-charts.css';

type MoneyProps = { currency: Currency; rate: ExchangeRate | null };
export type UsageTrendSelection = { dimension: 'total' | 'model' | 'channel'; measure: UsageMeasure };
type Series = { key: string; name: string; color: string; groups?: UsageGroup[] };
type TrendRow = { date: string; [key: string]: string | number | null };
const colors = ['#5274e8', '#13a58e', '#bd73df', '#e48b38', '#da628d', '#689f38', '#3e9dc4', '#a78146'];
const tokenSeries: Series[] = [
  { key: 'input', name: '未命中输入（含缓存写入）', color: colors[0] },
  { key: 'read', name: '缓存命中', color: colors[2] },
  { key: 'output', name: '输出', color: colors[1] },
];
const number = (value: number) => new Intl.NumberFormat('zh-CN').format(value);
const identity = (group: UsageGroup) => group.channel_id != null ? `channel:${group.channel_id}` : `model:${group.name}`;
const knownSum = (values: (number | null)[]) => values.every(value => value == null) ? null : values.reduce<number>((sum, value) => sum + (value ?? 0), 0);
function exactValue(value: number | null, measure: UsageMeasure, currency: Currency) {
  if (value == null) return '未知';
  return measure === 'tokens' ? `${number(value)} Token` : `${formatMetric(value, measure, currency)} ${currency}`;
}
function MeasureSwitch({ value, onChange }: { value: UsageMeasure; onChange: (measure: UsageMeasure) => void }) {
  return <div className="usage-chart-switch" role="group" aria-label="图表指标">{(['tokens', 'actual', 'official'] as const).map(measure => <button key={measure} type="button" aria-pressed={value === measure} onClick={() => onChange(measure)}>{measureLabels[measure]}</button>)}</div>;
}
function groupSeries(groups: UsageGroup[], measure: UsageMeasure, currency: Currency, rate: ExchangeRate | null): Series[] {
  const identities = groups.map(identity).sort();
  const ranked = [...groups].sort((a, b) => (metricValue(b, measure, currency, rate) ?? -1) - (metricValue(a, measure, currency, rate) ?? -1) || identity(a).localeCompare(identity(b)));
  const result: Series[] = ranked.slice(0, 5).map(group => ({
    key: `group${identities.indexOf(identity(group))}`,
    name: groups.filter(other => other.name === group.name).length > 1 ? `${group.name} #${group.channel_id}` : group.name || '未命名',
    color: colors[identities.indexOf(identity(group)) % colors.length], groups: [group],
  }));
  if (ranked.length > 5) result.push({ key: 'other', name: '其他', color: '#8893a7', groups: ranked.slice(5) });
  return result;
}
function allDates(days: UsageDay[]) {
  const dates = days.map(day => day.date).sort();
  if (!dates.length) return [];
  const result: string[] = [];
  const end = Date.parse(`${dates[dates.length - 1]}T00:00:00Z`);
  for (let day = Date.parse(`${dates[0]}T00:00:00Z`); day <= end; day += 86400000) result.push(new Date(day).toISOString().slice(0, 10));
  return result;
}
function tokenParts(metrics: UsageMetrics): (number | null)[] {
  if (!metrics.known_usage_requests && metrics.requests > 0) return [null, null, null];
  return [metrics.plain_input_tokens + metrics.cache_write_tokens, metrics.cache_read_tokens, metrics.output_tokens];
}
function trendRows(data: UsageSummary, series: Series[], measure: UsageMeasure, currency: Currency, rate: ExchangeRate | null, grouped: boolean): TrendRow[] {
  const byDay = new Map(data.daily.map(day => [day.date, day]));
  const groupDays = new Map(series.flatMap(item => (item.groups || []).map(group => [identity(group), new Map(group.daily.map(day => [day.date, day]))] as const)));
  return allDates(data.daily).map(date => {
    const row: TrendRow = { date };
    const day = byDay.get(date);
    if (grouped) {
      series.forEach(item => {
        const active = (item.groups || []).map(group => groupDays.get(identity(group))?.get(date)).filter((entry): entry is UsageDay => !!entry && (entry.requests > 0 || measure === 'actual' && (entry.plan_charges || 0) > 0));
        row[item.key] = active.length ? knownSum(active.map(entry => metricValue(entry, measure, currency, rate))) : 0;
      });
    } else if (measure === 'tokens') {
      const parts = day ? tokenParts(day) : [0, 0, 0];
      series.forEach((item, index) => { row[item.key] = parts[index]; });
    } else row.amount = day ? metricValue(day, measure, currency, rate) : 0;
    return row;
  });
}
function CoverageNote({ metrics, measure }: { metrics: UsageMetrics; measure: UsageMeasure }) {
  const unknown = measure === 'tokens' ? metrics.unknown_usage_requests : unpricedRequests(metrics, measure);
  return <p className="usage-chart-note">{measure === 'tokens' ? (unknown > 0 ? `含已记录 Token；${number(unknown)} 次请求用量未知，未知不按 0 计入。` : '未命中输入包含缓存写入；缓存命中与输出分别计量。') : `仅展示已计价部分${unknown > 0 ? `；${number(unknown)} 次请求未计价` : ''}。`}{measure === 'actual' && <> 月费按支付日计入总体与渠道，不分摊到模型。{!!metrics.unknown_plan_charges && ` ${number(metrics.unknown_plan_charges)} 笔月费金额未知。`}</>}<span> 空白表示未知{measure === 'actual' ? '或未分摊' : ''}。</span></p>;
}
function Legend({ series }: { series: Series[] }) {
  return <ul className="usage-chart-legend">{series.map(item => <li key={item.key}><i style={{ background: item.color }} /><span>{item.name}</span></li>)}</ul>;
}
function TrendTable({ rows, series, measure, currency }: { rows: TrendRow[]; series: Series[]; measure: UsageMeasure; currency: Currency }) {
  return <details className="usage-chart-data"><summary>查看趋势数据</summary><div className="usage-chart-table-scroll"><table><caption>每日{measureLabels[measure]}（{measure === 'tokens' ? 'Token' : currency}）</caption><thead><tr><th scope="col">日期</th>{series.map(item => <th key={item.key} scope="col">{item.name}</th>)}</tr></thead><tbody>{rows.map(row => <tr key={row.date}><th scope="row">{row.date}</th>{series.map(item => <td key={item.key}>{row[item.key] == null ? '未知' : formatMetric(row[item.key] as number, measure, currency)}</td>)}</tr>)}</tbody></table></div></details>;
}

export function UsageTrend({ data, currency, rate, selection, onSelectionChange }: { data: UsageSummary; selection: UsageTrendSelection; onSelectionChange: (next: UsageTrendSelection) => void } & MoneyProps) {
  const { dimension, measure } = selection;
  const groups = dimension === 'model' ? data.by_model : data.by_channel;
  const series: Series[] = dimension !== 'total' ? groupSeries(groups, measure, currency, rate) : measure === 'tokens' ? tokenSeries : [{ key: 'amount', name: measureLabels[measure], color: colors[0] }];
  const rows = trendRows(data, series, measure, currency, rate, dimension !== 'total');
  const cannotConvert = measure !== 'tokens' && pricingValues(data, measure).count > 0 && metricValue(data, measure, currency, rate) == null;
  const hasValue = rows.some(row => series.some(item => row[item.key] != null));
  const hasRecords = data.requests > 0 || measure === 'actual' && (data.plan_charges || 0) > 0;
  return <section className="panel usage-chart-panel" aria-label="用量趋势">
    <header className="usage-chart-header"><div><h2>用量趋势</h2><span className="usage-chart-unit">按日堆叠 · {measure === 'tokens' ? 'Token' : currency}</span><div className="usage-chart-dimensions" role="group" aria-label="趋势维度">{([['total', '总体'], ['model', '公开模型'], ['channel', '渠道']] as const).map(([value, label]) => <button type="button" key={value} aria-pressed={dimension === value} onClick={() => onSelectionChange({ ...selection, dimension: value })}>{label}</button>)}</div></div><MeasureSwitch value={measure} onChange={next => onSelectionChange({ ...selection, measure: next })} /></header>
    {!hasRecords || !rows.length ? <Empty title="所选期间暂无用量记录" /> : cannotConvert ? <Empty title="暂不能合计不同币种" description="缺少有效汇率，请到导出与汇率更新汇率，或查看原币明细。" /> : !hasValue ? <Empty title={measure === 'actual' && data.plan_requests ? '套餐月费未分摊到模型' : measure === 'tokens' ? '所选期间用量未知' : '所选期间尚未计价'} description="未知或未分摊的金额保留为空，不按零计入。" /> : <>
      <div className="usage-trend-canvas" role="img" aria-label={`${dimension === 'total' ? '总体' : dimension === 'model' ? '公开模型' : '渠道'}每日${measureLabels[measure]}趋势，单位${measure === 'tokens' ? 'Token' : currency}`}>
        <ResponsiveContainer width="100%" height={270}><AreaChart data={rows} margin={{ top: 12, right: 12, left: 0, bottom: 4 }} accessibilityLayer><CartesianGrid stroke="var(--color-border)" strokeDasharray="3 5" vertical={false} /><XAxis dataKey="date" tickFormatter={value => String(value).slice(5)} tick={{ fill: 'var(--color-text-secondary)', fontSize: 12 }} axisLine={false} tickLine={false} minTickGap={25} /><YAxis tickFormatter={value => formatMetric(Number(value), measure, currency, true)} tick={{ fill: 'var(--color-text-secondary)', fontSize: 12 }} axisLine={false} tickLine={false} width={60} allowDecimals={measure !== 'tokens'} /><Tooltip contentStyle={tooltipStyle} formatter={(value: number, name: string) => [exactValue(value, measure, currency), name]} labelFormatter={value => String(value)} />{series.map(item => <Area key={item.key} type="linear" name={item.name} dataKey={item.key} stackId="usage" stroke={item.color} fill={item.color} fillOpacity={0.18} strokeWidth={2} connectNulls={false} isAnimationActive={false} />)}</AreaChart></ResponsiveContainer>
      </div><Legend series={series} />
    </>}
    <CoverageNote metrics={data} measure={measure} />
    {rows.length > 0 && !cannotConvert && <TrendTable rows={rows} series={series} measure={measure} currency={currency} />}
  </section>;
}
const tooltipStyle = { borderRadius: 10, border: '1px solid var(--color-border)', background: 'var(--color-surface-1)', color: 'var(--color-text-primary)', fontSize: 12, boxShadow: '0 6px 20px rgba(0,0,0,.12)' };

export function UsageComposition({ groups, title, currency, rate, measure, onMeasureChange }: { groups: UsageGroup[]; title: string; measure: UsageMeasure; onMeasureChange: (next: UsageMeasure) => void } & MoneyProps) {
  const series = groupSeries(groups, measure, currency, rate);
  const entries = series.map(item => ({ ...item, value: knownSum((item.groups || []).map(group => metricValue(group, measure, currency, rate))) }));
  const total = entries.reduce((sum, entry) => sum + (entry.value ?? 0), 0);
  const requests = groups.reduce((sum, group) => sum + group.requests, 0);
  const cannotConvert = measure !== 'tokens' && groups.some(group => pricingValues(group, measure).count > 0 && metricValue(group, measure, currency, rate) == null);
  const unknown = groups.reduce((sum, group) => sum + (measure === 'tokens' ? group.unknown_usage_requests : unpricedRequests(group, measure)), 0);
  const hasRecords = requests > 0 || measure === 'actual' && groups.some(group => (group.plan_charges || 0) > 0);
  const planOnly = measure === 'actual' && groups.some(group => (group.plan_requests || 0) > 0) && entries.every(entry => entry.value == null);
  return <section className="panel usage-chart-panel usage-composition-panel" aria-label={title}><header className="usage-chart-header"><div><h2>{title}</h2><span className="usage-chart-unit">{measure === 'tokens' ? 'Token' : currency}</span></div><MeasureSwitch value={measure} onChange={onMeasureChange} /></header>
    {!hasRecords ? <Empty title="所选期间暂无用量记录" /> : cannotConvert ? <Empty title="暂不能合计不同币种" description="缺少有效汇率，请查看原币明细。" /> : <div className="usage-composition-body">
      {total > 0 ? <div className="usage-donut" role="img" aria-label={`${title}，${measureLabels[measure]}构成`}><ResponsiveContainer width="100%" height={190}><PieChart><Pie data={entries.filter(entry => entry.value != null && entry.value > 0)} dataKey="value" nameKey="name" innerRadius={56} outerRadius={82} paddingAngle={2} stroke="var(--color-surface-1)" isAnimationActive={false}>{entries.filter(entry => entry.value != null && entry.value > 0).map(entry => <Cell key={entry.key} fill={entry.color} />)}</Pie><Tooltip contentStyle={tooltipStyle} formatter={(value: number, name: string) => [exactValue(value, measure, currency), name]} /></PieChart></ResponsiveContainer><div className="usage-donut-total"><strong title={exactValue(total, measure, currency)}>{formatMetric(total, measure, currency, true)}</strong><span>{measure === 'tokens' ? 'Token' : currency}</span></div></div> : <div className="usage-chart-zero" role="status"><strong>{entries.some(entry => entry.value != null) ? formatMetric(0, measure, currency) : planOnly ? '套餐内' : '未知'}</strong><span>{entries.some(entry => entry.value != null) ? '已记录总量为 0，无比例可展示' : planOnly ? '月费未分摊到模型' : measure === 'tokens' ? '尚无已知用量' : '尚无已计价金额'}</span></div>}
      <ul className="usage-composition-list" aria-label={`${title}数值`}>{entries.map(entry => <li key={entry.key}><i style={{ background: entry.color }} /><span className="usage-composition-name">{entry.name}</span><span className="usage-composition-value" title={exactValue(entry.value, measure, currency)}>{entry.value == null ? '未知' : formatMetric(entry.value, measure, currency, true)}</span><span className="usage-composition-percent">{entry.value == null ? '—' : formatPercent(entry.value, total)}</span></li>)}</ul>
    </div>}
    {hasRecords && <p className="usage-chart-note">{measure === 'tokens' ? '按已记录 Token 计算比例。' : '仅按已计价部分计算比例，套餐月费不分摊到模型。'}{unknown > 0 && ` ${number(unknown)} 次请求${measure === 'tokens' ? '用量未知' : '未计价'}。`}</p>}
  </section>;
}

export function UsageInputDistribution({ buckets, unknownRequests }: { buckets: UsageInputBucket[]; unknownRequests: number }) {
  const requests = buckets.reduce((sum, bucket) => sum + bucket.requests, 0);
  return <section className="panel usage-chart-panel" aria-label="输入长度分布"><header className="usage-chart-header"><h2>每次输入长度</h2><span className="usage-chart-unit">请求数</span></header>
    {requests === 0 ? <Empty title={unknownRequests > 0 ? '尚无已知输入长度' : '所选期间暂无用量记录'} /> : <div className="usage-histogram" role="img" aria-label="每次请求总输入 Token 长度区间的请求数"><ResponsiveContainer width="100%" height={240}><BarChart data={buckets} margin={{ top: 12, right: 12, left: 0, bottom: 4 }} accessibilityLayer><CartesianGrid stroke="var(--color-border)" strokeDasharray="3 5" vertical={false} /><XAxis dataKey="label" tick={{ fill: 'var(--color-text-secondary)', fontSize: 12 }} interval={0} axisLine={false} tickLine={false} /><YAxis allowDecimals={false} width={45} axisLine={false} tickLine={false} tick={{ fill: 'var(--color-text-secondary)', fontSize: 12 }} tickFormatter={value => compactNumber(Number(value))} /><Tooltip contentStyle={tooltipStyle} formatter={(value: number) => [`${number(value)} 次`, '请求数']} labelFormatter={label => `总输入 ${label} Token`} /><Bar dataKey="requests" name="请求数" fill={colors[0]} radius={[4, 4, 0, 0]} isAnimationActive={false} /></BarChart></ResponsiveContainer></div>}
    <p className="usage-chart-note">有用量记录 {number(requests)} 次请求 · 用量未知 {number(unknownRequests)} 次。按每次总输入 Token 分组（含缓存读写）。</p>
    <details className="usage-chart-data"><summary>查看区间数据</summary><ul className="usage-bucket-values">{buckets.map(bucket => <li key={bucket.from_tokens}><span>{number(bucket.from_tokens)}–{bucket.to_tokens == null ? '以上' : `${number(bucket.to_tokens)}（不含）`} Token</span><strong>{number(bucket.requests)} 次</strong></li>)}</ul></details>
  </section>;
}

export function TokenComposition({ metrics }: { metrics: UsageMetrics }) {
  const parts = tokenParts(metrics);
  const total = knownSum(parts);
  return <div className="usage-token-composition"><p className="usage-chart-unit">Token 构成</p><div className="usage-token-bar" role="img" aria-label={total == null ? 'Token 构成未知' : total === 0 ? '已记录 Token 为 0' : tokenSeries.map((item, index) => `${item.name} ${formatPercent(parts[index] || 0, total)}`).join('，')}>{total != null && total > 0 && tokenSeries.map((item, index) => <span key={item.key} style={{ width: `${(parts[index] || 0) / total * 100}%`, background: item.color }} />)}</div><ul className="usage-token-values">{tokenSeries.map((item, index) => <li key={item.key}><i style={{ background: item.color }} /><span>{item.name}</span><strong title={exactValue(parts[index], 'tokens', 'USD')}>{parts[index] == null ? '未知' : compactNumber(parts[index]!)}</strong><span>{formatPercent(parts[index] || 0, total || 0)}</span></li>)}</ul>{metrics.unknown_usage_requests > 0 && <p className="usage-chart-note">{number(metrics.unknown_usage_requests)} 次请求用量未知；比例基于已记录 Token。</p>}</div>;
}
