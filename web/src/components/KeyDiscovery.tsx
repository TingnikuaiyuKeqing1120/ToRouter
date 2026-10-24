import { useEffect, useRef, useState, type ComponentProps } from 'react';
import { ArrowLeft, Check, Copy, Radar, RefreshCw } from 'lucide-react';
import { api, ApiError, endpointLabels, type EndpointFormat } from '../api';
import type { PluginList } from '../types/plugins';
import { ProviderIcon } from './ProviderIcons';
import { ChannelIconPicker } from './ChannelIconPicker';
import { Button, ErrorBox, Loading, Modal } from './ui';
import './KeyDiscovery.css';

type Source = { id: string; name: string; icon: string; enabled: boolean };
type SourceScan = { id: string; name: string; status: string; diagnostics: { code: string; message: string }[] | null };
type Target = { id: number; name: string; version: string };
type Candidate = { id: string; masked_key: string; sources: { id: string; name: string; icon: string }[]; status: string; disabled: boolean; formats: string[]; targets: Target[]; credential_type: string; reason?: string };
type Group = { id: string; url_display: string; formats: string[]; rows: Candidate[]; existing: boolean; target_count: number; copy_only: boolean; targets: Target[]; display_icon?: string };
type Preview = { target?: Target; id: string; expires_at: string; sources: SourceScan[]; groups: Group[] };
type ImportResult = { candidate_id: string; candidate_ids?: string[]; channel_id?: number; added: number; skipped: number; status: string };
type ImportResponse = { results: ImportResult[]; preview: Preview };
type Selection = { candidate_id: string; target_channel_id: number; expected_version: string; enabled?: boolean; display_icon?: string; name?: string };
type CopyResponse = { items: { credential_type: string; url: string; formats: string[]; sources: { id: string; name: string; icon: string }[]; api_key?: string; filename?: string; content?: string }[] };
const sourceSelectionKey = 'to-router.discovery-sources.v1';
function previousSources(): string[] | null {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(sourceSelectionKey) || 'null');
    return Array.isArray(value) ? value.filter((id): id is string => typeof id === 'string') : null;
  } catch { return null; }
}
function rememberSources(ids: string[]) {
  try { localStorage.setItem(sourceSelectionKey, JSON.stringify(ids)); } catch { /* Source preferences are optional. */ }
}
const copyOnly = (group: Group) => group.copy_only;
const formatName = (format: string) => endpointLabels[format as EndpointFormat] || format;
const defaultIcon = (group: Group) => group.display_icon === 'generic' ? '' : group.display_icon || '';

/** A deliberate local scan. Secrets are requested only by the explicit copy action. */
function DiscoverySurface({ embedded, ...props }: ComponentProps<typeof Modal> & { embedded?: boolean }) { return embedded ? <section className={props.className}>{props.children}</section> : <Modal {...props} />; }

