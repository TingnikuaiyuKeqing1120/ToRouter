// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { ConfirmProvider } from './components/Confirm';
import { api, channelPayload, discoverFormats, parseModels, usageFormatLabel, type Channel, type CredentialType, type DiscoverResult, type ProbeResult } from './api';
import Login from './pages/Login';
import Channels from './pages/Channels';
import { OAuthAuthorize } from './components/oauth';
import * as restart from './restart';
import Models from './pages/Models';
import Settings from './pages/Settings';
import ClientAccess from './pages/ClientAccess';
import Logs from './pages/Logs';
import App from './App';
import { ToastProvider } from './components/ui';
import { enabledPlugins } from './testFixtures/plugins';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });

const channel: Channel = { id: 12, name: 'Personal', api_key: '', has_key: true, models: ['model-a'], enabled: true, endpoints: [{ format: 'openai_chat_completions', base_url: 'https://example.com' }] };
const discoverOk = (models: string[], catalogue?: string[]) => response({ ok: true, models: models.map(id => ({ id })), duration_ms: 42, catalogue });
const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
function oauthFixture(value: Channel): Channel {
  return { ...value, credentials: [{ id: `credential-${value.id}`, enabled: true, has_oauth: true, oauth_info: value.oauth }] };
}
// Existing console scenarios do not customize display preferences. Keep their
// server mocks focused on the channel/editor requests they assert.
function stubFetch(fetcher: ReturnType<typeof vi.fn>) {
  vi.stubGlobal('fetch', vi.fn((url: string, options?: RequestInit) => url === '/api/plugins' && !options?.method ? Promise.resolve(response(enabledPlugins)) : url === '/api/channels/display' && !options?.method
    ? Promise.resolve(response({ sort_mode: 'name', order: [], icons: {} }))
    : url === '/api/settings/quota-refresh' && !options?.method ? Promise.resolve(response({ quota_refresh_seconds: 300 })) : fetcher(url, options)));
}
async function editorSection(name: '快捷配置' | '模型配置' | '渠道标签' | '更多配置') {
  const editor = screen.getByRole('dialog', { name: /^(编辑|添加)渠道$/ });
  fireEvent.click(within(editor).getByRole('button', { name }));
  if (name === '模型配置') await within(editor).findByRole('region', { name: '已启用模型' });
}
async function endpointInput(format: string) {
  await editorSection('更多配置');
  const input = screen.getByLabelText(`${format} 自定义地址`);
  const details = input.closest('details')!;
  if (!details.open) fireEvent.click(details.querySelector('summary')!);
  return input;
}
// The channel page asks for one credential type at a time, so a mock matching
// the list by path has to look past the query string.
const pathOf = (url: unknown) => String(url).split('?')[0];
beforeEach(() => {
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: vi.fn().mockResolvedValue(undefined) } });
  sessionStorage.clear(); localStorage.clear(); window.location.hash = '#/channels';
  vi.spyOn(window, 'open').mockReturnValue(null);
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function (this: HTMLDialogElement) { this.setAttribute('open', ''); } });
  Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value: function (this: HTMLDialogElement) { this.removeAttribute('open'); } });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('API contract helpers', () => {
  it('parses and deduplicates comma/newline model lists', () => { expect(parseModels('a,b\na， c\n')).toEqual(['a', 'b', 'c']); });
  it('strips read-only has_key while preserving numeric ID and blank key', () => { const payload = channelPayload(channel); expect(payload).not.toHaveProperty('has_key'); expect(payload.id).toBe(12); expect(payload.api_key).toBe(''); });
  it('uses cookie credentials and custom header, never admin bearer, and expires on 401', async () => {
    const fetcher = vi.fn().mockResolvedValue(response({ error: { message: 'Unauthorized' } }, 401)); stubFetch(fetcher);
    const expired = vi.fn(); window.addEventListener('auth-expired', expired);
    try {
      await expect(api('/channels', { headers: { Authorization: 'Bearer legacy', Accept: 'application/json' } })).rejects.toThrow('登录会话');
      const [url, options] = fetcher.mock.calls[0];
      expect(url).toBe('/api/channels'); expect(options.credentials).toBe('same-origin');
      expect(options.headers.get('X-To-Router')).toBe('1'); expect(options.headers.has('Authorization')).toBe(false);
      expect(options.headers.get('Accept')).toBe('application/json'); expect(expired).toHaveBeenCalledOnce();
    } finally { window.removeEventListener('auth-expired', expired); }
  });
});

describe('model inventory helpers', () => {
  it('reads one shape whether the discover answered flat or per format', () => {
    const flat: DiscoverResult = { ok: true, models: [{ id: 'a' }, { id: 'b' }], duration_ms: 9 };
    expect(discoverFormats(flat).map(entry => entry.ok)).toEqual([true]);
    expect(discoverFormats(flat)[0].models!.map(m => m.id)).toEqual(['a', 'b']);
    const failed: DiscoverResult = { ok: false, diagnostic: { source: 'upstream', code: 'auth', message: 'x', suggestion: 'y' }, duration_ms: 3 };
    expect(discoverFormats(failed).map(entry => [entry.ok, entry.diagnostic?.code])).toEqual([[false, 'auth']]);
    // A per-format answer is handed over as it arrived: the caller sees which
    // format each entry belongs to and does not have to know both shapes.
    const perFormat: DiscoverResult = {
      ok: false, duration_ms: 12, diagnostic: { source: 'upstream', code: 'upstream_http', message: 'x', suggestion: 'y' },
      formats: [
        { format: 'anthropic_messages', ok: true, models: [{ id: 'm' }], duration_ms: 5 },
        { format: 'gemini_generate_content', ok: false, models: [], duration_ms: 7, diagnostic: { source: 'upstream', code: 'upstream_http', message: 'x', suggestion: 'y', upstream_status: 503 } },
      ],
    };
    expect(discoverFormats(perFormat)).toEqual(perFormat.formats);
  });
});

