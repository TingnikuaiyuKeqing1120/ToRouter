package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

// mockCodexBackend is the Codex backend: the Responses facade with the CLI
// headers it expects.
func mockCodexBackend(t *testing.T, hits *atomic.Int32, seen *http.Request) (*httptest.Server, *[]byte) {
	t.Helper()
	var sent []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		sent, _ = io.ReadAll(r.Body)
		if seen != nil {
			*seen = *r.Clone(r.Context())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_codex","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}`)
	}))
	t.Cleanup(server.Close)
	return server, &sent
}

// siwcStub is a token service these tests never call: the gateway needs both
// handles to exist, and only the Codex track is exercised here.
func siwcStub(t *testing.T, s *store.Store) *siwc.Service {
	t.Helper()
	return siwc.NewService(s, nil, siwc.DefaultEndpoints())
}

func codexService(t *testing.T, s *store.Store, issuer string) *codex.Service {
	t.Helper()
	eps := codex.DefaultEndpoints()
	eps.Issuer = issuer
	eps.Token = issuer + "/oauth/token"
	return codex.NewService(s, &http.Client{Timeout: 5 * time.Second}, eps)
}

func addCodexChannel(t *testing.T, s *store.Store, base string) store.Channel {
	t.Helper()
	c := store.Channel{Name: "codex", CredentialType: store.CredentialCodexOAuth, Enabled: true,
		Models:    []string{"test"},
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: base}},
		OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderCodex, RefreshToken: "rt-stored", IDToken: "id-stored",
			ClientID: codex.ClientID, Account: "acct-1", Scopes: codex.ScopeAuthorize,
			AccessToken: "expired-access", ExpiresAt: time.Now().Add(-time.Minute).UTC(),
			AccountLabel: "person@example.test", APIBase: base, Status: store.OAuthStatusActive,
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

// The credential is rotated, and the request carries the Codex client headers:
// the account id, the originator and the CLI user agent — against the backend's
// own /responses path with no /v1 prefix. No profile is enforced: the backend's
// capability surface is unverified, so the body passes through.
func TestCodexChannelInjectsClientHeaders(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	var seen http.Request
	backend, sentBody := mockCodexBackend(t, &hits, &seen)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(codex.TokenResponse{
			AccessToken: "codex-fresh", RefreshToken: "rt-fresh", ExpiresIn: 3600, Scope: codex.ScopeRefresh,
		})
	}))
	defer issuer.Close()
	g := NewWithOAuth(s, copilotTestService(t, s), siwcStub(t, s), codexService(t, s, issuer.URL))
	siwcSettings(t, s)
	addCodexChannel(t, s, backend.URL+"/backend-api/codex")
	body := `{"model":"test","stream":false,"temperature":0.3,"store":true,"instructions":"brief","input":[{"type":"message","role":"user","content":"hi"}]}`
	w := invoke(g, "/v1/responses", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits %d", hits.Load())
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer codex-fresh" {
		t.Fatalf("authorization %q", got)
	}
	for header, want := range map[string]string{
		"ChatGPT-Account-ID": "acct-1",
		"originator":         codex.Originator,
		"User-Agent":         codex.UserAgentValue(),
	} {
		if got := seen.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if seen.URL.Path != "/backend-api/codex/responses" {
		t.Fatalf("path %s", seen.URL.Path)
	}
	// No profile enforcement: the parameters the SIWC track would strip are
	// still here, because the backend's capability surface is unverified.
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(*sentBody, &sent); err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"temperature", "store", "instructions", "input"} {
		if _, ok := sent[param]; !ok {
			t.Fatalf("parameter %s was stripped from a passthrough track", param)
		}
	}
	logs, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(logs) != 1 || logs[0].Degraded != "" {
		t.Fatalf("usage recorded a degradation on a passthrough track: %+v", logs)
	}
}

// The allowlist is one host and https: a row edited directly in the database
// cannot aim the credential anywhere else, and the runtime re-check catches it
// before anything is sent.
func TestCodexCredentialNeverSentToDirectlyEditedHost(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	impostor, _ := mockCodexBackend(t, &hits, nil)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer issuer.Close()
	g := NewWithOAuth(s, copilotTestService(t, s), siwcStub(t, s), codexService(t, s, issuer.URL))
	siwcSettings(t, s)
	c := addCodexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	rewriteChannelRow(t, dir, c.ID, map[string]any{
		"endpoints": []map[string]string{{"format": store.FormatOpenAIResponses, "base_url": impostor.URL + "/backend-api/codex"}},
	})
	w := invoke(g, "/v1/responses", `{"model":"test","input":[]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "allowlist") {
		t.Fatalf("error does not name the allowlist: %s", w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("the credential path received a request")
	}
}

// A rotation that fails is a request-level failure with the reason spelled out.
func TestCodexFailedRotationFailsRequest(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	backend, _ := mockCodexBackend(t, &hits, nil)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer issuer.Close()
	g := NewWithOAuth(s, copilotTestService(t, s), siwcStub(t, s), codexService(t, s, issuer.URL))
	siwcSettings(t, s)
	addCodexChannel(t, s, backend.URL+"/backend-api/codex")
	w := invoke(g, "/v1/responses", `{"model":"test","input":[]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "HTTP 500") {
		t.Fatalf("error does not name the failure: %s", w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("upstream contacted with a credential that could not be rotated")
	}
}
