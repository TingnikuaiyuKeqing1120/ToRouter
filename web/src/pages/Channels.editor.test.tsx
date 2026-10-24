// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import type { Channel } from '../api';
import Channels from './Channels';
import { enabledPlugins } from '../testFixtures/plugins';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const channel: Channel = {
  id: 12, name: 'Personal', api_key: '', has_key: true, credential_type: 'api_key', enabled: true,
  base_url: 'https://default.example/v1', endpoints: [{ format: 'openai_chat_completions', base_url: '' }],
  models: ['model-a'], available_models: ['model-a', 'model-b'],
  credentials: [
    { id: 'primary', label: 'Primary', api_key: '', has_key: true, enabled: true },
    { id: 'backup', label: 'Backup', api_key: '', has_key: true, enabled: true },
  ],
};
const tagGroups = [{ id: 'team', name: '分组', type: 'custom', tags: [{ id: 'team-red', group_id: 'team', name: '团队标签', color: 'rose' }] }];
type Write = { url: string; method: string; body: Record<string, unknown> };
function setup(row = channel, save: (write: Write) => Promise<Response> = () => Promise.resolve(json(row)), tags = () => Promise.resolve(json(tagGroups))) {
  const writes: Write[] = [];
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/plugins' && !init?.method) return Promise.resolve(json(enabledPlugins));
    if (init?.method && init.method !== 'GET') {
      const write = { url, method: init.method, body: JSON.parse(String(init.body)) };
      writes.push(write);
      return save(write);
    }
    if (url === '/api/settings') return Promise.resolve(json({ oauth_auto_open: false, request_defaults_enabled: false, request_defaults: {} }));
    if (url === '/api/settings/quota-refresh') return Promise.resolve(json({ quota_refresh_seconds: 0 }));
    if (url.startsWith('/api/oauth/codex/usage')) return Promise.resolve(json({ ok: true, buckets: [] }));
    if (url === '/api/channels/display') return Promise.resolve(json({ sort_mode: 'name', order: [], icons: {} }));
    if (url.startsWith('/api/channels?credential_type=')) return Promise.resolve(json(url.endsWith(row.credential_type || 'api_key') ? [row] : []));
    if (url === '/api/tag-groups') return tags();
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  render(<Channels />, { wrapper: ConfirmProvider });
  return { fetcher, writes };
}
async function edit(row = channel) {
  if (row.credential_type !== 'api_key') {
    fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    fireEvent.click(await screen.findByRole('tab', { name: 'Codex' }));
  }
  const button = await screen.findByRole('button', { name: `编辑渠道名称 ${row.name}` });
  await waitFor(() => expect(button).toHaveProperty('disabled', false));
  fireEvent.click(button);
  return screen.getByRole('dialog', { name: '编辑渠道' });
}
async function section(dialog: HTMLElement, name: '快捷配置' | '模型配置' | '渠道标签' | '更多配置') {
  fireEvent.click(within(dialog).getByRole('button', { name }));
  if (name === '模型配置') await within(dialog).findByRole('region', { name: '已启用模型' });
}
function change(dialog: HTMLElement, label: string, value: string) {
  fireEvent.change(within(dialog).getByLabelText(label), { target: { value } });
}
function endpointDetails(dialog: HTMLElement) {
  const input = within(dialog).getByLabelText('openai_chat_completions 自定义地址');
  const details = input.closest('details')!;
  if (!details.open) fireEvent.click(details.querySelector('summary')!);
  return { input, details };
}

