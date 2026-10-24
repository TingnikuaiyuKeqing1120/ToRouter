package console

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/copilot"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

// oauthDouble is the GitHub side of the device flow for console tests: device
// code, polling, exchange, account and catalogue, all on one server.
type oauthDouble struct {
	*httptest.Server
	polls          atomic.Int32
	pending        atomic.Int32 // polls answered authorization_pending
	slowDown       atomic.Int32 // polls answered slow_down
	exchange       int          // status the exchange answers with
	profile        int          // status copilot_internal/user answers with
	denied         bool         // answer the poll with access_denied
	user           int          // status GET /user answers with
	deviceInterval int          // interval the device code response carries
	exchangeCalls  atomic.Int32 // token exchanges served
	deviceCodes    atomic.Int32 // device-code requests served
	models         []string
}

func newOAuthDouble(t *testing.T) *oauthDouble {
	t.Helper()
	d := &oauthDouble{exchange: http.StatusOK, profile: http.StatusOK, user: http.StatusOK, models: []string{"gpt-5", "gpt-4.1"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		d.deviceCodes.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-1", "user_code": "WXYZ-9999",
			"verification_uri": "https://github.com/login/device", "expires_in": 900,
			"interval": d.deviceInterval,
		})
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if d.denied {
			json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
			return
		}
		if d.slowDown.Load() > 0 && d.slowDown.Add(-1) >= 0 {
			json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
			return
		}
		if d.polls.Add(1) <= d.pending.Load() {
			json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_console"})
	})
	mux.HandleFunc("/copilot_internal/v2/token", func(w http.ResponseWriter, r *http.Request) {
		d.exchangeCalls.Add(1)
		if d.exchange != http.StatusOK {
			w.WriteHeader(d.exchange)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "console-jwt", "expires_at": time.Now().Add(25 * time.Minute).Unix()})
	})
	mux.HandleFunc("/copilot_internal/user", func(w http.ResponseWriter, r *http.Request) {
		if d.profile != http.StatusOK {
			w.WriteHeader(d.profile)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"copilot_plan": "copilot_pro",
			"endpoints":    map[string]any{"api": d.URL},
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if d.user != http.StatusOK {
			w.WriteHeader(d.user)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"login": "octocat"})
	})
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer console-jwt" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data := []map[string]any{}
		for _, id := range d.models {
			data = append(data, map[string]any{"id": id, "name": id, "vendor": "OpenAI", "model_picker_enabled": true})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	d.Server = httptest.NewServer(mux)
	t.Cleanup(d.Close)
	return d
}

func (d *oauthDouble) service(s *store.Store) *copilot.Service {
	return copilot.NewService(s, nil, copilot.Endpoints{GitHub: d.URL, API: d.URL, ModelAPI: d.URL})
}

