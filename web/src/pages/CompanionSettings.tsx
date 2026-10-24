import { useEffect, useRef, useState } from 'react';
import { CompanionQuotaSettings } from '../components/CompanionQuotaSettings';
import { Sparkles } from 'lucide-react';
import { api, errorMessage } from '../api';
import { ErrorBox, PageHeader } from '../components/ui';

export default function CompanionSettings() {
  const [running, setRunning] = useState<boolean | null>(null);
  const [changing, setChanging] = useState<boolean | null>(null);
  const [readError, setReadError] = useState('');
  const [actionError, setActionError] = useState('');
  const generation = useRef(0);
  const pending = useRef<{ enabled: boolean; accepted: boolean } | null>(null);
  const timedOut = useRef<boolean | null>(null);
  const refresh = useRef<() => void>(() => {});
  const command = useRef<AbortController | null>(null);
  const deadline = useRef<ReturnType<typeof setTimeout>>();
  const mounted = useRef(false);

  useEffect(() => {
    mounted.current = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let controller: AbortController | null = null;
    const read = async () => {
      if (!mounted.current || document.hidden && !pending.current || controller) return;
      clearTimeout(timer);
      const mine = generation.current;
      const request = new AbortController(); controller = request;
      const timeout = setTimeout(() => request.abort(), 5000);
      try {
        const next = await api<{running:boolean}>('/desktop/pet', {signal:request.signal});
        if (!mounted.current || request.signal.aborted || mine !== generation.current) return;
        if (typeof next.running !== 'boolean') throw new Error('无法读取桌宠状态');
        setRunning(next.running); setReadError('');
        if (timedOut.current === next.running) { timedOut.current = null; setActionError(''); }
        if (pending.current?.accepted && pending.current.enabled === next.running) {
          pending.current = null; clearTimeout(deadline.current); setChanging(null);
        }
      } catch (e) {
        if (mounted.current && mine === generation.current) setReadError(request.signal.aborted ? '读取桌宠状态超时' : errorMessage(e));
      } finally {
        clearTimeout(timeout); controller = null;
        if (mounted.current && (!document.hidden || pending.current)) timer = setTimeout(() => void read(), 1000);
      }
    };
    refresh.current = () => void read();
    const visible = () => { if (!document.hidden || pending.current) void read(); else clearTimeout(timer); };
    document.addEventListener('visibilitychange', visible); void read();
    return () => {
      mounted.current = false; generation.current++; clearTimeout(timer); clearTimeout(deadline.current);
      controller?.abort(); command.current?.abort(); refresh.current = () => {};
      document.removeEventListener('visibilitychange', visible);
    };
  }, []);

  const toggle = async () => {
    if (running === null || pending.current || readError) return;
    const enabled = !running;
    const operation = {enabled, accepted:false}; pending.current = operation;
    timedOut.current = null;
    generation.current++; setChanging(enabled); setActionError('');
    const request = new AbortController(); command.current = request;
    deadline.current = setTimeout(() => {
      request.abort();
      if (mounted.current && pending.current === operation) {
        pending.current = null; setChanging(null);
        if (operation.accepted) timedOut.current = enabled;
        setActionError('未能确认桌宠状态，请检查后重试。');
      }
    }, 12000);
    try {
      await api(enabled ? '/desktop/launch-pet' : '/desktop/close-pet', {method:'POST',signal:request.signal});
      if (!mounted.current || pending.current !== operation) return;
      // A launch acknowledgement does not prove a visible native window.
      operation.accepted = true; refresh.current();
    } catch (e) {
      if (mounted.current && pending.current === operation) {
        pending.current = null; setChanging(null); clearTimeout(deadline.current);
        setActionError(errorMessage(e)); refresh.current();
      }
    } finally { if (command.current === request) command.current = null; }
  };

  return <div className="space-y-5">
    <PageHeader eyebrow="COMPANION" title="桌宠管理" description="蓝鲸与 Clawd 会在桌面陪伴你，展示实时请求状态。" />
    {(actionError || readError) && <ErrorBox message={actionError || readError} retry={readError ? () => refresh.current() : undefined} />}
    <section className="flex items-center justify-between gap-4 rounded-2xl border border-[var(--color-border)] bg-[var(--color-surface-1)] p-5" aria-label="桌宠开关">
      <div className="flex items-center gap-3">
        <span className="rounded-xl bg-[var(--color-surface-2)] p-2.5 text-[var(--brand-blue)]"><Sparkles size={22} /></span>
        <div><h2 className="text-sm font-semibold">桌面小宠</h2><p role="status" className="mt-1 text-xs text-[var(--color-text-muted)]">{changing !== null ? changing ? '正在开启…' : '正在关闭…' : running === null ? '正在读取状态…' : running ? '已开启' : '已关闭'}</p></div>
      </div>
      <button type="button" role="switch" aria-label="开启桌宠" aria-checked={running === true}
        className={`switch ${running ? 'switch-on' : ''}`} disabled={running === null || changing !== null || !!readError} onClick={() => void toggle()}><span /></button>
    </section>
    <CompanionQuotaSettings />
  </div>;
}
