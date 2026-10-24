import { ArrowRight, Download, RefreshCw, Trash2 } from 'lucide-react';
import { ProviderIcon } from './ProviderIcons';
import { Badge, Button } from './ui';
import type { MarketItem, PluginCapability, PluginInfo } from '../types/plugins';

export function MarketCard({ item, plugins, busy, pending, removing, toggleDisabled, targeted, onElement, onInstall, onRemove, onToggle }: {
  item: MarketItem; plugins: PluginInfo[]; busy: boolean; pending: boolean; removing: boolean; toggleDisabled: boolean;
  targeted?: boolean; onElement?: (element: HTMLElement | null) => void;
  onInstall: () => void; onRemove: () => void; onToggle: (plugin: PluginInfo, capability: PluginCapability) => void;
}) {
  const provider = item.installed_providers?.[0];
  const multiple = (item.installed_providers?.length || 0) > 1;
  function providerControl(type: string, name: string) {
    const plugin = plugins.find(value => value.credential_type === type);
    const capability = plugin?.capabilities.find(value => value.id === 'connect');
    return <div className="market-provider" key={type}>
      {multiple && <span>{name}</span>}
      <div className="plugin-actions">
        {capability?.available && capability.enabled && <a className="btn btn-ghost" href={`#/channels?plugin=${encodeURIComponent(type)}`}>去接入<ArrowRight size={14} /></a>}
        {plugin && capability && <label className="plugin-enable"><span>{capability.enabled ? '已启用' : '已停用'}</span><input type="checkbox" role="switch" aria-label={`${name} 反代直连`} checked={capability.enabled} disabled={busy || toggleDisabled || !capability.available && !capability.enabled} onChange={() => onToggle(plugin, capability)} /></label>}
      </div>
    </div>;
  }
  return <article ref={onElement} className={`plugin-card${targeted ? ' plugin-card-target' : ''}`} tabIndex={-1} aria-current={targeted ? 'true' : undefined} aria-label={item.name}>
    <div className="plugin-card-heading"><span className="plugin-icon"><ProviderIcon name={item.name} fallbackIcon={item.icon} size={32} /></span><h3>{item.name}</h3><Badge tone={item.community ? 'blue' : 'neutral'}>{item.community ? 'Magpie社区' : '第三方'}</Badge></div>
    <p className="plugin-description">{item.description || '为路由网关提供供应商接入。'}</p>
    {item.installed && multiple && <div className="market-provider-list">{item.installed_providers.map(value => providerControl(value.credential_type, value.name))}</div>}
    <div className="plugin-card-footer"><span className="plugin-version">{item.installed ? `已安装${item.installed_version ? ` ${item.installed_version}` : ''}` : item.version || ''}</span><div className="plugin-actions">
      {item.installed && provider && !multiple && providerControl(provider.credential_type, provider.name)}
      {(!item.installed || item.update_available || item.package.startsWith('github:')) && <Button variant="secondary" disabled={busy} aria-busy={pending} onClick={onInstall}>{pending ? <RefreshCw size={14} className="animate-spin" /> : <Download size={14} />}{pending ? item.installed ? '正在更新…' : '正在安装…' : item.installed ? '更新' : '安装'}</Button>}
      {item.installed && <Button variant="ghost" disabled={busy} aria-busy={removing} aria-label={`卸载 ${item.name}`} onClick={onRemove}><Trash2 size={14} />{removing ? '正在卸载…' : '卸载'}</Button>}
    </div></div>
  </article>;
}
