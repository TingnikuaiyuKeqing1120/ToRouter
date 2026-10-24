import { Fragment, useState } from 'react';
import { ChevronDown, ChevronRight, RefreshCw, RotateCcw, Search, Server } from 'lucide-react';
import { api, errorMessage, type RuntimeCredentialStatus, type RuntimeModelStatus } from '../api';
import { useRuntimeStatus } from '../hooks/useRuntimeStatus';
import { useConfirm } from '../components/Confirm';
import { Badge, Button, Empty, ErrorBox, Input, Loading, PageHeader, useToast } from '../components/ui';
import './Status.css';

function time(value: string) { return new Date(value).toLocaleString('zh-CN', { hour12: false }); }
function resettable(value: Pick<RuntimeModelStatus, 'red' | 'failures' | 'cooldown_until'>) {
  return value.red || value.failures > 0 || !!value.cooldown_until && new Date(value.cooldown_until).getTime() > Date.now();
}
function State({ value }: { value: Pick<RuntimeModelStatus, 'red' | 'reason' | 'failures' | 'cooldown_until'> }) {
  return <><Badge tone={value.red ? 'red' : 'teal'}>{value.red ? value.reason || '发现异常' : '未发现异常'}</Badge>{value.failures > 0 && <span className="table-sub">连续失败 {value.failures} 次</span>}{value.cooldown_until && new Date(value.cooldown_until).getTime() > Date.now() && <span className="table-sub">冷却至 {time(value.cooldown_until)}</span>}</>;
}

