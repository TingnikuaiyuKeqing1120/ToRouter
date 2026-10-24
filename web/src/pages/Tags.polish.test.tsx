// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import { NavigationGuard, type BeforeLeave } from '../components/NavigationGuard';
import Tags from './Tags';
import type { TagGroupWithTags } from '../types';

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const groups: TagGroupWithTags[] = [
  { id: 'work', name: '工作分类', description: '按工作用途分类', type: 'user', sort_order: 23, tags: [
    { id: 'work:a', group_id: 'work', name: 'Alpha', color: 'sky', description: '第一项', sort_order: 12 },
    { id: 'work:b', group_id: 'work', name: 'Beta', color: 'rose', description: '第二项', sort_order: 22 },
  ] },
  { id: 'empty-id', name: '空标签组', description: '', type: 'user', sort_order: 49, tags: [] },
];
let fetcher: ReturnType<typeof vi.fn>;
let leave: BeforeLeave;
function mount() { return render(<ConfirmProvider><NavigationGuard.Provider value={{ register: handler => { leave = handler; return () => {}; } }}><Tags /></NavigationGuard.Provider></ConfirmProvider>); }
const writes = () => fetcher.mock.calls.filter(([, init]) => ['PUT','POST','DELETE'].includes(init?.method || ''));
const groupIDs = () => Array.from(document.querySelector('.tags-groups')!.children).map(node => node.getAttribute('data-sortable-item'));
const tagIDs = () => Array.from(document.querySelector('.tags-rows')!.children).map(node => node.getAttribute('data-sortable-item'));
function drag(label: string, targetID: string) {
  const handle = screen.getByRole('button', { name: label });
  const source = handle.closest<HTMLElement>('[data-sortable-item]')!, list = source.parentElement!;
  const items = Array.from(list.children) as HTMLElement[];
  const rect = (index: number, count = 1) => ({ x:0,y:index*100,left:0,top:index*100,right:400,bottom:(index+count)*100,width:400,height:count*100,toJSON() {} }) as DOMRect;
  vi.spyOn(list,'getBoundingClientRect').mockImplementation(() => rect(0,items.length));
  items.forEach((item,index) => vi.spyOn(item,'getBoundingClientRect').mockImplementation(() => rect(index)));
  const target = items.find(item => item.dataset.sortableItem === targetID)!;
  const start = { pointerId:1, button:0, clientX:40, clientY:source.getBoundingClientRect().top+50 };
  const end = { ...start, clientY:target.getBoundingClientRect().top+75 };
  fireEvent.pointerDown(handle,start); fireEvent.pointerMove(window,end);
  return () => fireEvent.pointerUp(window,end);
}
beforeEach(() => {
  fetcher=vi.fn((url: string, init?: RequestInit) => Promise.resolve(json(url === '/api/tag-groups' && !init?.method ? groups : url.startsWith('/api/tags/references') ? { tag_id: new URL('http://test'+url).searchParams.get('id'), channel_names:[], downstream_models:[],client_key_names:[] } : { saved:true })));
  vi.stubGlobal('fetch',fetcher); vi.stubGlobal('alert',vi.fn());
  HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');}; HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
  vi.stubGlobal('PointerEvent',class extends MouseEvent { pointerId: number; isPrimary=true; constructor(type: string,init: PointerEventInit={}) { super(type,init);this.pointerId=init.pointerId??1; } });
  vi.stubGlobal('requestAnimationFrame',vi.fn(() => 1)); vi.stubGlobal('cancelAnimationFrame',vi.fn());
});
afterEach(() => { cleanup(); vi.restoreAllMocks();vi.unstubAllGlobals(); });

