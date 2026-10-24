package console

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestUsageExportFullPeriodEscapingAndUnknown(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 1, 16, 0, 0, 123000000, time.UTC)
	ptr := func(v float64) *float64 { return &v }
	dangerous := " \t=HYPERLINK(\"https://example.invalid\")\n渠道,名称"
	rows := []store.Usage{{Time: at, ChannelID: 2, ChannelName: dangerous, Model: "+SUM(1,2)", UpstreamModel: "@upstream", InputTokens: 11, OutputTokens: 13, CacheReadTokens: 17, CacheWriteTokens: 19, Estimated: true, OfficialValue: ptr(0), OfficialCurrency: "CNY", DurationMS: 23, TTFTMS: 29, Status: 503, Error: "PRIVATE-REQUEST-DETAIL", Attempts: []store.AttemptRecord{{CredentialID: "PRIVATE-CREDENTIAL", CredentialMaskKey: "PRIVATE-MASK"}}}}
	rows[0].OfficialPricing = &store.UsagePricingBasis{Method: "calculated", Currency: "CNY", UnitTokens: 1000, MissingReason: "usage_unknown", Note: "quoted, \"snapshot\"\n说明"}
	const bulk = 10005
	for i := 0; i < bulk; i++ {
		rows = append(rows, store.Usage{Time: at.Add(time.Duration(i+1) * time.Millisecond), ChannelID: 1, ChannelName: "safe", Model: "public", UpstreamModel: "real", Status: 200, ActualCost: ptr(0), Currency: "USD"})
	}
	rows = append(rows, store.Usage{Time: at, ChannelID: 9, NotSent: true, Model: "DO-NOT-EXPORT"}, store.Usage{Time: at.Add(-time.Millisecond), ChannelID: 3, Model: "OUTSIDE"}, store.Usage{Time: at.Add(24 * time.Hour), ChannelID: 3, Model: "OUTSIDE"})
	seedSummaryHistory(t, dir, rows)
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	query := url.Values{"from": {at.Format(time.RFC3339Nano)}, "to": {at.Add(time.Hour).Format(time.RFC3339Nano)}, "limit": {"1"}, "timezone": {"Asia/Shanghai"}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage/export?"+query.Encode(), nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/csv; charset=utf-8" || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	data, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != bulk+2 {
		t.Fatalf("incomplete export: got %d rows", len(data)-1)
	}
	columns := map[string]int{}
	for i, key := range data[0] {
		columns[key] = i
	}
	get := func(row []string, key string) string {
		t.Helper()
		i, ok := columns[key]
		if !ok {
			t.Fatalf("missing CSV column %s", key)
		}
		return row[i]
	}
	first := data[1]
	if get(first, "time") != at.Format(time.RFC3339Nano) || get(first, "channel_name") != "'"+dangerous || get(first, "model") != "'+SUM(1,2)" || get(first, "upstream_model") != "'@upstream" {
		t.Fatalf("time/CSV/formula escaping lost: %v", first)
	}
	if get(first, "estimated") != "true" || get(first, "actual_cost") != "" || get(first, "official_value") != "0" || get(first, "actual_pricing") != "" || get(first, "input_tokens") != "11" || get(first, "output_tokens") != "13" || get(first, "cache_read_tokens") != "17" || get(first, "cache_write_tokens") != "19" || get(first, "duration_ms") != "23" {
		t.Fatalf("missing/zero/usage columns: %v", first)
	}
	if get(data[2], "actual_cost") != "0" || get(data[2], "official_value") != "" {
		t.Fatal("zero and unknown conflated", data[2])
	}
	var exportedBasis store.UsagePricingBasis
	if err := json.Unmarshal([]byte(get(first, "official_pricing")), &exportedBasis); err != nil || exportedBasis.Note != rows[0].OfficialPricing.Note || exportedBasis.UnitTokens != 1000 || exportedBasis.MissingReason != "usage_unknown" {
		t.Fatal("saved pricing basis lost in CSV", exportedBasis, err)
	}
	for _, private := range []string{"PRIVATE-REQUEST-DETAIL", "PRIVATE-CREDENTIAL", "PRIVATE-MASK", "DO-NOT-EXPORT", "OUTSIDE"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("unexpected CSV value %s", private)
		}
	}
	query.Set("channel_id", "2")
	query.Set("model", "+SUM(1,2)")
	query.Set("status", "503")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage/export?"+query.Encode(), nil))
	filtered, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(filtered) != 2 || filtered[1][columns["model"]] != "'+SUM(1,2)" {
		t.Fatal("export filters differ", w.Code, filtered)
	}
}

func TestUsageExportReadFailureDoesNotSendPartialCSV(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows := make([]store.Usage, 200)
	for i := range rows {
		rows[i] = store.Usage{Time: time.UnixMilli(int64(i + 1)), Model: "already-read-before-late-failure"}
	}
	seedSummaryHistory(t, dir, rows)
	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(2000,0,'broken',200,?)", `{"estimated":"invalid-bool"}`); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage/export", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "channel_id,") || strings.Contains(w.Body.String(), "already-read-before-late-failure") || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("partial CSV exposed as success: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
}

func TestUsageTimezoneAPIValidationAndLocalDay(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC)
	if err := s.AddUsage(store.Usage{Time: at}); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	for _, path := range []string{"/usage", "/usage/export"} {
		for _, zone := range []string{"Mars/Olympus", "Local"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path+"?timezone="+url.QueryEscape(zone), nil))
			if w.Code != 400 {
				t.Fatalf("%s invalid timezone %q: %d", path, zone, w.Code)
			}
		}
	}
	for _, tc := range []struct{ zone, date string }{{"", "2026-10-01"}, {"Asia/Shanghai", "2026-10-02"}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/usage?timezone="+url.QueryEscape(tc.zone), nil))
		var got store.UsageSummary
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || len(got.Daily) != 1 || got.Daily[0].Date != tc.date {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// Date-only upper bounds still cover the entire absolute UTC date.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage/export?from=2026-10-01&to=2026-10-01", nil))
	data, err := csv.NewReader(w.Body).ReadAll()
	if err != nil || w.Code != 200 || len(data) != 2 {
		t.Fatal(w.Code, len(data), err)
	}
}
