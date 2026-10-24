import { useEffect, useState, type RefObject } from 'react';
import { api } from '../api';

declare global { interface Window {
  readonly __TO_ROUTER_DESKTOP__?: Readonly<{pet: boolean; origin: string}>;
  runtime?: {WindowSetLightTheme?: () => void; WindowSetDarkTheme?: () => void; BrowserOpenURL?: (url: string) => void};
} }
export const isNativePet = () => window.__TO_ROUTER_DESKTOP__?.pet === true;

export function useDesktopReady() {
  const [ready, setReady] = useState(() => !!window.__TO_ROUTER_DESKTOP__);
  useEffect(() => {
    const arrived = () => setReady(!!window.__TO_ROUTER_DESKTOP__);
    window.addEventListener('to-router-desktop-ready', arrived); arrived();
    return () => window.removeEventListener('to-router-desktop-ready', arrived);
  }, []);
  return ready;
}

// Content controls native height; browser views never call a native resize API.
export function usePetWindowSize(element: RefObject<HTMLElement>) {
  const ready = useDesktopReady();
  useEffect(() => {
    if (!isNativePet() || !element.current) return;
    let disposed = false;
    let wanted = 200;
    let applied = 0;
    let running = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let controller: AbortController | null = null;
    const resize = async () => {
      if (disposed || running || wanted === applied) return;
      running = true; const height = wanted;
      controller = new AbortController();
      const timeout = setTimeout(() => controller?.abort(), 5000);
      try {
        await api('/desktop/resize-pet', {method:'POST',body:JSON.stringify({height}),signal:controller.signal});
        applied = height;
      } catch { if (!disposed) retry = setTimeout(() => void resize(), 300); }
      finally { clearTimeout(timeout); running = false; if (!disposed && applied === height && wanted !== applied) void resize(); }
    };
    const measure = () => {
      if (!element.current) return;
      wanted = Math.max(200, Math.min(700, Math.ceil(element.current.getBoundingClientRect().height)));
      void resize();
    };
    const observer = new ResizeObserver(measure);
    observer.observe(element.current); measure();
    return () => { disposed = true; observer.disconnect(); clearTimeout(retry); controller?.abort(); };
  }, [element, ready]);
}
