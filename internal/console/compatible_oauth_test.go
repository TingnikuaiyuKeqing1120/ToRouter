package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/compatprofile"
	"torouter/internal/pluginhost"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

func TestCompatibleConsoleInstallScopeStartCancel(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	_, st := probeStore(t)
	install := pluginhost.NewInstallation(t.TempDir())
	m, err := plugins.New(st, install)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	for _, remote := range []string{"192.0.2.1:12345", "127.0.0.1:12345"} {
		req := httptest.NewRequest("POST", "http://127.0.0.1:17878/plugins/compatible/install", strings.NewReader(`{}`))
		req.RemoteAddr = remote
		if remote == "127.0.0.1:12345" {
			req.Header.Set("Origin", "https://example.invalid")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 403 || install.Status().Installed {
			t.Fatal("nonlocal/cross-origin install accepted")
		}
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1:17878/plugins/compatible/install", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !install.Status().Installed {
		t.Fatalf("local install: %d", w.Code)
	}
	consolePluginToggle(t, h, "qoder", true)
	start := pluginHTTP(t, h, "POST", authorizationFixturePath(t, h, "/oauth/qoder/start"), "", 200)
	var payload map[string]any
	if err = json.Unmarshal(start.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(payload["authorization_url"].(string), "https://qoder.com/device/selectAccounts?") || payload["redirect_uri"] != nil {
		t.Fatal("wrong device authorization UI contract")
	}
	// Starting only builds the independent PKCE URL; cancelling prevents the
	// first poll. This test deliberately performs no vendor request.
	pluginHTTP(t, h, "POST", "/oauth/qoder/cancel?flow="+payload["flow"].(string), "", 200)
	rows, _ := st.ListChannels()
	if len(rows) != 1 || len(rows[0].Credentials) != 0 {
		t.Fatal("start/cancel changed the empty target pool")
	}
}

func TestCompatibleConsentRequiresAuthorizedPollAndDoesNotExposeState(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "revoked"}[revoke], func(t *testing.T) {
			_, st := probeStore(t)
			profile, _ := compatprofile.Lookup(compatprofile.QoderCredential)
			o := &oauthTools{store: st, compatible: pluginhost.NewService(st, nil, profile), flows: map[string]*oauthFlow{}}
			state := `{"type":"oauth","uid":"synthetic-account","access":"synthetic-sensitive-access","refresh":"synthetic-refresh","expires":1900000000000}`
			credential, err := store.CompatibleOAuthCredential(profile.CredentialType, state)
			if err != nil {
				t.Fatal(err)
			}
			f := &oauthFlow{id: "synthetic-flow", provider: profile.CredentialType, state: flowPending, expiresAt: time.Now().Add(time.Minute),
				compatibleAuth: &pluginhost.Authorization{URL: profile.Web + "/device/selectAccounts"}, compatibleResult: make(chan compatibleConsentResult, 1)}
			f.compatibleResult <- compatibleConsentResult{oauth: credential}
			o.flows[f.id] = f
			ctx := accessguard.With(context.Background(), func() error {
				if revoke {
					return errors.New("revoked")
				}
				return nil
			})
			result, err := o.statusCompatibleCore(ctx, f.id, "")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "synthetic-sensitive") || strings.Contains(string(raw), "plugin_state") {
				t.Fatal("private consent escaped")
			}
			rows, _ := st.ListChannels()
			if revoke && len(rows) != 0 || !revoke && len(rows) != 1 {
				t.Fatal("authorization commit boundary failed")
			}
			if len(rows) > 0 {
				public, _ := json.Marshal(publicChannel(rows[0]))
				if strings.Contains(string(public), "plugin_state") || strings.Contains(string(public), "synthetic-sensitive") {
					t.Fatal("ordinary channel DTO exposed private plugin material")
				}
			}
		})
	}
}

func TestFeaturedProviderRoutingAndVerificationCode(t *testing.T) {
	for _, profile := range compatprofile.Profiles() {
		for _, name := range []string{profile.ID, profile.CredentialType} {
			got, err := oauthProvider(name)
			if err != nil || got != profile.CredentialType {
				t.Fatalf("provider alias %s: %s %v", name, got, err)
			}
		}
	}
	f := &oauthFlow{id: "synthetic-flow", expiresAt: time.Now().Add(time.Minute),
		compatibleAuth: &pluginhost.Authorization{URL: "https://account.minimax.cn/login", UserCode: "ABCD-1234", Instructions: "private plugin instructions are not a public contract"}}
	out := startPayloadCompatible(f)
	if out["user_code"] != "ABCD-1234" || out["verification_uri"] != f.compatibleAuth.URL {
		t.Fatal("verification code cannot be copied/opened by existing UI")
	}
	if strings.Contains(out["instructions"].(string), "private plugin") {
		t.Fatal("arbitrary plugin instructions escaped to UI")
	}
}
