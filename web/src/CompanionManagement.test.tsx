// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App from './App';

const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
function backend(initial = false) {
  let running = initial;
  let fail = false;
  let settleLaunch = true;
  const fetcher = vi.fn().mockImplementation((url: string) => {
    if (url === '/api/auth/session') return Promise.resolve(response({ authenticated: true }));
    if (url === '/api/companion/preferences') return Promise.resolve(response({theme:'dark',paused:false,revision:0}));
    if (url === '/api/quotas') return Promise.resolve(response({items:[],refresh_seconds:0}));
    if (url === '/api/desktop/pet') return Promise.resolve(response({running}));
    if (url === '/api/desktop/launch-pet' || url === '/api/desktop/close-pet') {
      if (fail) return Promise.resolve(response({error:'原生窗口不可用'},503));
      if (settleLaunch) running = url.endsWith('launch-pet');
      return Promise.resolve(response({requested:true}));
    }
    if (url === '/api/channels' || url === '/api/models') return Promise.resolve(response([]));
    return Promise.resolve(response({},404));
  });
  return {fetcher, setRunning:(value:boolean)=>{running=value;}, fail:()=>{fail=true;}, delay:()=>{settleLaunch=false;}};
}
beforeEach(()=>{
  history.replaceState(null,'','#/channels');
  vi.spyOn(document,'hidden','get').mockReturnValue(false);
});
afterEach(()=>{cleanup();vi.restoreAllMocks();vi.unstubAllGlobals();});

describe('desktop pet management',()=>{
  it('opens a management page without launching a pet and exposes no HUD entry',async()=>{
    const server=backend();vi.stubGlobal('fetch',server.fetcher);
    render(<App/>);
    fireEvent.click(await screen.findByRole('link',{name:'桌面小宠'}));
    await screen.findByRole('heading',{name:'桌宠管理'});
    expect(window.location.hash).toBe('#/companion');
    expect(screen.queryByText(/HUD/)).toBeNull();
    const toggle=await screen.findByRole('switch',{name:'开启桌宠'});
    await waitFor(()=>expect(toggle.hasAttribute('disabled')).toBe(false));
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    expect(server.fetcher.mock.calls.some(([url])=>url.endsWith('launch-pet'))).toBe(false);
    fireEvent.click(toggle);
    await waitFor(()=>expect(toggle.getAttribute('aria-checked')).toBe('true'));
    expect(window.location.hash).toBe('#/companion');
    const call=server.fetcher.mock.calls.find(([url])=>url==='/api/desktop/launch-pet')!;
    expect(new Headers(call[1].headers).get('X-To-Router')).toBe('1');
    expect(call[1].method).toBe('POST');
    fireEvent.click(toggle);
    await waitFor(()=>expect(toggle.getAttribute('aria-checked')).toBe('false'));
    expect(server.fetcher.mock.calls.filter(([url])=>url==='/api/desktop/close-pet')).toHaveLength(1);
  });
  it('reflects an already running pet and an external tray/window close',async()=>{
    history.replaceState(null,'','#/companion');
    const server=backend(true);vi.stubGlobal('fetch',server.fetcher);render(<App/>);
    const toggle=await screen.findByRole('switch',{name:'开启桌宠'});
    await waitFor(()=>expect(toggle.getAttribute('aria-checked')).toBe('true'));
    server.setRunning(false);
    await waitFor(()=>expect(toggle.getAttribute('aria-checked')).toBe('false'),{timeout:2500});
  });
  it('does not invent a ready window from an accepted launch and blocks repeated commands',async()=>{
    history.replaceState(null,'','#/companion');
    const server=backend();server.delay();vi.stubGlobal('fetch',server.fetcher);render(<App/>);
    const toggle=await screen.findByRole('switch',{name:'开启桌宠'});
    await waitFor(()=>expect(toggle.hasAttribute('disabled')).toBe(false));
    fireEvent.click(toggle);fireEvent.click(toggle);
    await waitFor(()=>expect(server.fetcher.mock.calls.filter(([url])=>url==='/api/desktop/launch-pet')).toHaveLength(1));
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    expect(toggle.hasAttribute('disabled')).toBe(true);
    server.setRunning(true);
    await waitFor(()=>{expect(toggle.getAttribute('aria-checked')).toBe('true');expect(toggle.hasAttribute('disabled')).toBe(false);},{timeout:2500});
  });
  it('keeps the actual state and explains a failed launch',async()=>{
    history.replaceState(null,'','#/companion');
    const server=backend();server.fail();vi.stubGlobal('fetch',server.fetcher);render(<App/>);
    const toggle=await screen.findByRole('switch',{name:'开启桌宠'});
    await waitFor(()=>expect(toggle.hasAttribute('disabled')).toBe(false));
    fireEvent.click(toggle);
    await screen.findByText('原生窗口不可用');
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    expect(toggle.hasAttribute('disabled')).toBe(false);
  });
  it('confirms an accepted action while hidden and leaves no false timeout after returning',async()=>{
    vi.useFakeTimers({toFake:['setTimeout','clearTimeout','Date']});
    try {
      history.replaceState(null,'','#/companion');let hidden=false;
      vi.spyOn(document,'hidden','get').mockImplementation(()=>hidden);
      const server=backend();server.delay();vi.stubGlobal('fetch',server.fetcher);render(<App/>);
      await act(async()=>{});
      const toggle=screen.getByRole('switch',{name:'开启桌宠'});
      fireEvent.click(toggle);await act(async()=>{});
      hidden=true;fireEvent(document,new Event('visibilitychange'));server.setRunning(true);
      await act(async()=>{await vi.advanceTimersByTimeAsync(13000);});
      hidden=false;fireEvent(document,new Event('visibilitychange'));await act(async()=>{});
      expect(toggle.getAttribute('aria-checked')).toBe('true');
      expect(toggle.hasAttribute('disabled')).toBe(false);
      expect(screen.queryByRole('alert')).toBeNull();
    } finally {vi.useRealTimers();}
  });
});
