// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ChannelIconPicker } from './ChannelIconPicker';
import { builtinProviderIcons, DeepSeekIcon, ProviderIcon, XiaomiIcon } from './ProviderIcons';

const png = 'data:image/png;base64,aWNvbg==';
let dimensions: [number, number];
let decode: 'success' | 'error' | 'pending';
let images: { onload: (() => void) | null; onerror: (() => void) | null }[];
let drawImage: ReturnType<typeof vi.fn>;

beforeEach(() => {
  dimensions = [400, 200]; decode = 'success'; images = []; drawImage = vi.fn();
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.setAttribute('open', ''); });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.removeAttribute('open'); });
  vi.stubGlobal('Image', vi.fn(function () {
    const image = { naturalWidth: dimensions[0], naturalHeight: dimensions[1], onload: null as (() => void) | null, onerror: null as (() => void) | null };
    Object.defineProperty(image, 'src', { set() { if (decode !== 'pending') queueMicrotask(() => decode === 'success' ? image.onload?.() : image.onerror?.()); } });
    images.push(image);
    return image;
  }));
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({ drawImage } as unknown as CanvasRenderingContext2D);
  vi.spyOn(HTMLCanvasElement.prototype, 'toDataURL').mockReturnValue(png);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function setup(value = '', onSelect = vi.fn(async (_value: string) => {})) {
  const onClose = vi.fn();
  const view = render(<ChannelIconPicker name="测试渠道" baseUrl="https://api.deepseek.com" value={value} onSelect={onSelect} onClose={onClose} />);
  const upload = (file = new File(['image'], 'icon.png', { type: 'image/png' })) => {
    fireEvent.change(screen.getByLabelText('选择图标图片'), { target: { files: [file] } });
    return file;
  };
  return { ...view, onSelect, onClose, upload };
}

