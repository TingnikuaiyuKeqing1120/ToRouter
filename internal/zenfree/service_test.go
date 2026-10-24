package zenfree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/nativeoauth"
	"torouter/internal/store"
	"torouter/internal/streamreply"
)

func fixtureStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestModelsRequireLiveFreePriceAndKnownProtocol(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/models":
			if r.Header.Get("Authorization") != "Bearer public" {
				t.Error("missing public authorization")
			}
			io.WriteString(w, `{"data":[{"id":"chat"},{"id":"response"},{"id":"message"},{"id":"paid-free"},{"id":"unknown-free"},{"id":"retired"},{"id":"systemone"},{"id":"chat"}]}`)
		case "/catalogue":
			io.WriteString(w, `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{
				"chat":{"cost":{"input":0,"output":0}},
				"response":{"cost":{"input":0,"output":0},"provider":{"npm":"@ai-sdk/openai"}},
				"message":{"cost":{"input":0,"output":0},"provider":{"npm":"@ai-sdk/anthropic"}},
				"paid-free":{"cost":{"input":1,"output":0}},
				"retired":{"cost":{"input":0,"output":0},"status":"retired"},
				"systemone":{"cost":{"input":0,"output":0}}
			}}}`)
		case "/docs":
			io.WriteString(w, "| System | `systemone` | `https://example.test/v1/systemone` |")
		default:
			t.Error("unexpected network path")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	service := NewService(fixtureStore(t), server.Client(), Endpoints{APIBase: server.URL, CatalogueURL: server.URL + "/catalogue", ProtocolsURL: server.URL + "/docs"})
	models, err := service.Models(context.Background())
	if err != nil || len(models) != 3 {
		t.Fatalf("models=%v err=%v", models, err)
	}
	got := map[string]string{}
	for _, m := range models {
		got[m.ID] = m.Format
	}
	if got["response"] != store.FormatOpenAIResponses || got["message"] != store.FormatAnthropicMessages || got["chat"] != store.FormatOpenAIChatCompletions || hits.Load() != 3 {
		t.Fatal(got, hits.Load())
	}
}

func TestPublicConnectIsLocalAndIdempotent(t *testing.T) {
	st := fixtureStore(t)
	service := NewService(st, nil, Endpoints{})
	first, linked, err := service.Connect(context.Background())
	if err != nil || linked {
		t.Fatal(linked, err)
	}
	second, linked, err := service.Connect(context.Background())
	if err != nil || !linked || second.ID != first.ID {
		t.Fatal("duplicate public channel", err)
	}
	loaded, err := st.Channel(first.ID)
	if err != nil || loaded.OAuth != nil || len(loaded.Models) != 0 || loaded.ModelFormats == nil || loaded.PrimaryKey() != "public" {
		t.Fatal("public channel invented an OAuth login or enabled models")
	}
}

func TestExecuteKeepsProtocolBodyAndDoesNotInventClientTools(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var hits atomic.Int32
	var session string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer public" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Error("wrong public profile")
		}
		if session != "" && session != r.Header.Get("x-opencode-session") {
			t.Error("session changed between turns")
		}
		session = r.Header.Get("x-opencode-session")
		if !nativeSession.MatchString(session) {
			t.Error("invalid session shape")
		}
		var v map[string]any
		if json.NewDecoder(r.Body).Decode(&v) != nil || v["stream"] != true {
			t.Error("not a streaming request")
		}
		if len(v["tools"].([]any)) != 5 || !strings.Contains(string(mustJSON(v["messages"])), "reasoning_content") {
			t.Error("changed client context")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"synthetic\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\",\"reasoning_content\":\"thought\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	service := NewService(fixtureStore(t), server.Client(), Endpoints{APIBase: server.URL})
	c := store.Channel{CredentialType: store.CredentialZenFree, APIKey: "public", ModelFormats: map[string][]string{"model": {store.FormatOpenAIChatCompletions}}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: server.URL}}}
	body := map[string]any{"model": "model", "messages": []any{map[string]any{"role": "user", "content": "same root"}, map[string]any{"role": "assistant", "content": "history", "reasoning_content": "earlier thought"}}}
	makeRequest := func() *http.Request {
		req, _ := http.NewRequest("POST", server.URL+"/chat/completions", bytes.NewReader(mustJSON(body)))
		return req
	}
	if _, err := service.Execute(server.Client(), makeRequest(), c); !errors.Is(err, nativeoauth.ErrUnsupportedRequest) || hits.Load() != 0 {
		t.Fatal("invented tools or sent before validation", err)
	}
	body["tools"] = ProbeTools(store.FormatOpenAIChatCompletions)
	for i := 0; i < 2; i++ {
		resp, err := service.Execute(server.Client(), makeRequest(), c)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(result), `"reasoning_content":"thought"`) || !strings.Contains(string(result), `"prompt_tokens":4`) {
			t.Fatal(string(result))
		}
	}
	if hits.Load() != 2 {
		t.Fatal("unexpected discovery or retry")
	}
}
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestAssemblyRequiresTerminalAndPreservesOpaqueFields(t *testing.T) {
	for _, tc := range []struct{ name, format, stream, contains string }{
		{"messages", store.FormatAnthropicMessages,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"a\",\"role\":\"assistant\",\"type\":\"message\",\"content\":[],\"usage\":{\"input_tokens\":9}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"opaque thought\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"synthetic-signature\"}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", `"signature":"synthetic-signature"`},
		{"responses", store.FormatOpenAIResponses,
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"a\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"synthetic-cipher\"}],\"usage\":{\"input_tokens\":9,\"output_tokens\":5}}}\n\n", `"encrypted_content":"synthetic-cipher"`},
		{"chat_tools", store.FormatOpenAIChatCompletions,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"p\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", `"arguments":"{\"p\":1}"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := streamreply.Assemble(strings.NewReader(tc.stream), "text/event-stream", tc.format)
			if err != nil || !strings.Contains(string(out), tc.contains) {
				t.Fatalf("%s %v", out, err)
			}
			// Cut at every byte of the final frame. No truncated prefix may
			// become a successful non-stream response.
			terminal := strings.LastIndex(strings.TrimSuffix(tc.stream, "\n\n"), "\n\n") + 2
			for n := terminal; n < len(tc.stream); n++ {
				if _, err := streamreply.Assemble(strings.NewReader(tc.stream[:n]), "text/event-stream", tc.format); err == nil {
					t.Fatalf("accepted truncated terminal at %d", n)
				}
			}
		})
	}
}
