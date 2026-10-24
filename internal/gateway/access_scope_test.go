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

func scopedTurn(g *Gateway, token, method, path, body, session string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("x-session-id", session)
	w := httptest.NewRecorder()
	auth.New(g.Store).RequireClient(g).ServeHTTP(w, r)
	return w
}

func scopeKey(t *testing.T, s *store.Store, scope store.ClientKeyScope) (store.ClientKey, string) {
	t.Helper()
	key, token, err := s.CreateClientKeyWithScope("scoped", scope)
	if err != nil {
		t.Fatal(err)
	}
	return key, token
}

func TestScopeFourFormatsAndNotSent(t *testing.T) {
	for _, tc := range []struct{ format, path, body, response string }{
		{store.FormatOpenAIChatCompletions, "/v1/chat/completions", `{"model":"test","messages":[]}`, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`},
		{store.FormatOpenAIResponses, "/v1/responses", `{"model":"test","input":"hi"}`, `{"id":"resp","object":"response","status":"completed","output":[]}`},
		{store.FormatAnthropicMessages, "/v1/messages", `{"model":"test","messages":[],"max_tokens":10}`, `{"id":"m","type":"message","content":[{"type":"text","text":"ok"}]}`},
		{store.FormatGeminiGenerateContent, "/v1beta/models/test:generateContent", `{"contents":[]}`, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`},
	} {
		t.Run(tc.format, func(t *testing.T) {
			g, s := setup(t)
			var deniedHits, allowedHits atomic.Int32
			denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { deniedHits.Add(1); io.WriteString(w, tc.response) }))
			defer denied.Close()
			allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { allowedHits.Add(1); io.WriteString(w, tc.response) }))
			defer allowed.Close()
			m := store.DownstreamModel{ID: "test", Enabled: true, Fallback: false}
			for i, url := range []string{denied.URL, allowed.URL} {
				tags := []string{"billing:payg"}
				if i == 1 {
					tags = []string{"billing:free"}
				}
				c := store.Channel{Name: "scope fixture", APIKey: "synthetic", Models: []string{"test"}, Enabled: true, Tags: tags, Endpoints: []store.Endpoint{{Format: tc.format, BaseURL: url}}}
				if err := s.SaveChannel(&c); err != nil {
					t.Fatal(err)
				}
				m.Bindings = append(m.Bindings, store.Binding{ChannelID: c.ID, Model: "test", Enabled: true, Priority: i})
			}
			if err := s.SaveDownstreamModel(m); err != nil {
				t.Fatal(err)
			}
			scope := store.DefaultClientKeyScope()
			scope.TagRules["billing"] = []string{"billing:free"}
			key, token := scopeKey(t, s, scope)
			if w := scopedTurn(g, token, "POST", tc.path, tc.body, ""); w.Code != 200 {
				t.Fatalf("allowed: %d %s", w.Code, w.Body)
			}
			if deniedHits.Load() != 0 || allowedHits.Load() != 1 {
				t.Fatalf("candidate filtering happened after priority/fallback: denied=%d allowed=%d", deniedHits.Load(), allowedHits.Load())
			}
			scope.TagRules["billing"] = []string{}
			if _, err := s.UpdateClientKey(key.ID, "closed", scope); err != nil {
				t.Fatal(err)
			}
			if w := scopedTurn(g, token, "POST", tc.path, tc.body, ""); w.Code != 403 {
				t.Fatalf("empty tag allowlist: %d %s", w.Code, w.Body)
			}
			if deniedHits.Load() != 0 || allowedHits.Load() != 1 {
				t.Fatal("denial reached upstream")
			}
			usage, err := s.ListUsage(store.UsageFilter{Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if len(usage) != 2 || !usage[0].NotSent || usage[0].Status != 403 || usage[0].Error != "client access scope denies" || len(usage[0].Attempts) != 0 {
				t.Fatalf("local denial audit: %+v", usage)
			}
			if len(failedCombinations(g)) != 0 {
				t.Fatal("scope denial affected upstream health")
			}
		})
	}
}

