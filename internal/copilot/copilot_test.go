package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub is the GitHub side of the Copilot track: device code, device
// polling, the token exchange and the account lookups. Everything tests need
// lives here, so no test ever reaches github.com.
type fakeGitHub struct {
	*httptest.Server
	devicePolls atomic.Int32
	exchanges   atomic.Int32
	pending     atomic.Int32 // polls answered with authorization_pending
	exchange    int          // status the token exchange answers with
	profile     int          // status copilot_internal/user answers with
	user        string       // login for GET /user, empty answers 404
	apiBase     string       // endpoints.api value for copilot_internal/user
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{exchange: http.StatusOK, profile: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("device code: %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("client_id"); got != ClientID() {
			t.Errorf("client_id %q", got)
		}
		if got := r.PostForm.Get("scope"); got != Scope {
			t.Errorf("scope %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent() {
			t.Errorf("user agent %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-123", "user_code": "ABCD-1234",
			"verification_uri": "https://github.com/login/device",
			"expires_in":       900, "interval": 0,
		})
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.PostForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("grant_type %q", got)
		}
		if got := r.Form.Get("client_id"); got != ClientID() {
			t.Errorf("poll client_id %q", got)
		}
		if got := r.PostForm.Get("device_code"); got != "dc-123" {
			t.Errorf("device_code %q", got)
		}
		if f.devicePolls.Add(1) <= f.pending.Load() {
			json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_authorized", "token_type": "bearer", "scope": "read:user"})
	})
	mux.HandleFunc("/copilot_internal/v2/token", func(w http.ResponseWriter, r *http.Request) {
		f.exchanges.Add(1)
		if f.exchange != http.StatusOK {
			w.WriteHeader(f.exchange)
			return
		}
		if got := r.Header.Get("Authorization"); got != "token gho_authorized" {
			t.Errorf("exchange authorization %q", got)
		}
		if got := r.Header.Get("Editor-Version"); got != EditorVersion() {
			t.Errorf("editor-version %q", got)
		}
		if got := r.Header.Get("Editor-Plugin-Version"); got != EditorPluginVersion() {
			t.Errorf("editor-plugin-version %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent() {
			t.Errorf("user agent %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"token": "copilot-jwt", "expires_at": time.Now().Add(25 * time.Minute).Unix(), "refresh_in": 1500})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if f.user == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "token gho_authorized" {
			t.Errorf("user authorization %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"login": f.user})
	})
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer copilot-jwt" {
			t.Errorf("catalog authorization %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "gpt-5", "model_picker_enabled": true},
			{"id": "hidden-model", "model_picker_enabled": false},
		}})
	})
	mux.HandleFunc("/copilot_internal/user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token gho_authorized" {
			t.Errorf("profile authorization %q", got)
		}
		if f.profile != http.StatusOK {
			w.WriteHeader(f.profile)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"copilot_plan": "copilot_pro",
			"endpoints":    map[string]any{"api": f.apiBase},
		})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) endpoints() Endpoints {
	return Endpoints{GitHub: f.URL, API: f.URL, ModelAPI: f.URL}
}

func testClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

