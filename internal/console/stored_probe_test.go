package console

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"torouter/internal/codex"
	"torouter/internal/copilot"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

// probeHit is one request a stored probe sent upstream: where it went, how it
// authenticated and what it carried, so a test can compare against what the
// gateway itself would have sent.
type probeHit struct {
	path   string
	header http.Header
	body   map[string]any
}

// probeUpstream is the inference side of a stored probe: the facades a channel
// declares, on one server. mode decides what it answers, and every request is
// recorded, because the point of the stored probe is that it reaches the same
// destination with the same credential the gateway uses.
type probeUpstream struct {
	*httptest.Server
	mu         sync.Mutex
	hits       []probeHit
	mode       string // json, sse, sse_error, http_error, bad_shape, redirect
	redirectTo string
}

func newProbeUpstream(t *testing.T) *probeUpstream {
	t.Helper()
	up := &probeUpstream{mode: "json"}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		up.mu.Lock()
		up.hits = append(up.hits, probeHit{path: r.URL.Path, header: r.Header.Clone(), body: body})
		mode, target := up.mode, up.redirectTo
		up.mu.Unlock()
		switch mode {
		case "redirect":
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusFound)
		case "sse":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n"+
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"message\"}]}}\n\n")
		case "sse_error":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"message\":\"the credential was refused\"}}\n\n")
		case "http_error":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"message":"upstream fell over"}}`)
		case "bad_shape":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"unexpected":"payload"}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/chat/completions"):
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
			case strings.HasSuffix(r.URL.Path, "/messages"):
				io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"OK"}]}`)
			default:
				io.WriteString(w, `{"id":"resp-1","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}`)
			}
		}
	}))
	t.Cleanup(up.Close)
	return up
}

func (up *probeUpstream) recorded() []probeHit {
	up.mu.Lock()
	defer up.mu.Unlock()
	return append([]probeHit{}, up.hits...)
}

func probeStore(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return dir, s
}

func storedProbeConsole(t *testing.T, s *store.Store, copilotSvc *copilot.Service, siwcSvc *siwc.Service, codexSvc *codex.Service) http.Handler {
	t.Helper()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, copilotSvc, siwcSvc, codexSvc)
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	return h
}

func postStoredProbe(t *testing.T, h http.Handler, id int64, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/channels/"+strconv.FormatInt(id, 10)+"/probe", bytes.NewReader(raw)))
	return w
}