describe('channel discover and probe', () => {
  const diagnostic = { source: 'upstream', code: 'auth', message: '密钥无效', suggestion: '检查密钥', upstream_status: 401, raw: '{"error":{"message":"bad key"}}', truncated: false };
  function mockBackend(discover = discoverOk(['model-a', 'model-b', 'shared-model'], ['model-a', 'model-b', 'shared-model'])) {
    let savedChannel: Channel = { ...channel };
    return vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url === '/api/channels/discover') {
        const body = JSON.parse(options.body);
        if (body.channel) return Promise.resolve(discover);
        return Promise.resolve(response({ ok: false, diagnostic, duration_ms: 7 }));
      }
      if (url === '/api/channels/probe') {
        const body = JSON.parse(options.body);
        return Promise.resolve(response(body.confirmed ? { ok: true, model: body.model, duration_ms: 120, upstream_status: 200 } : { ok: false, model: body.model, duration_ms: 1, diagnostic }));
      }
      if (url === '/api/channels' && method === 'POST') { savedChannel = { ...JSON.parse(options.body), id: 99, has_key: true }; return Promise.resolve(response(savedChannel)); }
      if (url.startsWith('/api/channels/') && method === 'PUT') { savedChannel = { ...JSON.parse(options.body), id: channel.id, has_key: true }; return Promise.resolve(response(savedChannel)); }
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([savedChannel]));
      return Promise.resolve(response({}, 404));
    });
  }
  async function openEditor() {
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
  }
  it('discovers models into draft inventory, applies the fetch, and requires explicit save', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    expect((await screen.findByText(/已获取 3 个模型/)).textContent).toContain('42');
    expect(screen.getByLabelText('启用模型 model-a')).toBeTruthy();
    const modelB = screen.getByLabelText('启用模型 model-b') as HTMLInputElement;
    expect(modelB.checked).toBe(false);
    fireEvent.click(modelB);
    expect((screen.getByLabelText('启用模型 model-b') as HTMLInputElement).checked).toBe(true);
    const draft = (screen.getByRole('heading', { name: /模型列表/ }).textContent);
    expect(draft).toContain('2 个启用');
    expect(fetcher.mock.calls.filter(call => call[0] === '/api/channels/12' && call[1].method === 'PUT')).toHaveLength(0);
    const discoverBody = JSON.parse(fetcher.mock.calls.find(call => call[0] === '/api/channels/discover')![1].body);
    // The pull is applied, so the store adopts the fetched models as the auto
    // part of the catalogue and answers with what the channel now lists.
    expect(discoverBody.apply).toBe(true);
    expect(discoverBody.channel.id).toBe(12); expect(discoverBody.channel.api_key).toBe(''); expect(discoverBody.channel).not.toHaveProperty('has_key');
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.models).toEqual(['model-a', 'model-b']);
    expect(put.available_models).toEqual(['model-a', 'model-b', 'shared-model']);
  });
  it('persists unchecking a saved model and allows zero enabled models', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    const saved = screen.getByLabelText('启用模型 model-a') as HTMLInputElement;
    expect(saved.checked).toBe(true);
    fireEvent.click(saved);
    expect((screen.getByLabelText('启用模型 model-a') as HTMLInputElement).checked).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.models).toEqual([]);
    expect(put.available_models).toEqual(['model-a']);
  });
  it('renders failure diagnostics with raw payload and supports manual model fallback', async () => {
    const fetcher = mockBackend(response({ ok: false, diagnostic, duration_ms: 7 })); stubFetch(fetcher);
    await openEditor();
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    expect(await screen.findByText('密钥无效')).toBeTruthy();
    expect(screen.getByText('建议：检查密钥')).toBeTruthy();
    expect(screen.getByText(/来源：upstream · 代码：auth · 上游 HTTP 401/)).toBeTruthy();
    fireEvent.click(screen.getByText('查看原始响应'));
    expect(screen.getByText(diagnostic.raw)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '复制原始响应' }));
    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledWith(diagnostic.raw));
    fireEvent.change(screen.getByLabelText('手动模型 ID'), { target: { value: ' manual-model ' } });
    fireEvent.click(screen.getByRole('button', { name: '手动添加' }));
    expect(screen.getByText('manual-model')).toBeTruthy();
    expect((screen.getByLabelText('启用模型 manual-model') as HTMLInputElement).checked).toBe(false);
  });
  it('probes only after explicit confirmation, maps aliases, and never mutates the saved channel', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '测试渠道 Personal' }));
    expect(screen.getByText(/向所选模型发送 hi 测试请求，可能消耗上游额度/)).toBeTruthy();
    expect(screen.getByText('将调用：model-a')).toBeTruthy();
    expect(fetcher.mock.calls.filter(call => call[0] === '/api/channels/probe')).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: '确认并测试（可能收费）' }));
    expect(await screen.findByText(/测试成功/)).toBeTruthy();
    // The probe is one paid generation: the console names no formats, so the
    // server tests one endpoint rather than every declared one.
    const probes = fetcher.mock.calls.filter(call => call[0] === '/api/channels/probe');
    expect(probes).toHaveLength(1);
    expect(JSON.parse(probes[0][1].body).formats).toBeUndefined();
    expect(fetcher.mock.calls.filter(call => call[1].method === 'PUT')).toHaveLength(0);
  });
  it('shows each format answer when a probe names several', async () => {
    // A caller that names formats gets one answer per format; the row reports
    // each outcome, and a failed facade names its own reason.
    const multi: ProbeResult = { ok: false, model: 'model-a', duration_ms: 30, formats: [
      { format: 'openai_chat_completions', ok: true, duration_ms: 12, upstream_status: 200 },
      { format: 'anthropic_messages', ok: false, duration_ms: 18, diagnostic },
    ] };
    const fetcher = vi.fn().mockImplementation((url) => url === '/api/channels/probe'
      ? Promise.resolve(response(multi))
      : pathOf(url) === '/api/channels' ? Promise.resolve(response([channel])) : Promise.resolve(response({}, 404)));
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '测试渠道 Personal' }));
    fireEvent.click(screen.getByRole('button', { name: '确认并测试（可能收费）' }));
    expect(await screen.findByText(/OpenAI Chat Completions：测试成功 · 12 ms/)).toBeTruthy();
    expect(screen.getByText(/Anthropic Messages：测试失败 · 18 ms/)).toBeTruthy();
    expect(screen.getByText('密钥无效')).toBeTruthy();
    // The shared one-line verdict is not shown on top of the per-format answers.
    expect(screen.queryByText(/测试失败 · model-a/)).toBeNull();
    expect(fetcher.mock.calls.filter(call => call[0] === '/api/channels/probe')).toHaveLength(1);
  });
  it('supports expanding advanced probe options and batch testing specific credentials and endpoints', async () => {
    const multiChannel: Channel = {
      ...channel,
      endpoints: [
        { format: 'openai_chat_completions', base_url: 'http://test/api' },
        { format: 'anthropic_messages', base_url: 'http://test/api' },
      ],
      credentials: [
        { id: 'c1', label: 'Primary Key', enabled: true },
        { id: 'c2', label: 'Backup Key', enabled: true },
      ],
    };
    const batchResult: ProbeResult = {
      ok: true,
      model: 'model-a',
      duration_ms: 50,
      formats: [
        { format: 'openai_chat_completions', credential_id: 'c1', credential_label: 'Primary Key', ok: true, duration_ms: 20 },
        { format: 'openai_chat_completions', credential_id: 'c2', credential_label: 'Backup Key', ok: true, duration_ms: 25 },
      ],
    };
    const fetcher = vi.fn().mockImplementation((url) => {
      if (url === '/api/channels/probe') return Promise.resolve(response(batchResult));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([multiChannel]));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: `测试渠道 ${multiChannel.name}` }));
    // Open advanced testing options
    fireEvent.click(screen.getByText(/高级测试选项/));
    expect(screen.getByText('测试协议与端点：')).toBeTruthy();
    expect(screen.getByText('测试凭据与账号：')).toBeTruthy();
    // Batch test button should show count
    const batchBtn = screen.getByRole('button', { name: /确认并批量测试/ });
    expect(batchBtn).toBeTruthy();
    fireEvent.click(batchBtn);
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/probe')).toBe(true));
    const probeBody = JSON.parse(fetcher.mock.calls.find(call => call[0] === '/api/channels/probe')![1].body);
    expect(probeBody.formats).toBeDefined();
    expect(probeBody.credential_ids).toBeDefined();
    expect(await screen.findByText(/\[Primary Key\] OpenAI Chat Completions：测试成功 · 20 ms/)).toBeTruthy();
    expect(screen.getByText(/\[Backup Key\] OpenAI Chat Completions：测试成功 · 25 ms/)).toBeTruthy();
  });
  it('tests the selected upstream model directly and allows manual actual IDs', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    fireEvent.click(screen.getByRole('button', { name: '测试模型 model-a' }));
    expect(await screen.findByText('将调用：model-a')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('测试模型'), { target: { value: '' } });
    fireEvent.change(screen.getByLabelText('实际上游模型 ID'), { target: { value: 'raw-upstream-id' } });
    expect(screen.getByText('将调用：raw-upstream-id')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '确认并测试（可能收费）' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/probe')).toBe(true));
    expect(JSON.parse(fetcher.mock.calls.find(call => call[0] === '/api/channels/probe')![1].body).model).toBe('raw-upstream-id');
  });
  it('blocks discovery and probe until the key is reentered after an endpoint changes', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    fireEvent.change(await endpointInput('openai_chat_completions'), { target: { value: 'https://changed.test' } });
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    expect((await screen.findByRole('alert')).textContent).toContain('重新输入 API 密钥');
    fireEvent.click(screen.getByRole('button', { name: '测试模型 model-a' }));
    expect(screen.getAllByRole('alert').every(el => el.textContent?.includes('重新输入 API 密钥'))).toBe(true);
    expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/discover' || call[0] === '/api/channels/probe')).toBe(false);
    await editorSection('快捷配置');
    fireEvent.change(screen.getByLabelText('API 密钥'), { target: { value: 'fresh-key' } });
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    expect(await screen.findByText(/已获取 3 个模型/)).toBeTruthy();
    const body = JSON.parse(fetcher.mock.calls.find(call => call[0] === '/api/channels/discover')![1].body);
    expect(body.channel.endpoints[0].base_url).toBe('https://changed.test'); expect(body.channel.api_key).toBe('fresh-key');
  });
  it('treats a re-pointed channel base as a changed destination for the saved key', async () => {
    // Stored with one shared base: the row reads back as inheriting it, so the
    // channel base is the one place the whole channel is re-pointed from.
    const shared: Channel = { ...channel, base_url: 'https://shared.example.com', endpoints: [{ format: 'openai_chat_completions', base_url: 'https://shared.example.com' }] };
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url === '/api/channels/discover') return Promise.resolve(response({ ok: false, diagnostic: { source: 'upstream', code: 'auth', message: '密钥无效', suggestion: 'x' }, duration_ms: 1 }));
      if (pathOf(url) !== '/api/channels') return Promise.resolve(response({}, 404));
      return Promise.resolve(response(method === 'PUT' ? shared : [shared]));
    });
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    expect((screen.getByLabelText('openai_chat_completions 自定义地址') as HTMLInputElement).value).toBe('');
    await editorSection('快捷配置');
    fireEvent.change(screen.getByLabelText('渠道 Base URL'), { target: { value: 'https://moved.example.com' } });
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    expect((await screen.findByRole('alert')).textContent).toContain('重新输入 API 密钥');
    expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/discover')).toBe(false);
  });
  it('allows discovery and probe when enabling a new format on the same base URL', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    // Toggle on anthropic_messages without changing base_url
    await editorSection('快捷配置');
    fireEvent.click(screen.getByRole('checkbox', { name: '启用格式 anthropic_messages' }));
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/discover')).toBe(true));
    expect(screen.queryByRole('alert')).toBeNull();
  });
  it('sends an empty preferred endpoint when the default order is kept', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    expect(JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body).preferred_endpoint).toBe('');
  });
  it('loads the saved preferred endpoint, offers all four formats, and saves the change', async () => {
    const multi: Channel = { ...channel, endpoints: [
      { format: 'openai_chat_completions', base_url: 'https://example.com' },
      { format: 'anthropic_messages', base_url: 'https://second.example' },
      { format: 'openai_responses', base_url: 'https://third.example' }], preferred_endpoint: 'anthropic_messages' };
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url.startsWith('/api/channels/') && method === 'PUT') return Promise.resolve(response({ ...JSON.parse(options.body), id: 12, has_key: true }));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([multi]));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    await editorSection('快捷配置');
    const preferred = screen.getByLabelText('偏好协议') as HTMLSelectElement;
    expect(preferred.value).toBe('anthropic_messages');
    // Responses participates in conversion and may be explicitly preferred.
    expect([...preferred.options].map(o => o.value)).toEqual(['', 'openai_chat_completions', 'anthropic_messages', 'openai_responses']);
    fireEvent.change(preferred, { target: { value: 'openai_chat_completions' } });
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.preferred_endpoint).toBe('openai_chat_completions');
    expect(put.endpoints.map((e: { format: string }) => e.format)).toEqual(['openai_chat_completions', 'anthropic_messages', 'openai_responses']);
  });
  it('declares a format by checking its box, and the row carries the channel default', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    // The declared endpoint keeps its own address: a stored row expands the
    // channel default into it, and the editor shows that address as its custom
    // one rather than hiding it.
    expect(await endpointInput('openai_chat_completions')).toHaveProperty('value', 'https://example.com');
    expect(screen.getByText('https://example.com/v1/chat/completions')).toBeTruthy();
    // An undeclared format has no row at all.
    expect(screen.queryByLabelText('anthropic_messages 自定义地址')).toBeNull();
    await editorSection('快捷配置');
    fireEvent.click(screen.getByRole('checkbox', { name: '启用格式 anthropic_messages' }));
    const added = await endpointInput('anthropic_messages') as HTMLInputElement;
    expect(added.value).toBe('');
    // The channel base is the address every endpoint without its own uses, and
    // the row says so before anything is saved.
    expect(within(added.closest('details')!).getByText('请填写有效的 Base URL')).toBeTruthy();
    await editorSection('快捷配置');
    fireEvent.change(screen.getByLabelText('渠道 Base URL'), { target: { value: 'https://shared.example.com' } });
    await endpointInput('anthropic_messages');
    expect(within(added.closest('details')!).getByText('https://shared.example.com/v1/messages')).toBeTruthy();
    // Unchecking the format removes its independent configuration.
    await editorSection('快捷配置');
    fireEvent.click(screen.getByRole('checkbox', { name: '启用格式 anthropic_messages' }));
    expect(screen.queryByLabelText('anthropic_messages 自定义地址')).toBeNull();
    expect((screen.getByRole('checkbox', { name: '启用格式 anthropic_messages' }) as HTMLInputElement).checked).toBe(false);
    await editorSection('快捷配置');
    fireEvent.click(screen.getByRole('checkbox', { name: '启用格式 anthropic_messages' }));
    expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(false);
    // Saving keeps the own address and stores the new format without one: the
    // server expands the default into it.
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.base_url).toBe('https://shared.example.com');
    expect(put.endpoints.map((e: { format: string; base_url: string }) => [e.format, e.base_url])).toEqual([['openai_chat_completions', 'https://example.com'], ['anthropic_messages', '']]);
  });
  it('shows the same checkbox endpoint editor for an OAuth channel, with no channel base', async () => {
    const fetcher = vi.fn().mockImplementation((url) => pathOf(url) === '/api/channels' ? Promise.resolve(response([{ ...channel, id: 21, name: 'Copilot', credential_type: 'copilot_oauth' as CredentialType, has_oauth: true }])) : Promise.resolve(response({}, 404)));
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Copilot' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    await editorSection('快捷配置');
    // T031: an OAuth channel is declared exactly like an API key one — by
    // checking the formats it serves — and the channel Base URL field shows too.
    // It starts empty: the platform's own endpoints carry their own addresses,
    // which the checkbox form reads back as already declared.
    expect((screen.getByLabelText('渠道 Base URL') as HTMLInputElement).value).toBe('');
    expect((screen.getByRole('checkbox', { name: '启用格式 openai_chat_completions' }) as HTMLInputElement).checked).toBe(true);
    await endpointInput('openai_chat_completions');
    expect(screen.getByText('https://example.com/v1/chat/completions')).toBeTruthy();
    // The ordered per-endpoint list is gone for both credential types.
    expect(screen.queryByLabelText('端点 1 地址')).toBeNull();
    expect(screen.queryByRole('button', { name: '添加端点' })).toBeNull();
  });
  it('declares an OAuth endpoint by checking its format and saves it like an API key row', async () => {
    const oauth = { ...channel, id: 21, name: 'Copilot', credential_type: 'copilot_oauth' as CredentialType, has_oauth: true };
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (String(url).startsWith('/api/channels/21') && method === 'PUT') return Promise.resolve(response({ ...JSON.parse(options.body), id: 21, has_oauth: true }));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([oauth]));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Copilot' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    await editorSection('快捷配置');
    // A newly checked format inherits the channel base; an address of its own
    // overrides it. The payload is the one an API key channel sends.
    await editorSection('快捷配置');
    fireEvent.click(screen.getByRole('checkbox', { name: '启用格式 anthropic_messages' }));
    fireEvent.change(await endpointInput('anthropic_messages'), { target: { value: 'https://anthropic.example' } });
    await editorSection('快捷配置');
    fireEvent.change(screen.getByLabelText('渠道 Base URL'), { target: { value: 'https://shared.example.com' } });
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.base_url).toBe('https://shared.example.com');
    expect(put.endpoints.map((e: { format: string; base_url: string }) => [e.format, e.base_url])).toEqual([['openai_chat_completions', 'https://example.com'], ['anthropic_messages', 'https://anthropic.example']]);
  });
  it('closes a draft that changed nothing without asking about unsaved changes', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    // Nothing changed: the close controls do their job without a question.
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    await waitFor(() => expect(screen.queryByLabelText('搜索模型')).toBeNull());
    expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull();
  });
  it('guards the X, the backdrop and the cancel button once the draft is dirty', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    fireEvent.change(screen.getByLabelText('渠道名称'), { target: { value: 'Renamed' } });
    // Every way out reaches the same shared confirmation and preserves the draft.
    const editor = screen.getByRole('dialog', { name: '编辑渠道' });
    for (const close of [() => fireEvent.click(within(editor).getByRole('button', { name: '关闭弹窗' })), () => fireEvent.click(editor), () => fireEvent.click(within(editor).getByRole('button', { name: '取消' }))]) {
      close();
      fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
    }
    // Refusing keeps the edit exactly where it was.
    expect((screen.getByLabelText('渠道名称') as HTMLInputElement).value).toBe('Renamed');
    expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(false);
    // Agreeing closes it and saves nothing.
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
    await waitFor(() => expect(screen.queryByLabelText('渠道名称')).toBeNull());
    expect(fetcher.mock.calls.every(call => call[1].method !== 'PUT')).toBe(true);
  });
  it('treats a typed api key as a change even when nothing else moved', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    await editorSection('快捷配置');
    fireEvent.change(screen.getByLabelText('API 密钥'), { target: { value: 'fresh-key' } });
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
    expect(screen.getByLabelText('渠道名称')).toBeTruthy();
  });
  it('manages multi-credential pool with dispatch mode and chain topology', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    await openEditor();
    await editorSection('快捷配置');
    expect(screen.getByRole('region', { name: '凭据池与调度策略' })).toBeTruthy();
    // Initially has 1 credential
    expect(screen.getByLabelText('凭据 1 备注')).toHaveProperty('value', '默认凭据');
    // Add backup credential
    fireEvent.click(screen.getByRole('button', { name: '添加凭证' }));
    expect(screen.getByLabelText('凭据 2 备注')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('凭据 2 备注'), { target: { value: '备份 Key 2' } });
    fireEvent.change(screen.getByLabelText('凭据 2 API 密钥'), { target: { value: 'sk-backup-key-2' } });

    // Switch dispatch mode to Round Robin and chain topology to Cyclic
    expect(screen.getByText('调用策略').closest('details')).toHaveProperty('open', true);
    fireEvent.click(screen.getByRole('button', { name: '按请求轮询' }));
    fireEvent.click(screen.getByRole('button', { name: '循环链' }));

    // Save channel
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.dispatch_mode).toBe('round_robin');
    expect(put.chain_type).toBe('cyclic');
    expect(put.credentials).toHaveLength(2);
    expect(put.credentials[1].label).toBe('备份 Key 2');
    expect(put.credentials[1].api_key).toBe('sk-backup-key-2');
  });
  it('shares channel-bound account addition and edits labels without copying another channels authorization', async () => {
    const copilot1: Channel = {
      id: 101,
      name: 'Copilot 1',
      api_key: '',
      has_key: false,
      has_oauth: true,
      credential_type: 'copilot_oauth',
      models: ['gpt-5'],
      enabled: true,
      endpoints: [{ format: 'openai_chat_completions', base_url: 'https://api.githubcopilot.com' }],
      oauth: { provider: 'copilot', account_label: 'user1@example.invalid', status: 'active', expires_at: '2026-10-03T00:00:00Z', updated_at: '2026-10-02T00:00:00Z' },
    };
    const copilot2: Channel = {
      id: 102,
      name: 'Copilot 2',
      api_key: '',
      has_key: false,
      has_oauth: true,
      credential_type: 'copilot_oauth',
      models: ['gpt-5'],
      enabled: true,
      endpoints: [{ format: 'openai_chat_completions', base_url: 'https://api.githubcopilot.com' }],
      oauth: { provider: 'copilot', account_label: 'user2@example.invalid', status: 'active', expires_at: '2026-10-03T00:00:00Z', updated_at: '2026-10-02T00:00:00Z' },
    };
    copilot1.credentials = [{ id: 'saved-first', label: '默认凭据', enabled: true, has_oauth: true, oauth_info: copilot1.oauth }];
    copilot2.credentials = [{ id: 'source-2', label: 'user2@example.invalid', enabled: true, has_oauth: true, oauth_info: copilot2.oauth }];
    const source3OAuth = { ...copilot2.oauth!, account_label: 'user3@example.invalid' };
    const copilot3: Channel = { ...copilot2, id: 103, name: 'Copilot 3', oauth: source3OAuth, credentials: [{ ...copilot2.credentials[0], id: 'source-3', label: '团队工作区', oauth_info: source3OAuth }] };
    const fetcher = vi.fn().mockImplementation((url) => {
      const p = pathOf(url);
      if (p === '/api/channels') return Promise.resolve(response([copilot1, copilot2, copilot3]));
      if (p.startsWith('/api/channels/')) return Promise.resolve(response(copilot1));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);

    fireEvent.click(await screen.findByRole('button', { name: '编辑 Copilot 1' }));
    expect(screen.getByRole('region', { name: '凭据池与调度策略' })).toBeTruthy();
    // Ordinary editing shows the credential label and authorization status;
    // account identity stays behind the main view's credential details.
    expect(within(screen.getByRole('dialog')).queryByText('user1@example.invalid')).toBeNull();
    expect(within(screen.getByRole('dialog')).getByText('凭证有效')).toBeTruthy();
    expect(screen.getByLabelText('凭据 1 备注')).toHaveProperty('value', '默认凭据');
    fireEvent.change(screen.getByLabelText('凭据 1 备注'), { target: { value: '主号' } });

    fireEvent.click(screen.getByRole('button', { name: '添加授权凭证' }));
    const adding = within(screen.getByRole('dialog', { name: '向「Copilot 1」添加账号 / 工作区' }));
    expect(adding.getAllByRole('tab')).toHaveLength(3);
    expect(adding.queryByText('从已有渠道关联账号：')).toBeNull();
    expect(adding.queryByRole('button', { name: 'Copilot 2' })).toBeNull();
    expect(adding.queryByRole('button', { name: /待授权凭据占位/ })).toBeNull();
    fireEvent.click(adding.getByRole('button', { name: '关闭' }));
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.credentials).toHaveLength(1);
    expect(put.credentials[0]).toMatchObject({ id: 'saved-first', label: '主号' });
    expect(put.credentials[0]).not.toHaveProperty('source_channel_id');
    for (const credential of put.credentials) {
      for (const field of ['oauth', 'oauth_info', 'has_oauth', 'account_label', 'access_token', 'refresh_token']) expect(credential).not.toHaveProperty(field);
    }
  });
  it('copies the channel upstream address from the row', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '复制 Personal 上游地址' }));
    await vi.waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith('https://example.com'));
  });
  it('ignores stale discovery results after the editor closes', async () => {
    let release!: (value: Response) => void;
    const fetcher = vi.fn().mockImplementation((url) => url === '/api/channels/discover'
      ? new Promise<Response>(resolve => { release = resolve; })
      : pathOf(url) === '/api/channels' ? Promise.resolve(response([channel])) : Promise.resolve(response({}, 404)));
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/discover')).toBe(true));
    // The draft is still untouched here (the pull is in flight), so the close
    // guard has nothing to ask about and the X closes straight away.
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    fireEvent.click(screen.getByRole('button', { name: '编辑 Personal' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    expect(screen.getByLabelText('启用模型 model-a')).toBeTruthy();
    await act(async () => release(discoverOk(['late-model'])));
    expect(screen.queryByLabelText('启用模型 late-model')).toBeNull();
    expect(screen.queryByText(/已获取 1 个模型/)).toBeNull();
  });
});

describe('channel model wiring', () => {
  // A channel whose catalogue lists one entry the last fetch did not return.
  const stale: Channel = { ...channel, models: ['model-a'], available_models: ['model-a', 'retired'] };
  const notice = '上游当前目录中没有模型 retired（上游可能已下线或已更名，也可能这次未拉取到）；已照常启用，请自行确认可用性。';
  function mockStale(options: { notice?: boolean } = {}) {
    return vi.fn().mockImplementation((url, options2 = {}) => {
      const method = options2.method || '';
      if (url === '/api/channels/discover') return Promise.resolve(discoverOk(['model-a', 'fresh'], ['model-a', 'fresh']));
      if (url.startsWith('/api/channels/12/models') && method === 'DELETE') {
        // The answer is the channel as it stands after the delete.
        const model = new URL('http://x' + String(url)).searchParams.get('model') || '';
        return Promise.resolve(response({ ...stale, has_key: true, models: stale.models.filter(m => m !== model), available_models: (stale.available_models || []).filter(m => m !== model) }));
      }
      if (url.startsWith('/api/channels/') && method === 'PUT') return Promise.resolve(response({ ...JSON.parse(options2.body), id: channel.id, has_key: true, ...(options.notice ? { notice } : {}) }));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([stale]));
      return Promise.resolve(response({}, 404));
    });
  }
  const enabledList = () => screen.getByRole('region', { name: '已启用模型' });
  const disabledList = () => screen.getByRole('region', { name: '未启用模型' });
  async function openStaleEditor() {
    // The toast is where a save's notice surfaces, so the editor is rendered
    // inside the provider the app wraps it in.
    render(<ToastProvider><Channels /></ToastProvider>);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
  }
  it('marks a hand-added model manual, applies the pull, and drops what the fetch no longer lists', async () => {
    const fetcher = mockStale(); stubFetch(fetcher);
    await openStaleEditor();
    // The loaded catalogue lists the entry the last fetch did not return.
    expect(within(disabledList()).getByLabelText('启用模型 retired')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('手动模型 ID'), { target: { value: ' hand-picked ' } });
    fireEvent.click(screen.getByRole('button', { name: '手动添加' }));
    expect(screen.getByLabelText('启用模型 hand-picked')).toBeTruthy();
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    await screen.findByText(/已获取 2 个模型/);
    expect(JSON.parse(fetcher.mock.calls.find(call => call[0] === '/api/channels/discover')![1].body).apply).toBe(true);
    // The answer's catalogue replaces the draft's: the entry the upstream no
    // longer lists is gone instead of being resurrected by a union, and the
    // hand-added one survives.
    expect(within(disabledList()).queryByLabelText('启用模型 retired')).toBeNull();
    expect(within(disabledList()).getByLabelText('启用模型 fresh')).toBeTruthy();
    expect(screen.getByLabelText('启用模型 hand-picked')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    // The manual marker rides with the channel, so a later pull keeps the entry.
    expect(put.manual_models).toEqual(['hand-picked']);
    expect(put.available_models).toContain('hand-picked');
    expect(put.available_models).toContain('fresh');
  });
  it('deletes a catalogue entry and an enabled one through the channel API', async () => {
    const fetcher = mockStale(); stubFetch(fetcher);
    await openStaleEditor();
    fireEvent.click(within(disabledList()).getByRole('button', { name: '删除模型 retired' }));
    fireEvent.click(await screen.findByRole('button', { name: '删除渠道模型' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'DELETE')).toBe(true));
    expect(fetcher.mock.calls.find(call => call[1].method === 'DELETE')![0]).toBe('/api/channels/12/models?model=retired');
    expect(within(disabledList()).queryByLabelText('启用模型 retired')).toBeNull();
    fireEvent.click(within(enabledList()).getByRole('button', { name: '删除模型 model-a' }));
    fireEvent.click(await screen.findByRole('button', { name: '删除渠道模型' }));
    await waitFor(() => expect(fetcher.mock.calls.filter(call => call[1].method === 'DELETE')).toHaveLength(2));
    expect(within(enabledList()).queryByLabelText('启用模型 model-a')).toBeNull();
  });
  it('shows the notice a save answers with about a model the catalogue no longer lists', async () => {
    const fetcher = mockStale({ notice: true }); stubFetch(fetcher);
    await openStaleEditor();
    fireEvent.click(screen.getByLabelText('启用模型 retired'));
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    // The notice says the upstream may have taken the model offline, and it is
    // shown instead of being swallowed by the save.
    expect(await screen.findByText(/渠道已保存。上游当前目录中没有模型 retired（上游可能已下线/)).toBeTruthy();
  });
  it('keeps an enabled model out of the catalogue instead of re-unioning it on save', async () => {
    // "ghost" is enabled but the catalogue no longer lists it — what a pull
    // leaves behind. Saving must not write it back into the catalogue; the union
    // is only for what the editor displays.
    const leftover: Channel = { ...channel, models: ['model-a', 'ghost'], available_models: ['model-a'], model_configs: { ghost: { max_output_tokens: 32 } } };
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url.startsWith('/api/channels/') && method === 'PUT') return Promise.resolve(response({ ...JSON.parse(options.body), id: channel.id, has_key: true }));
      if (pathOf(url) !== '/api/channels') return Promise.resolve(response({}, 404));
      return Promise.resolve(response(method === 'PUT' ? leftover : [leftover]));
    });
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    // It is displayed as enabled, because the editor shows the union.
    expect(within(enabledList()).getByLabelText('启用模型 ghost')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.models).toEqual(['model-a', 'ghost']);
    expect(put.available_models).toEqual(['model-a']);
  });
  it('drops the per-model config and evidence a pull took off the shelf', async () => {
    // The store prunes the config and evidence of models the channel no longer
    // lists, and a row may only name listed models — so the draft drops the same
    // keys, or its next save would be refused for referencing a removed model.
    const configured: Channel = { ...channel, models: ['model-a'], available_models: ['model-a', 'retired'], model_configs: { retired: { max_output_tokens: 32 } }, model_evidence: { retired: { source: 'test', api_format: 'openai_chat_completions' } } };
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url === '/api/channels/discover') return Promise.resolve(discoverOk(['model-a', 'fresh'], ['model-a', 'fresh']));
      if (url.startsWith('/api/channels/') && method === 'PUT') return Promise.resolve(response({ ...JSON.parse(options.body), id: channel.id, has_key: true }));
      if (pathOf(url) !== '/api/channels') return Promise.resolve(response({}, 404));
      return Promise.resolve(response(method === 'PUT' ? configured : [configured]));
    });
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    await screen.findByText(/已获取 2 个模型/);
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put).not.toHaveProperty('model_configs');
    expect(put.model_evidence).toEqual({});
    expect(put.available_models).toEqual(['model-a', 'fresh']);
  });
});

