// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Channel, CodexQuotaResponse, CredentialItem } from '../api';
import type { CredentialOperationsReport } from '../types/credential-operations';
import { CodexCredentialMenu, CodexCredentialOperations, type CodexOperationsView } from './CodexCredentialOperations';
import { ConfirmProvider } from './Confirm';

const base = '/api/channels/7/credentials/one%2Ftwo/operations';
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const credential: CredentialItem = { id: 'one/two', label: 'Private note', enabled: true, has_oauth: true, oauth_info: { provider: 'codex', account_label: 'hidden@example.invalid', status: 'active', expires_at: '', updated_at: '' } };
const channel: Channel = { id: 7, generation: 4, name: 'Codex account', api_key: '', has_key: false, credential_type: 'codex_oauth', enabled: true, models: ['enabled-model', 'second-model'], available_models: ['catalog-only'], credentials: [credential], endpoints: [] };
const quota: CodexQuotaResponse = { ok: true, buckets: [{ id: 'general', general: true, primary: { window_seconds: 18000, used_percent: 10 }, secondary: { window_seconds: 604800, used_percent: 50 } }, { id: 'extra', primary: { window_seconds: 2592000 } }] };
const freshReport = (): CredentialOperationsReport => ({ supported: true, channel_generation: 4, identity: 'opaque-identity', time_zone: 'Asia/Shanghai', config: { warmup: { enabled: false, mode: 'reset', model: '', daily_times: [] }, auto_reset: { enabled: false, rules: [] } }, reset_cards: { captured_at: '2026-10-18T01:00:00Z', available_count: 2, credits: [{ id: 'private-card-id', type: 'rate_limit', status: 'available', expires_at: 1795000000, available: true }] } });
function setup(view: CodexOperationsView = 'cards', initial = freshReport(), custom?: (url: string, init?: RequestInit) => Promise<Response> | undefined, row = channel) {
  let current = initial;
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    const response = custom?.(url, init); if (response) return response;
    if (url === base && !init?.method) return Promise.resolve(json(current));
    if (url === `${base}/refresh-reset-cards`) return Promise.resolve(json(current));
    if (url === base && init?.method === 'PUT') {
      const patch = JSON.parse(String(init.body));
      current = { ...current, config: { warmup: patch.warmup || (patch.warmup_model !== undefined ? { ...current.config.warmup, model: patch.warmup_model } : current.config.warmup), auto_reset: patch.auto_reset || current.config.auto_reset } };
      return Promise.resolve(json(current));
    }
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  const callbacks = { onClose: vi.fn(), onCards: vi.fn(), onChanged: vi.fn() };
  const props = { channel: row, credential: row.credentials![0], name: 'Private note', quota, initialView: view, blocked: false, ...callbacks };
  const result = render(<CodexCredentialOperations {...props} />, { wrapper: ConfirmProvider });
  return { ...result, fetcher, props, callbacks, currentReport: () => current, setReport: (next: CredentialOperationsReport) => { current = next; } };
}
const writes = (fetcher: ReturnType<typeof setup>['fetcher'], path: string, method = 'POST') => fetcher.mock.calls.filter(([url, init]) => url === `${base}${path}` && init?.method === method);
async function loaded() { await screen.findByRole('heading', { name: '可用 2 次' }); await waitFor(() => expect(screen.getByRole('button', { name: '刷新重置卡' })).toHaveProperty('disabled', false)); }
async function confirmConsume() { fireEvent.click(screen.getByRole('button', { name: '使用一张' })); fireEvent.click(screen.getByRole('button', { name: '确认使用一张' })); }

