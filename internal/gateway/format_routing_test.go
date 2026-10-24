package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/store"
)

// Responses is a fourth format: a Chat body must be converted before reaching
// its endpoint, and the answer converted back into the client's format.
func TestResponsesOnlyChannelConvertsChatRequest(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Error("converted request did not reach /responses")
		}
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["input"] == nil || request["messages"] != nil {
			t.Errorf("unconverted request: %v error=%v", request, err)
		}
		io.WriteString(w, `{"id":"resp_fixture","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "resp-only", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "test", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("converted request contacted the upstream %d times", hits.Load())
	}
	var body struct {
		Error struct {
			Recovery struct {
				Code string `json:"code"`
			} `json:"recovery"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Recovery.Code != "" || !strings.Contains(w.Body.String(), `"choices"`) || !strings.Contains(w.Body.String(), `"content":"ok"`) {
		t.Fatalf("not a converted Chat answer: %s", w.Body.String())
	}
}

// T019/T020: a channel declaring both Responses and Messages serves a chat
// request on its Messages endpoint, in either array order. Array order is
// display order; picking by it is what used to post chat bodies to /responses.
func TestMixedChannelServesChatFromMessagesEndpoint(t *testing.T) {
	orders := map[string][]string{
		"responses_first": {store.FormatOpenAIResponses, store.FormatAnthropicMessages},
		"messages_first":  {store.FormatAnthropicMessages, store.FormatOpenAIResponses},
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 1)
			var path atomic.Value
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path.Store(r.URL.Path)
				switch {
				case strings.HasSuffix(r.URL.Path, "/responses"):
					t.Error("a chat request reached /responses")
					io.WriteString(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/messages"):
					io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					io.WriteString(w, `{}`)
				}
			}))
			defer up.Close()
			c := store.Channel{Name: "both", APIKey: "k", Models: []string{"test"}, Enabled: true,
				Endpoints: []store.Endpoint{{Format: order[0], BaseURL: up.URL}, {Format: order[1], BaseURL: up.URL}}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "test", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if got, _ := path.Load().(string); !strings.HasSuffix(got, "/messages") {
				t.Fatalf("chat request served from %s", got)
			}
			if !strings.Contains(w.Body.String(), "ok") {
				t.Fatalf("converted answer lost: %s", w.Body.String())
			}
		})
	}
}

// T020: the preferred endpoint decides which endpoint serves a chat request
// when neither matches the inbound format; unset means the fixed default order
// Chat Completions > Messages > Gemini.
func TestPreferredEndpointDecidesConversionTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preferred string
		want      string
	}{
		{"default order picks messages", "", "/v1/messages"},
		{"preferred endpoint wins", store.FormatGeminiGenerateContent, "/v1beta/models/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 1)
			var path atomic.Value
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path.Store(r.URL.Path)
				switch {
				case strings.Contains(r.URL.Path, "/v1beta/models/"):
					io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
				case strings.HasSuffix(r.URL.Path, "/messages"):
					io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					io.WriteString(w, `{}`)
				}
			}))
			defer up.Close()
			c := store.Channel{Name: "two", APIKey: "k", Models: []string{"test"}, Enabled: true,
				Endpoints:         []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: up.URL}, {Format: store.FormatGeminiGenerateContent, BaseURL: up.URL}},
				PreferredEndpoint: tc.preferred}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "test", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if got, _ := path.Load().(string); !strings.Contains(got, tc.want) {
				t.Fatalf("request served from %s, want %s", got, tc.want)
			}
		})
	}
}

// T019: the format-based passthrough must not break the native direction. A
// responses request served by a responses endpoint passes through untouched,
// body included, while the channel's chat endpoint stays out of it.
func TestResponsesInboundPassesThroughToResponsesEndpoint(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var path atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		io.WriteString(w, `{"id":"resp_1","object":"response","output":[]}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "both", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}, {Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "test", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	w := invoke(g, "/v1/responses", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got, _ := path.Load().(string); !strings.HasSuffix(got, "/responses") {
		t.Fatalf("responses request served from %s", got)
	}
	if w.Body.String() != `{"id":"resp_1","object":"response","output":[]}` {
		t.Fatalf("responses body not passed through: %s", w.Body.String())
	}
}
