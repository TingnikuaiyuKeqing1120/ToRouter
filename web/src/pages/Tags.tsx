import React, { useEffect, useState, useMemo, useRef } from 'react';
import { Plus, RotateCcw, ShieldCheck, Edit2, Trash2, Search, GripVertical, ChevronDown, X } from 'lucide-react';
import { api, errorMessage, ApiError } from '../api';
import { useConfirm } from '../components/Confirm';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { useSortableList } from '../hooks/useSortableList';
import { Button, ErrorBox, Modal } from '../components/ui';
import { TagBadge } from '../components/TagBadge';
import type { TagGroupWithTags, Tag, TagReferences } from '../types';
import './Tags.css';

const colorOptions = [
  { id: 'emerald', label: '翡翠绿', bg: 'bg-emerald-500' },
  { id: 'sky',     label: '天空蓝', bg: 'bg-sky-500' },
  { id: 'amber',   label: '琥珀黄', bg: 'bg-amber-500' },
  { id: 'violet',  label: '紫罗兰', bg: 'bg-violet-500' },
  { id: 'rose',    label: '玫瑰红', bg: 'bg-rose-500' },
  { id: 'indigo',  label: '靛蓝',   bg: 'bg-indigo-500' },
  { id: 'slate',   label: '中性灰', bg: 'bg-slate-500' },
];

const BUILTIN_TAG_IDS = new Set([
  'billing:free', 'billing:plan', 'billing:payg', 'billing:unknown',
  'privacy:zdr', 'privacy:log_no_train', 'privacy:train', 'privacy:unknown',
  'modality:input:text', 'modality:input:image', 'modality:input:audio', 'modality:input:video', 'modality:input:file',
  'modality:output:text', 'modality:output:image', 'modality:output:audio', 'modality:output:video',
]);

type GroupDraft = { id: string; name: string; description: string; sort_order: number; isNew: boolean };
type TagDraft = Tag & { isNew: boolean; isBuiltin: boolean };
const hasReferences = (refs?: TagReferences) => !!refs && (refs.channel_names.length > 0 || refs.downstream_models.length > 0);
const hasKeyReferences = (refs?: TagReferences) => !!refs?.client_key_names?.length;
const isProtectedTag = (tag: Tag) => BUILTIN_TAG_IDS.has(tag.id) || tag.id === `${tag.group_id}:unknown`;
function moved(ids: string[], id: string, direction: number) {
  const index = ids.indexOf(id), next = index + direction;
  if (index < 0 || next < 0 || next >= ids.length) return null;
  const order = [...ids]; [order[index], order[next]] = [order[next], order[index]]; return order;
}

type GroupRowsProps = {
  group: TagGroupWithTags; disabled: boolean; onOrder: (order: string[]) => Promise<void>;
  onEdit: (tag: Tag) => void; onDelete: (tag: Tag) => void;
};
function GroupRows({ group, disabled, onOrder, onEdit, onDelete }: GroupRowsProps) {
  const sortable = useSortableList({ ids: group.tags.map(tag => tag.id), disabled, onCommit: onOrder });
  return <div className="tags-rows" {...sortable.listProps}>
    {group.tags.map(tag => <div key={tag.id} {...sortable.itemProps(tag.id)} className="tags-row">
      <button type="button" className="tags-handle tags-icon" {...sortable.handleProps(tag.id)} disabled={disabled || group.tags.length < 2}
        aria-label={`调整标签 ${tag.name} 顺序`} title="拖动排序，或使用上下方向键"
        onKeyDown={event => { if (!disabled && !sortable.dragging && !sortable.pending && (event.key === 'ArrowUp' || event.key === 'ArrowDown')) {
          event.preventDefault(); const order = moved(group.tags.map(tag => tag.id), tag.id, event.key === 'ArrowUp' ? -1 : 1);
          if (order) void onOrder(order).catch(() => {});
        } }}><GripVertical size={16} /></button>
      <div className="tags-row-name"><TagBadge tag={tag} /><code>{tag.id}</code></div>
      <p className="tags-description">{tag.description}</p>
      <div className="tags-actions">
        <button type="button" className="tags-icon" onClick={() => onEdit(tag)} aria-label={`编辑 ${tag.name}`} title={`编辑标签 ${tag.name}`}><Edit2 size={15} /></button>
        {!isProtectedTag(tag) && <button type="button" className="tags-icon tags-danger" onClick={() => onDelete(tag)} aria-label={`删除 ${tag.name}`} title={`删除标签 ${tag.name}`}><Trash2 size={15} /></button>}
      </div>
    </div>)}
  </div>;
}

