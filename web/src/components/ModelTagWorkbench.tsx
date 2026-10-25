import { useEffect, useMemo, useState } from 'react';
import { Edit2, Search, Tags as TagsIcon, X } from 'lucide-react';
import { api, errorMessage, type Channel, type TagGroupWithTags } from '../api';
import { useResource } from '../hooks';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { editableTagGroups, groupTagIDs } from '../services/tagAssignment';
import { channelPayload } from '../services/formatters';
import { managementTarget } from '../services/managementLinks';
import { TagBadge } from './TagBadge';
import { TagGroupOptions } from './TagGroupSelector';
import { Button, Empty, ErrorBox, Loading, Modal, Select, useToast } from './ui';
import './ModelTagWorkbench.css';

const channelSelectionKey = 'to-router.model-tags-channels.v1';

function savedChannels(): number[] | null {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(channelSelectionKey) || 'null');
    return Array.isArray(value) ? value.filter((id): id is number => Number.isInteger(id) && id > 0) : null;
  } catch {
    return null;
  }
}

export interface ModelTagRow {
  channel_id: number;
  channel_name: string;
  model: string;
  enabled: boolean;
  channel_tags: string[];
  model_tags?: string[];
  channel: Channel;
}

function rowKey(row: { channel_id: number; model: string }) {
  return `${row.channel_id}:${row.model}`;
}

function groupValues(values: string[] | undefined, group: TagGroupWithTags): string[] {
  const ids = groupTagIDs(group);
  return (values || []).filter(id => ids.has(id));
}

function valuesText(values: string[], group: TagGroupWithTags): string {
  if (!values.length) return '未知';
  return values.map(id => {
    if (id === `${group.id}:unknown`) return '未知';
    const tag = group.tags.find(t => t.id === id);
    return tag?.name || id;
  }).join('、');
}

