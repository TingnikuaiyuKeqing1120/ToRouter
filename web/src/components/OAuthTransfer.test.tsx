// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { OAuthTransfer } from './OAuthTransfer';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const material = '{"tokens":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh"}}';
const preview = () => ({ id: 'preview /?', target: { id: 7, name: '当前渠道', version: 'v7' }, expires_at: new Date(Date.now() + 60_000).toISOString(), rows: [
  { id: 'one', label: '凭据一', credential_type: 'codex_oauth', enabled: true, status: 'new', targets: [{ id: 7, name: '当前渠道', version: 'v7' }, { id: 8, name: '其它渠道', version: 'v8' }] },
  { id: 'two', label: '凭据二', credential_type: 'codex_oauth', enabled: true, status: 'new', targets: [{ id: 7, name: '当前渠道', version: 'v7' }] },
  { id: 'old', label: '已有凭据', credential_type: 'codex_oauth', enabled: false, status: 'exists', targets: [] },
] });
const base = '/api/oauth-transfer/sessions/preview%20%2F%3F';
const file = (content = material, name = 'auth.json', size?: number) => {
  const value = new File([size ? new Uint8Array(size) : content], name, { type: 'application/json' });
  Object.defineProperty(value, 'text', { value: vi.fn(() => Promise.resolve(content)) }); return value;
};
function setup(override?: (url: string, init?: RequestInit) => Promise<Response> | undefined) {
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    const custom = override?.(url, init); if (custom) return custom;
    if (url === '/api/oauth-transfer/preview') return Promise.resolve(json(preview()));
    if (url === base && init?.method === 'DELETE') return Promise.resolve(json({ cancelled: true }));
    throw new Error('unexpected fixture request');
  });
  vi.stubGlobal('fetch', fetcher);
  const props = { mode: 'import' as const, credentialType: 'codex_oauth' as const, targetChannelID: 7, onClose: vi.fn(), onChanged: vi.fn() };
  return { ...render(<OAuthTransfer {...props} />), props, fetcher };
}
function pick(values = [file()]) { fireEvent.change(screen.getByLabelText('凭证文件'), { target: { files: values } }); }
async function prepare() { pick(); fireEvent.click(screen.getByRole('button', { name: '预览凭证' })); await screen.findByRole('list', { name: '凭证导入预览' }); }
beforeEach(() => {
  localStorage.clear(); sessionStorage.clear();
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
  vi.stubGlobal('URL', class extends URL { static createObjectURL = vi.fn(() => 'blob:synthetic'); static revokeObjectURL = vi.fn(); });
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('locks a second-channel paste preview to the server target and wraps the selected token type explicitly', async () => {
  const locked = { id: 8, name: '第二渠道', version: 'second-v8' };
  const fetcher = vi.fn((url: string, init?: RequestInit) => {
    if (url === '/api/oauth-transfer/preview') return Promise.resolve(json({ ...preview(), target: locked, rows: [{ ...preview().rows[0], targets: [locked] }] }));
    if (url.endsWith('/import')) return Promise.resolve(json({ results: [{ channel_id: 8, added: 1, skipped: 0, status: 'completed' }], preview: { ...preview(), target: locked, rows: [] } }));
    if (init?.method === 'DELETE') return Promise.resolve(json({}));
    throw new Error('unexpected fixture request');
  });
  vi.stubGlobal('fetch', fetcher);
  render(<OAuthTransfer mode="import" credentialType="codex_oauth" targetChannelID={8} onClose={vi.fn()} />);
  fireEvent.change(screen.getByLabelText('凭证材料类型'), { target: { value: 'access_token' } });
  fireEvent.change(screen.getByLabelText('账号或工作空间 ID'), { target: { value: 'synthetic-workspace' } });
  fireEvent.change(screen.getByLabelText('粘贴凭证内容'), { target: { value: 'synthetic-access-only' } });
  fireEvent.click(screen.getByRole('button', { name: '预览凭证' }));
  await screen.findByText('添加到「第二渠道」');
  expect(screen.queryByLabelText('添加到')).toBeNull();
  expect(screen.queryByLabelText('新渠道名称')).toBeNull();
  const body = JSON.parse(String(fetcher.mock.calls.find(([url]) => url.endsWith('/preview'))![1]!.body));
  expect(body).toEqual({ credential_type: 'codex_oauth', target_channel_id: 8, file: JSON.stringify({ access_token: 'synthetic-access-only', account_id: 'synthetic-workspace' }) });
  fireEvent.click(screen.getByRole('button', { name: '添加选中（1）' }));
  await screen.findByText('已添加 1 个，跳过 0 个。');
  expect(JSON.parse(String(fetcher.mock.calls.find(([url]) => url.endsWith('/import'))![1]!.body))).toEqual({ items: [{ candidate_id: 'one', target_channel_id: 8, expected_version: 'second-v8' }] });
  expect(localStorage.length + sessionStorage.length).toBe(0);
});

it('previews native files without a password, defaults to the current channel and imports selected rows only', async () => {
  const view = setup((url, init) => url === base + '/import' && init?.method === 'POST' ? Promise.resolve(json({ results: [{ channel_id: 7, added: 1, skipped: 0, status: 'completed' }], preview: { ...preview(), rows: [] } })) : undefined);
  expect(view.fetcher).not.toHaveBeenCalled();
  expect(screen.queryByLabelText(/口令/)).toBeNull();
  await prepare();
  expect(screen.getByText('添加到「当前渠道」')).toBeTruthy();
  expect(screen.queryByLabelText('添加到')).toBeNull();
  expect(screen.getByLabelText('选择导入 凭据一')).toHaveProperty('checked', true);
  expect(screen.getByLabelText('选择导入 已有凭据')).toHaveProperty('disabled', true);
  expect(screen.queryByText(material)).toBeNull();
  fireEvent.click(screen.getByLabelText('选择导入 凭据二'));
  fireEvent.click(screen.getByRole('button', { name: '添加选中（1）' }));
  await screen.findByText('已添加 1 个，跳过 0 个。');
  expect(JSON.parse(String(view.fetcher.mock.calls.find(([url]) => url.endsWith('/preview'))![1]!.body))).toEqual({ credential_type: 'codex_oauth', target_channel_id: 7, file: material });
  expect(JSON.parse(String(view.fetcher.mock.calls.find(([url]) => url.endsWith('/import'))![1]!.body))).toEqual({ items: [{ candidate_id: 'one', target_channel_id: 7, expected_version: 'v7' }] });
  expect(view.props.onChanged).toHaveBeenCalledTimes(1);
  expect(localStorage.length + sessionStorage.length).toBe(0);
});

it('keeps failed rows selected for retry and only refreshes channels after an actual addition', async () => {
  let attempts = 0;
  const view = setup(url => url.endsWith('/import') ? Promise.resolve(json(++attempts === 1 ? {
    results: [{ candidate_ids: ['one', 'two'], status: 'failed', message: '暂时未完成，请重试。', added: 0, skipped: 0, channel_id: 0 }], preview: preview(),
  } : {
    results: [{ candidate_ids: ['one', 'two'], status: 'completed', added: 2, skipped: 0, channel_id: 7 }], preview: { ...preview(), rows: [] },
  })) : undefined);
  await prepare();
  fireEvent.click(screen.getByRole('button', { name: '添加选中（2）' }));
  await screen.findByText('暂时未完成，请重试。');
  expect(view.props.onChanged).not.toHaveBeenCalled();
  expect(screen.getByLabelText('选择导入 凭据一')).toHaveProperty('checked', true);
  expect(screen.getByLabelText('选择导入 凭据二')).toHaveProperty('checked', true);
  fireEvent.click(screen.getByRole('button', { name: '添加选中（2）' }));
  await screen.findByText('已添加 2 个，跳过 0 个。');
  expect(view.props.onChanged).toHaveBeenCalledTimes(1);
  const requests = view.fetcher.mock.calls.filter(([url]) => url.endsWith('/import'));
  expect(requests).toHaveLength(2);
  expect(requests[1][1]?.body).toEqual(requests[0][1]?.body);
});

it('keeps each selected file and pasted native payload separate for backend parsing', async () => {
  const view = setup(); const second = '{"accounts":[{"refresh_token":"synthetic-second"}]}';
  pick([file(), file(second, 'accounts.json')]);
  fireEvent.change(screen.getByLabelText('粘贴凭证内容'), { target: { value: '{"synthetic":"pasted"}' } });
  fireEvent.click(screen.getByRole('button', { name: '预览凭证' }));
  await screen.findByRole('list', { name: '凭证导入预览' });
  expect(JSON.parse(String(view.fetcher.mock.calls[0][1]!.body))).toEqual({ credential_type: 'codex_oauth', target_channel_id: 7, files: [material, second, '{"synthetic":"pasted"}'] });
});

it('keeps select-all within the fixed target and never offers creation or target switching', async () => {
  const view = setup((url) => url.endsWith('/import') ? Promise.resolve(json({ results: [{ channel_id: 9, added: 2, skipped: 0, status: 'completed' }], preview: { ...preview(), rows: [] } })) : undefined);
  await prepare();
  fireEvent.click(screen.getByLabelText('全选新凭证'));
  expect(screen.getByRole('button', { name: '添加选中（0）' })).toHaveProperty('disabled', true);
  fireEvent.click(screen.getByLabelText('全选新凭证'));
  expect(screen.queryByLabelText('添加到')).toBeNull();
  expect(screen.queryByLabelText('新渠道名称')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '添加选中（2）' }));
  await waitFor(() => expect(view.props.onChanged).toHaveBeenCalled());
  const sent = JSON.parse(String(view.fetcher.mock.calls.find(([url]) => url.endsWith('/import'))![1]!.body));
  expect(sent.items).toEqual(['one', 'two'].map(id => ({ candidate_id: id, target_channel_id: 7, expected_version: 'v7' })));
});

