package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/compatprofile"
	"torouter/internal/plugins"
	"torouter/internal/quota"
	"torouter/internal/store"
)

func TestCompatibleMissingRuntimeIsLocalAvailabilityWithRecovery(t *testing.T) {
	g, st := setup(t)
	state := `{"type":"oauth","uid":"synthetic","access":"synthetic-access","refresh":"synthetic-refresh","expires":1900000000000}`
	o, err := store.CompatibleOAuthCredential(compatprofile.QoderCredential, state)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := st.CommitPluginConsent(compatprofile.QoderCredential, o, store.PluginConsentTarget{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Models, c.AvailableModels = []string{"fixture-model"}, []string{"fixture-model"}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "fixture-model", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.SetPluginCapability("qoder", "connect", true); err != nil {
		t.Fatal(err)
	}
	g.Plugins, err = plugins.New(st)
	if err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "connection_plugin_unavailable") || !strings.Contains(w.Body.String(), "#/plugins") {
		t.Fatalf("missing runtime was not actionable: %d %s", w.Code, w.Body.String())
	}
	if g.Plugins.Existing(compatprofile.QoderCredential) != nil {
		t.Fatal("missing runtime constructed service")
	}
}

func setConnectionPlugin(t *testing.T, m *plugins.Manager, id string, enabled bool) {
	t.Helper()
	list, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetCapability(context.Background(), id, "connect", enabled, list.Version); err != nil {
		t.Fatal(err)
	}
}

func TestPluginDisableRemovesRoutingAndClientDiscoveryWithoutChangingScope(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	g, st := setup(t)
	var hits atomic.Int32
	up := mockCopilotChat(t, &hits, nil)
	c := addCopilotChannel(t, st, up.URL, store.FormatOpenAIChatCompletions, "synthetic-github-token")
	before, _ := st.Channel(c.ID)
	if g.Plugins.Existing(store.CredentialCopilotOAuth) != nil {
		t.Fatal("disabled provider was instantiated")
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "connection_plugin_disabled") || !strings.Contains(w.Body.String(), "#/plugins") {
		t.Fatalf("disabled plugin must be availability with guidance, got %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/v1/models", "/v1/models/specs"} {
		w = httptest.NewRecorder()
		serveTestClient(g, w, httptest.NewRequest(http.MethodGet, path, nil))
		var list struct {
			Data []json.RawMessage `json:"data"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Data) != 0 {
			t.Fatalf("disabled discovery %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	after, _ := st.Channel(c.ID)
	if store.EntityVersion(before) != store.EntityVersion(after) || hits.Load() != 0 || g.Plugins.Existing(store.CredentialCopilotOAuth) != nil {
		t.Fatal("disabled route changed data, created a runtime, or sent traffic")
	}
	// Disabling optional OAuth must not hide or break an ordinary API Key path.
	add(t, st, up.URL, "openai", 1)
	w = invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 || hits.Load() != 1 {
		t.Fatalf("API Key path affected: %d %s", w.Code, w.Body.String())
	}
}

func TestPluginToggleDuringRefreshSavesTokenButDoesNotSendInference(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-issued-bearer", "expires_at": time.Now().Add(time.Hour).Unix()})
	}))
	t.Cleanup(issuer.Close)
	var hits atomic.Int32
	up := mockCopilotChat(t, &hits, nil)
	c := addCopilotChannel(t, st, up.URL, store.FormatOpenAIChatCompletions, "synthetic-github-token")
	m, err := plugins.NewInjected(st, copilotService(t, st, issuer.URL), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := NewWithPlugins(st, m)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	setConnectionPlugin(t, m, "copilot", false)
	setConnectionPlugin(t, m, "copilot", true)
	close(release)
	select {
	case w := <-result:
		if w.Code != 503 || !strings.Contains(w.Body.String(), "connection_plugin_disabled") {
			t.Fatalf("old epoch request: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not finish")
	}
	saved, err := st.Channel(c.ID)
	if err != nil || saved.OAuth.AccessToken != "synthetic-issued-bearer" || hits.Load() != 0 {
		t.Fatal("issued token lost or obsolete request sent")
	}
	for _, fact := range g.Breaker.Snapshot() {
		if fact.Failures != 0 || fact.LastRequest != nil {
			t.Fatalf("local refusal became upstream health: %+v", fact)
		}
	}
}

func TestPluginDisableKeepsAlreadySentInferenceResponse(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"finished"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	issuer, _ := mockExchange(t, http.StatusOK)
	addCopilotChannel(t, st, up.URL, store.FormatOpenAIChatCompletions, "synthetic-github-token")
	m, err := plugins.NewInjected(st, copilotService(t, st, issuer.URL), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := NewWithPlugins(st, m)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inference did not start")
	}
	setConnectionPlugin(t, m, "copilot", false)
	close(release)
	select {
	case w := <-result:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "finished") || hits.Load() != 1 {
			t.Fatalf("sent result discarded/replayed: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request was not drained")
	}
}

func TestPluginToggleInvalidatesQuotaEligibilityAndRejectsLateFact(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	g, st := setup(t)
	c := addCopilotChannel(t, st, "http://127.0.0.1:1", store.FormatOpenAIChatCompletions, "synthetic-github-token")
	setConnectionPlugin(t, g.Plugins, "copilot", true)
	c, _ = st.Channel(c.ID)
	cred := c.Credentials[0]
	// Windows can return the same clock tick for the toggle and the synthetic
	// observation. Model a genuinely later completed query, without weakening
	// the production rule that facts at/before invalidation are rejected.
	g.runtimeMu.Lock()
	reset := g.runtimeLocked().resets[quotaKey{c.ID, c.Generation, cred.ID}]
	g.runtimeMu.Unlock()
	deadline := time.Now().Add(time.Second)
	for !time.Now().After(reset) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	available := false
	o := quota.Observation{ChannelID: c.ID, Generation: c.Generation, CredentialID: cred.ID, Available: &available, CheckedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	g.ObserveQuota(c, cred, o)
	if !quotaUnavailable(g.quotaObservation(c, cred)) {
		t.Fatal("fixture quota not recorded")
	}
	setConnectionPlugin(t, g.Plugins, "copilot", false)
	setConnectionPlugin(t, g.Plugins, "copilot", true)
	g.ObserveQuota(c, cred, o)
	if g.quotaObservation(c, cred).Available != nil {
		t.Fatal("old quota revived across plugin epoch")
	}
}
