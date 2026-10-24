// @vitest-environment jsdom
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Channel, CredentialItem } from '../api';
import type { CredentialOperationsReport } from '../types/credential-operations';
import { oauthCredentialIdentity, type OAuthBatchTarget } from '../hooks/useOAuthCredentialBatch';
import { quotaTargetKey, type QuotaObservation } from '../hooks/useCodexQuota';
import { OAuthCredentialBatch } from './OAuthCredentialBatch';
import { ConfirmProvider } from './Confirm';
const render = (ui: Parameters<typeof renderUI>[0]) => renderUI(ui, { wrapper: ConfirmProvider });

const config = { warmup: { enabled: false, mode: 'reset' as const, model: 'model-a', daily_times: [] }, auto_reset: { enabled: false, rules: [] } };
const credential = (id: string): CredentialItem => ({ id, label: id, enabled: true, has_oauth: true, oauth_info: { provider: 'codex', account_label: `${id}@example.invalid`, status: 'active', updated_at: '2026-10-18', expires_at: '' }, operations: structuredClone(config) });
const channel = (): Channel => ({ id: 7, generation: 4, name: 'Channel', enabled: true, api_key: '', has_key: false, endpoints: [], models: ['model-a'], credential_type: 'codex_oauth', credentials: [credential('one/ ?'), credential('two'), credential('three')] });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const report = (id: string): CredentialOperationsReport => ({ supported: true, identity: `identity:${id}`, channel_generation: 4, config: structuredClone(config), time_zone: 'Asia/Shanghai' });
function setup(source = channel(), override?: (url: string, init?: RequestInit) => Promise<Response> | undefined, selected = source.credentials!.slice(0, 2)) {
  const items: OAuthBatchTarget[] = selected.map(value => ({ credential: value, name: value.label!, generation: source.generation, target: { channelID: source.id, credentialID: value.id, identity: oauthCredentialIdentity(value) } }));
  const observations: Record<string, QuotaObservation> = Object.fromEntries(items.map(item => [quotaTargetKey(item.target), { snapshot: { ok: true, captured_at: '2026-10-18', buckets: [{ id: 'general', general: true, primary: { window_seconds: 18000, used_percent: 10 } }] } }]));
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    const custom = override?.(url, init); if (custom) return custom;
    const match = url.match(/\/credentials\/([^/]+)\/operations$/);
    if (match && !init?.method) return Promise.resolve(json(report(decodeURIComponent(match[1]))));
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  const props = { channel: source, items, codex: true, observations, refresh: vi.fn(), blocked: false, clear: vi.fn(), onChanged: vi.fn(), onBusy: vi.fn(), onGeneration: vi.fn() };
  return { ...render(<OAuthCredentialBatch {...props} />), props, fetcher };
}
beforeEach(() => {
  sessionStorage.clear();
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('offers native export for a single selected compatible credential even when networking is paused', () => {
  const source = channel(); source.credential_type = 'plugin:qoder';
  const view = setup(source, undefined, [source.credentials![1]]);
  view.rerender(<OAuthCredentialBatch {...view.props} codex={false} networkBlocked />);
  fireEvent.click(screen.getByRole('button', { name: '导出凭证' }));
  expect(screen.getByRole('dialog', { name: '导出凭证' })).toBeTruthy();
  expect(screen.getByRole('list', { name: '选中导出凭证' }).children).toHaveLength(1);
  expect(screen.queryByLabelText(/口令/)).toBeNull();
  expect(view.fetcher).not.toHaveBeenCalled();
});

it('invalidates an open export when the selected channel generation changes', async () => {
  const view = setup();
  fireEvent.click(screen.getByRole('button', { name: '导出凭证' }));
  view.rerender(<OAuthCredentialBatch {...view.props} channel={{ ...view.props.channel, generation: 8 }} />);
  expect(await screen.findByText('选中的凭证已变化，请关闭后重新选择。')).toBeTruthy();
  expect(screen.getByRole('button', { name: '下载凭证' })).toHaveProperty('disabled', true);
  expect(view.fetcher).not.toHaveBeenCalled();
});

it('does not expose encrypted export for public credentials', () => {
  const source = channel(); source.credential_type = 'zen_free';
  setup(source);
  expect(screen.queryByRole('button', { name: '导出凭证' })).toBeNull();
});

it('disables networking in an open batch dialog but retains selection and configuration controls', async () => {
  const view = setup();
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  view.rerender(<OAuthCredentialBatch {...view.props} networkBlocked />);
  expect(screen.getByRole('button', { name: '确认各发送一次 hi' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('button', { name: '关闭' })).toHaveProperty('disabled', false);
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(screen.getByRole('button', { name: '刷新额度' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('button', { name: '清除选择' })).toHaveProperty('disabled', false);
  expect(screen.getByRole('button', { name: '启用' })).toHaveProperty('disabled', false);
  expect(screen.getByRole('button', { name: '自动预热配置' })).toHaveProperty('disabled', false);
  expect(view.fetcher).not.toHaveBeenCalled();
});

it('stops queued batch warmups when the plugin disables during local preflight', async () => {
  const reads: Array<(value: Response) => void> = [];
  const view = setup(channel(), (url, init) => url.endsWith('/operations') && !init?.method ? new Promise(resolve => { reads.push(resolve); }) : undefined);
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await waitFor(() => expect(reads).toHaveLength(2));
  view.rerender(<OAuthCredentialBatch {...view.props} networkBlocked />);
  await act(async () => { reads[0](json(report('one/ ?'))); reads[1](json(report('two'))); });
  await screen.findAllByText('接入插件未就绪，已暂停预热。');
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(0);
});

it('saves edited batch configuration on exit and retains failed target drafts', async () => {
  let fail = true;
  const view = setup(channel(), (url, init) => init?.method === 'PUT'
    ? Promise.resolve(fail && url.includes('/two/') ? json({ error: 'batch save rejected' }, 409) : json(report(decodeURIComponent(url.match(/\/credentials\/([^/]+)\//)![1])))) : undefined);
  fireEvent.click(screen.getByRole('button', { name: '自动预热配置' }));
  fireEvent.change(screen.getByRole('combobox', { name: '预热模型' }), { target: { value: 'model-a' } });
  fireEvent.click(screen.getByRole('button', { name: '关闭' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('操作未完成：batch save rejected');
  expect(screen.getByRole('combobox', { name: '预热模型' })).toHaveProperty('value', 'model-a');
  fail = false;
  fireEvent.click(screen.getByRole('button', { name: '关闭' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(0);
});

it('prevents disabling every enabled credential of an enabled channel before any request', () => {
  const source = channel(); setup(source, undefined, source.credentials);
  fireEvent.click(screen.getByRole('button', { name: '停用' }));
  expect(screen.getByText(/请先停用渠道/)).toBeTruthy(); expect(fetch).not.toHaveBeenCalled();
});

it('chains atomic enablement generations, encodes IDs, and stops following writes after a conflict', async () => {
  const source = channel(); source.credentials!.push(credential('four'));
  const view = setup(source, (url, init) => init?.method === 'PUT' && url.endsWith('/enabled') ? Promise.resolve(url.includes('/two/') ? json({ error: '渠道已变化' }, 409) : json({ ok: true, generation: 5 })) : undefined, source.credentials!.slice(0, 3));
  fireEvent.click(screen.getByRole('button', { name: '停用' }));
  await screen.findByText('操作未完成：渠道已变化');
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
  expect(writes.map(([url, init]) => [url, JSON.parse(String(init!.body))])).toEqual([
    ['/api/channels/7/credentials/one%2F%20%3F/enabled', { enabled: false, expected_generation: 4 }],
    ['/api/channels/7/credentials/two/enabled', { enabled: false, expected_generation: 5 }],
  ]);
  expect(view.fetcher.mock.calls.some(([url]) => url.includes('/operations'))).toBe(false);
  expect(screen.getByText('完成')).toBeTruthy(); expect(screen.getByText('失败')).toBeTruthy();
  expect(screen.getByText(/停止后续写入/)).toBeTruthy();
  expect(view.props.onGeneration.mock.calls).toEqual([[5]]);
});

it('rejects selected targets after the account identity or channel generation changes', async () => {
  const view = setup();
  const changed = { ...view.props.channel, generation: 8 };
  view.rerender(<OAuthCredentialBatch {...view.props} channel={changed} />);
  fireEvent.click(screen.getByRole('button', { name: '启用' }));
  await screen.findAllByText(/渠道或凭证已变化/);
  expect(view.fetcher).not.toHaveBeenCalled();
});

it('requires explicit warmup confirmation and skips missing models without guessing', async () => {
  const source = channel(); source.credentials![1].operations!.warmup.model = '';
  const view = setup(source, (url, init) => {
    if (url.endsWith('/two/operations') && !init?.method) return Promise.resolve(json({ ...report('two'), config: { ...config, warmup: { ...config.warmup, model: '' } } }));
    if (url.endsWith('/warmup')) { const body = JSON.parse(String(init!.body)); return Promise.resolve(json({ ...report('one/ ?'), last_warmup: { request_id: body.request_id, state: 'succeeded', detail: '已发送hi', at: '2026-10-18' } })); }
  });
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  expect(view.fetcher).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findByText('已发送hi'); await screen.findByText(/未配置有效预热模型，请先/);
  const posts = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST');
  expect(posts).toHaveLength(1);
  expect(JSON.parse(String(posts[0][1]!.body))).toEqual({ expected_identity: 'identity:one/ ?', request_id: expect.stringMatching(/^[0-9a-f-]{36}$/) });
});

it('keeps an unresolved warmup UUID after a lost response and never sends a replacement request', async () => {
  const view = setup(channel(), (url, init) => url.endsWith('/warmup') && init?.method === 'POST' ? Promise.reject(new Error('network lost')) : undefined);
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findAllByText(/结果未确认；请到单条预热入口/);
  const posts = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST');
  expect(posts).toHaveLength(2);
  for (const [url, init] of posts) {
    const body = JSON.parse(String(init!.body));
    const base = url.slice('/api'.length, -'/warmup'.length);
    expect(JSON.parse(sessionStorage.getItem(`to-router.credential-operation:${base}`)!)).toEqual({ identity: body.expected_identity, kind: 'warmup', request_id: body.request_id });
  }
  expect(screen.getByRole('button', { name: '确认各发送一次 hi' })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findAllByText(/已有操作结果未确认/);
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(2);
});

it('applies only the selected automatic branch and retries failed items without erasing success or the draft', async () => {
  let twoFails = true;
  const view = setup(channel(), (url, init) => {
    if (init?.method === 'PUT' && url.endsWith('/operations')) {
      if (url.includes('/two/') && twoFails) return Promise.resolve(json({ error: 'save failed' }, 500));
      const id = decodeURIComponent(url.match(/\/credentials\/([^/]+)\//)![1]);
      return Promise.resolve(json(report(id)));
    }
  });
  fireEvent.click(screen.getByRole('button', { name: '自动预热配置' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '自动预热' }));
  fireEvent.change(screen.getByRole('combobox', { name: '预热模型' }), { target: { value: 'model-a' } });
  fireEvent.click(screen.getByRole('button', { name: '应用到选中凭证' }));
  await screen.findByText('操作未完成：save failed');
  expect(screen.getByRole('combobox', { name: '预热模型' })).toHaveProperty('value', 'model-a');
  expect(screen.getByText('完成')).toBeTruthy();
  twoFails = false;
  fireEvent.click(screen.getByRole('button', { name: '重试未完成项' }));
  await waitFor(() => expect(screen.getAllByText('完成')).toHaveLength(2));
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
  expect(writes).toHaveLength(3);
  expect(writes.map(([url]) => url.includes('/two/'))).toEqual([false, true, true]);
  for (const [, init] of writes) expect(JSON.parse(String(init!.body))).toEqual({ expected_identity: expect.any(String), warmup: { enabled: true, mode: 'reset', model: 'model-a', daily_times: [] } });
});

it('skips automatic warmup for unknown or weekly-only quotas while applying qualified targets', async () => {
  const view = setup(channel(), (_url, init) => init?.method === 'PUT' ? Promise.resolve(json(report('one/ ?'))) : undefined);
  const mixed = { ...view.props.observations, [quotaTargetKey(view.props.items[1].target)]: { snapshot: { ok: true, buckets: [{ id: 'general', general: true, primary: { window_seconds: 604800 } }] } } };
  view.rerender(<OAuthCredentialBatch {...view.props} observations={mixed} />);
  fireEvent.click(screen.getByRole('button', { name: '自动预热配置' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '自动预热' }));
  fireEvent.change(screen.getByRole('combobox', { name: '预热模型' }), { target: { value: 'model-a' } });
  fireEvent.click(screen.getByRole('button', { name: '应用到选中凭证' }));
  await screen.findByText(/上游未明确提供5小时通用额度，跳过/);
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
});

it('refreshes exact selected quota targets and reports each observation without clearing old values', async () => {
  const view = setup();
  fireEvent.click(screen.getByRole('button', { name: '刷新额度' }));
  expect(view.props.refresh.mock.calls.map(([target]) => target.credentialID)).toEqual(['one/ ?', 'two']);
  const pending = Object.fromEntries(Object.entries(view.props.observations).map(([key, value]) => [key, { ...value, pending: true }]));
  await act(async () => view.rerender(<OAuthCredentialBatch {...view.props} observations={pending} />));
  const finished = Object.fromEntries(Object.entries(pending).map(([key, value], index) => [key, { ...value, pending: false, error: index === 1 ? 'quota failed' : undefined }]));
  await act(async () => view.rerender(<OAuthCredentialBatch {...view.props} observations={finished} />));
  expect(screen.getByText('额度已刷新')).toBeTruthy(); expect(screen.getByText('刷新失败，保留旧额度：quota failed')).toBeTruthy();
  expect(view.fetcher).not.toHaveBeenCalled();
});
it('saves automatic reset independently from warmup configuration', async () => {
  const view = setup(channel(), (url, init) => init?.method === 'PUT' ? Promise.resolve(json(report(decodeURIComponent(url.match(/\/credentials\/([^/]+)\//)![1])))) : undefined);
  fireEvent.click(screen.getByRole('button', { name: '自动重置配置' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '自动重置' }));
  fireEvent.click(screen.getByRole('button', { name: '添加规则' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '规则 1 到期条件' }));
  fireEvent.change(screen.getByRole('spinbutton', { name: '规则 1 卡到期天数' }), { target: { value: '2' } });
  fireEvent.click(screen.getByRole('button', { name: '应用到选中凭证' }));
  await waitFor(() => expect(screen.getAllByText('完成')).toHaveLength(2));
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
  for (const [, init] of writes) expect(JSON.parse(String(init!.body))).toEqual({ expected_identity: expect.any(String), auto_reset: { enabled: true, rules: [{ window_seconds: 18000, remaining_below: null, expires_within_days: 2, join: 'and' }] } });
});

it('does not write a replacement account when preflight responses arrive after the selection became stale', async () => {
  const finishes: ((response: Response) => void)[] = [];
  const view = setup(channel(), (url, init) => url.endsWith('/operations') && !init?.method ? new Promise(resolve => finishes.push(resolve)) : undefined);
  fireEvent.click(screen.getByRole('button', { name: '自动重置配置' }));
  fireEvent.click(screen.getByRole('button', { name: '应用到选中凭证' }));
  expect(finishes).toHaveLength(2);
  const changed = { ...view.props.channel, credentials: view.props.channel.credentials!.map(credential => ({ ...credential, oauth_info: { ...credential.oauth_info!, updated_at: 'replacement' } })) };
  view.rerender(<OAuthCredentialBatch {...view.props} channel={changed} />);
  await act(async () => { finishes[0](json(report('one/ ?'))); finishes[1](json(report('two'))); });
  expect(screen.getAllByText(/渠道或凭证已变化/)).toHaveLength(2);
  expect(view.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
});
it.each(['identity', 'generation'] as const)('finishes quota refresh when the selected %s changes, even without an old observation', async kind => {
  const view = setup();
  fireEvent.click(screen.getByRole('button', { name: '刷新额度' }));
  expect(view.props.onBusy).toHaveBeenLastCalledWith(true);
  const changed = kind === 'generation' ? { ...view.props.channel, generation: 9 } : { ...view.props.channel, credentials: view.props.channel.credentials!.map(value => ({ ...value, oauth_info: { ...value.oauth_info!, updated_at: 'replacement' } })) };
  // A generation-only change must also wake the effect when observations retain their reference.
  view.rerender(<OAuthCredentialBatch {...view.props} channel={changed} observations={kind === 'identity' ? {} : view.props.observations} />);
  await waitFor(() => expect(view.props.onBusy).toHaveBeenLastCalledWith(false));
  expect(screen.getAllByText(/渠道或凭证已变化/)).toHaveLength(2);
  expect(screen.getByRole('button', { name: '刷新额度' })).toHaveProperty('disabled', false);
});
it('reports an explicit warmup 400 as failed, clears its claim, and allows a later confirmed intent', async () => {
  let rejected = true;
  const view = setup(channel(), (url, init) => {
    if (!url.endsWith('/warmup') || init?.method !== 'POST') return;
    if (rejected) return Promise.resolve(json({ error: '渠道或凭证已停用' }, 400));
    const id = decodeURIComponent(url.match(/\/credentials\/([^/]+)\//)![1]), body = JSON.parse(String(init.body));
    return Promise.resolve(json({ ...report(id), last_warmup: { request_id: body.request_id, state: 'succeeded', detail: '恢复后已预热', at: '2026-10-18' } }));
  });
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findAllByText('操作未完成：渠道或凭证已停用');
  expect(sessionStorage.length).toBe(0);
  expect(view.fetcher.mock.calls.filter(([, init]) => !init?.method)).toHaveLength(2);
  rejected = false;
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await waitFor(() => expect(screen.getAllByText('恢复后已预热')).toHaveLength(2));
  const posts = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST');
  expect(posts).toHaveLength(4);
  expect(JSON.parse(String(posts[0][1]!.body)).request_id).not.toBe(JSON.parse(String(posts[2][1]!.body)).request_id);
});

it.each([404, 409])('stops the remaining selected warmups after an explicit %i rejection without erasing a success', async status => {
  const source = channel();
  const view = setup(source, (url, init) => {
    if (!url.endsWith('/warmup') || init?.method !== 'POST') return;
    if (url.includes('/two/')) return Promise.resolve(json({ error: '目标已变化' }, status));
    const id = decodeURIComponent(url.match(/\/credentials\/([^/]+)\//)![1]), body = JSON.parse(String(init.body));
    return Promise.resolve(json({ ...report(id), last_warmup: { request_id: body.request_id, state: 'succeeded', detail: '已完成第一项', at: '2026-10-18' } }));
  }, source.credentials);
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findByText('操作未完成：目标已变化');
  expect(screen.getByText('已完成第一项')).toBeTruthy();
  expect(screen.getByText(/停止后续写入/)).toBeTruthy();
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(2);
  expect(view.fetcher.mock.calls.filter(([, init]) => !init?.method)).toHaveLength(status === 409 ? 4 : 3);
  expect(sessionStorage.length).toBe(0);
});

it('keeps a 5xx warmup unresolved and only reads receipts before allowing any later operation', async () => {
  const view = setup(channel(), (url, init) => url.endsWith('/warmup') && init?.method === 'POST' ? Promise.resolve(json({ error: 'internal error' }, 500)) : undefined);
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await waitFor(() => expect(screen.getAllByText(/结果未确认；请到单条预热入口/)).toHaveLength(2));
  expect(view.fetcher.mock.calls.filter(([, init]) => !init?.method)).toHaveLength(4);
  expect(sessionStorage.length).toBe(2);
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await waitFor(() => expect(screen.getAllByText(/已有操作结果未确认/)).toHaveLength(2));
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(2);
});
it.each(['running', 'unknown'] as const)('keeps the original UUID when a 409 reconciles to a matching %s receipt', async state => {
  const source = channel();
  let unknownRequest = '';
  const view = setup(source, (url, init) => {
    if (url.endsWith('/warmup') && init?.method === 'POST') {
      unknownRequest = JSON.parse(String(init.body)).request_id;
      return Promise.resolve(json({ error: '此凭证还有操作进行中或结果待核对' }, 409));
    }
    if (url.endsWith('/operations') && !init?.method && unknownRequest) return Promise.resolve(json({ ...report('one/ ?'), last_warmup: { request_id: unknownRequest, state, detail: '结果待确认', at: '2026-10-18' } }));
  }, source.credentials!.slice(0, 1));
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findByText(/结果未确认；请到单条预热入口/);
  expect(sessionStorage.length).toBe(1);
  expect(JSON.parse(sessionStorage.getItem('to-router.credential-operation:/channels/7/credentials/one%2F%20%3F/operations')!).request_id).toBe(unknownRequest);
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findByText(/已有操作结果未确认/);
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
});
it('retains the same pending UUID when reading the receipt after a 409 fails', async () => {
  const source = channel();
  let requestID = '', readFails = true;
  const view = setup(source, (url, init) => {
    if (url.endsWith('/warmup') && init?.method === 'POST') {
      requestID = JSON.parse(String(init.body)).request_id;
      return Promise.resolve(json({ error: '操作冲突' }, 409));
    }
    if (url.endsWith('/operations') && !init?.method && requestID && readFails) return Promise.reject(new Error('receipt unavailable'));
  }, source.credentials!.slice(0, 1));
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findByText(/结果未确认；请到单条预热入口/);
  const storage = 'to-router.credential-operation:/channels/7/credentials/one%2F%20%3F/operations';
  expect(JSON.parse(sessionStorage.getItem(storage)!)).toEqual({ identity: 'identity:one/ ?', kind: 'warmup', request_id: requestID });
  expect(view.fetcher.mock.calls.filter(([, init]) => !init?.method)).toHaveLength(2);
  readFails = false;
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findByText(/已有操作结果未确认/);
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
  expect(JSON.parse(sessionStorage.getItem(storage)!).request_id).toBe(requestID);
});

it('adopts a matching successful receipt after a 409 and stops the remaining stale selection', async () => {
  let requestID = '';
  const view = setup(channel(), (url, init) => {
    if (url.endsWith('/warmup') && init?.method === 'POST') {
      requestID = JSON.parse(String(init.body)).request_id;
      return Promise.resolve(json({ error: '操作冲突' }, 409));
    }
    if (url.includes('/one%2F%20%3F/operations') && !init?.method && requestID) return Promise.resolve(json({ ...report('one/ ?'), last_warmup: { request_id: requestID, state: 'succeeded', detail: '已核实本次预热完成', at: '2026-10-18' } }));
  });
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  await screen.findByText('已核实本次预热完成');
  expect(screen.getByText(/停止后续写入/)).toBeTruthy();
  expect(sessionStorage.length).toBe(0);
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
});
it('keeps batch results in selection order while each warmup is still awaiting its response', async () => {
  const source = channel();
  const pending: { id: string; requestID: string; finish: (value: Response) => void }[] = [];
  setup(source, (url, init) => {
    if (url.endsWith('/warmup') && init?.method === 'POST') return new Promise(resolve => pending.push({ id: decodeURIComponent(url.match(/\/credentials\/([^/]+)\//)![1]), requestID: JSON.parse(String(init.body)).request_id, finish: resolve }));
  }, source.credentials);
  fireEvent.click(screen.getByRole('button', { name: '预热' }));
  fireEvent.click(screen.getByRole('button', { name: '确认各发送一次 hi' }));
  const names = () => Array.from(screen.getByRole('list', { name: '批量操作结果' }).querySelectorAll('li > strong')).map(value => value.textContent);
  for (let index = 0; index < 3; index++) {
    await waitFor(() => expect(pending).toHaveLength(index + 1));
    expect(names()).toEqual(['one/ ?', 'two', 'three']);
    const current = pending[index];
    await act(async () => current.finish(json({ ...report(current.id), last_warmup: { request_id: current.requestID, state: 'succeeded', detail: '已完成', at: '2026-10-18' } })));
  }
  expect(names()).toEqual(['one/ ?', 'two', 'three']);
});
