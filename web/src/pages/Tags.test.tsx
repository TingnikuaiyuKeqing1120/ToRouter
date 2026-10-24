// @vitest-environment jsdom
import { cleanup, fireEvent, render as renderUI, screen, waitFor, within } from '@testing-library/react';
import { ConfirmProvider } from '../components/Confirm';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Tags from './Tags';
import type { TagGroupWithTags, TagReferences } from '../types';
const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) => renderUI(ui, { wrapper: ConfirmProvider, ...options });

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });

const mockGroups: TagGroupWithTags[] = [
  {
    id: 'billing',
    name: '计费与成本类型',
    description: '标识上游 API 渠道的付费形式',
    type: 'builtin',
    sort_order: 1,
    tags: [
      {
        id: 'billing:free',
        group_id: 'billing',
        name: '免费 / 赠额',
        color: 'emerald',
        description: '免费渠道、试用赠额或社区公益接口',
        sort_order: 1,
      },
      {
        id: 'billing:custom_free',
        group_id: 'billing',
        name: '自定义赠额',
        color: 'sky',
        description: '临时活动额度',
        sort_order: 2,
      },
    ],
  },
  {
    id: 'custom_group',
    name: '业务场景分类',
    description: '按内部业务场景划分',
    type: 'user',
    sort_order: 2,
    tags: [
      {
        id: 'scenario:testing',
        group_id: 'custom_group',
        name: '内部测试专用',
        color: 'amber',
        description: '只供测试',
        sort_order: 1,
      },
    ],
  },
];

let fetcher: ReturnType<typeof vi.fn>;

