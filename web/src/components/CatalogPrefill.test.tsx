// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { PrefillControls } from './CatalogPrefill';
import type { CatalogItem, CatalogModelRef, PrefillRequest, PrefillTarget } from '../api';

const model: CatalogItem = {
  source: 'builtin', id: 'mimo-v2.6-pro', name: 'mimo-v2.6-pro', provider: 'Xiaomi',
  context_length: 1000000, max_input_tokens: null, max_output_tokens: 32000,
  modalities: { input: ['text'], output: ['text'] }, thinking: { supported: false, modes: [] },
  offerings: [
    { key: 'china', provider: 'Xiaomi-中国', region: 'china', source_kind: 'original', quotes: [
      { key: '1h', name: '1 小时', source_kind: 'original', table: { currency: 'CNY', unit_tokens: 1000000, rates: { input: 7, output: 21, cache_read: 0.7, cache_write: 0 }, rules: [] } },
      { key: '24h', name: '24 小时', source_kind: 'original', table: { currency: 'CNY', unit_tokens: 1000000, rates: { input: 7, output: 21, cache_read: 0.7, cache_write: 3 }, rules: [] } },
    ] },
    { key: 'overseas', provider: 'Xiaomi-海外', region: 'global', source_kind: 'original', quotes: [
      { key: 'standard', source_kind: 'original', table: { currency: 'USD', unit_tokens: 1000000, rates: { input: 1, output: 3, cache_read: 0.1, cache_write: 0 }, rules: [] } },
    ] },
  ],
};
const target: PrefillTarget = { kind: 'downstream', model: model.id };
const ref = (offering_key: string): CatalogModelRef => ({ source: model.source, id: model.id, offering_key });
const json = (body: unknown) => new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });

