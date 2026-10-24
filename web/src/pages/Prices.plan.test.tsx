// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import { blankPrice } from '../hooks/usePriceWorkbench';
import { localMonth } from '../components/PlanPrices';
import Prices from './Prices';

beforeEach(() => { localStorage.clear(); history.replaceState(null, '', '/#/prices'); HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); }; HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); }; });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
function setup(options: { history?: boolean; newHistory?: boolean } = {}) {
  const channels = [{ id: 1, name: '套餐渠道', models: ['follow', 'payg', 'unknown'], enabled: true, endpoints: [], tags: ['billing:plan'], model_tags: { payg: ['billing:payg'], unknown: ['billing:unknown'] } }, { id: 2, name: '按量渠道', models: ['normal'], enabled: true, endpoints: [], tags: ['billing:payg'] }];
  let prices = [blankPrice(0, 'CNY', 1), ...['follow', 'payg', 'unknown'].map((model, index) => ({ ...blankPrice(1, 'CNY', index + 2), model }))].map(({ key: _key, ...row }) => ({ ...row, model: row.model || 'official' }));
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    let body: unknown = [];
    if (url === '/api/prices') { if (init?.method === 'PUT') prices = JSON.parse(String(init.body)); body = prices; }
    if (url === '/api/channels') body = channels;
    if (url === '/api/models') body = [{ id: 'official', enabled: true }];
    if (url === '/api/tag-groups') body = [{ id: 'billing', tags: ['plan', 'payg', 'free', 'unknown'].map(value => ({ id: `billing:${value}` })) }];
    if (url.startsWith('/api/plan-prices?')) { const month = new URL(url, 'http://test').searchParams.get('month'), another = options.newHistory && month === '2099-01'; body = { month, prices: [], scheduled: [], charges: options.history ? [{ channel_id: another ? 10 : 9, channel_name: another ? '另一历史套餐' : '已删除套餐', month, amount: 15, currency: 'USD', paid_at: '2026-01-01T00:00:00Z' }] : [] }; }
    if (url === '/api/exchange-rate') body = { available: true, rate: 7 };
    return Promise.resolve(new Response(JSON.stringify(body)));
  });
  vi.stubGlobal('fetch', fetcher); render(<ConfirmProvider><Prices /></ConfirmProvider>); return { fetcher, prices: () => prices };
}
it('shows independent payg and unknown models inside a plan channel, preserving hidden prices on save', async () => {
  const state = setup(); await screen.findByLabelText('输入 0:official'); fireEvent.click(screen.getByText('渠道价格（非套餐）'));
  await screen.findByLabelText('输入 1:payg'); expect(screen.queryByLabelText('输入 1:follow')).toBeNull(); expect(screen.getByLabelText('输入 1:unknown')).toBeTruthy();
  fireEvent.change(screen.getByLabelText('输入 1:payg'), { target: { value: '3' } }); fireEvent.click(screen.getByLabelText('保存价格'));
  await waitFor(() => expect(state.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true)); expect(state.prices().map(row => row.model)).toContain('follow');
});
it('offers deleted historical channels in the display picker without an edit link, and clearing hides them', async () => {
  history.replaceState(null, '', '/#/prices?view=plan'); setup({ history: true }); await screen.findByLabelText('本期金额 9');
  fireEvent.click(screen.getByRole('button', { name: /选择渠道 ·/ })); const historical = await screen.findByLabelText('选择渠道 已删除套餐'); expect(historical.getAttribute('aria-pressed')).toBe('true'); expect(screen.queryByLabelText('编辑渠道 已删除套餐')).toBeNull();
  fireEvent.click(screen.getByText('清除选择')); fireEvent.click(screen.getByText('完成选择')); expect(screen.queryByLabelText('本期金额 9')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: /选择渠道 ·/ })); fireEvent.click(screen.getByLabelText('选择渠道 已删除套餐')); fireEvent.click(screen.getByText('完成选择')); expect(screen.getByLabelText('本期金额 9')).toHaveProperty('value', '105');
});
it('does not add newly discovered history to an explicitly cleared display scope', async () => {
  history.replaceState(null, '', '/#/prices?view=plan'); setup({ history: true, newHistory: true }); await screen.findByLabelText('本期金额 9');
  fireEvent.click(screen.getByRole('button', { name: /选择渠道 ·/ })); fireEvent.click(screen.getByText('清除选择')); fireEvent.click(screen.getByText('完成选择'));
  fireEvent.change(screen.getByLabelText('账单月份'), { target: { value: '2099-01' } }); await screen.findByText('未选择展示渠道');
  fireEvent.click(screen.getByRole('button', { name: /选择渠道 ·/ })); expect((await screen.findByLabelText('选择渠道 另一历史套餐')).getAttribute('aria-pressed')).toBe('false'); expect(screen.getByLabelText('选择渠道 已删除套餐').getAttribute('aria-pressed')).toBe('false'); fireEvent.click(screen.getByText('完成选择')); expect(screen.queryByLabelText('本期金额 10')).toBeNull();
});
it('preserves each tab search and token draft while the plan page has no token unit', async () => {
  const state = setup(); await screen.findByLabelText('输入 0:official'); fireEvent.change(screen.getByLabelText('输入 0:official'), { target: { value: '6' } }); fireEvent.change(screen.getByLabelText('搜索价格'), { target: { value: 'official' } });
  fireEvent.click(screen.getByText('套餐价格')); await screen.findByLabelText('账单月份'); expect(screen.getByLabelText('页面币种')).toHaveProperty('value', 'CNY'); expect(screen.getByText('/ 每月')).toBeTruthy(); expect(screen.queryByText('/ 1M Tokens')).toBeNull(); expect(screen.queryByRole('dialog')).toBeNull();
  fireEvent.change(screen.getByLabelText('搜索价格'), { target: { value: '套餐' } }); fireEvent.click(screen.getByText('官方 API 价'));
  expect(screen.getByLabelText('搜索价格')).toHaveProperty('value', 'official'); expect(screen.getByLabelText('输入 0:official')).toHaveProperty('value', '6'); expect(state.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
  fireEvent.click(screen.getByText('套餐价格')); expect(screen.getByLabelText('搜索价格')).toHaveProperty('value', '套餐');
});
it('opens a plan deep link scoped to its channel', async () => {
  history.replaceState(null, '', '/#/prices?view=plan&channel_id=1'); setup(); expect(await screen.findByLabelText('账单月份')).toHaveProperty('value', localMonth()); await screen.findByLabelText('默认月费 1'); expect(screen.queryByLabelText('默认月费 2')).toBeNull(); expect(screen.getByText('套餐价格').getAttribute('aria-pressed')).toBe('true');
});
it('shares the display currency across tabs without mutating a historical bill', async () => {
  const state = setup({ history: true }); await screen.findByLabelText('输入 0:official');
  fireEvent.change(screen.getByLabelText('页面币种'), { target: { value: 'USD' } });
  fireEvent.click(screen.getByText('套餐价格')); await screen.findByLabelText('本期金额 9');
  expect(screen.getByLabelText('本期金额 9')).toHaveProperty('value', '15');
  fireEvent.change(screen.getByLabelText('页面币种'), { target: { value: 'CNY' } });
  expect(screen.getByLabelText('本期金额 9')).toHaveProperty('value', '105');
  expect(screen.getByRole('button', { name: '保存套餐价格' })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByText('官方 API 价'));
  expect(screen.getByLabelText('页面币种')).toHaveProperty('value', 'CNY');
  expect(state.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
});
it('does not apply a model pull that resolves after switching away and back', async () => {
  const state = setup(); await screen.findByLabelText('输入 0:official'); const normal = state.fetcher.getMockImplementation()!; let finish!: (response: Response) => void;
  state.fetcher.mockImplementation((url, init) => url === '/api/models' ? new Promise<Response>(resolve => { finish = resolve; }) : normal(url, init));
  fireEvent.click(screen.getByText('拉取当前模型')); await waitFor(() => expect(finish).toBeTypeOf('function'));
  fireEvent.click(screen.getByText('套餐价格')); fireEvent.click(screen.getByText('官方 API 价'));
  await act(async () => finish(new Response(JSON.stringify([{ id: 'late-model', enabled: true }]))));
  expect(screen.queryByLabelText('模型 ID 0:late-model')).toBeNull(); expect(screen.getByLabelText('模型 ID 0:official')).toBeTruthy();
});
