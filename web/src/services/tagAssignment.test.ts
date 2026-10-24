import { describe, expect, it } from 'vitest';
import type { TagGroupWithTags } from '../types';
import { toggleAssignedTag } from './tagAssignment';
const group = (id: string) => ({ id, tags: ['unknown', 'a', 'b'].map(value => ({ id: `${id}:${value}` })) }) as TagGroupWithTags;
describe('object tag assignment', () => {
  it.each(['billing', 'privacy'])('%s is single choice and preserves other groups', id => {
    expect(toggleAssignedTag([`${id}:a`, 'other:a'], group(id), `${id}:b`)).toEqual(['other:a', `${id}:b`]);
    expect(toggleAssignedTag([`${id}:a`, 'other:a'], group(id), `${id}:unknown`)).toEqual(['other:a', `${id}:unknown`]);
  });
  it('keeps custom concrete values multiple and makes the last removal explicitly unknown', () => {
    expect(toggleAssignedTag(['team:a'], group('team'), 'team:b')).toEqual(['team:a', 'team:b']);
    expect(toggleAssignedTag(['team:a'], group('team'), 'team:a')).toEqual(['team:unknown']);
    expect(toggleAssignedTag(['team:unknown'], group('team'), 'team:b')).toEqual(['team:b']);
    expect(toggleAssignedTag(['team:a', 'team:b'], group('team'), 'team:unknown')).toEqual(['team:unknown']);
  });
});
