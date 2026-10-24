import { useCallback, useLayoutEffect, useRef, useState, type CSSProperties, type PointerEvent as ReactPointerEvent } from 'react';
import '../components/SortableList.css';

type Axis = 'vertical' | 'horizontal';
type Options = {
  ids: string[];
  axis?: Axis;
  disabled?: boolean;
  revision?: string | number;
  /** Resolve after updating the rendered ids; reject on a failed save. */
  onCommit: (orderedIds: string[]) => void | Promise<void>;
  externalDrop?: {
    resolve: (x: number, y: number) => string | undefined;
    onHover: (target?: string, id?: string) => void;
    onDrop: (id: string, target: string) => Promise<void>;
  };
};
type Box = { id: string; start: number; size: number };
type ScrollPort = HTMLElement | Window;
type Session = {
  pointerId: number; id: string; ids: string[]; revision: Options['revision'];
  x: number; y: number; startX: number; startY: number; rect: DOMRect;
  boxes: Box[]; order: string[]; ports: { node: ScrollPort; start: number }[];
  active: boolean; pending: boolean; clone?: HTMLElement; frame?: number;
  removeListeners: () => void; restoreSelection?: () => void; stopObserving?: () => void;
  releaseCapture?: () => void;
};

let owner: symbol | undefined;
const equal = (a: string[], b: string[]) => a.length === b.length && a.every((id, i) => id === b[i]);
const isWindow = (port: ScrollPort): port is Window => port === window;
const position = (port: ScrollPort, axis: Axis) => isWindow(port)
  ? axis === 'vertical' ? port.scrollY : port.scrollX
  : axis === 'vertical' ? port.scrollTop : port.scrollLeft;

/** Snapshot coordinates are deliberately independent of the animated item transforms. */
export function sortableOrder(boxes: Box[], id: string, center: number): string[] {
  const rest = boxes.filter(box => box.id !== id);
  const target = rest.findIndex(box => center < box.start + box.size / 2);
  const ids = rest.map(box => box.id);
  ids.splice(target < 0 ? ids.length : target, 0, id);
  return ids;
}

export function sortableOffsets(boxes: Box[], order: string[]): Map<string, number> {
  const byId = new Map(boxes.map(box => [box.id, box]));
  const offsets = new Map<string, number>();
  let cursor = boxes[0]?.start ?? 0;
  order.forEach((id, index) => {
    const box = byId.get(id)!;
    offsets.set(id, cursor - box.start);
    // Preserve measured layout spacing while packing variable-sized cards.
    const gap = index < boxes.length - 1 ? Math.max(0, boxes[index + 1].start - boxes[index].start - boxes[index].size) : 0;
    cursor += box.size + gap;
  });
  return offsets;
}

function scrollPorts(list: HTMLElement): ScrollPort[] {
  const ports: ScrollPort[] = [];
  for (let node: HTMLElement | null = list; node; node = node.parentElement) {
    // window below already represents the document's scrolling element.
    if (node === document.scrollingElement) continue;
    const style = getComputedStyle(node);
    if (/(auto|scroll)/.test(`${style.overflow} ${style.overflowX} ${style.overflowY}`)) ports.push(node);
  }
  ports.push(window);
  return ports;
}

