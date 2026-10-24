package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/oauthbase"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

type nativeFlowDriver struct {
	definition nativeoauth.Definition
	calls      atomic.Int32
	account    string
	token      string
}

func (d *nativeFlowDriver) Definition() nativeoauth.Definition { return d.definition }
func (d *nativeFlowDriver) Begin(_ context.Context, redirect string) (nativeoauth.Authorization, error) {
	return nativeoauth.Authorization{URL: "https://example.invalid/authorize", State: "fixture-state", Verifier: "fixture-verifier", RedirectURI: redirect}, nil
}
func (d *nativeFlowDriver) Exchange(_ context.Context, _ nativeoauth.Authorization, _ oauthbase.CallbackResult) (store.OAuthCredential, error) {
	account, token := d.account, d.token
	if account == "" {
		account = "fixture-account"
	}
	if token == "" {
		token = "synthetic-access"
	}
	result := store.OAuthCredential{Provider: d.definition.Provider, ClientID: "fixture-client", Account: account, Subject: account, DeviceID: strings.Repeat("a", 64), ProjectID: "fixture-project", AccessToken: token, RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour), Status: store.OAuthStatusActive}
	if d.definition.CredentialType == store.CredentialDevinOAuth {
		result.RefreshToken, result.ExpiresAt = "", time.Time{}
	}
	return result, nil
}
func (d *nativeFlowDriver) Refresh(_ context.Context, c store.OAuthCredential) (store.OAuthCredential, error) {
	return c, nil
}
func (d *nativeFlowDriver) Models(context.Context, store.OAuthCredential) ([]nativeoauth.Model, error) {
	return []nativeoauth.Model{{ID: "fixture-model", Format: d.definition.Endpoints[0].Format}}, nil
}
func (d *nativeFlowDriver) Execute(client *http.Client, req *http.Request, _ store.OAuthCredential) (*http.Response, error) {
	d.calls.Add(1)
	return accessguard.Do(client, req)
}

