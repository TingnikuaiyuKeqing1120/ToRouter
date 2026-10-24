// @vitest-environment jsdom
import { useMemo, useRef, useState } from 'react';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import MCP from './MCP';
import { ConfirmProvider } from '../components/Confirm';
import { NavigationGuard, type BeforeLeave } from '../components/NavigationGuard';
import type { MCPConnection } from '../types/mcp';

const connection: MCPConnection = { id: 7, name: '桌面助手', enabled: true, permissions: ['read', 'manage'], created_at: '2026-10-18T00:00:00Z', last_used_at: null };
const status = { origin: 'http://127.0.0.1:49123', endpoint: 'http://127.0.0.1:49123/mcp', enabled: true, tool_count: 12, capability_count: 8 };
const json = (value: unknown, code = 200) => new Response(JSON.stringify(value), { status: code });
let records: MCPConnection[];
let writes: { url: string; method: string; body?: unknown }[];

function Harness() {
  const handlers = useRef<BeforeLeave[]>([]), [left, setLeft] = useState(false);
  const guard = useMemo(() => ({ register: (handler: BeforeLeave) => { handlers.current.push(handler); return () => { handlers.current = handlers.current.filter(value => value !== handler); }; } }), []);
  return <ConfirmProvider><NavigationGuard.Provider value={guard}><button onClick={async () => { try { for (const handler of [...handlers.current].reverse()) await handler(); setLeft(true); } catch {} }}>离开控制台</button>{left ? <p>已离开</p> : <MCP />}</NavigationGuard.Provider></ConfirmProvider>;
}
function mount() { return render(<Harness />); }
async function createDraft(name = '新助手') {
  fireEvent.click(await screen.findByRole('button', { name: '创建连接' }));
  fireEvent.change(screen.getByLabelText('连接名称'), { target: { value: name } });
  return within(screen.getByRole('dialog'));
}
beforeEach(() => {
  records = [connection]; writes = [];
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: vi.fn().mockResolvedValue(undefined) } });
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = String(input), method = init?.method || 'GET';
    if (method === 'GET') return json(url.endsWith('/status') ? { ...status, enabled: records.some(row => row.enabled) } : records);
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    writes.push({ url, method, body });
    if (method === 'POST' && url.endsWith('/connections')) {
      const created = { ...connection, id: 8, ...body }; records = [...records, created];
      return json({ connection: created, token: 'synthetic-one-time-token' }, 201);
    }
    if (method === 'POST' && url.endsWith('/rotate')) return json({ connection, token: 'synthetic-rotated-token' });
    if (method === 'DELETE') { records = []; return new Response(null, { status: 204 }); }
    const updated = { ...connection, ...body }; records = [updated]; return json(updated);
  }));
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('opens the toolbox with MCP disabled, restores focus on close and reuses its loaded catalogue', async () => {
  records = [];
  const original = vi.mocked(fetch).getMockImplementation()!;
  const tools = [{ name: 'get_quota_snapshots', capability: 'quota.read', description: '读取全部凭证额度。', permission: 'read', read_only: true }];
  vi.mocked(fetch).mockImplementation((input, init) => String(input).endsWith('/tools') ? Promise.resolve(json(tools)) : original(input, init));
  mount(); await screen.findByText('未启用');
  const trigger = screen.getByRole('button', { name: '查看MCP功能工具箱' });
  trigger.focus(); fireEvent.click(trigger);
  await screen.findByRole('heading', { name: '查看全部凭证额度' });
  fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(document.activeElement).toBe(trigger);
  fireEvent.click(trigger);
  expect(screen.getByRole('heading', { name: '查看全部凭证额度' })).toBeTruthy();
  expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/tools'))).toHaveLength(1);
  expect(writes).toEqual([]);
});

it('explains the complete setup path and generates client-specific templates from the actual gateway', async () => {
  records = []; mount();
  await screen.findByText('还没有管理连接');
  expect(screen.getByRole('button', { name: '通用指引' }).getAttribute('aria-pressed')).toBe('true');
  fireEvent.click(screen.getByRole('button', { name: 'Codex' }));
  expect(screen.getByText('发一句话，确认真正连通')).toBeTruthy();
  expect(screen.getAllByText(/49123/).length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole('button', { name: 'ZCode' }));
  fireEvent.click(screen.getByText('查看用于界面导入的 JSON'));
  fireEvent.click(screen.getByText('复制配置模板'));
  await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledOnce());
  const value = JSON.parse(vi.mocked(navigator.clipboard.writeText).mock.calls[0][0]);
  expect(value.mcpServers['to-router']).toEqual({ type: 'http', url: status.endpoint, headers: { Authorization: 'Bearer <管理连接密钥>' } });
  fireEvent.click(screen.getByRole('button', { name: '通用指引' }));
  expect(screen.queryByRole('button', { name: '复制配置模板' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'ZCode' }));
  expect(screen.getByRole('button', { name: '自己配置' }).getAttribute('aria-pressed')).toBe('true');
});

