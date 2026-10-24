import { useEffect, useMemo, useRef, useState } from 'react';
import { AudioLines, FileText, Image, Pencil, Search, Sparkles, Type, Video } from 'lucide-react';
import { api, errorMessage, type CatalogItem, type CatalogModelRef, type Channel, type ModelConfig, type PrefillPreview, type PrefillTarget, type UpstreamModelSpecs } from '../api';
import { useResource } from '../hooks';
import { Button, Empty, ErrorBox, Input, Loading, Modal, PageHeader, useToast } from '../components/ui';
import { ModelSpecsEditor } from '../components/ModelSpecsEditor';
import { PrefillControls } from '../components/CatalogPrefill';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { managementTarget } from '../services/managementLinks';
import { enabledChannelModels } from '../services/modelCandidates';
import { catalogEntries } from '../services/catalogEntries';
import { ModelCatalogPicker } from '../components/ModelCatalogPicker';
import './ModelSpecs.css';

const channelSelectionKey = 'to-router.model-specs-channels.v1';
function savedChannels(): number[] | null {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(channelSelectionKey) || 'null');
    return Array.isArray(value) ? value.filter((id): id is number => Number.isInteger(id) && id > 0) : null;
  } catch { return null; }
}
function rowKey(row: UpstreamModelSpecs) { return `${row.channel_id}:${row.model}`; }
function modelRef(ref?: CatalogModelRef): CatalogModelRef | undefined {
  return ref ? { source: ref.source, id: ref.id, ...(ref.offering_key ? { offering_key: ref.offering_key } : {}) } : undefined;
}
function modalities(spec: ModelConfig) {
  const input = spec.input_modalities, output = spec.output_modalities;
  const all = new Set([...(input || []), ...(output || [])]);
  if (!all.size) return <span>{input != null && output != null ? '空列表' : '未知'}</span>;
  const icons = [{ id: 'text', label: '文本', Icon: Type }, { id: 'image', label: '图像', Icon: Image }, { id: 'audio', label: '音频', Icon: AudioLines }, { id: 'file', label: '文件', Icon: FileText }, { id: 'video', label: '视频', Icon: Video }];
  return <span className="model-specs-modalities">{icons.filter(item => all.has(item.id)).map(({ id, label, Icon }) => <span key={id} title={`${label} · ${[input?.includes(id) ? '输入' : '', output?.includes(id) ? '输出' : ''].filter(Boolean).join(' / ')}`}><Icon size={16} aria-label={label} /></span>)}{[...all].filter(id => !icons.some(icon => icon.id === id)).map(id => <span key={id}>{id}</span>)}</span>;
}
function thinking(spec: ModelConfig) {
  if (spec.native_thinking?.supported === false) return '不支持';
  if (spec.thinking_efforts != null) return spec.thinking_efforts.length ? `${spec.thinking_efforts.length} 档 · ${spec.thinking_efforts.join(' / ')}` : '空列表';
  return spec.native_thinking?.supported === true ? (spec.native_thinking.mandatory ? '必需' : '支持') : '未知';
}

