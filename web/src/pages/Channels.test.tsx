// @vitest-environment jsdom
import { cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { ConfirmProvider } from '../components/Confirm';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Channels from './Channels';
import type { Channel, Price } from '../api';
import { enabledPlugins } from '../testFixtures/plugins';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });

const json = (body: unknown) => new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
const channel: Channel = { id: 12, name: 'Personal', api_key: '', has_key: true, models: ['model-a'], available_models: ['model-a'], enabled: true, endpoints: [{ format: 'openai_chat_completions', base_url: 'https://example.com' }], model_configs: { 'model-a': { max_output_tokens: 0, output_limit: 2048, output_token_field: 'max_completion_tokens', native_thinking: { supported: true, mandatory: false, modes: [{ id: 'effort', api_format: 'openai_responses', control: 'effort', efforts: ['low', 'high'], default_effort: 'high', disabled_efforts: [] }] }, streaming: false, input_modalities: [] } } };
const price: Price = { channel_id: 12, model: 'model-a', display_name: 'A', origin: 'manual', selection: { mode: 'manual', table: { currency: 'USD', unit_tokens: 1000000, rates: { input: 0, output: 1e-8, cache_read: null, cache_write: null }, rules: [] } } };
let fetcher: ReturnType<typeof vi.fn>;
beforeEach(() => {
  sessionStorage.clear(); localStorage.clear(); history.replaceState(null, '', '/');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/plugins') return Promise.resolve(json(enabledPlugins));
    if (url === '/api/channels/display' && !init?.method) return Promise.resolve(json({ sort_mode: 'name', order: [], icons: {} }));
    if (url.startsWith('/api/channels') && !init?.method) return Promise.resolve(json([{ ...channel, associated_prices: [price] }]));
    if (url === '/api/channels/12' && init?.method === 'PUT') return Promise.resolve(json(channel));
    if (url === '/api/settings') return Promise.resolve(json({ request_defaults_enabled: false, request_defaults: {} }));
    if (url === '/api/catalog/prefill/preview') return Promise.resolve(json({ request: JSON.parse(String(init?.body)), patches: [{ target: { kind: 'upstream', channel_id: 12, model: 'model-a' }, config: { ...channel.model_configs!['model-a'], max_input_tokens: 9000 }, catalog_model_ref: { source: 'builtin', id: 'source-model' }, price, warnings: [] }], updated: 1, skipped: 0 }));
    return Promise.resolve(json([]));
  });
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });
async function openChannel() {
  render(<Channels />);
  fireEvent.click(await screen.findByRole('button', { name: '编辑 Personal' }));
  return screen.getByRole('dialog', { name: '编辑渠道' });
}

describe('channel connection responsibility', () => {
  it('ordinary connection save omits specs, associations, channel priority and all price writes', async () => {
    const dialog = await openChannel();
    fireEvent.click(within(dialog).getByRole('button', { name: '模型配置' }));
    await within(dialog).findByRole('region', { name: '已启用模型' });
    expect(within(dialog).queryByLabelText('默认最大输出')).toBeNull();
    expect(within(dialog).queryByRole('button', { name: '配置模型 model-a' })).toBeNull();
    expect(within(dialog).queryByText('渠道优先级')).toBeNull();
    expect(within(dialog).queryByLabelText('优先级')).toBeNull();
    expect(within(dialog).getByText('已配规格')).toBeTruthy();
    expect(within(dialog).getByText('已配价格')).toBeTruthy();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    fireEvent.click(within(dialog).getByRole('button', { name: '复制渠道 ID' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('12'));
    expect(fetcher.mock.calls.some(([, init]) => init?.method)).toBe(false);
    expect(within(dialog).getByRole('button', { name: '模型规格 model-a' })).toHaveProperty('disabled', false);
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetcher.mock.calls.some(([url, init]) => url === '/api/channels/12' && init?.method === 'PUT')).toBe(true));
    const saved = JSON.parse(String(fetcher.mock.calls.find(([url, init]) => url === '/api/channels/12' && init?.method === 'PUT')![1]?.body));
    for (const field of ['config', 'model_configs', 'model_catalog_refs', 'associated_prices', 'associated_price_models', 'has_key', 'priority']) expect(saved).not.toHaveProperty(field);
    expect(saved.api_key).toBe(''); expect(saved.models).toEqual(['model-a']);
    expect(fetcher.mock.calls.some(([url, init]) => url === '/api/channels/display' && init?.method === 'PUT')).toBe(false);
  });
  it.each([['模型规格 model-a', '#/model-specs?channel_id=12&model=model-a'], ['模型价格 model-a', '#/prices?channel_id=12&model=model-a']])('opens the precise target from %s', async (button, hash) => {
    const dialog = await openChannel();
    fireEvent.click(within(dialog).getByRole('button', { name: '模型配置' }));
    await within(dialog).findByRole('region', { name: '已启用模型' });
    fireEvent.click(within(dialog).getByRole('button', { name: button }));
    expect(window.location.hash).toBe(hash);
    expect(fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
  });
  it('disables cross-page jumps and preserves a dirty connection draft on cancel', async () => {
    const dialog = await openChannel();
    const name = within(dialog).getByLabelText('渠道名称');
    fireEvent.change(name, { target: { value: 'Draft connection' } });
    fireEvent.click(within(dialog).getByRole('button', { name: '模型配置' }));
    await within(dialog).findByRole('region', { name: '已启用模型' });
    for (const button of ['模型规格 model-a', '模型价格 model-a']) expect(within(dialog).getByRole('button', { name: button })).toHaveProperty('disabled', true);
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
    expect(screen.getByRole('dialog', { name: '编辑渠道' })).toBe(dialog);
    expect(name).toHaveProperty('value', 'Draft connection');
    expect(fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
  });
});
