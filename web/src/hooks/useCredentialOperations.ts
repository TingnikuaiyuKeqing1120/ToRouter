import { useEffect, useRef, useState } from 'react';
import { api, ApiError, errorMessage } from '../api';
import type { CredentialOperationKind, CredentialOperationsPatch, CredentialOperationsReport } from '../types/credential-operations';

export type PendingCredentialOperation = { identity: string; kind: CredentialOperationKind; request_id: string };
const storageKey = (base: string) => `to-router.credential-operation:${base}`;
function remembered(base: string): PendingCredentialOperation | null {
  try {
    const value = JSON.parse(sessionStorage.getItem(storageKey(base)) || 'null');
    return value && typeof value.identity === 'string' && (value.kind === 'reset' || value.kind === 'warmup') && typeof value.request_id === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value.request_id) ? value : null;
  } catch { return null; }
}

// All POST retries keep their original UUID. A lost response is never a new action.
export function useCredentialOperations(channelID: number, credentialID: string, expectedGeneration: number | undefined, refreshCardsOnOpen: boolean, networkEnabled = true) {
  const base = `/channels/${channelID}/credentials/${encodeURIComponent(credentialID)}/operations`;
  const [report, setReport] = useState<CredentialOperationsReport>();
  const [error, setError] = useState('');
  const [pending, setPending] = useState('load');
  const [unresolved, setUnresolved] = useState<PendingCredentialOperation | null>(() => remembered(base));
  const unresolvedRef = useRef(unresolved), reportRef = useRef(report), inFlight = useRef(false), live = useRef(true), identity = useRef('');
  const [identityChanged, setIdentityChanged] = useState(false);
  const networkAllowed = useRef(networkEnabled); networkAllowed.current = networkEnabled;
  function remember(value: PendingCredentialOperation | null) {
    unresolvedRef.current = value; setUnresolved(value);
    try { if (value) sessionStorage.setItem(storageKey(base), JSON.stringify(value)); else sessionStorage.removeItem(storageKey(base)); } catch { /* In-memory protection remains active when storage is unavailable. */ }
  }
  function apply(next: CredentialOperationsReport) {
    if (!live.current) return;
    if (expectedGeneration !== undefined && next.channel_generation !== expectedGeneration) {
      setIdentityChanged(true); setError('渠道或凭证已变化，请关闭并刷新渠道。'); return;
    }
    if (identity.current && next.identity !== identity.current) {
      setIdentityChanged(true); setError('凭证身份已变化，请关闭并刷新渠道。'); return;
    }
    identity.current = next.identity; reportRef.current = next; setReport(next);
    const previous = unresolvedRef.current;
    if (previous && previous.identity !== next.identity) { remember(null); return; }
    const receipt = previous && (previous.kind === 'reset' ? next.last_reset : next.last_warmup);
    if (previous && receipt?.request_id === previous.request_id && ['succeeded', 'failed', 'acknowledged'].includes(receipt.state)) remember(null);
  }
  useEffect(() => {
    live.current = true;
    const abort = new AbortController();
    inFlight.current = true; setPending('load');
    void (async () => {
      try {
        const local = await api<CredentialOperationsReport>(base, { signal: abort.signal });
        if (abort.signal.aborted) return;
        apply(local);
        if (networkAllowed.current && refreshCardsOnOpen && local.supported && (expectedGeneration === undefined || local.channel_generation === expectedGeneration)) {
          setPending('cards');
          const fresh = await api<CredentialOperationsReport>(`${base}/refresh-reset-cards`, { method: 'POST', body: JSON.stringify({ expected_identity: local.identity }), signal: abort.signal });
          if (!abort.signal.aborted) apply(fresh);
        }
      } catch (cause) { if (!abort.signal.aborted) setError(`读取失败：${errorMessage(cause)}`); }
      finally { if (!abort.signal.aborted) { inFlight.current = false; setPending(''); } }
    })();
    return () => { live.current = false; abort.abort(); };
    // The dialog is mounted for one exact target and one initial entry point.
  }, [base]);

  async function request(label: string, path: string, method: string, body?: unknown) {
    if (!networkAllowed.current && ['/refresh-reset-cards', '/warmup', '/consume-reset'].includes(path)) return;
    if (inFlight.current || identityChanged || !reportRef.current?.supported) return;
    inFlight.current = true; setPending(label); setError('');
    try {
      const next = await api<CredentialOperationsReport>(`${base}${path}`, { method, ...(body === undefined ? {} : { body: JSON.stringify(body) }) });
      if (!live.current) return;
      apply(next);
      if (expectedGeneration !== undefined && next.channel_generation !== expectedGeneration || next.identity !== identity.current) return;
      return next;
    } catch (cause) {
      if (live.current) {
        // Validation and missing-target refusals do not create an action. A
        // conflict can also occur after sending, so keep its UUID for lookup.
        if ((path === '/warmup' || path === '/consume-reset') && cause instanceof ApiError && [400, 404].includes(cause.status)) remember(null);
        setError(`操作未完成：${errorMessage(cause)}`);
      }
    }
    finally { inFlight.current = false; if (live.current) setPending(''); }
  }
  async function reload() {
    if (inFlight.current) return;
    inFlight.current = true; setPending('load'); setError('');
    try { apply(await api<CredentialOperationsReport>(base)); }
    catch (cause) { if (live.current) setError(`读取失败，保留上次信息：${errorMessage(cause)}`); }
    finally { inFlight.current = false; if (live.current) setPending(''); }
  }
  async function execute(kind: CredentialOperationKind, retry = false) {
    if (!networkAllowed.current || !live.current || inFlight.current || identityChanged || !reportRef.current?.supported) return;
    const current = reportRef.current;
    const next = retry ? unresolvedRef.current : { identity: current.identity, kind, request_id: crypto.randomUUID() };
    if (!next || next.kind !== kind || next.identity !== current.identity || !retry && unresolvedRef.current) return;
    remember(next);
    const response = await request(kind, kind === 'reset' ? '/consume-reset' : '/warmup', 'POST', { expected_identity: next.identity, request_id: next.request_id });
    // Ambiguous failures remain unresolved until a matching durable receipt is read.
    if (!response && live.current && unresolvedRef.current) await reload();
    return response;
  }
  return {
    report, error, pending, unresolved: unresolved?.identity === report?.identity ? unresolved : null, identityChanged,
    reload,
    refreshCards: () => request('cards', '/refresh-reset-cards', 'POST', { expected_identity: reportRef.current?.identity }),
    save: (patch: CredentialOperationsPatch) => request('save', '', 'PUT', { expected_identity: reportRef.current?.identity, ...patch }),
    execute,
    acknowledge: (kind: CredentialOperationKind) => request('acknowledge', '/acknowledge', 'POST', { expected_identity: reportRef.current?.identity, kind }),
  };
}
