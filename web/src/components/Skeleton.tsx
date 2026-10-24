import { type CSSProperties, memo } from 'react';

export interface SkeletonProps {
  className?: string;
  style?: CSSProperties;
  width?: string | number;
  height?: string | number;
  rounded?: 'sm' | 'md' | 'lg' | 'full';
}

export const Skeleton = memo(function Skeleton({
  className = '',
  style,
  width,
  height,
  rounded = 'md',
}: SkeletonProps) {
  const roundedClass =
    rounded === 'sm'
      ? 'rounded-sm'
      : rounded === 'lg'
      ? 'rounded-lg'
      : rounded === 'full'
      ? 'rounded-full'
      : 'rounded-md';

  const inlineStyle: CSSProperties = {
    ...(width != null ? { width } : {}),
    ...(height != null ? { height } : {}),
    ...style,
  };

  return (
    <div
      className={`skeleton-shimmer ${roundedClass} ${className}`}
      style={inlineStyle}
      aria-hidden="true"
    />
  );
});

export const SkeletonTable = memo(function SkeletonTable({
  rows = 5,
  cols = 5,
  className = '',
}: {
  rows?: number;
  cols?: number;
  className?: string;
}) {
  return (
    <div className={`table-scroll ${className}`} role="status" aria-label="正在加载表格数据…">
      <div className="p-4 space-y-3.5">
        {/* Table Header Row */}
        <div className="flex gap-4 border-b border-[var(--color-border)] pb-3">
          {Array.from({ length: cols }).map((_, i) => (
            <Skeleton
              key={i}
              className="h-3.5 rounded"
              style={{ width: i === 0 ? '25%' : `${Math.floor(75 / (cols - 1))}%` }}
            />
          ))}
        </div>
        {/* Table Body Rows */}
        {Array.from({ length: rows }).map((_, r) => (
          <div
            key={r}
            className="flex gap-4 py-2.5 items-center border-b border-[var(--color-border)]/50 last:border-none"
            style={{ opacity: Math.max(0.35, 1 - r * 0.14) }}
          >
            {Array.from({ length: cols }).map((_, c) => (
              <Skeleton
                key={c}
                className="h-3 rounded"
                style={{ width: c === 0 ? '25%' : `${Math.floor(75 / (cols - 1))}%` }}
              />
            ))}
          </div>
        ))}
      </div>
    </div>
  );
});

export const SkeletonCard = memo(function SkeletonCard({
  count = 3,
  className = '',
}: {
  count?: number;
  className?: string;
}) {
  return (
    <div
      className={`grid gap-4 sm:grid-cols-2 lg:grid-cols-3 ${className}`}
      role="status"
      aria-label="正在加载卡片数据…"
    >
      {Array.from({ length: count }).map((_, i) => (
        <div
          key={i}
          className="panel p-5 space-y-4"
          style={{ opacity: Math.max(0.4, 1 - i * 0.15) }}
        >
          <div className="flex items-center justify-between">
            <Skeleton className="h-4.5 w-28 rounded-md" />
            <Skeleton className="h-4 w-12 rounded-full" />
          </div>
          <Skeleton className="h-7 w-24 rounded" />
          <div className="space-y-2 pt-1">
            <Skeleton className="h-3 w-full rounded" />
            <Skeleton className="h-3 w-2/3 rounded" />
          </div>
        </div>
      ))}
    </div>
  );
});

export const SkeletonMetrics = memo(function SkeletonMetrics() {
  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4 mb-6" role="status" aria-label="正在加载指标数据…">
      {Array.from({ length: 4 }).map((_, i) => (
        <div key={i} className="panel p-5 space-y-3">
          <Skeleton className="h-3.5 w-20 rounded" />
          <Skeleton className="h-7 w-28 rounded-md" />
          <Skeleton className="h-2.5 w-32 rounded" />
        </div>
      ))}
    </div>
  );
});
