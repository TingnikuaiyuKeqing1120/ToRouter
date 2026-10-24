// @vitest-environment jsdom
import { cleanup, fireEvent, render as renderUI, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { NavigationGuard, type BeforeLeave } from './NavigationGuard';
import { ConfirmProvider } from './Confirm';
const render = (ui: Parameters<typeof renderUI>[0]) => renderUI(ui, { wrapper: ConfirmProvider });
import { ExchangeRatePanel } from './ExchangeRatePanel';
import type { ExchangeRate } from '../api';

const response = (body: unknown) => new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
const rate: ExchangeRate = { available: true, base: 'USD', quote: 'CNY', rate: 7.1, date: '2026-10-02', attempted_on: '2026-10-05', fetched_at: '2026-10-05T00:00:00Z', stale: false, error: '', source_url: 'https://www.exchangerate-api.com', source_id: 'exchangerate_api', source_name: 'ExchangeRate-API' };
const config = { sources: ['ecb', 'frankfurter', 'exchangerate_api'], available: [{ id: 'ecb', name: '欧洲央行', url: 'https://www.ecb.europa.eu/' }, { id: 'frankfurter', name: 'Frankfurter', url: 'https://frankfurter.dev/' }, { id: 'exchangerate_api', name: 'ExchangeRate-API', url: 'https://www.exchangerate-api.com' }] };
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});

it('saves source ordering before navigation and keeps the selection after a rejected save', async () => {
  let fail = true, leave: BeforeLeave | undefined, left = false;
  const fetcher = vi.fn((_url: string, options?: RequestInit) => Promise.resolve(options?.method === 'PUT'
    ? fail ? new Response(JSON.stringify({ error: 'source save rejected' }), { status: 409 }) : response({ ...config, sources: JSON.parse(String(options.body)).sources })
    : response(config)));
  vi.stubGlobal('fetch', fetcher);
  render(<NavigationGuard.Provider value={{ register: handler => { leave = handler; return () => {}; } }}><button onClick={() => void leave?.().then(() => { left = true; }).catch(() => {})}>导航</button><ExchangeRatePanel data={rate} loading={false} error="" reload={() => {}} /></NavigationGuard.Provider>);
  const details = screen.getByText('汇率来源').closest('details')!; details.open = true; fireEvent(details, new Event('toggle'));
  fireEvent.click(await screen.findByRole('button', { name: '上移 Frankfurter' }));
  fireEvent.click(screen.getByText('导航')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('source save rejected'); expect(left).toBe(false);
  expect(screen.getByRole('checkbox', { name: '1. Frankfurter' })).toHaveProperty('checked', true);
  fail = false; fireEvent.click(screen.getByText('导航')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(left).toBe(true));
  expect(JSON.parse(String(fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')[1][1]?.body))).toEqual({ sources: ['frankfurter', 'ecb', 'exchangerate_api'] });
});

it('distinguishes the source quote date from the latest query time', () => {
  render(<ExchangeRatePanel data={{ ...rate, checked_at: '2026-10-05T00:15:00Z' }} loading={false} error="" reload={() => {}} />);
  expect(screen.getByText(/报价 2026-10-02/)).toBeTruthy();
  expect(screen.getByText('查询 10/05 08:15')).toBeTruthy();
});

it('manual retry sends POST and updates value instead of reading failed daily cache', async () => {
  const reload = vi.fn();
  const fetcher = vi.fn().mockResolvedValue(response(rate)); vi.stubGlobal('fetch', fetcher);
  render(<ExchangeRatePanel data={{ ...rate, available: false, error: '查询失败' }} loading={false} error="" reload={reload} />);
  fireEvent.click(screen.getByRole('button', { name: '刷新汇率' }));
  await waitFor(() => expect(screen.getByText('1 USD ≈ 7.1 CNY')).toBeTruthy());
  expect(fetcher.mock.calls[0][0]).toBe('/api/exchange-rate'); expect(fetcher.mock.calls[0][1].method).toBe('POST');
  expect(reload).toHaveBeenCalledOnce();
  expect(screen.queryByText('查询失败')).toBeNull();
  expect(screen.getByRole('link', { name: 'Rates By Exchange Rate API' }).getAttribute('href')).toBe('https://www.exchangerate-api.com');
});

it('retains the old rate on an actual manual request failure', async () => {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('连接失败')));
  render(<ExchangeRatePanel data={rate} loading={false} error="" reload={() => {}} />);
  fireEvent.click(screen.getByRole('button', { name: '刷新汇率' }));
  await waitFor(() => expect(screen.getByRole('status').textContent).toContain('连接失败'));
  expect(screen.getByText('1 USD ≈ 7.1 CNY（旧值）')).toBeTruthy();
});

it('loads folded sources on demand and saves enabled order', async () => {
  const fetcher = vi.fn().mockImplementation((_url, options) => Promise.resolve(response(options.method === 'PUT' ? { ...config, sources: JSON.parse(options.body).sources } : config)));
  vi.stubGlobal('fetch', fetcher);
  render(<ExchangeRatePanel data={rate} loading={false} error="" reload={() => {}} />);
  expect(fetcher).not.toHaveBeenCalled();
  const details = screen.getByText('汇率来源').closest('details')!;
  details.open = true; fireEvent(details, new Event('toggle'));
  await waitFor(() => expect(screen.getAllByRole('checkbox')).toHaveLength(3));
  fireEvent.click(screen.getByRole('button', { name: '上移 Frankfurter' }));
  fireEvent.click(screen.getByRole('checkbox', { name: /ExchangeRate-API/ }));
  fireEvent.click(screen.getByRole('button', { name: '保存来源' }));
  await waitFor(() => expect(screen.getByText('已保存')).toBeTruthy());
  const put = fetcher.mock.calls.find(([, options]) => options.method === 'PUT')!;
  expect(put[0]).toBe('/api/exchange-rate/sources'); expect(JSON.parse(put[1].body)).toEqual({ sources: ['frankfurter', 'ecb'] });
});
