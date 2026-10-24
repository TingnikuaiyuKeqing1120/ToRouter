// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import { NavigationGuard, type BeforeLeave } from '../components/NavigationGuard';
import ClientAccess from './ClientAccess';

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
function setup() {
  let fail = false, leave: BeforeLeave | undefined, left = false;
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (init?.method === 'POST') return Promise.resolve(fail ? json({ error: 'create rejected' }, 409) : json({ key: 'synthetic-new-key', client_key: { name: 'Draft application' } }));
    return Promise.resolve(json(url === '/api/connection' ? { origin: 'http://localhost:17878', port: 17878 } : []));
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ConfirmProvider><NavigationGuard.Provider value={{ register: handler => { leave = handler; return () => {}; } }}><button onClick={() => void leave?.().then(() => { left = true; }).catch(() => {})}>导航离开</button><ClientAccess /></NavigationGuard.Provider></ConfirmProvider>);
  return { fetcher, fail: (value: boolean) => { fail = value; }, left: () => left };
}
async function draft() {
  fireEvent.click(screen.getByRole('button', { name: '创建 API Key' }));
  fireEvent.change(screen.getByPlaceholderText('例如：桌面 Agent'), { target: { value: 'Draft application' } });
  fireEvent.click(screen.getByRole('button', { name: '取消' }));
}

it('keeps a creation draft when exit is dismissed and shows the generated key before allowing navigation', async () => {
  const view = setup(); await draft();
  fireEvent.click(within(await screen.findByRole('dialog', { name: '保存更改？' })).getByRole('button', { name: '关闭弹窗' }));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull());
  expect(screen.getByPlaceholderText('例如：桌面 Agent')).toHaveProperty('value', 'Draft application');
  expect(view.fetcher.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: '导航离开' })); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  expect(await screen.findByLabelText('新客户端 API Key')).toHaveProperty('value', 'synthetic-new-key');
  expect(view.left()).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: '已保存，关闭' }));
  fireEvent.click(screen.getByRole('button', { name: '导航离开' })); await waitFor(() => expect(view.left()).toBe(true));
  expect(view.fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
});

it('retains the name after an exit creation error and discards it only explicitly', async () => {
  const view = setup(); view.fail(true); await draft();
  fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await screen.findByText('create rejected'); expect(screen.getByPlaceholderText('例如：桌面 Agent')).toHaveProperty('value', 'Draft application');
  fireEvent.click(screen.getByRole('button', { name: '取消' })); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
});
