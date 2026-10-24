package pluginhost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/compatprofile"
	"torouter/internal/store"
)

// Runs the shipped JS through the real private worker against a local fake
// vendor. Only origins are redirected; provider code, paths and auth stay real.
func featuredFixture(t *testing.T, credentialType string, handler http.HandlerFunc, expire bool) (*Service, store.Channel, *store.Store) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	p, ok := compatprofile.Lookup(credentialType)
	if !ok {
		t.Fatal("unknown fixture provider")
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	expires := time.Now().Add(time.Hour)
	if expire {
		expires = time.Now().Add(5 * time.Second)
	}
	raw, _ := json.Marshal(map[string]any{"type": "oauth", "uid": "synthetic-account", "realUserID": "synthetic-account",
		"access": "synthetic-access", "refresh": "synthetic-refresh", "expires": expires.UnixMilli(),
		"domain": "example.test", "sessionID": "12345678-1234-4123-8123-123456789012"})
	o, err := store.CompatibleOAuthCredential(credentialType, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	channel, _, err := st.CommitPluginConsent(credentialType, o, store.PluginConsentTarget{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	for _, name := range []string{"index.mjs", "contract.mjs"} {
		data, err := packageFiles.ReadFile("packages/" + p.Package + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, from := range []string{origin(p.API), p.Web, p.OpenAPI} {
			data = []byte(strings.ReplaceAll(string(data), from, server.URL))
		}
		if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	bridge := filepath.Join(dir, "host.mjs")
	if err = os.WriteFile(bridge, Bridge, 0600); err != nil {
		t.Fatal(err)
	}
	p.API = strings.Replace(p.API, origin(p.API), server.URL, 1)
	p.OpenAPI, p.Web = server.URL, server.URL
	s := &Service{Store: st, Profile: p}
	s.launch = func(ctx context.Context, p compatprofile.Profile, state string) (*Worker, error) {
		w, err := Start(node, bridge)
		if err != nil {
			return nil, err
		}
		_, err = w.Invoke(ctx, "init", map[string]any{"protocol": 1, "module_path": filepath.Join(dir, "index.mjs"), "provider_id": p.ID,
			"export_name": p.Export, "methods": []int{0}, "auth": json.RawMessage(state)}, nil)
		if err != nil {
			w.Close()
			return nil, err
		}
		return w, nil
	}
	return s, channel, st
}

func TestFeaturedPackagesDurableRotationAndNativeResponses(t *testing.T) {
	for _, credentialType := range []string{compatprofile.WorkBuddyCredential, compatprofile.WorkBuddyAICredential, compatprofile.MiniMaxCredential, compatprofile.MiniMaxGlobalCredential} {
		t.Run(credentialType, func(t *testing.T) {
			mini := strings.Contains(credentialType, "minimax")
			var refreshes, requests atomic.Int32
			var st *store.Store
			var c store.Channel
			handler := func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth2/token" || r.URL.Path == "/v2/plugin/auth/token/refresh" {
					refreshes.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if mini {
						io.WriteString(w, `{"access_token":"synthetic-next","refresh_token":"synthetic-next-refresh","expires_in":3600}`)
					} else {
						io.WriteString(w, `{"code":0,"data":{"accessToken":"synthetic-next","refreshToken":"synthetic-next-refresh","expiresIn":3600}}`)
					}
					return
				}
				requests.Add(1)
				live, err := st.Channel(c.ID)
				if err != nil || !strings.Contains(live.OAuth.PluginState, "synthetic-next-refresh") || r.Header.Get("Authorization") != "Bearer synthetic-next" {
					t.Error("inference ran before rotated material was durable")
				}
				var request map[string]any
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("invalid request")
				}
				if mini {
					if r.URL.Path != "/mavis/api/v1/llm/v1/messages" || request["system"] != "Keep original system" || r.Header.Get("anthropic-beta") != "synthetic-beta" {
						t.Error("native Messages semantics or headers lost")
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"synthetic answer"}],"stop_reason":"end_turn"}`)
				} else {
					if r.URL.Path != "/v2/chat/completions" || request["stream"] != true {
						t.Error("wrong native Chat request")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"synthetic\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"synthetic answer\",\"reasoning_content\":\"kept thought\"},\"finish_reason\":\"stop\"}]}\n\n")
					if credentialType == compatprofile.WorkBuddyCredential {
						io.WriteString(w, "data: [DONE]\n\n")
					}
				}
			}
			var s *Service
			s, c, st = featuredFixture(t, credentialType, handler, true)
			suffix, body := "/chat/completions", `{"model":"synthetic","stream":false,"messages":[{"role":"user","content":"hello"}]}`
			if mini {
				suffix, body = "/messages", `{"model":"synthetic","max_tokens":30,"system":"Keep original system","messages":[{"role":"user","content":"hello"}]}`
			}
			req, _ := http.NewRequestWithContext(context.Background(), "POST", s.Profile.API+suffix, strings.NewReader(body))
			req.Header.Set("anthropic-beta", "synthetic-beta")
			resp, err := s.Execute(nil, req, c)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || !strings.Contains(string(data), "synthetic answer") || strings.Contains(string(data), `"usage"`) || refreshes.Load() != 1 || requests.Load() != 1 {
				t.Fatalf("response/rotation: %s %v, %d/%d", data, err, refreshes.Load(), requests.Load())
			}
			if !mini && (!strings.Contains(string(data), "kept thought") || resp.Header.Get("Content-Type") != "application/json") {
				t.Fatal("non-stream assembly lost reasoning or type")
			}
		})
	}
}

func TestWorkBuddyTruncatedStreamNeverBecomesSuccessOrReplay(t *testing.T) {
	var hits atomic.Int32
	s, c, _ := featuredFixture(t, compatprofile.WorkBuddyCredential, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
	}, false)
	req, _ := http.NewRequest("POST", s.Profile.API+"/chat/completions", strings.NewReader(`{"model":"synthetic","messages":[{"role":"user","content":"hello"}]}`))
	if _, err := s.Execute(nil, req, c); err == nil || hits.Load() != 1 {
		t.Fatalf("truncated response accepted or replayed: %v, %d", err, hits.Load())
	}
}

func TestFeaturedNetworkIsRegionalAndOperationSpecific(t *testing.T) {
	for _, p := range compatprofile.Profiles() {
		if approvedNetwork(p, networkEvent{"https://example.invalid", "/oauth2/token", "POST"}, false) ||
			approvedNetwork(p, networkEvent{p.OpenAPI, "/billing/meter/get-user-resource-summary", "GET"}, false) ||
			approvedNetwork(p, networkEvent{p.OpenAPI, "/v2/chat/completions", "POST"}, true) {
			t.Fatalf("%s broadened network authority", p.ID)
		}
	}
}
