// @vitest-environment jsdom
import { act, cleanup, fireEvent, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CompanionStage } from './CompanionStage';

describe('CompanionStage presentation lifecycle', () => {
  let frames: Map<number, FrameRequestCallback>;
  let nextFrame: number;
  let hidden: boolean;
  let reduced: boolean;
  let motionListeners: Set<() => void>;

  const step = (time: number) => act(() => {
    const pending = [...frames.values()];
    frames.clear();
    pending.forEach(callback => callback(time));
  });
  const visibility = (value: boolean) => act(() => {
    hidden = value;
    document.dispatchEvent(new Event('visibilitychange'));
  });

  beforeEach(() => {
    frames = new Map();
    nextFrame = 0;
    hidden = false;
    reduced = false;
    motionListeners = new Set();
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      const id = ++nextFrame;
      frames.set(id, callback);
      return id;
    });
    vi.stubGlobal('cancelAnimationFrame', (id: number) => frames.delete(id));
    vi.stubGlobal('matchMedia', () => ({
      get matches() { return reduced; },
      addEventListener: (_type: string, listener: () => void) => motionListeners.add(listener),
      removeEventListener: (_type: string, listener: () => void) => motionListeners.delete(listener),
    }));
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('ends ordinary click celebration using elapsed time, without changing its supplied scene', () => {
    const onGreet = vi.fn();
    const { container, getByRole } = render(<CompanionStage scene="work" theme="dark" onGreet={onGreet} />);
    const stage = container.firstElementChild!;
    const button = getByRole('button', { name: '摸摸蓝鲸和 Clawd' });
    expect(button.tagName).toBe('BUTTON');
    fireEvent.click(button);
    expect(stage.getAttribute('data-greeting')).toBe('true');
    expect(stage.getAttribute('data-reaction')).toBe('cheer');
    expect(onGreet).toHaveBeenCalledOnce();
    step(0);
    step(3000); // Low frame rate still expires a 2.1-second response.
    expect(stage.getAttribute('data-greeting')).toBe('false');
    expect(stage.getAttribute('data-scene')).toBe('work');
  });

  it('cancels an old greeting on scene switch and keeps done until its parent changes it', () => {
    const { container, getByRole, rerender } = render(<CompanionStage scene="idle" theme="dark" />);
    const stage = container.firstElementChild!;
    fireEvent.click(getByRole('button'));
    step(0);
    rerender(<CompanionStage scene="done" theme="dark" />);
    expect(stage.getAttribute('data-greeting')).toBe('false');
    step(1000);
    step(10000);
    expect(stage.getAttribute('data-scene')).toBe('done');
    expect(container.querySelector('.scene.is-active')?.getAttribute('data-scene')).toBe('done');
  });

  it.each(['retry', 'error', 'done'] as const)('responds with a bubble while retaining %s artwork', scene => {
    const { container, getByRole } = render(<CompanionStage scene={scene} theme="light" />);
    fireEvent.click(getByRole('button'));
    expect(container.firstElementChild?.getAttribute('data-reaction')).toBe('notice');
    expect(container.firstElementChild?.getAttribute('data-greeting')).toBe('true');
    expect(container.querySelector('.scene.is-active')?.getAttribute('data-scene')).toBe(scene);
    expect(getByRole('status').textContent).not.toBe('');
  });

  it('freezes greeting and tail time while paused or hidden, and cancels its frame on unmount', () => {
    const { container, getByRole, rerender, unmount } = render(<CompanionStage scene="surf" theme="dark" />);
    const stage = container.firstElementChild!;
    const tail = container.querySelector('.scene[data-scene="surf"] .whale-outline')!;
    fireEvent.click(getByRole('button'));
    step(0);
    step(600);
    const outline = tail.getAttribute('d');
    rerender(<CompanionStage scene="surf" theme="dark" paused />);
    expect(frames.size).toBe(0);
    step(90000);
    expect(stage.getAttribute('data-greeting')).toBe('true');
    expect(tail.getAttribute('d')).toBe(outline);
    rerender(<CompanionStage scene="surf" theme="dark" />);
    visibility(true);
    expect(frames.size).toBe(0);
    expect(stage.getAttribute('data-paused')).toBe('true');
    visibility(false);
    step(100000); // First resumed frame does not include background duration.
    expect(stage.getAttribute('data-greeting')).toBe('true');
    expect(tail.getAttribute('d')).toBe(outline);
    step(101600);
    expect(stage.getAttribute('data-greeting')).toBe('false');
    expect(tail.getAttribute('d')).not.toBe(outline);
    unmount();
    expect(frames.size).toBe(0);
    expect(motionListeners.size).toBe(0);
  });

  it('honors live reduced-motion changes and removes its listener', () => {
    reduced = true;
    const { container, getByRole, unmount } = render(<CompanionStage scene="work" theme="dark" />);
    expect(container.firstElementChild?.getAttribute('data-paused')).toBe('true');
    expect(frames.size).toBe(0);
    fireEvent.click(getByRole('button'));
    expect(container.firstElementChild?.getAttribute('data-greeting')).toBe('false');
    act(() => { reduced = false; motionListeners.forEach(listener => listener()); });
    expect(container.firstElementChild?.getAttribute('data-paused')).toBe('false');
    expect(frames.size).toBe(1);
    unmount();
    expect(motionListeners.size).toBe(0);
    expect(frames.size).toBe(0);
  });

  it('gives each instance local SVG references without changing document theme or scene', () => {
    const rootTheme = document.documentElement.getAttribute('data-theme');
    const rootScene = document.documentElement.getAttribute('data-scene');
    const { container } = render(<><CompanionStage scene="idle" theme="dark" /><CompanionStage scene="surf" theme="light" /></>);
    const ids = [...container.querySelectorAll('[id]')].map(element => element.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const stage of container.children) {
      const localIds = new Set([...stage.querySelectorAll('[id]')].map(element => element.id));
      for (const element of stage.querySelectorAll('[href], [fill], [clip-path]')) {
        for (const name of ['href', 'fill', 'clip-path']) {
          const value = element.getAttribute(name) ?? '';
          const referenced = name === 'href' && value.startsWith('#') ? value.slice(1) : value.match(/^url\(#(.+)\)$/)?.[1];
          if (referenced) expect(localIds.has(referenced)).toBe(true);
        }
      }
    }
    expect(document.documentElement.getAttribute('data-theme')).toBe(rootTheme);
    expect(document.documentElement.getAttribute('data-scene')).toBe(rootScene);
  });
});
