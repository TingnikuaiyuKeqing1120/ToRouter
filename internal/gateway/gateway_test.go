package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/auth"
	"torouter/internal/store"
)

func setup(t *testing.T) (*Gateway, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	settings, _ := s.GetSettings()
	settings.FailureCooldownSeconds = 1
	settings.RequestDefaultsEnabled = true
	settings.RequestDefaults = store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	return New(s), s
}

// formatFor maps the short protocol name used by tests onto the endpoint format
// it declares.
func formatFor(proto string) string {
	switch proto {
	case "openai":
		return store.FormatOpenAIChatCompletions
	case "responses":
		return store.FormatOpenAIResponses
	case "anthropic":
		return store.FormatAnthropicMessages
	case "gemini":
		return store.FormatGeminiGenerateContent
	}
	return ""
}

func add(t *testing.T, s *store.Store, url, proto string, priority int) {
	t.Helper()
	c := store.Channel{Name: "test", APIKey: "upstream-secret", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: formatFor(proto), BaseURL: url}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := store.DownstreamModel{ID: "test", Enabled: true, Fallback: true}
	rows, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range rows {
		if existing.ID == m.ID {
			m = existing
			break
		}
	}
	m.Bindings = append(m.Bindings, store.Binding{ChannelID: c.ID, Model: "test", Priority: priority, Enabled: true})
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
}
func invoke(g *Gateway, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	serveTestClient(g, w, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return w
}
func TestNativeFailoverPreservesBodyAndCredentials(t *testing.T) {
	g, s := setup(t)
	// One attempt per channel: this test is about failover, not retries.
	if err := s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: 1, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}}); err != nil {
		t.Fatal(err)
	}
	var bad, good atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bad.Add(1); w.WriteHeader(429) }))
	defer first.Close()
	body := `{"model":"test","messages":[{"role":"user","content":"hello"}],"custom_extension":true}`
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("wrong auth")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != body {
			t.Errorf("body modified %s", b)
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer second.Close()
	add(t, s, first.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	w := invoke(g, "/v1/chat/completions", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if bad.Load() != 1 || good.Load() != 1 {
		t.Fatal("failover not performed")
	}
}
func TestStreamTransparentAndNoRetryAfterCommit(t *testing.T) {
	g, s := setup(t)
	var backup atomic.Int32
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	defer upstream.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backup.Add(1) }))
	defer second.Close()
	add(t, s, upstream.URL, "openai", 0)
	add(t, s, second.URL, "openai", 1)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","stream":true,"messages":[]}`)
	if backup.Load() != 0 {
		t.Fatal("must not replay committed stream")
	}
	if !strings.HasPrefix(w.Body.String(), stream) || !strings.Contains(w.Body.String(), "interrupted") {
		t.Fatalf("body %s", w.Body.String())
	}
}
func TestObserverCumulativeUsageAndRawBytes(t *testing.T) {
	data := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\r\n\r\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	u := store.Usage{}
	o := newObserver(strings.NewReader(data), "anthropic", &u)
	got, err := io.ReadAll(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != data {
		t.Fatal("wire bytes changed")
	}
	if !o.complete || !o.hasUsage || u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Fatalf("usage %+v complete %v", u, o.complete)
	}
}
func TestNativeAnthropicAndGemini(t *testing.T) {
	for _, proto := range []string{"anthropic", "gemini"} {
		t.Run(proto, func(t *testing.T) {
			g, s := setup(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if proto == "anthropic" {
					if r.Header.Get("x-api-key") != "upstream-secret" || r.Header.Get("anthropic-version") == "" || r.Header.Get("Authorization") != "Bearer upstream-secret" {
						t.Error("anthropic headers")
					}
					io.WriteString(w, `{"id":"m","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
				} else {
					if r.URL.Path != "/v1beta/models/test:generateContent" || r.Header.Get("x-goog-api-key") != "upstream-secret" {
						t.Error("gemini request")
					}
					io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`)
				}
			}))
			defer up.Close()
			add(t, s, up.URL, proto, 0)
			path := "/v1/messages"
			body := `{"model":"test","messages":[],"max_tokens":10}`
			if proto == "gemini" {
				path = "/v1beta/models/test:generateContent"
				body = `{"contents":[]}`
			}
			w := invoke(g, path, body)
			if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestNoRetryOnBadRequest(t *testing.T) {
	g, s := setup(t)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(400) }))
	defer up.Close()
	add(t, s, up.URL, "openai", 0)
	add(t, s, up.URL, "openai", 1)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	if w.Code != 400 || hits.Load() != 1 {
		t.Fatalf("status %d hits %d", w.Code, hits.Load())
	}
}
func TestOnlyEnabledDownstreamModelsServeAndList(t *testing.T) {
	g, s := setup(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "prov", APIKey: "k", Models: []string{"real"}, AvailableModels: []string{"off"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "hidden", Enabled: false, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	// Unbound channel models are not routable, and disabled downstream models 503.
	w := invoke(g, "/v1/chat/completions", `{"model":"real","messages":[]}`)
	if w.Code != 503 {
		t.Fatalf("unbound upstream model should not be routable: %d %s", w.Code, w.Body.String())
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "public", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if w = invoke(g, "/v1/chat/completions", `{"model":"public","messages":[]}`); w.Code != 200 {
		t.Fatalf("enabled downstream model should route: %d %s", w.Code, w.Body.String())
	}
	lw := httptest.NewRecorder()
	serveTestClient(g, lw, httptest.NewRequest("GET", "/v1/models", nil))
	var listing struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Data) != 1 || listing.Data[0].ID != "public" {
		t.Fatalf("/v1/models must expose only enabled downstream models: %s", lw.Body.String())
	}
}

func defaultBudget(v int64) *int64 { return &v }

// Existing gateway tests exercise routing behind the authentication boundary.
func serveTestClient(g *Gateway, w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.ClientFromContextOK(r.Context()); !ok {
		r = r.WithContext(auth.WithClientContext(r.Context(), store.ClientKey{Scope: store.DefaultClientKeyScope()}))
	}
	g.ServeHTTP(w, r)
}
