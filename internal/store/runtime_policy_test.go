package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRuntimeSettingsUpgradeAtOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE settings SET data=json_set(json_remove(data,'$.failure_cooldown_seconds'),'$.retry_after_max_seconds',222)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var raw string
	if err = s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if _, exists := saved["retry_after_max_seconds"]; exists {
		t.Error("retired waiting ceiling remains in stored settings")
	}
	if string(saved["failure_cooldown_seconds"]) != "10" {
		t.Errorf("initial cooldown = %s, want 10, never the old 222-second ceiling", saved["failure_cooldown_seconds"])
	}
	after, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if after.GatewayKey != before.GatewayKey || after.MaxAttempts != before.MaxAttempts {
		t.Fatal("settings upgrade changed unrelated saved values")
	}
}

func TestUnsentRecordExcludedFromUsageAndPricing(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var rejected Usage
	if err = json.Unmarshal([]byte(`{"not_sent":true,"model":"public","status":503,"error":"all candidates cooling","input_tokens":123,"output_tokens":7}`), &rejected); err != nil {
		t.Fatal(err)
	}
	if err = s.AddUsage(rejected); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListUsage(UsageFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rejection must remain visible in logs: %d, %v", len(rows), err)
	}
	if rows[0].InputTokens != 0 || rows[0].OutputTokens != 0 || rows[0].ActualCost != nil || rows[0].OfficialValue != nil {
		t.Error("unsent request retained fabricated usage or prices")
	}
	encoded, _ := json.Marshal(rows[0])
	var row map[string]any
	_ = json.Unmarshal(encoded, &row)
	if row["not_sent"] != true {
		t.Error("unsent classification was not persisted")
	}
	totals, err := s.UsageTotalsBetween(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if totals.Records != 0 || totals.InputTokens != 0 {
		t.Errorf("unsent request counted as an upstream attempt: %+v", totals)
	}
	_, err = s.UsagePricingTransaction(BackfillFilter{}, -1, false, func(snapshot UsagePricingSnapshot) ([]UsagePricingPatch, error) {
		if len(snapshot.Usage) != 0 {
			t.Errorf("unsent rejection is available for price backfill: %d", len(snapshot.Usage))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
