import { Check, ExternalLink } from 'lucide-react';
import type { PriceChannelOption } from '../types/planPricing';
import { ProviderIcon } from './ProviderIcons';
import { Button, Empty, Modal } from './ui';

export function ChannelChoice({ channel, selected, disabled, label, onToggle, onNavigate }: { channel: PriceChannelOption; selected: boolean; disabled?: boolean; label: string; onToggle: () => void; onNavigate?: () => void }) {
  return <div className={`price-channel-choice ${selected ? 'is-selected' : ''}`}>
    <button type="button" className="price-channel-choice-main" aria-label={label} aria-pressed={selected} disabled={disabled} onClick={onToggle}>
      <ProviderIcon name={channel.name} baseUrl={channel.base_url || channel.endpoints?.[0]?.base_url} size={22} /><span>{channel.name}</span><span className="price-channel-check" aria-hidden="true">{selected && <Check size={15} />}</span>
    </button>{channel.deleted ? <span className="price-channel-history">历史</span> : <Button variant="ghost" aria-label={`编辑渠道 ${channel.name}`} title="前往渠道编辑" disabled={disabled} onClick={onNavigate}><ExternalLink size={15} /></Button>}
  </div>;
}
export function ChannelPicker({ channels, selected, onChange, onClose }: { channels: PriceChannelOption[]; selected: number[]; onChange: (ids: number[]) => void; onClose: () => void }) {
  return <Modal title="选择展示渠道" description="只调整本页展示范围。" onClose={onClose} className="price-channel-modal">
    <div className="price-channel-picker-actions"><Button variant="secondary" onClick={() => onChange(channels.map(channel => channel.id))}>全选渠道</Button><Button variant="ghost" onClick={() => onChange([])}>清除选择</Button><span>已选 {channels.filter(channel => selected.includes(channel.id)).length}</span></div>
    <div className="price-channel-list">{channels.map(channel => <ChannelChoice key={channel.id} channel={channel} selected={selected.includes(channel.id)} label={`选择渠道 ${channel.name}`} onToggle={() => onChange(selected.includes(channel.id) ? selected.filter(id => id !== channel.id) : [...selected, channel.id])} onNavigate={() => { window.location.hash = `#/channels?channel_id=${channel.id}`; }} />)}{!channels.length && <Empty title="暂无渠道" />}</div>
    <div className="modal-footer"><Button onClick={onClose}>完成选择</Button></div>
  </Modal>;
}