export default function Status() {
  const resource = useRuntimeStatus();
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [search, setSearch] = useState('');
  const [onlyProblems, setOnlyProblems] = useState(false);
  const [actionError, setActionError] = useState<{ row: RuntimeCredentialStatus; operation: 'reset' | 'refresh-quota'; model?: string; message: string } | null>(null);
  const confirm = useConfirm();
  const toast = useToast();
  const rows = [...(resource.data?.rows || [])].sort((a, b) => Number(b.red) - Number(a.red) || a.channel_name.localeCompare(b.channel_name, 'zh-CN') || a.channel_id - b.channel_id || a.credential_name.localeCompare(b.credential_name, 'zh-CN') || a.credential_id.localeCompare(b.credential_id));
  const term = search.trim().toLowerCase();
  const matchingModels = (row: RuntimeCredentialStatus) => (row.models || []).filter(model => `${model.name} ${model.model}`.toLowerCase().includes(term));
  const matchesIdentity = (row: RuntimeCredentialStatus) => `${row.channel_name} ${row.channel_id} ${row.credential_name} ${row.credential_id}`.toLowerCase().includes(term);
  const visible = rows.filter(row => (!onlyProblems || row.red) && (!term || matchesIdentity(row) || matchingModels(row).length));

  async function act(row: RuntimeCredentialStatus, operation: 'reset' | 'refresh-quota', model?: string) {
    if (busy) return;
    setBusy(true);
    try {
      if (operation === 'reset' && !await confirm({ title: '清除异常状态？', description: `清除「${row.channel_name} / ${row.credential_name}${model ? ` / ${model}` : '（整个凭据，含未显示模型）'}」的异常状态并允许重试，不改变启停。`, confirmLabel: '清除并允许重试' })) return;
      setActionError(null);
      await api(`/status/${operation}`, { method: 'POST', body: JSON.stringify({ channel_id: row.channel_id, credential_id: row.credential_id, ...(model ? { model } : {}) }) });
      resource.reload();
      toast(operation === 'reset' ? '已清除异常状态，后续请求可重试' : '额度已刷新');
    } catch (e) { setActionError({ row, operation, model, message: errorMessage(e) }); }
    finally { setBusy(false); }
  }

  return <div className="runtime-status-page">
    <PageHeader eyebrow="RUNTIME STATUS" title="运行状态" description="按渠道与凭据查看运行异常。绿色表示未发现异常。" actions={<Button variant="secondary" disabled={resource.loading} onClick={resource.reload}><RefreshCw size={15} className={resource.loading ? 'animate-spin' : ''} />刷新状态</Button>} />
    <section className="panel runtime-status-workbench">
      <div className="runtime-status-toolbar"><div className="search-field"><Search size={15} /><Input aria-label="搜索运行状态" placeholder="搜索渠道 / 凭据 / 模型" value={search} onChange={event => setSearch(event.target.value)} /></div><label className="runtime-problems-filter"><input type="checkbox" className="checkbox" checked={onlyProblems} onChange={event => setOnlyProblems(event.target.checked)} />仅异常</label>{(term || onlyProblems) && <Button variant="ghost" onClick={() => { setSearch(''); setOnlyProblems(false); }}>清除筛选</Button>}<span className="runtime-status-count" role="status">显示 {visible.length} / {rows.length} 个凭据 · 异常 {visible.filter(row => row.red).length} 个</span></div>
      <p className="runtime-status-update">每 5 秒自动刷新{resource.updatedAt != null && ` · ${resource.loading ? '正在刷新，上次' : '更新于'} ${new Date(resource.updatedAt).toLocaleTimeString('zh-CN', { hour12: false })}`}</p>
      {resource.error && <div className="p-4"><ErrorBox message={`${resource.error}${resource.data ? '（当前保留上次状态）' : ''}`} retry={resource.reload} /></div>}
      {!resource.data && resource.loading ? <Loading /> : !resource.data ? null : !rows.length ? <Empty title="暂无运行状态" description="添加上游渠道与凭据后，可在这里查看状态。" /> : !visible.length ? <Empty title="暂无匹配的运行状态" description="调整搜索条件或清除筛选。" action={<Button variant="secondary" onClick={() => { setSearch(''); setOnlyProblems(false); }}>显示全部状态</Button>} /> : <div className="table-scroll"><table className="data-table runtime-status-table" aria-label="渠道凭据运行状态">
        <thead><tr><th>渠道标识 / 名 / ID</th><th>凭据名</th><th>当前状态</th><th>详情</th></tr></thead>
        <tbody>{visible.map(row => {
          const key = `${row.channel_id}:${row.credential_id}`;
          const open = expanded.has(key);
          const identityMatch = matchesIdentity(row);
          const models = [...(term && !identityMatch ? matchingModels(row) : row.models || [])].filter(model => !onlyProblems || model.red).sort((a, b) => Number(b.red) - Number(a.red) || (a.name || a.model).localeCompare(b.name || b.model, 'zh-CN') || a.model.localeCompare(b.model));
          const failed = actionError?.row.channel_id === row.channel_id && actionError.row.credential_id === row.credential_id ? actionError : null;
          return <Fragment key={key}><tr>
            <td><div className="flex items-center gap-2"><Server size={15} className="shrink-0 text-[var(--color-text-muted)]" /><span className="font-medium break-words">{row.channel_name}</span></div><span className="table-sub font-mono">#{row.channel_id}</span></td>
            <td className="break-words">{row.credential_name || '默认凭据'}{term && !identityMatch && <span className="runtime-matched-models">匹配模型：{matchingModels(row).slice(0, 3).map(model => model.name && model.name !== model.model ? `${model.name} (${model.model})` : model.model).join('、')}{matchingModels(row).length > 3 ? ` 等 ${matchingModels(row).length} 个` : ''}</span>}</td>
            <td><State value={row} /></td>
            <td><Button variant="ghost" aria-label={`${open ? '收起' : '展开'} ${row.channel_name} / ${row.credential_name} 详情`} aria-expanded={open} onClick={() => setExpanded(previous => { const next = new Set(previous); if (open) next.delete(key); else next.add(key); return next; })}>{open ? <ChevronDown size={15} /> : <ChevronRight size={15} />}详情</Button></td>
          </tr>{open && <tr className="runtime-detail-row"><td colSpan={4}>
            <div className="runtime-detail">
              {failed && <div className="runtime-action-error"><ErrorBox message={failed.message} retry={busy ? undefined : () => void act(failed.row, failed.operation, failed.model)} /></div>}
              {(resettable(row) || row.models.some(resettable) || row.quota_supported) && <div className="flex flex-wrap items-center gap-2 mb-3">{(resettable(row) || row.models.some(resettable)) && <Button variant="secondary" disabled={busy} onClick={() => void act(row, 'reset')}><RotateCcw size={14} />恢复整个凭据</Button>}{row.quota_supported && <Button variant="secondary" disabled={busy} onClick={() => void act(row, 'refresh-quota')}><RefreshCw size={14} />刷新额度</Button>}</div>}
              {models.length ? <table className="data-table runtime-model-table" aria-label={`${row.channel_name} / ${row.credential_name} 模型状态`}><thead><tr><th>模型名 / ID</th><th>上游</th><th>状态</th><th>最近请求信息</th></tr></thead><tbody>{models.map(model => <tr key={model.model}>
                <td><span className="font-medium break-words">{model.name || model.model}</span>{model.name && model.name !== model.model && <span className="table-sub font-mono break-all">{model.model}</span>}</td>
                <td className="text-xs break-all">{model.upstream || '—'}</td>
                <td><State value={model} />{resettable(model) && <Button variant="ghost" className="runtime-model-reset" disabled={busy} aria-label={`恢复模型 ${model.model}`} onClick={() => void act(row, 'reset', model.model)}>清除异常状态</Button>}</td>
                <td>{model.last_request ? <><span className="text-xs whitespace-nowrap">{time(model.last_request.at)}</span><span className="table-sub font-mono">{model.last_request.status || '连接失败'} · {model.last_request.duration_ms} ms</span>{model.last_request.error && <span className="table-sub break-words">{model.last_request.error}</span>}</> : <span className="text-xs text-[var(--color-text-muted)]">暂无请求记录</span>}</td>
              </tr>)}</tbody></table> : <p className="text-xs text-[var(--color-text-muted)]">{term || onlyProblems ? '当前筛选下无匹配模型' : '暂无模型运行记录'}</p>}
            </div>
          </td></tr>}</Fragment>;
        })}</tbody>
      </table></div>}
    </section>
  </div>;
}
