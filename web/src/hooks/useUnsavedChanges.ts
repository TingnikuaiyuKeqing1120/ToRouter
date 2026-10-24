import { useEffect, useRef } from 'react';
import { useSaveOrDiscard, type SaveOrDiscardOptions } from '../components/Confirm';
import { NavigationCancelled, useBeforeLeave } from '../components/NavigationGuard';

/** A save must report success explicitly; validation/API failures keep the editor open. */
export function useUnsavedChanges({ dirty, busy = false, save, discard, prompt }: { dirty: boolean | (() => boolean); busy?: boolean | (() => boolean); save: () => Promise<boolean>; discard?: () => void; prompt?: SaveOrDiscardOptions }) {
  const ask = useSaveOrDiscard();
  const pending = useRef(false);
  async function canLeave() {
    if ((typeof busy === 'function' ? busy() : busy) || pending.current) return false;
    if (!(typeof dirty === 'function' ? dirty() : dirty)) return true;
    pending.current = true;
    try {
      const decision = await ask(prompt);
      if (decision === 'confirm') { discard?.(); return true; }
      return decision === 'save' && await save();
    } finally { pending.current = false; }
  }
  useBeforeLeave(async () => { if (!await canLeave()) throw new NavigationCancelled(); });
  useEffect(() => {
    if (!(typeof dirty === 'function' ? dirty() : dirty) && !(typeof busy === 'function' ? busy() : busy)) return;
    const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ''; };
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [dirty, busy]);
  return canLeave;
}
