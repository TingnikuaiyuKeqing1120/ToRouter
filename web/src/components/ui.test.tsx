// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Modal, PageHeader, ToastProvider, useToast } from './ui';

afterEach(() => { cleanup(); });

function ToastHarness() {
  const toast = useToast();
  return (
    <div>
      <button onClick={() => toast('已保存成功')}>Legacy Success</button>
      <button onClick={() => toast('保存失败，请重试')}>Inferred Error</button>
      <button onClick={() => toast('授权失效')}>Inferred Warning</button>
      <button onClick={() => toast.success('明确成功')}>Explicit Success</button>
      <button onClick={() => toast.error('明确错误')}>Explicit Error</button>
      <button onClick={() => toast.warning('明确警告')}>Explicit Warning</button>
      <button onClick={() => toast.info('明确信息')}>Explicit Info</button>
    </div>
  );
}

describe('ui component accessibility and polymorphic feedback', () => {
  describe('ToastProvider & useToast', () => {
    it('infers toast type and accessible role from legacy string content', async () => {
      render(
        <ToastProvider>
          <ToastHarness />
        </ToastProvider>
      );

      fireEvent.click(screen.getByText('Legacy Success'));
      const successToast = screen.getByRole('status');
      expect(successToast).toHaveProperty('textContent', expect.stringContaining('已保存成功'));
      expect(successToast.getAttribute('class')).toContain('toast-success');
      expect(successToast.getAttribute('aria-live')).toBe('polite');

      // Dismiss
      fireEvent.click(screen.getByRole('button', { name: '关闭通知' }));
      expect(screen.queryByRole('status')).toBeNull();

      fireEvent.click(screen.getByText('Inferred Error'));
      const errorToast = screen.getByRole('alert');
      expect(errorToast).toHaveProperty('textContent', expect.stringContaining('保存失败'));
      expect(errorToast.getAttribute('class')).toContain('toast-error');
      expect(errorToast.getAttribute('aria-live')).toBe('assertive');

      fireEvent.click(screen.getByRole('button', { name: '关闭通知' }));

      fireEvent.click(screen.getByText('Inferred Warning'));
      const warningToast = screen.getByRole('status');
      expect(warningToast).toHaveProperty('textContent', expect.stringContaining('授权失效'));
      expect(warningToast.getAttribute('class')).toContain('toast-warning');
    });

    it('supports polymorphic helper methods: toast.error, toast.success, toast.warning, toast.info', () => {
      render(
        <ToastProvider>
          <ToastHarness />
        </ToastProvider>
      );

      fireEvent.click(screen.getByText('Explicit Error'));
      const errorToast = screen.getByRole('alert');
      expect(errorToast).toHaveProperty('textContent', expect.stringContaining('明确错误'));
      expect(errorToast.getAttribute('class')).toContain('toast-error');

      fireEvent.click(screen.getByText('Explicit Success'));
      const successToast = screen.getByRole('status');
      expect(successToast).toHaveProperty('textContent', expect.stringContaining('明确成功'));
      expect(successToast.getAttribute('class')).toContain('toast-success');

      fireEvent.click(screen.getByText('Explicit Warning'));
      const warnToast = screen.getByRole('status');
      expect(warnToast).toHaveProperty('textContent', expect.stringContaining('明确警告'));
      expect(warnToast.getAttribute('class')).toContain('toast-warning');

      fireEvent.click(screen.getByText('Explicit Info'));
      const infoToast = screen.getByRole('status');
      expect(infoToast).toHaveProperty('textContent', expect.stringContaining('明确信息'));
      expect(infoToast.getAttribute('class')).toContain('toast-info');
    });
  });

  describe('Modal accessibility', () => {
    beforeEach(() => {
      HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
      HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
    });

    it('associates aria-labelledby with title and aria-describedby with description when present', () => {
      render(
        <Modal title="测试标题" description="这是测试描述" onClose={() => {}}>
          <div>内容</div>
        </Modal>
      );

      const dialog = screen.getByRole('dialog');
      const titleId = dialog.getAttribute('aria-labelledby');
      const descId = dialog.getAttribute('aria-describedby');

      expect(titleId).toBeTruthy();
      expect(descId).toBeTruthy();
      expect(screen.getByText('测试标题').getAttribute('id')).toBe(titleId);
      expect(screen.getByText('这是测试描述').getAttribute('id')).toBe(descId);
    });

    it('omits aria-describedby when description is undefined or absent', () => {
      render(
        <Modal title="仅标题弹窗" onClose={() => {}}>
          <div>内容</div>
        </Modal>
      );

      const dialog = screen.getByRole('dialog');
      expect(dialog.getAttribute('aria-labelledby')).toBeTruthy();
      expect(dialog.getAttribute('aria-describedby')).toBeNull();
    });
  });

  describe('PageHeader eyebrow rendering', () => {
    it('renders eyebrow only when non-empty string', () => {
      const { rerender } = render(<PageHeader eyebrow="CHANNELS" title="渠道" />);
      expect(screen.getByText('CHANNELS')).toHaveProperty('className', 'eyebrow');

      rerender(<PageHeader eyebrow="" title="渠道" />);
      expect(screen.queryByText('CHANNELS')).toBeNull();
      expect(document.querySelector('.eyebrow')).toBeNull();

      rerender(<PageHeader eyebrow="   " title="渠道" />);
      expect(document.querySelector('.eyebrow')).toBeNull();

      rerender(<PageHeader title="渠道" />);
      expect(document.querySelector('.eyebrow')).toBeNull();
    });
  });
});
