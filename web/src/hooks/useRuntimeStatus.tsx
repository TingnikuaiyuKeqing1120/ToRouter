import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react';
import { api, errorMessage, type RuntimeStatusSnapshot } from '../api';

const RuntimeStatusContext = createContext<{
  data: RuntimeStatusSnapshot | null;
  loading: boolean;
  error: string;
  updatedAt: number | null;
  reload: () => void;
} | null>(null);

// One authenticated console resource feeds both navigation and the page.
export function RuntimeStatusProvider({ children }: { children: ReactNode }) {
  const [data, setData] = useState<RuntimeStatusSnapshot | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [updatedAt, setUpdatedAt] = useState<number | null>(null);
  const [version, setVersion] = useState(0);
  const reload = useCallback(() => setVersion(value => value + 1), []);
  useEffect(() => {
    document.addEventListener('visibilitychange', reload);
    return () => document.removeEventListener('visibilitychange', reload);
  }, [reload]);
  useEffect(() => {
    if (document.hidden) { setLoading(false); return; }
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      setLoading(true);
      try {
        const result = await api<RuntimeStatusSnapshot>('/status', { signal: abort.signal });
        if (!abort.signal.aborted) { setData(result); setError(''); setUpdatedAt(Date.now()); }
      } catch (e) { if (!abort.signal.aborted) setError(errorMessage(e)); }
      finally {
        if (!abort.signal.aborted) {
          setLoading(false);
          timer = setTimeout(poll, 5000);
        }
      }
    }
    void poll();
    return () => { abort.abort(); clearTimeout(timer); };
  }, [version]);
  return <RuntimeStatusContext.Provider value={{ data, loading, error, updatedAt, reload }}>{children}</RuntimeStatusContext.Provider>;
}

export function useOptionalRuntimeStatus() {
  return useContext(RuntimeStatusContext);
}

export function useRuntimeStatus() {
  const value = useContext(RuntimeStatusContext);
  if (!value) throw new Error('useRuntimeStatus requires RuntimeStatusProvider');
  return value;
}
