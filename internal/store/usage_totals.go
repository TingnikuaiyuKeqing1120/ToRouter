package store

import (
	"context"
	"time"
)

// UsageTotals counts saved upstream attempts, not logical incoming requests.
// InputTokens includes both cache categories, matching the usage overview.
type UsageTotals struct {
	Records          int64 `json:"records"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	EstimatedRecords int64 `json:"estimated_records"`
}

// UsageTotalsBetween aggregates the entire half-open interval in SQLite. The
// HUD must not silently inherit ListUsage's 10,000-row display limit.
func (s *Store) UsageTotalsBetween(ctx context.Context, from, until time.Time) (UsageTotals, error) {
	var totals UsageTotals
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(COALESCE(json_extract(data,'$.input_tokens'),0) + COALESCE(json_extract(data,'$.cache_read_tokens'),0) + COALESCE(json_extract(data,'$.cache_write_tokens'),0)),0),
		COALESCE(SUM(json_extract(data,'$.output_tokens')),0),
		COALESCE(SUM(json_extract(data,'$.cache_read_tokens')),0),
		COALESCE(SUM(json_extract(data,'$.cache_write_tokens')),0),
		COALESCE(SUM(CASE WHEN json_extract(data,'$.estimated') = 1 THEN 1 ELSE 0 END),0)
		FROM usage_logs WHERE time>=? AND time<? AND COALESCE(json_extract(data,'$.not_sent'),0)=0`, from.UnixMilli(), until.UnixMilli()).Scan(
		&totals.Records, &totals.InputTokens, &totals.OutputTokens, &totals.CacheReadTokens, &totals.CacheWriteTokens, &totals.EstimatedRecords)
	return totals, err
}
