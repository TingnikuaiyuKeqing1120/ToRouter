import { useState } from 'react';
import { ChevronRight, RefreshCw, SlidersHorizontal } from 'lucide-react';
import { dateRange, isFailure, money, number, usageFormatLabel, type AttemptRecord, type Channel, type UsageLog } from '../api';
import { useResource } from '../hooks';
import { HistoryBackfill, pricingReason } from '../components/HistoryBackfill';
import { RequestDiagnostic } from '../components/RequestDiagnostic';
import { Badge, Button, DateFields, Empty, ErrorBox, Field, Input, Loading, Modal, PageHeader, Select } from '../components/ui';
import './Logs.css';

function time(value: string) { const date = new Date(value); return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false }); }
const degradationReasons = new Map([
  ['thought_signature_bypass', '请求转换：工具历史缺少 Gemini 思考签名，使用官方签名旁路值；不代表原签名已保留。'],
  ['thought_signature', '响应转换：目标格式无法承载的思考签名已剥离。'],
  ['thinking', '响应转换：目标格式无法承载的思考内容已剥离。'],
  ['reasoning_content', '响应转换：目标格式无法承载的推理内容已剥离。'],
]);
function degradationCodes(log: UsageLog) { return (log.degraded || '').split(',').map(code => code.trim()).filter(Boolean); }
function pricedAmount(log: UsageLog, measure: 'official' | 'actual') {
  if (log.not_sent) return '—';
  if (measure === 'actual' && log.actual_pricing?.resolution.layer === 'plan') return '套餐内';
  const value = measure === 'official' ? log.official_value : log.actual_cost;
  const currency = measure === 'official' ? log.official_currency : log.currency;
  return value != null && (currency === 'USD' || currency === 'CNY') ? `${money(value, currency)} ${currency}` : '—';
}

const actionTakenLabels: Record<string, string> = {
  cooling_switched: '冷却切换',
  exhausted_fallback: '本次尝试结束',
  retry: '单凭据重试',
  client_cancelled: '客户端取消',
  success: '成功',
};

function formatActionTaken(action?: string): string {
  if (!action) return '';
  return actionTakenLabels[action] || action;
}

function attemptStatusTone(status: number): 'teal' | 'amber' | 'red' | 'neutral' {
  if (status >= 200 && status < 300) return 'teal';
  if (status === 429) return 'amber';
  if (status >= 400 || status === 0) return 'red';
  return 'neutral';
}

function attemptActionTone(action?: string): 'teal' | 'amber' | 'neutral' | 'red' {
  if (action === 'success') return 'teal';
  if (action === 'client_cancelled') return 'neutral';
  if (action === 'cooling_switched' || action === 'exhausted_fallback' || action === 'retry') return 'amber';
  return 'neutral';
}

