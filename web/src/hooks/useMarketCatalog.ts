import { useCallback, useEffect, useRef, useState } from 'react';
import { api, errorMessage } from '../api';
import type { MarketCatalog } from '../types/plugins';

// The server returns its cached catalogue immediately and refreshes it in the
// background. Keep the successful view while subsequent reads are pending.
export function useMarketCatalog(query = '') {
  const normalizedQuery = query.trim();
  const [search, setSearch] = useState(normalizedQuery);
  const [data, setData] = useState<MarketCatalog | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const request = useRef<AbortController>();
  const live = useRef(false);
  useEffect(() => {
    if (search === normalizedQuery) return;
    const timer = window.setTimeout(() => setSearch(normalizedQuery), 300);
    return () => window.clearTimeout(timer);
  }, [normalizedQuery, search]);
  const load = useCallback(async (force: boolean, lookup = search) => {
    if (!live.current) return;
    request.current?.abort();
    const abort = new AbortController(); request.current = abort;
    setLoading(true);
    try {
      const suffix = lookup ? `?q=${encodeURIComponent(lookup)}` : '';
      if (force) await api(`/plugins/market/refresh${suffix}`, { method: 'POST', body: '{}', signal: abort.signal });
      const next = await api<MarketCatalog>(`/plugins/market${suffix}`, { signal: abort.signal });
      if (!live.current || abort.signal.aborted) return;
      setData({ ...next, items: next.items || [] }); setError('');
    } catch (cause) {
      if (live.current && !abort.signal.aborted) setError(errorMessage(cause));
    } finally {
      if (live.current && !abort.signal.aborted) setLoading(false);
    }
  }, [search]);
  const reload = useCallback(() => load(false), [load]);
  const refresh = useCallback(() => load(true, normalizedQuery), [load, normalizedQuery]);
  useEffect(() => {
    live.current = true; void reload();
    return () => { live.current = false; request.current?.abort(); };
  }, [reload]);
  useEffect(() => {
    if (!data?.refreshing || loading || error) return;
    const timer = window.setTimeout(() => void reload(), 2000);
    return () => window.clearTimeout(timer);
  }, [data?.refreshing, loading, error, reload]);
  return { data, loading, error, reload, refresh };
}
