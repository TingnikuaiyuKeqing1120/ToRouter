import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type ButtonHTMLAttributes, type InputHTMLAttributes, type ReactNode } from 'react';
import { AlertCircle, AlertTriangle, CheckCircle2, ChevronDown, Info, Loader2, X } from 'lucide-react';
import type { ChannelDiagnostic } from '../api';
import { MascotIllustration, type MascotVariant } from './MascotIllustration';
export { Skeleton, SkeletonTable, SkeletonCard, SkeletonMetrics } from './Skeleton';
export { MascotIllustration, type MascotVariant } from './MascotIllustration';

export function Button({ type = 'button', variant = 'primary', className = '', children, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: 'primary' | 'secondary' | 'ghost' | 'danger' }) {
  const variants = { primary: 'btn-primary', secondary: 'btn-secondary', ghost: 'btn-ghost', danger: 'btn-danger' };
  return <button type={type} className={`btn ${variants[variant]} ${className}`} {...props}>{children}</button>;
}
export function Input(props: InputHTMLAttributes<HTMLInputElement>) { return <input {...props} className={`input ${props.className || ''}`} />; }
export function Field({ label, hint, children, className = '' }: { label: string; hint?: string; children: ReactNode; className?: string }) {
  return <label className={`field ${className}`}><span className="field-label">{label}</span>{children}{hint && <span className="field-hint">{hint}</span>}</label>;
}
export function Badge({ children, tone = 'neutral', className = '' }: { children: ReactNode; tone?: 'neutral' | 'teal' | 'red' | 'amber' | 'cyan' | 'blue'; className?: string }) {
  return <span className={`badge badge-${tone} ${className}`}>{children}</span>;
}
export function ErrorBox({ message, retry }: { message: string; retry?: () => void }) {
  return <div role="alert" className="error-box"><AlertCircle size={18} className="shrink-0" /><span className="flex-1 break-words">{message}</span>{retry && <Button variant="ghost" onClick={retry}>重试</Button>}</div>;
}
export function Diagnostic({ diagnostic }: { diagnostic: ChannelDiagnostic }) {
  const [copyMessage, setCopyMessage] = useState('');
  async function copy() {
    try { await navigator.clipboard.writeText(diagnostic.raw || ''); setCopyMessage('原始响应已复制'); }
    catch { setCopyMessage('复制失败，请手动选择原始响应复制。'); }
  }
  return <section className="space-y-3 rounded-lg border border-red-500/20 bg-red-500/10 p-4 text-sm" aria-label="诊断信息">
    <p role="alert" className="break-words text-red-400">{diagnostic.message}</p>
    <p className="break-words text-[var(--color-text-secondary)]">建议：{diagnostic.suggestion}</p>
    <p className="text-xs text-[var(--color-text-muted)]">来源：{diagnostic.source} · 代码：{diagnostic.code}{diagnostic.upstream_status != null && ` · 上游 HTTP ${diagnostic.upstream_status}`}</p>
    {diagnostic.raw != null && <details><summary className="cursor-pointer text-xs">查看原始响应{diagnostic.truncated ? '（已截断）' : ''}</summary><pre className="mt-3 max-h-60 overflow-auto whitespace-pre-wrap break-words rounded bg-[var(--color-surface-2)] p-3 text-xs border border-[var(--color-border)]">{diagnostic.raw}</pre><Button type="button" variant="ghost" onClick={copy}>复制原始响应</Button>{copyMessage && <p role="status" className="text-xs">{copyMessage}</p>}</details>}
  </section>;
}
export function Loading() { return <div role="status" className="state-box"><Loader2 className="animate-spin text-[var(--brand-blue)]" size={24} /><span>正在加载数据…</span></div>; }
export function Empty({
  title = '暂无数据',
  description,
  action,
  variant,
}: {
  title?: string;
  description?: string;
  action?: ReactNode;
  variant?: MascotVariant;
}) {
  const resolvedVariant: MascotVariant =
    variant ||
    (title?.includes('渠道')
      ? title.includes('未找到')
        ? 'search'
        : 'channel'
      : title?.includes('请求') || title?.includes('日志')
      ? 'log'
      : title?.includes('Key') || title?.includes('密钥')
      ? 'key'
      : title?.includes('搜索') || title?.includes('未找到') || title?.includes('匹配')
      ? 'search'
      : title?.includes('模型')
      ? 'model'
      : title?.includes('用量') || title?.includes('记录')
      ? 'chart'
      : 'default');

  return (
    <div className="state-box">
      <MascotIllustration variant={resolvedVariant} />
      {title && <h3 className="font-semibold text-[var(--color-text-primary)]">{title}</h3>}
      {description && <p className="max-w-sm text-center text-sm text-[var(--color-text-muted)]">{description}</p>}
      {action}
    </div>
  );
}
export function PageHeader({ eyebrow, title, description, actions }: { eyebrow?: string; title: string; description?: string; actions?: ReactNode }) {
  const trimmedEyebrow = eyebrow?.trim();
  return <header className="page-header"><div>{trimmedEyebrow ? <p className="eyebrow">{trimmedEyebrow}</p> : null}<h1>{title}</h1>{description && <p className="page-description">{description}</p>}</div>{actions && <div className="flex flex-wrap gap-2">{actions}</div>}</header>;
}
export function Select({ children, ...props }: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return <span className="relative block"><select {...props} className={`input appearance-none pr-9 ${props.className || ''}`}>{children}</select><ChevronDown size={15} className="pointer-events-none absolute right-3 top-3 text-[var(--color-text-muted)]" /></span>;
}
let openModalCount = 0;
let bodyOverflowBeforeModals = '';

