package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"torouter/internal/auth"
	"torouter/internal/store"
)

func TestDesktopPetStatusUnsupported(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:19191", Hooks{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/desktop/pet", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "桌宠") {
		t.Fatalf("no state hook should report unavailable, got HTTP %d", w.Code)
	}
}

func TestDesktopPetStatusTracksRealWindowAndRequiresSession(t *testing.T) {
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
	running := false
	h := auth.New(s).RequireAdmin(New(s, "http://127.0.0.1:19191", Hooks{CompanionRunning: func() bool { return running }}))
	call := func(authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/desktop/pet", nil)
		if authenticated {
			r.AddCookie(&http.Cookie{Name: "to_router_session", Value: session})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call(false); w.Code != 401 {
		t.Fatalf("state exposed without session: HTTP %d", w.Code)
	}
	for _, value := range []bool{false, true, false} {
		running = value
		w := call(true)
		var got struct {
			Running bool `json:"running"`
		}
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("state HTTP %d/cache=%q", w.Code, w.Header().Get("Cache-Control"))
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Running != value {
			t.Fatal("state did not reflect current window")
		}
	}
}
