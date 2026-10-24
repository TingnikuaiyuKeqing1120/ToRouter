// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useCompanionPreferences } from './preferences';
const response=(body:unknown,status=200)=>new Response(JSON.stringify(body),{status});
const value=(theme='dark',revision=0)=>({theme,paused:false,revision});
function View(){ const prefs=useCompanionPreferences(true); return <><output>{prefs.value.theme}</output><button onClick={()=>prefs.update({theme:'light'})}>light</button><p role="alert">{prefs.error}</p></>; }
beforeEach(()=>{vi.spyOn(document,'hidden','get').mockReturnValue(false);});
afterEach(()=>{cleanup();vi.unstubAllGlobals();vi.restoreAllMocks();});
describe('shared preference ordering',()=>{
  it('keeps an acknowledged write when an older read settles later',async()=>{
    let finish!:(r:Response)=>void;const pending=new Promise<Response>(done=>{finish=done;});
    vi.stubGlobal('fetch',vi.fn((_url:string,options:RequestInit={})=>options.method==='PUT'?Promise.resolve(response(value('light',1))):pending));
    render(<View/>);fireEvent.click(screen.getByRole('button',{name:'light'}));
    await waitFor(()=>expect(screen.getByRole('status').textContent).toBe('light'));
    await act(async()=>{await Promise.resolve();});
    await act(async()=>finish(response(value('dark',0))));
    expect(screen.getByRole('status').textContent).toBe('light');
  });
  it('rolls back a rejected write to the saved state and reports the failure',async()=>{
    vi.stubGlobal('fetch',vi.fn((_url:string,options:RequestInit={})=>Promise.resolve(options.method==='PUT'?response({error:'save failed'},503):response(value()))));
    render(<View/>);fireEvent.click(screen.getByRole('button',{name:'light'}));
    await waitFor(()=>expect(screen.getByRole('alert').textContent).toContain('save failed'));
    await waitFor(()=>expect(screen.getByRole('status').textContent).toBe('dark'));
  });
  it('restores the last confirmed preference when both save and recovery read fail',async()=>{
    let reads=0;
    vi.stubGlobal('fetch',vi.fn((_url:string,options:RequestInit={})=>Promise.resolve(options.method==='PUT'?response({error:'save failed'},503):++reads===1?response(value()):response({error:'read failed'},503))));
    render(<View/>);await act(async()=>{});
    fireEvent.click(screen.getByRole('button',{name:'light'}));
    await waitFor(()=>expect(screen.getByRole('alert').textContent).toContain('save failed'));
    await waitFor(()=>expect(screen.getByRole('status').textContent).toBe('dark'));
  });
});
