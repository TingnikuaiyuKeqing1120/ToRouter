import { afterEach } from 'vitest';
import { clearStableResources } from './services/stableResources';
import { resetCatalogView } from './pages/Catalog.cache';

if (typeof window !== 'undefined') {
  window.scrollTo = () => {};
}

// Each test represents an independent browser session unless it explicitly
// remounts within the same test. Invalidate unresolved work as well as values.
afterEach(() => { clearStableResources(); resetCatalogView(); });
