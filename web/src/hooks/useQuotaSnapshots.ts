import { useEffect, useRef, useState } from 'react';
import { errorMessage } from '../api';
import { quotaKey, readQuotas, refreshQuota } from '../services/quotas';
import type { QuotaList, QuotaSnapshot, QuotaTarget } from '../types/quota';

export function useQuotaSnapshots(targets: QuotaTarget[], active = true) {
  const [data, setData] = useState<QuotaList>({ items: [], refresh_seconds: 0 });
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [pending, setPending] = useState<Set<string>>(new Set());
  const manual = useRef<(target: QuotaTarget) => void>(() => {});
  const serialized = JSON.stringify(targets.map(target => [target.channel_id, target.credential_id]));
  useEffect(() => {
    if (!active) return;
    const selected = new Set((JSON.parse(serialized) as [number, string][]).map(([channel_id, credential_id]) => quotaKey({ channel_id, credential_id })));
    let disposed = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let reading = false;
    let current: QuotaList = { items: [], refresh_seconds: 0 };
    const busy = new Set<string>();
    const retry = new Map<string, number>();
    const failures = new Map<string, string>();
    const versions = new Map<string, number>();
    let mutation = 0;
    const visible = () => !document.hidden && document.visibilityState !== 'hidden';
    function apply(item: QuotaSnapshot) {
      versions.set(quotaKey(item), ++mutation);
      current = { ...current, items: current.items.map(row => quotaKey(row) === quotaKey(item) ? item : row) };
      if (!disposed) setData(current);
    }
    async function update(target: QuotaTarget, force: boolean) {
      const key = quotaKey(target);
      if (disposed || !visible() || busy.has(key) || !selected.has(key)) return;
      const item = current.items.find(row => quotaKey(row) === key);
      if (!item?.enabled) return;
      busy.add(key); setPending(new Set(busy));
      try {
        const next = await refreshQuota(target, force, () => !disposed && visible() && selected.has(key) && !!current.items.find(row => quotaKey(row) === key)?.enabled);
        if (!disposed && next) {
          apply(next);
          // A rejected/unfinished observation may have no backend deadline.
          if (next.error) retry.set(key, Date.now() + 60_000);
          else { retry.delete(key); failures.delete(key); }
          setError('');
        }
      }
      catch (e) {
        if (!disposed) {
          const message = errorMessage(e);
          retry.set(key, Date.now() + 60_000); failures.set(key, message);
          const previous = current.items.find(row => quotaKey(row) === key);
          if (previous) apply({ ...previous, error: message, stale: true });
          setError(message);
        }
      }
      finally { busy.delete(key); if (!disposed) { setPending(new Set(busy)); schedule(); } }
    }
    function due(item: QuotaSnapshot) {
      const next = Date.parse(item.next_refresh_at || '');
      return Math.max(retry.get(quotaKey(item)) || 0, Number.isFinite(next) ? next : item.checked_at ? Date.parse(item.checked_at) + current.refresh_seconds * 1000 : 0);
    }
    function schedule() {
      clearTimeout(timer);
      if (disposed || !visible()) return;
      const next = current.refresh_seconds > 0 ? current.items.filter(item => selected.has(quotaKey(item)) && item.enabled && !busy.has(quotaKey(item))).map(due) : [];
      timer = setTimeout(() => void poll(), Math.max(1000, Math.min(15_000, ...next.map(at => at - Date.now()))));
    }
    async function poll() {
      if (disposed || !visible() || reading) return;
      reading = true;
      const readVersion = mutation;
      try {
        const next = await readQuotas();
        if (disposed || !visible()) return;
        current = { items: (Array.isArray(next.items) ? next.items : []).map(item => {
          const key = quotaKey(item);
          // An older cache read must not rewind an acknowledged refresh.
          if ((versions.get(key) || 0) > readVersion) return current.items.find(row => quotaKey(row) === key) || item;
          return failures.has(key) ? { ...item, error: failures.get(key), stale: true } : item;
        }), refresh_seconds: next.refresh_seconds || 0 };
        setData(current); setLoaded(true); setError('');
        if (current.refresh_seconds > 0) for (const item of current.items) {
          if (selected.has(quotaKey(item)) && item.enabled && due(item) <= Date.now()) void update(item, false);
        }
      } catch (e) { if (!disposed) setError(errorMessage(e)); }
      finally { reading = false; schedule(); }
    }
    const onVisible = () => { clearTimeout(timer); if (visible()) void poll(); };
    manual.current = target => void update(target, true);
    document.addEventListener('visibilitychange', onVisible);
    window.addEventListener('quota-settings-changed', onVisible);
    void poll();
    return () => { disposed = true; manual.current = () => {}; clearTimeout(timer); document.removeEventListener('visibilitychange', onVisible); window.removeEventListener('quota-settings-changed', onVisible); };
  }, [serialized, active]);
  return { ...data, loaded, error, pending, refresh: (target: QuotaTarget) => manual.current(target) };
}
