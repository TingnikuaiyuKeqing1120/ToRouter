package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/auth"
	"torouter/internal/store"
)

type codexProfileCapture struct {
	header http.Header
	body   []byte
}

func codexProfileGateway(t *testing.T, handler http.HandlerFunc) (*Gateway, *store.Store) {
	t.Helper()
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcStub(t, s), codexService(t, s, backend.URL))
	siwcSettings(t, s)
	c := addCodexChannel(t, s, backend.URL+"/backend-api/codex")
	c.OAuth.AccessToken = "synthetic-access"
	c.OAuth.ExpiresAt = time.Now().Add(time.Hour)
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return g, s
}

func codexProfileTurn(t *testing.T, g *Gateway, clientID int64, body string, headers http.Header) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header = headers.Clone()
	r = r.WithContext(auth.WithClientContext(r.Context(), store.ClientKey{ID: clientID, Scope: store.DefaultClientKeyScope()}))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("gateway = %d %s", w.Code, w.Body.String())
	}
}

func TestCodexProfileSessionLifecycleAndIsolation(t *testing.T) {
	seen := make(chan codexProfileCapture, 12)
	var rejectNext atomic.Bool
	g, s := codexProfileGateway(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- codexProfileCapture{r.Header.Clone(), body}
		w.Header().Set("Content-Type", "application/json")
		if rejectNext.Swap(false) {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
			return
		}
		io.WriteString(w, `{"id":"resp_profile","object":"response","status":"completed","output":[]}`)
	})
	settings, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.RetriesPerChannel = 2
	settings.FailureCooldownSeconds = 1
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Session-Id": {"native-session"}, "X-Session-Id": {"secondary-session"}}
	first := `{"model":"test","prompt_cache_key":"explicit-cache","input":"one"}`
	codexProfileTurn(t, g, 11, first, headers)
	initial := <-seen
	session := initial.header.Get("session-id")
	if session == "" || session == "native-session" {
		t.Fatalf("session must be present and caller-scoped, got %q", session)
	}
	// An explicit native source wins over generic headers and a cache key.
	headers.Set("X-Session-Id", "changed-secondary")
	second := `{"model":"test","prompt_cache_key":"different-explicit-cache","previous_response_id":"resp_profile","input":[{"type":"function_call_output","call_id":"call_1","output":"answer"},{"type":"reasoning","encrypted_content":"opaque-reasoning"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
	codexProfileTurn(t, g, 11, second, headers)
	next := <-seen
	if next.header.Get("session-id") != session {
		t.Fatal("continued turn changed its session")
	}
	// Every field outside the existing model mapping survives the real wire.
	var want, got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(second), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(next.body, &got); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"prompt_cache_key", "previous_response_id", "input", "tools"} {
		var a, b bytes.Buffer
		json.Compact(&a, want[name])
		json.Compact(&b, got[name])
		if !bytes.Equal(a.Bytes(), b.Bytes()) {
			t.Fatalf("%s changed: %s -> %s", name, a.Bytes(), b.Bytes())
		}
	}
	for _, name := range []string{"session_id", "conversation_id", "thread-id", "x-codex-window-id", "x-codex-turn-state"} {
		if next.header.Get(name) != "" {
			t.Fatalf("invented %s", name)
		}
	}
	rejectNext.Store(true)
	codexProfileTurn(t, g, 11, first, headers)
	for i := 0; i < 2; i++ {
		if retry := <-seen; retry.header.Get("session-id") != session {
			t.Fatal("retry changed its session")
		}
	}
	codexProfileTurn(t, g, 22, first, headers)
	if other := <-seen; other.header.Get("session-id") == session {
		t.Fatal("different client key shared the session")
	}
	headers.Set("Session-Id", "other-native-session")
	codexProfileTurn(t, g, 11, first, headers)
	if other := <-seen; other.header.Get("session-id") == session {
		t.Fatal("different conversation shared the session")
	}
	// A new Gateway has no learned affinity entries; the source still maps to
	// the same ID. No process salt or in-memory identity registry is involved.
	restarted := NewWithOAuth(s, copilotTestService(t, s), siwcStub(t, s), codexService(t, s, "http://127.0.0.1"))
	headers.Set("Session-Id", "native-session")
	codexProfileTurn(t, restarted, 11, first, headers)
	if again := <-seen; again.header.Get("session-id") != session {
		t.Fatal("gateway recreation changed the session")
	}
}

func TestCodexProfileSessionSourcesAndNoSource(t *testing.T) {
	seen := make(chan codexProfileCapture, 4)
	g, _ := codexProfileGateway(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- codexProfileCapture{r.Header.Clone(), body}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_profile","object":"response","status":"completed","output":[]}`)
	})
	for _, tc := range []struct {
		name, body string
		headers    http.Header
		present    bool
	}{
		{"native", `{"model":"test","input":"one"}`, http.Header{"Session-Id": {"source"}}, true},
		{"generic", `{"model":"test","input":"one"}`, http.Header{"X-Session-Id": {"source"}}, true},
		{"compatibility", `{"model":"test","input":"one"}`, http.Header{"Session_id": {"source"}}, true},
		{"cache", `{"model":"test","prompt_cache_key":"explicit-cache","input":"one"}`, http.Header{}, true},
		{"metadata", `{"model":"test","metadata":{"session_id":"source"},"input":"one"}`, http.Header{}, true},
		{"no source", `{"model":"test","input":"identical history"}`, http.Header{}, false},
		{"cursor is not a session", `{"model":"test","previous_response_id":"resp_profile","input":"identical history"}`, http.Header{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var session string
			for i := 0; i < 2; i++ {
				codexProfileTurn(t, g, 11, tc.body, tc.headers)
				capture := <-seen
				got := capture.header.Get("session-id")
				if (got != "") != tc.present {
					t.Fatalf("session-id = %q, present want %v", got, tc.present)
				}
				if i == 0 {
					session = got
				} else if got != session {
					t.Fatal("identical source changed its session")
				}
				if tc.name == "cache" && !bytes.Contains(capture.body, []byte(`"prompt_cache_key":"explicit-cache"`)) {
					t.Fatal("explicit cache key was overwritten")
				}
				if !tc.present && bytes.Contains(capture.body, []byte("prompt_cache_key")) {
					t.Fatal("invented a cache key without a source")
				}
			}
		})
	}
}

func TestCodexProfileSessionNamespace(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("X-Session-Id", "same-source")
	in := requestInfo{model: "public-model", format: store.FormatOpenAIResponses, responses: true}
	if got := codexSessionID(r, in); got != "" {
		t.Fatal("unauthenticated request acquired a caller session")
	}
	r = r.WithContext(auth.WithClientContext(r.Context(), store.ClientKey{ID: 11}))
	want := codexSessionID(r, in)
	if want == "" {
		t.Fatal("authenticated source has no session")
	}
	otherModel := in
	otherModel.model = "other-public-model"
	otherFormat := in
	otherFormat.format, otherFormat.responses = store.FormatOpenAIChatCompletions, false
	for _, other := range []requestInfo{otherModel, otherFormat} {
		if codexSessionID(r, other) == want {
			t.Fatal("different affinity namespace shared a session")
		}
	}
}
