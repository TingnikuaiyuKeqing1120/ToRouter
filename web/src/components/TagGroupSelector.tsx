import { useEffect, useState, useMemo } from 'react';
import { api } from '../api';
import { ErrorBox } from './ui';
import type { TagGroupWithTags } from '../types';
import { toggleAssignedTag } from '../services/tagAssignment';
import './TagGroupSelector.css';

export interface TagGroupSelectorProps {
  selectedTags: string[];
  onChange: (tags: string[]) => void;
  label?: string;
  hint?: string;
  excludeGroupIds?: string[];
  showEmptyState?: boolean;
  groupType?: TagGroupWithTags['type'];
}
const noExcludedGroups: string[] = [];

export function TagGroupSelector({
  selectedTags,
  onChange,
  label = '标签选择',
  hint,
  excludeGroupIds = noExcludedGroups,
  showEmptyState = false,
  groupType,
}: TagGroupSelectorProps) {
  const [groups, setGroups] = useState<TagGroupWithTags[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let active = true;
    setLoading(true); setError(false);
    api<TagGroupWithTags[]>('/tag-groups')
      .then(data => {
        if (active) {
          setGroups(Array.isArray(data) ? data : []);
          setLoading(false);
        }
      })
      .catch(() => {
        if (active) {
          setError(true);
          setLoading(false);
        }
      });
    return () => {
      active = false;
    };
  }, [attempt]);

  const visibleGroups = useMemo(() => {
    if (!Array.isArray(groups)) return [];
    return groups.filter(g => g.id !== 'modality' && (!groupType || g.type === groupType) && !excludeGroupIds.includes(g.id) && Array.isArray(g.tags) && g.tags.length > 0);
  }, [groups, excludeGroupIds, groupType]);

  if (loading) {
    return (
      <div className="space-y-1.5 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)]/60 p-3 text-xs text-[var(--color-text-muted)]">
        加载标签组中...
      </div>
    );
  }

  if (error) return <ErrorBox message="标签加载失败，请重试。" retry={() => setAttempt(current => current + 1)} />;
  if (visibleGroups.length === 0) return showEmptyState ? <p className="text-xs text-[var(--color-text-secondary)]">暂无可选标签，可在“标签配置”中创建。</p> : null;

  return <div className="tag-selector">
    {(label || hint) && <div className="tag-selector-heading">{label && <span>{label}</span>}{hint && <span className="tag-selector-note">{hint}</span>}</div>}
    <TagGroupOptions groups={visibleGroups} selectedTags={selectedTags} onChange={onChange} assignment />
  </div>;
}

/** Shared grouped choices for assigning tags and filtering by tags. */
export function TagGroupOptions({ groups, selectedTags, onChange, assignment = false }: { groups: TagGroupWithTags[]; selectedTags: string[]; onChange: (tags: string[]) => void; assignment?: boolean }) {
  const toggleTag = (tagId: string, group: TagGroupWithTags) => {
    if (assignment) { onChange(toggleAssignedTag(selectedTags, group, tagId)); return; }
    const next = selectedTags.includes(tagId)
      ? selectedTags.filter(id => id !== tagId)
      : [...selectedTags, tagId];
    onChange(next);
  };

  return (
    <div className="tag-group-options">
        {groups.filter(group => group.tags.length > 0).map(group => (
          <div key={group.id} className="tag-option-group" role="group" aria-label={group.name}>
            <span className="tag-option-group-name">{group.name}</span>
            <div className="tag-option-items">
              {group.tags.map(tag => {
                const isSelected = selectedTags.includes(tag.id) || (assignment && tag.id === `${group.id}:unknown` && !group.tags.some(value => selectedTags.includes(value.id)));
                return (
                  <button
                    key={tag.id}
                    type="button"
                    aria-pressed={isSelected}
                    onClick={() => toggleTag(tag.id, group)}
                    className="tag-option"
                    title={tag.description || undefined}
                  >
                    {tag.name}
                  </button>
                );
              })}
            </div>
          </div>
        ))}
    </div>
  );
}
