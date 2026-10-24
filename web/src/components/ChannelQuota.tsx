import { useState } from 'react';
import type { QuotaSnapshot } from '../types/quota';
import { useQuotaSnapshots } from '../hooks/useQuotaSnapshots';
import { quotaKey } from '../services/quotas';
import { QuotaReadout } from './QuotaReadout';
import './Quota.css';

export function ChannelQuota({ items }: { items: QuotaSnapshot[] }) {
  const [expanded, setExpanded] = useState(false);
  const visible = items.length === 1 || expanded;
  const quotas = useQuotaSnapshots(visible ? items : [], visible);
  const rows = items.map(item => quotas.items.find(row => quotaKey(row) === quotaKey(item)) || item);
  if (!items.length) return null;
  return <section className="channel-quota" aria-label="凭证余额">
    {items.length > 1 && <button type="button" className="channel-quota-toggle" aria-expanded={expanded} onClick={() => setExpanded(value => !value)}>凭证余额 · {items.length} 个凭证 <span>{expanded ? '收起' : '展开'}</span></button>}
    {visible && <>{rows.map(item => <div key={quotaKey(item)} className="channel-quota-row"><span className="channel-quota-name">{item.credential_name}</span><QuotaReadout item={item} pending={quotas.pending.has(quotaKey(item))} refresh={() => quotas.refresh(item)} /></div>)}{quotas.error && <p role="status" className="quota-readout-warning">{quotas.error}</p>}</>}
  </section>;
}
