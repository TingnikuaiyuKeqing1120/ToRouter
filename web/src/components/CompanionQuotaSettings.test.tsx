// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { CompanionQuotaSettings } from './CompanionQuotaSettings';
import { ConfirmProvider } from './Confirm';
import { NavigationGuard, type BeforeLeave } from './NavigationGuard';
const target = { channel_id: 8, credential_id: 'fixture-primary' };
const second = { ...target, credential_id: 'fixture-secondary' };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
function backend() {
  let preferences = { theme: 'light', paused: true, revision: 1, quota_targets: [] as typeof target[] };
  let reject = false;
  const fetcher = vi.fn((url: string, options: RequestInit = {}) => {
    if (url.endsWith('/quotas')) return Promise.resolve(json({ items: [target, second].map((target, i) => ({ ...target, channel_name: '示例渠道', credential_name: i ? '备用凭证' : '工作凭证', kind: 'balance', enabled: true, balances: [], windows: [], stale: true })), refresh_seconds: 60 }));
    if (options.method === 'PUT') {
      if (reject) return Promise.resolve(json({ error: '合成保存失败' }, 503));
      preferences = { ...preferences, ...JSON.parse(String(options.body)), revision: preferences.revision + 1 };
    }
    return Promise.resolve(json(preferences));
  });
  return { fetcher, reject: (value: boolean) => { reject = value; }, external: () => { preferences = { ...preferences, quota_targets: [second], revision: preferences.revision + 1 }; } };
}
beforeEach(() => { vi.spyOn(document, 'hidden', 'get').mockReturnValue(false); Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' }); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
it('starts empty, cancels locally, saves only exact targets and reflects saved selection in a new window', async () => {
  const server = backend(); vi.stubGlobal('fetch', server.fetcher);
  const view = render(<CompanionQuotaSettings />, { wrapper: ConfirmProvider });
  const checkbox = await screen.findByRole('checkbox', { name: '关注 示例渠道 工作凭证' });
  await waitFor(() => expect(checkbox).toHaveProperty('disabled', false));
  expect(checkbox).toHaveProperty('checked', false);
  fireEvent.click(checkbox); fireEvent.click(screen.getByRole('button', { name: '取消更改' }));
  expect(checkbox).toHaveProperty('checked', false); expect(server.fetcher.mock.calls.some(([, options]) => options?.method === 'PUT')).toBe(false);
  fireEvent.click(checkbox); fireEvent.click(screen.getByRole('button', { name: '保存关注' }));
  await waitFor(() => expect(screen.getByRole('button', { name: '保存关注' })).toHaveProperty('disabled', true));
  const write = server.fetcher.mock.calls.find(([, options]) => options?.method === 'PUT')!;
  expect(JSON.parse(String(write[1]!.body))).toEqual({ quota_targets: [target] });
  view.unmount(); render(<CompanionQuotaSettings />, { wrapper: ConfirmProvider });
  await waitFor(() => expect(screen.getByRole('checkbox', { name: '关注 示例渠道 工作凭证' })).toHaveProperty('checked', true));
  expect(server.fetcher.mock.calls.some(([url]) => url.endsWith('/refresh'))).toBe(false);
});
it('retains selection after rejected save and allows retry without overwriting theme or paused', async () => {
  const server = backend(); server.reject(true); vi.stubGlobal('fetch', server.fetcher);
  render(<CompanionQuotaSettings />, { wrapper: ConfirmProvider });
  const checkbox = await screen.findByRole('checkbox', { name: '关注 示例渠道 工作凭证' });
  await waitFor(() => expect(checkbox).toHaveProperty('disabled', false));
  fireEvent.click(checkbox); fireEvent.click(screen.getByRole('button', { name: '保存关注' }));
  await screen.findByText('合成保存失败');
  expect(checkbox).toHaveProperty('checked', true); expect(screen.getByRole('button', { name: '保存关注' })).toHaveProperty('disabled', false);
  server.reject(false); fireEvent.click(screen.getByRole('button', { name: '保存关注' }));
  await waitFor(() => expect(screen.getByRole('button', { name: '保存关注' })).toHaveProperty('disabled', true));
  for (const [, options] of server.fetcher.mock.calls.filter(([, options]) => options?.method === 'PUT')) expect(JSON.parse(String(options!.body))).toEqual({ quota_targets: [target] });
});
it('receives cross-window changes while preserving an unsaved local draft', async () => {
  vi.useFakeTimers(); const server = backend(); vi.stubGlobal('fetch', server.fetcher);
  render(<CompanionQuotaSettings />, { wrapper: ConfirmProvider }); await act(async () => {});
  const primary = screen.getByRole('checkbox', { name: '关注 示例渠道 工作凭证' });
  fireEvent.click(primary); server.external(); await act(async () => vi.advanceTimersByTimeAsync(1100));
  expect(primary).toHaveProperty('checked', true);
  expect(screen.getByRole('checkbox', { name: '关注 示例渠道 备用凭证' })).toHaveProperty('checked', false);
  fireEvent.click(screen.getByRole('button', { name: '取消更改' }));
  expect(primary).toHaveProperty('checked', false);
  expect(screen.getByRole('checkbox', { name: '关注 示例渠道 备用凭证' })).toHaveProperty('checked', true);
});
it('keeps the draft when the user cancels the existing leave confirmation', async () => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  const server = backend(); vi.stubGlobal('fetch', server.fetcher);
  let leave!: BeforeLeave;
  render(<ConfirmProvider><NavigationGuard.Provider value={{ register: handler => { leave = handler; return () => {}; } }}><CompanionQuotaSettings /></NavigationGuard.Provider></ConfirmProvider>);
  const checkbox = await screen.findByRole('checkbox', { name: '关注 示例渠道 工作凭证' });
  await waitFor(() => expect(checkbox).toHaveProperty('disabled', false)); fireEvent.click(checkbox);
  let decision!: Promise<boolean>;
  await act(async () => { decision = leave().then(() => true, () => false); });
  fireEvent.click(await screen.findByRole('button', { name: '关闭弹窗' }));
  await expect(decision).resolves.toBe(false);
  expect(checkbox).toHaveProperty('checked', true);
  expect(server.fetcher.mock.calls.some(([, options]) => options?.method === 'PUT')).toBe(false);
});
