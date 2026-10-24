// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useState } from 'react';
import { sortableOffsets, sortableOrder, useSortableList } from './useSortableList';

type HarnessProps = {
  initial?: string[]; controlledIds?: string[]; disabled?: boolean; revision?: number;
  axis?: 'vertical' | 'horizontal'; name?: string;
  save?: (ids: string[]) => Promise<void>; click?: () => void;
};
function Harness({ initial = ['a', 'b', 'c'], controlledIds, disabled, revision, axis = 'vertical', name = 'one', save, click }: HarnessProps) {
  const [localIds, setIds] = useState(initial);
  const ids = controlledIds ?? localIds;
  const sort = useSortableList({ ids, disabled, revision, axis, onCommit: async order => {
    await save?.(order);
    setIds(order);
  } });
  return <div {...sort.listProps} data-testid={name} data-phase={sort.pending ? 'pending' : sort.dragging ? 'dragging' : 'idle'}>
    {ids.map(id => <article {...sort.itemProps(id)} key={id} data-testid={`${name}-${id}`}>
      <button {...sort.handleProps(id)} aria-label={`${name}-${id}-handle`}>move</button>
      <button onClick={click}>{name}-{id}-action</button>
    </article>)}
  </div>;
}

function rect(x: number, y: number, width: number, height: number): DOMRect {
  return { x, y, left: x, top: y, right: x + width, bottom: y + height, width, height, toJSON: () => ({}) } as DOMRect;
}
function measure(name = 'one', axis: 'vertical' | 'horizontal' = 'vertical', sizes = [40, 80, 60]) {
  const list = screen.getByTestId(name);
  vi.spyOn(list, 'getBoundingClientRect').mockReturnValue(rect(10, 10, 300, 300));
  let position = 10;
  ['a', 'b', 'c'].forEach((id, i) => {
    const node = screen.getByTestId(`${name}-${id}`);
    vi.spyOn(node, 'getBoundingClientRect').mockReturnValue(axis === 'vertical' ? rect(10, position, 280, sizes[i]) : rect(position, 10, sizes[i], 80));
    position += sizes[i] + 10;
  });
  return list;
}
function pointer(type: string, target: Element | Window, x: number, y: number, pointerId = 1) {
  const event = new MouseEvent(type, { bubbles: true, cancelable: true, clientX: x, clientY: y, button: 0 });
  Object.defineProperties(event, { pointerId: { value: pointerId }, isPrimary: { value: true }, pointerType: { value: 'mouse' } });
  fireEvent(target, event);
}
const down = (id = 'a', x = 20, y = 30, name = 'one') => pointer('pointerdown', screen.getByRole('button', { name: `${name}-${id}-handle` }), x, y);
const move = (x: number, y: number) => pointer('pointermove', window, x, y);
const up = (x: number, y: number) => pointer('pointerup', window, x, y);
const frame = () => act(() => { const callbacks = [...frames.values()]; frames.clear(); callbacks.forEach(callback => callback(0)); });
let frames: Map<number, FrameRequestCallback>;

