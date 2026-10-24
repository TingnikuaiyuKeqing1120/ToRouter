// @vitest-environment jsdom
import { cleanup, fireEvent, render } from '@testing-library/react';
import { afterEach, expect, it } from 'vitest';
import { ProviderIcon } from './ProviderIcons';

afterEach(cleanup);

it('uses supplier image icons without overriding an explicitly chosen built-in icon', () => {
  const { container, rerender } = render(<ProviderIcon name="Fixture supplier" fallbackIcon="https://example.invalid/icon.svg" />);
  expect(container.querySelector('img')?.getAttribute('src')).toBe('https://example.invalid/icon.svg');
  expect(container.querySelector('img')?.getAttribute('referrerpolicy')).toBe('no-referrer');
  rerender(<ProviderIcon name="Fixture supplier" override="openai" fallbackIcon="https://example.invalid/icon.svg" />);
  expect(container.querySelector('img')).toBeNull();
  expect(container.querySelector('svg')).toBeTruthy();
});

it('falls back to a stable local icon when the supplier image fails or uses an unsupported scheme', () => {
  const { container, rerender } = render(<ProviderIcon name="Fixture supplier" fallbackIcon="https://example.invalid/icon.png" />);
  fireEvent.error(container.querySelector('img')!);
  expect(container.querySelector('img')).toBeNull();
  expect(container.querySelector('svg')).toBeTruthy();
  rerender(<ProviderIcon name="Fixture supplier" fallbackIcon="javascript:alert(1)" />);
  expect(container.querySelector('img')).toBeNull();
});

it.each([
  ['Cloudflare Workers AI', 'cloudflare-color', 'cloudflare-color.svg'],
  ['Cline', 'cline', 'cline.svg'],
  ['Factory', 'factory', 'factory.svg'],
  ['Kiro', 'kiro-color', 'kiro-color.svg'],
  ['Trae', 'trae', 'trae.svg'],
  ['Zed', 'zed', 'zed.svg'],
  ['Command Code', 'commandcode', 'commandcode.svg'],
  ['Xiaomi MiMo', 'mimocode', 'mimocode.svg'],
  ['WorkBuddy', 'workbuddy-color', 'workbuddy-color.svg'],
  ['Nous Portal (OAuth)', 'nousresearch', 'nousresearch.svg'],
  ['商汤小浣熊', 'sensenova-color', 'sensenova-color.svg'],
  ['ZCode', 'zcode', 'zcode.png'],
])('uses a bundled supplier icon for %s and its registry icon name', (name, fallbackIcon, file) => {
  const { container } = render(<ProviderIcon name={name} fallbackIcon={fallbackIcon} />);
  const source = container.querySelector('img')?.getAttribute('src') || container.querySelector('span')?.style.mask;
  expect(source).toContain(file);
});

it.each(['opencode-cloudflare-workers-ai-auth', '@magpie-community/opencode-cloudflare-workers-ai-auth'])('matches the supplier instead of the package wrapper for %s', name => {
  const { container } = render(<ProviderIcon name={name} />);
  expect(container.querySelector('img')?.getAttribute('src')).toContain('cloudflare-color.svg');
});

it.each(['opencode-fixture-auth', '@magpie-community/opencode-fixture-auth', 'Open Design', 'AutoClaw', 'CodeArts'])('retains the generic icon without reliable supplier artwork for %s', name => {
  const { container } = render(<ProviderIcon name={name} />);
  expect(container.querySelector('svg[stroke="currentColor"]')).toBeTruthy();
});

it('keeps user overrides ahead of bundled supplier icons and remote manifest logos', () => {
  const { container, rerender } = render(<ProviderIcon name="Kiro" override="cursor" fallbackIcon="kiro-color" />);
  expect(container.querySelector('span')?.style.mask).toContain('cursor.svg');
  rerender(<ProviderIcon name="Cloudflare Workers AI" override="data:image/png;base64,aWNvbg==" fallbackIcon="https://example.invalid/icon.svg" />);
  expect(container.querySelector('img')?.getAttribute('src')).toBe('data:image/png;base64,aWNvbg==');
});

it('uses a manifest logo first and recovers the local supplier icon when the logo fails', () => {
  const { container } = render(<ProviderIcon name="opencode-cloudflare-workers-ai-auth" fallbackIcon="https://example.invalid/icon.svg" />);
  expect(container.querySelector('img')?.getAttribute('src')).toBe('https://example.invalid/icon.svg');
  fireEvent.error(container.querySelector('img')!);
  expect(container.querySelector('img')?.getAttribute('src')).toContain('cloudflare-color.svg');
});
