// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import ModelSpecs from './ModelSpecs';

const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'Content-Type': 'application/json' } });
const rows = [
  { channel_id: 12, channel_name: 'Zeta', model: 'z-model', specs: { context_window: 0 } },
  { channel_id: 13, channel_name: 'Alpha', model: 'b-model', specs: {} },
  { channel_id: 12, channel_name: 'Zeta', model: 'a-model', specs: {} },
  { channel_id: 13, channel_name: 'Alpha', model: 'a-model', specs: {} },
  { channel_id: 12, channel_name: 'Zeta', model: 'disabled', specs: { max_output_tokens: 1234 } },
];
let fetcher: ReturnType<typeof vi.fn>;
beforeEach(() => {
  localStorage.clear();
  history.replaceState(null, '', '/#/model-specs');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/model-specs') return Promise.resolve(json(rows));
    if (url === '/api/channels') return Promise.resolve(json([
      { id: 12, name: 'Zeta', enabled: false, models: ['z-model', 'a-model'], available_models: ['disabled'] },
      { id: 13, name: 'Alpha', enabled: true, models: ['b-model', 'a-model'] },
    ]));
    if (url === '/api/catalog/prefill/preview') return Promise.resolve(json({ request: JSON.parse(String(init?.body)), patches: [], updated: 0, skipped: 0 }));
    return Promise.resolve(json([]));
  });
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); history.replaceState(null, '', '/'); });
const mount = () => render(<ModelSpecs />, { wrapper: ConfirmProvider });
const models = () => screen.queryAllByRole('button', { name: /^编辑规格 / }).map(button => button.getAttribute('aria-label'));
async function loaded() { await screen.findByRole('button', { name: '编辑规格 12:z-model' }); }
async function previewTargets() {
  fireEvent.click(screen.getByRole('button', { name: /^一键匹配/ }));
  await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url === '/api/catalog/prefill/preview')).toBe(true));
  const request = JSON.parse(String(fetcher.mock.calls.find(([url]) => url === '/api/catalog/prefill/preview')![1]?.body));
  return request.targets.map((target: { channel_id: number; model: string }) => `${target.channel_id}:${target.model}`);
}

it('starts with all channels expanded, sorts channel names and model IDs, and distinguishes zero from unknown', async () => {
  mount(); await loaded();
  expect(models()).toEqual(['编辑规格 13:a-model', '编辑规格 13:b-model', '编辑规格 12:a-model', '编辑规格 12:z-model']);
  expect(screen.getByText('显示渠道 · 2 / 2').closest('details')).toHaveProperty('open', true);
  expect(screen.getByRole('checkbox', { name: '显示渠道 Zeta' })).toHaveProperty('checked', true);
  expect(screen.getByRole('checkbox', { name: '显示渠道 Alpha' })).toHaveProperty('checked', true);
  expect(screen.getByText('0 / 未知')).toBeTruthy();
  expect(screen.getAllByText('未知 / 未知')).toHaveLength(3);
  expect(screen.queryByRole('button', { name: '编辑规格 12:disabled' })).toBeNull();
});

it('supports multiple channel checks, persists an empty selection and restores select all', async () => {
  const first = mount(); await loaded();
  fireEvent.click(screen.getByRole('checkbox', { name: '显示渠道 Alpha' }));
  expect(models()).toEqual(['编辑规格 12:a-model', '编辑规格 12:z-model']);
  fireEvent.click(screen.getByRole('checkbox', { name: '显示渠道 Alpha' }));
  expect(models()).toHaveLength(4);
  fireEvent.click(screen.getByRole('checkbox', { name: '全选渠道' }));
  expect(models()).toHaveLength(0);
  expect(screen.getByText('请选择要显示的渠道。')).toBeTruthy();
  first.unmount(); mount();
  await screen.findByText('显示渠道 · 0 / 2');
  expect(models()).toHaveLength(0);
  fireEvent.click(screen.getByRole('checkbox', { name: '全选渠道' }));
  await loaded();
  expect(models()).toHaveLength(4);
});

it('restores a nonempty channel preference and lets an initial target override it', async () => {
  const first = mount(); await loaded();
  fireEvent.click(screen.getByRole('checkbox', { name: '显示渠道 Zeta' }));
  first.unmount();
  const second = mount();
  await screen.findByRole('button', { name: '编辑规格 13:b-model' });
  expect(models()).toEqual(['编辑规格 13:a-model', '编辑规格 13:b-model']);
  second.unmount();
  history.replaceState(null, '', '/#/model-specs?channel_id=12&model=z-model');
  mount(); await loaded();
  expect(models()).toEqual(['编辑规格 12:z-model']);
  expect(screen.getByRole('checkbox', { name: '显示渠道 Zeta' })).toHaveProperty('checked', true);
  expect(screen.getByRole('checkbox', { name: '显示渠道 Alpha' })).toHaveProperty('checked', false);
});

it('keeps the selected channel count and filters when the channel section is collapsed', async () => {
  mount(); await loaded();
  fireEvent.click(screen.getByRole('checkbox', { name: '显示渠道 Alpha' }));
  fireEvent.click(screen.getByText('显示渠道 · 1 / 2'));
  expect(screen.getByText('显示渠道 · 1 / 2').closest('details')).toHaveProperty('open', false);
  fireEvent.change(screen.getByRole('textbox', { name: '搜索模型规格' }), { target: { value: 'z-model' } });
  expect(screen.getByText('显示渠道 · 1 / 2').closest('details')).toHaveProperty('open', false);
  expect(models()).toEqual(['编辑规格 12:z-model']);
});

it('disables empty batch actions and previews exactly the individually selected visible row', async () => {
  mount(); await loaded();
  expect(screen.getByRole('button', { name: /^一键匹配/ })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('checkbox', { name: '选择模型 12:a-model' }));
  expect(screen.getByRole('checkbox', { name: '全选可见模型' })).toHaveProperty('indeterminate', true);
  expect(await previewTargets()).toEqual(['12:a-model']);
});

it('excludes search-hidden selections from the batch and leaves hidden selection intact on visible deselection', async () => {
  mount(); await loaded();
  fireEvent.click(screen.getByRole('checkbox', { name: '全选可见模型' }));
  fireEvent.change(screen.getByRole('textbox', { name: '搜索模型规格' }), { target: { value: 'z-model' } });
  fireEvent.click(screen.getByRole('checkbox', { name: '全选可见模型' }));
  expect(screen.getByRole('button', { name: /^一键匹配/ })).toHaveProperty('disabled', true);
  fireEvent.change(screen.getByRole('textbox', { name: '搜索模型规格' }), { target: { value: 'b-model' } });
  expect(await previewTargets()).toEqual(['13:b-model']);
});

it('excludes channel-hidden selections from batch and selects only visible rows', async () => {
  mount(); await loaded();
  fireEvent.click(screen.getByRole('checkbox', { name: '选择模型 13:b-model' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '显示渠道 Alpha' }));
  expect(screen.getByRole('button', { name: /^一键匹配/ })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('checkbox', { name: '全选可见模型' }));
  expect(await previewTargets()).toEqual(['12:a-model', '12:z-model']);
});

it('continues to filter and select channels when browser preference storage is unavailable', async () => {
  vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('unavailable'); });
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('unavailable'); });
  mount(); await loaded();
  fireEvent.click(screen.getByRole('checkbox', { name: '显示渠道 Zeta' }));
  expect(models()).toEqual(['编辑规格 13:a-model', '编辑规格 13:b-model']);
});