// The whole login chain, one verbatim request at a time: device code, two
// polls (one pending), the copilot exchange and the model catalogue.
func TestDeviceFlowExchangeAndModels(t *testing.T) {
	f := newFakeGitHub(t)
	f.pending.Store(1)
	f.user = "octocat"
	f.apiBase = "https://api.githubcopilot.com"
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("catalog path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer copilot-jwt" {
			t.Errorf("catalog authorization %q", got)
		}
		if got := r.Header.Get("copilot-integration-id"); got != IntegrationID {
			t.Errorf("copilot-integration-id %q", got)
		}
		if got := r.Header.Get("x-github-api-version"); got != APIVersion {
			t.Errorf("x-github-api-version %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "gpt-5", "name": "GPT-5", "vendor": "OpenAI", "model_picker_enabled": true},
			{"id": "gpt-4.1", "name": "GPT-4.1", "vendor": "OpenAI", "model_picker_enabled": true},
			{"id": "hidden-model", "name": "Hidden", "vendor": "OpenAI", "model_picker_enabled": false},
		}})
	}))
	defer catalog.Close()
	ctx := context.Background()
	flow, err := StartDeviceFlow(ctx, testClient(), f.endpoints())
	if err != nil {
		t.Fatal(err)
	}
	if flow.UserCode != "ABCD-1234" || flow.VerificationURI != "https://github.com/login/device" || flow.DeviceCode != "dc-123" || flow.ID == "" {
		t.Fatalf("device flow %+v", flow)
	}
	if !flow.ExpiresAt.After(time.Now()) {
		t.Fatal("device code already expired")
	}
	first := PollDeviceFlow(ctx, testClient(), f.endpoints(), flow)
	if first.Verdict != PollPending {
		t.Fatalf("first poll %v %v", first.Verdict, first.Err)
	}
	second := PollDeviceFlow(ctx, testClient(), f.endpoints(), flow)
	if second.Verdict != PollComplete || second.GitHubToken != "gho_authorized" {
		t.Fatalf("second poll %v %v", second.Verdict, second.Err)
	}
	token, err := ExchangeToken(ctx, testClient(), f.endpoints(), second.GitHubToken)
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "copilot-jwt" || time.Until(token.ExpiresAt) < 20*time.Minute {
		t.Fatalf("exchange %+v", token)
	}
	profile, err := FetchCopilotUser(ctx, testClient(), f.endpoints(), second.GitHubToken)
	if err != nil {
		t.Fatal(err)
	}
	if profile.CopilotPlan != "copilot_pro" || profile.Endpoints.API != f.apiBase {
		t.Fatalf("profile %+v", profile)
	}
	label, err := GitHubLogin(ctx, testClient(), f.endpoints(), second.GitHubToken)
	if err != nil || label != "octocat" {
		t.Fatalf("login %q %v", label, err)
	}
	models, _, err := DiscoverModels(ctx, testClient(), catalog.URL, token.Token)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "gpt-5" || models[1] != "gpt-4.1" {
		t.Fatalf("models %v", models)
	}
}

// The three terminal device-flow errors and slow_down: each one must be a
// distinguishable verdict, not a generic failure.
func TestDeviceFlowErrorBranches(t *testing.T) {
	for _, tc := range []struct {
		answer map[string]any
		want   PollVerdict
		code   string
	}{
		{map[string]any{"error": "authorization_pending"}, PollPending, ""},
		{map[string]any{"error": "slow_down"}, PollSlowDown, ""},
		{map[string]any{"error": "expired_token"}, PollFailed, "expired_token"},
		{map[string]any{"error": "access_denied"}, PollFailed, "access_denied"},
	} {
		github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(tc.answer)
		}))
		eps := Endpoints{GitHub: github.URL, API: github.URL, ModelAPI: github.URL}
		flow := DeviceFlow{DeviceCode: "dc", Interval: 5, ExpiresAt: time.Now().Add(time.Minute)}
		got := PollDeviceFlow(context.Background(), testClient(), eps, flow)
		if got.Verdict != tc.want || got.Code != tc.code {
			t.Fatalf("%v: got %v/%q want %v/%q", tc.answer, got.Verdict, got.Code, tc.want, tc.code)
		}
		github.Close()
	}
}

// The exchange statuses that describe the account rather than the wire: 403 is
// a missing Copilot subscription, 401 a rejected GitHub token.
func TestExchangeReportsSubscriptionAndAuthFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusForbidden, ErrUnsubscribed},
		{http.StatusUnauthorized, ErrReauthRequired},
	} {
		f := newFakeGitHub(t)
		f.exchange = tc.status
		_, err := ExchangeToken(context.Background(), testClient(), f.endpoints(), "gho_authorized")
		if !errors.Is(err, tc.want) {
			t.Fatalf("status %d: %v", tc.status, err)
		}
	}
}

func TestMalformedEndpointFailsBeforeSending(t *testing.T) {
	// The endpoints struct is the only injection surface the service has; a
	// caller that points one of them at a value that is not even a URL gets a
	// build-time-shaped failure rather than a credential left on a stranger's
	// server.
	f := newFakeGitHub(t)
	bad := f.endpoints()
	bad.GitHub = "://not a url"
	if _, err := StartDeviceFlow(context.Background(), testClient(), bad); err == nil {
		t.Fatal("malformed device endpoint accepted")
	}
	bad.API = "://not a url"
	if _, err := ExchangeToken(context.Background(), testClient(), bad, "gho_authorized"); err == nil {
		t.Fatal("malformed exchange endpoint accepted")
	}
	if _, _, err := DiscoverModels(context.Background(), testClient(), "://not a url", "copilot-jwt"); err == nil {
		t.Fatal("malformed catalogue endpoint accepted")
	}
}