func TestNativeOAuthConsoleFlowModelsProbeAndDisabledLocalRead(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, provider := range []struct{ id, ctype, format string }{
		{"claude", store.CredentialClaudeOAuth, store.FormatAnthropicMessages},
		{"antigravity", store.CredentialAntigravityOAuth, store.FormatGeminiGenerateContent},
		{"devin", store.CredentialDevinOAuth, store.FormatOpenAIResponses},
	} {
		t.Run(provider.id, func(t *testing.T) {
			_, st := probeStore(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-access" {
					t.Error("probe omitted the selected native credential")
				}
				w.Header().Set("Content-Type", "application/json")
				if provider.id == "claude" {
					io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
				} else if provider.id == "devin" {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture-response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")
				} else {
					io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
				}
			}))
			defer up.Close()
			driver := &nativeFlowDriver{definition: nativeoauth.Definition{ID: provider.id, Provider: provider.id, Name: "Synthetic " + provider.id, CredentialType: provider.ctype, CallbackHost: "127.0.0.1", CallbackPath: "/callback", Endpoints: []store.Endpoint{{Format: provider.format, BaseURL: up.URL}}}}
			poolFixture := store.Channel{Name: driver.definition.Name, CredentialType: provider.ctype, Enabled: true, Endpoints: driver.definition.Endpoints}
			if err := st.SaveChannel(&poolFixture); err != nil {
				t.Fatal(err)
			}
			manager, err := plugins.NewWithNativeServices(st, map[string]*nativeoauth.Service{provider.ctype: nativeoauth.NewService(st, driver)})
			if err != nil {
				t.Fatal(err)
			}
			list, _ := manager.Snapshot()
			if _, err = manager.SetCapability(context.Background(), provider.id, "connect", true, list.Version); err != nil {
				t.Fatal(err)
			}
			handler := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, manager)
			t.Cleanup(func() { ReleaseOAuthListeners(handler) })
			base := "/oauth/" + provider.id
			var start struct {
				Flow     string `json:"flow"`
				Redirect string `json:"redirect_uri"`
				Path     string `json:"callback_path"`
			}
			_ = json.Unmarshal(pluginHTTP(t, handler, "POST", authorizationFixturePath(t, handler, base+"/start"), "", 200).Body.Bytes(), &start)
			if start.Flow == "" || start.Path != "/callback" {
				t.Fatalf("wrong start payload: %+v", start)
			}
			callback := start.Redirect + "?code=fixture-code&state=fixture-state"
			path := base + "/status?flow=" + start.Flow + "&callback=" + url.QueryEscape(callback)
			var result struct {
				Status    string `json:"status"`
				ChannelID int64  `json:"channel_id"`
			}
			_ = json.Unmarshal(pluginHTTP(t, handler, "GET", path, "", 200).Body.Bytes(), &result)
			if result.Status != "complete" || result.ChannelID == 0 {
				t.Fatalf("authorization not complete: %+v", result)
			}
			ch, err := st.Channel(result.ChannelID)
			if err != nil || len(ch.Models) != 0 || len(ch.AvailableModels) != 0 {
				t.Fatal("authorization should preserve account without auto-enabling models")
			}
			channelPath := "/channels/" + strconv.FormatInt(result.ChannelID, 10)
			fetched := pluginHTTP(t, handler, "POST", channelPath+"/oauth/models", "", 200)
			if !strings.Contains(fetched.Body.String(), "fixture-model") {
				t.Fatalf("model fetch failed: %s", fetched.Body.String())
			}
			probe := map[string]any{"model": "fixture-model", "confirmed": true, "formats": []string{provider.format}}
			encoded, _ := json.Marshal(probe)
			w := pluginHTTP(t, handler, "POST", channelPath+"/probe", string(encoded), 200)
			if driver.calls.Load() != 1 || !strings.Contains(w.Body.String(), `"ok":true`) {
				t.Fatalf("probe did not use native driver: %s", w.Body.String())
			}
			before, _ := st.Channel(result.ChannelID)
			authorize := func(query string) {
				t.Helper()
				start = struct {
					Flow     string `json:"flow"`
					Redirect string `json:"redirect_uri"`
					Path     string `json:"callback_path"`
				}{}
				_ = json.Unmarshal(pluginHTTP(t, handler, "POST", authorizationFixturePath(t, handler, base+"/start?"+query), "", 200).Body.Bytes(), &start)
				callback = start.Redirect + "?code=fixture-code&state=fixture-state"
				payload := pluginHTTP(t, handler, "GET", authorizationFixturePath(t, handler, base+"/status?flow="+start.Flow+"&callback="+url.QueryEscape(callback)), "", 200).Body.String()
				if !strings.Contains(payload, `"status":"complete"`) {
					t.Fatalf("native pool authorization: %s", payload)
				}
			}
			driver.account = "fixture-second"
			authorize("operation=add_credential&channel_id=" + strconv.FormatInt(result.ChannelID, 10))
			pool, _ := st.Channel(result.ChannelID)
			if len(pool.Credentials) != 2 {
				t.Fatal("new authorization did not join the pool directly")
			}
			all, _ := st.ListChannels()
			if len(all) != 1 {
				t.Fatal("authorization copied a temporary channel")
			}
			if err = st.SaveChannelConfig(&before); err != store.ErrEditConflict {
				t.Fatal("stale draft removed newly authorized pool member", err)
			}
			driver.token = "synthetic-reauth-two"
			authorize("operation=reauth&channel_id=" + strconv.FormatInt(result.ChannelID, 10) + "&credential_id=" + url.QueryEscape(pool.Credentials[1].ID))
			pool, _ = st.Channel(result.ChannelID)
			if pool.Credentials[0].OAuth.AccessToken != "synthetic-access" || pool.Credentials[1].OAuth.AccessToken != "synthetic-reauth-two" {
				t.Fatal("reauthorization updated the wrong credential")
			}
			injected, _ := json.Marshal(pool)
			pluginHTTP(t, handler, "PUT", channelPath, string(injected), 400)
			copyDraft := `{"credential_type":"` + provider.ctype + `","credentials":[{"id":"copy","enabled":true,"source_channel_id":` + strconv.FormatInt(pool.ID, 10) + `}]}`
			pluginHTTP(t, handler, "PUT", channelPath, copyDraft, 400)
			consolePluginToggle(t, handler, provider.id, false)
			pluginHTTP(t, handler, "POST", authorizationFixturePath(t, handler, base+"/start"), "", 503)
			pluginHTTP(t, handler, "GET", "/channels?credential_type="+provider.ctype, "", 200)
			if _, err := st.Channel(result.ChannelID); err != nil {
				t.Fatal("disabling lost native account")
			}
		})
	}
}
