// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ConfirmProvider } from '../components/Confirm';
import { I18nProvider } from '../i18n';
import Settings from './Settings';

const settingsData = {
  gateway_key: 'synthetic-admin',
  quota_refresh_seconds: 300,
  retention_days: 7,
  max_attempts: 3,
  retries_per_channel: 2,
  failure_cooldown_seconds: 10,
  reasoning_policy: 'preserve',
  request_defaults_enabled: false,
};

const json = (val: unknown) => new Response(JSON.stringify(val));

beforeEach(() => {
  localStorage.clear();
  vi.stubGlobal('navigator', { language: 'zh-CN' });
  vi.stubGlobal('fetch', vi.fn((url: string) => {
    if (url.includes('/settings')) return Promise.resolve(json(settingsData));
    if (url.includes('/runtime/port')) return Promise.resolve(json({ port: 17878, origin: 'http://127.0.0.1:17878' }));
    if (url.includes('/app-updates')) return Promise.resolve(json({ enabled: false, current_version: '1.1.0', state: 'idle' }));
    return Promise.resolve(json({}));
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function setup() {
  return render(
    <I18nProvider>
      <ConfirmProvider>
        <Settings />
      </ConfirmProvider>
    </I18nProvider>
  );
}

describe('Settings language switching', () => {
  it('renders Chinese by default and toggles to English immediately', async () => {
    setup();

    // Verify initial Chinese
    await screen.findByRole('heading', { level: 1, name: '系统设置' });
    expect(screen.getByText('界面偏好')).toBeTruthy();
    expect(screen.getByRole('button', { name: '简体中文' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: 'English' }).getAttribute('aria-pressed')).toBe('false');

    // Toggle to English
    fireEvent.click(screen.getByRole('button', { name: 'English' }));

    // Headings, buttons, and sections should instantly update to English
    expect(screen.getByRole('heading', { level: 1, name: 'System Settings' })).toBeTruthy();
    expect(screen.getByText('Preferences')).toBeTruthy();
    expect(screen.getByText('Interface Language')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'English' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: '简体中文' }).getAttribute('aria-pressed')).toBe('false');
    expect(localStorage.getItem('to-router.locale.v1')).toBe('en');

    // Toggle back to Chinese
    fireEvent.click(screen.getByRole('button', { name: '简体中文' }));
    expect(screen.getByRole('heading', { level: 1, name: '系统设置' })).toBeTruthy();
    expect(localStorage.getItem('to-router.locale.v1')).toBe('zh');
  });
});
