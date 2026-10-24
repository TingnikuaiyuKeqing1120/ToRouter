// @vitest-environment jsdom
import { cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { ConfirmProvider } from '../components/Confirm';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ModelSpecs from './ModelSpecs';
import type { UpstreamModelSpecs } from '../api';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });

const entry: UpstreamModelSpecs = { channel_id: 12, channel_name: 'Personal', model: 'vendor/a+b', specs: { context_window: 0, streaming: false, input_modalities: [], output_limit: 2048, output_token_field: 'max_completion_tokens', native_thinking: { supported: true, mandatory: false, modes: [{ id: 'effort', api_format: 'openai_responses', control: 'effort', efforts: ['low', 'high'], disabled_efforts: null }] } }, catalog_model_ref: { source: 'builtin', id: 'source-model', offering_key: 'china' } };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
let fetcher: ReturnType<typeof vi.fn>;
beforeEach(() => {
  localStorage.clear();
  history.replaceState(null, '', '/#/model-specs?channel_id=12&model=vendor%2Fa%2Bb');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/channels') return Promise.resolve(json([{ id: 12, models: [entry.model], enabled: true }, { id: 13, models: [entry.model], enabled: true }]));
    if (url === '/api/model-specs') return Promise.resolve(json(init?.method === 'PUT' ? entry : [entry, { ...entry, channel_id: 13, channel_name: 'Other' }]));
    if (url === '/api/catalog/prefill/preview') return Promise.resolve(json({ request: JSON.parse(String(init?.body)), patches: [{ target: { kind: 'upstream', channel_id: 12, model: entry.model }, config: { ...entry.specs, max_input_tokens: 9000 }, catalog_model_ref: entry.catalog_model_ref, warnings: [] }], updated: 1, skipped: 0 }));
    return Promise.resolve(json([]));
  });
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); history.replaceState(null, '', '/'); });
async function open() { render(<ModelSpecs />); fireEvent.click(await screen.findByRole('button', { name: `编辑规格 12:${entry.model}` })); return screen.getByRole('dialog'); }
const writes = () => fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');

