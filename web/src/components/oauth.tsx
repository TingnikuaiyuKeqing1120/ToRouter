import { useCallback, useEffect, useRef, useState } from 'react';
import { AlertTriangle, Copy, ExternalLink, KeyRound, RefreshCw } from 'lucide-react';
import { api, credentialNotes, errorMessage, oauthErrorHints, oauthRoutes, oauthStatusLabels, type CredentialType, type OAuthOperation, type OAuthStart, type OAuthStatus, type Settings } from '../api';
import { Badge, Button, ErrorBox, Field, Input, useToast } from './ui';
import { MarketAuthorization } from './MarketAuthorization';
import type { MarketAuthMethod } from '../types/plugins';

type AuthorizationProps = { type: CredentialType; authMethods?: MarketAuthMethod[]; operation?: OAuthOperation; channelID?: number; credentialID?: string; onDone: (result?: OAuthStatus) => void; resumeFlow?: string; onStarted?: (flowID: string | null) => void; onResumeGone?: () => void };

export function OAuthAuthorize(props: AuthorizationProps) {
  if (props.type.startsWith('market:')) {
    const [, packageID, providerID] = props.type.split(':');
    return <MarketAuthorization packageID={packageID} providerID={providerID} methods={props.authMethods || []} channelID={props.channelID} credentialID={props.credentialID} resumeFlow={props.resumeFlow} onStarted={props.onStarted} onResumeGone={props.onResumeGone} onDone={result => props.onDone({ status: 'complete', channel_id: result.channel_id || props.channelID || 0 })} />;
  }
  return <NativeOAuthAuthorize key={`${props.type}:${props.channelID || 0}:${props.credentialID || ''}`} {...props} />;
}

/** One authorization flow: start it, show the user what to do, poll until it
 * ends. Each track differs only in what the user does between start and done —
 * type a code, approve in a browser, or paste a link back — so the pacing is
 * shared and the prompts are per-track.
 *
 * resumeFlow is the flow id a previous visit left in flight: the console sees
 * the same flow, so a remount keeps polling it instead of starting a second
 * authorization (the server is idempotent per provider, and this is what makes
 * that idempotence safe to rely on). A flow the console no longer answers —
 * most often a restart that lost the in-memory flows — is reported through
 * onResumeGone so the parent forgets the id and the entry point comes back. */
