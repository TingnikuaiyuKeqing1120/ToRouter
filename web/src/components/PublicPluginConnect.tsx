import { useState } from 'react';
import { Plus } from 'lucide-react';
import { api, errorMessage } from '../api';
import { Button, ErrorBox, useToast } from './ui';

export function PublicPluginConnect({ blocked, connected, onDone }: { blocked: boolean; connected: boolean; onDone: () => void }) {
  const [busy, setBusy] = useState(false), [error, setError] = useState('');
  const toast = useToast();
  async function connect() {
    if (busy || blocked) return;
    setBusy(true); setError('');
    try {
      const result = await api<{ channel_id: number; already_linked: boolean }>('/plugins/opencode-zen-free/connect', { method: 'POST' });
      toast(result.already_linked ? '公共接入已存在' : '公共接入已添加，可获取并启用模型');
      onDone();
    } catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  }
  return <div>
    {!connected && <Button variant="secondary" disabled={blocked || busy} onClick={() => void connect()}><Plus size={15} />{busy ? '正在添加…' : '添加公共接入'}</Button>}
    <p className="oauth-main-model-note">无需登录或填写密钥。添加后获取模型目录，再选择启用的模型。</p>
    {error && <ErrorBox message={error} />}
  </div>;
}