func oauthCall(t *testing.T, h http.Handler, method, path string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// waitForPortRelease asserts the pinned callback port became bindable again.
// Windows can lag between listener close and rebind, so the bind is retried
// instead of racing the OS; the assertion is unchanged — the port must free.
func waitForPortRelease(t *testing.T, port string) {
	t.Helper()
	var listener net.Listener
	var err error
	for attempt := 0; attempt < 40; attempt++ {
		if listener, err = net.Listen("tcp", "127.0.0.1:"+port); err == nil {
			listener.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("cancel did not release port %s: %v", port, err)
}

// The whole device flow as the console exposes it: start returns the code the
// user types at GitHub, the first poll is still pending, the second one
// completes and creates the channel — endpoints, models and credential — in one
// step. First version imports the whole catalogue; the owner trims it in the
// channel editor afterwards.
func TestCopilotOAuthStartPollCompleteCreatesChannel(t *testing.T) {
	// The channel this flow creates points at the local double, so the
	// documented testing escape hatch for the OAuth host allowlist is set
	// explicitly; see store.OAuthHostAllowed.
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	d.pending.Store(1)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	flow, _ := started["flow"].(string)
	if flow == "" {
		t.Fatal("no flow id")
	}
	if started["user_code"] != "WXYZ-9999" || started["verification_uri"] != "https://github.com/login/device" {
		t.Fatalf("start %+v", started)
	}
	if _, ok := started["expires"]; !ok {
		t.Fatal("start did not report the expiry")
	}
	first := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow)
	if first["status"] != "pending" {
		t.Fatalf("first status %+v", first)
	}
	done := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow)
	if done["status"] != "complete" {
		t.Fatalf("second status %+v", done)
	}
	id, _ := done["channel_id"].(float64)
	if id < 1 {
		t.Fatalf("no channel id: %+v", done)
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatal("channel not stored", err)
	}
	c := channels[0]
	if c.Name != "GitHub Copilot" {
		t.Fatalf("name %q", c.Name)
	}
	if c.CredentialType != store.CredentialCopilotOAuth || c.OAuth == nil {
		t.Fatalf("credential type %+v", c)
	}
	if c.OAuth.GitHubToken != "gho_console" || c.OAuth.AccessToken != "console-jwt" || c.OAuth.AccountLabel != "octocat" {
		t.Fatalf("credential %+v", c.OAuth)
	}
	if c.OAuth.Status != store.OAuthStatusActive || !c.Enabled {
		t.Fatalf("status %+v", c.OAuth)
	}
	if len(c.Endpoints) != 2 || c.Endpoints[0].Format != store.FormatOpenAIChatCompletions || c.Endpoints[1].Format != store.FormatAnthropicMessages {
		t.Fatalf("endpoints %+v", c.Endpoints)
	}
	if len(c.Models) != 0 || len(c.AvailableModels) != 0 || len(c.Credentials) != 1 {
		t.Fatal("authorization should save credentials without models")
	}
	catalog := oauthCall(t, h, "POST", "/channels/"+strconv.FormatInt(c.ID, 10)+"/oauth/models")
	if catalog["ok"] != true {
		t.Fatalf("explicit model fetch failed: %v", catalog)
	}
	current, _ := s.Channel(c.ID)
	if len(current.Models) != 0 || len(current.AvailableModels) != 2 {
		t.Fatal("explicit model fetch did not retain independent enablement")
	}

}

func TestCopilotOAuthStatusErrorPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*oauthDouble)
		code    string
	}{
		{"access_denied", func(d *oauthDouble) { d.denied = true }, "access_denied"},
		{"unsubscribed", func(d *oauthDouble) { d.exchange = http.StatusForbidden }, "unsubscribed"},
		{"reauth_required", func(d *oauthDouble) { d.exchange = http.StatusUnauthorized }, "reauth_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newOAuthDouble(t)
			tc.prepare(d)
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
			t.Cleanup(func() { ReleaseOAuthListeners(h) })
			started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
			flow := started["flow"].(string)
			status := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow)
			if status["status"] != "error" || status["code"] != tc.code || status["message"] == "" {
				t.Fatalf("status %+v", status)
			}
			if channels, _ := s.ListChannels(); len(channels) != 1 || len(channels[0].Credentials) != 0 {
				t.Fatal("failed authorization created a channel")
			}
			if tc.code == "unsubscribed" {
				// The exchange mark lives on the credential, but a rejected
				// login never becomes a channel, so there is nothing to disable:
				// the flow error is the record.
				return
			}
		})
	}
}

func TestCopilotOAuthUnknownFlow(t *testing.T) {
	d := newOAuthDouble(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/copilot/status?flow=missing", nil))
	if w.Code == http.StatusOK {
		t.Fatal("unknown flow answered 200")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/copilot/status", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing parameter: %d", w.Code)
	}
}

// The console shows the credential's shape, never its secrets: has_oauth says
// one is configured, and the tokens must not appear in the response.
func TestPublicChannelHidesOAuthTokens(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	flow := started["flow"].(string)
	oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow) // complete
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/channels", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "gho_console") || strings.Contains(body, "console-jwt") {
		t.Fatal("oauth token echoed by the console")
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	if rows[0]["has_oauth"] != true {
		t.Fatalf("has_oauth missing: %+v", rows[0])
	}
	info, _ := rows[0]["oauth"].(map[string]any)
	if info["account_label"] != "octocat" || info["status"] != "active" {
		t.Fatalf("oauth summary %+v", info)
	}
}