it('reports individual invalid files while retaining valid rows', async () => {
  setup(url => url.endsWith('/preview') ? Promise.resolve(json({ ...preview(), errors: [{ file: 2, message: '凭证格式无法识别' }] })) : undefined);
  await prepare(); expect(screen.getByText('文件 2：凭证格式无法识别')).toBeTruthy();
  expect(screen.getByRole('button', { name: '添加选中（2）' })).toHaveProperty('disabled', false);
});

it('discards a late preview after close and never reopens the dialog', async () => {
  let resolve!: (value: Response) => void;
  const view = setup(url => url.endsWith('/preview') ? new Promise<Response>(done => { resolve = done; }) : undefined);
  pick(); fireEvent.click(screen.getByRole('button', { name: '预览凭证' }));
  await waitFor(() => expect(resolve).toBeDefined());
  fireEvent.click(screen.getByRole('button', { name: '关闭' }));
  await act(async () => { resolve(json(preview())); });
  expect(view.props.onClose).toHaveBeenCalledTimes(1);
  await waitFor(() => expect(view.fetcher.mock.calls.some(([url, init]) => url === base && init?.method === 'DELETE')).toBe(true));
  expect(screen.queryByRole('list', { name: '凭证导入预览' })).toBeNull();
});

it('rejects expired previews before sending an import', async () => {
  const view = setup(url => url.endsWith('/preview') ? Promise.resolve(json({ ...preview(), expires_at: new Date(0).toISOString() })) : undefined);
  await prepare(); fireEvent.click(screen.getByRole('button', { name: '添加选中（2）' }));
  expect(screen.getByText('预览已过期，请重新选择文件。')).toBeTruthy();
  expect(view.fetcher.mock.calls.some(([url]) => url.endsWith('/import'))).toBe(false);
});