func probeResult(t *testing.T, w *httptest.ResponseRecorder) toolResult {
	t.Helper()
	var result toolResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// oauthChannel saves one OAuth channel the way the authorization flow leaves it:
// a fresh access token, so resolving the credential costs no upstream call and
// the probe's only request is the paid one. A caller that wants an expired
// credential sets the fields itself; the zero values are filled in.
func oauthChannel(t *testing.T, s *store.Store, credentialType, provider, base, format string, oauth store.OAuthCredential) store.Channel {
	t.Helper()
	oauth.Provider = provider
	if oauth.AccessToken == "" {
		oauth.AccessToken = "fresh-access-token"
	}
	if oauth.ExpiresAt.IsZero() {
		oauth.ExpiresAt = time.Now().Add(time.Hour).UTC()
	}
	if oauth.Status == "" {
		oauth.Status = store.OAuthStatusActive
	}
	c := store.Channel{
		Name:           credentialType + " channel",
		CredentialType: credentialType,
		Enabled:        true,
		Models:         []string{"gpt-5"},
		Endpoints:      []store.Endpoint{{Format: format, BaseURL: base}},
		OAuth:          &oauth,
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// probeDB opens the store's own database file. A probe test needs the states
// SaveChannel refuses to write — an api_key channel whose key was never
// written, a channel that declares no endpoint — and the way to reach them is
// the one the runtime checks exist for: a row edited behind the console's back.
// The DSN carries the store's own busy_timeout pragma (store.go's connection
// setup): this second connection writes the same WAL database, and without the
// timeout a write that meets the store's lock fails with SQLITE_BUSY instead of
// waiting for it.
func probeDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func readRow(t *testing.T, dir string, id int64) map[string]any {
	t.Helper()
	db := probeDB(t, dir)
	var raw string
	if err := db.QueryRow("SELECT data FROM channels WHERE id=?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func writeRow(t *testing.T, dir string, id int64, row map[string]any) {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	db := probeDB(t, dir)
	if _, err := db.Exec("UPDATE channels SET data=? WHERE id=?", string(raw), id); err != nil {
		t.Fatal(err)
	}
}

// A stored api_key channel is probed with the key it already holds: one request,
// to the path the gateway would use for the format, carrying the saved key.
func TestStoredProbeAPIKeyChannelUsesSavedKey(t *testing.T) {
	up := newProbeUpstream(t)
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, nil, nil, nil)
	c := store.Channel{Name: "keyed", Enabled: true, Models: []string{"a"}, APIKey: "saved-key",
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL + "/api"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	w := postStoredProbe(t, h, c.ID, map[string]any{"model": "a", "confirmed": true})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if !result.OK || result.Model != "a" || result.Diagnostic != nil {
		t.Fatalf("probe %s", w.Body)
	}
	if result.DurationMS < 0 {
		t.Fatalf("duration %d", result.DurationMS)
	}
	hits := up.recorded()
	if len(hits) != 1 {
		t.Fatalf("probe made %d requests", len(hits))
	}
	if hits[0].path != "/api/v1/chat/completions" {
		t.Fatalf("path %q", hits[0].path)
	}
	if got := hits[0].header.Get("Authorization"); got != "Bearer saved-key" {
		t.Fatalf("credential %q", got)
	}
	if hits[0].body["model"] != "a" || hits[0].body["max_tokens"] != float64(64) || hits[0].body["stream"] != false {
		t.Fatalf("body %v", hits[0].body)
	}
	// A probe is not business traffic: nothing is recorded against the channel.
	if rows, err := s.ListUsage(store.UsageFilter{}); err != nil || len(rows) != 0 {
		t.Fatal("probe polluted business logs")
	}
}

// The Copilot facades are reached the way the gateway reaches them: both off the
// API base, the OpenAI one without the /v1 prefix the Anthropic one carries, and
// both with the client disguise the Copilot API asks for.
func TestStoredProbeCopilotFacades(t *testing.T) {
	// The channel this test writes points at the local double, so the documented
	// testing escape hatch for the OAuth host allowlist is set explicitly.
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, tc := range []struct {
		name   string
		format string
		want   string
	}{
		{"chat", store.FormatOpenAIChatCompletions, "/chat/completions"},
		{"anthropic", store.FormatAnthropicMessages, "/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newProbeUpstream(t)
			_, s := probeStore(t)
			h := storedProbeConsole(t, s, copilot.NewService(s, nil, copilot.DefaultEndpoints()), nil, nil)
			c := oauthChannel(t, s, store.CredentialCopilotOAuth, store.OAuthProviderCopilot, up.URL, tc.format,
				store.OAuthCredential{GitHubToken: "gho_test"})
			w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5", "confirmed": true, "formats": []string{tc.format}})
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			result := probeResult(t, w)
			if !result.OK {
				t.Fatalf("probe %s", w.Body)
			}
			hits := up.recorded()
			if len(hits) != 1 {
				t.Fatalf("probe made %d requests", len(hits))
			}
			if hits[0].path != tc.want {
				t.Fatalf("path %q, want %q", hits[0].path, tc.want)
			}
			if got := hits[0].header.Get("Authorization"); got != "Bearer fresh-access-token" {
				t.Fatalf("credential %q", got)
			}
			if hits[0].header.Get("copilot-integration-id") == "" || hits[0].header.Get("editor-version") == "" {
				t.Fatalf("copilot disguise missing: %v", hits[0].header)
			}
			// The body is the one that facade's shape asks for — the messages
			// shape on the Anthropic path, with the fixed probe prompt — so a
			// path that answers is answering the request the gateway would make.
			if got := hits[0].body["model"]; got != "gpt-5" {
				t.Fatalf("model %v", got)
			}
			if got := hits[0].body["max_tokens"]; got != float64(64) {
				t.Fatalf("max_tokens %v", got)
			}
			if got := hits[0].body["stream"]; got != false {
				t.Fatalf("stream %v", got)
			}
			messages, _ := hits[0].body["messages"].([]any)
			if len(messages) != 1 {
				t.Fatalf("messages %v", hits[0].body["messages"])
			}
			if turn, _ := messages[0].(map[string]any); turn["content"] != "hi" {
				t.Fatalf("prompt %v", messages[0])
			}
		})
	}
}

// The Codex backend is Responses-shaped at {base}/responses with no /v1 prefix,
// scoped by the account id, and it answers JSON when the caller does not stream.
func TestStoredProbeCodexResponses(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, tc := range []struct {
		name string
		mode string
	}{
		{"json answer", "json"},
		{"sse answer", "sse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newProbeUpstream(t)
			up.mode = tc.mode
			_, s := probeStore(t)
			h := storedProbeConsole(t, s, nil, nil, codex.NewService(s, nil, codex.DefaultEndpoints()))
			// The backend base is what the authorization stores: the documented
			// chatgpt.com/backend-api/codex, doubles and all.
			c := oauthChannel(t, s, store.CredentialCodexOAuth, store.OAuthProviderCodex, up.URL+"/backend-api/codex", store.FormatOpenAIResponses,
				store.OAuthCredential{RefreshToken: "rt_test", ClientID: "app_test", Account: "acct-1"})
			w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5-codex", "confirmed": true})
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			result := probeResult(t, w)
			if !result.OK {
				t.Fatalf("probe %s", w.Body)
			}
			hits := up.recorded()
			if len(hits) != 1 {
				t.Fatalf("probe made %d requests", len(hits))
			}
			if hits[0].path != "/backend-api/codex/responses" {
				t.Fatalf("path %q", hits[0].path)
			}
			if got := hits[0].header.Get("ChatGPT-Account-ID"); got != "acct-1" {
				t.Fatalf("account id %q", got)
			}
			if hits[0].header.Get("originator") == "" {
				t.Fatalf("codex originator missing: %v", hits[0].header)
			}
			if got := hits[0].header.Get("Authorization"); got != "Bearer fresh-access-token" {
				t.Fatalf("credential %q", got)
			}
			if got := hits[0].header.Get("Accept"); got != "text/event-stream" {
				t.Fatalf("accept %q", got)
			}
			if hits[0].body["stream"] != true || hits[0].body["store"] != false || hits[0].body["instructions"] != "" {
				t.Fatalf("body %v", hits[0].body)
			}
			// The Codex backend validates the body field by field: input as a
			// message list ("Input must be a list" for a string) and store false
			// ("Store must be set to false"). The shape mirrors Codex-Manager's
			// warmup request.
			input, ok := hits[0].body["input"].([]any)
			if !ok || len(input) != 1 {
				t.Fatalf("input %v", hits[0].body["input"])
			}
			item, ok := input[0].(map[string]any)
			if !ok || item["role"] != "user" || item["type"] != "message" {
				t.Fatalf("input item %v", input[0])
			}
		})
	}
}

// The SIWC profile answers SSE whatever the caller asked for, so its probe asks
// for SSE and reads the stream: a completion event is the model answering.
func TestStoredProbeChatGPTResponses(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	up := newProbeUpstream(t)
	up.mode = "sse"
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, nil, siwc.NewService(s, nil, siwc.DefaultEndpoints()), nil)
	c := oauthChannel(t, s, store.CredentialChatGPTOAuth, store.OAuthProviderChatGPT, up.URL+"/v1", store.FormatOpenAIResponses,
		store.OAuthCredential{RefreshToken: "rt_test", ClientID: "oaiapp_test", HostID: "urn:uuid:host"})
	w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5", "confirmed": true})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if !result.OK {
		t.Fatalf("probe %s", w.Body)
	}
	hits := up.recorded()
	if len(hits) != 1 {
		t.Fatalf("probe made %d requests", len(hits))
	}
	if hits[0].path != "/v1/responses" {
		t.Fatalf("path %q", hits[0].path)
	}
	if got := hits[0].header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept %q", got)
	}
	if got := hits[0].header.Get("Authorization"); got != "Bearer fresh-access-token" {
		t.Fatalf("credential %q", got)
	}
}

