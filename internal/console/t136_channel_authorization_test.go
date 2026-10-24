package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"torouter/internal/codex"
	"torouter/internal/copilot"
	"torouter/internal/nativeoauth"
	"torouter/internal/plugins"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

func t136HTTP(t *testing.T, h http.Handler, method, path, body string, status int) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	if w.Code != status {
		t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestT136NativePendingReauthRejectsReplacementMember(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	driver := &nativeFlowDriver{definition: nativeoauth.Definition{ID: "claude", Name: "Synthetic Claude", Provider: store.OAuthProviderClaude, CredentialType: store.CredentialClaudeOAuth, CallbackHost: "127.0.0.1", CallbackPath: "/callback", Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: "https://api.anthropic.com"}}}}
	service := nativeoauth.NewService(st, driver)
	manager, err := plugins.NewWithNativeServices(st, map[string]*nativeoauth.Service{store.CredentialClaudeOAuth: service})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := manager.Snapshot()
	if _, err = manager.SetCapability(context.Background(), "claude", "connect", true, list.Version); err != nil {
		t.Fatal(err)
	}
	source, _ := store.BuiltinChannel("Source", store.CredentialClaudeOAuth)
	material := store.OAuthCredential{Provider: store.OAuthProviderClaude, ClientID: "fixture-client", Account: "fixture-account", Subject: "fixture-account", DeviceID: strings.Repeat("a", 64), RefreshToken: "synthetic-refresh", AccessToken: "synthetic-before", ExpiresAt: time.Now().Add(time.Hour)}
	source.Credentials = []store.CredentialItem{{ID: "original", Enabled: true, OAuth: &material}}
	if err = st.SaveChannel(&source); err != nil {
		t.Fatal(err)
	}
	target, _ := store.BuiltinChannel("Target", store.CredentialClaudeOAuth)
	if err = st.SaveChannel(&target); err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, manager)
	defer ReleaseOAuthListeners(h)
	started := t136HTTP(t, h, "POST", fmt.Sprintf("/oauth/claude/start?operation=reauth&method=browser&channel_id=%d&credential_id=original", source.ID), "", 200)
	if _, _, err = st.MoveCredentials(source.ID, target.ID, []string{"original"}, source.Generation, target.Generation, nil); err != nil {
		t.Fatal(err)
	}
	// Reimporting the same account into the source creates another member.
	source, _ = st.Channel(source.ID)
	replacement := material
	replacement.AccessToken = "synthetic-replacement"
	source.Credentials = []store.CredentialItem{{ID: "replacement", Enabled: true, OAuth: &replacement}}
	if err = st.SaveChannel(&source); err != nil {
		t.Fatal(err)
	}
	callback := started["redirect_uri"].(string) + "?code=fixture-code&state=fixture-state"
	result := t136HTTP(t, h, "GET", "/oauth/claude/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(callback), "", 200)
	if result["status"] != "error" {
		t.Fatal("old callback authorized a replacement member", result)
	}
	live, _ := st.Channel(source.ID)
	if live.Credentials[0].ID != "replacement" || live.Credentials[0].OAuth.AccessToken != "synthetic-replacement" {
		t.Fatal("old callback overwrote replacement")
	}
}

