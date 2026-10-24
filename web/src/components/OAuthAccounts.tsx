import { useEffect, useLayoutEffect, useRef, useState, useCallback, type KeyboardEvent, type ReactNode } from 'react';
import { ArrowDown, ArrowUp, ChevronDown, Copy, Flame, GripVertical, KeyRound, Layers, MoreHorizontal, PackageSearch, Pencil, Plus, RefreshCw, RotateCcw, Settings2 } from 'lucide-react';
import { api, errorMessage, type Channel, type CodexQuotaWindow, type CredentialItem, type CredentialType, type RuntimeCredentialStatus } from '../api';
import { quotaTargetKey, useCodexQuota, type QuotaObservation, type QuotaTarget } from '../hooks/useCodexQuota';
import { OAuthCredentialBatch } from './OAuthCredentialBatch';
import { AddOAuthCredential } from './AddOAuthCredential';
import { oauthCredentialIdentity } from '../hooks/useOAuthCredentialBatch';
import { useSortableList } from '../hooks/useSortableList';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { codexHasFiveHourWindow } from '../services/credentialAutomation';
import { OAuthAuthorize } from './oauth';
import { useOptionalRuntimeStatus } from '../hooks/useRuntimeStatus';
import { ProviderIcon, builtinProviderIcons } from './ProviderIcons';
import { ChannelIconPicker } from './ChannelIconPicker';
import { Badge, Button, Input, Modal, useToast } from './ui';
import { CodexCredentialMenu, CodexCredentialOperations, type CodexOperationsView } from './CodexCredentialOperations';
import type { CredentialResetCards } from '../types/credential-operations';
import type { PluginAccess } from '../types/plugins';
import './OAuthAccounts.css';

type Provider = { type: Exclude<CredentialType, 'api_key'>; title: string; note: string; official: boolean };
type Pool = { provider: Provider; channels: Channel[] };
type Preferences = { platforms: string[]; accounts: Record<string, number[]>; hidden: Record<string, boolean>; icons: Record<string, string>; collapsedAccounts: Record<number, boolean>; active: string };
const preferenceKey = 'to-router.oauth-display.v1';
const platformTypes = ['codex_oauth', 'claude_oauth', 'antigravity_oauth', 'zen_free', 'devin_oauth', 'chatgpt_oauth', 'copilot_oauth', 'plugin:qoder', 'plugin:qoder-cn', 'plugin:workbuddy', 'plugin:workbuddy-ai', 'plugin:minimax-code', 'plugin:minimax-code-global'];
function readPreferences(): Preferences {
  try {
    const v = JSON.parse(localStorage.getItem(preferenceKey) || '{}');
    const validPlatform = (id: unknown): id is string => typeof id === 'string' && (platformTypes.includes(id) || /^market:[0-9a-f]{32}:[A-Za-z0-9][A-Za-z0-9._-]*$/.test(id));
    const savedTypes = [...new Set([...platformTypes, ...(Array.isArray(v.platforms) ? v.platforms : []), ...Object.keys(v.accounts || {}), ...Object.keys(v.hidden || {}), ...Object.keys(v.icons || {})].filter(validPlatform))];
    return {
      platforms: Array.isArray(v.platforms) ? [...new Set<string>(v.platforms.filter((id: unknown) => typeof id === 'string' && validPlatform(id)))] : [],
      accounts: Object.fromEntries(savedTypes.map(type => [type, Array.isArray(v.accounts?.[type]) ? v.accounts[type].filter((id: unknown) => typeof id === 'number' && Number.isInteger(id) && id > 0) : []])),
      hidden: Object.fromEntries(savedTypes.map(type => [type, v.hidden?.[type] === true])),
      icons: Object.fromEntries(savedTypes.map(type => [type, typeof v.icons?.[type] === 'string' && (builtinProviderIcons.some(icon => icon.id === v.icons[type]) || /^data:image\/png;base64,[A-Za-z0-9+/=]+$/.test(v.icons[type]) && v.icons[type].length < 90_000) ? v.icons[type] : ''])),
      collapsedAccounts: Object.fromEntries(Object.keys(v.collapsedAccounts || {}).filter(id => /^\d+$/.test(id) && v.collapsedAccounts[id] === true).map(id => [id, true])),
      active: validPlatform(v.active) ? v.active : '',
    };
  } catch { return { platforms: [], accounts: {}, hidden: {}, icons: {}, collapsedAccounts: {}, active: '' }; }
}
function ordered<T>(rows: T[], saved: unknown, id: (row: T) => string | number): T[] {
  const ids = Array.isArray(saved) ? saved : [];
  const rank = (row: T) => { const n = ids.indexOf(id(row)); return n < 0 ? ids.length : n; };
  return [...rows].sort((a, b) => rank(a) - rank(b));
}
function moved<T>(rows: T[], from: T, to: T): T[] {
  const next = rows.filter(id => id !== from), index = rows.indexOf(to);
  if (index < 0 || !rows.includes(from)) return rows;
  next.splice(index, 0, from); return next;
}
function time(value?: string) {
  if (!value) return '未知';
  const date = new Date(value);
  return Number.isFinite(date.getTime()) ? date.toLocaleString() : '未知';
}
function windowLabel(seconds?: number | null) {
  if (seconds == null || !Number.isFinite(seconds) || seconds <= 0) return '时长未知';
  if (seconds % 86400 === 0) return `${seconds / 86400} 天`;
  if (seconds % 3600 === 0) return `${seconds / 3600} 小时`;
  if (seconds % 60 === 0) return `${seconds / 60} 分钟`;
  return `${seconds} 秒`;
}
function QuotaWindow({ value, label }: { value?: CodexQuotaWindow | null; label: string }) {
  const used = value?.used_percent;
  const remaining = used != null && Number.isFinite(used) && used >= 0 && used <= 100 ? Math.round((100 - used) * 10) / 10 : null;
  const reset = value?.reset_at;
  return <div className="oauth-main-window"><div><span>{value ? windowLabel(value.window_seconds) : label}</span><strong>{remaining == null ? '剩余未知' : `剩余 ${remaining}%`}</strong></div>{remaining == null ? <span className="oauth-main-window-track" aria-hidden="true" /> : <progress max={100} value={remaining} aria-label={`${label}剩余比例`} />}<span title={reset == null ? undefined : new Date(reset * 1000).toLocaleString()}>重置：{reset != null && Number.isFinite(reset) ? new Date(reset * 1000).toLocaleString() : '未知'}</span></div>;
}
function credentialInfo(credential: CredentialItem) { return credential.oauth_info || credential.oauth; }
function credentialName(credential: CredentialItem, index: number, count: number) {
  const label = credential.label?.trim();
  return label && label !== credentialInfo(credential)?.account_label?.trim() ? label : count === 1 ? '默认凭据' : `凭据 ${index + 1}`;
}
function credentialTarget(channel: Channel, credential: CredentialItem): QuotaTarget {
  return { channelID: channel.id, credentialID: credential.id, identity: oauthCredentialIdentity(credential) };
}

