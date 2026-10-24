// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import type { ClientKeyScope } from '../api';
import ClientAccess from './ClientAccess';
const unrestricted: ClientKeyScope = { model_mode: 'all', model_ids: [], tag_rules: {} };
const groups = [{ id: 'billing', name: '计费', type: 'builtin', tags: ['unknown', 'free', 'plan'].map(id => ({ id: `billing:${id}`, group_id: 'billing', name: id, color: 'slate' })) }];
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
function setup(initial = unrestricted) {
  let fail = false;
  const fetcher = vi.fn((url: string, init?: RequestInit): Promise<Response> => {
    if (url === '/api/client-keys/preview') return Promise.resolve(json({ allowed_models: 1, total_models: 2, models: [{ model_id: 'public-a', allowed: false, reason: '未在允许范围', bindings: [{ channel_id: 1, channel_name: '测试渠道', model_id: 'upstream', allowed: false, reasons: ['计费条件不符'] }] }] }));
    if (url === '/api/client-keys/7/key') return Promise.resolve(json({ key: 'synthetic-existing-key' }));
    if (init?.method === 'PUT') return Promise.resolve(fail ? json({ error: 'scope rejected' }, 409) : json({ id: 7, name: 'Application', scope: JSON.parse(String(init.body)).scope }));
    if (init?.method === 'POST') return Promise.resolve(json({ key: 'synthetic-new-key', client_key: { name: 'New', scope: JSON.parse(String(init.body)).scope } }));
    if (url === '/api/client-keys') return Promise.resolve(json([{ id: 7, name: 'Application', prefix: 'sk-test', created_at: '', last_used_at: null, scope: initial }]));
    if (url === '/api/tag-groups') return Promise.resolve(json(groups));
    if (url === '/api/models') return Promise.resolve(json([{ id: 'public-a', enabled: true }, { id: 'public-b', enabled: true }]));
    return Promise.resolve(json({ origin: 'http://localhost:17878', port: 17878 }));
  });
  vi.stubGlobal('fetch', fetcher);
  const clipboard = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: clipboard } });
  render(<ConfirmProvider><ClientAccess /></ConfirmProvider>);
  return { fetcher, clipboard, fail: (value: boolean) => { fail = value; } };
}
beforeEach(() => { HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); }; HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); }; });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('keeps selected-empty and restricted-empty distinct and preserves the existing key when editing', async () => {
  const { fetcher, clipboard } = setup();
  fireEvent.click(await screen.findByRole('button', { name: '编辑访问范围 Application' }));
  fireEvent.click(screen.getByRole('button', { name: '指定公开模型' }));
  expect(screen.getByText('未选择任何公开模型：此 Key 不允许访问任何模型。')).toBeTruthy();
  fireEvent.click(await screen.findByRole('button', { name: '只允许所选' }));
  expect(screen.getByText('此组限制为空：没有上游符合规则。')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '保存访问范围' }));
  await waitFor(() => expect(fetcher.mock.calls.find(([, init]) => init?.method === 'PUT')).toBeTruthy());
  const write = fetcher.mock.calls.find(([, init]) => init?.method === 'PUT')!;
  expect(write[0]).toBe('/api/client-keys/7');
  expect(JSON.parse(String(write[1]?.body))).toEqual({ name: 'Application', scope: { model_mode: 'selected', model_ids: [], tag_rules: { billing: [] } } });
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  fireEvent.click(screen.getByRole('button', { name: '复制密钥 Application' }));
  await waitFor(() => expect(clipboard).toHaveBeenCalledWith('synthetic-existing-key'));
  expect(fetcher.mock.calls.some(([url, init]) => url === '/api/client-keys' && init?.method === 'POST')).toBe(false);
});

it('allows unknown with concrete values in a Key rule and shows server match reasons on demand', async () => {
  const { fetcher } = setup();
  fireEvent.click(await screen.findByRole('button', { name: '编辑访问范围 Application' }));
  fireEvent.click(await screen.findByRole('button', { name: '只允许所选' }));
  fireEvent.click(screen.getByRole('button', { name: 'unknown' })); fireEvent.click(screen.getByRole('button', { name: 'free' }));
  await screen.findByText('1', { selector: '.client-scope-preview strong' });
  expect(screen.getByRole('button', { name: 'unknown' })).toHaveProperty('ariaPressed', 'true');
  expect(screen.getByRole('button', { name: 'free' })).toHaveProperty('ariaPressed', 'true');
  fireEvent.click(screen.getByText('查看模型与不匹配原因'));
  expect(screen.getByText('测试渠道 · upstream：计费条件不符')).toBeTruthy();
  const previews = fetcher.mock.calls.filter(([url]) => url.endsWith('/preview'));
  expect(JSON.parse(String(previews[previews.length - 1][1]?.body)).scope.tag_rules).toEqual({ billing: ['billing:unknown', 'billing:free'] });
});

it('retains a scope draft after save rejection and only removes a restriction explicitly', async () => {
  const view = setup({ model_mode: 'selected', model_ids: ['removed-model'], tag_rules: { billing: ['billing:free'] } }); view.fail(true);
  fireEvent.click(await screen.findByRole('button', { name: '编辑访问范围 Application' }));
  expect(await screen.findByText('removed-model（已移除）')).toBeTruthy();
  fireEvent.click(await screen.findByRole('button', { name: 'free' }));
  fireEvent.click(screen.getByRole('button', { name: '保存访问范围' }));
  await screen.findByText('scope rejected');
  expect(screen.getByText('此组限制为空：没有上游符合规则。')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '取消' }));
  fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
  fireEvent.click(screen.getByRole('button', { name: '不限制' })); view.fail(false);
  fireEvent.click(screen.getByRole('button', { name: '保存访问范围' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  const writes = view.fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT');
  expect(JSON.parse(String(writes[writes.length - 1][1]?.body)).scope).toEqual({ model_mode: 'selected', model_ids: ['removed-model'], tag_rules: {} });
});

it('ignores an older preview after scope changes and preserves draft values on late reads', async () => {
  const view = setup(); const base = view.fetcher.getMockImplementation()!;
  let resolve!: (response: Response) => void, held = false;
  view.fetcher.mockImplementation((url, init) => url.endsWith('/preview') && !held ? (held = true, new Promise(done => { resolve = done; })) : base(url, init));
  fireEvent.click(await screen.findByRole('button', { name: '编辑访问范围 Application' }));
  await waitFor(() => expect(held).toBe(true));
  fireEvent.click(screen.getByRole('button', { name: '指定公开模型' }));
  await screen.findByText('1', { selector: '.client-scope-preview strong' });
  await act(async () => { resolve(json({ allowed_models: 99, total_models: 99, models: [] })); });
  expect(screen.queryByText('99', { selector: '.client-scope-preview strong' })).toBeNull();
  expect(screen.getByText('未选择任何公开模型：此 Key 不允许访问任何模型。')).toBeTruthy();
});
