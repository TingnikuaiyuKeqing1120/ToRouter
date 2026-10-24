// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import type { CatalogItem, PrefillPreview, PrefillRequest, UpstreamModelSpecs } from '../api';
import ModelSpecs from './ModelSpecs';

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const source: CatalogItem = {
  source: 'builtin', id: 'canonical', name: 'Canonical Model', provider: 'Vendor', context_length: 100000,
  max_input_tokens: null, max_output_tokens: 4000, modalities: { input: ['text', 'image'], output: ['text'] },
  thinking: { supported: true, mandatory: false, modes: [] },
  offerings: [
    { key: 'china', provider: 'Vendor CN', region: '中国', source_kind: 'original', quotes: [] },
    { key: 'global', provider: 'Vendor Global', region: '海外', source_kind: 'original', quotes: [] },
  ],
};
const existing = { context_window: 0, streaming: false, input_modalities: [], native_thinking: { supported: false, mandatory: false, modes: [] }, output_token_field: 'max_completion_tokens' };
let rows: UpstreamModelSpecs[];
let fetcher: ReturnType<typeof vi.fn>;
let previewHandler: ((request: PrefillRequest) => Promise<Response>) | undefined;
let applyHandler: ((request: PrefillRequest) => Promise<Response>) | undefined;
function plan(request: PrefillRequest): PrefillPreview {
  return {
    request, updated: request.targets.filter(target => target.model !== 'unmatched').length, skipped: 0,
    patches: request.targets.map(target => ({ target, config: { ...existing, max_output_tokens: 4000 }, warnings: [], ...(target.model === 'unmatched' ? {} : { catalog_model_ref: target.ref || { source: 'builtin', id: 'canonical' } }) })),
  };
}
beforeEach(() => {
  localStorage.clear(); history.replaceState(null, '', '/#/model-specs');
  previewHandler = undefined; applyHandler = undefined;
  rows = [
    { channel_id: 1, channel_name: 'First', model: 'alias', specs: { ...existing }, catalog_model_ref: { source: 'builtin', id: 'canonical', offering_key: 'china' } },
    { channel_id: 1, channel_name: 'First', model: 'unmatched', specs: {} },
    { channel_id: 2, channel_name: 'Second', model: 'other', specs: {} },
  ];
  fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/model-specs') return Promise.resolve(json(rows));
    if (url === '/api/channels') return Promise.resolve(json([{ id: 1, name: 'First', models: ['alias', 'unmatched'] }, { id: 2, name: 'Second', models: ['other'] }]));
    if (url === '/api/catalog/models?source=all') return Promise.resolve(json([source]));
    if (url === '/api/catalog/prefill/preview') {
      const request = JSON.parse(String(init?.body)); return previewHandler ? previewHandler(request) : Promise.resolve(json(plan(request)));
    }
    if (url === '/api/catalog/prefill/apply') {
      const request: PrefillRequest = JSON.parse(String(init?.body));
      if (applyHandler) return applyHandler(request);
      const applied = plan(request);
      rows = rows.map(row => {
        const patch = applied.patches.find(patch => patch.target.channel_id === row.channel_id && patch.target.model === row.model);
        return patch ? { ...row, specs: patch.config, catalog_model_ref: patch.catalog_model_ref } : row;
      });
      return Promise.resolve(json(applied));
    }
    return Promise.resolve(json([]));
  });
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); history.replaceState(null, '', '/'); });
const mount = async () => { render(<ModelSpecs />, { wrapper: ConfirmProvider }); await screen.findByRole('combobox', { name: '搜索规格资料 1:alias' }); };
const requests = (suffix: string): PrefillRequest[] => fetcher.mock.calls.filter(([url]) => url === `/api/catalog/prefill/${suffix}`).map(([, init]) => JSON.parse(String(init?.body)));
const selectAll = () => fireEvent.click(screen.getByRole('checkbox', { name: '全选可见模型' }));
const match = () => fireEvent.click(screen.getByRole('button', { name: /^一键匹配/ }));
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done; }); return { promise, resolve }; }

it('uses an indeterminate channel checkbox and renders explicit zero, empty modalities and unsupported thinking', async () => {
  await mount();
  fireEvent.click(screen.getByRole('checkbox', { name: '显示渠道 Second' }));
  expect(screen.getByRole('checkbox', { name: '全选渠道' })).toHaveProperty('indeterminate', true);
  const row = screen.getByRole('button', { name: '编辑规格 1:alias' }).closest('tr')!;
  expect(within(row).getByText('0 / 未知')).toBeTruthy();
  expect(within(row).getByText('不支持')).toBeTruthy();
  expect(screen.queryByRole('columnheader', { name: '规格来源' })).toBeNull();
  expect(within(row).getByText('builtin · canonical · 中国')).toBeTruthy();
});