// An upstream that answers 2xx without serving the model is a failed probe: the
// point is that the facade really runs the model, not that a URL answered.
func TestStoredProbeRejectsUnconvincingAnswers(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, tc := range []struct {
		name string
		mode string
		code string
	}{
		{"wrong shape", "bad_shape", "invalid_response"},
		{"http error", "http_error", "upstream_http"},
		{"sse error event", "sse_error", "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newProbeUpstream(t)
			up.mode = tc.mode
			_, s := probeStore(t)
			h := storedProbeConsole(t, s, nil, nil, codex.NewService(s, nil, codex.DefaultEndpoints()))
			c := oauthChannel(t, s, store.CredentialCodexOAuth, store.OAuthProviderCodex, up.URL, store.FormatOpenAIResponses,
				store.OAuthCredential{RefreshToken: "rt_test", ClientID: "app_test", Account: "acct-1"})
			w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5-codex", "confirmed": true})
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			result := probeResult(t, w)
			if result.OK {
				t.Fatalf("an unconvincing answer passed: %s", w.Body)
			}
			if result.Diagnostic == nil || result.Diagnostic.Code != tc.code {
				t.Fatalf("diagnostic %s", w.Body)
			}
		})
	}
}

// A track not explicitly injected starts disabled. Adding a channel afterwards
// does not override that persisted choice or contact the provider.
func TestStoredProbeWithoutTokenService(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	up := newProbeUpstream(t)
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, nil, siwc.NewService(s, nil, siwc.DefaultEndpoints()), nil)
	c := oauthChannel(t, s, store.CredentialCopilotOAuth, store.OAuthProviderCopilot, up.URL, store.FormatOpenAIChatCompletions,
		store.OAuthCredential{GitHubToken: "gho_test"})
	w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5", "confirmed": true})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if result.OK || result.Diagnostic == nil || result.Diagnostic.Code != "plugin_disabled" || !strings.Contains(result.Diagnostic.Suggestion, "插件库") {
		t.Fatalf("probe %s", w.Body)
	}
	if hits := up.recorded(); len(hits) != 0 {
		t.Fatalf("an unresolvable credential still sent %d requests", len(hits))
	}
}

