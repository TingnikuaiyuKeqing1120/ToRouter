import { useCallback, useEffect, useRef, useState } from 'react';
import { api, errorMessage } from '../api';
import { isStableResourcePath, readStableResource, stableResourceData } from '../services/stableResources';

export function useResource<T>(path: string) {
  const initial = isStableResourcePath(path) ? stableResourceData<T>(path) : undefined;
  const [data, setData] = useState<T | null>(() => initial ?? null);
  const [loadedPath, setLoadedPath] = useState(() => initial === undefined ? '' : path);
  const [loading, setLoading] = useState(initial === undefined);
  const [error, setError] = useState('');
  const [version, setVersion] = useState(0);
  const reload = useCallback(() => setVersion(v => v + 1), []);
  const latest = useRef(0);
  const previousRequest = useRef({ path, version });
  useEffect(() => {
    const id = ++latest.current;
    const abort = new AbortController();
    const cached = isStableResourcePath(path) ? stableResourceData<T>(path) : undefined;
    const manual = previousRequest.current.path === path && previousRequest.current.version !== version;
    previousRequest.current = { path, version };
    if (cached !== undefined) { setData(cached); setLoadedPath(path); }
    setLoading(cached === undefined || manual); setError('');
    const request = isStableResourcePath(path) ? readStableResource<T>(path, manual) : api<T>(path, { signal: abort.signal });
    request
      .then(value => { if (!abort.signal.aborted && latest.current === id) { setData(value); setLoadedPath(path); } })
      .catch(error => { if (!abort.signal.aborted && latest.current === id) setError(errorMessage(error)); })
      .finally(() => { if (!abort.signal.aborted && latest.current === id) setLoading(false); });
    return () => abort.abort();
  }, [path, version]);
  return { data, setData, loadedPath, loading, error, reload };
}

/** Several paths fetched together and flattened, for a view made of filtered
 * slices of one collection: the OAuth view asks the channel list for one
 * credential type per provider, so it never downloads the slices it will not
 * show. Callers pass a stable (memoized) array — a new one each render would
 * refetch forever. */
export function useResources<T>(paths: string[]) {
  const [data, setData] = useState<T[] | null>(null);
  const [loadedKey, setLoadedKey] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [version, setVersion] = useState(0);
  const reload = useCallback(() => setVersion(v => v + 1), []);
  const latest = useRef(0);
  useEffect(() => {
    const id = ++latest.current;
    const abort = new AbortController();
    setLoading(true); setError('');
    Promise.all(paths.map(path => api<T[]>(path, { signal: abort.signal })))
      .then(values => { if (latest.current === id) { setData(values.flat()); setLoadedKey(JSON.stringify(paths)); } })
      .catch(error => { if (!abort.signal.aborted && latest.current === id) setError(errorMessage(error)); })
      .finally(() => { if (!abort.signal.aborted && latest.current === id) setLoading(false); });
    return () => abort.abort();
  }, [paths, version]);
  return { data, setData, loadedKey, loading, error, reload };
}
