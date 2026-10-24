package console

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestQuotaRefreshAPIProjectsOneFieldAndNeverCleansHistory(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	before, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(store.Usage{Time: time.Now().AddDate(-1, 0, 0), ChannelID: 1, Model: "old-history"}); err != nil {
		t.Fatal(err)
	}
	call := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/settings/quota-refresh", strings.NewReader(body)))
		return w
	}
	w := call("GET", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"quota_refresh_seconds":300}` {
		t.Fatalf("GET %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{}`, `null`, `[]`, `{"quota_refresh_seconds":null}`, `{"quota_refresh_seconds":"secret-marker"}`, `{"quota_refresh_seconds":29}`, `{"quota_refresh_seconds":86401}`, `{"quota_refresh_seconds":1.5}`, `{"quota_refresh_seconds":30,"secret-marker":"value"}`, `{"quota_refresh_seconds":30} {}`} {
		w := call("PUT", body)
		if w.Code != 400 || strings.Contains(w.Body.String(), "secret-marker") {
			t.Fatalf("invalid request %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{"quota_refresh_seconds":0}`, `{"quota_refresh_seconds":30}`, `{"quota_refresh_seconds":86400}`} {
		w := call("PUT", body)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != body {
			t.Fatalf("PUT %d %s", w.Code, w.Body.String())
		}
	}
	after, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	after.QuotaRefreshSeconds = before.QuotaRefreshSeconds
	oldJSON, _ := json.Marshal(before)
	newJSON, _ := json.Marshal(after)
	if string(oldJSON) != string(newJSON) {
		t.Fatal("quota refresh endpoint changed unrelated settings")
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 1 || rows[0].Model != "old-history" {
		t.Fatalf("refresh setting cleaned history: %+v %v", rows, err)
	}
}
