import { useEffect, useRef, useState } from 'react';
import { Upload } from 'lucide-react';
import { builtinProviderIcons, ProviderIcon } from './ProviderIcons';
import { Button, ErrorBox, Input, Modal } from './ui';
import './ChannelIconPicker.css';

export interface ChannelIconPickerProps {
  title?: string;
  name: string;
  baseUrl?: string;
  value?: string;
  fallbackIcon?: string;
  onClose: () => void;
  onSelect: (value: string) => Promise<void>;
}

async function readIcon(file: File): Promise<string> {
  if (!['image/png', 'image/jpeg', 'image/webp'].includes(file.type)) throw new Error('请选择 PNG、JPEG 或 WebP 图片。');
  if (file.size > 2 * 1024 * 1024) throw new Error('图片不能超过 2MB，请选择较小的图片。');
  const source = await new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => typeof reader.result === 'string' ? resolve(reader.result) : reject(new Error('读取图片失败，请重新选择。'));
    reader.onerror = reader.onabort = () => reject(new Error('读取图片失败，请重新选择。'));
    reader.readAsDataURL(file);
  });
  const image = await new Promise<HTMLImageElement>((resolve, reject) => {
    const image = new Image();
    image.onload = () => resolve(image);
    image.onerror = () => reject(new Error('无法识别这张图片，请选择其他图片。'));
    image.src = source;
  });
  const { naturalWidth: width, naturalHeight: height } = image;
  if (!width || !height || width > 8192 || height > 8192 || width * height > 32 * 1024 * 1024) {
    throw new Error('图片尺寸过大或无效，请选择较小的图片。');
  }
  const canvas = document.createElement('canvas');
  canvas.width = canvas.height = 96;
  const context = canvas.getContext('2d');
  if (!context) throw new Error('当前浏览器无法处理图片，请重试。');
  const scale = Math.min(96 / width, 96 / height);
  context.drawImage(image, (96 - width * scale) / 2, (96 - height * scale) / 2, width * scale, height * scale);
  const result = canvas.toDataURL('image/png');
  if (!result.startsWith('data:image/png;base64,')) throw new Error('图片处理失败，请选择其他图片。');
  let bytes: number;
  try { bytes = atob(result.slice('data:image/png;base64,'.length)).length; }
  catch { throw new Error('图片处理失败，请选择其他图片。'); }
  if (!bytes || bytes > 64 * 1024) throw new Error('图片处理后仍过大，请选择其他图片。');
  return result;
}

export function ChannelIconPicker({ title = '更换渠道图标', name, baseUrl, value = '', fallbackIcon, onClose, onSelect }: ChannelIconPickerProps) {
  const input = useRef<HTMLInputElement>(null);
  const working = useRef(false);
  const active = useRef(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [search, setSearch] = useState('');
  const query = search.trim().toLocaleLowerCase();
  const filteredIcons = builtinProviderIcons.filter(icon => [icon.id, icon.label, ...icon.matches].some(term => term.toLocaleLowerCase().includes(query)));
  useEffect(() => { active.current = true; return () => { active.current = false; }; }, []);

  async function apply(next: string | File) {
    if (working.current) return;
    working.current = true;
    setBusy(true);
    setError('');
    try {
      const icon = typeof next === 'string' ? next : await readIcon(next);
      if (!active.current) return;
      await onSelect(icon);
      if (active.current) onClose();
    } catch (cause) {
      if (active.current) setError(cause instanceof Error ? cause.message : '更换图标失败，请重试。');
    } finally {
      working.current = false;
      if (active.current) setBusy(false);
    }
  }

  return <Modal title={title} description={name} busy={busy} onClose={() => { if (!working.current) onClose(); }} className="channel-icon-picker" initialFocus=".channel-icon-auto">
    <div className="channel-icon-picker-body" aria-busy={busy}>
      <Button variant="secondary" className="channel-icon-auto" aria-pressed={!value} disabled={busy} onClick={() => { void apply(''); }}>
        <ProviderIcon name={name} baseUrl={baseUrl} fallbackIcon={fallbackIcon} size={22} /><span>自动匹配</span>
      </Button>
      <div className="channel-icon-search">
        <Input type="search" aria-label="搜索内置图标" placeholder="搜索名称、ID 或别名" value={search} disabled={busy} onChange={event => setSearch(event.target.value)} />
        {search && <Button variant="ghost" disabled={busy} onClick={() => setSearch('')}>清除搜索</Button>}
      </div>
      <div className="channel-icon-grid" role="group" aria-label="内置图标">
        {filteredIcons.map(icon => <Button key={icon.id} variant="ghost" className="channel-icon-choice" aria-pressed={value === icon.id} disabled={busy} onClick={() => { void apply(icon.id); }}>
          <ProviderIcon override={icon.id} size={28} /><span>{icon.label}</span>
        </Button>)}
        {!filteredIcons.length && <p role="status" className="channel-icon-empty">没有匹配的图标</p>}
      </div>
      <div className="channel-icon-upload">
        <Button variant="secondary" aria-pressed={value.startsWith('data:image/png;base64,')} disabled={busy} onClick={() => input.current?.click()}><Upload size={16} />上传图片</Button>
        <span>PNG / JPEG / WebP，最大 2MB</span>
        <input ref={input} hidden type="file" accept="image/png,image/jpeg,image/webp" aria-label="选择图标图片" disabled={busy} onChange={event => {
          const file = event.currentTarget.files?.[0];
          event.currentTarget.value = '';
          if (file) void apply(file);
        }} />
      </div>
      {busy && <p role="status" className="channel-icon-status">正在更换图标…</p>}
      {error && <ErrorBox message={error} />}
    </div>
  </Modal>;
}
