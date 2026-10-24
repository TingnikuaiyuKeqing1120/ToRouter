import { useEffect, useRef, useState, type FormEvent } from 'react';
import { AlertTriangle, Archive, Check, Copy, Download, ExternalLink, Eye, EyeOff, KeyRound, Power, RefreshCw, RotateCcw, Save, Shuffle, Terminal, Upload } from 'lucide-react';
import { api, errorMessage, type Settings as SettingsData } from '../api';
import { useResource } from '../hooks';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { ModelDefaultsEditor } from '../components/ModelDefaultsEditor';
import { AppUpdatePanel } from '../components/AppUpdates';
import { restartConsole } from '../restart';
import { Button, ErrorBox, Field, Input, Loading, Modal, useToast } from '../components/ui';
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
  const [restorePassword, setRestorePassword] = useState('');
  const [showRestorePassword, setShowRestorePassword] = useState(false);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState('');
  const [previewSummary, setPreviewSummary] = useState<FullConfigSummary | null>(null);
  const [restoreConfirm, setRestoreConfirm] = useState(false);
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

  function handleFileChange(e: React.ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    if (!file) return;
    setRestoreFileName(file.name);
    setPreviewSummary(null);
    setPreviewError('');
    const reader = new FileReader();
    reader.onload = () => {
      setRestoreFileText(String(reader.result || ''));
    };
    reader.onerror = () => {
      setPreviewError('读取文件失败，请重新选择。');
    };
    reader.readAsText(file);
  }

  async function handlePreview() {
    if (!restoreFileText) {
      setPreviewError('请先选择备份文件。');
      return;
    }
    if (restorePassword.length < 12) {
      setPreviewError('请输入至少 12 个字符的备份口令。');
      return;
    }
    setPreviewBusy(true);
    setPreviewError('');
    try {
      const res = await api<{ summary: FullConfigSummary }>('/config-backup/preview', {
        method: 'POST',
        body: JSON.stringify({ file: restoreFileText, password: restorePassword }),
      });
      setPreviewSummary(res.summary);
    } catch (e) {
      setPreviewError(errorMessage(e));
    } finally {
      setPreviewBusy(false);
    }
  }

  async function handleRestore() {
    if (operation.current) return;
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
  const settingsStatus = dirty ? '运行与登录设置有未保存更改' : '运行与登录设置已保存';
  const portStatus = savedPort === null ? '端口尚未读取' : portDirty ? '端口有未保存更改' : portSaved !== null ? '端口已保存，等待重启生效' : '端口无未保存更改';
  const noticesURL = `${(window.__TO_ROUTER_DESKTOP__?.origin ?? window.location.origin).replace(/\/$/, '')}/third-party/notices.html`;
  return <div className="system-settings">
    <header className="settings-page-heading"><div><h1>系统设置</h1><p>运行策略、管理员登录与服务端口</p></div><Button type="submit" form="settings-form" disabled={saveDisabled}><Save size={16} />{busy || exitSaving ? '正在保存…' : '保存设置'}</Button></header>
    <AppUpdatePanel />
    <p className="settings-save-scope">保存设置会依次保存有改动的端口和运行／登录设置。</p>
    {resource.error && <ErrorBox message={`读取运行与登录设置失败：${resource.error}`} retry={resource.reload} />}
    {!form && resource.loading ? <Loading /> : form && <form id="settings-form" onSubmit={saveAll} className="settings-form">
      {error && <ErrorBox message={error} />}
      <fieldset disabled={busy || portBusy || exitSaving || controlBusy} className="settings-fields">
        <section className="panel settings-section"><div className="section-intro"><div className="section-icon"><Terminal size={19} /></div><div><h2>运行策略</h2><p>请求尝试、日志和额度刷新</p></div></div><div className="settings-body">
          <div className="settings-runtime-grid">
            <Field label="日志保留天数" hint="超过期限的历史记录将被清理。"><div className="relative"><Input type="number" required min={1} step={1} value={form.retention_days} onChange={e => update('retention_days', Number(e.target.value))} /><span className="input-unit">天</span></div></Field>
            <Field label="最大尝试次数" hint="单次请求可尝试的渠道上限，含首次。"><div className="relative"><Input type="number" required min={1} step={1} value={form.max_attempts} onChange={e => update('max_attempts', Number(e.target.value))} /><span className="input-unit">次</span></div></Field>
            <Field label="单渠道尝试次数" hint="含首次；范围 1..10 次。"><div className="relative"><Input type="number" required min={1} max={10} step={1} value={form.retries_per_channel} onChange={e => update('retries_per_channel', Number(e.target.value))} /><span className="input-unit">次</span></div></Field>
            <Field label="失败冷却时间" hint="默认 10 秒；范围 1..300 秒。"><div className="relative"><Input type="number" required min={1} max={300} step={1} value={form.failure_cooldown_seconds} onChange={e => update('failure_cooldown_seconds', Number(e.target.value))} /><span className="input-unit">秒</span></div></Field>
            <Field label="额度刷新周期" hint="0 为仅手动；自动刷新 30..86400 秒。"><div className="relative"><Input aria-label="额度刷新周期" type="number" required min={0} max={86400} step={1} value={form.quota_refresh_seconds ?? 300} onChange={e => update('quota_refresh_seconds', Number(e.target.value))} /><span className="input-unit">秒</span></div></Field>
            <Field label="授权时自动打开浏览器" hint="关闭后可复制授权链接到浏览器登录。"><input type="checkbox" className="checkbox" checked={form.oauth_auto_open !== false} onChange={e => update('oauth_auto_open', e.target.checked)} /></Field>
          </div>
          <details className="settings-details"><summary>重试与刷新规则</summary><div>
            <p>同渠道内重试不另占渠道次数；状态归属、禁用回退等规则可能进一步限制可尝试渠道。</p>
            <p>仅对 429/503、连接拒绝和 DNS 失败等确认可安全重试的错误重试，耗尽后才按路由规则换渠道。</p>
            <p>读超时、连接复位、408、500/502/504 可能已执行，不自动重发；上游已成功应答后也不重发。</p>
            <p>失败冷却每次翻倍，上限 5 分钟；上游有效等待提示优先。额度在页面或路由选择需要时按需刷新，默认周期 300 秒。</p>
          </div></details>
        </div></section>
        <section ref={conversionSection} id="settings-conversion" tabIndex={-1} aria-label="协议转换" className="panel settings-section settings-conversion"><div className="section-intro"><div className="section-icon"><Shuffle size={19} /></div><div><h2>协议转换</h2><p>思考状态与续接</p></div></div><div className="settings-body">
          <div className="settings-reasoning"><p id="reasoning-policy-label">思考状态处理</p><div className="settings-policy-switch" role="group" aria-labelledby="reasoning-policy-label">{([['preserve', '稳定优先'], ['availability', '可用优先']] as const).map(([value, label]) => <button key={value} type="button" aria-pressed={form.reasoning_policy === value} onClick={() => update('reasoning_policy', value)}>{label}</button>)}</div></div>
          <p className="settings-note">默认尽量保留思考状态。明确因思考不兼容被拒绝时，可用优先允许剥离对应思考状态重试一次；无法表达时也可按此策略处理。正文和工具历史保持完整。</p>
          <p className="settings-note">可能影响思考连续性和缓存收益；保存后仅对后续请求生效。</p>
        </div></section>
        <section className="panel settings-section"><div className="section-intro"><div className="section-icon"><KeyRound size={19} /></div><div><h2>管理员登录</h2><p>控制台管理凭证</p></div></div><div className="settings-body">
          <Field label="管理员登录密钥" hint="更改并保存后，所有管理员会话立即失效，需使用新密钥重新登录。客户端 API Key 不受影响。"><div className="settings-key"><Input type={showKey ? 'text' : 'password'} autoComplete="new-password" value={form.gateway_key} required className="min-w-0 flex-1 font-mono" onChange={e => update('gateway_key', e.target.value)} /><Button type="button" variant="secondary" aria-label={showKey ? '隐藏密钥' : '显示密钥'} onClick={() => setShowKey(!showKey)}>{showKey ? <EyeOff size={16} /> : <Eye size={16} />}</Button><Button type="button" variant="secondary" aria-label="复制密钥" onClick={copy}>{copied ? <Check size={16} /> : <Copy size={16} />}</Button></div></Field>
        </div></section>
        <section className="panel settings-section"><div className="section-intro"><div className="section-icon"><Terminal size={19} /></div><div><h2>全局请求兜底</h2><p>仅补充客户端未提供的参数</p></div></div><div className="settings-body">
          <label className="settings-default-toggle"><input aria-label="启用请求兜底参数" className="checkbox" type="checkbox" checked={form.request_defaults_enabled === true} onChange={event => update('request_defaults_enabled', event.target.checked)} />启用请求兜底参数<span>{form.request_defaults_enabled === true ? '已开启' : '默认关闭'}</span></label>
          <p className="settings-note">客户端明确提供的值始终优先；具体模型规格在<a href="#/model-specs">参数配置</a>中管理。</p>
          <details className="settings-details" open={form.request_defaults_enabled === true || undefined}><summary>编辑全局缺省值</summary><div><p>关闭时不注入输出预算、温度或思考值；只有具体上游模型支持时才补充，已填写的值会保留。</p><ModelDefaultsEditor value={form.request_defaults || {}} onChange={value => update('request_defaults', value)} /></div></details>
        </div></section>
      </fieldset>
      <div className="settings-save-bar"><p role="status">{settingsStatus} · {portStatus}</p><Button type="submit" disabled={saveDisabled}><Save size={16} />{busy || exitSaving ? '正在保存…' : '保存设置'}</Button></div>
    </form>}
    <section className="panel settings-section settings-backup"><div className="section-intro"><div className="section-icon"><Archive size={19} /></div><div><h2>配置备份与恢复</h2><p>完整系统配置的加密导出与恢复</p></div></div><div className="settings-body">
      <div className="settings-backup-grid">
        <div className="settings-backup-card">
          <h3>导出全部配置</h3>
          <p>将当前渠道、凭证、模型、价格、规则与客户端 Key 导出为加密文件。管理员登录密钥与历史账单不包含在备份中。</p>
          {exportError && <ErrorBox message={exportError} />}
          <Field label="备份加密口令" hint="口令须为 12..256 字符，用于加密备份文件。"><div className="settings-key"><Input aria-label="备份加密口令" type={showExportPassword ? 'text' : 'password'} value={exportPassword} placeholder="至少 12 个字符" onChange={e => setExportPassword(e.target.value)} /><Button type="button" variant="secondary" aria-label={showExportPassword ? '隐藏导出密码' : '显示导出密码'} onClick={() => setShowExportPassword(!showExportPassword)}>{showExportPassword ? <EyeOff size={16} /> : <Eye size={16} />}</Button></div></Field>
          <div className="settings-backup-actions">
            <Button type="button" variant="secondary" disabled={locked || exportBusy || exportPassword.length < 12} onClick={() => void handleExport()}><Download size={16} />{exportBusy ? '正在导出…' : '导出全部配置'}</Button>
          </div>
        </div>
        <div className="settings-backup-card">
          <h3>从备份恢复</h3>
          <p>上传已导出的备份文件并输入口令。可先预览配置概况，确认后覆盖恢复并自动重启服务。</p>
          {previewError && <ErrorBox message={previewError} />}
          <Field label="备份文件" hint={restoreFileName ? `已选文件：${restoreFileName}` : '请选择 .json 格式的备份文件'}><Input aria-label="选择备份文件" type="file" accept=".json,application/json" onChange={handleFileChange} /></Field>
          <Field label="解密口令" hint="输入导出时设置的口令。"><div className="settings-key"><Input aria-label="解密口令" type={showRestorePassword ? 'text' : 'password'} value={restorePassword} placeholder="输入备份口令" onChange={e => setRestorePassword(e.target.value)} /><Button type="button" variant="secondary" aria-label={showRestorePassword ? '隐藏解密密码' : '显示解密密码'} onClick={() => setShowRestorePassword(!showRestorePassword)}>{showRestorePassword ? <EyeOff size={16} /> : <Eye size={16} />}</Button></div></Field>
          {previewSummary && <div className="settings-backup-summary" role="status" aria-label="备份概况">
            <strong>备份概况（版本 {previewSummary.version}）</strong>
            <ul>
              <li>渠道：{previewSummary.channels_count} 个</li>
              <li>模型：{previewSummary.downstream_models_count} 个</li>
              <li>客户端 Key：{previewSummary.client_keys_count} 个</li>
              <li>套餐价格：{previewSummary.plan_prices_count} 个</li>
              <li>标签：{previewSummary.tags_count} 个</li>
              <li>导出时间：{new Date(previewSummary.exported_at).toLocaleString()}</li>
            </ul>
          </div>}
          <div className="settings-backup-actions">
            <Button type="button" variant="secondary" disabled={locked || previewBusy || !restoreFileText || restorePassword.length < 12} onClick={() => void handlePreview()}><Eye size={16} />{previewBusy ? '正在解析…' : '预览配置'}</Button>
            <Button type="button" variant="primary" disabled={locked || !restoreFileText || restorePassword.length < 12} onClick={() => setRestoreConfirm(true)}><Upload size={16} />从备份恢复</Button>
          </div>
        </div>
        {rollbackSnapshot && <div className="settings-backup-card settings-rollback-card">
          <h3>回退到恢复前快照</h3>
          <p>系统在上次恢复前自动生成了安全快照。如果恢复后的新配置不符合预期，可随时回退。</p>
          {rollbackError && <ErrorBox message={rollbackError} />}
          <div className="settings-note" role="status">
            <span>最近快照：</span><code className="font-mono text-xs">{rollbackSnapshot}</code>
          </div>
          <div className="settings-backup-actions">
            <Button type="button" variant="danger" disabled={locked || rollbackBusy} onClick={() => setRollbackConfirm(true)}>
              <RotateCcw size={16} />{rollbackBusy ? '正在回退…' : '回退到此快照'}
            </Button>
          </div>
        </div>}
      </div>
    </div></section>
    <section className="panel settings-section settings-service"><div className="section-intro"><div className="section-icon"><Power size={19} /></div><div><h2>服务控制</h2><p>监听端口与重启、退出</p></div></div><div className="settings-body">
      {runtime.error && <ErrorBox message={`读取监听端口失败：${runtime.error}`} retry={runtime.reload} />}
      {portError && <ErrorBox message={portError} />}
      <Field label="监听端口" hint={runtime.data?.origin ? `当前实际地址 ${runtime.data.origin}` : savedPort === null ? '尚未取得当前端口，请等待或重试读取。' : '修改后需重启生效。'}><div className="settings-port"><Input aria-label="监听端口" disabled={locked || savedPort === null} type="number" min={1} max={65535} step={1} value={portDraft} onChange={e => setPortDraft(e.target.value)} /><Button type="button" variant="secondary" disabled={locked || !portDirty || savedPort === null} onClick={() => void savePort()}>{portBusy ? '保存中…' : '保存端口'}</Button></div></Field>
      <p className="settings-note" role="status">{portSaved !== null ? `已保存端口 ${portSaved}，重启后生效。` : '端口改动只在保存并重启后生效。'}{portDirty && <strong> 当前输入尚未保存。</strong>}</p>
      <details className="settings-details"><summary>端口生效与服务操作说明</summary><div>
        <p>端口优先级：启动参数 -port ＞ 保存的端口 ＞ 默认 17878；手动启动时，启动参数指定的端口会覆盖保存值。控制台重启使用最新保存端口，未配置则沿用当前实际端口。若端口被占用，启动会失败。</p>
        <p>重启和退出都会先停止接收新请求，等待在途请求结束（超时则取消），再关闭数据库；重启还会检查新进程就绪，失败原因写入 to-router.log。</p>
      </div></details>
      {!confirm && controlError && <ErrorBox message={controlError} />}
      <div className="settings-controls"><Button type="button" variant="secondary" disabled={locked || savedPort === null || !form} onClick={() => void requestControl('restart')}><RefreshCw size={16} />重启服务</Button><Button type="button" variant="secondary" disabled={locked || !form} onClick={() => void requestControl('shutdown')}><Power size={16} />退出服务</Button></div>
    </div></section>
    {confirm && <Modal title={confirm === 'restart' ? '重启服务？' : '退出服务？'} description={confirm === 'restart' ? `将使用已保存端口 ${controlPort} 重启。服务会等待在途请求结束，再启动新进程；若启动失败，请查看 to-router.log。` : '服务会等待在途请求结束后退出；随后本页面无法访问，需要重新启动 to-router。'} onClose={() => setConfirm(null)} busy={controlBusy}>
      {controlProgress && <p role="status" className="settings-note">{controlProgress}</p>}{controlError && <ErrorBox message={controlError} />}
      <div className="modal-footer"><Button type="button" variant="secondary" disabled={controlBusy} onClick={() => setConfirm(null)}>取消</Button><Button type="button" variant={confirm === 'shutdown' ? 'danger' : 'primary'} disabled={controlBusy} onClick={() => void runControl(confirm)}><AlertTriangle size={16} />{controlBusy ? '处理中…' : confirm === 'restart' ? '确认重启' : '确认退出'}</Button></div>
    </Modal>}
    {restoreConfirm && <Modal title="确认恢复配置？" description="恢复将覆盖当前的渠道、模型、计费与客户端 Key 配置。管理员登录密钥、MCP 管理授权与历史用量日志保持不变。恢复完成后服务将自动重启以应用新配置。" onClose={() => !restoreBusy && setRestoreConfirm(false)} busy={restoreBusy}>
      {restoreProgress && <p role="status" className="settings-note">{restoreProgress}</p>}
      {restoreError && <ErrorBox message={restoreError} />}
      <div className="modal-footer">
        <Button type="button" variant="secondary" disabled={restoreBusy} onClick={() => setRestoreConfirm(false)}>取消</Button>
        <Button type="button" variant="danger" disabled={restoreBusy} onClick={() => void handleRestore()}><AlertTriangle size={16} />{restoreBusy ? '处理中…' : '确认恢复并重启'}</Button>
      </div>
    </Modal>}
    {rollbackConfirm && <Modal title="确认回退到恢复前快照？" description={`将使用快照 ${rollbackSnapshot} 恢复配置。恢复后服务将自动重启以生效。`} onClose={() => !rollbackBusy && setRollbackConfirm(false)} busy={rollbackBusy}>
      {rollbackProgress && <p role="status" className="settings-note">{rollbackProgress}</p>}
      {rollbackError && <ErrorBox message={rollbackError} />}
      <div className="modal-footer">
        <Button type="button" variant="secondary" disabled={rollbackBusy} onClick={() => setRollbackConfirm(false)}>取消</Button>
        <Button type="button" variant="danger" disabled={rollbackBusy} onClick={() => void handleRollback()}><AlertTriangle size={16} />{rollbackBusy ? '处理中…' : '确认回退并重启'}</Button>
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
    }}>第三方声明与许可证<ExternalLink size={14} /></a>{noticeError && <span role="alert">{noticeError}</span>}</footer>
  </div>;
}
