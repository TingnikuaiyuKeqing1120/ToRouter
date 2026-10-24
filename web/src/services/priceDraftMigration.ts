import type { Price } from '../api';

// Old browser drafts are converted once at their read boundary, like server backups.
// Runtime identity is always channel + model. Ordinary new duplicate rows must
// remain visible so save validation can report them instead of losing a draft.
export function normalizeLegacyPriceRows<T extends Price>(rows: T[]): T[] {
  if (!rows.some(row => Object.prototype.hasOwnProperty.call(row, 'role'))) return rows;
  const groups = new Map<string, (T & { role?: string })[]>();
  rows.forEach((row, index) => {
    const id = row.model.trim() ? `${row.channel_id}\u0000${row.model.trim()}` : `blank:${index}`;
    groups.set(id, [...(groups.get(id) || []), row]);
  });
  const configured = (row: Price) => row.selection.mode === 'catalog'
    || Object.values(row.selection.table.rates).some(value => value != null)
    || row.selection.table.rules.some(rule => rule.enabled);
  return [...groups.values()].map(group => {
    const chosen = group.find(row => row.channel_id === 0 && row.role === 'reference' && configured(row))
      || group.find(row => row.role !== 'reference') || group[0];
    const { role: _legacyRole, ...price } = chosen;
    return price as T;
  });
}
