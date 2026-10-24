import { useEffect, useRef, useState } from 'react';
import { api, errorMessage, type CodexQuotaResponse } from '../api';

export type QuotaTarget = { channelID: number; credentialID: string; identity?: string };
export type QuotaObservation = { snapshot?: CodexQuotaResponse; pending?: boolean; error?: string };

// Identity changes (including reauthorization) must never reuse a previous snapshot.
export function quotaTargetKey(target: QuotaTarget): string {
  return JSON.stringify([target.channelID, target.credentialID, target.identity || '']);
}

export function useCodexQuota(targets: QuotaTarget[], active: boolean, networkEnabled = true) {
  const [observations, setObservations] = useState<Record<string, QuotaObservation>>({});
  const manual = useRef<(target: QuotaTarget) => void>(() => {});
  const networkAllowed = useRef(networkEnabled); networkAllowed.current = networkEnabled;
  const serialized = JSON.stringify(targets.map(target => [target.channelID, target.credentialID, target.identity || '']));
  useEffect(() => {
    if (!active) { manual.current = () => {}; return; }
    const accounts: QuotaTarget[] = (JSON.parse(serialized) as [number, string, string][]).map(([channelID, credentialID, identity]) => ({ channelID, credentialID, identity }));
    const keys = new Set(accounts.map(quotaTargetKey));
    setObservations(current => Object.fromEntries(Object.entries(current).filter(([key]) => keys.has(key))));
    let stopped = false;
    const pending = new Set<string>();
    const reread = new Set<string>();
    const timers = new Map<string, ReturnType<typeof setTimeout>>();
    const clearTimer = (key: string) => { clearTimeout(timers.get(key)); timers.delete(key); };
    function apply(key: string, result: CodexQuotaResponse, cacheRead = false) {
      if (stopped) return;
      const emptyCache = cacheRead && !result.ok && !result.detail;
      setObservations(current => ({ ...current, [key]: {
        snapshot: result.ok ? result : emptyCache ? undefined : current[key]?.snapshot,
        pending: false,
        error: emptyCache ? undefined : result.detail || (!result.ok ? '未能获取额度，请重试。' : undefined),
      } }));
    }
    function schedule(target: QuotaTarget, result: CodexQuotaResponse) {
      const key = quotaTargetKey(target);
      clearTimer(key);
      if (stopped || !networkAllowed.current || document.visibilityState === 'hidden' || !result.refresh_seconds || !result.next_refresh_at) return;
      const delay = new Date(result.next_refresh_at).getTime() - Date.now();
      if (Number.isFinite(delay)) timers.set(key, setTimeout(() => void read(target), Math.max(1000, delay)));
    }
    async function read(target: QuotaTarget, force = false) {
      const key = quotaTargetKey(target);
      if (stopped || force && !networkAllowed.current || pending.has(key) || !force && document.visibilityState === 'hidden') return;
      clearTimer(key); pending.add(key);
      setObservations(current => ({ ...current, [key]: { ...current[key], pending: true } }));
      try {
        const query = new URLSearchParams({ channel_id: String(target.channelID), credential_id: target.credentialID });
        const path = `/oauth/codex/usage?${query}`;
        let result = await api<CodexQuotaResponse>(force ? `${path}&force=1` : path, force ? { method: 'POST', body: '{}' } : undefined);
        apply(key, result, !force);
        if (!stopped && networkAllowed.current && !force && result.refresh_due && document.visibilityState !== 'hidden') {
          setObservations(current => ({ ...current, [key]: { ...current[key], pending: true } }));
          result = await api<CodexQuotaResponse>(path, { method: 'POST', body: '{}' });
          apply(key, result);
        }
        schedule(target, result);
      } catch (error) {
        if (!stopped) {
          setObservations(current => ({ ...current, [key]: { ...current[key], pending: false, error: errorMessage(error) } }));
          if (networkAllowed.current && document.visibilityState !== 'hidden') timers.set(key, setTimeout(() => void read(target), 60_000));
        }
      } finally {
        pending.delete(key);
        // A settings change during an upstream request must be observed after
        // that request settles, including switching automatic refresh to zero.
        if (!stopped && reread.delete(key) && document.visibilityState !== 'hidden') void read(target);
      }
    }
    const visible = () => {
      for (const key of timers.keys()) clearTimer(key);
      if (document.visibilityState !== 'hidden') for (const target of accounts) {
        if (pending.has(quotaTargetKey(target))) reread.add(quotaTargetKey(target));
        else void read(target);
      }
    };
    manual.current = target => { if (accounts.some(account => quotaTargetKey(account) === quotaTargetKey(target))) void read(target, true); };
    document.addEventListener('visibilitychange', visible);
    window.addEventListener('quota-settings-changed', visible);
    visible();
    return () => {
      stopped = true; manual.current = () => {};
      for (const key of timers.keys()) clearTimer(key);
      document.removeEventListener('visibilitychange', visible);
      window.removeEventListener('quota-settings-changed', visible);
    };
  }, [serialized, active, networkEnabled]);
  return { observations, refreshQuota: (target: QuotaTarget) => manual.current(target) };
}