function RefreshSettings({ register }: { register: (handler: () => Promise<boolean>) => () => void }) {
  const [draft, setDraft] = useState('');
  const [saved, setSaved] = useState<number>();
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const dirty = useRef(false), pending = useRef(false);
  const canLeave = useUnsavedChanges({ dirty: () => dirty.current, busy: () => pending.current, save });
  const latestLeave = useRef(canLeave); latestLeave.current = canLeave;
  useEffect(() => { if (dirty.current || saving) return register(() => latestLeave.current()); }, [register, draft, saved, saving]);
  useEffect(() => {
    let live = true;
    const load = () => {
      setLoading(true);
      void api<{ quota_refresh_seconds: number }>('/settings/quota-refresh').then(result => {
        if (!live) return;
        setSaved(result.quota_refresh_seconds);
        if (!dirty.current) setDraft(String(result.quota_refresh_seconds));
        setError('');
      }).catch(cause => { if (live) setError(`读取失败：${errorMessage(cause)}`); }).finally(() => { if (live) setLoading(false); });
    };
    load();
    window.addEventListener('quota-settings-changed', load);
    return () => { live = false; window.removeEventListener('quota-settings-changed', load); };
  }, []);
  async function save() {
    if (pending.current) return false;
    const value = Number(draft);
    if (!draft.trim() || !Number.isInteger(value) || value !== 0 && (value < 30 || value > 86400)) { setError('请输入 0，或 30–86400 之间的整数。'); return false; }
    pending.current = true; setSaving(true); setError('');
    try {
      const result = await api<{ quota_refresh_seconds: number }>('/settings/quota-refresh', { method: 'PUT', body: JSON.stringify({ quota_refresh_seconds: value }) });
      setSaved(result.quota_refresh_seconds); setDraft(String(result.quota_refresh_seconds)); dirty.current = false;
      window.dispatchEvent(new Event('quota-settings-changed'));
      return true;
    } catch (cause) { setError(`保存失败，填写已保留：${errorMessage(cause)}`); return false; }
    finally { pending.current = false; setSaving(false); }
  }
  return <details className="oauth-main-refresh-settings"><summary>刷新周期<span>{saved === undefined ? '未读取' : saved === 0 ? '仅手动' : `${saved} 秒`}</span></summary><div className="oauth-main-refresh-form"><label htmlFor="oauth-quota-refresh">额度刷新周期（秒）</label><div><Input id="oauth-quota-refresh" aria-label="额度刷新周期（秒）" type="number" min={0} max={86400} step={1} value={draft} disabled={saving || loading} onChange={event => { dirty.current = true; setDraft(event.target.value); }} /><Button variant="secondary" disabled={saving || loading} onClick={() => void save()}>{saving ? '正在保存…' : '保存刷新周期'}</Button></div><p>全局生效，与系统设置同步。0 为仅手动，其余为 30–86400 秒。</p><p className="oauth-main-setting-status" role="status" title={error}>{error || (loading ? '正在读取…' : saved !== undefined && draft === String(saved) ? '已保存' : '待保存')}</p></div></details>;
}

