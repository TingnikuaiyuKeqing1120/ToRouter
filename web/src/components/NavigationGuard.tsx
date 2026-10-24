import { createContext, useContext, useEffect, useRef } from 'react';

export class NavigationCancelled extends Error {}

export type BeforeLeave = () => Promise<void>;
export const NavigationGuard = createContext<{ register: (handler: BeforeLeave) => () => void }>({ register: () => () => {} });
export function useBeforeLeave(handler: BeforeLeave) {
  const latest = useRef(handler); latest.current = handler;
  const { register } = useContext(NavigationGuard);
  useEffect(() => register(() => latest.current()), [register]);
}
