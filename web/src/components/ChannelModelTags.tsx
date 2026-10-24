import { useMemo, useState } from 'react';
import type { Channel, TagGroupWithTags } from '../api';
import { useResource } from '../hooks';
import { editableTagGroups, groupTagIDs } from '../services/tagAssignment';
import { TagGroupOptions } from './TagGroupSelector';
import { Button, ErrorBox, Input, Select } from './ui';
import './ChannelModelTags.css';

export function ChannelModelTags({ channel, onChange }: { channel: Channel; onChange: (channel: Channel) => void }) {
  const resource = useResource<TagGroupWithTags[]>('/tag-groups');
  const groups = useMemo(() => editableTagGroups(resource.data || []), [resource.data]);
  const [search, setSearch] = useState('');
  const [showDisabled, setShowDisabled] = useState(false);
  const [selected, setSelected] = useState<string[]>([]);
  const [opened, setOpened] = useState<Set<string>>(new Set());
  const [bulkGroup, setBulkGroup] = useState('');
  const [bulkMode, setBulkMode] = useState<'independent' | 'follow'>('independent');
  const [bulkTags, setBulkTags] = useState<string[]>([]);
  const [message, setMessage] = useState('');
  const allModels = useMemo(() => [...new Set([...channel.models, ...Object.keys(channel.model_tags || {}), ...(showDisabled ? channel.available_models || [] : [])])], [channel.models, channel.model_tags, channel.available_models, showDisabled]);
  const visible = allModels.filter(id => (showDisabled || channel.models.includes(id)) && id.toLowerCase().includes(search.toLowerCase()));
  const targets = visible.filter(id => selected.includes(id) && channel.models.includes(id));
  const chosenGroup = groups.find(group => group.id === bulkGroup) || groups[0];
  function groupValues(values: string[] | undefined, group: TagGroupWithTags) {
    const ids = groupTagIDs(group);
    return (values || []).filter(id => ids.has(id));
  }
  function replaceGroup(modelIDs: string[], group: TagGroupWithTags, values: string[]) {
    const ids = groupTagIDs(group), next = { ...channel.model_tags };
    modelIDs.forEach(id => {
      const tags = [...(next[id] || []).filter(value => !ids.has(value)), ...values];
      if (tags.length) next[id] = tags; else delete next[id];
    });
    onChange({ ...channel, model_tags: next });
  }
  function valuesText(values: string[], group: TagGroupWithTags) {
    return values.length ? values.map(id => group.tags.find(tag => tag.id === id)?.name || id).join('、') : '未知';
  }
  if (resource.error) return <ErrorBox message={`标签加载失败：${resource.error}`} retry={resource.reload} />;
  if (resource.loading && !resource.data) return <p role="status" className="channel-editor-note">加载标签组中…</p>;
  return <div className="channel-model-tags">
    <section><h3>渠道默认标签</h3><p className="channel-editor-note">模型按组跟随此处默认值；模型独立设置的组不受默认值变化影响。</p>
      <TagGroupOptions groups={groups} selectedTags={channel.tags || []} onChange={tags => onChange({ ...channel, tags })} assignment />
    </section>
    <section><h3>模型标签</h3><p className="channel-editor-note">展开模型可按组独立设置，选择“未知”也会停止跟随。与渠道一起保存。</p>
      <div className="channel-model-tag-toolbar"><Input aria-label="搜索模型标签" placeholder="搜索模型 ID…" value={search} onChange={event => setSearch(event.target.value)} /><label><input type="checkbox" checked={showDisabled} onChange={event => setShowDisabled(event.target.checked)} />显示停用模型</label></div>
      <div className="channel-tag-bulk"><label className="channel-tag-select-all"><input type="checkbox" aria-label="选择可见已启用模型标签" checked={!!visible.filter(id => channel.models.includes(id)).length && visible.filter(id => channel.models.includes(id)).every(id => selected.includes(id))} ref={node => { if (node) { const enabled = visible.filter(id => channel.models.includes(id)); node.indeterminate = enabled.some(id => selected.includes(id)) && !enabled.every(id => selected.includes(id)); } }} onChange={event => setSelected(current => { const enabled = visible.filter(id => channel.models.includes(id)); return event.target.checked ? [...new Set([...current, ...enabled])] : current.filter(id => !enabled.includes(id)); })} />当前可见已选 {targets.length} 个已启用模型</label>
        {!!targets.length && chosenGroup && <div className="channel-tag-bulk-editor"><Select aria-label="批量标签组" value={chosenGroup.id} onChange={event => { setBulkGroup(event.target.value); setBulkTags([]); }} >{groups.map(group => <option key={group.id} value={group.id}>{group.name}</option>)}</Select><Select aria-label="批量标签来源" value={bulkMode} onChange={event => setBulkMode(event.target.value as typeof bulkMode)}><option value="independent">独立设置</option><option value="follow">恢复跟随渠道</option></Select>
          {bulkMode === 'independent' && <TagGroupOptions groups={[chosenGroup]} selectedTags={bulkTags} onChange={setBulkTags} assignment />}
          <Button type="button" variant="secondary" onClick={() => { replaceGroup(targets, chosenGroup, bulkMode === 'follow' ? [] : bulkTags.length ? bulkTags : [`${chosenGroup.id}:unknown`]); setMessage(`已更新 ${targets.length} 个可见已选模型的${chosenGroup.name}，保存渠道后生效。`); }}>应用到这 {targets.length} 个模型</Button>
        </div>}
        {message && <p role="status" className="channel-editor-note">{message}</p>}
      </div>
      <div className="channel-model-tag-list">{visible.map(id => {
        const own = channel.model_tags?.[id] || [], enabled = channel.models.includes(id);
        return <div className="channel-model-tag-row" key={id}><input type="checkbox" aria-label={`选择模型标签 ${id}`} disabled={!enabled} checked={selected.includes(id)} onChange={() => setSelected(current => current.includes(id) ? current.filter(value => value !== id) : [...current, id])} />
          <details open={opened.has(id)} onToggle={event => { const isOpen = event.currentTarget.open; setOpened(current => { if (current.has(id) === isOpen) return current; const next = new Set(current); if (isOpen) next.add(id); else next.delete(id); return next; }); }}><summary><span>{id}{!enabled && '（已停用）'}</span><span className="channel-model-tag-summary">{groups.map(group => { const specific = groupValues(own, group); return `${group.name}：${valuesText(specific.length ? specific : groupValues(channel.tags, group), group)}${specific.length ? ' · 独立' : ' · 跟随'}`; }).join('；')}</span></summary>
            {opened.has(id) && <div className="channel-model-tag-groups">{groups.map(group => {
              const specific = groupValues(own, group), defaults = groupValues(channel.tags, group);
              return <div key={group.id} className="channel-model-tag-group"><div className="channel-model-tag-source"><strong>{group.name}</strong><span>{specific.length ? '独立设置' : `跟随渠道 · ${valuesText(defaults, group)}`}</span>{!!specific.length && <Button type="button" variant="ghost" onClick={() => replaceGroup([id], group, [])}>恢复跟随</Button>}</div><TagGroupOptions groups={[group]} selectedTags={specific.length ? specific : defaults} onChange={tags => replaceGroup([id], group, tags)} assignment /></div>;
            })}</div>}
          </details>
        </div>;
      })}{!visible.length && <p className="channel-editor-note">没有匹配的模型。停用配置会保留，可勾选“显示停用模型”维护。</p>}</div>
    </section>
    <p className="channel-editor-note">标签只描述模型条件，不保证剩余额度或免于额外费用。混合计费／隐私条件的凭据请分渠道配置，或标为未知。</p>
  </div>;
}
