package gateway

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/copilot"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

// addCopilotChannel binds a downstream model to a copilot_oauth channel whose
// stored bearer token has just expired, so the first request must exchange.
func addCopilotChannel(t *testing.T, s *store.Store, base, format, key string) store.Channel {
	t.Helper()
	c := store.Channel{Name: "copilot", CredentialType: store.CredentialCopilotOAuth, Enabled: true,
		Models:    []string{"test"},
		Endpoints: []store.Endpoint{{Format: format, BaseURL: base}},
		OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderCopilot, GitHubToken: key, AccessToken: "expired-bearer",
			ExpiresAt: time.Now().Add(-time.Minute).UTC(), Status: store.OAuthStatusActive,
		}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := store.DownstreamModel{ID: "test", Enabled: true, Fallback: true}
	for _, existing := range mustListModels(t, s) {
		if existing.ID == m.ID {
			m = existing
			break
		}
	}
	m.Bindings = append(m.Bindings, store.Binding{ChannelID: c.ID, Model: "test", Priority: 0, Enabled: true})
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	return c
}

func mustListModels(t *testing.T, s *store.Store) []store.DownstreamModel {
	t.Helper()
	rows, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// mockCopilotChat is the Copilot model API: it records how it was called and
// answers the OpenAI facade.
func mockCopilotChat(t *testing.T, hits *atomic.Int32, last *http.Request) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if last != nil {
			*last = *r.Clone(r.Context())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	return server
}

// mockExchange is the api.github.com side: the copilot_internal exchange, and
// nothing else. It counts its calls so a rotation can be proved single.
func mockExchange(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			w.WriteHeader(404)
			return
		}
		calls.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "fresh-copilot-jwt", "expires_at": time.Now().Add(25 * time.Minute).Unix()})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func copilotService(t *testing.T, s *store.Store, github string) *copilot.Service {
	t.Helper()
	return copilot.NewService(s, &http.Client{Timeout: 5 * time.Second}, copilot.Endpoints{GitHub: github, API: github, ModelAPI: github})
}

// The happy path: an expired credential is exchanged on the way out, and the
// request carries the Copilot client disguise — the bearer token, the
// copilot-integration-id/editor pair, the API version pin and the VS Code UA —
// against the path Copilot actually serves.
func TestCopilotChannelInjectsBearerAndFixedHeaders(t *testing.T) {
	// The allowlist is the hard rule; these tests drive the credential at a
	// local double, so the documented testing escape hatch is set explicitly.
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	var seen http.Request
	chat := mockCopilotChat(t, &hits, &seen)
	github, exchanges := mockExchange(t, http.StatusOK)
	g := NewWithOAuth(s, copilotService(t, s, github.URL), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	if err = s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: 1, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}}); err != nil {
		t.Fatal(err)
	}
	addCopilotChannel(t, s, chat.URL, store.FormatOpenAIChatCompletions, "gho_live")
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits %d", hits.Load())
	}
	if exchanges.Load() != 1 {
		t.Fatalf("exchanges %d", exchanges.Load())
	}
	if seen.URL.Path != "/chat/completions" {
		t.Fatalf("path %s", seen.URL.Path)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer fresh-copilot-jwt" {
		t.Fatalf("authorization %q", got)
	}
	for header, want := range map[string]string{
		"copilot-integration-id": copilot.IntegrationID,
		"editor-version":         copilot.EditorVersion(),
		"editor-plugin-version":  copilot.EditorPluginVersion(),
		"x-github-api-version":   copilot.APIVersion,
		"User-Agent":             copilot.UserAgent(),
		"Content-Type":           "application/json",
	} {
		if got := seen.Header.Get(header); got != want {
			t.Fatalf("%s: %q want %q", header, got, want)
		}
	}
}

// The Anthropic facade keeps its own path: /v1/messages off the API base, with
// no /v1 prefix added to the OpenAI-style one.
func TestCopilotAnthropicFacadePathAndCredential(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	var seen http.Request
	chat := mockCopilotChat(t, &hits, &seen)
	github, _ := mockExchange(t, http.StatusOK)
	g := NewWithOAuth(s, copilotService(t, s, github.URL), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	if err = s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: 1, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}}); err != nil {
		t.Fatal(err)
	}
	addCopilotChannel(t, s, chat.URL, store.FormatAnthropicMessages, "gho_live")
	w := invoke(g, "/v1/messages", `{"model":"test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if seen.URL.Path != "/v1/messages" {
		t.Fatalf("path %s", seen.URL.Path)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer fresh-copilot-jwt" {
		t.Fatalf("authorization %q", got)
	}
	if got := seen.Header.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key set on an oauth channel: %q", got)
	}
}

// The runtime half of the host allowlist: SaveChannel already refuses a
// non-allowlisted base URL, and this proves a row edited straight in the
// database cannot aim the credential at an impostor either. No request — and no
// Authorization header — reaches the fake.
func TestCopilotCredentialNeverSentToDirectlyEditedHost(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	var seen http.Request
	impostor := mockCopilotChat(t, &hits, &seen)
	github, exchanges := mockExchange(t, http.StatusOK)
	g := NewWithOAuth(s, copilotService(t, s, github.URL), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	if err = s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: 1, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}}); err != nil {
		t.Fatal(err)
	}
	c := addCopilotChannel(t, s, "https://api.githubcopilot.com", store.FormatOpenAIChatCompletions, "gho_live")
	// Re-point the stored row at the impostor without going through
	// SaveChannel: the sealed credential stays valid, only the destination moves.
	rewriteChannelRow(t, dir, c.ID, map[string]any{
		"endpoints": []map[string]string{{"format": store.FormatOpenAIChatCompletions, "base_url": impostor.URL}},
	})
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "allowlist") {
		t.Fatalf("error does not name the allowlist: %s", w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("credential path received a request")
	}
	if exchanges.Load() != 0 {
		t.Fatal("exchanged for a destination that must never be used")
	}
}

// An expired credential whose refresh fails is a request-level failure with the
// reason spelled out; the upstream is not contacted, and the stored credential
// is left as it was.
func TestCopilotFailedRefreshFailsRequestWithReason(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	chat := mockCopilotChat(t, &hits, nil)
	github, _ := mockExchange(t, http.StatusInternalServerError)
	g := NewWithOAuth(s, copilotService(t, s, github.URL), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	if err = s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: 1, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}}); err != nil {
		t.Fatal(err)
	}
	c := addCopilotChannel(t, s, chat.URL, store.FormatOpenAIChatCompletions, "gho_live")
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "copilot token exchange returned HTTP 500") {
		t.Fatalf("error does not name the failure: %s", w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("upstream contacted with a credential that could not be refreshed")
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusActive || stored.OAuth.AccessToken != "expired-bearer" {
		t.Fatalf("failed exchange damaged the credential: %+v", stored.OAuth)
	}
}

// rewriteChannelRow edits one stored channel row directly, the way a hand-edited
// database would, so the runtime guards can be tested against their threat model.
func rewriteChannelRow(t *testing.T, dir string, id int64, fields map[string]any) {
	t.Helper()
	db := openSQLite(t, dir)
	defer db.Close()
	var raw string
	if err := db.QueryRow("SELECT data FROM channels WHERE id=?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		row[key] = value
	}
	out, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE channels SET data=? WHERE id=?", string(out), id); err != nil {
		t.Fatal(err)
	}
}

func openSQLite(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
