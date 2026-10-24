// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Channel } from '../api';
import { ConfirmProvider } from './Confirm';
import { PlanDeclaration } from './PlanDeclaration';
import { ChannelPicker } from './ChannelPicker';

const channels = [{ id: 1, name: '套餐渠道', tags: ['billing:plan', 'privacy:private'], model_tags: { own: ['billing:payg'] }, endpoints: [] }, { id: 2, name: '按量渠道', tags: ['billing:payg'], endpoints: [] }] as unknown as Channel[];
beforeEach(() => { history.replaceState(null, '', '/#/prices'); HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); }; HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); }; });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
function setup() { const saved = vi.fn(), closed = vi.fn(), fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ ok: true }))); vi.stubGlobal('fetch', fetcher); render(<ConfirmProvider><PlanDeclaration channels={channels} onSaved={saved} onClose={closed} /></ConfirmProvider>); return { saved, closed, fetcher }; }

it('requires a replacement billing value and restores the opening snapshot', () => {
  setup(); fireEvent.click(screen.getByLabelText('声明套餐 套餐渠道'));
  expect(screen.getByRole('dialog', { name: '转出套餐' })).toBeTruthy(); fireEvent.click(screen.getByText('取消'));
  expect(screen.getByLabelText('声明套餐 套餐渠道').getAttribute('aria-pressed')).toBe('true');
  fireEvent.click(screen.getByLabelText('声明套餐 套餐渠道')); fireEvent.click(screen.getByText('免费'));
  fireEvent.click(screen.getByLabelText('声明套餐 按量渠道'));
  expect(screen.getByLabelText('声明套餐 按量渠道').getAttribute('aria-pressed')).toBe('true');
  fireEvent.click(screen.getByText('恢复修改前'));
  expect(screen.getByLabelText('声明套餐 套餐渠道').getAttribute('aria-pressed')).toBe('true'); expect(screen.getByLabelText('声明套餐 按量渠道').getAttribute('aria-pressed')).toBe('false');
});
it('sends only changed billing groups and keeps rejected drafts for retry', async () => {
  const state = setup(); state.fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ error: '保存失败' }), { status: 409 }));
  fireEvent.click(screen.getByLabelText('声明套餐 按量渠道')); fireEvent.click(screen.getByText('确认保存声明'));
  await screen.findByText('保存失败'); expect(state.closed).not.toHaveBeenCalled(); expect(screen.getByLabelText('声明套餐 按量渠道').getAttribute('aria-pressed')).toBe('true');
  expect(JSON.parse(state.fetcher.mock.calls[0][1].body)).toEqual({ changes: [{ channel_id: 2, billing_tag: 'billing:plan' }] });
  fireEvent.click(screen.getByText('确认保存声明')); await waitFor(() => expect(state.closed).toHaveBeenCalledOnce()); expect(state.saved).toHaveBeenCalledOnce();
});
it('guards channel navigation, dismissing the exit prompt keeps the draft and does not toggle the destination', async () => {
  const state = setup(); fireEvent.click(screen.getByLabelText('声明套餐 按量渠道')); fireEvent.click(screen.getByLabelText('编辑渠道 套餐渠道'));
  const prompt = await screen.findByRole('dialog', { name: '保存更改？' }); fireEvent.click(within(prompt).getByLabelText('关闭弹窗'));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: '保存更改？' })).toBeNull()); expect(location.hash).toBe('#/prices'); expect(state.closed).not.toHaveBeenCalled();
  expect(screen.getByLabelText('声明套餐 套餐渠道').getAttribute('aria-pressed')).toBe('true');
  fireEvent.click(screen.getByLabelText('编辑渠道 套餐渠道')); fireEvent.click(await screen.findByText('放弃更改'));
  await waitFor(() => expect(location.hash).toBe('#/channels?channel_id=1')); expect(state.fetcher).not.toHaveBeenCalled();
});
it('channel selection changes only the local display scope and navigation has its own hitbox', () => {
  const change = vi.fn(), fetcher = vi.fn(); vi.stubGlobal('fetch', fetcher);
  render(<ChannelPicker channels={channels} selected={[1]} onChange={change} onClose={() => {}} />);
  fireEvent.click(screen.getByLabelText('选择渠道 按量渠道')); expect(change).toHaveBeenLastCalledWith([1, 2]);
  fireEvent.click(screen.getByText('清除选择')); expect(change).toHaveBeenLastCalledWith([]);
  change.mockClear(); fireEvent.click(screen.getByLabelText('编辑渠道 套餐渠道')); expect(change).not.toHaveBeenCalled(); expect(fetcher).not.toHaveBeenCalled();
});
