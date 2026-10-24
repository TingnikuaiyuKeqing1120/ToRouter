// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { MCPToolbox } from './MCPToolbox';
import { mcp } from '../services/mcp';
import type { MCPTool } from '../types/mcp';

vi.mock('../services/mcp', () => ({ mcp: { tools: vi.fn() } }));
const tools: MCPTool[] = [
  { name: 'get_quota_snapshots', capability: 'quota.read', description: '读取全部额度。只读现有快照。', permission: 'read', read_only: true },
  { name: 'get_credential_quota', capability: 'quota.read', description: '查看指定凭证额度。', permission: 'read', read_only: true },
  { name: 'delete_channel', capability: 'channel.delete', description: '删除一个渠道。首次仅预览。', permission: 'maintain', read_only: false },
  { name: 'future_tool', capability: 'future.task', description: '新的未来功能', permission: 'manage', read_only: false },
];
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  vi.mocked(mcp.tools).mockResolvedValue(tools);
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

it('opens immediately, highlights only present core tools and derives unique capability counts', async () => {
  const loaded = vi.fn();
  render(<MCPToolbox cachedTools={null} onLoad={loaded} onClose={vi.fn()} />);
  const dialog = screen.getByRole('dialog', { name: 'MCP 功能工具箱' });
  expect(within(dialog).getByRole('searchbox')).toBe(document.activeElement);
  expect(within(dialog).getByText('正在加载数据…')).toBeTruthy();
  await screen.findByText('查看全部凭证额度');
  expect(dialog.querySelector('.mcp-toolbox-total')?.textContent).toBe('4 个工具 · 3 项能力');
  expect(dialog.querySelectorAll('details')).toHaveLength(1);
  expect(loaded).toHaveBeenCalledWith(tools);
  fireEvent.click(screen.getByRole('button', { name: '渠道与凭证 1' }));
  const card = screen.getByText('删除渠道').closest('details')!;
  expect(within(card).getByText('维护')).toBeTruthy();
  fireEvent.click(card.querySelector('summary')!);
  expect(card.open).toBe(true);
  expect(within(card).getByText('delete_channel')).toBeTruthy();
  expect(within(card).getByText('删除一个渠道。首次仅预览。')).toBeTruthy();
});

it('searches outside the selected category, exposes new tools and clears empty results', () => {
  render(<MCPToolbox cachedTools={tools} onLoad={vi.fn()} onClose={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: '额度与自动化 2' }));
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'future' } });
  expect(screen.getByRole('heading', { name: 'future_tool' })).toBeTruthy();
  expect(screen.getByRole('status').textContent).toBe('1 个工具');
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: '没有这个东西' } });
  expect(screen.getByText('没有匹配的功能')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '清空搜索' }));
  expect(screen.getAllByRole('group')).toHaveLength(2);
  fireEvent.click(screen.getByRole('button', { name: '全部功能 4' }));
  expect(screen.getAllByRole('group')).toHaveLength(4);
  expect(mcp.tools).not.toHaveBeenCalled();
});

it('allows retry after a failed fetch and does not cache a result after closure', async () => {
  vi.mocked(mcp.tools).mockRejectedValueOnce(new Error('temporarily offline'));
  const loaded = vi.fn();
  const view = render(<MCPToolbox cachedTools={null} onLoad={loaded} onClose={vi.fn()} />);
  await screen.findByRole('alert');
  fireEvent.click(screen.getByRole('button', { name: '重试' }));
  await screen.findByText('查看全部凭证额度');
  expect(mcp.tools).toHaveBeenCalledTimes(2);
  view.unmount();
  let finish!: (tools: MCPTool[]) => void;
  vi.mocked(mcp.tools).mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
  const late = vi.fn();
  const next = render(<MCPToolbox cachedTools={null} onLoad={late} onClose={vi.fn()} />);
  const calls = vi.mocked(mcp.tools).mock.calls;
  const signal = calls[calls.length - 1][0]!;
  next.unmount();
  expect(signal.aborted).toBe(true);
  await act(async () => finish(tools));
  expect(late).not.toHaveBeenCalled();
});

it('closes with Escape or the close button without calling a tool', async () => {
  const close = vi.fn();
  render(<MCPToolbox cachedTools={tools} onLoad={vi.fn()} onClose={close} />);
  fireEvent(screen.getByRole('dialog'), new Event('cancel', { bubbles: true, cancelable: true }));
  await waitFor(() => expect(close).toHaveBeenCalledOnce());
  fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
  expect(close).toHaveBeenCalledTimes(2);
  expect(mcp.tools).not.toHaveBeenCalled();
});