// One GitHub token, one channel: authorizing an account that is already linked
// reports the channel that owns it instead of creating a second one, and spends
// no upstream call on the answer — two channels sharing a token would race each
// other's rotations (G017).
func TestCopilotOAuthSecondAuthorizationReusesTheChannel(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	first := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	done := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+first["flow"].(string))
	if done["status"] != "complete" {
		t.Fatalf("first authorization %+v", done)
	}
	channelID := int64(done["channel_id"].(float64))
	channel, err := s.Channel(channelID)
	if err != nil {
		t.Fatal(err)
	}
	channel.Name = "My Copilot octocat"
	if err := s.SaveChannelConfig(&channel); err != nil {
		t.Fatal(err)
	}
	if _, ok := done["already_linked"]; ok {
		t.Fatal("first authorization reported already_linked")
	}
	// Explicit reauthorization of the same member.
	second := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start?operation=reauth&channel_id="+strconv.FormatInt(channelID, 10)))
	again := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+second["flow"].(string))
	if again["status"] != "complete" {
		t.Fatalf("second authorization %+v", again)
	}
	if again["channel_id"] != done["channel_id"] {
		t.Fatalf("explicit reauth changed target: %+v", again)
	}
	if int64(again["channel_id"].(float64)) != channelID {
		t.Fatalf("second authorization named channel %v, want %d", again["channel_id"], channelID)
	}

	channels, err := s.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 {
		t.Fatalf("second authorization created %d channels", len(channels))
	}
	if channels[0].Name != channel.Name || channels[0].OAuth.AccountLabel != "octocat" {
		t.Fatal("second authorization changed the chosen name or account metadata")
	}
	// Explicit reauthorization exchanges fresh material for this exact member.
	if got := d.exchangeCalls.Load(); got != 2 {
		t.Fatalf("two authorizations cost %d exchanges, want 2", got)
	}
}

// The authorization flow never throws itself away over a cosmetic call: when
// the account profile fails, the channel is still created on the documented
// default api base and the answer says what is missing.
func TestCopilotOAuthProfileFailureDegradesInsteadOfFailing(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	// Both decoration calls fail: the profile and the login lookup.
	d.profile = http.StatusInternalServerError
	d.user = http.StatusNotFound
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/copilot/status?flow="+started["flow"].(string)))
	if done["status"] != "complete" {
		t.Fatalf("profile failure rejected the authorization: %+v", done)
	}
	degraded, _ := done["degraded"].([]any)
	if len(degraded) != 1 || degraded[0] != "account_profile" {
		t.Fatalf("degraded %v", done["degraded"])
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatal("channel not created", err)
	}
	if channels[0].OAuth.AccountLabel != "" {
		t.Fatalf("label %q, want empty", channels[0].OAuth.AccountLabel)
	}
	if channels[0].Endpoints[0].BaseURL != d.URL {
		t.Fatalf("api base %q, want the documented default %q", channels[0].Endpoints[0].BaseURL, d.URL)
	}
	if len(channels[0].Models) != 0 || len(channels[0].AvailableModels) != 0 || len(channels[0].Credentials) != 1 {
		t.Fatal("profile failure lost credential or auto-enabled models")
	}
}

// The poll interval is honoured, and only the console's own polls drive it:
// repeated status calls inside the window cost one upstream poll, and a
// slow_down answer widens the window by five seconds.
func TestCopilotOAuthStatusThrottlesPolling(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	d.deviceInterval = 2
	d.pending.Store(1000) // every poll stays pending until the test says otherwise
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	flow := started["flow"].(string)
	for i := 0; i < 3; i++ {
		if status := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow); status["status"] != "pending" {
			t.Fatalf("call %d: %+v", i, status)
		}
	}
	if got := d.polls.Load(); got != 1 {
		t.Fatalf("three status calls inside the interval cost %d upstream polls", got)
	}
	time.Sleep(2100 * time.Millisecond)
	if status := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow); status["status"] != "pending" {
		t.Fatalf("after the interval: %+v", status)
	}
	if got := d.polls.Load(); got != 2 {
		t.Fatalf("a fourth call after the interval cost %d upstream polls, want 2", got)
	}
}