describe('Catalog prefill regional selection', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let requests: PrefillRequest[];
  beforeEach(() => {
    requests = [];
    fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url.includes('/catalog/models')) return Promise.resolve(json([model]));
      if (url.includes('/catalog/matches')) return Promise.resolve(json([{ item: model, match: 'exact', automatic: true }]));
      if (url.includes('/catalog/prefill/preview')) {
        const request = JSON.parse(String(init?.body)) as PrefillRequest;
        requests.push(request);
        return Promise.resolve(json({ request, updated: 1, skipped: 0, patches: [{ target: request.targets[0], config: { context_window: 1000000 }, catalog_model_ref: request.targets[0].ref, warnings: [] }] }));
      }
      return Promise.resolve(json({}));
    });
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

  async function selectRegion(region: '中国' | '海外') {
    fireEvent.focus(screen.getByLabelText('搜索参数库模型'));
    fireEvent.click(await screen.findByRole('button', { name: `builtin · Xiaomi-${region} · mimo-v2.6-pro` }));
  }
  async function preview() {
    fireEvent.click(screen.getByRole('button', { name: '预览预填' }));
    await screen.findByRole('status');
    return requests[requests.length - 1];
  }

  it('matches library prices without a mode selector and restricts exact regional TTL choices', async () => {
    render(<PrefillControls targets={[target]} />);
    await selectRegion('中国');
    if (!(screen.getByRole('checkbox', { name: '预填价格' }) as HTMLInputElement).checked) fireEvent.click(screen.getByRole('checkbox', { name: '预填价格' }));
    expect(screen.queryByRole('combobox', { name: '预填价格模式' })).toBeNull();
    const prices = screen.getByRole('combobox', { name: '精确报价' });
    expect(within(prices).getAllByRole('option')).toHaveLength(3);
    expect(within(prices).queryByRole('option', { name: /Xiaomi-海外/ })).toBeNull();
    fireEvent.change(prices, { target: { value: 'china\u000024h' } });
    const request = await preview();
    expect(request.price_mode).toBe('catalog');
    expect(request.targets[0]).not.toHaveProperty('price_role');
    expect(request.targets[0].ref).toEqual(ref('china'));
    expect(request.targets[0].price_ref).toEqual({ ...ref('china'), quote_key: '24h' });
    await selectRegion('海外');
    expect(screen.queryByRole('status')).toBeNull();
    expect((screen.getByRole('combobox', { name: '精确报价' }) as HTMLSelectElement).value).toBe('');
    expect(screen.queryByRole('option', { name: /1 小时|24 小时/ })).toBeNull();
    const overseas = await preview();
    expect(overseas.targets[0].ref).toEqual(ref('overseas'));
    expect(overseas.targets[0].price_ref).toBeUndefined();
  });

  it('saves association-only region and restores an existing regional reference', async () => {
    const onAssociation = vi.fn();
    const { rerender } = render(<PrefillControls targets={[target]} onAssociation={onAssociation} />);
    await selectRegion('海外');
    fireEvent.click(screen.getByRole('button', { name: '仅关联资料' }));
    expect(onAssociation).toHaveBeenLastCalledWith(ref('overseas'));
    rerender(<PrefillControls targets={[{ ...target, ref: ref('china') }]} onAssociation={onAssociation} />);
    await waitFor(() => expect(screen.getByLabelText('搜索参数库模型')).toHaveProperty('placeholder', 'builtin · Xiaomi-中国 · mimo-v2.6-pro'));
    const request = await preview();
    expect(request.targets[0].ref).toEqual(ref('china'));
  });

  it('does not silently select a price region when automatic canonical matching is ambiguous', async () => {
    render(<PrefillControls targets={[target]} />);
    await screen.findByText(/唯一匹配/);
    if (!(screen.getByRole('checkbox', { name: '预填价格' }) as HTMLInputElement).checked) fireEvent.click(screen.getByRole('checkbox', { name: '预填价格' }));
    expect(screen.getByText('请选择地区资料后预填价格；能力规格仍可按模型匹配。')).toBeDefined();
    expect(screen.queryByRole('combobox', { name: '精确报价' })).toBeNull();
    const request = await preview();
    expect(request.targets[0].ref).toEqual({ source: model.source, id: model.id });
    expect(request.targets[0].price_ref).toBeUndefined();
    expect(request.groups).toEqual(['price']);
    expect(screen.queryByRole('checkbox', { name: '预填能力规格' })).toBeNull();
    expect(screen.queryByRole('checkbox', { name: '预填请求默认' })).toBeNull();
  });

  it('removes an old exact price from another region when a new region is selected', async () => {
    render(<PrefillControls targets={[{ ...target, ref: ref('china'), price_ref: { ...ref('china'), offering_key: 'china', quote_key: '24h' } }]} />);
    await selectRegion('海外');
    const request = await preview();
    expect(request.targets[0].ref).toEqual(ref('overseas'));
    expect(request.targets[0].price_ref).toBeUndefined();
  });

  it('discards an in-flight preview after changing region', async () => {
    const base = fetchMock.getMockImplementation()!;
    let finish!: (response: Response) => void;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => url.includes('/catalog/prefill/preview')
      ? new Promise<Response>(resolve => { finish = resolve; }) : base(url, init));
    render(<PrefillControls targets={[target]} />);
    await selectRegion('中国');
    fireEvent.click(screen.getByRole('button', { name: '预览预填' }));
    await screen.findByRole('button', { name: '处理中…' });
    await selectRegion('海外');
    await act(async () => { finish(json({ updated: 1, skipped: 0, patches: [] })); });
    await screen.findByRole('button', { name: '预览预填' });
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('preserves a saved offering identity for server validation if it is missing from current catalog entries', async () => {
    const single = { ...model, offerings: [model.offerings[0]] };
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => url.includes('/catalog/models') ? Promise.resolve(json([single])) : base(url, init));
    render(<PrefillControls targets={[{ ...target, ref: ref('retired-region') }]} />);
    fireEvent.focus(screen.getByLabelText('搜索参数库模型'));
    await screen.findByRole('button', { name: 'builtin · Xiaomi · mimo-v2.6-pro' });
    expect((await preview()).targets[0].ref).toEqual(ref('retired-region'));
  });

  it('preserves every existing regional reference during batch prefill', async () => {
    const batch: PrefillTarget[] = [{ ...target, ref: ref('china') }, { ...target, model: 'overseas-alias', ref: ref('overseas') }];
    render(<PrefillControls targets={batch} />);
    expect(screen.queryByLabelText('搜索参数库模型')).toBeNull();
    const request = await preview();
    expect(request.targets.map(row => row.ref)).toEqual(batch.map(row => row.ref));
  });

  it('keeps single-offering models and external models on the original canonical entry path', async () => {
    const single = { ...model, id: 'single', name: 'single', offerings: [model.offerings[0]] };
    const external = { ...model, source: 'openrouter' };
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => url.includes('/catalog/models') ? Promise.resolve(json([single, external])) : base(url, init));
    render(<PrefillControls targets={[{ ...target, model: 'single' }]} />);
    fireEvent.focus(screen.getByLabelText('搜索参数库模型'));
    const singleOption = await screen.findByRole('button', { name: 'builtin · Xiaomi · single' });
    expect(screen.getByRole('button', { name: 'openrouter · Xiaomi · mimo-v2.6-pro' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /builtin · Xiaomi-海外/ })).toBeNull();
    fireEvent.click(singleOption);
    const request = await preview();
    expect(request.targets[0].ref).toEqual({ source: 'builtin', id: 'single' });
    fireEvent.focus(screen.getByLabelText('搜索参数库模型'));
    fireEvent.click(screen.getByRole('button', { name: 'openrouter · Xiaomi · mimo-v2.6-pro' }));
    if (!(screen.getByRole('checkbox', { name: '预填价格' }) as HTMLInputElement).checked) fireEvent.click(screen.getByRole('checkbox', { name: '预填价格' }));
    expect(within(screen.getByRole('combobox', { name: '精确报价' })).getAllByRole('option')).toHaveLength(4);
  });
});
