// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { I18nProvider, useI18n, formatTemplate, getInitialLocale } from './index';

describe('i18n infrastructure', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('formats templates correctly with parameters', () => {
    expect(formatTemplate('Hello {name}!', { name: 'World' })).toBe('Hello World!');
    expect(formatTemplate('Saved port {port}', { port: 17878 })).toBe('Saved port 17878');
    expect(formatTemplate('No params {count}')).toBe('No params {count}');
  });

  it('detects initial locale from localStorage', () => {
    localStorage.setItem('to-router.locale.v1', 'en');
    expect(getInitialLocale()).toBe('en');

    localStorage.setItem('to-router.locale.v1', 'zh');
    expect(getInitialLocale()).toBe('zh');
  });

  it('detects initial locale from navigator.language when storage is empty', () => {
    vi.stubGlobal('navigator', { language: 'en-US' });
    expect(getInitialLocale()).toBe('en');

    vi.stubGlobal('navigator', { language: 'zh-CN' });
    expect(getInitialLocale()).toBe('zh');
  });

  it('translates keys in default and toggled language', () => {
    vi.stubGlobal('navigator', { language: 'zh-CN' });
    const { result } = renderHook(() => useI18n(), {
      wrapper: ({ children }) => <I18nProvider>{children}</I18nProvider>,
    });

    // Default Chinese
    expect(result.current.locale).toBe('zh');
    expect(result.current.t('nav.channels')).toBe('上游接入');
    expect(result.current.t('settings.title')).toBe('系统设置');

    // Switch to English
    act(() => {
      result.current.setLocale('en');
    });

    expect(result.current.locale).toBe('en');
    expect(result.current.t('nav.channels')).toBe('Channels');
    expect(result.current.t('settings.title')).toBe('System Settings');
    expect(localStorage.getItem('to-router.locale.v1')).toBe('en');
  });
});