beforeEach(() => {
  sessionStorage.clear();
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('blocks a previously opened confirmation after the plugin is disabled while allowing local rereads', async () => {
  const view = setup(); await loaded();
  fireEvent.click(screen.getByRole('button', { name: '使用一张' }));
  await screen.findByRole('button', { name: '确认使用一张' });
  view.rerender(<CodexCredentialOperations {...view.props} networkBlocked />);
  fireEvent.click(screen.getByRole('button', { name: '确认使用一张' }));
  await waitFor(() => expect(screen.queryByRole('button', { name: '确认使用一张' })).toBeNull());
  expect(writes(view.fetcher, '/consume-reset')).toHaveLength(0);
  expect(screen.getByRole('button', { name: '刷新重置卡' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('button', { name: '使用一张' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('button', { name: '重新读取状态' })).toHaveProperty('disabled', false);
  fireEvent.click(screen.getByRole('button', { name: '重新读取状态' }));
  await waitFor(() => expect(screen.getByRole('button', { name: '关闭' })).toHaveProperty('disabled', false));
});

it('does not automatically refresh cards when the plugin disables during the initial local read', async () => {
  let finish!: (value: Response) => void;
  const view = setup('cards', freshReport(), (url, init) => url === base && !init?.method ? new Promise(resolve => { finish = resolve; }) : undefined);
  view.rerender(<CodexCredentialOperations {...view.props} networkBlocked />);
  await act(async () => finish(json(freshReport())));
  expect(await screen.findByRole('heading', { name: '可用 2 次' })).toBeTruthy();
  expect(writes(view.fetcher, '/refresh-reset-cards')).toHaveLength(0);
  expect(screen.getByRole('button', { name: '关闭' })).toHaveProperty('disabled', false);
});

it('retains and saves local configuration drafts after the plugin disables', async () => {
  const view = setup('warmup');
  const model = await screen.findByRole('combobox', { name: '预热模型' });
  fireEvent.change(model, { target: { value: 'second-model' } });
  view.rerender(<CodexCredentialOperations {...view.props} networkBlocked />);
  expect(model).toHaveProperty('value', 'second-model');
  expect(model).toHaveProperty('disabled', false);
  expect(screen.getByRole('button', { name: '保存并预热' })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(view.callbacks.onClose).toHaveBeenCalledOnce());
  expect(writes(view.fetcher, '', 'PUT')).toHaveLength(1);
  expect(writes(view.fetcher, '/warmup')).toHaveLength(0);
});

it('saves a manually selected warmup model on exit without sending a warmup message', async () => {
  const view = setup('warmup');
  fireEvent.change(await screen.findByRole('combobox', { name: '预热模型' }), { target: { value: 'second-model' } });
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(view.callbacks.onClose).toHaveBeenCalledOnce());
  expect(JSON.parse(String(writes(view.fetcher, '', 'PUT')[0][1]!.body))).toEqual({ expected_identity: 'opaque-identity', warmup_model: 'second-model' });
  expect(writes(view.fetcher, '/warmup')).toHaveLength(0);
});

it('keeps an automatic configuration after an exit save is rejected, then closes after a successful retry', async () => {
  let fail = true;
  const view = setup('auto-warmup', freshReport(), (url, init) => fail && url === base && init?.method === 'PUT' ? Promise.resolve(json({ error: '退出保存失败' }, 409)) : undefined);
  const model = await screen.findByRole('combobox', { name: '预热模型' });
  fireEvent.change(model, { target: { value: 'second-model' } });
  fireEvent.click(screen.getByRole('button', { name: '关闭' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('操作未完成：退出保存失败'); expect(model).toHaveProperty('value', 'second-model'); expect(view.callbacks.onClose).not.toHaveBeenCalled();
  fail = false;
  fireEvent.click(screen.getByRole('button', { name: '关闭' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(view.callbacks.onClose).toHaveBeenCalledOnce());
});

it('reads the exact target locally before refreshing cards and requires explicit consumption confirmation', async () => {
  let count = 0;
  const view = setup('cards', freshReport(), (url, init) => url === `${base}/consume-reset` ? Promise.resolve(json({ ...freshReport(), last_reset: { request_id: JSON.parse(String(init!.body)).request_id, state: 'succeeded', detail: '上游已接受使用一张', at: '2026-10-18T01:01:00Z' }, reset_cards: { ...freshReport().reset_cards!, available_count: ++count } })) : undefined);
  await loaded();
  expect(view.fetcher.mock.calls[0][0]).toBe(base);
  expect(JSON.parse(String(view.fetcher.mock.calls[1][1]!.body))).toEqual({ expected_identity: 'opaque-identity' });
  expect(screen.getByText('额度重置')).toBeTruthy();
  expect(screen.queryByText('hidden@example.invalid')).toBeNull(); expect(screen.queryByText('private-card-id')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '使用一张' }));
  const confirmation = screen.getByRole('dialog', { name: '使用一张重置卡？' });
  expect(within(confirmation).getByText(/消费一张可用重置卡/)).toBeTruthy();
  fireEvent.click(within(confirmation).getByRole('button', { name: '取消' }));
  expect(writes(view.fetcher, '/consume-reset')).toHaveLength(0);
  await confirmConsume();
  await screen.findByText('上游已接受使用一张');
  expect(writes(view.fetcher, '/consume-reset')).toHaveLength(1);
  const body = JSON.parse(String(writes(view.fetcher, '/consume-reset')[0][1]!.body));
  expect(body).toEqual({ expected_identity: 'opaque-identity', request_id: expect.stringMatching(/^[0-9a-f-]{36}$/) });
  expect(Object.keys(body)).not.toContain('card_id');
  expect(screen.getByRole('heading', { name: '可用 1 次' })).toBeTruthy();
});

it('retains card data on refresh failure and keeps unknown counts distinct from zero', async () => {
  const initial = freshReport(); initial.reset_cards!.available_count = null; initial.reset_cards!.credits[0].available = false;
  const view = setup('cards', initial, url => url === `${base}/refresh-reset-cards` ? Promise.resolve(json({ ...initial, reset_cards_error: '卡片查询失败' })) : undefined);
  await screen.findByText(/卡片查询失败（保留上次明细）/);
  expect(screen.getByRole('heading', { name: '可用 次数未知' })).toBeTruthy();
  expect(screen.getByText('额度重置')).toBeTruthy();
  expect(screen.getByRole('button', { name: '使用一张' })).toHaveProperty('disabled', true);
  expect(writes(view.fetcher, '/consume-reset')).toHaveLength(0);
});

it('saves only the shared model before one manual warmup and honors a failed receipt on HTTP 200', async () => {
  let finish!: (value: Response) => void;
  const initial = freshReport(); initial.config.warmup = { enabled: true, mode: 'daily', model: '', daily_times: ['08:00', '19:00'] };
  initial.config.auto_reset = { enabled: true, rules: [{ window_seconds: 604800, remaining_below: 10, expires_within_days: null, join: 'and' }] };
  const view = setup('warmup', initial, (url, init) => url === `${base}/warmup` ? new Promise(resolve => { finish = resolve; expect(JSON.parse(String(init!.body)).expected_identity).toBe('opaque-identity'); }) : undefined);
  const model = await screen.findByRole('combobox', { name: '预热模型' });
  expect(screen.getByRole('dialog', { name: '手动预热 · Private note' })).toBeTruthy();
  expect(screen.queryByRole('checkbox', { name: '自动预热' })).toBeNull();
  expect(screen.queryByRole('checkbox', { name: '自动重置' })).toBeNull();
  expect(screen.queryByRole('radio')).toBeNull();
  expect(screen.queryByRole('button', { name: '保存配置' })).toBeNull();
  expect(within(model).queryByRole('option', { name: 'catalog-only' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '预热一次' }));
  expect(await screen.findByText('请选择一个已启用的预热模型。')).toBeTruthy();
  expect(writes(view.fetcher, '/warmup')).toHaveLength(0);
  fireEvent.change(model, { target: { value: 'enabled-model' } });
  fireEvent.click(screen.getByRole('button', { name: '保存并预热' }));
  fireEvent.click(screen.getByRole('button', { name: '保存并预热' }));
  await waitFor(() => expect(writes(view.fetcher, '/warmup')).toHaveLength(1));
  expect(JSON.parse(String(writes(view.fetcher, '', 'PUT')[0][1]!.body))).toEqual({ expected_identity: 'opaque-identity', warmup_model: 'enabled-model' });
  const requestID = JSON.parse(String(writes(view.fetcher, '/warmup')[0][1]!.body)).request_id;
  await act(async () => finish(json({ ...initial, config: { ...initial.config, warmup: { ...initial.config.warmup, model: 'enabled-model' } }, last_warmup: { request_id: requestID, state: 'failed', detail: '模型请求未完成', at: '2026-10-18T01:00:00Z' } })));
  expect(screen.getByText('模型请求未完成')).toBeTruthy();
  expect(screen.getByText('未完成')).toBeTruthy(); expect(screen.queryByText('已完成')).toBeNull();
  expect(view.callbacks.onChanged).toHaveBeenCalledTimes(1);
});

it('adds and removes explicit daily times, deduplicates them and saves only automatic warmup', async () => {
  const view = setup('auto-warmup');
  const model = await screen.findByRole('combobox', { name: '预热模型' });
  expect(screen.getByRole('dialog', { name: '自动预热 · Private note' })).toBeTruthy();
  expect(screen.getByRole('checkbox', { name: '自动预热' })).toHaveProperty('checked', false);
  expect(screen.getByRole('radio', { name: '跟随额度自然重置' })).toHaveProperty('checked', true);
  expect(screen.queryByRole('checkbox', { name: '自动重置' })).toBeNull();
  expect(screen.queryByRole('button', { name: '预热一次' })).toBeNull();
  fireEvent.change(model, { target: { value: 'enabled-model' } });
  fireEvent.click(screen.getByRole('checkbox', { name: '自动预热' }));
  fireEvent.click(screen.getByRole('radio', { name: '每日定时' }));
  expect(screen.getByRole('radio', { name: '跟随额度自然重置' })).toHaveProperty('checked', false);
  expect(screen.queryByLabelText('每日预热时刻 1')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  expect(screen.getByText('每日定时至少需要一个预热时刻。')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '添加预热时刻' }));
  expect(screen.getByLabelText('每日预热时刻 1')).toHaveProperty('value', '');
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  expect(screen.getByText('请填写有效的每日预热时刻，或删除空时刻。')).toBeTruthy();
  for (const [index, time] of ['10:30', '08:00', '10:30'].entries()) {
    if (index) fireEvent.click(screen.getByRole('button', { name: '添加预热时刻' }));
    fireEvent.change(screen.getByLabelText(`每日预热时刻 ${index + 1}`), { target: { value: time } });
  }
  fireEvent.click(screen.getByRole('button', { name: '添加预热时刻' }));
  fireEvent.click(screen.getByRole('button', { name: '删除预热时刻 4' }));
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  await screen.findByText('已保存');
  expect(JSON.parse(String(writes(view.fetcher, '', 'PUT')[0][1]!.body))).toEqual({ expected_identity: 'opaque-identity', warmup: { enabled: true, mode: 'daily', model: 'enabled-model', daily_times: ['08:00', '10:30'] } });
  expect(writes(view.fetcher, '/warmup')).toHaveLength(0);
});

it('configures multiple flat rules with the longest general window, independent conditions and group joins', async () => {
  const view = setup('auto-reset');
  await screen.findByRole('checkbox', { name: '自动重置' });
  expect(screen.queryByRole('combobox', { name: '预热模型' })).toBeNull();
  fireEvent.click(screen.getByRole('checkbox', { name: '自动重置' }));
  fireEvent.click(screen.getByRole('button', { name: '添加规则' }));
  expect(screen.getByRole('combobox', { name: '规则 1 额度窗口' })).toHaveProperty('value', '604800');
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  expect(screen.getByText('第 1 组至少选择一个条件。')).toBeTruthy();
  fireEvent.click(screen.getByRole('checkbox', { name: '规则 1 额度条件' }));
  fireEvent.change(screen.getByRole('spinbutton', { name: '规则 1 剩余比例' }), { target: { value: '20' } });
  fireEvent.click(screen.getByRole('checkbox', { name: '规则 1 到期条件' }));
  fireEvent.change(screen.getByRole('spinbutton', { name: '规则 1 卡到期天数' }), { target: { value: '3' } });
  fireEvent.change(screen.getByRole('combobox', { name: '规则 1 条件关系' }), { target: { value: 'or' } });
  expect(screen.getByText('卡即将到期时，即使额度充足也会使用。')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '添加规则' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '规则 2 到期条件' }));
  fireEvent.change(screen.getByRole('spinbutton', { name: '规则 2 卡到期天数' }), { target: { value: '1' } });
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  await waitFor(() => expect(writes(view.fetcher, '', 'PUT')).toHaveLength(1));
  expect(JSON.parse(String(writes(view.fetcher, '', 'PUT')[0][1]!.body)).auto_reset).toEqual({ enabled: true, rules: [{ window_seconds: 604800, remaining_below: 20, expires_within_days: 3, join: 'or' }, { window_seconds: 604800, remaining_below: null, expires_within_days: 1, join: 'and' }] });
  expect(screen.getByText(/条件连续满足时只执行一次/)).toBeTruthy();
});

it('preserves configuration drafts after save errors and asks before closing unsaved changes', async () => {
  setup('auto-warmup', freshReport(), (url, init) => url === base && init?.method === 'PUT' ? Promise.resolve(json({ error: '保存被拒绝' }, 409)) : undefined);
  const model = await screen.findByRole('combobox', { name: '预热模型' });
  fireEvent.change(model, { target: { value: 'second-model' } });
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  await screen.findByText('操作未完成：保存被拒绝');
  expect(model).toHaveProperty('value', 'second-model');
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  const confirmation = screen.getByRole('dialog', { name: '保存更改？' });
  fireEvent.click(within(confirmation).getByRole('button', { name: '关闭弹窗' }));
  expect(model).toHaveProperty('value', 'second-model');
});

it('keeps unknown actions blocked until explicit acknowledgment', async () => {
  const initial = freshReport(); initial.last_reset = { request_id: crypto.randomUUID(), state: 'unknown', detail: '请核对实际结果', at: '2026-10-18T01:00:00Z' };
  const view = setup('cards', initial, url => url === `${base}/acknowledge` ? Promise.resolve(json({ ...initial, last_reset: { ...initial.last_reset!, state: 'acknowledged' } })) : undefined);
  await loaded();
  expect(screen.getByText('结果未确认')).toBeTruthy();
  expect(screen.getByRole('button', { name: '使用一张' })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('button', { name: '已核对重置卡结果' }));
  expect(writes(view.fetcher, '/acknowledge')).toHaveLength(0);
  fireEvent.click(screen.getByRole('button', { name: '确认已核对' }));
  await screen.findByText('已核对');
  expect(JSON.parse(String(writes(view.fetcher, '/acknowledge')[0][1]!.body))).toEqual({ expected_identity: 'opaque-identity', kind: 'reset' });
  expect(screen.getByRole('button', { name: '使用一张' })).toHaveProperty('disabled', false);
});

it('retains one UUID across lost responses and reopening, and retries only that original action explicitly', async () => {
  let calls = 0;
  const first = setup('cards', freshReport(), url => url === `${base}/consume-reset` && ++calls === 1 ? Promise.reject(new TypeError('connection lost')) : undefined);
  await loaded(); await confirmConsume();
  await screen.findByRole('button', { name: '重试本次操作' });
  expect(writes(first.fetcher, '/consume-reset')).toHaveLength(1);
  const original = JSON.parse(String(writes(first.fetcher, '/consume-reset')[0][1]!.body));
  first.unmount();
  const second = setup('cards', freshReport(), (url, init) => url === `${base}/consume-reset` ? Promise.resolve(json({ ...freshReport(), last_reset: { request_id: JSON.parse(String(init!.body)).request_id, state: 'succeeded', detail: '同一操作已完成', at: '2026-10-18T01:00:00Z' } })) : undefined);
  await loaded();
  expect(writes(second.fetcher, '/consume-reset')).toHaveLength(0);
  fireEvent.click(screen.getByRole('button', { name: '重试本次操作' }));
  await screen.findByText('同一操作已完成');
  expect(JSON.parse(String(writes(second.fetcher, '/consume-reset')[0][1]!.body))).toEqual(original);
  expect(sessionStorage.getItem(`to-router.credential-operation:${base.replace('/api', '')}`)).toBeNull();
});

it('does not keep a pending action after an explicit pre-send refusal', async () => {
  let refused = true;
  const view = setup('cards', freshReport(), (url, init) => url === `${base}/consume-reset` ? Promise.resolve(refused ? json({ error: '渠道或凭证已停用' }, 400) : json({ ...freshReport(), last_reset: { request_id: JSON.parse(String(init!.body)).request_id, state: 'succeeded', detail: '已恢复后完成', at: '2026-10-18T01:00:00Z' } })) : undefined);
  await loaded(); await confirmConsume();
  await screen.findByText('操作未完成：渠道或凭证已停用');
  await waitFor(() => expect(screen.getByRole('button', { name: '使用一张' })).toHaveProperty('disabled', false));
  expect(screen.getByText('操作未完成：渠道或凭证已停用')).toBeTruthy();
  expect(sessionStorage.getItem(`to-router.credential-operation:${base.replace('/api', '')}`)).toBeNull();
  expect(screen.queryByRole('button', { name: '重试本次操作' })).toBeNull();
  refused = false;
  await confirmConsume(); await screen.findByText('已恢复后完成');
  expect(writes(view.fetcher, '/consume-reset')).toHaveLength(2);
});

it('allows disabled or unauthorized credentials to read cards while blocking consuming operations', async () => {
  const disabled = { ...channel, credentials: [{ ...credential, enabled: false, oauth_info: { ...credential.oauth_info!, status: 'reauth_required' as const } }] };
  const view = setup('cards', freshReport(), undefined, disabled); await loaded();
  expect(screen.getByText('额度重置')).toBeTruthy();
  expect(screen.getByRole('button', { name: '使用一张' })).toHaveProperty('disabled', true);
  expect(writes(view.fetcher, '/consume-reset')).toHaveLength(0);
});

it('rejects a stale initial channel generation before any upstream card read or configuration write', async () => {
  const initial = { ...freshReport(), channel_generation: 5 };
  const view = setup('cards', initial);
  await screen.findByText('渠道或凭证已变化，请关闭并刷新渠道。');
  expect(writes(view.fetcher, '/refresh-reset-cards')).toHaveLength(0);
  expect(writes(view.fetcher, '', 'PUT')).toHaveLength(0);
});

it('uses local channel configuration for menu switches and reads operations only after an explicit click', async () => {
  const initial = freshReport(); initial.config.warmup.model = 'enabled-model'; initial.config.warmup.enabled = true;
  const fetcher = vi.fn((url: string, init?: RequestInit) => Promise.resolve(json(url === base && init?.method === 'PUT' ? { ...initial, config: { ...initial.config, warmup: JSON.parse(String(init.body)).warmup } } : initial)));
  vi.stubGlobal('fetch', fetcher);
  const configure = vi.fn(), onChanged = vi.fn(), onError = vi.fn();
  render(<CodexCredentialMenu channel={channel} credential={{ ...credential, operations: initial.config }} name="Private note" blocked={false} hasFiveHourWindow configure={configure} onChanged={onChanged} onError={onError} />);
  expect(fetcher).not.toHaveBeenCalled();
  fireEvent.click(screen.getByLabelText('更多凭证操作 Private note'));
  const toggle = screen.getByRole('switch', { name: '自动预热 Private note' });
  expect(toggle.getAttribute('aria-checked')).toBe('true');
  fireEvent.click(toggle);
  await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
  expect(JSON.parse(String(fetcher.mock.calls[1][1]!.body)).warmup.enabled).toBe(false);
  expect(configure).not.toHaveBeenCalled();
});

it('opens missing automatic setup as an unsaved enabled draft instead of enabling an incomplete plan', async () => {
  const initial = freshReport(), configure = vi.fn();
  const fetcher = vi.fn(() => Promise.resolve(json(initial))); vi.stubGlobal('fetch', fetcher);
  const menu = render(<CodexCredentialMenu channel={channel} credential={credential} name="Private note" blocked={false} hasFiveHourWindow configure={configure} onChanged={vi.fn()} onError={vi.fn()} />);
  fireEvent.click(screen.getByLabelText('更多凭证操作 Private note'));
  fireEvent.click(screen.getByRole('switch', { name: '自动预热 Private note' }));
  await waitFor(() => expect(configure).toHaveBeenCalledWith('auto-warmup', 'warmup'));
  expect(fetcher).toHaveBeenCalledTimes(1); menu.unmount();
  const view = setup('auto-warmup');
  view.rerender(<CodexCredentialOperations {...view.props} enableOnOpen="warmup" />);
  expect(await screen.findByRole('checkbox', { name: '自动预热' })).toHaveProperty('checked', true);
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  expect(screen.getByText('请选择一个已启用的预热模型。')).toBeTruthy();
  expect(writes(view.fetcher, '', 'PUT')).toHaveLength(0);
});

it('retains a saved window that is currently absent and validates blank numeric conditions without converting them to zero', async () => {
  const initial = freshReport(); initial.config.auto_reset = { enabled: true, rules: [{ window_seconds: 86400, remaining_below: 15, expires_within_days: null, join: 'and' }] };
  const view = setup('auto-reset', initial);
  const window = await screen.findByRole('combobox', { name: '规则 1 额度窗口' });
  expect(window).toHaveProperty('value', '86400');
  expect(within(window).getByRole('option', { name: '1 天（当前未提供）' })).toBeTruthy();
  fireEvent.change(screen.getByRole('spinbutton', { name: '规则 1 剩余比例' }), { target: { value: '' } });
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  expect(screen.getByText(/第 1 组请选择额度窗口/)).toBeTruthy();
  expect(writes(view.fetcher, '', 'PUT')).toHaveLength(0);
  fireEvent.change(screen.getByRole('spinbutton', { name: '规则 1 剩余比例' }), { target: { value: '0' } });
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  await screen.findByText('已保存');
  expect(JSON.parse(String(writes(view.fetcher, '', 'PUT')[0][1]!.body)).auto_reset.rules[0]).toEqual({ window_seconds: 86400, remaining_below: 0, expires_within_days: null, join: 'and' });
});

it('blocks actions if identity changes after opening, and never starts the save-and-warmup continuation after unmount', async () => {
  let finish!: (value: Response) => void;
  const view = setup('warmup', freshReport(), (url, init) => url === base && init?.method === 'PUT' ? new Promise(resolve => { finish = resolve; }) : undefined);
  fireEvent.change(await screen.findByRole('combobox', { name: '预热模型' }), { target: { value: 'enabled-model' } });
  fireEvent.click(screen.getByRole('button', { name: '保存并预热' }));
  view.unmount();
  await act(async () => finish(json(freshReport())));
  expect(writes(view.fetcher, '/warmup')).toHaveLength(0);
  const reopened = setup('warmup');
  await screen.findByRole('combobox', { name: '预热模型' });
  reopened.setReport({ ...freshReport(), identity: 'new-account' });
  fireEvent.click(screen.getByRole('button', { name: '重新读取状态' }));
  await screen.findByText('凭证身份已变化，请关闭并刷新渠道。');
  expect(screen.getByRole('button', { name: '预热一次' })).toHaveProperty('disabled', true);
});

it('keeps the pending UUID for unknown receipts and clears it only after acknowledgment succeeds', async () => {
  let current = freshReport();
  const view = setup('cards', current, (url, init) => {
    if (url === `${base}/consume-reset`) { current = { ...current, last_reset: { request_id: JSON.parse(String(init!.body)).request_id, state: 'unknown', detail: '响应未能确认', at: '2026-10-18T01:00:00Z' } }; return Promise.resolve(json(current)); }
    if (url === `${base}/acknowledge`) { current = { ...current, last_reset: { ...current.last_reset!, state: 'acknowledged' } }; return Promise.resolve(json(current)); }
  });
  await loaded(); await confirmConsume();
  await screen.findByText('响应未能确认');
  const key = `to-router.credential-operation:${base.replace('/api', '')}`;
  expect(sessionStorage.getItem(key)).not.toBeNull();
  expect(screen.queryByRole('button', { name: '重试本次操作' })).toBeNull();
  expect(screen.getByRole('button', { name: '使用一张' })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('button', { name: '已核对重置卡结果' }));
  fireEvent.click(screen.getByRole('button', { name: '确认已核对' }));
  await screen.findByText('已核对');
  expect(sessionStorage.getItem(key)).toBeNull();
  expect(writes(view.fetcher, '/consume-reset')).toHaveLength(1);
});

it('leaves a failed menu switch unchanged and refuses stale generation writes', async () => {
  let stale = false;
  const initial = freshReport(); initial.config.warmup.model = 'enabled-model';
  const fetcher = vi.fn((_url: string, init?: RequestInit) => Promise.resolve(init?.method === 'PUT' ? json({ error: '保存失败' }, 500) : json({ ...initial, channel_generation: stale ? 5 : 4 })));
  vi.stubGlobal('fetch', fetcher);
  const onError = vi.fn();
  render(<CodexCredentialMenu channel={channel} credential={credential} name="Private note" blocked={false} hasFiveHourWindow configure={vi.fn()} onChanged={vi.fn()} onError={onError} />);
  fireEvent.click(screen.getByLabelText('更多凭证操作 Private note'));
  const toggle = screen.getByRole('switch', { name: '自动预热 Private note' });
  fireEvent.click(toggle);
  await waitFor(() => expect(onError).toHaveBeenCalledWith('自动配置未保存：保存失败'));
  expect(toggle.getAttribute('aria-checked')).toBe('false');
  stale = true; fireEvent.click(toggle);
  await waitFor(() => expect(onError).toHaveBeenCalledWith('自动配置未保存：渠道或凭证已变化，请刷新渠道。'));
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
});

it('keeps a menu toggle intent when a fresh report already has that state and preserves its other configuration', async () => {
  const initial = freshReport(); initial.config.warmup = { enabled: true, model: 'enabled-model', mode: 'daily', daily_times: ['12:15'] };
  const fetcher = vi.fn((_url: string, init?: RequestInit) => Promise.resolve(json(init?.method === 'PUT' ? { ...initial, config: { ...initial.config, warmup: JSON.parse(String(init.body)).warmup } } : initial)));
  vi.stubGlobal('fetch', fetcher);
  const onChanged = vi.fn();
  render(<CodexCredentialMenu channel={channel} credential={credential} name="Private note" blocked={false} hasFiveHourWindow configure={vi.fn()} onChanged={onChanged} onError={vi.fn()} />);
  fireEvent.click(screen.getByLabelText('更多凭证操作 Private note'));
  fireEvent.click(screen.getByRole('switch', { name: '自动预热 Private note' }));
  await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
  expect(JSON.parse(String(fetcher.mock.calls[1][1]!.body)).warmup).toEqual(initial.config.warmup);
});


it.each([
  ['cards', '重置卡'], ['warmup', '手动预热'], ['auto-warmup', '自动预热'], ['auto-reset', '自动重置'],
] as const)('renders %s as a separate dialog without unrelated configuration or receipts', async (view, title) => {
  const initial = freshReport();
  initial.last_reset = { request_id: crypto.randomUUID(), state: 'succeeded', detail: 'Only reset receipt', at: '2026-10-18T01:00:00Z' };
  initial.last_warmup = { request_id: crypto.randomUUID(), state: 'succeeded', detail: 'Only warmup receipt', at: '2026-10-18T01:00:00Z' };
  const result = setup(view, initial);
  await waitFor(() => expect(result.fetcher.mock.calls.length).toBeGreaterThanOrEqual(view === 'cards' ? 2 : 1));
  await waitFor(() => expect(screen.queryByText('正在加载数据…')).toBeNull());
  expect(screen.getByRole('dialog', { name: `${title} · Private note` })).toBeTruthy();
  expect(screen.queryByRole('group', { name: '凭证操作视图' })).toBeNull();
  expect(!!screen.queryByRole('combobox', { name: '预热模型' })).toBe(view === 'warmup' || view === 'auto-warmup');
  expect(!!screen.queryByRole('checkbox', { name: '自动预热' })).toBe(view === 'auto-warmup');
  expect(!!screen.queryByRole('checkbox', { name: '自动重置' })).toBe(view === 'auto-reset');
  expect(!!screen.queryByText('Only reset receipt')).toBe(view === 'cards');
  expect(!!screen.queryByText('Only warmup receipt')).toBe(view === 'warmup');
  expect(!!screen.queryByRole('button', { name: '使用一张' })).toBe(view === 'cards');
  expect(!!screen.queryByRole('button', { name: '预热一次' })).toBe(view === 'warmup');
});

it('preserves a concurrent automatic-reset edit when saving only automatic warmup', async () => {
  const view = setup('auto-warmup');
  fireEvent.change(await screen.findByRole('combobox', { name: '预热模型' }), { target: { value: 'enabled-model' } });
  const remote = freshReport(); remote.config.auto_reset = { enabled: true, rules: [{ window_seconds: 18000, remaining_below: 7, expires_within_days: null, join: 'and' }] };
  view.setReport(remote);
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  await screen.findByText('已保存');
  const patch = JSON.parse(String(writes(view.fetcher, '', 'PUT')[0][1]!.body));
  expect(patch).toEqual({ expected_identity: 'opaque-identity', warmup: { enabled: false, model: 'enabled-model', mode: 'reset', daily_times: [] } });
  expect(view.currentReport().config.auto_reset).toEqual(remote.config.auto_reset);
});

it('does not validate or submit warmup fields in the automatic-reset dialog', async () => {
  const initial = freshReport(); initial.config.warmup = { enabled: true, model: 'retired-model', mode: 'daily', daily_times: [] };
  const view = setup('auto-reset', initial);
  await screen.findByRole('checkbox', { name: '自动重置' });
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  await screen.findByText('已保存');
  expect(JSON.parse(String(writes(view.fetcher, '', 'PUT')[0][1]!.body))).toEqual({ expected_identity: 'opaque-identity', auto_reset: { enabled: false, rules: [] } });
  expect(view.currentReport().config.warmup).toEqual(initial.config.warmup);
});

it('executes an unchanged manual model without saving any automatic configuration', async () => {
  const initial = freshReport(); initial.config.warmup = { enabled: true, model: 'enabled-model', mode: 'daily', daily_times: ['07:10', '13:20'] };
  const view = setup('warmup', initial, (url, init) => url === `${base}/warmup` ? Promise.resolve(json({ ...initial, last_warmup: { request_id: JSON.parse(String(init!.body)).request_id, state: 'succeeded', detail: '预热已完成', at: '2026-10-18T01:00:00Z' } })) : undefined);
  view.rerender(<CodexCredentialOperations {...view.props} quota={{ ok: true, buckets: [] }} />);
  await screen.findByRole('combobox', { name: '预热模型' });
  fireEvent.click(screen.getByRole('button', { name: '预热一次' }));
  await screen.findByText('预热已完成');
  expect(writes(view.fetcher, '', 'PUT')).toHaveLength(0);
  expect(writes(view.fetcher, '/warmup')).toHaveLength(1);
});

it('keeps another action blocked without rendering its receipt or retry in the wrong dialog', async () => {
  const initial = freshReport(); initial.config.warmup.model = 'enabled-model';
  initial.last_reset = { request_id: crypto.randomUUID(), state: 'unknown', detail: 'Foreign reset detail', at: '2026-10-18T01:00:00Z' };
  sessionStorage.setItem(`to-router.credential-operation:${base.replace('/api', '')}`, JSON.stringify({ identity: initial.identity, kind: 'reset', request_id: crypto.randomUUID() }));
  setup('warmup', initial);
  await screen.findByText('另有重置卡操作尚未确认，请到重置卡弹窗读取状态并核对。');
  expect(screen.getByRole('button', { name: '预热一次' })).toHaveProperty('disabled', true);
  expect(screen.queryByText('Foreign reset detail')).toBeNull();
  expect(screen.queryByRole('button', { name: '已核对重置卡结果' })).toBeNull();
  expect(screen.queryByRole('button', { name: '重试本次操作' })).toBeNull();
});

it('opens daily automatic setup instead of enabling a plan with no configured times', async () => {
  const initial = freshReport(); initial.config.warmup.model = 'enabled-model'; initial.config.warmup.mode = 'daily';
  const fetcher = vi.fn((_url: string, _init?: RequestInit) => Promise.resolve(json(initial)));
  vi.stubGlobal('fetch', fetcher);
  const configure = vi.fn();
  render(<CodexCredentialMenu channel={channel} credential={credential} name="Private note" blocked={false} hasFiveHourWindow configure={configure} onChanged={vi.fn()} onError={vi.fn()} />);
  fireEvent.click(screen.getByLabelText('更多凭证操作 Private note'));
  fireEvent.click(screen.getByRole('switch', { name: '自动预热 Private note' }));
  await waitFor(() => expect(configure).toHaveBeenCalledWith('auto-warmup', 'warmup'));
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
});

it.each(['reset', 'daily'] as const)('does not allow %s automatic warmup to be enabled when the five-hour window is unknown', async mode => {
  const initial = freshReport(); initial.config.warmup.mode = mode;
  const view = setup('auto-warmup', initial);
  view.rerender(<CodexCredentialOperations {...view.props} quota={{ ok: true, buckets: [{ id: 'extra', primary: { window_seconds: 18000 } }] }} />);
  const toggle = await screen.findByRole('checkbox', { name: '自动预热' });
  expect(toggle).toHaveProperty('disabled', true);
  expect(screen.getByText('上游未明确提供 5 小时额度窗口，暂不支持自动预热。')).toBeTruthy();
  expect(writes(view.fetcher, '', 'PUT')).toHaveLength(0);
});

it('allows an existing unsupported automatic plan to be disabled without touching reset rules', async () => {
  const initial = freshReport(); initial.config.warmup = { enabled: true, mode: 'daily', model: 'enabled-model', daily_times: ['18:00'] };
  const view = setup('auto-warmup', initial);
  view.rerender(<CodexCredentialOperations {...view.props} quota={undefined} />);
  const toggle = await screen.findByRole('checkbox', { name: '自动预热' });
  expect(toggle).toHaveProperty('disabled', false);
  fireEvent.click(toggle);
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }));
  await screen.findByText('已保存');
  expect(JSON.parse(String(writes(view.fetcher, '', 'PUT')[0][1]!.body))).toEqual({ expected_identity: initial.identity, warmup: { ...initial.config.warmup, enabled: false } });
  expect(toggle).toHaveProperty('disabled', true);
});
