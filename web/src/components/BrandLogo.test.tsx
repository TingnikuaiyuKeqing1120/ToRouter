// @vitest-environment jsdom
import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { BrandLogo } from './BrandLogo';

describe('BrandLogo', () => {
  it('renders default brand logo with SVG element', () => {
    const { container } = render(<BrandLogo size={32} />);
    const svg = container.querySelector('svg');
    expect(svg).toBeTruthy();
    expect(svg?.getAttribute('width')).toBe('32');
    expect(svg?.getAttribute('height')).toBe('32');
    expect(svg?.getAttribute('aria-label')).toBe('to-router');
  });

  it('renders dark theme with dark linear gradient background', () => {
    const { container } = render(<BrandLogo theme="dark" size={48} />);
    const gradient = container.querySelector('#brand-dark-bg');
    expect(gradient).toBeTruthy();
  });

  it('renders light theme with clean ceramic white fill', () => {
    const { container } = render(<BrandLogo theme="light" size={48} />);
    const rect = container.querySelector('rect[fill="#FFFFFF"]');
    expect(rect).toBeTruthy();
  });

  it('renders with custom className', () => {
    const { container } = render(<BrandLogo className="custom-logo-class" />);
    const svg = container.querySelector('svg');
    expect(svg).toBeTruthy();
    const cls = svg?.className.baseVal ?? svg?.getAttribute('class') ?? '';
    expect(cls).toContain('custom-logo-class');
  });
});
