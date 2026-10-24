// @vitest-environment jsdom
import { useMemo, useRef, useState } from 'react';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { CatalogItem, CatalogSourceStatus } from '../api';
import { ConfirmProvider } from '../components/Confirm';
import { NavigationGuard, type BeforeLeave } from '../components/NavigationGuard';
import { ToastProvider } from '../components/ui';
import Catalog from './Catalog';
import { catalogSessionCache, resetCatalogView } from './Catalog.cache';
import { clearStableResources } from '../services/stableResources';

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
const sources: CatalogSourceStatus[] = [
  { source: 'builtin', name: '精选库', enabled: true, prefill_enabled: true, priority: 1, item_count: 1 },
  { source: 'openrouter', name: 'OpenRouter', enabled: false, prefill_enabled: true, priority: 2, item_count: 1 },
  { source: 'litellm', name: 'LiteLLM', enabled: true, prefill_enabled: false, priority: 3, item_count: 0 },
];
function models(): CatalogItem[] {
  const quote = (currency: 'USD' | 'CNY') => ({ key: 'payg', name: '标准', source_kind: 'original', source_url: 'https://example.com/prices', verified_at: '2026-10-18', notes: ['报价备注'], table: { currency, unit_tokens: 1000000, rates: { input: 1, output: 0, cache_read: null, cache_write: 0.25 }, rules: [] } });
  return [
    { id: 'regional', name: '区域模型', provider: 'Vendor', source: 'builtin', context_length: 128000, max_input_tokens: 120000, max_output_tokens: 8192, modalities: { input: ['text', 'image'], output: ['text'] }, thinking: { supported: false, modes: [] }, notes: ['模型备注'], offerings: [
      { key: 'china', provider: 'Vendor-中国', region: 'china', source_kind: 'original', quotes: [quote('CNY')] },
      { key: 'global', provider: 'Vendor-海外', region: 'global', source_kind: 'original', quotes: [quote('USD')] },
    ] },
    { id: 'external', name: '外部模型', provider: 'External', source: 'openrouter', context_length: null, max_input_tokens: null, max_output_tokens: null, modalities: { input: [], output: [] }, thinking: { supported: null, modes: [] }, offerings: [] },
  ];
}
function Harness() {
  const handlers = useRef<BeforeLeave[]>([]), [left, setLeft] = useState(false);
  const guard = useMemo(() => ({ register(handler: BeforeLeave) { handlers.current.push(handler); return () => { handlers.current = handlers.current.filter(current => current !== handler); }; } }), []);
  return <NavigationGuard.Provider value={guard}><button onClick={async () => { try { for (const handler of [...handlers.current].reverse()) await handler(); setLeft(true); } catch {} }}>离开资料页</button>{left ? <p>导航完成</p> : <Catalog />}</NavigationGuard.Provider>;
}
function setup(initialModels = models(), deferInitialReads = false) {
  let currentSources = structuredClone(sources), currentModels = initialModels, rejectSave = false, rejectModels = false, rejectSources = false, rejectPull = false, deferSources = deferInitialReads, deferModels = deferInitialReads;
  const pendingSources: { finish: (response: Response) => void; value: CatalogSourceStatus[] }[] = [], pendingModels: { finish: (response: Response) => void; value: CatalogItem[] }[] = [];
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/catalog/models?source=all') {
      if (rejectModels) return Promise.resolve(json({ error: 'models unavailable' }, 503));
      if (deferModels) { deferModels = false; return new Promise<Response>(finish => pendingModels.push({ finish, value: structuredClone(currentModels) })); }
      return Promise.resolve(json(currentModels));
    }
    if (url === '/api/catalog/sources') {
      if (init?.method === 'PUT') {
        if (rejectSave) return Promise.resolve(json({ error: 'sources rejected' }, 409));
        currentSources = JSON.parse(String(init.body)); return Promise.resolve(json(currentSources));
      }
      if (rejectSources) return Promise.resolve(json({ error: 'sources unavailable' }, 503));
      if (deferSources) { deferSources = false; return new Promise<Response>(finish => pendingSources.push({ finish, value: structuredClone(currentSources) })); }
      return Promise.resolve(json(currentSources));
    }
    if (url === '/api/catalog/fetch') return Promise.resolve(rejectPull ? json({ error: 'pull unavailable' }, 503) : json({ source: JSON.parse(String(init?.body)).source, count: currentModels.length }));
    if (url === '/api/exchange-rate') return Promise.resolve(json({ available: true, rate: 7, base: 'USD', quote: 'CNY' }));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal('fetch', fetcher);
  const mount = () => render(<ConfirmProvider><ToastProvider><Harness /></ToastProvider></ConfirmProvider>);
  let result = mount();
  return {
    fetcher, result, sources: () => currentSources,
    remount: () => { result.unmount(); result = mount(); },
    updateSources: (value: CatalogSourceStatus[]) => { currentSources = value; },
    rejectSave: (value: boolean) => { rejectSave = value; }, rejectModels: (value: boolean) => { rejectModels = value; }, rejectSources: (value: boolean) => { rejectSources = value; }, rejectPull: (value: boolean) => { rejectPull = value; },
    deferSources: () => { deferSources = true; }, deferModels: () => { deferModels = true; }, models: (value: CatalogItem[]) => { currentModels = value; },
    pendingSources, pendingModels,
    finishSource: async () => { const pending = pendingSources.shift()!; await act(async () => pending.finish(json(pending.value))); },
    finishModel: async () => { const pending = pendingModels.shift()!; await act(async () => pending.finish(json(pending.value))); },
  };
}
async function ready() { await screen.findByRole('checkbox', { name: 'OpenRouter 参与预填' }); fireEvent.click(screen.getByText('来源与预填管理')); }
beforeEach(() => {
  clearStableResources(); resetCatalogView();
  catalogSessionCache.models = undefined; catalogSessionCache.sources = undefined;
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('protects dirty source preferences on navigation and retains rejected drafts until a successful save', async () => {
  const backend = setup(); await ready(); backend.rejectSave(true);
  fireEvent.click(screen.getByLabelText('OpenRouter 参与预填'));
  expect(screen.getByText('3 个来源 · 未保存')).toBeTruthy();
  fireEvent.click(screen.getByText('离开资料页')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('sources rejected'); expect(screen.queryByText('导航完成')).toBeNull();
  expect(screen.getByLabelText('OpenRouter 参与预填')).toHaveProperty('checked', false);
  backend.rejectSave(false); fireEvent.click(screen.getByText('离开资料页')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('导航完成'); expect(backend.sources().find(source => source.source === 'openrouter')?.prefill_enabled).toBe(false);
});

it('lets global discard leave without writing preferences', async () => {
  const backend = setup(); await ready(); fireEvent.click(screen.getByLabelText('OpenRouter 参与预填'));
  fireEvent.click(screen.getByText('离开资料页')); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
  await screen.findByText('导航完成'); expect(backend.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
});

it('keeps edited source preferences during a pull and ignores an old source read arriving after save', async () => {
  const backend = setup(); await ready(); fireEvent.click(screen.getByLabelText('OpenRouter 参与预填'));
  backend.deferSources(); fireEvent.click(screen.getByRole('button', { name: '立即拉取 OpenRouter' }));
  await waitFor(() => expect(backend.pendingSources).toHaveLength(1));
  fireEvent.change(screen.getByLabelText('OpenRouter 推荐优先级'), { target: { value: '8' } });
  fireEvent.click(screen.getByRole('button', { name: '保存来源偏好' }));
  await screen.findByText('来源偏好已保存'); await backend.finishSource();
  expect(screen.getByLabelText('OpenRouter 参与预填')).toHaveProperty('checked', false);
  expect(screen.getByLabelText('OpenRouter 推荐优先级')).toHaveProperty('value', '8');
  expect(screen.getByRole('button', { name: '保存来源偏好' })).toHaveProperty('disabled', true);
});

it('merges refreshed source metadata around unsaved controls and saves exact ordered priorities without changing enabled', async () => {
  const backend = setup(); await ready(); fireEvent.click(screen.getByLabelText('OpenRouter 参与预填'));
  fireEvent.click(screen.getByRole('button', { name: '上移来源 LiteLLM' }));
  fireEvent.click(screen.getByRole('button', { name: '立即拉取 OpenRouter' }));
  await waitFor(() => expect(screen.getByRole('button', { name: '立即拉取 OpenRouter' })).toHaveProperty('disabled', false));
  expect(screen.getByLabelText('OpenRouter 参与预填')).toHaveProperty('checked', false);
  fireEvent.click(screen.getByRole('button', { name: '保存来源偏好' })); await screen.findByText('来源偏好已保存');
  const body = JSON.parse(String(backend.fetcher.mock.calls.find(([url, init]) => url === '/api/catalog/sources' && init?.method === 'PUT')![1]?.body)) as CatalogSourceStatus[];
  expect(body.map(source => [source.source, source.priority, source.prefill_enabled, source.enabled])).toEqual([['builtin', 1, true, true], ['litellm', 2, false, true], ['openrouter', 3, false, false]]);
});

it('keeps empty-library pull labels consistent with the selected actual source and offers retry after failure', async () => {
  const backend = setup([]); await screen.findByText('模型数据库为空');
  fireEvent.change(screen.getByLabelText('拉取来源'), { target: { value: 'litellm' } }); backend.rejectPull(true);
  fireEvent.click(screen.getByRole('button', { name: '立即拉取 LiteLLM 数据库' }));
  await screen.findByText('拉取失败：pull unavailable');
  expect(backend.fetcher.mock.calls.find(([url]) => url === '/api/catalog/fetch')?.[1]?.body).toBe(JSON.stringify({ source: 'litellm' }));
  backend.rejectPull(false); fireEvent.click(screen.getByRole('button', { name: '重试' }));
  await waitFor(() => expect(backend.fetcher.mock.calls.filter(([url]) => url === '/api/catalog/fetch')).toHaveLength(2));
});

it('counts regional entries independently, clears every filter, and exposes only a page-level public-model link', async () => {
  setup(); await screen.findByText('显示 3 / 3 项资料');
  fireEvent.change(screen.getByLabelText('搜索模型数据库'), { target: { value: '中国' } });
  expect(screen.getByText('显示 1 / 3 项资料')).toBeTruthy();
  fireEvent.change(screen.getByLabelText('筛选厂商'), { target: { value: 'External' } });
  expect(screen.getByText('显示 0 / 3 项资料')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '清除筛选' })); expect(screen.getByText('显示 3 / 3 项资料')).toBeTruthy();
  expect(screen.getByLabelText('筛选厂商')).toHaveProperty('value', 'all'); expect(screen.getByLabelText('搜索模型数据库')).toHaveProperty('value', '');
  expect(screen.getAllByRole('link', { name: '公开模型' })).toHaveLength(1); expect(screen.getByRole('link', { name: '公开模型' }).getAttribute('href')).toBe('#/models');
});

it('preserves original canonical metadata and quote regions behind progressive disclosure', async () => {
  const original = models(); setup(original); await screen.findByText('Vendor-中国', { selector: 'span' });
  const china = screen.getByText('Vendor-中国', { selector: 'span' }).closest('article')!;
  expect(within(china).getByText('输入').parentElement?.textContent).toBe('输入 1');
  const overseas = screen.getByText('Vendor-海外', { selector: 'span' }).closest('article')!;
  expect(within(overseas).getByText('输入').parentElement?.textContent).toBe('输入 7');
  const raw = within(china).getByText('原始模型资料 JSON').closest('details')!;
  expect(raw.open).toBe(false); expect(raw.querySelector('pre')).toBeNull();
  fireEvent.click(within(china).getByText('原始模型资料 JSON'));
  await waitFor(() => expect(raw.querySelector('pre')).not.toBeNull());
  expect(JSON.parse(raw.querySelector('pre')!.textContent!)).toEqual(original[0]);
  expect(screen.getByRole('button', { name: '拉取该模型供应商报价' }).closest('details')).toBeNull();
});

it('keeps good model data visible when a later model refresh fails, independently of source loading', async () => {
  const backend = setup(); await screen.findByText('外部模型'); backend.rejectModels(true);
  fireEvent.click(screen.getByRole('button', { name: '立即拉取 OpenRouter' }));
  await screen.findByText('模型刷新失败，显示上次资料：models unavailable');
  expect(screen.getByText('外部模型')).toBeTruthy(); expect(screen.getByLabelText('OpenRouter 参与预填')).toBeTruthy();
});

it('does not let an older model read replace a newer completed pull', async () => {
  const backend = setup(); await screen.findByText('外部模型'); backend.deferModels();
  fireEvent.click(screen.getByRole('button', { name: '立即拉取 OpenRouter' })); await waitFor(() => expect(backend.pendingModels).toHaveLength(1));
  expect(screen.getByText('正在刷新模型资料，保留当前结果…')).toBeTruthy();
  backend.models([{ ...models()[1], name: '新报价模型' }]);
  fireEvent.click(screen.getByRole('button', { name: '拉取该模型供应商报价' })); await screen.findByText('新报价模型');
  await backend.finishModel(); expect(screen.queryByText('外部模型')).toBeNull(); expect(screen.getByText('新报价模型')).toBeTruthy();
  backend.rejectModels(true); backend.remount();
  expect(screen.getByText('新报价模型')).toBeTruthy();
  await screen.findByText('模型刷新失败，显示上次资料：models unavailable');
});

it('loads initially without a cache and immediately restores regional models and sources on remount while refreshing', async () => {
  const backend = setup(models(), true);
  expect(screen.getAllByText('正在加载数据…')).toHaveLength(2);
  expect(screen.queryByText('外部模型')).toBeNull();
  await backend.finishModel(); await backend.finishSource();
  expect(screen.getByText('显示 3 / 3 项资料')).toBeTruthy();

  backend.models([{ ...models()[1], name: '最新模型' }]);
  backend.updateSources(sources.map(source => ({ ...source, item_count: 7 })));
  backend.deferModels(); backend.deferSources(); backend.remount();
  expect(screen.getByText('显示 3 / 3 项资料')).toBeTruthy();
  expect(screen.getByText('Vendor-中国', { selector: 'span' })).toBeTruthy();
  expect(screen.getByText('Vendor-海外', { selector: 'span' })).toBeTruthy();
  expect(screen.getByLabelText('OpenRouter 参与预填')).toHaveProperty('checked', true);
  expect(screen.queryByText('正在加载数据…')).toBeNull();
  expect(screen.queryByText('正在刷新模型资料，保留当前结果…')).toBeNull();
  expect(screen.queryByText(/正在更新来源状态/)).toBeNull();
  await backend.finishModel(); await backend.finishSource();
  expect(screen.getByText('最新模型')).toBeTruthy();
  expect(screen.getByText('显示 1 / 1 项资料')).toBeTruthy();
  expect(screen.getAllByText('7 款')).toHaveLength(3);
});

it('retains both successful snapshots when background reads fail after remount', async () => {
  const backend = setup(); await ready(); await screen.findByText('外部模型');
  backend.rejectModels(true); backend.rejectSources(true); backend.remount();
  expect(screen.getByText('外部模型')).toBeTruthy();
  expect(screen.getByLabelText('OpenRouter 推荐优先级')).toHaveProperty('value', '2');
  await screen.findByText('模型刷新失败，显示上次资料：models unavailable');
  await screen.findByText('sources unavailable');
  backend.rejectModels(false); backend.rejectSources(false);
  backend.deferModels(); backend.deferSources(); backend.remount();
  expect(screen.getByText('外部模型')).toBeTruthy();
  expect(screen.getByLabelText('OpenRouter 参与预填')).toHaveProperty('checked', true);
  await backend.finishModel(); await backend.finishSource();
});

it('caches refreshed source metadata without caching unsaved preferences discarded on leave', async () => {
  const backend = setup(); await ready();
  fireEvent.click(screen.getByLabelText('OpenRouter 参与预填'));
  fireEvent.change(screen.getByLabelText('OpenRouter 推荐优先级'), { target: { value: '8' } });
  backend.updateSources(sources.map(source => source.source === 'openrouter' ? { ...source, item_count: 9, last_fetched_at: '2026-10-18T00:00:00Z' } : source));
  fireEvent.click(screen.getByRole('button', { name: '立即拉取 OpenRouter' }));
  await screen.findByText(/9 款 · 更新于/);
  expect(screen.getByLabelText('OpenRouter 参与预填')).toHaveProperty('checked', false);
  expect(screen.getByLabelText('OpenRouter 推荐优先级')).toHaveProperty('value', '8');
  fireEvent.click(screen.getByText('离开资料页'));
  fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
  await screen.findByText('导航完成');
  backend.deferModels(); backend.deferSources(); backend.remount();
  expect(screen.getByLabelText('OpenRouter 参与预填')).toHaveProperty('checked', true);
  expect(screen.getByLabelText('OpenRouter 推荐优先级')).toHaveProperty('value', '2');
  expect(screen.getByText(/9 款 · 更新于/)).toBeTruthy();
  expect(screen.getByRole('button', { name: '保存来源偏好' })).toHaveProperty('disabled', true);
  expect(backend.fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
  await backend.finishModel(); await backend.finishSource();
});

it('restores preferences and metadata returned by a successful save even if the next read fails', async () => {
  const backend = setup(); await ready();
  const base = backend.fetcher.getMockImplementation()!;
  backend.fetcher.mockImplementation((url: string, init?: RequestInit) => {
    if (url === '/api/catalog/sources' && init?.method === 'PUT') {
      const saved = (JSON.parse(String(init.body)) as CatalogSourceStatus[]).map(source => ({ ...source, item_count: 11 }));
      backend.updateSources(saved); return Promise.resolve(json(saved));
    }
    return base(url, init);
  });
  fireEvent.click(screen.getByLabelText('OpenRouter 参与预填'));
  fireEvent.change(screen.getByLabelText('OpenRouter 推荐优先级'), { target: { value: '8' } });
  fireEvent.click(screen.getByRole('button', { name: '保存来源偏好' }));
  await screen.findByText('来源偏好已保存');
  backend.rejectSources(true); backend.remount();
  expect(screen.getByLabelText('OpenRouter 参与预填')).toHaveProperty('checked', false);
  expect(screen.getByLabelText('OpenRouter 推荐优先级')).toHaveProperty('value', '8');
  expect(screen.getAllByText('11 款')).toHaveLength(3);
  expect(screen.getByRole('button', { name: '保存来源偏好' })).toHaveProperty('disabled', true);
  await screen.findByText('sources unavailable');
});

it('ignores delayed reads from an unmounted page without replacing the newer session cache', async () => {
  const backend = setup(); await ready(); await screen.findByText('外部模型');
  backend.deferModels(); backend.deferSources(); backend.remount();
  backend.models([{ ...models()[1], name: '重新进入后的模型' }]);
  backend.updateSources(sources.map(source => ({ ...source, item_count: 12 })));
  backend.remount();
  // Navigation reuses in-flight reads; an explicit pull supersedes them.
  fireEvent.click(screen.getByRole('button', { name: '立即拉取 OpenRouter' }));
  await screen.findByText('重新进入后的模型'); await screen.findAllByText('12 款');
  await backend.finishModel(); await backend.finishSource();
  backend.deferModels(); backend.deferSources(); backend.remount();
  expect(screen.getByText('重新进入后的模型')).toBeTruthy();
  expect(screen.queryByText('外部模型')).toBeNull();
  expect(screen.getAllByText('12 款')).toHaveLength(3);
  await backend.finishModel(); await backend.finishSource();
});

it('bounds large-library DOM, searches all pages, and restores the current page without flashing on identical revalidation', async () => {
  const base = models()[1];
  const large = Array.from({ length: 109 }, (_, index) => ({ ...base, id: `model-${index}`, name: `大库模型 ${index}` }));
  const backend = setup(large);
  await screen.findByText('显示 109 / 109 项资料');
  expect(document.querySelectorAll('.catalog-card')).toHaveLength(36);
  expect(screen.getByText('第 1 / 4 页 · 1–36 项')).toBeTruthy();
  expect(screen.queryByRole('heading', { name: '大库模型 36' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '下一页' }));
  expect(screen.getByText('第 2 / 4 页 · 37–72 项')).toBeTruthy();
  expect(screen.getByRole('heading', { name: '大库模型 36' })).toBeTruthy();
  const previous = catalogSessionCache.models;
  backend.deferModels(); backend.deferSources(); backend.remount();
  expect(screen.getByText('第 2 / 4 页 · 37–72 项')).toBeTruthy();
  expect(screen.queryByText(/正在刷新模型资料/)).toBeNull();
  expect(document.querySelectorAll('.catalog-card')).toHaveLength(36);
  await backend.finishModel(); await backend.finishSource();
  expect(catalogSessionCache.models).toBe(previous);
  fireEvent.change(screen.getByLabelText('搜索模型数据库'), { target: { value: 'model-108' } });
  expect(screen.getByText('显示 1 / 109 项资料')).toBeTruthy();
  expect(screen.getByRole('heading', { name: '大库模型 108' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '清除筛选' }));
  expect(screen.getByText('第 1 / 4 页 · 1–36 项')).toBeTruthy();
});

it('keeps first-load FX pending distinct from unavailable and restores valid FX silently on revisit', async () => {
  const backend = setup([], true);
  const base = backend.fetcher.getMockImplementation()!;
  let finishRate!: (response: Response) => void;
  backend.fetcher.mockImplementation((url: string, init?: RequestInit) => url === '/api/exchange-rate' ? new Promise<Response>(resolve => { finishRate = resolve; }) : base(url, init));
  // The initial exchange request has already started. Remount with a fresh
  // session so this scenario has a genuinely pending first FX response.
  clearStableResources(); backend.models(models()); backend.remount();
  await screen.findByText('Vendor-海外', { selector: 'span' });
  expect(screen.getByText('正在读取汇率…')).toBeTruthy();
  expect(screen.getByText('USD / 1M Tokens · 正在读取汇率，保留原币')).toBeTruthy();
  expect(screen.queryByText(/汇率不可用/)).toBeNull();
  await act(async () => finishRate(json({ available: true, rate: 7, base: 'USD', quote: 'CNY' })));
  expect(screen.getByText('1 USD ≈ 7 CNY')).toBeTruthy();
  backend.remount();
  expect(screen.getByText('1 USD ≈ 7 CNY')).toBeTruthy();
  expect(screen.queryByText('正在读取汇率…')).toBeNull();
  expect(screen.queryByText(/汇率不可用/)).toBeNull();
  await act(async () => finishRate(json({ available: true, rate: 7, base: 'USD', quote: 'CNY' })));
  await backend.finishModel(); await backend.finishSource();
});

it('supports pagination via bottom controls and updates page and disable boundaries', async () => {
  const base = models()[1];
  const large = Array.from({ length: 73 }, (_, index) => ({ ...base, id: `model-${index}`, name: `大库模型 ${index}` }));
  setup(large);
  await screen.findByText('显示 73 / 73 项资料');
  expect(screen.getByText('第 1 / 3 页')).toBeTruthy();
  const bottomPrev = screen.getByRole('button', { name: '上一页（底部）' });
  const bottomNext = screen.getByRole('button', { name: '下一页（底部）' });
  expect(bottomPrev).toHaveProperty('disabled', true);
  expect(bottomNext).toHaveProperty('disabled', false);

  fireEvent.click(bottomNext);
  expect(screen.getByText('第 2 / 3 页')).toBeTruthy();
  expect(bottomPrev).toHaveProperty('disabled', false);
  expect(bottomNext).toHaveProperty('disabled', false);

  fireEvent.click(bottomNext);
  expect(screen.getByText('第 3 / 3 页')).toBeTruthy();
  expect(bottomPrev).toHaveProperty('disabled', false);
  expect(bottomNext).toHaveProperty('disabled', true);

  fireEvent.click(bottomPrev);
  expect(screen.getByText('第 2 / 3 页')).toBeTruthy();
});