function SpecsDialog({ entry, onClose, onSaved }: { entry: UpstreamModelSpecs; onClose: () => void; onSaved: () => void }) {
  const [specs, setSpecs] = useState(entry.specs || {}), [ref, setRef] = useState(entry.catalog_model_ref);
  const [busy, setBusy] = useState(false), [error, setError] = useState('');
  const toast = useToast();
  const dirty = JSON.stringify({ specs, ref }) !== JSON.stringify({ specs: entry.specs || {}, ref: entry.catalog_model_ref });
  const canLeave = useUnsavedChanges({ dirty, busy, save });
  async function close() { if (await canLeave()) onClose(); }
  async function save() {
    setBusy(true); setError('');
    try { await api('/model-specs', { method: 'PUT', body: JSON.stringify({ channel_id: entry.channel_id, model: entry.model, specs, catalog_model_ref: ref }) }); toast('模型规格已保存'); onSaved(); return true; }
    catch (error) { setError(errorMessage(error)); return false; } finally { setBusy(false); }
  }
  return <Modal title={`编辑模型规格 · ${entry.model}`} description={entry.channel_name} onClose={close} busy={busy} wide className="model-specs-editor">
    <div className="model-specs-editor-body">
      {error && <ErrorBox message={error} />}
      <fieldset disabled={busy} className="space-y-4">
        <PrefillControls targets={[{ kind: 'upstream', channel_id: entry.channel_id, model: entry.model, draft: specs, ref }]} allowedGroups={['capabilities']} onDraft={patch => { setSpecs(patch.config); setRef(patch.catalog_model_ref); }} onAssociation={setRef} />
        <ModelSpecsEditor value={specs} onChange={setSpecs} />
      </fieldset>
    </div>
    <div className="modal-footer"><Button variant="secondary" disabled={busy} onClick={close}>取消</Button><Button disabled={busy} onClick={() => void save()}>{busy ? '正在保存…' : '保存规格'}</Button></div>
  </Modal>;
}

