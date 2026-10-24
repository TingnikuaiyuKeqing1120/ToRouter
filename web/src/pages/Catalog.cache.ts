import type { CatalogItem, CatalogSourceStatus } from '../api';
import { publishStableResource, stableResourceData } from '../services/stableResources';

// Page-lifetime drafts stay in Catalog; only accepted server responses live here.
export const catalogSessionCache = {
  get models() { return stableResourceData<CatalogItem[]>('/catalog/models?source=all'); },
  set models(value: CatalogItem[] | undefined) { publishStableResource('/catalog/models?source=all', value); },
  get sources() { return stableResourceData<CatalogSourceStatus[]>('/catalog/sources'); },
  set sources(value: CatalogSourceStatus[] | undefined) { publishStableResource('/catalog/sources', value); },
};
export const catalogView = { page: 1, search: '', tab: 'all' as 'all' | 'builtin' | 'external', externalSubSource: 'all' as 'all' | 'openrouter' | 'litellm', provider: 'all', currency: 'CNY' as 'CNY' | 'USD' };
export function resetCatalogView() { Object.assign(catalogView, { page: 1, search: '', tab: 'all', externalSubSource: 'all', provider: 'all', currency: 'CNY' }); }
