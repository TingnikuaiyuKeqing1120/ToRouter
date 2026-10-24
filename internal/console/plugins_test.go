package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/copilot"
	"torouter/internal/management"
	"torouter/internal/plugins"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

func pluginHTTP(t *testing.T, handler http.Handler, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	if w.Code != status {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}

func consolePluginToggle(t *testing.T, handler http.Handler, id string, enabled bool) {
	t.Helper()
	var list plugins.List
	if err := json.Unmarshal(pluginHTTP(t, handler, "GET", "/plugins", "", 200).Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"enabled": enabled, "expected_version": list.Version})
	pluginHTTP(t, handler, "PUT", "/plugins/"+url.PathEscape(id)+"/capabilities/connect", string(body), 200)
}

func TestPluginsHTTPLazyAndVersionedWrites(t *testing.T) {
	_, st := probeStore(t)
	m, err := plugins.New(st)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	list := pluginHTTP(t, h, "GET", "/plugins", "", 200)
	if strings.Contains(list.Body.String(), "default_enabled") || strings.Contains(list.Body.String(), "defaultEnabled") {
		t.Fatal("internal default leaked")
	}
	for _, provider := range []string{store.CredentialCodexOAuth, store.CredentialCopilotOAuth, store.CredentialChatGPTOAuth} {
		if m.Existing(provider) != nil {
			t.Fatal("console construction or list created a service")
		}
	}
	pluginHTTP(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"), "", 503)
	pluginHTTP(t, h, "PUT", "/plugins/copilot/capabilities/connect", `{"enabled":true}`, 409)
	pluginHTTP(t, h, "PUT", "/plugins/copilot/capabilities/unknown", `{"enabled":true,"expected_version":"stale"}`, 400)
	pluginHTTP(t, h, "PUT", "/plugins/copilot/capabilities/connect", `{"expected_version":"stale"}`, 400)
	consolePluginToggle(t, h, "copilot", true)
	if !m.Enabled(store.CredentialCopilotOAuth) || m.Existing(store.CredentialCopilotOAuth) != nil {
		t.Fatal("toggle failed or eagerly built service")
	}
	prefs, err := st.GetPluginPreferences()
	if err != nil || !prefs.Plugins["copilot"]["connect"] {
		t.Fatalf("toggle not persisted: %v", err)
	}
	pluginHTTP(t, h, "GET", "/channels", "", 200)
}

func TestPluginsMCPUsesSameSwitchAndPermissions(t *testing.T) {
	h, _ := fullManagementHarness(t)
	reader, _ := h.client(t, management.Read)
	list := dataSuccess(t, reader, "list_plugins", map[string]any{})
	dataDenied(t, reader, "set_plugin_capability", dataWrite(map[string]any{"plugin_id": "codex", "capability": "connect", "enabled": false, "expected_version": list["version"]}))
	manager, _ := h.client(t, management.Read, management.Manage)
	changed := dataSuccess(t, manager, "set_plugin_capability", dataWrite(map[string]any{"plugin_id": "codex", "capability": "connect", "enabled": false, "expected_version": list["version"]}))
	if changed["version"] == list["version"] {
		t.Fatal("MCP toggle did not change snapshot")
	}
	prefs, err := h.store.GetPluginPreferences()
	if err != nil || prefs.Plugins["codex"]["connect"] {
		t.Fatalf("MCP toggle did not update shared store: %v", err)
	}
	failed := dataCall(t, manager, "set_plugin_capability", dataWrite(map[string]any{"plugin_id": "codex", "capability": "connect", "enabled": true, "expected_version": list["version"]}))
	if failed["status"] != "failed" {
		t.Fatalf("stale MCP toggle accepted: %v", failed)
	}
	// The same setting gates the HTTP authorization surface.
	settings, err := h.store.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.BuiltinChannel("Disabled target", store.CredentialCodexOAuth)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.store.SaveChannel(&target); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", h.http.URL+"/api/oauth/codex/start?operation=add_credential&method=browser&channel_id="+strconv.FormatInt(target.ID, 10), nil)
	token, err := h.store.CreateAdminSession(settings.GatewayKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-To-Router", "1")
	req.AddCookie(&http.Cookie{Name: "to_router_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("HTTP bypassed MCP switch: %d", resp.StatusCode)
	}
}

func TestPluginsFlowEpochAndPendingCancellation(t *testing.T) {
	_, st := probeStore(t)
	d := newOAuthDouble(t)
	m, err := plugins.NewInjected(st, d.service(st), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	start := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	flow := start["flow"].(string)
	consolePluginToggle(t, h, "copilot", false)
	consolePluginToggle(t, h, "copilot", true)
	pluginHTTP(t, h, "GET", "/oauth/copilot/status?flow="+flow, "", 503)
	if d.polls.Load() != 0 || d.exchangeCalls.Load() != 0 {
		t.Fatal("old flow contacted provider after an off/on cycle")
	}
	pluginHTTP(t, h, "POST", "/oauth/copilot/cancel?flow="+flow, "", 200)
	o := h.(consoleHandler).oauth
	o.mu.Lock()
	f := o.flows[flow]
	o.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != flowCancelled {
		t.Fatalf("pending flow not canceled: %s", f.state)
	}
}

func TestPluginsStatusCannotAdoptNewEpochWhileWaiting(t *testing.T) {
	for _, phase := range []string{"between_epoch_check_and_native", "after_native_before_flow_lock"} {
		t.Run(phase, func(t *testing.T) {
			_, st := probeStore(t)
			d := newOAuthDouble(t)
			d.pending.Store(100)
			m, err := plugins.NewInjected(st, d.service(st), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			f := &oauthFlow{pluginEpoch: m.Epoch(store.CredentialCopilotOAuth), id: "synthetic-flow", provider: store.CredentialCopilotOAuth,
				state: flowPending, expiresAt: time.Now().Add(time.Hour), device: copilot.DeviceFlow{ID: "synthetic-device"}}
			// Deliberately defer cancellation delivery: the flow remains pending,
			// just as when CancelProviderFlows is still waiting for another poll.
			o := &oauthTools{plugins: m, store: st, flows: map[string]*oauthFlow{f.id: f}}
			checkpoint, resume := make(chan struct{}), make(chan struct{})
			var checkpointOnce, resumeOnce, unlockOnce sync.Once
			pause := func() { checkpointOnce.Do(func() { close(checkpoint); <-resume }) }
			unblock := func() { resumeOnce.Do(func() { close(resume) }) }
			f.mu.Lock() // Represents the older poll holding the flow lifecycle lock.
			unlock := func() { unlockOnce.Do(f.mu.Unlock) }
			defer unlock()
			defer unblock()
			ctx := context.Background()
			if phase == "between_epoch_check_and_native" {
				ctx = accessguard.With(ctx, func() error { pause(); return nil })
			} else {
				// A throttled reply must also recheck access after waiting for
				// the flow lock, even though it would not send another request.
				f.lastPoll, f.interval = time.Now(), time.Hour
				ctx = WithOAuthFlowAccess(ctx, func(string) error { pause(); return nil })
			}
			result := make(chan error, 1)
			go func() { _, err := o.ManagementStatusCallback(ctx, "copilot", f.id, ""); result <- err }()
			select {
			case <-checkpoint:
			case <-time.After(3 * time.Second):
				t.Fatal("status did not reach the scheduling checkpoint")
			}
			for _, enabled := range []bool{false, true} {
				snapshot, err := m.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := m.SetCapability(context.Background(), "copilot", "connect", enabled, snapshot.Version); err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			unlock()
			select {
			case err := <-result:
				if !errors.Is(err, plugins.ErrDisabled) {
					t.Fatalf("old pending flow adopted the new epoch: %v; polls=%d exchanges=%d", err, d.polls.Load(), d.exchangeCalls.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("status did not leave the old flow lock")
			}
			if d.polls.Load() != 0 || d.exchangeCalls.Load() != 0 {
				t.Fatal("old pending flow sent another provider request")
			}
		})
	}
}

func TestPluginsBrowserFlowReleasesListenerAndRejectsOldEpoch(t *testing.T) {
	previous := codex.CallbackPorts
	codex.CallbackPorts = []int{0} // Synthetic callback, independent of OS-reserved production ports.
	t.Cleanup(func() { codex.CallbackPorts = previous })
	_, st := probeStore(t)
	m, err := plugins.New(st)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	flow := started["flow"].(string)
	o := h.(consoleHandler).oauth
	o.mu.Lock()
	f := o.flows[flow]
	o.mu.Unlock()
	f.mu.Lock()
	redirect, err := url.Parse(f.listener.RedirectURI)
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	consolePluginToggle(t, h, "codex", false)
	f.mu.Lock()
	closed := f.listener == nil && f.state == flowCancelled
	f.mu.Unlock()
	if !closed {
		t.Fatal("disabled flow retained its callback listener")
	}
	if conn, err := net.DialTimeout("tcp", redirect.Host, time.Second); err == nil {
		conn.Close()
		t.Fatal("disabled flow still accepts callbacks")
	}
	consolePluginToggle(t, h, "codex", true)
	pluginHTTP(t, h, "GET", "/oauth/codex/status?flow="+flow, "", 503)
}

func TestPluginsInitializationErrorsFailClosed(t *testing.T) {
	_, st := probeStore(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []http.Handler{
		New(st, "http://127.0.0.1:17878", Hooks{}),
		NewWithOAuth(st, "http://127.0.0.1:17878", Hooks{}, nil, nil, nil),
		NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, nil),
	} {
		pluginHTTP(t, handler, "GET", "/plugins", "", 503)
	}
}

func TestPluginsDisabledLocalOperationsDoNotCreateRuntime(t *testing.T) {
	_, st := probeStore(t)
	c := store.Channel{Name: "synthetic", CredentialType: store.CredentialCodexOAuth, Enabled: true,
		Models:      []string{"model"},
		Endpoints:   []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		Credentials: []store.CredentialItem{{ID: "one", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: "synthetic-client", Account: "synthetic-account", RefreshToken: "synthetic-refresh", AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour)}}},
	}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m, err := plugins.New(st)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	consolePluginToggle(t, h, "codex", false)
	base := fmt.Sprintf("/channels/%d/credentials/one/operations", c.ID)
	var report codex.OperationReport
	if err = json.Unmarshal(pluginHTTP(t, h, "GET", base, "", 200).Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	pluginHTTP(t, h, "PUT", base, fmt.Sprintf(`{"expected_identity":%q,"warmup":{"enabled":false,"mode":"reset","model":"model"}}`, report.Identity), 200)
	pluginHTTP(t, h, "POST", base+"/warmup", `{"expected_identity":"irrelevant","request_id":"synthetic-id"}`, 503)
	path := fmt.Sprintf("/oauth/codex/usage?channel_id=%d&credential_id=one", c.ID)
	pluginHTTP(t, h, "GET", path, "", 200)
	pluginHTTP(t, h, "POST", path, "", 503)
	pluginHTTP(t, h, "DELETE", fmt.Sprintf("/channels/%d", c.ID), "", 200)
	if m.Existing(store.CredentialCodexOAuth) != nil {
		t.Fatal("disabled local read/config/delete constructed a service")
	}
}

func TestPluginsDisabledSIWCDeleteUsesExistingServiceWithoutRevoke(t *testing.T) {
	_, st := probeStore(t)
	var calls atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer issuer.Close()
	svc := siwc.NewService(st, issuer.Client(), siwc.Endpoints{Revoke: issuer.URL})
	m, err := plugins.NewInjected(st, nil, svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime, _, err := m.Native(context.Background(), store.CredentialChatGPTOAuth)
	if err != nil {
		t.Fatal(err)
	}
	c := store.Channel{Name: "synthetic", CredentialType: store.CredentialChatGPTOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"}},
		OAuth:     &store.OAuthCredential{Provider: store.OAuthProviderChatGPT, ClientID: "oaiapp_synthetic", HostID: "synthetic-host", RefreshToken: "synthetic-refresh"},
	}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	consolePluginToggle(t, h, "chatgpt", false)
	w := pluginHTTP(t, h, "DELETE", fmt.Sprintf("/channels/%d", c.ID), "", 200)
	if calls.Load() != 0 || m.Existing(store.CredentialChatGPTOAuth) != runtime || !strings.Contains(w.Body.String(), "未尝试远端撤销") {
		t.Fatal("disabled delete revoked remotely, changed service or hid its local-only result")
	}
}

func TestPluginsDisableDuringRevokeDoesNotClaimLocalDeletion(t *testing.T) {
	_, st := probeStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(200)
	}))
	defer issuer.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	svc := siwc.NewService(st, issuer.Client(), siwc.Endpoints{Revoke: issuer.URL})
	m, err := plugins.NewInjected(st, nil, svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := store.Channel{Name: "synthetic", CredentialType: store.CredentialChatGPTOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"}},
		OAuth:     &store.OAuthCredential{Provider: store.OAuthProviderChatGPT, ClientID: "oaiapp_synthetic", HostID: "synthetic-host", RefreshToken: "synthetic-refresh"},
	}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("DELETE", fmt.Sprintf("/channels/%d", c.ID), nil))
		result <- w
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("revoke did not start")
	}
	consolePluginToggle(t, h, "chatgpt", false)
	unblock()
	select {
	case w := <-result:
		if w.Code != 503 || strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("blocked local delete reported success: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoke did not drain")
	}
	if _, err = st.Channel(c.ID); err != nil {
		t.Fatalf("guarded delete unexpectedly removed the channel: %v", err)
	}
	// Retry is purely local while the capability is off.
	pluginHTTP(t, h, "DELETE", fmt.Sprintf("/channels/%d", c.ID), "", 200)
}

func TestPluginsDisableDuringRefreshKeepsIssuedToken(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	_, st := probeStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var discoveries atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			close(entered)
			<-release
			fmt.Fprintf(w, `{"token":"synthetic-new-token","expires_at":%d}`, time.Now().Add(time.Hour).Unix())
			return
		}
		discoveries.Add(1)
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer issuer.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c := oauthChannel(t, st, store.CredentialCopilotOAuth, store.OAuthProviderCopilot, issuer.URL, store.FormatOpenAIChatCompletions,
		store.OAuthCredential{GitHubToken: "synthetic-github", AccessToken: "synthetic-expired", ExpiresAt: time.Now().Add(-time.Hour)})
	svc := copilot.NewService(st, issuer.Client(), copilot.Endpoints{API: issuer.URL, ModelAPI: issuer.URL})
	m, err := plugins.NewInjected(st, svc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", fmt.Sprintf("/channels/%d/oauth/models", c.ID), nil))
		result <- w
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	consolePluginToggle(t, h, "copilot", false)
	unblock()
	select {
	case w := <-result:
		if w.Code != 503 || discoveries.Load() != 0 {
			t.Fatalf("next model request was not stopped: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight response did not drain")
	}
	fresh, err := st.Channel(c.ID)
	if err != nil || fresh.OAuth.AccessToken != "synthetic-new-token" {
		t.Fatalf("issued token discarded on disable: %v", err)
	}
	if m.Existing(store.CredentialCopilotOAuth).Copilot != svc {
		t.Fatal("console changed the shared service")
	}
}
