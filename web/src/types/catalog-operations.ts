import type { CatalogItem, CatalogModelRef, CatalogPriceRef, ModelConfig, Price, PriceResolution, PriceSelection, PriceTable } from './index';
export interface CatalogMatch { item: CatalogItem; match: string; automatic: boolean }
export type PrefillGroup = 'capabilities' | 'price';
export interface PrefillTarget { kind: 'downstream' | 'upstream'; model: string; channel_id?: number; draft?: ModelConfig; price_draft?: PriceSelection; ref?: CatalogModelRef; price_ref?: CatalogPriceRef }
export interface PrefillRequest { targets: PrefillTarget[]; groups: PrefillGroup[]; force_overwrite: boolean; price_mode: 'catalog' | 'manual' }
export interface PrefillPatch { target: PrefillTarget; config: ModelConfig; catalog_model_ref?: CatalogModelRef; price?: Price; warnings: string[] }
export interface PrefillPreview { request: PrefillRequest; patches: PrefillPatch[]; updated: number; skipped: number }
export interface BackfillFilter { from?: string; to?: string; model?: string; channel_id?: number; upstream_model?: string }
export type BackfillMetric = 'actual' | 'official';
export interface BackfillPrice { key: string; table: PriceTable }
export interface BackfillRequest { filter: BackfillFilter; amounts: BackfillMetric[]; mode: 'current' | 'manual'; prices?: BackfillPrice[]; note?: string; max_usage_id?: number; expected_resolution?: unknown[] }
export interface BackfillGroup { key: string; metric: BackfillMetric; channel_id: number; channel_name: string; model: string; public_models?: string[]; records: number; calculable_amounts: number; missing_reasons: Record<string, number>; first_time: string; last_time: string; resolution: PriceResolution }
export interface BackfillPreview { request: BackfillRequest; examined: number; calculable_amounts: number; existing_amounts: number; missing_amounts: number; missing_reasons: Record<string, number>; groups: BackfillGroup[]; expected_resolution: unknown[]; applied_amounts: number }
