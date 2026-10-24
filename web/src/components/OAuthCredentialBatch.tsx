import { useEffect, useRef, useState } from 'react';
import type { Channel, CodexQuotaResponse } from '../api';
import { quotaTargetKey, type QuotaObservation, type QuotaTarget } from '../hooks/useCodexQuota';
import { oauthCredentialIdentity, useOAuthCredentialBatch, type OAuthBatchTarget } from '../hooks/useOAuthCredentialBatch';
import type { CredentialAutoResetDraft, CredentialOperationsConfig, CredentialOperationsPatch } from '../types/credential-operations';
import { codexGeneralWindows, codexHasFiveHourWindow, createAutoResetDraft, validateAutomaticReset, validateAutomaticWarmup } from '../services/credentialAutomation';
import { CodexAutomaticResetForm, CodexAutomaticWarmupForm } from './CodexAutomationForms';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { Button, ErrorBox, Modal } from './ui';
import { OAuthTransfer } from './OAuthTransfer';
import './OAuthCredentialBatch.css';

type View = 'warmup' | 'auto-warmup' | 'auto-reset';
const labels = { pending: '等待', succeeded: '完成', failed: '失败', skipped: '跳过', unknown: '结果未确认' };
export function OAuthCredentialBatch({ channel, items, codex, observations, refresh, blocked, networkBlocked = false, clear, onChanged, onBusy, onGeneration }: {
  channel: Channel; items: OAuthBatchTarget[]; codex: boolean; observations: Record<string, QuotaObservation>; refresh: (target: QuotaTarget) => void; blocked: boolean; networkBlocked?: boolean; clear: () => void; onChanged: () => void; onBusy: (busy: boolean) => void; onGeneration?: (generation: number) => void;
}) {
  const batch = useOAuthCredentialBatch(channel, observations, refresh, onChanged, onGeneration);
  const [view, setView] = useState<View>();
  const [actionItems, setActionItems] = useState<OAuthBatchTarget[]>([]);
  const [warmup, setWarmup] = useState<CredentialOperationsConfig['warmup']>({ enabled: false, mode: 'reset', model: '', daily_times: [] });
  const [autoReset, setAutoReset] = useState<CredentialAutoResetDraft>(() => createAutoResetDraft({ enabled: false, rules: [] }));
  const [error, setError] = useState('');
  const [exportItems, setExportItems] = useState<OAuthBatchTarget[]>();
  const [transferBusy, setTransferBusy] = useState(false);
  const dirty = useRef(false), savedPatch = useRef<CredentialOperationsPatch>();
  const networkPaused = useRef(networkBlocked); networkPaused.current = networkBlocked;
  useEffect(() => { onBusy(batch.busy || transferBusy); }, [batch.busy, transferBusy]);
  useEffect(() => () => { onBusy(false); }, []);
  const busy = blocked || batch.busy || transferBusy;
  const transferable = !channel.credential_type?.startsWith('market:') && channel.credential_type !== 'zen_free' && channel.credential_type !== 'api_key';
  const exportValid = !!exportItems?.length && exportItems.every(item => {
    const credential = channel.credentials?.find(value => value.id === item.credential.id);
    return item.target.channelID === channel.id && item.generation === channel.generation && !!credential && item.target.identity === oauthCredentialIdentity(credential);
  });
  const quota = (item: OAuthBatchTarget): CodexQuotaResponse | undefined => observations[quotaTargetKey(item.target)]?.snapshot;
  const windows = [...new Set(actionItems.flatMap(item => codexGeneralWindows(quota(item))))].sort((a, b) => b - a);
  const someFiveHours = actionItems.some(item => codexHasFiveHourWindow(quota(item)));
  function open(next: View) { batch.clearResults(); if (next === 'auto-warmup') void batch.inspectTimeZone(items); setActionItems([...items]); setView(next); setError(''); dirty.current = false; savedPatch.current = undefined; }
  const canLeave = useUnsavedChanges({ dirty: () => !!view && dirty.current, busy: batch.busy, save: () => save() });
  async function close() { if (await canLeave()) { dirty.current = false; setView(undefined); } }
  async function enable(enabled: boolean) {
    setError('');
    if (!enabled && channel.enabled && !(channel.credentials || []).some(credential => credential.enabled !== false && !items.some(item => item.credential.id === credential.id))) {
      setError('启用渠道必须保留至少一个启用凭证；如需全部停用，请先停用渠道。'); return;
    }
    await batch.run(items, enabled ? 'enable' : 'disable');
  }
  function skipConfiguration(item: OAuthBatchTarget, patch: CredentialOperationsPatch) {
    if ('warmup' in patch && patch.warmup?.enabled && !codexHasFiveHourWindow(quota(item))) return '上游未明确提供5小时通用额度，跳过开启自动预热。';
    if ('auto_reset' in patch && patch.auto_reset?.enabled && patch.auto_reset.rules.some(rule => rule.remaining_below != null && !codexGeneralWindows(quota(item)).includes(rule.window_seconds))) return '当前通用额度未提供所选窗口，跳过开启自动重置。';
  }
  async function save(retry = false) {
    let patch = savedPatch.current;
    if (!retry) {
      const validation = view === 'auto-warmup' ? validateAutomaticWarmup(warmup, channel.models, someFiveHours) : validateAutomaticReset(autoReset);
      if (validation.error || !validation.value) { setError(validation.error || '请检查自动配置。'); return false; }
      patch = view === 'auto-warmup' ? { warmup: validation.value as CredentialOperationsConfig['warmup'] } : { auto_reset: validation.value as CredentialOperationsConfig['auto_reset'] };
      savedPatch.current = patch;
    }
    if (!patch) return false;
    const targets = retry ? actionItems.filter(item => batch.results.some(result => result.key === quotaTargetKey(item.target) && result.state === 'failed')) : actionItems;
    setError('');
    const outcome = await batch.run(targets, 'config', { patch, skip: item => skipConfiguration(item, patch!), retry });
    // Failed targets retain the edited draft and successful target receipts.
    dirty.current = !outcome || outcome.some(result => result.state !== 'succeeded');
    return !dirty.current;
  }
  const resultList = batch.results.length > 0 && <ul className="oauth-batch-results" aria-label="批量操作结果" aria-live="polite">{batch.results.map((result, index) => <li key={result.key}><strong>{result.name}</strong><span className={`is-${result.state}`}>{labels[result.state]}</span><p>{result.detail}</p><span className="sr-only">第 {index + 1} 项</span></li>)}</ul>;
  return <div className="oauth-credential-batch">
    <div className="oauth-batch-toolbar" aria-label={`${channel.name} 批量凭证操作`}><span role="status">已选 {items.length} / {channel.credentials?.length || 0} 个凭证</span><Button variant="ghost" disabled={busy} onClick={clear}>清除选择</Button><div>{transferable && <Button variant="secondary" disabled={busy || !items.length} onClick={() => setExportItems([...items])}>导出凭证</Button>}<Button variant="secondary" disabled={busy} onClick={() => void enable(true)}>启用</Button><Button variant="secondary" disabled={busy} onClick={() => void enable(false)}>停用</Button>{codex && <><Button variant="secondary" disabled={busy || networkBlocked} onClick={() => { if (!networkPaused.current) batch.refreshItems(items); }}>刷新额度</Button><Button variant="secondary" disabled={busy || networkBlocked} onClick={() => open('warmup')}>预热</Button><Button variant="secondary" disabled={busy} onClick={() => open('auto-warmup')}>自动预热配置</Button><Button variant="secondary" disabled={busy} onClick={() => open('auto-reset')}>自动重置配置</Button></>}</div></div>
    {!view && error && <ErrorBox message={error} />}{!view && resultList}
    {view && <Modal title={view === 'warmup' ? '批量预热' : view === 'auto-warmup' ? '批量自动预热配置' : '批量自动重置配置'} description={`${channel.name} · ${actionItems.length} 个选中凭证`} busy={batch.busy} onClose={close} className="oauth-batch-dialog">
      <div className="oauth-batch-content">{view === 'warmup' ? <><p>将按每个凭证已保存的预热模型，各发送一次短消息 hi，会使用账号额度。</p><p>未配置有效模型、已停用或授权异常的凭证将跳过。结果未确认的操作需到单条入口人工核对。</p><ul className="oauth-batch-targets">{actionItems.map(item => <li key={quotaTargetKey(item.target)}><strong>{item.name}</strong><span>{item.credential.operations?.warmup.model && channel.models.includes(item.credential.operations.warmup.model) ? item.credential.operations.warmup.model : '未配置有效模型，将跳过'}</span></li>)}</ul></> : <><p>以下同一份配置仅应用于当前选中凭证；每项结果分别显示。</p>{view === 'auto-warmup' ? <CodexAutomaticWarmupForm value={warmup} onChange={value => { dirty.current = true; savedPatch.current = undefined; setWarmup(value); setError(''); }} models={channel.models} hasFiveHourWindow={someFiveHours} timeZone={batch.timeZone} disabled={batch.busy} /> : <CodexAutomaticResetForm value={autoReset} onChange={value => { dirty.current = true; savedPatch.current = undefined; setAutoReset(value); setError(''); }} windows={windows} disabled={batch.busy} />}</>}{error && <ErrorBox message={error} />}{resultList}</div>
      <div className="modal-footer"><Button variant="secondary" disabled={batch.busy} onClick={close}>关闭</Button>{view === 'warmup' ? <Button disabled={busy || networkBlocked || batch.results.some(result => result.state === 'succeeded' || result.state === 'unknown')} onClick={() => { if (!networkPaused.current) void batch.run(actionItems, 'warmup', { skip: () => networkPaused.current ? '接入插件未就绪，已暂停预热。' : undefined }); }}>确认各发送一次 hi</Button> : <><Button disabled={busy} onClick={() => void save()}>应用到选中凭证</Button>{savedPatch.current && batch.results.some(result => result.state === 'failed') && <Button variant="secondary" disabled={busy} onClick={() => void save(true)}>重试未完成项</Button>}</>}</div>
    </Modal>}
    {exportItems && transferable && <OAuthTransfer mode="export" refs={exportItems.map(item => ({ channel_id: item.target.channelID, credential_id: item.credential.id }))} labels={exportItems.map(item => item.name)} valid={exportValid} onClose={() => setExportItems(undefined)} onBusy={setTransferBusy} />}
  </div>;
}
