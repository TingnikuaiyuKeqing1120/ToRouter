// @vitest-environment jsdom
import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { MascotIllustration, type MascotVariant } from './MascotIllustration';
import { Empty } from './ui';

describe('MascotIllustration', () => {
  const variants: MascotVariant[] = ['channel', 'log', 'search', 'key', 'model', 'chart', 'default'];

  variants.forEach(v => {
    it(`renders ${v} variant successfully with svg and proper paths`, () => {
      const { container } = render(<MascotIllustration variant={v} size={150} />);
      const svg = container.querySelector('svg');
      expect(svg).toBeTruthy();
      expect(svg?.getAttribute('aria-hidden')).toBe('true');
      expect(svg?.getAttribute('width')).toBe('150');
      // Should contain pixel paths
      expect(container.querySelectorAll('path').length).toBeGreaterThan(0);
    });
  });

  it('renders within Empty component automatically selecting appropriate variant', () => {
    const { container: c1 } = render(<Empty title="未找到匹配渠道" />);
    expect(c1.querySelector('svg')).toBeTruthy();
    expect(c1.textContent).toContain('未找到匹配渠道');

    const { container: c2 } = render(<Empty title="暂无匹配的请求" />);
    expect(c2.querySelector('svg')).toBeTruthy();
    expect(c2.textContent).toContain('暂无匹配的请求');

    const { container: c3 } = render(<Empty title="尚未创建客户端 API Key" />);
    expect(c3.querySelector('svg')).toBeTruthy();
    expect(c3.textContent).toContain('尚未创建客户端 API Key');
  });
});
