// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, renderHook, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import FloatingPet from './FloatingPet';
import { failureLabel, useCompanionLive, type CompanionSnapshot, type FinishedRequest, type LiveRequest } from './live';

const request = (overrides: Partial<LiveRequest> = {}): LiveRequest => ({
  id:'r1',model:'configured-model',channel:'personal-channel',channel_id:7,
  started_at:new Date().toISOString(),attempt:1,phase:'waiting',output_tokens:null,
  output_tokens_source:'',tokens_per_second:null,first_byte_ms:null,...overrides,
});
const finished = (overrides: Partial<FinishedRequest> = {}): FinishedRequest => ({
  ...request(),sequence:1,finished_at:new Date().toISOString(),outcome:'success',status:200,error_code:'',...overrides,
});
const snapshot = (overrides: Partial<CompanionSnapshot> = {}): CompanionSnapshot => ({
  server_time:new Date().toISOString(),session_id:'server-1',sequence:0,active_count:0,active:[],recent:[],
  today:{records:3,input_tokens:1234,output_tokens:56,cache_read_tokens:200,cache_write_tokens:50,estimated_records:1},...overrides,
});
const response = (body:unknown) => new Response(JSON.stringify(body),{status:200,headers:{'Content-Type':'application/json'}});
function deferred<T>() { let resolve!:(value:T)=>void;const promise=new Promise<T>(done=>{resolve=done;});return {promise,resolve}; }
async function settle() {await act(async()=>{});}
async function advance(ms=1000) {await act(async()=>{await vi.advanceTimersByTimeAsync(ms);});}
beforeEach(()=>{
  vi.useFakeTimers({toFake:['setTimeout','clearTimeout','Date']});vi.setSystemTime(new Date('2026-10-06T09:00:00Z'));
  vi.spyOn(document,'hidden','get').mockReturnValue(false);
  vi.stubGlobal('requestAnimationFrame',vi.fn(()=>1));vi.stubGlobal('cancelAnimationFrame',vi.fn());
});
afterEach(()=>{cleanup();vi.useRealTimers();vi.unstubAllGlobals();vi.restoreAllMocks();});

