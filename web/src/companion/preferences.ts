import { useCallback, useEffect, useRef, useState } from 'react';
import { api, errorMessage } from '../api';
import type { QuotaTarget } from '../types/quota';

export interface CompanionPreferences { theme: 'dark' | 'light'; paused: boolean; revision: number; quota_targets: QuotaTarget[] }
type Patch = Partial<Pick<CompanionPreferences, 'theme' | 'paused' | 'quota_targets'>>;
const defaults: CompanionPreferences = { theme: 'dark', paused: false, revision: -1, quota_targets: [] };

export function useCompanionPreferences(enabled: boolean) {
  const [value, setValue] = useState(defaults);
  const [readError, setReadError] = useState('');
  const [writeError, setWriteError] = useState('');
  const revision = useRef(-1);
  const confirmed = useRef(defaults);
  const generation = useRef(0);
  const pending = useRef(0);
  const queue = useRef<Promise<void>>(Promise.resolve());
  const apply = useCallback((next: CompanionPreferences, optimistic = false) => {
    next = { ...next, quota_targets: next.quota_targets || [] };
    if (next.theme !== 'dark' && next.theme !== 'light' || typeof next.paused !== 'boolean' || !Number.isFinite(next.revision)) throw new Error('伴侣偏好响应无效');
    if (next.revision < revision.current) return;
    revision.current = next.revision;
    confirmed.current = next;
    setValue(current => optimistic ? { ...current, revision: next.revision } :
      current.theme === next.theme && current.paused === next.paused && current.revision === next.revision && JSON.stringify(current.quota_targets) === JSON.stringify(next.quota_targets) ? current : next);
  }, []);

  useEffect(() => {
    const mine = ++generation.current;
    if (!enabled) return;
    let disposed = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let controller: AbortController | null = null;
    const poll = async () => {
      if (disposed || document.hidden || controller) return;
      const request = new AbortController(); controller = request;
      const timeout = setTimeout(() => request.abort(), 8000);
      try {
        const next = await api<CompanionPreferences>('/companion/preferences', { signal: request.signal });
        if (!disposed && mine === generation.current && !request.signal.aborted && pending.current === 0) { apply(next); setReadError(''); }
      } catch (e) {
        if (!disposed && mine === generation.current && !request.signal.aborted) setReadError(errorMessage(e));
      } finally {
        clearTimeout(timeout); controller = null;
        if (!disposed && !document.hidden) timer = setTimeout(() => void poll(), 1000);
      }
    };
    const visibility = () => { clearTimeout(timer); if (!document.hidden) void poll(); };
    document.addEventListener('visibilitychange', visibility);
    void poll();
    return () => { disposed = true; clearTimeout(timer); controller?.abort(); document.removeEventListener('visibilitychange', visibility); };
  }, [enabled, apply]);

  const update = useCallback((patch: Patch) => {
    const mine = generation.current;
    pending.current++;
    setValue(current => ({ ...current, ...patch }));
    const operation = queue.current.then(async () => {
      const request = new AbortController();
      const timeout = setTimeout(() => request.abort(), 8000);
      try {
        if (mine !== generation.current) return false;
        const next = await api<CompanionPreferences>('/companion/preferences', { method: 'PUT', body: JSON.stringify(patch), signal: request.signal });
        if (mine === generation.current) { apply(next, pending.current > 1); setWriteError(''); return true; }
      } catch (e) {
        if (mine === generation.current) {
          setWriteError(errorMessage(e));
          try { const next = await api<CompanionPreferences>('/companion/preferences', {signal:request.signal}); if (mine === generation.current) apply(next, pending.current > 1); }
          catch { if (mine === generation.current) apply(confirmed.current, pending.current > 1); }
        }
      } finally { clearTimeout(timeout); pending.current--; }
      return false;
    });
    queue.current = operation.then(() => {}, () => {});
    return operation;
  }, [apply]);
  return { value, error: writeError || readError, update };
}