// A format the profile does not serve is refused with the gateway's own wording
// before anything is sent: naming it asks for a facade the credential has no
// business at. The host allowlist is re-checked the same way, per request,
// because a row can be re-pointed behind the console's back.
func TestStoredProbeRefusesUnsendableDeclarations(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, tc := range []struct {
		name    string
		edit    func(row map[string]any)
		draft   map[string]any
		formats []string
		want    string
	}{
		{
			name:    "format the profile does not serve",
			formats: []string{store.FormatOpenAIChatCompletions},
			want:    "the codex backend serves only the responses endpoint",
		},
		{
			name: "host off the allowlist",
			edit: func(row map[string]any) {
				endpoints, _ := row["endpoints"].([]any)
				e, _ := endpoints[0].(map[string]any)
				e["base_url"] = "https://api.example.test/v1"
			},
			want: "is not on the codex_oauth host allowlist",
		},
		{
			// The editor's draft names the destination, so its endpoints are
			// checked by the same allowlist a stored row is: the security
			// bottom line does not move because the address arrived in a probe
			// body rather than in the channel row.
			name: "draft endpoint off the allowlist",
			draft: map[string]any{
				"endpoints": []map[string]any{{"format": store.FormatOpenAIResponses, "base_url": "https://api.example.test"}},
			},
			want: "is not on the codex_oauth host allowlist",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newProbeUpstream(t)
			dir, s := probeStore(t)
			h := storedProbeConsole(t, s, nil, nil, codex.NewService(s, nil, codex.DefaultEndpoints()))
			c := oauthChannel(t, s, store.CredentialCodexOAuth, store.OAuthProviderCodex, up.URL, store.FormatOpenAIResponses,
				store.OAuthCredential{RefreshToken: "rt_test", ClientID: "app_test", Account: "acct-1"})
			if tc.edit != nil {
				row := readRow(t, dir, c.ID)
				tc.edit(row)
				writeRow(t, dir, c.ID, row)
			}
			body := map[string]any{"model": "gpt-5-codex", "confirmed": true, "formats": tc.formats}
			if tc.draft != nil {
				body["channel"] = tc.draft
			}
			w := postStoredProbe(t, h, c.ID, body)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			result := probeResult(t, w)
			if result.OK {
				t.Fatalf("an unsendable declaration probed anyway: %s", w.Body)
			}
			if result.Diagnostic == nil || !strings.Contains(result.Diagnostic.Message, tc.want) {
				t.Fatalf("diagnostic %s", w.Body)
			}
			if hits := up.recorded(); len(hits) != 0 {
				t.Fatalf("a refused declaration still sent %d requests", len(hits))
			}
		})
	}
}

