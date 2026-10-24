// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import Models from './Models';
import { ConfirmProvider } from '../components/Confirm';
import type { Channel, DownstreamModelView, TagGroupWithTags } from '../api';

const groups: TagGroupWithTags[] = [
  { id: 'modality', type: 'builtin', name: '模态支持', description: '', sort_order: 0, tags: [{ id: 'modality:input:image', group_id: 'modality', name: '输入图像', color: 'sky', description: '', sort_order: 0 }] },
  { id: 'billing', type: 'builtin', name: '计费模式', description: '', sort_order: 1, tags: [{ id: 'billing:free', group_id: 'billing', name: '免费', color: 'sky', description: '', sort_order: 0 }] },
  { id: 'purpose', type: 'user', name: '用途', description: '', sort_order: 2, tags: ['编程', '写作'].map((name, i) => ({ id: 'purpose:' + i, group_id: 'purpose', name, color: 'sky', description: '', sort_order: i })) },
  { id: 'team', type: 'user', name: '团队', description: '', sort_order: 3, tags: [{ id: 'team:a', group_id: 'team', name: '个人', color: 'sky', description: '', sort_order: 0 }] },
];
const channels: Channel[] = [{ id: 1, name: '测试渠道', api_key: '', models: ['enabled-model'], available_models: ['enabled-model', 'disabled-model', 'legacy-model'], enabled: true, endpoints: [], has_key: true }];
let models: DownstreamModelView[];
let fetcher: ReturnType<typeof vi.fn>;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  models = ['alpha', 'beta', 'gamma'].map((id, i) => ({ id, enabled: true, fallback: true, tags: i === 1 ? ['purpose:1'] : ['purpose:0', 'team:a'], effective_tags: i === 1 ? ['purpose:1'] : ['purpose:0', 'team:a', ...(i === 0 ? ['modality:input:image'] : [])], bindings: [{ channel_id: 1, channel_name: '测试渠道', channel_exists: true, model: i === 0 ? 'legacy-model' : 'enabled-model', enabled: true, priority: 0 }] }));
  fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === '/api/channels') return json(channels);
    if (url === '/api/tag-groups') return json(groups);
    if (url === '/api/models/specs') return json({ data: [] });
    if (url === '/api/models' && !init?.method) return json(models);
    return json({});
  });
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });
const mount = () => render(<ConfirmProvider><Models /></ConfirmProvider>);
const visibleIDs = () => screen.getAllByRole('article').map(row => within(row).getByRole('heading').textContent);

it('filters with grouped multiple choices: OR within a group and AND across groups, then clears', async () => {
  mount(); await screen.findByRole('heading', { name: 'alpha' });
  const filters = screen.getByRole('region', { name: '按标签筛选' });
  const purpose = await within(filters).findByRole('group', { name: '用途' });
  expect(within(filters).queryByRole('group', { name: '计费模式' })).toBeNull();
  fireEvent.click(within(purpose).getByRole('button', { name: '编程' }));
  expect(visibleIDs()).toEqual(['alpha', 'gamma']);
  fireEvent.click(within(purpose).getByRole('button', { name: '写作' }));
  expect(visibleIDs()).toEqual(['alpha', 'beta', 'gamma']);
  fireEvent.click(within(filters).getByRole('button', { name: '输入图像' }));
  expect(visibleIDs()).toEqual(['alpha']);
  fireEvent.click(within(filters).getByRole('button', { name: /清除标签筛选/ }));
  expect(visibleIDs()).toEqual(['alpha', 'beta', 'gamma']);
});

it('starts with expanded tag filters and preserves active criteria while collapsed', async () => {
  mount(); await screen.findByRole('heading', { name: 'alpha' });
  const filters = screen.getByRole('region', { name: '按标签筛选' });
  const toggle = within(filters).getByRole('button', { name: '标签筛选' });
  expect(toggle).toHaveProperty('ariaExpanded', 'true');
  fireEvent.click(await within(filters).findByRole('button', { name: '编程' }));
  expect(visibleIDs()).toEqual(['alpha', 'gamma']);
  fireEvent.click(toggle);
  expect(toggle).toHaveProperty('ariaExpanded', 'false');
  expect(within(filters).queryByRole('button', { name: '编程' })).toBeNull();
  expect(within(filters).getByText('已选 1 项')).toBeTruthy();
  expect(visibleIDs()).toEqual(['alpha', 'gamma']);
  fireEvent.click(within(filters).getByRole('button', { name: /清除标签筛选/ }));
  expect(visibleIDs()).toEqual(['alpha', 'beta', 'gamma']);
  expect(toggle).toHaveProperty('ariaExpanded', 'false');
  fireEvent.click(toggle);
  expect(within(filters).getByRole('button', { name: '编程' })).toHaveProperty('ariaPressed', 'false');
});

