package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestDiscoveryBoundsAndEmptyList(t *testing.T) {
	for _, mode := range []string{"empty", "pages", "models"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				switch mode {
				case "empty":
					fmt.Fprint(w, `{"data":[]}`)
				case "pages":
					fmt.Fprintf(w, `{"data":[{"id":"m%d"}],"has_more":true,"last_id":"m%d"}`, n, n)
				case "models":
					models := make([]map[string]string, 10001)
					for i := range models {
						models[i] = map[string]string{"id": fmt.Sprintf("m%d", i)}
					}
					json.NewEncoder(w).Encode(map[string]any{"data": models})
				}
			}))
			defer upstream.Close()
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			body, _ := json.Marshal(map[string]any{"channel": store.Channel{Protocol: "openai", BaseURL: upstream.URL, APIKey: "test-secret"}, "formats": []string{store.FormatOpenAIChatCompletions}})
			w := httptest.NewRecorder()
			New(s, "http://127.0.0.1:17878", Hooks{}).ServeHTTP(w, httptest.NewRequest("POST", "/channels/discover", strings.NewReader(string(body))))
			var result toolResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "empty":
				if !result.OK || !strings.Contains(w.Body.String(), `"models":[]`) {
					t.Fatal(w.Body.String())
				}
			case "pages":
				if result.OK || result.Diagnostic == nil || result.Diagnostic.Code != "page_limit" || calls.Load() != 20 {
					t.Fatal(w.Body.String())
				}
			case "models":
				if result.OK || result.Diagnostic == nil || result.Diagnostic.Code != "model_limit" {
					t.Fatal(w.Body.String())
				}
			}
		})
	}
}

func TestUpstreamCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(canceled) }))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := store.Channel{Protocol: "openai", BaseURL: upstream.URL, APIKey: "test-secret"}
	target, _ := endpoint(c.Base(), "openai", "/models")
	u := upstreamTools{client: upstream.Client()}
	done := make(chan *diagnostic, 1)
	go func() { _, _, d := u.request(ctx, "openai", c.APIKey, "GET", target, nil, false); done <- d }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case d := <-done:
		if d == nil || d.Code != "connection" {
			t.Fatal("cancellation not reported")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request ignored cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not canceled")
	}
}
