// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import Logs from './Logs';
import type { UsageLog } from '../api';

const response = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
const log: UsageLog = { id: 1, time: '2026-10-09T08:00:00Z', channel_id: 12, channel_name: 'Main', model: 'old-model', upstream_model: 'upstream', inbound: 'openai_chat_completions', outbound: 'openai_responses', input_tokens: 100, output_tokens: 20, cache_read_tokens: 0, cache_write_tokens: 0, estimated: false, duration_ms: 250, ttft_ms: 50, status: 200, error: '', official_value: 0, official_currency: 'USD', actual_cost: null, currency: '' };
let fetcher: ReturnType<typeof vi.fn>;
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  fetcher = vi.fn((url: string, _init?: RequestInit) => Promise.resolve(response(url.startsWith('/api/logs?') ? [log] : [{ id: 12, name: 'Main' }])));
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });
const logCalls = () => fetcher.mock.calls.filter(([url]) => url.startsWith('/api/logs?'));
const changeModel = (value: string) => fireEvent.change(screen.getByRole('textbox', { name: '请求模型ID（精确匹配）' }), { target: { value } });
const query = () => fireEvent.click(screen.getByRole('button', { name: '查询日志' }));
const range = () => screen.getByLabelText('已应用查询范围').textContent;
async function loaded() { await screen.findByRole('button', { name: '查看请求 1 详情' }); }

it('keeps drafts separate from results and sends a trimmed exact model ID only after query', async () => {
  render(<Logs />); await loaded();
  const originalRange = range();
  changeModel(' vendor/a+b ');
  expect(screen.getByText('筛选已更改，等待查询。')).toBeTruthy();
  expect(logCalls()).toHaveLength(1);
  expect(range()).toBe(originalRange);
  query();
  await waitFor(() => expect(logCalls()).toHaveLength(2));
  expect(new URLSearchParams(logCalls()[1][0].split('?')[1]).get('model')).toBe('vendor/a+b');
  await waitFor(() => expect(range()).toContain('模型 ID：vendor/a+b'));
  expect(screen.queryByText('筛选已更改，等待查询。')).toBeNull();
});

it('labels old rows with their successful range while a new query is loading or has failed', async () => {
  render(<Logs />); await loaded();
  const originalRange = range();
  let finish!: (value: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
  changeModel('new-model'); query();
  await screen.findByText('正在查询新条件，当前保留上次结果');
  expect(screen.getByRole('button', { name: '查看请求 1 详情' })).toBeTruthy();
  expect(range()).toBe(originalRange);
  await act(async () => finish(response({ error: '日志读取失败' }, 503)));
  await screen.findByText('日志读取失败（当前保留上次结果）');
  expect(range()).toBe(originalRange);
  expect(screen.getByRole('button', { name: '查看请求 1 详情' })).toBeTruthy();
  fetcher.mockImplementation((url, _init) => Promise.resolve(response(url.startsWith('/api/logs?') ? [{ ...log, id: 2, model: 'new-model' }] : [])));
  fireEvent.click(screen.getByRole('button', { name: '重试' }));
  await screen.findByRole('button', { name: '查看请求 2 详情' });
  expect(range()).toContain('模型 ID：new-model');
  expect(screen.queryByRole('button', { name: '查看请求 1 详情' })).toBeNull();
});

it('preserves rows and an opened detail during a same-query refresh failure', async () => {
  render(<Logs />); await loaded();
  let finish!: (value: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
  fireEvent.click(screen.getByRole('button', { name: '刷新日志' }));
  await screen.findByText('正在刷新，当前保留上次结果');
  fireEvent.click(screen.getByRole('button', { name: '查看请求 1 详情' }));
  fireEvent.click(within(screen.getByRole('dialog')).getByText('Token 与计价资料'));
  const section = screen.getByText('Token 与计价资料').closest('details');
  expect(section).toHaveProperty('open', true);
  await act(async () => finish(response({ error: '刷新失败' }, 503)));
  await screen.findByText('刷新失败（当前保留上次结果）');
  expect(screen.getByRole('dialog')).toBeTruthy();
  expect(section).toHaveProperty('open', true);
  expect(screen.getByRole('button', { name: '查看请求 1 详情' })).toBeTruthy();
});

it('lets a newer query supersede an in-flight query and rejects its late response', async () => {
  render(<Logs />); await loaded();
  let finishA!: (value: Response) => void;
  let finishB!: (value: Response) => void;
  fetcher.mockImplementation((url, _init) => {
    const model = new URLSearchParams(url.split('?')[1]).get('model');
    if (model === 'a') return new Promise(resolve => { finishA = resolve; });
    if (model === 'b') return new Promise(resolve => { finishB = resolve; });
    return Promise.resolve(response([log]));
  });
  changeModel('a'); query();
  await waitFor(() => expect(logCalls()).toHaveLength(2));
  const signalA = logCalls()[1][1]?.signal;
  changeModel('b');
  expect(screen.getByRole('button', { name: '查询日志' })).toHaveProperty('disabled', false);
  query();
  await waitFor(() => expect(logCalls()).toHaveLength(3));
  expect(signalA?.aborted).toBe(true);
  await act(async () => finishB(response([{ ...log, id: 3, model: 'b' }])));
  await screen.findByRole('button', { name: '查看请求 3 详情' });
  await act(async () => finishA(response([{ ...log, id: 2, model: 'a' }])));
  expect(range()).toContain('模型 ID：b');
  expect(screen.queryByRole('button', { name: '查看请求 2 详情' })).toBeNull();
});

it('applies channel, status, date and limit together and treats clearing as another draft', async () => {
  render(<Logs />); await loaded();
  fireEvent.change(screen.getByRole('combobox', { name: '渠道' }), { target: { value: '12' } });
  fireEvent.change(screen.getByRole('spinbutton', { name: '状态码（精确匹配）' }), { target: { value: '503' } });
  fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: '2026-10-01' } });
  fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '2026-10-09' } });
  fireEvent.change(screen.getByRole('combobox', { name: '返回条数' }), { target: { value: '200' } });
  query();
  await waitFor(() => expect(range()).toContain('状态 503'));
  const params = new URLSearchParams(logCalls()[1][0].split('?')[1]);
  expect(params.get('channel_id')).toBe('12'); expect(params.get('status')).toBe('503'); expect(params.get('limit')).toBe('200');
  expect(new Date(params.get('from')!).getDate()).toBe(1); expect(new Date(params.get('to')!).getDate()).toBe(9);
  expect(range()).toContain('渠道：Main');
  fireEvent.click(screen.getByRole('button', { name: '清除筛选' }));
  expect(logCalls()).toHaveLength(2);
  expect(screen.getByText('筛选已更改，等待查询。')).toBeTruthy();
  expect(range()).toContain('状态 503');
  query();
  await waitFor(() => expect(range()).toContain('全部状态'));
  expect(logCalls()[2][0]).toBe('/api/logs?limit=100');
});

