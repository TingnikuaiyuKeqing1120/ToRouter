import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../api';
import type { CompanionScene } from './CompanionStage';

export interface LiveRequest {
  id: string;
  model: string;
  channel: string;
  channel_id: number;
  started_at: string;
  attempt: number;
  phase: 'waiting' | 'streaming' | 'retrying';
  output_tokens: number | null;
  output_tokens_source?: '' | 'reported' | 'estimated';
  tokens_per_second: number | null;
  first_byte_ms: number | null;
}
export interface FinishedRequest extends LiveRequest {
  sequence: number;
  finished_at: string;
  outcome: 'success' | 'error' | 'cancelled' | 'accepted';
  status: number;
  error_code: string;
}
export interface CompanionSnapshot {
  server_time: string;
  session_id: string;
  sequence: number;
  active_count: number;
  active: LiveRequest[];
  recent: FinishedRequest[];
  today: {
    records: number;
    input_tokens: number;
    output_tokens: number;
    cache_read_tokens: number;
    cache_write_tokens: number;
    estimated_records: number;
  };
}
export type ConnectionState = 'loading' | 'connected' | 'error';
type Notice = { scene: 'done' | 'error'; request: FinishedRequest; until: number };

// Translate only fixed codes; never echo upstream bodies or diagnostic text.
export function failureLabel(code: string): string {
  const labels: Record<string, string> = {
    upstream_error: '上游返回错误',
    request_rejected: '本次请求被拒绝',
    no_usable_endpoint: '暂无可用渠道',
    all_channels_cooling: '可用渠道正在冷却',
    stream_interrupted: '输出流中断',
    response_incomplete: '输出未完成',
    configuration_error: '渠道配置需要检查',
    client_cancelled: '请求已取消',
  };
  return labels[code] || '本次请求未完成';
}

export function selectScene(snapshot: CompanionSnapshot | null, connection: ConnectionState, notice: Notice | null): CompanionScene {
  if (connection === 'error') return 'error';
  if (connection !== 'connected' || !snapshot) return 'idle';
  if (snapshot.active_count > 0) {
    if (snapshot.active.some(request => request.phase === 'retrying')) return 'retry';
    return snapshot.active_count > 1 ? 'surf' : 'work';
  }
  return notice?.scene ?? 'idle';
}

export function useCompanionLive() {
  const [snapshot, setSnapshot] = useState<CompanionSnapshot | null>(null);
  const [connection, setConnection] = useState<ConnectionState>('loading');
  const [notice, setNotice] = useState<Notice | null>(null);
  const baseline = useRef<{ session: string; sequence: number } | null>(null);
  const pollNow = useRef<() => void>(() => {});
  const refresh = useCallback(() => pollNow.current(), []);

  useEffect(() => {
    let disposed = false;
    let generation = 0;
    let controller: AbortController | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let deadline: ReturnType<typeof setTimeout> | undefined;
    const invalidate = () => {
      generation++;
      clearTimeout(timer);
      clearTimeout(deadline);
      controller?.abort();
      controller = null;
    };
    const poll = async () => {
      if (disposed || document.hidden || controller) return;
      clearTimeout(timer);
      const mine = generation;
      const request = new AbortController();
      controller = request;
      deadline = setTimeout(() => {
        if (disposed || mine !== generation) return;
        // End this generation even if a fetch adapter settles late after abort.
        // Hiding/unmounting clears this deadline without reporting a failure.
        invalidate();
        setConnection('error');
        setNotice(null);
        timer = setTimeout(() => void poll(), 1000);
      }, 8000);
      try {
        const next = await api<CompanionSnapshot>('/companion', { signal: request.signal });
        if (disposed || request.signal.aborted || mine !== generation) return;
        const previous = baseline.current;
        if (previous && next.session_id === previous.session && next.sequence < previous.sequence) return;
        setSnapshot(next);
        setConnection('connected');
        if (!previous || previous.session !== next.session_id) {
          // First load and server restart establish a history baseline.
          setNotice(null);
        } else if (next.active_count > 0) {
          setNotice(null);
        } else {
          const latest = next.recent.filter(item => item.sequence > previous.sequence)
            .sort((a, b) => b.sequence - a.sequence)[0];
          if (latest) {
            const lifetime = latest.outcome === 'success' ? 2800 : 5000;
            const age = Math.max(0, Date.parse(next.server_time) - Date.parse(latest.finished_at));
            if ((latest.outcome === 'success' || latest.outcome === 'error') && Number.isFinite(age) && age < lifetime) {
              setNotice({ scene: latest.outcome === 'success' ? 'done' : 'error', request: latest, until: Date.now() + lifetime - age });
            } else setNotice(null);
          }
        }
        baseline.current = { session: next.session_id, sequence: next.sequence };
      } catch {
        if (!disposed && !request.signal.aborted && mine === generation) {
          setConnection('error');
          setNotice(null);
        }
      } finally {
        if (!disposed && mine === generation) {
          clearTimeout(deadline);
          controller = null;
          if (!document.hidden) timer = setTimeout(() => void poll(), 1000);
        }
      }
    };
    const visibility = () => {
      invalidate();
      if (!document.hidden) void poll();
    };
    pollNow.current = () => void poll();
    document.addEventListener('visibilitychange', visibility);
    void poll();
    return () => {
      disposed = true;
      invalidate();
      pollNow.current = () => {};
      document.removeEventListener('visibilitychange', visibility);
    };
  }, []);

  useEffect(() => {
    if (!notice) return;
    const timer = setTimeout(() => setNotice(null), Math.max(0, notice.until - Date.now()));
    return () => clearTimeout(timer);
  }, [notice]);

  return { snapshot, connection, notice, refresh, scene: selectScene(snapshot, connection, notice) };
}
