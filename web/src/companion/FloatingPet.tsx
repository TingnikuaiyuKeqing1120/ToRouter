import { memo, useEffect, useMemo, useRef, useState } from 'react';
import { ArrowLeft, Moon, Pause, Play, SlidersHorizontal, Sun, X, Zap } from 'lucide-react';
import { api, errorMessage, number } from '../api';
import { isNativePet, useDesktopReady, usePetWindowSize } from './desktop';
import { CompanionStage, type CompanionScene } from './CompanionStage';
import {
  failureLabel,
  useCompanionLive,
  type CompanionSnapshot,
  type ConnectionState,
  type FinishedRequest,
  type LiveRequest,
} from './live';
import { CompanionQuotaDock } from '../components/CompanionQuotaDock';
import type { QuotaTarget } from '../types/quota';
import './floating.css';

export interface FloatingPetLive {
  snapshot: CompanionSnapshot | null;
  connection: ConnectionState;
  notice: { scene: 'done' | 'error'; request: FinishedRequest; until: number } | null;
  scene: CompanionScene;
  refresh?: () => void;
}

export interface FloatingPetProps {
  theme?: 'dark' | 'light';
  toggleTheme?: () => void;
  className?: string;
  initialPaused?: boolean;
  paused?: boolean;
  togglePaused?: () => void;
  preferenceError?: string;
  live?: FloatingPetLive;
  quotaTargets?: QuotaTarget[];
}

export function formatSpeed(speed: number | null | undefined, estimated?: boolean, phase?: string): string {
  if (speed !== null && speed !== undefined && Number.isFinite(speed)) {
    const formatted = speed.toFixed(1);
    return `${estimated ? '≈' : ''}${formatted} tok/s`;
  }
  if (phase === 'waiting') {
    return '等待首包';
  }
  return '—';
}

export function calculatePeakSpeed(requests: LiveRequest[]): { peakStr: string; isEstimated: boolean } {
  let peak = 0;
  let hasValid = false;
  let isEstimated = false;

  for (const req of requests) {
    if (req.tokens_per_second !== null && req.tokens_per_second !== undefined && Number.isFinite(req.tokens_per_second)) {
      if (req.tokens_per_second > peak) {
        peak = req.tokens_per_second;
        hasValid = true;
        isEstimated = req.output_tokens_source === 'estimated';
      }
    }
  }

  const prefix = isEstimated ? '≈' : '';
  const peakStr = hasValid ? `${prefix}${peak.toFixed(1)}` : '0.0';
  return { peakStr, isEstimated };
}

const wailsDragStyle = {
  WebkitAppRegion: 'drag',
  '--wails-draggable': 'drag',
} as React.CSSProperties;

const wailsNoDragStyle = {
  WebkitAppRegion: 'no-drag',
  '--wails-draggable': 'no-drag',
} as React.CSSProperties;