function CredentialRow({ channel, credential, name, codex, observation, refresh, runtime, staleRuntime, blocked, networkBlocked, index, move, toggle, rowProps, handleProps, onChanged, selected, onSelect, onMoveTo }: {
  channel: Channel; credential: CredentialItem; name: string; codex: boolean; observation?: QuotaObservation; refresh: (target: QuotaTarget) => void;
  runtime?: RuntimeCredentialStatus; staleRuntime: boolean; blocked: boolean; networkBlocked: boolean; index: number; selected: boolean; onSelect: () => void;
  onMoveTo?: () => void; move: (from: string, to: string) => void; toggle: () => void; rowProps: ReturnType<ReturnType<typeof useSortableList>['itemProps']>; handleProps: ReturnType<ReturnType<typeof useSortableList>['handleProps']>; onChanged: () => void;
}) {
  const [details, setDetails] = useState(false);
  const [showAccount, setShowAccount] = useState(false);
  const [operationsView, setOperationsView] = useState<CodexOperationsView>();
  const [enableOnOpen, setEnableOnOpen] = useState<'warmup' | 'auto_reset'>();
  const [cards, setCards] = useState<CredentialResetCards>();
  const [operationError, setOperationError] = useState('');
  const updateCards = useCallback((next: CredentialResetCards) => setCards(next), []);
  const toast = useToast();
  async function copyID(label: string, value: string) {
    try { await navigator.clipboard.writeText(value); toast(`${label}已复制`); }
    catch { toast('复制失败，请手动选择 ID 复制。'); }
  }
  const oauth = credentialInfo(credential), snapshot = observation?.snapshot;
  const closeDetails = () => { setDetails(false); setShowAccount(false); };
  const general = snapshot?.buckets?.find(bucket => bucket.general === true);
  const detailBuckets = snapshot?.buckets?.filter(bucket => bucket.general !== true) || [];
  const status = !codex ? '' : observation?.pending ? snapshot ? '正在刷新 · 保留上次额度' : '正在获取额度…' : observation?.error ? `${snapshot ? '旧快照 · ' : ''}刷新失败：${observation.error}` : snapshot ? `${snapshot.stale ? '旧快照 · ' : ''}更新：${time(snapshot.captured_at)}` : '额度未获取';
  const credentials = channel.credentials || [];
  const publicAccess = channel.credential_type === 'zen_free';
  const authorizationIssue = publicAccess ? credential.has_key ? '' : '缺少公共凭证' : oauth?.status === 'reauth_required' ? '需重新授权' : oauth?.status === 'unsubscribed' ? '未订阅' : credential.has_oauth === false || !credential.has_oauth && !oauth ? '未授权' : '';
  const cardCount = cards && (!snapshot?.captured_at || new Date(cards.captured_at).getTime() >= new Date(snapshot.captured_at).getTime()) ? cards.available_count : snapshot?.reset_credit_count;
  const disabledReason = !channel.enabled ? '渠道停用' : credential.enabled === false ? '凭证停用' : '';
  const statusReason = disabledReason || (authorizationIssue ? oauth?.status_reason || authorizationIssue : runtime?.red ? runtime.reason || '发现异常' : '');
  const generalWindows = [general?.primary, general?.secondary].filter(value => value != null);
  const onlyWeeklyQuota = generalWindows.length === 1 && generalWindows[0]?.window_seconds === 604800;
  const runtimeText = disabledReason ? '已停用' : authorizationIssue || runtime?.red ? '不可用' : runtime ? '未发现异常' : '状态未读取';
  const statusTone = disabledReason ? 'neutral' : authorizationIssue || runtime?.red ? 'red' : runtime && !staleRuntime ? 'green' : 'neutral';
  return <article className="oauth-main-credential" data-credential-id={credential.id} aria-label={`凭证 ${name}`} {...rowProps}><div className="oauth-main-credential-body"><div className="oauth-main-row-controls"><label className="oauth-main-select"><input type="checkbox" aria-label={`选择凭证 ${name}`} checked={selected} disabled={blocked} onChange={onSelect} /></label><button type="button" className="oauth-main-credential-drag" disabled={blocked || credentials.length < 2 && !onMoveTo} aria-label={`拖动凭证 ${name}`} title="拖动调整调用顺序，也可用上下方向键移动" aria-keyshortcuts="ArrowUp ArrowDown" {...handleProps} onKeyDown={event => { const target = event.key === 'ArrowUp' ? credentials[index - 1] : event.key === 'ArrowDown' ? credentials[index + 1] : undefined; if (target) { event.preventDefault(); move(credential.id, target.id); } }}><GripVertical size={15} /></button></div><div className="oauth-main-identity"><div className="oauth-main-credential-title"><strong title={name}>{name}</strong><span className="oauth-main-plan">{publicAccess ? '免费接入' : codex ? snapshot?.plan_type || '套餐未知' : '套餐未知'}</span></div><div className="oauth-main-credential-order"><Button variant="ghost" aria-label={`上移凭证 ${name}`} disabled={blocked || index === 0} onClick={() => move(credential.id, credentials[index - 1].id)}><ArrowUp size={14} /></Button><Button variant="ghost" aria-label={`下移凭证 ${name}`} disabled={blocked || index === credentials.length - 1} onClick={() => move(credential.id, credentials[index + 1].id)}><ArrowDown size={14} /></Button></div></div>{codex ? <div className="oauth-main-quota-column"><div className="oauth-main-windows" aria-label="通用额度"><QuotaWindow value={general?.primary} label="主窗口" /><QuotaWindow value={general?.secondary} label="次窗口" /></div><button type="button" className={`oauth-main-reset-count ${cardCount != null && cardCount > 0 ? 'has-cards' : ''}`} aria-label={`重置卡 ${name}`} disabled={blocked} onClick={() => { setEnableOnOpen(undefined); setOperationsView('cards'); }}><RotateCcw size={14} aria-hidden="true" />重置卡 <span>{cardCount == null ? '未知' : cardCount}</span></button></div> : <div className="oauth-main-quota-unconnected">额度尚未接入</div>}<div className="oauth-main-credential-status"><span className={`oauth-main-runtime is-${statusTone}`}><span aria-hidden="true" className="oauth-main-status-dot" />{runtimeText}</span>{statusReason && <span className="oauth-main-status-reason" title={statusReason}>{statusReason}</span>}{onlyWeeklyQuota && <span className="oauth-main-plan">仅7天额度</span>}{staleRuntime && runtime && <span className="oauth-main-plan">上次状态</span>}{codex && <span className="oauth-main-warmup-summary">自动预热：{credential.operations?.warmup.enabled ? '开' : '关'}</span>}<Button variant="ghost" aria-label={`额度详情 ${name}`} onClick={() => setDetails(true)}>详情</Button></div><div className="oauth-main-credential-actions">{onMoveTo && <details className="oauth-main-menu" onKeyDown={event => { if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); event.currentTarget.open = false; event.currentTarget.querySelector('summary')?.focus(); } }}><summary aria-label={`更多账号管理 ${name}`}><MoreHorizontal size={16} /></summary><div><Button variant="ghost" disabled={blocked} onClick={onMoveTo}>移到其他渠道</Button></div></details>}<label className="oauth-main-credential-enabled"><button type="button" role="switch" aria-label={`${credential.enabled === false ? '启用' : '停用'}凭证 ${name}`} aria-checked={credential.enabled !== false} disabled={blocked} className={`switch ${credential.enabled !== false ? 'switch-on' : ''}`} onClick={toggle}><span /></button><span>{credential.enabled === false ? '已停用' : '已启用'}</span></label>{codex && <Button variant="ghost" aria-label={`刷新额度 ${name}`} title="刷新此凭证额度" disabled={blocked || networkBlocked || !!observation?.pending} onClick={() => refresh(credentialTarget(channel, credential))}><RefreshCw size={15} /></Button>}{codex && <><Button variant="ghost" aria-label={`预热 ${name}`} title="预热此凭证" disabled={blocked || networkBlocked || !channel.enabled || credential.enabled === false || !!authorizationIssue} onClick={() => { setEnableOnOpen(undefined); setOperationsView('warmup'); }}><Flame size={15} /></Button><CodexCredentialMenu hasFiveHourWindow={codexHasFiveHourWindow(snapshot)} channel={channel} credential={credential} name={name} blocked={blocked} configure={(view, enable) => { setEnableOnOpen(enable); setOperationsView(view); }} onChanged={onChanged} onError={setOperationError} /></>}</div></div><p className={`oauth-main-quota-status ${observation?.error || operationError ? 'has-error' : ''}`} role="status" title={operationError || status}>{operationError || status}</p>{operationsView && <CodexCredentialOperations channel={channel} credential={credential} name={name} quota={snapshot} initialView={operationsView} enableOnOpen={enableOnOpen} blocked={blocked} networkBlocked={networkBlocked} onClose={() => setOperationsView(undefined)} onCards={updateCards} onChanged={onChanged} />}{details && <Modal title={`凭证详情 · ${name}`} onClose={closeDetails} className="oauth-main-quota-dialog"><div className="oauth-main-quota-detail">{oauth?.account_label && <div className="oauth-main-account"><Button variant="ghost" aria-expanded={showAccount} onClick={() => setShowAccount(current => !current)}>{showAccount ? '隐藏账号' : '显示账号'}</Button>{showAccount && <span>{oauth.account_label}</span>}</div>}{observation?.error && <p>额度刷新失败：{observation.error}</p>}<p>运行状态：{runtimeText}{statusReason ? ` · ${statusReason}` : ''}{staleRuntime && runtime ? '（上次状态）' : ''}</p><p>授权：{publicAccess ? '公共访问，无需登录' : oauth?.status_reason || (oauth?.status === 'active' ? '已授权' : '状态未知')}</p>{!publicAccess && <p>套餐：{codex ? snapshot?.plan_type || '未知' : '未知'}</p>}{codex ? <><p>通用额度 · 上游允许：{general?.allowed == null ? '未知' : general.allowed ? '是' : '否'} · 达到限额：{general?.limit_reached == null ? '未知' : general.limit_reached ? '是' : '否'}</p>{detailBuckets.map((bucket, index) => <section key={`extra:${index}:${bucket.id}`}><h3>{bucket.name || (bucket.id === 'code_review_rate_limit' ? '代码审查额度' : bucket.id)}</h3><p>上游允许：{bucket.allowed == null ? '未知' : bucket.allowed ? '是' : '否'} · 达到限额：{bucket.limit_reached == null ? '未知' : bucket.limit_reached ? '是' : '否'}</p><div className="oauth-main-windows"><QuotaWindow value={bucket.primary} label="主窗口" /><QuotaWindow value={bucket.secondary} label="次窗口" /></div></section>)}{!detailBuckets.length && <p>没有其他额度明细。</p>}</> : <p>此接入方式尚未接入额度查询。</p>}
        <details className="advanced">
          <summary>诊断信息</summary>
          <div className="oauth-main-diagnostic-body">
            <div className="flex flex-wrap items-center gap-2"><span>渠道 ID：<code>{channel.id}</code></span><Button type="button" variant="ghost" className="min-h-11" aria-label="复制渠道 ID" onClick={() => void copyID('渠道 ID ', String(channel.id))}><Copy size={14} />复制</Button></div>
            <div className="flex flex-wrap items-center gap-2"><span>凭证 ID：<code>{credential.id}</code></span><Button type="button" variant="ghost" className="min-h-11" aria-label="复制凭证 ID" onClick={() => void copyID('凭证 ID ', credential.id)}><Copy size={14} />复制</Button></div>
            {!publicAccess && (channel.credential_type === 'devin_oauth' ? <p>会话未声明固定到期时间；上游拒绝会话时，请重新授权。</p> : (channel.credential_type?.startsWith('plugin:') || channel.credential_type?.startsWith('market:')) ? <p>由兼容插件按需续期；需要重新授权时会另行提示。</p> : <p>当前访问令牌到期：{time(oauth?.expires_at)}<span className="oauth-main-token-note">由系统按需续期；需要重新授权时会另行提示。</span></p>)}
          </div>
        </details>
      </div></Modal>}</article>;
}

