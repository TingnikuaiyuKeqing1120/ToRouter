import { useEffect, useRef, useState, type FormEvent, type ComponentProps } from 'react';
import { Download, Upload } from 'lucide-react';
import { api, ApiError, credentialLabels, type CredentialType } from '../api';
import { Button, ErrorBox, Input, Modal } from './ui';
import './OAuthTransfer.css';

export type OAuthTransferRef = { channel_id: number; credential_id: string };
type Target = { id: number; name: string; version: string };
type Row = { id: string; label: string; credential_type: CredentialType; enabled: boolean; status: 'new' | 'exists'; targets: Target[] };
type Preview = { target?: Target; id: string; expires_at: string; rows: Row[]; errors?: { file: number; message: string }[] };
type Result = { candidate_ids?: string[]; channel_id: number; added: number; skipped: number; status: string; message?: string };
type Props = { embedded?: boolean; targetChannelID?: number; targetName?: string; mode: 'import' | 'export'; credentialType?: CredentialType; refs?: OAuthTransferRef[]; labels?: string[]; valid?: boolean; onClose: () => void; onChanged?: () => void; onBusy?: (busy: boolean) => void };
const sessionPath = (id: string) => `/oauth-transfer/sessions/${encodeURIComponent(id)}`;
const discard = (id: string) => { void api(sessionPath(id), { method: 'DELETE' }).catch(() => {}); };
function failure(cause: unknown, action: string) {
  if (cause instanceof ApiError) {
    if ([401, 403].includes(cause.status)) return '登录状态或权限已变化，请重新连接。';
    if ([404, 409, 410].includes(cause.status)) return '预览或目标已变化，请重新预览。';
    if (cause.status === 413) return '文件过大，请减少本次选择。';
    if (cause.status === 400) return '文件格式或凭证材料不完整，请核对对应工具的凭证文件。';
  }
  return `${action}未完成，请重试或检查凭证文件。`;
}
function formatHint(kind?: CredentialType) {
  if (kind === 'codex_oauth') return 'Codex auth.json 或 Codex-Manager 导出的账号 JSON';
  if (kind === 'claude_oauth') return 'Claude Code .credentials.json 或 Magpie 支持的 Claude 账号 JSON';
  if (kind === 'antigravity_oauth') return 'Antigravity Manager、Cockpit 或 CPA 导出的账号 JSON';
  if (kind === 'copilot_oauth') return 'Copilot apps.json、hosts.json 或含令牌的认证 JSON';
  return '对应工具的原生凭证文件或 Magpie 认证 JSON';
}

function TransferSurface({ embedded, ...props }: ComponentProps<typeof Modal> & { embedded?: boolean }) { return embedded ? <section className="oauth-transfer-dialog">{props.children}</section> : <Modal {...props} />; }

