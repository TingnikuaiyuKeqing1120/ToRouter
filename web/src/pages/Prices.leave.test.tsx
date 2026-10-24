// @vitest-environment jsdom
import { useMemo, useRef, useState } from 'react';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import { NavigationGuard, type BeforeLeave } from '../components/NavigationGuard';
import { blankPrice } from '../hooks/usePriceWorkbench';
import Prices from './Prices';

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
function Harness() {
  const handlers = useRef<BeforeLeave[]>([]), [left, setLeft] = useState(false);
  const guard = useMemo(() => ({ register: (handler: BeforeLeave) => { handlers.current.push(handler); return () => { handlers.current = handlers.current.filter(item => item !== handler); }; } }), []);
  return <NavigationGuard.Provider value={guard}><button onClick={async () => { try { for (const handler of [...handlers.current].reverse()) await handler(); setLeft(true); } catch {} }}>离开价格页</button>{left ? <p>导航完成</p> : <Prices />}</NavigationGuard.Provider>;
}
function setup() {
  let reject = false;
  const { key: _key, ...initial } = blankPrice(0, 'CNY', 1);
  if (initial.selection.mode === 'manual') initial.selection.table.rates.input = 1;
  let prices = [{ ...initial, model: 'public-a' }];
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/prices') {
      if (init?.method === 'PUT') {
        if (reject) return Promise.resolve(json({ error: 'save rejected' }, 409));
        prices = JSON.parse(String(init.body));
      }
      return Promise.resolve(json(prices));
    }
    if (url === '/api/models') return Promise.resolve(json([{ id: 'public-a', enabled: true }]));
    if (url === '/api/exchange-rate') return Promise.resolve(json({ available: true, rate: 7, base: 'USD', quote: 'CNY' }));
    return Promise.resolve(json([]));
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ConfirmProvider><Harness /></ConfirmProvider>);
  return { fetcher, reject: (value: boolean) => { reject = value; }, prices: () => prices };
}
beforeEach(() => {
  localStorage.clear(); history.replaceState(null, '', '/#/prices');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it.each(['close', 'discard', 'save'] as const)('pauses timed writes while exit is undecided, then handles %s without an accidental save', async choice => {
  vi.useFakeTimers(); let view!: ReturnType<typeof setup>;
  await act(async () => { view = setup(); });
  fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '2' } });
  fireEvent.click(screen.getByText('离开价格页'));
  const confirmation = screen.getByRole('dialog', { name: '保存更改？' });
  await act(async () => { await vi.advanceTimersByTimeAsync(60000); });
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
  expect(view.prices()[0].selection).toMatchObject({ table: { rates: { input: 1 } } });
  await act(async () => { fireEvent.click(within(confirmation).getByRole('button', { name: choice === 'close' ? '关闭弹窗' : choice === 'discard' ? '放弃更改' : '确认保存' })); });
  if (choice === 'close') {
    expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '2');
    expect(screen.queryByText('导航完成')).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(29999); });
    expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
  } else {
    expect(screen.getByText('导航完成')).toBeTruthy();
    await act(async () => { await vi.advanceTimersByTimeAsync(30000); });
  }
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(choice === 'discard' ? 0 : 1);
  expect(view.prices()[0].selection).toMatchObject({ table: { rates: { input: choice === 'discard' ? 1 : 2 } } });
});

it('pauses the parent timer through both nested advanced and parent exit decisions', async () => {
  vi.useFakeTimers(); let view!: ReturnType<typeof setup>;
  await act(async () => { view = setup(); });
  fireEvent.change(screen.getByLabelText('输入 0:public-a'), { target: { value: '2' } });
  fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
  fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '3' } });
  fireEvent.click(screen.getByText('离开价格页'));
  await act(async () => { await vi.advanceTimersByTimeAsync(30000); });
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: '放弃更改' })); });
  // The inner draft was discarded, but the main table's value 2 is still pending.
  expect(screen.queryByRole('dialog', { name: '高级价格 · public-a' })).toBeNull();
  expect(screen.getByRole('dialog', { name: '保存更改？' })).toBeTruthy();
  expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '2');
  await act(async () => { await vi.advanceTimersByTimeAsync(30000); });
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: '放弃更改' })); });
  expect(screen.getByText('导航完成')).toBeTruthy();
  expect(view.prices()[0].selection).toMatchObject({ table: { rates: { input: 1 } } });
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
});

it('keeps rejected advanced saves outside the automatic queue, and explicit discard does not save them later', async () => {
  const view = setup(); view.reject(true); await screen.findByLabelText('输入 0:public-a');
  fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
  fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '2' } });
  fireEvent.click(screen.getByText('离开价格页')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await within(screen.getByRole('dialog', { name: '高级价格 · public-a' })).findByText('save rejected');
  expect(screen.getByLabelText('基础 输入')).toHaveProperty('value', '2'); expect(screen.getByLabelText('输入 0:public-a')).toHaveProperty('value', '1');
  expect(screen.queryByText('导航完成')).toBeNull(); expect(localStorage.getItem('to-router.price-advanced.drafts.v2')).toContain('"input":2');
  fireEvent.click(screen.getByText('离开价格页')); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
  await screen.findByText('导航完成');
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
  expect(localStorage.getItem('to-router.price-advanced.drafts.v2')).toBeNull();
});

it('awaits an advanced exit save and leaves without prompting again for the now-saved parent row', async () => {
  const view = setup(); const base = view.fetcher.getMockImplementation()!; let finish!: (response: Response) => void;
  view.fetcher.mockImplementation((url, init) => init?.method === 'PUT' ? new Promise<Response>(resolve => { finish = resolve; }) : base(url, init));
  await screen.findByLabelText('输入 0:public-a'); fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' }));
  fireEvent.change(screen.getByLabelText('基础 输入'), { target: { value: '2' } });
  fireEvent.click(screen.getByText('离开价格页')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(view.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true));
  expect(screen.queryByText('导航完成')).toBeNull();
  const body = view.fetcher.mock.calls.find(([, init]) => init?.method === 'PUT')![1]!.body;
  await act(async () => finish(json(JSON.parse(String(body)))));
  await screen.findByText('导航完成'); expect(screen.queryByRole('dialog')).toBeNull();
});

it('restores parent protection after closing an unchanged advanced dialog and rejects an incomplete new row', async () => {
  const view = setup(); await screen.findByLabelText('输入 0:public-a');
  fireEvent.click(screen.getByRole('button', { name: '高级 0:public-a' })); fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  fireEvent.click(screen.getByRole('button', { name: '手工添加价格' }));
  fireEvent.click(screen.getByText('离开价格页')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('请填写模型 ID，或删除空白行后再保存。');
  expect(screen.queryByText('导航完成')).toBeNull(); expect(view.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
});
