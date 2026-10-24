import { useEffect, useState } from 'react';
import { api, errorMessage, validExchangeRate, type ExchangeRate, type ExchangeSourcesConfig } from '../api';
import { Button } from './ui';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';

export function ExchangeRatePanel({ data, loading, error, reload }: { data: ExchangeRate | null; loading: boolean; error: string; reload: () => void }) {
  const [fresh, setFresh] = useState<ExchangeRate | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [refreshError, setRefreshError] = useState('');
  const [config, setConfig] = useState<ExchangeSourcesConfig | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [configBusy, setConfigBusy] = useState(false);
  const [configError, setConfigError] = useState('');
  const [saved, setSaved] = useState(false);
  useUnsavedChanges({ dirty: !!config && JSON.stringify(selected) !== JSON.stringify(config.sources), busy: configBusy, save: saveSources });
  useEffect(() => { setFresh(null); }, [data]);
  const value = fresh || data;
  const message = refreshError || (!fresh && error) || value?.error;
  async function refresh() {
    if (refreshing) return;
    setRefreshing(true); setRefreshError('');
    try { const result = await api<ExchangeRate>('/exchange-rate', { method: 'POST', body: '{}' }); setFresh(result); reload(); }
    catch (e) { setRefreshError(errorMessage(e)); }
    finally { setRefreshing(false); }
  }
  async function loadSources() {
    if (config || configBusy) return;
    setConfigBusy(true); setConfigError('');
    try { const result = await api<ExchangeSourcesConfig>('/exchange-rate/sources'); setConfig(result); setSelected(result.sources); }
    catch (e) { setConfigError(errorMessage(e)); }
    finally { setConfigBusy(false); }
  }
  async function saveSources() {
    setConfigBusy(true); setConfigError(''); setSaved(false);
    try { const result = await api<ExchangeSourcesConfig>('/exchange-rate/sources', { method: 'PUT', body: JSON.stringify({ sources: selected }) }); setConfig(result); setSelected(result.sources); setSaved(true); return true; }
    catch (e) { setConfigError(errorMessage(e)); return false; }
    finally { setConfigBusy(false); }
  }
  function move(id: string, step: number) {
    setSaved(false);
    setSelected(current => { const next = [...current], index = next.indexOf(id); [next[index], next[index + step]] = [next[index + step], next[index]]; return next; });
  }
  const sourceLabel = value?.source_id === 'exchangerate_api' ? 'Rates By Exchange Rate API' : value?.source_name || '汇率来源';
  const checkedAt = value?.checked_at ? new Date(value.checked_at).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }) : '';
  const ordered = config ? [...selected, ...config.available.map(source => source.id).filter(id => !selected.includes(id))] : [];
  return <div className="exchange-rate-panel text-xs" aria-label="汇率参考">
    <div className="flex flex-wrap items-center justify-between gap-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1"><span>{validExchangeRate(value) ? `1 USD ≈ ${Number(value.rate.toFixed(6))} CNY${message || value.stale ? '（旧值）' : ''}` : loading || refreshing ? '正在查询汇率…' : '汇率暂不可用'}</span>{validExchangeRate(value) && <span className="text-[var(--color-text-muted)]">报价 {value.date}{value.source_url && <> · <a href={value.source_url} target="_blank" rel="noreferrer noopener" className="underline">{sourceLabel}</a></>}</span>}{checkedAt && <span className="text-[var(--color-text-muted)]" title={value?.checked_at}>查询 {checkedAt}</span>}</div>
      <Button variant="ghost" disabled={loading || refreshing} onClick={() => void refresh()}>{refreshing ? '刷新中…' : '刷新汇率'}</Button>
    </div>
    {message && <p role="status" className="text-amber-700 dark:text-amber-400">{message}</p>}
    <details className="mt-1" onToggle={e => { if (e.currentTarget.open) void loadSources(); }}><summary className="cursor-pointer text-[var(--color-text-muted)]">汇率来源</summary>
      <div className="mt-2 space-y-2">{ordered.map(id => {
        const source = config!.available.find(item => item.id === id)!;
        const index = selected.indexOf(id), enabled = index >= 0;
        return <div key={id} className="flex items-center gap-2"><label className="flex flex-1 items-center gap-2"><input type="checkbox" checked={enabled} disabled={configBusy} onChange={e => { setSaved(false); setSelected(current => e.target.checked ? [...current, id] : current.filter(value => value !== id)); }} />{enabled ? `${index + 1}. ` : ''}{source.name}</label>{enabled && <><Button variant="ghost" aria-label={`上移 ${source.name}`} disabled={configBusy || index === 0} onClick={() => move(id, -1)}>↑</Button><Button variant="ghost" aria-label={`下移 ${source.name}`} disabled={configBusy || index === selected.length - 1} onClick={() => move(id, 1)}>↓</Button></>}</div>;
      })}{config && <Button variant="secondary" disabled={configBusy} onClick={() => void saveSources()}>保存来源</Button>}{!config && configBusy && <span>正在读取…</span>}{!config && configError && <Button variant="ghost" onClick={() => void loadSources()}>重试</Button>}{saved && <span className="text-[var(--color-text-muted)]">已保存</span>}{configError && <p role="status" className="text-amber-700 dark:text-amber-400">{configError}</p>}</div>
    </details>
  </div>;
}
