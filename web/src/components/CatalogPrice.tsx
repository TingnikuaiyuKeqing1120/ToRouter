import { Clock, Layers, Tag, Zap } from 'lucide-react';
import type { CatalogItem, CatalogQuote, Currency, ExchangeRate } from '../api';
import { catalogAmount, catalogQuoteScope, catalogRateFields, catalogRateLabels, catalogRuleConditions, displayCatalogTable, selectCatalogQuote } from '../services/catalogPriceDisplay';

function QuotePrice({ quote, currency, rate, rateLoading = false }: { quote: CatalogQuote; currency: Currency; rate: ExchangeRate | null; rateLoading?: boolean }) {
  const { table, missingFx, validUnit } = displayCatalogTable(quote.table, currency, rate);
  return <>
    <p className="inline-flex items-center text-xs font-mono font-semibold px-2 py-0.5 rounded-md bg-emerald-500/10 text-emerald-400 border border-emerald-500/25 mb-1.5">{table.currency} / {validUnit ? '1M' : String(table.unit_tokens)} Tokens{missingFx ? rateLoading ? ' · 正在读取汇率，保留原币' : ' · 汇率不可用，保留原币' : ''}{!validUnit ? ' · 计价单位不可用' : ''}</p>
    <div className="grid grid-cols-2 gap-2 p-2.5 rounded-lg bg-[var(--color-surface-2)] border border-[var(--color-border)] text-xs mt-1" aria-label="基础四项价格">
      {catalogRateFields.map((field, index) => <p key={field} className="flex items-center justify-between"><span className="text-[var(--color-text-muted)] text-[11px]">{catalogRateLabels[index]} </span><span className="font-mono font-semibold text-[var(--color-text-primary)]">{catalogAmount(table.rates[field])}</span></p>)}
    </div>
    {!!table.rules.length && <details className="mt-2 text-xs rounded-lg border border-amber-500/30 bg-amber-500/5 p-2.5"><summary className="cursor-pointer font-medium text-amber-500 hover:text-amber-400 transition-colors flex items-center justify-between"><span>条件报价 · {table.rules.length} 条</span><span className="text-[10px] text-[var(--color-text-muted)] font-normal">点击展开详情 ↓</span></summary><div className="space-y-2 pt-2 mt-1.5 border-t border-amber-500/20">
      <p className="text-[10px] text-[var(--color-text-muted)]">按顺序执行，同条条件同时满足；未列出的价格继承本表。</p>
      <div className="space-y-2">
        {table.rules.map((rule, index) => {
          const isTimeWindow = Boolean(rule.when?.time_window);
          const isInputRange = Boolean(rule.when?.input_range);
          return (
            <div key={rule.id || index} className={`rounded-lg border ${rule.enabled ? 'border-amber-500/25 bg-[var(--color-surface-2)]/90 hover:border-amber-500/40 shadow-xs' : 'border-[var(--color-border)]/50 bg-[var(--color-surface-2)]/40 opacity-70'} p-2.5 space-y-1.5 transition-all text-xs`}>
              <div className="flex items-center justify-between gap-1.5">
                <div className="flex items-center gap-1.5 flex-wrap">
                  <span className="text-[10px] font-mono font-bold text-amber-400/90 bg-amber-500/10 px-1.5 py-0.5 rounded border border-amber-500/20">#{index + 1}</span>
                  {isInputRange && <span className="inline-flex items-center gap-1 text-[10px] font-semibold text-purple-300 bg-purple-500/15 px-1.5 py-0.5 rounded border border-purple-500/25"><Layers size={10} />按上下文长度</span>}
                  {isTimeWindow && <span className="inline-flex items-center gap-1 text-[10px] font-semibold text-cyan-300 bg-cyan-500/15 px-1.5 py-0.5 rounded border border-cyan-500/25"><Clock size={10} />按时段规则</span>}
                  {!isInputRange && !isTimeWindow && <span className="inline-flex items-center gap-1 text-[10px] font-semibold text-amber-300 bg-amber-500/15 px-1.5 py-0.5 rounded border border-amber-500/25"><Tag size={10} />特定条件</span>}
                  {!rule.enabled && <span className="text-[10px] text-[var(--color-text-muted)] bg-[var(--color-surface-1)] px-1.5 py-0.5 rounded">未启用</span>}
                </div>
              </div>
              <div className="flex items-baseline gap-1.5 flex-wrap text-xs">
                <span className="text-[11px] text-[var(--color-text-muted)] shrink-0 font-medium">触发：</span>
                <span className="font-mono text-[11px] text-[var(--color-text-primary)] bg-[var(--color-surface-1)] px-2 py-0.5 rounded border border-[var(--color-border)] leading-relaxed">
                  {catalogRuleConditions(rule)}
                </span>
                {!rule.enabled && <span className="text-[10px] text-amber-500/80 font-normal">· 未启用</span>}
              </div>
              {rule.action.rate_changes && (
                <div className="space-y-1.5 pt-0.5" aria-label="条件覆盖费率">
                  <div className="flex items-center justify-between text-[11px]">
                    <span className="text-[var(--color-text-secondary)] font-medium inline-flex items-center gap-1">
                      <Zap size={11} className="text-amber-400" />
                      覆盖单价
                    </span>
                    <span className="font-mono text-[10px] text-[var(--color-text-muted)] bg-[var(--color-surface-1)] px-1.5 py-0.5 rounded border border-[var(--color-border)]">
                      {table.currency} / {validUnit ? '1M' : table.unit_tokens} Tokens
                    </span>
                  </div>
                  <div className="grid grid-cols-2 sm:grid-cols-4 gap-1.5">
                    {catalogRateFields.filter(field => field in rule.action.rate_changes!).map(field => {
                      const label = `覆盖${catalogRateLabels[catalogRateFields.indexOf(field)]}`;
                      const amount = catalogAmount(rule.action.rate_changes![field]);
                      return (
                        <div key={field} className="px-2 py-1.5 rounded-lg bg-[var(--color-surface-1)] border border-[var(--color-border)] flex items-center justify-between">
                          <span className="text-[11px] text-[var(--color-text-muted)]">{label}</span>
                          <span className="font-mono font-bold text-xs text-amber-300">{amount}</span>
                        </div>
                      );
                    })}
                  </div>
                </div>
              )}
              {rule.action.multiplier != null && (
                <div className="px-2.5 py-1.5 rounded-lg bg-amber-500/10 border border-amber-500/25 flex items-center justify-between text-xs">
                  <span className="text-amber-300 font-medium inline-flex items-center gap-1.5">
                    <Zap size={12} className="text-amber-400" />
                    费率折算
                  </span>
                  <span className="font-mono font-bold text-amber-200">
                    当前价格 × {rule.action.multiplier}
                  </span>
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div></details>}
    {!!table.uncovered_conditions?.length && <details className="mt-1.5 text-xs rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)]/40 p-2"><summary className="cursor-pointer text-[var(--color-text-muted)] hover:text-[var(--color-text-secondary)]">价格备注 · {table.uncovered_conditions.length} 项</summary>{table.uncovered_conditions.map(condition => <p key={condition} className="pt-1">{condition}</p>)}</details>}
    {(quote.source_url || quote.verified_at || quote.notes?.length) && <details className="mt-1.5 text-xs rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)]/40 p-2"><summary className="cursor-pointer text-[var(--color-text-muted)] hover:text-[var(--color-text-secondary)]">报价来源与备注</summary><div className="space-y-1 pt-1">{quote.source_url && <a className="text-[var(--brand-blue)] hover:underline inline-flex items-center gap-1" href={quote.source_url} target="_blank" rel="noreferrer">报价出处</a>}{quote.verified_at && <p>核实 {quote.verified_at}</p>}{quote.notes?.map(note => <p key={note}>{note}</p>)}</div></details>}
  </>;
}

export function CatalogPrice({ item, currency, rate, rateLoading = false }: { item: CatalogItem; currency: Currency; rate: ExchangeRate | null; rateLoading?: boolean }) {
  const { entry, entries, label } = selectCatalogQuote(item, currency, rate);
  if (!entry) return <p className="text-xs my-2 text-[var(--color-text-muted)]">暂无报价</p>;
  const other = entries.filter(candidate => candidate !== entry);
  return <section className="text-xs my-3 space-y-1" aria-label={`${item.name || item.id} 价格`}>
    <div className="flex items-center justify-between"><span className="font-semibold text-[var(--color-text-primary)]">{label}</span>{other.length > 0 && <span className="text-[10px] text-blue-400 bg-blue-500/10 px-1.5 py-0.5 rounded border border-blue-500/20 font-medium">多档位可选</span>}</div>
    <p className="text-[10px] text-[var(--color-text-muted)]">{catalogQuoteScope(entry)}</p>
    <QuotePrice quote={entry.quote} currency={currency} rate={rate} rateLoading={rateLoading} />
    {!!other.length && <details className="mt-2 text-xs rounded-lg border border-blue-500/30 bg-blue-500/5 p-2"><summary className="cursor-pointer font-medium text-blue-400 hover:text-blue-300">其他报价与服务范围 · {other.length} 项</summary>{other.map(({ offering, quote }) => <div key={`${offering.key}/${quote.key}`} className="border-t border-[var(--color-border)] mt-2 pt-2"><p className="mb-1 text-[11px] font-medium text-[var(--color-text-secondary)]">{catalogQuoteScope({ offering, quote })}</p><QuotePrice quote={quote} currency={currency} rate={rate} rateLoading={rateLoading} /></div>)}</details>}
  </section>;
}