describe('console interactions', () => {
  it.each(['success', 'failure', 'validation'])('saves a channel through the real navigation guard before leaving: %s', async outcome => {
    let resolveSave!: (value: Response) => void;
    const pending = new Promise<Response>(done => { resolveSave = done; });
    const fetcher = vi.fn((url: string, options?: RequestInit) => {
      if (url === '/api/auth/session') return Promise.resolve(response({ authenticated: true }));
      if (url === '/api/channels/12' && options?.method === 'PUT') return outcome === 'failure' ? Promise.resolve(response({ error: 'navigation save failed' }, 503)) : pending;
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([channel]));
      if (url === '/api/settings') return Promise.resolve(response({ quota_refresh_seconds: 300, retention_days: 7, max_attempts: 3, retries_per_channel: 2, failure_cooldown_seconds: 10 }));
      if (url === '/api/runtime/port') return Promise.resolve(response({ port: 2886, origin: 'http://127.0.0.1:2886' }));
      if (url === '/api/companion/preferences') return Promise.resolve(response({ theme: 'dark', paused: false, revision: 0 }));
      return Promise.resolve(response([]));
    });
    stubFetch(fetcher); render(<App />);
    // App loads the page lazily. On a cold Windows worker its first mount can
    // reach the loading shell after the default 1s query deadline. Wait for the
    // actual channel control before testing save/navigation (no assertion skipped).
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }, { timeout: 5000 }));
    const editor = screen.getByRole('dialog', { name: '编辑渠道' });
    fireEvent.change(within(editor).getByLabelText('渠道名称'), { target: { value: 'Navigation draft' } });
    if (outcome === 'validation') fireEvent.change(within(editor).getByLabelText('渠道 Base URL'), { target: { value: 'ftp://invalid.example' } });
    fireEvent.click(screen.getAllByRole('button', { name: '系统设置' })[0]);
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '确认保存' }));
    const writes = () => fetcher.mock.calls.filter(([url, options]) => url === '/api/channels/12' && options?.method === 'PUT');
    if (outcome === 'validation') {
      expect((await within(editor).findByRole('alert')).textContent).toContain('有效的 HTTP(S) 地址');
      expect(writes()).toHaveLength(0);
    } else {
      await waitFor(() => expect(writes()).toHaveLength(1));
      expect(JSON.parse(String(writes()[0][1]?.body))).toMatchObject({ name: 'Navigation draft', models: ['model-a'] });
      if (outcome === 'failure') expect((await within(editor).findByRole('alert')).textContent).toContain('navigation save failed');
      else {
        expect(window.location.hash).toBe('#/channels');
        expect(screen.getByRole('dialog', { name: '编辑渠道' })).toBe(editor);
        await act(async () => { resolveSave(response(channel)); await pending; });
        await screen.findByLabelText('额度刷新周期');
        expect(window.location.hash).toBe('#/settings');
        expect(screen.queryByRole('dialog', { name: '编辑渠道' })).toBeNull();
        expect(writes()).toHaveLength(1);
        return;
      }
    }
    expect(window.location.hash).toBe('#/channels');
    expect(screen.getByRole('dialog', { name: '编辑渠道' })).toBe(editor);
    expect(within(editor).getByLabelText('渠道名称')).toHaveProperty('value', 'Navigation draft');
    expect(screen.queryByLabelText('额度刷新周期')).toBeNull();
  });

  it('treats continuing an unsaved Settings draft as normal navigation cancellation', async () => {
    window.location.hash = '#/settings';
    const config = { gateway_key: 'synthetic-admin', quota_refresh_seconds: 300, retention_days: 7, max_attempts: 3, retries_per_channel: 2, failure_cooldown_seconds: 10 };
    stubFetch(vi.fn((url: string) => Promise.resolve(response(url === '/api/settings' ? config : url === '/api/runtime/port' ? { port: 2886, origin: 'http://127.0.0.1:2886' } : url === '/api/companion/preferences' ? { theme: 'dark', paused: false, revision: 0 } : []))));
    render(<App />);
    fireEvent.change(await screen.findByLabelText('额度刷新周期'), { target: { value: '123' } });
    fireEvent.click(screen.getAllByRole('button', { name: '上游接入' })[0]);
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByLabelText('额度刷新周期')).toHaveProperty('value', '123');
    expect(window.location.hash).toBe('#/settings'); expect(screen.queryByRole('alert')).toBeNull();
    fireEvent.click(screen.getAllByRole('button', { name: '上游接入' })[0]);
    fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
    await screen.findByRole('heading', { name: '上游接入' });
    expect(window.location.hash).toBe('#/channels'); expect(screen.queryByRole('alert')).toBeNull();
  });

  it('restores a cookie session before protected requests, clears legacy key and revokes logout', async () => {
    sessionStorage.setItem('to-router.access-key', 'legacy-admin');
    let restore!: (value: Response) => void;
    const fetcher = vi.fn().mockImplementation((url, _options) => url === '/api/auth/session' ? new Promise<Response>(resolve => { restore = resolve; }) : Promise.resolve(response([])));
    stubFetch(fetcher); render(<App />);
    expect(sessionStorage.getItem('to-router.access-key')).toBeNull();
    expect(fetcher).toHaveBeenCalledOnce(); expect(fetcher.mock.calls[0][0]).toBe('/api/auth/session');
    await act(async () => restore(response({ authenticated: true })));
    await screen.findByRole('heading', { name: '上游接入' });
    fireEvent.click(screen.getAllByRole('button', { name: '退出登录' })[0]);
    await screen.findByLabelText('管理员密钥');
    expect(fetcher.mock.calls.some(call => call[0] === '/api/auth/logout' && call[1].method === 'POST')).toBe(true);
    for (const [url, options] of fetcher.mock.calls) {
      if (String(url).startsWith('/api/')) {
        expect(options.credentials).toBe('same-origin'); expect(options.headers.get('X-To-Router')).toBe('1'); expect(options.headers.has('Authorization')).toBe(false);
      } else {
        expect(String(url)).toMatch(/\/_console\/ready(?:\?watch=1)?$/);
        expect(options.credentials).toBe('omit');
        expect(new Headers(options.headers).has('Authorization')).toBe(false);
        expect(new Headers(options.headers).has('Cookie')).toBe(false);
      }
    }
  });
  it('shows login on session 401 without protected requests', async () => {
    const fetcher = vi.fn().mockResolvedValue(response({}, 401)); stubFetch(fetcher);
    render(<App />); await screen.findByLabelText('管理员密钥'); expect(fetcher).toHaveBeenCalledTimes(2);
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual(['/api/auth/session', '/api/auth/local']);
  });
  it('lets session restore failures retry rather than claiming the user is logged out', async () => {
    const fetcher = vi.fn().mockResolvedValueOnce(response({ error: '暂时不可用' }, 503)).mockResolvedValueOnce(response({}, 401)); stubFetch(fetcher);
    render(<App />); expect((await screen.findByRole('alert')).textContent).toContain('暂时不可用');
    expect(screen.queryByLabelText('管理员密钥')).toBeNull(); fireEvent.click(screen.getByRole('button', { name: '重试' }));
    await screen.findByLabelText('管理员密钥'); expect(fetcher).toHaveBeenCalledTimes(3);
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual(['/api/auth/session', '/api/auth/session', '/api/auth/local']);
  });
  it('reports rejected login and never invokes success', async () => {
    stubFetch(vi.fn().mockResolvedValue(response({}, 401))); const success = vi.fn(); render(<Login onSuccess={success} />);
    fireEvent.change(screen.getByLabelText('管理员密钥'), { target: { value: 'wrong' } }); fireEvent.click(screen.getByRole('button', { name: '进入控制台' }));
    expect((await screen.findByRole('alert')).textContent).toContain('管理员密钥不正确'); expect(success).not.toHaveBeenCalled();
  });
  it.each([false, true])('logs in without storing the admin key (remember=%s)', async remember => {
    const fetcher = vi.fn().mockResolvedValue(response({})); stubFetch(fetcher); const onSuccess = vi.fn();
    render(<Login onSuccess={onSuccess} />);
    const checkbox = screen.getByRole('checkbox', { name: '下次自动登录（30 天）' }) as HTMLInputElement;
    expect(checkbox.checked).toBe(false); if (remember) fireEvent.click(checkbox);
    fireEvent.change(screen.getByLabelText('管理员密钥'), { target: { value: ' test-key ' } }); fireEvent.click(screen.getByRole('button', { name: '进入控制台' }));
    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce());
    expect(fetcher).toHaveBeenCalledOnce(); expect(fetcher.mock.calls[0][0]).toBe('/api/auth/login');
    expect(fetcher.mock.calls[0][1].method).toBe('POST');
    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({ key: 'test-key', remember });
    expect(sessionStorage.length).toBe(0); expect(localStorage.length).toBe(0);
    expect((screen.getByLabelText('管理员密钥') as HTMLInputElement).value).toBe('');
  });
  it('creates a named client key once, copies it, hides it on close, and confirms revocation', async () => {
    const clientKey = { id: 7, name: 'Desktop Agent', prefix: 'tr-client', created_at: '2026-09-17T10:00:00Z', last_used_at: null, scope: { model_mode: 'all', model_ids: [], tag_rules: {} } };
    let list: typeof clientKey[] = [];
    const fetcher = vi.fn().mockImplementation((url, options) => {
      if (url === '/api/connection') return Promise.resolve(response({ origin: 'http://localhost:8787' }));
      if (url === '/api/channels') return Promise.resolve(response([channel]));
      if (url === '/api/models') return Promise.resolve(response([{ id: 'model-a', enabled: true, fallback: true, config: {}, bindings: [{ channel_id: 12, model: 'model-a', priority: 0, enabled: true, channel_exists: true }] }]));
      if (pathOf(url) === '/api/client-keys/7/key') return Promise.resolve(response({ key: 'raw-revealed-key' }));
      if (options.method === 'POST') { list = [clientKey]; return Promise.resolve(response({ client_key: clientKey, key: 'raw-once-only' })); }
      if (options.method === 'DELETE') { list = []; return Promise.resolve(new Response(null, { status: 204 })); }
      return Promise.resolve(response(list));
    });
    const copy = vi.fn().mockResolvedValue(undefined); Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: copy } });
    stubFetch(fetcher); const { unmount } = render(<ClientAccess />);
    await screen.findByText('尚未创建客户端 API Key');
    fireEvent.click(screen.getAllByRole('button', { name: '复制 Base URL' })[0]);
    expect(copy).toHaveBeenLastCalledWith('http://localhost:8787/v1');
    fireEvent.click(screen.getByRole('button', { name: '复制 OpenAI 配置' }));
    expect(JSON.parse(copy.mock.calls[1][0])).toEqual({ baseURL: 'http://localhost:8787/v1', apiKey: '<CLIENT_API_KEY>' });
    fireEvent.click(screen.getByRole('button', { name: '复制 Anthropic 配置' }));
    expect(JSON.parse(copy.mock.calls[2][0]).baseURL).toBe('http://localhost:8787');
    fireEvent.click(screen.getByRole('button', { name: '复制 Gemini 配置' }));
    expect(JSON.parse(copy.mock.calls[3][0]).httpOptions.baseUrl).toBe('http://localhost:8787');
    expect(screen.getByText('model-a')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '创建 API Key' }));
    fireEvent.change(screen.getByLabelText('密钥名称'), { target: { value: ' Desktop Agent ' } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));
    expect((await screen.findByLabelText('新客户端 API Key') as HTMLInputElement).value).toBe('raw-once-only');
    expect(screen.getByText(/关闭后可在列表中复制/)).toBeTruthy();
    expect(JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'POST')![1].body)).toEqual({ name: 'Desktop Agent', scope: { model_mode: 'all', model_ids: [], tag_rules: {} } });
    fireEvent.click(screen.getByRole('button', { name: '复制客户端密钥' })); expect(copy).toHaveBeenLastCalledWith('raw-once-only');
    fireEvent.click(screen.getByRole('button', { name: '已保存，关闭' })); expect(screen.queryByDisplayValue('raw-once-only')).toBeNull();
    expect(sessionStorage.length).toBe(0); expect(localStorage.length).toBe(0);
    await screen.findByText('尚未使用');
    fireEvent.click(screen.getByRole('button', { name: '复制密钥 Desktop Agent' }));
    await waitFor(() => expect(copy).toHaveBeenLastCalledWith('raw-revealed-key'));
    fireEvent.click(screen.getByRole('button', { name: '撤销 Desktop Agent' }));
    expect(fetcher.mock.calls.some(call => call[1].method === 'DELETE')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    fireEvent.click(screen.getByRole('button', { name: '撤销 Desktop Agent' })); fireEvent.click(screen.getByRole('button', { name: '确认撤销' }));
    await screen.findByText('尚未创建客户端 API Key');
    expect(fetcher.mock.calls.find(call => call[1].method === 'DELETE')![0]).toBe('/api/client-keys/7');
    expect(fetcher.mock.calls.every(call => String(call[0]).startsWith('/api/'))).toBe(true);
    unmount(); render(<ClientAccess />); await screen.findByText('尚未创建客户端 API Key'); expect(screen.queryByDisplayValue('raw-once-only')).toBeNull();
  });
  it('toggles a channel with blank key and no read-only fields', async () => {
    const fetcher = vi.fn().mockImplementation((_url, options) => Promise.resolve(response(options.method === 'PUT' ? channel : [channel]))); stubFetch(fetcher);
    render(<Channels />); const toggle = await screen.findByRole('switch'); fireEvent.click(toggle);
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const saved = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(saved.enabled).toBe(false); expect(saved.api_key).toBe(''); expect(saved).not.toHaveProperty('has_key'); expect(saved.id).toBe(12);
  });
  it('saves the listen port and controls the service without touching settings', async () => {
    const settings = { gateway_key: 'old-key', prices: [], retention_days: 30, max_attempts: 3, retries_per_channel: 5, failure_cooldown_seconds: 10, request_defaults_enabled: false, request_defaults: { max_output_tokens: 4096 } };
    let savedPort = 0;
    vi.spyOn(restart, 'restartConsole').mockImplementation(async progress => { progress('服务正在重启，等待恢复…'); await api('/restart', { method: 'POST', body: '{}' }); throw new Error('确认恢复超时，请查看 to-router.log。'); });
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      if (url === '/api/channels') return Promise.resolve(response([]));
      if (url === '/api/runtime/port' && options.method === 'PUT') { savedPort = JSON.parse(options.body).port; return Promise.resolve(response({ port: savedPort, restart_required: true })); }
      if (url === '/api/runtime/port') return Promise.resolve(response({ port: 17878, origin: 'http://127.0.0.1:17878' }));
      if (url === '/api/restart') return Promise.resolve(response({ ok: true }));
      if (url === '/api/settings') return Promise.resolve(response(settings));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<ToastProvider><Settings /></ToastProvider>);
    const port = (await screen.findByLabelText('监听端口')) as HTMLInputElement;
    await waitFor(() => expect(port.value).toBe('17878'));
    expect(screen.queryByText(/无法在此修改/)).toBeNull();
    expect(screen.getByText(/启动参数.*保存的端口.*17878/)).toBeTruthy();
    expect(screen.getByText(/仅对.*429\/503.*连接拒绝.*DNS/)).toBeTruthy();
    expect(screen.getByText(/读超时.*连接复位.*408.*500\/502\/504.*不.*重发/)).toBeTruthy();
    fireEvent.change(port, { target: { value: '18888' } });
    fireEvent.click(screen.getByRole('button', { name: '保存端口' }));
    await waitFor(() => expect(savedPort).toBe(18888));
    expect(await screen.findByText('已保存端口 18888，重启后生效。')).toBeTruthy();
    // The port control must not submit or invalidate the settings form.
    expect(fetcher.mock.calls.some(call => call[0] === '/api/settings' && call[1].method === 'PUT')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: '重启服务' }));
    expect(screen.getByText(/若启动失败.*to-router.log/)).toBeTruthy();
    fireEvent.click(await screen.findByRole('button', { name: '确认重启' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/restart' && call[1].method === 'POST')).toBe(true));
    expect((await screen.findAllByText('确认恢复超时，请查看 to-router.log。')).length).toBe(1);
    expect((screen.getByRole('button', { name: '确认重启' }) as HTMLButtonElement).disabled).toBe(false);
  });
  it('keeps request defaults disabled until explicitly enabled and preserves configured zero values', async () => {
    const settings = { gateway_key: 'old-key', prices: [], retention_days: 30, max_attempts: 3, retries_per_channel: 5, failure_cooldown_seconds: 10, request_defaults_enabled: false, request_defaults: { max_output_tokens: 0, temperature: 0, thinking: { mode: 'enabled', enabled: false } } };
    const fetcher = vi.fn().mockImplementation((url, options = {}) => Promise.resolve(response(url === '/api/runtime/port' ? { port: 17878, origin: 'http://127.0.0.1:17878' } : options.method === 'PUT' ? {} : settings)));
    stubFetch(fetcher); render(<Settings />);
    expect(await screen.findByLabelText('启用请求兜底参数')).toHaveProperty('checked', false);
    expect(screen.getByLabelText('默认 temperature')).toHaveProperty('value', '0'); expect(screen.getByLabelText('默认思考开关')).toHaveProperty('value', 'false');
    fireEvent.click(screen.getByLabelText('启用请求兜底参数')); fireEvent.click(screen.getAllByRole('button', { name: '保存设置' })[0]);
    await waitFor(() => expect(fetcher.mock.calls.some(([, init]) => init.method === 'PUT')).toBe(true));
    const body = JSON.parse(fetcher.mock.calls.find(([, init]) => init.method === 'PUT')![1].body);
    expect(body.request_defaults_enabled).toBe(true); expect(body.request_defaults).toEqual(settings.request_defaults); expect(body).not.toHaveProperty('config'); expect(body).not.toHaveProperty('prices');
  });
  it('saves the global output default without expiring an unchanged admin credential', async () => {
    const settings = { gateway_key: 'old-key', prices: [], retention_days: 30, max_attempts: 3, retries_per_channel: 5, failure_cooldown_seconds: 10, request_defaults_enabled: false, request_defaults: { max_output_tokens: 4096 } };
    const fetcher = vi.fn().mockImplementation((url, options) => Promise.resolve(response(url === '/api/channels' ? [] : url === '/api/runtime/port' ? { port: 17878, origin: 'http://127.0.0.1:17878' } : options.method === 'PUT' ? {} : settings)));
    const expired = vi.fn(); window.addEventListener('auth-expired', expired); stubFetch(fetcher);
    try {
      render(<Settings />); fireEvent.change(await screen.findByLabelText('默认最大输出'), { target: { value: '8192' } });
      fireEvent.click(screen.getAllByRole('button', { name: '保存设置' })[0]);
      await waitFor(() => expect(fetcher.mock.calls.filter(call => call[0] === '/api/settings' && !call[1].method)).toHaveLength(2));
      expect(JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body).request_defaults.max_output_tokens).toBe(8192);
      expect(expired).not.toHaveBeenCalled();
    } finally { window.removeEventListener('auth-expired', expired); }
  });
  it('saves the failure cooldown from the request policy section', async () => {
    const settings = { gateway_key: 'old-key', prices: [], retention_days: 30, max_attempts: 3, retries_per_channel: 5, failure_cooldown_seconds: 10, request_defaults_enabled: false, request_defaults: { max_output_tokens: 4096 } };
    const fetcher = vi.fn().mockImplementation((url, options) => Promise.resolve(response(url === '/api/channels' ? [] : url === '/api/runtime/port' ? { port: 17888, origin: 'http://127.0.0.1:17888' } : options.method === 'PUT' ? {} : settings)));
    stubFetch(fetcher);
    render(<Settings />);
    fireEvent.change(await screen.findByLabelText(/失败冷却时间/), { target: { value: '120' } });
    fireEvent.click(screen.getAllByRole('button', { name: '保存设置' })[0]);
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/settings' && call[1].method === 'PUT')).toBe(true));
    expect(JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body).failure_cooldown_seconds).toBe(120);
  });
  it('saves the OAuth auto-open switch, which a legacy settings row leaves on', async () => {
    // No oauth_auto_open key: the switch from before it existed.
    const settings = { gateway_key: 'old-key', prices: [], retention_days: 30, max_attempts: 3, retries_per_channel: 5, failure_cooldown_seconds: 10, request_defaults_enabled: false, request_defaults: { max_output_tokens: 4096 } };
    const fetcher = vi.fn().mockImplementation((url, options) => Promise.resolve(response(url === '/api/channels' ? [] : url === '/api/runtime/port' ? { port: 17878, origin: 'http://127.0.0.1:17878' } : options.method === 'PUT' ? {} : settings)));
    stubFetch(fetcher);
    render(<Settings />);
    const toggle = (await screen.findByLabelText(/授权时自动打开浏览器/)) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
    fireEvent.click(toggle);
    fireEvent.click(screen.getAllByRole('button', { name: '保存设置' })[0]);
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/settings' && call[1].method === 'PUT')).toBe(true));
    expect(JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body).oauth_auto_open).toBe(false);
  });
  it('expires the session on admin key change without reloading or automatically logging in', async () => {
    const settings = { gateway_key: 'old-key', prices: [], retention_days: 30, max_attempts: 3, retries_per_channel: 5, failure_cooldown_seconds: 10, request_defaults_enabled: false, request_defaults: { max_output_tokens: 4096 } };
    const fetcher = vi.fn().mockImplementation((url, options) => Promise.resolve(response(url === '/api/channels' ? [] : url === '/api/runtime/port' ? { port: 17878, origin: 'http://127.0.0.1:17878' } : options.method === 'PUT' ? {} : settings)));
    stubFetch(fetcher); const expired = vi.fn(); window.addEventListener('auth-expired', expired);
    try {
      render(<Settings />);
      const input = await screen.findByLabelText(/管理员登录密钥/); fireEvent.change(input, { target: { value: 'new-key' } });
      fireEvent.click(screen.getAllByRole('button', { name: '保存设置' })[0]);
      await waitFor(() => expect(expired).toHaveBeenCalledOnce());
      expect(fetcher.mock.calls.filter(call => call[0] === '/api/settings' && !call[1].method)).toHaveLength(1);
      expect(fetcher.mock.calls.some(call => call[0] === '/api/auth/login')).toBe(false);
      expect(sessionStorage.length).toBe(0); expect(localStorage.length).toBe(0);
    } finally { window.removeEventListener('auth-expired', expired); }
  });
});