function floatingCopy(node: HTMLElement, rect: DOMRect): HTMLElement {
  const clone = node.cloneNode(true) as HTMLElement;
  const originals = [node, ...node.querySelectorAll<HTMLElement>('*')];
  const copies = [clone, ...clone.querySelectorAll<HTMLElement>('*')];
  originals.forEach((original, index) => {
    const copy = copies[index];
    const computed = getComputedStyle(original);
    for (let i = 0; i < computed.length; i++) {
      const property = computed[i];
      copy.style.setProperty(property, computed.getPropertyValue(property));
    }
    copy.removeAttribute('id');
    copy.removeAttribute('name');
    copy.removeAttribute('data-testid');
    copy.removeAttribute('data-sortable-state');
    copy.removeAttribute('data-sortable-item');
    copy.style.setProperty('pointer-events', 'none', 'important');
    copy.style.animation = 'none';
  });
  clone.classList.add('sortable-floating');
  clone.setAttribute('aria-hidden', 'true');
  clone.inert = true;
  Object.assign(clone.style, {
    position: 'fixed', left: `${rect.left}px`, top: `${rect.top}px`,
    width: `${rect.width}px`, height: `${rect.height}px`, margin: '0',
    boxSizing: 'border-box', pointerEvents: 'none', zIndex: '10000',
    transition: 'none', transform: 'none', opacity: '0.94',
    boxShadow: '0 10px 26px rgba(0, 0, 0, 0.2)', outline: '1px solid rgba(127, 127, 127, 0.3)',
  });
  document.body.appendChild(clone);
  originals.forEach((original, index) => {
    copies[index].scrollTop = original.scrollTop;
    copies[index].scrollLeft = original.scrollLeft;
  });
  return clone;
}

function containsVisiblePoint(container: HTMLElement, x: number, y: number) {
  const rect = container.getBoundingClientRect();
  let left = Math.max(0, rect.left), right = Math.min(innerWidth, rect.right);
  let top = Math.max(0, rect.top), bottom = Math.min(innerHeight, rect.bottom);
  for (let node = container.parentElement; node; node = node.parentElement) {
    const style = getComputedStyle(node);
    const bounds = node.getBoundingClientRect();
    if (/(auto|scroll|hidden|clip)/.test(`${style.overflow} ${style.overflowX}`)) {
      left = Math.max(left, bounds.left); right = Math.min(right, bounds.right);
    }
    if (/(auto|scroll|hidden|clip)/.test(`${style.overflow} ${style.overflowY}`)) {
      top = Math.max(top, bounds.top); bottom = Math.min(bottom, bounds.bottom);
    }
  }
  return x >= left && x <= right && y >= top && y <= bottom;
}

/** Single-list pointer sorting. Only spread handleProps on the explicit drag handle.
 * Keep keyboard reorder actions in the consumer. onCommit must update ids on success.
 */
