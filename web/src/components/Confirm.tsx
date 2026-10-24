import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { Button, Modal } from './ui';

interface ConfirmOptions {
  title: string;
  description: string;
  confirmLabel: string;
  cancelLabel?: string;
  saveLabel?: string;
}
type Decision = 'confirm' | 'save' | 'cancel';
type Ask = (options: ConfirmOptions) => Promise<Decision>;
type Confirm = (options: ConfirmOptions) => Promise<boolean>;
const ConfirmContext = createContext<Ask | null>(null);
const SaveDecisionContext = createContext<() => boolean>(() => false);

// Read synchronously from timers, including those owned by a containing editor.
export function usePendingSaveDecision() { return useContext(SaveDecisionContext); }

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [options, setOptions] = useState<ConfirmOptions | null>(null);
  const pending = useRef<((answer: Decision) => void) | null>(null);
  const pendingSaveDecision = useRef(false);
  const hasPendingSaveDecision = useCallback(() => pendingSaveDecision.current, []);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; const resolve = pending.current; pending.current = null; pendingSaveDecision.current = false; resolve?.('cancel'); };
  }, []);
  const confirm = useCallback<Ask>(next => {
    // A second caller must not share a positive answer and repeat the action.
    if (!mounted.current || pending.current) return Promise.resolve('cancel');
    return new Promise(resolve => { pending.current = resolve; pendingSaveDecision.current = !!next.saveLabel; setOptions(next); });
  }, []);
  function finish(answer: Decision) {
    const resolve = pending.current;
    if (!resolve) return;
    pending.current = null; pendingSaveDecision.current = false; setOptions(null); resolve(answer);
  }
  return <ConfirmContext.Provider value={confirm}><SaveDecisionContext.Provider value={hasPendingSaveDecision}>{children}{options && <Modal title={options.title} description={options.description} initialFocus="[data-confirm-safe]" onClose={() => finish('cancel')}><div className="modal-footer"><Button variant="danger" onClick={() => finish('confirm')}>{options.confirmLabel}</Button><Button data-confirm-safe onClick={() => finish(options.saveLabel ? 'save' : 'cancel')}>{options.saveLabel || options.cancelLabel || '取消'}</Button></div></Modal>}</SaveDecisionContext.Provider></ConfirmContext.Provider>;
}

function useDecision(): Ask {
  const confirm = useContext(ConfirmContext);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  if (!confirm) throw new Error('useConfirm requires ConfirmProvider');
  return useCallback(async options => {
    const answer = await confirm(options);
    return mounted.current ? answer : 'cancel';
  }, [confirm]);
}

export function useConfirm(): Confirm {
  const ask = useDecision();
  return useCallback(async options => (await ask(options)) === 'confirm', [ask]);
}

export type SaveOrDiscardOptions = Partial<Pick<ConfirmOptions, 'title' | 'description' | 'confirmLabel' | 'saveLabel'>>;
export function useSaveOrDiscard() {
  const ask = useDecision();
  return useCallback((options?: SaveOrDiscardOptions) => ask({
    title: '保存更改？',
    description: '关闭此提示可继续编辑。',
    confirmLabel: '放弃更改',
    saveLabel: '确认保存',
    ...options,
  }), [ask]);
}