describe('downstream models page', () => {
  const channelRow: Channel = { ...channel, id: 12, models: ['model-a', 'model-b'], available_models: ['model-a', 'model-b', 'shared-model'] };
  const view = {
    id: 'public', enabled: true, fallback: true, config: { max_output_tokens: 0 },
    bindings: [{
      channel_id: 12, model: 'model-a', priority: 0, enabled: true, channel_exists: true, channel_name: 'Personal', protocol: 'openai',
      effective: { config: { max_output_tokens: 0, streaming: true }, sources: { max_output_tokens: '下游模型', streaming: '上游提供商' } },
    }],
  };
  function mockBackend() {
    return vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url === '/api/channels') return Promise.resolve(response([channelRow]));
      if (url === '/api/models' && method === 'POST') return Promise.resolve(response(JSON.parse(options.body)));
      if (url.startsWith('/api/models/') && method === 'PUT') return Promise.resolve(response(JSON.parse(options.body)));
      if (url.startsWith('/api/models/') && method === 'DELETE') return Promise.resolve(response({ ok: true }));
      return Promise.resolve(response([view]));
    });
  }
  it('lists models with bindings and effective value sources', async () => {
    stubFetch(mockBackend());
    render(<Models />);
    expect((await screen.findByRole('heading', { name: 'public' })).textContent).toBe('public');
    expect(screen.getByText('Personal')).toBeTruthy();
    expect(screen.getByText(/1\/1 个绑定启用/)).toBeTruthy();
    expect(screen.getByText(/来源：下游模型/)).toBeTruthy();
    expect(screen.getByText(/来源：上游提供商/)).toBeTruthy();
  });
  it('creates a downstream model with a searchable binding select', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '新增公开模型' }));
    fireEvent.change(await screen.findByLabelText('对外模型 ID'), { target: { value: 'my-model' } });
    fireEvent.click(screen.getByRole('button', { name: '添加绑定' }));
    const provider = screen.getByLabelText('上游提供商') as HTMLSelectElement;
    expect(provider.value).toBe('12');
    const model = screen.getByLabelText('上游模型') as HTMLSelectElement;
    expect(model.value).toBe('model-a');
    fireEvent.change(model, { target: { value: 'model-b' } });
    fireEvent.click(screen.getByRole('button', { name: '创建模型' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/models' && call[1].method === 'POST')).toBe(true));
    const posted = JSON.parse(fetcher.mock.calls.find(call => call[0] === '/api/models' && call[1].method === 'POST')![1].body);
    expect(posted).toMatchObject({ id: 'my-model', enabled: true, fallback: true, bindings: [{ channel_id: 12, model: 'model-b', priority: 0, enabled: true }] });
    expect(posted).not.toHaveProperty('config');
  });
  it('has no editable capability or price overrides on public models', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '新增公开模型' }));
    expect(screen.queryByLabelText('设置 最大输出 tokens')).toBeNull();
    expect(screen.queryByLabelText('默认最大输出')).toBeNull();
    expect(screen.queryByLabelText('官方价 输入')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '公开规格' }));
    expect(screen.getByRole('button', { name: '管理官方 API 价 ↗' }).hasAttribute('disabled')).toBe(true);
  });
  it('saves an existing model without read-only view fields', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑模型 public' }));
    fireEvent.click(await screen.findByRole('button', { name: '添加绑定' }));
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.id).toBe('public');
    expect(put.bindings).toEqual([
      { channel_id: 12, model: 'model-a', priority: 0, enabled: true },
      { channel_id: 12, model: 'model-a', priority: 1, enabled: true },
    ]);
    for (const binding of put.bindings) {
      expect(Object.keys(binding).sort()).toEqual(['channel_id', 'enabled', 'model', 'priority']);
    }
  });
  it('toggles and deletes with undo snackbar or instant commit', async () => {
    const fetcher = mockBackend(); stubFetch(fetcher);
    render(<Models />);
    fireEvent.click(await screen.findByRole('switch', { name: '停用 public' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'PUT')).toBe(true));
    const putBody = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(putBody.enabled).toBe(false);
    expect(putBody.channel_exists).toBeUndefined();
    expect(putBody.bindings[0].channel_exists).toBeUndefined();
    fireEvent.click(screen.getByRole('button', { name: '删除模型 public' }));
    expect(screen.getByText(/已移除「public」/)).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: '立即生效' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1].method === 'DELETE')).toBe(true));
    expect(fetcher.mock.calls.find(call => call[1].method === 'DELETE')![0]).toBe('/api/models/public');
  });
  it('refuses an empty binding list and explains why', async () => {
    stubFetch(mockBackend());
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '新增公开模型' }));
    fireEvent.change(screen.getByLabelText('对外模型 ID'), { target: { value: 'my-model' } });
    fireEvent.click(screen.getByRole('button', { name: '创建模型' }));
    expect((await screen.findByRole('alert')).textContent).toContain('至少需要一个上游绑定');
  });
  it('shows the catalogue-gap notice a re-enable answers with', async () => {
    const notice = '上游当前目录中没有绑定模型 model-a（上游可能已下线或已更名，也可能这次未拉取到）；已照常启用，请自行确认可用性。';
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([channelRow]));
      if (url.startsWith('/api/models') && options.method) return Promise.resolve(response({ model: view, notice }));
      return Promise.resolve(response([view]));
    });
    stubFetch(fetcher);
    render(<ToastProvider><Models /></ToastProvider>);
    fireEvent.click(await screen.findByRole('button', { name: '编辑模型 public' }));
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    expect(await screen.findByText(/上游可能已下线/)).toBeTruthy();
    const put = JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body);
    expect(put.id).toBe('public');
  });
});