export function useSortableList({ ids, axis = 'vertical', disabled = false, revision, onCommit, externalDrop }: Options) {
  const list = useRef<HTMLElement | null>(null);
  const nodes = useRef(new Map<string, HTMLElement>());
  const callbacks = useRef(new Map<string, (node: HTMLElement | null) => void>());
  const session = useRef<Session>();
  const token = useRef(Symbol('sortable-list'));
  const latest = useRef({ ids, disabled, revision, onCommit, externalDrop });
  latest.current = { ids, disabled, revision, onCommit, externalDrop };
  const mounted = useRef(true);
  const [status, setStatus] = useState({ dragging: false, pending: false });
  const suppressUntil = useRef(0);
  const clickCleanup = useRef<() => void>();

  const suppressClick = useCallback(() => {
    clickCleanup.current?.();
    suppressUntil.current = Date.now() + 350;
    const clearSuppression = () => {
      window.clearTimeout(timer);
      document.removeEventListener('click', block, true);
      document.removeEventListener('pointerdown', clearSuppression, true);
      clickCleanup.current = undefined;
      suppressUntil.current = 0;
    };
    const block = (event: MouseEvent) => {
      if (event.detail === 0 || Date.now() > suppressUntil.current) return;
      event.preventDefault(); event.stopImmediatePropagation();
      clearSuppression();
    };
    document.addEventListener('click', block, true);
    // A new press begins an explicit gesture, even when the previous drag did
    // not produce a trailing click (for example after releasing outside a tab).
    document.addEventListener('pointerdown', clearSuppression, true);
    const timer = window.setTimeout(clearSuppression, 350);
    clickCleanup.current = clearSuppression;
  }, []);

  const clear = useCallback(() => {
    const drag = session.current;
    if (!drag) return;
    drag.removeListeners();
    drag.releaseCapture?.();
    drag.stopObserving?.();
    if (drag.frame !== undefined) cancelAnimationFrame(drag.frame);
    drag.clone?.remove();
    drag.restoreSelection?.();
    latest.current.externalDrop?.onHover();
    nodes.current.forEach(node => {
      delete node.dataset.sortableState;
      node.style.removeProperty('--sortable-x');
      node.style.removeProperty('--sortable-y');
    });
    if (list.current) delete list.current.dataset.sortableActive;
    session.current = undefined;
    if (owner === token.current) owner = undefined;
    if (mounted.current) setStatus({ dragging: false, pending: false });
  }, []);

  const cancel = useCallback(() => {
    if (session.current?.active) suppressClick();
    clear();
  }, [clear, suppressClick]);

  useLayoutEffect(() => {
    const drag = session.current;
    if (!drag) return;
    if (drag.pending) {
      if (!equal(ids, drag.ids)) clear();
    } else if (disabled || revision !== drag.revision || !equal(ids, drag.ids)) cancel();
  }, [ids, disabled, revision, cancel, clear]);

  useLayoutEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; clear(); clickCleanup.current?.(); };
  }, [clear]);

  const start = useCallback((id: string, event: ReactPointerEvent<HTMLElement>) => {
    if (latest.current.disabled || latest.current.ids.length < 2 && !latest.current.externalDrop || event.button !== 0 || event.isPrimary === false || owner || session.current) return;
    const item = nodes.current.get(id);
    const container = list.current;
    const handle = event.currentTarget;
    if (!item || !container || !latest.current.ids.includes(id)) return;
    owner = token.current;
    const drag: Session = {
      pointerId: event.pointerId, id, ids: [...latest.current.ids], revision: latest.current.revision,
      x: event.clientX, y: event.clientY, startX: event.clientX, startY: event.clientY,
      rect: item.getBoundingClientRect(), boxes: [], order: [...latest.current.ids],
      ports: [], active: false, pending: false, removeListeners: () => {},
    };
    session.current = drag;

    const update = () => {
      if (session.current !== drag || !drag.active || drag.pending) return;
      const dx = drag.x - drag.startX, dy = drag.y - drag.startY;
      if (drag.clone) drag.clone.style.transform = `translate3d(${dx}px, ${dy}px, 0)`;
      latest.current.externalDrop?.onHover(latest.current.externalDrop.resolve(drag.x, drag.y), id);
      const scroll = drag.ports.reduce((sum, port) => sum + position(port.node, axis) - port.start, 0);
      const box = drag.boxes.find(box => box.id === id)!;
      const center = box.start + box.size / 2 + (axis === 'vertical' ? dy : dx) + scroll;
      drag.order = sortableOrder(drag.boxes, id, center);
      sortableOffsets(drag.boxes, drag.order).forEach((offset, itemId) => {
        const node = nodes.current.get(itemId);
        if (!node) return;
        node.style.setProperty('--sortable-x', `${axis === 'horizontal' ? offset : 0}px`);
        node.style.setProperty('--sortable-y', `${axis === 'vertical' ? offset : 0}px`);
        node.dataset.sortableState = itemId === id ? 'placeholder' : 'moving';
      });
    };

    const tick = () => {
      if (session.current !== drag || !drag.active || drag.pending) return;
      // Scroll the innermost available port. Outer ports take over at its limit.
      for (const { node } of drag.ports) {
        const bounds = isWindow(node)
          ? { top: 0, left: 0, right: innerWidth, bottom: innerHeight }
          : node.getBoundingClientRect();
        if (drag.x < bounds.left || drag.x > bounds.right || drag.y < bounds.top || drag.y > bounds.bottom) continue;
        const coordinate = axis === 'vertical' ? drag.y : drag.x;
        const low = axis === 'vertical' ? bounds.top : bounds.left;
        const high = axis === 'vertical' ? bounds.bottom : bounds.right;
        const edge = Math.min(40, (high - low) / 4);
        const amount = coordinate < low + edge ? -12 * (1 - (coordinate - low) / edge)
          : coordinate > high - edge ? 12 * (1 - (high - coordinate) / edge) : 0;
        if (!amount) continue;
        const before = position(node, axis);
        if (isWindow(node)) {
          const root = document.scrollingElement;
          const max = root ? axis === 'vertical' ? root.scrollHeight - innerHeight : root.scrollWidth - innerWidth : 0;
          if ((amount < 0 && before > 0) || (amount > 0 && before < max)) node.scrollBy(axis === 'horizontal' ? amount : 0, axis === 'vertical' ? amount : 0);
        } else if (axis === 'vertical') node.scrollTop += amount;
        else node.scrollLeft += amount;
        if (position(node, axis) !== before) break;
      }
      update();
      drag.frame = requestAnimationFrame(tick);
    };

    const begin = () => {
      drag.boxes = drag.ids.map(itemId => {
        const rect = nodes.current.get(itemId)?.getBoundingClientRect();
        return rect ? { id: itemId, start: axis === 'vertical' ? rect.top : rect.left, size: axis === 'vertical' ? rect.height : rect.width } : undefined;
      }).filter((box): box is Box => !!box);
      if (drag.boxes.length !== drag.ids.length || drag.boxes.some(box => box.size <= 0)) { cancel(); return; }
      drag.ports = scrollPorts(container).map(node => ({ node, start: position(node, axis) }));
      drag.clone = floatingCopy(item, drag.rect);
      drag.active = true;
      if (typeof handle.setPointerCapture === 'function') {
        try {
          handle.setPointerCapture(drag.pointerId);
          drag.releaseCapture = () => {
            drag.releaseCapture = undefined;
            try {
              if (typeof handle.releasePointerCapture === 'function' &&
                (typeof handle.hasPointerCapture !== 'function' || handle.hasPointerCapture(drag.pointerId))) {
                handle.releasePointerCapture(drag.pointerId);
              }
            } catch { /* The browser may already have released a cancelled pointer. */ }
          };
        } catch { /* Detached handles or unsupported pointer types keep the window fallback. */ }
      }
      const selection = document.body.style.userSelect;
      const cursor = document.body.style.cursor;
      document.body.style.userSelect = 'none';
      document.body.style.cursor = 'grabbing';
      drag.restoreSelection = () => { document.body.style.userSelect = selection; document.body.style.cursor = cursor; };
      container.dataset.sortableActive = 'true';
      if (typeof ResizeObserver !== 'undefined') {
        const sizes = new Map([container, ...drag.ids.map(itemId => nodes.current.get(itemId)!)].map(node => {
          const rect = node.getBoundingClientRect();
          return [node, { width: rect.width, height: rect.height }] as const;
        }));
        const observer = new ResizeObserver(entries => {
          if (session.current !== drag || drag.pending) return;
          // The observer's initial notification is expected. Compare border-box sizes
          // rather than its contentRect, which excludes the cards' padding/borders.
          if (entries.some(entry => {
            const before = sizes.get(entry.target as HTMLElement);
            const current = entry.target.getBoundingClientRect();
            return before && (Math.abs(current.width - before.width) > 0.5 || Math.abs(current.height - before.height) > 0.5);
          })) cancel();
        });
        drag.stopObserving = () => observer.disconnect();
        sizes.forEach((_, node) => observer.observe(node));
      }
      setStatus({ dragging: true, pending: false });
      update();
      drag.frame = requestAnimationFrame(tick);
    };

    const move = (native: PointerEvent) => {
      if (native.pointerId !== drag.pointerId) return;
      drag.x = native.clientX; drag.y = native.clientY;
      if (!drag.active && Math.hypot(drag.x - drag.startX, drag.y - drag.startY) >= 6) begin();
      if (drag.active) { native.preventDefault(); update(); }
    };
    const end = (native: PointerEvent) => {
      if (native.pointerId !== drag.pointerId) return;
      drag.x = native.clientX; drag.y = native.clientY;
      if (!drag.active) { clear(); return; }
      update();
      const target = latest.current.externalDrop?.resolve(native.clientX, native.clientY);
      if (target && latest.current.externalDrop) {
        suppressClick();
        const drop = latest.current.externalDrop.onDrop;
        clear();
        void drop(id, target).catch(() => { /* The consumer reports the safe failure. */ });
        return;
      }
      if (!containsVisiblePoint(container, native.clientX, native.clientY) || equal(drag.order, drag.ids)) { cancel(); return; }
      suppressClick();
      drag.pending = true;
      if (owner === token.current) owner = undefined;
      drag.removeListeners();
      drag.releaseCapture?.();
      drag.stopObserving?.(); drag.stopObserving = undefined;
      if (drag.frame !== undefined) cancelAnimationFrame(drag.frame);
      drag.clone?.remove(); drag.clone = undefined;
      const source = nodes.current.get(id);
      if (source) source.dataset.sortableState = 'pending';
      drag.restoreSelection?.(); drag.restoreSelection = undefined;
      setStatus({ dragging: false, pending: true });
      try {
        Promise.resolve(latest.current.onCommit([...drag.order])).then(() => {
          // Consumers own persistent order. Keep the preview until React renders it.
          if (session.current === drag && !equal(latest.current.ids, drag.ids)) clear();
        }, () => { if (session.current === drag) clear(); });
      } catch { clear(); }
    };
    const abort = (native: PointerEvent) => { if (native.pointerId === drag.pointerId) cancel(); };
    const lostCapture = (native: PointerEvent) => { if (drag.active && native.pointerId === drag.pointerId) cancel(); };
    const key = (native: KeyboardEvent) => { if (native.key === 'Escape') { native.preventDefault(); cancel(); } };
    const resize = () => cancel();
    window.addEventListener('pointermove', move, { passive: false });
    window.addEventListener('pointerup', end);
    window.addEventListener('pointercancel', abort);
    window.addEventListener('keydown', key);
    window.addEventListener('blur', resize);
    window.addEventListener('resize', resize);
    window.addEventListener('scroll', update, true);
    handle.addEventListener('lostpointercapture', lostCapture);
    drag.removeListeners = () => {
      window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', end);
      window.removeEventListener('pointercancel', abort); window.removeEventListener('keydown', key);
      window.removeEventListener('blur', resize); window.removeEventListener('resize', resize);
      window.removeEventListener('scroll', update, true);
      handle.removeEventListener('lostpointercapture', lostCapture);
    };
  }, [axis, cancel, clear, suppressClick]);

  return {
    listProps: { ref: (node: HTMLElement | null) => { list.current = node; }, 'data-sortable-list': axis },
    itemProps: (id: string) => {
      if (!callbacks.current.has(id)) callbacks.current.set(id, node => { if (node) nodes.current.set(id, node); else nodes.current.delete(id); });
      return { ref: callbacks.current.get(id)!, 'data-sortable-item': id, style: {} as CSSProperties };
    },
    handleProps: (id: string) => ({
      onPointerDown: (event: ReactPointerEvent<HTMLElement>) => start(id, event),
      onClickCapture: (event: React.MouseEvent<HTMLElement>) => {
        if (event.detail !== 0 && Date.now() < suppressUntil.current) { event.preventDefault(); event.stopPropagation(); }
      },
      onDragStart: (event: React.DragEvent<HTMLElement>) => event.preventDefault(),
      style: { touchAction: 'none' } as CSSProperties,
    }),
    cancel, ...status,
  };
}
