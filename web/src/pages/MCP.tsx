import { useEffect, useRef, useState } from 'react';
import { BriefcaseBusiness, Copy, Pencil, Plus, RefreshCw, RotateCcw, Trash2 } from 'lucide-react';
import { ApiError, errorMessage } from '../api';
import { useConfirm } from '../components/Confirm';
import { Badge, Button, Empty, ErrorBox, Field, Input, Loading, Modal, PageHeader } from '../components/ui';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { mcp } from '../services/mcp';
import { mcpAgentInstructions, mcpClientSetup, mcpEndpoint, type MCPClient } from '../services/mcpSetup';
import { MCPClientChoice, MCPSetupGuide } from '../components/MCPSetupGuide';
import { MCPToolbox } from '../components/MCPToolbox';
import type { MCPConnection, MCPConnectionSecret, MCPPermission, MCPStatus, MCPTool } from '../types/mcp';
import './MCP.css';

const permissionLabels: Record<MCPPermission, string> = { read: '查看', manage: '日常管理', maintain: '维护', sensitive: '敏感读取' };
const permissions: MCPPermission[] = ['read', 'manage', 'maintain', 'sensitive'];
const permissionHints: Record<MCPPermission, string> = {
  read: '查询配置、状态和用量。', manage: '添加与调整配置，执行日常操作。',
  maintain: '删除、覆盖、清理与重启等维护操作。', sensitive: '读取业务密钥等敏感内容。',
};
type Draft = { id?: number; name: string; permissions: MCPPermission[] };
const fingerprint = (value: Draft) => JSON.stringify({ ...value, permissions: permissions.filter(permission => value.permissions.includes(permission)) });
function date(value?: string | null) { return value && !value.startsWith('0001-') ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '尚未使用'; }

