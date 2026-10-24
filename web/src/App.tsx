import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Activity, BarChart3, BookOpen, Coins, KeyRound, Layers, LogOut, Moon, Puzzle, ScrollText, Search, Settings as SettingsIcon, Sparkles, Sun, Tag as TagIcon, Unplug, Plug } from 'lucide-react';
import { BrandLogo } from './components/BrandLogo';
import { api, ApiError, clearLegacyKey, errorMessage } from './api';
import { ErrorBox, Loading, ToastProvider } from './components/ui';
import { CommandPalette } from './components/CommandPalette';
import FloatingPet from './companion/FloatingPet';
import { useCompanionPreferences } from './companion/preferences';
import { useDesktopReady } from './companion/desktop';
import { useBackendLifecycle } from './hooks/backendLifecycle';
import Login from './pages/Login';
import { RuntimeStatusProvider } from './hooks/useRuntimeStatus';
import { RuntimeStatusIndicator } from './components/RuntimeStatusIndicator';
import { ConfirmProvider } from './components/Confirm';
import { NavigationCancelled, NavigationGuard, type BeforeLeave } from './components/NavigationGuard';
import { clearStableResources } from './services/stableResources';
import { resetCatalogView } from './pages/Catalog.cache';
import { AppUpdateArrow, AppUpdatesProvider } from './components/AppUpdates';

// The floating pet never renders the management pages or their chart runtime.
const Channels = lazy(() => import('./pages/Channels'));
const ClientAccess = lazy(() => import('./pages/ClientAccess'));
const CompanionSettings = lazy(() => import('./pages/CompanionSettings'));
const Status = lazy(() => import('./pages/Status'));
const MCP = lazy(() => import('./pages/MCP'));
const Logs = lazy(() => import('./pages/Logs'));
const Models = lazy(() => import('./pages/Models'));
const ModelSpecs = lazy(() => import('./pages/ModelSpecs'));
const Catalog = lazy(() => import('./pages/Catalog'));
const Plugins = lazy(() => import('./pages/Plugins'));
const Tags = lazy(() => import('./pages/Tags'));
const Settings = lazy(() => import('./pages/Settings'));
const Usage = lazy(() => import('./pages/Usage'));
const Prices = lazy(() => import('./pages/Prices'));

const pages = [
  { id: 'usage', label: '用量概览', icon: BarChart3, component: Usage, group: '数据看板' },
  { id: 'channels', label: '上游接入', icon: Unplug, component: Channels, group: '路由调配' },
  { id: 'models', label: '公开模型', icon: Layers, component: Models, group: '路由调配' },
  { id: 'client-access', label: '客户端接入', icon: KeyRound, component: ClientAccess, group: '路由调配' },
  { id: 'model-specs', label: '参数配置', icon: BookOpen, component: ModelSpecs, group: '模型配置' },
  { id: 'prices', label: '价格配置', icon: Coins, component: Prices, group: '模型配置' },
  { id: 'tags', label: '标签配置', icon: TagIcon, component: Tags, group: '模型配置' },
  { id: 'status', label: '运行状态', icon: Activity, component: Status, group: '运维中枢' },
  { id: 'mcp', label: 'MCP控制台', icon: Plug, component: MCP, group: '运维中枢' },
  { id: 'logs', label: '请求日志', icon: ScrollText, component: Logs, group: '运维中枢' },
  { id: 'catalog', label: '模型数据库', icon: BookOpen, component: Catalog, group: '运维中枢' },
  { id: 'plugins', label: '插件库', icon: Puzzle, component: Plugins, group: '运维中枢' },
  { id: 'settings', label: '系统设置', icon: SettingsIcon, component: Settings, group: '运维中枢' },
  { id: 'companion', label: '桌宠管理', icon: Sparkles, component: CompanionSettings, group: '桌宠' },
] as const;

const navPages = pages.filter(item => item.id !== 'companion');

