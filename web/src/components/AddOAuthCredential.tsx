import { useState } from 'react';
import { api, oauthRoutes, type Channel, type CredentialType } from '../api';
import type { PluginAccess } from '../types/plugins';
import { Modal, Button } from './ui';
import { OAuthAuthorize } from './oauth';
import { OAuthTransfer } from './OAuthTransfer';
import { KeyDiscovery } from './KeyDiscovery';

const officialSources: Record<string, string> = { codex_oauth: 'source-codex', claude_oauth: 'source-claude', copilot_oauth: 'source-copilot' };
export function AddOAuthCredential({ channel, access, onClose, onChanged, resumeFlow, onStarted }: {
  channel: Channel; access?: PluginAccess; onClose: () => void; onChanged: () => void;
  resumeFlow?: string; onStarted?: (flow: string | null) => void;
}) {
  const type = channel.credential_type as Exclude<CredentialType, 'api_key'>;
  const market = type.startsWith('market:');
  const [tab, setTab] = useState('login'), [busy, setBusy] = useState(false);
  const source = officialSources[type];
  function changed() { onChanged(); }
  async function cancelBlocked() {
    if (!resumeFlow || busy) return;
    setBusy(true);
    try { await api(`${oauthRoutes[type]}/cancel?flow=${encodeURIComponent(resumeFlow)}`, { method: 'POST' }); }
    catch { /* An expired flow is already cancelled. */ }
    finally { setBusy(false); onStarted?.(null); }
  }
  return <Modal title={`向「${channel.name}」添加账号 / 工作区`} onClose={onClose} busy={busy} wide>
    <p className="text-sm text-[var(--color-text-secondary)]">目标固定为「{channel.name}」。保存后请按需获取并启用模型。</p>
    {!market && <div role="tablist" aria-label="添加凭证方式" className="segmented my-3">{[['login', '官方登录'], ['paste', '粘贴凭证'], ['local', '从本地官方工具导入']].map(([key, label]) => <button key={key} type="button" role="tab" aria-selected={tab === key} disabled={busy} className={tab === key ? 'active' : ''} onClick={() => setTab(key)}>{label}</button>)}</div>}
    {tab === 'login' && <div role="tabpanel">{access?.allowed === false ? <><p role="status">{access.message || '接入插件未启用，暂不可登录。'}</p>{resumeFlow && <Button disabled={busy} onClick={() => void cancelBlocked()}>取消授权</Button>}</> : <OAuthAuthorize type={type} authMethods={access?.authMethods} operation="add_credential" channelID={channel.id} resumeFlow={resumeFlow} onStarted={onStarted} onResumeGone={() => onStarted?.(null)} onDone={() => { onStarted?.(null); changed(); onClose(); }} />}</div>}
    {tab === 'paste' && <div role="tabpanel"><OAuthTransfer mode="import" embedded credentialType={type} targetChannelID={channel.id} targetName={channel.name} onBusy={setBusy} onClose={onClose} onChanged={changed} /></div>}
    {tab === 'local' && <div role="tabpanel">{!source && <p className="my-3 text-sm">此对象的官方本地工具读取尚未适配。可使用官方登录、粘贴原生凭证，或选择下方其他已支持工具。</p>}{type === 'copilot_oauth' && <p className="my-3 text-sm">官方读取支持 apps.json / hosts.json 文件，暂不支持编辑器加密存储。</p>}<KeyDiscovery embedded initialSourceID={source || 'source-magpie'} targetChannelID={channel.id} credentialType={type} onBusy={setBusy} onClose={onClose} onChanged={changed} /></div>}
    {tab === 'login' && <div className="modal-footer"><Button variant="secondary" onClick={onClose}>关闭</Button></div>}
  </Modal>;
}
