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
	"torouter/internal/copilot"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

// copilotTestService is an unused copilot service: these tests only drive the
// SIWC track, and the gateway needs both handles to exist.
func copilotTestService(t *testing.T, s *store.Store) *copilot.Service {
	t.Helper()
	return copilot.NewService(s, nil, copilot.DefaultEndpoints())
}

// mockOpenAI is the inference half of the SIWC profile: the one Responses
// endpoint, answering with the SSE stream the profile mandates.
func mockOpenAI(t *testing.T, hits *atomic.Int32, seen *http.Request) (*httptest.Server, *[]byte) {
	t.Helper()
	var sentBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		// The body is read here, while it is still open: a cloned request's
		// Body is not readable after the handler returns.
		sentBody, _ = io.ReadAll(r.Body)
		if seen != nil {
			*seen = *r.Clone(r.Context())
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, &sentBody
}

// mockAuth is the token endpoint: it serves the rotation the gateway performs
// when the stored access token is inside the refresh skew.
func mockAuth(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/oauth/token" {
			w.WriteHeader(404)
			return
		}
		calls.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type %q", got)
		}
		json.NewEncoder(w).Encode(siwc.TokenResponse{
			AccessToken: "siwc-fresh", RefreshToken: "rt-fresh", IDToken: "id-fresh",
			ExpiresIn: 3600, Scope: siwc.Scope, ClientID: "oaiapp_gateway",
		})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func siwcService(t *testing.T, s *store.Store, auth string) *siwc.Service {
	t.Helper()
	eps := siwc.DefaultEndpoints()
	eps.Token = auth + "/api/accounts/oauth/token"
	return siwc.NewService(s, &http.Client{Timeout: 5 * time.Second}, eps)
}

// addChatGPTChannel binds a downstream model to a chatgpt_oauth channel whose
// stored access token has just expired, so the first request must rotate.
func addChatGPTChannel(t *testing.T, s *store.Store, base string) store.Channel {
	t.Helper()
	c := store.Channel{Name: "chatgpt", CredentialType: store.CredentialChatGPTOAuth, Enabled: true,
		Models:    []string{"test"},
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: base}},
		OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderChatGPT, RefreshToken: "rt-stored", IDToken: "id-stored",
			ClientID: "oaiapp_gateway", HostID: "urn:uuid:11111111-1111-4111-8111-111111111111", Subject: "sub-gateway",
			Scopes: siwc.Scope, AccessToken: "expired-access", ExpiresAt: time.Now().Add(-time.Minute).UTC(),
			AccountLabel: "person@example.test", APIBase: "https://api.openai.com/v1", Status: store.OAuthStatusActive,
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

func siwcSettings(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: 1, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}}); err != nil {
		t.Fatal(err)
	}
}

// The profile is enforced end to end: the credential is rotated, the request is
// forced to stream with store off, the omit list is stripped and recorded, and a
// non-streaming client gets the completed Response object back.
func TestChatGPTChannelEnforcesProfile(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	var seen http.Request
	openai, sentBody := mockOpenAI(t, &hits, &seen)
	auth, rotations := mockAuth(t, http.StatusOK)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
	siwcSettings(t, s)
	c := addChatGPTChannel(t, s, openai.URL)
	body := `{"model":"test","instructions":"be brief","temperature":0.2,"top_p":0.9,"user":"client-1","metadata":{"k":"v"},"store":true,"stream":false,"input":[{"type":"message","role":"user","content":"hi"}]}`
	w := invoke(g, "/v1/responses", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 || rotations.Load() != 1 {
		t.Fatalf("upstream %d rotations %d", hits.Load(), rotations.Load())
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer siwc-fresh" {
		t.Fatalf("authorization %q", got)
	}
	if seen.URL.Path != "/v1/responses" {
		t.Fatalf("path %s", seen.URL.Path)
	}
	if got := seen.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("accept %q: the profile streams whatever the client asked", got)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(*sentBody, &sent); err != nil {
		t.Fatal(err)
	}
	if string(sent["stream"]) != "true" || string(sent["store"]) != "false" {
		t.Fatalf("profile not forced: stream=%s store=%s", sent["stream"], sent["store"])
	}
	for _, param := range []string{"temperature", "top_p", "user", "metadata"} {
		if _, ok := sent[param]; ok {
			t.Fatalf("omitted parameter %s reached the upstream", param)
		}
	}
	if string(sent["instructions"]) != `"be brief"` {
		t.Fatalf("instructions lost: %s", sent["instructions"])
	}
	// The client asked for JSON and got the completed Response object.
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("response is not JSON: %s", w.Body.String())
	}
	if string(answer["id"]) != `"resp_1"` || string(answer["object"]) != `"response"` {
		t.Fatalf("assembled response %s", w.Body.String())
	}
	if usage, ok := answer["usage"]; !ok || !strings.Contains(string(usage), `"input_tokens":5`) {
		t.Fatalf("usage not carried into the answer: %s", answer["usage"])
	}
	// The omit strip is recorded as a degradation on the usage row.
	logs, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(logs) != 1 {
		t.Fatal("usage not recorded", err)
	}
	for _, want := range []string{"siwc_omit:temperature", "siwc_omit:top_p", "siwc_omit:user", "siwc_omit:metadata"} {
		if !strings.Contains(logs[0].Degraded, want) {
			t.Fatalf("degradation %s missing: %q", want, logs[0].Degraded)
		}
	}
	if logs[0].Status != 200 || logs[0].InputTokens != 5 || logs[0].OutputTokens != 2 || logs[0].Estimated {
		t.Fatalf("usage row %+v", logs[0])
	}
	_ = c
}