// A credential that cannot be resolved is an answer, not a refusal: nothing was
// sent, and the reason travels desensitized.
func TestStoredProbeBearerFailure(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	up := newProbeUpstream(t)
	var exchanges atomic.Int32
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		exchanges.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(github.Close)
	_, s := probeStore(t)
	svc := copilot.NewService(s, nil, copilot.Endpoints{GitHub: github.URL, API: github.URL, ModelAPI: github.URL})
	h := storedProbeConsole(t, s, svc, nil, nil)
	c := oauthChannel(t, s, store.CredentialCopilotOAuth, store.OAuthProviderCopilot, up.URL, store.FormatOpenAIChatCompletions,
		store.OAuthCredential{GitHubToken: "gho_secret", AccessToken: "expired-bearer", ExpiresAt: time.Now().Add(-time.Minute).UTC()})
	w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5", "confirmed": true})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if result.OK {
		t.Fatalf("a credential that could not be refreshed probed anyway: %s", w.Body)
	}
	if result.Diagnostic == nil || result.Diagnostic.Source != "gateway" || result.Diagnostic.Code != "auth" {
		t.Fatalf("diagnostic %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "gho_secret") || strings.Contains(w.Body.String(), "expired-bearer") {
		t.Fatalf("the credential leaked into the answer: %s", w.Body)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("exchanges %d", exchanges.Load())
	}
	if hits := up.recorded(); len(hits) != 0 {
		t.Fatalf("a failed credential still sent %d requests", len(hits))
	}
}

// The refusals are about the request itself, so they never reach the upstream:
// no confirmation, no model, no endpoint, no saved key.
func TestStoredProbeRefusals(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	up := newProbeUpstream(t)
	dir, s := probeStore(t)
	h := storedProbeConsole(t, s, nil, nil, nil)
	// A channel that stores no api key at all, so the keyless api_key row below
	// can borrow the ciphertext such a channel writes for the empty key.
	oauthChannel(t, s, store.CredentialCopilotOAuth, store.OAuthProviderCopilot, up.URL, store.FormatOpenAIChatCompletions,
		store.OAuthCredential{GitHubToken: "gho_test"})
	keyed := store.Channel{Name: "keyed", Enabled: true, Models: []string{"a"}, APIKey: "saved-key",
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL + "/api"}}}
	if err := s.SaveChannel(&keyed); err != nil {
		t.Fatal(err)
	}
	// An api_key channel whose key was never written, and one that declares no
	// endpoint: neither is a state SaveChannel writes, so the rows are edited
	// directly — the scenario the 400s exist for.
	keyless := store.Channel{Name: "keyless", Enabled: true, Models: []string{"a"}, APIKey: "saved-key",
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL + "/api"}}}
	if err := s.SaveChannel(&keyless); err != nil {
		t.Fatal(err)
	}
	row := readRow(t, dir, keyless.ID)
	row["api_key"] = ""
	row["credentials"] = []any{}
	writeRow(t, dir, keyless.ID, row)

	endpointless := store.Channel{Name: "endpointless", Enabled: true, Models: []string{"a"}, APIKey: "saved-key",
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL + "/api"}}}
	if err := s.SaveChannel(&endpointless); err != nil {
		t.Fatal(err)
	}
	row = readRow(t, dir, endpointless.ID)
	row["endpoints"] = []any{}
	writeRow(t, dir, endpointless.ID, row)

	for _, tc := range []struct {
		name    string
		channel store.Channel
		body    map[string]any
		want    string
	}{
		{"unconfirmed", keyed, map[string]any{"model": "a"}, "付费"},
		{"empty model", keyed, map[string]any{"model": "  ", "confirmed": true}, "模型"},
		{"no endpoint", endpointless, map[string]any{"model": "a", "confirmed": true}, "端点"},
		{"no saved key", keyless, map[string]any{"model": "a", "confirmed": true}, "密钥"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postStoredProbe(t, h, tc.channel.ID, tc.body)
			if w.Code != 400 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("message %s", w.Body)
			}
		})
	}
	if hits := up.recorded(); len(hits) != 0 {
		t.Fatalf("a refused probe still sent %d requests", len(hits))
	}
	// A channel that is not there is a 404, not a probe that fails upstream.
	w := postStoredProbe(t, h, 99999, map[string]any{"model": "a", "confirmed": true})
	if w.Code != 404 || !strings.Contains(w.Body.String(), "channel not found") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// The client refuses to follow a redirect, so a credential has no second hop at
