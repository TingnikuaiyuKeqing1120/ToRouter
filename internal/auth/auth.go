package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
	"torouter/internal/store"
)

const cookieName = "to_router_session"

type clientContextKey struct{}

func ClientFromContext(ctx context.Context) store.ClientKey {
	k, _ := ClientFromContextOK(ctx)
	return k
}

func ClientFromContextOK(ctx context.Context) (store.ClientKey, bool) {
	k, ok := ctx.Value(clientContextKey{}).(store.ClientKey)
	return k, ok
}

func WithClientContext(ctx context.Context, k store.ClientKey) context.Context {
	return context.WithValue(ctx, clientContextKey{}, k)
}

type Auth struct {
	Store        *store.Store
	mu           sync.Mutex
	failures     int
	blockedUntil time.Time
}

func New(s *store.Store) *Auth { return &Auth{Store: s} }
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func denied(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]any{"error": map[string]string{"message": msg}})
}
func (a *Auth) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-To-Router") != "1" {
			denied(w, 403, "management request header required")
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || !a.Store.ValidAdminSession(c.Value) {
			denied(w, 401, "admin session required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *Auth) RequireClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		header := r.Header.Get("Authorization")
		if len(header) > 7 && header[:7] == "Bearer " {
			token = header[7:]
		}
		if token == "" {
			token = r.Header.Get("x-api-key")
		}
		if token == "" {
			token = r.Header.Get("x-goog-api-key")
		}
		if token == "" {
			denied(w, 401, "client API key required")
			return
		}
		k, err := a.Store.AuthenticateClientKey(token)
		if err != nil {
			denied(w, 401, "invalid client API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithClientContext(r.Context(), k)))
	})
}
func (a *Auth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/local", a.localSession)
	mux.Handle("GET /api/auth/session", a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]bool{"authenticated": true}) })))
	mux.Handle("POST /api/auth/logout", a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie(cookieName)
		if err := a.Store.DeleteAdminSession(c.Value); err != nil {
			denied(w, 500, "cannot revoke session")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/api", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		reply(w, 200, map[string]bool{"ok": true})
	})))
	return mux
}
func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-To-Router") != "1" {
		denied(w, 403, "management request header required")
		return
	}
	var body struct {
		Key      string `json:"key"`
		Remember bool   `json:"remember"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		denied(w, 400, "invalid login body")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		denied(w, 400, "single JSON object required")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Now().Before(a.blockedUntil) {
		w.Header().Set("Retry-After", "30")
		denied(w, 429, "too many login attempts; retry later")
		return
	}
	settings, err := a.Store.GetSettings()
	if err != nil {
		denied(w, 500, "settings unavailable")
		return
	}
	if body.Key == "" || subtle.ConstantTimeCompare([]byte(body.Key), []byte(settings.GatewayKey)) != 1 {
		a.failures++
		if a.failures >= 5 {
			a.blockedUntil = time.Now().Add(30 * time.Second)
			a.failures = 0
		}
		denied(w, 401, "invalid admin key")
		return
	}
	lifetime := 12 * time.Hour
	maxAge := 0
	if body.Remember {
		lifetime = 30 * 24 * time.Hour
		maxAge = int(lifetime.Seconds())
	}
	token, err := a.Store.CreateAdminSession(settings.GatewayKey, lifetime)
	if err != nil {
		denied(w, 500, "cannot create session")
		return
	}
	a.failures = 0
	a.blockedUntil = time.Time{}
	if old, err := r.Cookie(cookieName); err == nil {
		if err := a.Store.DeleteAdminSession(old.Value); err != nil {
			a.Store.DeleteAdminSession(token)
			denied(w, 500, "cannot replace session")
			return
		}
	}
	c := &http.Cookie{Name: cookieName, Value: token, Path: "/api", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: maxAge}
	if maxAge > 0 {
		c.Expires = time.Now().Add(lifetime)
	}
	http.SetCookie(w, c)
	reply(w, 200, map[string]bool{"authenticated": true})
}
