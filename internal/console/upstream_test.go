package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"torouter/internal/store"
)

func TestDiscoverAndProbeProtocols(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch protocol {
				case "openai":
					if r.Header.Get("Authorization") != "Bearer test-secret" {
						t.Error("auth")
					}
				case "anthropic":
					if r.Header.Get("x-api-key") != "test-secret" || r.Header.Get("anthropic-version") == "" || r.Header.Get("Authorization") != "Bearer test-secret" {
						t.Error("auth")
					}
				case "gemini":
					if r.Header.Get("x-goog-api-key") != "test-secret" {
						t.Error("auth")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					version := "/v1"
					if protocol == "gemini" {
						version = "/v1beta"
					}
					if r.URL.Path != "/api"+version+"/models" {
						t.Error(r.URL.Path)
					}
					if protocol == "gemini" {
						if r.URL.Query().Get("pageToken") == "two" {
							fmt.Fprint(w, `{"models":[{"name":"models/b","supportedGenerationMethods":["generateContent"]},{"name":"models/embed","supportedGenerationMethods":["embedContent"]}]}`)
						} else {
							fmt.Fprint(w, `{"models":[{"name":"models/a","displayName":"A"}],"nextPageToken":"two"}`)
						}
					} else {
						if r.URL.Query().Get("after_id") == "a" {
							fmt.Fprint(w, `{"data":[{"id":"a"},{"id":"b"}],"has_more":false}`)
						} else {
							fmt.Fprint(w, `{"data":[{"id":"a"}],"has_more":true,"last_id":"a"}`)
						}
					}
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				switch protocol {
				case "openai":
					if r.URL.Path != "/api/v1/chat/completions" || body["model"] != "a" {
						t.Error("bad probe")
					}
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
				case "anthropic":
					if r.URL.Path != "/api/v1/messages" || body["max_tokens"] != float64(64) {
						t.Error("bad probe")
					}
					fmt.Fprint(w, `{"type":"message","content":[{"type":"text","text":"OK"}]}`)
				case "gemini":
					if r.URL.Path != "/api/v1beta/models/a:generateContent" {
						t.Error("bad probe")
					}
					fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`)
				}
			}))
			defer upstream.Close()
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := New(s, "http://127.0.0.1:17878", Hooks{})
			c := store.Channel{Name: "test", Protocol: protocol, BaseURL: upstream.URL + "/api", APIKey: "test-secret", Enabled: true, Models: []string{"a"}, AvailableModels: []string{"a", "b"}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			c.APIKey = ""
			c.Credentials = nil
			call := func(path string, confirmed bool) *httptest.ResponseRecorder {
				b, _ := json.Marshal(map[string]any{"channel": c, "model": "a", "confirmed": confirmed})
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(string(b))))
				return w
			}
			// A legacy "openai" row implies both OpenAI facades, so discover
			// answers per format; the other two declare exactly one. The calls a
			// discovery makes is one paginated fetch per format.
			declared := map[string][]string{
				"openai":    {store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses},
				"anthropic": {store.FormatAnthropicMessages},
				"gemini":    {store.FormatGeminiGenerateContent},
			}[protocol]
			discovered := func(result toolResult) []discoveredModel {
				out := []discoveredModel{}
				seen := map[string]bool{}
				for _, entry := range result.Formats {
					for _, m := range entry.Models {
						if !seen[m.ID] {
							seen[m.ID] = true
							out = append(out, m)
						}
					}
				}
				if len(result.Formats) == 0 {
					out = append(out, result.Models...)
				}
				return out
			}
			w := call("/channels/discover", false)
			var result toolResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || !result.OK || len(discovered(result)) != 2 {
				t.Fatalf("discover %d %s", w.Code, w.Body)
			}
			if calls.Load() != int32(2*len(declared)) {
				t.Fatal("pagination")
			}
			// More than one format is answered per format; exactly one keeps the
			// flat answer the console's model list and its existing clients read.
			if len(declared) > 1 {
				if len(result.Formats) != len(declared) {
					t.Fatalf("one answer per declared format, got %d want %d: %s", len(result.Formats), len(declared), w.Body)
				}
			} else if len(result.Models) != 2 {
				t.Fatalf("a single-format discover lost the flat answer: %s", w.Body)
			}
			w = call("/channels/probe", false)
			if w.Code != 400 || calls.Load() != int32(2*len(declared)) {
				t.Fatal("unconfirmed paid request sent")
			}
			w = call("/channels/probe", true)
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.OK {
				t.Fatalf("probe %s", w.Body)
			}
			if got := int(calls.Load()) - 2*len(declared); got != 1 {
				t.Fatalf("probe made %d requests, want one", got)
			}
			calls.Store(0)
			rows, err := s.ListUsage(store.UsageFilter{})
			if err != nil || len(rows) != 0 {
				t.Fatal("probe polluted business logs")
			}
			listed, _ := s.ListChannels()
			if len(listed[0].Models) != 1 || len(listed[0].AvailableModels) != 2 {
				t.Fatal("discovery mutated config")
			}
			// Changing the destination must force the key to be re-entered; with
			// endpoints that means changing an endpoint's base URL.
			c.Endpoints[0].BaseURL += "/changed"
			w = call("/channels/discover", false)
			if w.Code != 400 || calls.Load() != 0 {
				t.Fatal("saved secret used with changed destination")
			}
			// An unchanged destination still reuses the key.
			c.Endpoints[0].BaseURL = strings.TrimSuffix(c.Endpoints[0].BaseURL, "/changed")
			w = call("/channels/discover", false)
			if w.Code != 200 || calls.Load() != int32(2*len(declared)) {
				t.Fatalf("unchanged destination should reuse the key: %d %s", w.Code, w.Body)
			}
		})
	}
}
func TestUpstreamDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body, code string
	}{
		{"redirect", 302, "move", "redirect"}, {"auth", 401, `{"error":{"message":"bad test-secret","api_key":"another-secret"}}`, "upstream_http"},
		{"rate", 429, `{"error":"quota"}`, "upstream_http"}, {"html", 200, "<html>login</html>", "invalid_response"},
		{"loop", 200, `{"data":[{"id":"a"}],"has_more":true,"last_id":"a"}`, "pagination_loop"},
		{"too_big", 200, strings.Repeat("a", diagnosticBodyLimit+1), "response_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var redirected atomic.Bool
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Store(true) }))
			defer sink.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", sink.URL)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer upstream.Close()
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := New(s, "http://127.0.0.1:17878", Hooks{})
			b, _ := json.Marshal(map[string]any{"channel": store.Channel{Protocol: "openai", BaseURL: upstream.URL, APIKey: "test-secret"}, "formats": []string{store.FormatOpenAIChatCompletions}})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/channels/discover", strings.NewReader(string(b))))
			var result toolResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.OK || result.Diagnostic == nil || result.Diagnostic.Code != test.code {
				t.Fatalf("%s", w.Body)
			}
			if strings.Contains(w.Body.String(), "test-secret") || strings.Contains(w.Body.String(), "another-secret") {
				t.Fatal("secret exposed")
			}
			if redirected.Load() {
				t.Fatal("followed redirect")
			}
		})
	}
}
func TestModelInventoryCanDisableAll(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := store.Channel{Name: "all-disabled", Protocol: "openai", BaseURL: "http://localhost:9999", APIKey: "secret", Enabled: true, Models: []string{}, AvailableModels: []string{"a", "b"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 || len(rows[0].Models) != 0 || len(rows[0].AvailableModels) != 2 {
		t.Fatal("inventory not persisted")
	}
}

// An applied discover writes the fetched catalogue back to the channel: the
// auto part becomes what the upstream returned, entries it no longer lists
// disappear, and the owner's manual entries and enabled models are kept. Without
// apply the fetch stays a read-only look, exactly as before.
func TestDiscoverAppliesCatalogueReplacement(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"kept"},{"id":"fresh"}],"has_more":false}`)
	}))
	defer upstream.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	c := store.Channel{Name: "test", Protocol: "openai", BaseURL: upstream.URL, APIKey: "test-secret", Enabled: true,
		Models: []string{"kept", "retired"}, AvailableModels: []string{"kept", "retired", "stale", "hand"}, ManualModels: []string{"hand"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	draft, _ := json.Marshal(c)
	call := func(apply bool) toolResult {
		body := map[string]any{"channel": json.RawMessage(draft)}
		if apply {
			body["apply"] = true
		}
		raw, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/channels/discover", strings.NewReader(string(raw))))
		var result toolResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.OK {
			t.Fatalf("discover failed: %s", w.Body)
		}
		return result
	}
	// Read-only: the channel is untouched, as the existing tool promised.
	call(false)
	stored, _ := s.ListChannels()
	if len(stored[0].AvailableModels) != 4 || len(stored[0].Models) != 2 {
		t.Fatalf("read-only discover mutated the channel: %+v", stored[0])
	}
	result := call(true)
	// The catalogue arrives in the fetch's own (sorted) order, manual entries
	// appended after what the upstream listed.
	want := []string{"fresh", "kept", "hand"}
	if !slices.Equal(result.Catalogue, want) {
		t.Fatalf("applied catalogue = %v, want %v", result.Catalogue, want)
	}
	stored, _ = s.ListChannels()
	if !slices.Equal(stored[0].AvailableModels, want) {
		t.Fatalf("replacement not persisted: %v", stored[0].AvailableModels)
	}
	if !slices.Equal(stored[0].Models, c.Models) {
		t.Fatalf("enabled models changed by the fetch: %v", stored[0].Models)
	}
}

// unauthorized answers the way an upstream refusing a credential does.
func unauthorized(w http.ResponseWriter) {
	w.WriteHeader(401)
	fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
}

// A format the channel does not declare is asked at the channel default, the
// only address it could live at; one with no default anywhere is reported as an
// invalid address instead of being guessed at.
func TestDiscoverReportsFormatWithoutAnAddress(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	body, _ := json.Marshal(map[string]any{
		"channel": map[string]any{"name": "own-addresses", "api_key": "test-secret", "endpoints": []map[string]any{{"format": store.FormatOpenAIChatCompletions, "base_url": "https://api.example.com"}}},
		"formats": []string{store.FormatAnthropicMessages},
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/channels/discover", strings.NewReader(string(body))))
	var result toolResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	// One named format keeps the flat answer, so the reason sits at the top.
	failure := result.Diagnostic
	if failure == nil && len(result.Formats) == 1 {
		failure = result.Formats[0].Diagnostic
	}
	if result.OK || failure == nil || failure.Code != "url" {
		t.Fatalf("undeclared format without an address = %s", w.Body)
	}
}

// One channel, two facades off one base: the answer is per format, so a base
// that serves Chat Completions and not Gemini says exactly that instead of one
// verdict about "the upstream". The channel declares no endpoint addresses of
// its own — the channel default is where both are reached, which is the shape
// the checkbox editor saves — so this also covers the console resolving that
// fallback per format.
func TestDiscoverAnswersPerFormat(t *testing.T) {
	var geminiDown atomic.Bool
	var requests atomic.Int32
	geminiDown.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gemini := strings.HasPrefix(r.URL.Path, "/v1beta")
		if gemini && r.Header.Get("x-goog-api-key") != "test-secret" {
			unauthorized(w)
			return
		}
		if !gemini && r.Header.Get("Authorization") != "Bearer test-secret" {
			unauthorized(w)
			return
		}
		if gemini && geminiDown.Load() {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"gemini facade down"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"chat-a"},{"id":"shared"}],"has_more":false}`)
		case "/v1beta/models":
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-a","displayName":"Gemini A"}]}`)
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
		case "/v1beta/models/shared:generateContent":
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	c := store.Channel{Name: "two-facades", APIKey: "test-secret", Enabled: true,
		BaseURL:   upstream.URL,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions}, {Format: store.FormatGeminiGenerateContent}},
		Models:    []string{"shared"}, AvailableModels: []string{"shared", "retired"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	// The default is stored expanded, so every reader sees the address each
	// endpoint is reached at.
	stored, _ := s.ListChannels()
	for _, e := range stored[0].Endpoints {
		if e.BaseURL != upstream.URL {
			t.Fatalf("endpoint %s stored with address %q, want the channel default", e.Format, e.BaseURL)
		}
	}
	// The draft keeps the empty addresses: the console resolves them per format.
	draft := func(patch map[string]any) map[string]any {
		body := map[string]any{
			"channel": map[string]any{
				"id": c.ID, "name": c.Name, "api_key": "test-secret", "enabled": true,
				"base_url":  upstream.URL,
				"endpoints": []map[string]any{{"format": store.FormatOpenAIChatCompletions}, {"format": store.FormatGeminiGenerateContent}},
			},
		}
		for k, v := range patch {
			body[k] = v
		}
		return body
	}
	call := func(path string, body map[string]any) (int, toolResult) {
		raw, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(string(raw))))
		var result toolResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Code, result
	}
	catalogue := func() []string {
		rows, err := s.ListChannels()
		if err != nil || len(rows) != 1 {
			t.Fatal(err)
		}
		return rows[0].AvailableModels
	}
	format := func(result toolResult, want string) formatResult {
		t.Helper()
		for _, entry := range result.Formats {
			if entry.Format == want {
				return entry
			}
		}
		t.Fatalf("no answer for %s: %+v", want, result.Formats)
		return formatResult{}
	}
	// The Gemini facade is down: one answer per format, the failure named, and
	// nothing written back.
	code, result := call("/channels/discover", draft(nil))
	if code != 200 || result.OK || len(result.Formats) != 2 {
		t.Fatalf("per-format discover: %d %+v", code, result)
	}
	chat := format(result, store.FormatOpenAIChatCompletions)
	if !chat.OK || len(chat.Models) != 2 || chat.Diagnostic != nil {
		t.Fatalf("chat completions answer = %+v", chat)
	}
	gemini := format(result, store.FormatGeminiGenerateContent)
	if gemini.OK || gemini.Diagnostic == nil || gemini.Diagnostic.Code != "upstream_http" || gemini.UpstreamStatus != 503 {
		t.Fatalf("gemini answer = %+v", gemini)
	}
	// A partial answer is not authority over the whole catalogue, so applying it
	// writes nothing: the facade that is down would silently delete its models.
	if code, result = call("/channels/discover", draft(map[string]any{"apply": true})); code != 200 || result.OK || result.Catalogue != nil {
		t.Fatalf("applied a partial fetch: %d %+v", code, result)
	}
	if got := catalogue(); !slices.Equal(got, []string{"shared", "retired"}) {
		t.Fatalf("partial fetch wrote the catalogue: %v", got)
	}
	// Both facades answer: the union is the catalogue, manual entries saved.
	if code, result = call("/channels/discover", draft(nil)); code != 200 || result.OK {
		t.Fatalf("gemini still down: %d %+v", code, result)
	}
	geminiDown.Store(false)
	code, result = call("/channels/discover", draft(map[string]any{"apply": true}))
	if code != 200 || !result.OK || len(result.Formats) != 2 {
		t.Fatalf("both facades up: %d %+v", code, result)
	}
	if got := result.Catalogue; !slices.Equal(got, []string{"chat-a", "shared", "gemini-a"}) {
		t.Fatalf("catalogue = %v, want the union across the formats that answered", got)
	}
	if got := catalogue(); !slices.Equal(got, []string{"chat-a", "shared", "gemini-a"}) {
		t.Fatalf("catalogue not persisted: %v", got)
	}
	// Naming one format answers for that one and keeps the flat shape.
	code, result = call("/channels/discover", draft(map[string]any{"formats": []string{store.FormatGeminiGenerateContent}}))
	if code != 200 || !result.OK || len(result.Formats) != 0 || len(result.Models) != 1 || result.Models[0].ID != "gemini-a" {
		t.Fatalf("single named format: %d %+v", code, result)
	}
	// An unknown name is refused rather than skipped: a silently dropped format
	// is an answer the caller cannot see.
	if code, _ = call("/channels/discover", draft(map[string]any{"formats": []string{"translated_from_latin"}})); code != 400 {
		t.Fatalf("unknown format = %d, want a refusal", code)
	}
	// A re-pointed channel default is a changed destination: the saved key may
	// not be reused against it.
	moved := draft(nil)
	moved["channel"].(map[string]any)["api_key"] = ""
	moved["channel"].(map[string]any)["base_url"] = upstream.URL + "/moved"
	if code, _ = call("/channels/discover", moved); code != 400 {
		t.Fatalf("reused the saved key against a moved default: %d", code)
	}
	// Naming formats is how a probe asks the other facades, and a paid request
	// still needs the explicit confirmation.
	if code, _ = call("/channels/probe", draft(map[string]any{"model": "shared"})); code != 400 {
		t.Fatalf("unconfirmed probe = %d", code)
	}
	geminiDown.Store(true)
	code, result = call("/channels/probe", draft(map[string]any{"model": "shared", "confirmed": true, "formats": []string{store.FormatOpenAIChatCompletions, store.FormatGeminiGenerateContent}}))
	if code != 200 || result.OK || len(result.Formats) != 2 {
		t.Fatalf("per-format probe: %d %+v", code, result)
	}
	if !format(result, store.FormatOpenAIChatCompletions).OK || format(result, store.FormatGeminiGenerateContent).OK {
		t.Fatalf("probe answers = %+v", result.Formats)
	}
	// An unnamed probe is one paid generation, not one per declared format: the
	// channel declares two and the console's per-model test promises one.
	geminiDown.Store(false)
	requests.Store(0)
	code, result = call("/channels/probe", draft(map[string]any{"model": "shared", "confirmed": true}))
	if code != 200 || !result.OK || len(result.Formats) != 0 || requests.Load() != 1 {
		t.Fatalf("unnamed probe made %d requests: %d %+v", requests.Load(), code, result)
	}
}

