import { useState, useRef, useEffect } from 'react';
import { Plus, X } from 'lucide-react';
import { Button, Input } from './ui';
import { defaultThinkingRows, type ThinkingRow } from '../services/thinkingPickerBinding';
export { defaultThinkingRows, type ThinkingRow } from '../services/thinkingPickerBinding';
const emptyEfforts: string[] = [];

function RowCheckbox({
  row,
  selected,
  onToggleRow,
  disabled,
}: {
  row: ThinkingRow;
  selected: string[];
  onToggleRow: (row: ThinkingRow, allSelected: boolean) => void;
  disabled?: boolean;
}) {
  const checkRef = useRef<HTMLInputElement>(null);
  const rowSelectedCount = row.items.filter(item => selected.includes(item)).length;
  const isAll = rowSelectedCount === row.items.length;
  const isPartial = rowSelectedCount > 0 && !isAll;

  useEffect(() => {
    if (checkRef.current) {
      checkRef.current.indeterminate = isPartial;
    }
  }, [isPartial]);

  return (
    <input
      ref={checkRef}
      type="checkbox"
      className="checkbox shrink-0"
      aria-label={`全选 ${row.label} 行`}
      title={`全选/取消 ${row.label}`}
      checked={isAll}
      disabled={disabled}
      onChange={() => onToggleRow(row, isAll)}
    />
  );
}

export interface ThinkingEffortPickerProps {
  efforts?: string[];
  defaultEffort?: string;
  onChange: (efforts: string[] | undefined, defaultEffort?: string) => void;
  disabled?: boolean;
  rows?: ThinkingRow[];
  canSelectDefault?: (item: string) => boolean;
  showDefault?: boolean;
}