beforeEach(() => {
  sessionStorage.clear();
  localStorage.clear();

  fetcher = vi.fn((url: string, init?: RequestInit) => {
    // GET /api/tag-groups
    if (url === '/api/tag-groups' && (!init?.method || init.method === 'GET')) {
      return Promise.resolve(json(mockGroups));
    }
    // POST /api/tags/reset-builtin
    if (url === '/api/tags/reset-builtin' && init?.method === 'POST') {
      return Promise.resolve(json({ message: 'reset ok' }));
    }
    // POST /api/tag-groups
    if (url === '/api/tag-groups' && init?.method === 'POST') {
      return Promise.resolve(json({ id: 'new_group', name: '新组', type: 'user', sort_order: 3 }));
    }
    // POST /api/tags
    if (url === '/api/tags' && init?.method === 'POST') {
      return Promise.resolve(json({ id: 'billing:vip', name: 'VIP专享', color: 'rose', sort_order: 3 }));
    }
    // PUT /api/tags/billing%3Acustom_free
    if (url.startsWith('/api/tags/billing%3Acustom_free') && init?.method === 'PUT') {
      return Promise.resolve(json({ id: 'billing:custom_free', name: '改名后的赠额', color: 'violet', sort_order: 2 }));
    }
    // GET /api/tags/references
    if (url.includes('/api/tags/references')) {
      if (url.includes('scenario%3Atesting')) {
        const refs: TagReferences = {
          tag_id: 'scenario:testing',
          channel_names: ['OpenAI 官方主通道'],
          downstream_models: ['GPT-4o 旗舰'],
          client_key_names: [],
        };
        return Promise.resolve(json(refs));
      }
      return Promise.resolve(json({ tag_id: 'billing:custom_free', channel_names: [], downstream_models: [], client_key_names: [] } as TagReferences));
    }
    // DELETE /api/tags/...
    if (url.startsWith('/api/tags/') && init?.method === 'DELETE') {
      return Promise.resolve(json({ message: 'deleted ok' }));
    }
    return Promise.resolve(json([]));
  });

  vi.stubGlobal('fetch', fetcher);
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  vi.stubGlobal('alert', vi.fn());
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('Tags Page Management', () => {
  it('keeps a tag draft when dismissing exit, retains a rejected save, and closes after retry succeeds', async () => {
    let fail = true;
    const base = fetcher.getMockImplementation()!;
    fetcher.mockImplementation((url, init) => init?.method === 'PUT' && fail ? Promise.resolve(json({ error: 'tag save rejected' }, 409)) : base(url, init));
    render(<Tags />); fireEvent.click(await screen.findByRole('button', { name: '编辑 自定义赠额' }));
    const name = screen.getByDisplayValue('自定义赠额'); fireEvent.change(name, { target: { value: 'Changed tag' } });
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull()); expect(name).toHaveProperty('value', 'Changed tag');
    fireEvent.click(screen.getByRole('button', { name: '取消' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await within(screen.getByRole('dialog', { name: '编辑标签' })).findByText('保存失败: tag save rejected');
    expect(name).toHaveProperty('value', 'Changed tag'); expect(alert).not.toHaveBeenCalled();
    fail = false; fireEvent.click(screen.getByRole('button', { name: '取消' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('validates new group fields on exit and saves the completed group before closing', async () => {
    render(<Tags />); await screen.findByText('计费与成本类型');
    fireEvent.click(screen.getByRole('button', { name: /新建标签组/ }));
    fireEvent.change(screen.getByPlaceholderText('如 scenario, domain'), { target: { value: 'new-group' } });
    fireEvent.click(screen.getByRole('button', { name: '取消' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
    expect(screen.getByRole('dialog', { name: '新建标签组' })).toBeTruthy(); expect(fetcher.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false);
    fireEvent.change(screen.getByPlaceholderText('如 使用场景, 业务线'), { target: { value: 'New group' } });
    fireEvent.click(screen.getByRole('button', { name: '取消' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(JSON.parse(String(fetcher.mock.calls.find(([, init]) => init?.method === 'POST')![1]?.body))).toMatchObject({ id: 'new-group', name: 'New group' });
  });

  it('renders tag groups, tags, and builtin badges correctly', async () => {
    render(<Tags />);

    // 页面标题
    expect(await screen.findByText('标签配置')).toBeTruthy();

    // 组标题与系统内置徽标
    expect(screen.getByText('计费与成本类型')).toBeTruthy();
    expect(screen.getByText('业务场景分类')).toBeTruthy();
    expect(screen.getByText('系统内置')).toBeTruthy();
    expect(screen.getByText('用户自定义')).toBeTruthy();

    // 标签名称
    expect(screen.getByText('免费 / 赠额')).toBeTruthy();
    expect(screen.getByText('自定义赠额')).toBeTruthy();
    expect(screen.getByText('内部测试专用')).toBeTruthy();

    // 内置核心标签不能有删除按钮
    expect(screen.queryByRole('button', { name: '删除 免费 / 赠额' })).toBeNull();
    // 非内置标签应有删除按钮
    expect(screen.getByRole('button', { name: '删除 自定义赠额' })).toBeTruthy();
    expect(screen.getByRole('button', { name: '删除 内部测试专用' })).toBeTruthy();
  });

  it('filters tags via search box', async () => {
    render(<Tags />);
    await screen.findByText('计费与成本类型');

    const searchInput = screen.getByPlaceholderText('搜索标签名称、ID 或描述...');
    fireEvent.change(searchInput, { target: { value: '内部测试' } });

    expect(screen.queryByText('免费 / 赠额')).toBeNull();
    expect(screen.getByText('内部测试专用')).toBeTruthy();
  });

  it('creates a new custom tag under an existing group', async () => {
    render(<Tags />);
    await screen.findByText('计费与成本类型');

    // 点击添加标签按钮
    const addTagButtons = screen.getAllByRole('button', { name: /添加标签/ });
    fireEvent.click(addTagButtons[0]);

    // 弹窗出现
    expect(screen.getByText('新建标签')).toBeTruthy();

    // 填写表单
    const idInput = screen.getByPlaceholderText('如 billing:custom 或 scenario:test');
    const nameInput = screen.getByPlaceholderText('如 免费 / 赠额');
    fireEvent.change(idInput, { target: { value: 'billing:vip' } });
    fireEvent.change(nameInput, { target: { value: 'VIP专享' } });

    // 提交保存
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => {
      const calls = fetcher.mock.calls;
      const postCall = calls.find(([url, init]) => url === '/api/tags' && init?.method === 'POST');
      expect(postCall).toBeTruthy();
      const body = JSON.parse(postCall![1].body);
      expect(body.id).toBe('billing:vip');
      expect(body.name).toBe('VIP专享');
    });
  });

  it('edits an existing tag without allowing ID mutation', async () => {
    render(<Tags />);
    await screen.findByText('自定义赠额');

    // 找到自定义赠额的编辑按钮
    const editBtn = screen.getByRole('button', { name: '编辑 自定义赠额' });
    fireEvent.click(editBtn);

    expect(screen.getByText('编辑标签')).toBeTruthy();

    // ID 输入框应该被 disabled
    const idInput = screen.getByDisplayValue('billing:custom_free') as HTMLInputElement;
    expect(idInput.disabled).toBe(true);

    // 修改名称
    const nameInput = screen.getByDisplayValue('自定义赠额');
    fireEvent.change(nameInput, { target: { value: '改名后的赠额' } });

    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => {
      const putCall = fetcher.mock.calls.find(
        ([url, init]) => url === '/api/tags/billing%3Acustom_free' && init?.method === 'PUT'
      );
      expect(putCall).toBeTruthy();
      const body = JSON.parse(putCall![1].body);
      expect(body.name).toBe('改名后的赠额');
    });
  });

  it('safely deletes a tag when no references exist', async () => {
    render(<Tags />);
    await screen.findByText('自定义赠额');

    const deleteBtn = screen.getByRole('button', { name: '删除 自定义赠额' });
    fireEvent.click(deleteBtn);

    // 弹窗提示
    await waitFor(() => {
      expect(screen.getByText('确认删除标签？')).toBeTruthy();
      expect(screen.getByText('该标签未被任何渠道或模型使用，可安全删除。')).toBeTruthy();
    });

    // 点击确认删除
    fireEvent.click(screen.getByRole('button', { name: '确认删除' }));

    await waitFor(() => {
      const delCall = fetcher.mock.calls.find(
        ([url, init]) => url === '/api/tags/billing%3Acustom_free?force=false' && init?.method === 'DELETE'
      );
      expect(delCall).toBeTruthy();
    });
  });

  it('warns with cascade detach when deleting a tag with references', async () => {
    render(<Tags />);
    await screen.findByText('内部测试专用');

    const deleteBtn = screen.getByRole('button', { name: '删除 内部测试专用' });
    fireEvent.click(deleteBtn);

    // 弹窗提示引用
    await waitFor(() => {
      expect(screen.getByText('确认删除标签？')).toBeTruthy();
      expect(screen.getByText(/检测到该标签正被以下对象使用：/)).toBeTruthy();
      expect(screen.getByText(/OpenAI 官方主通道/)).toBeTruthy();
      expect(screen.getByText(/GPT-4o 旗舰/)).toBeTruthy();
    });

    // 点击强制解绑删除
    fireEvent.click(screen.getByRole('button', { name: '确认删除并解绑' }));

    await waitFor(() => {
      const delCall = fetcher.mock.calls.find(
        ([url, init]) => url === '/api/tags/scenario%3Atesting?force=true' && init?.method === 'DELETE'
      );
      expect(delCall).toBeTruthy();
    });
  });

  it('resets builtin tags when clicking reset button', async () => {
    render(<Tags />);
    await screen.findByText('标签配置');

    const resetBtn = screen.getByRole('button', { name: '重置内置预设' });
    fireEvent.click(resetBtn);
    fireEvent.click(await screen.findByRole('button', { name: '重置内置标签' }));

    await waitFor(() => {
      const resetCall = fetcher.mock.calls.find(
        ([url, init]) => url === '/api/tags/reset-builtin' && init?.method === 'POST'
      );
      expect(resetCall).toBeTruthy();
    });
  });
});
