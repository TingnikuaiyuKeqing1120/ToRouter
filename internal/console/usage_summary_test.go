package console

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"torouter/internal/store"
)

// Seed one transaction so the regression exercises a real large history without
// spending its time on a separate disk commit for every synthetic request.
func seedSummaryHistory(t *testing.T, dir string, rows []store.Usage) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stmt.Exec(row.Time.UnixMilli(), row.ChannelID, row.Model, row.Status, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestUsageSummaryCoversFullPeriod(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(48*time.Hour - time.Millisecond)
	amount := func(v float64) *float64 { return &v }
	rows := []store.Usage{
		{Time: from, ChannelID: 2, ChannelName: "A early", Model: "early", Status: 503, InputTokens: 11, CacheReadTokens: 13, CacheWriteTokens: 17, OutputTokens: 19, OfficialValue: amount(7), OfficialCurrency: "USD", ActualCost: amount(0), Currency: "CNY"},
		{Time: from.Add(time.Millisecond), ChannelID: 2, ChannelName: "A early", Model: "early", Status: 500},
	}
	const bulk = 10005
	for i := 0; i < bulk; i++ {
		rows = append(rows, store.Usage{Time: from.Add(24 * time.Hour), ChannelID: 1, ChannelName: "B main", Model: "main", Status: 200, InputTokens: 2, CacheReadTokens: 5, CacheWriteTokens: 7, OutputTokens: 3, OfficialValue: amount(2.5), OfficialCurrency: "CNY", ActualCost: amount(1.25), Currency: "USD"})
	}
	rows = append(rows,
		store.Usage{Time: to, ChannelID: 3, ChannelName: "C edge", Model: "edge", Status: 200, InputTokens: 23, OutputTokens: 29, OfficialValue: amount(0), OfficialCurrency: "USD", ActualCost: amount(9), Currency: "CNY"},
		store.Usage{Time: from.Add(-time.Millisecond), ChannelID: 4, Model: "outside", Status: 200, InputTokens: 999999},
		store.Usage{Time: to.Add(time.Millisecond), ChannelID: 4, Model: "outside", Status: 200, InputTokens: 999999},
		store.Usage{Time: from, ChannelID: 2, ChannelName: "A early", Model: "early", Status: 503, NotSent: true, InputTokens: 999999, ActualCost: amount(999999), Currency: "USD"},
	)
	seedSummaryHistory(t, dir, rows)
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	query := url.Values{"from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}, "limit": {"1"}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage?"+query.Encode(), nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var got struct {
		store.UsagePricingTotals
		Requests int                `json:"requests"`
		Failures int                `json:"failures"`
		Input    int64              `json:"input_tokens"`
		Output   int64              `json:"output_tokens"`
		Daily    []store.UsageDay   `json:"daily"`
		Channels []store.UsageGroup `json:"by_channel"`
		Models   []store.UsageGroup `json:"by_model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Requests != bulk+3 || got.Failures != 2 || got.Input != 14*bulk+41+23 || got.Output != 3*bulk+19+29 {
		t.Fatalf("incomplete period: requests=%d failures=%d input=%d output=%d", got.Requests, got.Failures, got.Input, got.Output)
	}
	if got.OfficialValues["CNY"] != 2.5*bulk || got.OfficialValues["USD"] != 7 || got.ActualCosts["USD"] != 1.25*bulk || got.ActualCosts["CNY"] != 9 || got.OfficialPricedRequests != bulk+2 || got.ActualPricedRequests != bulk+2 {
		t.Fatalf("incomplete pricing or lost zero/unknown distinction: %+v", got.UsagePricingTotals)
	}
	if len(got.Daily) != 2 || got.Daily[0].Date != "2026-10-01" || got.Daily[0].Requests != 2 || got.Daily[0].OfficialValues["USD"] != 7 || got.Daily[0].ActualPricedRequests != 1 || got.Daily[1].Requests != bulk+1 {
		t.Fatalf("incomplete daily trend: %+v", got.Daily)
	}
	for _, groups := range [][]store.UsageGroup{got.Channels, got.Models} {
		total, official, actual := 0, 0, 0
		for _, item := range groups {
			total += item.Requests
			official += item.OfficialPricedRequests
			actual += item.ActualPricedRequests
		}
		if len(groups) != 3 || total != got.Requests || official != got.OfficialPricedRequests || actual != got.ActualPricedRequests {
			t.Fatalf("incomplete groups: %+v", groups)
		}
	}
	var fields map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"sample_limit", "possibly_truncated"} {
		if _, ok := fields[removed]; ok {
			t.Errorf("retired sampling field remains: %s", removed)
		}
	}

	query.Set("channel_id", "2")
	query.Set("model", "early")
	query.Set("status", "503")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage?"+query.Encode(), nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 || got.Input != 41 || got.ActualPricedRequests != 1 || got.ActualCosts["CNY"] != 0 {
		t.Fatalf("summary filters differ from logs: %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/logs?limit=100000", nil))
	var logs []store.Usage
	if err := json.Unmarshal(w.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(logs) != 10000 {
		t.Fatalf("log display limit changed: status=%d rows=%d", w.Code, len(logs))
	}
}