export function ThinkingEffortPicker({
  efforts = emptyEfforts,
  defaultEffort,
  onChange,
  disabled = false,
  rows = defaultThinkingRows,
  canSelectDefault = () => true,
  showDefault = true,
}: ThinkingEffortPickerProps) {
  // Preset rows + any user-created custom items
  const allPresetItems = rows.flatMap(r => r.items);
  const initialCustom = efforts.filter(e => !allPresetItems.includes(e));

  const [customItems, setCustomItems] = useState<string[]>(initialCustom);
  const [newInput, setNewInput] = useState('');

  useEffect(() => {
    setCustomItems(efforts.filter(e => !rows.some(row => row.items.includes(e))));
  }, [efforts, rows]);

  const selectedSet = new Set(efforts);

  function updateEfforts(next: string[], nextDefault = defaultEffort) {
    const cleaned = next.filter((v, i, a) => a.indexOf(v) === i);
    // If current default is not in selected set, reset default
    const removedDefault = nextDefault !== undefined && efforts.includes(nextDefault) && !cleaned.includes(nextDefault);
    const validDefault = removedDefault ? undefined : nextDefault;
    onChange(cleaned, validDefault);
  }

  function toggleItem(item: string) {
    if (disabled) return;
    if (selectedSet.has(item)) {
      updateEfforts(efforts.filter(x => x !== item));
    } else {
      updateEfforts([...efforts, item]);
    }
  }

  function toggleRow(row: ThinkingRow, isAll: boolean) {
    if (disabled) return;
    if (isAll) {
      // Remove all items of this row
      updateEfforts(efforts.filter(x => !row.items.includes(x)));
    } else {
      // Add all items of this row
      updateEfforts([...efforts, ...row.items]);
    }
  }

  function addCustomItem() {
    const trimmed = newInput.trim();
    if (!trimmed || disabled) return;
    if (!customItems.includes(trimmed)) {
      setCustomItems(curr => [...curr, trimmed]);
    }
    if (!selectedSet.has(trimmed)) {
      updateEfforts([...efforts, trimmed]);
    }
    setNewInput('');
  }

  function removeCustomItem(item: string) {
    if (disabled) return;
    setCustomItems(curr => curr.filter(x => x !== item));
    updateEfforts(efforts.filter(x => x !== item));
  }

  return (
    <div className="space-y-3 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3 text-xs">
      <div className="space-y-2">
        {rows.map(row => (
          <div
            key={row.label}
            className="flex flex-wrap items-center gap-2.5 rounded-md border border-[var(--color-border)]/50 bg-[var(--color-surface-1)] px-2.5 py-1.5"
          >
            <div className="flex items-center gap-2 shrink-0 border-r border-[var(--color-border)]/60 pr-2">
              <RowCheckbox
                row={row}
                selected={efforts}
                onToggleRow={toggleRow}
                disabled={disabled}
              />
              <span className="text-[11px] font-medium text-[var(--color-text-muted)] select-none">
                {row.label}
              </span>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              {row.items.map(item => {
                const checked = selectedSet.has(item);
                return (
                  <label
                    key={item}
                    className={`inline-flex items-center gap-1.5 rounded px-2 py-0.5 cursor-pointer font-mono text-xs transition-colors select-none ${
                      checked
                        ? 'bg-[var(--brand-blue)]/15 text-[var(--brand-blue)] font-semibold'
                        : 'text-[var(--color-text-secondary)] hover:bg-[var(--color-surface-hover)]'
                    }`}
                  >
                    <input
                      type="checkbox"
                      className="checkbox h-3.5 w-3.5"
                      checked={checked}
                      disabled={disabled}
                      onChange={() => toggleItem(item)}
                    />
                    <span>{item}</span>
                  </label>
                );
              })}
            </div>
          </div>
        ))}

        {/* Custom Created Efforts */}
        {customItems.length > 0 && (
          <div className="flex flex-wrap items-center gap-2.5 rounded-md border border-[var(--color-border)]/50 bg-[var(--color-surface-1)] px-2.5 py-1.5">
            <div className="flex items-center gap-2 shrink-0 border-r border-[var(--color-border)]/60 pr-2">
              <RowCheckbox
                row={{ label: '自创档位', items: customItems }}
                selected={efforts}
                onToggleRow={toggleRow}
                disabled={disabled}
              />
              <span className="text-[11px] font-medium text-[var(--color-text-muted)] select-none">
                自创档位
              </span>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              {customItems.map(item => {
                const checked = selectedSet.has(item);
                return (
                  <span
                    key={item}
                    className={`inline-flex items-center gap-1.5 rounded px-2 py-0.5 font-mono text-xs transition-colors select-none ${
                      checked
                        ? 'bg-[var(--brand-blue)]/15 text-[var(--brand-blue)] font-semibold'
                        : 'bg-[var(--color-surface-2)] text-[var(--color-text-secondary)]'
                    }`}
                  >
                    <label className="inline-flex items-center gap-1.5 cursor-pointer">
                      <input
                        type="checkbox"
                        className="checkbox h-3.5 w-3.5"
                        checked={checked}
                        disabled={disabled}
                        onChange={() => toggleItem(item)}
                      />
                      <span>{item}</span>
                    </label>
                    <button
                      type="button"
                      disabled={disabled}
                      aria-label={`删除自创档位 ${item}`}
                      onClick={() => removeCustomItem(item)}
                      className="ml-0.5 text-[var(--color-text-muted)] hover:text-rose-500"
                    >
                      <X size={12} />
                    </button>
                  </span>
                );
              })}
            </div>
          </div>
        )}
      </div>

      {/* Add Custom Effort Input */}
      <div className="flex items-center gap-2 pt-1 border-t border-[var(--color-border)]/60">
        <Input
          placeholder="自创新档位（如 dynamic）"
          className="h-7 text-xs font-mono max-w-xs"
          value={newInput}
          disabled={disabled}
          onChange={e => setNewInput(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') {
              e.preventDefault();
              addCustomItem();
            }
          }}
        />
        <Button
          type="button"
          variant="secondary"
          className="!py-1 !px-2.5 h-7 text-xs"
          disabled={disabled || !newInput.trim()}
          onClick={addCustomItem}
        >
          <Plus size={13} />
          <span>添加自创档位</span>
        </Button>
      </div>

      {/* Default Thinking Effort Selector (when multiple are selected) */}
      {showDefault && (
        <div className="flex flex-wrap items-center gap-2.5 pt-1.5 border-t border-[var(--color-border)]/60 text-[11px] text-[var(--color-text-secondary)]">
          <span className="shrink-0 font-medium text-[var(--color-text-muted)]">默认生效档位：</span>
          <select
            aria-label="默认思考档位"
            className="select h-7 text-xs font-mono py-0.5 px-2 bg-[var(--color-surface-1)] border border-[var(--color-border)] rounded-md"
            value={defaultEffort ?? ''}
            disabled={disabled}
            onChange={e => {
              if (!e.target.value || canSelectDefault(e.target.value)) onChange(efforts, e.target.value || undefined);
            }}
          >
            <option value="">（不指定默认值，由客户端决定）</option>
            {defaultEffort && !efforts.includes(defaultEffort) && <option value={defaultEffort}>{defaultEffort}（当前默认）</option>}
            {efforts.map(e => (
              <option key={e} value={e} disabled={!canSelectDefault(e)}>
                {e}{!canSelectDefault(e) ? '（尚未确认支持）' : ''}
              </option>
            ))}
          </select>
          <span className="text-[var(--color-text-muted)]">仅补客户端未提供的思考参数</span>
        </div>
      )}
    </div>
  );
}

