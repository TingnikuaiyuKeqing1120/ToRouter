import type { TagGroupWithTags } from '../types';

export const editableTagGroups = (groups: TagGroupWithTags[]) => groups.filter(group => group.id !== 'modality');
export const groupTagIDs = (group: TagGroupWithTags) => new Set([...group.tags.map(tag => tag.id), `${group.id}:unknown`]);

/** Assignment is distinct from a filter: unknown excludes concrete values. */
export function toggleAssignedTag(selected: string[], group: TagGroupWithTags, id: string): string[] {
  const ids = groupTagIDs(group), unknown = `${group.id}:unknown`;
  const outside = selected.filter(value => !ids.has(value));
  if (id === unknown) return [...outside, unknown];
  if (group.id === 'billing' || group.id === 'privacy') return [...outside, id];
  const concrete = selected.filter(value => ids.has(value) && value !== unknown);
  const next = concrete.includes(id) ? concrete.filter(value => value !== id) : [...concrete, id];
  return [...outside, ...(next.length ? next : [unknown])];
}
