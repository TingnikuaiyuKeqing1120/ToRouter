// @vitest-environment jsdom
import { useState } from 'react';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfirmProvider, useConfirm, useSaveOrDiscard } from './Confirm';
import { Button, Modal } from './ui';

function Request({ answer, repeat = false }: { answer: (value: boolean) => void; repeat?: boolean }) {
  const confirm = useConfirm();
  return <Button onClick={() => { void confirm({ title: '删除测试项目？', description: '将删除所选项目。', confirmLabel: '确认删除' }).then(answer); if (repeat) void confirm({ title: '删除测试项目？', description: '将删除所选项目。', confirmLabel: '确认删除' }).then(answer); }}>关闭编辑器</Button>;
}
function SaveRequest({ answer }: { answer: (value: string) => void }) {
  const ask = useSaveOrDiscard();
  return <Button onClick={() => void ask().then(answer)}>退出渠道编辑</Button>;
}
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.setAttribute('open', ''); });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.removeAttribute('open'); });
});

describe('save or discard confirmation', () => {
  it.each([['确认保存', 'save'], ['放弃更改', 'confirm']])('returns %s as its own decision without submitting a form', async (label, decision) => {
    const answer = vi.fn(), submit = vi.fn(event => event.preventDefault());
    render(<ConfirmProvider><form onSubmit={submit}><SaveRequest answer={answer} /></form></ConfirmProvider>);
    fireEvent.click(screen.getByRole('button', { name: '退出渠道编辑' }));
    const dialog = await screen.findByRole('dialog', { name: '保存更改？' });
    expect(within(dialog).queryByRole('button', { name: '继续编辑' })).toBeNull();
    expect(document.activeElement).toBe(within(dialog).getByRole('button', { name: '确认保存' }));
    fireEvent.click(within(dialog).getByRole('button', { name: label }));
    await waitFor(() => expect(answer.mock.calls).toEqual([[decision]]));
    expect(submit).not.toHaveBeenCalled();
  });

  it.each(['close', 'escape', 'backdrop'])('continues editing through %s without saving or discarding', async method => {
    const answer = vi.fn(); render(<ConfirmProvider><SaveRequest answer={answer} /></ConfirmProvider>);
    const opener = screen.getByRole('button', { name: '退出渠道编辑' }); opener.focus(); fireEvent.click(opener);
    const dialog = await screen.findByRole('dialog', { name: '保存更改？' });
    if (method === 'close') fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
    if (method === 'escape') { const event = new Event('cancel', { cancelable: true }); fireEvent(dialog, event); expect(event.defaultPrevented).toBe(true); }
    if (method === 'backdrop') fireEvent.click(dialog);
    await waitFor(() => expect(answer.mock.calls).toEqual([['cancel']]));
    expect(document.activeElement).toBe(opener);
  });

  it('settles an unmounted requester as cancel even if the user subsequently selects save', async () => {
    const answer = vi.fn();
    const view = render(<ConfirmProvider><SaveRequest answer={answer} /></ConfirmProvider>);
    fireEvent.click(screen.getByRole('button', { name: '退出渠道编辑' }));
    view.rerender(<ConfirmProvider>{null}</ConfirmProvider>);
    fireEvent.click(screen.getByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(answer.mock.calls).toEqual([['cancel']]));
  });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('shared confirmation', () => {
  it.each(['safe', 'close', 'escape', 'backdrop'])('cancels with %s, defaults focus to the safe action and restores focus in an underlying dialog', async method => {
    const answer = vi.fn(); const outerClose = vi.fn();
    render(<ConfirmProvider><Modal title="编辑模型" onClose={outerClose}><Request answer={answer} /></Modal></ConfirmProvider>);
    const opener = screen.getByRole('button', { name: '关闭编辑器' }); opener.focus(); fireEvent.click(opener);
    const dialog = await screen.findByRole('dialog', { name: '删除测试项目？' });
    const safe = within(dialog).getByRole('button', { name: '取消' });
    expect(document.activeElement).toBe(safe);
    expect(HTMLDialogElement.prototype.showModal).toHaveBeenCalledTimes(2);
    expect(screen.getByRole('dialog', { name: '编辑模型' })).toHaveProperty('open', true);
    if (method === 'safe') fireEvent.click(safe);
    if (method === 'close') fireEvent.click(within(dialog).getByRole('button', { name: '关闭弹窗' }));
    // Browsers translate Escape on the top-layer dialog into its cancel event.
    if (method === 'escape') { const event = new Event('cancel', { cancelable: true }); fireEvent(dialog, event); expect(event.defaultPrevented).toBe(true); }
    if (method === 'backdrop') fireEvent.click(dialog);
    await waitFor(() => expect(answer.mock.calls).toEqual([[false]]));
    expect(document.activeElement).toBe(opener); expect(outerClose).not.toHaveBeenCalled();
  });

  it('resolves duplicate requests as false and executes the accepted action only once without submitting a form', async () => {
    const answer = vi.fn(); const submit = vi.fn(event => event.preventDefault());
    render(<ConfirmProvider><form onSubmit={submit}><Request answer={answer} repeat /></form></ConfirmProvider>);
    fireEvent.click(screen.getByRole('button', { name: '关闭编辑器' }));
    await waitFor(() => expect(answer.mock.calls).toEqual([[false]]));
    const discard = screen.getByRole('button', { name: '确认删除' });
    expect(discard).toHaveProperty('type', 'button');
    fireEvent.click(discard); fireEvent.click(discard);
    await waitFor(() => expect(answer.mock.calls).toEqual([[false], [true]]));
    expect(submit).not.toHaveBeenCalled();
  });

  it('settles pending confirmation as false on provider unmount', async () => {
    const answer = vi.fn(); const view = render(<ConfirmProvider><Request answer={answer} /></ConfirmProvider>);
    fireEvent.click(screen.getByRole('button', { name: '关闭编辑器' }));
    view.unmount(); await waitFor(() => expect(answer.mock.calls).toEqual([[false]]));
  });

  it('does not authorize an action whose editor unmounted while the prompt was pending', async () => {
    const answer = vi.fn();
    const view = render(<ConfirmProvider><Request answer={answer} /></ConfirmProvider>);
    fireEvent.click(screen.getByRole('button', { name: '关闭编辑器' }));
    view.rerender(<ConfirmProvider>{null}</ConfirmProvider>);
    fireEvent.click(screen.getByRole('button', { name: '确认删除' }));
    await waitFor(() => expect(answer.mock.calls).toEqual([[false]]));
  });
  it('releases the scroll lock when accepting closes both the confirmation and its editor', async () => {
    function Editor() {
      const [open, setOpen] = useState(true);
      return <ConfirmProvider>{open && <Modal title="编辑模型" onClose={() => setOpen(false)}><Request answer={accepted => { if (accepted) setOpen(false); }} /></Modal>}</ConfirmProvider>;
    }
    document.body.style.overflow = 'auto';
    render(<Editor />);
    fireEvent.click(screen.getByRole('button', { name: '关闭编辑器' }));
    fireEvent.click(screen.getByRole('button', { name: '确认删除' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(document.body.style.overflow).toBe('auto');
  });

  it('releases nested scroll locks when the provider unmounts with a pending prompt', async () => {
    const answer = vi.fn(); document.body.style.overflow = 'auto';
    const view = render(<ConfirmProvider><Modal title="编辑模型" onClose={() => {}}><Request answer={answer} /></Modal></ConfirmProvider>);
    fireEvent.click(screen.getByRole('button', { name: '关闭编辑器' }));
    view.unmount();
    await waitFor(() => expect(answer.mock.calls).toEqual([[false]]));
    expect(document.body.style.overflow).toBe('auto');
  });

});
