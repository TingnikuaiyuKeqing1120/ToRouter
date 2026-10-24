package console

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestOAuthAutoOpenAPIOnlyPatchesBooleanWithoutRollingBackQuota(t *testing.T) {
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
	quota := httptest.NewRecorder()
	h.ServeHTTP(quota, httptest.NewRequest("PUT", "/settings/quota-refresh", strings.NewReader(`{"quota_refresh_seconds":60}`)))
	if quota.Code != 200 {
		t.Fatalf("quota patch: %d %s", quota.Code, quota.Body.String())
	}
	for _, body := range []string{`{}`, `null`, `[]`, `{"oauth_auto_open":null}`, `{"oauth_auto_open":0}`, `{"oauth_auto_open":"secret-marker"}`, `{"oauth_auto_open":false,"secret-marker":"value"}`, `{"oauth_auto_open":true} {}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("PUT", "/settings/oauth-auto-open", strings.NewReader(body)))
		if w.Code != 400 || strings.Contains(w.Body.String(), "secret-marker") {
			t.Fatalf("invalid body %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{"oauth_auto_open":false}`, `{"oauth_auto_open":true}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("PUT", "/settings/oauth-auto-open", strings.NewReader(body)))
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != body {
			t.Fatalf("patch: %d %s", w.Code, w.Body.String())
		}
	}
	after, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if *after.QuotaRefreshSeconds != 60 {
		t.Fatal("auto-open toggle rolled back quota interval")
	}
	after.QuotaRefreshSeconds = before.QuotaRefreshSeconds
	after.OAuthAutoOpen = before.OAuthAutoOpen
	oldRaw, _ := json.Marshal(before)
	newRaw, _ := json.Marshal(after)
	if string(oldRaw) != string(newRaw) {
		t.Fatal("auto-open patch changed unrelated settings")
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("auto-open patch cleaned history: %d %v", len(rows), err)
	}
}
