import { useEffect, useRef, useState } from 'react';
import { api, ApiError, errorMessage, type Channel, type CredentialItem } from '../api';
import { quotaTargetKey, type QuotaObservation, type QuotaTarget } from './useCodexQuota';
import type { CredentialOperationsPatch, CredentialOperationsReport } from '../types/credential-operations';

export type OAuthBatchTarget = { credential: CredentialItem; name: string; target: QuotaTarget; generation?: number };
export type OAuthBatchResult = { key: string; name: string; state: 'pending' | 'succeeded' | 'failed' | 'skipped' | 'unknown'; detail: string };
export function oauthCredentialIdentity(credential: CredentialItem) {
  const oauth = credential.oauth_info || credential.oauth;
  return JSON.stringify([credential.has_oauth, oauth?.provider, oauth?.account_label, oauth?.updated_at, oauth?.expires_at]);
}
const operationBase = (channelID: number, credentialID: string) => `/channels/${channelID}/credentials/${encodeURIComponent(credentialID)}/operations`;
const pendingKey = (base: string) => `to-router.credential-operation:${base}`;

export function useOAuthCredentialBatch(channel: Channel, observations: Record<string, QuotaObservation>, refresh: (target: QuotaTarget) => void, onChanged: () => void, onGeneration?: (generation: number) => void) {
  const [results, setResults] = useState<OAuthBatchResult[]>([]);
  const [busy, setBusy] = useState(false);
  const [timeZone, setTimeZone] = useState('');
  const resultsRef = useRef<OAuthBatchResult[]>([]);
  const pending = useRef(false), live = useRef(true), current = useRef(channel);
  const refreshActive = useRef(false), unresolvedMemory = useRef(new Map<string, string>());
  const refreshes = useRef(new Map<string, { item: OAuthBatchTarget; before?: QuotaObservation; seen: boolean }>());
  current.current = channel;
  useEffect(() => { live.current = true; return () => { live.current = false; }; }, []);
  const key = (item: OAuthBatchTarget) => quotaTargetKey(item.target);
  function valid(item: OAuthBatchTarget) {
    const credential = current.current.credentials?.find(value => value.id === item.credential.id);
    return current.current.id === item.target.channelID && current.current.generation === item.generation && !!credential && oauthCredentialIdentity(credential) === item.target.identity;
  }
  function result(item: OAuthBatchTarget, state: OAuthBatchResult['state'], detail: string) {
    if (live.current) {
      const next = { key: key(item), name: item.name, state, detail };
      const index = resultsRef.current.findIndex(value => value.key === next.key);
      resultsRef.current = index < 0 ? [...resultsRef.current, next] : resultsRef.current.map((value, position) => position === index ? next : value);
      setResults(resultsRef.current);
    }
  }
  function start(items: OAuthBatchTarget[], retry = false) {
    if (pending.current || !items.length) return false;
    pending.current = true; setBusy(true);
    if (!retry) { resultsRef.current = items.map(item => ({ key: key(item), name: item.name, state: 'pending', detail: '等待执行' })); setResults(resultsRef.current); }
    return true;
  }
  function finish() { pending.current = false; if (live.current) setBusy(false); }
  useEffect(() => {
    for (const [targetKey, entry] of refreshes.current) {
      if (!valid(entry.item)) {
        result(entry.item, 'skipped', '渠道或凭证已变化，请清除选择后重新选择。');
        refreshes.current.delete(targetKey); continue;
      }
      const observation = observations[targetKey];
      if (observation?.pending) { entry.seen = true; continue; }
      if (!entry.seen && observation === entry.before) continue;
      if (!observation) continue;
      result(entry.item, observation.error ? 'failed' : 'succeeded', observation.error ? `刷新失败，保留旧额度：${observation.error}` : '额度已刷新');
      refreshes.current.delete(targetKey);
    }
    if (refreshActive.current && !refreshes.current.size) { refreshActive.current = false; finish(); }
  }, [observations, channel]);
  async function run(items: OAuthBatchTarget[], action: 'enable' | 'disable' | 'warmup' | 'config', options?: { patch?: CredentialOperationsPatch; skip?: (item: OAuthBatchTarget) => string | undefined; retry?: boolean }) {
    if (!start(items, options?.retry)) return;
    // Preflight every target before any mutation, so our own enablement changes do not stale later reads.
    const reports = new Map<string, CredentialOperationsReport>();
    const enablement = action === 'enable' || action === 'disable';
    let expectedGeneration = items[0]?.generation, conflict = false;
    let savedGeneration: number | undefined;
    try {
      if (!enablement) await Promise.all(items.map(async item => {
        try {
          if (!valid(item)) throw new Error('渠道或凭证已变化，请清除选择后重新选择。');
          const report = await api<CredentialOperationsReport>(operationBase(channel.id, item.credential.id));
          if (!valid(item) || item.generation !== undefined && report.channel_generation !== item.generation) throw new Error('渠道或凭证已变化，请清除选择后重新选择。');
          if (!report.supported) throw new Error('此凭证暂不支持该操作。');
          reports.set(key(item), report);
        } catch (cause) { result(item, 'failed', errorMessage(cause)); }
      }));
      for (const item of items) {
        if (!live.current) break;
        const report = reports.get(key(item)); if (!enablement && !report) continue;
        if (conflict) { result(item, 'skipped', '渠道配置已冲突，停止后续写入；请刷新并重新选择。'); continue; }
        if (!valid(item)) { result(item, 'skipped', '渠道或凭证已变化，请清除选择后重新选择。'); continue; }
        const skip = options?.skip?.(item); if (skip) { result(item, 'skipped', skip); continue; }
        const base = operationBase(channel.id, item.credential.id);
        try {
          result(item, 'pending', '正在执行…');
          if (action === 'enable' || action === 'disable') {
            if ((item.credential.enabled !== false) === (action === 'enable')) { result(item, 'skipped', action === 'enable' ? '已启用' : '已停用'); continue; }
            const response = await api<{ ok: boolean; generation: number }>(`/channels/${channel.id}/credentials/${encodeURIComponent(item.credential.id)}/enabled`, { method: 'PUT', body: JSON.stringify({ enabled: action === 'enable', expected_generation: expectedGeneration }) });
            if (!response.ok) throw new Error('未能保存凭证启停设置。');
            expectedGeneration = response.generation; savedGeneration = response.generation;
            result(item, 'succeeded', action === 'enable' ? '已启用' : '已停用');
          } else if (action === 'config') {
            if (!options?.patch) throw new Error('自动配置尚未填写。');
            const response = await api<CredentialOperationsReport>(base, { method: 'PUT', body: JSON.stringify({ expected_identity: report!.identity, ...options.patch }) });
            if (response.identity !== report!.identity) throw new Error('凭证身份已变化，请刷新渠道。');
            result(item, 'succeeded', '配置已保存');
          } else {
            const oauth = item.credential.oauth_info || item.credential.oauth;
            if (!channel.enabled || item.credential.enabled === false) { result(item, 'skipped', '渠道或凭证已停用'); continue; }
            if (item.credential.has_oauth === false || oauth?.status === 'reauth_required' || oauth?.status === 'unsubscribed') { result(item, 'skipped', '凭证需要有效授权'); continue; }
            if (!report!.config.warmup.model || !channel.models.includes(report!.config.warmup.model)) { result(item, 'skipped', '未配置有效预热模型，请先到单条预热入口配置。'); continue; }
            if (report!.config.warmup.model !== item.credential.operations?.warmup.model) { result(item, 'skipped', '预热模型已变化，请刷新渠道并重新确认。'); continue; }
            let remembered: { identity?: string; request_id?: string } | null = null;
            try { remembered = JSON.parse(sessionStorage.getItem(pendingKey(base)) || 'null'); } catch { /* Durable backend receipts still protect unresolved actions. */ }
            const receipt = report!.last_warmup;
            if (receipt && receipt.request_id === unresolvedMemory.current.get(`${base}:${report!.identity}`) && ['succeeded', 'failed', 'acknowledged'].includes(receipt.state)) unresolvedMemory.current.delete(`${base}:${report!.identity}`);
            if (remembered?.identity === report!.identity && receipt && receipt.request_id === remembered.request_id && ['succeeded', 'failed', 'acknowledged'].includes(receipt.state)) { sessionStorage.removeItem(pendingKey(base)); remembered = null; }
            if (unresolvedMemory.current.has(`${base}:${report!.identity}`) || remembered?.identity === report!.identity || [report!.last_warmup, report!.last_reset].some(value => value?.state === 'unknown' || value?.state === 'running')) { result(item, 'unknown', '已有操作结果未确认，请到单条入口读取并人工核对。'); continue; }
            const requestID = crypto.randomUUID();
            unresolvedMemory.current.set(`${base}:${report!.identity}`, requestID);
            try { sessionStorage.setItem(pendingKey(base), JSON.stringify({ identity: report!.identity, kind: 'warmup', request_id: requestID })); } catch { /* Receipt remains durable on the server. */ }
            function clearCurrentClaim() {
              if (unresolvedMemory.current.get(`${base}:${report!.identity}`) === requestID) unresolvedMemory.current.delete(`${base}:${report!.identity}`);
              try {
                const stored = JSON.parse(sessionStorage.getItem(pendingKey(base)) || 'null');
                if (stored?.identity === report!.identity && stored.request_id === requestID) sessionStorage.removeItem(pendingKey(base));
              } catch { /* Never erase another operation's persisted claim. */ }
            }
            let response: CredentialOperationsReport | undefined;
            try { response = await api<CredentialOperationsReport>(`${base}/warmup`, { method: 'POST', body: JSON.stringify({ expected_identity: report!.identity, request_id: requestID }) }); }
            catch (cause) {
              if (cause instanceof ApiError && (cause.status === 400 || cause.status === 404)) {
                clearCurrentClaim();
                if (cause.status === 404) conflict = true;
                throw cause;
              }
              // A 409 can follow a persisted claim, so reconcile this exact UUID before classifying it.
              if (cause instanceof ApiError && cause.status === 409) conflict = true;
              try { response = await api<CredentialOperationsReport>(base); } catch { /* Keep unresolved UUID and never create a replacement. */ }
              if (cause instanceof ApiError && cause.status === 409 && response && (response.identity !== report!.identity || response.last_warmup?.request_id !== requestID)) {
                clearCurrentClaim(); throw cause;
              }
            }
            const outcome = response?.identity === report!.identity && response.last_warmup?.request_id === requestID ? response.last_warmup : undefined;
            if (outcome && ['succeeded', 'failed', 'acknowledged'].includes(outcome.state)) {
              clearCurrentClaim();
              result(item, outcome.state === 'succeeded' ? 'succeeded' : 'failed', outcome.detail || (outcome.state === 'succeeded' ? '已预热' : '预热未完成'));
            } else result(item, 'unknown', '结果未确认；请到单条预热入口读取并人工核对，勿重复发送。');
          }
        } catch (cause) { if (cause instanceof ApiError && cause.status === 409) conflict = true; result(item, 'failed', `操作未完成：${errorMessage(cause)}`); }
      }
      if (live.current) { if (savedGeneration !== undefined) onGeneration?.(savedGeneration); onChanged(); }
      return resultsRef.current;
    } finally { finish(); }
  }
  async function inspectTimeZone(items: OAuthBatchTarget[]) {
    setTimeZone('');
    const item = items[0]; if (!item || !valid(item)) return;
    try { const report = await api<CredentialOperationsReport>(operationBase(channel.id, item.credential.id)); if (live.current && valid(item) && (item.generation === undefined || report.channel_generation === item.generation)) setTimeZone(report.time_zone || ''); } catch { /* Explicit unknown remains visible; no schedule timezone is inferred. */ }
  }
  function refreshItems(items: OAuthBatchTarget[]) {
    if (!start(items)) return;
    refreshActive.current = true;
    for (const item of items) {
      if (!valid(item)) { result(item, 'skipped', '渠道或凭证已变化，请重新选择。'); continue; }
      const targetKey = key(item), before = observations[targetKey];
      if (before?.pending) { result(item, 'skipped', '额度正在刷新，请等待完成。'); continue; }
      refreshes.current.set(targetKey, { item, before, seen: false }); refresh(item.target);
    }
    if (!refreshes.current.size) { refreshActive.current = false; finish(); }
  }
  return { results, busy, timeZone, inspectTimeZone, run, refreshItems, clearResults: () => { resultsRef.current = []; setResults([]); } };
}