func TestScopeHardPinSoftPinAndSuccessfulPath(t *testing.T) {
	for _, mode := range []string{"hard", "soft", "path"} {
		t.Run(mode, func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 1)
			var firstHits, secondHits atomic.Int32
			for i, hits := range []*atomic.Int32{&firstHits, &secondHits} {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if mode == "hard" {
						completedResponses(w, r)
						return
					}
					io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
				}))
				defer up.Close()
				add(t, s, up.URL, "openai", i)
			}
			channels, _ := s.ListChannels()
			channels[0].Tags = []string{"billing:payg"}
			channels[1].Tags = []string{"billing:free"}
			for i := range channels {
				if mode == "hard" {
					channels[i].Endpoints[0].Format = store.FormatOpenAIResponses
				}
				if err := s.SaveChannel(&channels[i]); err != nil {
					t.Fatal(err)
				}
			}
			key, token := scopeKey(t, s, store.DefaultClientKeyScope())
			session := "scope-session"
			if mode == "path" {
				session = ""
			}
			path, firstBody := "/v1/chat/completions", `{"model":"test","messages":[]}`
			if mode == "hard" {
				path, firstBody = "/v1/responses", `{"model":"test","input":"hi"}`
			}
			if w := scopedTurn(g, token, "POST", path, firstBody, session); w.Code != 200 {
				t.Fatalf("seed: %d %s", w.Code, w.Body)
			}
			scope := store.DefaultClientKeyScope()
			scope.TagRules["billing"] = []string{"billing:free"}
			if _, err := s.UpdateClientKey(key.ID, "narrowed", scope); err != nil {
				t.Fatal(err)
			}
			body := `{"model":"test","messages":[]}`
			want := 200
			if mode == "hard" {
				body = `{"model":"test","previous_response_id":"resp_native","input":"continue"}`
				want = 403
			} else if mode == "soft" {
				body = `{"model":"test","messages":[{"role":"assistant","content":"old","reasoning_content":"opaque"}]}`
			}
			w := scopedTurn(g, token, "POST", path, body, session)
			if w.Code != want {
				t.Fatalf("%s: %d %s", mode, w.Code, w.Body)
			}
			wantSecond := int32(1)
			if mode == "hard" {
				wantSecond = 0
			}
			if firstHits.Load() != 1 || secondHits.Load() != wantSecond {
				t.Fatalf("old permission path reused: first=%d second=%d", firstHits.Load(), secondHits.Load())
			}
		})
	}
}

