import { useCallback, useEffect, useRef, useState } from 'react';
import { api, ApiError, errorMessage } from '../api';
import type { PluginCapabilityID, PluginList } from '../types/plugins';

// This resource belongs to the mounted authenticated page; it never survives logout.
export function usePlugins() {
  const [data, setData] = useState<PluginList | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const live = useRef(false), writing = useRef(false), blocked = useRef(true);
  const request = useRef<AbortController | null>(null);

  const reload = useCallback(async () => {
    if (writing.current || !live.current) return;
    request.current?.abort();
    const abort = new AbortController(); request.current = abort;
    setLoading(true);
    try {
      const next = await api<PluginList>('/plugins', { signal: abort.signal });
      if (!abort.signal.aborted && live.current) { setData(next); setError(''); blocked.current = false; }
    } catch (cause) {
      if (!abort.signal.aborted && live.current) { setError(errorMessage(cause)); blocked.current = true; }
    } finally {
      if (!abort.signal.aborted && live.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    live.current = true;
    void reload();
    const visible = () => { if (!document.hidden) void reload(); };
    document.addEventListener('visibilitychange', visible);
    return () => { live.current = false; request.current?.abort(); document.removeEventListener('visibilitychange', visible); };
  }, [reload]);

  async function setCapability(id: string, capability: PluginCapabilityID, enabled: boolean, version: string) {
    if (writing.current || blocked.current || !live.current) return;
    writing.current = true; setSaving(true);
    request.current?.abort();
    const abort = new AbortController(); request.current = abort;
    setLoading(false);
    try {
      const next = await api<PluginList>(`/plugins/${encodeURIComponent(id)}/capabilities/${capability}`, {
        method: 'PUT', signal: abort.signal, body: JSON.stringify({ enabled, expected_version: version }),
      });
      if (!abort.signal.aborted && live.current) { setData(next); setError(''); }
    } catch (cause) {
      if (!abort.signal.aborted && live.current) {
        setError(cause instanceof ApiError && cause.status === 409 ? '插件状态已变化，请重试读取最新状态后操作。' : errorMessage(cause));
        blocked.current = true;
      }
    } finally {
      writing.current = false;
      if (live.current) setSaving(false);
    }
  }

  return { data, loading, saving, error, reload, setCapability };
}