beforeEach(() => {
  frames = new Map(); let sequence = 0;
  vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { frames.set(++sequence, callback); return sequence; });
  vi.stubGlobal('cancelAnimationFrame', (id: number) => frames.delete(id));
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('measured ordering', () => {
  const boxes = [{ id: 'a', start: 10, size: 40 }, { id: 'b', start: 60, size: 80 }, { id: 'c', start: 150, size: 60 }];
  it('uses original centers and variable sizes without index oscillation', () => {
    expect(sortableOrder(boxes, 'a', 101)).toEqual(['b', 'a', 'c']);
    expect(sortableOrder(boxes, 'a', 101)).toEqual(['b', 'a', 'c']);
    expect(sortableOrder(boxes, 'a', 181)).toEqual(['b', 'c', 'a']);
    expect([...sortableOffsets(boxes, ['b', 'a', 'c'])]).toEqual([['b', -50], ['a', 90], ['c', 0]]);
    expect([...sortableOffsets(boxes, ['b', 'c', 'a'])]).toEqual([['b', -50], ['c', -50], ['a', 160]]);
  });
});

it('starts only from a handle after threshold, follows pointer and opens a measured placeholder', () => {
  const save = vi.fn().mockResolvedValue(undefined), click = vi.fn();
  render(<Harness save={save} click={click} />); const list = measure();
  pointer('pointerdown', screen.getByText('one-a-action'), 20, 30); move(20, 180);
  expect(document.querySelector('.sortable-floating')).toBeNull();
  down(); move(23, 32); up(23, 32);
  expect(save).not.toHaveBeenCalled(); expect(list.dataset.phase).toBe('idle');
  fireEvent.click(screen.getByText('one-a-action')); expect(click).toHaveBeenCalledOnce();
  down(); move(20, 110);
  const floating = document.querySelector<HTMLElement>('.sortable-floating')!;
  expect(floating.style.transform).toBe('translate3d(0px, 80px, 0)');
  expect(floating.getAttribute('aria-hidden')).toBe('true');
  expect(floating.style.boxShadow).toContain('0 10px 26px');
  expect(floating.style.outline).toContain('1px solid');
  expect(floating.querySelector('[data-sortable-item]')).toBeNull();
  expect(screen.getByTestId('one-b').style.getPropertyValue('--sortable-y')).toBe('-50px');
  expect(screen.getByTestId('one-a').style.getPropertyValue('--sortable-y')).toBe('90px');
  expect(screen.getByTestId('one-a').dataset.sortableState).toBe('placeholder');
  expect(save).not.toHaveBeenCalled();
});

it('commits once on release and keeps the projected positions until successful ids acknowledgement', async () => {
  let finish!: () => void;
  const save = vi.fn(() => new Promise<void>(resolve => { finish = resolve; }));
  render(<Harness save={save} />); const list = measure();
  down(); move(20, 190); up(20, 190); up(20, 190);
  expect(save).toHaveBeenCalledOnce(); expect(save).toHaveBeenCalledWith(['b', 'c', 'a']);
  expect(list.dataset.phase).toBe('pending');
  expect(screen.getByTestId('one-a').style.getPropertyValue('--sortable-y')).toBe('160px');
  expect(document.querySelector('.sortable-floating')).toBeNull();
  await act(async () => finish());
  expect(list.dataset.phase).toBe('idle');
  expect([...list.children].map(node => node.getAttribute('data-sortable-item'))).toEqual(['b', 'c', 'a']);
  expect(screen.getByTestId('one-a').style.getPropertyValue('--sortable-y')).toBe('');
});

it('restores the actual order and all visual state after rejection', async () => {
  const save = vi.fn().mockRejectedValue(new Error('save refused'));
  document.body.style.userSelect = 'text'; document.body.style.cursor = 'crosshair';
  render(<Harness save={save} />); const list = measure();
  down(); move(20, 190); up(20, 190);
  await act(async () => {});
  expect([...list.children].map(node => node.getAttribute('data-sortable-item'))).toEqual(['a', 'b', 'c']);
  expect(list.dataset.phase).toBe('idle');
  expect(screen.getByTestId('one-a').hasAttribute('data-sortable-state')).toBe(false);
  expect(document.body.style.userSelect).toBe('text'); expect(document.body.style.cursor).toBe('crosshair');
  document.body.style.userSelect = ''; document.body.style.cursor = '';
});

it.each(['Escape', 'pointercancel', 'outside', 'resize', 'blur'])('cancels %s without saving', kind => {
  const save = vi.fn(); render(<Harness save={save} />); const list = measure();
  down(); move(20, 190);
  if (kind === 'Escape') fireEvent.keyDown(window, { key: 'Escape' });
  else if (kind === 'pointercancel') pointer('pointercancel', window, 20, 190);
  else if (kind === 'outside') up(400, 190);
  else fireEvent(window, new Event(kind));
  up(20, 190);
  expect(save).not.toHaveBeenCalled(); expect(list.dataset.phase).toBe('idle');
  expect(document.querySelector('.sortable-floating')).toBeNull();
});

it('does not commit an unchanged order or pointer events belonging to another pointer', () => {
  const save = vi.fn(); render(<Harness save={save} />); measure();
  down(); pointer('pointermove', window, 20, 190, 2); pointer('pointerup', window, 20, 190, 2);
  expect(document.querySelector('.sortable-floating')).toBeNull();
  move(20, 37); up(20, 37);
  expect(save).not.toHaveBeenCalled();
});

it('cancels on external ids/revision/disabled changes and unmount, but not busy during save', async () => {
  const save = vi.fn().mockResolvedValue(undefined);
  const view = render(<Harness save={save} revision={0} />); const list = measure();
  down(); move(20, 190); view.rerender(<Harness save={save} revision={1} />); up(20, 190);
  expect(save).not.toHaveBeenCalled(); expect(list.dataset.phase).toBe('idle');
  down(); move(20, 190); view.rerender(<Harness save={save} revision={1} disabled />); up(20, 190);
  expect(save).not.toHaveBeenCalled();
  view.rerender(<Harness save={save} revision={1} />); down(); move(20, 190); view.unmount();
  expect(document.querySelector('.sortable-floating')).toBeNull(); expect(frames.size).toBe(0);
  const second = render(<Harness save={save} />); measure(); down(); move(20, 190); up(20, 190);
  second.rerender(<Harness save={save} disabled revision={10} />);
  await act(async () => {});
  expect(save).toHaveBeenCalledOnce(); expect(screen.getByTestId('one').dataset.phase).toBe('idle');
});

it('supports horizontal measured sorting and list end whitespace', async () => {
  const save = vi.fn().mockResolvedValue(undefined);
  render(<Harness axis="horizontal" save={save} />); measure('one', 'horizontal');
  down('a', 30, 20); move(295, 20);
  expect(screen.getByTestId('one-b').style.getPropertyValue('--sortable-x')).toBe('-50px');
  up(295, 20); await act(async () => {});
  expect(save).toHaveBeenCalledOnce(); expect(save).toHaveBeenCalledWith(['b', 'c', 'a']);
});

it('isolates lists, cancels drops outside the original list and suppresses the follow-up click only', () => {
  const save = vi.fn(), otherSave = vi.fn(), click = vi.fn();
  render(<><Harness save={save} click={click} /><Harness name="two" save={otherSave} click={click} /></>);
  measure(); measure('two');
  vi.spyOn(screen.getByTestId('two'), 'getBoundingClientRect').mockReturnValue(rect(400, 10, 300, 300));
  down(); move(420, 190); up(420, 190);
  expect(save).not.toHaveBeenCalled(); expect(otherSave).not.toHaveBeenCalled();
  fireEvent.click(screen.getByText('two-a-action'), { detail: 1 }); expect(click).not.toHaveBeenCalled();
  fireEvent.click(screen.getByText('two-a-action'), { detail: 0 }); expect(click).toHaveBeenCalledOnce();
  fireEvent.click(screen.getByText('two-a-action'), { detail: 1 }); expect(click).toHaveBeenCalledTimes(2);
  expect(screen.getByTestId('two-a').style.getPropertyValue('--sortable-y')).toBe('');
});

it('tracks internal manual scroll and edge autoscroll while leaving a normal page scroll alone', () => {
  const save = vi.fn(); render(<Harness save={save} />); const list = measure();
  list.style.overflowY = 'auto';
  Object.defineProperty(list, 'scrollHeight', { value: 700 });
  Object.defineProperty(list, 'clientHeight', { value: 300 });
  down(); move(20, 50);
  list.scrollTop = 100; fireEvent.scroll(list);
  expect(screen.getByTestId('one-a').style.getPropertyValue('--sortable-y')).toBe('90px');
  move(20, 300); const before = list.scrollTop; frame();
  expect(list.scrollTop).toBeGreaterThan(before);
  fireEvent.keyDown(window, { key: 'Escape' });
  const normal = new MouseEvent('pointermove', { cancelable: true }); window.dispatchEvent(normal);
  expect(normal.defaultPrevented).toBe(false); expect(save).not.toHaveBeenCalled();
});

it('cancels an in-progress gesture on external ids change without saving a stale order', () => {
  const save = vi.fn(); const view = render(<Harness save={save} controlledIds={['a', 'b', 'c']} />); measure();
  down(); move(20, 190);
  view.rerender(<Harness save={save} controlledIds={['b', 'a', 'c']} />); up(20, 190);
  expect(save).not.toHaveBeenCalled(); expect(screen.getByTestId('one').dataset.phase).toBe('idle');
  expect(document.querySelector('.sortable-floating')).toBeNull();
});

it('rejects a release in the clipped part of a scroll ancestor', () => {
  const save = vi.fn(); render(<div data-testid="viewport" style={{ overflowY: 'auto' }}><Harness save={save} /></div>);
  measure(); vi.spyOn(screen.getByTestId('viewport'), 'getBoundingClientRect').mockReturnValue(rect(10, 10, 300, 120));
  down(); move(20, 190); up(20, 190);
  expect(save).not.toHaveBeenCalled(); expect(screen.getByTestId('one').dataset.phase).toBe('idle');
});

it('keeps only the original DOM identity and focus while the inert floating copy exists', () => {
  render(<Harness />); measure();
  const source = screen.getByTestId('one-a'); source.id = 'card-identity';
  const handle = screen.getByRole('button', { name: 'one-a-handle' }); handle.focus();
  down(); move(20, 110);
  expect(document.querySelectorAll('#card-identity')).toHaveLength(1);
  const clone = document.querySelector<HTMLElement>('.sortable-floating')!;
  expect(clone.inert).toBe(true); expect(clone.querySelector('button')!.style.pointerEvents).toBe('none');
  fireEvent.keyDown(window, { key: 'Escape' });
  expect(document.activeElement).toBe(handle);
});

it('keeps the measured geometry after the first resize notification, then cancels actual item/container size changes', () => {
  let report!: ResizeObserverCallback;
  const observed = new Set<Element>();
  const disconnect = vi.fn(() => observed.clear());
  vi.stubGlobal('ResizeObserver', class {
    constructor(callback: ResizeObserverCallback) { report = callback; }
    observe = (element: Element) => { observed.add(element); };
    disconnect = disconnect;
  });
  const save = vi.fn(); render(<Harness save={save} />); const list = measure();
  const notify = (element: Element) => act(() => report([{ target: element } as ResizeObserverEntry], {} as ResizeObserver));
  down(); move(20, 110);
  expect(observed.size).toBe(4);
  notify(screen.getByTestId('one-b'));
  expect(list.dataset.phase).toBe('dragging'); expect(disconnect).not.toHaveBeenCalled();
  vi.mocked(screen.getByTestId('one-b').getBoundingClientRect).mockReturnValue(rect(10, 60, 280, 130));
  notify(screen.getByTestId('one-b')); up(20, 190);
  expect(list.dataset.phase).toBe('idle'); expect(save).not.toHaveBeenCalled();
  expect(disconnect).toHaveBeenCalledOnce(); expect(observed.size).toBe(0);
  down(); move(20, 110);
  vi.mocked(list.getBoundingClientRect).mockReturnValue(rect(10, 10, 280, 300));
  notify(list);
  expect(list.dataset.phase).toBe('idle'); expect(save).not.toHaveBeenCalled();
});

it('disconnects geometry observation before saving and on unmount', async () => {
  const disconnect = vi.fn();
  vi.stubGlobal('ResizeObserver', class {
    observe() {}
    disconnect = disconnect;
  });
  const save = vi.fn().mockResolvedValue(undefined);
  const view = render(<Harness save={save} />); measure();
  down(); move(20, 190); up(20, 190);
  expect(disconnect).toHaveBeenCalledOnce();
  await act(async () => {});
  down('b'); move(20, 110); view.unmount();
  expect(disconnect).toHaveBeenCalledTimes(2);
});

it('counts document root scrolling once while retaining an independent body scroll container', () => {
  const root = document.documentElement;
  const scrollingDescriptor = Object.getOwnPropertyDescriptor(document, 'scrollingElement');
  const scrollYDescriptor = Object.getOwnPropertyDescriptor(window, 'scrollY');
  const rootOverflow = root.style.overflowY, bodyOverflow = document.body.style.overflowY;
  const rootScroll = root.scrollTop, bodyScroll = document.body.scrollTop;
  try {
    Object.defineProperty(document, 'scrollingElement', { configurable: true, value: root });
    Object.defineProperty(window, 'scrollY', { configurable: true, writable: true, value: 0 });
    root.style.overflowY = 'auto'; document.body.style.overflowY = 'auto';
    root.scrollTop = 0; document.body.scrollTop = 0;
    render(<Harness />); measure(); down(); move(20, 50);
    root.scrollTop = 40;
    Object.defineProperty(window, 'scrollY', { configurable: true, writable: true, value: 40 });
    fireEvent.scroll(window);
    // Pointer center 50 + root scroll 40 stays before b's measured center 100.
    expect(screen.getByTestId('one-a').style.getPropertyValue('--sortable-y')).toBe('0px');
    document.body.scrollTop = 40; fireEvent.scroll(document.body);
    expect(screen.getByTestId('one-a').style.getPropertyValue('--sortable-y')).toBe('90px');
    fireEvent.keyDown(window, { key: 'Escape' });
  } finally {
    root.style.overflowY = rootOverflow; document.body.style.overflowY = bodyOverflow;
    root.scrollTop = rootScroll; document.body.scrollTop = bodyScroll;
    if (scrollingDescriptor) Object.defineProperty(document, 'scrollingElement', scrollingDescriptor);
    else Reflect.deleteProperty(document, 'scrollingElement');
    if (scrollYDescriptor) Object.defineProperty(window, 'scrollY', scrollYDescriptor);
    else Reflect.deleteProperty(window, 'scrollY');
  }
});

function mockCapture(handle: HTMLElement) {
  const pointers = new Set<number>();
  const capture = vi.fn((id: number) => { pointers.add(id); });
  const release = vi.fn((id: number) => {
    pointers.delete(id);
    // Native release can synchronously notify; listeners must already be removed.
    pointer('lostpointercapture', handle, 20, 190, id);
  });
  Object.assign(handle, {
    setPointerCapture: capture, releasePointerCapture: release,
    hasPointerCapture: (id: number) => pointers.has(id),
  });
  return { capture, release, pointers };
}

it('captures only after the threshold and releases before saving without cancelling the pending preview', async () => {
  let finish!: () => void;
  const save = vi.fn(() => new Promise<void>(resolve => { finish = resolve; }));
  render(<Harness save={save} />); const list = measure();
  const handle = screen.getByRole('button', { name: 'one-a-handle' });
  const { capture, release } = mockCapture(handle);
  down(); move(22, 32); expect(capture).not.toHaveBeenCalled();
  move(20, 190); expect(capture).toHaveBeenCalledOnce(); expect(capture).toHaveBeenCalledWith(1);
  up(20, 190);
  expect(release).toHaveBeenCalledOnce(); expect(release).toHaveBeenCalledWith(1);
  expect(list.dataset.phase).toBe('pending');
  expect(screen.getByTestId('one-a').style.getPropertyValue('--sortable-y')).toBe('160px');
  expect(save).toHaveBeenCalledOnce();
  await act(async () => finish());
  expect(list.dataset.phase).toBe('idle'); expect(release).toHaveBeenCalledOnce();
});

it('cancels a lost captured pointer without saving and removes capture listeners on unmount', () => {
  const save = vi.fn(); const view = render(<Harness save={save} />); const list = measure();
  const handle = screen.getByRole('button', { name: 'one-a-handle' });
  const { capture, release, pointers } = mockCapture(handle);
  down(); move(20, 190);
  pointer('lostpointercapture', handle, 20, 190, 2);
  expect(list.dataset.phase).toBe('dragging');
  pointers.delete(1); pointer('lostpointercapture', handle, 20, 190);
  move(20, 190); up(20, 190);
  expect(list.dataset.phase).toBe('idle'); expect(save).not.toHaveBeenCalled();
  expect(document.querySelector('.sortable-floating')).toBeNull();
  expect(release).not.toHaveBeenCalled();
  down(); move(20, 190); expect(capture).toHaveBeenCalledTimes(2);
  view.unmount(); expect(release).toHaveBeenCalledOnce();
  pointer('lostpointercapture', handle, 20, 190);
  expect(save).not.toHaveBeenCalled(); expect(frames.size).toBe(0);
});

it('allows a new explicit pointer gesture immediately after a drag with no trailing click', () => {
  const click = vi.fn();
  render(<><Harness save={() => new Promise<void>(() => {})} /><button onClick={click}>System settings</button></>);
  measure(); down(); move(20, 190); up(20, 190);
  const settings = screen.getByRole('button', { name: 'System settings' });
  // Keyboard activation remains available without consuming the drag suppression.
  fireEvent.click(settings, { detail: 0 }); expect(click).toHaveBeenCalledOnce();
  pointer('pointerdown', settings, 400, 20); pointer('pointerup', settings, 400, 20);
  fireEvent.click(settings, { detail: 1 }); expect(click).toHaveBeenCalledTimes(2);
});

it('clears handle click suppression on a fresh pointerdown as well as the document listener', () => {
  render(<Harness />); measure(); down(); move(20, 190);
  fireEvent.keyDown(window, { key: 'Escape' });
  const handle = screen.getByRole('button', { name: 'one-a-handle' });
  down(); up(20, 30);
  const click = new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 });
  fireEvent(handle, click);
  expect(click.defaultPrevented).toBe(false);
});

