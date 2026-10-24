// @vitest-environment jsdom
import { act,cleanup,renderHook,waitFor } from '@testing-library/react';
import { afterEach,beforeEach,describe,expect,it,vi } from 'vitest';
import { useBackendLifecycle } from './backendLifecycle';

const port=Number(new URL(window.location.origin).port||80);
const ready={service:'to-router',installation_id:'install-A',instance_id:'process-A',port};
const response=(value:unknown)=>new Response(JSON.stringify(value),{status:200});
function deferred(){let resolve!:(value:Response)=>void;return {promise:new Promise<Response>(done=>resolve=done),resolve:(value:Response)=>resolve(value)};}
beforeEach(()=>{vi.stubGlobal('fetch',vi.fn());});
afterEach(()=>{cleanup();vi.useRealTimers();vi.unstubAllGlobals();vi.restoreAllMocks();});

describe('backend lifecycle observation',()=>{
  it('waits beyond the normal restart grace and accepts only a new instance after updating, including rollback',async()=>{
    vi.useFakeTimers();const event=deferred(),nextWatch=deferred();let watches=0,successor=false;
    const fetcher=vi.fn().mockImplementation((url:string)=>{
      if(url.includes('watch=1'))return ++watches===1?event.promise:nextWatch.promise;
      return Promise.resolve(response(successor?{...ready,instance_id:'rollback-process',version:'1.0.0'}:ready));
    });vi.stubGlobal('fetch',fetcher);const reconnect=vi.fn();const view=renderHook(()=>useBackendLifecycle(true,reconnect));
    await act(async()=>{await vi.advanceTimersByTimeAsync(0);});
    await act(async()=>event.resolve(response({...ready,state:'updating'})));
    expect(view.result.current.state).toBe('updating');expect(reconnect).not.toHaveBeenCalled();
    await act(async()=>{await vi.advanceTimersByTimeAsync(45000);});
    expect(view.result.current.state).toBe('updating');expect(reconnect).not.toHaveBeenCalled();
    act(()=>view.result.current.expectUpdate(false));
    await act(async()=>{await vi.advanceTimersByTimeAsync(500);});
    expect(view.result.current.state).toBe('updating');
    successor=true;
    await act(async()=>{await vi.advanceTimersByTimeAsync(500);});
    expect(reconnect).toHaveBeenCalledTimes(1);
    expect(reconnect).toHaveBeenCalledWith(window.location.origin,{...ready,instance_id:'rollback-process',version:'1.0.0'},true);
    expect(view.result.current.state).toBe('running');view.unmount();
  });
  it('uses update intent if the lifecycle watch disconnects before the updating event arrives',async()=>{
    vi.useFakeTimers();const event=deferred();let watch=false;
    const fetcher=vi.fn().mockImplementation((url:string)=>{
      if(url.includes('watch=1')){watch=true;return event.promise;}
      return watch?Promise.reject(new TypeError('offline')):Promise.resolve(response(ready));
    });vi.stubGlobal('fetch',fetcher);const reconnect=vi.fn();const view=renderHook(()=>useBackendLifecycle(true,reconnect));
    await act(async()=>{await vi.advanceTimersByTimeAsync(0);});
    act(()=>view.result.current.expectUpdate(true));
    // A failed watch response follows the same recovery path as a transport loss.
    await act(async()=>event.resolve(new Response('',{status:503})));
    await act(async()=>{await vi.advanceTimersByTimeAsync(45000);});
    expect(view.result.current.state).toBe('updating');
    await act(async()=>{await vi.advanceTimersByTimeAsync(46000);});
    expect(view.result.current.state).toBe('stopped');view.unmount();
  });
  it('returns to the original live instance if update intent is cancelled before any server updating event',async()=>{
    vi.useFakeTimers();const event=deferred(),nextWatch=deferred();let watches=0;
    const fetcher=vi.fn().mockImplementation((url:string)=>url.includes('watch=1')?(++watches===1?event.promise:nextWatch.promise):Promise.resolve(response(ready)));
    vi.stubGlobal('fetch',fetcher);const reconnect=vi.fn();const view=renderHook(()=>useBackendLifecycle(true,reconnect));
    await act(async()=>{await vi.advanceTimersByTimeAsync(0);});
    act(()=>view.result.current.expectUpdate(true));
    await act(async()=>event.resolve(new Response('',{status:503})));
    expect(view.result.current.state).toBe('updating');expect(reconnect).not.toHaveBeenCalled();
    act(()=>view.result.current.expectUpdate(false));
    await act(async()=>{await vi.advanceTimersByTimeAsync(500);});
    expect(view.result.current.state).toBe('running');expect(reconnect).toHaveBeenCalledWith(window.location.origin);
    expect(reconnect).toHaveBeenCalledTimes(1);view.unmount();
  });
  it('does not let a late stopped event replace the active update flow',async()=>{
    const event=deferred(),nextWatch=deferred();let probes=0,watches=0;
    const fetcher=vi.fn().mockImplementation((url:string)=>{
      if(url.includes('watch=1'))return ++watches===1?event.promise:nextWatch.promise;
      return Promise.resolve(response(++probes===1?ready:{...ready,instance_id:'new-process',version:'1.1.0'}));
    });vi.stubGlobal('fetch',fetcher);const reconnect=vi.fn();const view=renderHook(()=>useBackendLifecycle(true,reconnect));
    await waitFor(()=>expect(watches).toBe(1));act(()=>view.result.current.expectUpdate(true));
    await act(async()=>event.resolve(response({...ready,state:'stopped'})));
    await waitFor(()=>expect(reconnect).toHaveBeenCalledWith(window.location.origin,{...ready,instance_id:'new-process',version:'1.1.0'},true));
    expect(view.result.current.state).toBe('running');view.unmount();
  });
  it('retries when the first response body times out and then observes stop',async()=>{
    vi.useFakeTimers();const event=deferred();
    const fetcher=vi.fn().mockImplementationOnce((_url:string,init:RequestInit)=>Promise.resolve({ok:true,json:()=>new Promise((_resolve,reject)=>{
      init.signal!.addEventListener('abort',()=>reject(new DOMException('body cancelled','AbortError')),{once:true});
    })} as Response)).mockResolvedValueOnce(response(ready)).mockReturnValue(event.promise);
    vi.stubGlobal('fetch',fetcher);const reconnect=vi.fn();const view=renderHook(()=>useBackendLifecycle(true,reconnect));
    await act(async()=>{await vi.advanceTimersByTimeAsync(2100);});
    expect(fetcher).toHaveBeenCalledTimes(3);
    await act(async()=>event.resolve(response({...ready,state:'stopped'})));
    expect(view.result.current.state).toBe('stopped');view.unmount();
  });
  it('retries a transient first probe and then observes backend stop',async()=>{
    const event=deferred();const fetcher=vi.fn().mockRejectedValueOnce(new TypeError('network unavailable')).mockResolvedValueOnce(response(ready)).mockReturnValue(event.promise);
    vi.stubGlobal('fetch',fetcher);const reconnect=vi.fn();const view=renderHook(()=>useBackendLifecycle(true,reconnect));
    await waitFor(()=>expect(fetcher).toHaveBeenCalledTimes(3),{timeout:1800});
    await act(async()=>event.resolve(response({...ready,state:'stopped'})));
    expect(view.result.current.state).toBe('stopped');view.unmount();
  });
  it('stops on an explicit matching event and cancels the long poll when unmounted',async()=>{
    const event=deferred();const fetcher=vi.fn().mockResolvedValueOnce(response(ready)).mockReturnValue(event.promise);vi.stubGlobal('fetch',fetcher);
    const reconnect=vi.fn();const view=renderHook(()=>useBackendLifecycle(true,reconnect));
    await waitFor(()=>expect(fetcher).toHaveBeenCalledTimes(2));
    await act(async()=>event.resolve(response({...ready,state:'stopped'})));
    expect(view.result.current.state).toBe('stopped');expect(reconnect).not.toHaveBeenCalled();
    view.unmount();
  });
  it('keeps a restart alive and follows only a verified successor at the target port',async()=>{
    const event=deferred(),nextWatch=deferred();let watches=0;
    const fetcher=vi.fn().mockImplementation((url:string)=>{
      if(url.includes('watch=1'))return ++watches===1?event.promise:nextWatch.promise;
      return Promise.resolve(response(url.includes(':31234')?{...ready,port:31234,instance_id:'process-B'}:ready));
    });vi.stubGlobal('fetch',fetcher);const reconnect=vi.fn();
    const view=renderHook(()=>useBackendLifecycle(true,reconnect));await waitFor(()=>expect(watches).toBe(1));
    await act(async()=>event.resolve(response({...ready,state:'restarting',target_port:31234})));
    await waitFor(()=>expect(reconnect).toHaveBeenCalledWith(new URL(window.location.origin).protocol+'//'+new URL(window.location.origin).hostname+':31234'));
    expect(view.result.current.state).toBe('running');expect(watches).toBe(2);
    const signal=fetcher.mock.calls.find(([url])=>url.includes(':31234')&&url.includes('watch'))![1].signal as AbortSignal;
    view.unmount();expect(signal.aborted).toBe(true);
  });
  it('does not accept another installation after a restart',async()=>{
    const event=deferred();const fetcher=vi.fn().mockImplementation((url:string)=>url.includes('watch')?event.promise:Promise.resolve(response(url.includes(':31234')?{...ready,installation_id:'install-B',port:31234}:ready)));
    vi.stubGlobal('fetch',fetcher);const reconnect=vi.fn();const view=renderHook(()=>useBackendLifecycle(true,reconnect));
    await waitFor(()=>expect(fetcher).toHaveBeenCalledTimes(2));await act(async()=>event.resolve(response({...ready,state:'restarting',target_port:31234})));
    expect(view.result.current.state).toBe('restarting');expect(reconnect).not.toHaveBeenCalled();view.unmount();
  });
});
