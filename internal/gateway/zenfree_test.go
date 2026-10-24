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

	"torouter/internal/plugins"
	"torouter/internal/store"
	"torouter/internal/zenfree"
)

func TestZenFreeUsesModelProtocolAndNeverReplaysTruncatedAnswer(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, truncated := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "truncated"}[truncated], func(t *testing.T) {
			var hits atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.URL.Path != "/v1/responses" {
					t.Errorf("model protocol not applied: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if truncated {
					io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"a\",\"status\":\"in_progress\"}}\n\n")
					return
				}
				io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"a\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"wire-model\",\"output\":[{\"type\":\"message\",\"id\":\"msg_a\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"fixture reply\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
			}))
			defer up.Close()
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			service := zenfree.NewService(st, up.Client(), zenfree.Endpoints{APIBase: up.URL + "/v1"})
			manager, err := plugins.NewWithZenFreeService(st, service)
			if err != nil {
				t.Fatal(err)
			}
			list, _ := manager.Snapshot()
			if _, err = manager.SetCapability(context.Background(), "opencode-zen-free", "connect", true, list.Version); err != nil {
				t.Fatal(err)
			}
			c, _, err := service.Connect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			c.Models = []string{"wire-model"}
			c.ModelFormats = map[string][]string{"wire-model": {store.FormatOpenAIResponses}}
			if err = st.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if err = st.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "wire-model", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": "fixture request"}}, "tools": zenfree.ProbeTools(store.FormatOpenAIChatCompletions)})
			w := invoke(NewWithPlugins(st, manager), "/v1/chat/completions", string(body))
			if hits.Load() != 1 {
				t.Fatalf("expected exactly one inference, got %d: %d %s", hits.Load(), w.Code, w.Body.String())
			}
			if truncated {
				if w.Code != 502 || !strings.Contains(w.Body.String(), "did not retry") {
					t.Fatalf("truncated: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code != 200 || !strings.Contains(w.Body.String(), "fixture reply") {
				t.Fatalf("converted: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
