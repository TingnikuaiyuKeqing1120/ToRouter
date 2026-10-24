// @vitest-environment jsdom
import { useState } from 'react';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { CredentialItem } from '../api';
import { CredentialKeyInput } from './CredentialKeyInput';

const credential: CredentialItem = { id: 'backup/key +2', label: 'Backup', has_key: true, masked_key: 'sk-••••ab12', enabled: true };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const path = '/api/channels/12/credentials/backup%2Fkey%20%2B2/key';
let writeText: ReturnType<typeof vi.fn>;
function Editor({ onChange }: { onChange: (value: string) => void }) {
  const [value, setValue] = useState('');
  return <CredentialKeyInput channelID={12} credential={{ ...credential, api_key: value }} index={1} onChange={next => { setValue(next); onChange(next); }} />;
}
beforeEach(() => {
  localStorage.clear(); sessionStorage.clear();
  writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('saved credential key copying', () => {
  it('previews the saved mask and copies the precise saved credential instead of the replacement draft', async () => {
    const fetcher = vi.fn().mockResolvedValue(json({ key: 'synthetic-saved-key' }));
    vi.stubGlobal('fetch', fetcher);
    const onChange = vi.fn();
    render(<Editor onChange={onChange} />);
    const input = screen.getByLabelText('凭据 2 API 密钥');
    expect(input).toHaveProperty('value', '');
    expect(input).toHaveProperty('placeholder', '已配置 sk-••••ab12 · 留空保留');
    fireEvent.change(input, { target: { value: 'synthetic-new-draft' } });
    expect(input).toHaveProperty('type', 'password');
    expect(fetcher).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '复制凭据 2 已保存的 Key' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('synthetic-saved-key'));
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(fetcher.mock.calls[0]).toEqual([path, expect.objectContaining({ cache: 'no-store', credentials: 'same-origin', signal: expect.any(AbortSignal) })]);
    expect(fetcher.mock.calls[0][1].method).toBeUndefined();
    expect(input).toHaveProperty('value', 'synthetic-new-draft');
    expect(onChange.mock.calls).toEqual([['synthetic-new-draft']]);
    expect(await screen.findByRole('status')).toHaveProperty('textContent', '已复制已保存的 Key');
    expect(localStorage.length).toBe(0); expect(sessionStorage.length).toBe(0);
  });

  it.each([{ channelID: 0, has_key: true }, { channelID: 12, has_key: false }])('offers no saved-key copy without a stored key: %j', props => {
    const fetcher = vi.fn(); vi.stubGlobal('fetch', fetcher);
    render(<CredentialKeyInput channelID={props.channelID} credential={{ ...credential, has_key: props.has_key, api_key: 'synthetic-unsaved-key' }} index={0} onChange={vi.fn()} />);
    expect(screen.queryByRole('button', { name: /复制/ })).toBeNull();
    expect(screen.getByLabelText('API 密钥')).toHaveProperty('placeholder', '输入上游 API 密钥');
    expect(fetcher).not.toHaveBeenCalled(); expect(writeText).not.toHaveBeenCalled();
  });

  it.each(['http', 'clipboard'])('allows retry after a %s failure without exposing the key or changing the draft', async failure => {
    const fetcher = vi.fn().mockImplementation(() => Promise.resolve(json({ key: 'synthetic-private-response' })));
    if (failure === 'http') fetcher.mockResolvedValueOnce(json({ error: 'synthetic-private-response' }, 503));
    else writeText.mockRejectedValueOnce(new Error('synthetic-private-response'));
    vi.stubGlobal('fetch', fetcher);
    const onChange = vi.fn(); render(<Editor onChange={onChange} />);
    const copy = screen.getByRole('button', { name: '复制凭据 2 已保存的 Key' });
    fireEvent.click(copy);
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', '复制失败，请重试。');
    expect(screen.queryByText(/synthetic-private-response/)).toBeNull();
    expect(copy).toHaveProperty('disabled', false);
    fireEvent.click(copy);
    await screen.findByRole('status');
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual([path, path]);
    expect(writeText).toHaveBeenLastCalledWith('synthetic-private-response');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.getByLabelText('凭据 2 API 密钥')).toHaveProperty('value', '');
    expect(onChange).not.toHaveBeenCalled();
  });

  it('deduplicates an in-flight copy and discards its late response after unmounting', async () => {
    let resolve!: (value: Response) => void;
    const pending = new Promise<Response>(done => { resolve = done; });
    const fetcher = vi.fn().mockReturnValue(pending); vi.stubGlobal('fetch', fetcher);
    const onChange = vi.fn();
    const view = render(<Editor onChange={onChange} />);
    const copy = screen.getByRole('button', { name: '复制凭据 2 已保存的 Key' });
    fireEvent.click(copy); fireEvent.click(copy);
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(copy).toHaveProperty('disabled', true);
    const signal = fetcher.mock.calls[0][1].signal as AbortSignal;
    view.unmount();
    expect(signal.aborted).toBe(true);
    await act(async () => { resolve(json({ key: 'synthetic-late-key' })); await pending; });
    expect(writeText).not.toHaveBeenCalled(); expect(onChange).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0); expect(sessionStorage.length).toBe(0);
  });

  it('toggles password visibility with accessible names matching credential index', () => {
    const onChange = vi.fn();
    const { unmount } = render(<CredentialKeyInput channelID={12} credential={{ ...credential, api_key: 'my-secret' }} index={0} onChange={onChange} />);

    const input0 = screen.getByLabelText('API 密钥');
    expect(input0).toHaveProperty('type', 'password');
    const toggle0 = screen.getByRole('button', { name: '显示 API 密钥' });
    fireEvent.click(toggle0);
    expect(input0).toHaveProperty('type', 'text');
    expect(screen.getByRole('button', { name: '隐藏 API 密钥' })).toBeTruthy();
    unmount();

    // With index > 0
    render(<CredentialKeyInput channelID={12} credential={{ ...credential, api_key: 'my-secret' }} index={2} onChange={onChange} />);
    const input2 = screen.getByLabelText('凭据 3 API 密钥');
    expect(input2).toHaveProperty('type', 'password');
    const toggle2 = screen.getByRole('button', { name: '显示凭据 3 API 密钥' });
    fireEvent.click(toggle2);
    expect(input2).toHaveProperty('type', 'text');
    expect(screen.getByRole('button', { name: '隐藏凭据 3 API 密钥' })).toBeTruthy();
  });
});
