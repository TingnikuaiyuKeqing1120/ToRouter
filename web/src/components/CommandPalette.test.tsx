// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CommandPalette } from './CommandPalette';
import { ToastProvider } from './ui';

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(JSON.stringify({ origin: 'http://127.0.0.1:2886', port: 2886 })))));
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: vi.fn().mockResolvedValue(undefined) } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function palette() { return render(<ToastProvider><CommandPalette isOpen onClose={vi.fn()} onNavigate={vi.fn()} currentPage="usage" toggleTheme={vi.fn()} theme="dark" onLogout={vi.fn()} /></ToastProvider>); }

describe('CommandPalette', () => {
  it('does not render when isOpen is false', () => {
    const { container } = render(
      <ToastProvider>
        <CommandPalette
          isOpen={false}
          onClose={vi.fn()}
          onNavigate={vi.fn()}
          currentPage="usage"
          toggleTheme={vi.fn()}
          theme="dark"
          onLogout={vi.fn()}
        />
      </ToastProvider>
    );
    expect(container.querySelector('[role="dialog"]')).toBeNull();
  });

  it('renders and supports search filtering and keyboard selection', () => {
    const onNavigate = vi.fn();
    const onClose = vi.fn();

    render(
      <ToastProvider>
        <CommandPalette
          isOpen={true}
          onClose={onClose}
          onNavigate={onNavigate}
          currentPage="usage"
          toggleTheme={vi.fn()}
          theme="dark"
          onLogout={vi.fn()}
        />
      </ToastProvider>
    );

    const dialog = screen.getByRole('dialog', { name: '命令导航面板' });
    expect(dialog).toBeTruthy();

    const input = screen.getByPlaceholderText(/搜索功能、页面或快捷操作/);
    expect(input).toBeTruthy();

    // Filter by query "渠道"
    fireEvent.change(input, { target: { value: '渠道' } });
    expect(screen.getByText('上游接入')).toBeTruthy();
    expect(screen.queryByText('系统设置')).toBeNull();

    // Select with Enter key
    fireEvent.keyDown(window, { key: 'Enter' });
    expect(onNavigate).toHaveBeenCalledWith('channels');
    expect(onClose).toHaveBeenCalled();
  });

  it('closes on Escape key', () => {
    const onClose = vi.fn();
    render(
      <ToastProvider>
        <CommandPalette
          isOpen={true}
          onClose={onClose}
          onNavigate={vi.fn()}
          currentPage="usage"
          toggleTheme={vi.fn()}
          theme="dark"
          onLogout={vi.fn()}
        />
      </ToastProvider>
    );

    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });
});


describe('CommandPalette actual connection', () => {
  it.each([17878, 2886])('copies only the actual port %s from connection, independent of the page origin', async port => {
    vi.mocked(fetch).mockResolvedValue(new Response(JSON.stringify({ origin: `http://127.0.0.1:${port}`, port })));
    palette();
    await screen.findByText(`http://127.0.0.1:${port}/v1`);
    fireEvent.click(screen.getByRole('button', { name: /复制网关 BaseURL/ }));
    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith(`http://127.0.0.1:${port}/v1`));
    await screen.findByText(`已复制网关 BaseURL http://127.0.0.1:${port}/v1`);
    fireEvent.click(screen.getByRole('button', { name: /复制本地监听端口/ }));
    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith(`127.0.0.1:${port}`));
    await screen.findByText(`已复制本地监听地址 127.0.0.1:${port}`);
  });

  it('reports the same actual address when clipboard access fails', async () => {
    vi.mocked(navigator.clipboard.writeText).mockRejectedValue(new Error('clipboard denied'));
    palette(); await screen.findByText('http://127.0.0.1:2886/v1');
    fireEvent.click(screen.getByRole('button', { name: /复制网关 BaseURL/ }));
    await screen.findByText('复制失败，请手动复制：http://127.0.0.1:2886/v1');
  });

  it.each(['missing', 'error', 'invalid'])('does not invent or copy an address for %s connection information', async kind => {
    vi.mocked(fetch).mockImplementation(() => kind === 'error' ? Promise.reject(new Error('offline')) : Promise.resolve(new Response(JSON.stringify(kind === 'missing' ? {} : { origin: 'wails://host' }))));
    palette();
    await screen.findAllByText('无法获取网关地址，暂不可复制');
    const copy = screen.getByRole('button', { name: /复制网关 BaseURL/ });
    expect(copy).toHaveProperty('disabled', true); fireEvent.click(copy);
    fireEvent.change(screen.getByPlaceholderText(/搜索功能、页面或快捷操作/), { target: { value: 'BaseURL' } });
    fireEvent.keyDown(window, { key: 'Enter' });
    expect(navigator.clipboard.writeText).not.toHaveBeenCalled();
    expect(screen.queryByText(/17878/)).toBeNull();
  });
});
