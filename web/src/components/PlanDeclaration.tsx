import { useRef, useState } from 'react';
import { api, errorMessage, type Channel } from '../api';
import type { BillingTag } from '../types/planPricing';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { ChannelChoice } from './ChannelPicker';
import { Button, ErrorBox, Modal } from './ui';

export function PlanDeclaration({ channels, onSaved, onClose }: { channels: Channel[]; onSaved: () => void; onClose: () => void }) {
  const [initial] = useState(() => Object.fromEntries(channels.map(channel => [channel.id, channel.tags?.find(tag => ['billing:free', 'billing:plan', 'billing:payg', 'billing:unknown'].includes(tag)) || 'billing:unknown'])) as Record<number, BillingTag>);
  const baseline = useRef(initial);
  const [draft, setDraft] = useState(initial), [turningOff, setTurningOff] = useState<number | null>(null), [busy, setBusy] = useState(false), [error, setError] = useState('');
  const dirty = JSON.stringify(draft) !== JSON.stringify(baseline.current);
  async function save() {
    if (busy) return false;
    const changes = channels.filter(channel => draft[channel.id] !== baseline.current[channel.id]).map(channel => ({ channel_id: channel.id, billing_tag: draft[channel.id] }));
    if (!changes.length) return true;
    setBusy(true); setError('');
    try { await api('/channels/billing', { method: 'PUT', body: JSON.stringify({ changes }) }); baseline.current = { ...draft }; onSaved(); return true; }
    catch (cause) { setError(errorMessage(cause)); return false; }
    finally { setBusy(false); }
  }
  const canLeave = useUnsavedChanges({ dirty, busy, save, discard: () => setDraft({ ...baseline.current }) });
  async function close() { if (await canLeave()) onClose(); }
  return <Modal title="套餐声明" description="亮起表示渠道默认计费为套餐。保存后生效。" busy={busy} onClose={() => void close()} className="price-channel-modal">
    {error && <ErrorBox message={error} />}
    <div className="price-channel-list">{channels.map(channel => <ChannelChoice key={channel.id} channel={channel} selected={draft[channel.id] === 'billing:plan'} disabled={busy} label={`声明套餐 ${channel.name}`} onToggle={() => draft[channel.id] === 'billing:plan' ? setTurningOff(channel.id) : setDraft(current => ({ ...current, [channel.id]: 'billing:plan' }))} onNavigate={() => { void (async () => { if (await canLeave()) { onClose(); window.location.hash = `#/channels?channel_id=${channel.id}`; } })(); }} />)}</div>
    <div className="modal-footer"><Button variant="ghost" disabled={busy} onClick={() => { setDraft({ ...initial }); setTurningOff(null); }}>恢复修改前</Button><Button disabled={busy || !dirty} onClick={() => { void save().then(ok => { if (ok) onClose(); }); }}>{busy ? '保存中…' : '确认保存声明'}</Button></div>
    {turningOff != null && <Modal title="转出套餐" description="选择渠道之后的默认计费类型，已有月费记录会保留。" onClose={() => setTurningOff(null)}><div className="price-billing-options">{([['billing:free', '免费'], ['billing:payg', '按量付费'], ['billing:unknown', '未知']] as const).map(([tag, label]) => <Button key={tag} variant="secondary" onClick={() => { setDraft(current => ({ ...current, [turningOff]: tag })); setTurningOff(null); }}>{label}</Button>)}</div><div className="modal-footer"><Button variant="ghost" onClick={() => setTurningOff(null)}>取消</Button></div></Modal>}
  </Modal>;
}