// all: the probe reports the redirect and the cross-host target is never asked.
func TestStoredProbeDoesNotFollowRedirect(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	up := newProbeUpstream(t)
	up.mode = "redirect"
	other := newProbeUpstream(t)
	up.redirectTo = other.URL + "/elsewhere"
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, copilot.NewService(s, nil, copilot.DefaultEndpoints()), nil, nil)
	c := oauthChannel(t, s, store.CredentialCopilotOAuth, store.OAuthProviderCopilot, up.URL, store.FormatOpenAIChatCompletions,
		store.OAuthCredential{GitHubToken: "gho_test"})
	w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5", "confirmed": true})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if result.OK || result.UpstreamStatus != http.StatusFound {
		t.Fatalf("probe %s", w.Body)
	}
	if result.Diagnostic == nil || result.Diagnostic.Code != "redirect" {
		t.Fatalf("diagnostic %s", w.Body)
	}
	hits := up.recorded()
	if len(hits) != 1 {
		t.Fatalf("the first host saw %d requests", len(hits))
	}
	if got := hits[0].header.Get("Authorization"); got != "Bearer fresh-access-token" {
		t.Fatalf("credential %q", got)
	}
	if followed := other.recorded(); len(followed) != 0 {
		t.Fatalf("the credential followed the redirect: %d requests on the second host", len(followed))
	}
}

// Several named formats are answered per format, one entry each, and the unnamed
// call still costs one request.
func TestStoredProbeFormats(t *testing.T) {
	up := newProbeUpstream(t)
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, nil, nil, nil)
	c := store.Channel{Name: "two facades", Enabled: true, Models: []string{"a"}, APIKey: "saved-key",
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL + "/api"},
			{Format: store.FormatOpenAIResponses, BaseURL: up.URL + "/api"},
		}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	w := postStoredProbe(t, h, c.ID, map[string]any{"model": "a", "confirmed": true})
	result := probeResult(t, w)
	if !result.OK || len(result.Formats) != 0 || result.Models != nil {
		t.Fatalf("an unnamed probe must stay flat: %s", w.Body)
	}
	if paths := probePaths(up.recorded()); len(paths) != 1 || paths[0] != "/api/v1/chat/completions" {
		t.Fatalf("an unnamed probe sent %v", paths)
	}
	up.mu.Lock()
	up.hits = nil
	up.mu.Unlock()
	w = postStoredProbe(t, h, c.ID, map[string]any{"model": "a", "confirmed": true,
		"formats": []string{store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses}})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result = probeResult(t, w)
	if len(result.Formats) != 2 || !result.OK {
		t.Fatalf("per-format answer %s", w.Body)
	}
	for _, entry := range result.Formats {
		if !entry.OK || entry.Diagnostic != nil || entry.DurationMS < 0 {
			t.Fatalf("entry %+v", entry)
		}
	}
	if got := probePaths(up.recorded()); len(got) != 2 || got[0] != "/api/v1/chat/completions" || got[1] != "/api/v1/responses" {
		t.Fatalf("named formats went to %v", got)
	}
}

