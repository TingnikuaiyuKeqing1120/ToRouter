import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { ArrowUp, Download, Info, RefreshCw } from 'lucide-react';
import { api, ApiError, errorMessage } from '../api';
import type { AppUpdateInstallRequest, AppUpdateStatus } from '../types/appUpdates';
import { NavigationCancelled } from './NavigationGuard';
import { Button, ErrorBox, Modal } from './ui';
import './AppUpdates.css';

interface UpdateContext {
  status: AppUpdateStatus | null;
  error: string;
  busy: boolean;
  installing: boolean;
  open: () => void;
  check: () => void;
  observe: () => () => void;
}
const AppUpdates = createContext<UpdateContext>({ status: null, error: '', busy: false, installing: false, open: () => {}, check: () => {}, observe: () => () => {} });

export function AppUpdatesProvider({ children, beforeInstall, onUpdateIntent }: { children: ReactNode; beforeInstall: () => Promise<void>; onUpdateIntent: (active: boolean) => void }) {
  const [status, setStatus] = useState<AppUpdateStatus | null>(null);
  const [error, setError] = useState('');
  const [open, setOpen] = useState(false);
  const [observers, setObservers] = useState(0);
  const [busy, setBusy] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [preparing, setPreparing] = useState(false);
  const session = useRef<AbortController>();
  const sequence = useRef(0);
  const operation = useRef(false);
  const installPending = useRef(false);
  const installAccepted = useRef(false);
  const updateIntent = useRef(onUpdateIntent); updateIntent.current = onUpdateIntent;
  const polling = useRef(false);
  const observe = useCallback(() => { setObservers(value => value + 1); return () => setObservers(value => value - 1); }, []);
  const refresh = useCallback(async (manual = false) => {
    const controller = session.current;
    if (!controller || controller.signal.aborted || (!manual && polling.current) || (operation.current && manual)) return;
    if (manual) { operation.current = true; setBusy(true); setError(''); }
    polling.current = true;
    const id = ++sequence.current;
    const acceptedAtStart = installAccepted.current;
    try {
      const next = await api<AppUpdateStatus>(`/app-updates${manual ? '/check' : ''}`, { method: manual ? 'POST' : 'GET', signal: controller.signal, suppressAuthExpired: installPending.current && !manual });
      if (!controller.signal.aborted && id === sequence.current) {
        setStatus(next); setError('');
        // An acknowledged request can still fail to launch its replacement.
        // Only a subsequent live status response ends that pending install;
        // connection failures continue waiting for the verified successor.
        if (installPending.current && acceptedAtStart && next.state === 'error') {
          installPending.current = false; installAccepted.current = false; operation.current = false;
          setPreparing(false); setInstalling(false); updateIntent.current(false);
          setError(next.error || '更新未能启动，请重试。');
        }
      }
    } catch (e) { if (!controller.signal.aborted && id === sequence.current) setError(errorMessage(e)); }
    finally { polling.current = false; if (manual) { operation.current = false; if (!controller.signal.aborted) setBusy(false); } }
  }, []);
  useEffect(() => {
    const controller = new AbortController(); session.current = controller; polling.current = false;
    const expired = () => { controller.abort(); ++sequence.current; setStatus(null); setError(''); };
    window.addEventListener('auth-expired', expired);
    void refresh();
    return () => { controller.abort(); ++sequence.current; window.removeEventListener('auth-expired', expired); };
  }, [refresh]);
  useEffect(() => {
    if (status && !status.enabled) return;
    // The visible sidebar needs cached status too: server checks may finish
    // after the initial GET. Hidden idle tabs do not keep polling.
    const timer = window.setInterval(() => { if (installing || document.visibilityState !== 'hidden') void refresh(); }, installing ? 1000 : open || observers || status?.state === 'checking' ? 15000 : 60000);
    return () => window.clearInterval(timer);
  }, [open, observers, installing, status?.state, status?.enabled, refresh]);
  async function install() {
    const controller = session.current;
    if (operation.current || !status?.available || !controller || controller.signal.aborted) return;
    operation.current = true; setPreparing(true); setError('');
    let submitted = false;
    try {
      // The existing page owns save/discard/cancel; the update cannot bypass it.
      await beforeInstall();
      if (controller.signal.aborted) return;
      setPreparing(false); setInstalling(true); installPending.current = true; installAccepted.current = false; updateIntent.current(true); ++sequence.current; submitted = true;
      const request: AppUpdateInstallRequest = { version: status.available.version, sha256: status.available.sha256, ...(window.__TO_ROUTER_DESKTOP__ ? { open_mode: 'desktop' as const } : {}) };
      await api<{ requested: boolean; port: number; version: string }>('/app-updates/install', { method: 'POST', body: JSON.stringify(request), signal: controller.signal });
      if (!controller.signal.aborted) installAccepted.current = true;
      // Keep locked until the successor reloads or a live status reports failure.
    } catch (e) {
      if (controller.signal.aborted) return;
      if (!(e instanceof NavigationCancelled)) setError(submitted && !(e instanceof ApiError) ? '更新请求的连接已中断，正在等待程序恢复，请勿重复安装。' : errorMessage(e));
      if (!submitted || e instanceof ApiError) { submitted = false; installPending.current = false; installAccepted.current = false; setInstalling(false); updateIntent.current(false); operation.current = false; }
    } finally { if (!controller.signal.aborted) { setPreparing(false); if (!submitted) operation.current = false; } }
  }
  const locked = busy || preparing || installing || status?.state === 'checking' || status?.state === 'downloading';
  const message = updateMessage(status, busy, installing);
  return <AppUpdates.Provider value={{ status, error, busy: !!locked, installing, open: () => setOpen(true), check: () => void refresh(true), observe }}>
    {children}
    {open && <Modal title="软件更新" description="查看 to-router 版本与更新说明。" onClose={() => setOpen(false)} busy={preparing || installing}>
      <div className="app-update-content">
        <div className="app-update-versions"><span>当前版本 <strong>{status?.current_version || '读取中…'}</strong></span>{status?.available && <span>新版本 <strong>{status.available.version}</strong></span>}</div>
        <p role="status" className="app-update-status">{message}</p>
        {(error || status?.error) && <ErrorBox message={error || status?.error || ''} />}
        <AppUpdateResult status={status} />
        {status?.available && <section className="app-update-notes" aria-label="更新说明"><h3>更新说明</h3><p>{status.available.notes?.trim() || '本版本未提供更新说明。'}</p></section>}
        {status?.available && <p className="app-update-hint">更新将下载并校验新版本，然后自动重启程序。你的数据与启动偏好会保留。</p>}
      </div>
      <div className="modal-footer app-update-actions">
        <Button variant="secondary" disabled={!!locked || !status?.enabled} onClick={() => void refresh(true)}><RefreshCw size={15} />检查更新</Button>
        {status?.available && <Button disabled={!!locked} onClick={() => void install()}><Download size={15} />{preparing ? '正在准备…' : installing ? '更新并重启中…' : '更新并重启'}</Button>}
      </div>
    </Modal>}
  </AppUpdates.Provider>;
}