func TestT136ConcurrentStartsKeepExactCredentialTarget(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		json.NewEncoder(w).Encode(map[string]any{"device_code": "synthetic-device", "user_code": "SYNTHETIC", "verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": 1})
	}))
	defer up.Close()
	c, _ := store.BuiltinChannel("Concurrent", store.CredentialCopilotOAuth)
	for _, id := range []string{"one", "two"} {
		c.Credentials = append(c.Credentials, store.CredentialItem{ID: id, Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCopilot, Account: id, GitHubToken: "synthetic-github-" + id}})
	}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	o := &oauthTools{store: st, copilot: copilot.NewService(st, up.Client(), copilot.Endpoints{GitHub: up.URL}), flows: map[string]*oauthFlow{}}
	type answer struct {
		out any
		err error
	}
	first, second := make(chan answer, 1), make(chan answer, 1)
	go func() {
		out, e := o.ManagementAuthorize(context.Background(), "copilot", "reauth", "device", c.ID, "one")
		first <- answer{out, e}
	}()
	<-entered
	// Wait until the second request queues behind the provider start lock.
	go func() {
		// Enter the core with a target already checked before the first flow
		// was registered, reproducing the disputed check/start interleaving.
		target := store.PluginConsentTarget{ChannelID: c.ID, CredentialID: "two", Version: store.AccountOAuthIdentity(c.CredentialType, c.Credentials[1].OAuth)}
		ctx := context.WithValue(context.Background(), accountConsentKey{}, target)
		out, e := o.startCopilotCore(ctx)
		second <- answer{out, e}
	}()
	select {
	case got := <-second:
		close(release)
		t.Fatal("second request did not wait for pending start", got.err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	a, b := <-first, <-second
	if a.err != nil {
		t.Fatal(a.err)
	}
	var conflict *ManagementToolError
	if !errors.As(b.err, &conflict) || conflict.Status != 409 {
		t.Fatal("different credential reused the first flow", b.out, b.err)
	}
	again, e := o.ManagementAuthorize(context.Background(), "copilot", "reauth", "device", c.ID, "one")
	if e != nil || asObject(again)["flow"] != asObject(a.out)["flow"] {
		t.Fatal("same target lost pending idempotence", e)
	}
}

func TestT136HTTPCreateDefaultEnabledEmptyPoolsAndMove(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := New(st, "http://127.0.0.1:17878", Hooks{})
	first := t136HTTP(t, h, "POST", "/channels", `{"name":"First","credential_type":"codex_oauth"}`, 200)
	second := t136HTTP(t, h, "POST", "/channels", `{"name":"Second","credential_type":"codex_oauth","enabled":true}`, 200)
	if first["enabled"] != true || second["enabled"] != true {
		t.Fatal("empty pool defaults disabled")
	}
	a, _ := st.Channel(int64(first["id"].(float64)))
	b, _ := st.Channel(int64(second["id"].(float64)))
	a, _, err = st.CommitAccountConsent(store.CredentialCodexOAuth, store.OAuthCredential{Provider: store.OAuthProviderCodex, Account: "synthetic-account", ClientID: "synthetic-client", AccessToken: "synthetic-private-access"}, store.PluginConsentTarget{ChannelID: a.ID, Version: store.CatalogueSourceVersion(a)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"target_channel_id":%d,"credential_ids":[%q],"expected_generation":%d,"expected_target_generation":%d}`, b.ID, a.Credentials[0].ID, a.Generation, b.Generation)
	out := t136HTTP(t, h, "POST", fmt.Sprintf("/channels/%d/credentials/move", a.ID), body, 200)
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "synthetic-private-access") {
		t.Fatal("move leaked token")
	}
	a, _ = st.Channel(a.ID)
	b, _ = st.Channel(b.ID)
	if len(a.Credentials) != 0 || len(b.Credentials) != 1 || !a.Enabled {
		t.Fatal("HTTP move lost empty source or member")
	}
	t136HTTP(t, h, "POST", fmt.Sprintf("/channels/%d/credentials/move", a.ID), body, 409)
	foreign := t136HTTP(t, h, "POST", "/channels", `{"name":"Foreign type","credential_type":"claude_oauth"}`, 200)
	wrong := fmt.Sprintf(`{"target_channel_id":%.0f,"credential_ids":[%q],"expected_generation":%d,"expected_target_generation":%.0f}`, foreign["id"], b.Credentials[0].ID, b.Generation, foreign["generation"])
	t136HTTP(t, h, "POST", fmt.Sprintf("/channels/%d/credentials/move", b.ID), wrong, 400)
	live, _ := st.Channel(b.ID)
	if len(live.Credentials) != 1 {
		t.Fatal("invalid move lost source member")
	}
	// Removing the last member retains the enabled provider configuration.
	live.Credentials = []store.CredentialItem{}
	live.OAuth = nil
	live.APIKey = ""
	draft, _ := json.Marshal(live)
	var fields map[string]any
	_ = json.Unmarshal(draft, &fields)
	fields["credentials"] = []any{}
	draft, _ = json.Marshal(fields)
	t136HTTP(t, h, "PUT", fmt.Sprintf("/channels/%d", b.ID), string(draft), 200)
	live, _ = st.Channel(b.ID)
	if len(live.Credentials) != 0 || live.OAuth != nil || !live.Enabled {
		t.Fatal("last member removal recreated or disabled pool")
	}
}

func TestT136CopilotAppendsToSelectedPoolAndRejectsCrossPoolOverwrite(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := newOAuthDouble(t)
	h := NewWithOAuth(st, "http://127.0.0.1:17878", Hooks{}, d.service(st), siwc.NewService(st, nil, siwc.DefaultEndpoints()), codex.NewService(st, nil, codex.DefaultEndpoints()))
	defer ReleaseOAuthListeners(h)
	first := t136HTTP(t, h, "POST", "/channels", `{"name":"Selected","credential_type":"copilot_oauth"}`, 200)
	second := t136HTTP(t, h, "POST", "/channels", `{"name":"Other","credential_type":"copilot_oauth"}`, 200)
	authorize := func(id int64, operation, cid string) map[string]any {
		t.Helper()
		started := t136HTTP(t, h, "POST", fmt.Sprintf("/oauth/copilot/start?operation=%s&method=device&channel_id=%d&credential_id=%s", operation, id, cid), "", 200)
		return t136HTTP(t, h, "GET", "/oauth/copilot/status?flow="+started["flow"].(string), "", 200)
	}
	id := int64(first["id"].(float64))
	done := authorize(id, "add_credential", "")
	if done["status"] != "complete" {
		t.Fatal("authorization failed", done)
	}
	c, _ := st.Channel(id)
	if c.Name != "Selected" || len(c.Credentials) != 1 || len(c.Models) != 0 {
		t.Fatal("authorization overwrote channel configuration")
	}
	token := c.Credentials[0].OAuth.AccessToken
	duplicate := authorize(int64(second["id"].(float64)), "add_credential", "")
	if duplicate["status"] != "error" || duplicate["existing_channel_id"] != float64(id) || duplicate["existing_credential_id"] != c.Credentials[0].ID {
		t.Fatal("other pool silently overwritten", duplicate)
	}
	live, _ := st.Channel(id)
	if live.Credentials[0].OAuth.AccessToken != token {
		t.Fatal("duplicate changed original owner")
	}
	reauth := authorize(id, "reauth", c.Credentials[0].ID)
	if reauth["status"] != "complete" {
		t.Fatal("exact reauth failed", reauth)
	}
	t136HTTP(t, h, "POST", fmt.Sprintf("/oauth/copilot/start?operation=reauth&method=device&channel_id=%d", id), "", 400)
	t136HTTP(t, h, "POST", "/oauth/copilot/start?mode=add", "", 400)
}