func TestScopeDiscoverySpecsUnknownAndAdminIsolation(t *testing.T) {
	g, s := setup(t)
	add(t, s, "http://127.0.0.1:1", "openai", 0)
	add(t, s, "http://127.0.0.1:1", "openai", 1)
	channels, _ := s.ListChannels()
	channels[0].Name = "secret-payg-provider"
	channels[0].Tags = []string{"billing:payg"}
	channels[0].ModelConfigs = map[string]store.ModelConfig{"test": {ContextWindow: defaultBudget(999)}}
	channels[1].Name = "allowed-unknown-provider"
	channels[1].ModelConfigs = map[string]store.ModelConfig{"test": {ContextWindow: defaultBudget(123)}}
	for i := range channels {
		if err := s.SaveChannel(&channels[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "empty", Enabled: true, Bindings: []store.Binding{{ChannelID: channels[0].ID, Model: "test", Enabled: false}}}); err != nil {
		t.Fatal(err)
	}
	scope := store.DefaultClientKeyScope()
	scope.TagRules["billing"] = []string{"billing:unknown"}
	key, token := scopeKey(t, s, scope)
	for _, path := range []string{"/v1/models", "/v1/models/specs"} {
		w := scopedTurn(g, token, "GET", path, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"test"`) || strings.Contains(w.Body.String(), `"id":"empty"`) {
			t.Fatalf("listing: %d %s", w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "secret-payg-provider") || strings.Contains(w.Body.String(), "999") {
			t.Fatalf("unauthorized specs leaked: %s", w.Body)
		}
		if path == "/v1/models/specs" && (!strings.Contains(w.Body.String(), `"context_length":123`) || !strings.Contains(w.Body.String(), "allowed-unknown-provider")) {
			t.Fatal(w.Body)
		}
	}
	admin := httptest.NewRecorder()
	g.ModelSpecs(admin, httptest.NewRequest("GET", "/models/specs", nil))
	if admin.Code != 200 || !strings.Contains(admin.Body.String(), "secret-payg-provider") {
		t.Fatalf("admin projection narrowed: %d %s", admin.Code, admin.Body)
	}
	scope.ModelMode = "selected"
	scope.ModelIDs = []string{}
	if _, err := s.UpdateClientKey(key.ID, "empty", scope); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/models", "/v1/models/specs"} {
		w := scopedTurn(g, token, "GET", path, "", "")
		var result struct {
			Data []any `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		if w.Code != 200 || len(result.Data) != 0 {
			t.Fatalf("selected empty listing: %d %s", w.Code, w.Body)
		}
	}
	if w := scopedTurn(g, token, "POST", "/v1/chat/completions", `{"model":"test","messages":[]}`, ""); w.Code != 403 {
		t.Fatalf("selected empty call: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/v1/models", "/v1/models/specs", "/v1/chat/completions"} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatalf("missing client context: %s %d", path, w.Code)
		}
	}
}

func TestScopeRequestSnapshotSurvivesUpdateDuringFallback(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var key store.ClientKey
	var firstHits, forbiddenHits, finalHits atomic.Int32
	for i, hits := range []*atomic.Int32{&firstHits, &forbiddenHits, &finalHits} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if i == 0 {
				closed := store.DefaultClientKeyScope()
				closed.ModelMode = "selected"
				if _, err := s.UpdateClientKey(key.ID, "updated during request", closed); err != nil {
					t.Error(err)
				}
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(429)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		}))
		defer up.Close()
		add(t, s, up.URL, "openai", i)
	}
	channels, _ := s.ListChannels()
	for i := range channels {
		channels[i].Tags = []string{"billing:free"}
		if i == 1 {
			channels[i].Tags = []string{"billing:payg"}
		}
		if err := s.SaveChannel(&channels[i]); err != nil {
			t.Fatal(err)
		}
	}
	scope := store.DefaultClientKeyScope()
	scope.TagRules["billing"] = []string{"billing:free"}
	var token string
	key, token = scopeKey(t, s, scope)
	w := scopedTurn(g, token, "POST", "/v1/chat/completions", `{"model":"test","messages":[]}`, "")
	if w.Code != 200 || firstHits.Load() != 1 || forbiddenHits.Load() != 0 || finalHits.Load() != 1 {
		t.Fatalf("snapshot/fallback: %d %s hits=%d/%d/%d", w.Code, w.Body, firstHits.Load(), forbiddenHits.Load(), finalHits.Load())
	}
	if w = scopedTurn(g, token, "POST", "/v1/chat/completions", `{"model":"test","messages":[]}`, ""); w.Code != 403 {
		t.Fatalf("new request ignored update: %d %s", w.Code, w.Body)
	}
}

