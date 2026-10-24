import type { CatalogItem, CatalogModelRef } from '../api';

export function catalogEntryKey(ref: CatalogModelRef) {
  return `${ref.source}\u0000${ref.id}\u0000${ref.offering_key || ''}`;
}

// Regional entries retain the canonical model identity and only narrow its offerings.
export function catalogEntries(items: CatalogItem[]) {
  return items.flatMap(item => {
    if (item.source === 'builtin' && item.offerings.length > 1) {
      return item.offerings.map(offering => ({
        item: { ...item, provider: offering.provider, offerings: [offering] },
        ref: { source: item.source, id: item.id, offering_key: offering.key },
      }));
    }
    return [{ item, ref: { source: item.source, id: item.id } as CatalogModelRef }];
  });
}
