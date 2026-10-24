// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Channel, Currency, ExchangeRate } from '../api';
import type { PlanPricesResponse } from '../types/planPricing';
import { ConfirmProvider } from './Confirm';
import { localMonth, PlanPrices } from './PlanPrices';

const month = localMonth(), config = { channel_id: 1, default_amount: 20, currency: 'USD' as const, first_payment_at: `${month}-01T12:00:00Z`, timezone: 'UTC' };
const channel = { id: 1, name: '套餐渠道', enabled: true, tags: ['billing:plan'], endpoints: [] } as unknown as Channel;
const result = (): PlanPricesResponse => ({ month, prices: [config], scheduled: [], charges: [{ channel_id: 1, channel_name: channel.name, month, amount: null, currency: 'CNY', paid_at: `${month}-01T12:00:00Z` }] });
beforeEach(() => { HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); }; HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); }; });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });
const exchangeRate = (rate: number): ExchangeRate => ({ available: true, base: 'USD', quote: 'CNY', rate, date: '2026-10-18', attempted_on: '', fetched_at: '', stale: false, error: '', source_url: '' });
function setup(data = result(), channels = [channel], selected = [1], initial: { currency?: Currency; rate?: ExchangeRate | null; rateLoading?: boolean } = { rate: exchangeRate(1) }) {
  const fetcher = vi.fn((url: string, init?: RequestInit) => Promise.resolve(new Response(JSON.stringify(init?.method ? { ok: true } : { ...data, month: new URL(url, 'http://test').searchParams.get('month') || month })))); vi.stubGlobal('fetch', fetcher);
  let display = initial;
  const element = () => <ConfirmProvider><PlanPrices channels={channels} selected={selected} search="" {...display} /></ConfirmProvider>;
  const view = render(element()); return Object.assign(fetcher, { update: (props: typeof initial) => { display = { ...display, ...props }; view.rerender(element()); } });
}
const writes = (fetcher: ReturnType<typeof setup>) => fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
it('keeps unknown distinct from zero, and changing only the default does not create an override', async () => {
  const fetcher = setup(); await screen.findByLabelText('默认月费 1'); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('value', ''); expect(screen.queryByLabelText('本期币种 1')).toBeNull(); expect(screen.queryByLabelText('默认币种 1')).toBeNull();
  fireEvent.change(screen.getByLabelText('默认月费 1'), { target: { value: '0' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); const [url, init] = writes(fetcher)[0]; expect(url).toBe('/api/plan-prices/1'); expect(JSON.parse(String(init?.body))).toMatchObject({ default_amount: 0, currency: 'USD' }); expect(JSON.parse(String(init?.body)).charge).toBeUndefined();
});
it('saves a period edit independently and combines default plus period edits in one transaction', async () => {
  const fetcher = setup(); await screen.findByLabelText('本期金额 1'); fireEvent.change(screen.getByLabelText('本期金额 1'), { target: { value: '0' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(writes(fetcher)[0][0]).toBe(`/api/plan-prices/1/charges/${month}`);
  await screen.findByLabelText('默认月费 1'); fireEvent.change(screen.getByLabelText('默认月费 1'), { target: { value: '30' } }); fireEvent.change(screen.getByLabelText('本期金额 1'), { target: { value: '25' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(2)); expect(JSON.parse(String(writes(fetcher)[1][1]?.body))).toMatchObject({ default_amount: 30, charge: { month, amount: 25, currency: 'CNY' } });
});
it('preserves both drafts after an atomic save fails', async () => {
  const fetcher = setup(), normal = fetcher.getMockImplementation()!; fetcher.mockImplementation((url, init) => init?.method ? Promise.resolve(new Response(JSON.stringify({ error: '写入失败' }), { status: 409 })) : normal(url, init));
  await screen.findByLabelText('默认月费 1'); fireEvent.change(screen.getByLabelText('默认月费 1'), { target: { value: '30' } }); fireEvent.change(screen.getByLabelText('本期金额 1'), { target: { value: '25' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await screen.findByLabelText('默认月费 1'); await waitFor(() => expect(screen.getByLabelText('默认月费 1')).toHaveProperty('value', '30')); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('value', '25'); expect(screen.getByText('有未保存的套餐修改')).toBeTruthy();
});
it('retains selected records for deleted or departed channels and marks unpaid future records', async () => {
  const data = result(); data.charges.push({ channel_id: 9, channel_name: '旧套餐', month, amount: 10, currency: 'USD', paid_at: '2099-01-01T00:00:00Z' }); setup(data, [channel], [1, 9]);
  await screen.findByText('旧套餐'); expect(screen.getByText('历史账单 · 渠道已删除')).toBeTruthy(); expect(screen.getByText('已记录 · 未到支付时间')).toBeTruthy(); expect(screen.getByLabelText('本期金额 9')).toHaveProperty('value', '10');
});
it('hides every historical row when the channel selection is empty', async () => {
  const data = result(); data.charges.push({ channel_id: 9, channel_name: '旧套餐', month, amount: 10, currency: 'USD', paid_at: '2099-01-01T00:00:00Z' });
  setup(data, [channel], []); await screen.findByText('套餐价格已保存'); expect(screen.queryByText('旧套餐')).toBeNull(); expect(screen.queryByLabelText('本期金额 9')).toBeNull(); expect(screen.queryByLabelText('本期金额 1')).toBeNull();
});
it('ignores a late previous-month response and retains edited periods while switching months', async () => {
  const fetcher = setup(); await screen.findByLabelText('本期金额 1'); fireEvent.change(screen.getByLabelText('本期金额 1'), { target: { value: '7' } });
  let finish!: (response: Response) => void;
  const normal = fetcher.getMockImplementation()!; fetcher.mockImplementation((url, init) => url.includes('2099-01') ? new Promise<Response>(resolve => { finish = resolve; }) : normal(url, init));
  fireEvent.change(screen.getByLabelText('账单月份'), { target: { value: '2099-01' } }); await waitFor(() => expect(finish).toBeTypeOf('function'));
  fireEvent.change(screen.getByLabelText('账单月份'), { target: { value: month } }); await screen.findByLabelText('本期金额 1'); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('value', '7');
  await act(async () => finish(new Response(JSON.stringify({ month: '2099-01', prices: [], charges: [], scheduled: [] })))); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('value', '7');
});
it('preserves the server scheduled instant when only the amount changes across the DST gap', async () => {
  vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-02-09T12:00:00Z'));
  const data: PlanPricesResponse = { month: '2026-03', prices: [{ ...config, first_payment_at: '2026-02-08T07:30:00Z', timezone: 'America/New_York' }], charges: [], scheduled: [{ channel_id: 1, channel_name: channel.name, month: '2026-03', amount: 20, currency: 'USD', paid_at: '2026-03-08T06:30:00Z' }] };
  const fetcher = setup(data); await screen.findByLabelText('默认月费 1'); fireEvent.change(screen.getByLabelText('账单月份'), { target: { value: '2026-03' } }); await screen.findByLabelText('本期金额 1');
  fireEvent.change(screen.getByLabelText('本期金额 1'), { target: { value: '25' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body)).paid_at).toBe('2026-03-08T06:30:00Z');
});
it('requires saving a new payment schedule before editing a projected period', async () => {
  const fetcher = setup({ month, prices: [], charges: [], scheduled: [] }); await screen.findByLabelText('默认月费 1');
  fireEvent.change(screen.getByLabelText('默认月费 1'), { target: { value: '15' } }); expect(screen.getByText('请先保存支付设置')).toBeTruthy(); expect(screen.queryByLabelText('本期金额 1')).toBeNull();
  fireEvent.click(screen.getByText('保存套餐价格')); await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body)).charge).toBeUndefined();
});
it('projects the page currency without writes or dirty state and preserves the exact saved amount after repeated switches', async () => {
  const precise = 10.123456789012345, data = result(); data.prices = [{ ...config, default_amount: precise }];
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: exchangeRate(7) });
  await screen.findByLabelText('默认月费 1'); expect(screen.getByLabelText('默认月费 1')).toHaveProperty('value', String(Number((precise * 7).toFixed(6))));
  for (let index = 0; index < 4; index++) { fetcher.update({ currency: 'USD' }); fetcher.update({ currency: 'CNY' }); }
  fetcher.update({ currency: 'USD' }); expect(screen.getByLabelText('默认月费 1')).toHaveProperty('value', String(precise));
  expect(screen.getByText('套餐价格已保存')).toBeTruthy(); expect(screen.getByText('保存套餐价格')).toHaveProperty('disabled', true); expect(writes(fetcher)).toHaveLength(0);
  expect(fetcher.mock.calls).toHaveLength(1);
});
it('converts an edited displayed value back to the configured original currency', async () => {
  const data = result(); data.prices = [{ ...config, default_amount: 10 }];
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: exchangeRate(7) });
  const input = await screen.findByLabelText('默认月费 1'); expect(input).toHaveProperty('value', '70');
  fireEvent.change(input, { target: { value: '77' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body))).toMatchObject({ default_amount: 11, currency: 'USD' });
});
it('starts a new config in the page currency and retains that draft currency through later switches', async () => {
  const fetcher = setup({ month, prices: [], charges: [], scheduled: [] }, [channel], [1], { currency: 'USD', rate: exchangeRate(7) });
  fireEvent.change(await screen.findByLabelText('默认月费 1'), { target: { value: '10' } });
  fetcher.update({ currency: 'CNY' }); expect(screen.getByLabelText('默认月费 1')).toHaveProperty('value', '70');
  fireEvent.change(screen.getByLabelText('默认月费 1'), { target: { value: '77' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body))).toMatchObject({ default_amount: 11, currency: 'USD' });
});
it('keeps a period override in its own original currency independently of the default', async () => {
  const data = result(); data.charges[0] = { ...data.charges[0], amount: 70 };
  const fetcher = setup(data, [channel], [1], { currency: 'USD', rate: exchangeRate(7) });
  const period = await screen.findByLabelText('本期金额 1'); expect(period).toHaveProperty('value', '10');
  fireEvent.change(screen.getByLabelText('默认月费 1'), { target: { value: '30' } });
  expect(period).toHaveProperty('value', '10'); fireEvent.change(period, { target: { value: '11' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body))).toMatchObject({ default_amount: 30, currency: 'USD', charge: { amount: 77, currency: 'CNY', month } });
});
it('shows original values with missing rates, blocks cross-currency edits, and permits editing after switching back', async () => {
  const fetcher = setup(result(), [channel], [1], { currency: 'CNY', rate: null });
  const input = await screen.findByLabelText('默认月费 1'); expect(input).toHaveProperty('value', '20'); expect(input).toHaveProperty('disabled', true);
  expect(screen.getByText('汇率不可用 · 原币 USD 20，切回 USD 编辑')).toBeTruthy();
  expect(screen.getByLabelText('本期金额 1')).toHaveProperty('value', ''); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('disabled', false);
  fetcher.update({ currency: 'USD' }); expect(input).toHaveProperty('disabled', false); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('disabled', true);
  expect(screen.getByText('汇率不可用 · 原币 CNY 未知，切回 CNY 编辑')).toBeTruthy();
  fireEvent.change(input, { target: { value: '0' } }); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body))).toMatchObject({ default_amount: 0, currency: 'USD' });
});
it('keeps unknown and explicit zero distinct under conversion and rejects malformed text without losing it', async () => {
  const data = result(); data.prices[0] = { ...config, default_amount: 0 };
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: exchangeRate(7) });
  const input = await screen.findByLabelText('默认月费 1'); expect(input).toHaveProperty('value', '0'); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('value', '');
  fireEvent.focus(input); fireEvent.change(input, { target: { value: '.' } }); fireEvent.blur(input); fireEvent.click(screen.getByText('保存套餐价格'));
  await screen.findByText('月费金额必须为空或非负数'); expect(writes(fetcher)).toHaveLength(0); expect(input).toHaveProperty('value', '.');
  fireEvent.focus(input); fireEvent.change(input, { target: { value: '' } }); fireEvent.blur(input); fireEvent.click(screen.getByText('保存套餐价格'));
  await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body))).toMatchObject({ default_amount: null, currency: 'USD' });
});
it('freezes the focused rate and raw decimal text until blur, then uses the latest rate for display', async () => {
  const data = result(); data.prices[0] = { ...config, default_amount: 10 };
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: exchangeRate(7) });
  const input = await screen.findByLabelText('默认月费 1'); fireEvent.focus(input); fireEvent.change(input, { target: { value: '77.' } });
  expect(input).toHaveProperty('value', '77.'); fetcher.update({ rate: exchangeRate(8) }); expect(input).toHaveProperty('value', '77.');
  fireEvent.change(input, { target: { value: '84' } }); fireEvent.blur(input); expect(input).toHaveProperty('value', '96');
  fireEvent.click(screen.getByText('保存套餐价格')); await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body))).toMatchObject({ default_amount: 12, currency: 'USD' });
});
it('keeps a focused editing rate when a background refresh loses the rate', async () => {
  const data = result(); data.prices[0] = { ...config, default_amount: 10 };
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: exchangeRate(7) });
  const input = await screen.findByLabelText('默认月费 1'); fireEvent.focus(input); fetcher.update({ rate: null });
  expect(input).toHaveProperty('value', '70'); expect(input).toHaveProperty('disabled', false);
  fireEvent.change(input, { target: { value: '77' } }); fireEvent.blur(input); expect(input).toHaveProperty('value', '11'); expect(input).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByText('保存套餐价格')); await waitFor(() => expect(writes(fetcher)).toHaveLength(1)); expect(JSON.parse(String(writes(fetcher)[0][1]?.body))).toMatchObject({ default_amount: 11, currency: 'USD' });
});
it('ends the previous editing snapshot when the page currency changes', async () => {
  const data = result(); data.prices[0] = { ...config, default_amount: 10 };
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: exchangeRate(7) });
  const input = await screen.findByLabelText('默认月费 1'); fireEvent.focus(input); fireEvent.change(input, { target: { value: '77.' } });
  fetcher.update({ currency: 'USD', rate: exchangeRate(8) }); expect(input).toHaveProperty('value', '11');
  fetcher.update({ currency: 'CNY' }); expect(input).toHaveProperty('value', '88'); expect(writes(fetcher)).toHaveLength(0);
});
it.each([0, -7, Infinity, NaN])('does not use an invalid exchange rate %s to relabel or edit original amounts', async rate => {
  setup(result(), [channel], [1], { currency: 'CNY', rate: exchangeRate(rate) });
  const input = await screen.findByLabelText('默认月费 1'); expect(input).toHaveProperty('value', '20'); expect(input).toHaveProperty('disabled', true);
  expect(screen.getByText('汇率不可用 · 原币 USD 20，切回 USD 编辑')).toBeTruthy();
});
it('shows original currency while the first rate is loading and reports unavailable only after loading ends', async () => {
  const data = result(); data.charges[0] = { ...data.charges[0], amount: 5, currency: 'USD' };
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: null, rateLoading: true });
  const input = await screen.findByLabelText('默认月费 1'); expect(input).toHaveProperty('value', '20'); expect(input).toHaveProperty('disabled', true);
  expect(screen.getByLabelText('本期金额 1')).toHaveProperty('value', '5'); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('disabled', true);
  expect(screen.getByText('原币 USD 20')).toBeTruthy(); expect(screen.getByText('原币 USD 5')).toBeTruthy(); expect(screen.queryByText(/汇率不可用/)).toBeNull();
  fetcher.update({ rateLoading: false }); expect(screen.getByText('汇率不可用 · 原币 USD 20，切回 USD 编辑')).toBeTruthy(); expect(input).toHaveProperty('disabled', true);
  fetcher.update({ rate: exchangeRate(7) }); expect(input).toHaveProperty('value', '140'); expect(input).toHaveProperty('disabled', false); expect(screen.getByLabelText('本期金额 1')).toHaveProperty('value', '35'); expect(screen.queryByText(/汇率不可用/)).toBeNull();
  expect(screen.getByText('套餐价格已保存')).toBeTruthy(); expect(writes(fetcher)).toHaveLength(0);
});
it('limits converted display decimals, exposes the full value on focus and title, and leaves focus-only interactions clean', async () => {
  const data = result(); data.prices[0] = { ...config, default_amount: 30 };
  const full = Number((30 * 6.686217008797667).toPrecision(14));
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: exchangeRate(6.686217008797667) });
  const input = await screen.findByLabelText('默认月费 1'); expect(input).toHaveProperty('value', '200.58651'); expect(input).toHaveProperty('title', `${full} CNY/月`);
  fireEvent.focus(input); expect(input).toHaveProperty('value', String(full)); fireEvent.blur(input); expect(input).toHaveProperty('value', '200.58651');
  fetcher.update({ currency: 'USD' }); expect(input).toHaveProperty('value', '30'); expect(screen.getByText('套餐价格已保存')).toBeTruthy(); expect(writes(fetcher)).toHaveLength(0);
});
it('retains significant digits for tiny nonzero converted amounts and keeps original precision intact', async () => {
  const original = 0.0000000123456789012345, data = result(); data.prices[0] = { ...config, default_amount: original };
  const fetcher = setup(data, [channel], [1], { currency: 'CNY', rate: exchangeRate(7) });
  const input = await screen.findByLabelText('默认月费 1'); expect(input).toHaveProperty('value', '8.64198e-8'); expect(Number((input as HTMLInputElement).value)).toBeGreaterThan(0);
  fireEvent.focus(input); expect(input).toHaveProperty('value', String(Number((original * 7).toPrecision(14)))); fireEvent.blur(input); expect(input).toHaveProperty('value', '8.64198e-8');
  fetcher.update({ currency: 'USD' }); expect(input).toHaveProperty('value', String(original)); expect(screen.getByText('套餐价格已保存')).toBeTruthy(); expect(writes(fetcher)).toHaveLength(0);
});
