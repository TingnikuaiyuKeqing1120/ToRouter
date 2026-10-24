import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Search } from 'lucide-react';
import type { CatalogItem, CatalogPriceRef, PriceTable } from '../api';
import { Input } from './ui';

export const originalQuote = (kind: string) => ['original', 'official', 'original_vendor'].includes(kind);
export function usablePriceTable(table: PriceTable | undefined): table is PriceTable {
  return !!table && ['USD', 'CNY'].includes(table.currency) && Number.isFinite(table.unit_tokens) && table.unit_tokens > 0
    && !!table.rates && Array.isArray(table.rules)
    && Object.values(table.rates).every(value => value == null || typeof value === 'number' && Number.isFinite(value) && value >= 0);
}
export function priceCatalogOptions(items: CatalogItem[], official: boolean) {
  return items.flatMap(item => item.offerings.flatMap(offering => offering.quotes
    .filter(quote => !official || originalQuote(quote.source_kind))
    .map(quote => ({
      ref: { source: item.source, id: item.id, offering_key: offering.key, quote_key: quote.key } as CatalogPriceRef,
      item, offering, quote,
      label: `${item.name || item.id} · ${offering.provider || item.provider} · ${offering.region || '全域'} · ${quote.name || quote.key} · ${quote.table.currency}`,
      disabled: item.needs_refresh || !usablePriceTable(quote.table)
        || !Object.values(quote.table.rates).some(value => value != null) && !quote.table.rules.some(rule => rule.enabled),
    }))));
}

/** One shared catalog read belongs to the page; row pickers only search those bytes. */
export function PriceCatalogPicker({ items, official, model, label, value, disabled, choices, onSelect }: {
  items: CatalogItem[]; official: boolean; model: string; label: string; value?: CatalogPriceRef; disabled?: boolean;
  choices?: ReturnType<typeof priceCatalogOptions>;
  onSelect: (ref: CatalogPriceRef) => void;
}) {
  const id = useId(), input = useRef<HTMLInputElement | null>(null), popup = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false), [query, setQuery] = useState(''), [active, setActive] = useState(-1);
  const [position, setPosition] = useState({ left: 0, top: 0, width: 360, maxHeight: 280 });
  const options = useMemo(() => choices || priceCatalogOptions(items, official), [choices, items, official]);
  const entries = useMemo(() => {
    const term = query.trim().toLowerCase();
    return options.filter(option => !term || `${option.label} ${option.item.id} ${option.item.aliases?.join(' ')} ${option.item.source}`.toLowerCase().includes(term))
      .sort((a, b) => Number(b.item.id === model) - Number(a.item.id === model));
  }, [options, query, model]);
  const selected = value && options.find(({ ref }) => ref.source === value.source && ref.id === value.id && ref.offering_key === value.offering_key && ref.quote_key === value.quote_key);
  useEffect(() => { setActive(-1); }, [query]);
  useEffect(() => {
    if (!open) return;
    function place() {
      const box = input.current?.getBoundingClientRect(); if (!box) return;
      const width = Math.min(480, Math.max(320, box.width), window.innerWidth - 16);
      const below = window.innerHeight - box.bottom - 12;
      const height = Math.min(280, Math.max(below, box.top - 12));
      setPosition({ left: Math.min(Math.max(8, box.left), window.innerWidth - width - 8), top: below >= height ? box.bottom + 4 : Math.max(8, box.top - height - 4), width, maxHeight: height });
    }
    function closeOutside(event: Event) { if (!input.current?.contains(event.target as Node) && !popup.current?.contains(event.target as Node)) setOpen(false); }
    place(); document.addEventListener('pointerdown', closeOutside); document.addEventListener('focusin', closeOutside);
    window.addEventListener('resize', place); window.addEventListener('scroll', place, true);
    return () => { document.removeEventListener('pointerdown', closeOutside); document.removeEventListener('focusin', closeOutside); window.removeEventListener('resize', place); window.removeEventListener('scroll', place, true); };
  }, [open]);
  function choose(index: number) { const option = entries[index]; if (!option || option.disabled) return; onSelect(option.ref); setOpen(false); setQuery(''); input.current?.focus(); }
  return <div className="price-catalog-picker"><Search size={14} aria-hidden="true" /><Input aria-label={`搜索价格资料 ${label}`} role="combobox" aria-autocomplete="list" aria-expanded={open} aria-controls={open ? id : undefined} aria-activedescendant={open && active >= 0 ? `${id}-${active}` : undefined}
    value={query} disabled={disabled} placeholder={selected ? `${selected.offering.region || '全域'} · ${selected.quote.name || selected.quote.key}` : '搜索模型 / 厂商 / 地区'}
    onFocus={event => { input.current = event.currentTarget; setOpen(true); }}
    onChange={event => { input.current = event.currentTarget; setQuery(event.target.value); setOpen(true); }}
    onKeyDown={event => {
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); setOpen(false); setActive(-1); }
      if (event.key === 'Tab') setOpen(false);
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') { event.preventDefault(); setOpen(true); const direction = event.key === 'ArrowDown' ? 1 : -1; let next = active; for (let n = 0; n < entries.length; n++) { next = (next + direction + entries.length) % entries.length; if (!entries[next].disabled) break; } setActive(next); requestAnimationFrame(() => document.getElementById(`${id}-${next}`)?.scrollIntoView?.({ block: 'nearest' })); }
      if (event.key === 'Enter' && open) { event.preventDefault(); choose(active); }
    }} />
    {open && createPortal(<div ref={popup} id={id} role="listbox" aria-label={`价格资料 ${label}`} className="price-catalog-options" style={{ position: 'fixed', ...position }}>
      {!entries.length && <p>未找到匹配资料</p>}
      {entries.map((entry, index) => <button key={JSON.stringify(entry.ref)} id={`${id}-${index}`} type="button" role="option" tabIndex={-1} aria-selected={active === index} aria-disabled={!!entry.disabled} className={active === index ? 'active' : ''} onMouseDown={event => event.preventDefault()} onClick={() => choose(index)}>
        <strong>{entry.item.name || entry.item.id}</strong><span>{entry.offering.provider} · {entry.offering.region || '全域'} · {entry.quote.name || entry.quote.key} · {entry.quote.table.currency}</span><small>{entry.item.source} · {entry.item.id}{entry.disabled ? ' · 需刷新 / 报价未核实' : ''}</small>
      </button>)}
    </div>, document.body)}
  </div>;
}
