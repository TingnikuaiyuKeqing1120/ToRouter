// @vitest-environment jsdom
import { useRef } from 'react';
import { act, cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { usePetWindowSize } from './desktop';
function View(){const root=useRef<HTMLDivElement>(null);usePetWindowSize(root);return <div ref={root}/>;}
afterEach(()=>{cleanup();Reflect.deleteProperty(window,'__TO_ROUTER_DESKTOP__');vi.useRealTimers();vi.unstubAllGlobals();vi.restoreAllMocks();});
describe('native measured resize contract',()=>{
  it('starts measuring when the native marker arrives after React mounted',async()=>{
    vi.spyOn(HTMLElement.prototype,'getBoundingClientRect').mockImplementation(()=>({height:350} as DOMRect));
    vi.stubGlobal('ResizeObserver',class{ observe(){} disconnect(){} });
    const fetcher=vi.fn().mockResolvedValue(new Response('{}',{status:200}));vi.stubGlobal('fetch',fetcher);
    render(<View/>);expect(fetcher).not.toHaveBeenCalled();
    await act(async()=>{Object.defineProperty(window,'__TO_ROUTER_DESKTOP__',{configurable:true,value:{pet:true,origin:'http://127.0.0.1:19191'}});window.dispatchEvent(new Event('to-router-desktop-ready'));});
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({height:350});
  });
  it('retries DOM-not-ready and sends later content heights as managed POST requests',async()=>{
    vi.useFakeTimers();Object.defineProperty(window,'__TO_ROUTER_DESKTOP__',{configurable:true,value:{pet:true,origin:'http://127.0.0.1:19191'}});
    let height=350,measure=()=>{};
    vi.spyOn(HTMLElement.prototype,'getBoundingClientRect').mockImplementation(()=>({height} as DOMRect));
    vi.stubGlobal('ResizeObserver',class{ constructor(callback:()=>void){measure=callback;} observe(){} disconnect(){} });
    const fetcher=vi.fn().mockResolvedValueOnce(new Response('',{status:503})).mockImplementation(()=>Promise.resolve(new Response('{}',{status:200})));
    vi.stubGlobal('fetch',fetcher);const rendered=render(<View/>);
    await act(async()=>{});expect(fetcher).toHaveBeenCalledTimes(1);
    await act(async()=>vi.advanceTimersByTimeAsync(300));expect(fetcher).toHaveBeenCalledTimes(2);
    await act(async()=>{height=428;measure();});expect(fetcher).toHaveBeenCalledTimes(3);
    const [url,options]=fetcher.mock.calls[2];
    expect(url).toBe('/api/desktop/resize-pet');expect(options.method).toBe('POST');
    expect(new Headers(options.headers).get('X-To-Router')).toBe('1');expect(JSON.parse(options.body)).toEqual({height:428});
    rendered.unmount();await act(async()=>vi.advanceTimersByTimeAsync(1000));expect(fetcher).toHaveBeenCalledTimes(3);
  });
});
