import { useEffect, useRef, useState } from 'react';
import { MoreVertical, RefreshCw } from 'lucide-react';
import { api, errorMessage, type Channel, type CodexQuotaResponse, type CredentialItem } from '../api';
import { useCredentialOperations } from '../hooks/useCredentialOperations';
import type { CredentialOperationKind, CredentialOperationReceipt, CredentialOperationsConfig, CredentialOperationsPatch, CredentialOperationsReport, CredentialResetCards, CredentialAutoResetDraft } from '../types/credential-operations';
import { useConfirm } from './Confirm';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { Badge, Button, ErrorBox, Field, Loading, Modal, Select } from './ui';
import { CodexAutomaticResetForm, CodexAutomaticWarmupForm } from './CodexAutomationForms';
import { codexGeneralWindows, codexHasFiveHourWindow, createAutoResetDraft, validateAutomaticReset, validateAutomaticWarmup } from '../services/credentialAutomation';
import './CodexCredentialOperations.css';

export type CodexOperationsView = 'cards' | 'warmup' | 'auto-warmup' | 'auto-reset';
type Draft = { warmup: CredentialOperationsConfig['warmup']; auto_reset: CredentialAutoResetDraft };
const toDraft = (config: CredentialOperationsConfig): Draft => ({ warmup: { ...config.warmup, daily_times: [...config.warmup.daily_times] }, auto_reset: createAutoResetDraft(config.auto_reset) });
function displayTime(value?: string | number) {
  if (value == null || value === '') return '未知';
  const date = new Date(typeof value === 'number' ? value * 1000 : value);
  return Number.isFinite(date.getTime()) ? date.toLocaleString() : '未知';
}
const receiptLabels = { running: '正在执行', succeeded: '已完成', failed: '未完成', unknown: '结果未确认', acknowledged: '已核对' };