describe('dedicated model specifications', () => {
  it('filters an encoded target and saves only its specs while retaining zero, false, empty and native fields', async () => {
    const dialog = await open();
    expect(screen.queryByRole('button', { name: `编辑规格 13:${entry.model}` })).toBeNull();
    expect(within(dialog).queryByRole('checkbox', { name: '预填价格' })).toBeNull();
    expect(within(dialog).queryByLabelText('默认最大输出')).toBeNull();
    fireEvent.change(within(dialog).getByLabelText('最大输出 tokens'), { target: { value: '4096' } });
    fireEvent.click(within(dialog).getByRole('button', { name: '保存规格' }));
    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0][0]).toBe('/api/model-specs');
    expect(JSON.parse(String(writes()[0][1]?.body))).toEqual({ channel_id: 12, model: entry.model, specs: { ...entry.specs, max_output_tokens: 4096 }, catalog_model_ref: entry.catalog_model_ref });
  });
  it('keeps catalog prefill in the draft until save and uses capabilities only', async () => {
    const dialog = await open();
    fireEvent.click(within(dialog).getByRole('button', { name: '预览预填' }));
    fireEvent.click(await within(dialog).findByRole('button', { name: '填入当前草稿' }));
    expect(writes()).toHaveLength(0);
    expect(fetcher.mock.calls.some(([url]) => url === '/api/catalog/prefill/apply')).toBe(false);
    const preview = JSON.parse(String(fetcher.mock.calls.find(([url]) => url === '/api/catalog/prefill/preview')![1]?.body));
    expect(preview.groups).toEqual(['capabilities']);
    fireEvent.click(within(dialog).getByRole('button', { name: '保存规格' }));
    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(JSON.parse(String(writes()[0][1]?.body)).specs.max_input_tokens).toBe(9000);
  });
  it('keeps a failed save editable and retains a dirty draft when leaving is declined', async () => {
    const base = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => init?.method === 'PUT' ? Promise.resolve(json({ error: '保存失败' }, 500)) : base(url, init));
    const dialog = await open();
    fireEvent.change(within(dialog).getByLabelText('最大输出 tokens'), { target: { value: '123' } });
    fireEvent.click(within(dialog).getByRole('button', { name: '保存规格' }));
    await within(dialog).findByText('保存失败');
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    const confirmation = await screen.findByRole('dialog', { name: '保存更改？' });
    fireEvent.click(within(confirmation).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
    expect(screen.getByRole('dialog')).toBe(dialog);
    expect(within(dialog).getByLabelText('最大输出 tokens')).toHaveProperty('value', '123');
    expect(location.hash).toBe('#/model-specs?channel_id=12&model=vendor%2Fa%2Bb');
  });
  it('allows explicit unknown, empty ranges and empty native metadata without adding defaults', async () => {
    const dialog = await open();
    fireEvent.change(within(dialog).getByLabelText('上下文总窗口'), { target: { value: '' } });
    fireEvent.click(within(dialog).getByLabelText('设置支持的思考选项'));
    fireEvent.click(within(dialog).getByRole('button', { name: '保存规格' }));
    await waitFor(() => expect(writes()).toHaveLength(1));
    const specs = JSON.parse(String(writes()[0][1]?.body)).specs;
    expect(specs).not.toHaveProperty('context_window'); expect(specs).not.toHaveProperty('defaults');
    expect(specs.thinking_efforts).toEqual([]); expect(specs.native_thinking.modes[0].disabled_efforts).toBeNull();
  });
  it.each([null, []])('preserves native enum metadata %j after opening, focusing and leaving advanced information', async enums => {
    const modes = [{ ...entry.specs.native_thinking!.modes[0], efforts: enums, disabled_efforts: enums, default_effort: 'high', budget_min: 0 }];
    const specs = { ...entry.specs, native_thinking: { ...entry.specs.native_thinking!, modes } };
    const base = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => url === '/api/model-specs' && !init?.method ? Promise.resolve(json([{ ...entry, specs }])) : base(url, init));
    const dialog = await open();
    fireEvent.click(within(dialog).getByText('其他规格与原生映射'));
    const details = within(dialog).getByText('其他规格与原生映射').closest('details')!;
    expect(details.open).toBe(true);
    const mapping = within(dialog).getByLabelText('原生思考映射资料');
    expect(JSON.parse(mapping.textContent!)).toEqual({ output_token_field: specs.output_token_field, modes });
    expect(mapping.getAttribute('contenteditable')).toBeNull();
    for (const label of ['原生档位 1', '关闭档位 1', '模式 ID 1', '模式格式 1', '模式控制 1', '最小思考预算 1', '最大思考预算 1', '原生输出预算字段', '输出上限']) expect(within(dialog).queryByLabelText(label)).toBeNull();
    expect(within(dialog).queryByRole('button', { name: '添加原生思考模式' })).toBeNull();
    fireEvent.focus(mapping); fireEvent.blur(mapping);
    fireEvent.change(within(dialog).getByLabelText('最大输入 tokens'), { target: { value: '1234' } });
    fireEvent.change(within(dialog).getByLabelText('原生思考支持'), { target: { value: 'false' } });
    fireEvent.change(within(dialog).getByLabelText('思考是否必需'), { target: { value: 'true' } });
    fireEvent.change(within(dialog).getByLabelText('思考是否必需'), { target: { value: 'false' } });
    fireEvent.click(within(dialog).getByRole('button', { name: '保存规格' }));
    await waitFor(() => expect(writes()).toHaveLength(1));
    const saved = JSON.parse(String(writes()[0][1]?.body)).specs;
    expect(saved.native_thinking).toEqual({ ...specs.native_thinking, supported: false, mandatory: false });
    expect(saved.native_thinking.modes).toEqual(modes);
    expect(saved.max_input_tokens).toBe(1234); expect(saved.output_limit).toBe(specs.output_limit);
    expect(saved.output_token_field).toBe(specs.output_token_field);
  });
  it('keeps selections after matching fails and rebuilds the preview when retried', async () => {
    const base = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => url === '/api/catalog/prefill/apply' ? Promise.resolve(json({ error: '来源已变化，请重新预览' }, 409)) : base(url, init));
    render(<ModelSpecs />);
    fireEvent.click(await screen.findByRole('checkbox', { name: `选择模型 12:${entry.model}` }));
    fireEvent.click(screen.getByRole('button', { name: /^一键匹配/ }));
    await screen.findByText('来源已变化，请重新预览');
    const applied = JSON.parse(String(fetcher.mock.calls.find(([url]) => url === '/api/catalog/prefill/apply')![1]?.body));
    expect(applied.groups).toEqual(['capabilities']); expect(applied.targets).toHaveLength(1);
    expect(applied).not.toHaveProperty('expected_resolution');
    expect(screen.getByRole('checkbox', { name: `选择模型 12:${entry.model}` })).toHaveProperty('checked', true);
    fireEvent.click(screen.getByRole('button', { name: /^一键匹配/ }));
    await waitFor(() => expect(fetcher.mock.calls.filter(([url]) => url === '/api/catalog/prefill/preview')).toHaveLength(2));
  });
});