export function KeyDiscovery({ initialSourceID, targetChannelID, credentialType, embedded, onBusy, onClose, onChanged }: { initialSourceID?: string; targetChannelID?: number; credentialType?: string; embedded?: boolean; onBusy?: (busy: boolean) => void; onClose: () => void; onChanged?: () => void }) {
  const [sources, setSources] = useState<Source[]>([]);
  const [sourceIDs, setSourceIDs] = useState<string[]>([]);
  const [sourcesLoading, setSourcesLoading] = useState(true);
  const [sourcesError, setSourcesError] = useState('');
  const [changingSource, setChangingSource] = useState<string | null>(null);
  const [step, setStep] = useState<'sources' | 'results'>('sources');
  const [preview, setPreview] = useState<Preview | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [names, setNames] = useState<Record<string, string>>({});
  const [icons, setIcons] = useState<Record<string, string>>({});
  const [editingIcon, setEditingIcon] = useState<Group | null>(null);
  const [scanning, setScanning] = useState(false);
  const [importing, setImporting] = useState(false);
  const [copying, setCopying] = useState<string | null>(null);
  const [copied, setCopied] = useState<string | null>(null);
  const [imported, setImported] = useState<ImportResult[] | null>(null);
  const [error, setError] = useState('');
  const live = useRef(false);
  const operation = useRef<AbortController | null>(null);
  const sourceOperation = useRef<AbortController | null>(null);
  const sessionID = useRef<string | null>(null);
  const loadVersion = useRef(0);

  function deleteSession(id: string) {
    void api(`/key-discovery/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' }).catch(() => { /* Sessions also expire on the server. */ });
  }
  function discardSession() {
    operation.current?.abort(); operation.current = null;
    if (sessionID.current) deleteSession(sessionID.current);
    sessionID.current = null;
    setPreview(null); setSelected([]); setImported(null); setCopied(null);
  }
  async function loadSources() {
    const version = ++loadVersion.current;
    setSourcesLoading(true); setSourcesError('');
    try {
      const result = await api<{ sources: Source[] }>('/key-discovery/sources');
      if (!live.current || version !== loadVersion.current) return;
      const rows = (result.sources || []).filter(source => !credentialType || [initialSourceID, 'source-magpie', ...(credentialType === 'codex_oauth' ? ['source-codex-manager', 'source-ccswitch'] : credentialType === 'copilot_oauth' ? ['source-ccswitch'] : [])].includes(source.id));
      setSources(rows);
      const saved = initialSourceID ? [initialSourceID] : previousSources();
      setSourceIDs(rows.filter(source => saved ? saved.includes(source.id) : source.enabled).map(source => source.id));
    } catch { if (live.current && version === loadVersion.current) setSourcesError('读取工具列表失败，请重试。'); }
    finally { if (live.current && version === loadVersion.current) setSourcesLoading(false); }
  }
  useEffect(() => {
    live.current = true;
    void loadSources();
    return () => {
      live.current = false; loadVersion.current++;
      sourceOperation.current?.abort(); operation.current?.abort();
      if (sessionID.current) deleteSession(sessionID.current);
      sessionID.current = null;
    };
  }, []);

  async function toggleSource(source: Source, checked: boolean) {
    if (sourceOperation.current || operation.current) return;
    setError('');
    if (checked && !source.enabled) {
      const abort = new AbortController(); sourceOperation.current = abort;
      setChangingSource(source.id);
      try {
        const plugins = await api<PluginList>('/plugins', { signal: abort.signal });
        if (abort.signal.aborted || !live.current) return;
        await api(`/plugins/${encodeURIComponent(source.id)}/capabilities/discover_api_key`, {
          method: 'PUT', signal: abort.signal, body: JSON.stringify({ enabled: true, expected_version: plugins.version }),
        });
        if (abort.signal.aborted || !live.current) return;
        setSources(current => current.map(item => item.id === source.id ? { ...item, enabled: true } : item));
      } catch {
        if (!abort.signal.aborted && live.current) setError('启用工具未完成，请重试。');
        return;
      } finally {
        if (sourceOperation.current === abort) sourceOperation.current = null;
        if (live.current) setChangingSource(null);
      }
    }
    if (!live.current) return;
    setSourceIDs(current => {
      const next = checked ? [...new Set([...current, source.id])] : current.filter(id => id !== source.id);
      rememberSources(next);
      return next;
    });
  }

  async function scan() {
    if (operation.current || sourceOperation.current || !sourceIDs.length) return;
    discardSession();
    setStep('results'); setScanning(true); setError(''); setNames({}); setIcons({});
    const abort = new AbortController(); operation.current = abort;
    const active = () => live.current && !abort.signal.aborted && operation.current === abort;
    const chosen = sources.filter(source => sourceIDs.includes(source.id));
    rememberSources(chosen.map(source => source.id));
    // An explicitly linked or remembered source may have been disabled since
    // last use. Enable selected tools within this single deliberate start.
    let next: Preview | null = null;
    const failedSources = new Set<string>();
    try {
      for (const source of chosen.filter(source => !source.enabled)) {
        const plugins = await api<PluginList>('/plugins', { signal: abort.signal });
        if (!active()) return;
        await api(`/plugins/${encodeURIComponent(source.id)}/capabilities/discover_api_key`, { method: 'PUT', signal: abort.signal, body: JSON.stringify({ enabled: true, expected_version: plugins.version }) });
        if (!active()) return;
        setSources(current => current.map(item => item.id === source.id ? { ...item, enabled: true } : item));
      }
      next = await api<Preview>('/key-discovery/sessions', { method: 'POST', body: JSON.stringify({ source_ids: chosen.map(source => source.id), target_channel_id: targetChannelID, credential_type: credentialType }), signal: abort.signal });
      if (!active()) { deleteSession(next.id); return; }
      sessionID.current = next.id; setPreview(next);
      for (const source of chosen) {
        if (!active()) return;
        setPreview(current => current && { ...current, sources: current.sources.map(item => item.id === source.id ? { ...item, status: 'scanning' } : item) });
        try {
          const scanned: Preview = await api<Preview>(`/key-discovery/sessions/${encodeURIComponent(next.id)}/scan`, { method: 'POST', body: JSON.stringify({ source_id: source.id }), signal: abort.signal });
          if (!active()) return;
          next = { ...scanned, sources: scanned.sources.map(item => failedSources.has(item.id) ? { ...item, status: 'error', diagnostics: [{ code: 'scan_failed', message: '扫描失败，请返回重试。' }] } : item) };
        } catch (cause) {
          if (!active()) return;
          if (cause instanceof ApiError && [403, 404, 409, 410].includes(cause.status)) throw cause;
          failedSources.add(source.id);
          next = { ...next, sources: next.sources.map(item => item.id === source.id ? { ...item, status: 'error', diagnostics: [{ code: 'scan_failed', message: '扫描失败，请返回重试。' }] } : item) };
        }
        setPreview(next);
      }
      if (active()) setSelected(next.groups.flatMap(group => group.rows.filter(row => row.status === 'new').map(row => row.id)));
    } catch (cause) {
      if (!active()) return;
      if (cause instanceof ApiError && [403, 404, 409, 410].includes(cause.status)) {
        if (sessionID.current) deleteSession(sessionID.current);
        sessionID.current = null; setPreview(null);
      }
      setError('扫描未完成，请返回上一步重试。');
    } finally {
      if (operation.current === abort) { operation.current = null; if (live.current) setScanning(false); }
    }
  }

  const groups = (preview?.groups || []).map(group => ({ ...group, rows: group.rows.filter(row => row.status === 'new') })).filter(group => group.rows.length > 0);
  const eligible = groups.flatMap(group => group.rows.map(row => row.id));
  const importable = groups.filter(group => !copyOnly(group)).flatMap(group => group.rows.filter(row => selected.includes(row.id)).map(row => ({ group, row })));
  const selectedCopyCount = groups.filter(copyOnly).flatMap(group => group.rows.filter(row => selected.includes(row.id))).length;
  const busy = importing || copying != null;
  useEffect(() => { onBusy?.(busy); return () => onBusy?.(false); }, [busy, onBusy]);
  function toggleRow(id: string, checked: boolean) {
    setSelected(current => checked ? [...new Set([...current, id])] : current.filter(value => value !== id));
    setCopied(null);
  }
  function back() {
    if (busy) return;
    discardSession(); setScanning(false); setError(''); setStep('sources');
  }
  function selectionFor(group: Group, row: Candidate): Selection {
    const target = preview?.target;
    return {
      candidate_id: row.id, target_channel_id: target?.id || 0, expected_version: target?.version || '',
      ...(!targetChannelID ? { enabled: true, ...(names[group.id]?.trim() ? { name: names[group.id].trim() } : {}) } : {}),
      display_icon: icons[group.id] ?? defaultIcon(group),
    };
  }
  async function importSelected() {
    if (!preview || scanning || busy || operation.current || !importable.length) return;
    const abort = new AbortController(); operation.current = abort;
    setImporting(true); setError('');
    try {
      const response = await api<ImportResponse>(`/key-discovery/sessions/${encodeURIComponent(preview.id)}/import`, { method: 'POST', signal: abort.signal, body: JSON.stringify({ items: importable.map(({ group, row }) => selectionFor(group, row)) }) });
      if (!live.current || abort.signal.aborted) return;
      setPreview(response.preview); setImported(response.results);
      const remaining = new Set(response.preview.groups.flatMap(group => group.rows.filter(row => row.status === 'new').map(row => row.id)));
      setSelected(current => current.filter(id => remaining.has(id)));
      if (response.results.some(result => result.added > 0)) onChanged?.();
    } catch (cause) {
      if (!live.current || abort.signal.aborted) return;
      if (cause instanceof ApiError && [403, 404, 409, 410].includes(cause.status)) {
        setPreview(null); setSelected([]);
        setError('预览或目标已变化，请返回上一步重新扫描。');
      } else setError('添加未完成，所选凭证已保留，请重试。');
    } finally {
      if (operation.current === abort) operation.current = null;
      if (live.current) setImporting(false);
    }
  }
  async function copyGroup(group: Group) {
    if (!preview || busy || scanning || operation.current) return;
    const ids = group.rows.filter(row => selected.includes(row.id)).map(row => row.id);
    if (!ids.length) return;
    const abort = new AbortController(); operation.current = abort;
    setCopying(group.id); setCopied(null); setError('');
    try {
      const response = await api<CopyResponse>(`/key-discovery/sessions/${encodeURIComponent(preview.id)}/copy`, { method: 'POST', signal: abort.signal, body: JSON.stringify({ candidate_ids: ids }) });
      if (!live.current || abort.signal.aborted) return;
      await navigator.clipboard.writeText(JSON.stringify(response, null, 2));
      if (live.current && !abort.signal.aborted) setCopied(group.id);
    } catch { if (live.current && !abort.signal.aborted) setError('复制未完成，请重试。'); }
    finally {
      if (operation.current === abort) operation.current = null;
      if (live.current) setCopying(null);
    }
  }
  function close() {
    if (busy) return;
    live.current = false; loadVersion.current++;
    sourceOperation.current?.abort(); discardSession(); onClose();
  }
  const completed = preview?.sources.filter(source => !['pending', 'scanning'].includes(source.status)).length || 0;
  const sourceWarnings = preview?.sources.filter(source => source.status === 'error' || (source.diagnostics || []).some(item => item.code !== 'not_found')) || [];
  return <><DiscoverySurface embedded={embedded} title="发现凭证" wide onClose={close} busy={busy} className={`key-discovery ${step === 'sources' ? 'key-discovery-source-step' : ''}`}>
    <div className="key-discovery-steps" aria-label="发现步骤"><span className={step === 'sources' ? 'is-current' : ''}>1 选择工具</span><span aria-hidden="true">›</span><span className={step === 'results' ? 'is-current' : ''}>2 扫描结果</span></div>
    <div className="key-discovery-toolbar">
      {step === 'sources' ? <><span>已选 {sourceIDs.length} 个工具</span><Button disabled={sourcesLoading || !!sourcesError || changingSource != null || !sourceIDs.length} onClick={() => void scan()}><Radar size={15} />开始扫描</Button></> : <><Button variant="ghost" disabled={busy} onClick={back}><ArrowLeft size={15} />返回上一步</Button><div className="key-discovery-selection"><Button variant="ghost" disabled={scanning || busy || !eligible.length} onClick={() => setSelected(eligible)}>全选</Button><Button variant="ghost" disabled={scanning || busy || !selected.length} onClick={() => setSelected([])}>清空</Button></div></>}
    </div>
    <div className="key-discovery-body">
      {error && <ErrorBox message={error} />}
      {step === 'sources' ? <section aria-label="扫描来源">{sourcesLoading ? <Loading /> : sourcesError ? <ErrorBox message={sourcesError} retry={() => void loadSources()} /> : !sources.length ? <p className="key-discovery-empty">暂无可扫描的工具。</p> : <div className="key-discovery-tools">{sources.map(source => <label title={credentialType ? source.id === initialSourceID && source.id !== 'source-magpie' ? '官方工具' : '其他已支持工具' : undefined} key={source.id} className={`key-discovery-tool ${sourceIDs.includes(source.id) ? 'is-selected' : ''} ${changingSource === source.id ? 'is-changing' : ''}`}><input type="checkbox" aria-label={`选择来源 ${source.name}`} checked={sourceIDs.includes(source.id)} disabled={changingSource != null} onChange={event => void toggleSource(source, event.target.checked)} /><ProviderIcon name={source.name} override={source.icon} size={32} /><span>{source.name}</span>{credentialType && <small>{source.id === initialSourceID && source.id !== 'source-magpie' ? '官方工具' : '其他已支持工具'}</small>}{changingSource === source.id ? <RefreshCw size={13} className="animate-spin key-discovery-tool-check" /> : sourceIDs.includes(source.id) && <Check size={13} className="key-discovery-tool-check" />}</label>)}</div>}</section> : <section aria-label="发现结果">
        {scanning ? <div className="key-discovery-progress" role="status"><RefreshCw className="animate-spin" size={25} /><strong>正在扫描 {completed} / {preview?.sources.length || sourceIDs.length}</strong><progress value={completed} max={preview?.sources.length || sourceIDs.length} />{preview?.sources.find(source => source.status === 'scanning')?.name}</div> : <>
          {sourceWarnings.length > 0 && <div className="key-discovery-scan-notes">{sourceWarnings.map(source => <p key={source.id}>{source.name}：{source.status === 'error' ? '扫描失败，请返回重试' : '部分配置未能读取'}</p>)}</div>}
          {imported && <div className="key-discovery-import-result" role="status">{targetChannelID ? '已追加' : `已创建 ${new Set(imported.filter(item => item.added > 0 && item.channel_id).map(item => item.channel_id)).size} 个渠道，添加`} {imported.reduce((sum, item) => sum + item.added, 0)} 个凭证{imported.some(item => item.status === 'failed') && '；部分添加失败，可重试所选项。'}{imported.filter(item => item.added > 0 && item.channel_id).map(item => <a key={item.channel_id} href={`#/channels?channel_id=${item.channel_id}`} onClick={close}> 查看渠道</a>)}</div>}
          {!groups.length && preview && <p className="key-discovery-empty">未发现新增凭证。</p>}
          <div className="key-discovery-grid">{groups.map(group => <article key={group.id} className="key-discovery-card" aria-label={`凭证组 ${group.url_display}`}>
            <header><input type="checkbox" className="checkbox" aria-label={`选择凭证组 ${group.url_display}`} checked={group.rows.every(row => selected.includes(row.id))} ref={node => { if (node) node.indeterminate = group.rows.some(row => selected.includes(row.id)) && !group.rows.every(row => selected.includes(row.id)); }} disabled={busy} onChange={event => { const ids = group.rows.map(row => row.id); setSelected(current => event.target.checked ? [...new Set([...current, ...ids])] : current.filter(id => !ids.includes(id))); }} /><button type="button" className="key-discovery-provider" aria-label={`更换 ${group.url_display} 图标`} disabled={busy} onClick={() => setEditingIcon(group)}><ProviderIcon name="" baseUrl={group.url_display} override={icons[group.id] ?? defaultIcon(group)} size={26} /></button><strong title={group.url_display}>{group.url_display}</strong></header>
            <div className="key-discovery-formats">{group.formats.map(format => <span key={format}>{formatName(format)}</span>)}<span>{targetChannelID ? `添加到「${preview?.target?.name || targetChannelID}」` : '新建默认启用渠道'}</span></div>
            {!targetChannelID && <input className="input" aria-label={`渠道名称 ${group.url_display}`} placeholder="渠道名称（可选）" value={names[group.id] || ''} disabled={busy} onChange={event => setNames(current => ({ ...current, [group.id]: event.target.value }))} />}<div className="key-discovery-credentials">{group.rows.map(row => <label key={row.id} className="key-discovery-credential"><input type="checkbox" className="checkbox" aria-label={`选择凭证 ${row.masked_key}`} checked={selected.includes(row.id)} disabled={busy} onChange={event => toggleRow(row.id, event.target.checked)} /><code>{row.masked_key}</code>{row.credential_type !== 'api_key' && <span className="key-discovery-kind">OAuth</span>}<span className="key-discovery-origins">{row.sources.map(source => <span key={source.id} title={source.name} aria-label={`来源 ${source.name}`}><ProviderIcon name={source.name} override={source.icon} size={19} /></span>)}</span></label>)}</div>
            {copyOnly(group) && <div className="key-discovery-copy"><p>此来源材料暂不可直接添加，可显式复制。</p><Button variant="secondary" disabled={busy || !group.rows.some(row => selected.includes(row.id))} onClick={() => void copyGroup(group)}><Copy size={14} />{copied === group.id ? '已复制' : copying === group.id ? '正在复制…' : '复制凭证与信息'}</Button></div>}
          </article>)}</div>
        </>}
      </section>}
    </div>
    <footer className="modal-footer key-discovery-footer"><Button variant="ghost" disabled={busy} onClick={close}>关闭</Button>{step === 'sources' ? <Button disabled={sourcesLoading || !!sourcesError || changingSource != null || !sourceIDs.length} onClick={() => void scan()}><Radar size={15} />开始扫描</Button> : <><span className="key-discovery-count">已选 {importable.length} 个可添加凭证{selectedCopyCount > 0 && ` · ${selectedCopyCount} 个需复制`}</span>{scanning && <Button variant="secondary" onClick={back}>取消扫描</Button>}<Button disabled={scanning || busy || !importable.length} onClick={() => void importSelected()}><Check size={15} />{importing ? '正在添加…' : `${targetChannelID ? '添加到当前渠道' : '创建渠道并添加'} (${importable.length})`}</Button></>}</footer>
  </DiscoverySurface>{editingIcon && <ChannelIconPicker title="更换渠道图标" name={editingIcon.url_display} baseUrl={editingIcon.url_display} value={icons[editingIcon.id] ?? defaultIcon(editingIcon)} onClose={() => setEditingIcon(null)} onSelect={async value => { setIcons(current => ({ ...current, [editingIcon.id]: value })); }} />}</>;
}