export function CodexCredentialMenu({ channel, credential, name, blocked, hasFiveHourWindow = false, configure, onChanged, onError }: { channel: Channel; credential: CredentialItem; name: string; blocked: boolean; hasFiveHourWindow?: boolean; configure: (view: CodexOperationsView, enable?: 'warmup' | 'auto_reset') => void; onChanged: () => void; onError: (message: string) => void }) {
  const [saving, setSaving] = useState(false);
  const pending = useRef(false), live = useRef(true), menuRef = useRef<HTMLDetailsElement>(null);
  useEffect(() => { live.current = true; return () => { live.current = false; }; }, []);
  useEffect(() => {
    const close = () => { if (menuRef.current) menuRef.current.open = false; };
    document.addEventListener('scroll', close, true); window.addEventListener('resize', close);
    return () => { document.removeEventListener('scroll', close, true); window.removeEventListener('resize', close); };
  }, []);
  async function toggle(kind: 'warmup' | 'auto_reset') {
    if (blocked || pending.current) return;
    pending.current = true; setSaving(true); onError('');
    try {
      const base = `/channels/${channel.id}/credentials/${encodeURIComponent(credential.id)}/operations`;
      const current = await api<CredentialOperationsReport>(base);
      if (!live.current) return;
      if (channel.generation !== undefined && current.channel_generation !== channel.generation) throw new Error('渠道或凭证已变化，请刷新渠道。');
      if (!current.supported) throw new Error('此凭证暂不支持 Codex 操作。');
      const enabled = credential.operations?.[kind].enabled !== true;
      if (kind === 'warmup' && enabled && !hasFiveHourWindow) throw new Error('上游未明确提供 5 小时额度窗口，暂不支持自动预热。');
      if (enabled && (kind === 'warmup' ? !channel.models.includes(current.config.warmup.model) || current.config.warmup.mode === 'daily' && !current.config.warmup.daily_times.length : !current.config.auto_reset.rules.length)) { configure(kind === 'warmup' ? 'auto-warmup' : 'auto-reset', kind); return; }
      const patch = kind === 'warmup' ? { warmup: { ...current.config.warmup, enabled } } : { auto_reset: { ...current.config.auto_reset, enabled } };
      await api<CredentialOperationsReport>(base, { method: 'PUT', body: JSON.stringify({ expected_identity: current.identity, ...patch }) });
      if (live.current) onChanged();
    } catch (cause) { if (live.current) onError(`自动配置未保存：${errorMessage(cause)}`); }
    finally { pending.current = false; if (live.current) setSaving(false); }
  }
  return <details ref={menuRef} className="oauth-main-menu codex-credential-menu" onToggle={event => {
    const menu = event.currentTarget, panel = menu.querySelector<HTMLElement>(':scope > div');
    if (!menu.open || !panel) return;
    const rect = menu.getBoundingClientRect();
    panel.style.left = `${Math.max(12, Math.min(rect.right - 250, window.innerWidth - 262))}px`;
    panel.style.top = `${rect.bottom + 144 < window.innerHeight ? rect.bottom + 4 : Math.max(12, rect.top - 140)}px`;
  }} onKeyDown={event => { if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); event.currentTarget.open = false; event.currentTarget.querySelector('summary')?.focus(); } }}>
    <summary aria-label={`更多凭证操作 ${name}`} title="更多凭证操作"><MoreVertical size={17} /></summary>
    <div>{(['warmup', 'auto_reset'] as const).map(kind => <div className="codex-credential-menu-option" key={kind}><span>{kind === 'warmup' ? '自动预热' : '自动重置'}</span><button type="button" role="switch" className={`switch ${credential.operations?.[kind].enabled ? 'switch-on' : ''}`} aria-label={`${kind === 'warmup' ? '自动预热' : '自动重置'} ${name}`} aria-checked={credential.operations?.[kind].enabled || false} disabled={blocked || saving || kind === 'warmup' && !hasFiveHourWindow && !credential.operations?.warmup.enabled} onClick={() => void toggle(kind)}><span /></button><Button variant="ghost" disabled={blocked || saving} aria-label={`配置${kind === 'warmup' ? '自动预热' : '自动重置'} ${name}`} onClick={() => configure(kind === 'warmup' ? 'auto-warmup' : 'auto-reset')}>配置</Button></div>)}</div>
  </details>;
}

function Receipt({ kind, value, busy, acknowledge }: { kind: CredentialOperationKind; value?: CredentialOperationReceipt; busy: boolean; acknowledge: () => void }) {
  if (!value) return null;
  return <div className="codex-operation-receipt" role="status">
    <div><span>{kind === 'reset' ? '上次使用重置卡' : '上次预热'}</span><Badge tone={value.state === 'succeeded' ? 'teal' : value.state === 'failed' || value.state === 'unknown' ? 'amber' : 'neutral'}>{receiptLabels[value.state] || '结果未确认'}</Badge></div>
    <p>{value.detail}</p><span>{displayTime(value.at)}</span>
    {value.state === 'unknown' && <div><span>核对账号实际结果后，可解除操作阻塞。</span><Button variant="secondary" disabled={busy} onClick={acknowledge}>已核对{kind === 'reset' ? '重置卡结果' : '预热结果'}</Button></div>}
  </div>;
}