it('loads the catalog once for all rows and keeps searchable regional choices in a portal with keyboard and Escape support', async () => {
  await mount();
  const picker = screen.getByRole('combobox', { name: '搜索规格资料 1:alias' });
  fireEvent.focus(picker);
  const list = screen.getByRole('listbox', { name: '规格资料 1:alias' });
  expect(list.parentElement).toBe(document.body);
  fireEvent.change(picker, { target: { value: '海外' } });
  expect(within(list).getAllByRole('option')).toHaveLength(1);
  fireEvent.keyDown(picker, { key: 'ArrowDown' });
  expect(picker.getAttribute('aria-activedescendant')).toBe(within(list).getByRole('option').id);
  fireEvent.keyDown(picker, { key: 'Escape' });
  expect(screen.queryByRole('listbox')).toBeNull();
  fireEvent.focus(screen.getByRole('combobox', { name: '搜索规格资料 2:other' }));
  fireEvent.keyDown(screen.getByRole('combobox', { name: '搜索规格资料 2:other' }), { key: 'Tab' });
  expect(screen.queryByRole('listbox')).toBeNull();
  expect(fetcher.mock.calls.filter(([url]) => url === '/api/catalog/models?source=all')).toHaveLength(1);
  expect(requests('apply')).toHaveLength(0);
});

it('saves an explicitly chosen region with capabilities-only fill and never sends a quotation reference', async () => {
  await mount();
  const picker = screen.getByRole('combobox', { name: '搜索规格资料 1:alias' });
  fireEvent.change(picker, { target: { value: '海外' } });
  fireEvent.keyDown(picker, { key: 'ArrowDown' }); fireEvent.keyDown(picker, { key: 'Enter' });
  await screen.findByText('匹配完成：更新 1 项，保留 0 项。');
  for (const request of [...requests('preview'), ...requests('apply')]) {
    expect(request.groups).toEqual(['capabilities']); expect(request.force_overwrite).toBe(false);
    expect(request.targets).toEqual([{ kind: 'upstream', channel_id: 1, model: 'alias', ref: { source: 'builtin', id: 'canonical', offering_key: 'global' } }]);
    expect(JSON.stringify(request)).not.toContain('quote_key'); expect(JSON.stringify(request)).not.toContain('price_ref');
  }
  await waitFor(() => expect(screen.getByText('builtin · canonical · 海外')).toBeTruthy());
  expect(rows[0].specs).toEqual({ ...existing, max_output_tokens: 4000 });
});

it('previews only checked visible rows and applies only identities verified by the server', async () => {
  await mount(); selectAll();
  fireEvent.click(screen.getByRole('checkbox', { name: '显示渠道 Second' })); match();
  await screen.findByText('First · unmatched：未找到明确匹配，请搜索选择资料。');
  expect(requests('preview')[0].targets.map(target => target.model)).toEqual(['alias', 'unmatched']);
  expect(requests('apply')[0].targets.map(target => target.model)).toEqual(['alias']);
  expect(requests('apply')[0].targets[0].ref).toEqual({ source: 'builtin', id: 'canonical', offering_key: 'china' });
});

it('reports unmatched models without applying a guessed alias or region', async () => {
  await mount(); fireEvent.click(screen.getByRole('checkbox', { name: '选择模型 1:unmatched' })); match();
  await screen.findByText('First · unmatched：未找到明确匹配，请搜索选择资料。');
  expect(requests('apply')).toHaveLength(0);
});

it.each(['search', 'channels', 'selection'])('discards late previews when %s changes, even if the original scope is restored', async kind => {
  const late = deferred<Response>(); previewHandler = () => late.promise;
  await mount(); selectAll(); match(); await waitFor(() => expect(requests('preview')).toHaveLength(1));
  if (kind === 'search') {
    const search = screen.getByRole('textbox', { name: '搜索模型规格' });
    fireEvent.change(search, { target: { value: 'other' } }); fireEvent.change(search, { target: { value: '' } });
  } else {
    const checkbox = screen.getByRole('checkbox', { name: kind === 'channels' ? '显示渠道 Second' : '选择模型 2:other' });
    fireEvent.click(checkbox); fireEvent.click(checkbox);
  }
  await act(async () => late.resolve(json(plan(requests('preview')[0]))));
  await waitFor(() => expect(screen.getByRole('button', { name: /^一键匹配/ })).toHaveProperty('disabled', false));
  expect(requests('apply')).toHaveLength(0);
});

it('locks edits and scope during apply and keeps the view and selection on failure for a fresh retry', async () => {
  const late = deferred<Response>(); applyHandler = () => late.promise;
  await mount(); selectAll(); match();
  await waitFor(() => expect(requests('apply')).toHaveLength(1));
  expect(screen.getByRole('textbox', { name: '搜索模型规格' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('checkbox', { name: '全选渠道' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('checkbox', { name: '全选可见模型' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('button', { name: '编辑规格 1:alias' })).toHaveProperty('disabled', true);
  expect(screen.getByRole('combobox', { name: '搜索规格资料 1:alias' })).toHaveProperty('disabled', true);
  await act(async () => late.resolve(json({ error: '保存失败' }, 500)));
  await screen.findByText('保存失败');
  expect(screen.getByRole('checkbox', { name: '选择模型 1:alias' })).toHaveProperty('checked', true);
  expect(screen.getByRole('button', { name: '编辑规格 1:alias' })).toHaveProperty('disabled', false);
  applyHandler = undefined; match();
  await screen.findByText('匹配完成：更新 2 项，保留 0 项。');
  expect(requests('preview')).toHaveLength(2);
});

it('does not apply when an in-flight preview resolves after unmount', async () => {
  const late = deferred<Response>(); previewHandler = () => late.promise;
  await mount(); selectAll(); match(); await waitFor(() => expect(requests('preview')).toHaveLength(1));
  cleanup(); await act(async () => late.resolve(json(plan(requests('preview')[0]))));
  expect(requests('apply')).toHaveLength(0);
});
