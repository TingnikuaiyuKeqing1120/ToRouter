// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import ModelSpecs from './ModelSpecs';

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
let fetcher: ReturnType<typeof vi.fn>;
beforeEach(() => {
  localStorage.clear();
  history.replaceState(null, '', '/#/model-specs');
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/channels') return Promise.resolve(json([{ id: 12, name: 'Paused channel', enabled: false, models: ['enabled-manual'], available_models: ['disabled-discovered'] }]));
    if (url === '/api/model-specs') return Promise.resolve(json(['enabled-manual', 'disabled-discovered'].map(model => ({ channel_id: 12, channel_name: 'Paused channel', model, specs: {} }))));
    if (url === '/api/catalog/prefill/preview') return Promise.resolve(json({ request: JSON.parse(String(init?.body)), patches: [], updated: 0, skipped: 0 }));
    return Promise.resolve(json([]));
  });
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); history.replaceState(null, '', '/'); });

it('limits specs and selected batch targets to enabled models even when the channel is paused', async () => {
  render(<ModelSpecs />, { wrapper: ConfirmProvider });
  await screen.findByRole('button', { name: '编辑规格 12:enabled-manual' });
  expect(screen.queryByRole('button', { name: '编辑规格 12:disabled-discovered' })).toBeNull();
  expect(screen.queryByRole('checkbox', { name: '查看全部模型' })).toBeNull();
  expect(screen.getByRole('button', { name: /^一键匹配/ })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('checkbox', { name: '选择模型 12:enabled-manual' }));
  fireEvent.click(screen.getByRole('button', { name: /^一键匹配/ }));
  await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url === '/api/catalog/prefill/preview')).toBe(true));
  const request = JSON.parse(String(fetcher.mock.calls.find(([url]) => url === '/api/catalog/prefill/preview')![1]?.body));
  expect(request.targets.map((target: { model: string }) => target.model)).toEqual(['enabled-manual']);
  expect(fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
});

it('honors a direct link to a model that was subsequently disabled', async () => {
  history.replaceState(null, '', '/#/model-specs?channel_id=12&model=disabled-discovered');
  render(<ModelSpecs />, { wrapper: ConfirmProvider });
  expect(await screen.findByText('目标模型 disabled-discovered 尚未启用，已保留其规格。')).toBeTruthy();
  expect(screen.getByRole('link', { name: '前往上游接入启用' }).getAttribute('href')).toBe('#/channels');
  expect(screen.queryByRole('button', { name: '编辑规格 12:disabled-discovered' })).toBeNull();
  expect(screen.getByRole('button', { name: /^一键匹配/ })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByRole('button', { name: '清除搜索' }));
  expect(await screen.findByRole('button', { name: '编辑规格 12:enabled-manual' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: '编辑规格 12:disabled-discovered' })).toBeNull();
  expect(fetcher.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
});

it('reports candidate loading failure instead of silently presenting an empty model set', async () => {
  const base = fetcher.getMockImplementation()!;
  fetcher.mockImplementation((url, init) => url === '/api/channels' ? Promise.resolve(json({ error: '渠道读取失败' }, 500)) : base(url, init));
  render(<ModelSpecs />, { wrapper: ConfirmProvider });
  await screen.findByText('渠道读取失败');
  expect(screen.queryByText('暂无匹配的上游模型')).toBeNull();
  expect(screen.getByRole('button', { name: /^一键匹配/ })).toHaveProperty('disabled', true);
});