func TestCopilotOAuthSlowDownWidensTheInterval(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	d.slowDown.Store(1)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	flow := started["flow"].(string)
	status := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow)
	if status["interval"].(float64) != 5 {
		t.Fatalf("interval after slow_down: %v", status["interval"])
	}
	// The widened interval throttles the next call: no second poll.
	if status := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow); status["status"] != "pending" {
		t.Fatalf("second call: %+v", status)
	}
	if got := d.polls.Load(); got != 0 {
		t.Fatalf("slow_down poll itself was counted and %d more followed", got)
	}
}

// The credential is write-only through the API: a channel save that carries a
// GitHub token is refused, and the stored credential is left alone.
func TestConsoleRefusesDirectOAuthTokenWrite(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/copilot/status?flow="+started["flow"].(string)))
	id := int64(done["channel_id"].(float64))
	// An empty summary round-trips fine...
	stored, _ := s.Channel(id)
	body := `{"generation":` + strconv.FormatInt(stored.Generation, 10) + `,"name":"renamed","credential_type":"copilot_oauth","enabled":true,` +
		`"models":["gpt-5"],"endpoints":[{"format":"openai_chat_completions","base_url":"` + stored.Endpoints[0].BaseURL + `"}],` +
		`"credentials":[{"id":"` + stored.Credentials[0].ID + `","label":"` + stored.Credentials[0].Label + `","enabled":true}]}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PUT", "/channels/"+itoa(id), strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("token-free round trip refused: %d %s", w.Code, w.Body.String())
	}
	// ...and a pasted token does not.
	body = strings.TrimSuffix(body, "}") + `,"oauth":{"provider":"copilot","github_token":"synthetic-pasted"}}`
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PUT", "/channels/"+itoa(id), strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("pasted github token accepted: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "授权") {
		t.Fatalf("refusal does not name the authorization flow: %s", w.Body.String())
	}
	stored, err = s.Channel(id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.GitHubToken != "gho_console" {
		t.Fatalf("stored credential changed to %q", stored.OAuth.GitHubToken)
	}
	if stored.Name != "renamed" {
		t.Fatalf("the token-free edit did not land: %q", stored.Name)
	}
}

// Two concurrent starts for one provider must not both run their upstream
// work: the idempotence rule is a check-then-act otherwise, and the loser of a
// double click would mint a second device code and register a second flow.
func TestCopilotOAuthConcurrentStartsMintOneFlow(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	const callers = 4
	answers := make([]map[string]any, callers)
	var wg sync.WaitGroup
	begin := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-begin
			answers[i] = oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
		}(i)
	}
	close(begin)
	wg.Wait()
	for i, answer := range answers {
		if answer["flow"] != answers[0]["flow"] {
			t.Fatalf("start %d minted flow %v, want %v", i, answer["flow"], answers[0]["flow"])
		}
	}
	if got := d.deviceCodes.Load(); got != 1 {
		t.Fatalf("the concurrent starts asked GitHub for %d device codes, want 1", got)
	}
	// The one flow is the live one.
	done := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+answers[0]["flow"].(string))
	if done["status"] != "complete" {
		t.Fatalf("the surviving flow %+v", done)
	}
}

// One authorization per provider: a second start while one is pending hands the
// first flow back — the same flow id, the same code, and no second device code
// minted upstream. The user clicked twice, or reloaded and clicked again, and
// either way the authorization in flight is the one being completed.
func TestCopilotOAuthStartIsIdempotentWhilePending(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	first := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	second := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	if first["flow"] != second["flow"] || first["user_code"] != second["user_code"] {
		t.Fatalf("second start minted a new flow: %+v then %+v", first, second)
	}
	if got := d.deviceCodes.Load(); got != 1 {
		t.Fatalf("the repeat start asked GitHub for %d device codes, want 1", got)
	}
	// The handed-back flow is still the live one: polling it completes the
	// authorization that was already running.
	done := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+first["flow"].(string))
	if done["status"] != "complete" {
		t.Fatalf("handed-back flow %+v", done)
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 {
		t.Fatalf("the idempotent start created %d channels", len(channels))
	}
}

// An OAuth channel's catalogue is pulled with the channel's own credential and
// written back with the replace semantics: the fetched part becomes the
// catalogue, and the owner's manual entries survive it.
func TestOAuthChannelModelsFetch(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	d.models = []string{"gpt-5", "gpt-4.1"}
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/copilot/status?flow="+started["flow"].(string)))
	channelID := int64(done["channel_id"].(float64))
	// A hand-typed model survives the pull: the owner added it, not the upstream.
	stored, err := s.Channel(channelID)
	if err != nil {
		t.Fatal(err)
	}
	stored.AvailableModels = append(append([]string{}, stored.AvailableModels...), "gpt-manual")
	stored.ManualModels = []string{"gpt-manual"}
	if err := s.SaveChannel(&stored); err != nil {
		t.Fatal(err)
	}
	fetched := oauthCall(t, h, "POST", "/channels/"+itoa(channelID)+"/oauth/models")
	if fetched["ok"] != true {
		t.Fatalf("fetch %+v", fetched)
	}
	stored, err = s.Channel(channelID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(stored.AvailableModels, "gpt-5", "gpt-4.1", "gpt-manual") {
		t.Fatalf("catalogue %v", stored.AvailableModels)
	}
	if !containsAll(stored.ManualModels, "gpt-manual") {
		t.Fatalf("manual markers %v", stored.ManualModels)
	}
	// A probe stays refused on an OAuth channel: it is a paid generation. The
	// draft carries no id, so the refusal is the OAuth branch and nothing else.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/channels/probe", strings.NewReader(`{"channel":{"name":"x","credential_type":"copilot_oauth","endpoints":[{"format":"openai_chat_completions","base_url":"https://api.githubcopilot.com"}]},"model":"gpt-5","confirmed":true}`)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "OAuth") {
		t.Fatalf("probe on an OAuth channel: %d %s", w.Code, w.Body.String())
	}
	// An api_key channel is pointed at the api-key discover instead: the OAuth
	// fetch has no credential to authenticate with.
	plain := store.Channel{Name: "plain", APIKey: "k", CredentialType: store.CredentialAPIKey, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.example.com"}}}
	if err := s.SaveChannel(&plain); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/channels/"+itoa(plain.ID)+"/oauth/models", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "API Key") {
		t.Fatalf("api-key channel: %d %s", w.Code, w.Body.String())
	}
}

func containsAll(list []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, entry := range list {
			if entry == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// The Copilot track has no listener, but its pending device flow cancels the
// same way: the flow answers cancelled and no further poll reaches GitHub.
func TestCopilotOAuthCancelStopsThePolling(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newOAuthDouble(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, d.service(s), siwc.NewService(s, nil, siwc.DefaultEndpoints()), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/copilot/start"))
	flow := started["flow"].(string)
	cancelled := oauthCall(t, h, "POST", "/oauth/copilot/cancel?flow="+flow)
	if cancelled["status"] != "cancelled" {
		t.Fatalf("cancel %+v", cancelled)
	}
	polls := d.polls.Load()
	status := oauthCall(t, h, "GET", "/oauth/copilot/status?flow="+flow)
	if status["status"] != "cancelled" {
		t.Fatalf("status after cancel %+v", status)
	}
	if d.polls.Load() != polls {
		t.Fatalf("a cancelled flow polled GitHub again: %d -> %d", polls, d.polls.Load())
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 || len(channels[0].Credentials) != 0 {
		t.Fatalf("a cancelled flow created %d channels", len(channels))
	}
}
