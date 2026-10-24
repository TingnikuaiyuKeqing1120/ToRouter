import { useState, useMemo, useEffect, useRef } from 'react';
import { Sparkles, Search, Check, ChevronDown, Link as LinkIcon } from 'lucide-react';
import { api, errorMessage, type CatalogItem, type CatalogMatch, type CatalogModelRef, type PrefillGroup, type PrefillPatch, type PrefillPreview, type PrefillTarget } from '../api';
import { Button, ErrorBox, Input, Select } from './ui';
import { catalogEntries, catalogEntryKey } from '../services/catalogEntries';

export function PrefillControls({
  targets,
  onDraft,
  onApplied,
  onAssociation,
  allowedGroups,
}: {
  targets: PrefillTarget[];
  onDraft?: (patch: PrefillPatch) => void;
  onApplied?: () => void;
  onAssociation?: (ref: CatalogModelRef | undefined) => void;
  allowedGroups?: PrefillGroup[];
}) {
  const [matches, setMatches] = useState<CatalogMatch[]>([]);
  const [items, setItems] = useState<CatalogItem[]>([]);
  const [choice, setChoice] = useState('');
  const [quote, setQuote] = useState('');
  const [groups, setGroups] = useState<PrefillGroup[]>(() => allowedGroups || (targets.every(target => target.kind === 'upstream') ? ['capabilities'] : ['price']));
  const [force, setForce] = useState(false);
  const [preview, setPreview] = useState<PrefillPreview | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [searchTerm, setSearchTerm] = useState('');
  const [dropdownOpen, setDropdownOpen] = useState(false);
  const comboboxRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleClickOutside(e: MouseEvent) {
      if (comboboxRef.current && !comboboxRef.current.contains(e.target as Node)) {
        setDropdownOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const signature = JSON.stringify(targets);
  const entries = useMemo(() => catalogEntries(items), [items]);
  const savedRef = targets.length === 1 ? targets[0].ref : undefined;

  useEffect(() => {
    if (!savedRef) return;
    const entry = entries.find(
      e =>
        catalogEntryKey(e.ref) === catalogEntryKey(savedRef) ||
        (!e.ref.offering_key && e.ref.source === savedRef.source && e.ref.id === savedRef.id)
    );
    setChoice(entry ? catalogEntryKey(entry.ref) : '');
    setQuote('');
  }, [savedRef?.source, savedRef?.id, savedRef?.offering_key, entries]);

  const currentRequest = JSON.stringify({ targets, choice, quote, groups, force });
  const requestKey = useRef(currentRequest);
  requestKey.current = currentRequest;

  useEffect(() => {
    setPreview(null);
  }, [signature, choice, quote, groups, force]);

  useEffect(() => {
    if (targets.length !== 1 || !targets[0].model.trim()) {
      setMatches([]);
      return;
    }
    let live = true;
    const target = targets[0];
    const query = new URLSearchParams({ model: target.model });
    if (target.ref) {
      query.set('source', target.ref.source);
      query.set('id', target.ref.id);
    }
    api<CatalogMatch[]>(`/catalog/matches?${query}`)
      .then(rows => {
        if (!Array.isArray(rows) || rows.some(match => !match?.item))
          throw new Error('参数库匹配响应无效，请刷新后重试。');
        if (live) setMatches(rows);
      })
      .catch(err => {
        if (live) {
          setMatches([]);
          setError(errorMessage(err));
        }
      });
    return () => {
      live = false;
    };
  }, [targets[0]?.model, targets[0]?.ref?.source, targets[0]?.ref?.id, targets.length]);

  useEffect(() => {
    api<CatalogItem[]>('/catalog/models?source=all')
      .then(rows => setItems(Array.isArray(rows) ? rows.filter(item => typeof item?.source === 'string' && typeof item?.id === 'string') : []))
      .catch(() => {});
  }, []);

  const chosenEntry = choice ? entries.find(entry => catalogEntryKey(entry.ref) === choice) : undefined;
  const selected =
    targets.length === 1
      ? chosenEntry?.item || (!choice ? matches.find(match => match.automatic)?.item : undefined)
      : undefined;

  const selectedRef: CatalogModelRef | undefined = selected
    ? {
        source: selected.source,
        id: selected.id,
        ...(chosenEntry?.ref.offering_key
          ? { offering_key: chosenEntry.ref.offering_key }
          : savedRef?.source === selected.source && savedRef?.id === selected.id && savedRef?.offering_key
          ? { offering_key: savedRef.offering_key }
          : {}),
      }
    : undefined;

  const needsRegion = selected?.source === 'builtin' && selected.offerings.length > 1 && !selectedRef?.offering_key;
  const quotes = needsRegion
    ? []
    : selected?.offerings
        ?.filter(offering => !selectedRef?.offering_key || offering.key === selectedRef.offering_key)
        .flatMap(offering => offering.quotes.map(q => ({ offering, q }))) || [];

  // Filtered entries for search
  const filteredEntries = useMemo(() => {
    const term = searchTerm.toLowerCase().trim();
    if (term) {
      return entries.filter(
        ({ item }) =>
          item.name.toLowerCase().includes(term) ||
          item.id.toLowerCase().includes(term) ||
          item.provider.toLowerCase().includes(term) ||
          item.source.toLowerCase().includes(term)
      );
    }
    if (targets.length === 1 && targets[0].model.trim()) {
      const modelKeyword = targets[0].model.toLowerCase().trim();
      const matched = entries.filter(
        ({ item }) =>
          item.id.toLowerCase().includes(modelKeyword) ||
          item.name.toLowerCase().includes(modelKeyword)
      );
      const rest = entries.filter(
        ({ item }) =>
          !item.id.toLowerCase().includes(modelKeyword) &&
          !item.name.toLowerCase().includes(modelKeyword)
      );
      return [...matched, ...rest];
    }
    return entries;
  }, [entries, searchTerm, targets]);

  async function build() {
    setBusy(true);
    setError('');
    try {
      const pair = quotes.find(entry => `${entry.offering.key}\u0000${entry.q.key}` === quote);
      const key = requestKey.current;
      const result = await api<PrefillPreview>('/catalog/prefill/preview', {
        method: 'POST',
        body: JSON.stringify({
          targets: targets.map(target => {
            if (!selectedRef) return target;
            const { price_ref: _oldPriceRef, ...rest } = target;
            return {
              ...rest,
              ref: selectedRef,
              ...(pair ? { price_ref: { ...selectedRef, offering_key: pair.offering.key, quote_key: pair.q.key } } : {}),
            };
          }),
          groups,
          force_overwrite: force,
          price_mode: 'catalog',
        }),
      });
      if (key === requestKey.current) {
        setPreview(result);
        return result;
      }
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  async function apply(overridePreview?: PrefillPreview) {
    const toApply = overridePreview || preview;
    if (!toApply) return;
    setBusy(true);
    setError('');
    try {
      if (onDraft) {
        toApply.patches.forEach(onDraft);
        setPreview(null);
      } else {
        await api('/catalog/prefill/apply', {
          method: 'POST',
          body: JSON.stringify(toApply.request),
        });
        setPreview(null);
        onApplied?.();
      }
    } catch (e) {
      setError(errorMessage(e));
      setPreview(null);
    } finally {
      setBusy(false);
    }
  }

  // One-click quick fill
  async function quickFill() {
    const p = await build();
    if (p) {
      await apply(p);
    }
  }

  const singleTarget = targets.length === 1 ? targets[0] : null;
  const bestMatch = matches.find(m => m.automatic) || matches[0];

  return (
    <section className="space-y-3.5 rounded-xl border border-[var(--color-border)] bg-[var(--color-surface-2)]/60 p-3.5 transition-all">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <div className="flex h-6 w-6 items-center justify-center rounded-md bg-[var(--brand-blue)]/10 text-[var(--brand-blue)]">
            <Sparkles size={14} />
          </div>
          <h3 className="text-xs font-semibold text-[var(--color-text-primary)]">参数库预填</h3>
          {singleTarget && singleTarget.ref && (
            <span className="inline-flex items-center gap-1 rounded bg-[var(--color-surface-1)] px-1.5 py-0.5 text-[10px] text-[var(--color-text-muted)] border border-[var(--color-border)]">
              <LinkIcon size={10} />
              已关联: {singleTarget.ref.id}
            </span>
          )}
        </div>

        {/* 仅关联资料或取消关联 */}
        {singleTarget && onAssociation && (
          <div className="flex items-center gap-1.5">
            <Button
              type="button"
              variant="ghost"
              className="!h-6 !text-[11px] !px-2 text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
              disabled={!selectedRef}
              onClick={() => {
                if (selectedRef) onAssociation(selectedRef);
              }}
              title="仅在模型与参数库之间建立关联引用，不改变当前输入框的数值"
            >
              仅关联资料
            </Button>
            {singleTarget.ref && (
              <Button
                type="button"
                variant="ghost"
                className="!h-6 !text-[11px] !px-2 text-rose-500 hover:text-rose-600"
                onClick={() => onAssociation(undefined)}
              >
                取消资料关联
              </Button>
            )}
          </div>
        )}
      </div>

      {error && <ErrorBox message={error} />}

      {singleTarget && (
        <div className="space-y-2.5">
          {/* 智能匹配结果高亮卡片 */}
          {bestMatch && (
            <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-[var(--brand-blue)]/30 bg-[var(--brand-blue)]/10 px-3 py-2 text-xs">
              <div className="flex items-center gap-2">
                <span className="flex h-2 w-2 rounded-full bg-[var(--brand-blue)] animate-pulse" />
                <span className="font-semibold text-[var(--color-text-primary)]">
                  {bestMatch.automatic ? '🎯 自动唯一匹配：' : '💡 智能推荐匹配：'}
                </span>
                <span className="font-mono font-medium text-[var(--brand-blue)]">
                  {bestMatch.item.provider} · {bestMatch.item.name || bestMatch.item.id}
                </span>
                <span className="text-[10px] text-[var(--color-text-muted)]">({bestMatch.item.source})</span>
              </div>

              {/* 一键采纳匹配按钮 */}
              {(() => {
                const matchingEntry =
                  entries.find(
                    e =>
                      e.ref.source === bestMatch.item.source &&
                      e.ref.id === bestMatch.item.id &&
                      (!e.ref.offering_key || e.ref.offering_key === bestMatch.item.offerings?.[0]?.key)
                  ) ||
                  entries.find(e => e.ref.source === bestMatch.item.source && e.ref.id === bestMatch.item.id);
                const matchKey = matchingEntry ? catalogEntryKey(matchingEntry.ref) : '';
                const isAdopted = Boolean(matchKey && (choice === matchKey || (!choice && bestMatch.automatic)));
                return (
                  <button
                    type="button"
                    className={`inline-flex items-center gap-1 px-2.5 py-1 rounded-md text-[11px] font-medium transition-colors ${
                      isAdopted
                        ? 'bg-[var(--brand-blue)] text-white shadow-xs font-semibold'
                        : 'bg-[var(--color-surface-1)] border border-[var(--color-border)] text-[var(--color-text-primary)] hover:border-[var(--brand-blue)]'
                    }`}
                    onClick={() => {
                      if (matchKey) {
                        setChoice(matchKey);
                        setQuote('');
                        setDropdownOpen(false);
                      }
                    }}
                  >
                    <Check size={11} />
                    <span>{isAdopted ? '已采用此资料' : '一键采纳匹配'}</span>
                  </button>
                );
              })()}
            </div>
          )}

          {/* 没有匹配时的提示 */}
          {!bestMatch && (
            <p className="text-xs text-[var(--color-text-muted)]">
              未发现唯一匹配，可手动搜索或在下方选择精确资料。
            </p>
          )}

          {/* 一体化浮动 Combobox 搜索与选择 (EP-002 惯例优先) */}
          <div className="relative space-y-1.5" ref={comboboxRef}>
            <div className="relative flex items-center">
              <Search size={13} className="absolute left-2.5 top-2.5 text-[var(--color-text-muted)] pointer-events-none" />
              <Input
                className="!h-8 !pl-7 !pr-16 text-xs font-mono cursor-pointer"
                placeholder={
                  chosenEntry
                    ? `${chosenEntry.item.source} · ${chosenEntry.item.provider} · ${chosenEntry.item.name || chosenEntry.item.id}`
                    : singleTarget?.model
                    ? `点击展开资料或输入搜索 (默认: ${singleTarget.model})`
                    : '搜索参数库模型 (如 gpt-4o, claude-3-5...)'
                }
                value={searchTerm}
                onFocus={() => setDropdownOpen(true)}
                onChange={e => {
                  setSearchTerm(e.target.value);
                  setDropdownOpen(true);
                }}
                aria-label="搜索参数库模型"
              />
              <div className="absolute right-2 top-1.5 flex items-center gap-1">
                {searchTerm && (
                  <button
                    type="button"
                    className="text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)] text-xs px-1"
                    onClick={() => setSearchTerm('')}
                    title="清空搜索"
                  >
                    ✕
                  </button>
                )}
                <button
                  type="button"
                  className="text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)] p-0.5"
                  onClick={() => setDropdownOpen(o => !o)}
                  title="展开/收起可选列表"
                >
                  <ChevronDown size={14} className={`transition-transform duration-150 ${dropdownOpen ? 'rotate-180' : ''}`} />
                </button>
              </div>
            </div>

            {/* 浮动下拉面板 */}
            {dropdownOpen && (
              <div className="absolute left-0 right-0 top-full mt-1 z-50 max-h-60 overflow-y-auto rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-1)] shadow-xl p-1 text-xs font-mono space-y-0.5">
                <div
                  className={`flex items-center justify-between px-2.5 py-1.5 rounded cursor-pointer transition-colors ${
                    !choice ? 'bg-[var(--brand-blue)]/10 text-[var(--brand-blue)] font-medium' : 'hover:bg-[var(--color-surface-hover)] text-[var(--color-text-muted)]'
                  }`}
                  onClick={() => {
                    setChoice('');
                    setQuote('');
                    setDropdownOpen(false);
                  }}
                >
                  <span>按服务端匹配与来源偏好 (自动)</span>
                  {!choice && <Check size={12} />}
                </div>

                {filteredEntries.length === 0 ? (
                  <div className="px-3 py-3 text-center text-xs text-[var(--color-text-muted)]">
                    未找到与「{searchTerm}」匹配的模型资料
                  </div>
                ) : (
                  filteredEntries.map(({ item, ref }) => {
                    const key = catalogEntryKey(ref);
                    const isSelected = choice === key;
                    return (
                      <button
                        type="button"
                        aria-label={`${item.source} · ${item.provider} · ${item.name || item.id}`}
                        key={key}
                        className={`flex w-full items-center justify-between px-2.5 py-1.5 rounded cursor-pointer transition-colors ${
                          isSelected
                            ? 'bg-[var(--brand-blue)]/10 text-[var(--brand-blue)] font-semibold'
                            : 'hover:bg-[var(--color-surface-hover)] text-[var(--color-text-primary)]'
                        }`}
                        onClick={() => {
                          setChoice(key);
                          setQuote('');
                          setDropdownOpen(false);
                          setSearchTerm('');
                        }}
                      >
                        <div className="flex items-center gap-2 truncate">
                          <span className="text-[10px] text-[var(--color-text-muted)] uppercase shrink-0">
                            [{item.source}]
                          </span>
                          <span className="truncate">
                            {item.provider} · {item.name || item.id}
                          </span>
                        </div>
                        {isSelected && <Check size={13} className="shrink-0 text-[var(--brand-blue)]" />}
                      </button>
                    );
                  })
                )}
              </div>
            )}

          </div>
        </div>
      )}

      {/* 预填分组复选框与模式 */}
      <div className="space-y-2 pt-1 border-t border-[var(--color-border)]/50">
        <div className="flex flex-wrap items-center gap-3">
          <span className="text-xs font-medium text-[var(--color-text-secondary)]">预填范围:</span>
          {(
            [
              ['capabilities', '能力规格'],
              ['price', '价格'],
            ] as const
          )
            .filter(([key]) => (key !== 'capabilities' || targets.every(target => target.kind === 'upstream')) && (!allowedGroups || allowedGroups.includes(key)))
            .map(([key, label]) => (
              <label key={key} className="flex items-center gap-1.5 text-xs cursor-pointer select-none">
                <input
                  type="checkbox"
                  className="checkbox h-3.5 w-3.5"
                  aria-label={`预填${label}`}
                  checked={groups.includes(key)}
                  onChange={e =>
                    setGroups(current =>
                      e.target.checked ? [...current, key] : current.filter(group => group !== key)
                    )
                  }
                />
                <span>{label}</span>
              </label>
            ))}
        </div>

        {/* 覆盖策略 */}
        <label className="flex items-center gap-2 text-xs text-[var(--color-text-muted)] cursor-pointer select-none">
          <input
            type="checkbox"
            className="checkbox h-3.5 w-3.5"
            aria-label="强制覆盖所选分组"
            checked={force}
            onChange={e => setForce(e.target.checked)}
          />
          <span>强制覆盖已有配置（默认仅填补空白项，不改动已有内容）</span>
        </label>
      </div>

      {/* 价格配置子项（勾选价格时展示） */}
      {groups.includes('price') && (
        <div className="space-y-2 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-1)] p-3">
          <p className="text-xs text-[var(--color-text-secondary)]">匹配后跟随资料库更新；直接修改价格后独立保存。</p>

          {needsRegion && (
            <p className="text-xs text-amber-500">
              请选择地区资料后预填价格；能力规格仍可按模型匹配。
            </p>
          )}

          {quotes.length > 0 && (
            <div className="space-y-1">
              <label className="text-[11px] text-[var(--color-text-muted)]">精确报价来源:</label>
              <Select
                aria-label="精确报价"
                value={quote}
                onChange={e => setQuote(e.target.value)}
                className="!h-8 text-xs font-mono"
              >
                <option value="">
                  {selectedRef?.offering_key && quotes.length > 1 ? '请选择精确报价' : '自动采用已核实原厂报价'}
                </option>
                {quotes
                  .filter(
                    ({ offering, q }) =>
                      targets.every(target => target.kind === 'upstream') ||
                      ['original', 'official', 'original_vendor'].includes(q.source_kind || offering.source_kind || '')
                  )
                  .map(({ offering, q }) => (
                    <option key={`${offering.key}:${q.key}`} value={`${offering.key}\u0000${q.key}`}>
                      {offering.provider} · {offering.region || '全域'} · {offering.service_tier || '标准'} ·{' '}
                      {q.name || q.key} · {q.source_kind} · {q.table.currency}
                    </option>
                  ))}
              </Select>
            </div>
          )}

        </div>
      )}

      {/* 底部操作条：一键快捷填入 + 预览预填 */}
      <div className="flex flex-wrap items-center gap-2 pt-1">
        {onDraft && (
          <Button
            type="button"
            className="!h-8 !text-xs font-semibold"
            disabled={busy || !targets.length || !groups.length || targets.some(target => !target.model)}
            onClick={() => void quickFill()}
          >
            <Sparkles size={13} />
            <span>{busy ? '正在填入…' : '一键填入当前草稿'}</span>
          </Button>
        )}

        <Button
          type="button"
          variant="secondary"
          className="!h-8 !text-xs"
          disabled={busy || !targets.length || !groups.length || targets.some(target => !target.model)}
          onClick={() => void build()}
        >
          {busy ? '处理中…' : '预览预填'}
        </Button>
      </div>

      {/* 预览结果与确认卡片 */}
      {preview && (
        <div className="space-y-2.5 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-1)] p-3 text-xs" role="status">
          <div className="flex items-center justify-between font-medium">
            <span className="text-[var(--color-text-primary)]">
              预计更新 {preview.updated} 项，跳过 {preview.skipped} 项
            </span>
            <Button
              type="button"
              className="!h-7 !text-xs !py-0"
              disabled={busy}
              onClick={() => void apply()}
            >
              {onDraft ? '填入当前草稿' : '应用预填'}
            </Button>
          </div>

          <div className="space-y-2">
            {preview.patches.map((patch, index) => (
              <div
                key={index}
                className="space-y-1.5 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-2.5"
              >
                <div className="flex items-center justify-between font-semibold">
                  <span className="text-[var(--color-text-primary)] font-mono">{patch.target.model}</span>
                  <span className="text-[11px] font-normal text-[var(--color-text-muted)]">
                    {patch.catalog_model_ref ? `匹配来源: ${patch.catalog_model_ref.source}` : '未匹配'}
                  </span>
                </div>

                <div className="flex flex-wrap gap-1.5 text-[11px]">
                  {patch.config.context_window != null && (
                    <span className="rounded border border-[var(--color-border)] bg-[var(--color-surface-1)] px-1.5 py-0.5 font-mono">
                      窗口 {patch.config.context_window >= 1000 ? `${patch.config.context_window / 1000}K` : patch.config.context_window}
                    </span>
                  )}
                  {patch.config.max_output_tokens != null && (
                    <span className="rounded border border-[var(--color-border)] bg-[var(--color-surface-1)] px-1.5 py-0.5 font-mono">
                      输出 {patch.config.max_output_tokens >= 1000 ? `${patch.config.max_output_tokens / 1000}K` : patch.config.max_output_tokens}
                    </span>
                  )}
                  {patch.config.native_thinking?.supported && (
                    <span className="rounded border border-blue-500/20 bg-blue-500/10 px-1.5 py-0.5 text-blue-400">
                      支持思考
                    </span>
                  )}
                  {patch.price?.selection.table?.rates.input != null && (
                    <span className="rounded border border-emerald-500/20 bg-emerald-500/10 px-1.5 py-0.5 text-emerald-400 font-mono">
                      输入单价 {patch.price.selection.table.rates.input}
                    </span>
                  )}
                  {patch.price?.selection.table?.rates.output != null && (
                    <span className="rounded border border-emerald-500/20 bg-emerald-500/10 px-1.5 py-0.5 text-emerald-400 font-mono">
                      输出单价 {patch.price.selection.table.rates.output}
                    </span>
                  )}
                </div>

                {patch.warnings?.map((warning, wIdx) => (
                  <p key={wIdx} className="text-[11px] text-amber-500">
                    ⚠️ {warning}
                  </p>
                ))}
              </div>
            ))}
          </div>
        </div>
      )}
    </section>
  );
}