// Deleting one model from a channel takes it out of the catalogue and the
// enabled list; both a catalogue-only entry and an enabled one may go.
func TestDeleteChannelModelEndpoint(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	c := store.Channel{Name: "test", Protocol: "openai", BaseURL: "http://localhost:9999", APIKey: "secret", Enabled: true,
		Models: []string{"enabled", "hand"}, AvailableModels: []string{"enabled", "hand", "catalogue-only"}, ManualModels: []string{"hand"},
		ModelConfigs: map[string]store.ModelConfig{"enabled": {MaxOutputTokens: ptr(int64(32))}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	remove := func(model string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("DELETE", "/channels/"+fmt.Sprint(c.ID)+"/models?model="+url.QueryEscape(model), nil))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, body := remove("enabled"); code != 200 {
		t.Fatalf("delete an enabled entry: %d %v", code, body)
	}
	if code, _ := remove("catalogue-only"); code != 200 {
		t.Fatal("delete a catalogue-only entry")
	}
	if code, _ := remove("hand"); code != 200 {
		t.Fatal("delete a manual entry")
	}
	if code, _ := remove("ghost"); code != 404 {
		t.Fatal("deleting a model the channel does not list")
	}
	if code, _ := remove(""); code != 400 {
		t.Fatal("deleting without a model id")
	}
	stored, _ := s.ListChannels()
	if len(stored[0].Models) != 0 || len(stored[0].AvailableModels) != 0 || len(stored[0].ManualModels) != 0 {
		t.Fatalf("deletes left traces: %+v", stored[0])
	}
	if _, ok := stored[0].ModelConfigs["enabled"]; ok {
		t.Fatal("config of a deleted model survived")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("DELETE", "/channels/999/models?model=x", nil))
	if w.Code != 404 {
		t.Fatal("deleting from a channel that does not exist")
	}
}
