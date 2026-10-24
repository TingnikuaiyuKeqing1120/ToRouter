// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ConfirmProvider } from '../components/Confirm';
import { useUnsavedChanges } from './useUnsavedChanges';

function Editor({ save, discard, busy = false }: { save: () => Promise<boolean>; discard: () => void; busy?: boolean }) {
  const canLeave = useUnsavedChanges({ dirty: true, busy, save, discard });
  return <button onClick={() => void canLeave().then(allowed => { if (allowed) document.body.dataset.left = 'yes'; })}>离开</button>;
}
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
});
afterEach(() => { cleanup(); delete document.body.dataset.left; });

it('waits for the real save result, prevents duplicate exits and leaves only after success', async () => {
  let finish!: (success: boolean) => void;
  const save = vi.fn(() => new Promise<boolean>(resolve => { finish = resolve; })), discard = vi.fn();
  render(<ConfirmProvider><Editor save={save} discard={discard} /></ConfirmProvider>);
  fireEvent.click(screen.getByText('离开'));
  fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(save).toHaveBeenCalledOnce());
  fireEvent.click(screen.getByText('离开'));
  expect(screen.queryByRole('dialog')).toBeNull(); expect(document.body.dataset.left).toBeUndefined();
  await act(async () => finish(true));
  expect(document.body.dataset.left).toBe('yes'); expect(discard).not.toHaveBeenCalled();
});

it('keeps the editor after a failed save and dismissing the prompt, and discards only explicitly', async () => {
  const save = vi.fn().mockResolvedValue(false), discard = vi.fn();
  render(<ConfirmProvider><Editor save={save} discard={discard} /></ConfirmProvider>);
  fireEvent.click(screen.getByText('离开')); fireEvent.click(await screen.findByRole('button', { name: '确认保存' }));
  await waitFor(() => expect(save).toHaveBeenCalledOnce()); expect(document.body.dataset.left).toBeUndefined();
  fireEvent.click(screen.getByText('离开'));
  fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '关闭弹窗' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(discard).not.toHaveBeenCalled(); expect(document.body.dataset.left).toBeUndefined();
  fireEvent.click(screen.getByText('离开')); fireEvent.click(await screen.findByRole('button', { name: '放弃更改' }));
  await waitFor(() => expect(document.body.dataset.left).toBe('yes')); expect(discard).toHaveBeenCalledOnce();
});

it('protects browser unload and blocks exit while saving', async () => {
  const save = vi.fn(), discard = vi.fn();
  const view = render(<ConfirmProvider><Editor save={save} discard={discard} busy /></ConfirmProvider>);
  const unload = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(unload); expect(unload.defaultPrevented).toBe(true);
  fireEvent.click(screen.getByText('离开')); await act(async () => {});
  expect(screen.queryByRole('dialog')).toBeNull(); expect(save).not.toHaveBeenCalled();
  view.unmount(); const after = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(after); expect(after.defaultPrevented).toBe(false);
});