// The retired HUD's transport/state regressions now protect the shared live
// observer used by the floating pet, independently of the removed page.
describe('companion live observer',()=>{
  it('authenticates, serializes polling, and preserves unknown and reported metrics',async()=>{
    const pending=deferred<Response>();const data=snapshot({active_count:1,active:[request()]});
    const fetcher=vi.fn().mockReturnValueOnce(pending.promise).mockResolvedValue(response(data));vi.stubGlobal('fetch',fetcher);
    const {result}=renderHook(()=>useCompanionLive());
    expect(result.current.connection).toBe('loading');expect(result.current.scene).toBe('idle');expect(result.current.snapshot).toBeNull();
    await advance(5000);expect(fetcher).toHaveBeenCalledTimes(1);
    const [url,options]=fetcher.mock.calls[0];expect(url).toBe('/api/companion');expect(options.credentials).toBe('same-origin');expect(options.headers.get('X-To-Router')).toBe('1');
    pending.resolve(response(data));await settle();
    expect(result.current.scene).toBe('work');expect(result.current.snapshot?.active[0]).toMatchObject({output_tokens:null,tokens_per_second:null,first_byte_ms:null});
    expect(result.current.snapshot?.today.input_tokens).toBe(1234);expect(result.current.snapshot?.today.estimated_records).toBe(1);
    await advance();expect(fetcher).toHaveBeenCalledTimes(2);expect(result.current.snapshot?.active[0].output_tokens).toBeNull();
  });
  it('reports connection loss safely and refreshes from a real snapshot',async()=>{
    const fetcher=vi.fn().mockRejectedValueOnce(new Error('secret upstream body')).mockResolvedValue(response(snapshot({active_count:1,active:[request({tokens_per_second:8.5,first_byte_ms:120})]})));vi.stubGlobal('fetch',fetcher);
    const {result}=renderHook(()=>useCompanionLive());await settle();
    expect(result.current.connection).toBe('error');expect(result.current.scene).toBe('error');expect(JSON.stringify(result.current)).not.toContain('secret');
    act(()=>result.current.refresh());await settle();
    expect(result.current.connection).toBe('connected');expect(result.current.scene).toBe('work');expect(result.current.snapshot?.active[0].tokens_per_second).toBe(8.5);
  });
  it('times out and recovers without accepting a late hanging response',async()=>{
    const hanging=deferred<Response>();const fetcher=vi.fn().mockResolvedValueOnce(response(snapshot({active_count:1,active:[request({model:'previous'})]}))).mockReturnValueOnce(hanging.promise).mockResolvedValue(response(snapshot({active_count:1,active:[request()]})));vi.stubGlobal('fetch',fetcher);
    const {result}=renderHook(()=>useCompanionLive());await settle();await advance();
    const signal=fetcher.mock.calls[1][1].signal as AbortSignal;await advance(7999);expect(result.current.connection).toBe('connected');
    await advance(1);expect(signal.aborted).toBe(true);expect(result.current.scene).toBe('error');
    await advance();expect(fetcher).toHaveBeenCalledTimes(3);expect(result.current.scene).toBe('work');
    hanging.resolve(response(snapshot()));await settle();expect(result.current.scene).toBe('work');expect(result.current.snapshot?.active[0].model).toBe('configured-model');
  });
  it('uses the true active count and prioritizes retrying requests',async()=>{
    let data=snapshot({active_count:19,active:[request(),request({id:'r2'})]});vi.stubGlobal('fetch',vi.fn().mockImplementation(()=>Promise.resolve(response(data))));
    const {result}=renderHook(()=>useCompanionLive());await settle();expect(result.current.scene).toBe('surf');expect(result.current.snapshot?.active_count).toBe(19);expect(result.current.snapshot?.active).toHaveLength(2);
    data=snapshot({active_count:2,active:[request(),request({id:'r2',phase:'retrying',attempt:2})]});await advance();expect(result.current.scene).toBe('retry');
  });
  it('retains estimates then calibrated values and expires a final reported notice',async()=>{
    let data=snapshot({active_count:1,active:[request({phase:'streaming',output_tokens:15,output_tokens_source:'estimated',tokens_per_second:5.2,first_byte_ms:210})]});vi.stubGlobal('fetch',vi.fn().mockImplementation(()=>Promise.resolve(response(data))));
    const {result}=renderHook(()=>useCompanionLive());await settle();expect(result.current.snapshot?.active[0]).toMatchObject({output_tokens:15,output_tokens_source:'estimated',tokens_per_second:5.2,first_byte_ms:210});
    data=snapshot({active_count:1,active:[request({output_tokens:18,output_tokens_source:'reported',tokens_per_second:6})]});await advance();expect(result.current.snapshot?.active[0]).toMatchObject({output_tokens:18,output_tokens_source:'reported',tokens_per_second:6});
    data=snapshot({sequence:1,recent:[finished({output_tokens:24,output_tokens_source:'reported',tokens_per_second:8})]});await advance();expect(result.current.scene).toBe('done');expect(result.current.notice?.request).toMatchObject({output_tokens:24,output_tokens_source:'reported',tokens_per_second:8});
    await advance(2800);expect(result.current.notice).toBeNull();expect(result.current.scene).toBe('idle');
  });
  it('keeps unknown estimates null, expires errors, and skips cancellation notices',async()=>{
    let data=snapshot({active_count:1,active:[request({output_tokens_source:'estimated'})]});vi.stubGlobal('fetch',vi.fn().mockImplementation(()=>Promise.resolve(response(data))));
    const {result}=renderHook(()=>useCompanionLive());await settle();expect(result.current.snapshot?.active[0].output_tokens).toBeNull();expect(result.current.snapshot?.active[0].tokens_per_second).toBeNull();
    data=snapshot({sequence:1,recent:[finished({outcome:'error',status:502,error_code:'stream_interrupted',output_tokens:3,output_tokens_source:'estimated'})]});await advance();expect(result.current.scene).toBe('error');expect(result.current.notice?.request).toMatchObject({outcome:'error',output_tokens:3,output_tokens_source:'estimated'});
    await advance(5000);expect(result.current.notice).toBeNull();
    data=snapshot({sequence:2,recent:[finished({sequence:2,outcome:'cancelled',status:499})]});await advance();expect(result.current.notice).toBeNull();expect(result.current.scene).toBe('idle');
  });
  it('detects short completions once without replaying initial history',async()=>{
    let data=snapshot({sequence:1,recent:[finished()]});vi.stubGlobal('fetch',vi.fn().mockImplementation(()=>Promise.resolve(response(data))));
    const {result}=renderHook(()=>useCompanionLive());await settle();expect(result.current.scene).toBe('idle');
    data=snapshot({sequence:2,recent:[finished({sequence:2})]});await advance();expect(result.current.scene).toBe('done');
    await advance(2799);expect(result.current.scene).toBe('done');await advance(1);expect(result.current.scene).toBe('idle');
    await advance(3000);expect(result.current.scene).toBe('idle');
  });
  it('does not celebrate an accepted background response that is still running',async()=>{
    let data=snapshot();vi.stubGlobal('fetch',vi.fn().mockImplementation(()=>Promise.resolve(response(data))));
    const {result}=renderHook(()=>useCompanionLive());await settle();data=snapshot({sequence:1,recent:[finished({outcome:'accepted',status:200})]});await advance();expect(result.current.notice).toBeNull();expect(result.current.scene).toBe('idle');
    await advance(3000);expect(result.current.notice).toBeNull();
  });
  it('rebuilds its baseline on restart and skips cancelled or old completions',async()=>{
    let data=snapshot({sequence:4});vi.stubGlobal('fetch',vi.fn().mockImplementation(()=>Promise.resolve(response(data))));
    const {result}=renderHook(()=>useCompanionLive());await settle();data=snapshot({session_id:'server-2',sequence:1,recent:[finished()]});await advance();expect(result.current.scene).toBe('idle');
    data=snapshot({session_id:'server-2',sequence:2,recent:[finished({sequence:2,outcome:'cancelled',status:499})]});await advance();expect(result.current.scene).toBe('idle');
    data=snapshot({session_id:'server-2',sequence:3,recent:[finished({sequence:3,finished_at:'2026-10-06T08:00:00Z'})]});await advance();expect(result.current.scene).toBe('idle');
  });
  it('resumes from hidden state without accepting an obsolete poll',async()=>{
    let hidden=false;vi.spyOn(document,'hidden','get').mockImplementation(()=>hidden);
    const first=deferred<Response>();const fetcher=vi.fn().mockReturnValueOnce(first.promise).mockResolvedValue(response(snapshot({active_count:1,active:[request()]})));vi.stubGlobal('fetch',fetcher);
    const {result}=renderHook(()=>useCompanionLive());const signal=fetcher.mock.calls[0][1].signal as AbortSignal;
    await act(async()=>{hidden=true;document.dispatchEvent(new Event('visibilitychange'));});expect(signal.aborted).toBe(true);
    await advance(5000);expect(fetcher).toHaveBeenCalledTimes(1);
    await act(async()=>{hidden=false;document.dispatchEvent(new Event('visibilitychange'));});expect(result.current.scene).toBe('work');
    first.resolve(response(snapshot({session_id:'obsolete'})));await settle();expect(result.current.scene).toBe('work');
  });
  it('replaces a failed request notice with new work and then success',async()=>{
    let data=snapshot();vi.stubGlobal('fetch',vi.fn().mockImplementation(()=>Promise.resolve(response(data))));
    const {result}=renderHook(()=>useCompanionLive());await settle();
    data=snapshot({sequence:1,recent:[finished({outcome:'error',status:401,error_code:'request_rejected'})]});await advance();
    expect(result.current.scene).toBe('error');expect(failureLabel(result.current.notice!.request.error_code)).toBe('本次请求被拒绝');
    data=snapshot({sequence:1,active_count:1,active:[request()]});await advance();expect(result.current.scene).toBe('work');expect(result.current.notice).toBeNull();
    data=snapshot({sequence:2,recent:[finished({sequence:2})]});await advance();expect(result.current.scene).toBe('done');
  });
  it('uses safe summaries for unknown failure codes and expires the notice',async()=>{
    let data=snapshot();vi.stubGlobal('fetch',vi.fn().mockImplementation(()=>Promise.resolve(response(data))));
    const {result}=renderHook(()=>useCompanionLive());await settle();
    data=snapshot({sequence:1,recent:[finished({outcome:'error',error_code:'private diagnostic detail'})]});await advance();
    expect(failureLabel(result.current.notice!.request.error_code)).toBe('本次请求未完成');await advance(5000);expect(result.current.scene).toBe('idle');
  });
  it('aborts outstanding polling on unmount and schedules no further work',async()=>{
    const pending=deferred<Response>();const fetcher=vi.fn().mockReturnValue(pending.promise);vi.stubGlobal('fetch',fetcher);
    const view=renderHook(()=>useCompanionLive());const signal=fetcher.mock.calls[0][1].signal as AbortSignal;
    view.unmount();expect(signal.aborted).toBe(true);pending.resolve(response(snapshot()));await settle();await advance(5000);expect(fetcher).toHaveBeenCalledTimes(1);
  });
  it('pauses pet artwork while real polling and theme controls remain usable',async()=>{
    let data=snapshot({active_count:1,active:[request()]});
    const fetcher=vi.fn((url:string)=>{
      if(url==='/api/companion')return Promise.resolve(response(data));
      if(url==='/api/quotas')return Promise.resolve(response({items:[],refresh_seconds:0}));
      throw new Error(`Unexpected request: ${url}`);
    });vi.stubGlobal('fetch',fetcher);
    const polls=()=>fetcher.mock.calls.filter(([url])=>url==='/api/companion');
    const toggleTheme=vi.fn();render(<FloatingPet theme="dark" toggleTheme={toggleTheme}/>);await settle();
    expect(polls()).toHaveLength(1);
    fireEvent.click(screen.getByRole('button',{name:'打开控制坞'}));fireEvent.click(screen.getByRole('button',{name:'切换主题'}));expect(toggleTheme).toHaveBeenCalledOnce();
    fireEvent.click(screen.getByRole('button',{name:'暂停动画'}));expect(document.querySelector('.companion-stage')?.getAttribute('data-paused')).toBe('true');expect(screen.queryByText('暂停路由')).toBeNull();
    // Opening the dock reads quota metadata too; only /companion is live polling.
    await settle();expect(polls()).toHaveLength(1);expect(fetcher.mock.calls.filter(([url])=>url==='/api/quotas')).toHaveLength(1);
    data=snapshot({active_count:1,active:[request({output_tokens:87,tokens_per_second:8.7})]});await advance();expect(polls()).toHaveLength(2);
    fireEvent.click(screen.getByRole('button',{name:'关闭控制坞'}));expect(screen.getByText(/8\.7 tok\/s/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button',{name:'打开控制坞'}));fireEvent.click(screen.getByRole('button',{name:'恢复动画'}));expect(document.querySelector('.companion-stage')?.getAttribute('data-paused')).toBe('false');
  });
});
