import { useEffect, useRef, useState, type FormEvent } from 'react';
import { AlertTriangle, Archive, Check, Copy, Download, ExternalLink, Eye, EyeOff, KeyRound, Languages, Power, RefreshCw, RotateCcw, Save, Shuffle, Terminal, Upload } from 'lucide-react';
import { api, errorMessage, type Settings as SettingsData } from '../api';
import { useResource } from '../hooks';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { ModelDefaultsEditor } from '../components/ModelDefaultsEditor';
import { AppUpdatePanel } from '../components/AppUpdates';
import { restartConsole } from '../restart';
import { Button, ErrorBox, Field, Input, Loading, Modal, useToast } from '../components/ui';
import { useI18n } from '../i18n';
import './Settings.css';

interface FullConfigSummary {
  channels_count: number;
  downstream_models_count: number;
  client_keys_count: number;
  plan_prices_count: number;
  tags_count: number;
  exported_at: string;
  version: number;
}

const normalize = (value: SettingsData): SettingsData => ({ ...value, failure_cooldown_seconds: value.failure_cooldown_seconds ?? 10, quota_refresh_seconds: value.quota_refresh_seconds ?? 300, reasoning_policy: value.reasoning_policy ?? 'preserve' });
export default function Settings() {
  const { t, locale, setLocale } = useI18n();
  const resource = useResource<SettingsData>('/settings');
  const runtime = useResource<{ port: number; origin: string }>('/runtime/port');
  const [form, setForm] = useState<SettingsData | null>(null);
  const [savedSettings, setSavedSettings] = useState<SettingsData | null>(null);
  const savedSettingsRef = useRef<SettingsData | null>(null);
  const [showKey, setShowKey] = useState(false);
  const [copied, setCopied] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [noticeError, setNoticeError] = useState('');
  const [confirm, setConfirm] = useState<'restart' | 'shutdown' | null>(null);
  const [portDraft, setPortDraft] = useState('');
  const [portBusy, setPortBusy] = useState(false);
  const [exitSaving, setExitSaving] = useState(false);
  const [savedPort, setSavedPort] = useState<number | null>(null);
  const savedPortRef = useRef<number | null>(null);
  const [portSaved, setPortSaved] = useState<number | null>(null);
  const [portError, setPortError] = useState('');
  const [controlBusy, setControlBusy] = useState(false);
  const [controlProgress, setControlProgress] = useState('');
  const [controlError, setControlError] = useState('');
  const [controlPort, setControlPort] = useState<number | null>(null);
  const [preparingControl, setPreparingControl] = useState(false);
  const [exportPassword, setExportPassword] = useState('');
  const [showExportPassword, setShowExportPassword] = useState(false);
  const [exportBusy, setExportBusy] = useState(false);
  const [exportError, setExportError] = useState('');
  const [restoreFileText, setRestoreFileText] = useState('');
  const [restoreFileName, setRestoreFileName] = useState('');
  const [restoreFileReading, setRestoreFileReading] = useState(false);
  const restoreFileGeneration = useRef(0);
  const restoreRevision = useRef(0);
  const previewGeneration = useRef(0);
  const [restorePassword, setRestorePassword] = useState('');
  const [showRestorePassword, setShowRestorePassword] = useState(false);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState('');
  const [previewSummary, setPreviewSummary] = useState<FullConfigSummary | null>(null);
  const [restoreConfirm, setRestoreConfirm] = useState<number | null>(null);
  const [restoreBusy, setRestoreBusy] = useState(false);
  const [restoreProgress, setRestoreProgress] = useState('');
  const [restoreError, setRestoreError] = useState('');
  const [rollbackSnapshot, setRollbackSnapshot] = useState('');
  const [rollbackBusy, setRollbackBusy] = useState(false);
  const [rollbackProgress, setRollbackProgress] = useState('');
  const [rollbackError, setRollbackError] = useState('');
  const [rollbackConfirm, setRollbackConfirm] = useState(false);
  const preparingRef = useRef(false);
  const operation = useRef(false);
  const sessionInvalidated = useRef(false);
  const conversionSection = useRef<HTMLElement>(null);
  const toast = useToast();
  const dirty = !!form && !!savedSettings && JSON.stringify(form) !== JSON.stringify(savedSettings);
  const portDirty = savedPort !== null && Number(portDraft) !== savedPort;
  const locked = busy || portBusy || exitSaving || controlBusy || preparingControl || exportBusy || previewBusy || restoreBusy || rollbackBusy;
  const restoreReady = !restoreFileReading && !!restoreFileText && restorePassword.length >= 12;

  useEffect(() => () => {
    // File reads and preview requests may settle after this page has gone away.
    ++restoreFileGeneration.current;
    ++previewGeneration.current;
    ++restoreRevision.current;
  }, []);

  useEffect(() => {
    let alive = true;
    api<{ available: boolean; snapshot: string }>('/config-backup/rollback')
      .then(res => {
        if (alive && res?.available && res.snapshot) {
          setRollbackSnapshot(res.snapshot);
        }
      })
      .catch(() => {});
    return () => { alive = false; };
  }, []);

  // Seed each editor once. Reloads after writes must not replace a newer draft
  // or reset the accepted local baseline to an older server snapshot.
  useEffect(() => {
    if (resource.data && !savedSettingsRef.current) {
      const value = normalize(resource.data);
      savedSettingsRef.current = value; setSavedSettings(value); setForm(value);
    }
  }, [resource.data]);
  useEffect(() => {
    if (runtime.data && savedPortRef.current === null) {
      savedPortRef.current = runtime.data.port; setSavedPort(runtime.data.port); setPortDraft(String(runtime.data.port));
    }
  }, [runtime.data]);
  useEffect(() => { if (!copied) return; const timer = setTimeout(() => setCopied(false), 2500); return () => clearTimeout(timer); }, [copied]);
  useEffect(() => {
    if (!form || new URLSearchParams(window.location.hash.split('?')[1]).get('section') !== 'conversion') return;
    conversionSection.current?.scrollIntoView?.({ block: 'start' });
    conversionSection.current?.focus({ preventScroll: true });
  }, [!!form]);
  function update<K extends keyof SettingsData>(key: K, value: SettingsData[K]) { setForm(current => current ? { ...current, [key]: value } : current); }
  async function copy() { if (!form) return; try { await navigator.clipboard.writeText(form.gateway_key); setCopied(true); } catch { setError('无法访问剪贴板，请显示密钥后手动复制。'); } }
  function validPort() {
    const next = Number(portDraft);
    if (!Number.isInteger(next) || next < 1 || next > 65535) { setPortError('端口必须是 1..65535 的整数。'); return false; }
    return true;
  }
  function validSettings() {
    if (!form) return false;
    if (!form.gateway_key.trim()) { setError('管理员登录密钥不能为空。'); return false; }
    if (!Number.isInteger(form.failure_cooldown_seconds) || form.failure_cooldown_seconds < 1 || form.failure_cooldown_seconds > 300) { setError('失败冷却时间须为 1..300 秒的整数。'); return false; }
    const period = form.quota_refresh_seconds ?? 300;
    if (!Number.isInteger(period) || (period !== 0 && (period < 30 || period > 86400))) { setError('额度刷新周期须为 0（仅手动）或 30..86400 秒的整数。'); return false; }
    const element = document.getElementById('settings-form') as HTMLFormElement | null;
    if (element && !element.checkValidity()) { element.reportValidity(); return false; }
    return true;
  }
  async function persistPort() {
    setPortBusy(true); setPortError('');
    try {
      const result = await api<{ port: number; restart_required: boolean }>('/runtime/port', { method: 'PUT', body: JSON.stringify({ port: Number(portDraft) }) });
      savedPortRef.current = result.port; setSavedPort(result.port); setPortDraft(String(result.port)); setPortSaved(result.port);
      toast(`端口已设置为 ${result.port}，重启后生效。`); runtime.reload(); return true;
    } catch (e) { setPortError(errorMessage(e)); return false; }
    finally { setPortBusy(false); }
  }
  async function savePort() {
    if (operation.current || !portDirty || !validPort()) return false;
    operation.current = true;
    try { return await persistPort(); } finally { operation.current = false; }
  }
  async function persistSettings() {
    if (!form) return false;
    setBusy(true); setError('');
    const accepted = { ...form, gateway_key: form.gateway_key.trim() };
    try {
      const { prices: _prices, default_launch_mode: _launchMode, ...settings } = accepted;
      await api('/settings', { method: 'PUT', body: JSON.stringify(settings) });
      window.dispatchEvent(new Event('quota-settings-changed'));
      const keyChanged = accepted.gateway_key !== savedSettingsRef.current?.gateway_key;
      savedSettingsRef.current = accepted; setSavedSettings(accepted); setForm(accepted);
      if (keyChanged) {
        sessionInvalidated.current = true; setForm(null);
        toast('管理员登录密钥已更改，所有登录会话已失效，请重新登录。');
        window.dispatchEvent(new Event('auth-expired')); return true;
      }
      toast('运行与登录设置已保存'); resource.reload(); return true;
    } catch (e) { setError(errorMessage(e)); return false; }
    finally { setBusy(false); }
  }
  async function saveAll(event?: FormEvent) {
    event?.preventDefault();
    if (operation.current) return false;
    setError(''); setPortError('');
    if ((portDirty && !validPort()) || (dirty && !validSettings())) return false;
    operation.current = true; setExitSaving(true);
    try {
      // The port must precede settings because changing the admin key expires this session.
      if (portDirty && !await persistPort()) return false;
      return !dirty || await persistSettings();
    } finally { operation.current = false; setExitSaving(false); }
  }
  const canLeave = useUnsavedChanges({ dirty: dirty || portDirty, busy: busy || portBusy || exitSaving,
    save: () => saveAll(), discard: () => {
      setForm(savedSettingsRef.current); setPortDraft(savedPortRef.current === null ? '' : String(savedPortRef.current)); setError(''); setPortError('');
    },
  });
  async function requestControl(action: 'restart' | 'shutdown') {
    if (operation.current || preparingRef.current || sessionInvalidated.current) return;
    preparingRef.current = true; setPreparingControl(true);
    try {
      if (((dirty || portDirty) && !await canLeave()) || sessionInvalidated.current) return;
      setControlPort(savedPortRef.current); setControlProgress(''); setControlError(''); setConfirm(action);
    } finally { preparingRef.current = false; setPreparingControl(false); }
  }
  async function runControl(action: 'restart' | 'shutdown') {
    if (operation.current || sessionInvalidated.current) return;
    operation.current = true; setControlBusy(true); setControlError(''); setControlProgress('');
    if (action === 'restart') {
      try {
        const target = await restartConsole(setControlProgress);
        setControlProgress('服务已恢复，正在转到新页面…');
        if (new URL(target).origin === window.location.origin) { window.history.replaceState(null, '', target); window.location.reload(); }
        else window.location.replace(target);
      } catch (e) { setControlProgress(''); setControlError(errorMessage(e)); setControlBusy(false); operation.current = false; }
      return;
    }
    try { await api('/shutdown', { method: 'POST', body: '{}' }); setConfirm(null); toast('服务正在退出，本页面即将无法访问。'); }
    catch { setControlError('请求未得到确认；请检查服务状态后重试。若已退出，请重新启动 to-router。'); setControlBusy(false); operation.current = false; }
  }

  async function handleExport() {
    if (exportPassword.length < 12) {
      setExportError('备份口令至少需要 12 个字符。');
      return;
    }
    setExportBusy(true);
    setExportError('');
    try {
      const res = await api<{ file: string; filename: string }>('/config-backup/export', {
        method: 'POST',
        body: JSON.stringify({ password: exportPassword }),
      });
      const blob = new Blob([res.file], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = res.filename || `torouter-backup-${new Date().toISOString().slice(0, 10)}.json`;
      a.click();
      URL.revokeObjectURL(url);
      toast('配置已成功加密导出');
      setExportPassword('');
    } catch (e) {
      setExportError(errorMessage(e));
    } finally {
      setExportBusy(false);
    }
  }

  function invalidateRestoreInput() {
    ++restoreRevision.current;
    ++previewGeneration.current;
    setPreviewSummary(null);
    setPreviewError('');
    setPreviewBusy(false);
    setRestoreConfirm(null);
    setRestoreError('');
    setRestoreProgress('');
  }

  function handleRestorePasswordChange(value: string) {
    invalidateRestoreInput();
    setRestorePassword(value);
  }

  function handleFileChange(e: React.ChangeEvent<HTMLInputElement>) {
    const generation = ++restoreFileGeneration.current;
    invalidateRestoreInput();
    setRestoreFileText('');
    const file = e.target.files?.[0];
    setRestoreFileName(file?.name ?? '');
    setRestoreFileReading(!!file);
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      if (generation !== restoreFileGeneration.current) return;
      setRestoreFileText(String(reader.result || ''));
      setRestoreFileReading(false);
    };
    const readFailed = () => {
      if (generation !== restoreFileGeneration.current) return;
      setRestoreFileText('');
      setRestoreFileReading(false);
      setPreviewError('读取文件失败，请重新选择。');
    };
    reader.onerror = readFailed;
    try { reader.readAsText(file); }
    catch { readFailed(); }
  }

  async function handlePreview() {
    if (restoreFileReading || !restoreFileText) {
      setPreviewError('请先选择备份文件。');
      return;
    }
    if (restorePassword.length < 12) {
      setPreviewError('请输入至少 12 个字符的备份口令。');
      return;
    }
    const generation = ++previewGeneration.current;
    setPreviewBusy(true);
    setPreviewError('');
    try {
      const res = await api<{ summary: FullConfigSummary }>('/config-backup/preview', {
        method: 'POST',
        body: JSON.stringify({ file: restoreFileText, password: restorePassword }),
      });
      if (generation === previewGeneration.current) setPreviewSummary(res.summary);
    } catch (e) {
      if (generation === previewGeneration.current) setPreviewError(errorMessage(e));
    } finally {
      if (generation === previewGeneration.current) setPreviewBusy(false);
    }
  }

  async function handleRestore() {
    if (operation.current || !restoreReady || restoreConfirm === null || restoreConfirm !== restoreRevision.current) return;
    operation.current = true;
    setRestoreBusy(true);
    setRestoreError('');
    setRestoreProgress('正在提交恢复并写入配置…');
    try {
      const res = await api<{ ok: boolean; restart_required: boolean; snapshot?: string }>('/config-backup/restore', {
        method: 'POST',
        body: JSON.stringify({ file: restoreFileText, password: restorePassword, confirm: true }),
      });
      if (res?.snapshot) {
        setRollbackSnapshot(res.snapshot);
      }
      const snapNote = res?.snapshot ? `（快照：${res.snapshot}）` : '';
      setRestoreProgress(`恢复成功${snapNote}，正在重启服务以生效全部配置…`);
      const target = await restartConsole(setRestoreProgress);
      setRestoreProgress('服务已恢复，正在转到新页面…');
      if (new URL(target).origin === window.location.origin) {
        window.history.replaceState(null, '', target);
        window.location.reload();
      } else {
        window.location.replace(target);
      }
    } catch (e) {
      setRestoreProgress('');
      setRestoreError(errorMessage(e));
      setRestoreBusy(false);
      operation.current = false;
    }
  }

  async function handleRollback() {
    if (operation.current) return;
    operation.current = true;
    setRollbackBusy(true);
    setRollbackError('');
    setRollbackProgress('正在提交回退…');
    try {
      await api('/config-backup/rollback', {
        method: 'POST',
        body: JSON.stringify({ snapshot: rollbackSnapshot, confirm: true }),
      });
      setRollbackProgress('回退成功，正在重启服务以生效全部配置…');
      const target = await restartConsole(setRollbackProgress);
      setRollbackProgress('服务已恢复，正在转到新页面…');
      if (new URL(target).origin === window.location.origin) {
        window.history.replaceState(null, '', target);
        window.location.reload();
      } else {
        window.location.replace(target);
      }
    } catch (e) {
      setRollbackProgress('');
      setRollbackError(errorMessage(e));
      setRollbackBusy(false);
      operation.current = false;
    }
  }
  const saveDisabled = locked || (!dirty && !portDirty) || !form;
  const settingsStatus = dirty ? t('settings.dirty') : t('settings.saved');
  const portStatus = savedPort === null ? '端口尚未读取' : portDirty ? '端口有未保存更改' : portSaved !== null ? '端口已保存，等待重启生效' : '端口无未保存更改';
  const noticesURL = `${(window.__TO_ROUTER_DESKTOP__?.origin ?? window.location.origin).replace(/\/$/, '')}/third-party/notices.html`;
  return <div className="system-settings">
    <header className="settings-page-heading"><div><h1>{t('settings.title')}</h1><p>{t('settings.subtitle')}</p></div><Button type="submit" form="settings-form" disabled={saveDisabled}><Save size={16} />{busy || exitSaving ? t('common.saving') : t('settings.saveSettings')}</Button></header>
    <AppUpdatePanel />
    <section className="panel settings-section settings-preferences"><div className="section-intro"><div className="section-icon"><Languages size={19} /></div><div><h2>{t('settings.preferencesTitle')}</h2><p>{t('settings.preferencesDesc')}</p></div></div><div className="settings-body">
      <div className="settings-reasoning">
        <div>
          <p id="locale-switch-label" className="font-medium text-sm">{t('settings.interfaceLanguage')}</p>
          <p className="settings-note">{t('settings.interfaceLanguageHint')}</p>
        </div>
        <div className="settings-policy-switch" role="group" aria-labelledby="locale-switch-label">
          <button type="button" aria-pressed={locale === 'zh'} onClick={() => setLocale('zh')}>{t('settings.langZh')}</button>
          <button type="button" aria-pressed={locale === 'en'} onClick={() => setLocale('en')}>{t('settings.langEn')}</button>
        </div>
      </div>
    </div></section>
    <p className="settings-save-scope">{t('settings.saveScope')}</p>
    {resource.error && <ErrorBox message={`读取运行与登录设置失败：${resource.error}`} retry={resource.reload} />}
    {!form && resource.loading ? <Loading /> : form && <form id="settings-form" onSubmit={saveAll} className="settings-form">
      {error && <ErrorBox message={error} />}
      <fieldset disabled={busy || portBusy || exitSaving || controlBusy} className="settings-fields">
        <section className="panel settings-section"><div className="section-intro"><div className="section-icon"><Terminal size={19} /></div><div><h2>{t('settings.runtimeTitle')}</h2><p>{t('settings.runtimeDesc')}</p></div></div><div className="settings-body">
          <div className="settings-runtime-grid">
            <Field label={t('settings.logRetentionDays')} hint={t('settings.logRetentionHint')}><div className="relative"><Input type="number" required min={1} step={1} value={form.retention_days} onChange={e => update('retention_days', Number(e.target.value))} /><span className="input-unit">天</span></div></Field>
            <Field label={t('settings.maxAttempts')} hint={t('settings.maxAttemptsHint')}><div className="relative"><Input type="number" required min={1} step={1} value={form.max_attempts} onChange={e => update('max_attempts', Number(e.target.value))} /><span className="input-unit">次</span></div></Field>
            <Field label={t('settings.retriesPerChannel')} hint={t('settings.retriesPerChannelHint')}><div className="relative"><Input type="number" required min={1} max={10} step={1} value={form.retries_per_channel} onChange={e => update('retries_per_channel', Number(e.target.value))} /><span className="input-unit">次</span></div></Field>
            <Field label={t('settings.failureCooldown')} hint={t('settings.failureCooldownHint')}><div className="relative"><Input type="number" required min={1} max={300} step={1} value={form.failure_cooldown_seconds} onChange={e => update('failure_cooldown_seconds', Number(e.target.value))} /><span className="input-unit">秒</span></div></Field>
            <Field label={t('settings.quotaRefresh')} hint={t('settings.quotaRefreshHint')}><div className="relative"><Input aria-label="额度刷新周期" type="number" required min={0} max={86400} step={1} value={form.quota_refresh_seconds ?? 300} onChange={e => update('quota_refresh_seconds', Number(e.target.value))} /><span className="input-unit">秒</span></div></Field>
            <Field label={t('settings.oauthAutoOpen')} hint={t('settings.oauthAutoOpenHint')}><input type="checkbox" className="checkbox" checked={form.oauth_auto_open !== false} onChange={e => update('oauth_auto_open', e.target.checked)} /></Field>
          </div>
          <details className="settings-details"><summary>{t('settings.rulesSummary')}</summary><div>
            <p>{t('settings.rulesSummaryP1')}</p>
            <p>{t('settings.rulesSummaryP2')}</p>
            <p>{t('settings.rulesSummaryP3')}</p>
            <p>{t('settings.rulesSummaryP4')}</p>
          </div></details>
        </div></section>
        <section ref={conversionSection} id="settings-conversion" tabIndex={-1} aria-label="协议转换" className="panel settings-section settings-conversion"><div className="section-intro"><div className="section-icon"><Shuffle size={19} /></div><div><h2>{t('settings.conversionTitle')}</h2><p>{t('settings.conversionDesc')}</p></div></div><div className="settings-body">
          <div className="settings-reasoning"><p id="reasoning-policy-label">{t('settings.reasoningLabel')}</p><div className="settings-policy-switch" role="group" aria-labelledby="reasoning-policy-label">{([['preserve', t('settings.reasoningPreserve')], ['availability', t('settings.reasoningAvailability')]] as const).map(([value, label]) => <button key={value} type="button" aria-pressed={form.reasoning_policy === value} onClick={() => update('reasoning_policy', value)}>{label}</button>)}</div></div>
          <p className="settings-note">{t('settings.reasoningNote1')}</p>
          <p className="settings-note">{t('settings.reasoningNote2')}</p>
        </div></section>
        <section className="panel settings-section"><div className="section-intro"><div className="section-icon"><KeyRound size={19} /></div><div><h2>{t('settings.adminLoginTitle')}</h2><p>{t('settings.adminLoginDesc')}</p></div></div><div className="settings-body">
          <Field label={t('settings.gatewayKey')} hint={t('settings.gatewayKeyHint')}><div className="settings-key"><Input type={showKey ? 'text' : 'password'} autoComplete="new-password" value={form.gateway_key} required className="min-w-0 flex-1 font-mono" onChange={e => update('gateway_key', e.target.value)} /><Button type="button" variant="secondary" aria-label={showKey ? t('settings.hideKey') : t('settings.showKey')} onClick={() => setShowKey(!showKey)}>{showKey ? <EyeOff size={16} /> : <Eye size={16} />}</Button><Button type="button" variant="secondary" aria-label={t('settings.copyKey')} onClick={copy}>{copied ? <Check size={16} /> : <Copy size={16} />}</Button></div></Field>
        </div></section>
        <section className="panel settings-section"><div className="section-intro"><div className="section-icon"><Terminal size={19} /></div><div><h2>{t('settings.defaultsTitle')}</h2><p>{t('settings.defaultsDesc')}</p></div></div><div className="settings-body">
          <label className="settings-default-toggle"><input aria-label={t('settings.defaultsToggle')} className="checkbox" type="checkbox" checked={form.request_defaults_enabled === true} onChange={event => update('request_defaults_enabled', event.target.checked)} />{t('settings.defaultsToggle')}<span>{form.request_defaults_enabled === true ? t('settings.defaultsEnabled') : t('settings.defaultsDefaultOff')}</span></label>
          <p className="settings-note">{t('settings.defaultsNote')}</p>
          <details className="settings-details" open={form.request_defaults_enabled === true || undefined}><summary>{t('settings.defaultsEdit')}</summary><div><p>{t('settings.defaultsEditDesc')}</p><ModelDefaultsEditor value={form.request_defaults || {}} onChange={value => update('request_defaults', value)} /></div></details>
        </div></section>
      </fieldset>
      <div className="settings-save-bar"><p role="status">{settingsStatus} · {portStatus}</p><Button type="submit" disabled={saveDisabled}><Save size={16} />{busy || exitSaving ? t('common.saving') : t('settings.saveSettings')}</Button></div>
    </form>}
    <section className="panel settings-section settings-backup"><div className="section-intro"><div className="section-icon"><Archive size={19} /></div><div><h2>{t('settings.backupTitle')}</h2><p>{t('settings.backupDesc')}</p></div></div><div className="settings-body">
      <div className="settings-backup-grid">
        <div className="settings-backup-card">
          <h3>{t('settings.exportCardTitle')}</h3>
          <p>{t('settings.exportCardDesc')}</p>
          {exportError && <ErrorBox message={exportError} />}
          <Field label={t('settings.exportPassword')} hint={t('settings.exportPasswordHint')}><div className="settings-key"><Input aria-label="备份加密口令" type={showExportPassword ? 'text' : 'password'} value={exportPassword} placeholder="至少 12 个字符" onChange={e => setExportPassword(e.target.value)} /><Button type="button" variant="secondary" aria-label={showExportPassword ? '隐藏导出密码' : '显示导出密码'} onClick={() => setShowExportPassword(!showExportPassword)}>{showExportPassword ? <EyeOff size={16} /> : <Eye size={16} />}</Button></div></Field>
          <div className="settings-backup-actions">
            <Button type="button" variant="secondary" disabled={locked || exportBusy || exportPassword.length < 12} onClick={() => void handleExport()}><Download size={16} />{exportBusy ? t('common.saving') : t('settings.exportButton')}</Button>
          </div>
        </div>
        <div className="settings-backup-card">
          <h3>{t('settings.restoreCardTitle')}</h3>
          <p>{t('settings.restoreCardDesc')}</p>
          {previewError && <ErrorBox message={previewError} />}
          <Field label={t('settings.restoreFile')} hint={restoreFileName ? `${t('settings.restoreFileSelected')}：${restoreFileName}` : t('settings.restoreFileHint')}><Input aria-label="选择备份文件" type="file" accept=".json,application/json" disabled={restoreBusy} onChange={handleFileChange} /></Field>
          <Field label={t('settings.restorePassword')} hint={t('settings.restorePasswordHint')}><div className="settings-key"><Input aria-label="解密口令" type={showRestorePassword ? 'text' : 'password'} value={restorePassword} placeholder="输入备份口令" disabled={restoreBusy} onChange={e => handleRestorePasswordChange(e.target.value)} /><Button type="button" variant="secondary" aria-label={showRestorePassword ? '隐藏解密密码' : '显示解密密码'} onClick={() => setShowRestorePassword(!showRestorePassword)}>{showRestorePassword ? <EyeOff size={16} /> : <Eye size={16} />}</Button></div></Field>
          {previewSummary && <div className="settings-backup-summary" role="status" aria-label="备份概况">
            <strong>{t('settings.previewSummaryTitle', { version: previewSummary.version })}</strong>
            <ul>
              <li>{t('settings.previewChannels', { count: previewSummary.channels_count })}</li>
              <li>{t('settings.previewModels', { count: previewSummary.downstream_models_count })}</li>
              <li>{t('settings.previewKeys', { count: previewSummary.client_keys_count })}</li>
              <li>{t('settings.previewPrices', { count: previewSummary.plan_prices_count })}</li>
              <li>{t('settings.previewTags', { count: previewSummary.tags_count })}</li>
              <li>{t('settings.previewExportTime', { time: new Date(previewSummary.exported_at).toLocaleString(locale === 'zh' ? 'zh-CN' : 'en-US') })}</li>
            </ul>
          </div>}
          <div className="settings-backup-actions">
            <Button type="button" variant="secondary" disabled={locked || !restoreReady} onClick={() => void handlePreview()}><Eye size={16} />{previewBusy ? t('settings.parsing') : t('settings.previewButton')}</Button>
            <Button type="button" variant="primary" disabled={locked || !restoreReady} onClick={() => { if (restoreReady) setRestoreConfirm(restoreRevision.current); }}><Upload size={16} />{t('settings.restoreButton')}</Button>
          </div>
        </div>
        {rollbackSnapshot && <div className="settings-backup-card settings-rollback-card">
          <h3>{t('settings.rollbackCardTitle')}</h3>
          <p>{t('settings.rollbackCardDesc')}</p>
          {rollbackError && <ErrorBox message={rollbackError} />}
          <div className="settings-note" role="status">
            <span>{t('settings.latestSnapshot')}</span><code className="font-mono text-xs">{rollbackSnapshot}</code>
          </div>
          <div className="settings-backup-actions">
            <Button type="button" variant="danger" disabled={locked || rollbackBusy} onClick={() => setRollbackConfirm(true)}>
              <RotateCcw size={16} />{rollbackBusy ? t('settings.rollingBack') : t('settings.rollbackButton')}
            </Button>
          </div>
        </div>}
      </div>
    </div></section>
    <section className="panel settings-section settings-service"><div className="section-intro"><div className="section-icon"><Power size={19} /></div><div><h2>{t('settings.serviceTitle')}</h2><p>{t('settings.serviceDesc')}</p></div></div><div className="settings-body">
      {runtime.error && <ErrorBox message={`读取监听端口失败：${runtime.error}`} retry={runtime.reload} />}
      {portError && <ErrorBox message={portError} />}
      <Field label={t('settings.portLabel')} hint={runtime.data?.origin ? t('settings.portHintActual', { origin: runtime.data.origin }) : savedPort === null ? t('settings.portHintWaiting') : t('settings.portHintRestartRequired')}><div className="settings-port"><Input aria-label="监听端口" disabled={locked || savedPort === null} type="number" min={1} max={65535} step={1} value={portDraft} onChange={e => setPortDraft(e.target.value)} /><Button type="button" variant="secondary" disabled={locked || !portDirty || savedPort === null} onClick={() => void savePort()}>{portBusy ? t('settings.savingPort') : t('settings.savePort')}</Button></div></Field>
      <p className="settings-note" role="status">{portSaved !== null ? t('settings.portNoteSaved', { port: portSaved }) : t('settings.portNoteUnsaved')}{portDirty && <strong>{t('settings.portNoteDirty')}</strong>}</p>
      <details className="settings-details"><summary>{t('settings.portExplanationSummary')}</summary><div>
        <p>{t('settings.portExplanationP1')}</p>
        <p>{t('settings.portExplanationP2')}</p>
      </div></details>
      {!confirm && controlError && <ErrorBox message={controlError} />}
      <div className="settings-controls"><Button type="button" variant="secondary" disabled={locked || savedPort === null || !form} onClick={() => void requestControl('restart')}><RefreshCw size={16} />{t('settings.restartService')}</Button><Button type="button" variant="secondary" disabled={locked || !form} onClick={() => void requestControl('shutdown')}><Power size={16} />{t('settings.shutdownService')}</Button></div>
    </div></section>
    {confirm && <Modal title={confirm === 'restart' ? t('settings.restartModalTitle') : t('settings.shutdownModalTitle')} description={confirm === 'restart' ? t('settings.restartModalDesc', { port: controlPort ?? '' }) : t('settings.shutdownModalDesc')} onClose={() => setConfirm(null)} busy={controlBusy}>
      {controlProgress && <p role="status" className="settings-note">{controlProgress}</p>}{controlError && <ErrorBox message={controlError} />}
      <div className="modal-footer"><Button type="button" variant="secondary" disabled={controlBusy} onClick={() => setConfirm(null)}>{t('common.cancel')}</Button><Button type="button" variant={confirm === 'shutdown' ? 'danger' : 'primary'} disabled={controlBusy} onClick={() => void runControl(confirm)}><AlertTriangle size={16} />{controlBusy ? t('settings.processing') : confirm === 'restart' ? t('settings.confirmRestart') : t('settings.confirmShutdown')}</Button></div>
    </Modal>}
    {restoreConfirm !== null && <Modal title={t('settings.restoreModalTitle')} description={t('settings.restoreModalDesc')} onClose={() => !restoreBusy && setRestoreConfirm(null)} busy={restoreBusy}>
      {restoreProgress && <p role="status" className="settings-note">{restoreProgress}</p>}
      {restoreError && <ErrorBox message={restoreError} />}
      <div className="modal-footer">
        <Button type="button" variant="secondary" disabled={restoreBusy} onClick={() => setRestoreConfirm(null)}>{t('common.cancel')}</Button>
        <Button type="button" variant="danger" disabled={restoreBusy || !restoreReady || restoreConfirm !== restoreRevision.current} onClick={() => void handleRestore()}><AlertTriangle size={16} />{restoreBusy ? t('settings.processing') : t('settings.confirmRestore')}</Button>
      </div>
    </Modal>}
    {rollbackConfirm && <Modal title={t('settings.rollbackModalTitle')} description={t('settings.rollbackModalDesc', { snapshot: rollbackSnapshot ?? '' })} onClose={() => !rollbackBusy && setRollbackConfirm(false)} busy={rollbackBusy}>
      {rollbackProgress && <p role="status" className="settings-note">{rollbackProgress}</p>}
      {rollbackError && <ErrorBox message={rollbackError} />}
      <div className="modal-footer">
        <Button type="button" variant="secondary" disabled={rollbackBusy} onClick={() => setRollbackConfirm(false)}>{t('common.cancel')}</Button>
        <Button type="button" variant="danger" disabled={rollbackBusy} onClick={() => void handleRollback()}><AlertTriangle size={16} />{rollbackBusy ? t('settings.processing') : t('settings.confirmRollback')}</Button>
      </div>
    </Modal>}
    <footer className="settings-legal"><a href={noticesURL} target="_blank" rel="noopener noreferrer" onClick={event => {
      if (!window.__TO_ROUTER_DESKTOP__) return;
      event.preventDefault();
      try {
        if (!window.runtime?.BrowserOpenURL) throw new Error('browser unavailable');
        window.runtime.BrowserOpenURL(noticesURL);
        setNoticeError('');
      } catch { setNoticeError('暂时无法打开浏览器，请稍后重试。'); }
    }}>{t('settings.thirdPartyNotices')}<ExternalLink size={14} /></a>{noticeError && <span role="alert">{noticeError}</span>}</footer>
  </div>;
}
