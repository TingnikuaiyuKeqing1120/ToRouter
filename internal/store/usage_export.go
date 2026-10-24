package store

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// csvUsageText neutralizes formulas while leaving csv.Writer responsible for
// commas, quotes and embedded newlines. Numeric columns remain numeric.
func csvUsageText(value string) string {
	trimmed := strings.TrimLeftFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") || len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}
func csvUsageAmount(amount *float64) string {
	if amount == nil {
		return ""
	}
	return strconv.FormatFloat(*amount, 'g', -1, 64)
}
func csvUsageBasis(basis *UsagePricingBasis) (string, error) {
	if basis == nil {
		return "", nil
	}
	raw, err := json.Marshal(basis)
	return string(raw), err
}

// ExportUsageCSV scans the full filtered snapshot one row at a time. Callers
// must stage this output before serving it: a later read or write can fail.
func (s *Store) ExportUsageCSV(ctx context.Context, f UsageFilter, out io.Writer) error {
	if _, err := UsageLocation(f.Timezone); err != nil {
		return err
	}
	f.ExcludeNotSent = true
	where, args := usageWhere(f)
	rows, err := s.db.QueryContext(ctx, "SELECT id,time,channel_id,model,status,data FROM usage_logs"+where+" ORDER BY time,id", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	writer := csv.NewWriter(out)
	if err := writer.Write([]string{"time", "channel_id", "channel_name", "model", "upstream_model", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "estimated", "actual_cost", "currency", "actual_pricing", "official_value", "official_currency", "official_pricing", "status", "duration_ms", "ttft_ms"}); err != nil {
		return err
	}
	for rows.Next() {
		var u Usage
		var id, millis, channelID int64
		var model, raw string
		var status int
		if err := rows.Scan(&id, &millis, &channelID, &model, &status, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			return err
		}
		actual, err := csvUsageBasis(u.ActualPricing)
		if err != nil {
			return err
		}
		official, err := csvUsageBasis(u.OfficialPricing)
		if err != nil {
			return err
		}
		record := []string{
			time.UnixMilli(millis).UTC().Format(time.RFC3339Nano), strconv.FormatInt(channelID, 10), csvUsageText(u.ChannelName), csvUsageText(model), csvUsageText(u.UpstreamModel),
			strconv.FormatInt(u.InputTokens, 10), strconv.FormatInt(u.OutputTokens, 10), strconv.FormatInt(u.CacheReadTokens, 10), strconv.FormatInt(u.CacheWriteTokens, 10), strconv.FormatBool(u.Estimated),
			csvUsageAmount(u.ActualCost), csvUsageText(u.Currency), actual, csvUsageAmount(u.OfficialValue), csvUsageText(u.OfficialCurrency), official,
			strconv.Itoa(status), strconv.FormatInt(u.DurationMS, 10), strconv.FormatInt(u.TTFTMS, 10),
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}
