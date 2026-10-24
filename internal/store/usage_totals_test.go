package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestUsageTotalsIncludesAllAttemptsAndHalfOpenDay(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	if err = s.AddUsage(Usage{Time: day.Add(-time.Millisecond), OutputTokens: 99}); err != nil {
		t.Fatal(err)
	}
	if err = s.AddUsage(Usage{Time: day.AddDate(0, 0, 1), OutputTokens: 99}); err != nil {
		t.Fatal(err)
	}
	// Insert more than the log display cap without a slow fsync per row.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	raw, _ := json.Marshal(Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, Estimated: true})
	for i := 0; i < 10001; i++ {
		if _, err = tx.Exec("INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,?,?,?,?)", day.UnixMilli(), 1, "test", 200, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	totals, err := s.UsageTotalsBetween(context.Background(), day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if totals.Records != 10001 || totals.InputTokens != 80008 || totals.OutputTokens != 20002 || totals.EstimatedRecords != 10001 {
		t.Fatalf("unbounded day totals: %+v", totals)
	}
}
