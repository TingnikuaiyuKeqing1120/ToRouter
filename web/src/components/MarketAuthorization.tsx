import { useEffect, useRef, useState } from 'react';
import { api, ApiError, errorMessage, type Settings } from '../api';
import type { MarketAuthMethod, MarketAuthorizationStatus, MarketPrompt } from '../types/plugins';
import { Button, ErrorBox, Field, Input, Select } from './ui';

function visible(prompt: MarketPrompt, inputs: Record<string, string>) {
  const condition = prompt.condition;
  if (!condition) return true;
  const equal = (inputs[condition.key] || '') === condition.value;
  return condition.op === 'neq' || condition.op === '!=' ? !equal : equal;
}
function safeURL(value?: string) {
  try { const url = new URL(value || ''); return ['https:', 'http:'].includes(url.protocol) ? url.href : undefined; }
  catch { return undefined; }
}

export function MarketAuthorization({ packageID, providerID, methods, channelID, credentialID, resumeFlow, onStarted, onResumeGone, onDone }: {
  packageID: string; providerID: string; methods: MarketAuthMethod[]; channelID?: number; credentialID?: string;
  resumeFlow?: string; onStarted?: (flow: string | null) => void; onResumeGone?: () => void;
  onDone: (result: MarketAuthorizationStatus) => void;
}) {
  const [methodIndex, setMethodIndex] = useState(methods[0]?.index ?? 0);
  const [inputs, setInputs] = useState<Record<string, string>>({});
  const [flow, setFlow] = useState<MarketAuthorizationStatus | null>(null);
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const live = useRef(false), generation = useRef(0), writing = useRef(false);
  const timer = useRef<ReturnType<typeof setTimeout>>();
  const callbacks = useRef({ onStarted, onDone, onResumeGone }); callbacks.current = { onStarted, onDone, onResumeGone };
  const autoOpen = useRef<Promise<boolean>>(Promise.resolve(false));
  const method = methods.find(value => value.index === methodIndex) || methods[0];
  const prompts: MarketPrompt[] = [...(method?.prompts || [])];
  if (method?.type === 'api' && !prompts.length) prompts.push({ type: 'password', key: 'key', label: 'API Key' });
  const values = { ...Object.fromEntries(prompts.filter(prompt => prompt.type === 'select').map(prompt => [prompt.key, prompt.options?.[0]?.value || ''])), ...inputs };
  const shown = prompts.filter(prompt => visible(prompt, values));
  const flowPath = (id: string) => `/plugins/market/auth/${encodeURIComponent(id)}`;

  useEffect(() => {
    live.current = true;
    if (methods.some(value => value.type === 'oauth')) autoOpen.current = api<Settings>('/settings').then(settings => settings.oauth_auto_open !== false).catch(() => false);
    return () => { live.current = false; generation.current++; clearTimeout(timer.current); };
  }, []);

  function accept(next: MarketAuthorizationStatus, epoch: number) {
    if (!live.current || epoch !== generation.current) return;
    setFlow(next);
    if (next.status === 'success') { callbacks.current.onStarted?.(null); callbacks.current.onDone(next); }
    else if (next.status === 'pending') {
      callbacks.current.onStarted?.(next.flow_id);
      // Code flows wait for an explicit submission. Automatic flows are polled.
      if (next.method !== 'code') timer.current = setTimeout(() => void poll(next.flow_id, epoch), 2000);
    } else { callbacks.current.onStarted?.(null); setError(next.error || (next.status === 'cancelled' ? '授权已取消' : '授权未完成')); }
  }
  async function poll(id: string, epoch: number, resumed = false) {
    try { accept(await api<MarketAuthorizationStatus>(flowPath(id)), epoch); }
    catch (cause) {
      if (!live.current || epoch !== generation.current) return;
      if (resumed && cause instanceof ApiError && cause.status === 404) { callbacks.current.onStarted?.(null); callbacks.current.onResumeGone?.(); }
      else setError(errorMessage(cause));
    }
  }
  useEffect(() => {
    if (!resumeFlow) return;
    const epoch = ++generation.current;
    clearTimeout(timer.current);
    void poll(resumeFlow, epoch, true);
  }, [resumeFlow]);

  async function start() {
    if (writing.current || !method) return;
    writing.current = true; setBusy(true); setError('');
    const epoch = ++generation.current;
    clearTimeout(timer.current);
    // Open during the click gesture; navigate only after both preference and
    // the server's authorization URL are known.
    const popup = method.type === 'oauth' ? window.open('about:blank', '_blank') : null;
    try {
      const next = await api<MarketAuthorizationStatus>(`/plugins/market/${encodeURIComponent(packageID)}/auth/${encodeURIComponent(providerID)}/start`, {
        method: 'POST', body: JSON.stringify({ method: method.index, inputs: Object.fromEntries(shown.map(prompt => [prompt.key, values[prompt.key] || ''])), ...(channelID ? { channel_id: channelID } : {}), ...(credentialID ? { credential_id: credentialID } : {}) }),
      });
      if (live.current && epoch === generation.current) {
        setInputs({});
        const url = safeURL(next.url);
        if (popup && url && await autoOpen.current) popup.location.href = url; else popup?.close();
        accept(next, epoch);
      } else popup?.close();
    } catch (cause) { popup?.close(); if (live.current && epoch === generation.current) setError(errorMessage(cause)); }
    finally { writing.current = false; if (live.current) setBusy(false); }
  }
  async function complete() {
    if (!flow || writing.current || !code.trim()) return;
    writing.current = true; setBusy(true); setError('');
    const epoch = generation.current;
    try { accept(await api<MarketAuthorizationStatus>(`${flowPath(flow.flow_id)}/complete`, { method: 'POST', body: JSON.stringify({ code: code.trim() }) }), epoch); }
    catch (cause) { if (live.current && epoch === generation.current) setError(errorMessage(cause)); }
    finally { writing.current = false; if (live.current) setBusy(false); }
  }
  async function cancel() {
    if (writing.current) return;
    const id = flow?.flow_id || resumeFlow;
    ++generation.current; clearTimeout(timer.current);
    writing.current = true; setBusy(true); setError('');
    try {
      if (id) await api(flowPath(id), { method: 'DELETE' });
      if (live.current) { setFlow(null); setCode(''); callbacks.current.onStarted?.(null); }
    } catch (cause) { if (live.current) setError(errorMessage(cause)); }
    finally { writing.current = false; if (live.current) setBusy(false); }
  }
  const pending = flow?.status === 'pending';
  const url = safeURL(flow?.url);
  return <div className="space-y-3" aria-label="供应商授权">
    {!pending && <>
      {methods.length > 1 && <Field label="接入方式"><Select aria-label="接入方式" value={method?.index} disabled={busy} onChange={event => { setMethodIndex(Number(event.target.value)); setInputs({}); setError(''); }}>{methods.map(value => <option key={value.index} value={value.index}>{value.label}</option>)}</Select></Field>}
      {methods.length === 1 && <p>{method?.label}</p>}
      {shown.map(prompt => <Field key={prompt.key} label={prompt.label}>{prompt.type === 'select' ? <Select aria-label={prompt.label} value={values[prompt.key] || ''} disabled={busy} onChange={event => setInputs(current => ({ ...current, [prompt.key]: event.target.value }))}>{prompt.options?.map(option => <option key={option.value} value={option.value}>{option.label}{option.hint ? ` · ${option.hint}` : ''}</option>)}</Select> : <Input aria-label={prompt.label} type={prompt.type === 'password' || /key|secret|token|password/i.test(prompt.key) ? 'password' : 'text'} autoComplete="off" placeholder={prompt.placeholder} value={values[prompt.key] || ''} disabled={busy} onChange={event => setInputs(current => ({ ...current, [prompt.key]: event.target.value }))} />}</Field>)}
      {!method && <p>此供应商未提供可用的接入方式。</p>}
      <Button disabled={busy || !method} aria-busy={busy} onClick={() => void start()}>{busy ? '正在连接…' : method?.type === 'api' ? '连接' : '开始授权'}</Button>
    </>}
    {pending && <>
      {flow.instructions && <p className="whitespace-pre-wrap break-words">{flow.instructions}</p>}
      {url && <a className="btn btn-secondary" href={url} target="_blank" rel="noopener noreferrer">打开授权页面</a>}
      {flow.method === 'code' ? <><Field label="授权码"><Input aria-label="授权码" value={code} disabled={busy} onChange={event => setCode(event.target.value)} autoComplete="off" /></Field><Button disabled={busy || !code.trim()} onClick={() => void complete()}>{busy ? '正在确认…' : '完成授权'}</Button></> : <p role="status">正在等待授权完成…</p>}
      {error && flow.method !== 'code' && <Button variant="secondary" disabled={busy} onClick={() => { setError(''); clearTimeout(timer.current); void poll(flow.flow_id, generation.current); }}>重试查询</Button>}
      <Button variant="ghost" disabled={busy} onClick={() => void cancel()}>取消授权</Button>
    </>}
    {error && <ErrorBox message={error} />}
  </div>;
}
