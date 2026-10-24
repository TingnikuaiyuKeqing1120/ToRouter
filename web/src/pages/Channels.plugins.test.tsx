// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Channels, { getChannelModelFormats } from './Channels';
import { ConfirmProvider } from '../components/Confirm';
import type { Channel } from '../api';
import type { PluginList } from '../types/plugins';
import { enabledPlugins } from '../testFixtures/plugins';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const account: Channel = { id: 7, name: 'Saved Codex', enabled: true, credential_type: 'codex_oauth', api_key: '', has_key: false, has_oauth: true, models: ['model-a'], available_models: ['model-a'], endpoints: [], credentials: [{ id: 'saved', label: 'Saved credential', enabled: true, has_oauth: true, oauth_info: { provider: 'codex', status: 'active', account_label: 'fixture@example.invalid', expires_at: '', updated_at: '' } }] };
const apiChannel: Channel = { id: 8, name: 'API channel', enabled: true, api_key: '', has_key: true, credential_type: 'api_key', models: ['model-a'], endpoints: [], credentials: [{ id: 'key', enabled: true, has_key: true }] };
function setup(pluginRead: () => Promise<Response> = () => Promise.resolve(json(enabledPlugins)), rows = [account]) {
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/plugins') return pluginRead();
    if (url === '/api/settings') return Promise.resolve(json({ oauth_auto_open: false }));
    if (url === '/api/settings/oauth-auto-open' && init?.method === 'PUT') return Promise.resolve(json({ ok: true }));
    if (url === '/api/settings/quota-refresh') return Promise.resolve(json({ quota_refresh_seconds: 0 }));
    if (url === '/api/channels/display') return Promise.resolve(json({ sort_mode: 'name', order: [], icons: {} }));
    if (url === '/api/quotas') return Promise.resolve(json({ items: [], refresh_seconds: 0 }));
    if (url.startsWith('/api/oauth/codex/usage') && !init?.method) return Promise.resolve(json({ ok: true, refresh_due: true, refresh_seconds: 300, next_refresh_at: new Date(Date.now() + 300_000).toISOString(), buckets: [] }));
    if (url.startsWith('/api/channels?credential_type=')) return Promise.resolve(json(rows.filter(row => row.credential_type === url.split('=')[1])));
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ConfirmProvider><Channels /></ConfirmProvider>);
  return fetcher;
}
function disabledPlugins(): PluginList {
  return { ...enabledPlugins, items: enabledPlugins.items.map(plugin => ({ ...plugin, capabilities: plugin.capabilities.map(cap => ({ ...cap, enabled: false })) })) };
}
beforeEach(() => {
  sessionStorage.clear(); localStorage.clear(); history.replaceState(null, '', '/#/channels?plugin=codex');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('channel plugin integration', () => {
  it.each(['codex', 'claude', 'copilot', 'chatgpt', 'antigravity', 'devin', 'qoder', 'qoder-cn', 'workbuddy', 'workbuddy-ai', 'minimax-code', 'minimax-code-global', 'opencode-zen-free'])('creates an enabled %s channel without authorization or model selection', async provider => {
    history.replaceState(null, '', `/#/channels?plugin=${provider}`);
    const credentialType = provider === 'opencode-zen-free' ? 'zen_free' : ['qoder', 'qoder-cn', 'workbuddy', 'workbuddy-ai', 'minimax-code', 'minimax-code-global'].includes(provider) ? `plugin:${provider}` : `${provider}_oauth`;
    const rows: Channel[] = [];
    const plugins: PluginList = { ...enabledPlugins, items: [...enabledPlugins.items, ...(credentialType.startsWith('plugin:') ? [{ id: provider, name: provider, credential_type: credentialType as Channel['credential_type'], description: 'Synthetic fixture', icon: '', implementation: 'compatible' as const, featured: true, version: 'fixture', capabilities: [{ id: 'connect' as const, enabled: true, available: true, affected_channel_ids: [] }] }] : [])] };
    const fetcher = setup(() => Promise.resolve(json(plugins)), rows), original = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => {
      if (url === '/api/channels' && init?.method === 'POST') {
        const body = JSON.parse(String(init.body));
        expect(body).toEqual({ name: 'Fresh pool', credential_type: credentialType, enabled: true });
        const created = { ...account, ...body, id: 20, has_oauth: false, has_key: false, models: [], credentials: [] };
        rows.push(created); return Promise.resolve(json(created));
      }
      return original(url, init);
    });
    await waitFor(() => expect(screen.getByRole('button', { name: '添加渠道' })).toHaveProperty('disabled', false));
    fireEvent.click(screen.getByRole('button', { name: '添加渠道' }));
    const creation = screen.getByRole('dialog', { name: '添加渠道' });
    expect(within(creation).queryByLabelText('渠道 Base URL')).toBeNull();
    fireEvent.change(within(creation).getByLabelText('渠道名称'), { target: { value: 'Fresh pool' } });
    fireEvent.click(within(creation).getByRole('button', { name: '创建渠道' }));
    const region = await screen.findByRole('region', { name: '渠道 Fresh pool' });
    expect(within(region).getByText('0 个凭证')).toBeTruthy();
    expect(within(region).getByRole('switch', { name: '停用 Fresh pool' })).toBeTruthy();
    expect(within(region).queryByRole('button', { name: '向 Fresh pool 添加账号 / 工作区' }) != null).toBe(provider !== 'opencode-zen-free');
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
    expect(fetcher.mock.calls.some(([url]) => url.includes('/start') || url.endsWith('/oauth/models'))).toBe(false);
  });

  it('loads dynamic supplier pools and appends or reauthorizes exact credentials while retaining the channel draft', async () => {
    const type = 'market:0123456789abcdef0123456789abcdef:fixture';
    const row: Channel = { ...account, name: 'Market pool', credential_type: type, generation: 1, endpoints: [{ format: 'openai_chat_completions', base_url: 'https://example.invalid/v1' }], credentials: [{ ...account.credentials![0], id: 'first' }] };
    const plugins: PluginList = { ...enabledPlugins, items: [...enabledPlugins.items, { id: type, credential_type: type, name: 'Fixture Supplier', description: 'Synthetic supplier', package: 'fixture-supplier', icon: '', implementation: 'compatible', featured: false, version: '1', auth_methods: [{ index: 4, type: 'api', label: '密钥接入' }], capabilities: [{ id: 'connect', available: true, enabled: true, affected_channel_ids: [7] }] }] };
    history.replaceState(null, '', `/#/channels?plugin=${encodeURIComponent(type)}`);
    const fetcher = setup(() => Promise.resolve(json(plugins)), [row]), original = fetcher.getMockImplementation()!;
    const startBodies: Record<string, unknown>[] = [];
    fetcher.mockImplementation((url, init) => {
      if (url === '/api/plugins/market/0123456789abcdef0123456789abcdef/auth/fixture/start') {
        const body = JSON.parse(String(init?.body)); startBodies.push(body);
        if (!body.credential_id) row.credentials!.push({ ...row.credentials![0], id: 'second' });
        row.generation = (row.generation || 0) + 1;
        return Promise.resolve(json({ status: 'success', channel_id: 7, flow_id: 'market-flow' }));
      }
      if (url === '/api/channels') return Promise.resolve(json([row]));
      return original(url, init);
    });
    await screen.findByRole('tab', { name: 'Fixture Supplier' });
    expect(screen.queryByRole('button', { name: '导入凭证' })).toBeNull();
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Market pool' }));
    const editor = screen.getByRole('dialog', { name: '编辑渠道' });
    fireEvent.change(within(editor).getByLabelText('渠道名称'), { target: { value: 'Preserved market draft' } });
    expect(within(editor).getByLabelText('渠道 Base URL')).toHaveProperty('readOnly', true);
    fireEvent.click(within(editor).getByRole('button', { name: '添加授权凭证' }));
    const adding = screen.getByRole('dialog', { name: '向「Preserved market draft」添加账号 / 工作区' });
    fireEvent.change(within(adding).getByLabelText('API Key'), { target: { value: 'synthetic-key' } });
    fireEvent.click(within(adding).getByRole('button', { name: '连接' }));
    await waitFor(() => expect(within(editor).getAllByRole('button', { name: '重新授权此账号' })).toHaveLength(2));
    expect(startBodies[0]).toEqual({ method: 4, inputs: { key: 'synthetic-key' }, channel_id: 7 });
    fireEvent.click(within(editor).getAllByRole('button', { name: '重新授权此账号' })[1]);
    fireEvent.change(within(editor).getByLabelText('API Key'), { target: { value: 'synthetic-replacement' } });
    fireEvent.click(within(editor).getByRole('button', { name: '连接' }));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    expect(startBodies[1]).toEqual({ method: 4, inputs: { key: 'synthetic-replacement' }, channel_id: 7, credential_id: 'second' });
    expect(within(editor).getByLabelText('渠道名称')).toHaveProperty('value', 'Preserved market draft');
    expect(fetcher.mock.calls.some(([url]) => url.startsWith('/api/oauth/market'))).toBe(false);
  });

  it('keeps uninstalled supplier accounts visible with networking blocked and hides unsupported transfers', async () => {
    const type = 'market:0123456789abcdef0123456789abcdef:fixture';
    const row: Channel = { ...account, name: 'Retained supplier', credential_type: type };
    const plugins: PluginList = { version: 'retained', items: [{ id: type, credential_type: type, name: 'Fixture Supplier', description: '', package: 'fixture-supplier', icon: '', implementation: 'compatible', featured: false, version: '', capabilities: [{ id: 'connect', enabled: false, available: false, affected_channel_ids: [7] }] }] };
    history.replaceState(null, '', '/#/channels');
    sessionStorage.setItem('to-router.channels.v1', JSON.stringify({ view: 'oauth', search: '', editor: null, flows: {} }));
    setup(() => Promise.resolve(json(plugins)), [row]);
    const tab = await screen.findByRole('tab', { name: 'Fixture Supplier' });
    fireEvent.click(tab);
    const region = await screen.findByRole('region', { name: '渠道 Retained supplier' });
    expect(screen.queryByRole('button', { name: '导入凭证' })).toBeNull();
    fireEvent.click(within(region).getByRole('checkbox', { name: '选择凭证 Saved credential' }));
    expect(screen.queryByRole('button', { name: '导出凭证' })).toBeNull();
    expect(within(region).getByRole('button', { name: '获取模型 Retained supplier', hidden: true })).toHaveProperty('disabled', true);
    expect(within(region).getByRole('button', { name: '编辑 Retained supplier' })).toHaveProperty('disabled', false);
  });

  it('keeps common actions and search in both views, searches channel fields only, and moves the browser preference into plugin navigation', async () => {
    const saved: Channel = { ...account, base_url: 'https://saved.example/v1', endpoints: [{ format: 'openai_responses', base_url: 'https://endpoint.example/v1' }] };
    const other: Channel = { ...account, id: 9, name: 'Other account', models: ['model-b'], credentials: [] };
    const fetcher = setup(undefined, [saved, other]);
    await screen.findByRole('region', { name: '渠道 Saved Codex' });
    const header = document.querySelector('.page-header')! as HTMLElement;
    expect(within(header).getByRole('link', { name: '插件库' })).toBeTruthy();
    expect(within(header).getByRole('button', { name: '刷新渠道' })).toBeTruthy();
    expect(within(header).getByRole('button', { name: '发现凭证' })).toBeTruthy();
    expect(within(header).getByRole('button', { name: '添加渠道' })).toBeTruthy();
    const search = screen.getByRole('textbox', { name: '搜索渠道或模型' });
    expect(search.closest('.upstream-toolbar')).toBeTruthy();
    const autoOpen = screen.getByRole('checkbox', { name: '授权时自动打开浏览器' });
    expect(autoOpen.closest('.oauth-main-navigation')).toBeTruthy();
    fireEvent.click(autoOpen);
    await waitFor(() => expect(fetcher.mock.calls.some(([url, init]) => url === '/api/settings/oauth-auto-open' && init?.body === '{"oauth_auto_open":true}')).toBe(true));
    for (const query of ['sAvEd', 'model-a', 'saved.example', 'endpoint.example']) {
      fireEvent.change(search, { target: { value: query } });
      expect(screen.getByRole('region', { name: '渠道 Saved Codex' })).toBeTruthy();
      expect(screen.queryByRole('region', { name: '渠道 Other account' })).toBeNull();
    }
    for (const query of ['fixture@example.invalid', 'Saved credential']) {
      fireEvent.change(search, { target: { value: query } });
      expect(screen.queryByRole('region', { name: '渠道 Saved Codex' })).toBeNull();
      expect(screen.getByText('未找到匹配渠道，试试其他名称、模型或地址。')).toBeTruthy();
    }
    fireEvent.click(screen.getByRole('button', { name: '清除搜索' }));
    fireEvent.click(within(header).getByRole('button', { name: '添加渠道' }));
    const dialog = screen.getByRole('dialog', { name: '添加渠道' });
    expect(within(dialog).getByLabelText('渠道名称')).toBeTruthy();
    expect(within(dialog).queryByLabelText('渠道 Base URL')).toBeNull();
    expect(within(dialog).getByText(/创建后在渠道中添加/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    fireEvent.click(screen.getByRole('button', { name: 'API Key 渠道' }));
    expect(screen.getByRole('textbox', { name: '搜索渠道或模型' })).toBe(search);
    expect(within(header).getByRole('button', { name: '发现凭证' })).toBeTruthy();
    expect(within(header).getByRole('button', { name: '添加渠道' })).toBeTruthy();
  });

  it.each(['codex', 'chatgpt', 'copilot', 'antigravity', 'claude', 'devin', 'qoder', 'qoder-cn', 'workbuddy', 'workbuddy-ai', 'minimax-code', 'minimax-code-global'] as const)('authorizes %s directly into its pool and reauthorizes the exact member while preserving the draft', async provider => {
    vi.spyOn(window, 'open').mockReturnValue(null);
    history.replaceState(null, '', `/#/channels?plugin=${provider}`);
    const credentialType: Exclude<Channel['credential_type'], undefined> = ['qoder', 'qoder-cn', 'workbuddy', 'workbuddy-ai', 'minimax-code', 'minimax-code-global'].includes(provider as 'qoder' | 'qoder-cn' | 'workbuddy' | 'workbuddy-ai' | 'minimax-code' | 'minimax-code-global') ? `plugin:${provider}` as Exclude<Channel['credential_type'], undefined> : `${provider}_oauth` as Exclude<Channel['credential_type'], undefined>;
    const row: Channel = { ...account, name: 'Native pool', credential_type: credentialType, generation: 1, endpoints: [{ format: 'openai_chat_completions', base_url: 'https://compat.example/v1' }],
      credentials: [{ id: 'first', label: 'First account', enabled: true, has_oauth: true, oauth_info: { ...account.credentials![0].oauth_info!, provider } }] };
    const providerPlugins: PluginList = { ...enabledPlugins, items: [...enabledPlugins.items, ...(credentialType.startsWith('plugin:') ? [{ id: provider, name: provider, credential_type: credentialType, description: 'Synthetic compatible fixture', icon: provider, implementation: 'compatible' as const, featured: true, version: 'fixture', capabilities: [{ id: 'connect' as const, enabled: true, available: true, affected_channel_ids: [] }] }] : [])] };
    const fetcher = setup(() => Promise.resolve(json(providerPlugins)), [row]), original = fetcher.getMockImplementation()!;
    const startLabel = provider === 'codex' ? '浏览器授权' : provider === 'copilot' ? '开始设备码授权' : '开始授权';
    let phase = 'add';
    fetcher.mockImplementation((url, init) => {
      if (url.startsWith(`/api/oauth/${provider}/start?`)) {
        const query = new URLSearchParams(url.split('?')[1]);
        expect(query.get('channel_id')).toBe('7');
        expect(query.get('mode')).toBeNull();
        expect(query.get('method')).toBe(['copilot', 'qoder', 'qoder-cn', 'minimax-code', 'minimax-code-global'].includes(provider) ? 'device' : 'browser');
        if (phase === 'add') {
          expect(query.get('operation')).toBe('add_credential');
          expect(query.get('credential_id')).toBeNull();
        } else {
          expect(query.get('operation')).toBe('reauth');
          expect(query.get('credential_id')).toBe('second');
        }
        return Promise.resolve(json({ flow: 'fixture-' + phase, method: 'browser', authorization_url: 'https://example.invalid/authorize', redirect_uri: 'http://localhost:54545/callback' }));
      }
      if (url.startsWith(`/api/oauth/${provider}/status?`)) {
        if (phase === 'add' && row.credentials!.length === 1) {
          row.credentials!.push({ ...row.credentials![0], id: 'second', label: 'Second account' });
          row.generation = 2;
        }
        return Promise.resolve(json({ status: 'complete', channel_id: 7 }));
      }
      if (url === '/api/channels') return Promise.resolve(json([row]));
      return original(url, init);
    });
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Native pool' }));
    const editor = await screen.findByRole('dialog', { name: '编辑渠道' });
    if (credentialType.startsWith('plugin:')) {
      expect(within(editor).getByLabelText('渠道 Base URL')).toHaveProperty('readOnly', true);
      expect(within(editor).getByLabelText('启用格式 openai_chat_completions')).toHaveProperty('disabled', true);
    }
    fireEvent.change(within(editor).getByLabelText('渠道名称'), { target: { value: 'Unsaved native name' } });
    fireEvent.click(within(editor).getByRole('button', { name: '添加授权凭证' }));
    expect(within(editor).queryByText('从已有渠道关联账号：')).toBeNull();
    expect(within(editor).queryByRole('button', { name: /添加待授权凭据占位/ })).toBeNull();
    const adding = screen.getByRole('dialog', { name: '向「Unsaved native name」添加账号 / 工作区' });
    expect(within(adding).getAllByRole('tab')).toHaveLength(3);
    fireEvent.click(within(adding).getByRole('button', { name: startLabel }));
    await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url.startsWith(`/api/oauth/${provider}/start?`))).toBe(true));
    await waitFor(() => expect(within(editor).getAllByRole('button', { name: '重新授权此账号' })).toHaveLength(2));
    expect(within(editor).getByLabelText('渠道名称')).toHaveProperty('value', 'Unsaved native name');
    phase = 'reauth';
    fireEvent.click(within(editor).getAllByRole('button', { name: '重新授权此账号' })[1]);
    fireEvent.click(within(editor).getByRole('button', { name: startLabel }));
    await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url.includes('/status?flow=fixture-reauth'))).toBe(true));
    await waitFor(() => expect(within(editor).queryByRole('button', { name: startLabel })).toBeNull());
    expect(within(editor).getByLabelText('渠道名称')).toHaveProperty('value', 'Unsaved native name');
    expect(fetcher.mock.calls.some(([url]) => url.includes('source_channel'))).toBe(false);
  });
  it('creates public Zen access without OAuth or editable credentials', async () => {
    history.replaceState(null, '', '/#/channels?plugin=opencode-zen-free');
    const rows: Channel[] = [];
    const fetcher = setup(undefined, rows), original = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => {
      if (url === '/api/channels' && init?.method === 'POST') {
        rows.push({ id: 45, name: 'OpenCode Zen Free', credential_type: 'zen_free', enabled: true, api_key: '', has_key: true, models: [], available_models: [], model_formats: {}, endpoints: [{ format: 'openai_chat_completions', base_url: 'https://opencode.ai/inference/v1' }], credentials: [{ id: 'public', label: '公共访问', has_key: true, enabled: true }] });
        return Promise.resolve(json({ channel_id: 45, already_linked: false }));
      }
      return original(url, init);
    });
    await screen.findByRole('tab', { name: 'OpenCode Zen Free' });
    fireEvent.click(screen.getByRole('button', { name: '添加渠道' }));
    const creation = screen.getByRole('dialog', { name: '添加渠道' });
    fireEvent.click(within(creation).getByRole('button', { name: '创建渠道' }));
    const region = await screen.findByRole('region', { name: '渠道 OpenCode Zen Free' });
    expect(within(region).queryByText('未授权')).toBeNull();
    expect(within(region).queryByText('套餐未知')).toBeNull();
    fireEvent.click(within(region).getByRole('button', { name: '额度详情 公共访问' }));
    const detail = await screen.findByRole('dialog', { name: '凭证详情 · 公共访问' });
    expect(within(detail).getByText('授权：公共访问，无需登录')).toBeTruthy();
    expect(within(detail).queryByText(/当前访问令牌到期/)).toBeNull();
    fireEvent.click(within(detail).getByRole('button', { name: '关闭弹窗' }));
    expect(screen.queryByRole('button', { name: '开始授权' })).toBeNull();
    expect(within(region).queryByRole('button', { name: '重新授权 OpenCode Zen Free', hidden: true })).toBeNull();
    fireEvent.click(within(region).getByRole('button', { name: '编辑 OpenCode Zen Free' }));
    const editor = await screen.findByRole('dialog', { name: '编辑渠道' });
    expect(within(editor).getByText('公共访问 · public')).toBeTruthy();
    expect(within(editor).queryByRole('button', { name: '添加凭证' })).toBeNull();
    expect(within(editor).queryByLabelText('API 密钥')).toBeNull();
    expect(fetcher.mock.calls.some(([url]) => url.includes('/oauth/') || url.includes('/start'))).toBe(false);
  });
  it('keeps disabled accounts and local management while blocking OAuth networking', async () => {
    const fetcher = setup(() => Promise.resolve(json(disabledPlugins())));
    const region = await screen.findByRole('region', { name: '渠道 Saved Codex' });
    expect(screen.getByText('接入插件未启用，账号与配置已保留')).toBeTruthy();
    expect(screen.getByRole('link', { name: '前往插件库' }).getAttribute('href')).toBe('#/plugins?category=connect&plugin=codex');
    expect(within(region).getByRole('button', { name: '测试 Saved Codex' })).toHaveProperty('disabled', true);
    expect(within(region).getByRole('button', { name: '获取模型 Saved Codex', hidden: true })).toHaveProperty('disabled', true);
    expect(within(region).getByRole('button', { name: '重新授权 Saved Codex', hidden: true })).toHaveProperty('disabled', true);
    expect(within(region).getByRole('button', { name: '刷新额度 Saved credential' })).toHaveProperty('disabled', true);
    expect(within(region).getByRole('button', { name: '预热 Saved credential' })).toHaveProperty('disabled', true);
    expect(within(region).getByRole('switch', { name: '停用 Saved Codex' }).getAttribute('aria-checked')).toBe('true');
    expect(within(region).getByRole('button', { name: '删除 Saved Codex', hidden: true })).toHaveProperty('disabled', false);
    fireEvent.click(screen.getByRole('button', { name: '向 Saved Codex 添加账号 / 工作区' }));
    expect(screen.getByRole('dialog', { name: '向「Saved Codex」添加账号 / 工作区' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: '浏览器授权' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '关闭' }));
    fireEvent.click(within(region).getByRole('button', { name: '编辑 Saved Codex' }));
    const editor = await screen.findByRole('dialog', { name: '编辑渠道' });
    expect(within(editor).getByLabelText('渠道名称')).toHaveProperty('disabled', false);
    expect(within(editor).getByRole('button', { name: '重新授权此账号' })).toHaveProperty('disabled', true);
    fireEvent.click(within(editor).getByRole('button', { name: '模型配置' }));
    expect(await within(editor).findByRole('button', { name: '从上游获取模型' })).toHaveProperty('disabled', true);
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(0);
  });

  it.each(['codex_oauth', 'chatgpt_oauth'])('selects a hidden deep-link provider from previous active %s without starting login', async active => {
    history.replaceState(null, '', '/#/channels?plugin=chatgpt');
    localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ active, hidden: { chatgpt_oauth: true } }));
    const fetcher = setup();
    await waitFor(() => expect(screen.getByRole('tab', { name: 'ChatGPT' }).getAttribute('aria-selected')).toBe('true'));
    expect(screen.getByRole('button', { name: 'OAuth 账号渠道' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.queryByRole('button', { name: '添加账号 / 工作区' })).toBeNull();
    expect(screen.getByRole('button', { name: '添加渠道' })).toHaveProperty('disabled', false);
    expect(fetcher.mock.calls.some(([url]) => url.includes('/start'))).toBe(false);
    expect(screen.getByRole('link', { name: '插件库' }).getAttribute('href')).toBe('#/plugins');
    expect(screen.getByRole('button', { name: '发现凭证' })).toBeTruthy();
    expect(screen.getByRole('button', { name: '添加渠道' })).toBeTruthy();
  });

  it('keeps the account after plugin reread fails and resumes only after successful reread', async () => {
    let current = Promise.resolve(json(disabledPlugins()));
    const fetcher = setup(() => current);
    const region = await screen.findByRole('region', { name: '渠道 Saved Codex' });
    current = Promise.resolve(json({ error: '读取权限已撤销' }, 403));
    await act(async () => document.dispatchEvent(new Event('visibilitychange')));
    await screen.findByRole('alert');
    expect(screen.getByRole('region', { name: '渠道 Saved Codex' })).toBe(region);
    expect(screen.getAllByText(/插件状态读取失败/).length).toBeGreaterThan(0);
    expect(within(region).getByRole('button', { name: '测试 Saved Codex' })).toHaveProperty('disabled', true);
    current = Promise.resolve(json(enabledPlugins));
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => expect(within(region).getByRole('button', { name: '测试 Saved Codex' })).toHaveProperty('disabled', false));
    expect(fetcher.mock.calls.filter(([url]) => url === '/api/plugins')).toHaveLength(3);
  });

  it('treats unknown plugin state as blocked without blocking API Key editing', async () => {
    history.replaceState(null, '', '/#/channels');
    const fetcher = setup(() => Promise.resolve(json({ error: '插件读取失败' }, 500)), [apiChannel]);
    const channel = await screen.findByRole('article', { name: '渠道 API channel' });
    expect(within(channel).getByRole('button', { name: '编辑 API channel' })).toHaveProperty('disabled', false);
    expect(within(channel).getByRole('button', { name: '测试渠道 API channel' })).toHaveProperty('disabled', false);
    fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    await screen.findByRole('alert');
    expect(screen.queryByRole('button', { name: '添加账号 / 工作区' })).toBeNull();
    expect(screen.queryByRole('tab')).toBeNull();
    expect(fetcher.mock.calls.some(([url]) => url.includes('/start'))).toBe(false);
  });

  it('hides disabled empty plugins from navigation and display management', async () => {
    setup(() => Promise.resolve(json(disabledPlugins())), []);
    await screen.findByText('暂无可见接入方式，可管理显示或前往插件库启用。');
    expect(screen.queryByRole('tab')).toBeNull();
    expect(screen.queryByRole('button', { name: '添加账号 / 工作区' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '管理接入方式' }));
    expect(within(screen.getByRole('dialog', { name: '管理接入方式' })).queryByRole('switch')).toBeNull();
  });

  it('hides saved disabled channels until their plugin is re-enabled, with no disabled filter', async () => {
    history.replaceState(null, '', '/#/channels');
    sessionStorage.setItem('to-router.channels.v1', JSON.stringify({ view: 'oauth' }));
    let plugins = disabledPlugins();
    setup(() => Promise.resolve(json(plugins)));
    await screen.findByText('暂无可见接入方式，可管理显示或前往插件库启用。');
    expect(screen.queryByRole('region', { name: '渠道 Saved Codex' })).toBeNull();
    expect(screen.queryByRole('button', { name: /已停用/ })).toBeNull();
    expect(screen.getByRole('checkbox', { name: '授权时自动打开浏览器' })).toBeTruthy();
    plugins = enabledPlugins;
    fireEvent.click(screen.getByRole('button', { name: '刷新渠道' }));
    const region = await screen.findByRole('region', { name: '渠道 Saved Codex' });
    expect(within(region).getByRole('button', { name: '编辑 Saved Codex' })).toHaveProperty('disabled', false);
  });

  it('allows explicit management of a hidden disabled deep-link channel without changing its display preference', async () => {
    localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ hidden: { codex_oauth: true } }));
    setup(() => Promise.resolve(json(disabledPlugins())));
    const region = await screen.findByRole('region', { name: '渠道 Saved Codex' });
    expect(within(region).getByRole('button', { name: '编辑 Saved Codex' })).toHaveProperty('disabled', false);
    expect(within(region).getByRole('button', { name: '测试 Saved Codex' })).toHaveProperty('disabled', true);
    expect(screen.queryByRole('button', { name: /已停用/ })).toBeNull();
    expect(JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!).hidden.codex_oauth).toBe(true);
  });

  describe('getChannelModelFormats', () => {
    const ep1 = { format: 'openai_chat_completions' as const, base_url: 'https://api.openai.com/v1' };
    const ep2 = { format: 'anthropic_messages' as const, base_url: 'https://api.anthropic.com' };

    it('returns all endpoints when modelName is empty or model_formats is undefined', () => {
      const ch: Channel = { ...apiChannel, endpoints: [ep1, ep2], model_formats: undefined };
      expect(getChannelModelFormats(ch, '')).toEqual(['openai_chat_completions', 'anthropic_messages']);
      expect(getChannelModelFormats(ch, 'gpt-4o')).toEqual(['openai_chat_completions', 'anthropic_messages']);
    });

    it('inherits channel endpoints for unlisted models on standard API Key channels', () => {
      const ch: Channel = {
        ...apiChannel,
        credential_type: 'api_key',
        endpoints: [ep1, ep2],
        model_formats: { 'gpt-4o': ['openai_chat_completions'] },
      };
      // Explicit override filters to allowed
      expect(getChannelModelFormats(ch, 'gpt-4o')).toEqual(['openai_chat_completions']);
      // Unlisted model inherits channel endpoints (does not fail closed to [])
      expect(getChannelModelFormats(ch, 'claude-3-5-sonnet')).toEqual(['openai_chat_completions', 'anthropic_messages']);
    });

    it('fails closed for unlisted models on catalog provider channels', () => {
      const ch: Channel = {
        ...apiChannel,
        credential_type: 'zen_free',
        endpoints: [ep1, ep2],
        model_formats: { 'zen-gpt-4o': ['openai_chat_completions'] },
      };
      expect(getChannelModelFormats(ch, 'zen-gpt-4o')).toEqual(['openai_chat_completions']);
      // Catalog providers fail closed on unlisted models
      expect(getChannelModelFormats(ch, 'unlisted-zen-model')).toEqual([]);
    });
  });
});