function updateMessage(status: AppUpdateStatus | null, checking: boolean, installing: boolean) {
  if (installing) return status?.state === 'downloading' ? '正在下载并校验更新，完成后将自动重启…' : '正在准备更新并重启，请稍候…';
  if (!status) return '正在读取版本信息…';
  if (!status.enabled) return '此构建暂未启用自动更新。';
  if (checking || status.state === 'checking') return '正在检查更新…';
  if (status.state === 'downloading') return '正在下载并校验更新…';
  if (status.available) return `发现新版本 ${status.available.version}`;
  if (status.error || status.state === 'error') return '检查更新未完成，请重试。';
  return status.last_checked ? '当前已是最新版本。' : '尚未检查更新。';
}

function AppUpdateResult({ status }: { status: AppUpdateStatus | null }) {
  const result = status?.last_result;
  if (!status || !result) return null;
  let summary: string;
  if (result.state === 'updated') summary = `最近已成功更新至 ${result.version}。当前运行版本 ${status.current_version}。`;
  else if (result.state === 'rolled_back') summary = result.executable_restored
    ? `更新 ${result.version} 失败，已恢复旧版程序，当前运行版本 ${status.current_version}。`
    : `更新 ${result.version} 失败，已重新启动原版本 ${status.current_version}。`;
  else summary = `最近更新 ${result.version} 未完成。当前运行版本 ${status.current_version}。`;
  return <section className="app-update-result" aria-label="最近更新结果">
    <p role="status" className="app-update-result-summary">{summary}</p>
    {result.error && <p>原因：{result.error}</p>}
    {result.data_restored && <p>数据已恢复到本次更新前的快照；更新尝试期间的写入不会保留。</p>}
    {result.state === 'rolled_back' && !result.data_restored && <p>本次未执行数据快照回退。</p>}
    {result.view_error && <p role="status">程序已恢复运行，但暂未打开视图：{result.view_error}。可从托盘重新打开。</p>}
  </section>;
}

export function AppUpdateArrow() {
  const updates = useContext(AppUpdates);
  return <span className="app-update-arrow-slot">{updates.status?.available && <button type="button" className="app-update-arrow" aria-label={`发现新版本 ${updates.status.available.version}，查看更新`} title={`发现新版本 ${updates.status.available.version}`} onClick={updates.open}><ArrowUp size={16} strokeWidth={2.5} /></button>}</span>;
}

export function AppUpdatePanel() {
  const updates = useContext(AppUpdates);
  useEffect(() => updates.observe(), [updates.observe]);
  return <section className="panel app-update-panel" aria-label="关于与更新">
    <div className="app-update-about"><Info size={18} /><div><h2>关于 to-router</h2><p>当前版本 {updates.status?.current_version || '读取中…'}</p></div></div>
    <div className="app-update-panel-status"><p role="status">{updateMessage(updates.status, updates.busy, updates.installing)}</p>{(updates.error || updates.status?.error) && <p role="alert">{updates.error || updates.status?.error}</p>}<AppUpdateResult status={updates.status} /></div>
    <div className="app-update-panel-actions"><Button variant="secondary" disabled={updates.busy || !updates.status?.enabled} onClick={updates.check}><RefreshCw size={14} />检查更新</Button><Button variant="ghost" onClick={updates.open}>版本与说明</Button></div>
  </section>;
}
