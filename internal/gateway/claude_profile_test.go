package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"torouter/internal/claude"
)

func TestNativeClaudeGatewayPreservesExplicitFeatureHeaders(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var betas string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betas = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fixture-message","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"fixture answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer up.Close()
	g, _ := nativeGatewayFixture(t, claude.NewDriver(up.Client(), claude.Endpoints{APIBase: up.URL}))
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test","max_tokens":128,"messages":[{"role":"user","content":"fixture greeting"}]}`))
	r.Header.Add("Anthropic-Beta", "context-1m-2025-08-07")
	r.Header.Add("Anthropic-Beta", "synthetic-caller-feature")
	w := httptest.NewRecorder()
	serveTestClient(g, w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("request failed: %d", w.Code)
	}
	for _, beta := range []string{"context-1m-2025-08-07", "synthetic-caller-feature", "oauth-2025-04-20"} {
		if !strings.Contains(","+betas+",", ","+beta+",") {
			t.Errorf("missing feature declaration %s", beta)
		}
	}
}