export default function ModelSpecs() {
  const resource = useResource<UpstreamModelSpecs[]>('/model-specs');
  const connections = useResource<Channel[]>('/channels');
  const catalog = useResource<CatalogItem[]>('/catalog/models?source=all');
  const choices = useMemo(() => catalogEntries(catalog.data || []), [catalog.data]);
  const [target] = useState(managementTarget);
  const [selectedChannels, setSelectedChannels] = useState<number[] | null>(() => target.channelID == null ? savedChannels() : [target.channelID]);
  const [search, setSearch] = useState(target.model);
  const [selectedRows, setSelectedRows] = useState<Set<string>>(() => new Set());
  const [editing, setEditing] = useState<UpstreamModelSpecs | null>(null);
  const [matching, setMatching] = useState(false), [saving, setSaving] = useState(false);
  const [matchError, setMatchError] = useState(''), [results, setResults] = useState<string[]>([]);
  const pending = useRef(false), live = useRef(true);
  useEffect(() => { live.current = true; return () => { live.current = false; }; }, []);
  useUnsavedChanges({ dirty: false, busy: matching, save: async () => false });
  useEffect(() => {
    if (selectedChannels === null) return;
    try { localStorage.setItem(channelSelectionKey, JSON.stringify(selectedChannels)); } catch { /* Optional browser preference. */ }
  }, [selectedChannels]);
  const rows = resource.data || [];
  const channels = (connections.data || []).map(channel => ({ id: channel.id, name: channel.name || rows.find(row => row.channel_id === channel.id)?.channel_name || String(channel.id) }))
    .sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id);
  const channelIDs = channels.map(channel => channel.id);
  const displayedChannels = selectedChannels ?? channelIDs;
  const displayedCount = channelIDs.filter(id => displayedChannels.includes(id)).length;
  const enabled = new Set((connections.data || []).flatMap(channel => enabledChannelModels(channel).map(model => `${channel.id}|${model}`)));
  const isEnabled = (row: UpstreamModelSpecs) => enabled.has(`${row.channel_id}|${row.model}`);
  const visible = rows.filter(row => isEnabled(row) && displayedChannels.includes(row.channel_id) && `${row.model} ${row.channel_name}`.toLowerCase().includes(search.trim().toLowerCase()))
    .sort((a, b) => a.channel_name.localeCompare(b.channel_name) || a.channel_id - b.channel_id || a.model.localeCompare(b.model));
  const selectedVisible = visible.filter(row => selectedRows.has(rowKey(row)));
  const scope = JSON.stringify([search, displayedChannels, [...selectedRows].sort(), visible.map(rowKey)]);
  const currentScope = useRef({ key: scope, revision: 0 });
  if (currentScope.current.key !== scope) currentScope.current = { key: scope, revision: currentScope.current.revision + 1 };
  const ready = !resource.loading && !connections.loading && !resource.error && !connections.error;
  const disabledTarget = ready && target.channelID != null && !!target.model && rows.some(row => row.channel_id === target.channelID && row.model === target.model && !isEnabled(row));
  function selectVisible(checked: boolean) {
    setSelectedRows(previous => {
      const next = new Set(previous);
      visible.forEach(row => { if (checked) next.add(rowKey(row)); else next.delete(rowKey(row)); });
      return next;
    });
  }
  async function matchRows(entries: UpstreamModelSpecs[], ref?: CatalogModelRef) {
    if (pending.current || !entries.length) return;
    pending.current = true; setMatching(true); setMatchError(''); setResults([]);
    const revision = currentScope.current.revision;
    const isCurrent = () => live.current && revision === currentScope.current.revision;
    try {
      const targets: PrefillTarget[] = entries.map(entry => ({ kind: 'upstream', channel_id: entry.channel_id, model: entry.model, ref: modelRef(ref || entry.catalog_model_ref) }));
      const request = { targets, groups: ['capabilities'], force_overwrite: false, price_mode: 'catalog' };
      const preview = await api<PrefillPreview>('/catalog/prefill/preview', { method: 'POST', body: JSON.stringify(request) });
      if (!isCurrent()) return;
      if (!Array.isArray(preview.patches)) throw new Error('规格匹配响应无效，请重试。');
      // Only server-resolved identities may be applied. Preserve exact regional choices.
      const matched = targets.flatMap(target => {
        const patch = preview.patches.find(patch => patch.target.kind === 'upstream' && patch.target.channel_id === target.channel_id && patch.target.model === target.model);
        return patch?.catalog_model_ref ? [{ ...target, ref: modelRef(patch.catalog_model_ref) }] : [];
      });
      const unmatched = entries.filter(entry => !matched.some(target => target.channel_id === entry.channel_id && target.model === entry.model));
      const skipped = unmatched.map(entry => `${entry.channel_name} · ${entry.model}：未找到明确匹配，请搜索选择资料。`);
      if (!matched.length) { setResults(skipped); return; }
      setSaving(true);
      const applied = await api<PrefillPreview>('/catalog/prefill/apply', { method: 'POST', body: JSON.stringify({ ...request, targets: matched }) });
      if (!live.current) return;
      setResults([`匹配完成：更新 ${applied.updated} 项，保留 ${applied.skipped} 项。`, ...skipped]);
      resource.reload();
    } catch (error) { if (isCurrent()) setMatchError(errorMessage(error)); }
    finally { pending.current = false; if (live.current) { setMatching(false); setSaving(false); } }
  }
  return <div className="model-specs-page">
    <PageHeader eyebrow="MODEL PARAMETERS" title="参数配置" description="管理已启用上游模型的能力规格与原生映射。" />
    {disabledTarget && <div role="status" className="model-specs-target-note">目标模型 {target.model} 尚未启用，已保留其规格。<a href="#/channels">前往上游接入启用</a></div>}
    <section className="panel model-specs-workbench">
      <div className="model-specs-toolbar">
        <div className="search-field"><Search size={15} /><Input aria-label="搜索模型规格" placeholder="搜索模型 ID / 渠道" value={search} disabled={saving} onChange={event => setSearch(event.target.value)} /></div>
        {search && <Button variant="ghost" disabled={saving} onClick={() => setSearch('')}>清除搜索</Button>}
        <span className="model-specs-count" role="status">{visible.length} 个模型 · 已选 {selectedVisible.length} 项</span>
        <Button disabled={!selectedVisible.length || !ready || matching} onClick={() => void matchRows(selectedVisible)}><Sparkles size={15} />{matching ? '匹配中…' : `一键匹配（${selectedVisible.length}）`}</Button>
      </div>
      <p className="model-specs-match-hint">匹配仅补全未知规格并保存资料关联；已有规格与原生映射保留。需要覆盖时可进入编辑预填。</p>
      <details className="model-specs-channels" open>
        <summary>显示渠道 · {displayedCount} / {channels.length}</summary>
        <div className="model-specs-channel-options">
          <label><input type="checkbox" className="checkbox" aria-label="全选渠道" disabled={!channels.length || saving} checked={!!channels.length && displayedCount === channels.length} ref={element => { if (element) element.indeterminate = displayedCount > 0 && displayedCount < channels.length; }} onChange={event => setSelectedChannels(event.target.checked ? channelIDs : [])} /><span>全选渠道</span></label>
          {channels.map(channel => <label key={channel.id}><input type="checkbox" className="checkbox" aria-label={`显示渠道 ${channel.name}`} disabled={saving} checked={displayedChannels.includes(channel.id)} onChange={event => setSelectedChannels(event.target.checked ? [...displayedChannels, channel.id] : displayedChannels.filter(id => id !== channel.id))} /><span>{channel.name}</span></label>)}
        </div>
      </details>
      {catalog.error && <div className="model-specs-feedback"><ErrorBox message={`规格资料读取失败：${catalog.error}`} retry={catalog.reload} /></div>}
      {matchError && <div className="model-specs-feedback"><ErrorBox message={matchError} /></div>}
      {!!results.length && <details className="model-specs-results" open><summary>匹配结果</summary><ul>{results.map((result, index) => <li key={index}>{result}</li>)}</ul></details>}
      {resource.loading || connections.loading ? <Loading /> : resource.error ? <ErrorBox message={resource.error} retry={resource.reload} /> : connections.error ? <ErrorBox message={connections.error} retry={connections.reload} /> : !visible.length ? <Empty title="暂无匹配的上游模型" description={!displayedCount ? '请选择要显示的渠道。' : '调整搜索条件，或在上游接入页启用模型。'} /> : <div className="table-scroll"><table className="data-table model-specs-table"><thead><tr>
        <th className="model-specs-selection"><label><input type="checkbox" className="checkbox" aria-label="全选可见模型" disabled={saving} checked={selectedVisible.length === visible.length} ref={element => { if (element) element.indeterminate = selectedVisible.length > 0 && selectedVisible.length < visible.length; }} onChange={event => selectVisible(event.target.checked)} /></label></th><th>渠道</th><th>模型 ID</th><th>规格摘要</th><th>资料库匹配</th><th>操作</th>
      </tr></thead><tbody>{visible.map(row => <tr key={rowKey(row)} data-selected={selectedRows.has(rowKey(row))}>
        <td className="model-specs-selection"><label><input type="checkbox" className="checkbox" aria-label={`选择模型 ${rowKey(row)}`} disabled={saving} checked={selectedRows.has(rowKey(row))} onChange={event => setSelectedRows(previous => { const next = new Set(previous); if (event.target.checked) next.add(rowKey(row)); else next.delete(rowKey(row)); return next; })} /></label></td>
        <td className="model-specs-channel-name">{row.channel_name}</td><td className="model-specs-model-id">{row.model}</td><td><div className="model-specs-summary"><div><small>上下文 / 输出</small><span title="上下文 / 最大输出 tokens" className="model-specs-tokens">{row.specs?.context_window ?? '未知'} / {row.specs?.max_output_tokens ?? '未知'}</span></div><div><small>模态</small>{modalities(row.specs || {})}</div><div><small>思考</small><span>{thinking(row.specs || {})}</span></div></div></td><td><ModelCatalogPicker choices={choices} model={row.model} label={rowKey(row)} value={row.catalog_model_ref} disabled={matching || !!editing || catalog.loading || !!catalog.error} onSelect={ref => void matchRows([row], ref)} /></td><td><Button variant="ghost" aria-label={`编辑规格 ${rowKey(row)}`} disabled={matching} onClick={() => setEditing(row)}><Pencil size={14} />编辑</Button></td>
      </tr>)}</tbody></table></div>}
    </section>
    {editing && <SpecsDialog entry={editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); resource.reload(); }} />}
  </div>;
}
