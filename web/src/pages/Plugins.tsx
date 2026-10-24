import { useEffect, useRef, useState } from 'react';
import { ArrowRight, Download, Plug, Radar, RefreshCw, Search } from 'lucide-react';
import { api, errorMessage } from '../api';
import { usePlugins } from '../hooks/usePlugins';
import { useMarketCatalog } from '../hooks/useMarketCatalog';
import { MarketCard } from '../components/MarketCard';
import { useConfirm } from '../components/Confirm';
import { ProviderIcon } from '../components/ProviderIcons';
import { Badge, Button, Empty, ErrorBox, Field, Input, Loading, Modal, PageHeader } from '../components/ui';
import type { MarketItem, PluginCapability, PluginCapabilityID, PluginInfo } from '../types/plugins';
import './Plugins.css';

const categories = [
  { id: 'connect', label: '反代直连', icon: Plug },
  { id: 'discover_api_key', label: '自动嗅探', icon: Radar },
] as const;

export default function Plugins() {
  const resource = usePlugins(), confirm = useConfirm();
  const [marketPending, setMarketPending] = useState('');
  const [marketRemoving, setMarketRemoving] = useState(false);
  const [marketError, setMarketError] = useState('');
  const marketWriting = useRef(false);
  const [route] = useState(() => new URLSearchParams(window.location.hash.split('?')[1] || ''));
  const [category, setCategory] = useState<PluginCapabilityID>(() => route.get('category') === 'discover_api_key' ? 'discover_api_key' : 'connect');
  const [queries, setQueries] = useState<Record<PluginCapabilityID, string>>({ connect: '', discover_api_key: '' });
  const query = queries[category];
  const market = useMarketCatalog(category === 'connect' ? query : '');
  const [pending, setPending] = useState(false);
  const [installOpen, setInstallOpen] = useState(false);
  const [runtimePath, setRuntimePath] = useState('');
  const [installError, setInstallError] = useState('');
  const target = route.get('plugin'), focused = useRef(false), changing = useRef(false);
  const cards = useRef(new Map<string, HTMLElement>());
  const busy = pending || resource.saving || !!marketPending;
  const marketRefreshing = market.loading || !!market.data?.refreshing && !market.error;

  async function marketAction(item: MarketItem, remove = false) {
    if (marketWriting.current) return;
    marketWriting.current = true; setMarketPending(item.id); setMarketRemoving(remove); setMarketError('');
    try {
      if (remove && !await confirm({ title: `卸载 ${item.name}？`, description: '渠道、账号与配置会保留，重新安装后可继续接入。', confirmLabel: '卸载' })) return;
      await api(remove ? `/plugins/market/${encodeURIComponent(item.id)}` : '/plugins/market/install', {
        method: remove ? 'DELETE' : 'POST', ...(remove ? {} : { body: JSON.stringify({ package: item.package }) }),
      });
      await Promise.all([market.reload(), resource.reload()]);
    } catch (cause) { setMarketError(errorMessage(cause)); }
    finally { marketWriting.current = false; setMarketPending(''); setMarketRemoving(false); }
  }

  useEffect(() => {
    const card = target ? cards.current.get(target) : undefined;
    if (card && !focused.current) { focused.current = true; card.focus(); card.scrollIntoView?.({ block: 'nearest' }); }
  }, [resource.data, market.data, category, target]);

  async function toggle(plugin: PluginInfo, capability: PluginCapability) {
    if (changing.current || !resource.data || resource.loading || resource.error || !capability.available && !capability.enabled) return;
    changing.current = true; setPending(true);
    const version = resource.data.version;
    try {
      if (capability.id === 'connect' && capability.enabled && capability.affected_channel_ids.length) {
        const accepted = await confirm({
          title: `停用 ${plugin.name}？`,
          description: `将影响 ${capability.affected_channel_ids.length} 个渠道的接入。渠道、账号与配置数据会保留，重新启用后可继续使用。`,
          confirmLabel: '确认停用',
        });
        if (!accepted) return;
      }
      await resource.setCapability(plugin.id, capability.id, !capability.enabled, version);
    } finally { changing.current = false; setPending(false); }
  }

  async function install() {
    if (changing.current) return;
    changing.current = true; setPending(true); setInstallError('');
    try {
      await api('/plugins/compatible/install', { method: 'POST', body: JSON.stringify({ runtime_path: runtimePath.trim() }) });
      await resource.reload();
      setInstallOpen(false);
    } catch (error) { setInstallError(errorMessage(error)); }
    finally { changing.current = false; setPending(false); }
  }

  const marketPackages = new Set(market.data?.items.map(item => item.package));
  const candidates = (resource.data?.items ?? []).filter(plugin => plugin.capabilities.some(cap => cap.id === category) && !(plugin.package && marketPackages.has(plugin.package)));
  const normalized = query.trim().toLocaleLowerCase();
  const filtered = candidates.filter(plugin => [plugin.name, plugin.description].some(value => value.toLocaleLowerCase().includes(normalized)));
  const marketItems = category === 'connect' ? (market.data?.items || []).filter(item => [item.name, item.description, item.package, ...(item.providers || []), ...(item.installed_providers || []).flatMap(provider => [provider.id, provider.name])].some(value => value.toLocaleLowerCase().includes(normalized))).sort((a, b) => Number(b.community) - Number(a.community) || a.name.localeCompare(b.name)) : [];

  return <div className="plugins-page">
    <PageHeader eyebrow="PLUGINS" title="插件库" description="管理反代直连与本机 API Key 嗅探能力。" actions={
      <Button variant="secondary" aria-busy={resource.loading || marketRefreshing} disabled={resource.loading || market.loading || busy} onClick={() => { void resource.reload(); void market.refresh(); }}><RefreshCw size={15} className={resource.loading || marketRefreshing ? 'animate-spin' : ''} />刷新</Button>
    } />
    <div className="plugins-toolbar">
      <div role="tablist" aria-label="插件分类" className="plugins-tabs">
        {categories.map(item => <button key={item.id} type="button" role="tab" id={`plugins-tab-${item.id}`} aria-selected={category === item.id} aria-controls="plugins-panel" onClick={() => setCategory(item.id)}><item.icon size={19} />{item.label}</button>)}
      </div>
      <label className="plugins-search"><Search size={16} /><Input aria-label="搜索插件" placeholder="搜索插件" value={query} onChange={event => setQueries(current => ({ ...current, [category]: event.target.value }))} /></label>
    </div>
    {resource.error && <ErrorBox message={resource.error} retry={busy || resource.loading ? undefined : () => void resource.reload()} />}
    {category === 'connect' && (marketError || market.error || market.data?.warning) && <ErrorBox message={marketError || market.error || market.data?.warning || ''} retry={busy || market.loading ? undefined : () => void market.refresh()} />}
    <div id="plugins-panel" role="tabpanel" aria-labelledby={`plugins-tab-${category}`}>
      {!resource.data ? resource.loading ? <Loading /> : null : <>
        {([true, false] as const).map(featured => {
          const items = filtered.filter(plugin => plugin.featured === featured);
          return <section className="plugins-section" key={String(featured)} aria-label={featured ? '精选' : '其他'}>
            <h2>{featured ? '精选' : '其他'}<span>{items.length + (!featured ? marketItems.length : 0)}</span></h2>
            {items.length || !featured && marketItems.length ? <div className="plugins-grid">{items.map(plugin => {
              const capability = plugin.capabilities.find(cap => cap.id === category)!;
              const action = category === 'connect' ? '去接入' : '开始嗅探';
              return <article key={plugin.id} ref={element => { if (element) cards.current.set(plugin.id, element); else cards.current.delete(plugin.id); }} className={`plugin-card${target === plugin.id ? ' plugin-card-target' : ''}`} tabIndex={-1} aria-label={plugin.name} aria-current={target === plugin.id ? 'true' : undefined}>
                <div className="plugin-card-heading"><span className="plugin-icon"><ProviderIcon name={plugin.name} override={plugin.icon} size={32} /></span><h3>{plugin.name}</h3><Badge tone={plugin.implementation === 'native' ? 'blue' : 'neutral'}>{plugin.implementation === 'native' ? '原生' : '兼容'}</Badge></div>
                <p className="plugin-description">{plugin.description}</p>
                {!capability.available && <p className="plugin-unavailable">{capability.unavailable_reason || '当前不可用'}</p>}
                <div className="plugin-card-footer"><span className="plugin-version">{plugin.version}</span><div className="plugin-actions">
                  {capability.available && capability.enabled && <a className="btn btn-ghost" href={category === 'connect' ? `#/channels?plugin=${encodeURIComponent(plugin.id)}` : `#/channels?discover=1&source=${encodeURIComponent(plugin.id)}`}>{action}<ArrowRight size={14} /></a>}
                  {category === 'connect' && plugin.implementation === 'compatible' && !plugin.credential_type?.startsWith('market:') && !capability.available && <Button variant="secondary" disabled={busy} onClick={() => { setInstallError(''); setInstallOpen(true); }}><Download size={14} />安装兼容层</Button>}
                  <label className="plugin-enable"><span>{capability.enabled ? '已启用' : '已停用'}</span><input type="checkbox" role="switch" aria-label={`${plugin.name} ${category === 'connect' ? '反代直连' : '自动嗅探'}`} checked={capability.enabled} disabled={busy || resource.loading || !!resource.error || !capability.available && !capability.enabled} onChange={() => void toggle(plugin, capability)} /></label>
                </div></div>
              </article>;
            })}{!featured && marketItems.map(item => {
              const targeted = target === item.id || !!target?.startsWith(`market:${item.id}:`);
              const key = targeted && target ? target : item.id;
              return <MarketCard key={item.id} item={item} plugins={resource.data?.items || []} busy={busy} pending={marketPending === item.id && !marketRemoving} removing={marketPending === item.id && marketRemoving} toggleDisabled={resource.loading || !!resource.error} targeted={targeted} onElement={element => { if (element) cards.current.set(key, element); else cards.current.delete(key); }} onInstall={() => void marketAction(item)} onRemove={() => void marketAction(item, true)} onToggle={(plugin, capability) => void toggle(plugin, capability)} />;
            })}</div> : filtered.length > 0 ? <p className="plugins-section-empty">{candidates.length && normalized ? '没有匹配的插件' : '暂无插件'}</p> : !featured && category === 'connect' && !market.data && market.loading ? <p className="plugins-section-empty" role="status">正在读取供应商目录…</p> : null}
          </section>;
        })}
        {!filtered.length && !marketItems.length && !(category === 'connect' && market.loading && !market.data) && <Empty title={normalized && (candidates.length || market.data?.items.length) ? '未找到匹配插件' : category === 'connect' ? '暂无反代直连插件' : '暂无自动嗅探插件'} description={normalized && candidates.length ? '试试其他名称或关键词。' : category === 'discover_api_key' ? '当前没有可用的 API Key 嗅探插件。' : '当前没有可用的直连插件。'} />}
      </>}
    </div>
    {installOpen && <Modal title="安装兼容层" onClose={() => { if (!pending) setInstallOpen(false); }}>
      <div className="space-y-4">
        <p>将安装随程序提供并核验的 Qoder、WorkBuddy、MiniMax Code 兼容包。兼容插件以当前用户权限运行，需要本机 Node.js 24。</p>
        <p className="text-sm text-[var(--color-text-secondary)]">已安装 Node.js 时可直接继续；尚未安装时，先从 <a className="underline" href="https://nodejs.org/en/download" target="_blank" rel="noreferrer noopener">Node.js 官网</a>安装 24 版，再返回此处。兼容层不会读取原工具的 OAuth 登录。</p>
        <details><summary>手动指定 Node.js 位置</summary><Field label="Node.js 可执行文件" hint="留空时自动从本机 PATH 查找。"><Input aria-label="Node.js 可执行文件" placeholder="node 可执行文件的完整路径" value={runtimePath} onChange={e => setRuntimePath(e.target.value)} disabled={pending} /></Field></details>
        {installError && <ErrorBox message={installError} />}
        <div className="flex justify-end gap-2"><Button variant="ghost" disabled={pending} onClick={() => setInstallOpen(false)}>取消</Button><Button disabled={pending} aria-busy={pending} onClick={() => void install()}>{pending ? '正在安装…' : '检测并安装'}</Button></div>
      </div>
    </Modal>}
  </div>;
}
