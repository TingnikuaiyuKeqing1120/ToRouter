import { expect, it } from 'vitest';
import type { Price } from '../api';
import { normalizeLegacyPriceRows } from './priceDraftMigration';

const price = (input: number | null): Price => ({ channel_id: 0, model: 'public', display_name: 'Public', origin: 'manual', selection: { mode: 'manual', table: { currency: 'CNY', unit_tokens: 1000, rates: { input, output: null, cache_read: null, cache_write: null }, rules: [], uncovered_conditions: ['retained'] } } });

it('keeps the previously active reference including explicit zero and removes obsolete identity', () => {
  const rows = [{ ...price(8), role: 'default', key: 1 }, { ...price(0), role: 'reference', key: 2 }];
  const migrated = normalizeLegacyPriceRows(rows);
  expect(migrated).toHaveLength(1);
  expect(migrated[0]).toEqual({ ...price(0), key: 2 });
  expect(rows[1]).toHaveProperty('role', 'reference');
  expect(normalizeLegacyPriceRows(migrated)).toBe(migrated);
});

it('keeps a missing quote reference instead of silently falling back to the old default', () => {
  const linked: Price = { ...price(null), selection: { mode: 'catalog', ref: { source: 'missing', id: 'missing', offering_key: 'region', quote_key: 'q' } } };
  expect(normalizeLegacyPriceRows([{ ...price(8), role: 'default' }, { ...linked, role: 'reference' }])).toEqual([linked]);
});

it('uses the old default when a reference has only disabled conditions, but retains an enabled zero multiplier', () => {
  const ref = price(null);
  if (ref.selection.mode !== 'manual') throw new Error('fixture');
  ref.selection.table.rules = [{ enabled: false, when: {}, action: { multiplier: 0 } }];
  expect(normalizeLegacyPriceRows([{ ...price(8), role: 'default' }, { ...ref, role: 'reference' }])).toEqual([price(8)]);
  ref.selection.table.rules[0].enabled = true;
  expect(normalizeLegacyPriceRows([{ ...price(8), role: 'default' }, { ...ref, role: 'reference' }])).toEqual([ref]);
});

it('preserves channels and independent blank drafts, and leaves new duplicates for validation', () => {
  const rows = [price(1), price(2)];
  expect(normalizeLegacyPriceRows(rows)).toBe(rows);
  const old = [{ ...price(1), role: 'default' }, { ...price(3), channel_id: 9, role: 'default' }, { ...price(null), model: '', key: 4, role: 'default' }, { ...price(null), model: '', key: 5, role: 'default' }];
  const result = normalizeLegacyPriceRows(old);
  expect(result).toHaveLength(4);
  expect(result.map(row => row.channel_id)).toEqual([0, 9, 0, 0]);
  expect(result.every(row => !('role' in row))).toBe(true);
});
