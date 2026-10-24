import { useEffect, useRef, useState } from 'react';
import { api, errorMessage, type Price } from '../api';
import { useUnsavedChanges } from './useUnsavedChanges';
import { usePendingSaveDecision } from '../components/Confirm';
import { normalizeLegacyPriceRows } from '../services/priceDraftMigration';

export type PriceRow = Price & { key: number };
export const priceFields = ['input', 'output', 'cache_read', 'cache_write'] as const;
const draftKey = 'to-router.price-workbench.draft.v2';
export function blankPrice(channel_id: number, currency: 'USD' | 'CNY', key: number, origin: Price['origin'] = 'manual'): PriceRow {
  return { key, channel_id, model: '', display_name: '', origin, selection: { mode: 'manual', table: { currency, unit_tokens: 1000000, rates: { input: null, output: null, cache_read: null, cache_write: null }, rules: [] } } };
}
export function configured(row: Price) {
  return row.selection.mode === 'catalog' || priceFields.some(field => row.selection.mode === 'manual' && row.selection.table.rates[field] != null) || (row.selection.mode === 'manual' && (row.selection.table.rules.length > 0 || row.selection.table.unit_tokens !== 1000000 || !!row.selection.table.uncovered_conditions?.length));
}
function fingerprint(value: Price[]) { return JSON.stringify(value, (_key, item) => item && typeof item === 'object' && !Array.isArray(item) ? Object.fromEntries(Object.entries(item).sort(([a], [b]) => a.localeCompare(b))) : item); }
function payload(rows: PriceRow[]): Price[] {
  return rows.filter(row => row.model.trim() && Number.isInteger(row.channel_id) && row.channel_id >= 0).map(({ key: _key, ...row }) => ({ ...row, model: row.model.trim(), display_name: row.display_name.trim() }));
}
export function usePriceWorkbench() {
  const hasPendingSaveDecision = usePendingSaveDecision();
  const [rows, renderRows] = useState<PriceRow[]>([]), [loading, setLoading] = useState(true), [ready, setReady] = useState(false), [busy, setBusy] = useState(false), [error, setError] = useState(''), [dirty, setDirty] = useState(false);
  const current = useRef<PriceRow[]>([]), serial = useRef(0), revision = useRef(0), saved = useRef('[]'), running = useRef<Promise<void> | null>(null), live = useRef(true);
  function persist() {
    const pending = fingerprint(payload(current.current)) !== saved.current || current.current.some(row => !row.model.trim());
    try { if (pending) localStorage.setItem(draftKey, JSON.stringify(current.current)); else localStorage.removeItem(draftKey); } catch { /* editing still works without storage */ }
    if (live.current) setDirty(pending);
  }
  function update(change: (rows: PriceRow[]) => PriceRow[]) {
    current.current = change(current.current); revision.current++; renderRows(current.current); persist();
  }
  const nextKey = () => ++serial.current;
  async function load() {
    setLoading(true); setError('');
    try {
      const prices = await api<Price[]>('/prices');
      if (!live.current) return;
      saved.current = fingerprint(prices);
      let draft: PriceRow[] | null = null;
      try {
        const raw = localStorage.getItem(draftKey), value = JSON.parse(raw || 'null');
        if (Array.isArray(value)) {
          draft = normalizeLegacyPriceRows(value);
          if (draft !== value && raw) {
            localStorage.setItem('to-router.price-workbench.draft.pre-official.v2', raw);
            localStorage.setItem(draftKey, JSON.stringify(draft));
          }
        }
      } catch { /* editing still works without browser storage */ }
      current.current = draft || prices.map(row => ({ ...row, key: nextKey() }));
      serial.current = Math.max(serial.current, ...current.current.map(row => row.key)); renderRows(current.current); setReady(true); persist();
    } catch (e) { if (live.current) setError(errorMessage(e)); } finally { if (live.current) setLoading(false); }
  }
  useEffect(() => { live.current = true; void load(); return () => { live.current = false; }; }, []);
  async function flush() {
    if (running.current) { await running.current; return flush(); }
    if (!ready) return;
    const body = payload(current.current), bodyFingerprint = fingerprint(body);
    if (bodyFingerprint === saved.current) { persist(); return; }
    const ids = body.map(row => `${row.channel_id}\u0000${row.model}`);
    if (new Set(ids).size !== ids.length) { const message = '同一渠道的模型 ID 不能重复'; setError(message); throw new Error(message); }
    const version = revision.current, snapshot = current.current;
    setBusy(true); setError('');
    running.current = (async () => {
      try {
        const result = await api<Price[]>('/prices', { method: 'PUT', body: JSON.stringify(body) });
        saved.current = bodyFingerprint;
        if (revision.current === version && live.current) {
          const keys = new Map(snapshot.map(row => [`${row.channel_id}\u0000${row.model.trim()}`, row.key]));
          current.current = [...result.map(row => ({ ...row, key: keys.get(`${row.channel_id}\u0000${row.model}`) || nextKey() })), ...snapshot.filter(row => !row.model.trim())];
          renderRows(current.current);
        }
        persist();
      } catch (e) { if (live.current) setError(errorMessage(e)); throw e; }
      finally { running.current = null; if (live.current) setBusy(false); }
    })();
    await running.current;
    if (fingerprint(payload(current.current)) !== saved.current) await flush();
  }
  const latestFlush = useRef(flush); latestFlush.current = flush;
  const canLeave = useUnsavedChanges({ dirty: () => fingerprint(payload(current.current)) !== saved.current || current.current.some(row => !row.model.trim()), busy: () => !!running.current, save: async () => {
    if (current.current.some(row => !row.model.trim())) { setError('请填写模型 ID，或删除空白行后再保存。'); return false; }
    try { await latestFlush.current(); return true; } catch { return false; }
  }, discard: () => {
    current.current = (JSON.parse(saved.current) as Price[]).map(row => ({ ...row, key: nextKey() }));
    revision.current++; renderRows(current.current); persist();
  } });
  useEffect(() => { const timer = setInterval(() => { if (!hasPendingSaveDecision() && !running.current && fingerprint(payload(current.current)) !== saved.current) void latestFlush.current().catch(() => {}); }, 30000); return () => clearInterval(timer); }, [hasPendingSaveDecision]);
  return { rows, update, nextKey, flush, canLeave, loading, ready, busy, dirty, error, setError, retryLoad: load };
}
