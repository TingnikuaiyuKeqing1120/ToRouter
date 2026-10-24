import { useEffect, useRef, useState } from 'react';
import { Check, Copy, Eye, EyeOff, Loader2 } from 'lucide-react';
import { api, type CredentialItem } from '../api';
import { Button, Input } from './ui';

/** The editable replacement is separate from the saved key being identified/copied. */
export function CredentialKeyInput({ channelID, credential, index, onChange }: {
  channelID: number; credential: CredentialItem; index: number; onChange: (value: string) => void;
}) {
  const [copying, setCopying] = useState(false);
  const [copied, setCopied] = useState(false);
  const [showKey, setShowKey] = useState(false);
  const [error, setError] = useState('');
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);
  const hasSavedKey = channelID > 0 && credential.has_key;
  const mask = credential.masked_key || '••••';

  async function copySavedKey() {
    if (!hasSavedKey || controller.current) return;
    const request = new AbortController(); controller.current = request;
    setCopying(true); setCopied(false); setError('');
    try {
      const result = await api<{ key: string }>(`/channels/${channelID}/credentials/${encodeURIComponent(credential.id)}/key`, { cache: 'no-store', signal: request.signal });
      if (request.signal.aborted) return;
      try { await navigator.clipboard.writeText(result.key); }
      finally { result.key = ''; }
      if (!request.signal.aborted) setCopied(true);
    } catch {
      // Neither a secret response nor clipboard contents belong in diagnostics.
      if (!request.signal.aborted) setError('复制失败，请重试。');
    } finally {
      if (controller.current === request) controller.current = null;
      if (!request.signal.aborted) setCopying(false);
    }
  }

  return <div className="channel-editor-key">
    <div className={`channel-editor-key-field ${hasSavedKey ? 'has-copy' : ''}`}>
      <Input type={showKey ? 'text' : 'password'} autoComplete="new-password" aria-label={index === 0 ? 'API 密钥' : `凭据 ${index + 1} API 密钥`} placeholder={hasSavedKey ? `已配置 ${mask} · 留空保留` : '输入上游 API 密钥'} value={credential.api_key || ''} onChange={event => onChange(event.target.value)} />
      <div className="channel-editor-key-actions">
        <Button variant="ghost" aria-label={index === 0 ? (showKey ? '隐藏 API 密钥' : '显示 API 密钥') : (showKey ? `隐藏凭据 ${index + 1} API 密钥` : `显示凭据 ${index + 1} API 密钥`)} title={showKey ? '隐藏密钥' : '显示密钥'} onClick={() => setShowKey(v => !v)}>{showKey ? <EyeOff size={16} /> : <Eye size={16} />}</Button>
        {hasSavedKey && <Button variant="ghost" aria-label={`复制凭据 ${index + 1} 已保存的 Key`} title="复制已保存的 Key" disabled={copying} onClick={() => void copySavedKey()}>{copying ? <Loader2 size={16} className="animate-spin" /> : copied ? <Check size={16} /> : <Copy size={16} />}</Button>}
      </div>
    </div>
    {hasSavedKey && credential.api_key && <span className="channel-editor-key-note">已保存 {mask}；输入内容将在保存后替换</span>}
    {copied && <span role="status" className="sr-only">已复制已保存的 Key</span>}
    {error && <span role="alert" className="channel-editor-key-error">{error}</span>}
  </div>;
}
