// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App from './App';
import FloatingPet from './companion/FloatingPet';

const response = (body: unknown, status=200) => new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
const snapshot = {server_time:new Date().toISOString(),session_id:'shared',sequence:0,active_count:0,active:[],recent:[],today:{records:0,input_tokens:0,output_tokens:0,cache_read_tokens:0,cache_write_tokens:0,estimated_records:0}};
function backend(session=true,local=true) {
  let preferences={theme:'dark',paused:false,revision:0};
  let running=false;
  return vi.fn(async (url:string,options:RequestInit={}) => {
    if(url==='/api/auth/session') return response({authenticated:session},session?200:401);
    if(url==='/api/auth/local') return response({authenticated:local},local?200:403);
    if(url==='/api/companion/preferences') {
      if(options.method==='PUT') preferences={...preferences,...JSON.parse(String(options.body)),revision:preferences.revision+1};
      return response(preferences);
    }
    if(url==='/api/companion') return response(snapshot);
    if(url==='/api/desktop/pet') return response({running});
    if(url==='/api/desktop/launch-pet'||url==='/api/companion/open') {
      if(new Headers(options.headers).get('X-To-Router')!=='1') return response({},403);
      running=true;return response({requested:true});
    }
    if(url==='/api/channels'||url==='/api/models') return response([]);
    return response({},404);
  });
}
beforeEach(()=>{
  history.replaceState(null,'','#/channels'); localStorage.clear();
  vi.spyOn(document,'hidden','get').mockReturnValue(false);
  vi.stubGlobal('requestAnimationFrame',vi.fn(()=>1));vi.stubGlobal('cancelAnimationFrame',vi.fn());
});
afterEach(()=>{cleanup();vi.restoreAllMocks();vi.unstubAllGlobals();});

describe('companion cross-client regression',()=>{
  it('opens a native pet from management with the management header and keeps the console visible',async()=>{
    const fetcher=backend();vi.stubGlobal('fetch',fetcher);render(<App/>);
    fireEvent.click(await screen.findByRole('link',{name:'桌面小宠'}));
    const toggle=await screen.findByRole('switch',{name:'开启桌宠'},{timeout:4000});
    await waitFor(()=>expect(toggle.hasAttribute('disabled')).toBe(false));
    fireEvent.click(toggle);
    await waitFor(()=>expect(fetcher.mock.calls.some(([url])=>url==='/api/desktop/launch-pet')).toBe(true));
    await act(async()=>{});
    expect(window.location.hash).toBe('#/companion');
    const launch=fetcher.mock.calls.find(([url])=>url==='/api/desktop/launch-pet')!;
    expect(new Headers(launch[1]?.headers).get('X-To-Router')).toBe('1');
    expect(screen.queryByRole('button',{name:'摸摸蓝鲸和 Clawd'})).toBeNull();
  });
  it('automatically establishes a local session on a direct pet route',async()=>{
    history.replaceState(null,'','#/pet');const fetcher=backend(false,true);vi.stubGlobal('fetch',fetcher);render(<App/>);
    await screen.findByRole('button',{name:'摸摸蓝鲸和 Clawd'});
    expect(fetcher.mock.calls.some(([url])=>url==='/api/auth/local')).toBe(true);
    expect(screen.queryByLabelText('管理员密钥')).toBeNull();
  });
  it('provides a login fallback if local session restoration fails',async()=>{
    history.replaceState(null,'','#/pet');vi.stubGlobal('fetch',backend(false,false));render(<App/>);
    await screen.findByLabelText('管理员密钥');
    expect(screen.queryByRole('button',{name:'摸摸蓝鲸和 Clawd'})).toBeNull();
  });
  it('shares theme and pause between two mounted clients',async()=>{
    history.replaceState(null,'','#/pet');const fetcher=backend();vi.stubGlobal('fetch',fetcher);
    const first=render(<App/>), second=render(<App/>);
    await waitFor(()=>expect(first.container.querySelector('.companion-stage')).toBeTruthy());
    await waitFor(()=>expect(second.container.querySelector('.companion-stage')).toBeTruthy());
    fireEvent.click(within(first.container).getByRole('button',{name:'摸摸蓝鲸和 Clawd'}));
    fireEvent.click(within(first.container).getByRole('button',{name:'切换主题'}));
    fireEvent.click(within(first.container).getByRole('button',{name:'暂停动画'}));
    await waitFor(()=>{
      expect(second.container.querySelector('.companion-stage')?.getAttribute('data-theme')).toBe('light');
      expect(second.container.querySelector('.companion-stage')?.getAttribute('data-paused')).toBe('true');
    },{timeout:2500});
  });
  it('keeps the control dock usable when animation is paused and avoids browser resize calls',()=>{
    const fetcher=backend();vi.stubGlobal('fetch',fetcher);
    render(<FloatingPet initialPaused live={{snapshot,connection:'connected',notice:null,scene:'idle'}}/>);
    fireEvent.click(screen.getByRole('button',{name:'摸摸蓝鲸和 Clawd'}));
    expect(screen.getByLabelText('极简控制坞')).toBeTruthy();
    expect(fetcher.mock.calls.some(([url])=>url.includes('resize-pet'))).toBe(false);
  });
  it('does not undo a drawer open when mouse enter precedes the first click',()=>{
    vi.stubGlobal('fetch',backend());
    const active=Array.from({length:4},(_,i)=>({id:String(i),model:'model',channel:'channel',channel_id:1,started_at:new Date().toISOString(),attempt:1,phase:'streaming' as const,output_tokens:12,tokens_per_second:10,first_byte_ms:20}));
    render(<FloatingPet live={{snapshot:{...snapshot,active_count:4,active},connection:'connected',notice:null,scene:'surf'}}/>);
    const pill=screen.getByRole('button',{name:'聚合并发药丸'});fireEvent.mouseEnter(pill);fireEvent.click(pill);
    expect(screen.getByLabelText('并发通道详情抽屉')).toBeTruthy();
  });
});
