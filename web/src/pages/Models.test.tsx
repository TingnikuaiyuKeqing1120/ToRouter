// @vitest-environment jsdom
import { cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { ConfirmProvider } from '../components/Confirm';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Models from './Models';
import type { Channel, DownstreamModelView } from '../api';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });

const mockChannels: Channel[] = [
  { id: 1, name: 'OpenAI 官方', api_key: 'sk-xxx', has_key: true, models: ['gpt-4o', 'o3-mini'], enabled: true, endpoints: [] },
  { id: 2, name: 'Anthropic 官方', api_key: 'sk-yyy', has_key: true, models: ['claude-3-7-sonnet'], enabled: true, endpoints: [] },
];

const mockModels: DownstreamModelView[] = [
  {
    id: 'gpt-4o',
    display_name: 'GPT-4o Omnipresent',
    enabled: true,
    fallback: true,
    bindings: [
      { channel_id: 1, channel_name: 'OpenAI 官方', channel_exists: true, model: 'gpt-4o', priority: 0, enabled: true, effective: { config: { context_window: 128000 }, sources: { context_window: '下游模型' } } },
    ],
  },
  {
    id: 'claude-3-7-sonnet',
    display_name: 'Claude 3.7 Sonnet',
    enabled: false,
    fallback: false,
    bindings: [
      { channel_id: 2, channel_name: 'Anthropic 官方', channel_exists: true, model: 'claude-3-7-sonnet', priority: 0, enabled: true, effective: { config: { context_window: 200000 }, sources: { context_window: '下游模型' } } },
      { channel_id: 1, channel_name: 'OpenAI 官方', channel_exists: true, model: 'gpt-4o', priority: 1, enabled: true },
      { channel_id: 1, channel_name: 'OpenAI 官方', channel_exists: true, model: 'o3-mini', priority: 2, enabled: false },
    ],
  },
  {
    id: 'o3-mini',
    display_name: 'O3 Mini Reasoning',
    enabled: true,
    fallback: true,
    bindings: [
      { channel_id: 1, channel_name: 'OpenAI 官方', channel_exists: true, model: 'o3-mini', priority: 0, enabled: true, effective: { config: { context_window: 64000 }, sources: { context_window: '下游模型' } } },
      { channel_id: 1, channel_name: 'OpenAI 官方', channel_exists: true, model: 'gpt-4o', priority: 1, enabled: true },
    ],
  },
];

const jsonResponse = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