func TestStoredProbeCredentials(t *testing.T) {
	up := newProbeUpstream(t)
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, nil, nil, nil)
	c := store.Channel{
		Name:    "multi-cred",
		Enabled: true,
		Models:  []string{"a"},
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL + "/api"},
		},
		Credentials: []store.CredentialItem{
			{ID: "c1", Label: "Key One", APIKey: "key-1", Enabled: true},
			{ID: "c2", Label: "Key Two", APIKey: "key-2", Enabled: true},
		},
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	// Test both credentials specifically
	w := postStoredProbe(t, h, c.ID, map[string]any{
		"model":          "a",
		"confirmed":      true,
		"formats":        []string{store.FormatOpenAIChatCompletions},
		"credential_ids": []string{"c1", "c2"},
	})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if len(result.Formats) != 2 || !result.OK {
		t.Fatalf("multi-cred probe should return 2 entries, got %d: %s", len(result.Formats), w.Body)
	}
	if result.Formats[0].CredentialID != "c1" || result.Formats[0].CredentialLabel != "Key One" {
		t.Errorf("entry 0 cred mismatch: %+v", result.Formats[0])
	}
	if result.Formats[1].CredentialID != "c2" || result.Formats[1].CredentialLabel != "Key Two" {
		t.Errorf("entry 1 cred mismatch: %+v", result.Formats[1])
	}
	hits := up.recorded()
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].header.Get("Authorization") != "Bearer key-1" || hits[1].header.Get("Authorization") != "Bearer key-2" {
		t.Errorf("expected bearer key-1 and key-2, got %q and %q", hits[0].header.Get("Authorization"), hits[1].header.Get("Authorization"))
	}
}

func probePaths(hits []probeHit) []string {
	out := []string{}
	for _, hit := range hits {
		out = append(out, hit.path)
	}
	return out
}

// The editor's draft decides where a stored probe goes: the request is sent to
// the endpoints the editor shows, so a re-pointed endpoint is tested at its new
// address before anything is saved. The credential is still the stored row's —
// the draft's own credential type and api key are ignored, and a draft endpoint
// is checked by the same allowlist a stored one is.
func TestStoredProbeUsesDraftEndpoints(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	up := newProbeUpstream(t)
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, copilot.NewService(s, nil, copilot.DefaultEndpoints()), nil, nil)
	c := oauthChannel(t, s, store.CredentialCopilotOAuth, store.OAuthProviderCopilot, up.URL, store.FormatOpenAIChatCompletions,
		store.OAuthCredential{GitHubToken: "gho_test"})
	// The draft re-points the endpoint and claims an api_key credential of its
	// own: neither may move the destination's credential.
	draft := map[string]any{
		"credential_type": store.CredentialAPIKey,
		"api_key":         "draft-key",
		"base_url":        up.URL + "/draft",
		"endpoints":       []map[string]any{{"format": store.FormatOpenAIChatCompletions, "base_url": up.URL + "/draft"}},
	}
	w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5", "confirmed": true, "channel": draft})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if !result.OK || result.Diagnostic != nil {
		t.Fatalf("probe %s", w.Body)
	}
	hits := up.recorded()
	if len(hits) != 1 {
		t.Fatalf("probe made %d requests", len(hits))
	}
	if hits[0].path != "/draft/chat/completions" {
		t.Fatalf("path %q, want the draft endpoint's address", hits[0].path)
	}
	if got := hits[0].header.Get("Authorization"); got != "Bearer fresh-access-token" {
		t.Fatalf("credential %q, want the stored row's OAuth token", got)
	}
	// A probe that names no channel is the account card's, and it still tests
	// the stored row's own endpoint.
	w = postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5", "confirmed": true})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if result = probeResult(t, w); !result.OK {
		t.Fatalf("probe without a draft %s", w.Body)
	}
	if hits := up.recorded(); len(hits) != 2 || hits[1].path != "/chat/completions" {
		t.Fatalf("probe without a draft went to %v", probePaths(hits))
	}
}

// An address that is not there, or that names no scheme the HTTP client can use,
// is the draft probe's "无效地址。": the destination the probe would need does
// not exist. Nothing is sent, and the English transport error the client would
// produce does not travel.
func TestStoredProbeReportsInvalidAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		base string
	}{
		{"empty address", ""},
		{"address without a scheme", "api.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newProbeUpstream(t)
			dir, s := probeStore(t)
			h := storedProbeConsole(t, s, nil, nil, nil)
			c := store.Channel{Name: "keyed", Enabled: true, Models: []string{"a"}, APIKey: "saved-key",
				Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL + "/api"}}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			// A state SaveChannel refuses to write, reached the way the runtime
			// checks exist for: a row edited behind the console's back.
			row := readRow(t, dir, c.ID)
			endpoints, _ := row["endpoints"].([]any)
			endpoints[0].(map[string]any)["base_url"] = tc.base
			writeRow(t, dir, c.ID, row)
			w := postStoredProbe(t, h, c.ID, map[string]any{"model": "a", "confirmed": true})
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			result := probeResult(t, w)
			if result.OK || result.Diagnostic == nil || result.Diagnostic.Code != "url" || result.Diagnostic.Message != "无效地址。" {
				t.Fatalf("probe %s", w.Body)
			}
			if hits := up.recorded(); len(hits) != 0 {
				t.Fatalf("an invalid address still sent %d requests", len(hits))
			}
		})
	}
}

