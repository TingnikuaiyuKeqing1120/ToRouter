// @vitest-environment jsdom
import { cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { ConfirmProvider } from './Confirm';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ModelTagWorkbench } from './ModelTagWorkbench';
import type { Channel, TagGroupWithTags } from '../types';

const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) =>
  renderUI(ui, { wrapper: ConfirmProvider, ...options });

const json = (value: unknown, status = 200) =>
  new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });

const mockTagGroups: TagGroupWithTags[] = [
  {
    id: 'billing',
    name: '计费模式',
    description: '付费形式',
    type: 'builtin',
    sort_order: 1,
    tags: [
      { id: 'billing:free', group_id: 'billing', name: '免费', description: '', color: 'emerald', sort_order: 1 },
      { id: 'billing:payg', group_id: 'billing', name: '按量', description: '', color: 'sky', sort_order: 2 },
    ],
  },
  {
    id: 'privacy',
    name: '数据隐私',
    description: '隐私政策',
    type: 'builtin',
    sort_order: 2,
    tags: [
      { id: 'privacy:zdr', group_id: 'privacy', name: '零数据留存', description: '', color: 'violet', sort_order: 1 },
    ],
  },
  {
    id: 'scenario',
    name: '业务场景',
    description: '使用场景分类',
    type: 'user',
    sort_order: 3,
    tags: [
      { id: 'scenario:coding', group_id: 'scenario', name: '代码开发', description: '', color: 'amber', sort_order: 1 },
      { id: 'scenario:chat', group_id: 'scenario', name: '通用对话', description: '', color: 'slate', sort_order: 2 },
    ],
  },
];

const mockChannels: Channel[] = [
  {
    id: 1,
    name: '硅谷主力',
    api_key: 'sk-1',
    has_key: true,
    enabled: true,
    models: ['gpt-4o', 'claude-3-5-sonnet'],
    available_models: ['gpt-4o', 'claude-3-5-sonnet', 'o1-mini'],
    endpoints: [{ format: 'openai_chat_completions', base_url: 'https://api.openai.com', state: '' as any }],
    tags: ['billing:payg'],
    model_tags: {
      'gpt-4o': ['billing:free', 'scenario:coding'],
    },
  },
  {
    id: 2,
    name: '备用通道',
    api_key: 'sk-2',
    has_key: true,
    enabled: true,
    models: ['deepseek-chat'],
    available_models: ['deepseek-chat'],
    endpoints: [{ format: 'openai_chat_completions', base_url: 'https://api.deepseek.com', state: '' as any }],
    tags: ['billing:free', 'privacy:zdr'],
  },
];

let fetcher: ReturnType<typeof vi.fn>;

beforeEach(() => {
  localStorage.clear();
  HTMLDialogElement.prototype.showModal = function () {
    this.setAttribute('open', '');
  };
  HTMLDialogElement.prototype.close = function () {
    this.removeAttribute('open');
  };

  fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/channels') {
      return Promise.resolve(json(mockChannels));
    }
    if (url.startsWith('/api/channels/') && init?.method === 'PUT') {
      const id = Number(url.replace('/api/channels/', ''));
      const body = JSON.parse(String(init.body));
      return Promise.resolve(json({ id, ...body }));
    }
    if (url === '/api/tag-groups') {
      return Promise.resolve(json(mockTagGroups));
    }
    return Promise.resolve(json([]));
  });

  vi.stubGlobal('fetch', fetcher);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const writes = () => fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');