describe('ChannelIconPicker', () => {
  it('retains the 11 original builtins, offers 52 choices, and focuses automatic matching', () => {
    setup('generic');
    expect(builtinProviderIcons.slice(0, 11).map(icon => icon.id)).toEqual(['generic', 'openai', 'anthropic', 'deepseek', 'moonshot', 'zhipu', 'minimax', 'stepfun', 'nvidia', 'openrouter', 'xiaomi']);
    expect(builtinProviderIcons).toHaveLength(52);
    expect(new Set(builtinProviderIcons.map(icon => icon.id)).size).toBe(52);
    for (const icon of builtinProviderIcons) expect(screen.getByRole('button', { name: icon.label })).toBeTruthy();
    expect(screen.getByRole('button', { name: '通用' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: '自动匹配' }).getAttribute('aria-pressed')).toBe('false');
    expect(document.activeElement).toBe(screen.getByRole('button', { name: '自动匹配' }));
  });

  it.each([['OpenAI', 'openai'], ['Claude Code', 'claudecode'], ['Cursor', 'cursor'], ['自动匹配', '']])('awaits %s, blocks duplicate selection and all close methods until saved', async (label, value) => {
    let complete!: () => void;
    const selected = vi.fn(() => new Promise<void>(resolve => { complete = resolve; }));
    const { onClose } = setup('deepseek', selected);
    fireEvent.click(screen.getByRole('button', { name: label }));
    fireEvent.click(screen.getByRole('button', { name: label }));
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    const dialog = screen.getByRole('dialog');
    fireEvent(dialog, new Event('cancel', { cancelable: true }));
    fireEvent.click(dialog);
    expect(selected.mock.calls).toEqual([[value]]);
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '上传图片' })).toHaveProperty('disabled', true);
    await act(async () => complete());
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('searches names, IDs and aliases, reports no match, and clears without changing the selection', () => {
    const { onSelect, onClose } = setup('cursor');
    const search = screen.getByRole('searchbox', { name: '搜索内置图标' });
    for (const query of ['GitHub', 'githubcopilot', 'COPILOT']) {
      fireEvent.change(search, { target: { value: query } });
      expect(screen.getByRole('button', { name: 'GitHub Copilot' })).toBeTruthy();
      expect(screen.queryByRole('button', { name: 'OpenAI' })).toBeNull();
    }
    fireEvent.change(search, { target: { value: '不存在的图标' } });
    expect(screen.getByRole('status').textContent).toBe('没有匹配的图标');
    fireEvent.click(screen.getByRole('button', { name: '清除搜索' }));
    expect(search).toHaveProperty('value', '');
    expect(screen.getByRole('button', { name: 'Cursor' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: 'OpenAI' })).toBeTruthy();
    expect(onSelect).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('retains the panel on save failure and allows retry', async () => {
    const selected = vi.fn().mockRejectedValueOnce(new Error('保存失败')).mockResolvedValueOnce(undefined);
    const { onClose } = setup('', selected);
    fireEvent.click(screen.getByRole('button', { name: 'OpenAI' }));
    expect((await screen.findByRole('alert')).textContent).toContain('保存失败');
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'OpenAI' }));
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(selected.mock.calls).toEqual([['openai'], ['openai']]);
  });

  it.each(['image/png', 'image/jpeg', 'image/webp'])('contains %s on a transparent square, applies once, and waits for save before closing', async type => {
    let complete!: () => void;
    const selected = vi.fn(() => new Promise<void>(resolve => { complete = resolve; }));
    const { upload, onClose } = setup('', selected);
    upload(new File(['image'], 'photo', { type }));
    await waitFor(() => expect(selected.mock.calls).toEqual([[png]]));
    expect(drawImage).toHaveBeenCalledWith(images[0], 0, 24, 96, 48);
    expect(HTMLCanvasElement.prototype.toDataURL).toHaveBeenCalledWith('image/png');
    const canvas = vi.mocked(HTMLCanvasElement.prototype.getContext).mock.contexts[0] as HTMLCanvasElement;
    expect([canvas.width, canvas.height]).toEqual([96, 96]);
    expect(screen.getByLabelText('选择图标图片')).toHaveProperty('value', '');
    fireEvent.click(screen.getByRole('button', { name: '自动匹配' }));
    expect(selected).toHaveBeenCalledTimes(1);
    expect(onClose).not.toHaveBeenCalled();
    await act(async () => complete());
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('blocks closing during image decoding, then continues only once', async () => {
    decode = 'pending';
    const { upload, onClose, onSelect } = setup();
    upload();
    await waitFor(() => expect(images).toHaveLength(1));
    fireEvent.click(screen.getByRole('dialog'));
    fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true }));
    expect(screen.getByRole('button', { name: '关闭弹窗' })).toHaveProperty('disabled', true);
    expect(onClose).not.toHaveBeenCalled();
    expect(onSelect).not.toHaveBeenCalled();
    await act(async () => images[0].onload?.());
    expect(onSelect.mock.calls).toEqual([[png]]);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('keeps an upload save failure open and retries the same file without a false success', async () => {
    const selected = vi.fn().mockRejectedValueOnce(new Error('保存失败')).mockResolvedValueOnce(undefined);
    const { upload, onClose } = setup('', selected); const file = upload();
    expect((await screen.findByRole('alert')).textContent).toContain('保存失败');
    expect(onClose).not.toHaveBeenCalled();
    upload(file);
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(selected.mock.calls).toEqual([[png], [png]]);
  });

  it('does not apply a late decoded upload after the picker unmounts', async () => {
    decode = 'pending';
    const { upload, unmount, onSelect, onClose } = setup();
    upload();
    await waitFor(() => expect(images).toHaveLength(1));
    unmount();
    await act(async () => images[0].onload?.());
    expect(onSelect).not.toHaveBeenCalled(); expect(onClose).not.toHaveBeenCalled();
  });

  it.each([
    ['unsupported', () => new File(['<svg/>'], 'icon.svg', { type: 'image/svg+xml' }), 'PNG'],
    ['over 2MB', () => new File([new Uint8Array(2 * 1024 * 1024 + 1)], 'big.png', { type: 'image/png' }), '2MB'],
  ])('rejects %s before decoding', async (_label, file, error) => {
    const { upload, onSelect, onClose } = setup(); upload(file());
    expect((await screen.findByRole('alert')).textContent).toContain(error);
    expect(images).toHaveLength(0); expect(onSelect).not.toHaveBeenCalled(); expect(onClose).not.toHaveBeenCalled();
  });

  it.each([[0, 10], [8193, 10], [6000, 6000]])('rejects invalid or excessive decoded dimensions %s × %s', async (width, height) => {
    dimensions = [width, height];
    const { upload, onSelect, onClose } = setup(); upload();
    expect((await screen.findByRole('alert')).textContent).toContain('尺寸');
    expect(drawImage).not.toHaveBeenCalled(); expect(onSelect).not.toHaveBeenCalled(); expect(onClose).not.toHaveBeenCalled();
  });

  it('keeps a bad-image error open and lets the same file be selected again', async () => {
    decode = 'error';
    const { upload, onSelect, onClose } = setup(); const file = upload();
    expect((await screen.findByRole('alert')).textContent).toContain('无法识别');
    expect(onClose).not.toHaveBeenCalled(); expect(onSelect).not.toHaveBeenCalled();
    decode = 'success'; upload(file);
    await waitFor(() => expect(onSelect.mock.calls).toEqual([[png]]));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it.each(['error', 'abort'])('handles a FileReader %s without applying', async event => {
    vi.spyOn(FileReader.prototype, 'readAsDataURL').mockImplementation(function (this: FileReader) {
      this.dispatchEvent(new ProgressEvent(event));
    });
    const { upload, onSelect, onClose } = setup(); upload();
    expect((await screen.findByRole('alert')).textContent).toContain('读取图片失败');
    expect(onSelect).not.toHaveBeenCalled(); expect(onClose).not.toHaveBeenCalled();
  });

  it.each(['missing context', 'canvas error', 'wrong output', 'invalid base64', 'too large'])('handles %s without applying', async failure => {
    if (failure === 'missing context') vi.mocked(HTMLCanvasElement.prototype.getContext).mockReturnValue(null);
    if (failure === 'canvas error') vi.mocked(HTMLCanvasElement.prototype.toDataURL).mockImplementation(() => { throw new Error('图片处理失败'); });
    if (failure === 'wrong output') vi.mocked(HTMLCanvasElement.prototype.toDataURL).mockReturnValue('data:,');
    if (failure === 'invalid base64') vi.mocked(HTMLCanvasElement.prototype.toDataURL).mockReturnValue('data:image/png;base64,?');
    if (failure === 'too large') vi.mocked(HTMLCanvasElement.prototype.toDataURL).mockReturnValue(`data:image/png;base64,${btoa('a'.repeat(64 * 1024 + 1))}`);
    const { upload, onSelect, onClose } = setup(); upload();
    await screen.findByRole('alert');
    expect(onSelect).not.toHaveBeenCalled(); expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'OpenAI' })).toHaveProperty('disabled', false);
  });
});

