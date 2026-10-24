// @vitest-environment jsdom
import { cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Catalog from './Catalog';
import { catalogSessionCache, resetCatalogView } from './Catalog.cache';
import { clearStableResources } from '../services/stableResources';
import { ToastProvider } from '../components/ui';
import type { CatalogItem, CatalogSourceStatus, ExchangeRate } from '../api';
import { displayCatalogTable } from '../services/catalogPriceDisplay';
import { ConfirmProvider } from '../components/Confirm';

const render = (ui: Parameters<typeof renderUI>[0]) => renderUI(ui, { wrapper: ConfirmProvider });

const mockCatalogItems: CatalogItem[] = [
  {
    id: 'gpt-4o',
    name: 'GPT-4o',
    provider: 'OpenAI',
    source: 'builtin',
    context_length: 128000,
    max_input_tokens: null,
    max_output_tokens: 16384,
    modalities: { input: ['text', 'image'], output: ['text'] },
    thinking: { supported: false, modes: [] },
    offerings: [{ key: 'original', provider: 'Vendor', source_kind: 'original', quotes: [{ key: 'standard', source_kind: 'original', table: { currency: 'USD', unit_tokens: 1000000, rates: { input: 2.50, output: 10.00, cache_read: null, cache_write: null }, rules: [] } }] }],
    doc_url: 'https://platform.openai.com/docs/models/gpt-4o',
  },
  {
    id: 'deepseek-chat',
    name: 'DeepSeek V3',
    provider: 'DeepSeek',
    source: 'builtin',
    context_length: 65536,
    max_input_tokens: null,
    max_output_tokens: 8192,
    modalities: { input: ['text'], output: ['text'] },
    thinking: { supported: false, modes: [] },
    offerings: [{ key: 'original', provider: 'Vendor', source_kind: 'original', quotes: [{ key: 'standard', source_kind: 'original', table: { currency: 'CNY', unit_tokens: 1000000, rates: { input: 1.00, output: 2.00, cache_read: null, cache_write: null }, rules: [] } }] }],
    doc_url: 'https://api-docs.deepseek.com/quick_start/pricing',
  },
  {
    id: 'meta-llama/llama-3.3-70b-instruct',
    name: 'Llama 3.3 70B',
    provider: 'meta-llama',
    source: 'openrouter',
    context_length: 131072,
    max_input_tokens: null,
    max_output_tokens: 8192,
    modalities: { input: ['text'], output: ['text'] },
    thinking: { supported: false, modes: [] },
    offerings: [{ key: 'original', provider: 'Vendor', source_kind: 'original', quotes: [{ key: 'standard', source_kind: 'original', table: { currency: 'USD', unit_tokens: 1000000, rates: { input: 0.35, output: 0.40, cache_read: null, cache_write: null }, rules: [] } }] }],
  },
];

const mockSources: CatalogSourceStatus[] = [
  { source: 'builtin', name: '精选静态库 (官方)', enabled: true, prefill_enabled: true, priority: 1, item_count: 2 },
  { source: 'openrouter', name: 'OpenRouter', enabled: true, prefill_enabled: true, priority: 1, item_count: 1 },
  { source: 'litellm', name: 'LiteLLM', enabled: true, prefill_enabled: true, priority: 1, item_count: 0 },
];

const jsonResponse = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const exchange: ExchangeRate = { available: true, base: 'USD', quote: 'CNY', rate: 7, date: '2026-10-10', attempted_on: '2026-10-10', fetched_at: '2026-10-10T00:00:00Z', stale: false, error: '', source_url: 'https://frankfurter.dev/' };

describe('Catalog Page (T052)', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    clearStableResources(); resetCatalogView();
    catalogSessionCache.models = undefined; catalogSessionCache.sources = undefined;
    fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method || 'GET';

      if (url.includes('/exchange-rate')) return Promise.resolve(jsonResponse(exchange));
      if (url.includes('/catalog/models')) {
        return Promise.resolve(jsonResponse(mockCatalogItems));
      }
      if (url.includes('/catalog/sources')) {
        return Promise.resolve(jsonResponse(mockSources));
      }
      if (url.includes('/catalog/fetch') && method === 'POST') {
        return Promise.resolve(jsonResponse({ source: 'openrouter', count: 120 }));
      }
      return Promise.resolve(jsonResponse({}));
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('renders model catalog with header and model items', async () => {
    render(
      <ToastProvider>
        <Catalog />
      </ToastProvider>
    );

    await waitFor(() => {
      expect(screen.getByText('模型数据库')).toBeDefined();
      expect(screen.getByText('GPT-4o')).toBeDefined();
      expect(screen.getByText('DeepSeek V3')).toBeDefined();
      expect(screen.getByText('Llama 3.3 70B')).toBeDefined();
    });

    // Check badges
    expect(screen.getAllByText('官方精选').length).toBe(2);
    expect(screen.getAllByText('OpenRouter').length).toBeGreaterThan(0);
  });

  it('filters models by tab (builtin vs external)', async () => {
    render(
      <ToastProvider>
        <Catalog />
      </ToastProvider>
    );

    await waitFor(() => {
      expect(screen.getByText('GPT-4o')).toBeDefined();
    });

    // Switch to Builtin
    const builtinTab = screen.getByRole('button', { name: /^精选静态库 \(官方\)/ });
    fireEvent.click(builtinTab);

    expect(screen.getByText('GPT-4o')).toBeDefined();
    expect(screen.getByText('DeepSeek V3')).toBeDefined();
    expect(screen.queryByText('Llama 3.3 70B')).toBeNull();

    // Switch to External
    const externalTab = screen.getByRole('button', { name: /外部数据库/ });
    fireEvent.click(externalTab);

    expect(screen.queryByText('GPT-4o')).toBeNull();
    expect(screen.getByText('Llama 3.3 70B')).toBeDefined();
  });

  it('searches models by query string', async () => {
    render(
      <ToastProvider>
        <Catalog />
      </ToastProvider>
    );

    await waitFor(() => {
      expect(screen.getByText('GPT-4o')).toBeDefined();
    });

    const searchInput = screen.getByPlaceholderText(/搜索模型 ID、名称或厂商/);
    fireEvent.change(searchInput, { target: { value: 'deepseek' } });

    expect(screen.getByText('DeepSeek V3')).toBeDefined();
    expect(screen.queryByText('GPT-4o')).toBeNull();
    expect(screen.queryByText('Llama 3.3 70B')).toBeNull();
  });

  it('triggers source pulling via pull button', async () => {
    render(
      <ToastProvider>
        <Catalog />
      </ToastProvider>
    );

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /立即拉取/ })).toBeDefined();
    });

    const pullBtn = screen.getByRole('button', { name: /立即拉取/ });
    fireEvent.click(pullBtn);

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining('/catalog/fetch'),
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ source: 'openrouter' }),
        })
      );
      expect(screen.getByText(/拉取成功：已收录 120 款 openrouter 模型/)).toBeDefined();
    });
  });
  it('saves source prefill preferences while keeping disabled sources browsable', async () => {
    render(<ToastProvider><Catalog /></ToastProvider>);
    const checkbox = await screen.findByRole('checkbox', { name: 'OpenRouter 参与预填' });
    fireEvent.click(checkbox);
    fireEvent.change(screen.getByRole('spinbutton', { name: 'OpenRouter 推荐优先级' }), { target: { value: '9' } });
    fireEvent.click(screen.getByRole('button', { name: '保存来源偏好' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => String(url).includes('/catalog/sources') && init?.method === 'PUT')).toBe(true));
    const body = JSON.parse(String(fetchMock.mock.calls.find(([url, init]) => String(url).includes('/catalog/sources') && init?.method === 'PUT')![1]?.body));
    expect(body.find((source: CatalogSourceStatus) => source.source === 'openrouter')).toMatchObject({ prefill_enabled: false, priority: 9 });
    expect(screen.getByText('Llama 3.3 70B')).toBeDefined();
  });

  it('shows unknown and zero prices distinctly, preserves micro prices, and fetches offerings for an exact model', async () => {
    const base = fetchMock.getMockImplementation()!;
    const item: CatalogItem = { ...mockCatalogItems[2], thinking: { supported: null, modes: [] }, offerings: [{ key: 'hosted', provider: 'Host', source_kind: 'hosted', quotes: [{ key: 'micro', source_kind: 'hosted', table: { currency: 'USD', unit_tokens: 1, rates: { input: 0.00000001, output: 0, cache_read: null, cache_write: null }, rules: [], uncovered_conditions: ['audio'] } }] }] };
    fetchMock.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => String(url).includes('/catalog/models') ? Promise.resolve(jsonResponse([item])) : base(url, init));
    render(<ToastProvider><Catalog /></ToastProvider>);
    await screen.findByText('Llama 3.3 70B');
    const prices = within(screen.getByRole('region', { name: 'Llama 3.3 70B 价格' }));
    expect(prices.getByText('输入').parentElement?.textContent).toBe('输入 0.07');
    expect(prices.getByText('输出').parentElement?.textContent).toBe('输出 0');
    expect(prices.getByText('缓存读').parentElement?.textContent).toBe('缓存读 未知');
    expect(screen.getByText('思考档位：未知')).toBeDefined();
    expect(screen.getByText('audio').closest('details')).toHaveProperty('open', false);
    fireEvent.click(screen.getByText('价格备注 · 1 项'));
    expect(screen.getByText('audio').closest('details')).toHaveProperty('open', true);
    fireEvent.click(screen.getByRole('button', { name: '拉取该模型供应商报价' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/catalog/offerings/fetch', expect.objectContaining({ method: 'POST', body: JSON.stringify({ source: 'openrouter', id: item.id }) })));
  });

  it('uses one page currency and per-million units without changing source tables or writing configuration', async () => {
    render(<ToastProvider><Catalog /></ToastProvider>);
    const usdModel = within(await screen.findByRole('region', { name: 'GPT-4o 价格' }));
    const cnyModel = within(screen.getByRole('region', { name: 'DeepSeek V3 价格' }));
    expect(usdModel.getByText('输入').parentElement?.textContent).toBe('输入 17.5');
    expect(cnyModel.getByText('输入').parentElement?.textContent).toBe('输入 1');
    fireEvent.change(screen.getByRole('combobox', { name: '页面币种' }), { target: { value: 'USD' } });
    expect(usdModel.getByText('输入').parentElement?.textContent).toBe('输入 2.5');
    expect(cnyModel.getByText('输入').parentElement?.textContent).toBe('输入 0.14');
    fireEvent.change(screen.getByRole('combobox', { name: '页面币种' }), { target: { value: 'CNY' } });
    expect(usdModel.getByText('输入').parentElement?.textContent).toBe('输入 17.5');
    expect(fetchMock.mock.calls.every(([, init]) => !init?.method || init.method === 'GET')).toBe(true);

    const table = { currency: 'USD' as const, unit_tokens: 1000, rates: { input: 0.002, output: 0, cache_read: null, cache_write: 0.003 }, rules: [{ enabled: true, when: { input_range: { from_tokens: 10001, to_tokens: null } }, action: { rate_changes: { input: 0.004, cache_read: null }, multiplier: 0.5 } }] };
    const original = structuredClone(table);
    const display = displayCatalogTable(table, 'CNY', exchange);
    expect(display.table.rates).toEqual({ input: 14, output: 0, cache_read: null, cache_write: 21 });
    expect(display.table.rules[0].action).toEqual({ rate_changes: { input: 28, cache_read: null }, multiplier: 0.5 });
    expect(table).toEqual(original);
  });

  it('keeps length and promotion conditions separate from standard base prices and folds technical notes', async () => {
    const base = fetchMock.getMockImplementation()!;
    const item: CatalogItem = { ...mockCatalogItems[0], notes: ['Model implementation note'], thinking: { supported: true, modes: [{ id: 'effort', api_format: 'openai_responses', control: 'effort' }] }, offerings: [{ ...mockCatalogItems[0].offerings[0], region: 'global', service_tier: 'standard', quotes: [{ ...mockCatalogItems[0].offerings[0].quotes[0], notes: ['Source caveat'], table: { ...mockCatalogItems[0].offerings[0].quotes[0].table, rules: [{ enabled: true, when: { input_range: { from_tokens: 200001, to_tokens: null } }, action: { rate_changes: { input: 5, output: 20 } } }, { enabled: true, when: { time_window: { start: '22:00', end: '06:00', all_day: false, timezone: 'Asia/Shanghai' }, date_set: ['2026-10-11'], timezone: 'Asia/Shanghai' }, action: { multiplier: 0.5 } }] } }] }] };
    fetchMock.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => String(url).includes('/catalog/models') ? Promise.resolve(jsonResponse([item])) : base(url, init));
    render(<ToastProvider><Catalog /></ToastProvider>);
    const prices = within(await screen.findByRole('region', { name: 'GPT-4o 价格' }));
    expect(prices.getByText('输入').parentElement?.textContent).toBe('输入 17.5');
    const conditions = prices.getByText('条件报价 · 2 条').closest('details')!;
    expect(conditions.open).toBe(false);
    fireEvent.click(prices.getByText('条件报价 · 2 条'));
    expect(conditions.open).toBe(true);
    expect(prices.getByText(/输入长度 200,001–不限 Tokens/)).toBeDefined();
    expect(prices.getByText(/22:00–06:00（跨夜） Asia\/Shanghai/)).toBeDefined();
    expect(prices.getByText('当前价格 × 0.5')).toBeDefined();
    const overrideRates = prices.getByLabelText('条件覆盖费率');
    expect(within(overrideRates).getByText('覆盖输入').parentElement?.textContent).toContain('35');
    expect(within(overrideRates).getByText('覆盖输出').parentElement?.textContent).toContain('140');
    expect(within(overrideRates).getByText('CNY / 1M Tokens')).toBeDefined();
    expect(prices.getByText('输入').parentElement?.textContent).toBe('输入 17.5');
    expect(screen.queryByText(/"input_range"/)).toBeNull();
    expect(screen.getByText('Source caveat').closest('details')?.open).toBe(false);
    expect(screen.getByText('Model implementation note').closest('details')?.open).toBe(false);
    expect(screen.getByText(/effort · openai_responses · effort/).closest('details')?.open).toBe(false);
  });

  it('preserves actual currency and unknown amounts when exchange rates are unavailable, and can retry', async () => {
    const base = fetchMock.getMockImplementation()!;
    let available = false;
    fetchMock.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => String(url).includes('/exchange-rate') ? Promise.resolve(jsonResponse({ ...exchange, available, rate: available ? 7 : 0 })) : base(url, init));
    render(<ToastProvider><Catalog /></ToastProvider>);
    const usdModel = within(await screen.findByRole('region', { name: 'GPT-4o 价格' }));
    expect(usdModel.getByText('USD / 1M Tokens · 汇率不可用，保留原币')).toBeDefined();
    expect(usdModel.getByText('输入').parentElement?.textContent).toBe('输入 2.5');
    expect(usdModel.getByText('缓存读').parentElement?.textContent).toBe('缓存读 未知');
    available = true;
    fireEvent.click(screen.getByRole('button', { name: '读取汇率' }));
    await waitFor(() => expect(usdModel.getByText('输入').parentElement?.textContent).toBe('输入 17.5'));
  });

  it('shows builtin offerings as separate entries and chooses external candidates as complete tables', async () => {
    const base = fetchMock.getMockImplementation()!;
    const offering = (key: string, source: string, region: string, input: number | null, output: number, read: number | null, write: number) => ({ key, provider: key, region, service_tier: 'standard', source_kind: source, quotes: [{ key: 'token', source_kind: source, table: { currency: 'USD' as const, unit_tokens: 1000000, rates: { input, output, cache_read: read, cache_write: write }, rules: [] } }] });
    const builtin = { ...mockCatalogItems[0], offerings: [offering('CheapHost', 'hosted', 'global', 0, 0, 0, 0), offering('RegionalOriginal', 'original', 'china', 1, 1, 1, 1), offering('GlobalOriginal', 'original', 'global', 4, 8, null, 5)] };
    const external = { ...mockCatalogItems[2], offerings: [offering('HostA', 'hosted', 'eu', 1, 6, 0, 3), offering('HostB', 'hosted', 'us', 5, 1, null, 8)] };
    fetchMock.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => String(url).includes('/catalog/models') ? Promise.resolve(jsonResponse([builtin, external])) : base(url, init));
    render(<ToastProvider><Catalog /></ToastProvider>);
    await screen.findByText('GlobalOriginal', { selector: 'span' });
    expect(screen.getAllByRole('heading', { name: 'GPT-4o' })).toHaveLength(3);
    const official = within(screen.getByText('GlobalOriginal', { selector: 'span' }).closest('.group')!);
    const hosted = within(screen.getByRole('region', { name: 'Llama 3.3 70B 价格' }));
    const officialRates = within(official.getAllByLabelText('基础四项价格')[0]);
    expect(officialRates.getByText('输入').parentElement?.textContent).toBe('输入 28');
    expect(officialRates.getByText('缓存读').parentElement?.textContent).toBe('缓存读 未知');
    expect(hosted.getByText('候选最低基价（输入＋输出）')).toBeDefined();
    expect(hosted.getByText('HostB · us · standard · 托管')).toBeDefined();
    const hostedRates = within(hosted.getAllByLabelText('基础四项价格')[0]);
    expect(hostedRates.getByText('输入').parentElement?.textContent).toBe('输入 35');
    expect(hostedRates.getByText('输出').parentElement?.textContent).toBe('输出 7');
    expect(hostedRates.getByText('缓存读').parentElement?.textContent).toBe('缓存读 未知');
    expect(hostedRates.getByText('缓存写').parentElement?.textContent).toBe('缓存写 56');
    expect(hosted.getByText('其他报价与服务范围 · 1 项').closest('details')?.open).toBe(false);
  });

  it('filters and counts regional cards independently while copying the canonical model ID', async () => {
    const base = fetchMock.getMockImplementation()!;
    const regional: CatalogItem = {
      ...mockCatalogItems[0], id: 'mimo-v2.6-pro', name: 'mimo-v2.6-pro', provider: 'Xiaomi',
      offerings: [
        { ...mockCatalogItems[0].offerings[0], key: 'china', provider: 'Xiaomi-中国', region: 'china', quotes: [{ ...mockCatalogItems[0].offerings[0].quotes[0], table: { ...mockCatalogItems[0].offerings[0].quotes[0].table, currency: 'CNY', rates: { input: 7, output: 21, cache_read: null, cache_write: 0 } } }] },
        { ...mockCatalogItems[0].offerings[0], key: 'overseas', provider: 'Xiaomi-海外', region: 'global' },
      ],
    };
    const external = { ...regional, source: 'openrouter', provider: 'Xiaomi', name: '外部 MiMo' };
    fetchMock.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => String(url).includes('/catalog/models') ? Promise.resolve(jsonResponse([regional, external])) : base(url, init));
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal('navigator', { clipboard: { writeText } });
    render(<ToastProvider><Catalog /></ToastProvider>);
    await screen.findByText('Xiaomi-中国', { selector: 'span' });
    expect(screen.getAllByRole('heading', { name: 'mimo-v2.6-pro' })).toHaveLength(2);
    expect(screen.getAllByRole('heading', { name: '外部 MiMo' })).toHaveLength(1);
    expect(screen.getByRole('button', { name: '全部 (3)' })).toBeDefined();
    expect(screen.getByRole('button', { name: '精选静态库 (官方) (2)' })).toBeDefined();
    expect(screen.getByRole('button', { name: '外部数据库 (1)' })).toBeDefined();
    const china = within(screen.getByText('Xiaomi-中国', { selector: 'span' }).closest('.group')!);
    expect(china.getByText('输入').parentElement?.textContent).toBe('输入 7');
    fireEvent.click(china.getByRole('button', { name: '复制模型 ID' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('mimo-v2.6-pro'));
    fireEvent.change(screen.getByRole('combobox', { name: '筛选厂商' }), { target: { value: 'Xiaomi-海外' } });
    expect(screen.queryByText('Xiaomi-中国', { selector: 'span' })).toBeNull();
    expect(screen.getByRole('heading', { name: 'mimo-v2.6-pro' })).toBeDefined();
    expect(screen.queryByRole('heading', { name: '外部 MiMo' })).toBeNull();
    fireEvent.change(screen.getByRole('combobox', { name: '筛选厂商' }), { target: { value: 'all' } });
    fireEvent.change(screen.getByPlaceholderText(/搜索模型 ID、名称或厂商/), { target: { value: '中国' } });
    expect(screen.getByText('Xiaomi-中国', { selector: 'span' })).toBeDefined();
    expect(screen.queryByText('Xiaomi-海外', { selector: 'span' })).toBeNull();
  });

  it('displays unified thinking tier accurately for effort levels, budget, and unsupported models', async () => {
    const base = fetchMock.getMockImplementation()!;
    const itemEfforts: CatalogItem = {
      ...mockCatalogItems[0],
      id: 'model-efforts',
      name: 'Reasoning Model',
      thinking: {
        supported: true,
        modes: [{ id: 'effort', api_format: 'openai_responses', control: 'effort', efforts: ['low', 'medium', 'high'] }],
      },
    };
    const itemBudget: CatalogItem = {
      ...mockCatalogItems[1],
      id: 'model-budget',
      name: 'Budget Model',
      thinking: {
        supported: true,
        modes: [{ id: 'budget', api_format: 'anthropic_messages', control: 'budget', budget_max: 64000 }],
      },
    };
    fetchMock.mockImplementation((url: RequestInfo | URL, init?: RequestInit) =>
      String(url).includes('/catalog/models') ? Promise.resolve(jsonResponse([itemEfforts, itemBudget])) : base(url, init)
    );
    render(<ToastProvider><Catalog /></ToastProvider>);
    await screen.findByText('Reasoning Model');
    expect(screen.getByText('思考档位：low · medium · high')).toBeDefined();
    expect(screen.getByText('思考档位：由思考预算控制')).toBeDefined();
  });

});