/** 单模型标签编辑弹窗 */
function ModelTagDialog({
  entry,
  groups,
  onClose,
  onSaved,
}: {
  entry: ModelTagRow;
  groups: TagGroupWithTags[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [draftTags, setDraftTags] = useState<string[]>(() => entry.model_tags || []);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const toast = useToast();

  const isDirty = JSON.stringify(draftTags.slice().sort()) !== JSON.stringify((entry.model_tags || []).slice().sort());
  const canLeave = useUnsavedChanges({ dirty: isDirty, busy, save });

  async function close() {
    if (await canLeave()) onClose();
  }

  function handleRevertGroup(group: TagGroupWithTags) {
    const ids = groupTagIDs(group);
    setDraftTags(current => current.filter(id => !ids.has(id)));
  }

  function handleUpdateGroupTags(group: TagGroupWithTags, newTags: string[]) {
    const ids = groupTagIDs(group);
    // 过滤掉当前组旧标签，并添加新标签
    setDraftTags(current => [...current.filter(id => !ids.has(id)), ...newTags]);
  }

  async function save() {
    setBusy(true);
    setError('');
    try {
      const nextModelTags = { ...(entry.channel.model_tags || {}) };
      if (draftTags.length > 0) {
        nextModelTags[entry.model] = draftTags;
      } else {
        delete nextModelTags[entry.model];
      }
      const updatedChannel: Channel = {
        ...entry.channel,
        model_tags: Object.keys(nextModelTags).length > 0 ? nextModelTags : undefined,
      };
      await api(`/channels/${encodeURIComponent(entry.channel.id)}`, {
        method: 'PUT',
        body: JSON.stringify(channelPayload(updatedChannel)),
      });
      toast(`模型 ${entry.model} 标签已保存`);
      onSaved();
      return true;
    } catch (err) {
      setError(errorMessage(err));
      return false;
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      title={`编辑模型标签 · ${entry.model}`}
      description={`所属渠道：${entry.channel_name}`}
      onClose={close}
      busy={busy}
      wide
      className="model-tag-editor-dialog"
    >
      <div className="model-tag-editor-body">
        {error && <ErrorBox message={error} />}
        {groups.map(group => {
          const specific = groupValues(draftTags, group);
          const defaults = groupValues(entry.channel_tags, group);
          const isIndependent = specific.length > 0;
          const activeTags = isIndependent ? specific : defaults;

          return (
            <div key={group.id} className="model-tag-group-edit-section">
              <div className="model-tag-group-edit-header">
                <div>
                  <strong>{group.name}</strong>
                  <span className="model-tag-group-edit-source ml-2">
                    {isIndependent ? '独立设置' : `跟随渠道 · ${valuesText(defaults, group)}`}
                  </span>
                </div>
                {isIndependent && (
                  <Button
                    type="button"
                    variant="ghost"
                    disabled={busy}
                    aria-label={`恢复跟随 ${group.name}`}
                    onClick={() => handleRevertGroup(group)}
                  >
                    恢复跟随渠道
                  </Button>
                )}
              </div>
              <TagGroupOptions
                groups={[group]}
                selectedTags={activeTags}
                onChange={newTags => handleUpdateGroupTags(group, newTags)}
                assignment
              />
            </div>
          );
        })}
      </div>
      <div className="modal-footer">
        <Button variant="secondary" disabled={busy} onClick={close}>
          取消
        </Button>
        <Button disabled={busy} onClick={() => void save()}>
          {busy ? '正在保存…' : '保存标签'}
        </Button>
      </div>
    </Modal>
  );
}

/** 批量模型标签设置弹窗 */
function BulkTagDialog({
  targets,
  groups,
  onClose,
  onSaved,
}: {
  targets: ModelTagRow[];
  groups: TagGroupWithTags[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [selectedGroupId, setSelectedGroupId] = useState(groups[0]?.id || '');
  const [mode, setMode] = useState<'independent' | 'follow'>('independent');
  const [bulkTags, setBulkTags] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const toast = useToast();

  const chosenGroup = groups.find(g => g.id === selectedGroupId) || groups[0];
  const affectedChannels = useMemo(() => {
    const map = new Map<number, Channel>();
    targets.forEach(t => map.set(t.channel.id, t.channel));
    return Array.from(map.values());
  }, [targets]);

  async function applyBulk() {
    if (!chosenGroup) return;
    setBusy(true);
    setError('');
    try {
      const groupIds = groupTagIDs(chosenGroup);
      const valuesToApply = mode === 'follow' ? [] : (bulkTags.length ? bulkTags : [`${chosenGroup.id}:unknown`]);

      // 按渠道分组更新
      const promises = affectedChannels.map(channel => {
        const nextModelTags = { ...(channel.model_tags || {}) };
        const channelTargets = targets.filter(t => t.channel.id === channel.id);

        channelTargets.forEach(target => {
          const currentTags = nextModelTags[target.model] || [];
          const remainingTags = currentTags.filter(id => !groupIds.has(id));
          const newTags = [...remainingTags, ...valuesToApply];
          if (newTags.length > 0) {
            nextModelTags[target.model] = newTags;
          } else {
            delete nextModelTags[target.model];
          }
        });

        const updatedChannel: Channel = {
          ...channel,
          model_tags: Object.keys(nextModelTags).length > 0 ? nextModelTags : undefined,
        };

        return api(`/channels/${encodeURIComponent(channel.id)}`, {
          method: 'PUT',
          body: JSON.stringify(channelPayload(updatedChannel)),
        });
      });

      await Promise.all(promises);
      toast(`已批量更新 ${targets.length} 个模型的 ${chosenGroup.name}`);
      onSaved();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      title="批量设置模型标签"
      description={`已选择 ${targets.length} 个模型（涉及 ${affectedChannels.length} 个渠道）`}
      onClose={onClose}
      busy={busy}
      className="model-tag-bulk-dialog"
    >
      <div className="model-tag-bulk-body">
        {error && <ErrorBox message={error} />}
        <div className="model-tag-bulk-field">
          <label htmlFor="bulk-tag-group-select">目标标签组</label>
          <Select
            id="bulk-tag-group-select"
            aria-label="选择批量标签组"
            disabled={busy}
            value={selectedGroupId}
            onChange={e => {
              setSelectedGroupId(e.target.value);
              setBulkTags([]);
            }}
          >
            {groups.map(group => (
              <option key={group.id} value={group.id}>
                {group.name}
              </option>
            ))}
          </Select>
        </div>

        <div className="model-tag-bulk-field">
          <label htmlFor="bulk-tag-mode-select">配置方式</label>
          <Select
            id="bulk-tag-mode-select"
            aria-label="选择配置方式"
            disabled={busy}
            value={mode}
            onChange={e => setMode(e.target.value as typeof mode)}
          >
            <option value="independent">独立设置指定标签</option>
            <option value="follow">恢复跟随各渠道默认标签</option>
          </Select>
        </div>

        {mode === 'independent' && chosenGroup && (
          <div className="model-tag-bulk-field">
            <label>选择标签值</label>
            <TagGroupOptions
              groups={[chosenGroup]}
              selectedTags={bulkTags}
              onChange={setBulkTags}
              assignment
            />
          </div>
        )}
      </div>
      <div className="modal-footer">
        <Button variant="secondary" disabled={busy} onClick={onClose}>
          取消
        </Button>
        <Button disabled={busy} onClick={() => void applyBulk()}>
          {busy ? '正在应用…' : `应用到这 ${targets.length} 个模型`}
        </Button>
      </div>
    </Modal>
  );
}

export function ModelTagWorkbench() {
  const channelsResource = useResource<Channel[]>('/channels');
  const tagGroupsResource = useResource<TagGroupWithTags[]>('/tag-groups');

  const [target] = useState(managementTarget);
  const [selectedChannels, setSelectedChannels] = useState<number[] | null>(() =>
    target.channelID == null ? savedChannels() : [target.channelID]
  );
  const [search, setSearch] = useState(target.model);
  const [showDisabled, setShowDisabled] = useState(false);
  const [selectedRows, setSelectedRows] = useState<Set<string>>(() => new Set());
  const [editingEntry, setEditingEntry] = useState<ModelTagRow | null>(null);
  const [bulkEditing, setBulkEditing] = useState(false);

  useEffect(() => {
    if (selectedChannels === null) return;
    try {
      localStorage.setItem(channelSelectionKey, JSON.stringify(selectedChannels));
    } catch {
      // Optional browser storage.
    }
  }, [selectedChannels]);

  const editableGroups = useMemo(
    () => editableTagGroups(tagGroupsResource.data || []),
    [tagGroupsResource.data]
  );

  const channels = useMemo(() => {
    return (channelsResource.data || [])
      .map(ch => ({ id: ch.id, name: ch.name || String(ch.id), channel: ch }))
      .sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id);
  }, [channelsResource.data]);

  const channelIDs = useMemo(() => channels.map(c => c.id), [channels]);
  const displayedChannels = selectedChannels ?? channelIDs;
  const displayedCount = channelIDs.filter(id => displayedChannels.includes(id)).length;

  const allRows: ModelTagRow[] = useMemo(() => {
    const list: ModelTagRow[] = [];
    for (const c of channels) {
      const channel = c.channel;
      const modelSet = new Set([
        ...channel.models,
        ...Object.keys(channel.model_tags || {}),
        ...(showDisabled ? channel.available_models || [] : []),
      ]);
      for (const model of modelSet) {
        list.push({
          channel_id: channel.id,
          channel_name: c.name,
          model,
          enabled: channel.models.includes(model),
          channel_tags: channel.tags || [],
          model_tags: channel.model_tags?.[model],
          channel,
        });
      }
    }
    return list;
  }, [channels, showDisabled]);

  const visible = useMemo(() => {
    const query = search.trim().toLowerCase();
    return allRows
      .filter(row => {
        if (!displayedChannels.includes(row.channel_id)) return false;
        if (!showDisabled && !row.enabled) return false;
        if (!query) return true;
        return `${row.model} ${row.channel_name}`.toLowerCase().includes(query);
      })
      .sort(
        (a, b) =>
          a.channel_name.localeCompare(b.channel_name) ||
          a.channel_id - b.channel_id ||
          a.model.localeCompare(b.model)
      );
  }, [allRows, displayedChannels, showDisabled, search]);

  const selectedVisible = useMemo(
    () => visible.filter(row => selectedRows.has(rowKey(row))),
    [visible, selectedRows]
  );

  function selectVisible(checked: boolean) {
    setSelectedRows(previous => {
      const next = new Set(previous);
      visible.forEach(row => {
        if (checked) next.add(rowKey(row));
        else next.delete(rowKey(row));
      });
      return next;
    });
  }

  const ready = !channelsResource.loading && !tagGroupsResource.loading && !channelsResource.error && !tagGroupsResource.error;

  return (
    <div className="model-tags-workbench-view">
      <div className="tags-searchbar model-tags-searchbar">
        <div className="tags-search">
          <Search size={16} />
          <input
            aria-label="搜索模型标签"
            placeholder="搜索模型 ID / 渠道..."
            value={search}
            onChange={event => setSearch(event.target.value)}
          />
          {search && (
            <button
              type="button"
              className="tags-icon"
              aria-label="清除搜索"
              onClick={() => setSearch('')}
            >
              <X size={16} />
            </button>
          )}
        </div>
        <div className="model-tags-search-actions">
          <label className="model-tags-show-disabled">
            <input
              type="checkbox"
              checked={showDisabled}
              onChange={e => setShowDisabled(e.target.checked)}
            />
            <span>显示停用模型</span>
          </label>
          <span className="tags-count" role="status">
            {visible.length} 个模型 · 已选 {selectedVisible.length} 项
          </span>
          <Button
            disabled={!selectedVisible.length || !ready}
            onClick={() => setBulkEditing(true)}
          >
            <TagsIcon size={15} />
            {`批量设置标签（${selectedVisible.length}）`}
          </Button>
        </div>
      </div>

      <section className="panel model-tags-workbench">

      <details className="model-tags-channels" open>
        <summary>显示渠道 · {displayedCount} / {channels.length}</summary>
        <div className="model-tags-channel-options">
          <label>
            <input
              type="checkbox"
              className="checkbox"
              aria-label="全选渠道"
              disabled={!channels.length}
              checked={!!channels.length && displayedCount === channels.length}
              ref={el => {
                if (el) el.indeterminate = displayedCount > 0 && displayedCount < channels.length;
              }}
              onChange={e => setSelectedChannels(e.target.checked ? channelIDs : [])}
            />
            <span>全选渠道</span>
          </label>
          {channels.map(channel => (
            <label key={channel.id}>
              <input
                type="checkbox"
                className="checkbox"
                aria-label={`显示渠道 ${channel.name}`}
                checked={displayedChannels.includes(channel.id)}
                onChange={e =>
                  setSelectedChannels(
                    e.target.checked
                      ? [...displayedChannels, channel.id]
                      : displayedChannels.filter(id => id !== channel.id)
                  )
                }
              />
              <span>{channel.name}</span>
            </label>
          ))}
        </div>
      </details>

      {channelsResource.loading || tagGroupsResource.loading ? (
        <Loading />
      ) : channelsResource.error ? (
        <ErrorBox message={channelsResource.error} retry={channelsResource.reload} />
      ) : tagGroupsResource.error ? (
        <ErrorBox message={tagGroupsResource.error} retry={tagGroupsResource.reload} />
      ) : !visible.length ? (
        <Empty
          title="暂无匹配的模型"
          description={
            !displayedCount
              ? '请选择要显示的渠道。'
              : '调整搜索条件，或在上游接入页启用模型。'
          }
        />
      ) : (
        <div className="table-scroll">
          <table className="data-table model-tags-table">
            <thead>
              <tr>
                <th className="model-tags-selection">
                  <label>
                    <input
                      type="checkbox"
                      className="checkbox"
                      aria-label="全选可见模型"
                      checked={visible.length > 0 && selectedVisible.length === visible.length}
                      ref={el => {
                        if (el) el.indeterminate = selectedVisible.length > 0 && selectedVisible.length < visible.length;
                      }}
                      onChange={e => selectVisible(e.target.checked)}
                    />
                  </label>
                </th>
                <th>渠道</th>
                <th>模型 ID</th>
                <th>标签分配状态</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {visible.map(row => {
                const key = rowKey(row);
                const isSelected = selectedRows.has(key);

                return (
                  <tr key={key} data-selected={isSelected}>
                    <td className="model-tags-selection">
                      <label>
                        <input
                          type="checkbox"
                          className="checkbox"
                          aria-label={`选择模型 ${row.model}`}
                          checked={isSelected}
                          onChange={e =>
                            setSelectedRows(prev => {
                              const next = new Set(prev);
                              if (e.target.checked) next.add(key);
                              else next.delete(key);
                              return next;
                            })
                          }
                        />
                      </label>
                    </td>
                    <td className="model-tags-channel-name">{row.channel_name}</td>
                    <td className="model-tags-model-id">
                      {row.model}
                      {!row.enabled && <span className="model-tags-disabled-badge">（已停用）</span>}
                    </td>
                    <td>
                      <div className="model-tags-summary">
                        {editableGroups.map(group => {
                          const specific = groupValues(row.model_tags, group);
                          const defaults = groupValues(row.channel_tags, group);
                          const isIndependent = specific.length > 0;
                          const effective = isIndependent ? specific : defaults;

                          return (
                            <div key={group.id} className="model-tag-group-row">
                              <span className="model-tag-group-label">{group.name}：</span>
                              <div className="model-tag-badges">
                                {effective.length > 0 ? (
                                  effective.map(id => {
                                    if (id === `${group.id}:unknown`) {
                                      return (
                                        <span key={id} className="text-xs text-[var(--color-text-muted)]">
                                          未知
                                        </span>
                                      );
                                    }
                                    const tag = group.tags.find(t => t.id === id);
                                    return tag ? (
                                      <TagBadge key={id} tag={tag} size="sm" />
                                    ) : (
                                      <span key={id} className="text-xs">
                                        {id}
                                      </span>
                                    );
                                  })
                                ) : (
                                  <span className="model-tag-none">未知</span>
                                )}
                                <span
                                  className={`model-tag-source-badge ${
                                    isIndependent ? 'independent' : 'follow'
                                  }`}
                                  title={isIndependent ? '该模型独立自定义设置' : '跟随渠道默认标签设置'}
                                >
                                  {isIndependent ? '独立' : '跟随'}
                                </span>
                              </div>
                            </div>
                          );
                        })}
                      </div>
                    </td>
                    <td>
                      <Button
                        variant="ghost"
                        aria-label={`编辑模型标签 ${row.model}`}
                        onClick={() => setEditingEntry(row)}
                      >
                        <Edit2 size={14} />
                        编辑
                      </Button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {editingEntry && (
        <ModelTagDialog
          entry={editingEntry}
          groups={editableGroups}
          onClose={() => setEditingEntry(null)}
          onSaved={() => {
            setEditingEntry(null);
            channelsResource.reload();
          }}
        />
      )}

      {bulkEditing && (
        <BulkTagDialog
          targets={selectedVisible}
          groups={editableGroups}
          onClose={() => setBulkEditing(false)}
          onSaved={() => {
            setBulkEditing(false);
            setSelectedRows(new Set());
            channelsResource.reload();
          }}
        />
      )}
    </section>
  </div>
  );
}