// A token endpoint's refusal travels its own words, and those words can quote
// the credential it refused — a token no shape pattern recognizes. The
// diagnostic masks it by exact substring, so a token the 401's body echoed never
// reaches the console's answer.
func TestStoredProbeMasksTokenEchoedByRefreshFailure(t *testing.T) {
	// The channel points at the local doubles, so the documented testing escape
	// hatch for the OAuth host allowlist is set explicitly.
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	up := newProbeUpstream(t)
	var exchanges atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token rt_leaked_secret was already used"}`)
	}))
	t.Cleanup(issuer.Close)
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, nil, siwc.NewService(s, nil, siwc.Endpoints{Token: issuer.URL}), nil)
	c := oauthChannel(t, s, store.CredentialChatGPTOAuth, store.OAuthProviderChatGPT, up.URL+"/v1", store.FormatOpenAIResponses,
		store.OAuthCredential{RefreshToken: "rt_leaked_secret", ClientID: "oaiapp_test", HostID: "urn:uuid:host",
			AccessToken: "expired-bearer", ExpiresAt: time.Now().Add(-time.Minute).UTC()})
	w := postStoredProbe(t, h, c.ID, map[string]any{"model": "gpt-5", "confirmed": true})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if result.OK || result.Diagnostic == nil || result.Diagnostic.Source != "gateway" || result.Diagnostic.Code != "auth" {
		t.Fatalf("probe %s", w.Body)
	}
	// The reason is kept — the owner needs to know the credential was refused —
	// with the credential itself masked.
	if !strings.Contains(result.Diagnostic.Message, "无法解析或刷新") {
		t.Fatalf("reason lost: %s", w.Body)
	}
	if strings.Contains(result.Diagnostic.Message, "rt_leaked_secret") || strings.Contains(w.Body.String(), "rt_leaked_secret") {
		t.Fatalf("the token the refusal echoed leaked: %s", w.Body)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("exchanges %d", exchanges.Load())
	}
	if hits := up.recorded(); len(hits) != 0 {
		t.Fatalf("a failed credential still sent %d requests", len(hits))
	}
}

// The draft probe reaches the Responses endpoint where that facade lives:
// {base}/v1/responses, with the Responses shape, and it reads the answer the
// Responses reader expects.
func TestDraftProbeResponsesDestination(t *testing.T) {
	up := newProbeUpstream(t)
	_, s := probeStore(t)
	h := storedProbeConsole(t, s, nil, nil, nil)
	body, err := json.Marshal(map[string]any{
		"channel": map[string]any{"name": "responses draft", "api_key": "draft-key", "enabled": true,
			"models":    []string{"a"},
			"endpoints": []map[string]any{{"format": store.FormatOpenAIResponses, "base_url": up.URL + "/api"}}},
		"model": "a", "confirmed": true, "formats": []string{store.FormatOpenAIResponses},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/channels/probe", bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	result := probeResult(t, w)
	if !result.OK || result.Diagnostic != nil {
		t.Fatalf("probe %s", w.Body)
	}
	hits := up.recorded()
	if len(hits) != 1 {
		t.Fatalf("probe made %d requests", len(hits))
	}
	if hits[0].path != "/api/v1/responses" {
		t.Fatalf("path %q, want the responses facade under /v1", hits[0].path)
	}
	if got := hits[0].body["model"]; got != "a" {
		t.Fatalf("model %v", got)
	}
	input, ok := hits[0].body["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input %v", hits[0].body["input"])
	}
	if item, ok := input[0].(map[string]any); !ok || item["role"] != "user" || item["type"] != "message" {
		t.Fatalf("input item %v", input[0])
	}
	if got := hits[0].body["store"]; got != false {
		t.Fatalf("store %v", got)
	}
	if got := hits[0].body["stream"]; got != true {
		t.Fatalf("stream %v", got)
	}
	if got := hits[0].header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("accept %q", got)
	}
}
