import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Search } from 'lucide-react';
import type { CatalogModelRef } from '../api';
import { catalogEntries, catalogEntryKey } from '../services/catalogEntries';
import { Input } from './ui';
import './ModelCatalogPicker.css';

/** The page loads and expands the catalog once; row controls only search it. */
export function ModelCatalogPicker({ choices, model, label, value, disabled, onSelect }: {
  choices: ReturnType<typeof catalogEntries>; model: string; label: string;
  value?: CatalogModelRef; disabled?: boolean; onSelect: (ref: CatalogModelRef) => void;
}) {
  const id = useId(), input = useRef<HTMLInputElement | null>(null), popup = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false), [query, setQuery] = useState(''), [active, setActive] = useState(-1);
  const [position, setPosition] = useState({ left: 8, top: 8, width: 360, maxHeight: 280 });
  const entries = useMemo(() => {
    const term = query.trim().toLowerCase();
    return choices.filter(({ item }) => !term || `${item.name} ${item.id} ${item.provider} ${item.source} ${item.aliases?.join(' ')} ${item.offerings.map(offering => `${offering.region} ${offering.provider}`).join(' ')}`.toLowerCase().includes(term))
      .sort((a, b) => Number(b.item.id === model) - Number(a.item.id === model));
  }, [choices, query, model]);
  const selected = value && choices.find(entry => catalogEntryKey(entry.ref) === catalogEntryKey(value));
  useEffect(() => { setActive(-1); }, [query]);
  useEffect(() => { if (disabled) setOpen(false); }, [disabled]);
  useEffect(() => {
    if (!open) return;
    function place() {
      const box = input.current?.getBoundingClientRect(); if (!box) return;
      const width = Math.min(480, Math.max(320, box.width), window.innerWidth - 16);
      const below = window.innerHeight - box.bottom - 12;
      const height = Math.min(300, Math.max(120, below, box.top - 12));
      setPosition({ left: Math.min(Math.max(8, box.left), window.innerWidth - width - 8), top: below >= height ? box.bottom + 4 : Math.max(8, box.top - height - 4), width, maxHeight: Math.min(height, window.innerHeight - 16) });
    }
    function closeOutside(event: Event) { if (!input.current?.contains(event.target as Node) && !popup.current?.contains(event.target as Node)) setOpen(false); }
    place(); document.addEventListener('pointerdown', closeOutside); document.addEventListener('focusin', closeOutside);
    window.addEventListener('resize', place); window.addEventListener('scroll', place, true);
    return () => { document.removeEventListener('pointerdown', closeOutside); document.removeEventListener('focusin', closeOutside); window.removeEventListener('resize', place); window.removeEventListener('scroll', place, true); };
  }, [open]);
  function choose(index: number) {
    const entry = entries[index]; if (!entry || entry.item.needs_refresh || disabled) return;
    setOpen(false); setQuery(''); input.current?.focus();
    // Model associations deliberately exclude quotation identity.
    onSelect({ source: entry.ref.source, id: entry.ref.id, ...(entry.ref.offering_key ? { offering_key: entry.ref.offering_key } : {}) });
  }
  return <div className="model-catalog-picker">
    <div className="model-catalog-search"><Search size={14} aria-hidden="true" /><Input role="combobox" aria-label={`搜索规格资料 ${label}`} aria-autocomplete="list" aria-expanded={open && !disabled} aria-controls={open && !disabled ? id : undefined} aria-activedescendant={open && active >= 0 ? `${id}-${active}` : undefined}
      disabled={disabled} value={query} placeholder={selected ? selected.item.name || selected.item.id : '搜索模型 / 厂商 / 地区'}
      onFocus={event => { input.current = event.currentTarget; setOpen(true); }} onChange={event => { input.current = event.currentTarget; setQuery(event.target.value); setOpen(true); }}
      onKeyDown={event => {
        if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); setOpen(false); setActive(-1); }
        if (event.key === 'Tab') setOpen(false);
        if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && entries.length) {
          event.preventDefault(); setOpen(true); const direction = event.key === 'ArrowDown' ? 1 : -1;
          let next = active < 0 ? (direction > 0 ? -1 : 0) : active;
          for (let n = 0; n < entries.length; n++) { next = (next + direction + entries.length) % entries.length; if (!entries[next].item.needs_refresh) break; }
          setActive(next); requestAnimationFrame(() => document.getElementById(`${id}-${next}`)?.scrollIntoView?.({ block: 'nearest' }));
        }
        if (event.key === 'Enter' && open) { event.preventDefault(); choose(active); }
      }} /></div>
    <small title={value ? `${value.source} · ${value.id}${value.offering_key ? ` · ${value.offering_key}` : ''}` : undefined}>{value ? `${value.source} · ${value.id}${value.offering_key ? ` · ${selected?.item.offerings[0]?.region || value.offering_key}` : ''}` : '未关联资料'}</small>
    {open && !disabled && createPortal(<div ref={popup} id={id} role="listbox" aria-label={`规格资料 ${label}`} className="model-catalog-options" style={{ position: 'fixed', ...position }}>
      {!entries.length && <p>未找到匹配资料</p>}
      {entries.map((entry, index) => <button key={catalogEntryKey(entry.ref)} id={`${id}-${index}`} type="button" role="option" tabIndex={-1} aria-selected={active === index} aria-disabled={!!entry.item.needs_refresh} className={active === index ? 'active' : ''} onMouseDown={event => event.preventDefault()} onClick={() => choose(index)}>
        <strong>{entry.item.name || entry.item.id}</strong><span>{entry.item.provider}{entry.ref.offering_key ? ` · ${entry.item.offerings[0]?.region || entry.ref.offering_key}` : ''}</span><small>{entry.item.source} · {entry.item.id}{entry.item.needs_refresh ? ' · 需刷新' : ''}</small>
      </button>)}
    </div>, document.body)}
  </div>;
}