it('does not echo raw server errors or upload oversized files', async () => {
  const view = setup(url => url.endsWith('/preview') ? Promise.resolve(json({ error: 'synthetic-private-token-and-path' }, 400)) : undefined);
  pick([file(material, 'large.json', 4 * 1024 * 1024 + 1)]);
  expect(screen.getByText('单个文件不能超过 4 MiB。')).toBeTruthy(); expect(view.fetcher).not.toHaveBeenCalled();
  pick(); fireEvent.click(screen.getByRole('button', { name: '预览凭证' }));
  await screen.findByText('文件格式或凭证材料不完整，请核对对应工具的凭证文件。');
  expect(document.body.textContent).not.toContain('synthetic-private');
});

it.each([['auth.json', undefined, material, 'application/json'], ['credentials.zip', 'base64', 'c3ludGhldGlj', 'application/zip']])('downloads %s directly with no password or forced JSON suffix', async (filename, encoding, content, mime) => {
  const fetcher = vi.fn((_url: string, _init?: RequestInit) => Promise.resolve(json({ file: content, filename, encoding, mime_type: mime, count: 1 })));
  vi.stubGlobal('fetch', fetcher);
  render(<OAuthTransfer mode="export" refs={[{ channel_id: 7, credential_id: 'one' }]} labels={['我的凭证']} onClose={() => {}} />);
  expect(screen.queryByLabelText(/口令/)).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '下载凭证' }));
  await screen.findByText('已下载 1 个凭证。');
  expect(JSON.parse(String(fetcher.mock.calls[0][1]?.body))).toEqual({ refs: [{ channel_id: 7, credential_id: 'one' }] });
  expect(HTMLAnchorElement.prototype.click).toHaveBeenCalledTimes(1);
  const anchor = vi.mocked(HTMLAnchorElement.prototype.click).mock.instances[0] as unknown as HTMLAnchorElement;
  expect(anchor.download).toBe(filename);
});

it('does not download an export if its selected references changed in flight', async () => {
  let resolve!: (value: Response) => void;
  vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(done => { resolve = done; })));
  const view = render(<OAuthTransfer mode="export" refs={[{ channel_id: 7, credential_id: 'one' }]} onClose={() => {}} />);
  fireEvent.click(screen.getByRole('button', { name: '下载凭证' }));
  view.rerender(<OAuthTransfer mode="export" refs={[{ channel_id: 7, credential_id: 'two' }]} onClose={() => {}} />);
  await act(async () => resolve(json({ file: material, filename: 'auth.json', count: 1, mime_type: 'application/json' })));
  expect(HTMLAnchorElement.prototype.click).not.toHaveBeenCalled();
});