function CredentialSortableList({ channel, blocked, revision, onCommit, externalDrop, onAdd, render }: {
  channel: Channel; blocked: boolean; revision: string; onCommit: (ids: string[]) => Promise<void>;
  onAdd?: () => void; externalDrop?: Parameters<typeof useSortableList>[0]['externalDrop'];
  render: (credential: CredentialItem, index: number, sortable: ReturnType<typeof useSortableList>) => ReactNode;
}) {
  const credentials = channel.credentials || [];
  const sortable = useSortableList({ ids: credentials.map(credential => credential.id), disabled: blocked, revision, onCommit, externalDrop });
  return <div className="oauth-main-credentials" aria-label={`${channel.name} 凭证列表`} tabIndex={credentials.length > 3 ? 0 : undefined} {...sortable.listProps}>{credentials.length ? credentials.map((credential, index) => render(credential, index, sortable)) : <div className="oauth-main-empty"><p>{channel.credential_type === 'zen_free' ? '公共访问，无需个人凭证' : '待添加凭证'}</p>{onAdd && <Button variant="secondary" onClick={onAdd}><Plus size={14} />添加账号 / 工作区</Button>}</div>}</div>;
}

function CredentialSelectAll({ channel, selected, blocked, onSelect }: { channel: Channel; selected: Set<string>; blocked: boolean; onSelect: () => void }) {
  const input = useRef<HTMLInputElement>(null);
  const credentials = channel.credentials || [];
  const count = credentials.filter(credential => selected.has(quotaTargetKey(credentialTarget(channel, credential)))).length;
  useEffect(() => { if (input.current) input.current.indeterminate = count > 0 && count < credentials.length; }, [count, credentials.length]);
  return <label className="oauth-main-select"><input ref={input} type="checkbox" aria-label={`全选 ${channel.name} 凭证`} checked={credentials.length > 0 && count === credentials.length} disabled={blocked || !credentials.length} onChange={onSelect} /></label>;
}