describe('Models Page - Dense View & Sorting (T071)', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) {
      this.setAttribute('open', '');
    });
    HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) {
      this.removeAttribute('open');
    });
    fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/models' && (!init || !init.method || init.method === 'GET')) {
        return Promise.resolve(jsonResponse(mockModels));
      }
      if (url === '/api/channels') {
        return Promise.resolve(jsonResponse(mockChannels));
      }
      if (url.startsWith('/api/models/') && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({ id: 'mock' }));
      }
      if (url === '/api/models/batch/status' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({ count: 2 }));
      }
      if (url === '/api/models/import' && init?.method === 'POST') {
        return Promise.resolve(jsonResponse({ count: 1 }));
      }
      return Promise.resolve(jsonResponse({}, 404));
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders high-density cards with accessible touch targets >= 32px', async () => {
    render(<Models />);
    await screen.findByRole('heading', { name: '公开模型' });

    // Verify all 3 models rendered
    expect(await screen.findByText('GPT-4o Omnipresent')).toBeDefined();
    expect(screen.getByText('Claude 3.7 Sonnet')).toBeDefined();
    expect(screen.getByText('O3 Mini Reasoning')).toBeDefined();

    // Verify accessible switch targets
    const switches = screen.getAllByRole('switch');
    expect(switches.length).toBe(3);
    for (const sw of switches) {
      // Must contain relative hit area expander before:absolute before:-inset-y-1.5
      expect(sw.className).toContain('before:-inset-y-1.5');
    }

    // Verify action buttons have >= 32px height constraint classes
    const editButtons = screen.getAllByRole('button', { name: /编辑模型/ });
    expect(editButtons.length).toBe(3);
    for (const btn of editButtons) {
      expect(btn.className).toContain('!min-h-[32px]');
      expect(btn.className).toContain('!h-8');
      expect(btn.className).toContain('!w-8');
    }
  });

  it('uses the first surviving saved binding icon including uploaded images', async () => {
    const original = fetchMock.getMockImplementation()!;
    const custom = 'data:image/png;base64,c3ludGhldGlj';
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/channels/display') return Promise.resolve(jsonResponse({ icons: { '1': custom } }));
      if (url === '/api/plugins') return Promise.resolve(jsonResponse({ items: [], version: 'fixture' }));
      if (url === '/api/models') return Promise.resolve(jsonResponse([{ ...mockModels[0], bindings: [{ ...mockModels[0].bindings[0], channel_id: 999 }, { ...mockModels[0].bindings[0], priority: 99, enabled: false }] }]));
      return original(url, init);
    });
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');
    await waitFor(() => expect(document.querySelector(`img[src="${custom}"]`)).toBeTruthy());
  });

  it('resolves icon from channel primary endpoint when channel base_url is empty', async () => {
    const original = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/channels') {
        return Promise.resolve(jsonResponse([
          { id: 10, name: '中性名称渠道', api_key: 'sk-xxx', has_key: true, models: ['deepseek-chat'], enabled: true, base_url: '', endpoints: [{ format: 'openai/chat_completions', base_url: 'https://api.deepseek.com' }] },
        ]));
      }
      if (url === '/api/models') {
        return Promise.resolve(jsonResponse([
          {
            id: 'deepseek-v3',
            display_name: '通用推理模型',
            enabled: true,
            fallback: false,
            bindings: [
              { channel_id: 10, channel_name: '中性名称渠道', channel_exists: true, model: 'deepseek-chat', priority: 0, enabled: true },
            ],
          },
        ]));
      }
      return original(url, init);
    });
    const { container } = render(<Models />);
    await screen.findByText('通用推理模型');
    await waitFor(() => {
      const path = container.querySelector('.public-model-identity svg path[fill="#4D6BFE"]');
      expect(path).toBeTruthy();
    });
  });

  it('expands cascade configuration details when row or chevron is clicked', async () => {
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');

    const articles = screen.getAllByRole('article');
    const gptArticle = articles[0];
    const details = gptArticle.querySelector('details')!;

    // Initially cascade configuration details are not expanded
    expect(details.hasAttribute('open')).toBe(false);

    // Click the chevron button to expand
    const chevronBtn = screen.getByRole('button', { name: '展开模型 gpt-4o 详情' });
    fireEvent.click(chevronBtn);

    // Now cascade details panel should be open
    expect(details.hasAttribute('open')).toBe(true);

    // Click chevron again to collapse
    const collapseBtn = screen.getByRole('button', { name: '折叠模型 gpt-4o 详情' });
    fireEvent.click(collapseBtn);
    expect(details.hasAttribute('open')).toBe(false);

    // Click row itself to expand
    const rowTitle = screen.getByText('GPT-4o Omnipresent');
    fireEvent.click(rowTitle);
    expect(details.hasAttribute('open')).toBe(true);
  });

  it('clicking switch or action buttons stops propagation and does not toggle expansion', async () => {
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');

    const articles = screen.getAllByRole('article');
    const gptArticle = articles[0];
    const details = gptArticle.querySelector('details')!;
    expect(details.hasAttribute('open')).toBe(false);

    const sw = screen.getByRole('switch', { name: '停用 gpt-4o' });
    fireEvent.click(sw);

    // PUT request made
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/models/gpt-4o',
        expect.objectContaining({ method: 'PUT' })
      );
    });

    // Cascade details should NOT be opened by clicking switch
    expect(details.hasAttribute('open')).toBe(false);
  });

  it('supports instant multi-dimensional sorting without changing backend payload order', async () => {
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');

    const sortSelect = screen.getByRole('combobox', { name: '排序模型' });

    const getRenderedModelIds = () => {
      const articles = screen.getAllByRole('article');
      return articles.map(art => {
        const title = art.querySelector('h2')?.getAttribute('title') || art.querySelector('h2')?.textContent || '';
        return title;
      });
    };

    // Default order matches mockModels order: gpt-4o, claude-3-7-sonnet, o3-mini
    expect(getRenderedModelIds()).toEqual([
      'GPT-4o Omnipresent',
      'Claude 3.7 Sonnet',
      'O3 Mini Reasoning',
    ]);

    // Sort: id_asc (A-Z by ID: claude-3-7-sonnet, gpt-4o, o3-mini)
    fireEvent.change(sortSelect, { target: { value: 'id_asc' } });
    expect(getRenderedModelIds()).toEqual([
      'Claude 3.7 Sonnet',
      'GPT-4o Omnipresent',
      'O3 Mini Reasoning',
    ]);

    // Sort: id_desc (Z-A by ID: o3-mini, gpt-4o, claude-3-7-sonnet)
    fireEvent.change(sortSelect, { target: { value: 'id_desc' } });
    expect(getRenderedModelIds()).toEqual([
      'O3 Mini Reasoning',
      'GPT-4o Omnipresent',
      'Claude 3.7 Sonnet',
    ]);

    // Sort: bindings_desc (bindings count: claude has 3, o3-mini has 2, gpt-4o has 1)
    fireEvent.change(sortSelect, { target: { value: 'bindings_desc' } });
    expect(getRenderedModelIds()).toEqual([
      'Claude 3.7 Sonnet',
      'O3 Mini Reasoning',
      'GPT-4o Omnipresent',
    ]);

    // Sort: enabled_first (gpt-4o and o3-mini are enabled, claude is disabled)
    fireEvent.change(sortSelect, { target: { value: 'enabled_first' } });
    const enabledOrder = getRenderedModelIds();
    expect(enabledOrder[2]).toBe('Claude 3.7 Sonnet'); // disabled is last
    expect(enabledOrder.slice(0, 2)).toEqual(['GPT-4o Omnipresent', 'O3 Mini Reasoning']);

    // Revert to default order
    fireEvent.change(sortSelect, { target: { value: 'default' } });
    expect(getRenderedModelIds()).toEqual([
      'GPT-4o Omnipresent',
      'Claude 3.7 Sonnet',
      'O3 Mini Reasoning',
    ]);
  });

  it('identifies models by name and ID and supports batch selection and actions', async () => {
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');

    // Display positions are not additional model identifiers.
    expect(screen.queryByText('#01')).toBeNull();
    expect(screen.getByRole('heading', { name: 'GPT-4o Omnipresent' }).closest('article')?.querySelector('.public-model-id')?.textContent).toBe('gpt-4o');
    expect(screen.getByRole('heading', { name: 'Claude 3.7 Sonnet' }).closest('article')?.querySelector('.public-model-id')?.textContent).toBe('claude-3-7-sonnet');

    // 2. Multi-select checkboxes
    const selectGpt = screen.getByRole('checkbox', { name: '选择模型 gpt-4o' });
    const selectClaude = screen.getByRole('checkbox', { name: '选择模型 claude-3-7-sonnet' });
    const selectAll = screen.getByRole('checkbox', { name: '全选所有模型' });

    // Floating batch bar should not be present initially
    expect(screen.queryByText(/已选择.*项/)).toBeNull();

    // Select gpt-4o
    fireEvent.click(selectGpt);
    expect(screen.getByText((_, el) => el?.textContent === '已选择 1 项')).toBeDefined();

    // Select claude-3-7-sonnet
    fireEvent.click(selectClaude);
    expect(screen.getByText((_, el) => el?.textContent === '已选择 2 项')).toBeDefined();

    // Click batch enable
    const batchEnableBtn = screen.getByRole('button', { name: '批量启用' });
    fireEvent.click(batchEnableBtn);

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/models/batch/status',
        expect.objectContaining({
          method: 'PUT',
          body: JSON.stringify({ ids: ['gpt-4o', 'claude-3-7-sonnet'], enabled: true }),
        })
      );
    });

    // Select all test
    fireEvent.click(selectAll);
    expect(screen.getByText((_, el) => el?.textContent === '已选择 3 项')).toBeDefined();

    // Cancel selection
    const cancelBtn = screen.getByRole('button', { name: '取消选择' });
    fireEvent.click(cancelBtn);
    expect(screen.queryByText((_, el) => !!el?.textContent?.includes('已选择'))).toBeNull();
  });

  it('supports 5-second undoable deletion without blocking confirm dialog', async () => {
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');

    // Model is currently present
    expect(screen.getByText('GPT-4o Omnipresent')).toBeDefined();

    // Click delete button on gpt-4o
    const deleteBtn = screen.getByRole('button', { name: '删除模型 gpt-4o' });
    fireEvent.click(deleteBtn);

    // Optimistically removed from list immediately
    expect(screen.queryByText('GPT-4o Omnipresent')).toBeNull();

    // Undo snackbar appears with remaining seconds
    expect(screen.getByText(/已移除「GPT-4o Omnipresent」/)).toBeDefined();
    const undoBtn = screen.getByRole('button', { name: '撤回' });
    expect(undoBtn).toBeDefined();

    // Click undo
    fireEvent.click(undoBtn);

    // Model is immediately restored
    expect(screen.getByText('GPT-4o Omnipresent')).toBeDefined();
    expect(screen.queryByText(/已移除「GPT-4o Omnipresent」/)).toBeNull();

    // No DELETE request was executed to backend
    expect(fetchMock).not.toHaveBeenCalledWith(
      '/api/models/gpt-4o',
      expect.objectContaining({ method: 'DELETE' })
    );
  });

  it('disables reordering handle when non-default sort is active', async () => {
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');

    // Default sort: handle is active
    const handles = screen.getAllByLabelText(/拖拽调整模型.*顺序/);
    expect(handles.length).toBe(3);
    expect(handles[0].getAttribute('title')).toBe('拖拽或使用上下方向键调整顺序');

    // Switch to id_asc
    const sortSelect = screen.getByRole('combobox', { name: '排序模型' });
    fireEvent.change(sortSelect, { target: { value: 'id_asc' } });

    // Handle is now disabled with tooltip
    expect(handles[0].getAttribute('title')).toBe('请切回默认排序以自定义调整顺序');
  });

  it('opens configuration import dialog and submits import payload', async () => {
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');

    // Click "恢复配置" button
    const importBtn = screen.getByRole('button', { name: /恢复配置/ });
    fireEvent.click(importBtn);

    // Modal opens
    expect(screen.getByRole('dialog')).toBeDefined();
    expect(screen.getByRole('heading', { name: '恢复 / 导入公开模型配置' })).toBeDefined();

    // Paste JSON into textarea
    const textarea = screen.getByPlaceholderText(/\[ { "id": "gpt-4o"/);
    fireEvent.change(textarea, {
      target: {
        value: JSON.stringify([
          { id: 'custom-model-1', display_name: 'Custom 1', enabled: true },
        ]),
      },
    });

    expect(screen.getByText('已成功识别 1 个模型配置')).toBeDefined();

    // Submit import form
    const submitBtn = screen.getByRole('button', { name: /确认合并导入/ });
    fireEvent.click(submitBtn);

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/models/import',
        expect.objectContaining({
          method: 'POST',
          body: expect.stringContaining('custom-model-1'),
        })
      );
    });
  });
  it('saves identity and bindings without writing capabilities, defaults or prices', async () => {
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑模型 gpt-4o' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).queryByLabelText('默认最大输出')).toBeNull();
    expect(within(dialog).queryByLabelText('对外上下文总窗口')).toBeNull();
    expect(within(dialog).queryByLabelText('官方价 输入')).toBeNull();
    fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/models/gpt-4o' && init?.method === 'PUT')).toBe(true));
    const saved = JSON.parse(fetchMock.mock.calls.find(([url, init]) => url === '/api/models/gpt-4o' && init?.method === 'PUT')![1].body);
    for (const field of ['config', 'published_specs', 'associated_prices']) expect(saved).not.toHaveProperty(field);
    expect(saved.bindings[0]).not.toHaveProperty('effective');
  });

  it('imports identity and references while leaving retired declarations out of the write', async () => {
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: /恢复配置/ }));
    const backup = { models: [{ id: 'restored', config: { context_window: 1 }, published_specs: { streaming: false }, catalog_model_ref: { source: 'builtin', id: 'source' }, bindings: [] }], prices: [{ channel_id: 0, model: 'restored', role: 'reference' }] };
    fireEvent.change(screen.getByPlaceholderText(/\[ { "id": "gpt-4o"/), { target: { value: JSON.stringify(backup) } });
    fireEvent.click(screen.getByRole('button', { name: /确认合并导入/ }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => url === '/api/models/import')).toBe(true));
    const body = JSON.parse(fetchMock.mock.calls.find(([url]) => url === '/api/models/import')![1].body);
    expect(body.prices).toEqual(backup.prices);
    expect(body.models[0].catalog_model_ref).toEqual(backup.models[0].catalog_model_ref);
    expect(body.models[0]).not.toHaveProperty('config'); expect(body.models[0]).not.toHaveProperty('published_specs');
  });

  it('shows server-computed mixed specifications read-only and still allows saving', async () => {
    const first = { ...mockModels[0].bindings[0], effective: { config: { context_window: 1000000 }, sources: {} } };
    const second = { ...first, channel_id: 2, channel_name: 'Alternate', effective: { config: { context_window: 128000 }, sources: {} } };
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/models' && !init?.method) return Promise.resolve(jsonResponse([{ ...mockModels[0], bindings: [first, second] }]));
      if (url === '/api/models/specs') return Promise.resolve(jsonResponse({ data: [{ id: 'gpt-4o', context_length: null, max_output_tokens: 0, streaming: false, modalities: { input: [], output: ['text'] }, differences: [{ field: 'context_window', bindings: [{ name: 'OpenAI', model: 'gpt-4o', known: true, value: 1000000 }, { name: 'Alternate', model: 'gpt-4o', known: true, value: 128000 }] }], recommendation: '存在差异，建议拆分公开 ID' }] }));
      return base(url, init);
    });
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');
    expect(screen.getByTitle('上下文：未知')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: '编辑模型 gpt-4o' }));
    fireEvent.click(screen.getByRole('button', { name: /公开规格/ }));
    await screen.findByText('绑定规格差异 · 1');
    expect(screen.getByText('存在差异，建议拆分公开 ID')).toBeDefined();
    expect(screen.queryByLabelText('对外上下文总窗口')).toBeNull();
    const specs = screen.getByLabelText('只读公开规格');
    expect(within(specs).getByText('不支持')).toBeDefined(); expect(within(specs).getByText('空列表')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/models/gpt-4o' && init?.method === 'PUT')).toBe(true));
  });

  it('folds API specification differences with clear labels and keeps only additional warnings', async () => {
    const warning = 'bindings have differing or unknown specifications';
    const recommendation = 'Split differing specifications into separate public model IDs; complete unknown upstream model specifications.';
    const differences = [
      { field: 'context_length', bindings: [{ name: 'Origin', model: 'gpt-4o', known: false, value: null }] },
      { field: 'thinking_supported', bindings: [{ name: 'Origin', model: 'gpt-4o', known: true, value: false }] },
      { field: 'thinking_mandatory', bindings: [{ name: 'Origin', model: 'gpt-4o', known: true, value: true }, { name: 'Other', model: 'gpt-4o', known: true, value: false }] },
      { field: 'thinking_modes', bindings: [{ name: 'Origin', model: 'gpt-4o', known: true, value: [] }] },
      { field: 'thinking_options', bindings: [{ name: 'Origin', model: 'gpt-4o', known: true, value: [] }] },
      { field: 'output_limit', bindings: [{ name: 'Origin', model: 'gpt-4o', known: true, value: 2048 }] },
    ];
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => url === '/api/models/specs' ? Promise.resolve(jsonResponse({ data: [{ id: 'gpt-4o', streaming: false, modalities: { input: [], output: ['text'] }, differences, warnings: [warning, 'New upstream warning'], recommendation }] })) : base(url, init));
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑模型 gpt-4o' }));
    fireEvent.click(screen.getByRole('button', { name: /公开规格/ }));
    const summary = await screen.findByText('绑定规格差异 · 6');
    expect(summary.closest('details')).toHaveProperty('open', false);
    const specs = screen.getByLabelText('只读公开规格');
    expect(within(specs).getByText('不支持')).toBeTruthy();
    expect(within(specs).queryByText(warning)).toBeNull(); expect(within(specs).queryByText(recommendation)).toBeNull();
    expect(within(specs).getByText('New upstream warning')).toBeTruthy();
    fireEvent.click(summary);
    expect(summary.closest('details')).toHaveProperty('open', true);
    for (const label of ['上下文总窗口：Origin / gpt-4o = 未知', '思考支持：Origin / gpt-4o = 不支持', '思考是否必需：Origin / gpt-4o = 必需；Other / gpt-4o = 可关闭', '原生思考映射资料：Origin / gpt-4o = 空列表', '支持的思考选项：Origin / gpt-4o = 空列表', '原生输出限制资料：Origin / gpt-4o = 2048']) expect(within(specs).getByText(label)).toBeTruthy();
  });

  it('keeps unsaved identity drafts and disables jumps until saved', async () => {
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑模型 gpt-4o' }));
    const dialog = screen.getByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText('模型显示名'), { target: { value: 'Draft name' } });
    fireEvent.click(within(dialog).getByRole('button', { name: /公开规格/ }));
    expect(within(dialog).getByRole('button', { name: '管理官方 API 价 ↗' }).hasAttribute('disabled')).toBe(true);
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
    expect(screen.getByRole('dialog')).toBe(dialog);
    expect((within(dialog).getByLabelText('模型显示名') as HTMLInputElement).value).toBe('Draft name');
  });

  it('exports model references and the official API price without old overrides', async () => {
    const rows = [{ channel_id: 0, model: 'gpt-4o', display_name: 'GPT-4o', origin: 'manual', selection: { mode: 'manual', table: { currency: 'USD', unit_tokens: 1000000, rates: { input: 0, output: 1, cache_read: null, cache_write: null }, rules: [] } } }];
    const reference = { source: 'builtin', id: 'origin' };
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => url === '/api/models' && !init?.method ? Promise.resolve(jsonResponse([{ ...mockModels[0], associated_prices: rows, catalog_model_ref: reference }])) : base(url, init));
    let text = '';
    vi.stubGlobal('Blob', class { constructor(parts: string[]) { text = parts.join(''); } });
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: vi.fn(() => 'blob:backup') });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() });
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    render(<Models />);
    await screen.findByText('GPT-4o Omnipresent');
    fireEvent.click(screen.getByRole('button', { name: /备份配置/ }));
    const backup = JSON.parse(text);
    expect(backup.prices).toEqual(rows); expect(backup.models[0].catalog_model_ref).toEqual(reference);
    expect(backup.models[0]).not.toHaveProperty('published_specs'); expect(backup.models[0]).not.toHaveProperty('config');
    expect(backup.models[0].bindings[0]).not.toHaveProperty('effective');
  });
  it('renames once and keeps the editor open when the atomic rename is rejected', async () => {
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => url.endsWith('/rename') ? Promise.resolve(jsonResponse({ error: '目标模型已存在' }, 409)) : base(url, init));
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑模型 gpt-4o' }));
    fireEvent.change(screen.getByLabelText('对外模型 ID'), { target: { value: 'already-used' } });
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    expect(within(await screen.findByRole('dialog', { name: '修改公开模型 ID？' })).getByText(/承接当前官方 API 价/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '修改模型 ID' }));
    await screen.findByText('目标模型已存在');
    const writes = fetchMock.mock.calls.filter(([, init]) => init?.method && init.method !== 'GET');
    expect(writes).toHaveLength(1);
    expect(writes[0][0]).toBe('/api/models/gpt-4o/rename');
    expect(JSON.parse(String(writes[0][1].body)).id).toBe('already-used');
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(screen.getByLabelText('对外模型 ID')).toHaveProperty('value', 'already-used');
  });

  it('uses public specs for row summaries even when native binding metadata disagrees', async () => {
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => url === '/api/models/specs' ? Promise.resolve(jsonResponse({ data: [{ id: 'gpt-4o', context_length: 123000, modalities: { input: [], output: [] }, thinking: { supported: true, options: [] } }] })) : base(url, init));
    render(<Models />);
    await screen.findByTitle('上下文：123K');
    const row = screen.getByText('GPT-4o Omnipresent').closest('article')!;
    expect(within(row).getAllByText('空列表')).toHaveLength(2);
    const disabled = screen.getByText('Claude 3.7 Sonnet').closest('article')!;
    expect(within(disabled).queryByText('200K')).toBeNull();
    expect(within(disabled).getByTitle('上下文：未公开')).toBeTruthy();
    const before = fetchMock.mock.calls.filter(([url]) => url === '/api/models/specs').length;
    fireEvent.click(screen.getByRole('switch', { name: '停用 gpt-4o' }));
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => url === '/api/models/specs').length).toBeGreaterThan(before));
  });

  it('closes after a successful atomic rename and refreshes the public projection', async () => {
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => url.endsWith('/rename') ? Promise.resolve(jsonResponse(JSON.parse(String(init?.body)))) : base(url, init));
    render(<Models />);
    fireEvent.click(await screen.findByRole('button', { name: '编辑模型 gpt-4o' }));
    fireEvent.change(screen.getByLabelText('对外模型 ID'), { target: { value: 'renamed' } });
    fireEvent.click(screen.getByRole('button', { name: '保存更改' }));
    expect(within(await screen.findByRole('dialog', { name: '修改公开模型 ID？' })).getByText(/承接当前官方 API 价/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '修改模型 ID' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const writes = fetchMock.mock.calls.filter(([, init]) => init?.method && init.method !== 'GET');
    expect(writes).toHaveLength(1); expect(writes[0][0]).toBe('/api/models/gpt-4o/rename');
    expect(fetchMock.mock.calls.filter(([url]) => url === '/api/models/specs')).toHaveLength(2);
  });

  it.each([{ options: [] }, { options: null }, { options: ['enable', 'high'] }])('shares known-empty, unknown and semantic thinking options %j with the details view', async ({ options }) => {
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/models/specs') return Promise.resolve(jsonResponse({ data: [{ id: 'gpt-4o', context_length: null, modalities: { input: null, output: null }, thinking: { supported: true, options } }] }));
      if (url === '/api/models' && !init?.method) return Promise.resolve(jsonResponse([{ ...mockModels[0], bindings: mockModels[0].bindings.map(binding => ({ ...binding, effective: { config: { thinking_efforts: ['other'], native_thinking: { supported: true, modes: [{ id: 'native-mode' }] } }, sources: {} } })) }]));
      return base(url, init);
    });
    render(<Models />); await screen.findByText('GPT-4o Omnipresent');
    const row = screen.getByText('GPT-4o Omnipresent').closest('article')!;
    if (options?.length) expect(await within(row).findByTitle('支持选项: enable, high')).toHaveProperty('textContent', '2 挡');
    else if (options) expect(await within(row).findByText('空列表')).toBeTruthy();
    else expect(within(row).queryByTitle(/支持选项/)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '编辑模型 gpt-4o' }));
    fireEvent.click(screen.getByRole('button', { name: /公开规格/ }));
    const detail = await screen.findByLabelText('只读公开规格');
    expect(within(detail).getByText('支持的思考档位').nextElementSibling?.textContent).toBe(options == null ? '未知' : options.length ? 'enable / high' : '空列表');
    expect(fetchMock.mock.calls.filter(([url]) => url === '/api/models/specs')).toHaveLength(1);
  });

});