function NativeOAuthAuthorize({ type, operation = 'add_credential', channelID, credentialID, onDone, resumeFlow, onStarted, onResumeGone }: AuthorizationProps) {
  const route = type === 'api_key' ? '' : oauthRoutes[type];
  const [start, setStart] = useState<OAuthStart | null>(null);
  const [status, setStatus] = useState<OAuthStatus | null>(null);
  const [pasted, setPasted] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const toast = useToast();
  const timer = useRef<number | null>(null);
  const requestEpoch = useRef(0);
  // The auto-open preference is read once, when this flow view mounts, and its
  // default is on: a row from before the switch existed (or one this console
  // cannot read) keeps the previous behaviour. When it is off, the link below
  // is the only door — the user pastes it into whichever browser is signed in.
  // Start awaits the promise rather than the state, so a click landing inside
  // the read window never opens a tab the user switched off.
  const [autoOpen, setAutoOpen] = useState(true);
  const autoOpenPref = useRef<Promise<boolean> | null>(null);
  useEffect(() => {
    const loaded = api<Settings>('/settings')
      .then(v => v.oauth_auto_open !== false)
      .catch(() => true /* keep the default */);
    autoOpenPref.current = loaded;
    let live = true;
    loaded.then(v => { if (live) setAutoOpen(v); });
    return () => { live = false; };
  }, []);

  const stop = useCallback(() => {
    if (timer.current !== null) { window.clearTimeout(timer.current); timer.current = null; }
  }, []);
  useEffect(() => stop, [stop]);

  // A flow id from a previous visit is adopted as-is: the console still holds
  // it, so polling is the whole recovery.
  const [resumed] = useState(!!resumeFlow);

  async function poll(flow: string, epoch = requestEpoch.current) {
    if (cancelled.current || epoch !== requestEpoch.current) return;
    try {
      const next = await api<OAuthStatus>(`${route}/status?flow=${encodeURIComponent(flow)}`);
      if (cancelled.current || epoch !== requestEpoch.current) return;
      setStatus(next);
      if (next.status === 'complete') {
        stop();
        // A degraded answer is not a failure: the channel exists and the notice
        // below says what is missing, so the panel waits for the user to read
        // it instead of unmounting on its own.
        if (next.degraded && next.degraded.length) { toast('授权完成，渠道已就绪；部分步骤未完成，请查看提示。'); return; }
        toast('凭证已保存到当前渠道');
        onDone(next);
        return;
      }
      if (next.status === 'error') { stop(); return; }
      // The console paces the flow, so the poll interval is the one the flow
      // reported; a short default covers a start that did not name one.
      const seconds = 'interval' in next && next.interval ? next.interval : 2;
      stop();
      timer.current = window.setTimeout(() => { void poll(flow, epoch); }, Math.max(1, seconds) * 1000);
    } catch (e) {
      if (cancelled.current || epoch !== requestEpoch.current) return;
      // A flow the console no longer answers leaves nothing to resume: the
      // parent forgets the id (so the start button returns) and the panel shows
      // no stale error, because the fix is a fresh start, not a retry.
      if (resumed) { onResumeGone?.(); setStatus(null); setError(''); stop(); return; }
      setError(errorMessage(e));
      stop();
    }
  }

  // The tab the consent page opens in. It is opened synchronously inside the
  // click handler — a popup blocker lets a window through while the gesture is
  // still live and would refuse one opened after an await — and the start
  // answer only navigates it. When the preference turns out to be off, the tab
  // is closed again unread instead of being navigated.
  const popup = useRef<Window | null>(null);
  const [popupBlocked, setPopupBlocked] = useState(false);
  const [canceling, setCanceling] = useState(false);
  // A poll that was already in flight when the user cancelled must not put the
  // waiting state back on screen.
  const cancelled = useRef(false);
  const closePopup = useCallback(() => {
    try { popup.current?.close(); } catch { /* the tab is already gone */ }
    popup.current = null;
  }, []);

  useEffect(() => { cancelled.current = false; return () => { requestEpoch.current++; cancelled.current = true; closePopup(); stop(); }; }, [closePopup, stop]);
  useEffect(() => { if (resumeFlow && !start) { stop(); void poll(resumeFlow, ++requestEpoch.current); } }, [resumeFlow]);

  // Cancel tells the console to stop waiting: it releases the flow's listener
  // and marks it cancelled, and the panel goes back to the start state. The
  // parent is told the same way a completed flow is (a null id), so the stored
  // entry is forgotten and a later visit starts clean.
  async function cancelFlow() {
    const id = start?.flow || resumeFlow;
    if (!id || canceling) return;
    cancelled.current = true;
    requestEpoch.current++;
    stop();
    setCanceling(true);
    try {
      await api(`${route}/cancel?flow=${encodeURIComponent(id)}`, { method: 'POST' });
    } catch {
      // The flow is over locally either way: a 404 means the console already
      // pruned it, which is the outcome being asked for.
    } finally { setCanceling(false); }
    setStart(null); setStatus(null); setPasted(''); setPopupBlocked(false);
    onStarted?.(null);
  }

  async function startFlow(device = false) {
    if (busy) return;
    setBusy(true); setError(''); setStatus(null); setPasted(''); setPopupBlocked(false);
    cancelled.current = false;
    const epoch = ++requestEpoch.current;
    if (autoOpen) {
      // No noopener here: with it the browser hands back a null handle by
      // standard, and the opener reference is what lets the start answer
      // navigate the tab it just opened.
      popup.current = window.open('about:blank', '_blank');
    }
    try {
      const query = new URLSearchParams();
      query.set('operation', operation === 'reauth' ? 'reauth' : 'add_credential');
      query.set('method', device || type === 'copilot_oauth' || type.startsWith('plugin:qoder') || type.startsWith('plugin:minimax-code') ? 'device' : 'browser');
      if (channelID) query.set('channel_id', String(channelID));
      if (operation === 'reauth' && credentialID) query.set('credential_id', credentialID);
      const opened = await api<OAuthStart>(`${route}/start?${query.toString()}`, { method: 'POST' });
      onStarted?.(opened.flow);
      if (cancelled.current || epoch !== requestEpoch.current) { closePopup(); return; }
      setStart(opened);
      if ('authorization_url' in opened) {
        // The preference is awaited here rather than read from the click, so a
        // start inside the read window cannot navigate a tab the user switched
        // off: it is closed again instead, before it ever shows a page.
        const shouldOpen = await (autoOpenPref.current ?? Promise.resolve(true));
        if (cancelled.current || epoch !== requestEpoch.current) return;
        if (shouldOpen) {
          if (popup.current) {
            popup.current.location = opened.authorization_url;
          } else {
            // A null handle is the browser refusing the tab (a popup blocker, or
            // one that nulls the handle anyway): the rendered link is the way,
            // and the hint says so instead of leaving the user waiting for a tab
            // that will never appear.
            setPopupBlocked(true);
          }
        } else {
          closePopup();
        }
      }
      void poll(opened.flow, epoch);
    } catch (e) { if (!cancelled.current && epoch === requestEpoch.current) { closePopup(); setError(errorMessage(e)); } } finally { if (epoch === requestEpoch.current) setBusy(false); }
  }

  async function submitPaste() {
    if (!start || !pasted.trim()) return;
    setError('');
    const epoch = requestEpoch.current;
    try {
      const next = await api<OAuthStatus>(`${route}/status?flow=${encodeURIComponent(start.flow)}&callback=${encodeURIComponent(pasted.trim())}`);
      if (cancelled.current || epoch !== requestEpoch.current) return;
      setStatus(next);
      if (next.status === 'complete') {
        stop();
        // Same rule as the poll: a degraded answer stays on screen.
        if (next.degraded && next.degraded.length) { toast('授权完成，渠道已就绪；部分步骤未完成，请查看提示。'); return; }
        toast('授权完成，渠道已就绪');
        onDone(next);
      }
    } catch (e) { if (!cancelled.current && epoch === requestEpoch.current) setError(errorMessage(e)); }
  }

  async function copy(text: string, label: string) {
    try { await navigator.clipboard.writeText(text); toast(`${label}已复制`); }
    catch { setError('复制失败，请手动选择文本复制。'); }
  }

  const done = status?.status === 'complete';
  const degraded = status?.status === 'complete' ? status.degraded || [] : [];
  const degradedLabels: Record<string, string> = { account_profile: '账号资料' };
  return (
    <div className="space-y-3 rounded-lg border border-[var(--color-border)] p-4">
      <p className="text-xs leading-5 text-[var(--color-text-secondary)]">{credentialNotes[type]}</p>
      {!start && !resumeFlow && (
        <div className="flex flex-wrap gap-2">
          <Button type="button" disabled={busy} onClick={() => void startFlow(false)}>
            <KeyRound size={15} /> {type === 'copilot_oauth' ? '开始设备码授权' : type === 'codex_oauth' ? '浏览器授权' : '开始授权'}
          </Button>
          {type === 'codex_oauth' && <Button type="button" variant="secondary" disabled={busy} onClick={() => void startFlow(true)}>设备码授权</Button>}
        </div>
      )}
      {error && <ErrorBox message={error} />}
      {start && (
        <div className="space-y-3">
          {'user_code' in start ? (
            <>
              <Field label="验证码" hint={`在 ${start.verification_uri} 输入这组字符。`}>
                <div className="flex gap-2">
                  <code className="flex-1 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 py-2 font-mono text-lg tracking-widest text-[var(--color-text-primary)]">{start.user_code}</code>
                  <Button type="button" variant="secondary" aria-label="复制验证码" onClick={() => void copy(start.user_code, '验证码')}><Copy size={15} /></Button>
                </div>
              </Field>
              <a className="inline-flex items-center gap-1.5 text-sm text-teal-700 dark:text-teal-400 underline" href={start.verification_uri} target="_blank" rel="noreferrer noopener">打开验证页面 <ExternalLink size={14} /></a>
            </>
          ) : (
            <>
              <Field label="授权链接" hint={popupBlocked ? '浏览器没有交出自动打开的标签页：请手动点击下面的链接完成授权。' : autoOpen ? (start.paste_hint || '在新标签页中完成授权，然后回到这里。') : '自动打开已关闭：复制授权链接，在你要登录的浏览器中打开，完成授权后回到这里。'}>
                <div className="flex gap-2">
                  <code className="min-w-0 flex-1 truncate rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 py-2 font-mono text-xs text-[var(--color-text-secondary)]">{start.authorization_url}</code>
                  <Button type="button" variant="secondary" aria-label="复制授权链接" onClick={() => void copy(start.authorization_url, '授权链接')}><Copy size={15} /></Button>
                </div>
              </Field>
              <div className="flex flex-wrap items-center gap-2">
                <a className="inline-flex items-center gap-1.5 text-sm text-teal-700 dark:text-teal-400 underline" href={start.authorization_url} target="_blank" rel="noreferrer noopener">重新打开授权页 <ExternalLink size={14} /></a>
                {start.redirect_uri && <Button type="button" variant="ghost" onClick={() => void copy(start.redirect_uri!, '回调地址')}>复制本机回调地址</Button>}
              </div>
              {start.redirect_uri ? <Field label="粘贴回调链接" hint="浏览器无法自动跳转时，把地址栏里的完整链接粘贴到这里。">
                <div className="flex gap-2"><Input aria-label="回调链接" value={pasted} onChange={e => setPasted(e.target.value)} placeholder="http://127.0.0.1…/auth/callback?code=…" /><Button type="button" variant="secondary" disabled={!pasted.trim()} onClick={() => void submitPaste()}>提交</Button></div>
              </Field> : <p className="text-xs text-[var(--color-text-secondary)]">{start.instructions || '在浏览器完成授权后，此处会自动确认。'}</p>}
              {start.non_official && <p className="flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/10 p-3 text-xs leading-5 text-amber-500 dark:text-amber-400 font-medium"><AlertTriangle size={14} className="mt-0.5 shrink-0" />{start.non_official}</p>}
            </>
          )}
          <div className="space-y-2">
            <div className="flex flex-wrap items-center gap-2 text-xs text-[var(--color-text-secondary)]">
              <RefreshCw size={13} className={status?.status === 'pending' ? 'animate-spin' : ''} />
              {status?.status === 'pending' ? '等待授权完成…' : done ? '授权已完成' : resumed ? '正在等待上次未完成的授权…' : '正在开始…'}
              {status?.status === 'cancelled' && <span>授权已取消。</span>}
              {status?.status === 'error' && <span className="text-red-600">{status.message}</span>}
            </div>
            {type === 'codex_oauth' && status?.status === 'pending' && <p className="text-xs text-[var(--color-text-muted)]">如果已关闭授权页面，请点下方取消后重新开始。</p>}
            {!done && !degraded.length && status?.status !== 'error' && <div className="flex"><Button type="button" variant="ghost" disabled={canceling} onClick={() => void cancelFlow()}>{canceling ? '正在取消…' : '取消授权'}</Button></div>}
          </div>
        </div>
      )}
      {!start && resumeFlow && (
        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-2 text-xs text-[var(--color-text-secondary)]">
            <RefreshCw size={13} className={status?.status === 'pending' ? 'animate-spin' : ''} />
            {status?.status === 'pending' ? '等待授权完成…' : status?.status === 'complete' ? '授权已完成' : status?.status === 'cancelled' ? '授权已取消。' : '正在恢复上次未完成的授权…'}
            {status?.status === 'error' && <span className="text-red-600">{status.message}</span>}
          </div>
          {type === 'codex_oauth' && status?.status === 'pending' && <p className="text-xs text-[var(--color-text-muted)]">如果已关闭授权页面，请点下方取消后重新开始。</p>}
          {status?.status !== 'complete' && status?.status !== 'error' && <div className="flex"><Button type="button" variant="ghost" disabled={canceling} onClick={() => void cancelFlow()}>{canceling ? '正在取消…' : '取消授权'}</Button></div>}
        </div>
      )}
      {(status?.status === 'error' || status?.status === 'cancelled') && (
        <div className="space-y-2">
          {status.status === 'error' && <ErrorBox message={status.message} />}
          {status.status === 'error' && status.existing_channel_id && <p className="text-xs">凭证已在其他渠道中保存。<a className="underline" href={`#/channels?channel_id=${status.existing_channel_id}${status.existing_credential_id ? `&credential_id=${encodeURIComponent(status.existing_credential_id)}` : ''}`}>前往已有凭证</a>，可从凭证菜单选择“移到其他渠道”。</p>}
          {status.status === 'error' && oauthErrorHints[status.code] && <p className="text-xs leading-5 text-[var(--color-text-secondary)]">{oauthErrorHints[status.code]}</p>}
          <Button type="button" variant="secondary" onClick={() => { setStart(null); setStatus(null); setPopupBlocked(false); if (resumed) onResumeGone?.(); }}>重新开始</Button>
        </div>
      )}
      {!!degraded.length && (
        <div className="space-y-2 rounded-lg border border-amber-500/25 bg-amber-500/10 p-3 text-xs leading-5 text-amber-500 dark:text-amber-400 font-medium" role="status">
          <p className="flex items-start gap-2"><AlertTriangle size={14} className="mt-0.5 shrink-0" />渠道已就绪，但以下步骤没有完成：{degraded.map(code => degradedLabels[code] || code).join('、')}。{status?.status === 'complete' && status.message ? status.message : ''}</p>
          <div className="flex justify-end"><Button type="button" variant="secondary" onClick={() => onDone(status || undefined)}>知道了</Button></div>
        </div>
      )}
    </div>
  );
}

/** The credential status of an existing OAuth channel: what the gateway knows
 * about it, why it is not usable, and how to re-authorize. */
export function OAuthStatusBadge({ oauth, onReauthorize }: { oauth?: { status?: string; status_reason?: string; account_label?: string; api_base?: string; expires_at?: string; updated_at?: string }; onReauthorize?: () => void }) {
  const status = oauth?.status || 'unknown';
  const tone = status === 'active' ? 'teal' : status === 'unknown' ? 'neutral' : 'red';
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5 text-xs">
      <Badge tone={tone}>{status === 'unknown' ? '凭据状态未知' : oauthStatusLabels[status] || status}</Badge>
      {oauth?.account_label && <span className="text-[var(--color-text-secondary)]">{oauth.account_label}</span>}
      {status !== 'active' && <span className="text-red-600">{oauth?.status_reason}</span>}
      {status !== 'active' && onReauthorize && <Button type="button" variant="ghost" className="!px-1.5 !py-0.5 !text-xs" onClick={onReauthorize}>重新授权</Button>}
    </span>
  );
}