describe('request logs format labels', () => {
  const log = (inbound: string, outbound: string) => ({ id: 7, time: '2026-10-02T02:00:00Z', channel_id: 12, channel_name: 'Personal', model: 'model-a', upstream_model: 'model-a', inbound, outbound, input_tokens: 100, output_tokens: 50, cache_read_tokens: 10, cache_write_tokens: 0, estimated: false, cost: 0.01, duration_ms: 1200, ttft_ms: 120, status: 200, error: '' });
  function renderLogs(rows: Record<string, unknown>[]) {
    stubFetch(vi.fn().mockImplementation((url: string) =>
      url === '/api/channels' ? Promise.resolve(response([channel])) : Promise.resolve(response(rows))));
    render(<Logs />);
  }
  it('labels endpoint formats and marks rows written before the split', () => {
    expect(usageFormatLabel('openai_chat_completions')).toBe('OpenAI Chat');
    expect(usageFormatLabel('openai_responses')).toBe('OpenAI Responses');
    expect(usageFormatLabel('anthropic_messages')).toBe('Anthropic Messages');
    expect(usageFormatLabel('gemini_generate_content')).toBe('Gemini');
    // "openai" once covered both facades; it is recorded as legacy, not guessed.
    expect(usageFormatLabel('openai')).toContain('旧');
    expect(usageFormatLabel('anthropic')).toContain('旧');
    expect(usageFormatLabel('gemini')).toContain('旧');
  });
  it('renders endpoint formats and legacy family values in the log list', async () => {
    renderLogs([log('openai_chat_completions', 'openai_responses'), log('openai', 'anthropic')]);
    await screen.findByText('OpenAI Chat → OpenAI Responses');
    expect(screen.getByText('OpenAI（旧） → Anthropic（旧）')).toBeTruthy();
    expect(screen.queryByText('openai → anthropic')).toBeNull();
  });
  it('shows the shared labels in the request detail panel', async () => {
    renderLogs([log('anthropic_messages', 'gemini_generate_content')]);
    fireEvent.click(await screen.findByRole('button', { name: '查看请求 7 详情' }));
    const inbound = await screen.findByText('Anthropic Messages');
    expect(inbound.closest('dd')?.textContent).toBe('Anthropic Messages');
    expect(screen.getByText('Gemini').closest('dd')?.textContent).toBe('Gemini');
  });
  it('shows native state and both adaptation directions with explanations and original codes', async () => {
    renderLogs([{ ...log('openai_chat_completions', 'gemini_generate_content'), stateful: true,
      degraded: 'thought_signature_bypass,thought_signature,thinking,reasoning_content,future_loss,toString' }]);
    expect(await screen.findByText('原生状态')).toBeTruthy();
    expect(screen.getByText('有损转换')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '查看请求 7 详情' }));
    const detail = screen.getByRole('dialog');
    expect(within(detail).getByText(/请求携带上游原生状态/)).toBeTruthy();
    expect(within(detail).getByText(/请求转换.*签名旁路/)).toBeTruthy();
    expect(within(detail).getByText(/响应转换.*思考签名.*剥离/)).toBeTruthy();
    expect(within(detail).getByText(/响应转换.*思考内容.*剥离/)).toBeTruthy();
    expect(within(detail).getByText(/响应转换.*推理内容.*剥离/)).toBeTruthy();
    expect(within(detail).getByText('future_loss')).toBeTruthy();
    expect(within(detail).getAllByText(/尚无中文解释/)).toHaveLength(2);
    expect(Array.from(detail.querySelectorAll('code')).map(el => el.textContent)).toEqual([
      'thought_signature_bypass', 'thought_signature', 'thinking', 'reasoning_content', 'future_loss', 'toString',
    ]);
  });
  it.each([false, undefined])('distinguishes absent state evidence (%s) and empty degradation in detail', async stateful => {
    renderLogs([{ ...log('openai_chat_completions', 'openai_chat_completions'), stateful, degraded: '' }]);
    fireEvent.click(await screen.findByRole('button', { name: '查看请求 7 详情' }));
    const detail = screen.getByRole('dialog');
    expect(within(detail).getByText(stateful === false ? '未检测到上游原生状态' : '未记录')).toBeTruthy();
    expect(within(detail).getByText('无已记录的转换损失')).toBeTruthy();
    expect(screen.queryByText('有损转换')).toBeNull();
  });
  it('keeps an incomplete HTTP 200 response marked as a failure and exposes the cause', async () => {
    renderLogs([{ ...log('openai_chat_completions', 'openai_responses'), status: 200, error: 'upstream response incomplete: max_output_tokens' }]);
    const status = await screen.findByText('200');
    expect(status.className).toContain('badge-red');
    fireEvent.click(screen.getByRole('button', { name: '查看请求 7 详情' }));
    expect(within(screen.getByRole('dialog')).getByText('upstream response incomplete: max_output_tokens')).toBeTruthy();
  });
});