export default function Tags() {
  const confirmAction = useConfirm();
  const [groups, setGroups] = useState<TagGroupWithTags[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [editorError, setEditorError] = useState('');
  const [search, setSearch] = useState('');
  const query = search.trim().toLowerCase();
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const [editingGroup, setEditingGroup] = useState<GroupDraft | null>(null);
  const [editingTag, setEditingTag] = useState<TagDraft | null>(null);
  const [deleteConfirm, setDeleteConfirm] = useState<{ tag: Tag; references?: TagReferences; loading: boolean; error?: string } | null>(null);
  const deleteRequest = useRef(0);
  const [saving, setSaving] = useState(false);
  const [ordering, setOrdering] = useState(false);
  const mutation = useRef(false);
  const [orderMessage, setOrderMessage] = useState('');
  const baseline = useRef('');
  function openGroup(value: GroupDraft) { baseline.current = JSON.stringify(value); setEditorError(''); setEditingGroup(value); }
  function openTag(value: TagDraft) { baseline.current = JSON.stringify(value); setEditorError(''); setEditingTag(value); }
  const canLeave = useUnsavedChanges({ dirty: !!(editingGroup || editingTag) && JSON.stringify(editingGroup || editingTag) !== baseline.current, busy: saving || ordering,
    save: async () => {
      const form = document.getElementById(editingGroup ? 'tag-group-form' : 'tag-form') as HTMLFormElement | null;
      if (form && !form.reportValidity()) return false;
      return editingGroup ? handleSaveGroup() : handleSaveTag();
    },
  });
  async function closeEditor() { if (await canLeave()) { setEditingGroup(null); setEditingTag(null); } }
  async function fetchGroups() {
    try { setGroups(await api<TagGroupWithTags[]>('/tag-groups')); setError(''); }
    catch (err) { setError('读取标签失败：' + errorMessage(err)); }
    finally { setLoading(false); }
  }
  useEffect(() => { void fetchGroups(); return () => { deleteRequest.current++; }; }, []);
  async function handleResetBuiltin() {
    if (!await confirmAction({ title: '重置内置标签预设？', description: '内置标签组与核心标签的名称、颜色、说明和顺序将恢复预设，自定义标签保留。', confirmLabel: '重置内置标签' })) return;
    if (mutation.current) return;
    mutation.current = true; setSaving(true); setError('');
    try { await api('/tags/reset-builtin', { method: 'POST' }); await fetchGroups(); }
    catch (err) { setError('重置失败：' + errorMessage(err)); }
    finally { mutation.current = false; setSaving(false); }
  }
  async function handleSaveGroup(event?: React.FormEvent) {
    event?.preventDefault();
    if (!editingGroup || mutation.current) return false;
    mutation.current = true; setSaving(true); setEditorError('');
    const { isNew, ...group } = editingGroup;
    try {
      await api(isNew ? '/tag-groups' : `/tag-groups/${encodeURIComponent(group.id)}`, { method: isNew ? 'POST' : 'PUT', body: JSON.stringify(group) });
      setEditingGroup(null); await fetchGroups(); return true;
    } catch (err) { setEditorError('保存失败: ' + errorMessage(err)); return false; }
    finally { mutation.current = false; setSaving(false); }
  }
  async function handleDeleteGroup(group: TagGroupWithTags) {
    if (!await confirmAction({ title: '删除自定义标签组？', description: `删除“${group.name}”及各对象中该组的未知标记。若客户端 Key 仍引用此组，需要先调整其访问范围。此操作无法撤销。`, confirmLabel: '删除标签组' })) return;
    if (mutation.current) return;
    mutation.current = true; setSaving(true); setError('');
    try { await api(`/tag-groups/${encodeURIComponent(group.id)}`, { method: 'DELETE' }); await fetchGroups(); }
    catch (err) { setError('删除标签组失败：' + errorMessage(err)); }
    finally { mutation.current = false; setSaving(false); }
  }
  async function handleSaveTag(event?: React.FormEvent) {
    event?.preventDefault();
    if (!editingTag || mutation.current) return false;
    mutation.current = true; setSaving(true); setEditorError('');
    const { isNew, isBuiltin: _isBuiltin, ...tag } = editingTag;
    try {
      await api(isNew ? '/tags' : `/tags/${encodeURIComponent(tag.id)}`, { method: isNew ? 'POST' : 'PUT', body: JSON.stringify(tag) });
      setEditingTag(null); await fetchGroups(); return true;
    } catch (err) { setEditorError('保存失败: ' + errorMessage(err)); return false; }
    finally { mutation.current = false; setSaving(false); }
  }
  function closeDelete() { if (!saving) { deleteRequest.current++; setDeleteConfirm(null); } }
  async function handleInitiateDeleteTag(tag: Tag) {
    const token = ++deleteRequest.current;
    setDeleteConfirm({ tag, loading: true });
    try {
      const refs = await api<TagReferences>(`/tags/references?id=${encodeURIComponent(tag.id)}`);
      if (token === deleteRequest.current) setDeleteConfirm({ tag, references: refs, loading: false });
    } catch (err) {
      if (token === deleteRequest.current) setDeleteConfirm({ tag, loading: false, error: '引用检查失败，暂不能删除：' + errorMessage(err) });
    }
  }
  async function handleExecuteDeleteTag() {
    if (!deleteConfirm?.references || deleteConfirm.loading || deleteConfirm.error || hasKeyReferences(deleteConfirm.references) || mutation.current) return;
    const target = deleteConfirm;
    mutation.current = true; setSaving(true);
    try {
      await api(`/tags/${encodeURIComponent(target.tag.id)}?force=${hasReferences(target.references) ? 'true' : 'false'}`, { method: 'DELETE' });
      deleteRequest.current++; setDeleteConfirm(null); await fetchGroups();
    } catch (err) {
      setDeleteConfirm({ ...target, references: undefined, error: err instanceof ApiError && err.status === 409
        ? '标签引用已变化，请重新检查后确认删除。' : '删除失败，请重新检查后重试：' + errorMessage(err) });
    } finally { mutation.current = false; setSaving(false); }
  }
  async function saveOrder(order: string[], groupID?: string) {
    if (mutation.current || query) throw new Error('暂不能排序');
    mutation.current = true; setOrdering(true); setError(''); setOrderMessage('正在保存顺序…');
    try {
      await api(groupID === undefined ? '/tag-groups/reorder' : '/tags/reorder', { method: 'PUT', body: JSON.stringify(groupID === undefined ? { order } : { group_id: groupID, order }) });
      setGroups(current => groupID === undefined
        ? order.map((id, index) => ({ ...current.find(group => group.id === id)!, sort_order: index + 1 }))
        : current.map(group => group.id !== groupID ? group : { ...group, tags: order.map((id, index) => ({ ...group.tags.find(tag => tag.id === id)!, sort_order: index + 1 })) }));
      setOrderMessage('顺序已保存');
    } catch (err) { setOrderMessage(''); setError('顺序保存失败，已保留原顺序：' + errorMessage(err)); throw err; }
    finally { mutation.current = false; setOrdering(false); }
  }
  const sortingDisabled = !!query || saving || ordering || !!editingGroup || !!editingTag || !!deleteConfirm;
  const sortable = useSortableList({ ids: groups.map(group => group.id), disabled: sortingDisabled, onCommit: order => saveOrder(order) });
  const filteredGroups = useMemo(() => groups.flatMap(group => {
    if (!query || [group.name, group.id, group.description].some(value => value?.toLowerCase().includes(query))) return [group];
    const tags = group.tags.filter(tag => [tag.name, tag.id, tag.description].some(value => value?.toLowerCase().includes(query)));
    return tags.length ? [{ ...group, tags }] : [];
  }), [groups, query]);
  const tagCount = filteredGroups.reduce((count, group) => count + group.tags.length, 0);

  return <div className="tags-page">
    <fieldset disabled={saving || ordering} className="tags-manager">
      <header className="tags-toolbar">
        <div><h1 className="text-xl font-bold">标签配置</h1><p className="tags-subtitle">管理标签组、标签与显示顺序</p></div>
        <div className="tags-actions">
          <details className="tags-more"><summary>更多操作</summary><button type="button" className="btn btn-secondary" onClick={handleResetBuiltin}><RotateCcw size={15} />重置内置预设</button></details>
          <button type="button" className="btn btn-primary" onClick={() => openGroup({ id: '', name: '', description: '', sort_order: Math.max(0, ...groups.map(group => group.sort_order)) + 1, isNew: true })}><Plus size={16} />新建标签组</button>
        </div>
      </header>
      <div className="tags-searchbar">
        <div className="tags-search"><Search size={16} /><input aria-label="搜索标签和标签组" placeholder="搜索标签名称、ID 或描述..." value={search} onChange={event => setSearch(event.target.value)} />{search && <button type="button" className="tags-icon" aria-label="清除搜索" onClick={() => setSearch('')}><X size={16} /></button>}</div>
        <span className="tags-count" role="status">{filteredGroups.length} 个标签组 · {tagCount} 个标签{query ? '匹配' : ''}</span>
      </div>
      <p className="tags-sort-hint">{query ? '清除搜索后可调整顺序' : '拖动手柄调整顺序，也可聚焦手柄后按上下方向键'}<span role="status">{orderMessage}</span></p>
      {error && !editingGroup && !editingTag && <ErrorBox message={error} retry={() => void fetchGroups()} />}
      {loading ? <p role="status" className="tags-empty">加载标签定义中…</p> : <div className="tags-groups" {...sortable.listProps}>
        {filteredGroups.map(group => {
          const isBuiltin = group.type === 'builtin';
          const folded = !query && collapsed.has(group.id);
          return <section key={group.id} {...sortable.itemProps(group.id)} className="tags-group" aria-label={`标签组 ${group.name}`}>
            <div className="tags-group-heading">
              <button type="button" className="tags-handle tags-icon" {...sortable.handleProps(group.id)} disabled={sortingDisabled || groups.length < 2} aria-label={`调整标签组 ${group.name} 顺序`} title="拖动排序，或使用上下方向键" onKeyDown={event => {
                if (!sortingDisabled && !sortable.dragging && !sortable.pending && (event.key === 'ArrowUp' || event.key === 'ArrowDown')) {
                  event.preventDefault(); const order = moved(groups.map(value => value.id), group.id, event.key === 'ArrowUp' ? -1 : 1);
                  if (order) void saveOrder(order).catch(() => {});
                }
              }}><GripVertical size={16} /></button>
              <button type="button" className="tags-group-toggle" aria-expanded={!folded} aria-controls={`tag-list-${group.id}`} onClick={() => setCollapsed(current => { const next = new Set(current); if (next.has(group.id)) next.delete(group.id); else next.add(group.id); return next; })} disabled={!!query}>
                <ChevronDown size={16} className={folded ? 'tags-chevron-folded' : ''} /><h2>{group.name}</h2><span className="tags-count">{group.tags.length}</span>
              </button>
              <span className="tags-type">{isBuiltin && <ShieldCheck size={13} />}{isBuiltin ? '系统内置' : '用户自定义'}</span>
              <div className="tags-actions tags-group-actions">
                <button type="button" className="tags-add" onClick={() => openTag({ id: `${group.id}:`, group_id: group.id, name: '', color: 'slate', description: '', sort_order: Math.max(0, ...group.tags.map(tag => tag.sort_order)) + 1, isNew: true, isBuiltin: false })}><Plus size={15} />添加标签</button>
                <button type="button" className="tags-icon" aria-label={`编辑标签组 ${group.name}`} title="编辑组信息" onClick={() => openGroup({ id: group.id, name: group.name, description: group.description, sort_order: group.sort_order, isNew: false })}><Edit2 size={15} /></button>
                {!isBuiltin && <button type="button" className="tags-icon tags-danger" aria-label={`删除标签组 ${group.name}`} title={group.tags.some(tag => tag.id !== `${group.id}:unknown`) ? '请先删除组内标签' : '删除该组'} disabled={group.tags.some(tag => tag.id !== `${group.id}:unknown`)} onClick={() => void handleDeleteGroup(group)}><Trash2 size={15} /></button>}
              </div>
            </div>
            <div className="tags-group-meta"><code>{group.id}</code>{group.description && <span>{group.description}</span>}</div>
            <div id={`tag-list-${group.id}`} hidden={folded}>
              {group.tags.length ? <GroupRows group={group} disabled={sortingDisabled || sortable.dragging} onOrder={order => saveOrder(order, group.id)} onEdit={tag => openTag({ ...tag, isNew: false, isBuiltin: BUILTIN_TAG_IDS.has(tag.id) })} onDelete={tag => void handleInitiateDeleteTag(tag)} /> : <p className="tags-empty">该组暂无标签，可点击“添加标签”创建。</p>}
            </div>
          </section>;
        })}
        {!filteredGroups.length && <div className="tags-empty">{query ? <><p>没有匹配的标签或标签组</p><button type="button" className="btn btn-secondary" onClick={() => setSearch('')}>清除搜索</button></> : <p>尚未创建标签组，点击“新建标签组”开始。</p>}</div>}
      </div>}
    </fieldset>
      {/* 编辑组弹窗 */}
      {editingGroup && (
        <Modal title={editingGroup?.isNew ? '新建标签组' : '编辑标签组'} onClose={closeEditor} busy={saving}>
          <form
            id="tag-group-form" onSubmit={handleSaveGroup}
            className="space-y-4"
          >
            {editorError && <ErrorBox message={editorError} />}<fieldset disabled={saving} className="space-y-4">

            <div className="space-y-3">
              <div>
                <label htmlFor="group-field-0" className="block text-xs font-medium mb-1">标签组 ID</label>
                <input id="group-field-0"
                  type="text"
                  disabled={!editingGroup.isNew}
                  value={editingGroup.id}
                  onChange={e => setEditingGroup({ ...editingGroup, id: e.target.value })}
                  placeholder="如 scenario, domain"
                  className="w-full px-3 py-1.5 text-sm font-mono rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] disabled:opacity-60 focus:outline-none"
                  required
                />
                <span className="text-xs text-[var(--color-text-secondary)]">创建后不可修改</span>
              </div>

              <div>
                <label htmlFor="group-field-1" className="block text-xs font-medium mb-1">显示名称</label>
                <input id="group-field-1"
                  type="text"
                  value={editingGroup.name}
                  onChange={e => setEditingGroup({ ...editingGroup, name: e.target.value })}
                  placeholder="如 使用场景, 业务线"
                  className="w-full px-3 py-1.5 text-sm rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] focus:outline-none"
                  required
                />
              </div>

              <div>
                <label htmlFor="group-field-2" className="block text-xs font-medium mb-1">组说明</label>
                <textarea id="group-field-2"
                  value={editingGroup.description}
                  onChange={e => setEditingGroup({ ...editingGroup, description: e.target.value })}
                  rows={2}
                  className="w-full px-3 py-1.5 text-sm rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] focus:outline-none"
                />
              </div>
            </div>

            <div className="flex justify-end gap-2 pt-2">
              <Button type="button" variant="secondary" onClick={closeEditor}>取消</Button>
              <Button type="submit" variant="primary" disabled={saving}>
                {saving ? '保存中...' : '保存'}
              </Button>
            </div>
          </fieldset></form>
        </Modal>
      )}

      {/* 编辑/新建标签弹窗 */}
      {editingTag && (
        <Modal title={editingTag?.isNew ? '新建标签' : '编辑标签'} onClose={closeEditor} busy={saving}>
          <form
            id="tag-form" onSubmit={handleSaveTag}
            className="space-y-4"
          >
            {editorError && <ErrorBox message={editorError} />}<fieldset disabled={saving} className="space-y-4">

            <div className="space-y-3">
              <div>
                <label htmlFor="tag-field-0" className="block text-xs font-medium mb-1">标签 ID</label>
                <input id="tag-field-0"
                  type="text"
                  disabled={!editingTag.isNew}
                  value={editingTag.id}
                  onChange={e => setEditingTag({ ...editingTag, id: e.target.value })}
                  placeholder="如 billing:custom 或 scenario:test"
                  className="w-full px-3 py-1.5 text-sm font-mono rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] disabled:opacity-60 focus:outline-none"
                  required
                />
                <span className="text-xs text-[var(--color-text-secondary)]">创建后作为全局唯一标识不可修改</span>
              </div>

              <div>
                <label htmlFor="tag-field-1" className="block text-xs font-medium mb-1">显示名称</label>
                <input id="tag-field-1"
                  type="text"
                  value={editingTag.name}
                  onChange={e => setEditingTag({ ...editingTag, name: e.target.value })}
                  placeholder="如 免费 / 赠额"
                  className="w-full px-3 py-1.5 text-sm rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] focus:outline-none"
                  required
                />
              </div>

              <div>
                <label className="block text-xs font-medium mb-1">主题色彩</label>
                <div className="flex items-center gap-2 flex-wrap">
                  {colorOptions.map(opt => (
                    <button
                      key={opt.id}
                      type="button"
                      onClick={() => setEditingTag({ ...editingTag, color: opt.id })}
                      aria-pressed={editingTag.color === opt.id} className={`inline-flex items-center gap-1.5 px-2.5 py-1 rounded text-xs border transition-all ${
                        editingTag.color === opt.id
                          ? 'border-[var(--brand-blue)] ring-1 ring-[var(--brand-blue)] font-semibold'
                          : 'border-[var(--color-border)] hover:bg-[var(--color-surface-hover)]'
                      }`}
                    >
                      <span className={`w-2.5 h-2.5 rounded-full ${opt.bg}`} />
                      {opt.label}
                    </button>
                  ))}
                </div>
              </div>

              <div>
                <label htmlFor="tag-field-2" className="block text-xs font-medium mb-1">详细说明</label>
                <textarea id="tag-field-2"
                  value={editingTag.description}
                  onChange={e => setEditingTag({ ...editingTag, description: e.target.value })}
                  rows={2}
                  className="w-full px-3 py-1.5 text-sm rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] focus:outline-none"
                  placeholder="关于该标签适用的场景或上游条款说明..."
                />
              </div>

              <div className="pt-1">
                <span className="text-xs text-[var(--color-text-muted)]">实时预览：</span>
                <div className="mt-1">
                  <TagBadge tag={{ ...editingTag } as Tag} />
                </div>
              </div>
            </div>

            <div className="flex justify-end gap-2 pt-2">
              <Button type="button" variant="secondary" onClick={closeEditor}>取消</Button>
              <Button type="submit" variant="primary" disabled={saving}>
                {saving ? '保存中...' : '保存'}
              </Button>
            </div>
          </fieldset></form>
        </Modal>
      )}

      {deleteConfirm && <Modal title="确认删除标签？" onClose={closeDelete} busy={saving}>
        <div className="tags-delete space-y-4">
          <p>删除标签 <TagBadge tag={deleteConfirm.tag} />？此操作无法撤销。</p>
          {deleteConfirm.loading ? <p role="status">正在检查受影响的渠道与模型引用…</p>
            : deleteConfirm.error ? <ErrorBox message={deleteConfirm.error} retry={() => void handleInitiateDeleteTag(deleteConfirm.tag)} />
            : hasKeyReferences(deleteConfirm.references) ? <div className="tags-reference-list"><p>此标签用于客户端 Key 访问规则，暂不能删除。</p><p>客户端 Key：{deleteConfirm.references?.client_key_names.join('、')}</p><p>请先在“客户端接入”编辑这些 Key 的访问范围，再返回删除，避免意外扩大权限。</p></div>
            : hasReferences(deleteConfirm.references) ? <div className="tags-reference-list">
              <p>检测到该标签正被以下对象使用：</p>
              {!!deleteConfirm.references?.channel_names.length && <p>渠道：{deleteConfirm.references.channel_names.join('、')}</p>}
              {!!deleteConfirm.references?.downstream_models.length && <p>公开模型：{deleteConfirm.references.downstream_models.join('、')}</p>}
              <p>确认后将从引用它的渠道和模型中解绑，并删除标签。</p>
            </div> : <p>该标签未被任何渠道或模型使用，可安全删除。</p>}
          <div className="flex justify-end gap-2"><button type="button" className="btn btn-secondary" onClick={closeDelete} disabled={saving}>取消</button>
            <button type="button" className="btn btn-danger" disabled={saving || deleteConfirm.loading || !!deleteConfirm.error || !deleteConfirm.references || hasKeyReferences(deleteConfirm.references)} onClick={() => void handleExecuteDeleteTag()}>{saving ? '删除中…' : hasKeyReferences(deleteConfirm.references) ? '请先处理 Key 引用' : hasReferences(deleteConfirm.references) ? '确认删除并解绑' : '确认删除'}</button>
          </div>
        </div>
      </Modal>}
  </div>;
}
