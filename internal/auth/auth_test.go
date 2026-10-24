package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestSessionAndClientIsolation(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	settings, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	a := New(s)
	success := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	call := func(h http.Handler, method, path, body, token string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if csrf {
			r.Header.Set("X-To-Router", "1")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	login := func(remember bool) *http.Cookie {
		b, _ := json.Marshal(map[string]any{"key": settings.GatewayKey, "remember": remember})
		w := call(a.Handler(), "POST", "/api/auth/login", string(b), "", nil, true)
		if w.Code != 200 {
			t.Fatalf("login: %d %s", w.Code, w.Body)
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("missing cookie")
		}
		c := cookies[0]
		if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/api" {
			t.Fatal("unsafe cookie attributes")
		}
		if remember && c.MaxAge != 30*24*3600 {
			t.Fatal("remember expiry")
		}
		if !remember && c.MaxAge != 0 {
			t.Fatal("session cookie persistent")
		}
		return c
	}
	if w := call(a.Handler(), "POST", "/api/auth/login", `{"key":"wrong"}`, "", nil, false); w.Code != 403 {
		t.Fatal("CSRF header missing accepted")
	}
	if w := call(a.Handler(), "POST", "/api/auth/login", `{"key":"wrong"}`, "", nil, true); w.Code != 401 {
		t.Fatal("wrong login accepted")
	}
	session := login(false)
	remember := login(true)
	if w := call(a.Handler(), "GET", "/api/auth/session", "", "", session, false); w.Code != 200 {
		t.Fatal("session not accepted")
	}
	key, token, err := s.CreateClientKey("Codex")
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{token, settings.GatewayKey} {
		if w := call(a.RequireAdmin(success), "GET", "/api/settings", "", credential, nil, false); w.Code != 401 {
			t.Fatal("bearer credential authorized admin")
		}
	}
	if w := call(a.RequireClient(success), "GET", "/v1/models", "", settings.GatewayKey, nil, false); w.Code != 401 {
		t.Fatal("admin key authorized model")
	}
	if w := call(a.RequireClient(success), "GET", "/v1/models", "", "", session, false); w.Code != 401 {
		t.Fatal("session authorized model")
	}
	clientHandler := a.RequireClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ClientFromContext(r.Context()); got.ID != key.ID || got.Name != "Codex" {
			t.Error("missing client attribution")
		}
		w.WriteHeader(204)
	}))
	if w := call(clientHandler, "GET", "/v1/models", "", token, nil, false); w.Code != 204 {
		t.Fatal("client denied")
	}
	if w := call(a.RequireAdmin(success), "PUT", "/api/settings", "", "", session, false); w.Code != 403 {
		t.Fatal("session mutation lacks CSRF check")
	}
	if w := call(a.Handler(), "POST", "/api/auth/logout", "", "", session, true); w.Code != 200 {
		t.Fatal("logout failed")
	}
	if w := call(a.Handler(), "GET", "/api/auth/session", "", "", session, false); w.Code != 401 {
		t.Fatal("logout did not revoke")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a = New(s)
	if w := call(a.Handler(), "GET", "/api/auth/session", "", "", remember, false); w.Code != 200 {
		t.Fatal("remember did not persist")
	}
	expired, err := s.CreateAdminSession(settings.GatewayKey, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.ValidAdminSession(expired) {
		t.Fatal("expired session valid")
	}
	settings.GatewayKey = "rotated-admin-key-123456789"
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if s.ValidAdminSession(remember.Value) {
		t.Fatal("admin rotation retained session")
	}
	if _, err := s.AuthenticateClientKey(token); err != nil {
		t.Fatal("admin rotation revoked client", err)
	}
	if err := s.DeleteClientKey(key.ID); err != nil {
		t.Fatal(err)
	}
	if w := call(a.RequireClient(success), "GET", "/v1/models", "", token, nil, false); w.Code != 401 {
		t.Fatal("revoked key accepted")
	}
}

func TestLoginRateLimit(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := New(s)
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"key":"wrong"}`))
		r.Header.Set("X-To-Router", "1")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		expected := 401
		if i == 5 {
			expected = 429
		}
		if w.Code != expected {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
}