describe('OAuth tracks in the channel editor', () => {
  const oauthChannel: Channel = oauthFixture({
    id: 21, name: 'Copilot', api_key: '', has_key: false, has_oauth: true, models: ['gpt-5'], enabled: true,
    endpoints: [{ format: 'openai_chat_completions', base_url: 'https://api.githubcopilot.com' }],
    credential_type: 'copilot_oauth',
    oauth: { provider: 'copilot', account_label: 'octocat', status: 'active', expires_at: '2026-10-03T00:00:00Z', updated_at: '2026-10-02T00:00:00Z' },
  });
  function mockOAuth(routes: Record<string, (body: unknown) => unknown>, settings: unknown = { oauth_auto_open: true }) {
    return vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      // The flow view reads the auto-open preference from the settings row.
      if (pathOf(url) === '/api/settings') return Promise.resolve(response(settings));
      const match = Object.keys(routes).find(route => url.startsWith('/api' + route));
      if (match && match.endsWith('/start') && method === 'POST') return Promise.resolve(response(routes[match](options.body ? JSON.parse(options.body) : {})));
      if (match && match.endsWith('/status')) return Promise.resolve(response(routes[match](undefined)));
      if (pathOf(url) === '/api/channels') { const type = new URLSearchParams(String(url).split('?')[1] || '').get('credential_type'); return Promise.resolve(response(!type || type === 'api_key' || type === oauthChannel.credential_type ? [oauthChannel] : [])); }
      return Promise.resolve(response({}, 404));
    });
  }
  async function openOAuthEditor() {
    render(<Channels />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Copilot' }));
    await editorSection('模型配置');
    return screen.findByLabelText('搜索模型');
  }
  it.each(['claude_oauth', 'antigravity_oauth', 'devin_oauth'] as const)('cancels %s without reporting an expired resumed flow', async type => {
    let finishStatus!: (value: Response) => void;
    const pending = new Promise<Response>(resolve => { finishStatus = resolve; });
    const route = type.replace('_oauth', '');
    const gone = vi.fn(), started = vi.fn();
    const fetcher = vi.fn((url: string) => {
      if (url.includes('/status?')) return pending;
      return Promise.resolve(response(url === '/api/settings' ? { oauth_auto_open: false } : { status: 'cancelled' }));
    });
    stubFetch(fetcher);
    render(<OAuthAuthorize type={type} resumeFlow="synthetic-resumed" onDone={() => {}} onStarted={started} onResumeGone={gone} />);
    fireEvent.click(await screen.findByRole('button', { name: '取消授权' }));
    await waitFor(() => expect(started).toHaveBeenCalledWith(null));
    await act(async () => { finishStatus(response({ error: { message: 'flow cancelled' } }, 404)); await pending; });
    expect(gone).not.toHaveBeenCalled();
    expect(fetcher.mock.calls.some(([url]) => url === `/api/oauth/${route}/cancel?flow=synthetic-resumed`)).toBe(true);
  });
  it('hides the key field and shows the copilot device-code entry', async () => {
    const fetcher = mockOAuth({ '/oauth/copilot/start': () => ({ flow: 'f1', user_code: 'ABCD-1234', verification_uri: 'https://github.com/login/device', expires: '2026-10-02T01:00:00Z', interval: 5 }) });
    stubFetch(fetcher);
    await openOAuthEditor();
    expect(screen.queryByLabelText('API 密钥')).toBeNull();
    await editorSection('模型配置');
    expect(screen.getByRole('button', { name: '从上游获取模型' })).toBeTruthy();
    expect(screen.getByText(/在这里或插件渠道的菜单中获取模型目录/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: /重新授权/ })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    fireEvent.click(await screen.findByRole('tab', { name: /Copilot/ }));
    const group = await screen.findByRole('tabpanel', { name: /Copilot/ });
    fireEvent.click(within(group).getByRole('button', { name: '向 Copilot 添加账号 / 工作区' }));
    fireEvent.click(screen.getByRole('button', { name: /开始设备码授权/ }));
    expect(await screen.findByText('ABCD-1234')).toBeTruthy();
    expect(screen.getByRole('link', { name: /打开验证页面/ }).getAttribute('href')).toBe('https://github.com/login/device');
  });
  it('probes an OAuth channel model through the stored channel credential', async () => {
    // T032: the model row offers the test entry the API key rows have, and the
    // probe rides the credential the gateway already holds for this channel —
    // the console names the stored channel and sends only the model and the
    // confirmation, never a credential-bearing draft.
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (pathOf(url) === '/api/settings') return Promise.resolve(response({ oauth_auto_open: true }));
      if (url === '/api/channels/21/probe' && method === 'POST') {
        const body = JSON.parse(options.body);
        return Promise.resolve(response(body.confirmed ? { ok: true, model: body.model, duration_ms: 90, upstream_status: 200 } : { ok: false, model: body.model, duration_ms: 1 }));
      }
      if (pathOf(url) === '/api/channels') { const type = new URLSearchParams(String(url).split('?')[1] || '').get('credential_type'); return Promise.resolve(response(!type || type === 'api_key' || type === oauthChannel.credential_type ? [oauthChannel] : [])); }
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    await openOAuthEditor();
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '测试模型 gpt-5' }));
    expect(await screen.findByText('将调用：gpt-5')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '确认并测试（可能收费）' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/21/probe')).toBe(true));
    const probe = fetcher.mock.calls.find(call => call[0] === '/api/channels/21/probe')!;
    const body = JSON.parse(probe[1].body);
    expect(body.model).toBe('gpt-5'); expect(body.confirmed).toBe(true);
    // The stored channel's own credential answers, and the draft rides along
    // for its destination only: the body names the endpoints the editor shows,
    // and the server reads the credential from the stored row it names.
    expect(body.channel.endpoints).toEqual([{ format: 'openai_chat_completions', base_url: 'https://api.githubcopilot.com' }]);
    expect(body.channel.api_key).toBe('');
    expect(await screen.findByText(/测试成功/)).toBeTruthy();
    // The draft path is not used for a stored OAuth account: no credential
    // leaves the console on it either.
    expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/probe')).toBe(false);
  });
  it('probes the endpoint address the OAuth editor shows, not the one still stored', async () => {
    // The editor is where a re-pointed endpoint is edited, so the test has to
    // answer for what it shows: the stored probe carries the draft's endpoint,
    // and the server tests that address instead of the one the row still holds.
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (pathOf(url) === '/api/settings') return Promise.resolve(response({ oauth_auto_open: true }));
      if (url === '/api/channels/21/probe' && method === 'POST') {
        const body = JSON.parse(options.body);
        return Promise.resolve(response(body.confirmed ? { ok: true, model: body.model, duration_ms: 90, upstream_status: 200 } : { ok: false, model: body.model, duration_ms: 1 }));
      }
      if (pathOf(url) === '/api/channels') { const type = new URLSearchParams(String(url).split('?')[1] || '').get('credential_type'); return Promise.resolve(response(!type || type === 'api_key' || type === oauthChannel.credential_type ? [oauthChannel] : [])); }
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    await openOAuthEditor();
    fireEvent.change(await endpointInput('openai_chat_completions'), { target: { value: 'https://moved.example.test' } });
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '测试模型 gpt-5' }));
    expect(await screen.findByText('将调用：gpt-5')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '确认并测试（可能收费）' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/21/probe')).toBe(true));
    const probe = JSON.parse(fetcher.mock.calls.find(call => call[0] === '/api/channels/21/probe')![1].body);
    expect(probe.channel.endpoints).toEqual([{ format: 'openai_chat_completions', base_url: 'https://moved.example.test' }]);
    // The credential is still the stored row's: no key travels with the draft.
    expect(probe.channel.api_key).toBe('');
    expect(await screen.findByText(/测试成功/)).toBeTruthy();
  });
  it('offers both codex modes and flags the track as non-official', async () => {
    const fetcher = mockOAuth({ '/oauth/codex/start': () => ({ flow: 'f2', method: 'browser', authorization_url: 'https://chatgpt.com/oauth/authorize?x=1', redirect_uri: 'http://localhost:1455/auth/callback', callback_path: '/auth/callback', non_official: '非 OpenAI 官方支持路径' }) });
    stubFetch(fetcher);
    render(<OAuthAuthorize type='codex_oauth' operation='reauth' credentialID='cred_1' channelID={21} onDone={() => {}} />);
    expect(screen.getByRole('button', { name: '浏览器授权' })).toBeTruthy();
    expect(screen.getByRole('button', { name: '设备码授权' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
    expect(await screen.findByText(/非 OpenAI 官方支持路径/)).toBeTruthy();
    expect(screen.getByLabelText('回调链接')).toBeTruthy();
  });
  it('polls a codex device flow to completion and refreshes the list', async () => {
    const fetcher = mockOAuth({
      '/oauth/codex/start': () => ({ flow: 'f3', method: 'device', user_code: 'CODE-ZZ', verification_uri: 'https://chatgpt.com/codex/device', expires: '2026-10-02T01:00:00Z', interval: 1 }),
      '/oauth/codex/status': () => ({ status: 'complete', channel_id: 22 }),
    });
    stubFetch(fetcher);
    render(<OAuthAuthorize type='codex_oauth' operation='reauth' credentialID='cred_1' channelID={21} onDone={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: '设备码授权' }));
    expect(await screen.findByText('CODE-ZZ')).toBeTruthy();
    await waitFor(() => expect(fetcher.mock.calls.some(call => String(call[0]).includes('/oauth/codex/status'))).toBe(true));
  });
  it('opens the SIWC consent page and accepts a pasted callback link', async () => {
    const fetcher = mockOAuth({ '/oauth/chatgpt/start': () => ({ flow: 'f4', method: 'browser', authorization_url: 'https://auth.openai.com/api/accounts/authorize?client_id=dynamic_agent_client', redirect_uri: 'http://127.0.0.1:51000/auth/callback', callback_path: '/auth/callback', paste_hint: '浏览器无法自动跳转时粘贴链接' }) });
    stubFetch(fetcher);
    render(<OAuthAuthorize type='chatgpt_oauth' operation='reauth' credentialID='cred_1' channelID={21} onDone={() => {}} />);
    fireEvent.click(await screen.findByRole('button', { name: '开始授权' }));
    expect(await screen.findByText('授权链接')).toBeTruthy();
    expect(screen.getByText(/dynamic_agent_client/)).toBeTruthy();
    expect(screen.getByRole('button', { name: '复制本机回调地址' })).toBeTruthy();
    fireEvent.change(screen.getByLabelText('回调链接'), { target: { value: 'http://127.0.0.1:51000/auth/callback?code=c&state=s' } });
    fireEvent.click(screen.getByRole('button', { name: '提交' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => String(call[0]).includes('/oauth/chatgpt/status'))).toBe(true));
  });
  // The consent page opens in a tab the click itself created: the window exists
  // synchronously (which is what a popup blocker allows), and the start answer
  // only navigates it. With the switch off nothing is navigated at all — the
  // tab the click opened is closed again, and the link plus its copy button are
  // the way.
  // A stand-in for the tab the click opens: it records what was navigated to,
  // the way a real window's location assignment would.
  const fakePopup = () => {
    const navigated: string[] = [];
    const popup = { close: vi.fn(), navigated } as unknown as Window & { close: ReturnType<typeof vi.fn>; navigated: string[] };
    Object.defineProperty(popup, 'location', { set: (url: string) => { navigated.push(String(url)); }, get: () => ({ href: navigated[navigated.length - 1] || 'about:blank' }) as Location });
    return popup;
  };
  it('opens the consent page on its own while the auto-open switch is on', async () => {
    const popup = fakePopup();
    const open = vi.spyOn(window, 'open').mockReturnValue(popup);
    const fetcher = mockOAuth({
      '/oauth/chatgpt/start': () => ({ flow: 'f4', method: 'browser', authorization_url: 'https://auth.openai.com/api/accounts/authorize?client_id=dynamic_agent_client', redirect_uri: 'http://127.0.0.1:51000/auth/callback', callback_path: '/auth/callback' }),
    });
    stubFetch(fetcher);
    render(<OAuthAuthorize type='chatgpt_oauth' operation='reauth' credentialID='cred_1' channelID={21} onDone={() => {}} />);
    fireEvent.click(await screen.findByRole('button', { name: '开始授权' }));
    await screen.findByRole('button', { name: '复制授权链接' });
    // The tab is opened by the click, before any await, and without noopener:
    // the opener reference is the only way the start answer can navigate it.
    expect(open).toHaveBeenCalledWith('about:blank', '_blank');
    expect(open.mock.calls[0]).toHaveLength(2);
    // ...and the consent page navigates it once the flow has started.
    expect(popup.navigated).toEqual(['https://auth.openai.com/api/accounts/authorize?client_id=dynamic_agent_client']);
    expect(popup.close).not.toHaveBeenCalled();
  });
  it('falls back to the rendered link when the browser hands back no window', async () => {
    // A null handle is what a blocker (and, by standard, noopener) returns.
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const fetcher = mockOAuth({
      '/oauth/codex/start': () => ({ flow: 'f6', method: 'browser', authorization_url: 'https://chatgpt.com/oauth/authorize?x=1', redirect_uri: 'http://localhost:1455/auth/callback', callback_path: '/auth/callback' }),
    });
    stubFetch(fetcher);
    render(<OAuthAuthorize type='codex_oauth' operation='reauth' credentialID='cred_1' channelID={21} onDone={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
    await screen.findByRole('button', { name: '复制授权链接' });
    // No navigation was attempted on a handle that does not exist; the link the
    // panel renders is the way, and the hint says so.
    expect(open.mock.calls).toHaveLength(1);
    expect(screen.getByText(/浏览器没有交出自动打开的标签页/)).toBeTruthy();
  });
  it('shows only the link when the auto-open switch is off', async () => {
    const popup = fakePopup();
    const open = vi.spyOn(window, 'open').mockReturnValue(popup);
    const fetcher = mockOAuth({
      '/oauth/codex/start': () => ({ flow: 'f2', method: 'browser', authorization_url: 'https://chatgpt.com/oauth/authorize?x=1', redirect_uri: 'http://localhost:1455/auth/callback', callback_path: '/auth/callback' }),
    }, { oauth_auto_open: false });
    stubFetch(fetcher);
    render(<OAuthAuthorize type='codex_oauth' operation='reauth' credentialID='cred_1' channelID={21} onDone={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
    // The link is the way: shown with its copy button, and the never-used tab is
    // closed rather than navigated.
    await screen.findByRole('button', { name: '复制授权链接' });
    expect(screen.getByText(/自动打开已关闭/)).toBeTruthy();
    expect(popup.navigated).toEqual([]);
    expect(popup.close).toHaveBeenCalled();
    expect(open).not.toHaveBeenCalledWith('https://chatgpt.com/oauth/authorize?x=1', '_blank', 'noopener,noreferrer');
  });
  // A degraded answer is not a failure: the channel exists, so the panel stays
  // and says what is missing instead of unmounting on its own.
  it('keeps the panel open and names the degraded steps when the login had any', async () => {
    const fetcher = mockOAuth({
      '/oauth/copilot/start': () => ({ flow: 'f9', user_code: 'WXYZ-9999', verification_uri: 'https://github.com/login/device', expires: '2026-10-02T01:00:00Z', interval: 1 }),
      '/oauth/copilot/status': () => ({ status: 'complete', channel_id: 5, degraded: ['account_profile'], message: '渠道已创建，但账号信息拉取失败（显示名与动态接口地址使用默认值）。' }),
    });
    stubFetch(fetcher);
    render(<OAuthAuthorize type='copilot_oauth' onDone={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: /开始设备码授权/ }));
    expect(await screen.findByText(/账号资料/)).toBeTruthy();
    expect(screen.getByText(/渠道已创建，但账号信息拉取失败/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '知道了' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => String(call[0]).includes('/oauth/copilot/status'))).toBe(true));
  });
  // A closed browser tab is the common way an authorization dies, and the
  // waiting state is where the user looks for a way out.
  it('tells a Codex user how to leave a closed authorization page', async () => {
    const fetcher = mockOAuth({
      '/oauth/codex/start': () => ({ flow: 'f7', method: 'browser', authorization_url: 'https://chatgpt.com/oauth/authorize?x=1', redirect_uri: 'http://localhost:1455/auth/callback', callback_path: '/auth/callback' }),
      '/oauth/codex/status': () => ({ status: 'pending', interval: 5 }),
    });
    stubFetch(fetcher);
    render(<OAuthAuthorize type='codex_oauth' operation='reauth' credentialID='cred_1' channelID={21} onDone={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
    expect(await screen.findByText('如果已关闭授权页面，请点下方取消后重新开始。')).toBeTruthy();
    expect(screen.getByRole('button', { name: '取消授权' })).toBeTruthy();
  });

  it('keeps credential kind fixed and hides Copilot reauthorization in the editor', async () => {
    stubFetch(mockOAuth({}));
    await openOAuthEditor();
    expect(screen.queryByLabelText('凭证类型')).toBeNull();
    expect(screen.queryByLabelText('API 密钥')).toBeNull();
    expect(screen.queryByRole('button', { name: /重新授权/ })).toBeNull();
    expect(screen.queryByRole('button', { name: '添加账号 / 工作区' })).toBeNull();
    expect(screen.getByLabelText('搜索模型')).toBeTruthy();
  });
  it('shows the credential status and the reauthorize entry when the mark says so', async () => {
    const reauth = oauthFixture({ ...oauthChannel, credential_type: 'codex_oauth' as CredentialType, oauth: { ...oauthChannel.oauth!, provider: 'codex', status: 'reauth_required', status_reason: 'invalid_grant' } });
    const fetcher = vi.fn().mockImplementation((url) => {
      if (pathOf(url) === '/api/channels') return Promise.resolve(response(new URL(String(url), 'http://localhost').searchParams.get('credential_type') === 'codex_oauth' ? [reauth] : []));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    expect(await screen.findByText('不可用')).toBeTruthy();
    expect(screen.getByText('invalid_grant')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '额度详情 默认凭据' }));
    expect(screen.getByText('授权：invalid_grant')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    fireEvent.click(await screen.findByRole('button', { name: '编辑 Copilot' }));
    fireEvent.click(screen.getByRole('button', { name: '重新授权此账号' }));
    expect(screen.getByRole('button', { name: '浏览器授权' })).toBeTruthy();
  });
});

describe('channel view switch and OAuth account pools', () => {
  const oauthChannel = (id: number, name: string, credential: CredentialType, provider: string, label: string): Channel => oauthFixture({
    id, name, api_key: '', has_key: false, has_oauth: true, models: ['gpt-5', 'gpt-4.1'], enabled: true,
    endpoints: [{ format: 'openai_chat_completions', base_url: 'https://api.githubcopilot.com' }],
    credential_type: credential,
    oauth: { provider, account_label: label, status: 'active', expires_at: '2026-10-03T00:00:00Z', updated_at: '2026-10-02T00:00:00Z' },
  });
  const copilot = oauthChannel(21, 'Copilot A', 'copilot_oauth', 'copilot', 'octocat');
  const chatgpt = oauthChannel(22, 'ChatGPT B', 'chatgpt_oauth', 'chatgpt', 'me@example.com');
  const codex = oauthChannel(23, 'Codex C', 'codex_oauth', 'codex', 'codex-user');
  // A row written before credential types existed has none, and the console
  // reads it back as api_key.
  const legacy: Channel = { ...channel, id: 13, name: 'Legacy', credential_type: undefined };
  const oauthRoutes: Record<string, (body?: unknown) => unknown> = {
    '/oauth/copilot/start': () => ({ flow: 'f1', user_code: 'ABCD-1234', verification_uri: 'https://github.com/login/device', expires: '2026-10-02T01:00:00Z', interval: 5 }),
    '/oauth/codex/start': () => ({ flow: 'f2', method: 'browser', authorization_url: 'https://chatgpt.com/oauth/authorize?x=1', redirect_uri: 'http://localhost:1455/auth/callback', callback_path: '/auth/callback', non_official: '非 OpenAI 官方支持路径' }),
    '/oauth/copilot/status': () => ({ status: 'pending', interval: 5 }),
    '/oauth/codex/status': () => ({ status: 'pending', interval: 5 }),
  };
  function mockChannels(rows: Channel[], statuses: Record<string, (body?: unknown) => unknown> = {}) {
    const routes = { ...oauthRoutes, ...statuses };
    return vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      const route = Object.keys(routes).find(entry => String(url).startsWith('/api' + entry));
      if (route && route.endsWith('/start') && method === 'POST') return Promise.resolve(response(routes[route](options.body ? JSON.parse(options.body) : {})));
      if (route && route.endsWith('/status')) return Promise.resolve(response(routes[route]()));
      if (String(url).includes('/cancel') && method === 'POST') return Promise.resolve(response({ status: 'cancelled' }));
      // The console filters by credential type, with no type reading as api_key.
      const type = new URLSearchParams(String(url).split('?')[1] || '').get('credential_type');
      if (pathOf(url) !== '/api/channels') return Promise.resolve(response({}, 404));
      return Promise.resolve(response(type ? rows.filter(c => (c.credential_type || 'api_key') === type) : rows));
    });
  }
  const card = (title: string) => {
    fireEvent.click(screen.getByRole('tab', { name: new RegExp(title) }));
    return screen.getByRole('tabpanel', { name: new RegExp(title) });
  };
  async function openOAuthView() {
    fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    await waitFor(() => expect(fetcherUrls().some(url => url.includes('credential_type=copilot_oauth'))).toBe(true));
    card('Copilot');
  }
  const fetcherUrls = () => (globalThis.fetch as unknown as { mock: { calls: [string, RequestInit?][] } }).mock.calls
    .filter(([url, options]) => url !== '/api/channels/display' || !!options?.method).map(call => String(call[0]));

  it('pulls an OAuth account model catalogue with the account credential and reports the result', async () => {
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url === '/api/channels?credential_type=copilot_oauth') return Promise.resolve(response([copilot]));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([]));
      if (url === '/api/channels/21/oauth/models' && method === 'POST') return Promise.resolve(response({ ok: true, fetched: 2, catalogue: ['gpt-5', 'gpt-4.1', 'gpt-manual'], models: ['gpt-5'] }));
      if (pathOf(url) === '/api/settings') return Promise.resolve(response({ oauth_auto_open: true }));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);
    await openOAuthView();
    fireEvent.click(within(card('Copilot')).getByLabelText('更多操作 Copilot A'));
    fireEvent.click(await screen.findByRole('button', { name: '获取模型 Copilot A' }));
    // The pull is server side with the account's own credential; the row only
    // reports what came back — this pull's own count, manual entries excluded.
    expect(await screen.findByText(/已获取 2 个模型/)).toBeTruthy();
    expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/21/oauth/models' && call[1].method === 'POST')).toBe(true);
  });

  it('keeps model counts compact and opens the plain editor with every model', async () => {
    // Six enabled models, the user's exact "只能看到有 6 个模型" case: the card
    // must name them, not just count them.
    const six = { ...codex, models: ['m1', 'm2', 'm3', 'm4', 'm5', 'm6'], available_models: ['m1', 'm2', 'm3', 'm4', 'm5', 'm6'] };
    stubFetch(mockChannels([six]));
    render(<Channels />);
    await openOAuthView();
    const pool = card('Codex');
    expect(within(pool).getByText('6 个启用模型')).toBeTruthy();
    const modelSummary = within(pool).getByLabelText('查看 Codex C 全部模型');
    fireEvent.click(modelSummary);
    expect(modelSummary.closest('details')!.open).toBe(true);
    expect(within(modelSummary.closest('details')!).getByText('m6')).toBeTruthy();
    // The same editor still manages the complete inventory.
    // The card's edit entry opens the channel editor (not the re-authorize
    // focus), where every catalogue model can be toggled individually.
    fireEvent.click(within(pool).getByRole('button', { name: '编辑 Codex C' }));
    await editorSection('模型配置');
    await screen.findByLabelText('搜索模型');
    expect((screen.getByLabelText('启用模型 m1') as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText('启用模型 m6') as HTMLInputElement).checked).toBe(true);
    // The re-authorize entry still sits on the card beside the edit one.
    fireEvent.click(within(pool).getByLabelText('更多操作 Codex C'));
    expect(within(pool).getByRole('button', { name: '重新授权 Codex C' })).toBeTruthy();
  });

  it('probes a pooled account from the card through the stored channel credential', async () => {
    // T032: the account card opens the same probe dialog, and the stored
    // channel's own credential is what answers — the draft carries the
    // destination only, and the credential is read from the stored row.
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url === '/api/channels/21/probe' && method === 'POST') {
        const body = JSON.parse(options.body);
        return Promise.resolve(response(body.confirmed ? { ok: true, model: body.model, duration_ms: 12, upstream_status: 200 } : { ok: false, model: body.model, duration_ms: 1 }));
      }
      if (url === '/api/channels?credential_type=copilot_oauth') return Promise.resolve(response([copilot]));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([]));
      if (pathOf(url) === '/api/settings') return Promise.resolve(response({ oauth_auto_open: true }));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);
    await openOAuthView();
    fireEvent.click(within(card('Copilot')).getByRole('button', { name: '测试 Copilot A' }));
    expect(await screen.findByText('将调用：gpt-5')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '确认并测试（可能收费）' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/21/probe')).toBe(true));
    const body = JSON.parse(fetcher.mock.calls.find(call => call[0] === '/api/channels/21/probe')![1].body);
    // The card's row is the stored channel, so the endpoints it names are the
    // stored ones: the probe behaves exactly as it did before the draft rode
    // along, and no key travels with it.
    expect(body.model).toBe('gpt-5'); expect(body.confirmed).toBe(true);
    expect(body.channel.endpoints).toEqual([{ format: 'openai_chat_completions', base_url: 'https://api.githubcopilot.com' }]);
    expect(body.channel.api_key).toBe('');
    expect(await screen.findByText(/测试成功/)).toBeTruthy();
    expect(fetcher.mock.calls.some(call => call[0] === '/api/channels/probe')).toBe(false);
  });

  it('saves the auto-open switch from the OAuth view toolbar', async () => {
    const settings = { gateway_key: 'k', prices: [], retention_days: 30, max_attempts: 3, retries_per_channel: 5, failure_cooldown_seconds: 10, request_defaults_enabled: false, request_defaults: { max_output_tokens: 4096 }, oauth_auto_open: false };
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      if (url === '/api/settings/oauth-auto-open' && options.method === 'PUT') return Promise.resolve(response({ oauth_auto_open: true }));
      if (pathOf(url) === '/api/settings' && !options.method) return Promise.resolve(response(settings));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([]));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);
    await openOAuthView();
    const toggle = (await screen.findByLabelText('授权时自动打开浏览器')) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    fireEvent.click(toggle);
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/settings/oauth-auto-open' && call[1].method === 'PUT')).toBe(true));
    expect(JSON.parse(fetcher.mock.calls.find(call => call[1].method === 'PUT')![1].body)).toEqual({ oauth_auto_open: true });
    expect(toggle.checked).toBe(true);
  });

  it('keeps the saved quota period when the toolbar changes auto-open from an older settings snapshot', async () => {
    let saved = { quota_refresh_seconds: 300, oauth_auto_open: false };
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      if (url === '/api/plugins' && !options.method) return Promise.resolve(response(enabledPlugins));
      if (url === '/api/settings' && !options.method) return Promise.resolve(response({ ...saved, request_defaults_enabled: false, request_defaults: {}, retention_days: 30 }));
      if (url === '/api/settings/quota-refresh') {
        if (options.method === 'PUT') saved = { ...saved, ...JSON.parse(options.body) };
        return Promise.resolve(response({ quota_refresh_seconds: saved.quota_refresh_seconds }));
      }
      if (url === '/api/settings/oauth-auto-open' && options.method === 'PUT') { saved = { ...saved, ...JSON.parse(options.body) }; return Promise.resolve(response({ oauth_auto_open: saved.oauth_auto_open })); }
      if (url === '/api/channels/display') return Promise.resolve(response({ sort_mode: 'name', order: [], icons: {} }));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([]));
      return Promise.resolve(response({}, 404));
    });
    vi.stubGlobal('fetch', fetcher);
    render(<Channels />); await openOAuthView(); card('Codex');
    fireEvent.click(screen.getByText('刷新周期'));
    const period = screen.getByLabelText('额度刷新周期（秒）');
    await waitFor(() => expect(period).toHaveProperty('value', '300'));
    fireEvent.change(period, { target: { value: '60' } });
    fireEvent.click(screen.getByRole('button', { name: '保存刷新周期' }));
    await waitFor(() => expect(saved.quota_refresh_seconds).toBe(60));
    await waitFor(() => expect(period).toHaveProperty('disabled', false));
    const toggle = screen.getByLabelText('授权时自动打开浏览器');
    fireEvent.click(toggle);
    await waitFor(() => expect(saved.oauth_auto_open).toBe(true));
    expect(saved.quota_refresh_seconds).toBe(60); expect(period).toHaveProperty('value', '60');
    const writes = fetcher.mock.calls.filter(call => call[1]?.method === 'PUT');
    expect(writes.map(call => [call[0], JSON.parse(call[1].body)])).toEqual([
      ['/api/settings/quota-refresh', { quota_refresh_seconds: 60 }],
      ['/api/settings/oauth-auto-open', { oauth_auto_open: true }],
    ]);
  });

  it('rolls the toolbar auto-open switch back when its isolated write fails', async () => {
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      if (url === '/api/settings' && !options.method) return Promise.resolve(response({ oauth_auto_open: false }));
      if (url === '/api/settings/oauth-auto-open' && options.method === 'PUT') return Promise.resolve(response({ error: { message: 'auto-open save failed' } }, 500));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([]));
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher); render(<Channels />); await openOAuthView();
    const toggle = screen.getByLabelText('授权时自动打开浏览器') as HTMLInputElement;
    await waitFor(() => expect(toggle.checked).toBe(false));
    fireEvent.click(toggle);
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[0] === '/api/settings/oauth-auto-open')).toBe(true));
    await waitFor(() => expect(toggle.checked).toBe(false));
    expect(fetcher.mock.calls.filter(call => call[1]?.method === 'PUT').map(call => JSON.parse(call[1].body))).toEqual([{ oauth_auto_open: true }]);
  });

  it('restores the view and an in-flight flow after a route switch', async () => {
    const statuses: Record<string, (body?: unknown) => unknown> = { '/oauth/copilot/status': () => ({ status: 'pending', interval: 5 }) };
    stubFetch(mockChannels([copilot, chatgpt, codex], statuses));
    const first = render(<Channels />);
    await openOAuthView();
    // Each provider card can start its own flow; the copilot one is the flow
    // that must survive the route switch.
    fireEvent.click(within(card('Copilot')).getByRole('button', { name: '向 Copilot A 添加账号 / 工作区' }));
    fireEvent.click(await screen.findByRole('button', { name: '开始设备码授权' }));
    await waitFor(() => expect(JSON.parse(sessionStorage.getItem('to-router.channels.v1')!).flows['channel:21']).toBe('f1'));
    // The user switches to another page and back: the view, the panel and the
    // flow the console still holds all come back — and no second authorization
    // is started, because the first one is still answerable.
    first.unmount();
    render(<Channels />);
    await waitFor(() => expect(fetcherUrls().some(url => url.includes('credential_type=copilot_oauth'))).toBe(true));
    await waitFor(() => expect(fetcherUrls().filter(url => url.includes('/oauth/copilot/status?flow=f1')).length).toBeGreaterThan(0));
    expect(fetcherUrls().filter(url => url.startsWith('/api/oauth/copilot/start')).length).toBe(1);
  });

  it('brings the entry point back when the resumed flow is gone', async () => {
    // A previous visit stored a flow id the console no longer answers: the
    // process restarted, and its flows lived in memory. The status call 404s.
    sessionStorage.setItem('to-router.channels.v1', JSON.stringify({ view: 'oauth', editor: null, flows: { 'channel:21': 'gone-1' } }));
    localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ active: 'copilot_oauth' }));
    let starts = 0;
    const fetcher = vi.fn().mockImplementation((url, options = {}) => {
      const method = options.method || '';
      if (url === '/api/channels?credential_type=copilot_oauth') return Promise.resolve(response([copilot]));
      if (pathOf(url) === '/api/channels') return Promise.resolve(response([]));
      if (url === '/api/oauth/copilot/status?flow=gone-1') return Promise.resolve(response({ error: { message: '授权流程不存在或已过期，请重新开始。' } }, 404));
      if (pathOf(url) === '/api/settings') return Promise.resolve(response({ oauth_auto_open: true }));
      if (pathOf(url) === '/api/oauth/copilot/start' && method === 'POST') {
        starts += 1;
        return Promise.resolve(response({ flow: 'f2', user_code: 'WXYZ-9999', verification_uri: 'https://github.com/login/device', expires: '2026-10-02T01:00:00Z', interval: 5 }));
      }
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    render(<Channels />);
    // The stored view comes back with the flow's panel resuming...
    await waitFor(() => expect(fetcherUrls().some(url => url === '/api/oauth/copilot/status?flow=gone-1')).toBe(true));
    // ...and when the console says it is gone, the start button returns instead
    // of leaving the entry point dead.
    await waitFor(() => expect(screen.queryByRole('button', { name: /开始设备码授权/ })).toBeTruthy());
    expect(JSON.parse(sessionStorage.getItem('to-router.channels.v1')!).flows['channel:21']).toBeUndefined();
    // A fresh start is a new flow, not a retry of the dead one.
    fireEvent.click(screen.getByRole('button', { name: /开始设备码授权/ }));
    await waitFor(() => expect(starts).toBe(1));
    expect(await screen.findByText('WXYZ-9999')).toBeTruthy();
  });

  it('cancels an in-flight authorization and returns the entry point to its start state', async () => {
    const statuses: Record<string, (body?: unknown) => unknown> = { '/oauth/copilot/status': () => ({ status: 'pending', interval: 5 }) };
    stubFetch(mockChannels([copilot, chatgpt, codex], statuses));
    render(<Channels />);
    await openOAuthView();
    fireEvent.click(within(card('Copilot')).getByRole('button', { name: '向 Copilot A 添加账号 / 工作区' }));
    fireEvent.click(await screen.findByRole('button', { name: '开始设备码授权' }));
    await waitFor(() => expect(JSON.parse(sessionStorage.getItem('to-router.channels.v1')!).flows['channel:21']).toBe('f1'));
    // The user gives up: the console is told to stop waiting, the stored flow id
    // is forgotten, and the panel is back at its start state.
    fireEvent.click(screen.getByRole('button', { name: '取消授权' }));
    await waitFor(() => expect(fetcherUrls().some(url => url.startsWith('/api/oauth/copilot/cancel?flow=f1'))).toBe(true));
    await waitFor(() => expect(JSON.parse(sessionStorage.getItem('to-router.channels.v1')!).flows['channel:21']).toBeUndefined());
    expect(screen.getByRole('button', { name: /开始设备码授权/ })).toBeTruthy();
    // A later visit starts clean: nothing is resumed.
    expect(screen.queryByRole('button', { name: '取消授权' })).toBeNull();
  });

  it('asks for the api_key type in the default view and keeps legacy channels out of the pools', async () => {
    stubFetch(mockChannels([channel, legacy, copilot, chatgpt, codex]));
    render(<Channels />);
    expect(await screen.findByRole('button', { name: '编辑 Personal' })).toBeTruthy();
    expect(await screen.findByRole('button', { name: '编辑 Legacy' })).toBeTruthy();
    expect(fetcherUrls()).toContain('/api/channels?credential_type=api_key');
    // OAuth accounts live in their own view, not in the API key list.
    expect(screen.queryByText('octocat')).toBeNull();
    expect(screen.queryByRole('button', { name: /重新授权 Copilot A/ })).toBeNull();
  });

  it('switches views and asks for each OAuth credential type separately', async () => {
    stubFetch(mockChannels([channel, legacy, copilot, chatgpt, codex]));
    render(<Channels />);
    await screen.findByRole('button', { name: '编辑 Personal' });
    await openOAuthView();
    for (const type of ['copilot_oauth', 'chatgpt_oauth', 'codex_oauth']) {
      expect(fetcherUrls()).toContain(`/api/channels?credential_type=${type}`);
    }
    expect(screen.getByRole('button', { name: 'OAuth 账号渠道' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: 'API Key 渠道' }).getAttribute('aria-pressed')).toBe('false');
    // The list rows are gone; the account pool replaced them.
    expect(screen.queryByRole('button', { name: '编辑 Personal' })).toBeNull();
    expect(screen.getByLabelText('搜索渠道或模型')).toBeTruthy();
    expect(screen.getByRole('button', { name: '发现凭证' })).toBeTruthy();
    expect(screen.getByRole('button', { name: '添加渠道' })).toBeTruthy();
  });

  it('groups accounts into the three provider cards and marks the community track', async () => {
    stubFetch(mockChannels([channel, legacy, copilot, chatgpt, codex]));
    render(<Channels />);
    await screen.findByRole('button', { name: '编辑 Personal' });
    await openOAuthView();
    const copilotCard = card('Copilot');
    expect(within(copilotCard).getByText('默认凭据')).toBeTruthy();
    expect(within(copilotCard).queryByText('octocat')).toBeNull();
    expect(within(copilotCard).getByText('2 个启用模型')).toBeTruthy();
    expect(within(copilotCard).getByText('状态未读取')).toBeTruthy();
    expect(within(card('ChatGPT')).getByText('默认凭据')).toBeTruthy();
    expect(within(card('ChatGPT')).queryByText('me@example.com')).toBeNull();
    expect(within(card('Codex')).getByText('默认凭据')).toBeTruthy();
    expect(within(card('Codex')).queryByText('codex-user')).toBeNull();
    // Provenance remains visible in the selected access method.
    expect(within(card('Codex')).getByText('非官方')).toBeTruthy();
    expect(within(card('Copilot')).queryByText('非官方')).toBeNull();
    for (const title of ['Copilot', 'ChatGPT', 'Codex']) {
      expect(within(card(title)).getByRole('button', { name: new RegExp(`向 ${title} .*添加账号`) })).toBeTruthy();
    }
  });

  it('shows the empty state for a provider with no connected account', async () => {
    stubFetch(mockChannels([channel, legacy, copilot]));
    render(<Channels />);
    await screen.findByRole('button', { name: '编辑 Personal' });
    await openOAuthView();
    expect(within(card('Copilot')).getByText('默认凭据')).toBeTruthy();
    expect(within(card('Copilot')).queryByText('octocat')).toBeNull();
    expect(within(card('ChatGPT')).getByText(/尚未添加渠道/g)).toBeTruthy();
    expect(within(card('Codex')).getByText(/尚未添加渠道/g)).toBeTruthy();
    // The empty card still offers the entry point.
    expect(within(card('Codex')).queryByRole('button', { name: '添加账号 / 工作区' })).toBeNull();
    expect(screen.getByRole('button', { name: '添加渠道' })).toHaveProperty('disabled', false);
  });

  it('re-authorizes Codex through the low-frequency menu with the original channel ID', async () => {
    stubFetch(mockChannels([channel, codex]));
    render(<Channels />);
    await screen.findByRole('button', { name: '编辑 Personal' });
    await openOAuthView();
    fireEvent.click(within(card('Codex')).getByLabelText('更多操作 Codex C'));
    fireEvent.click(within(card('Codex')).getByRole('button', { name: '重新授权 Codex C' }));
    fireEvent.click(await screen.findByRole('button', { name: '浏览器授权' }));
    await waitFor(() => expect(fetcherUrls().some(url => url.includes('operation=reauth'))).toBe(true));
    const start = fetcherUrls().find(url => url.startsWith('/api/oauth/codex/start')) as string;
    expect(start).toContain('operation=reauth');
    expect(start).toContain('channel_id=23');
  });

  it('adds an account to a pool in add mode, starting that track own flow', async () => {
    stubFetch(mockChannels([channel, legacy, copilot, codex]));
    render(<Channels />);
    await screen.findByRole('button', { name: '编辑 Personal' });
    await openOAuthView();
    fireEvent.click(within(card('Codex')).getByRole('button', { name: '向 Codex C 添加账号 / 工作区' }));
    fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
    await waitFor(() => expect(fetcherUrls().some(url => url.startsWith('/api/oauth/codex/start'))).toBe(true));
    // The flow repeats the track's provenance note in its own words.
    expect((await screen.findAllByText(/非 OpenAI 官方支持路径/)).length).toBeGreaterThan(0);
    // Adding a second account registers a new client: no channel to reuse.
    const add = fetcherUrls().find(url => url.startsWith('/api/oauth/codex/start')) as string;
    expect(add).not.toContain('reauth');
    expect(add).toContain('channel_id=23');
    expect(add).toContain('operation=add_credential');
    expect(add).toContain('method=browser');
  });

  it('reloads the pools when an added account finishes', async () => {
    stubFetch(mockChannels([channel, legacy, copilot, codex], { '/oauth/codex/status': () => ({ status: 'complete', channel_id: 23 }) }));
    render(<Channels />);
    await screen.findByRole('button', { name: '编辑 Personal' });
    await openOAuthView();
    const requested = () => fetcherUrls().filter(url => url.includes('credential_type=codex_oauth')).length;
    fireEvent.click(within(card('Codex')).getByRole('button', { name: '向 Codex C 添加账号 / 工作区' }));
    fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
    // The finished flow reloads the list, so the new account reaches the card.
    await waitFor(() => expect(requested()).toBeGreaterThan(1));
  });
});


