package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/codex"
	"torouter/internal/management"
	"torouter/internal/siwc"
)

type callbackUpstreamTransport struct{ calls atomic.Int32 }

func (t *callbackUpstreamTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("synthetic upstream secret")
}

func TestOAuthCallbackBindFailureHTTPAndMCP(t *testing.T) {
	for _, provider := range []string{"chatgpt", "codex"} {
		t.Run(provider, func(t *testing.T) {
			// Use a port selected by the kernel, never production's pinned ports.
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer occupied.Close()
			port := occupied.Addr().(*net.TCPAddr).Port
			if provider == "codex" {
				ipv6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
				if err != nil {
					t.Skipf("cannot hold IPv6 loopback for the localhost failure test: %v", err)
				}
				defer ipv6.Close()
				original := codex.CallbackPorts
				codex.CallbackPorts = []int{port}
				defer func() { codex.CallbackPorts = original }()
			} else {
				original := siwc.CallbackPorts
				siwc.CallbackPorts = []int{port}
				defer func() { siwc.CallbackPorts = original }()
			}

			_, st := probeStore(t)
			upstream := &callbackUpstreamTransport{}
			client := &http.Client{Transport: upstream}
			h := httptest.NewUnstartedServer(nil)
			origin := "http://" + h.Listener.Addr().String()
			console := NewWithOAuth(st, origin, Hooks{}, nil,
				siwc.NewService(st, client, siwc.DefaultEndpoints()),
				codex.NewService(st, client, codex.DefaultEndpoints())).(consoleHandler)
			defer ReleaseOAuthListeners(console)
			mux := http.NewServeMux()
			mux.Handle("/mcp", ManagementHandler(console))
			h.Config.Handler = mux
			h.Start()
			defer h.Close()

			w := httptest.NewRecorder()
			console.ServeHTTP(w, httptest.NewRequest(http.MethodPost, authorizationFixturePath(t, console, "/oauth/"+provider+"/start"), nil))
			if w.Code != http.StatusBadGateway {
				t.Fatalf("HTTP status %d, body %s", w.Code, w.Body.String())
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{occupied.Addr().String(), "地址已被占用", "检查占用端口的程序"} {
				if !strings.Contains(body.Error.Message, want) {
					t.Fatalf("HTTP diagnostic missing %q: %s", want, body.Error.Message)
				}
			}
			if provider == "codex" && !strings.Contains(body.Error.Message, net.JoinHostPort("::1", strconv.Itoa(port))) {
				t.Fatalf("IPv6 failure missing: %s", body.Error.Message)
			}

			harness := &dataHarness{store: st, http: h}
			mcpClient, _ := harness.client(t, management.Read, management.Manage)
			out := dataCall(t, mcpClient, "start_oauth", dataWrite(map[string]any{"provider": provider, "operation": "add_credential", "method": "browser", "channel_id": 1}))
			if out["status"] != "failed" {
				t.Fatalf("local callback failure must be known failed: %v", out)
			}
			public := out["result"].(map[string]any)["error"].(map[string]any)
			if public["code"] != "operation_failed" || public["message"] != body.Error.Message {
				t.Fatalf("MCP did not preserve the HTTP diagnostic: %v", out)
			}
			console.oauth.mu.Lock()
			flowCount := len(console.oauth.flows)
			console.oauth.mu.Unlock()
			channels, err := st.ListChannels()
			if err != nil || flowCount != 0 || len(channels) != 1 || len(channels[0].Credentials) != 0 || upstream.calls.Load() != 0 {
				t.Fatalf("failed bind started effects: flows=%d channels=%d upstream=%d err=%v", flowCount, len(channels), upstream.calls.Load(), err)
			}

			// A different 502 may follow an external action. Its outcome remains
			// unknown and the upstream's private error text remains hidden.
			if provider == "codex" {
				out = dataCall(t, mcpClient, "start_oauth", dataWrite(map[string]any{"provider": provider, "operation": "add_credential", "method": "device", "channel_id": 1}))
				raw, err := json.Marshal(out)
				if err != nil {
					t.Fatal(err)
				}
				if out["status"] != "unknown" || strings.Contains(string(raw), "synthetic upstream secret") || upstream.calls.Load() != 1 {
					t.Fatalf("non-callback 502 lost uncertainty/privacy: %s calls=%d", raw, upstream.calls.Load())
				}
			}
		})
	}
}

func TestManagementCallbackBindClassificationUsesType(t *testing.T) {
	const diagnostic = "无法监听授权回调端口：受控本地诊断"
	local := &oauthCallbackBindError{&ManagementToolError{Status: 502, Message: diagnostic}}
	wrapped := fmt.Errorf("wrapper: %w", local)
	var endpoint *ManagementToolError
	if !errors.As(wrapped, &endpoint) || endpoint.Status != 502 || endpoint.Message != diagnostic {
		t.Fatalf("HTTP status/message not preserved: %v", wrapped)
	}
	var safe *management.Error
	if !errors.As(managementDomainError(wrapped), &safe) || safe.Code != "operation_failed" || safe.Message != diagnostic {
		t.Fatalf("typed local failure not preserved: %v", safe)
	}
	// Matching text must not grant another error permission to expose its body.
	remote := &ManagementToolError{Status: 502, Message: diagnostic + ": synthetic private detail"}
	if !errors.As(managementDomainError(remote), &safe) || safe.Code != "unknown" || strings.Contains(safe.Message, "受控本地诊断") || strings.Contains(safe.Message, "synthetic private detail") {
		t.Fatalf("arbitrary 502 was exposed: %v", safe)
	}
}