it('new binding choices use enabled models but a saved disabled binding remains visible and writable', async () => {
  mount(); fireEvent.click(await screen.findByRole('button', { name: '编辑模型 alpha' }));
  const dialog = screen.getByRole('dialog');
  const select = within(dialog).getByRole('combobox', { name: '上游模型' });
  expect(select).toHaveProperty('value', 'legacy-model');
  expect(within(select).getByRole('option', { name: /legacy-model.*保留已有绑定/ })).toBeTruthy();
  expect(within(select).queryByRole('option', { name: 'disabled-model' })).toBeNull();
  fireEvent.click(within(dialog).getByRole('button', { name: '添加绑定' }));
  expect(within(dialog).getAllByRole('combobox', { name: '上游模型' })[1]).toHaveProperty('value', 'enabled-model');
  fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
  await waitFor(() => expect(fetcher.mock.calls.some(([u, i]) => u === '/api/models/alpha' && i?.method === 'PUT')).toBe(true));
  expect(JSON.parse(fetcher.mock.calls.find(([u, i]) => u === '/api/models/alpha' && i?.method === 'PUT')![1].body).bindings.map((b: { model: string }) => b.model)).toEqual(['legacy-model', 'enabled-model']);
});

it('edits only custom tags in a dedicated pane and preserves the routing draft', async () => {
  mount(); fireEvent.click(await screen.findByRole('button', { name: '编辑模型 alpha' }));
  const dialog = screen.getByRole('dialog');
  fireEvent.change(within(dialog).getByLabelText('模型显示名'), { target: { value: '我的模型' } });
  fireEvent.click(within(dialog).getByRole('button', { name: '模型标签' }));
  await within(dialog).findByRole('group', { name: '用途' });
  expect(within(dialog).queryByRole('group', { name: '模态支持' })).toBeNull();
  expect(within(dialog).queryByRole('group', { name: '计费模式' })).toBeNull();
  fireEvent.click(within(dialog).getByRole('button', { name: '写作' }));
  fireEvent.click(within(dialog).getByRole('button', { name: '调用配置' }));
  expect(within(dialog).getByLabelText('模型显示名')).toHaveProperty('value', '我的模型');
  fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
  fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  const payload = JSON.parse(fetcher.mock.calls.find(([u, i]) => u === '/api/models/alpha' && i?.method === 'PUT')![1].body);
  expect(payload.display_name).toBe('我的模型'); expect(payload.tags).toContain('purpose:1');
});

it('failed save from the exit prompt keeps the same draft and hidden validation returns to the input', async () => {
  const base = fetcher.getMockImplementation()!;
  fetcher.mockImplementation((u: string, i?: RequestInit) => i?.method === 'PUT' ? Promise.resolve(json({ error: '保存被拒绝' }, 409)) : base(u, i));
  mount(); fireEvent.click(await screen.findByRole('button', { name: '编辑模型 alpha' }));
  const dialog = screen.getByRole('dialog');
  fireEvent.change(within(dialog).getByLabelText('模型显示名'), { target: { value: '保留草稿' } });
  fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
  fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '确认保存' }));
  await within(dialog).findByText('保存被拒绝');
  expect(within(dialog).getByLabelText('模型显示名')).toHaveProperty('value', '保留草稿');
  fireEvent.change(within(dialog).getByLabelText('对外模型 ID'), { target: { value: '' } });
  fireEvent.click(within(dialog).getByRole('button', { name: '模型标签' }));
  fireEvent.click(within(dialog).getByRole('button', { name: '保存更改' }));
  await waitFor(() => expect(document.activeElement).toBe(within(dialog).getByLabelText('对外模型 ID')));
});

it('keyboard reordering uses the real reorder contract and filtering prevents ordering changes', async () => {
  mount(); const handle = await screen.findByRole('button', { name: '拖拽调整模型 alpha 顺序' });
  fireEvent.keyDown(handle, { key: 'ArrowDown' });
  await waitFor(() => expect(fetcher.mock.calls.some(([u, i]) => u === '/api/models/reorder' && i?.method === 'PUT')).toBe(true));
  expect(JSON.parse(fetcher.mock.calls.find(([u, i]) => u === '/api/models/reorder' && i?.method === 'PUT')![1].body)).toEqual({ order: ['beta', 'alpha', 'gamma'] });
  await waitFor(() => expect(screen.getByRole('button', { name: '编辑模型 alpha' })).not.toHaveProperty('disabled', true));
  fireEvent.click(screen.getByRole('button', { name: '编程' }));
  fireEvent.keyDown(handle, { key: 'ArrowDown' });
  expect(fetcher.mock.calls.filter(([u]) => u === '/api/models/reorder')).toHaveLength(1);
});

it('preserves explicit custom tags when importing a backup through the exit confirmation', async () => {
  mount(); fireEvent.click(await screen.findByRole('button', { name: /恢复配置/ }));
  fireEvent.change(screen.getByPlaceholderText(/\[ { "id": "gpt-4o"/), { target: { value: JSON.stringify({ models: [{ id: 'restore', tags: ['purpose:0'], bindings: [] }] }) } });
  fireEvent.click(screen.getByRole('button', { name: '取消' }));
  fireEvent.click(within(await screen.findByRole('dialog', { name: '导入当前配置？' })).getByRole('button', { name: '确认导入' }));
  await waitFor(() => expect(fetcher.mock.calls.some(([u, i]) => u === '/api/models/import' && i?.method === 'POST')).toBe(true));
  expect(JSON.parse(fetcher.mock.calls.find(([u]) => u === '/api/models/import')![1].body).models[0].tags).toEqual(['purpose:0']);
});
