package console

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torouter/internal/auth"
	"torouter/internal/store"
)

func channelKeyFixture(t *testing.T) (*store.Store, store.Channel, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := store.Channel{
		Name: "Synthetic key pool", Protocol: "openai", BaseURL: "https://example.com", Enabled: true,
		Credentials: []store.CredentialItem{
			{ID: "first", APIKey: "synthetic-first-key", Enabled: true},
			{ID: "second", APIKey: "synthetic-second-key", Enabled: false},
			{ID: "empty", Enabled: true},
			{ID: "with/slash%2F", APIKey: "synthetic-escaped-id-key", Enabled: true},
		},
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Keep the observer on one connection so PRAGMA data_version can detect
	// commits by the store connection, even if a write restored the same data.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return s, c, db
}

func channelKeyRequest(t *testing.T, h http.Handler, path string, status int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != status {
		t.Fatalf("credential disclosure status = %d, want %d", w.Code, status)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential disclosure response permits caching")
	}
	return w
}

func TestChannelCredentialKeyExactIdentityAndReadOnly(t *testing.T) {
	s, c, db := channelKeyFixture(t)
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	assertKey := func(id, expected string) {
		t.Helper()
		var before, after int
		if err := db.QueryRow("PRAGMA data_version").Scan(&before); err != nil {
			t.Fatal(err)
		}
		w := channelKeyRequest(t, h, fmt.Sprintf("/channels/%d/credentials/%s/key", c.ID, url.PathEscape(id)), http.StatusOK)
		var result map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result) != 1 || result["key"] != expected {
			t.Fatal("disclosed key does not match the exact saved credential")
		}
		if err := db.QueryRow("PRAGMA data_version").Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatal("credential disclosure committed a database write")
		}
	}
	// Disabled credentials remain inspectable; no fallback to the first key.
	assertKey("second", "synthetic-second-key")
	assertKey("with/slash%2F", "synthetic-escaped-id-key")
	c.Credentials[0], c.Credentials[1] = c.Credentials[1], c.Credentials[0]
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	assertKey("second", "synthetic-second-key")
	c.Credentials[0].APIKey = "synthetic-latest-saved-key"
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	assertKey("second", "synthetic-latest-saved-key")
	// Normal channel reads still redact every stored plaintext key.
	w := channelKeyRequest(t, h, "/channels", http.StatusOK)
	for _, cred := range c.Credentials {
		if cred.APIKey != "" && strings.Contains(w.Body.String(), cred.APIKey) {
			t.Fatal("ordinary channel response leaked plaintext")
		}
	}
}

func TestChannelCredentialKeyDefaultUsesNormalizedID(t *testing.T) {
	s, _, _ := channelKeyFixture(t)
	c := store.Channel{Name: "Synthetic default", Protocol: "openai", BaseURL: "https://example.com", APIKey: "synthetic-default-key", Enabled: true}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	w := channelKeyRequest(t, h, fmt.Sprintf("/channels/%d/credentials/cred_default/key", c.ID), http.StatusOK)
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["key"] != c.APIKey {
		t.Fatal("normalized default credential was not disclosed")
	}
	channelKeyRequest(t, h, fmt.Sprintf("/channels/%d/credentials/arbitrary/key", c.ID), http.StatusNotFound)
}