export function CodexCredentialOperations({ channel, credential, name, quota, initialView: view, enableOnOpen, blocked, networkBlocked = false, onClose, onCards, onChanged }: {
  channel: Channel; credential: CredentialItem; name: string; quota?: CodexQuotaResponse; initialView: CodexOperationsView; enableOnOpen?: 'warmup' | 'auto_reset'; blocked: boolean; networkBlocked?: boolean; onClose: () => void; onCards: (cards: CredentialResetCards) => void; onChanged: () => void;
}) {
  const operations = useCredentialOperations(channel.id, credential.id, channel.generation, view === 'cards', !networkBlocked);
  const { report, pending, unresolved } = operations;
  const [draft, setDraft] = useState<Draft>();
  const [formError, setFormError] = useState('');
  const [saved, setSaved] = useState(false);
  const dirty = useRef(false), initialized = useRef(false), submitting = useRef(false), seenCards = useRef<CredentialResetCards>();
  const confirm = useConfirm();
  const windows = codexGeneralWindows(quota);
  const hasFiveHourWindow = codexHasFiveHourWindow(quota);
  const oauth = credential.oauth_info || credential.oauth;
  const authorized = credential.has_oauth !== false && !!(credential.has_oauth || oauth) && oauth?.status !== 'reauth_required' && oauth?.status !== 'unsubscribed';
  const enabled = channel.enabled && credential.enabled !== false;
  const busy = !!pending;
  const readOnly = busy || !report?.supported || operations.identityChanged;
  const previousUnresolved = [report?.last_reset, report?.last_warmup].some(receipt => receipt?.state === 'running' || receipt?.state === 'unknown');
  const actionBlocked = readOnly || blocked || networkBlocked || !enabled || !authorized || !!unresolved || previousUnresolved;
  const receiptKind = view === 'cards' ? 'reset' : view === 'warmup' ? 'warmup' : undefined;
  const currentReceipt = receiptKind === 'reset' ? report?.last_reset : receiptKind === 'warmup' ? report?.last_warmup : undefined;
  const currentPendingReceipt = unresolved && (unresolved.kind === 'reset' ? report?.last_reset : report?.last_warmup);
  const missingReceipt = unresolved?.kind === receiptKind && currentPendingReceipt?.request_id !== unresolved?.request_id;
  const blockedKind = unresolved?.kind || (report?.last_reset?.state === 'running' || report?.last_reset?.state === 'unknown' ? 'reset' : report?.last_warmup?.state === 'running' || report?.last_warmup?.state === 'unknown' ? 'warmup' : undefined);
  const otherActionPending = receiptKind && blockedKind && blockedKind !== receiptKind;

  useEffect(() => {
    if (report?.supported && !dirty.current) {
      const next = toDraft(report.config);
      if (!initialized.current && ((view === 'auto-warmup' && enableOnOpen === 'warmup' && hasFiveHourWindow) || (view === 'auto-reset' && enableOnOpen === 'auto_reset'))) {
        next[enableOnOpen!].enabled = true; dirty.current = true;
      }
      initialized.current = true; setDraft(next);
    }
    if (view === 'cards' && report?.reset_cards && seenCards.current !== report.reset_cards) { seenCards.current = report.reset_cards; onCards(report.reset_cards); }
  }, [report, onCards, view, enableOnOpen, hasFiveHourWindow]);

  function update(change: (current: Draft) => Draft) { dirty.current = true; setSaved(false); setFormError(''); setDraft(current => current && change(current)); }
  function validateModel(required: boolean) {
    if (!draft) return false;
    if (required && !draft.warmup.model) { setFormError('请选择一个已启用的预热模型。'); return false; }
    if (draft.warmup.model && !channel.models.includes(draft.warmup.model)) { setFormError('原预热模型已停用，请重新选择，或清空模型。'); return false; }
    return true;
  }
  function configPatch(): CredentialOperationsPatch | undefined {
    if (!draft) return;
    if (view === 'auto-warmup') {
      const result = validateAutomaticWarmup(draft.warmup, channel.models, hasFiveHourWindow);
      if (result.error) { setFormError(result.error); return; }
      if (result.value) return { warmup: result.value };
    }
    if (view === 'auto-reset') {
      const result = validateAutomaticReset(draft.auto_reset);
      if (result.error) { setFormError(result.error); return; }
      if (result.value) return { auto_reset: result.value };
    }
  }
  async function saveConfiguration() {
    if (readOnly || submitting.current) return false;
    const patch = view === 'warmup' ? (validateModel(false) ? { warmup_model: draft!.warmup.model } : undefined) : configPatch(); if (!patch) return false;
    submitting.current = true;
    try {
      const result = await operations.save(patch); if (!result) return false;
      dirty.current = false; setSaved(true); setDraft(toDraft(result.config)); onChanged();
      return true;
    } finally { submitting.current = false; }
  }
  async function warmupOnce() {
    if (actionBlocked || submitting.current || !draft || !validateModel(true)) return;
    submitting.current = true;
    try {
      if (draft.warmup.model !== report?.config.warmup.model) {
        const result = await operations.save({ warmup_model: draft.warmup.model }); if (!result) return;
        dirty.current = false; setDraft(toDraft(result.config));
      }
      dirty.current = false;
      await operations.execute('warmup'); onChanged();
    } finally { submitting.current = false; }
  }
  const canLeave = useUnsavedChanges({ dirty: () => dirty.current, busy: () => busy || submitting.current, save: saveConfiguration });
  async function close() { if (await canLeave()) onClose(); }
  async function consume() {
    if (actionBlocked || !await confirm({ title: '使用一张重置卡？', description: `将为「${name}」消费一张可用重置卡，由上游选择。此操作会消耗重置卡。`, confirmLabel: '确认使用一张' })) return;
    const result = await operations.execute('reset');
    if (result?.last_reset?.state === 'succeeded') onChanged();
  }
  async function acknowledge(kind: CredentialOperationKind) {
    if (!await confirm({ title: '已核对实际结果？', description: '确认你已核对账号中的实际结果。解除阻塞后，将允许后续手动或自动操作。', confirmLabel: '确认已核对' })) return;
    await operations.acknowledge(kind);
  }
  const knownUnavailable = report?.reset_cards?.available_count === 0;
  const hasAvailableCard = (report?.reset_cards?.available_count || 0) > 0 || report?.reset_cards?.credits.some(card => card.available);
  const titles = { cards: '重置卡', warmup: '手动预热', 'auto-warmup': '自动预热', 'auto-reset': '自动重置' };
  const modelField = draft && <Field label="预热模型"><Select aria-label="预热模型" value={draft.warmup.model} disabled={readOnly} onChange={event => update(current => ({ ...current, warmup: { ...current.warmup, model: event.target.value } }))}><option value="">选择已启用的模型</option>{draft.warmup.model && !channel.models.includes(draft.warmup.model) && <option value={draft.warmup.model} disabled>{draft.warmup.model}（已停用）</option>}{channel.models.map(model => <option key={model} value={model}>{model}</option>)}</Select>{!channel.models.length && <span>渠道尚未启用模型，请先编辑渠道。</span>}</Field>;
  return <Modal title={`${titles[view]} · ${name}`} description={channel.name} onClose={() => void close()} busy={busy} className={`codex-operations-dialog codex-operations-${view}`}>
    <div className="codex-operations-content">
      {operations.error && <ErrorBox message={operations.error} />}
      {networkBlocked && <p className="codex-operations-muted">接入插件未就绪，联网操作已暂停；配置与已保存信息仍可管理。</p>}
      {operations.identityChanged && <p>请刷新渠道后重新打开此凭证。</p>}
      {!report && busy ? <Loading /> : !report ? <Button variant="secondary" disabled={busy} onClick={() => void operations.reload()}>重新读取状态</Button> : !report.supported ? <p>此凭证暂不支持 Codex 操作。</p> : <>
        {view === 'cards' && <section className="codex-reset-cards" aria-label="重置卡明细">
          <div className="codex-operations-section-heading"><h3>可用 {report.reset_cards?.available_count == null ? '次数未知' : `${report.reset_cards.available_count} 次`}</h3><Button variant="ghost" disabled={readOnly || networkBlocked} onClick={() => void operations.refreshCards()}><RefreshCw size={14} />{pending === 'cards' ? '正在刷新…' : '刷新重置卡'}</Button></div>
          {report.reset_cards_error && <ErrorBox message={`${report.reset_cards_error}${report.reset_cards ? '（保留上次明细）' : ''}`} />}
          {report.reset_cards ? <><p className="codex-operations-muted">更新：{displayTime(report.reset_cards.captured_at)}</p>{!report.reset_cards.credits.length ? <p>暂无重置卡明细。</p> : <ul className="codex-reset-card-list">{report.reset_cards.credits.map((card, index) => <li key={`${index}:${card.id}`}><div><strong>{({ rate_limit: '额度重置', rate_limit_reset: '额度重置', full_reset: '完整重置' } as Record<string, string>)[card.type] || card.type || '类型未知'}</strong><Badge>{card.available ? '可用' : ({ redeemed: '已使用', expired: '已到期', available: '不可用' } as Record<string, string>)[card.status] || '状态未知'}</Badge></div><p>到期：{displayTime(card.expires_at)}</p>{card.redeemed_at != null && <p>使用：{displayTime(card.redeemed_at)}</p>}</li>)}</ul>}</> : <p>尚未获取重置卡。</p>}
          <p className="codex-operations-muted">每次使用一张，由上游选卡。</p>
          <Button disabled={actionBlocked || knownUnavailable || !hasAvailableCard} onClick={() => void consume()}>使用一张</Button>
        </section>}
        {view === 'warmup' && draft && <section className="codex-operations-config" aria-label="手动预热配置">{modelField}<p className="codex-operations-muted">发送短消息 hi，会使用账号额度。此模型与自动预热共用。</p></section>}
        {view === 'auto-warmup' && draft && <CodexAutomaticWarmupForm value={draft.warmup} onChange={warmup => update(current => ({ ...current, warmup }))} models={channel.models} hasFiveHourWindow={hasFiveHourWindow} timeZone={report.time_zone} disabled={readOnly} />}
        {view === 'auto-reset' && draft && <CodexAutomaticResetForm value={draft.auto_reset} onChange={auto_reset => update(current => ({ ...current, auto_reset }))} windows={windows} disabled={readOnly} />}
        {formError && <ErrorBox message={formError} />}
        {receiptKind && <>
          {(!enabled || !authorized) && <p className="codex-operations-muted">{!enabled ? '渠道或凭证已停用' : '凭证需要有效授权'}，可查看信息，暂不能{receiptKind === 'reset' ? '使用重置卡' : '预热'}。</p>}
          <Receipt kind={receiptKind} value={currentReceipt} busy={readOnly} acknowledge={() => void acknowledge(receiptKind)} />
          {missingReceipt && unresolved && !busy && <div className="codex-operation-receipt" role="status"><strong>结果未确认</strong><p>尚未读取到本次{receiptKind === 'reset' ? '重置卡' : '预热'}回执。重试会沿用原请求，不创建新操作。</p><Button variant="secondary" disabled={readOnly || blocked || networkBlocked || !enabled || !authorized} onClick={() => void operations.execute(receiptKind, true)}>重试本次操作</Button></div>}
          {otherActionPending && <p className="codex-operations-muted">另有{blockedKind === 'reset' ? '重置卡' : '预热'}操作尚未确认，请到{blockedKind === 'reset' ? '重置卡' : '手动预热'}弹窗读取状态并核对。</p>}
          <Button variant="ghost" disabled={readOnly} onClick={() => void operations.reload()}><RefreshCw size={14} />重新读取状态</Button>
        </>}
      </>}
    </div>
    <div className="modal-footer"><span className="codex-operations-muted" role="status">{pending === 'warmup' ? '正在预热…' : pending === 'reset' ? '正在使用重置卡…' : pending === 'save' ? '正在保存…' : pending === 'cards' ? '正在刷新重置卡…' : saved ? '已保存' : ''}</span><div className="codex-operations-footer-actions"><Button variant="secondary" disabled={busy} onClick={() => void close()}>关闭</Button>{view === 'warmup' && <Button disabled={actionBlocked} onClick={() => void warmupOnce()}>{dirty.current ? '保存并预热' : '预热一次'}</Button>}{(view === 'auto-warmup' || view === 'auto-reset') && <Button disabled={readOnly} onClick={() => void saveConfiguration()}>保存配置</Button>}</div></div>
  </Modal>;
}
