// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Channel } from '../api';
import { AddOAuthCredential } from './AddOAuthCredential';

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200 });
const channel = (id: number): Channel => ({ id, name: `Pool ${id}`, credential_type: 'codex_oauth', enabled: true, models: [], endpoints: [], credentials: [], api_key: '', has_key: false });
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done; }); return { promise, resolve }; }
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  localStorage.clear(); sessionStorage.clear();
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('ignores a late poll after changing tabs and resumes the same non-secret flow on return', async () => {
  const late = deferred<Response>(), closed = vi.fn(), changed = vi.fn(), started = vi.fn();
  let flow: string | undefined;
  let polls = 0;
  const fetcher = vi.fn((url: string) => {
    if (url === '/api/settings') return Promise.resolve(json({ oauth_auto_open: false }));
    if (url.includes('/start?')) return Promise.resolve(json({ flow: 'synthetic-flow', method: 'browser', authorization_url: 'https://example.invalid/login' }));
    if (url.includes('/status?')) return ++polls === 1 ? late.promise : Promise.resolve(json({ status: 'complete', channel_id: 8 }));
    throw new Error('unexpected fixture request');
  });
  vi.stubGlobal('fetch', fetcher);
  const props = { channel: channel(8), onClose: closed, onChanged: changed, onStarted: (value: string | null) => { flow = value || undefined; started(value); } };
  const view = render(<AddOAuthCredential {...props} />);
  await waitFor(() => expect(fetcher).toHaveBeenCalledWith('/api/settings', expect.anything()));
  fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
  await waitFor(() => expect(started).toHaveBeenCalledWith('synthetic-flow'));
  view.rerender(<AddOAuthCredential {...props} resumeFlow={flow} />);
  fireEvent.click(screen.getByRole('tab', { name: '粘贴凭证' }));
  fireEvent.change(screen.getByLabelText('粘贴凭证内容'), { target: { value: 'synthetic-private-input' } });
  await act(async () => late.resolve(json({ status: 'complete', channel_id: 8 })));
  expect(closed).not.toHaveBeenCalled(); expect(changed).not.toHaveBeenCalled();
  expect(screen.getByLabelText('粘贴凭证内容')).toHaveProperty('value', 'synthetic-private-input');
  expect(localStorage.length + sessionStorage.length).toBe(0);
  fireEvent.click(screen.getByRole('tab', { name: '官方登录' }));
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(1));
  expect(fetcher.mock.calls.filter(([url]) => url.includes('/start?'))).toHaveLength(1);
});

it('does not close or refresh a new target when a previous target poll completes after closing', async () => {
  const late = deferred<Response>(), closed = vi.fn(), changed = vi.fn();
  vi.stubGlobal('fetch', vi.fn((url: string) => url === '/api/settings' ? Promise.resolve(json({ oauth_auto_open: false })) : late.promise));
  const view = render(<AddOAuthCredential channel={channel(7)} resumeFlow="old-target-flow" onClose={closed} onChanged={changed} />);
  view.rerender(<AddOAuthCredential key="new-target" channel={channel(8)} onClose={closed} onChanged={changed} />);
  fireEvent.click(screen.getByRole('tab', { name: '粘贴凭证' }));
  await act(async () => late.resolve(json({ status: 'complete', channel_id: 7 })));
  expect(screen.getByRole('dialog', { name: '向「Pool 8」添加账号 / 工作区' })).toBeTruthy();
  expect(closed).not.toHaveBeenCalled(); expect(changed).not.toHaveBeenCalled();
});

it('cancels and restarts a flow without accepting the earlier pending poll', async () => {
  const late = deferred<Response>(), changed = vi.fn();
  let starts = 0;
  const fetcher = vi.fn((url: string) => {
    if (url === '/api/settings') return Promise.resolve(json({ oauth_auto_open: false }));
    if (url.includes('/start?')) return Promise.resolve(json({ flow: `synthetic-${++starts}`, method: 'browser', authorization_url: 'https://example.invalid/login' }));
    if (url.includes('/cancel?')) return Promise.resolve(json({ status: 'cancelled' }));
    if (url.includes('/status?flow=synthetic-1')) return late.promise;
    if (url.includes('/status?flow=synthetic-2')) return Promise.resolve(json({ status: 'pending', interval: 60 }));
    throw new Error('unexpected fixture request');
  });
  vi.stubGlobal('fetch', fetcher);
  render(<AddOAuthCredential channel={channel(8)} onClose={vi.fn()} onChanged={changed} />);
  await waitFor(() => expect(fetcher).toHaveBeenCalledWith('/api/settings', expect.anything()));
  fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
  await screen.findByRole('button', { name: '取消授权' });
  fireEvent.click(screen.getByRole('button', { name: '取消授权' }));
  await screen.findByRole('button', { name: '浏览器授权' });
  fireEvent.click(screen.getByRole('button', { name: '浏览器授权' }));
  await waitFor(() => expect(starts).toBe(2));
  await act(async () => late.resolve(json({ status: 'complete', channel_id: 8 })));
  expect(changed).not.toHaveBeenCalled();
  expect(screen.getByRole('button', { name: '取消授权' })).toBeTruthy();
});

it('links a duplicate identity to its saved location instead of claiming addition succeeded', async () => {
  const changed = vi.fn();
  vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(json(url === '/api/settings' ? { oauth_auto_open: false } : { status: 'error', code: 'already_linked', message: '该授权已在其他渠道中保存。', existing_channel_id: 7, existing_credential_id: 'synthetic-member' }))));
  render(<AddOAuthCredential channel={channel(8)} resumeFlow="duplicate-flow" onClose={vi.fn()} onChanged={changed} />);
  const link = await screen.findByRole('link', { name: '前往已有凭证' });
  expect(link.getAttribute('href')).toBe('#/channels?channel_id=7&credential_id=synthetic-member');
  expect(changed).not.toHaveBeenCalled();
});

it('hides pasted token material by default, releases it on tab change and only names supported sources', async () => {
  vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(json(url === '/api/settings' ? { oauth_auto_open: false } : { sources: [{ id: 'source-codex', name: 'Codex', enabled: true }, { id: 'source-copilot', name: 'Copilot', enabled: true }, { id: 'source-ccswitch', name: 'CC Switch', enabled: true }] }))));
  render(<AddOAuthCredential channel={channel(8)} onClose={vi.fn()} onChanged={vi.fn()} />);
  fireEvent.click(screen.getByRole('tab', { name: '粘贴凭证' }));
  const input = screen.getByLabelText('粘贴凭证内容');
  fireEvent.change(input, { target: { value: 'synthetic-private-token' } });
  expect(input.getAttribute('style')).toContain('disc');
  fireEvent.click(screen.getByRole('button', { name: '显示凭证内容' }));
  expect(input.getAttribute('style') || '').not.toContain('disc');
  fireEvent.click(screen.getByRole('tab', { name: '从本地官方工具导入' }));
  await screen.findByText('Codex'); expect(screen.getByText('CC Switch')).toBeTruthy();
  expect(screen.queryByText('Copilot')).toBeNull();
  expect(screen.queryByText(/apps.json/)).toBeNull();
  fireEvent.click(screen.getByRole('tab', { name: '粘贴凭证' }));
  expect(screen.getByLabelText('粘贴凭证内容')).toHaveProperty('value', '');
  expect(localStorage.length + sessionStorage.length).toBe(0);
});
