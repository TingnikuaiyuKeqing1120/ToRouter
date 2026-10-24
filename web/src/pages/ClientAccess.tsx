import { useRef, useState, type FormEvent } from 'react';
import { Copy, KeyRound, Pencil, Plus, RefreshCw, Trash2 } from 'lucide-react';
import { api, errorMessage, type ClientKey, type CreatedClientKey, type DownstreamModelView, type ClientKeyScope, type TagGroupWithTags } from '../api';
import { ClientKeyScopeEditor } from '../components/ClientKeyScopeEditor';
import './ClientAccess.css';
import { useResource } from '../hooks';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { Badge, Button, Empty, ErrorBox, Field, Input, Loading, Modal, PageHeader, useToast } from '../components/ui';

function date(value: string | null) {
  if (!value || value.startsWith('0001-')) return '尚未使用';
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString('zh-CN', { hour12: false });
}

const unrestricted = (): ClientKeyScope => ({ model_mode: 'all', model_ids: [], tag_rules: {} });

export default function ClientAccess() {
  const keys = useResource<ClientKey[]>('/client-keys');
  const connection = useResource<{ origin: string; port: number }>('/connection');
  const models = useResource<DownstreamModelView[]>('/models');
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState('');
  const [editing, setEditing] = useState<ClientKey | null>(null);
  const [scope, setScope] = useState<ClientKeyScope>(unrestricted);
  const baseline = useRef('');
  const tags = useResource<TagGroupWithTags[]>('/tag-groups');
  function openEditor(key?: ClientKey) {
    if (key && !key.scope) { toast('访问范围未读取，请刷新密钥列表后重试。'); return; }
    const next = key ? structuredClone(key.scope) : unrestricted();
    setEditing(key || null); setName(key?.name || ''); setScope(next); setError('');
    baseline.current = JSON.stringify({ name: key?.name || '', scope: next }); setCreating(true);
  }
  function scopeSummary(value: ClientKeyScope) {
    const modelSummary = value.model_mode === 'all' ? '全部公开模型（含新增）' : `指定 ${value.model_ids.length} 个公开模型`;
    const conditions = Object.entries(value.tag_rules).map(([id, allowed]) => {
      const group = tags.data?.find(group => group.id === id);
      return `${group?.name || id}：${allowed.length ? allowed.map(value => group?.tags.find(tag => tag.id === value)?.name || value).join(' / ') : '无允许项'}`;
    });
    return `${modelSummary} · ${conditions.length ? conditions.join('；') : '上游标签不限制'}`;
  }
  const [created, setCreated] = useState<CreatedClientKey | null>(null);
  const [revoking, setRevoking] = useState<ClientKey | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const toast = useToast();
  const available = (models.data || []).filter(m => m.enabled).map(m => m.id);
  const origin = connection.data?.origin.replace(/\/+$/, '');
  const examples = origin ? [
    { title: 'OpenAI', baseURL: `${origin}/v1`, config: { baseURL: `${origin}/v1`, apiKey: '<CLIENT_API_KEY>' } },
    { title: 'Anthropic', baseURL: origin, config: { baseURL: origin, apiKey: '<CLIENT_API_KEY>' } },
    { title: 'Gemini', baseURL: origin, config: { apiKey: '<CLIENT_API_KEY>', httpOptions: { baseUrl: origin } } },
  ] : [];
  async function copy(text: string) {
    try { await navigator.clipboard.writeText(text); toast('已复制'); }
    catch { toast('无法访问剪贴板，请选中文本手动复制。'); }
  }
  async function copyKey(key: ClientKey) {
    try {
      const res = await api<{ key: string }>(`/client-keys/${key.id}/key`);
      await copy(res.key);
    } catch (e) {
      toast(`复制失败：${errorMessage(e)}`);
    }
  }
  const canLeave = useUnsavedChanges({ dirty: creating && JSON.stringify({ name, scope }) !== baseline.current, busy, save: async () => {
    const wasEditing = !!editing;
    const saved = await create();
    // Keep a newly generated key visible before navigation continues.
    return wasEditing && saved;
  } });
  async function closeCreate() { if (await canLeave()) { setCreating(false); setName(''); setEditing(null); } }
  async function create(e?: FormEvent) {
    e?.preventDefault(); if (!name.trim()) { setError('请填写密钥名称。'); return false; }
    setBusy(true); setError('');
    try {
      const result = await api<CreatedClientKey | ClientKey>(editing ? `/client-keys/${editing.id}` : '/client-keys', { method: editing ? 'PUT' : 'POST', body: JSON.stringify({ name: name.trim(), scope }) });
      if (!editing) setCreated(result as CreatedClientKey); else toast('访问范围已保存，对新请求生效');
      setCreating(false); setName(''); setEditing(null); keys.reload();
      return true;
    } catch (e) { setError(errorMessage(e)); return false; } finally { setBusy(false); }
  }
  async function revoke() {
    if (!revoking) return;
    setBusy(true); setError('');
    try {
      await api(`/client-keys/${encodeURIComponent(revoking.id)}`, { method: 'DELETE' });
      setRevoking(null); keys.reload(); toast('客户端 API Key 已撤销');
    } catch (e) { setError(errorMessage(e)); } finally { setBusy(false); }
  }
  return <>
    <PageHeader eyebrow="CLIENT ACCESS" title="客户端接入" description="为 Agent 或应用创建独立 API Key。不要将管理员登录密钥填入客户端。" actions={<Button onClick={() => openEditor()}><Plus size={16} />创建 API Key</Button>} />
    <section className="panel mb-6">
      <div className="panel-toolbar"><div className="flex items-center gap-3"><h2>客户端 API Key</h2>{keys.data && <Badge>{keys.data.length}</Badge>}</div><Button variant="secondary" aria-label="刷新客户端密钥" disabled={keys.loading} onClick={keys.reload}><RefreshCw size={16} /></Button></div>
      {keys.error && <div className="p-5"><ErrorBox message={keys.error} retry={keys.reload} /></div>}
      {keys.loading ? <Loading /> : !keys.error && (!keys.data?.length ? <Empty title="尚未创建客户端 API Key" description="按应用命名密钥，便于识别用量和独立撤销。" /> : <div className="table-scroll"><table className="data-table client-key-table"><thead><tr><th>名称</th><th>访问范围</th><th>密钥前缀</th><th>创建时间</th><th>最后使用</th><th><span className="sr-only">操作</span></th></tr></thead><tbody>{keys.data.map(key => <tr key={key.id}><td className="client-key-name">{key.name}</td><td className="client-key-scope"><p className="client-key-scope-summary">{key.scope ? scopeSummary(key.scope) : '访问范围未读取，请刷新'}</p></td><td className="font-mono text-xs client-key-prefix"><div className="flex items-center gap-1.5"><span>{key.prefix}…</span><Button variant="ghost" className="!p-1 h-6 w-6" aria-label={`复制密钥 ${key.name}`} onClick={() => copyKey(key)}><Copy size={13} /></Button></div></td><td data-label="创建时间">{date(key.created_at)}</td><td data-label="最后使用">{date(key.last_used_at)}</td><td className="client-key-controls"><div className="client-key-actions"><Button variant="ghost" aria-label={`编辑访问范围 ${key.name}`} onClick={() => openEditor(key)}><Pencil size={16} /></Button><Button variant="ghost" aria-label={`撤销 ${key.name}`} onClick={() => { setRevoking(key); setError(''); }}><Trash2 size={16} /></Button></div></td></tr>)}</tbody></table></div>)}
      <p className="border-t border-[var(--color-border)] px-6 py-4 text-xs leading-6 text-[var(--color-text-muted)]">每个应用独立设置访问范围。编辑范围保留原密钥，保存后对新请求生效；复制按钮复制原密钥。</p>
    </section>
    <section className="panel settings-section"><div className="section-intro"><div className="section-icon"><KeyRound size={19} /></div><div><h2>连接配置</h2><p>使用网关实际地址，不是开发控制台的代理地址。</p></div></div><div className="settings-body space-y-5">
      {connection.error && <ErrorBox message={connection.error} retry={connection.reload} />}
      {connection.loading ? <Loading /> : !connection.error && origin && <><div className="connection-box"><span>网关地址</span><code>{origin}</code><Button variant="ghost" aria-label="复制网关地址" onClick={() => copy(origin)}><Copy size={16} /></Button></div>{examples.map(example => {
        const text = JSON.stringify(example.config, null, 2);
        return <div key={example.title}><div className="mb-2 flex items-center justify-between"><h3 className="text-sm font-semibold">{example.title}</h3><div className="flex items-center gap-2"><Button variant="ghost" onClick={() => copy(example.baseURL)}><Copy size={14} />复制 Base URL</Button><Button variant="secondary" onClick={() => copy(text)}><Copy size={14} />复制 {example.title} 配置</Button></div></div><pre className="overflow-auto rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-4 font-mono text-xs text-[var(--color-text-primary)]">{text}</pre></div>;
      })}</>}
      <p className="text-xs leading-6 text-[var(--color-text-secondary)]">以上为 SDK 初始化选项示例。将 &lt;CLIENT_API_KEY&gt; 替换为刚创建的客户端密钥。OpenAI baseURL 包含 /v1；Anthropic 使用 origin，由 SDK 添加 /v1/messages；Gemini 使用 origin，由 SDK 添加版本和模型路径。不同 SDK 的 baseURL / baseUrl / base_url 与版本配置约定不同，请按所用 SDK 调整，避免重复添加路径。</p>
      {models.error ? <ErrorBox message={`模型列表加载失败：${models.error}`} retry={models.reload} /> : models.loading ? <Loading /> : <div><h3 className="mb-2 text-sm font-semibold">可请求的对外模型</h3><div className="flex flex-wrap gap-2">{available.map(model => <span className="model-chip" key={model}>{model}</span>)}</div><p className="mt-2 text-xs leading-6 text-[var(--color-text-muted)]">{available.length ? '请求中的 model 请填写此处列出的对外模型 ID。此列表来自下游模型绑定，不代表上游实时可用性。' : '尚无启用的下游模型，请先在下游模型页配置并启用。'} 控制台不会自动调用 /v1/models 或外部模型 API。</p></div>}
    </div></section>
    {creating && <Modal title={editing ? '编辑客户端 API Key' : '创建客户端 API Key'} description="名称、公开模型与上游条件一并保存。" onClose={closeCreate} busy={busy} wide className="client-key-editor"><form onSubmit={create} className="client-key-form"><div className="client-key-form-scroll">{error && <ErrorBox message={error} />}<fieldset disabled={busy} className="space-y-5"><Field label="密钥名称"><Input required autoFocus value={name} placeholder="例如：桌面 Agent" onChange={e => setName(e.target.value)} /></Field><ClientKeyScopeEditor scope={scope} onChange={setScope} models={models.data || []} modelsError={models.error} reloadModels={models.reload} /></fieldset></div><div className="modal-footer"><Button type="button" variant="secondary" disabled={busy} onClick={closeCreate}>取消</Button><Button type="submit" disabled={busy || !name.trim()}>{busy ? '正在保存…' : editing ? '保存访问范围' : '确认创建'}</Button></div></form></Modal>}
    {created && <Modal title="请保存客户端 API Key" description={`「${created.client_key.name}」的完整密钥请妥善保管；关闭后可在列表中复制。不要分享。`} onClose={() => setCreated(null)}><Field label="新客户端 API Key"><Input readOnly autoComplete="off" className="font-mono" value={created.key} onFocus={e => e.target.select()} /></Field><div className="modal-footer"><Button variant="secondary" onClick={() => copy(created.key)}><Copy size={16} />复制客户端密钥</Button><Button onClick={() => setCreated(null)}>已保存，关闭</Button></div></Modal>}
    {revoking && <Modal title="撤销客户端 API Key？" description={`撤销「${revoking.name}」后，使用此密钥的客户端将立即无法调用。此操作无法撤销。`} onClose={() => setRevoking(null)} busy={busy}>{error && <ErrorBox message={error} />}<div className="modal-footer"><Button variant="secondary" disabled={busy} onClick={() => setRevoking(null)}>取消</Button><Button variant="danger" disabled={busy} onClick={revoke}>{busy ? '正在撤销…' : '确认撤销'}</Button></div></Modal>}
  </>;
}