export function OAuthAccounts({ search = '', autoOpen = true, autoOpenDisabled = false, onAutoOpenChange, pools, loading, pluginAccess, initialProvider, onProviderSelected, flows, onFlowStarted, onReauthorize, onEdit, onAdded, onCredentialOrderSaved, onTest, onToggle, busy, onDelete }: { search?: string; autoOpen?: boolean; autoOpenDisabled?: boolean; onAutoOpenChange?: (next: boolean) => void; pluginAccess: Record<string, PluginAccess>; initialProvider?: string; onProviderSelected?: (provider: Provider | undefined) => void; pools: Pool[]; loading: boolean; flows: Record<string, string>; onFlowStarted: (key: string, flow: string | null) => void; onReauthorize: (channel: Channel) => void; onEdit: (channel: Channel, section?: 'credentials') => void; onAdded: () => void; onCredentialOrderSaved: (channelID: number, ids: string[]) => void; onTest: (channel: Channel) => void; onToggle: (channel: Channel) => void; busy: number | null; onDelete: (channel: Channel) => void }) {
  const root = useRef<HTMLDivElement>(null);
  const refreshLeave = useRef<(() => Promise<boolean>) | null>(null);
  const registerRefreshLeave = useCallback((handler: () => Promise<boolean>) => { refreshLeave.current = handler; return () => { if (refreshLeave.current === handler) refreshLeave.current = null; }; }, []);
  function afterRefreshDecision(action: () => void) {
    if (!refreshLeave.current) action();
    else void refreshLeave.current().then(allowed => { if (allowed) action(); });
  }
  const runtime = useOptionalRuntimeStatus();
  const pendingCredentials = useRef(new Set<number>());
  const [credentialBusy, setCredentialBusy] = useState<Set<number>>(new Set());
  const [selectedCredentials, setSelectedCredentials] = useState<Set<string>>(new Set());
  const selectionGenerations = useRef(new Map<string, number | undefined>());
  const [batchBusy, setBatchBusy] = useState<Set<number>>(new Set());
  const [credentialErrors, setCredentialErrors] = useState<Record<number, string>>({});
  useEffect(() => {
    const closeOutsideMenu = (event: Event) => {
      if (!(event.target instanceof Node)) return;
      root.current?.querySelectorAll<HTMLDetailsElement>('.oauth-main-menu[open]').forEach(menu => {
        if (!menu.contains(event.target as Node)) menu.open = false;
      });
    };
    document.addEventListener('pointerdown', closeOutsideMenu);
    document.addEventListener('focusin', closeOutsideMenu);
    return () => {
      document.removeEventListener('pointerdown', closeOutsideMenu);
      document.removeEventListener('focusin', closeOutsideMenu);
    };
  }, []);
  const [preferences, setPreferences] = useState(readPreferences);
  const locatedProvider = useRef('');
  const [adding, setAdding] = useState<Record<string, boolean>>({});
  const [managing, setManaging] = useState(false);
  const [addingTo, setAddingTo] = useState<Channel>();
  const [moveDialog, setMoveDialog] = useState<{ source: Channel; ids: string[] }>();
  const [moveTarget, setMoveTarget] = useState('');
  const [moveBusy, setMoveBusy] = useState(false);
  const moving = useRef(false);
  const [moveError, setMoveError] = useState('');
  const [dropHover, setDropHover] = useState<{ source: number; target: number; credential: string }>();
  const [iconProvider, setIconProvider] = useState<Provider>();
  const displayed = ordered(ordered(pools, platformTypes, pool => pool.provider.type), preferences.platforms, pool => pool.provider.type);
  const enabled = (pool: Pool) => pluginAccess[pool.provider.type]?.enabled ?? pluginAccess[pool.provider.type]?.allowed ?? false;
  const manageable = displayed.filter(enabled);
  const query = search.trim().toLowerCase();
  const matchesSearch = (channel: Channel) => `${channel.name} ${channel.base_url || ''} ${(channel.endpoints || []).map(endpoint => endpoint.base_url).join(' ')} ${(channel.models || []).join(' ')}`.toLowerCase().includes(query);
  const visible = displayed.filter(pool => (enabled(pool)
    ? !preferences.hidden[pool.provider.type] || initialProvider === pool.provider.type
    : pool.channels.length > 0 && (initialProvider === pool.provider.type || pool.provider.type.startsWith('market:')) || !!flows[`card:${pool.provider.type}`]))
    // Searching must not unmount the active provider's unsaved refresh settings.
    .filter(pool => !query || pool.provider.type === preferences.active || pool.channels.some(matchesSearch));
  const selected = visible.find(pool => pool.provider.type === preferences.active) || visible[0];
  const type = selected?.provider.type;
  useEffect(() => {
    if (addingTo || !type) return;
    const pending = selected?.channels.find(channel => flows[`channel:${channel.id}`]);
    if (pending) setAddingTo(pending);
  }, [type, pools]);
  useEffect(() => { onProviderSelected?.(selected?.provider); }, [type, onProviderSelected]);
  const visibleOrder = visible.map(pool => pool.provider.type).join(',');
  useLayoutEffect(() => {
    const reveal = () => {
      const item = document.getElementById(`oauth-tab-${type}`)?.parentElement;
      const strip = item?.parentElement;
      if (!item || !strip) return;
      const bounds = item.getBoundingClientRect(), viewport = strip.getBoundingClientRect();
      if (bounds.right > viewport.right - 4) strip.scrollLeft += bounds.right - viewport.right + 4;
      else if (bounds.left < viewport.left + 4) strip.scrollLeft += bounds.left - viewport.left - 4;
    };
    reveal();
    // Fonts and asynchronously loaded counts may resize earlier tabs without
    // changing the selected type. Observe widths, never scroll the whole page.
    const strip = document.getElementById(`oauth-tab-${type}`)?.parentElement?.parentElement;
    const observer = typeof ResizeObserver === 'undefined' ? undefined : new ResizeObserver(reveal);
    if (strip) { observer?.observe(strip); for (const child of strip.children) observer?.observe(child); }
    window.addEventListener('resize', reveal);
    return () => { observer?.disconnect(); window.removeEventListener('resize', reveal); };
  }, [type, visibleOrder]);
  const access = type ? pluginAccess[type] : undefined;
  const networkBlocked = !access?.allowed;
  useEffect(() => {
    if (initialProvider && locatedProvider.current !== initialProvider && pools.some(pool => pool.provider.type === initialProvider)) {
      locatedProvider.current = initialProvider;
      setPreferences(current => ({ ...current, active: initialProvider }));
      return;
    }
    if (type && type !== preferences.active) setPreferences(current => ({ ...current, active: type }));
  }, [initialProvider, type, preferences.active]);
  const targets = selected?.provider.type === 'codex_oauth' ? selected.channels.flatMap(channel => (channel.credentials || []).map(credential => credentialTarget(channel, credential))) : [];
  const { observations, refreshQuota } = useCodexQuota(targets, !loading, !networkBlocked);
  const [fetching, setFetching] = useState<Set<number>>(new Set());
  const [notes, setNotes] = useState<Record<number, string>>({});
  const live = useRef(true), pendingModels = useRef(new Set<number>()), present = useRef(new Set<number>());
  if (!loading) present.current = new Set(pools.flatMap(pool => pool.channels.map(channel => channel.id)));
  useEffect(() => { live.current = true; return () => { live.current = false; }; }, []);
  useEffect(() => { try { localStorage.setItem(preferenceKey, JSON.stringify(preferences)); } catch { /* Browser display preferences are optional. */ } }, [preferences]);
  const toast = useToast();
  async function fetchModels(channel: Channel) {
    if (!pluginAccess[channel.credential_type || type || '']?.allowed || pendingModels.current.has(channel.id)) return;
    pendingModels.current.add(channel.id); setFetching(current => new Set(current).add(channel.id));
    try {
      const result = await api<{ ok: boolean; detail?: string; fetched?: number }>(`/channels/${channel.id}/oauth/models`, { method: 'POST', body: '{}' });
      if (!live.current || !present.current.has(channel.id)) return;
      setNotes(current => ({ ...current, [channel.id]: result.ok ? `已获取 ${result.fetched || 0} 个模型（手动添加的模型保留）` : `获取失败：${result.detail || '未知原因'}` }));
      if (result.ok) { toast(`已更新 ${channel.name} 的模型目录`); onAdded(); }
    } catch (cause) { if (live.current && present.current.has(channel.id)) setNotes(current => ({ ...current, [channel.id]: errorMessage(cause) })); }
    finally { pendingModels.current.delete(channel.id); if (live.current) setFetching(current => { const next = new Set(current); next.delete(channel.id); return next; }); }
  }

  async function changeCredentials(channel: Channel, path: string, body: unknown, propagateError = false, onSaved?: () => void) {
    if (loading || busy != null || batchBusy.has(channel.id) || pendingCredentials.current.has(channel.id)) {
      if (propagateError) throw new Error('凭证设置正在更新，请稍后重试。');
      return;
    }
    pendingCredentials.current.add(channel.id);
    setCredentialBusy(current => new Set(current).add(channel.id));
    setCredentialErrors(current => ({ ...current, [channel.id]: '' }));
    try {
      const result = await api<{ ok: boolean }>(`/channels/${channel.id}/${path}`, { method: 'PUT', body: JSON.stringify(body) });
      if (!result.ok) throw new Error('未能保存凭证设置，请重试。');
      if (live.current && present.current.has(channel.id)) { onSaved?.(); onAdded(); }
    } catch (cause) {
      if (live.current && present.current.has(channel.id)) setCredentialErrors(current => ({ ...current, [channel.id]: `保存失败，保留原设置：${errorMessage(cause)}` }));
      if (propagateError) throw cause;
    } finally {
      pendingCredentials.current.delete(channel.id);
      if (live.current) setCredentialBusy(current => { const next = new Set(current); next.delete(channel.id); return next; });
    }
  }
  function selectCredential(key: string, generation?: number) {
    setSelectedCredentials(current => { const next = new Set(current); if (next.has(key)) { next.delete(key); selectionGenerations.current.delete(key); } else { next.add(key); selectionGenerations.current.set(key, generation); } return next; });
  }
  function selectChannel(channel: Channel) {
    const keys = (channel.credentials || []).map(credential => quotaTargetKey(credentialTarget(channel, credential)));
    setSelectedCredentials(current => {
      const next = new Set(current), all = keys.every(key => current.has(key));
      keys.forEach(key => { if (all) { next.delete(key); selectionGenerations.current.delete(key); } else { next.add(key); selectionGenerations.current.set(key, channel.generation); } });
      return next;
    });
  }
  function saveCredentialOrder(channel: Channel, ids: string[], propagateError = false) {
    return changeCredentials(channel, 'credential-order', { credential_ids: ids }, propagateError, () => {
      onCredentialOrderSaved(channel.id, ids);
      setNotes(current => ({ ...current, [channel.id]: '凭证调用顺序已保存' }));
    });
  }
  function moveCredential(channel: Channel, from: string, to: string) {
    const ids = (channel.credentials || []).map(credential => credential.id);
    if (from === to || !ids.includes(from) || !ids.includes(to)) return;
    void saveCredentialOrder(channel, moved(ids, from, to));
  }
  const movable = (channel: Channel) => !!channel.credential_type && channel.credential_type !== 'zen_free' && !channel.credential_type.startsWith('market:');
  function moveTargets(source: Channel) { return pools.flatMap(pool => pool.channels).filter(target => target.id !== source.id && target.credential_type === source.credential_type && movable(source)); }
  function openMove(source: Channel, ids: string[]) { setMoveDialog({ source, ids }); setMoveTarget(''); setMoveError(''); }
  async function transferCredentials(source: Channel, ids: string[], targetID: number) {
    const target = moveTargets(source).find(item => item.id === targetID);
    if (!target || !ids.length || moving.current) return;
    if (pendingCredentials.current.has(source.id) || pendingCredentials.current.has(target.id) || batchBusy.has(source.id) || batchBusy.has(target.id)) { setMoveError('源渠道或目标渠道正在保存，请稍后重试。'); return; }
    moving.current = true; setMoveBusy(true); setMoveError('');
    pendingCredentials.current.add(source.id); pendingCredentials.current.add(target.id);
    setCredentialBusy(current => new Set([...current, source.id, target.id]));
    try {
      await api<{ source: Channel; target: Channel }>(`/channels/${source.id}/credentials/move`, { method: 'POST', body: JSON.stringify({ target_channel_id: target.id, credential_ids: ids, expected_generation: source.generation, expected_target_generation: target.generation }) });
      setMoveDialog(undefined); setSelectedCredentials(current => { const next = new Set(current); for (const id of ids) { const key = quotaTargetKey({ channelID: source.id, credentialID: id }); next.delete(key); selectionGenerations.current.delete(key); } return next; });
      onAdded(); toast(`已移到「${target.name}」`);
    } catch { const message = '移动未完成，凭证保留原位置。请刷新渠道后重试。'; setMoveError(message); setCredentialErrors(current => ({ ...current, [source.id]: message })); }
    finally { moving.current = false; pendingCredentials.current.delete(source.id); pendingCredentials.current.delete(target.id); setMoveBusy(false); setCredentialBusy(current => { const next = new Set(current); next.delete(source.id); next.delete(target.id); return next; }); }
  }
  function externalDrop(source: Channel): Parameters<typeof useSortableList>[0]['externalDrop'] {
    if (!movable(source) || !moveTargets(source).length) return undefined;
    return {
      resolve: (x, y) => {
        for (const target of moveTargets(source)) {
          if (pendingCredentials.current.has(target.id) || batchBusy.has(target.id)) continue;
          const node = root.current?.querySelector<HTMLElement>(`[data-channel-drop="${target.id}"]`), rect = node?.getBoundingClientRect();
          if (rect && x >= rect.left && x <= rect.right && y >= rect.top && y <= rect.bottom && x >= 0 && y >= 0 && x <= innerWidth && y <= innerHeight) return String(target.id);
        }
      },
      onHover: (target, credential) => setDropHover(current => {
        if (!target || !credential) return current ? undefined : current;
        return current?.source === source.id && current.target === Number(target) && current.credential === credential ? current : { source: source.id, target: Number(target), credential };
      }),
      onDrop: (id, target) => transferCredentials(source, [id], Number(target)),
    };
  }
  function reorderPlatform(from: string, to: string) { setPreferences(current => ({ ...current, platforms: moved(displayed.map(pool => pool.provider.type), from, to) })); }
  function saveAccountOrder(platform: string, ids: number[]) {
    const visibleIDs = new Set(ids);
    setPreferences(current => {
      const all = ordered([...(pools.find(pool => pool.provider.type === platform)?.channels || [])].sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id), current.accounts[platform], channel => channel.id);
      let index = 0;
      return { ...current, accounts: { ...current.accounts, [platform]: all.map(channel => visibleIDs.has(channel.id) ? ids[index++] : channel.id) } };
    });
  }
  function reorderAccount(platform: string, rows: Channel[], from: number, to: number) { saveAccountOrder(platform, moved(rows.map(channel => channel.id), from, to)); }
  function tabKey(event: KeyboardEvent, index: number) {
    let next = index;
    if (event.key === 'ArrowRight') next = (index + 1) % visible.length;
    else if (event.key === 'ArrowLeft') next = (index - 1 + visible.length) % visible.length;
    else if (event.key === 'Home') next = 0;
    else if (event.key === 'End') next = visible.length - 1;
    else return;
    event.preventDefault();
    if (visible[next].provider.type !== type) afterRefreshDecision(() => {
      setPreferences(current => ({ ...current, active: visible[next].provider.type }));
      document.getElementById(`oauth-tab-${visible[next].provider.type}`)?.focus();
    });
  }
  const rows = selected ? ordered(selected.channels.filter(matchesSearch).sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id), preferences.accounts[selected.provider.type], channel => channel.id) : [];
  function saveVisiblePlatformOrder(ids: string[]) {
    const visibleIDs = new Set(ids);
    let index = 0;
    setPreferences(current => ({ ...current, platforms: displayed.map(pool => visibleIDs.has(pool.provider.type) ? ids[index++] : pool.provider.type) }));
  }
  const platformSortable = useSortableList({
    ids: visible.map(pool => pool.provider.type), axis: 'horizontal', disabled: loading,
    revision: JSON.stringify([pools, preferences]),
    onCommit: saveVisiblePlatformOrder,
  });
  const accountSortable = useSortableList({
    ids: rows.map(channel => String(channel.id)), disabled: loading,
    revision: JSON.stringify([type, rows, preferences.collapsedAccounts, [...selectedCredentials], observations, runtime?.data]),
    onCommit: ids => { if (type) saveAccountOrder(type, ids.map(Number)); },
  });
  return <div className="oauth-main" ref={root} onClickCapture={event => {
    if (!(event.target instanceof Element)) return;
    const selectedMenu = event.target.closest('summary')?.closest('.oauth-main-menu');
    if (selectedMenu) root.current?.querySelectorAll<HTMLDetailsElement>('.oauth-main-menu[open]').forEach(menu => { if (menu !== selectedMenu) menu.open = false; });
    const button = event.target.closest('button:not(:disabled)');
    const menu = button?.closest<HTMLDetailsElement>('.oauth-main-menu[open]');
    if (!menu || !root.current?.contains(menu)) return;
    // Restore a visible origin before an action mounts a modal and captures focus.
    menu.open = false;
    menu.querySelector('summary')?.focus();
  }}><div className="oauth-main-navigation"><div className="oauth-main-tabs" role="tablist" aria-label="OAuth 接入方式" {...platformSortable.listProps}>{visible.map((pool, index) => <div key={pool.provider.type} className={`oauth-main-tab-wrap ${type === pool.provider.type ? 'is-active' : ''}`} {...platformSortable.itemProps(pool.provider.type)}><button type="button" className="oauth-main-drag oauth-main-platform-drag" aria-label={`拖动接入方式 ${pool.provider.title}`} title="拖动改变接入方式显示顺序，也可用左右方向键移动" aria-keyshortcuts="ArrowLeft ArrowRight" disabled={loading || visible.length < 2} {...platformSortable.handleProps(pool.provider.type)} onKeyDown={event => { const target = event.key === 'ArrowLeft' ? visible[index - 1] : event.key === 'ArrowRight' ? visible[index + 1] : undefined; if (target) { event.preventDefault(); saveVisiblePlatformOrder(moved(visible.map(item => item.provider.type), pool.provider.type, target.provider.type)); } }}><GripVertical size={14} /></button><button type="button" role="tab" aria-label={pool.provider.title} id={`oauth-tab-${pool.provider.type}`} aria-controls={`oauth-panel-${pool.provider.type}`} aria-selected={type === pool.provider.type} tabIndex={type === pool.provider.type ? 0 : -1} onKeyDown={event => tabKey(event, index)} onClick={() => { if (type !== pool.provider.type) afterRefreshDecision(() => setPreferences(current => ({ ...current, active: pool.provider.type }))); }}>
            <span className="oauth-main-provider-icon" aria-hidden="true"><ProviderIcon name={pool.provider.title} override={preferences.icons[pool.provider.type]} fallbackIcon={pluginAccess[pool.provider.type]?.icon} size={20} /></span>
            <span className="oauth-main-tab-label">{pool.provider.title}<span>{pool.channels.length}</span></span>
          </button>
          {type === pool.provider.type && <button type="button" className="oauth-main-icon-edit" aria-label={`更换 ${pool.provider.title} 图标`} aria-haspopup="dialog" title="更换接入方式图标" onClick={() => setIconProvider(pool.provider)} /> }</div>)}</div><Button variant="ghost" onClick={() => setManaging(true)}><Settings2 size={16} />管理接入方式</Button><label className="oauth-main-auto-open" title="授权时自动打开浏览器；关闭后手动复制链接到任意浏览器登录。"><input type="checkbox" className="checkbox" aria-label="授权时自动打开浏览器" checked={autoOpen} disabled={autoOpenDisabled} onChange={event => onAutoOpenChange?.(event.target.checked)} /><span>自动打开浏览器</span></label></div>{loading && <p className="oauth-main-loading" role="status">正在更新渠道…</p>}{selected ? <section role="tabpanel" id={`oauth-panel-${type}`} aria-labelledby={`oauth-tab-${type}`} className="oauth-main-panel"><div className="oauth-main-pool-heading"><p>{selected.provider.note}{!selected.provider.official && <Badge tone="amber">非官方</Badge>}</p>{type === 'codex_oauth' && <RefreshSettings register={registerRefreshLeave} />}</div>{networkBlocked && <p className="oauth-main-plugin-notice" role="status">{access?.message || '接入插件状态未就绪，暂不可执行联网操作。'} {access?.pluginID && <a href={`#/plugins?category=connect&plugin=${encodeURIComponent(access.pluginID)}`}>前往插件库</a>}</p>}{!rows.length && !loading && <p className="oauth-main-empty">{query ? '未找到匹配渠道，试试其他名称、模型或地址。' : type === 'zen_free' ? '尚未添加公共接入。' : '尚未添加渠道，请先在页头添加渠道。'}</p>}<div className="oauth-main-channel-list" {...accountSortable.listProps}>{rows.map((channel, index) => <section key={channel.id} className={`oauth-main-channel${dropHover?.target === channel.id ? ' is-drop-target' : ''}`} data-channel-drop={channel.id} aria-label={`渠道 ${channel.name}`} {...accountSortable.itemProps(String(channel.id))}>{dropHover?.target === channel.id && <p className="oauth-main-drop-label" role="status">{(() => { const source = selected?.channels.find(item => item.id === dropHover.source); const index = source?.credentials?.findIndex(item => item.id === dropHover.credential) ?? -1; return source && index >= 0 ? credentialName(source.credentials![index], index, source.credentials!.length) : '凭证'; })()} · 移到「{channel.name}」</p>}<header className="oauth-main-channel-heading"><button type="button" className="oauth-main-drag" aria-label={`拖动渠道 ${channel.name}`} title="拖动渠道显示顺序，也可用上下方向键移动" aria-keyshortcuts="ArrowUp ArrowDown" disabled={loading || rows.length < 2} {...accountSortable.handleProps(String(channel.id))} onKeyDown={event => { const target = event.key === 'ArrowUp' ? rows[index - 1] : event.key === 'ArrowDown' ? rows[index + 1] : undefined; if (target) { event.preventDefault(); reorderAccount(type!, rows, channel.id, target.id); } }}><GripVertical size={16} /></button><div className="oauth-main-channel-title"><div className="oauth-main-channel-name"><h2><button type="button" disabled={loading || busy != null || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} aria-label={`编辑渠道名称 ${channel.name}`} onClick={() => onEdit(channel)}>{channel.name}</button></h2><button type="button" className="oauth-main-credential-link" disabled={loading || busy != null || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} aria-label={`配置 ${channel.name} 凭证`} onClick={() => onEdit(channel, 'credentials')}><KeyRound size={13} />{(channel.credentials || []).length} 个凭证</button><button type="button" className="oauth-main-collapse" aria-label={`${preferences.collapsedAccounts[channel.id] ? '展开' : '收起'} ${channel.name} 凭证区`} aria-expanded={!preferences.collapsedAccounts[channel.id]} aria-controls={`oauth-credentials-${channel.id}`} onClick={() => setPreferences(current => ({ ...current, collapsedAccounts: { ...current.collapsedAccounts, [channel.id]: !current.collapsedAccounts[channel.id] } }))}><ChevronDown size={16} /><span>{preferences.collapsedAccounts[channel.id] ? '展开' : '收起'}</span></button></div><details className="oauth-main-models"><summary aria-label={`查看 ${channel.name} 全部模型`}><Layers size={14} />{channel.models.length} 个启用模型<ChevronDown size={14} /></summary><div>{channel.models.length ? channel.models.map(model => <span key={model}>{model}</span>) : <span>尚未启用模型</span>}</div></details></div><div className="oauth-main-channel-actions">{type !== 'zen_free' && <Button variant="secondary" aria-label={`向 ${channel.name} 添加账号 / 工作区`} disabled={loading || busy != null || credentialBusy.has(channel.id) || batchBusy.has(channel.id)} onClick={() => setAddingTo(channel)}><Plus size={14} />添加账号 / 工作区</Button>}<Button variant="ghost" aria-label={`编辑 ${channel.name}`} disabled={loading || busy != null || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} onClick={() => onEdit(channel)}><Pencil size={14} />编辑</Button><Button variant="ghost" aria-label={`测试 ${channel.name}`} disabled={networkBlocked || loading || busy != null || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} onClick={() => onTest(channel)}>测试</Button><button type="button" role="switch" aria-checked={channel.enabled} aria-label={`${channel.enabled ? '停用' : '启用'} ${channel.name}`} disabled={busy != null || loading || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} className={`switch ${channel.enabled ? 'switch-on' : ''}`} onClick={() => onToggle(channel)}><span /></button><span className="oauth-main-enabled">{channel.enabled ? '已启用' : '已停用'}</span><details className="oauth-main-menu" onKeyDown={event => { if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); event.currentTarget.open = false; event.currentTarget.querySelector('summary')?.focus(); } }}><summary aria-label={`更多操作 ${channel.name}`} title="更多操作"><MoreHorizontal size={18} /></summary><div><Button variant="ghost" aria-label={`获取模型 ${channel.name}`} disabled={networkBlocked || loading || (credentialBusy.has(channel.id) || batchBusy.has(channel.id)) || fetching.has(channel.id)} onClick={() => void fetchModels(channel)}><PackageSearch size={14} />{fetching.has(channel.id) ? '正在获取…' : '获取模型'}</Button>{type !== 'zen_free' && !type?.startsWith('market:') && <Button variant="ghost" aria-label={`重新授权 ${channel.name}`} disabled={networkBlocked || loading || busy != null || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} onClick={() => onReauthorize(channel)}>重新授权</Button>}<Button variant="ghost" aria-label={`上移账号 ${channel.name}`} disabled={index === 0} onClick={() => reorderAccount(type!, rows, channel.id, rows[index - 1].id)}>上移</Button><Button variant="ghost" aria-label={`下移账号 ${channel.name}`} disabled={index === rows.length - 1} onClick={() => reorderAccount(type!, rows, rows[index + 1].id, channel.id)}>下移</Button><Button variant="ghost" aria-label={`删除 ${channel.name}`} disabled={busy != null || loading || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} onClick={() => onDelete(channel)}>删除渠道</Button></div></details></div></header><div id={`oauth-credentials-${channel.id}`} hidden={preferences.collapsedAccounts[channel.id]}><div className="oauth-main-credential-columns"><div className="oauth-main-row-controls"><CredentialSelectAll channel={channel} selected={selectedCredentials} blocked={loading || busy != null || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} onSelect={() => selectChannel(channel)} /><span aria-hidden="true" /></div><span>凭证信息</span><span>额度</span><span>状态</span><span>操作</span></div>{(channel.credentials || []).some(credential => selectedCredentials.has(quotaTargetKey(credentialTarget(channel, credential)))) && <>{movable(channel) && <Button variant="secondary" disabled={loading || moveBusy || busy != null || credentialBusy.has(channel.id) || batchBusy.has(channel.id)} onClick={() => openMove(channel, (channel.credentials || []).filter(credential => selectedCredentials.has(quotaTargetKey(credentialTarget(channel, credential)))).map(credential => credential.id))}>移动选中凭证</Button>}<OAuthCredentialBatch channel={channel} items={(channel.credentials || []).flatMap((credential, credentialIndex) => { const target = credentialTarget(channel, credential); const key = quotaTargetKey(target); return selectedCredentials.has(key) ? [{ credential, name: credentialName(credential, credentialIndex, channel.credentials!.length), target, generation: selectionGenerations.current.get(key) }] : []; })} codex={type === 'codex_oauth'} networkBlocked={networkBlocked} observations={observations} refresh={refreshQuota} blocked={loading || busy != null || credentialBusy.has(channel.id)} onChanged={onAdded} onGeneration={generation => setSelectedCredentials(current => { for (const credential of channel.credentials || []) { const key = quotaTargetKey(credentialTarget(channel, credential)); if (current.has(key) && selectionGenerations.current.get(key) === channel.generation) selectionGenerations.current.set(key, generation); } return new Set(current); })} onBusy={value => setBatchBusy(current => { const next = new Set(current); if (value) next.add(channel.id); else next.delete(channel.id); return next; })} clear={() => setSelectedCredentials(current => { const next = new Set(current); for (const credential of channel.credentials || []) { const key = quotaTargetKey(credentialTarget(channel, credential)); next.delete(key); selectionGenerations.current.delete(key); } return next; })} /></>}<CredentialSortableList onAdd={type !== 'zen_free' ? () => setAddingTo(channel) : undefined} externalDrop={externalDrop(channel)} channel={channel} blocked={loading || busy != null || credentialBusy.has(channel.id) || batchBusy.has(channel.id) || !!preferences.collapsedAccounts[channel.id]} revision={JSON.stringify([channel, preferences.collapsedAccounts[channel.id], observations, runtime?.data])} onCommit={ids => saveCredentialOrder(channel, ids, true)} render={(credential, credentialIndex, sortable) => { const target = credentialTarget(channel, credential); return <CredentialRow key={quotaTargetKey(target)} channel={channel} credential={credential} name={credentialName(credential, credentialIndex, channel.credentials!.length)} selected={selectedCredentials.has(quotaTargetKey(target))} onMoveTo={movable(channel) ? () => openMove(channel, [credential.id]) : undefined} onSelect={() => selectCredential(quotaTargetKey(target), channel.generation)} codex={type === 'codex_oauth'} observation={observations[quotaTargetKey(target)]} refresh={refreshQuota} onChanged={onAdded} runtime={runtime?.data?.rows.find(row => row.channel_id === channel.id && row.credential_id === credential.id)} staleRuntime={!!runtime?.error} blocked={loading || busy != null || (credentialBusy.has(channel.id) || batchBusy.has(channel.id))} networkBlocked={networkBlocked} index={credentialIndex} move={(from, to) => moveCredential(channel, from, to)} toggle={() => void changeCredentials(channel, `credentials/${encodeURIComponent(credential.id)}/enabled`, { enabled: credential.enabled === false })} rowProps={sortable.itemProps(credential.id)} handleProps={sortable.handleProps(credential.id)} />; }} /></div>{credentialErrors[channel.id] && <p className="oauth-main-model-note" role="alert">{credentialErrors[channel.id]}</p>}{notes[channel.id] && <p className="oauth-main-model-note" role="status" title={notes[channel.id]}>{notes[channel.id]}</p>}</section>)}</div>{type?.startsWith('market:') && <div className="oauth-main-add">{!networkBlocked && adding[type] ? <OAuthAuthorize type={type} authMethods={access?.authMethods} onDone={() => { setAdding(current => ({ ...current, [type]: false })); onAdded(); }} /> : <Button disabled={networkBlocked} onClick={() => setAdding(current => ({ ...current, [type!]: true }))}>添加账号 / 工作区</Button>}</div>}</section> : <p className="oauth-main-empty">{query ? '未找到匹配渠道，试试其他名称、模型或地址。' : '暂无可见接入方式，可管理显示或前往插件库启用。'}<a href="#/plugins?category=connect">前往插件库</a></p>}{managing && <Modal title="管理接入方式" description="仅调整本浏览器的显示与顺序；插件启停请前往插件库，渠道调用启停在渠道中设置。" onClose={() => setManaging(false)} className="oauth-main-manage"><div className="oauth-main-manage-list">{manageable.map((pool, index) => <div key={pool.provider.type}><span>{pool.provider.title}</span><button type="button" role="switch" aria-label={`显示 ${pool.provider.title}`} aria-checked={!preferences.hidden[pool.provider.type]} className={`switch ${!preferences.hidden[pool.provider.type] ? 'switch-on' : ''}`} onClick={() => { const change = () => setPreferences(current => ({ ...current, hidden: { ...current.hidden, [pool.provider.type]: !current.hidden[pool.provider.type] } })); if (type === pool.provider.type) afterRefreshDecision(change); else change(); }}><span /></button><span>{preferences.hidden[pool.provider.type] ? '已隐藏' : '显示'}</span><Button variant="ghost" aria-label={`上移接入方式 ${pool.provider.title}`} disabled={index === 0} onClick={() => reorderPlatform(pool.provider.type, manageable[index - 1].provider.type)}><ArrowUp size={16} /></Button><Button variant="ghost" aria-label={`下移接入方式 ${pool.provider.title}`} disabled={index === manageable.length - 1} onClick={() => reorderPlatform(manageable[index + 1].provider.type, pool.provider.type)}><ArrowDown size={16} /></Button></div>)}</div></Modal>}{moveDialog && <Modal title="移到其他渠道" onClose={() => setMoveDialog(undefined)} busy={moveBusy}><p>将 {moveDialog.ids.length} 个凭证从「{moveDialog.source.name}」移出，保留备注、启停与自动操作配置。</p><label>目标渠道<select className="input" aria-label="移动目标渠道" value={moveTarget} disabled={moveBusy} onChange={event => setMoveTarget(event.target.value)}><option value="">选择渠道</option>{moveTargets(moveDialog.source).map(target => <option key={target.id} value={target.id}>{target.name}</option>)}</select></label>{!moveTargets(moveDialog.source).length && <p>请先创建同一对象 / 地区的其他渠道。</p>}{moveError && <p role="alert">{moveError}</p>}<div className="modal-footer"><Button variant="secondary" disabled={moveBusy} onClick={() => setMoveDialog(undefined)}>取消</Button><Button disabled={moveBusy || !moveTarget} onClick={() => void transferCredentials(moveDialog.source, moveDialog.ids, Number(moveTarget))}>{moveBusy ? '正在移动…' : '确认移动'}</Button></div></Modal>}{addingTo && <AddOAuthCredential channel={addingTo} access={pluginAccess[addingTo.credential_type || '']} onClose={() => setAddingTo(undefined)} onChanged={onAdded} resumeFlow={flows[`channel:${addingTo.id}`]} onStarted={flow => onFlowStarted(`channel:${addingTo.id}`, flow)} />}{iconProvider && <ChannelIconPicker title="更换接入方式图标" name={iconProvider.title} fallbackIcon={pluginAccess[iconProvider.type]?.icon} value={preferences.icons[iconProvider.type]} onClose={() => setIconProvider(undefined)} onSelect={async value => { setPreferences(current => ({ ...current, icons: { ...current.icons, [iconProvider.type]: value } })); }} />}</div>;
}
