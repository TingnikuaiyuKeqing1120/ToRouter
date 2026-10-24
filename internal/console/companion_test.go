package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"torouter/internal/auth"
	"torouter/internal/companion"
	"torouter/internal/store"
)

func TestCompanionRequiresAdminAndProtectsWindowAction(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAdminSession(settings.GatewayKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	live := companion.New()
	opened := 0
	h := auth.New(s).RequireAdmin(New(s, "http://127.0.0.1:19191", Hooks{Companion: live.Snapshot, OpenCompanion: func() error { opened++; return nil }}))
	request := func(method, path string, cookie, header bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if cookie {
			r.AddCookie(&http.Cookie{Name: "to_router_session", Value: session})
		}
		if header {
			r.Header.Set("X-To-Router", "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/companion", false, false); w.Code != 401 {
		t.Fatal("snapshot exposed without authentication")
	}
	if w := request("POST", "/companion/open", true, false); w.Code != 403 || opened != 0 {
		t.Fatal("window opened without management header")
	}
	if w := request("POST", "/companion/open", false, true); w.Code != 401 || opened != 0 {
		t.Fatal("window opened without session")
	}
	if err := s.AddUsage(store.Usage{Time: time.Now(), InputTokens: 7, OutputTokens: 9, CacheReadTokens: 3, Status: 200}); err != nil {
		t.Fatal(err)
	}
	w := request("GET", "/companion", true, false)
	var v struct {
		companion.Snapshot
		Today store.UsageTotals `json:"today"`
	}
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot HTTP %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.SessionID == "" || v.Active == nil || v.Recent == nil || v.Today.Records != 1 || v.Today.InputTokens != 10 || v.Today.OutputTokens != 9 {
		t.Fatalf("bad snapshot contract: %+v", v)
	}
	w = request("POST", "/companion/open", true, true)
	var result struct {
		Requested bool `json:"requested"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Requested || opened != 1 {
		t.Fatal("window request not acknowledged once")
	}
}
