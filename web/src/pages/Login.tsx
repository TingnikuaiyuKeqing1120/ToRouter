import { useState, type FormEvent } from 'react';
import { Eye, EyeOff, KeyRound } from 'lucide-react';
import { BrandLogo } from '../components/BrandLogo';
import { api, errorMessage } from '../api';
import { Button, ErrorBox, Field, Input } from '../components/ui';

export default function Login({ onSuccess }: { onSuccess: () => void }) {
  const [key, setKey] = useState('');
  const [showKey, setShowKey] = useState(false);
  const [remember, setRemember] = useState(false);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault(); setError('');
    setBusy(true);
    try {
      await api('/auth/login', { method: 'POST', body: JSON.stringify({ key: key.trim(), remember }) });
      setKey(''); onSuccess();
    } catch (e) {
      setError(errorMessage(e));
    } finally { setBusy(false); }
  }
  const isLight = typeof document !== 'undefined' && (document.documentElement.classList.contains('light') || document.documentElement.getAttribute('data-theme') === 'light');
  return <div className="login-page"><form onSubmit={submit} className="login-card">
    <div className="login-brand"><span className="login-logo"><BrandLogo size={56} theme={isLight ? 'light' : 'dark'} /></span><div><h1>to-router 控制台</h1><p>个人模型路由器管理入口</p></div></div>
    <p className="login-intro">输入管理员密钥以继续。在服务器上运行 <code>.\torouter.exe -show-key</code> 查看。管理员密钥仅用于登录控制台，不是 Agent / 客户端调用模型的 API Key。</p>
    <div className="space-y-5">
      {error && <ErrorBox message={error} />}
      <Field label="管理员密钥"><div className="relative"><KeyRound size={15} className="absolute left-3 top-3 text-[var(--color-text-muted)] pointer-events-none" /><Input type={showKey ? 'text' : 'password'} required autoFocus autoComplete="current-password" className="!pl-9 !pr-10" placeholder="粘贴管理员密钥" value={key} disabled={busy} onChange={e => setKey(e.target.value)} /><button type="button" className="absolute right-3 top-1/2 -translate-y-1/2 text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)] transition-colors p-1" onClick={() => setShowKey(v => !v)} aria-label={showKey ? '隐藏密钥' : '显示密钥'} title={showKey ? '隐藏密钥' : '显示密钥'}>{showKey ? <EyeOff size={16} /> : <Eye size={16} />}</button></div></Field>
      <label className="flex items-center gap-3 text-sm"><input type="checkbox" className="checkbox" checked={remember} disabled={busy} onChange={e => setRemember(e.target.checked)} />下次自动登录（30 天）</label>
      <Button type="submit" className="w-full" disabled={busy || !key.trim()}>{busy ? '正在验证…' : '进入控制台'}</Button>
    </div>
    <p className="login-footnote">管理员密钥不写入浏览器存储。默认使用最长 12 小时的浏览器会话；勾选后使用 30 天登录会话。退出登录会撤销当前会话。</p>
  </form></div>;
}