it('shows errors and attempt trace before progressive token and pricing diagnostics without changing legacy resolution', async () => {
  const basis = { method: 'request', calculated_at: log.time, currency: 'USD', unit_tokens: 1000000, resolution: { layer: 'historylegacy', model: 'old-model' }, rates: { input: 1, output: 2 }, matched_rules: ['legacy-rule'], note: '历史备注' };
  fetcher.mockImplementation(url => Promise.resolve(response(url.startsWith('/api/logs?') ? [{ ...log, error: '请求未完成', actual_pricing: basis, attempts: [{ attempt_index: 1, channel_id: 12, channel_name: 'Main', upstream_model: 'upstream', status_code: 503, duration_ms: 250, error_reason: '上游繁忙' }] }] : [])));
  render(<Logs />); await loaded();
  fireEvent.click(screen.getByRole('button', { name: '查看请求 1 详情' }));
  const dialog = screen.getByRole('dialog');
  const error = within(dialog).getByText('请求未完成');
  const trace = within(dialog).getByRole('region', { name: '尝试与切换轨迹' });
  const token = within(dialog).getByText('Token 与计价资料').closest('details')!;
  expect(error.compareDocumentPosition(trace) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(trace.compareDocumentPosition(token) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(token.open).toBe(false);
  fireEvent.click(within(dialog).getByText('Token 与计价资料'));
  expect(within(dialog).getByText('historylegacy · old-model')).toBeTruthy();
  expect(within(dialog).getByText('历史备注')).toBeTruthy();
  expect(within(dialog).getByText('legacy-rule')).toBeTruthy();
  expect(within(dialog).getByText('等价API价值').nextElementSibling?.textContent).toBe('$0.00 USD');
  expect(within(dialog).getByText('实际支出').nextElementSibling?.textContent).toBe('—');
});

it('distinguishes an initial query error from an empty successful result and supports retry', async () => {
  fetcher.mockImplementation(url => Promise.resolve(url.startsWith('/api/logs?') ? response({ error: '读取失败' }, 503) : response([])));
  render(<Logs />); await screen.findByText('读取失败');
  expect(screen.queryByText('暂无匹配的请求')).toBeNull();
  expect(screen.queryByLabelText('已应用查询范围')).toBeNull();
  fetcher.mockImplementation(() => Promise.resolve(response([])));
  fireEvent.click(screen.getByRole('button', { name: '重试' }));
  await screen.findByText('暂无匹配的请求');
  expect(range()).toContain('全部模型');
});