it('defaults to daily permissions, grants maintenance separately, and clears the one-time token after collection', async () => {
  const stored = vi.spyOn(Storage.prototype, 'setItem'); mount();
  await screen.findByRole('article', { name: connection.name });
  const editor = await createDraft();
  expect(editor.getByRole('checkbox', { name: /查看/ })).toHaveProperty('checked', true);
  expect(editor.getByRole('checkbox', { name: /日常管理/ })).toHaveProperty('checked', true);
  expect(editor.getByRole('checkbox', { name: /维护/ })).toHaveProperty('checked', false);
  expect(editor.getByRole('checkbox', { name: /敏感读取/ })).toHaveProperty('checked', false);
  fireEvent.click(editor.getByRole('checkbox', { name: /维护/ }));
  fireEvent.click(editor.getByRole('button', { name: '创建连接' }));
  const token = await screen.findByLabelText('管理连接密钥');
  expect(token).toHaveProperty('value', 'synthetic-one-time-token');
  expect(writes[0].body).toEqual({ name: '新助手', permissions: ['read', 'manage', 'maintain'] });
  fireEvent.click(screen.getByRole('button', { name: '仅复制密钥' }));
  await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledWith('synthetic-one-time-token'));
  expect(stored).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: '完成' }));
  expect(screen.queryByLabelText('管理连接密钥')).toBeNull();
  expect(document.body.textContent).not.toContain('synthetic-one-time-token');
});

it('copies token-free agent instructions normally, and includes only the newly issued key on an explicit copy action', async () => {
  const stored = vi.spyOn(Storage.prototype, 'setItem'); mount();
  await screen.findByRole('article', { name: connection.name });
  fireEvent.click(screen.getByRole('button', { name: '交给 Agent' }));
  fireEvent.click(screen.getByRole('button', { name: '复制给 Agent' }));
  await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledOnce());
  let copied = vi.mocked(navigator.clipboard.writeText).mock.calls.slice(-1)[0][0];
  expect(copied).toContain('本说明未包含密钥');
  expect(copied).toContain(status.endpoint);
  expect(copied).toContain('gateway_overview');
  expect(copied).toContain('请先判断我当前使用的 AI 工具能否接入');
  expect(copied).not.toContain('[mcp_servers.to-router]');
  expect(copied).toContain('TOROUTER_MCP_URL');
  const editor = await createDraft('ZCode');
  fireEvent.click(editor.getByRole('button', { name: '创建连接' }));
  await screen.findByLabelText('管理连接密钥');
  const secretDialog = within(screen.getByRole('dialog'));
  fireEvent.click(secretDialog.getByRole('button', { name: 'ZCode' }));
  fireEvent.click(secretDialog.getByRole('button', { name: '复制给 Agent（含密钥）' }));
  await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledTimes(2));
  copied = vi.mocked(navigator.clipboard.writeText).mock.calls.slice(-1)[0][0];
  expect(copied).toContain('synthetic-one-time-token');
  expect(copied).toContain('mcp.servers["to-router"]');
  expect(copied).toContain('不要在回复、日志或命令回显中展示');
  expect(stored).not.toHaveBeenCalled();
  fireEvent.click(secretDialog.getByRole('button', { name: '完成' }));
  fireEvent.click(screen.getByRole('button', { name: '复制给 Agent' }));
  await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledTimes(3));
  expect(vi.mocked(navigator.clipboard.writeText).mock.calls.slice(-1)[0][0]).not.toContain('synthetic-one-time-token');
  expect(document.body.textContent).not.toContain('synthetic-one-time-token');
});

it('does not offer a working configuration when the reported endpoint has a different origin', async () => {
  const original = vi.mocked(fetch).getMockImplementation()!;
  vi.mocked(fetch).mockImplementation((input, init) => String(input).endsWith('/status') ? Promise.resolve(json({ ...status, endpoint: 'https://other.invalid/mcp' })) : original(input, init));
  mount(); await screen.findByRole('article', { name: connection.name });
  fireEvent.click(screen.getByRole('button', { name: 'Codex' }));
  fireEvent.click(screen.getByRole('button', { name: '交给 Agent' }));
  expect(screen.getByRole('button', { name: '复制给 Agent' })).toHaveProperty('disabled', true);
  expect(screen.queryByText(/other\.invalid/)).toBeNull();
});

