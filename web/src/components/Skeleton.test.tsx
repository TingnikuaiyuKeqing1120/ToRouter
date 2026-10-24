// @vitest-environment jsdom
import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Skeleton, SkeletonCard, SkeletonMetrics, SkeletonTable } from './Skeleton';

describe('Skeleton Components', () => {
  it('renders atomic Skeleton with shimmer classes and inline styles', () => {
    const { container } = render(<Skeleton width="100px" height="20px" rounded="full" className="custom-sk" />);
    const el = container.firstElementChild as HTMLElement;
    expect(el).toBeTruthy();
    expect(el.className).toContain('skeleton-shimmer');
    expect(el.className).toContain('rounded-full');
    expect(el.className).toContain('custom-sk');
    expect(el.style.width).toBe('100px');
    expect(el.style.height).toBe('20px');
  });

  it('renders SkeletonTable with status role and custom rows/cols', () => {
    const { container } = render(<SkeletonTable rows={4} cols={3} />);
    const table = container.querySelector('[role="status"]');
    expect(table).toBeTruthy();
    expect(table?.getAttribute('aria-label')).toBe('正在加载表格数据…');
    // Header has 3 skeletons + 4 rows with 3 each = 15 skeletons
    expect(container.querySelectorAll('.skeleton-shimmer').length).toBe(15);
  });

  it('renders SkeletonCard with designated count', () => {
    const { container } = render(<SkeletonCard count={4} />);
    const cardGroup = container.querySelector('[role="status"]');
    expect(cardGroup).toBeTruthy();
    expect(container.querySelectorAll('.panel').length).toBe(4);
  });

  it('renders SkeletonMetrics', () => {
    const { container } = render(<SkeletonMetrics />);
    expect(container.querySelectorAll('.panel').length).toBe(4);
  });
});