function LogDetail({ log, close }: { log: UsageLog; close: () => void }) {
  const degraded = degradationCodes(log);
  const requestID = log.request_id || log.diagnostic?.request_id;
  const values = [
    ['请求 ID', log.id], ['时间', time(log.time)], ['渠道', log.channel_name || log.channel_id || '—'],
    ['客户端密钥', log.client_key_name || '—'], ['请求模型', log.model], ['上游模型', log.upstream_model || '—'],
    ['状态', log.not_sent ? `${log.status} · 未发送` : String(log.status)], ['总耗时', `${number(log.duration_ms)} ms`], ['首 Token 延迟', log.ttft_ms > 0 ? `${number(log.ttft_ms)} ms` : '—'],
  ];
  const usageValues = [
    ['输入 Tokens', log.not_sent ? '—' : number(log.input_tokens)], ['输出 Tokens', log.not_sent ? '—' : number(log.output_tokens)],
    ['缓存读取 Tokens', log.not_sent ? '—' : number(log.cache_read_tokens)], ['缓存写入 Tokens', log.not_sent ? '—' : number(log.cache_write_tokens)],
    ['用量来源', log.not_sent ? '未发送，不计入上游调用' : log.estimated ? '用量未提供' : '上游报告'], ['实际支出', pricedAmount(log, 'actual')], ['等价API价值', pricedAmount(log, 'official')],
  ];
  const technicalValues = [
    ['入站格式', usageFormatLabel(log.inbound) || '—'], ['出站格式', usageFormatLabel(log.outbound) || '—'],
    ['原生状态', log.stateful === true ? '请求携带上游原生状态，续接需留在原状态所属域。' : log.stateful === false ? '未检测到上游原生状态' : '未记录'],
    ['转换损失', degraded.length ? `${degraded.length} 项（见下方原因）` : '无已记录的转换损失'],
  ];
  const sortedAttempts: AttemptRecord[] = (log.attempts || []).slice().sort((a, b) => (a.attempt_index ?? 0) - (b.attempt_index ?? 0));
  return (
    <Modal title="请求详情" description={`请求 #${log.id} · ${time(log.time)}`} onClose={close} wide className="logs-detail">
      <div className="logs-detail-body">
      <RequestDiagnostic log={log} />
      {log.error && <details className="logs-detail-section logs-raw-error"><summary>错误信息 · 已记录摘要</summary><pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-rose-500/20 bg-rose-500/10 p-4 font-mono text-xs text-rose-600 dark:text-rose-400">{log.error}</pre></details>}
      {sortedAttempts.length > 0 && (
        <section className="mt-5" aria-label="尝试与切换轨迹">
          <div className="mb-2 flex items-center justify-between">
            <h3 className="text-xs font-semibold text-[var(--color-text-primary)]">尝试与切换轨迹</h3>
            <span className="text-[11px] text-[var(--color-text-muted)]">共 {sortedAttempts.length} 次尝试</span>
          </div>
          <div className="space-y-2">
            {sortedAttempts.map((attempt, idx) => {
              const action = formatActionTaken(attempt.action_taken);
              const seq = attempt.attempt_index > 0 ? `#${attempt.attempt_index}` : `#${idx + 1}`;
              return (
                <div key={idx} className="rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3 text-xs">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge tone="neutral" className="font-mono">{seq}</Badge>
                      <span className="font-medium text-[var(--color-text-primary)]">
                        {attempt.channel_name || (attempt.channel_id ? `渠道 ${attempt.channel_id}` : '—')}
                      </span>
                      {attempt.credential_mask_key && (
                        <span className="font-mono text-[var(--color-text-muted)]">
                          ({attempt.credential_mask_key})
                        </span>
                      )}
                      <span className="text-[var(--color-text-muted)]">→</span>
                      <span className="font-mono text-[var(--color-text-secondary)]">
                        {attempt.upstream_model}
                      </span>
                    </div>
                    <div className="flex items-center gap-2">
                      <span className="font-mono text-[var(--color-text-muted)]">
                        {attempt.duration_ms} ms
                      </span>
                      <Badge tone={attemptStatusTone(attempt.status_code)}>
                        {attempt.status_code}
                      </Badge>
                      {action && (
                        <Badge tone={attemptActionTone(attempt.action_taken)}>
                          {action}
                        </Badge>
                      )}
                    </div>
                  </div>
                  {attempt.error_reason && (
                    <p className="mt-2 text-[11px] leading-relaxed text-[var(--color-text-muted)] break-words">
                      {attempt.error_reason}
                    </p>
                  )}
                </div>
              );
            })}
          </div>
        </section>
      )}
      <dl className="detail-grid logs-request-summary">{values.map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{value}</dd></div>)}</dl>
      <details className="logs-detail-section"><summary>Token 与计价资料</summary>
        <dl className="detail-grid">{usageValues.map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{value}</dd></div>)}</dl>
      {[['actual', log.actual_pricing], ['official', log.official_pricing]].map(([metric, raw]) => {
        const basis = raw as UsageLog['actual_pricing']; if (log.not_sent || !basis) return null;
        if (metric === 'actual' && basis.resolution.layer === 'plan') return <details key="actual" className="mt-4 text-xs"><summary>实际支出 · 套餐内</summary><p className="mt-2">按请求时的模型计费声明记录。月费按支付日期独立计入渠道支出，不分摊到本次请求。</p><a href={`#/prices?view=plan&channel_id=${log.channel_id}`} className="mt-2 inline-block">查看套餐价格 ↗</a></details>;
        return <details key={String(metric)} className="mt-4 text-xs"><summary>{metric === 'actual' ? '实际支出' : '等价API价值'}计价依据 · {basis.method === 'backfill_current' ? '当前配置补算' : basis.method === 'backfill_manual' ? '手工价补算' : '请求时计价'}</summary><dl className="detail-grid mt-2"><div><dt>计算时间</dt><dd>{time(basis.calculated_at)}</dd></div><div><dt>价表来源</dt><dd>{basis.resolution.layer} · {basis.resolution.model}{basis.resolution.ref && ` · ${basis.resolution.ref.source} / ${basis.resolution.ref.id}`}</dd></div><div><dt>单位</dt><dd>{basis.currency || '未定'} / {number(basis.unit_tokens)} Tokens</dd></div><div><dt>采用单价</dt><dd>输入 {basis.rates?.input ?? '未知'} / 输出 {basis.rates?.output ?? '未知'} / 缓存读 {basis.rates?.cache_read ?? '未知'} / 缓存写 {basis.rates?.cache_write ?? '未知'}</dd></div><div><dt>命中规则</dt><dd>{basis.matched_rules?.join('、') || '基础价'}</dd></div>{basis.missing_reason && <div><dt>缺失原因</dt><dd>{pricingReason(basis.missing_reason)}</dd></div>}{basis.note && <div><dt>备注</dt><dd>{basis.note}</dd></div>}</dl></details>;
      })}
      </details>
      <details className="logs-detail-section" open={degraded.length > 0}><summary>协议与转换资料{degraded.length ? ` · ${degraded.length} 项转换损失` : ''}</summary>
        <dl className="detail-grid">{technicalValues.map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{value}</dd></div>)}</dl>
      {degraded.length > 0 && <section className="mt-5 rounded-lg border border-amber-500/25 bg-amber-500/10 p-4 text-xs text-amber-600 dark:text-amber-400" aria-label="转换损失原因"><h3 className="mb-2 font-semibold">转换损失原因</h3><ul className="space-y-3">{degraded.map(code => <li key={code}><code className="break-all">{code}</code><p className="mt-1 leading-5">{degradationReasons.get(code) || '已记录的转换损失；此代码尚无中文解释，请保留原始代码用于排查。'}</p></li>)}</ul><p className="mt-3 leading-5">记录合并本次尝试的请求与响应损失；流式响应开始后发现的损失可能只记录在日志中。</p></section>}
      </details>
      </div>
      <div className="modal-footer">{requestID && <a className="btn btn-secondary" href={`#/logs?request_id=${encodeURIComponent(requestID)}`} onClick={close}>查看同请求全部尝试</a>}<Button variant="secondary" onClick={close}>关闭</Button></div>
    </Modal>
  );
}
function logQuery(model: string, channel: string, status: string, from: string, to: string, limit: string, requestID = '') {
  const params = dateRange(from, to); params.set('limit', limit);
  if (model.trim()) params.set('model', model.trim());
  if (channel) params.set('channel_id', channel);
  if (status.trim()) params.set('status', status.trim());
  if (requestID) params.set('request_id', requestID);
  return params.toString();
}
function querySummary(query: string, channels: Channel[]) {
  const params = new URLSearchParams(query);
  const channelID = params.get('channel_id');
  const dates = ['from', 'to'].map(key => params.get(key));
  return [params.get('model') ? `模型 ID：${params.get('model')}` : '全部模型',
    ...(params.get('request_id') ? [`关联请求：${params.get('request_id')}`] : []),
    channelID ? `渠道：${channels.find(channel => String(channel.id) === channelID)?.name || `#${channelID}`}` : '全部渠道',
    params.get('status') ? `状态 ${params.get('status')}` : '全部状态',
    dates.some(Boolean) ? `${dates[0] ? new Date(dates[0]).toLocaleDateString('zh-CN') : '不限起始'} — ${dates[1] ? new Date(dates[1]).toLocaleDateString('zh-CN') : '至今'}` : '全部时间',
    `最近 ${params.get('limit') || '100'} 条`].join(' · ');
}
export default function Logs() {
  const [requestID, setRequestID] = useState(() => new URLSearchParams(window.location.hash.split('?')[1]).get('request_id') || '');
  const [model, setModel] = useState('');
  const [channel, setChannel] = useState('');
  const [status, setStatus] = useState('');
  const [from, setFrom] = useState('');
  const [to, setTo] = useState('');
  const [limit, setLimit] = useState('100');
  const [query, setQuery] = useState(() => logQuery('', '', '', '', '', '100', requestID));
  const path = `/logs?${query}`;
  const { data, loadedPath, loading, error, reload } = useResource<UsageLog[]>(path);
  const channels = useResource<Channel[]>('/channels');
  const [selected, setSelected] = useState<UsageLog | null>(null);
  const [backfill, setBackfill] = useState(false);
  const draftQuery = logQuery(model, channel, status, from, to, limit, requestID);
  const pending = draftQuery !== query;
  const previousQuery = !!loadedPath && loadedPath !== path;
  function apply() { setQuery(draftQuery); if (draftQuery === query) reload(); }
  function reset() { setModel(''); setChannel(''); setStatus(''); setFrom(''); setTo(''); setLimit('100'); setRequestID(''); }
  return <div className="logs-page">
    <PageHeader eyebrow="REQUEST LOGS" title="请求日志" description="查看调用结果、实际支出与等价API价值。" actions={<><Button variant="secondary" onClick={() => setBackfill(true)}>补算未计价</Button><Button variant="secondary" onClick={reload} disabled={loading}><RefreshCw size={15} className={loading ? 'animate-spin' : ''} />刷新日志</Button></>} />
    <section className="panel logs-filters-panel">
      <div className="panel-toolbar"><h2 className="flex items-center gap-2"><SlidersHorizontal size={16} />筛选请求</h2><Button variant="ghost" onClick={reset}>清除筛选</Button></div>
      <form className="log-filters" onSubmit={event => { event.preventDefault(); apply(); }}>
        <Field label="请求模型ID（精确匹配）"><Input placeholder="全部请求模型" value={model} onChange={event => setModel(event.target.value)} /></Field>
        <Field label="渠道"><Select value={channel} onChange={event => setChannel(event.target.value)}><option value="">全部渠道</option>{(channels.data || []).map(channel => <option key={channel.id} value={channel.id}>{channel.name}</option>)}</Select></Field>
        <Field label="状态码（精确匹配）"><Input type="number" min={100} max={599} step={1} list="log-statuses" placeholder="全部状态" value={status} onChange={event => setStatus(event.target.value)} /><datalist id="log-statuses"><option value="200" /><option value="400" /><option value="401" /><option value="429" /><option value="499" /><option value="500" /><option value="502" /></datalist></Field>
        <DateFields from={from} to={to} setFrom={setFrom} setTo={setTo} />
        <Field label="返回条数"><Select value={limit} onChange={event => setLimit(event.target.value)}><option value="50">最近 50 条</option><option value="100">最近 100 条</option><option value="200">最近 200 条</option><option value="500">最近 500 条</option></Select></Field>
        <Button type="submit" className="self-end" disabled={loading && !pending}>查询日志</Button>
      </form>
      {requestID && <div className="logs-request-filter" aria-label="关联请求筛选"><span>关联请求：<code>{requestID}</code></span><Button variant="ghost" onClick={() => setRequestID('')}>清除关联请求筛选</Button></div>}
      {pending && <p className="logs-draft-note" role="status">筛选已更改，等待查询。</p>}
      {channels.error && <div className="logs-filter-error"><ErrorBox message={`渠道选项加载失败：${channels.error}`} retry={channels.reload} /></div>}
    </section>
    <section className="panel logs-results">
      <div className="panel-toolbar"><div className="flex items-center gap-3"><h2>调用记录</h2>{data && <Badge>{data.length} 条</Badge>}</div>{loading && data && <span className="logs-refresh-note" role="status">{previousQuery ? '正在查询新条件，当前保留上次结果' : '正在刷新，当前保留上次结果'}</span>}</div>
      {loadedPath && <p className="logs-applied-range" aria-label="已应用查询范围">已应用：{querySummary(loadedPath.slice('/logs?'.length), channels.data || [])}</p>}
      {error && <div className="logs-results-error"><ErrorBox message={`${error}${data ? '（当前保留上次结果）' : ''}`} retry={reload} /></div>}
      {!data && loading ? <Loading /> : data && !data.length ? <Empty title="暂无匹配的请求" description="调整筛选后点击查询，或通过路由器发起一次调用。" /> : data && <div className="table-scroll"><table className="data-table logs-table"><thead><tr><th>时间 / 渠道</th><th>模型</th><th className="text-right">Tokens <span className="font-normal">入 / 出</span></th><th className="text-right">等价API价值</th><th className="text-right">实际支出</th><th className="text-right">耗时</th><th>状态</th><th><span className="sr-only">操作</span></th></tr></thead><tbody>{(data || []).map((log, index) => <tr key={`${log.id}-${index}`}><td><span className="whitespace-nowrap text-xs font-medium">{time(log.time)}</span><span className="table-sub">{log.channel_name || log.channel_id || '—'}</span></td><td><span className="block max-w-48 truncate font-mono text-xs" title={log.model}>{log.model}</span><span className="table-sub">{usageFormatLabel(log.inbound) || '—'} → {usageFormatLabel(log.outbound) || '—'}</span></td><td className="text-right tabular-nums"><span className="whitespace-nowrap font-mono tabular-nums text-xs">{log.not_sent ? '—' : number(log.input_tokens)} <span className="text-[var(--color-text-muted)] opacity-40">/</span> {log.not_sent ? '—' : number(log.output_tokens)}</span>{!log.not_sent && log.estimated && <span className="table-sub !text-amber-600 dark:!text-amber-400">用量未提供</span>}</td><td className="text-right whitespace-nowrap font-mono tabular-nums text-xs">{pricedAmount(log, 'official')}</td><td className="text-right whitespace-nowrap font-mono tabular-nums text-xs">{pricedAmount(log, 'actual')}</td><td className="text-right tabular-nums"><span className="whitespace-nowrap font-mono tabular-nums text-xs">{(log.duration_ms / 1000).toFixed(2)} s</span><span className="table-sub">首字 {log.ttft_ms > 0 ? `${log.ttft_ms} ms` : '—'}</span></td><td><Badge tone={isFailure(log) ? 'red' : 'teal'}>{String(log.status)}</Badge>{log.not_sent && <span className="table-sub">未发送</span>}{log.stateful && <span className="table-sub"><Badge>原生状态</Badge></span>}{degradationCodes(log).length > 0 && <span className="table-sub"><Badge tone="amber">有损转换</Badge></span>}{log.attempts && log.attempts.length > 1 && <span className="table-sub"><Badge tone={isFailure(log) ? 'red' : 'amber'}>尝试 {log.attempts.length} 次</Badge></span>}</td><td><Button variant="ghost" aria-label={`查看请求 ${log.id} 详情`} onClick={() => setSelected(log)}><ChevronRight size={17} /></Button></td></tr>)}</tbody></table></div>}
      {!!data?.length && <p className="logs-results-footer">显示已应用范围内最近 {data.length} 条。可增加返回条数或缩小日期范围。</p>}
    </section>
    {selected && <LogDetail log={selected} close={() => setSelected(null)} />}
    {backfill && <HistoryBackfill channels={channels.data || []} onClose={() => setBackfill(false)} onApplied={reload} />}
  </div>;
}