it('preserves a rejected edit through exit-save and leaves only after the real retry succeeds', async () => {
  const original = vi.mocked(fetch).getMockImplementation()!;
  let reject = true;
  vi.mocked(fetch).mockImplementation((input, init) => init?.method === 'PATCH' && reject ? Promise.resolve(json({ error: '名称已占用' }, 400)) : original(input, init));
  mount(); fireEvent.click(await screen.findByRole('button', { name: '编辑 桌面助手' }));
  fireEvent.change(screen.getByLabelText('连接名称'), { target: { value: '已修改名称' } });
  fireEvent.click(screen.getByRole('button', { name: '离开控制台' }));
  fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText(/名称已占用/);
  expect(screen.getByLabelText('连接名称')).toHaveProperty('value', '已修改名称'); expect(screen.queryByText('已离开')).toBeNull();
  reject = false;
  fireEvent.click(screen.getByRole('button', { name: '离开控制台' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('已离开');
  expect(writes[0]).toEqual({ url: '/api/mcp/connections/7', method: 'PATCH', body: { name: '已修改名称', permissions: ['read', 'manage'] } });
});

it('lets Escape cancel the exit prompt and only discards on an explicit discard action', async () => {
  mount(); await screen.findByRole('article', { name: connection.name }); await createDraft();
  fireEvent.click(screen.getByRole('button', { name: '离开控制台' }));
  const confirmation = await screen.findByRole('dialog', { name: '保存更改？' });
  fireEvent(confirmation, new Event('cancel', { bubbles: true, cancelable: true }));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
  expect(screen.getByLabelText('连接名称')).toHaveProperty('value', '新助手');
  fireEvent.click(screen.getByRole('button', { name: '离开控制台' })); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
  await screen.findByText('已离开'); expect(writes).toEqual([]);
});

it('keeps a newly created token visible when creating through exit-save instead of navigating past it', async () => {
  mount(); await screen.findByRole('article', { name: connection.name }); await createDraft();
  fireEvent.click(screen.getByRole('button', { name: '离开控制台' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByLabelText('管理连接密钥'); expect(screen.queryByText('已离开')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '离开控制台' }));
  expect(screen.queryByText('已离开')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '完成' })); fireEvent.click(screen.getByRole('button', { name: '离开控制台' }));
  await screen.findByText('已离开');
});

it('retains existing rows during refresh and refresh failure, then recovers without losing the list', async () => {
  mount(); const article = await screen.findByRole('article', { name: connection.name });
  let finish!: (response: Response) => void;
  const original = vi.mocked(fetch).getMockImplementation()!;
  vi.mocked(fetch).mockImplementation((input, init) => String(input).endsWith('/connections') ? new Promise(resolve => { finish = resolve; }) : original(input, init));
  fireEvent.click(screen.getByRole('button', { name: '刷新MCP状态' }));
  expect(screen.getByRole('article', { name: connection.name })).toBe(article);
  await act(async () => finish(json({ error: '暂时无法读取' }, 503)));
  await screen.findByText(/保留上次连接列表/);
  expect(screen.getByRole('article', { name: connection.name })).toBe(article);
  vi.mocked(fetch).mockImplementation(original); fireEvent.click(screen.getByRole('button', { name: '重试' }));
  await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
});

it('updates enabled state and requires confirmation for rotating and revoking a connection', async () => {
  mount(); fireEvent.click(await screen.findByRole('button', { name: '停用 桌面助手' }));
  await screen.findByRole('button', { name: '启用 桌面助手' });
  expect(writes[0].body).toEqual({ enabled: false });
  fireEvent.click(screen.getByText('更多')); fireEvent.click(screen.getByRole('button', { name: '轮换密钥' }));
  expect(writes).toHaveLength(1);
  const confirm = within(await screen.findByRole('dialog', { name: '轮换连接密钥？' })); fireEvent.click(confirm.getByRole('button', { name: '轮换密钥' }));
  expect(await screen.findByLabelText('管理连接密钥')).toHaveProperty('value', 'synthetic-rotated-token');
  const secretDialog = within(screen.getByRole('dialog'));
  fireEvent.click(secretDialog.getByRole('button', { name: 'Claude Code' }));
  fireEvent.click(secretDialog.getByRole('button', { name: '复制更新命令（含密钥）' }));
  await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledOnce());
  const update = vi.mocked(navigator.clipboard.writeText).mock.calls[0][0];
  expect(update).toMatch(/^claude mcp remove --scope user to-router\nclaude mcp add/);
  expect(update).toContain('synthetic-rotated-token');
  fireEvent.click(screen.getByRole('button', { name: '完成' }));
  fireEvent.click(screen.getByText('更多')); fireEvent.click(screen.getByRole('button', { name: '撤销连接' }));
  const revoke = within(await screen.findByRole('dialog', { name: '撤销管理连接？' })); fireEvent.click(revoke.getByRole('button', { name: '撤销连接' }));
  await screen.findByText('还没有管理连接'); expect(writes.map(value => value.method)).toEqual(['PATCH', 'POST', 'DELETE']);
});

it('does not repeat a creation whose response was lost and offers a list check while retaining the draft', async () => {
  const original = vi.mocked(fetch).getMockImplementation()!;
  vi.mocked(fetch).mockImplementation((input, init) => init?.method === 'POST' ? Promise.reject(new TypeError('network lost')) : original(input, init));
  mount(); await screen.findByRole('article', { name: connection.name }); const editor = await createDraft();
  fireEvent.click(editor.getByRole('button', { name: '创建连接' }));
  await screen.findByText(/请求结果尚未确认/);
  expect(screen.getByLabelText('连接名称')).toHaveProperty('value', '新助手');
  expect(within(screen.getByRole('dialog')).getByRole('button', { name: '创建连接' })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('button', { name: '返回连接列表核对' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
});
