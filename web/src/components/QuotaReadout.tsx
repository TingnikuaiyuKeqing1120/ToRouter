import { RefreshCw } from 'lucide-react';
import type { QuotaSnapshot } from '../types/quota';

function time(value?: string) {
  const date = value ? new Date(value) : null;
  return date && Number.isFinite(date.getTime()) ? date.toLocaleString() : '未知';
}
export function QuotaReadout({ item, pending = false, refresh, compact = false }: { item: QuotaSnapshot; pending?: boolean; refresh?: () => void; compact?: boolean }) {
  const expired = item.stale || !!item.expires_at && Date.parse(item.expires_at) <= Date.now();
  const status = item.error ? '更新失败' : !item.checked_at ? '未查询' : expired ? '已过期' : '';
  return <div className={`quota-readout ${compact ? 'quota-readout-compact' : ''}`}>
    <div className="quota-readout-values" aria-label={item.kind === 'balance' ? '余额' : '剩余额度'}>
      {item.kind === 'balance' ? item.balances.length ? item.balances.map(balance => <span key={balance.currency}>{balance.currency} <strong>{balance.total.trim() || '—'}</strong></span>) : <span>余额 —</span> : item.windows.length ? item.windows.map(window => <span key={window.id} title={`重置：${time(window.resets_at)}`}>{window.label} <strong>{window.remaining_percent == null || !Number.isFinite(window.remaining_percent) ? '—' : `${window.remaining_percent.toFixed(0)}%`}</strong>{!compact && <small>重置 {time(window.resets_at)}</small>}</span>) : <span>剩余额度 —</span>}
    </div>
    <div className="quota-readout-meta">
      {!item.enabled && <span>已停用</span>}
      {status && <span className="quota-readout-warning">{status}</span>}
      {!compact && <span>更新 {time(item.checked_at)}</span>}
      {refresh && <button type="button" className="quota-refresh" aria-label={`刷新额度 ${item.credential_name}`} title="刷新此凭证额度" disabled={pending || !item.enabled} onClick={refresh}><RefreshCw size={13} className={pending ? 'animate-spin' : ''} /><span>刷新</span></button>}
    </div>
    {item.error && !compact && <p className="quota-readout-warning" role="status">{item.error}</p>}
  </div>;
}
