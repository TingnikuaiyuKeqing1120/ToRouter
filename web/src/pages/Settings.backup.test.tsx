// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render as renderUI, screen, waitFor } from '@testing-library/react';
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

class ControlledFileReader {
  static instances: ControlledFileReader[] = [];
  result: string | null = null;
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor() { ControlledFileReader.instances.push(this); }
  readAsText(_blob: Blob) {}
  succeed(text: string) { this.result = text; this.onload?.(); }
  fail() { this.onerror?.(); }
}

const summary = (count: number) => ({
  channels_count: count, downstream_models_count: 5, client_keys_count: 2,
  plan_prices_count: 1, tags_count: 4, exported_at: '2026-10-22T00:00:00Z', version: 1,
});

function selectFile(name: string) {
  fireEvent.change(screen.getByLabelText('选择备份文件'), {
    target: { files: [new File([name], name, { type: 'application/json' })] },
  });
  return ControlledFileReader.instances[ControlledFileReader.instances.length - 1];
}

function backupRequests(path: string) {
  return vi.mocked(fetch).mock.calls.filter(([url]) => url === `/api/config-backup/${path}`);
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
  async function setupControlledReader() {
    ControlledFileReader.instances = [];
    vi.stubGlobal('FileReader', ControlledFileReader);
    const view = render(<Settings />);
    await screen.findByRole('button', { name: '预览配置' });
    fireEvent.change(screen.getByLabelText('解密口令'), { target: { value: 'valid-long-password-123' } });
    return view;
  }

  it('disables preview and restore while replacement file is loading and after it fails', async () => {
    await setupControlledReader();
    const a = selectFile('a.json');
    act(() => a.succeed('backup-a'));
    expect(screen.getByRole('button', { name: '从备份恢复' })).toHaveProperty('disabled', false);
    const b = selectFile('b.json');
    expect(screen.getByRole('button', { name: '预览配置' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: '从备份恢复' })).toHaveProperty('disabled', true);
    act(() => b.fail());
    expect(screen.getByText('读取文件失败，请重新选择。')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: '预览配置' }));
    fireEvent.click(screen.getByRole('button', { name: '从备份恢复' }));
    expect(screen.queryByRole('button', { name: '确认恢复并重启' })).toBeNull();
    expect(backupRequests('preview')).toHaveLength(0);
    expect(backupRequests('restore')).toHaveLength(0);
  });

  it('uses the latest selection when file reads finish out of order', async () => {
    vi.mocked(fetch).mockImplementation(url => Promise.resolve(json(url === '/api/config-backup/preview' ? { summary: summary(2) } : settings)));
    await setupControlledReader();
    const a = selectFile('a.json');
    const b = selectFile('b.json');
    act(() => b.succeed('backup-b'));
    act(() => a.succeed('backup-a'));
    fireEvent.click(screen.getByRole('button', { name: '预览配置' }));
    await screen.findByRole('status', { name: '备份概况' });
    expect(JSON.parse(backupRequests('preview')[0][1]!.body as string).file).toBe('backup-b');
    act(() => a.fail());
    expect(screen.queryByText('读取文件失败，请重新选择。')).toBeNull();
  });

  it.each(['file', 'password'] as const)('does not display an old preview after changing %s', async change => {
    let respond!: (value: Response) => void;
    vi.mocked(fetch).mockImplementation(url => url === '/api/config-backup/preview'
      ? new Promise<Response>(resolve => { respond = resolve; })
      : Promise.resolve(json(settings)));
    await setupControlledReader();
    act(() => selectFile('a.json').succeed('backup-a'));
    fireEvent.click(screen.getByRole('button', { name: '预览配置' }));
    await waitFor(() => expect(respond).toBeDefined());
    if (change === 'file') selectFile('b.json');
    else fireEvent.change(screen.getByLabelText('解密口令'), { target: { value: 'another-valid-password' } });
    await act(async () => { respond(json({ summary: summary(91) })); });
    expect(screen.queryByRole('status', { name: '备份概况' })).toBeNull();
  });

  it.each(['file', 'password'] as const)('ignores a stale preview after changing %s and keeps the newer preview busy', async change => {
    const responses: ((value: Response) => void)[] = [];
    vi.mocked(fetch).mockImplementation(url => url === '/api/config-backup/preview'
      ? new Promise<Response>(resolve => responses.push(resolve))
      : Promise.resolve(json(settings)));
    await setupControlledReader();
    act(() => selectFile('a.json').succeed('backup-a'));
    fireEvent.click(screen.getByRole('button', { name: '预览配置' }));
    await waitFor(() => expect(responses).toHaveLength(1));
    if (change === 'file') act(() => selectFile('b.json').succeed('backup-b'));
    else fireEvent.change(screen.getByLabelText('解密口令'), { target: { value: 'another-valid-password' } });
    expect(screen.getByRole('button', { name: '预览配置' })).toHaveProperty('disabled', false);
    fireEvent.click(screen.getByRole('button', { name: '预览配置' }));
    await waitFor(() => expect(responses).toHaveLength(2));
    await act(async () => { responses[0](json({ summary: summary(91) })); });
    expect(screen.queryByRole('status', { name: '备份概况' })).toBeNull();
    expect(screen.getByRole('button', { name: '正在解析…' })).toHaveProperty('disabled', true);
    await act(async () => { responses[1](json({ summary: summary(2) })); });
    expect(screen.getByText('渠道：2 个')).toBeDefined();
    expect(screen.queryByText('渠道：91 个')).toBeNull();
    fireEvent.change(screen.getByLabelText('解密口令'), { target: { value: 'changed-again-password' } });
    expect(screen.queryByRole('status', { name: '备份概况' })).toBeNull();
  });

  it.each(['file', 'password'] as const)('invalidates restore confirmation after changing %s', async change => {
    await setupControlledReader();
    act(() => selectFile('a.json').succeed('backup-a'));
    fireEvent.click(screen.getByRole('button', { name: '从备份恢复' }));
    const confirm = await screen.findByRole('button', { name: '确认恢复并重启' });
    if (change === 'file') selectFile('b.json');
    else fireEvent.change(screen.getByLabelText('解密口令'), { target: { value: 'another-valid-password' } });
    fireEvent.click(confirm);
    expect(backupRequests('restore')).toHaveLength(0);
    expect(screen.queryByRole('button', { name: '确认恢复并重启' })).toBeNull();
  });

  it('ignores a file callback after unmount and does not leak it into a fresh page', async () => {
    const view = await setupControlledReader();
    const a = selectFile('a.json');
    view.unmount();
    await setupControlledReader();
    act(() => a.succeed('backup-a'));
    expect(screen.getByRole('button', { name: '预览配置' })).toHaveProperty('disabled', true);
    expect(backupRequests('preview')).toHaveLength(0);
  });

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