describe('OAuth account display order, quota and entry isolation', () => {
  const account = (id: number, name: string): Channel => oauthFixture({ ...channel, id, name, has_key: false, has_oauth: true, credential_type: 'codex_oauth', oauth: { provider: 'codex', account_label: name + '@example.test', status: 'active', expires_at: '', updated_at: '' } });
  const first = account(31, 'Codex One'), second = account(32, 'Codex Two');
  const snapshot = { ok: true, captured_at: '2026-10-04T12:00:00Z', plan_type: 'plus', buckets: [{ id: 'rate_limit', general: true, name: '常规额度', allowed: true, limit_reached: false, primary: { used_percent: 100, window_seconds: 10800, reset_at: 1791118800 }, secondary: { used_percent: null, window_seconds: 864000, reset_at: null } }] };
  function backend(rows: Channel[], quota: (id: number) => unknown = () => snapshot) {
    let current = rows;
    return vi.fn().mockImplementation((url: string, options = {}) => {
      if (url.startsWith('/api/oauth/codex/usage')) return Promise.resolve(quota(Number(new URLSearchParams(url.split('?')[1]).get('channel_id')))).then(value => response(value));
      if (url === '/api/settings') return Promise.resolve(response({ oauth_auto_open: false }));
      if (url.startsWith('/api/channels/') && options.method === 'PUT') { const body = JSON.parse(options.body); current = current.map(c => c.id === body.id ? { ...c, ...body } : c); return Promise.resolve(response(current.find(c => c.id === body.id))); }
      if (url.startsWith('/api/channels/') && options.method === 'DELETE') { current = current.filter(c => c.id !== Number(url.split('/').pop())); return Promise.resolve(response({ ok: true })); }
      if (pathOf(url) === '/api/channels') { const type = new URLSearchParams(url.split('?')[1]).get('credential_type'); return Promise.resolve(response(current.filter(c => (c.credential_type || 'api_key') === type))); }
      return Promise.resolve(response({}, 404));
    });
  }
  async function showOAuth() {
    render(<Channels />);
    fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    fireEvent.click(await screen.findByRole('tab', { name: /Codex/ }));
    return screen.findByRole('region', { name: '渠道 Codex One' });
  }
  function menu(name: string) { fireEvent.click(screen.getByLabelText(`更多操作 ${name}`)); }
  const quotaCalls = (fetcher: ReturnType<typeof backend>) => fetcher.mock.calls.filter(call => call[0].startsWith('/api/oauth/codex/usage'));

  it('adds only API Key channels and exposes no credential-type switch or weight', async () => {
    const fetcher = backend([channel]); stubFetch(fetcher);
    render(<Channels />);
    fireEvent.click(screen.getByRole('button', { name: '添加渠道' }));
    expect(screen.getByLabelText('API 密钥')).toBeTruthy();
    expect(screen.queryByLabelText('凭证类型')).toBeNull();
    expect(screen.queryByText('权重')).toBeNull();
    expect(screen.queryByRole('button', { name: '浏览器授权' })).toBeNull();
    await editorSection('模型配置');
    expect(screen.getByLabelText('手动模型 ID')).toBeTruthy();
  });

  it('persists access-method and channel order and hidden navigation without changing routing', async () => {
    const fetcher = backend([first, second]); stubFetch(fetcher);
    await showOAuth();
    fireEvent.click(screen.getByRole('button', { name: '管理接入方式' }));
    fireEvent.click(screen.getByRole('button', { name: '下移接入方式 Codex' }));
    fireEvent.click(screen.getByRole('switch', { name: '显示 ChatGPT' }));
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    menu('Codex Two'); fireEvent.click(screen.getByRole('button', { name: '上移账号 Codex Two' }));
    const channelNames = () => screen.getAllByRole('region', { name: /^渠道 / }).map(el => el.getAttribute('aria-label'));
    expect(channelNames()).toEqual(['渠道 Codex Two', '渠道 Codex One']);
    cleanup(); render(<Channels />);
    await screen.findByRole('region', { name: '渠道 Codex One' });
    expect(screen.getAllByRole('tab').map(el => el.textContent)).toEqual(['Claude 订阅0', 'Codex2', 'Antigravity0', 'OpenCode Zen Free0', 'Devin0', 'Copilot0']);
    expect(screen.queryByRole('tab', { name: /ChatGPT/ })).toBeNull();
    expect(channelNames()).toEqual(['渠道 Codex Two', '渠道 Codex One']);
    expect(fetcher.mock.calls.some(call => call[1]?.method)).toBe(false);
    const persisted = JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!);
    expect(persisted.accounts.codex_oauth).toEqual([32, 31]);
    expect(JSON.stringify(persisted)).not.toContain('example.test');
  });

  it('supports dragging access methods and channels while ignoring stale saved IDs', async () => {
    vi.stubGlobal('PointerEvent', class extends MouseEvent {
      pointerId: number; isPrimary = true; pointerType = 'mouse';
      constructor(type: string, init: PointerEventInit = {}) { super(type, init); this.pointerId = init.pointerId ?? 1; }
    });
    vi.stubGlobal('requestAnimationFrame', vi.fn(() => 1));
    vi.stubGlobal('cancelAnimationFrame', vi.fn());
    function drag(handle: HTMLElement, target: HTMLElement, horizontal = false) {
      const source = handle.closest<HTMLElement>('[data-sortable-item]')!, list = source.parentElement!;
      const items = Array.from(list.children).filter((item): item is HTMLElement => item instanceof HTMLElement && item.hasAttribute('data-sortable-item'));
      const rect = (index: number, length = 1) => ({ x: horizontal ? index * 100 : 0, y: horizontal ? 0 : index * 100, left: horizontal ? index * 100 : 0, top: horizontal ? 0 : index * 100, right: horizontal ? (index + length) * 100 : 300, bottom: horizontal ? 100 : (index + length) * 100, width: horizontal ? length * 100 : 300, height: horizontal ? 100 : length * 100, toJSON() {} });
      vi.spyOn(list, 'getBoundingClientRect').mockImplementation(() => rect(0, items.length) as DOMRect);
      items.forEach((item, index) => vi.spyOn(item, 'getBoundingClientRect').mockImplementation(() => rect(index) as DOMRect));
      const from = source.getBoundingClientRect(), to = target.getBoundingClientRect();
      const start = { pointerId: 1, button: 0, clientX: horizontal ? from.left + 50 : 50, clientY: horizontal ? 50 : from.top + 50 };
      const end = { ...start, clientX: horizontal ? to.left + (to.left < from.left ? 25 : 75) : 50, clientY: horizontal ? 50 : to.top + (to.top < from.top ? 25 : 75) };
      fireEvent.pointerDown(handle, start);
      fireEvent.pointerMove(window, end);
      expect(target.style.getPropertyValue(horizontal ? '--sortable-x' : '--sortable-y')).not.toBe('0px');
      fireEvent.pointerUp(window, end);
    }
    localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ platforms: ['deleted-platform', 'codex_oauth'], accounts: { codex_oauth: [999, 31] } }));
    const fetcher = backend([first, second]); stubFetch(fetcher); await showOAuth();
    const one = screen.getByRole('region', { name: '渠道 Codex One' }), two = screen.getByRole('region', { name: '渠道 Codex Two' });
    await within(one).findByText('剩余 0%'); await within(two).findByText('剩余 0%');
    const channelNames = () => screen.getAllByRole('region', { name: /^渠道 / }).map(el => el.getAttribute('aria-label'));
    expect(channelNames()).toEqual(['渠道 Codex One', '渠道 Codex Two']);
    drag(within(two).getByRole('button', { name: '拖动渠道 Codex Two' }), one);
    expect(channelNames()).toEqual(['渠道 Codex Two', '渠道 Codex One']);
    drag(within(two).getByRole('button', { name: '拖动渠道 Codex Two' }), one);
    expect(channelNames()).toEqual(['渠道 Codex One', '渠道 Codex Two']);
    const codexTab = screen.getByRole('tab', { name: /Codex/ }).parentElement!;
    drag(screen.getByRole('button', { name: '拖动接入方式 Copilot' }), codexTab, true);
    expect(screen.getAllByRole('tab')[0].textContent).toContain('Copilot');
    drag(screen.getByRole('button', { name: '拖动接入方式 Copilot' }), codexTab, true);
    expect(screen.getAllByRole('tab')[0].textContent).toContain('Codex');
    expect(fetcher.mock.calls.some(call => call[1]?.method)).toBe(false);
    const persisted = JSON.parse(localStorage.getItem('to-router.oauth-display.v1')!);
    expect(persisted.platforms).not.toContain('deleted-platform');
    expect(persisted.accounts.codex_oauth).toEqual([31, 32]);
  });

  it('reads visible Codex cache without upstream refresh, distinguishes zero/unknown and keeps routing enabled', async () => {
    const fetcher = backend([first]); stubFetch(fetcher); const row = await showOAuth();
    await within(row).findByText('剩余 0%');
    expect(within(row).getByText('剩余未知')).toBeTruthy();
    expect(within(row).getByText('3 小时')).toBeTruthy();
    expect(within(row).getByText('10 天')).toBeTruthy();
    expect(within(row).getByText('重置：未知')).toBeTruthy();
    fireEvent.click(within(row).getByRole('button', { name: '额度详情 默认凭据' }));
    expect(screen.getByText('通用额度 · 上游允许：是 · 达到限额：否')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    expect(within(row).getByRole('switch', { name: '停用 Codex One' }).getAttribute('aria-checked')).toBe('true');
    expect(within(row).getByRole('switch', { name: '停用凭证 默认凭据' }).getAttribute('aria-checked')).toBe('true');
    expect(quotaCalls(fetcher)).toHaveLength(1);
    fireEvent.click(screen.getByRole('tab', { name: /Copilot/ }));
    fireEvent.click(screen.getByRole('tab', { name: /Codex/ }));
    expect(quotaCalls(fetcher)).toHaveLength(2);
    expect(quotaCalls(fetcher).filter(call => call[1]?.method === 'POST')).toHaveLength(0);
    expect(fetcher.mock.calls.some(call => call[1]?.method === 'PUT')).toBe(false);
    const currentRow = screen.getByRole('region', { name: '渠道 Codex One' });
    menu('Codex One');
    expect(within(currentRow).getByRole('button', { name: '获取模型 Codex One' })).toBeTruthy();
    expect(within(currentRow).getByRole('button', { name: '测试 Codex One' })).toBeTruthy();
  });

  it('waits for the Codex access method to become visible before observing quota', async () => {
    localStorage.setItem('to-router.oauth-display.v1', JSON.stringify({ hidden: { codex_oauth: true } }));
    const fetcher = backend([first]); stubFetch(fetcher);
    render(<Channels />); fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    await screen.findByRole('tab', { name: /ChatGPT/ });
    expect(quotaCalls(fetcher)).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: '管理接入方式' }));
    fireEvent.click(screen.getByRole('switch', { name: '显示 Codex' }));
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    fireEvent.click(screen.getByRole('tab', { name: /Codex/ }));
    await screen.findByText('剩余 0%'); expect(quotaCalls(fetcher)).toHaveLength(1);
  });

  it('retains the last successful snapshot and its timestamp on a failed manual refresh', async () => {
    let count = 0;
    const fetcher = backend([first], () => ++count === 1 ? snapshot : { ok: false, detail: 'upstream unavailable' });
    stubFetch(fetcher); await showOAuth(); await screen.findByText('剩余 0%');
    menu('Codex One'); fireEvent.click(screen.getByRole('button', { name: '刷新额度 默认凭据' }));
    expect(await screen.findByText('旧快照 · 刷新失败：upstream unavailable')).toBeTruthy();
    expect(screen.getByText('剩余 0%')).toBeTruthy();
    expect(screen.getByText('旧快照 · 刷新失败：upstream unavailable')).toBeTruthy();
    expect(screen.getByRole('switch', { name: '停用 Codex One' }).getAttribute('aria-checked')).toBe('true');
    expect(quotaCalls(fetcher)).toHaveLength(2);
    expect(quotaCalls(fetcher)[1][0]).toContain('force=1');
    expect(quotaCalls(fetcher)[1][1]?.method).toBe('POST');
    expect(fetcher.mock.calls.some(call => call[1]?.method === 'PUT')).toBe(false);
  });

  it('does not duplicate a pending account quota request or apply an old completion to a new page', async () => {
    let resolve!: (value: unknown) => void;
    const delayed = new Promise(r => { resolve = r; });
    const fetcher = backend([first], () => delayed); stubFetch(fetcher); await showOAuth();
    menu('Codex One'); const refresh = screen.getByRole('button', { name: '刷新额度 默认凭据' });
    expect((refresh as HTMLButtonElement).disabled).toBe(true); fireEvent.click(refresh);
    expect(quotaCalls(fetcher)).toHaveLength(1);
    cleanup(); stubFetch(backend([second], () => ({ ok: true, captured_at: snapshot.captured_at, buckets: [] })));
    render(<Channels />); await screen.findByRole('region', { name: '渠道 Codex Two' });
    await act(async () => { resolve(snapshot); await delayed; });
    expect(screen.queryByText('剩余 0%')).toBeNull();
    expect(screen.getAllByText('剩余未知').length).toBeGreaterThan(0);
    expect(screen.queryByRole('region', { name: '渠道 Codex One' })).toBeNull();
  });

  it('uses the same enabled value for the account switch and editor and preserves model configuration', async () => {
    const configured = { ...first, model_configs: { 'model-a': { max_output_tokens: 4096 } } };
    const fetcher = backend([configured]); stubFetch(fetcher); await showOAuth();
    fireEvent.click(screen.getByRole('switch', { name: '停用 Codex One' }));
    await screen.findByRole('switch', { name: '启用 Codex One' });
    fireEvent.click(screen.getByRole('button', { name: '编辑 Codex One' }));
    const enabled = screen.getByRole('checkbox', { name: /启用此渠道/ });
    expect((enabled as HTMLInputElement).checked).toBe(false);
    expect(screen.queryByLabelText('凭证类型')).toBeNull();
    fireEvent.change(screen.getByLabelText('渠道名称'), { target: { value: 'Renamed Codex' } });
    fireEvent.click(enabled); fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await screen.findByRole('switch', { name: '停用 Renamed Codex' });
    const writes = fetcher.mock.calls.filter(call => call[1]?.method === 'PUT').map(call => JSON.parse(call[1].body));
    expect(writes.map(c => c.enabled)).toEqual([false, true]);
    expect(writes[1]).not.toHaveProperty('model_configs');
    expect(writes[1].credential_type).toBe('codex_oauth');
    expect(writes[1]).not.toHaveProperty('weight');
  });

  it('offers precise Copilot reauthorization while retaining model management', async () => {
    const copilot = oauthFixture({ ...first, name: 'Copilot One', credential_type: 'copilot_oauth' as CredentialType, oauth: { ...first.oauth!, provider: 'copilot', status: 'reauth_required' as const, status_reason: 'invalid token' } });
    stubFetch(backend([copilot]));
    render(<Channels />); fireEvent.click(screen.getByRole('button', { name: 'OAuth 账号渠道' }));
    fireEvent.click(await screen.findByRole('tab', { name: /Copilot/ }));
    await screen.findByRole('region', { name: '渠道 Copilot One' });
    expect(screen.getByText('不可用')).toBeTruthy();
    expect(screen.getByText('invalid token')).toBeTruthy();
    menu('Copilot One'); expect(screen.getByRole('button', { name: '重新授权 Copilot One' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '编辑 Copilot One' }));
    expect(within(screen.getByRole('dialog')).getByText('需重新授权')).toBeTruthy();
    expect(within(screen.getByRole('dialog')).getByRole('button', { name: '重新授权此账号' })).toBeTruthy();
    await editorSection('模型配置');
    expect(screen.getByLabelText('启用模型 model-a')).toBeTruthy();
    expect(screen.getByRole('button', { name: '从上游获取模型' })).toBeTruthy();
  });

  it('keeps OAuth manual model, enable/search/delete management and config through directory refresh', async () => {
    const configured = { ...first, available_models: ['model-a', 'obsolete'], model_configs: { 'model-a': { max_output_tokens: 4096 } } };
    const fallback = backend([configured]);
    const fetcher = vi.fn().mockImplementation((url: string, options = {}) => {
      if (url === '/api/channels/31/oauth/models') return Promise.resolve(response({ ok: true, fetched: 2, catalogue: ['model-a', 'fresh'], models: ['model-a'] }));
      if (url === '/api/channels/31/models?model=fresh' && options.method === 'DELETE') return Promise.resolve(response({ ...configured, available_models: ['model-a', 'manual-added'], manual_models: ['manual-added'] }));
      return fallback(url, options);
    });
    stubFetch(fetcher); await showOAuth();
    fireEvent.click(screen.getByRole('button', { name: '编辑 Codex One' }));
    await editorSection('模型配置');
    fireEvent.change(screen.getByLabelText('手动模型 ID'), { target: { value: 'manual-added' } });
    fireEvent.click(screen.getByRole('button', { name: '手动添加' }));
    await editorSection('模型配置');
    fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' }));
    const fresh = await screen.findByLabelText('启用模型 fresh');
    expect((fresh as HTMLInputElement).checked).toBe(false);
    expect(screen.getByLabelText('启用模型 manual-added')).toBeTruthy();
    expect(screen.queryByLabelText('启用模型 obsolete')).toBeNull();
    fireEvent.change(screen.getByLabelText('搜索模型'), { target: { value: 'fresh' } });
    expect(screen.queryByLabelText('启用模型 model-a')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '删除模型 fresh' }));
    fireEvent.click(await screen.findByRole('button', { name: '删除渠道模型' }));
    await waitFor(() => expect(screen.queryByLabelText('启用模型 fresh')).toBeNull());
    fireEvent.change(screen.getByLabelText('搜索模型'), { target: { value: '' } });
    fireEvent.click(screen.getByLabelText('启用模型 manual-added'));
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(call => call[1]?.method === 'PUT')).toBe(true));
    const saved = JSON.parse(fetcher.mock.calls.find(call => call[1]?.method === 'PUT')![1].body);
    expect(saved.models).toEqual(['model-a', 'manual-added']);
    expect(saved.available_models).toEqual(['model-a', 'manual-added']);
    expect(saved.manual_models).toEqual(['manual-added']);
    expect(saved).not.toHaveProperty('model_configs');
    expect(saved.credential_type).toBe('codex_oauth');
  });


  it.each(['refresh', 'delete'])('preserves the persisted %s result after closing the editor and toggling the account', async operation => {
    const config = { 'model-a': { max_output_tokens: 4096 }, obsolete: { max_output_tokens: 100 } };
    const evidence = { 'model-a': { source: 'manual', api_format: 'openai_responses' }, obsolete: { source: 'manual', api_format: 'openai_responses' } };
    let stored: Channel = { ...first, models: operation === 'delete' ? ['model-a', 'obsolete'] : ['model-a'], available_models: ['model-a', 'obsolete'], model_configs: config, model_evidence: evidence };
    const afterModels = { models: ['model-a'], available_models: operation === 'refresh' ? ['model-a', 'fresh'] : ['model-a'], manual_models: [], model_configs: { 'model-a': config['model-a'] }, model_evidence: { 'model-a': evidence['model-a'] } };
    const fetcher = vi.fn().mockImplementation((url: string, options = {}) => {
      if (url === '/api/settings') return Promise.resolve(response({ oauth_auto_open: false }));
      if (url.startsWith('/api/oauth/codex/usage')) return Promise.resolve(response({ ok: true, buckets: [] }));
      if (url === '/api/channels/31/oauth/models' && options.method === 'POST') { stored = { ...stored, ...afterModels }; return Promise.resolve(response({ ok: true, fetched: 2, catalogue: stored.available_models, models: stored.models, manual: stored.manual_models })); }
      if (url === '/api/channels/31/models?model=obsolete' && options.method === 'DELETE') { stored = { ...stored, ...afterModels }; return Promise.resolve(response(stored)); }
      if (url === '/api/channels/31' && options.method === 'PUT') { stored = { ...stored, ...JSON.parse(options.body) }; return Promise.resolve(response(stored)); }
      if (pathOf(url) === '/api/channels') { const type = new URLSearchParams(url.split('?')[1]).get('credential_type'); return Promise.resolve(response(type === 'codex_oauth' ? [stored] : [])); }
      return Promise.resolve(response({}, 404));
    });
    stubFetch(fetcher);
    await showOAuth(); fireEvent.click(screen.getByRole('button', { name: '编辑 Codex One' }));
    fireEvent.change(screen.getByLabelText('渠道名称'), { target: { value: 'unsaved rename' } });
    await editorSection('模型配置');
    if (operation === 'refresh') { fireEvent.click(screen.getByRole('button', { name: '从上游获取模型' })); await screen.findByLabelText('启用模型 fresh'); }
    else { fireEvent.click(screen.getByRole('button', { name: '删除模型 obsolete' })); fireEvent.click(await screen.findByRole('button', { name: '删除渠道模型' })); await waitFor(() => expect(screen.queryByLabelText('启用模型 obsolete')).toBeNull()); }
    expect((screen.getByLabelText('渠道名称') as HTMLInputElement).value).toBe('unsaved rename');
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const toggle = await screen.findByRole('switch', { name: '停用 Codex One' });
    await waitFor(() => expect((toggle as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(toggle);
    await waitFor(() => expect(stored.enabled).toBe(false));
    expect(stored.name).toBe('Codex One');
    expect(stored.models).toEqual(afterModels.models);
    expect(stored.available_models).toEqual(afterModels.available_models);
    expect(stored.model_configs).toEqual(afterModels.model_configs);
    expect(stored.model_evidence).toEqual(afterModels.model_evidence);
    const written = JSON.parse(fetcher.mock.calls.find(call => call[1]?.method === 'PUT')![1].body);
    expect(written.available_models).toEqual(afterModels.available_models);
    expect(written).not.toHaveProperty('model_configs');
    expect(written.model_evidence).toEqual(afterModels.model_evidence);
  });

});
