import { X } from 'lucide-react';
import type { Tag } from '../types';

export interface TagBadgeProps {
  tag?: Tag;
  tagId?: string;
  name?: string;
  color?: string;
  sources?: string[];
  removable?: boolean;
  onRemove?: () => void;
  className?: string;
  size?: 'sm' | 'md';
}

const colorMap: Record<string, { bg: string; text: string; border: string }> = {
  emerald: { bg: 'bg-emerald-500/10 dark:bg-emerald-500/20', text: 'text-emerald-700 dark:text-emerald-300', border: 'border-emerald-500/30' },
  sky:     { bg: 'bg-sky-500/10 dark:bg-sky-500/20',         text: 'text-sky-700 dark:text-sky-300',         border: 'border-sky-500/30' },
  amber:   { bg: 'bg-amber-500/10 dark:bg-amber-500/20',     text: 'text-amber-700 dark:text-amber-300',     border: 'border-amber-500/30' },
  violet:  { bg: 'bg-violet-500/10 dark:bg-violet-500/20',   text: 'text-violet-700 dark:text-violet-300',   border: 'border-violet-500/30' },
  rose:    { bg: 'bg-rose-500/10 dark:bg-rose-500/20',       text: 'text-rose-700 dark:text-rose-300',       border: 'border-rose-500/30' },
  indigo:  { bg: 'bg-indigo-500/10 dark:bg-indigo-500/20',   text: 'text-indigo-700 dark:text-indigo-300',   border: 'border-indigo-500/30' },
  slate:   { bg: 'bg-slate-500/10 dark:bg-slate-500/20',     text: 'text-slate-700 dark:text-slate-300',     border: 'border-slate-500/30' },
};

export function TagBadge({
  tag,
  tagId,
  name,
  color,
  sources,
  removable = false,
  onRemove,
  className = '',
  size = 'md',
}: TagBadgeProps) {
  const displayId = tag?.id || tagId || '';
  const displayName = tag?.name || name || displayId;
  const tagColor = tag?.color || color || 'slate';
  const scheme = colorMap[tagColor] || colorMap.slate;

  const tooltipParts: string[] = [];
  if (tag?.description) {
    tooltipParts.push(tag.description);
  }
  if (sources && sources.length > 0) {
    tooltipParts.push(`来源: ${sources.join('；')}`);
  }
  const title = tooltipParts.join('\n');

  const sizeClasses = size === 'sm' ? 'px-1.5 py-0.5 text-xs' : 'px-2 py-0.5 text-xs font-medium';

  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1 rounded border transition-colors ${scheme.bg} ${scheme.text} ${scheme.border} ${sizeClasses} ${className}`}
    >
      <span>{displayName}</span>
      {removable && onRemove && (
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            onRemove();
          }}
          className="ml-0.5 hover:opacity-75 focus:outline-none"
        >
          <X className="w-3 h-3" />
        </button>
      )}
    </span>
  );
}
