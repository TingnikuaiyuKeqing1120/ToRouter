package gateway

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/antigravity"
	"torouter/internal/claude"
	"torouter/internal/devin"
	"torouter/internal/nativeoauth"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

func nativeGatewayFixture(t *testing.T, driver nativeoauth.Driver) (*Gateway, store.Channel) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	def := driver.Definition()
	c := store.Channel{
		Name: "Synthetic native", CredentialType: def.CredentialType, Enabled: true,
		Endpoints: def.Endpoints, Models: []string{"fixture-model"},
		Credentials: []store.CredentialItem{{ID: "fixture", Enabled: true, OAuth: &store.OAuthCredential{
			Provider: def.Provider, ClientID: "fixture-client", Account: "fixture-account", Subject: "fixture-account",
			DeviceID: strings.Repeat("a", 64), ProjectID: "fixture-project", AccessToken: "synthetic-access",
			RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour), Status: store.OAuthStatusActive,
		}}},
	}
	if def.CredentialType == store.CredentialDevinOAuth {
		c.Credentials[0].OAuth.RefreshToken, c.Credentials[0].OAuth.ExpiresAt = "", time.Time{}
	}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "fixture-model", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	manager, err := plugins.NewWithNativeServices(st, map[string]*nativeoauth.Service{def.CredentialType: nativeoauth.NewService(st, driver)})
	if err != nil {
		t.Fatal(err)
	}
	return NewWithPlugins(st, manager), c
}

func TestNativeDevinGatewayConvertsAndDoesNotReplayTruncation(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, stream := range []bool{false, true} {
		for _, truncated := range []bool{false, true} {
			t.Run(map[bool]string{false: "json", true: "stream"}[stream]+map[bool]string{false: "complete", true: "truncated"}[truncated], func(t *testing.T) {
				var calls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/exa.api_server_pb.ApiServerService/GetChatMessage" || r.Header.Get("Authorization") != "Basic synthetic-access" {
						t.Error("wrong Devin wire")
					}
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), "fixture-model") || !strings.Contains(string(body), "fixture greeting") {
						t.Error("request context lost")
					}
					w.Header().Set("Content-Type", "application/connect+proto")
					writeFrame := func(flags byte, b []byte) {
						header := []byte{flags, 0, 0, 0, 0}
						binary.BigEndian.PutUint32(header[1:], uint32(len(b)))
						w.Write(header)
						w.Write(b)
					}
					// Protobuf field 3 = text, field 5 = actual stop, field 7 = usage.
					payload := append([]byte{0x1a, 14}, []byte("fixture answer")...)
					writeFrame(0, payload)
					if !truncated {
						writeFrame(0, []byte{0x28, 2, 0x3a, 4, 0x10, 3, 0x18, 2})
						writeFrame(2, []byte(`{}`))
					}
				}))
				defer up.Close()
				g, _ := nativeGatewayFixture(t, devin.NewDriver(up.Client(), devin.Endpoints{ServerURL: up.URL}))
				body, _ := json.Marshal(map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": "fixture greeting"}}, "stream": stream})
				w := invoke(g, "/v1/chat/completions", string(body))
				if calls.Load() != 1 {
					t.Fatalf("replayed Devin inference: %d", calls.Load())
				}
				if truncated {
					if !stream && (w.Code != 502 || !strings.Contains(w.Body.String(), "did not retry")) {
						t.Fatalf("bad ambiguous failure: %d %s", w.Code, w.Body.String())
					}
					if stream && strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
						t.Fatal("truncated RPC marked successful")
					}
				} else if w.Code != 200 || !strings.Contains(w.Body.String(), "fixture answer") {
					t.Fatalf("response conversion: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestNativeAntigravityGatewayWrapsConvertedRequestAndUnwrapsResponse(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1internal:generateContent" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Errorf("wrong native destination/auth: %s", r.URL.Path)
		}
		var envelope struct {
			Model, Project string
			Request        map[string]json.RawMessage
		}
		if json.NewDecoder(r.Body).Decode(&envelope) != nil || envelope.Project != "fixture-project" || envelope.Model != "fixture-model" || !strings.Contains(string(envelope.Request["contents"]), "fixture greeting") {
			t.Error("native wrapper lost converted request or selected project")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"fixture answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}`)
	}))
	defer up.Close()
	g, _ := nativeGatewayFixture(t, antigravity.NewDriver(up.Client(), antigravity.Endpoints{APIBase: up.URL}))
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"fixture greeting"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fixture answer") || calls.Load() != 1 {
		t.Fatalf("native gateway: %d %s calls=%d", w.Code, w.Body.String(), calls.Load())
	}
}

func TestNativeClaudeGatewayUsesProfileAndPluginGate(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer synthetic-access" || r.Header.Get("ChatGPT-Account-ID") != "" || r.Header.Get("anthropic-beta") == "" {
			t.Error("Claude request used another provider's profile")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || !strings.Contains(string(body["messages"]), "fixture greeting") || !strings.Contains(string(body["metadata"]), "fixture-account") {
			t.Error("Claude request lost content or credential identity")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fixture-message","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"fixture answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer up.Close()
	g, _ := nativeGatewayFixture(t, claude.NewDriver(up.Client(), claude.Endpoints{APIBase: up.URL}))
	body := `{"model":"test","max_tokens":128,"messages":[{"role":"user","content":"fixture greeting"}]}`
	w := invoke(g, "/v1/messages", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fixture answer") || calls.Load() != 1 {
		t.Fatalf("native Claude gateway: %d %s calls=%d", w.Code, w.Body.String(), calls.Load())
	}
	list, _ := g.Plugins.Snapshot()
	if _, err := g.Plugins.SetCapability(context.Background(), "claude", "connect", false, list.Version); err != nil {
		t.Fatal(err)
	}
	w = invoke(g, "/v1/messages", body)
	if w.Code != 503 || calls.Load() != 1 {
		t.Fatal("disabled native provider still sent inference")
	}
}