describe('ModelTagWorkbench', () => {
  it('renders models table with channel inheritance and independent tags correctly', async () => {
    render(<ModelTagWorkbench />);

    // Table rows
    expect(await screen.findByText('gpt-4o')).toBeTruthy();
    expect(screen.getByText('claude-3-5-sonnet')).toBeTruthy();
    expect(screen.getByText('deepseek-chat')).toBeTruthy();

    // Check count
    expect(screen.getByText(/3 个模型/)).toBeTruthy();

    // gpt-4o has independent billing:free and scenario:coding
    const gptRow = screen.getByText('gpt-4o').closest('tr')!;
    expect(within(gptRow).getByText('免费')).toBeTruthy();
    expect(within(gptRow).getByText('代码开发')).toBeTruthy();
    expect(within(gptRow).getAllByText('独立').length).toBe(2);

    // claude-3-5-sonnet has no model_tags, follows channel tags (billing:payg)
    const claudeRow = screen.getByText('claude-3-5-sonnet').closest('tr')!;
    expect(within(claudeRow).getByText('按量')).toBeTruthy();
    expect(within(claudeRow).getAllByText('跟随').length).toBe(3);
  });

  it('filters rows by search input and channel filter', async () => {
    render(<ModelTagWorkbench />);

    await screen.findByText('gpt-4o');

    // Search by model name
    const searchInput = screen.getByLabelText('搜索模型标签');
    fireEvent.change(searchInput, { target: { value: 'deepseek' } });
    expect(screen.queryByText('gpt-4o')).toBeNull();
    expect(screen.getByText('deepseek-chat')).toBeTruthy();

    // Clear search
    fireEvent.click(screen.getByRole('button', { name: /清除搜索/ }));
    expect(screen.getByText('gpt-4o')).toBeTruthy();

    // Uncheck channel 1 (硅谷主力)
    const channel1Checkbox = screen.getByRole('checkbox', { name: '显示渠道 硅谷主力' });
    fireEvent.click(channel1Checkbox);
    expect(screen.queryByText('gpt-4o')).toBeNull();
    expect(screen.getByText('deepseek-chat')).toBeTruthy();
  });

  it('shows disabled models when showDisabled is checked', async () => {
    render(<ModelTagWorkbench />);

    await screen.findByText('gpt-4o');
    expect(screen.queryByText('o1-mini')).toBeNull();

    // Check show disabled
    fireEvent.click(screen.getByRole('checkbox', { name: '显示停用模型' }));
    expect(await screen.findByText('o1-mini')).toBeTruthy();
    expect(screen.getByText('（已停用）')).toBeTruthy();
  });

  it('edits a single model tag via dialog and saves updated channel', async () => {
    render(<ModelTagWorkbench />);

    // Click edit on claude-3-5-sonnet
    const editBtn = await screen.findByRole('button', { name: '编辑模型标签 claude-3-5-sonnet' });
    fireEvent.click(editBtn);

    const dialog = screen.getByRole('dialog', { name: /编辑模型标签 · claude-3-5-sonnet/ });
    expect(dialog).toBeTruthy();

    // Click '免费' in billing group
    const freeBtn = within(dialog).getByRole('button', { name: '免费' });
    fireEvent.click(freeBtn);

    // Save
    fireEvent.click(within(dialog).getByRole('button', { name: '保存标签' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0][0]).toBe('/api/channels/1');

    const body = JSON.parse(String(writes()[0][1]?.body));
    expect(body.model_tags['claude-3-5-sonnet']).toContain('billing:free');
  });

  it('reverts a model group tag to follow channel defaults', async () => {
    render(<ModelTagWorkbench />);

    // Click edit on gpt-4o which has independent tags
    const editBtn = await screen.findByRole('button', { name: '编辑模型标签 gpt-4o' });
    fireEvent.click(editBtn);

    const dialog = screen.getByRole('dialog', { name: /编辑模型标签 · gpt-4o/ });

    // Click '恢复跟随渠道' on billing group
    const revertBtn = within(dialog).getByRole('button', { name: '恢复跟随 计费模式' });
    fireEvent.click(revertBtn);

    // Save
    fireEvent.click(within(dialog).getByRole('button', { name: '保存标签' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    const body = JSON.parse(String(writes()[0][1]?.body));
    // billing:free removed, only scenario:coding remains
    expect(body.model_tags['gpt-4o']).toEqual(['scenario:coding']);
  });

  it('batch updates multiple selected models across channels', async () => {
    render(<ModelTagWorkbench />);

    await screen.findByText('gpt-4o');

    // Select all visible models
    fireEvent.click(screen.getByRole('checkbox', { name: '全选可见模型' }));

    // Click batch button
    const batchBtn = screen.getByRole('button', { name: /批量设置标签（3）/ });
    fireEvent.click(batchBtn);

    const dialog = screen.getByRole('dialog', { name: '批量设置模型标签' });
    expect(within(dialog).getByText(/已选择 3 个模型（涉及 2 个渠道）/)).toBeTruthy();

    // Select '免费' for billing group
    fireEvent.click(within(dialog).getByRole('button', { name: '免费' }));

    // Click Apply
    fireEvent.click(within(dialog).getByRole('button', { name: '应用到这 3 个模型' }));

    await waitFor(() => expect(writes()).toHaveLength(2));

    // Both channel 1 and channel 2 were saved
    const channel1Call = writes().find(([url]) => url === '/api/channels/1');
    const channel2Call = writes().find(([url]) => url === '/api/channels/2');
    expect(channel1Call).toBeTruthy();
    expect(channel2Call).toBeTruthy();

    const body1 = JSON.parse(String(channel1Call![1]?.body));
    const body2 = JSON.parse(String(channel2Call![1]?.body));

    expect(body1.model_tags['gpt-4o']).toContain('billing:free');
    expect(body1.model_tags['claude-3-5-sonnet']).toContain('billing:free');
    expect(body2.model_tags['deepseek-chat']).toContain('billing:free');
  });

  it('batch reverts multiple selected models to follow channel defaults', async () => {
    render(<ModelTagWorkbench />);

    await screen.findByText('gpt-4o');

    // Select all visible models
    fireEvent.click(screen.getByRole('checkbox', { name: '全选可见模型' }));

    // Click batch button
    fireEvent.click(screen.getByRole('button', { name: /批量设置标签（3）/ }));

    const dialog = screen.getByRole('dialog', { name: '批量设置模型标签' });

    // Change mode to follow channel defaults
    fireEvent.change(within(dialog).getByLabelText('选择配置方式'), { target: { value: 'follow' } });

    // Click Apply
    fireEvent.click(within(dialog).getByRole('button', { name: '应用到这 3 个模型' }));

    await waitFor(() => expect(writes()).toHaveLength(2));

    const channel1Call = writes().find(([url]) => url === '/api/channels/1');
    const body1 = JSON.parse(String(channel1Call![1]?.body));

    // gpt-4o billing tag removed, only scenario:coding remains
    expect(body1.model_tags['gpt-4o']).not.toContain('billing:free');
    expect(body1.model_tags['gpt-4o']).toEqual(['scenario:coding']);
  });
});