describe('Tags polish and safety', () => {
  it('protects the unknown option in every custom group and allows deleting a group with only unknown', async () => {
    const unknown = { id: 'work:unknown', group_id: 'work', name: '未知', color: 'slate', description: '', sort_order: 99 };
    const base = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => url === '/api/tag-groups' ? Promise.resolve(json([{ ...groups[0], tags: [unknown] }])) : base(url, init));
    mount(); await screen.findByText('work:unknown');
    expect(screen.queryByRole('button', { name: '删除 未知' })).toBeNull();
    const removeGroup = screen.getByRole('button', { name: '删除标签组 工作分类' });
    expect(removeGroup).toHaveProperty('disabled', false);
    fireEvent.click(removeGroup);
    fireEvent.click(await screen.findByRole('button', { name: '删除标签组' }));
    await waitFor(() => expect(writes().some(([url]) => url === '/api/tag-groups/work')).toBe(true));
  });
  it('shows client Key references and blocks forced deletion until access rules are handled', async () => {
    const base = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => url.startsWith('/api/tags/references') ? Promise.resolve(json({ tag_id: 'work:a', channel_names: ['渠道'], downstream_models: [], client_key_names: ['受限应用'] })) : base(url, init));
    mount(); fireEvent.click(await screen.findByRole('button', { name: '删除 Alpha' }));
    await screen.findByText('客户端 Key：受限应用');
    const remove = screen.getByRole('button', { name: '请先处理 Key 引用' });
    expect(remove).toHaveProperty('disabled', true); fireEvent.click(remove);
    expect(writes()).toHaveLength(0);
    expect(screen.queryByRole('button', { name: '确认删除并解绑' })).toBeNull();
  });
  it('blocks deletion on failed references and retries before allowing a non-force delete', async () => {
    const base=fetcher.getMockImplementation()!; let failed=true;
    fetcher.mockImplementation((url,init) => url.startsWith('/api/tags/references') && failed ? Promise.resolve(json({error:'unavailable'},500)) : base(url,init));
    mount();fireEvent.click(await screen.findByRole('button',{name:'删除 Alpha'}));
    const dialog=await screen.findByRole('dialog',{name:'确认删除标签？'});
    await within(dialog).findByText(/引用检查失败/);
    expect((within(dialog).getByRole('button',{name:'确认删除'}) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(within(dialog).getByRole('button',{name:'确认删除'}));expect(writes()).toHaveLength(0);
    failed=false;fireEvent.click(within(dialog).getByRole('button',{name:'重试'}));
    await within(dialog).findByText('该标签未被任何渠道或模型使用，可安全删除。');
    fireEvent.click(within(dialog).getByRole('button',{name:'确认删除'}));
    await waitFor(() => expect(writes()[0][0]).toBe('/api/tags/work%3Aa?force=false'));expect(alert).not.toHaveBeenCalled();
  });
  it('isolates a late reference success after closing and while checking a different tag', async () => {
    const base=fetcher.getMockImplementation()!; let resolve!: (value: Response) => void;
    fetcher.mockImplementation((url,init) => url.includes('references?id=work%3Aa') ? new Promise<Response>(done => { resolve=done; }) : base(url,init));
    mount();fireEvent.click(await screen.findByRole('button',{name:'删除 Alpha'}));
    fireEvent.click(within(screen.getByRole('dialog',{name:'确认删除标签？'})).getByRole('button',{name:'取消'}));
    await act(async () => { resolve(json({ tag_id:'work:a',channel_names:['Late'],downstream_models:[],client_key_names:[] })); });
    expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.click(screen.getByRole('button',{name:'删除 Alpha'}));
    fireEvent.click(screen.getByRole('button',{name:'删除 Beta'}));
    await screen.findByText('该标签未被任何渠道或模型使用，可安全删除。');
    await act(async () => { resolve(json({ tag_id:'work:a',channel_names:['Late'],downstream_models:[],client_key_names:[] })); });
    expect(screen.queryByText('Late')).toBeNull();
    fireEvent.click(within(screen.getByRole('dialog',{name:'确认删除标签？'})).getByRole('button',{name:'确认删除'}));
    await waitFor(() => expect(writes()[0][0]).toBe('/api/tags/work%3Ab?force=false'));
  });
  it('isolates a late reference failure after closing', async () => {
    const base=fetcher.getMockImplementation()!;let reject!: (reason: Error) => void;
    fetcher.mockImplementation((url,init) => url.includes('references') ? new Promise<Response>((_,no) => {reject=no;}) : base(url,init));
    mount();fireEvent.click(await screen.findByRole('button',{name:'删除 Alpha'}));fireEvent.click(within(screen.getByRole('dialog')).getByRole('button',{name:'取消'}));
    await act(async () => {reject(new Error('late'));});expect(screen.queryByRole('dialog')).toBeNull();expect(screen.queryByRole('alert')).toBeNull();
  });
  it('requires renewed preview after a deletion conflict instead of escalating force', async () => {
    const base=fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url,init) => init?.method==='DELETE' ? Promise.resolve(json({error:'标签正在被使用'},409)) : base(url,init));
    mount();fireEvent.click(await screen.findByRole('button',{name:'删除 Alpha'}));await screen.findByText('该标签未被任何渠道或模型使用，可安全删除。');
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button',{name:'确认删除'}));await screen.findByText(/标签引用已变化/);
    expect((within(screen.getByRole('dialog')).getByRole('button',{name:'确认删除'}) as HTMLButtonElement).disabled).toBe(true);expect(writes()[0][0]).toContain('force=false');
  });
  it('preserves group order and the draft after rejected save, then saves on global leave', async () => {
    const base=fetcher.getMockImplementation()!;let reject=true;
    fetcher.mockImplementation((url,init) => init?.method==='PUT' && reject ? Promise.resolve(json({error:'rejected'},400)) : base(url,init));
    mount();fireEvent.click(await screen.findByRole('button',{name:'编辑标签组 工作分类'}));
    fireEvent.change(screen.getByLabelText('显示名称'),{target:{value:'Renamed'}});fireEvent.click(screen.getByRole('button',{name:'保存'}));
    await screen.findByText('保存失败: rejected');expect(screen.getByLabelText('显示名称')).toHaveProperty('value','Renamed');
    expect(JSON.parse(writes()[0][1].body)).toMatchObject({id:'work',name:'Renamed',sort_order:23});expect(alert).not.toHaveBeenCalled();
    reject=false; let leaving!: Promise<void>; act(() => {leaving=leave();});fireEvent.click(await screen.findByRole('button',{name:'确认保存'}));
    await act(async () => {await leaving;});expect(screen.queryByRole('dialog')).toBeNull();expect(JSON.parse(writes()[1][1].body).sort_order).toBe(23);
  });
  it('trims search, finds an empty group by ID, reports counts, and clears the empty result', async () => {
    mount();await screen.findByText('Alpha');const input=screen.getByRole('textbox',{name:'搜索标签和标签组'});
    fireEvent.change(input,{target:{value:'  empty-id  '}});expect(screen.getByText('空标签组')).toBeTruthy();expect(screen.getByText('1 个标签组 · 0 个标签匹配')).toBeTruthy();expect(screen.getByText('清除搜索后可调整顺序')).toBeTruthy();
    expect((screen.getByRole('button',{name:'调整标签组 空标签组 顺序'}) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(input,{target:{value:'missing'}});expect(screen.getByText('没有匹配的标签或标签组')).toBeTruthy();
    fireEvent.click(within(document.querySelector('.tags-empty') as HTMLElement).getByRole('button',{name:'清除搜索'}));expect(screen.getByText('Alpha')).toBeTruthy();
  });
  it('keeps folding independent of edit and handle actions', async () => {
    mount();await screen.findByText('Alpha');const toggle=screen.getByRole('button',{name:'工作分类 2'});fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');fireEvent.click(screen.getByRole('button',{name:'编辑标签组 工作分类'}));
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button',{name:'取消'}));expect(toggle.getAttribute('aria-expanded')).toBe('false');fireEvent.click(toggle);expect(screen.getByRole('button',{name:'编辑 Alpha'})).toBeTruthy();
  });
  it('saves complete group and group-local tag order with keyboard alternatives', async () => {
    mount();await screen.findByText('Alpha');fireEvent.keyDown(screen.getByRole('button',{name:'调整标签组 工作分类 顺序'}),{key:'ArrowDown'});
    await screen.findByText('顺序已保存');expect(groupIDs()).toEqual(['empty-id','work']);expect(JSON.parse(writes()[0][1].body)).toEqual({order:['empty-id','work']});
    fireEvent.keyDown(screen.getByRole('button',{name:'调整标签 Alpha 顺序'}),{key:'ArrowDown'});
    await waitFor(() => expect(tagIDs()).toEqual(['work:b','work:a']));expect(JSON.parse(writes()[1][1].body)).toEqual({group_id:'work',order:['work:b','work:a']});
  });
  it('keeps original order after a rejected keyboard save, then allows retry', async () => {
    const base=fetcher.getMockImplementation()!;let failed=true;fetcher.mockImplementation((url,init) => init?.method==='PUT' && failed ? Promise.resolve(json({error:'write rejected'},400)) : base(url,init));
    mount();await screen.findByText('Alpha');fireEvent.keyDown(screen.getByRole('button',{name:'调整标签 Alpha 顺序'}),{key:'ArrowDown'});
    await screen.findByText(/顺序保存失败，已保留原顺序/);expect(tagIDs()).toEqual(['work:a','work:b']);failed=false;
    fireEvent.keyDown(screen.getByRole('button',{name:'调整标签 Alpha 顺序'}),{key:'ArrowDown'});await waitFor(() => expect(tagIDs()).toEqual(['work:b','work:a']));
  });
  it('previews and saves a tag drag once, and cancels group drag without writing', async () => {
    mount();await screen.findByText('Alpha');const drop=drag('调整标签 Alpha 顺序','work:b');
    expect(document.querySelector('[data-sortable-state="placeholder"]')).toBeTruthy();expect(writes()).toHaveLength(0);drop();
    await waitFor(() => expect(tagIDs()).toEqual(['work:b','work:a']));expect(writes()).toHaveLength(1);
    drag('调整标签组 工作分类 顺序','empty-id');fireEvent.keyDown(window,{key:'Escape'});expect(document.querySelector('.sortable-floating')).toBeNull();expect(writes()).toHaveLength(1);expect(groupIDs()).toEqual(['work','empty-id']);
  });
  it('restores the drag preview after save failure and forbids dropping into another group', async () => {
    const base=fetcher.getMockImplementation()!;fetcher.mockImplementation((url,init) => init?.method==='PUT' ? Promise.resolve(json({error:'failed'},500)) : base(url,init));
    mount();await screen.findByText('Alpha');drag('调整标签 Alpha 顺序','work:b')();await screen.findByText(/顺序保存失败/);expect(tagIDs()).toEqual(['work:a','work:b']);expect(document.querySelector('[data-sortable-state]')).toBeNull();
    drag('调整标签 Alpha 顺序','work:b');fireEvent.pointerUp(window,{pointerId:1,button:0,clientX:40,clientY:350});expect(writes()).toHaveLength(1);expect(tagIDs()).toEqual(['work:a','work:b']);
  });
  it('disables pointer and keyboard order writes during filtered search', async () => {
    mount();await screen.findByText('Alpha');fireEvent.change(screen.getByRole('textbox',{name:'搜索标签和标签组'}),{target:{value:'工作'}});
    fireEvent.keyDown(screen.getByRole('button',{name:'调整标签 Alpha 顺序'}),{key:'ArrowDown'});drag('调整标签 Alpha 顺序','work:b')();expect(writes()).toHaveLength(0);
  });
});
