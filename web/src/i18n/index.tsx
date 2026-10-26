import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import type { Locale, TranslationSchema } from './types';
import { zh } from './zh';
import { en } from './en';

const LOCALE_STORAGE_KEY = 'to-router.locale.v1';

const dictionaries: Record<Locale, TranslationSchema> = { zh, en };

// Helper type for nested dot notation keys: "nav.usage" | "settings.title" | ...
type NestedKeyOf<ObjectType extends object> = {
  [Key in keyof ObjectType & (string | number)]: ObjectType[Key] extends object
    ? `${Key}.${NestedKeyOf<ObjectType[Key]>}`
    : `${Key}`;
}[keyof ObjectType & (string | number)];

export type TranslationKey = NestedKeyOf<TranslationSchema>;

export interface I18nContextValue {
  locale: Locale;
  setLocale: (locale: Locale) => void;
  t: (key: TranslationKey, params?: Record<string, string | number>) => string;
}

const defaultI18nContext: I18nContextValue = {
  locale: 'zh',
  setLocale: () => {},
  t: (key, params) => {
    const str = resolvePath(zh, key) ?? key;
    return formatTemplate(str, params);
  },
};

const I18nContext = createContext<I18nContextValue>(defaultI18nContext);

export function getInitialLocale(): Locale {
  try {
    if (typeof window !== 'undefined' && window.localStorage) {
      const stored = window.localStorage.getItem(LOCALE_STORAGE_KEY);
      if (stored === 'zh' || stored === 'en') {
        return stored;
      }
    }
  } catch {
    // LocalStorage might be restricted; continue to fallback
  }

  if (typeof navigator !== 'undefined' && navigator.language) {
    return navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en';
  }

  return 'zh';
}

function resolvePath(obj: unknown, path: string): string | undefined {
  const parts = path.split('.');
  let current: unknown = obj;
  for (const part of parts) {
    if (current && typeof current === 'object' && part in current) {
      current = (current as Record<string, unknown>)[part];
    } else {
      return undefined;
    }
  }
  return typeof current === 'string' ? current : undefined;
}

export function formatTemplate(template: string, params?: Record<string, string | number>): string {
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (match, paramKey) => {
    return paramKey in params ? String(params[paramKey]) : match;
  });
}

export function I18nProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(getInitialLocale);

  const setLocale = (nextLocale: Locale) => {
    setLocaleState(nextLocale);
    try {
      if (typeof window !== 'undefined' && window.localStorage) {
        window.localStorage.setItem(LOCALE_STORAGE_KEY, nextLocale);
      }
    } catch {
      // Storage failure is non-fatal
    }
  };

  useEffect(() => {
    if (typeof document !== 'undefined') {
      document.documentElement.lang = locale === 'zh' ? 'zh-CN' : 'en';
    }
  }, [locale]);

  const value = useMemo<I18nContextValue>(() => {
    const activeDict = dictionaries[locale] || dictionaries.zh;
    const fallbackDict = dictionaries.zh;

    const t = (key: TranslationKey, params?: Record<string, string | number>): string => {
      const str = resolvePath(activeDict, key) ?? resolvePath(fallbackDict, key) ?? key;
      return formatTemplate(str, params);
    };

    return {
      locale,
      setLocale,
      t,
    };
  }, [locale]);

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): I18nContextValue {
  return useContext(I18nContext);
}
