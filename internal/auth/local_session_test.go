package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"torouter/internal/store"
)

func TestLocalSessionWithoutKeyAndIsolation(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := New(s)
	call := func(host, peer, origin string, header bool, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://"+host+"/api/auth/local", nil)
		r.RemoteAddr = peer
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if header {
			r.Header.Set("X-To-Router", "1")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	w := call("127.0.0.1:19191", "127.0.0.1:3456", "http://127.0.0.1:19191", true, nil)
	if w.Code != 200 {
		t.Fatalf("local no-key session: got %d want 200", w.Code)
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 || !s.ValidAdminSession(cs[0].Value) {
		t.Fatal("local session did not create usable management cookie")
	}
	if !cs[0].HttpOnly || cs[0].SameSite != http.SameSiteStrictMode || cs[0].Path != "/api" {
		t.Fatal("local cookie attributes lost")
	}
	if got := call("127.0.0.1:19191", "127.0.0.1:3456", "", true, cs[0]); got.Code != 200 || len(got.Result().Cookies()) != 0 {
		t.Fatal("valid local session should be reused")
	}
	for _, tc := range []struct {
		name, host, peer, origin string
		header                   bool
	}{
		{"missing management header", "127.0.0.1:19191", "127.0.0.1:3456", "", false},
		{"foreign origin", "127.0.0.1:19191", "127.0.0.1:3456", "https://example.com", true},
		{"remote peer", "127.0.0.1:19191", "192.0.2.1:3456", "", true},
		{"foreign host", "example.com", "127.0.0.1:3456", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := call(tc.host, tc.peer, tc.origin, tc.header, nil); got.Code != 403 {
				t.Fatalf("got %d want 403", got.Code)
			}
		})
	}
	model := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.AddCookie(cs[0])
	a.RequireClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(model, r)
	if model.Code != 401 {
		t.Fatal("local management cookie authorized model API")
	}
}