export default function MCP() {
  const [status, setStatus] = useState<MCPStatus | null>(null);
  const [toolboxOpen, setToolboxOpen] = useState(false), [toolboxTools, setToolboxTools] = useState<MCPTool[] | null>(null);
  const [setupClient, setSetupClient] = useState<MCPClient>('general');
  const [connections, setConnections] = useState<MCPConnection[] | null>(null);
  const [loading, setLoading] = useState(true), [error, setError] = useState('');
  const [draft, setDraft] = useState<Draft | null>(null), [editorError, setEditorError] = useState('');
  const [unknownResult, setUnknownResult] = useState(false);
  const [secret, setSecret] = useState<(MCPConnectionSecret & { rotated?: boolean }) | null>(null);
  const [busy, setBusy] = useState(false), [copyMessage, setCopyMessage] = useState('');
  const baseline = useRef(''), mutation = useRef(false), mounted = useRef(true), loadVersion = useRef(0);
  const confirm = useConfirm();

  async function load(signal?: AbortSignal) {
    const version = ++loadVersion.current;
    setLoading(true);
    try {
      const [nextStatus, nextConnections] = await Promise.all([mcp.status(signal), mcp.connections(signal)]);
      if (!mounted.current || signal?.aborted || version !== loadVersion.current) return;
      setStatus(nextStatus); setConnections(nextConnections || []); setError('');
    } catch (e) {
      if (mounted.current && !signal?.aborted && version === loadVersion.current) setError(`读取 MCP 连接失败：${errorMessage(e)}`);
    } finally { if (mounted.current && !signal?.aborted && version === loadVersion.current) setLoading(false); }
  }
  useEffect(() => {
    mounted.current = true;
    const abort = new AbortController(); void load(abort.signal);
    return () => { mounted.current = false; abort.abort(); loadVersion.current++; };
  }, []);

  function openEditor(connection?: MCPConnection) {
    const value: Draft = connection ? { id: connection.id, name: connection.name, permissions: [...connection.permissions] } : { name: '', permissions: ['read', 'manage'] };
    baseline.current = fingerprint(value); setDraft(value); setEditorError(''); setUnknownResult(false);
  }
  function upsert(connection: MCPConnection) {
    setConnections(old => old?.some(row => row.id === connection.id) ? old.map(row => row.id === connection.id ? connection : row) : [...(old || []), connection]);
  }
  async function refreshStatus() {
    try { const value = await mcp.status(); if (mounted.current) setStatus(value); }
    catch { if (mounted.current) setError('连接更改已保存，状态暂未更新。请刷新重试。'); }
  }
  async function save() {
    if (!draft || mutation.current || unknownResult) return false;
    const form = document.getElementById('mcp-connection-form') as HTMLFormElement | null;
    if (!form?.reportValidity()) return false;
    if (!draft.name.trim()) { setEditorError('请填写连接名称。'); return false; }
    if (!draft.permissions.length) { setEditorError('请至少选择一项权限。'); return false; }
    mutation.current = true; setBusy(true); setEditorError('');
    const creating = !draft.id;
    try {
      const payload = { name: draft.name.trim(), permissions: draft.permissions };
      if (draft.id) upsert(await mcp.update(draft.id, payload));
      else { const result = await mcp.create(payload); upsert(result.connection); setCopyMessage(''); setSecret(result); }
      setDraft(null); void refreshStatus();
      // Keep the page open after an exit-save so the new key can be collected.
      return !creating;
    } catch (e) {
      if (e instanceof ApiError) setEditorError(`保存失败：${errorMessage(e)}`);
      else { setUnknownResult(true); setEditorError('请求结果尚未确认。请返回连接列表刷新核对；若连接已创建，请轮换密钥重新领取。'); }
      return false;
    }
    finally { mutation.current = false; if (mounted.current) setBusy(false); }
  }
  const canLeave = useUnsavedChanges({ dirty: !!draft && fingerprint(draft) !== baseline.current, busy: busy || !!secret, save, discard: () => setDraft(null) });
  async function closeEditor() { if (await canLeave()) setDraft(null); }

  async function change(connection: MCPConnection, action: 'toggle' | 'rotate' | 'revoke') {
    if (mutation.current) return;
    if (action !== 'toggle' && !await confirm({
      title: action === 'rotate' ? '轮换连接密钥？' : '撤销管理连接？',
      description: action === 'rotate' ? `“${connection.name}”的旧密钥将立即失效，请将新密钥更新到客户端。` : `“${connection.name}”将无法继续管理网关。`,
      confirmLabel: action === 'rotate' ? '轮换密钥' : '撤销连接',
    })) return;
    if (mutation.current) return;
    mutation.current = true; setBusy(true); setError('');
    try {
      if (action === 'revoke') { await mcp.revoke(connection.id); setConnections(old => old?.filter(row => row.id !== connection.id) || []); }
      else if (action === 'toggle') upsert(await mcp.update(connection.id, { enabled: !connection.enabled }));
      else { const result = await mcp.rotate(connection.id); upsert(result.connection); setCopyMessage(''); setSecret({ ...result, rotated: true }); }
      void refreshStatus();
    } catch (e) { setError(`操作失败：${errorMessage(e)}`); }
    finally { mutation.current = false; if (mounted.current) setBusy(false); }
  }
  async function copy(value: string, label: string) {
    try { await navigator.clipboard.writeText(value); setCopyMessage(`已复制${label}`); }
    catch { setCopyMessage('复制失败，请选择内容手动复制。'); }
  }
  let endpoint = '';
  try { if (status) endpoint = mcpEndpoint(status); } catch { /* Invalid addresses must not produce usable-looking setup. */ }
  const locked = busy || loading || !connections || !!error;

  return <div className="mcp-page">
    <PageHeader eyebrow="MCP CONSOLE" title="MCP控制台" description="连接 AI 客户端，管理当前网关。" actions={<Button disabled={locked} onClick={() => openEditor()}><Plus size={17} />创建连接</Button>} />
    <section className="panel mcp-endpoint" aria-label="MCP服务状态">
      <div className="mcp-endpoint-heading"><div><Badge tone={status?.enabled ? 'teal' : 'neutral'}>{status ? status.enabled ? '可连接' : '未启用' : '正在读取'}</Badge><span className="mcp-toolbox-entry"><span className="mcp-secondary">{status ? `${status.tool_count} 个工具 · ${status.capability_count} 项能力` : '等待网关状态'}</span><Button variant="ghost" className="mcp-toolbox-trigger" aria-label="查看MCP功能工具箱" aria-haspopup="dialog" title="查看可用功能" onClick={() => setToolboxOpen(true)}><BriefcaseBusiness size={19} /></Button></span></div><Button variant="ghost" aria-label="刷新MCP状态" disabled={loading || busy} onClick={() => { setToolboxTools(null); void load(); }}><RefreshCw size={16} className={loading ? 'animate-spin' : ''} />刷新</Button></div>
      <div className="mcp-address"><code>{endpoint || (status ? '暂无可用连接地址' : '正在读取连接地址…')}</code><Button variant="ghost" aria-label="复制MCP地址" disabled={!endpoint} onClick={() => endpoint && void copy(endpoint, '连接地址')}><Copy size={16} /></Button></div>
    </section>
    {error && <ErrorBox message={`${error}${connections ? '（保留上次连接列表）' : ''}`} retry={() => void load()} />}
    <MCPSetupGuide status={status} client={setupClient} onClientChange={value => { setSetupClient(value); setCopyMessage(''); }} onCreate={() => openEditor()} canCreate={!locked} copy={copy} copyMessage={copyMessage} />
    <section className="panel mcp-connections" aria-labelledby="mcp-connections-title">
      <div className="mcp-section-heading"><h2 id="mcp-connections-title">管理连接</h2><span className="mcp-secondary" role="status">{loading ? connections ? '正在刷新…' : '正在加载…' : `${connections?.length || 0} 个连接`}</span></div>
      {!connections && loading ? <Loading /> : connections?.length === 0 ? <Empty title="还没有管理连接" description="为每个客户端创建一个命名连接，默认允许查看和日常管理。" variant="key" action={<Button disabled={locked} onClick={() => openEditor()}><Plus size={16} />创建第一个连接</Button>} /> : connections?.map(connection => <article className="mcp-connection" key={connection.id} aria-label={connection.name}>
        <div className="mcp-connection-main"><div className="mcp-connection-name"><h3>{connection.name}</h3><Badge tone={connection.enabled ? 'teal' : 'neutral'}>{connection.enabled ? '已启用' : '已停用'}</Badge></div><p className="mcp-permission-summary">{connection.permissions.map(permission => permissionLabels[permission]).join(' · ')}</p><p className="mcp-secondary">最近使用：{date(connection.last_used_at)}</p></div>
        <div className="mcp-connection-actions"><Button variant="secondary" disabled={locked} aria-label={`${connection.enabled ? '停用' : '启用'} ${connection.name}`} onClick={() => void change(connection, 'toggle')}>{connection.enabled ? '停用' : '启用'}</Button><Button variant="ghost" disabled={locked} aria-label={`编辑 ${connection.name}`} onClick={() => openEditor(connection)}><Pencil size={15} />编辑</Button><details className="mcp-more" onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget as Node | null)) event.currentTarget.open = false; }} onKeyDown={event => { if (event.key === 'Escape') { event.currentTarget.open = false; event.currentTarget.querySelector('summary')?.focus(); } }}><summary aria-label={`${connection.name} 更多操作`}>更多</summary><div><Button variant="ghost" disabled={locked} onClick={event => { event.currentTarget.closest('details')?.removeAttribute('open'); void change(connection, 'rotate'); }}><RotateCcw size={15} />轮换密钥</Button><Button variant="ghost" disabled={locked} onClick={event => { event.currentTarget.closest('details')?.removeAttribute('open'); void change(connection, 'revoke'); }}><Trash2 size={15} />撤销连接</Button></div></details></div>
      </article>)}
    </section>
    {draft && <Modal title={draft.id ? '编辑管理连接' : '创建管理连接'} description="按客户端命名，方便分别管理。" onClose={() => void closeEditor()} busy={busy} className="mcp-editor" initialFocus="#mcp-connection-name"><form id="mcp-connection-form" onSubmit={event => { event.preventDefault(); void save(); }}><Field label="连接名称"><Input id="mcp-connection-name" required maxLength={100} value={draft.name} disabled={busy} placeholder="例如：桌面助手" onChange={event => setDraft({ ...draft, name: event.target.value })} /></Field><fieldset className="mcp-permissions" disabled={busy}><legend>授权范围</legend>{permissions.map(permission => <label className="mcp-permission" key={permission}><input className="checkbox" type="checkbox" checked={draft.permissions.includes(permission)} onChange={event => setDraft({ ...draft, permissions: permissions.filter(value => value === permission ? event.target.checked : draft.permissions.includes(value)) })} /><span><strong>{permissionLabels[permission]}</strong><span>{permissionHints[permission]}</span></span></label>)}</fieldset>{editorError && <ErrorBox message={editorError} />}{unknownResult && <Button variant="secondary" onClick={() => { setDraft(null); void load(); }}>返回连接列表核对</Button>}<div className="modal-footer"><Button variant="secondary" disabled={busy} onClick={() => void closeEditor()}>取消</Button><Button type="submit" disabled={busy || unknownResult}>{busy ? '正在保存…' : draft.id ? '保存连接' : '创建连接'}</Button></div></form></Modal>}
    {secret && <Modal title="领取连接密钥" description={`“${secret.connection.name}”的密钥仅在此显示一次，关闭前请复制保存。`} onClose={() => { setSecret(null); setCopyMessage(''); }} className="mcp-secret" initialFocus="[data-copy-secret]">
      <label className="field"><span className="field-label">管理连接密钥</span><textarea readOnly rows={3} value={secret.token} aria-label="管理连接密钥" spellCheck={false} /></label>
      <div className="mcp-secret-setup">
        <h3>直接配置到工具</h3>
        <MCPClientChoice value={setupClient} onChange={value => { setSetupClient(value); setCopyMessage(''); }} />
        <div className="mcp-setup-actions">
          <Button disabled={!endpoint} onClick={() => status && void copy(mcpAgentInstructions(status, setupClient, secret.token), '含密钥的 Agent 配置说明')}><Copy size={15} />复制给 Agent（含密钥）</Button>
          <Button variant="secondary" disabled={!endpoint} onClick={() => { if (!status) return; const setup = mcpClientSetup(status, setupClient, secret.token); void copy(secret.rotated && setup.updateConfiguration ? setup.updateConfiguration : setup.configuration, '含密钥的连接资料'); }}><Copy size={15} />{setupClient === 'general' ? '复制连接资料（含密钥）' : secret.rotated && setupClient === 'claude' ? '复制更新命令（含密钥）' : '复制配置（含密钥）'}</Button>
        </div>
        <p className="mcp-secondary">含密钥版本会把这条连接的权限交给你粘贴的工具或 Agent，请只发给你准备授权的对象。关闭本窗口后不再保留密钥。</p>
        {secret.rotated && setupClient === 'claude' && <p className="mcp-secondary">更新命令会替换 Claude Code 用户级的 to-router 登记；先用 claude mcp get to-router 确认目标作用域。</p>}
      </div>
      <p className="mcp-secondary" role="status">{copyMessage}</p>
      <div className="modal-footer"><Button data-copy-secret variant="secondary" onClick={() => void copy(secret.token, '管理连接密钥')}><Copy size={16} />仅复制密钥</Button><Button onClick={() => { setSecret(null); setCopyMessage(''); }}>完成</Button></div>
    </Modal>}
    {toolboxOpen && <MCPToolbox cachedTools={toolboxTools} onLoad={setToolboxTools} onClose={() => setToolboxOpen(false)} />}
  </div>;
}