func TestScopeAllowedUnavailableIs503AndStaysDiscoverable(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 1, 1)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer up.Close()
	add(t, s, up.URL, "openai", 0)
	_, token := scopeKey(t, s, store.DefaultClientKeyScope())
	if w := scopedTurn(g, token, "POST", "/v1/chat/completions", `{"model":"test","messages":[]}`, ""); w.Code != 429 {
		t.Fatalf("initial: %d %s", w.Code, w.Body)
	}
	if w := scopedTurn(g, token, "POST", "/v1/chat/completions", `{"model":"test","messages":[]}`, ""); w.Code != 503 {
		t.Fatalf("cooldown should be temporary: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/v1/models", "/v1/models/specs"} {
		w := scopedTurn(g, token, "GET", path, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"test"`) {
			t.Fatalf("temporary health hid model: %d %s", w.Code, w.Body)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("cooldown reached upstream: %d", hits.Load())
	}
}

func TestScopeNarrowingDoesNotEstablishNativeStateOwnership(t *testing.T) {
	fixtures := []struct {
		name, format, path, first, next, response string
		originRequired                            bool
	}{
		{"responses cursor", store.FormatOpenAIResponses, "/v1/responses", `{"model":"test","input":"hi"}`, `{"model":"test","previous_response_id":"resp_from_a","input":"continue"}`, `{"id":"resp_from_a","object":"response","status":"completed","output":[]}`, true},
		{"chat reasoning", store.FormatOpenAIChatCompletions, "/v1/chat/completions", `{"model":"test","messages":[]}`, `{"model":"test","messages":[{"role":"assistant","content":"old","reasoning_content":"reasoning_from_a"}]}`, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`, false},
		{"messages signature", store.FormatAnthropicMessages, "/v1/messages", `{"model":"test","messages":[],"max_tokens":10}`, `{"model":"test","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"old","signature":"signature_from_a"}]}]}`, `{"id":"msg","type":"message","content":[{"type":"text","text":"ok"}]}`, false},
		{"gemini signature", store.FormatGeminiGenerateContent, "/v1beta/models/test:generateContent", `{"contents":[]}`, `{"contents":[{"role":"model","parts":[{"text":"old","thoughtSignature":"signature_from_a"}]}]}`, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`, false},
		{"gemini cached history", store.FormatGeminiGenerateContent, "/v1beta/models/test:generateContent", `{"contents":[]}`, `{"cachedContent":"cachedContents/from_a","contents":[{"role":"user","parts":[{"text":"continue"}]}]}`, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`, true},
	}
	for _, state := range responsesStateFixtures {
		fixtures = append(fixtures, struct {
			name, format, path, first, next, response string
			originRequired                            bool
		}{state.name, store.FormatOpenAIResponses, "/v1/responses", `{"model":"test","input":"hi"}`, `{"model":"test",` + state.fields + `}`, `{"id":"resp_from_a","object":"response","status":"completed","output":[]}`, true})
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			g, s := setup(t)
			var hitsA, hitsB atomic.Int32
			model := store.DownstreamModel{ID: "test", Enabled: true, Fallback: true}
			for i, hits := range []*atomic.Int32{&hitsA, &hitsB} {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if i == 1 && !fixture.originRequired {
						raw, err := io.ReadAll(r.Body)
						marker := "signature_from_a"
						if fixture.format == store.FormatOpenAIChatCompletions {
							marker = "reasoning_from_a"
						}
						if err != nil || !strings.Contains(string(raw), marker) {
							t.Errorf("authorized path must preserve carried thinking: %s %v", raw, err)
						}
					}
					io.WriteString(w, fixture.response)
				}))
				defer up.Close()
				tags := []string{"billing:payg"}
				if i == 1 {
					tags = []string{"billing:free"}
				}
				channel := store.Channel{Name: "scope state", APIKey: "synthetic", Models: []string{"test"}, Enabled: true, Tags: tags, Endpoints: []store.Endpoint{{Format: fixture.format, BaseURL: up.URL}}}
				if err := s.SaveChannel(&channel); err != nil {
					t.Fatal(err)
				}
				model.Bindings = append(model.Bindings, store.Binding{ChannelID: channel.ID, Model: "test", Enabled: true, Priority: i})
			}
			if err := s.SaveDownstreamModel(model); err != nil {
				t.Fatal(err)
			}
			key, token := scopeKey(t, s, store.DefaultClientKeyScope())
			if w := scopedTurn(g, token, "POST", fixture.path, fixture.first, ""); w.Code != 200 {
				t.Fatalf("first turn: %d %s", w.Code, w.Body)
			}
			if hitsA.Load() != 1 || hitsB.Load() != 0 || len(g.affinity.entries) != 0 {
				t.Fatal("first stateless turn must reach A without establishing a session/carrier pin")
			}
			narrowed := store.DefaultClientKeyScope()
			narrowed.TagRules["billing"] = []string{"billing:free"}
			if _, err := s.UpdateClientKey(key.ID, "B only", narrowed); err != nil {
				t.Fatal(err)
			}
			w := scopedTurn(g, token, "POST", fixture.path, fixture.next, "")
			if !fixture.originRequired {
				if w.Code != 200 || hitsA.Load() != 1 || hitsB.Load() != 1 {
					t.Fatalf("carried thinking may use only the current authorized path: status=%d hits=%d/%d body=%s", w.Code, hitsA.Load(), hitsB.Load(), w.Body)
				}
				return
			}
			if w.Code != 409 || hitsA.Load() != 1 || hitsB.Load() != 0 {
				t.Fatalf("permission filtering must not establish state ownership: status=%d hits=%d/%d body=%s", w.Code, hitsA.Load(), hitsB.Load(), w.Body)
			}
			if code, _ := recoveryOf(t, w.Body.Bytes()); code != "state_affinity_unknown" {
				t.Fatalf("recovery=%q", code)
			}
		})
	}
}
