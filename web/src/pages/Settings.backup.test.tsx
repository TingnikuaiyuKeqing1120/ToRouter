// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render as renderUI, screen, waitFor } from '@testing-library/react';
import { ConfirmProvider } from '../components/Confirm';
import { restartConsole } from '../restart';
import Settings from './Settings';

vi.mock('../restart', () => ({
  restartConsole: vi.fn().mockResolvedValue('http://127.0.0.1:2886/#/settings'),
}));

class MockFileReader {
  result: string | ArrayBuffer | null = null;
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;

  readAsText(_blob: Blob) {
    setTimeout(() => {
      this.result = '{"format":"torouter-full-config"}';
      this.onload?.();
    }, 0);
  }
}

const render = (ui: Parameters<typeof renderUI>[0], options?: Parameters<typeof renderUI>[1]) =>
  renderUI(ui, { wrapper: ConfirmProvider, ...options });

const settings = {
  gateway_key: 'synthetic-admin',
  quota_refresh_seconds: 300,
  retention_days: 7,
  max_attempts: 3,
  retries_per_channel: 2,
  failure_cooldown_seconds: 10,
};

const json = (value: unknown, status = 200) =>
  new Response(JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });

beforeEach(() => {
  vi.stubGlobal('FileReader', MockFileReader);
  HTMLDialogElement.prototype.showModal = function () {
    this.setAttribute('open', '');
  };
  HTMLDialogElement.prototype.close = function () {
    this.removeAttribute('open');
  };
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      if (url === '/api/settings') return Promise.resolve(json(settings));
      if (url === '/api/runtime/port') return Promise.resolve(json({ port: 2886, origin: 'http://127.0.0.1:2886' }));
      return Promise.resolve(json({}));
    })
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('Settings backup and restore', () => {
  it('enforces 12-character minimum for export password and triggers export download', async () => {
    let exportBody: string | null = null;
    vi.mocked(fetch).mockImplementation((url, init) => {
      if (url === '/api/config-backup/export' && init?.method === 'POST') {
        exportBody = init.body as string;
        return Promise.resolve(
          json({
            file: '{"format":"torouter-full-config","version":1}',
            filename: 'torouter-backup-20261022.json',
          })
        );
      }
      if (url === '/api/settings') return Promise.resolve(json(settings));
      return Promise.resolve(json({ port: 2886 }));
    });

    render(<Settings />);

    const exportBtn = await screen.findByRole('button', { name: '导出全部配置' });
    expect(exportBtn).toHaveProperty('disabled', true);

    const exportPwdInput = screen.getByLabelText('备份加密口令');
    fireEvent.change(exportPwdInput, { target: { value: 'short-pwd' } });
    expect(exportBtn).toHaveProperty('disabled', true);

    fireEvent.change(exportPwdInput, { target: { value: 'valid-long-password-123' } });
    expect(exportBtn).toHaveProperty('disabled', false);

    fireEvent.click(exportBtn);

    await waitFor(() => expect(exportBody).not.toBeNull());
    expect(JSON.parse(exportBody!)).toEqual({ password: 'valid-long-password-123' });
  });

  it('previews backup file summary and confirms restore with restart', async () => {
    let previewCalled = false;
    let restoreCalled = false;
    vi.mocked(fetch).mockImplementation((url, init) => {
      if (url === '/api/config-backup/preview' && init?.method === 'POST') {
        previewCalled = true;
        return Promise.resolve(
          json({
            summary: {
              channels_count: 3,
              downstream_models_count: 5,
              client_keys_count: 2,
              plan_prices_count: 1,
              tags_count: 4,
              exported_at: '2026-10-22T00:00:00Z',
              version: 1,
            },
          })
        );
      }
      if (url === '/api/config-backup/restore' && init?.method === 'POST') {
        restoreCalled = true;
        return Promise.resolve(json({ ok: true, restart_required: true }));
      }
      if (url === '/api/settings') return Promise.resolve(json(settings));
      return Promise.resolve(json({ port: 2886 }));
    });

    render(<Settings />);

    const previewBtn = await screen.findByRole('button', { name: '预览配置' });
    const restoreBtn = screen.getByRole('button', { name: '从备份恢复' });

    expect(previewBtn).toHaveProperty('disabled', true);
    expect(restoreBtn).toHaveProperty('disabled', true);

    // Provide file and password
    const file = new File(['{"format":"torouter-full-config"}'], 'backup.json', { type: 'application/json' });
    const fileInput = screen.getByLabelText('选择备份文件');
    fireEvent.change(fileInput, { target: { files: [file] } });

    const restorePwdInput = screen.getByLabelText('解密口令');
    fireEvent.change(restorePwdInput, { target: { value: 'valid-long-password-123' } });

    await waitFor(() => expect(previewBtn).toHaveProperty('disabled', false));
    expect(restoreBtn).toHaveProperty('disabled', false);

    // Click preview
    fireEvent.click(previewBtn);
    await screen.findByRole('status', { name: /备份概况/i });
    expect(screen.getByText(/渠道：3 个/)).toBeDefined();
    expect(screen.getByText(/模型：5 个/)).toBeDefined();
    expect(screen.getByText(/客户端 Key：2 个/)).toBeDefined();
    expect(previewCalled).toBe(true);

    // Click restore -> opens modal
    fireEvent.click(restoreBtn);
    const confirmBtn = await screen.findByRole('button', { name: '确认恢复并重启' });
    expect(confirmBtn).toBeDefined();

    // Confirm restore
    fireEvent.click(confirmBtn);
    await waitFor(() => expect(restoreCalled).toBe(true));
    await waitFor(() => expect(restartConsole).toHaveBeenCalledOnce());
  });

  it('discovers existing snapshot, shows rollback card, and confirms rollback with restart', async () => {
    let rollbackBody: string | null = null;
    vi.mocked(fetch).mockImplementation((url, init) => {
      if (url === '/api/config-backup/rollback' && (!init || !init.method || init.method === 'GET')) {
        return Promise.resolve(
          json({ available: true, snapshot: 'pre-restore-snapshot-20261022-120000.json' })
        );
      }
      if (url === '/api/config-backup/rollback' && init?.method === 'POST') {
        rollbackBody = init.body as string;
        return Promise.resolve(
          json({ ok: true, restart_required: true, restored_snapshot: 'pre-restore-snapshot-20261022-120000.json' })
        );
      }
      if (url === '/api/settings') return Promise.resolve(json(settings));
      return Promise.resolve(json({ port: 2886 }));
    });

    render(<Settings />);

    const rollbackBtn = await screen.findByRole('button', { name: '回退到此快照' });
    expect(screen.getByText(/最近快照：/)).toBeDefined();
    expect(screen.getByText(/pre-restore-snapshot-20261022-120000\.json/)).toBeDefined();

    fireEvent.click(rollbackBtn);

    const confirmModalBtn = await screen.findByRole('button', { name: '确认回退并重启' });
    expect(confirmModalBtn).toBeDefined();

    fireEvent.click(confirmModalBtn);

    await waitFor(() => expect(rollbackBody).not.toBeNull());
    expect(JSON.parse(rollbackBody!)).toEqual({
      snapshot: 'pre-restore-snapshot-20261022-120000.json',
      confirm: true,
    });
    await waitFor(() => expect(restartConsole).toHaveBeenCalledOnce());
  });
});