beforeEach(() => {
  sessionStorage.clear(); localStorage.clear(); history.replaceState(null, '', '/');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('channel editor sections and unified save', () => {
  it('prepares the model page after showing its frame, keeps it mounted, and saves the independent tag page draft', async () => {
    const { writes, fetcher } = setup();
    const dialog = await edit();
    expect(fetcher.mock.calls.some(([url]) => url === '/api/tag-groups')).toBe(false);
    expect(within(within(dialog).getByRole('navigation', { name: '渠道配置' })).getAllByRole('button').map(button => button.textContent)).toEqual(['快捷配置', '模型配置', '渠道标签', '更多配置']);
    fireEvent.click(within(dialog).getByRole('button', { name: '模型配置' }));
    expect(within(dialog).getByRole('button', { name: '模型配置' }).getAttribute('aria-current')).toBe('page');
    expect(within(dialog).getByText('正在准备模型列表…')).toBeTruthy();
    const models = await within(dialog).findByRole('region', { name: '已启用模型' });
    const search = within(dialog).getByLabelText('搜索模型');
    fireEvent.change(search, { target: { value: 'model-b' } });
    fireEvent.click(within(dialog).getByRole('checkbox', { name: '启用模型 model-b' }));
    await section(dialog, '渠道标签');
    fireEvent.click(await within(dialog).findByRole('button', { name: '团队标签' }));
    await section(dialog, '更多配置');
    expect(within(within(dialog).getByRole('region', { name: '更多配置' })).queryByText('选择标签')).toBeNull();
    await section(dialog, '模型配置');
    expect(within(dialog).getByRole('region', { name: '已启用模型' })).toBe(models);
    expect(within(dialog).getByLabelText('搜索模型')).toBe(search);
    expect(search).toHaveProperty('value', 'model-b');
    expect(within(dialog).queryByText('正在准备模型列表…')).toBeNull();
    expect(within(dialog).getByRole('checkbox', { name: '启用模型 model-b' })).toHaveProperty('checked', true);
    await section(dialog, '渠道标签');
    expect(within(dialog).getByRole('button', { name: '团队标签' }).getAttribute('aria-pressed')).toBe('true');
    expect(fetcher.mock.calls.filter(([url]) => url === '/api/tag-groups')).toHaveLength(1);
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    expect(writes).toHaveLength(1);
    expect(writes[0].body).toMatchObject({ models: ['model-a', 'model-b'], tags: ['team-red'] });
  });

  it('retains tag-page load failure across switches and fetches again only on an explicit retry', async () => {
    let attempts = 0;
    const { writes, fetcher } = setup(channel, undefined, () => Promise.resolve(++attempts === 1 ? json({ error: 'temporary tag failure' }, 503) : json(tagGroups)));
    const dialog = await edit();
    await section(dialog, '渠道标签');
    expect((await within(dialog).findByRole('alert')).textContent).toContain('标签加载失败');
    await section(dialog, '快捷配置');
    await section(dialog, '渠道标签');
    expect(within(dialog).getByRole('alert').textContent).toContain('标签加载失败');
    expect(fetcher.mock.calls.filter(([url]) => url === '/api/tag-groups')).toHaveLength(1);
    fireEvent.click(within(dialog).getByRole('button', { name: '重试' }));
    fireEvent.click(await within(dialog).findByRole('button', { name: '团队标签' }));
    await section(dialog, '快捷配置');
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    expect(fetcher.mock.calls.filter(([url]) => url === '/api/tag-groups')).toHaveLength(2);
    expect(writes).toHaveLength(1); expect(writes[0].body.tags).toEqual(['team-red']);
  });

  it('offers single-credential guidance and opens the call strategy when another credential is added', async () => {
    const { writes } = setup({ ...channel, credentials: [channel.credentials![0]] });
    const dialog = await edit();
    expect(within(dialog).getByText('添加新的凭证后，可启用故障转移或轮询，并调整调用顺序。')).toBeTruthy();
    expect(within(dialog).queryByRole('group', { name: '调度方式' })).toBeNull();
    fireEvent.click(within(dialog).getByRole('button', { name: '添加凭证' }));
    expect(within(dialog).getByText('调用策略').closest('details')).toHaveProperty('open', true);
    expect(within(dialog).getByRole('button', { name: '故障转移' }).getAttribute('aria-pressed')).toBe('true');
    expect(within(dialog).getByRole('button', { name: '单向链' }).getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(within(dialog).getByRole('button', { name: '按请求轮询' }));
    fireEvent.click(within(dialog).getByRole('button', { name: '循环链' }));
    expect(within(dialog).getByRole('button', { name: '故障转移' }).getAttribute('aria-pressed')).toBe('false');
    expect(within(dialog).getByRole('button', { name: '按请求轮询' }).getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    expect(writes[0].body).toMatchObject({ dispatch_mode: 'round_robin', chain_type: 'cyclic', credentials: [expect.objectContaining({ id: 'primary' }), expect.objectContaining({ enabled: true })] });
  });

  it('waits for a confirmed save to finish before closing the editor', async () => {
    let resolve!: (value: Response) => void;
    const pending = new Promise<Response>(done => { resolve = done; });
    const { writes } = setup(channel, () => pending);
    const dialog = await edit();
    change(dialog, '渠道名称', 'Confirmed draft');
    fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
    const prompt = await screen.findByRole('dialog', { name: '保存更改？' });
    fireEvent.click(within(prompt).getByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(screen.getByRole('dialog', { name: '编辑渠道' })).toBe(dialog);
    expect(within(dialog).getByRole('button', { name: '关闭弹窗' })).toHaveProperty('disabled', true);
    expect(writes[0]).toMatchObject({ url: '/api/channels/12', method: 'PUT', body: { name: 'Confirmed draft' } });
    await act(async () => { resolve(json(channel)); await pending; });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    expect(writes).toHaveLength(1);
  });

  it('explains why confirmed saving must wait for discovery and saves the retained draft after discovery finishes', async () => {
    let resolveDiscovery!: (value: Response) => void;
    const discovery = new Promise<Response>(done => { resolveDiscovery = done; });
    const { writes } = setup(channel, write => write.url === '/api/channels/discover' ? discovery : Promise.resolve(json(channel)));
    const dialog = await edit();
    change(dialog, '渠道名称', 'Draft during discovery');
    await section(dialog, '模型配置');
    fireEvent.click(within(dialog).getByRole('button', { name: '从上游获取模型' }));
    await waitFor(() => expect(writes.filter(write => write.url === '/api/channels/discover')).toHaveLength(1));
    fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '确认保存' }));
    expect((await within(dialog).findByRole('alert')).textContent).toContain('正在获取模型，请完成后再保存。');
    expect(screen.getByRole('dialog', { name: '编辑渠道' })).toBe(dialog);
    expect(within(dialog).getByLabelText('渠道名称')).toHaveProperty('value', 'Draft during discovery');
    expect(writes.filter(write => write.method === 'PUT')).toEqual([]);
    await act(async () => { resolveDiscovery(json({ ok: true, models: [{ id: 'model-a' }, { id: 'fresh' }], catalogue: ['model-a', 'fresh'], duration_ms: 5 })); await discovery; });
    expect(await within(dialog).findByRole('checkbox', { name: '启用模型 fresh' })).toHaveProperty('checked', false);
    fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    const channelWrites = writes.filter(write => write.method === 'PUT');
    expect(channelWrites).toHaveLength(1);
    expect(channelWrites[0]).toMatchObject({ url: '/api/channels/12', body: { name: 'Draft during discovery', models: ['model-a'], available_models: ['model-a', 'fresh'] } });
  });

  it('retains a confirmed-save draft after a server failure and lets the user retry', async () => {
    let attempts = 0;
    const { writes } = setup(channel, () => Promise.resolve(++attempts === 1 ? json({ error: 'save failed' }, 503) : json(channel)));
    const dialog = await edit();
    change(dialog, '渠道名称', 'Retained after failure');
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '确认保存' }));
    expect((await within(dialog).findByRole('alert')).textContent).toContain('save failed');
    expect(within(dialog).getByLabelText('渠道名称')).toHaveProperty('value', 'Retained after failure');
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    expect(writes).toHaveLength(2); expect(writes[1]).toEqual(writes[0]);
  });

  it('keeps a confirmed-save draft open and locates invalid input without sending a request', async () => {
    const { writes } = setup();
    const dialog = await edit();
    change(dialog, '渠道 Base URL', 'https://invalid.example?secret=synthetic');
    await section(dialog, '更多配置');
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '确认保存' }));
    expect((await within(dialog).findByRole('alert')).textContent).toContain('有效的 HTTP(S) 地址');
    await waitFor(() => expect(document.activeElement).toBe(within(dialog).getByLabelText('渠道 Base URL')));
    expect(within(dialog).getByLabelText('渠道 Base URL')).toHaveProperty('value', 'https://invalid.example?secret=synthetic');
    expect(writes).toEqual([]);
  });

  it('creates an API channel with the combined draft from all sections', async () => {
    const { writes } = setup();
    await screen.findByRole('button', { name: `编辑 ${channel.name}` });
    fireEvent.click(screen.getByRole('button', { name: '添加渠道' }));
    const dialog = screen.getByRole('dialog', { name: '添加渠道' });
    change(dialog, '渠道名称', '  New gateway  ');
    change(dialog, '渠道 Base URL', 'https://new.example/v1');
    change(dialog, 'API 密钥', '  synthetic-new-key  ');
    fireEvent.click(within(dialog).getByRole('checkbox', { name: '启用格式 anthropic_messages' }));
    change(dialog, '偏好协议', 'anthropic_messages');
    await section(dialog, '模型配置');
    change(dialog, '手动模型 ID', 'manual-model');
    fireEvent.click(within(dialog).getByRole('button', { name: '手动添加' }));
    fireEvent.click(within(dialog).getByRole('checkbox', { name: '启用模型 manual-model' }));
    await section(dialog, '更多配置');
    const { input } = endpointDetails(dialog);
    fireEvent.change(input, { target: { value: 'https://chat.example/v1/' } });
    change(dialog, 'openai_chat_completions 服务端状态', 'stateless');
    await section(dialog, '快捷配置');
    expect(within(dialog).getByLabelText('API 密钥')).toHaveProperty('value', '  synthetic-new-key  ');
    expect(within(dialog).getByLabelText('偏好协议')).toHaveProperty('value', 'anthropic_messages');
    expect(writes).toEqual([]);
    fireEvent.click(within(dialog).getByRole('button', { name: '创建渠道' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '添加渠道' })).toBeNull());
    expect(writes).toEqual([{ url: '/api/channels', method: 'POST', body: expect.objectContaining({
      name: 'New gateway', enabled: true, credential_type: 'api_key', base_url: 'https://new.example/v1', api_key: '',
      endpoints: [{ format: 'openai_chat_completions', base_url: 'https://chat.example/v1', state: 'stateless' }, { format: 'anthropic_messages', base_url: '', state: '' }],
      preferred_endpoint: 'anthropic_messages', models: ['manual-model'], available_models: ['manual-model'], manual_models: ['manual-model'],
      credentials: [{ id: 'cred_1', label: '主凭据', api_key: 'synthetic-new-key', enabled: true }],
      dispatch_mode: 'failover', chain_type: 'unidirectional',
    }) }]);
    for (const field of ['has_key', 'has_oauth', 'model_configs', 'associated_prices']) expect(writes[0].body).not.toHaveProperty(field);
  });

  it('keeps all section drafts after a save failure and sends them unchanged on retry', async () => {
    let attempts = 0;
    const { writes } = setup(channel, () => Promise.resolve(++attempts === 1 ? json({ error: 'save unavailable' }, 503) : json(channel)));
    const dialog = await edit();
    change(dialog, '渠道名称', 'Draft Personal');
    await section(dialog, '模型配置');
    change(dialog, '搜索模型', 'model-b');
    fireEvent.click(within(dialog).getByRole('checkbox', { name: '启用模型 model-b' }));
    change(dialog, '手动模型 ID', 'not-added-yet');
    await section(dialog, '更多配置');
    endpointDetails(dialog);
    change(dialog, 'openai_chat_completions 服务端状态', 'stateless');
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    expect((await within(dialog).findByRole('alert')).textContent).toContain('save unavailable');
    expect(within(dialog).getByLabelText('渠道名称')).toHaveProperty('value', 'Draft Personal');
    expect(within(dialog).getByLabelText('openai_chat_completions 服务端状态')).toHaveProperty('value', 'stateless');
    await section(dialog, '模型配置');
    expect(within(dialog).getByLabelText('搜索模型')).toHaveProperty('value', 'model-b');
    expect(within(dialog).getByLabelText('手动模型 ID')).toHaveProperty('value', 'not-added-yet');
    expect(within(dialog).getByRole('checkbox', { name: '启用模型 model-b' })).toHaveProperty('checked', true);
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    expect(writes).toHaveLength(2);
    expect(writes[1]).toEqual(writes[0]);
    expect(writes[1].body).toMatchObject({ name: 'Draft Personal', models: ['model-a', 'model-b'] });
  });

  it.each(['取消', '关闭弹窗'])('confirms %s for a draft changed on another section and discards only after acceptance', async close => {
    const { writes } = setup();
    const dialog = await edit();
    await section(dialog, '模型配置');
    fireEvent.click(within(dialog).getByRole('checkbox', { name: '启用模型 model-b' }));
    await section(dialog, '更多配置');
    fireEvent.click(within(dialog).getByRole('button', { name: close }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
    await section(dialog, '模型配置');
    expect(within(dialog).getByRole('checkbox', { name: '启用模型 model-b' })).toHaveProperty('checked', true);
    fireEvent.click(within(dialog).getByRole('button', { name: close }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '放弃更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    const reopened = await edit();
    await section(reopened, '模型配置');
    expect(within(reopened).getByRole('checkbox', { name: '启用模型 model-b' })).toHaveProperty('checked', false);
    expect(writes).toEqual([]);
  });

  it('reveals and focuses an invalid custom address after its section and disclosure were hidden', async () => {
    const { writes } = setup();
    const dialog = await edit();
    await section(dialog, '更多配置');
    const { input, details } = endpointDetails(dialog);
    fireEvent.change(input, { target: { value: 'https://chat.example/v1?key=synthetic' } });
    fireEvent.click(details.querySelector('summary')!);
    expect(details.open).toBe(false);
    await section(dialog, '模型配置');
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    expect((await within(dialog).findByRole('alert')).textContent).toContain('有效的 HTTP(S) 地址');
    await waitFor(() => expect(document.activeElement).toBe(input));
    expect(details.open).toBe(true);
    expect(within(dialog).getByRole('button', { name: '更多配置' }).getAttribute('aria-current')).toBe('page');
    expect(writes).toEqual([]);
  });

  it.each([false, true])('returns to an invalid default address even with a custom endpoint: %s', async customEndpoint => {
    const { writes } = setup();
    const dialog = await edit();
    if (customEndpoint) {
      await section(dialog, '更多配置');
      fireEvent.change(endpointDetails(dialog).input, { target: { value: 'https://custom.example/v1' } });
      await section(dialog, '快捷配置');
    }
    change(dialog, '渠道 Base URL', 'ftp://invalid.example');
    await section(dialog, '模型配置');
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(document.activeElement).toBe(within(dialog).getByLabelText('渠道 Base URL')));
    expect(within(dialog).getByRole('button', { name: '快捷配置' }).getAttribute('aria-current')).toBe('page');
    expect(writes).toEqual([]);
  });

  it('preserves credential IDs and blank keys while saving reordered and disabled credentials from another section', async () => {
    const { writes } = setup();
    const dialog = await edit();
    fireEvent.click(within(dialog).getByRole('button', { name: '上移凭据 2' }));
    fireEvent.click(within(dialog).getByRole('checkbox', { name: '启用凭据 1' }));
    expect(within(dialog).getByLabelText('凭据 1 备注')).toHaveProperty('value', 'Backup');
    expect(within(dialog).getByLabelText('API 密钥')).toHaveProperty('value', '');
    const strategy = within(dialog).getByText('调用策略').closest('summary')!;
    expect(strategy.closest('details')).toHaveProperty('open', true);
    fireEvent.click(within(dialog).getByRole('button', { name: '按请求轮询' }));
    fireEvent.click(within(dialog).getByRole('button', { name: '循环链' }));
    await section(dialog, '模型配置');
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    expect(writes).toHaveLength(1);
    expect(writes[0]).toMatchObject({ url: '/api/channels/12', method: 'PUT', body: {
      api_key: '', dispatch_mode: 'round_robin', chain_type: 'cyclic',
      credentials: [{ id: 'backup', label: 'Backup', api_key: '', enabled: false }, { id: 'primary', label: 'Primary', api_key: '', enabled: true }],
    } });
    expect(writes[0].body.credentials).toEqual([{ id: 'backup', label: 'Backup', api_key: '', enabled: false }, { id: 'primary', label: 'Primary', api_key: '', enabled: true }]);
  });

  it('saves ordinary OAuth configuration without starting authorization or sending token and credential metadata', async () => {
    const oauth = { provider: 'codex', account_label: 'Synthetic account', status: 'active' as const, expires_at: '', updated_at: '' };
    const row: Channel = { ...channel, credential_type: 'codex_oauth', has_key: false, has_oauth: true, oauth,
      base_url: 'https://chatgpt.com/backend-api/codex', endpoints: [{ format: 'openai_responses', base_url: '' }],
      credentials: [{ id: 'oauth-one', label: 'Account', enabled: true, has_oauth: true, oauth_info: oauth }],
    };
    const { writes, fetcher } = setup(row);
    const dialog = await edit(row);
    expect(within(dialog).queryByText(oauth.account_label)).toBeNull();
    expect(within(dialog).getByRole('button', { name: '重新授权此账号' })).toBeTruthy();
    change(dialog, '渠道名称', 'Renamed OAuth');
    await section(dialog, '模型配置');
    fireEvent.click(within(dialog).getByRole('checkbox', { name: '启用模型 model-b' }));
    await section(dialog, '更多配置');
    await section(dialog, '快捷配置');
    expect(within(dialog).getByRole('button', { name: '重新授权此账号' })).toBeTruthy();
    expect(writes).toEqual([]);
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull());
    expect(writes).toHaveLength(1);
    expect(writes[0]).toMatchObject({ url: '/api/channels/12', method: 'PUT', body: { name: 'Renamed OAuth', credential_type: 'codex_oauth', models: ['model-a', 'model-b'] } });
    expect(writes[0].body.credentials).toEqual([{ id: 'oauth-one', label: 'Account', api_key: '', enabled: true }]);
    for (const field of ['access_token', 'refresh_token', 'id_token', 'oauth_info', 'has_oauth']) expect(JSON.stringify(writes[0].body)).not.toContain(`"${field}"`);
    expect(fetcher.mock.calls.some(([url]) => /authorize|\/oauth\/.+\/(start|complete|refresh)/.test(url))).toBe(false);
  });
});