func TestChannelCredentialKeyRejectsMissingAndOAuthCredentials(t *testing.T) {
	s, c, db := channelKeyFixture(t)
	c.Credentials = append(c.Credentials, store.CredentialItem{
		ID: "oauth-member", APIKey: "synthetic-mixed-key", Enabled: true,
		OAuth: &store.OAuthCredential{AccessToken: "synthetic-access-token", RefreshToken: "synthetic-refresh-token"},
	})
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	oauth := store.Channel{
		Name: "Synthetic OAuth", CredentialType: store.CredentialCopilotOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.githubcopilot.com"}},
		OAuth:     &store.OAuthCredential{Provider: store.OAuthProviderCopilot, GitHubToken: "synthetic-github-token", AccessToken: "synthetic-access-token"},
	}
	if err := s.SaveChannel(&oauth); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	var before, after int
	if err := db.QueryRow("PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, path string
		status     int
	}{
		{"unknown credential", fmt.Sprintf("/channels/%d/credentials/unknown/key", c.ID), 404},
		{"missing key", fmt.Sprintf("/channels/%d/credentials/empty/key", c.ID), 404},
		{"missing credential ID", fmt.Sprintf("/channels/%d/credentials//key", c.ID), 400},
		{"blank credential ID", fmt.Sprintf("/channels/%d/credentials/%%20/key", c.ID), 404},
		{"unknown channel", "/channels/999999/credentials/first/key", 404},
		{"invalid channel ID", "/channels/abc/credentials/first/key", 400},
		{"missing channel ID", "/channels//credentials/first/key", 400},
		{"zero channel ID", "/channels/0/credentials/first/key", 400},
		{"negative channel ID", "/channels/-1/credentials/first/key", 400},
		{"overflow channel ID", "/channels/9223372036854775808/credentials/first/key", 400},
		{"mixed OAuth member", fmt.Sprintf("/channels/%d/credentials/oauth-member/key", c.ID), 404},
		{"OAuth channel", fmt.Sprintf("/channels/%d/credentials/cred_default/key", oauth.ID), 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := channelKeyRequest(t, h, tt.path, tt.status)
			if strings.Contains(w.Body.String(), "synthetic-") {
				t.Fatal("error disclosed a credential secret")
			}
		})
	}
	if err := db.QueryRow("PRAGMA data_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("failed credential disclosure committed a database write")
	}
	// Invalid escapes must be rejected rather than interpreted as another ID.
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/channels/%d/credentials/first/key", c.ID), nil)
	r.URL.RawPath = fmt.Sprintf("/channels/%d/credentials/%%zz/key", c.ID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "synthetic-") {
		t.Fatal("invalid escaped ID did not fail closed with no-store")
	}
}

func TestChannelCredentialKeyAdminProtection(t *testing.T) {
	s, c, _ := channelKeyFixture(t)
	settings, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAdminSession(settings.GatewayKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, clientToken, err := s.CreateClientKey("Synthetic client")
	if err != nil {
		t.Fatal(err)
	}
	// This is the same wrapper and /api mount used by internal/app.
	h := auth.New(s).RequireAdmin(http.StripPrefix("/api", New(s, "http://127.0.0.1:17878", Hooks{})))
	for _, tt := range []struct {
		name, cookie, bearer string
		status               int
	}{
		{"anonymous", "", "", 401},
		{"invalid session", "invalid", "", 401},
		{"client key", "", clientToken, 401},
		{"admin bearer", "", settings.GatewayKey, 401},
		{"admin session", session, "", 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/channels/%d/credentials/second/key", c.ID), nil)
			r.Header.Set("X-To-Router", "1")
			if tt.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "to_router_session", Value: tt.cookie})
			}
			if tt.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tt.bearer)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("protected disclosure status = %d, want %d; Cache-Control = %q", w.Code, tt.status, w.Header().Get("Cache-Control"))
			}
			if tt.status != 200 && strings.Contains(w.Body.String(), "synthetic-") {
				t.Fatal("unauthorized response disclosed a credential")
			}
		})
	}
}

func TestChannelCredentialKeyStorageErrorDoesNotLeak(t *testing.T) {
	s, c, _ := channelKeyFixture(t)
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	w := channelKeyRequest(t, h, fmt.Sprintf("/channels/%d/credentials/first/key", c.ID), http.StatusInternalServerError)
	if strings.Contains(w.Body.String(), "synthetic-") || strings.Contains(w.Body.String(), "sql:") {
		t.Fatal("storage error exposed secret or implementation detail")
	}
}