export const FloatingPet = memo(function FloatingPet({
  theme = 'dark',
  toggleTheme,
  className = '',
  initialPaused = false,
  paused: controlledPaused,
  togglePaused,
  preferenceError = '',
  live,
  quotaTargets = [],
}: FloatingPetProps) {
  const defaultLive = useCompanionLive();
  const activeLive = live || defaultLive;
  const { snapshot, connection, notice, scene } = activeLive;

  const [localPaused, setLocalPaused] = useState(initialPaused);
  const paused = controlledPaused ?? localPaused;
  const onTogglePaused = togglePaused ?? (() => setLocalPaused(p => !p));
  const [isHovered, setIsHovered] = useState(false);
  const [drawerPinned, setDrawerPinned] = useState(false);
  const [drawerHovered, setDrawerHovered] = useState(false);
  const drawerOpen = drawerPinned || drawerHovered;
  const [dockOpen, setDockOpen] = useState(false);
  const [actionError, setActionError] = useState('');
  const [actionBusy, setActionBusy] = useState(false);
  const content = useRef<HTMLDivElement>(null);
  const desktopReady = useDesktopReady();
  usePetWindowSize(content);
  const runAction = async (path: string, after?: () => void) => {
    if (actionBusy) return;
    setActionBusy(true); setActionError('');
    try { await api(path, {method:'POST'}); after?.(); }
    catch (e) { setActionError(errorMessage(e)); }
    finally { setActionBusy(false); }
  };

  const isLight = theme === 'light';

  useEffect(() => {
    document.body.classList.add('pet-mode');
    document.documentElement.classList.add('pet-mode');
    if (isNativePet()) document.documentElement.classList.add('native-pet-mode');
    return () => {
      document.body.classList.remove('pet-mode');
      document.documentElement.classList.remove('pet-mode');
      document.documentElement.classList.remove('native-pet-mode');
    };
  }, [desktopReady]);

  const connected = connection === 'connected';
  const requests: LiveRequest[] = connected ? snapshot?.active ?? [] : [];
  const activeCount = connected ? snapshot?.active_count ?? 0 : 0;
  useEffect(() => { if (activeCount <= 3 || dockOpen) { setDrawerPinned(false); setDrawerHovered(false); } }, [activeCount, dockOpen]);

  const { peakStr: peakSpeed } = useMemo(() => calculatePeakSpeed(requests), [requests]);

  const today = snapshot?.today ?? {
    records: 0,
    input_tokens: 0,
    output_tokens: 0,
    estimated_records: 0,
  };

  const handleMascotClick = () => {
    setDockOpen(prev => !prev);
  };

  return (
    <div
      ref={content}
      className={`pet-drag-region relative flex min-h-[200px] flex-col items-center justify-end w-full select-none bg-transparent px-2 pt-2 pb-1 ${
        isLight ? 'text-slate-800' : 'text-slate-100'
      } ${className}`}
      data-wails-draggable="drag"
      style={wailsDragStyle}
      onMouseEnter={() => setIsHovered(true)}
      onMouseLeave={() => {
        setIsHovered(false);
        setDrawerPinned(false); setDrawerHovered(false);
      }}
    >
      {(actionError || preferenceError) && <p role="alert" className="pet-no-drag mb-2 w-[240px] rounded-xl bg-rose-950/90 p-2 text-[11px] text-rose-100">{actionError || preferenceError}</p>}
      {/* L2 Micro Control Dock (极简毛玻璃控制坞 - 位于桌宠正上方) */}
      {dockOpen && (
        <aside
          aria-label="极简控制坞"
          className={`pet-no-drag pet-glass-dock relative z-40 w-[248px] rounded-2xl p-3 shadow-2xl mb-1.5 animate-in fade-in slide-in-from-bottom-2 duration-150 ${
            isLight
              ? 'bg-white/95 border border-slate-300/80 text-slate-800'
              : 'border border-white/15 text-slate-100'
          }`}
          data-wails-draggable="no-drag"
          style={wailsNoDragStyle}
        >
          {/* Header */}
          <div className={`flex items-center justify-between pb-1.5 border-b ${isLight ? 'border-slate-200' : 'border-white/10'}`}>
            <span className={`text-xs font-semibold tracking-wide flex items-center gap-1.5 ${isLight ? 'text-slate-800' : 'text-slate-200'}`}>
              <span className="h-2 w-2 rounded-full bg-sky-400" />
              <span>to-router 伴侣控制坞</span>
            </span>
            <button
              type="button"
              aria-label="关闭控制坞"
              onClick={() => setDockOpen(false)}
              className={`p-1 rounded-lg transition-colors ${
                isLight ? 'text-slate-400 hover:text-slate-700 hover:bg-slate-100' : 'text-slate-400 hover:text-white hover:bg-white/10'
              }`}
            >
              <X size={13} />
            </button>
          </div>

          {/* Today's Usage Summary */}
          <div className={`mt-2 rounded-xl p-2 border ${
            isLight ? 'bg-slate-100/90 border-slate-200/80 text-slate-800' : 'bg-white/5 border-white/5 text-slate-100'
          }`}>
            <div className={`text-[10px] font-medium ${isLight ? 'text-slate-500' : 'text-slate-400'}`}>今日调用概览</div>
            <div className="mt-1 grid grid-cols-3 gap-1 text-center font-mono">
              <div>
                <div className={`text-[9px] ${isLight ? 'text-slate-500' : 'text-slate-400'}`}>调用记录</div>
                <div className={`mt-0.5 text-xs font-semibold ${isLight ? 'text-slate-900' : 'text-slate-100'}`}>
                  {number(today.records)}
                </div>
              </div>
              <div>
                <div className={`text-[9px] ${isLight ? 'text-slate-500' : 'text-slate-400'}`}>已报告输入</div>
                <div className={`mt-0.5 text-xs font-semibold truncate ${isLight ? 'text-slate-900' : 'text-slate-100'}`} title={String(today.input_tokens)}>
                  {number(today.input_tokens)}
                </div>
              </div>
              <div>
                <div className={`text-[9px] ${isLight ? 'text-slate-500' : 'text-slate-400'}`}>已报告输出</div>
                <div className={`mt-0.5 text-xs font-semibold truncate ${isLight ? 'text-emerald-600' : 'text-emerald-400'}`} title={String(today.output_tokens)}>
                  {number(today.output_tokens)}
                </div>
              </div>
            </div>
            {today.estimated_records > 0 && (
              <div className={`mt-1 text-[9px] text-center font-mono ${isLight ? 'text-amber-600' : 'text-amber-400'}`}>
                {number(today.estimated_records)} 条调用含估算数据
              </div>
            )}
          </div>

          <CompanionQuotaDock targets={quotaTargets} />

          {/* Quick Actions */}
          <div className="mt-2.5 flex items-center justify-between gap-1.5 text-xs">
            {/* Theme Toggle */}
            {toggleTheme && (
              <button
                type="button"
                aria-label="切换主题"
                title={isLight ? '切换深色模式' : '切换浅色模式'}
                onClick={toggleTheme}
                className={`flex-1 flex items-center justify-center gap-1 py-1.5 rounded-lg border text-[11px] font-medium transition-colors ${
                  isLight
                    ? 'bg-white hover:bg-slate-50 text-slate-700 hover:text-slate-900 border-slate-200 shadow-xs'
                    : 'bg-white/5 hover:bg-white/10 text-slate-300 hover:text-white border-white/5'
                }`}
              >
                {isLight ? <Moon size={12} /> : <Sun size={12} />}
                <span>{isLight ? '深色' : '浅色'}</span>
              </button>
            )}

            {/* Animation Play / Pause */}
            <button
              type="button"
              aria-label={paused ? '恢复动画' : '暂停动画'}
              title={paused ? '恢复动画' : '暂停动画'}
              aria-pressed={paused}
              onClick={onTogglePaused}
              className={`flex-1 flex items-center justify-center gap-1 py-1.5 rounded-lg border text-[11px] font-medium transition-colors ${
                isLight
                  ? 'bg-white hover:bg-slate-50 text-slate-700 hover:text-slate-900 border-slate-200 shadow-xs'
                  : 'bg-white/5 hover:bg-white/10 text-slate-300 hover:text-white border-white/5'
              }`}
            >
              {paused ? <Play size={12} /> : <Pause size={12} />}
              <span>{paused ? '恢复' : '暂停'}</span>
            </button>

            {/* Launch Full Console Window (Desktop Aware, Does NOT navigate pet window) */}
            <button
              type="button"
              aria-label="返回控制台"
              title="打开独立完整控制台"
              disabled={actionBusy}
              onClick={() => void runAction('/desktop/launch-gui')}
              className={`flex-1 flex items-center justify-center gap-1 py-1.5 rounded-lg border text-[11px] font-medium transition-colors ${
                isLight
                  ? 'bg-white hover:bg-slate-50 text-slate-700 hover:text-slate-900 border-slate-200 shadow-xs'
                  : 'bg-white/5 hover:bg-white/10 text-slate-300 hover:text-white border-white/5'
              }`}
            >
              <ArrowLeft size={12} />
              <span>控制台</span>
            </button>

            {/* Close Floating Pet Window */}
            <button
              type="button"
              aria-label="关闭桌宠"
              title="关闭悬浮桌宠"
              disabled={actionBusy}
              onClick={() => void runAction('/desktop/close-pet', () => { if (!isNativePet()) window.location.hash = '#/usage'; })}
              className={`flex-1 flex items-center justify-center gap-1 py-1.5 rounded-lg border text-[11px] font-medium transition-colors ${
                isLight
                  ? 'bg-white hover:bg-rose-50 border-slate-200 hover:border-rose-300 text-slate-700 hover:text-rose-600 shadow-xs'
                  : 'bg-white/5 hover:bg-rose-500/20 border-white/5 hover:border-rose-500/30 text-slate-300 hover:text-rose-200'
              }`}
            >
              <X size={12} />
              <span>关闭</span>
            </button>
          </div>
        </aside>
      )}

      {/* Floating Speed Pills Area (Displayed when dock is closed) */}
      {!dockOpen && (
        <div
          className="pet-no-drag relative z-20 flex flex-col items-center justify-end min-h-[32px] mb-1"
          data-wails-draggable="no-drag"
          style={wailsNoDragStyle}
        >
          {/* Case 0: 0 Concurrency (Standby / Done) */}
          {activeCount === 0 && (
            <div
              className={`transition-all duration-300 transform ${
                isHovered
                  ? 'opacity-100 translate-y-0 scale-100'
                  : 'opacity-0 translate-y-1 scale-95 pointer-events-none'
              }`}
            >
              <div
                className={`pet-glass-pill inline-flex items-center gap-1.5 px-3 h-[22px] rounded-full border text-[11px] shadow-sm ${
                  isLight
                    ? 'bg-white/90 border-slate-300/80 text-slate-700'
                    : 'bg-slate-900/70 border-white/10 text-slate-300'
                }`}
                role="status"
                aria-label="待机状态"
              >
                <span
                  className={`h-1.5 w-1.5 rounded-full ${
                    connection === 'error'
                      ? 'bg-rose-500'
                      : connection === 'loading'
                      ? 'bg-slate-400'
                      : scene === 'done'
                      ? 'bg-emerald-400'
                      : 'bg-sky-400'
                  }`}
                />
                <span>
                  {connection === 'loading'
                    ? '正在连接网关...'
                    : connection === 'error'
                    ? '连接中断，重试中'
                    : scene === 'done'
                    ? '🎉 本次任务已完成'
                    : scene === 'error'
                    ? `⚠️ ${failureLabel(notice?.request.error_code ?? '')}`
                    : '💤 待机打盹中 · 准备就绪'}
                </span>
              </div>
            </div>
          )}

          {/* Case A: 1~3 Concurrency (Vertical Micro Luminous Tier) */}
          {activeCount >= 1 && activeCount <= 3 && (
            <div
              className="flex flex-col items-center gap-1 w-full"
              aria-label="实时并发梯队"
            >
              {requests.map((req, idx) => {
                const isEstimated = req.output_tokens_source === 'estimated';
                const speedText = formatSpeed(req.tokens_per_second, isEstimated, req.phase);

                return (
                  <div
                    key={req.id || idx}
                    className={`pet-glass-pill inline-flex items-center gap-2 px-2.5 h-[20px] rounded-full border shadow-sm text-[11px] font-mono tracking-tight transition-transform duration-150 hover:scale-105 ${
                      isLight
                        ? 'bg-white/90 border-slate-300/80 text-slate-800'
                        : 'bg-slate-900/75 border-white/15 text-slate-100'
                    }`}
                    title={`${req.model} (${req.channel || '默认渠道'})`}
                  >
                    {/* Micro indicator light */}
                    <span
                      className={`h-1.5 w-1.5 shrink-0 rounded-full ${
                        req.phase === 'retrying'
                          ? 'bg-amber-400 shadow-[0_0_6px_rgba(245,158,11,0.8)]'
                          : req.phase === 'streaming'
                          ? 'bg-emerald-400 shadow-[0_0_6px_rgba(52,211,153,0.8)] animate-pulse'
                          : 'bg-sky-400/80'
                      }`}
                    />
                    {/* Channel Index / Short ID */}
                    <span className={`font-semibold shrink-0 ${isLight ? 'text-sky-600' : 'text-sky-400'}`}>#{idx + 1}</span>
                    {/* Model Short Name */}
                    <span className={`max-w-[100px] truncate ${isLight ? 'text-slate-700' : 'text-slate-200'}`}>{req.model}</span>
                    {/* Real-time Speed */}
                    <span className={`font-medium shrink-0 ${isLight ? 'text-emerald-600' : 'text-emerald-400'}`}>{speedText}</span>
                  </div>
                );
              })}
            </div>
          )}

          {/* Case C: >3 Concurrency (Aggregate Pill + Hover/Click Drawer) */}
          {activeCount > 3 && (
            <div className="relative flex flex-col items-center">
              {/* Single Aggregate Pill */}
              <button
                type="button"
                className={`pet-glass-pill order-2 inline-flex items-center gap-2 px-3 h-[22px] rounded-full border shadow-md text-[11px] font-mono cursor-pointer transition-all duration-150 hover:scale-105 ${
                  isLight
                    ? 'bg-white/95 border-amber-500/40 text-amber-800 hover:bg-amber-50/80'
                    : 'bg-slate-900/80 border-amber-500/30 text-amber-200 hover:bg-slate-800/90'
                }`}
                aria-label="聚合并发药丸"
                aria-expanded={drawerOpen}
                onMouseEnter={() => setDrawerHovered(true)}
                onClick={() => { if (drawerPinned) { setDrawerPinned(false); setDrawerHovered(false); } else setDrawerPinned(true); }}
              >
                <span className="tracking-tight">
                  🔥 {activeCount} 并发在飞 · 峰值 {peakSpeed} tok/s
                </span>
              </button>

              {/* Hover / Click Expanded Drawer */}
              {drawerOpen && (
                <div
                  className={`pet-glass-dock relative order-1 mb-1 z-30 flex flex-col gap-1.5 p-2.5 rounded-xl border shadow-2xl text-[11px] font-mono w-[244px] animate-in fade-in zoom-in-95 duration-150 ${
                    isLight
                      ? 'bg-white/95 border-slate-300/80 text-slate-800'
                      : 'bg-slate-950/90 border-white/15 text-slate-200'
                  }`}
                  aria-label="并发通道详情抽屉"
                  onMouseEnter={() => setDrawerHovered(true)}
                  onMouseLeave={() => setDrawerHovered(false)}
                >
                  <div className={`flex items-center justify-between border-b pb-1 text-[10px] ${
                    isLight ? 'border-slate-200 text-slate-500' : 'border-white/10 text-slate-400'
                  }`}>
                    <span className="flex items-center gap-1">
                      <Zap size={11} className={isLight ? 'text-amber-500' : 'text-amber-400'} />
                      <span>{activeCount > requests.length ? `并发通道 (${requests.length}/${activeCount})` : `全部并发通道 (${requests.length})`}</span>
                    </span>
                    <span>首包 / 速率</span>
                  </div>
                  <div className="max-h-[140px] overflow-y-auto space-y-1 pr-0.5">
                    {requests.map((req, idx) => {
                      const isEstimated = req.output_tokens_source === 'estimated';
                      const speedText = formatSpeed(req.tokens_per_second, isEstimated, req.phase);
                      const latencyText = req.first_byte_ms != null ? `${req.first_byte_ms}ms` : '—';

                      return (
                        <div
                          key={req.id || idx}
                          className={`flex items-center justify-between gap-1.5 rounded-lg px-2 py-1 ${
                            isLight ? 'bg-slate-100 hover:bg-slate-200/70' : 'bg-white/5 hover:bg-white/10'
                          }`}
                        >
                          <div className="flex items-center gap-1.5 truncate">
                            <span
                              className={`h-1.5 w-1.5 shrink-0 rounded-full ${
                                req.phase === 'retrying'
                                  ? 'bg-amber-400'
                                  : req.phase === 'streaming'
                                  ? 'bg-emerald-400'
                                  : 'bg-sky-400'
                              }`}
                            />
                            <span className={`font-semibold ${isLight ? 'text-sky-600' : 'text-sky-400'}`}>#{idx + 1}</span>
                            <span className={`truncate ${isLight ? 'text-slate-800' : 'text-slate-200'}`} title={req.model}>
                              {req.model}
                            </span>
                          </div>
                          <div className="flex items-center gap-2 shrink-0 text-[10px]">
                            <span className={isLight ? 'text-slate-500' : 'text-slate-400'}>{latencyText}</span>
                            <span className={`font-medium ${isLight ? 'text-emerald-600' : 'text-emerald-400'}`}>{speedText}</span>
                          </div>
                        </div>
                      );
                    })}
                  </div>
                </div>
              )}
            </div>
          )}
        </div>
      )}

      {/* Mascot Artwork (CompanionStage - 始终稳定锚定在底部) */}
      <div className="relative w-[240px] max-w-full">
        <CompanionStage
          scene={scene}
          theme={theme}
          paused={paused}
          onActivate={handleMascotClick}
          className="mx-auto"
        />

        {/* Quick Dock Trigger (Visible on Hover / Focus) */}
        <button
          type="button"
          aria-label="打开控制坞"
          title="展开快捷控制坞"
          onClick={() => setDockOpen(open => !open)}
          className={`pet-no-drag absolute top-1 right-1 p-1.5 rounded-full pet-glass-pill border transition-opacity duration-200 ${
            isLight
              ? 'bg-white/80 hover:bg-white text-slate-700 hover:text-slate-900 border-slate-300/80 shadow-xs'
              : 'bg-slate-900/60 hover:bg-slate-800 text-slate-300 hover:text-white border-white/10'
          } ${
            isHovered || dockOpen ? 'opacity-100' : 'opacity-0 pointer-events-none'
          }`}
          data-wails-draggable="no-drag"
          style={wailsNoDragStyle}
        >
          <SlidersHorizontal size={13} />
        </button>
      </div>
    </div>
  );
});

export default FloatingPet;