export function Modal({ title, description, children, onClose, busy = false, wide = false, initialFocus, className = '' }: { title: string; description?: string; children: ReactNode; onClose: () => void; busy?: boolean; wide?: boolean; initialFocus?: string; className?: string }) {
  const ref = useRef<HTMLDialogElement>(null);
  const id = useId();
  const descId = description ? `${id}-desc` : undefined;
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const dialog = ref.current;
    dialog?.showModal();
    if (initialFocus) dialog?.querySelector<HTMLElement>(initialFocus)?.focus();
    if (openModalCount++ === 0) bodyOverflowBeforeModals = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      dialog?.close();
      if (--openModalCount === 0) document.body.style.overflow = bodyOverflowBeforeModals;
      if (previous?.isConnected) previous.focus();
    };
  }, []);
  return <dialog ref={ref} aria-labelledby={id} aria-describedby={descId} className={`modal ${wide ? 'modal-wide' : ''} ${className}`} onCancel={e => { e.preventDefault(); if (!busy) onClose(); }} onClick={e => { if (e.target === e.currentTarget && !busy) onClose(); }}>
    <div className="modal-content"><div className="modal-heading"><div><h2 id={id}>{title}</h2>{description && <p id={descId}>{description}</p>}</div><Button variant="ghost" aria-label="关闭弹窗" disabled={busy} onClick={onClose}><X size={20} /></Button></div>{children}</div>
  </dialog>;
}

export type ToastType = 'success' | 'error' | 'warning' | 'info';
export interface ToastOptions {
  message?: string;
  text?: string;
  type?: ToastType;
}
export interface ToastFn {
  (textOrOptions: string | ToastOptions, type?: ToastType): void;
  success: (message: string) => void;
  error: (message: string) => void;
  warning: (message: string) => void;
  info: (message: string) => void;
}

interface ToastState {
  text: string;
  type: ToastType;
}

const defaultToastFn: ToastFn = Object.assign(
  (() => {}) as (textOrOptions: string | ToastOptions, type?: ToastType) => void,
  {
    success: () => {},
    error: () => {},
    warning: () => {},
    info: () => {},
  }
);

const ToastContext = createContext<ToastFn>(defaultToastFn);
export const useToast = () => useContext(ToastContext);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toast, setToast] = useState<ToastState | null>(null);
  useEffect(() => {
    if (!toast) return;
    const timer = setTimeout(() => setToast(null), 4000);
    return () => clearTimeout(timer);
  }, [toast]);

  const showToast = useCallback((textOrOptions: string | ToastOptions, explicitType?: ToastType) => {
    let text = '';
    let type = explicitType;
    if (typeof textOrOptions === 'string') {
      text = textOrOptions;
    } else if (textOrOptions && typeof textOrOptions === 'object') {
      text = textOrOptions.message || textOrOptions.text || '';
      type = type || textOrOptions.type;
    }
    if (!text) return;
    if (!type) {
      if (text.includes('失败') || text.includes('错误') || text.toLowerCase().includes('error')) {
        type = 'error';
      } else if (text.includes('失效') || text.includes('警告')) {
        type = 'warning';
      } else {
        type = 'success';
      }
    }
    setToast({ text, type });
  }, []);

  const toastContextValue = useMemo<ToastFn>(() => {
    const fn = ((textOrOptions: string | ToastOptions, type?: ToastType) => showToast(textOrOptions, type)) as ToastFn;
    fn.success = (msg: string) => showToast(msg, 'success');
    fn.error = (msg: string) => showToast(msg, 'error');
    fn.warning = (msg: string) => showToast(msg, 'warning');
    fn.info = (msg: string) => showToast(msg, 'info');
    return fn;
  }, [showToast]);

  const icons: Record<ToastType, { icon: typeof CheckCircle2; className: string }> = {
    success: { icon: CheckCircle2, className: 'text-[var(--color-success)]' },
    error: { icon: AlertCircle, className: 'text-[var(--color-danger)]' },
    warning: { icon: AlertTriangle, className: 'text-[var(--color-warning)]' },
    info: { icon: Info, className: 'text-[var(--brand-blue)]' },
  };

  const currentIcon = toast ? icons[toast.type] || icons.success : null;
  const IconComponent = currentIcon?.icon;

  return (
    <ToastContext.Provider value={toastContextValue}>
      {children}
      {toast && IconComponent && (
        <div
          role={toast.type === 'error' ? 'alert' : 'status'}
          aria-live={toast.type === 'error' ? 'assertive' : 'polite'}
          className={`toast toast-${toast.type}`}
        >
          <IconComponent size={18} className={`shrink-0 ${currentIcon.className}`} />
          <span className="flex-1 break-words">{toast.text}</span>
          <button type="button" onClick={() => setToast(null)} aria-label="关闭通知">
            <X size={16} />
          </button>
        </div>
      )}
    </ToastContext.Provider>
  );
}
export function DateFields({ from, to, setFrom, setTo }: { from: string; to: string; setFrom: (x: string) => void; setTo: (x: string) => void }) {
  return <><Field label="开始日期"><Input type="date" value={from} max={to || undefined} onChange={e => setFrom(e.target.value)} /></Field><Field label="结束日期"><Input type="date" value={to} min={from || undefined} onChange={e => setTo(e.target.value)} /></Field></>;
}