export default function App() {
  const [authed, setAuthed] = useState(false);
  const [restoring, setRestoring] = useState(true);
  const [sessionError, setSessionError] = useState('');
  const [sessionVersion, setSessionVersion] = useState(0);
  const [loggingOut, setLoggingOut] = useState(false);
  const [logoutError, setLogoutError] = useState('');
  const [leaveError, setLeaveError] = useState('');
  const [leaving, setLeaving] = useState(false);
  const desktopReady = useDesktopReady();
  const reconnect=useCallback((origin:string, _ready?: unknown, updated?: boolean)=>{
    if (window.__TO_ROUTER_DESKTOP__) return;
    if(origin!==window.location.origin) window.location.replace(`${origin}/${window.location.hash}`);
    else if (updated) window.location.reload();
  },[]);
  const lifecycle=useBackendLifecycle(authed&&(!desktopReady||!window.__TO_ROUTER_DESKTOP__),reconnect);
  const preferences = useCompanionPreferences(authed&&lifecycle.state!=='stopped');
  const { theme, paused } = preferences.value;
  const gatewayAddress = (window.__TO_ROUTER_DESKTOP__?.origin ?? window.location.origin).replace(/^https?:\/\//, '');
  const [paletteOpen, setPaletteOpen] = useState(false);

  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setPaletteOpen(open => !open);
      }
    };
    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, []);

  const leaveHandlers = useRef<BeforeLeave[]>([]);
  const navigating = useRef(false);
  const guard = useMemo(() => ({ register: (handler: BeforeLeave) => { leaveHandlers.current.push(handler); return () => { leaveHandlers.current = leaveHandlers.current.filter(item => item !== handler); }; } }), []);
  async function beforeLeave() {
    // Inner dialogs settle their drafts before the containing page is checked.
    for (const handler of [...leaveHandlers.current].reverse()) await handler();
  }
  function parseHashPath(raw: string): string {
    return raw.replace(/^#\/?/, '').split('?')[0].trim();
  }

  const [isPet, setIsPet] = useState(() => {
    const hash = parseHashPath(window.location.hash);
    return hash === 'pet' || hash === 'floating-pet';
  });
  const [page, setPage] = useState<(typeof pages)[number]['id']>(() => {
    const hash = parseHashPath(window.location.hash);
    return pages.some(p => p.id === hash) ? hash as (typeof pages)[number]['id'] : 'usage';
  });
  const [routeHash, setRouteHash] = useState(() => window.location.hash || `#/${page}`);
  const routeHashRef = useRef(routeHash); routeHashRef.current = routeHash;
  const pageRef = useRef(page); pageRef.current = page;
  const petRef = useRef(isPet); petRef.current = isPet;

  useEffect(() => {
    try { localStorage.setItem('to-router-theme', theme); } catch {}
    if (theme === 'light') {
      document.documentElement.classList.add('light');
      document.documentElement.classList.remove('dark');
      document.documentElement.setAttribute('data-theme', 'light');
    } else {
      document.documentElement.classList.add('dark');
      document.documentElement.classList.remove('light');
      document.documentElement.setAttribute('data-theme', 'dark');
    }
    const applyNativeTheme = () => {
      if (!window.__TO_ROUTER_DESKTOP__) return;
      if (theme === 'light') window.runtime?.WindowSetLightTheme?.();
      else window.runtime?.WindowSetDarkTheme?.();
    };
    window.addEventListener('to-router-desktop-ready', applyNativeTheme);
    applyNativeTheme();
    return () => window.removeEventListener('to-router-desktop-ready', applyNativeTheme);
  }, [theme]);

  const toggleTheme = () => preferences.update({ theme: theme === 'dark' ? 'light' : 'dark' });
  const togglePaused = () => preferences.update({ paused: !paused });

  async function navigate(next: (typeof pages)[number]['id'] | 'pet', destination = `#/${next}`) {
    const current = petRef.current ? 'pet' : pageRef.current;
    if ((next === current && destination === routeHashRef.current) || navigating.current) return;
    navigating.current = true; setLeaving(true); setLeaveError('');
    try {
      await beforeLeave();
      history.replaceState(null, '', destination);
      setRouteHash(destination);
      setIsPet(next === 'pet');
      if (next !== 'pet') setPage(next);
    }
    catch (e) { if (!(e instanceof NavigationCancelled)) setLeaveError(errorMessage(e)); history.replaceState(null, '', routeHashRef.current); }
    finally { navigating.current = false; setLeaving(false); }
  }

  useEffect(() => {
    if (isPet) {
      window.location.hash = '#/pet';
      document.title = 'to-router 悬浮桌宠';
    } else {
      window.location.hash = routeHash;
      document.title = `${pages.find(p => p.id === page)?.label} · to-router 控制台`;
    }
  }, [page, isPet, routeHash]);

  useEffect(() => {
    const changed = () => {
      const id = parseHashPath(window.location.hash);
      const next = id === 'pet' || id === 'floating-pet' ? 'pet' : id;
      const destination = window.location.hash;
      if ((next === 'pet' || pages.some(p => p.id === next)) && destination !== routeHashRef.current) {
        // Keep the current page mounted and its URL intact until before-leave
        // finishes. A failed save must never replace the price workbench.
        history.replaceState(null, '', routeHashRef.current);
        void navigate(next as (typeof pages)[number]['id'] | 'pet', destination);
      }
    };
    window.addEventListener('hashchange', changed);
    return () => window.removeEventListener('hashchange', changed);
  }, []);

  useEffect(() => {
    const expired = () => { clearStableResources(); resetCatalogView(); setAuthed(false); setSessionVersion(v => v + 1); };
    window.addEventListener('auth-expired', expired);
    return () => window.removeEventListener('auth-expired', expired);
  }, []);

  useEffect(() => {
    clearLegacyKey();
    const abort = new AbortController();
    setRestoring(true); setSessionError('');
    const restore = async () => {
      try { await api('/auth/session', { signal: abort.signal, suppressAuthExpired: true }); }
      catch (e) {
        if (!(e instanceof ApiError && e.status === 401)) throw e;
        const host = window.location.hostname;
        if (host !== '127.0.0.1' && host !== 'localhost' && host !== 'wails.localhost' && host !== '[::1]') throw e;
        try {
          const local = await api<{authenticated: boolean}>('/auth/local', { method: 'POST', signal: abort.signal, suppressAuthExpired: true });
          if (!local.authenticated) throw e;
        } catch { throw e; }
      }
    };
    restore()
      .then(() => { if (!abort.signal.aborted) setAuthed(true); })
      .catch(e => { if (!abort.signal.aborted && !(e instanceof ApiError && e.status === 401)) setSessionError(errorMessage(e)); })
      .finally(() => { if (!abort.signal.aborted) setRestoring(false); });
    return () => abort.abort();
  }, [sessionVersion]);

  async function logout() {
    setLoggingOut(true); setLogoutError('');
    try { await beforeLeave(); await api('/auth/logout', { method: 'POST' }); clearStableResources(); resetCatalogView(); setAuthed(false); }
    catch (e) { if (!(e instanceof NavigationCancelled)) setLogoutError(errorMessage(e)); }
    finally { setLoggingOut(false); }
  }

  if(lifecycle.state==='stopped')return <div className="login-page"><div className="space-y-4 text-center"><h1 className="text-lg font-semibold">网关已停止</h1><p className="text-sm text-[var(--color-text-muted)]">重新启动 to-router 后即可继续使用。</p><button type="button" className="btn btn-secondary" onClick={()=>{lifecycle.retry();setSessionVersion(value=>value+1);}}>重新检查连接</button></div></div>;
  if (restoring) return <Loading />;
  if (sessionError) return <div className="login-page"><ErrorBox message={sessionError} retry={() => setSessionVersion(v => v + 1)} /></div>;
  if (!authed) return <ToastProvider><Login onSuccess={() => { setAuthed(true); setLogoutError(''); }} /></ToastProvider>;
  if (isPet) {
    return (
      <ToastProvider>
        <FloatingPet theme={theme} toggleTheme={toggleTheme} paused={paused} togglePaused={togglePaused} preferenceError={preferences.error} quotaTargets={preferences.value.quota_targets} />
      </ToastProvider>
    );
  }

  const Page = pages.find(p => p.id === page)!.component;

  return (
    <ToastProvider><ConfirmProvider><RuntimeStatusProvider><AppUpdatesProvider beforeInstall={beforeLeave} onUpdateIntent={lifecycle.expectUpdate}>
      <div className="app-shell">
        <aside className="sidebar">
          <div className="sidebar-brand">
            <span className="login-logo">
              <BrandLogo size={52} theme={theme} />
            </span>
            <div>
              <strong>to-router<AppUpdateArrow /></strong>
              <span>个人模型中枢</span>
            </div>
          </div>

          <div className="sidebar-status" title={`当前网关 (${gatewayAddress})`}>
            <span className="status-dot animate-pulse" />
            <span className="status-text font-mono">{gatewayAddress}</span>
            <span className="status-badge font-mono">在线</span>
          </div>

          <button
            type="button"
            className="sidebar-search-btn"
            onClick={() => setPaletteOpen(true)}
            title="快捷命令与导航 (Ctrl + K)"
          >
            <Search size={14} className="text-[var(--color-text-muted)] shrink-0" />
            <span>快速命令导航...</span>
            <kbd className="sidebar-kbd">Ctrl K</kbd>
          </button>

          <nav aria-label="主导航">
            {navPages.map((item, idx) => {
              const showGroup = idx === 0 || navPages[idx - 1].group !== item.group;
              return (
                <div key={item.id} className="flex flex-col">
                  {showGroup && <div className="nav-group-title">{item.group}</div>}
                  <button
                    className={page === item.id ? 'nav-item nav-active' : 'nav-item'}
                    aria-current={page === item.id ? 'page' : undefined}
                    disabled={leaving}
                    onClick={() => void navigate(item.id)}
                  >
                    <item.icon size={17} />
                    <span>{item.label}</span>{item.id === 'status' && <RuntimeStatusIndicator />}
                  </button>
                </div>
              );
            })}
          </nav>

          <div className="sidebar-footer">
            <a
              href="#/companion"
              className={page === 'companion' ? 'nav-item nav-active' : 'nav-item'}
              aria-current={page === 'companion' ? 'page' : undefined}
              title="管理桌面小宠"
              aria-disabled={leaving}
              onClick={event => { event.preventDefault(); void navigate('companion'); }}
            >
              <Sparkles size={17} />
              <span>桌面小宠</span>
            </a>
            <button
              type="button"
              className="nav-item"
              title={theme === 'dark' ? '切换为浅色模式' : '切换为暗黑模式'}
              onClick={toggleTheme}
            >
              {theme === 'dark' ? <Sun size={17} /> : <Moon size={17} />}
              <span>{theme === 'dark' ? '浅色模式' : '暗黑模式'}</span>
            </button>
            <button
              className="nav-item nav-exit"
              disabled={loggingOut}
              onClick={logout}
            >
              <LogOut size={17} />
              <span>退出登录</span>
            </button>
          </div>
        </aside>

        <div className="sidebar-mobile" role="navigation" aria-label="页面导航">
          {pages.map(item => (
            <button
              key={item.id}
              className={page === item.id ? 'nav-item nav-active' : 'nav-item'}
              aria-current={page === item.id ? 'page' : undefined}
              disabled={leaving}
              onClick={() => void navigate(item.id)}
            >
              <item.icon size={16} />
              <span>{item.label}</span>{item.id === 'status' && <RuntimeStatusIndicator />}
            </button>
          ))}
        </div>

        <main className="main" id="main">
          <div className="mb-4 flex items-center justify-between md:hidden">
            <div className="flex items-center gap-2">
              <BrandLogo size={36} theme={theme} />
              <span className="font-semibold text-sm">to-router<AppUpdateArrow /></span>
            </div>
            <div className="flex items-center gap-3">
              <button
                type="button"
                className="text-xs text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)] p-2 transition-colors"
                onClick={() => setPaletteOpen(true)}
                title="快速命令导航 (Ctrl + K)"
                aria-label="快速命令导航"
              >
                <Search size={15} />
              </button>
              <button
                type="button"
                className="text-xs text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)] p-2 transition-colors"
                onClick={toggleTheme}
                title="切换主题"
              >
                {theme === 'dark' ? <Sun size={15} /> : <Moon size={15} />}
              </button>
              <button
                type="button"
                className="flex items-center gap-1.5 text-xs text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)] p-2 transition-colors"
                disabled={loggingOut}
                onClick={logout}
              >
                <LogOut size={14} />
                <span>退出登录</span>
              </button>
            </div>
          </div>
          {logoutError && <ErrorBox message={logoutError} />}
          {leaveError && <ErrorBox message={leaveError} />}
          {preferences.error && <ErrorBox message={preferences.error} />}
          <NavigationGuard.Provider value={guard}>
            <Suspense fallback={<Loading />}><Page key={routeHash} /></Suspense>
          </NavigationGuard.Provider>
        </main>
        <CommandPalette
          isOpen={paletteOpen}
          onClose={() => setPaletteOpen(false)}
          onNavigate={id => void navigate(id as (typeof pages)[number]['id'])}
          currentPage={page}
          toggleTheme={toggleTheme}
          theme={theme}
          onLogout={logout}
        />
      </div>
    </AppUpdatesProvider></RuntimeStatusProvider></ConfirmProvider></ToastProvider>
  );
}