export function OAuthTransfer({ embedded, targetChannelID, targetName, mode, credentialType, refs = [], labels = [], valid = true, onClose, onChanged, onBusy }: Props) {
  const [material, setMaterial] = useState<'json' | 'access_token' | 'refresh_token' | 'oauth_token'>('json');
  const [accountID, setAccountID] = useState(''), [showSecret, setShowSecret] = useState(false);
  const [files, setFiles] = useState<File[]>([]), [pasted, setPasted] = useState('');
  const [preview, setPreview] = useState<Preview | null>(null), [selected, setSelected] = useState<string[]>([]);
  const [busy, setBusy] = useState<'preview' | 'import' | 'export' | null>(null);
  const [error, setError] = useState(''), [notice, setNotice] = useState(''), [results, setResults] = useState<Result[]>([]);
  const epoch = useRef(0), closed = useRef(false), controller = useRef<AbortController>();
  const active = useRef<Preview | null>(null), input = useRef<HTMLInputElement>(null);
  const latest = useRef({ valid, refs: JSON.stringify(refs) });
  latest.current = { valid, refs: JSON.stringify(refs) };
  useEffect(() => {
    closed.current = false;
    return () => { closed.current = true; epoch.current++; controller.current?.abort(); if (active.current) discard(active.current.id); };
  }, []);
  useEffect(() => { onBusy?.(!!busy); }, [busy, onBusy]);
  useEffect(() => () => onBusy?.(false), [onBusy]);
  function clearPreview() {
    if (active.current) discard(active.current.id);
    active.current = null; setPreview(null); setSelected([]); setResults([]);
  }
  function close() {
    if (busy === 'import') return;
    closed.current = true; epoch.current++; controller.current?.abort(); clearPreview(); onClose();
  }
  function start(action: NonNullable<typeof busy>) {
    controller.current?.abort(); controller.current = new AbortController();
    const job = ++epoch.current; setBusy(action); setError(''); setNotice('');
    return { job, signal: controller.current.signal };
  }
  const current = (job: number) => !closed.current && epoch.current === job;
  function finish(job: number) { if (current(job)) { setBusy(null); controller.current = undefined; } }
  const newRows = preview?.rows.filter(row => row.status === 'new') || [];
  async function prepare(event: FormEvent) {
    event.preventDefault();
    if (busy || !files.length && !pasted.trim()) return;
    const picked = [...files];
    const text = material === 'json' ? pasted.trim() : JSON.stringify({ [material]: pasted.trim(), ...(accountID.trim() ? { account_id: accountID.trim() } : {}) });
    const { job, signal } = start('preview'); clearPreview();
    try {
      const contents = await Promise.all(picked.map(file => file.text()));
      if (text) contents.push(text);
      if (!current(job)) return;
      if (contents.reduce((sum, value) => sum + new Blob([value]).size, 0) > 8 * 1024 * 1024) { setError('本次文件总大小不能超过 8 MiB。'); return; }
      const payload = { target_channel_id: targetChannelID, credential_type: credentialType || undefined, ...(contents.length === 1 ? { file: contents[0] } : { files: contents }) };
      const next = await api<Preview>('/oauth-transfer/preview', { method: 'POST', body: JSON.stringify(payload), signal });
      if (!current(job)) { discard(next.id); return; }
      active.current = next; setPreview(next);
      const rows = next.rows.filter(row => row.status === 'new'); setSelected(rows.map(row => row.id));
      setFiles([]); setPasted(''); if (input.current) input.current.value = '';
    } catch (cause) { if (current(job)) setError(failure(cause, '预览')); }
    finally { finish(job); }
  }
  async function importSelected() {
    const snapshot = active.current;
    if (busy || !snapshot || !selected.length) return;
    if (Date.parse(snapshot.expires_at) <= Date.now()) { clearPreview(); setError('预览已过期，请重新选择文件。'); return; }
    const rows = snapshot.rows.filter(row => row.status === 'new' && selected.includes(row.id));
    const targetID = snapshot.target?.id || targetChannelID || 0;
    if (!snapshot.target && rows.some(row => targetID !== 0 && !row.targets.some(target => target.id === targetID))) { setError('请重新选择添加位置。'); return; }
    const items = rows.map(row => ({ candidate_id: row.id, target_channel_id: targetID, expected_version: snapshot.target?.version || row.targets.find(target => target.id === targetID)?.version || '' }));
    const { job, signal } = start('import');
    try {
      const response = await api<{ results: Result[]; preview: Preview }>(`${sessionPath(snapshot.id)}/import`, { method: 'POST', body: JSON.stringify({ items }), signal });
      if (!current(job)) return;
      active.current = response.preview; setPreview(response.preview); setResults(response.results);
      setSelected(response.results.filter(result => result.status === 'failed').flatMap(result => result.candidate_ids || []).filter(id => response.preview.rows.some(row => row.id === id && row.status === 'new')));
      if (response.results.some(result => result.added > 0)) onChanged?.();
    } catch (cause) { if (current(job)) setError(failure(cause, '添加')); }
    finally { finish(job); }
  }
  async function exportSelected() {
    if (busy || !valid || !refs.length) return;
    const captured = JSON.stringify(refs), { job, signal } = start('export');
    try {
      const response = await api<{ file: string; filename: string; count: number; encoding?: string; mime_type: string }>('/oauth-transfer/export', { method: 'POST', body: JSON.stringify({ refs }), signal });
      if (!current(job) || !latest.current.valid || latest.current.refs !== captured) return;
      const content = response.encoding === 'base64' ? Uint8Array.from(atob(response.file), char => char.charCodeAt(0)) : response.file;
      const url = URL.createObjectURL(new Blob([content], { type: response.mime_type || 'application/json' }));
      const anchor = document.createElement('a'); anchor.href = url; anchor.download = response.filename.split(/[\\/]/).pop() || 'credentials.json';
      try { document.body.append(anchor); anchor.click(); } finally { anchor.remove(); window.setTimeout(() => URL.revokeObjectURL(url), 0); }
      setNotice(`已下载 ${response.count} 个凭证。`);
    } catch (cause) { if (current(job)) setError(failure(cause, '导出')); }
    finally { finish(job); }
  }
  return <TransferSurface embedded={embedded} title={mode === 'export' ? '导出凭证' : '导入凭证'} onClose={close} busy={busy === 'import'} className="oauth-transfer-dialog">
    <div className="oauth-transfer-content">
      {mode === 'export' ? <><p>按对应工具的凭证格式下载；多项会放在一个 ZIP 中。</p><ul className="oauth-transfer-labels" aria-label="选中导出凭证">{labels.map((label, index) => <li key={index}>{label}</li>)}</ul>{!valid && <p role="alert">选中的凭证已变化，请关闭后重新选择。</p>}</>
      : !preview ? <form id="oauth-transfer-prepare" onSubmit={prepare} className="oauth-transfer-form">
        <p>{formatHint(credentialType)}</p>
        {(credentialType === 'codex_oauth' || credentialType === 'copilot_oauth') && <label>材料类型<select aria-label="凭证材料类型" className="input" value={material} disabled={!!busy} onChange={event => { setMaterial(event.target.value as typeof material); setPasted(''); setFiles([]); }}><option value="json">原生 JSON</option>{credentialType === 'codex_oauth' ? <><option value="access_token">Access token</option><option value="refresh_token">Refresh token</option></> : <option value="oauth_token">GitHub 登录 token</option>}</select></label>}
        {material === 'access_token' && <><Input aria-label="账号或工作空间 ID" placeholder="账号或工作空间 ID（令牌声明缺少时必填）" value={accountID} onChange={event => setAccountID(event.target.value)} /><p>仅 Access token 无法自动续期；到期后需补充材料或重新登录。</p></>}
        {material === 'oauth_token' && <p>只接受 GitHub 登录 token；不接受短期 Copilot 推理 bearer。</p>}
        {material === 'json' && <label>读取凭证文件<input className="input" ref={input} aria-label="凭证文件" type="file" multiple accept=".json,.txt,.toml,.info,application/json" disabled={!!busy} onChange={event => {
          const picked = Array.from(event.currentTarget.files || []); setError('');
          if (picked.some(file => file.size > 4 * 1024 * 1024)) { event.currentTarget.value = ''; setFiles([]); setError('单个文件不能超过 4 MiB。'); return; }
          setFiles(picked);
        }} /></label>}
        <label>粘贴凭证内容<textarea aria-label="粘贴凭证内容" className="input oauth-transfer-paste" style={!showSecret ? { WebkitTextSecurity: 'disc' } as React.CSSProperties : undefined} placeholder={material === 'json' ? '粘贴对应工具的原生 JSON' : '粘贴所选类型的 token'} value={pasted} disabled={!!busy} onChange={event => setPasted(event.target.value)} /></label><Button type="button" variant="ghost" onClick={() => setShowSecret(value => !value)}>{showSecret ? '隐藏凭证内容' : '显示凭证内容'}</Button>
      </form> : <>
        {newRows.length > 0 && <div className="oauth-transfer-preview-heading"><label><input type="checkbox" aria-label="全选新凭证" checked={selected.length === newRows.length} ref={node => { if (node) node.indeterminate = selected.length > 0 && selected.length < newRows.length; }} disabled={!!busy} onChange={event => setSelected(event.target.checked ? newRows.map(row => row.id) : [])} /> 全选</label>{targetChannelID && <span>添加到「{preview.target?.name || targetName || targetChannelID}」</span>}</div>}

        <ul className="oauth-transfer-rows" aria-label="凭证导入预览">{preview.rows.map((row, index) => <li key={row.id}><div className="oauth-transfer-row-heading"><label><input type="checkbox" aria-label={`选择导入 ${row.label || `凭证 ${index + 1}`}`} checked={selected.includes(row.id)} disabled={!!busy || row.status === 'exists'} onChange={event => setSelected(old => event.target.checked ? [...old, row.id] : old.filter(id => id !== row.id))} /><strong>{row.label || `凭证 ${index + 1}`}</strong></label><span>{row.status === 'exists' ? '已存在，跳过' : credentialLabels[row.credential_type] || '新凭证'}</span></div></li>)}</ul>
        {!!preview.errors?.length && <ul className="oauth-transfer-results" role="status">{preview.errors.map((item, index) => <li key={index}>文件 {item.file}：{item.message}</li>)}</ul>}
        {!preview.rows.length && <p>没有可导入的凭证。</p>}
      </>}
      {error && <ErrorBox message={error} />}{notice && <p role="status">{notice}</p>}
      {!!results.length && <ul className="oauth-transfer-results" aria-label="凭证导入结果">{results.map((result, index) => <li key={index}>{result.status === 'failed' ? result.message || '添加未完成，请核对凭证材料。' : `已添加 ${result.added} 个，跳过 ${result.skipped} 个。`}{result.channel_id > 0 && <a href={`#/channels?channel_id=${result.channel_id}`} onClick={close}>查看渠道</a>}</li>)}</ul>}
    </div>
    <div className="modal-footer"><Button variant="secondary" disabled={busy === 'import'} onClick={close}>关闭</Button>{mode === 'export' ? <Button disabled={!!busy || !valid || !refs.length} onClick={() => void exportSelected()}><Download size={15} />{busy === 'export' ? '正在导出…' : '下载凭证'}</Button> : preview ? <><Button variant="secondary" disabled={!!busy} onClick={() => { clearPreview(); setError(''); }}>重新选择</Button><Button disabled={!!busy || !selected.length} onClick={() => void importSelected()}>{busy === 'import' ? '正在添加…' : `添加选中（${selected.length}）`}</Button></> : <Button type="submit" form="oauth-transfer-prepare" disabled={!!busy || !files.length && !pasted.trim()}><Upload size={15} />{busy === 'preview' ? '正在读取…' : '预览凭证'}</Button>}</div>
  </TransferSurface>;
}