// A streaming client sees the SSE stream as it is: the profile's forced stream
// is the same wire, so nothing is buffered and nothing is rewritten.
func TestChatGPTStreamingPassesThrough(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	openai, _ := mockOpenAI(t, nil, nil)
	auth, _ := mockAuth(t, http.StatusOK)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
	siwcSettings(t, s)
	addChatGPTChannel(t, s, openai.URL)
	body := `{"model":"test","stream":true,"input":[{"type":"message","role":"user","content":"hi"}]}`
	w := invoke(g, "/v1/responses", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("stream not passed through: %s", w.Body.String())
	}
}

// Server-side state on a profile that keeps none is refused, not dropped: the
// client is told the endpoint has no state and what to do instead, and the
// upstream is never contacted.
func TestChatGPTRefusesServerState(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, param := range []string{"previous_response_id", "conversation"} {
		s, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var hits atomic.Int32
		openai, _ := mockOpenAI(t, &hits, nil)
		auth, rotations := mockAuth(t, http.StatusOK)
		g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
		siwcSettings(t, s)
		addChatGPTChannel(t, s, openai.URL)
		body := `{"model":"test","stream":false,` + `"` + param + `":"resp_previous",` + `"input":[{"type":"message","role":"user","content":"hi"}]}`
		w := invoke(g, "/v1/responses", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", param, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "no server-side state") && !strings.Contains(w.Body.String(), "server-side conversation state") {
			t.Fatalf("%s: refusal does not explain the profile: %s", param, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "siwc_profile_stateless") {
			t.Fatalf("%s: no recovery code: %s", param, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "start_fresh_session") {
			t.Fatalf("%s: no recovery actions: %s", param, w.Body.String())
		}
		if hits.Load() != 0 || rotations.Load() != 0 {
			t.Fatalf("%s: upstream contacted (%d hits, %d rotations)", param, hits.Load(), rotations.Load())
		}
		s.Close()
	}
}

// The runtime half of the allowlist: a row edited straight in the database
// cannot aim an SIWC credential at an impostor either. No request — and no
// rotation — goes there.
func TestChatGPTCredentialNeverSentToDirectlyEditedHost(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	impostor, _ := mockOpenAI(t, &hits, nil)
	auth, rotations := mockAuth(t, http.StatusOK)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
	siwcSettings(t, s)
	c := addChatGPTChannel(t, s, "https://api.openai.com/v1")
	rewriteChannelRow(t, dir, c.ID, map[string]any{
		"endpoints": []map[string]string{{"format": store.FormatOpenAIResponses, "base_url": impostor.URL}},
	})
	w := invoke(g, "/v1/responses", `{"model":"test","stream":true,"input":[]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "allowlist") {
		t.Fatalf("error does not name the allowlist: %s", w.Body.String())
	}
	if hits.Load() != 0 || rotations.Load() != 0 {
		t.Fatalf("credential path received work: %d hits, %d rotations", hits.Load(), rotations.Load())
	}
}

// A rotation that fails is a request-level failure with the reason spelled out;
// the upstream is not contacted and the stored credential is left alone.
func TestChatGPTFailedRotationFailsRequest(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	openai, _ := mockOpenAI(t, &hits, nil)
	auth, _ := mockAuth(t, http.StatusInternalServerError)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
	siwcSettings(t, s)
	c := addChatGPTChannel(t, s, openai.URL)
	w := invoke(g, "/v1/responses", `{"model":"test","stream":true,"input":[]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_grant") && !strings.Contains(w.Body.String(), "HTTP 500") {
		t.Fatalf("error does not name the failure: %s", w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("upstream contacted with a credential that could not be rotated")
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusActive || stored.OAuth.AccessToken != "expired-access" {
		t.Fatalf("failed rotation damaged the credential: %+v", stored.OAuth)
	}
}

// A row edited directly in the database could declare a non-Responses endpoint
// on a ChatGPT channel; the runtime re-check refuses it before anything is sent.
func TestChatGPTRefusesNonResponsesEndpoint(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	openai, _ := mockOpenAI(t, &hits, nil)
	auth, _ := mockAuth(t, http.StatusOK)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
	siwcSettings(t, s)
	c := addChatGPTChannel(t, s, openai.URL)
	rewriteChannelRow(t, dir, c.ID, map[string]any{
		"endpoints": []map[string]string{{"format": store.FormatOpenAIChatCompletions, "base_url": openai.URL}},
	})
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "responses endpoint") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("upstream contacted on a facade the profile does not serve")
	}
}

// P2-3: the runtime re-check covers the scheme too. A row edited directly in
// the database can move an endpoint onto cleartext http; the credential must not
// travel over it, and the refusal must say why.
func TestChatGPTRefusesCleartextEndpoint(t *testing.T) {
	// The escape hatch is NOT set: this is the rule, not the diagnostic path.
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	impostor, _ := mockOpenAI(t, &hits, nil)
	auth, rotations := mockAuth(t, http.StatusOK)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
	siwcSettings(t, s)
	c := addChatGPTChannel(t, s, "https://api.openai.com/v1")
	rewriteChannelRow(t, dir, c.ID, map[string]any{
		"endpoints": []map[string]string{{"format": store.FormatOpenAIResponses, "base_url": strings.Replace(impostor.URL, "http://", "http://", 1)}},
	})
	w := invoke(g, "/v1/responses", `{"model":"test","stream":true,"input":[]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "https") && !strings.Contains(w.Body.String(), "allowlist") {
		t.Fatalf("refusal does not name the rule: %s", w.Body.String())
	}
	if hits.Load() != 0 || rotations.Load() != 0 {
		t.Fatalf("credential path received work: %d hits, %d rotations", hits.Load(), rotations.Load())
	}
}

// P1-4: a system message in the input is rewritten to a developer message —
// the profile rejects the system item, and developer is the Responses API's own
// name for model instructions, so the text is carried across unchanged. The
// rewrite is recorded as a degradation, and items elsewhere in the array keep
// their positions.
func TestChatGPTAdaptsSystemInputToDeveloper(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var hits atomic.Int32
	openai, sentBody := mockOpenAI(t, &hits, nil)
	auth, _ := mockAuth(t, http.StatusOK)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
	siwcSettings(t, s)
	addChatGPTChannel(t, s, openai.URL)
	body := `{"model":"test","stream":false,"input":[{"type":"message","role":"system","content":"be terse"},{"type":"message","role":"user","content":"hi"}]}`
	w := invoke(g, "/v1/responses", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(*sentBody, &sent); err != nil {
		t.Fatal(err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(sent["input"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("input length changed: %d", len(items))
	}
	if string(items[0]["role"]) != `"developer"` {
		t.Fatalf("first item role %s, want developer", items[0]["role"])
	}
	if !strings.Contains(string(items[0]["content"]), "be terse") {
		t.Fatalf("system text lost: %s", items[0]["content"])
	}
	if string(items[1]["role"]) != `"user"` {
		t.Fatalf("the second item moved: %s", items[1]["role"])
	}
	logs, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(logs) != 1 {
		t.Fatal("usage not recorded", err)
	}
	if !strings.Contains(logs[0].Degraded, "siwc_adapt:system_to_developer") {
		t.Fatalf("adaptation not recorded: %q", logs[0].Degraded)
	}
}

// A non-system input passes through untouched: nothing is rewritten, nothing is
// recorded.
func TestChatGPTLeavesNonSystemInputAlone(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	openai, sentBody := mockOpenAI(t, nil, nil)
	auth, _ := mockAuth(t, http.StatusOK)
	g := NewWithOAuth(s, copilotTestService(t, s), siwcService(t, s, auth.URL), codex.NewService(s, nil, codex.DefaultEndpoints()))
	siwcSettings(t, s)
	addChatGPTChannel(t, s, openai.URL)
	body := `{"model":"test","stream":false,"instructions":"be terse","input":[{"type":"message","role":"developer","content":"already fine"},{"type":"message","role":"user","content":"hi"}]}`
	w := invoke(g, "/v1/responses", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(*sentBody), `"developer"`) || strings.Contains(string(*sentBody), `"system"`) {
		t.Fatalf("input rewritten: %s", *sentBody)
	}
	logs, _ := s.ListUsage(store.UsageFilter{})
	if len(logs) != 1 || strings.Contains(logs[0].Degraded, "siwc_adapt") {
		t.Fatalf("an adaptation was recorded: %q", logs[0].Degraded)
	}
}
