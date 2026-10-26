// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import App from './App';
import { I18nProvider } from './i18n';

const settings = {
  gateway_key: 'synthetic-admin', quota_refresh_seconds: 300, retention_days: 7,
  max_attempts: 3, retries_per_channel: 2, failure_cooldown_seconds: 10,
};
const json = (value: unknown) => new Response(JSON.stringify(value));

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem('to-router.locale.v1', 'zh');
  history.replaceState(null, '', '#/settings');
  vi.stubGlobal('fetch', vi.fn((url: string) => {
    if (url === '/api/settings') return Promise.resolve(json(settings));
    if (url === '/api/runtime/port') return Promise.resolve(json({ port: 2886, origin: 'http://localhost:2886' }));
    if (url === '/api/companion/preferences') return Promise.resolve(json({ theme: 'dark', paused: false, revision: 1, quota_targets: [] }));
    if (url === '/api/status') return Promise.resolve(json({ problem_count: 0 }));
    if (url === '/api/app-updates') return Promise.resolve(json({ enabled: false, current_version: '1.1.0', state: 'idle' }));
    // An unverified readiness reply ends the lifecycle watcher in this DOM test.
    return Promise.resolve(json({}));
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  history.replaceState(null, '', '#/');
  document.title = '';
});

it('updates the real App title immediately on language changes without remounting Settings or losing drafts', async () => {
  render(<I18nProvider><App /></I18nProvider>);
  await screen.findByRole('heading', { level: 1, name: '系统设置' });
  const port = await screen.findByRole('spinbutton', { name: '监听端口' });
  await waitFor(() => expect(port).toHaveProperty('value', '2886'));
  fireEvent.change(port, { target: { value: '2999' } });
  expect(document.title).toBe('系统设置 · to-router 控制台');

  fireEvent.click(screen.getByRole('button', { name: 'English' }));
  expect(screen.getByRole('heading', { level: 1, name: 'System Settings' })).toBeDefined();
  expect(document.title).toBe('Settings · to-router 控制台');
  expect(window.location.hash).toBe('#/settings');
  expect(screen.getByRole('spinbutton', { name: '监听端口' })).toBe(port);
  expect(port).toHaveProperty('value', '2999');

  fireEvent.click(screen.getByRole('button', { name: '简体中文' }));
  expect(document.title).toBe('系统设置 · to-router 控制台');
  expect(port).toHaveProperty('value', '2999');
  expect(vi.mocked(fetch).mock.calls.filter(([url]) => url === '/api/settings')).toHaveLength(1);
});
