import { useState } from 'react';
import { api, errorMessage } from '../api';
import { useQuotaSnapshots } from '../hooks/useQuotaSnapshots';
import { quotaKey } from '../services/quotas';
import type { QuotaTarget } from '../types/quota';
import { QuotaReadout } from './QuotaReadout';
import './Quota.css';

export function CompanionQuotaDock({ targets }: { targets: QuotaTarget[] }) {
  const [expanded, setExpanded] = useState(false);
  const [error, setError] = useState('');
  const [opening, setOpening] = useState(false);
  const shown = expanded ? targets : targets.slice(0, 3);
  const quotas = useQuotaSnapshots(shown);
  async function open(target: QuotaTarget) {
    if (opening) return;
    setOpening(true); setError('');
    try { await api('/desktop/open-channel', { method: 'POST', body: JSON.stringify({ channel_id: target.channel_id, credential_id: target.credential_id }) }); }
    catch (e) { setError(errorMessage(e)); }
    finally { setOpening(false); }
  }
  if (!targets.length) return null;
  return <section aria-label="关注额度" className="mt-2">
    <h3 className="text-[10px] opacity-70">关注额度</h3>
    <div className="companion-quota-list" role="region" aria-label="关注额度列表" tabIndex={0}>
    {shown.map(target => {
      const item = quotas.items.find(row => quotaKey(row) === quotaKey(target));
      return <div key={quotaKey(target)} className="companion-quota-row">
        {item ? <><button type="button" className="block w-full truncate text-left text-[11px]" aria-label={`查看渠道 ${item.channel_name} ${item.credential_name}`} disabled={opening} onClick={() => void open(target)}>{item.channel_name} · {item.credential_name} ↗</button><QuotaReadout compact item={item} pending={quotas.pending.has(quotaKey(item))} refresh={() => quotas.refresh(item)} />{item.windows.map(window => <p key={window.id} className="text-[9px] opacity-60">{window.label} 重置 {window.resets_at && Number.isFinite(Date.parse(window.resets_at)) ? new Date(window.resets_at).toLocaleString() : '未知'}</p>)}</> : <span>{quotas.loaded ? '关注凭证不可用' : '正在读取额度…'}</span>}
      </div>;
    })}
    </div>
    {targets.length > 3 && <button type="button" className="py-1 text-[10px] opacity-70" aria-expanded={expanded} onClick={() => setExpanded(value => !value)}>{expanded ? '收起' : `展开更多（${targets.length - 3}）`}</button>}
    {(error || quotas.error) && <p role="status" className="companion-quota-status text-[10px] text-amber-500">{error || quotas.error}</p>}
  </section>;
}