it.each(['new pointerdown', 'timeout', 'unmount', 'trailing click'])('removes both suppression listeners and the timer after %s', trigger => {
  vi.useFakeTimers();
  try {
    const add = vi.spyOn(document, 'addEventListener'), remove = vi.spyOn(document, 'removeEventListener');
    const click = vi.fn();
    const view = render(<><Harness save={() => new Promise<void>(() => {})} /><button onClick={click}>System settings</button></>);
    measure(); down(); move(20, 190); up(20, 190);
    const clickListener = add.mock.calls.filter(args => args[0] === 'click' && args[2] === true).slice(-1)[0][1];
    const pointerListener = add.mock.calls.filter(args => args[0] === 'pointerdown' && args[2] === true).slice(-1)[0][1];
    expect(vi.getTimerCount()).toBe(1);
    const settings = screen.getByRole('button', { name: 'System settings' });
    if (trigger === 'new pointerdown') pointer('pointerdown', settings, 400, 20);
    else if (trigger === 'timeout') act(() => vi.advanceTimersByTime(350));
    else if (trigger === 'unmount') view.unmount();
    else { fireEvent.click(settings, { detail: 1 }); expect(click).not.toHaveBeenCalled(); }
    expect(remove).toHaveBeenCalledWith('click', clickListener, true);
    expect(remove).toHaveBeenCalledWith('pointerdown', pointerListener, true);
    expect(vi.getTimerCount()).toBe(0);
    if (trigger !== 'unmount') { fireEvent.click(settings, { detail: 1 }); expect(click).toHaveBeenCalledOnce(); }
  } finally { vi.useRealTimers(); }
});