describe('ProviderIcon overrides', () => {
  it.each([
    ['Claude Code', 'https://api.anthropic.com', 'claudecode-color.svg'],
    ['Gemini CLI', 'https://generativelanguage.googleapis.com', 'geminicli-color.svg'],
    ['OpenAI Codex', '', 'codex-color.svg'],
    ['Qwen 通义', '', 'qwen-color.svg'],
    ['custom', 'https://generativelanguage.gemini.example.com/v1', 'gemini-color.svg'],
  ])('matches %s with a bundled icon and explicit tool priority', (name, baseUrl, file) => {
    const { container } = render(<ProviderIcon name={name} baseUrl={baseUrl} size={24} />);
    const image = container.querySelector('img')!;
    expect(image.getAttribute('src')).toContain(file);
    expect(image.width).toBe(24);
    expect(image.alt).toBe('');
  });
  it.each([
    ['metadata gateway', 'https://metadata.example.com/v1'],
    ['custom', 'https://example.com/cursor?model=gemini'],
    ['custom', 'https://cursor:gemini@example.com/v1'],
    ['recursive tool', 'not-a-url-gemini'],
  ])('does not infer new brands from unrelated names or URL parts: %s %s', (name, baseUrl) => {
    const { container } = render(<ProviderIcon name={name} baseUrl={baseUrl} />);
    expect(container.querySelector('svg[stroke="currentColor"]')).toBeTruthy();
    expect(container.querySelector('img')).toBeNull();
  });
  it('renders monochrome icons using a local currentColor mask', () => {
    const { container } = render(<ProviderIcon override="cursor" size={32} className="test-icon" />);
    const icon = container.querySelector('span')!;
    expect(icon.className).toBe('test-icon');
    expect(icon.style.width).toBe('32px');
    expect(icon.style.height).toBe('32px');
    expect(icon.style.backgroundColor.toLowerCase()).toBe('currentcolor');
    expect(icon.style.mask).toContain('cursor.svg');
    expect(container.querySelector('img')).toBeNull();
  });
  it.each(['not-a-provider', 'https://example.com/icon.svg', '//example.com/icon.png', 'data:image/svg+xml;base64,PHN2Zy8+'])('ignores unknown or remote overrides: %s', override => {
    const view = render(<DeepSeekIcon size={20} />);
    const expected = view.container.innerHTML;
    view.rerender(<ProviderIcon name="DeepSeek" override={override} />);
    expect(view.container.innerHTML).toBe(expected);
  });
  it('preserves automatic name/baseURL matching and its original priority, including unknown overrides', () => {
    const view = render(<DeepSeekIcon size={24} />); const deepseek = view.container.innerHTML;
    view.rerender(<ProviderIcon name="OpenAI DeepSeek" size={24} />); expect(view.container.innerHTML).toBe(deepseek);
    view.rerender(<ProviderIcon name="unrelated" baseUrl="https://api.deepseek.com" override="unknown" size={24} />); expect(view.container.innerHTML).toBe(deepseek);
    view.rerender(<XiaomiIcon size={24} />); const xiaomi = view.container.innerHTML;
    view.rerender(<ProviderIcon name="NVIDIA 小米" size={24} />); expect(view.container.innerHTML).toBe(xiaomi);
  });
  it('forces all builtin choices, including a generic icon on branded channels', () => {
    const view = render(<ProviderIcon />); const generic = view.container.innerHTML;
    for (const { id } of builtinProviderIcons) {
      view.rerender(<ProviderIcon override={id} />); const expected = view.container.innerHTML;
      view.rerender(<ProviderIcon name="DeepSeek" override={id} />); expect(view.container.innerHTML).toBe(expected);
      if (id === 'generic') expect(expected).toBe(generic);
    }
  });
  it('renders uploaded PNG data at the requested size with contain fitting', () => {
    const { container } = render(<ProviderIcon name="OpenAI" override={png} size={32} className="test-icon" />);
    const image = container.querySelector('img')!;
    expect(image.src).toBe(png); expect(image.alt).toBe(''); expect(image.width).toBe(32); expect(image.height).toBe(32);
    expect(image.style.objectFit).toBe('contain'); expect(image.className).toBe('test-icon');
  });
});
