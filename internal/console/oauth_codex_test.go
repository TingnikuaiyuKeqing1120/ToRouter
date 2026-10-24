package console

import (
	"encoding/base64"
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

	"torouter/internal/codex"
	"torouter/internal/store"
)

// codexDouble is the issuer and the backend for the console flow.
type codexDouble struct {
	*httptest.Server
	deviceEnabled bool
	polls         atomic.Int32
	exchanges     atomic.Int32
	models        []string
	// account is the account the minted id_token names: a re-authorization can
	// be completed as somebody else, and the console must notice.
	account   string
	tokenHook func(http.ResponseWriter, *http.Request) bool
}

func newCodexDouble(t *testing.T) *codexDouble {
	t.Helper()
	d := &codexDouble{deviceEnabled: true, models: []string{"gpt-5-codex", "gpt-5"}, account: "acct-console"}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if d.tokenHook != nil && d.tokenHook(w, r) {
				return
			}
			d.exchanges.Add(1)
			idToken := idTokenFor(t, d.account, d.account+"@example.test")
			json.NewEncoder(w).Encode(codex.TokenResponse{
				AccessToken: "at-console", RefreshToken: "rt-console", IDToken: idToken,
				ExpiresIn: 3600, Scope: codex.ScopeAuthorize,
			})
		case "/api/accounts/deviceauth/usercode":
			if !d.deviceEnabled {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "da-1", "user_code": "CODE-ZZ", "interval": 0, "expires_in": 900})
		case "/api/accounts/deviceauth/token":
			json.NewEncoder(w).Encode(map[string]any{"authorization_code": "device-code", "code_verifier": "device-verifier"})
		case "/backend-api/codex/models":
			// The catalogue is account-scoped: the pull carries the bearer and the
			// account id the tokens name. A request without the account id is the
			// 400 the real backend answers, so the double refuses it.
			if got := r.Header.Get("Authorization"); got != "Bearer at-console" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("ChatGPT-Account-ID"); got != d.account {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"detail":"missing ChatGPT-Account-ID header"}`)
				return
			}
			data := []map[string]any{}
			for _, id := range d.models {
				data = append(data, map[string]any{"id": id, "slug": id})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": data})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(d.Close)
	return d
}

func idTokenFor(t *testing.T, account, email string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": account, "email": email, "chatgpt_account_id": account, "exp": time.Now().Add(time.Hour).Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func (d *codexDouble) service(s *store.Store) *codex.Service {
	eps := codex.DefaultEndpoints()
	eps.Issuer = d.URL
	eps.Authorize = d.URL + "/oauth/authorize"
	eps.Token = d.URL + "/oauth/token"
	eps.DeviceCode = d.URL + "/api/accounts/deviceauth/usercode"
	eps.DeviceToken = d.URL + "/api/accounts/deviceauth/token"
	eps.Backend = d.URL + "/backend-api/codex"
	eps.Models = d.URL + "/backend-api/codex/models"
	eps.Usage = d.URL + "/backend-api/wham/usage"
	return codex.NewService(s, nil, eps)
}

func codexConsole(t *testing.T, d *codexDouble) (*store.Store, http.Handler) {
	t.Helper()
	previous := codex.CallbackPorts
	codex.CallbackPorts = []int{0}
	t.Cleanup(func() { codex.CallbackPorts = previous })
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, nil, nil, d.service(s))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	return s, h
}

// The browser path: the authorize URL, the localhost redirect, and the channel
// that appears once the code comes back.
func TestCodexOAuthBrowserCompletes(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	if started["method"] != "browser" {
		t.Fatalf("mode %v", started["method"])
	}
	parsed, err := url.Parse(started["authorization_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("client_id") != codex.ClientID || parsed.Query().Get("originator") != codex.Originator {
		t.Fatalf("authorize URL %v", parsed.Query())
	}
	if started["non_official"] == "" {
		t.Fatal("the console did not flag the track as non-official")
	}
	redirect := started["redirect_uri"].(string)
	if !strings.HasPrefix(redirect, "http://localhost:") {
		t.Fatalf("redirect %q", redirect)
	}
	state := parsed.Query().Get("state")
	// The browser hits the listener.
	resp, err := http.Get(redirect + "?code=code-browser&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)))
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatal("channel not created", err)
	}
	c := channels[0]
	if c.CredentialType != store.CredentialCodexOAuth || c.OAuth.Account != "acct-console" || c.OAuth.ClientID != codex.ClientID {
		t.Fatalf("credential %+v", c.OAuth)
	}
	if len(c.Models) != 0 || len(c.AvailableModels) != 0 || len(c.Credentials) != 1 || len(c.Endpoints) != 1 || c.Endpoints[0].Format != store.FormatOpenAIResponses {
		t.Fatalf("channel %+v", c)
	}
}

// The paste path: the browser never reached the listener, so the user pastes the
// callback link back — on this track's host, which is localhost, not 127.0.0.1.
func TestCodexOAuthPastedCallbackCompletes(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	parsed, _ := url.Parse(started["authorization_url"].(string))
	pasted := started["redirect_uri"].(string) + "?code=code-pasted&state=" + url.QueryEscape(parsed.Query().Get("state"))
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	channels, _ := s.ListChannels()
	if len(channels) != 1 || channels[0].OAuth.AccessToken != "at-console" {
		t.Fatalf("channel %+v", channels)
	}
	// A link on the SIWC host is not this flow's link.
	started = oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	wrong := "http://127.0.0.1:1455/auth/callback?code=x&state=y"
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(wrong)))
	if status["status"] != "pending" {
		t.Fatalf("a foreign callback link was accepted: %+v", status)
	}
}

// The device-code path: the user code, the paced polls, and the exchange of the
// server-minted pair.
func TestCodexOAuthDeviceCodeCompletes(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=add_credential&method=device"))
	if started["method"] != "device" || started["user_code"] != "CODE-ZZ" || started["verification_uri"] != d.URL+"/codex/device" {
		t.Fatalf("start %+v", started)
	}
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)))
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	channels, _ := s.ListChannels()
	if len(channels) != 1 || channels[0].OAuth.AccessToken != "at-console" {
		t.Fatalf("channel %+v", channels)
	}
}

// A server without device auth says so, instead of offering a login that cannot
// finish.
func TestCodexOAuthDeviceAuthDisabled(t *testing.T) {
	d := newCodexDouble(t)
	d.deviceEnabled = false
	_, h := codexConsole(t, d)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=add_credential&method=device"), nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "设备码") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// One account is one channel: a second authorization rotates the channel that
// already holds it, and no exchange happens for the identity check.
func TestCodexOAuthSecondAuthorizationRotates(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	complete := func(path string) map[string]any {
		started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, path))
		parsed, _ := url.Parse(started["authorization_url"].(string))
		pasted := started["redirect_uri"].(string) + "?code=code-2&state=" + url.QueryEscape(parsed.Query().Get("state"))
		return oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	}
	first := complete("/oauth/codex/start")
	if first["status"] != "complete" {
		t.Fatalf("first %+v", first)
	}
	second := complete("/oauth/codex/start?operation=reauth&channel_id=" + strconv.FormatInt(int64(first["channel_id"].(float64)), 10))
	if second["status"] != "complete" || second["channel_id"] != first["channel_id"] {
		t.Fatalf("second %+v", second)
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatalf("second authorization created %d channels", len(channels))
	}
	if channels[0].ID != int64(first["channel_id"].(float64)) {
		t.Fatal("the second authorization named a different channel")
	}
	if channels[0].OAuth.RefreshToken != "rt-console" {
		t.Fatalf("credential not rotated: %+v", channels[0].OAuth)
	}
}

// The usage endpoint reports dynamic rate windows for the console's status display.
func TestCodexUsageEndpoint(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	parsed, _ := url.Parse(started["authorization_url"].(string))
	pasted := started["redirect_uri"].(string) + "?code=code-u&state=" + url.QueryEscape(parsed.Query().Get("state"))
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	id := int64(done["channel_id"].(float64))
	stored, _ := s.Channel(id)
	// The double has no usage route, so the answer is an explicit failure.
	usage := oauthCall(t, h, "GET", "/oauth/codex/usage?channel_id="+strings.TrimSpace(strconv.FormatInt(id, 10))+"&credential_id="+url.QueryEscape(stored.Credentials[0].ID))
	if usage["ok"] != false {
		t.Fatalf("usage %+v", usage)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/codex/usage", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing channel id: %d", w.Code)
	}
}

// P1: a callback that carries an error is reported as a refusal, not as a dead
// credential, and no exchange is attempted.
func TestCodexOAuthCallbackErrorIsARefusal(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	pasted := started["redirect_uri"].(string) + "?error=access_denied&error_description=the+user+said+no&state=" + started["flow"].(string)[:4]
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if status["status"] != "error" || status["code"] != "access_denied" {
		t.Fatalf("status %+v", status)
	}
	if !strings.Contains(status["message"].(string), "拒绝") {
		t.Fatalf("message does not say the user refused: %v", status["message"])
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 || len(channels[0].Credentials) != 0 {
		t.Fatal("a refused callback created a channel")
	}
}

// P1: a callback whose state is not this flow's is refused before the token
// endpoint is reached — that is the check that makes a crafted link useless.
func TestCodexOAuthStateMismatchRefused(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	parsed, _ := url.Parse(started["authorization_url"].(string))
	forged := started["redirect_uri"].(string) + "?code=code-forged&state=" + url.QueryEscape("not-the-state")
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(forged)))
	if status["status"] != "error" || status["code"] != "state_mismatch" {
		t.Fatalf("status %+v", status)
	}
	if !strings.Contains(status["message"].(string), "伪造") {
		t.Fatalf("message does not name the forgery: %v", status["message"])
	}
	// The listener path is checked the same way: a redirect that arrives with
	// the wrong state is refused too.
	started = oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	resp, err := http.Get(started["redirect_uri"].(string) + "?code=code-listener&state=" + url.QueryEscape("bogus"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	listenerStatus := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)))
	if listenerStatus["status"] != "error" || listenerStatus["code"] != "state_mismatch" {
		t.Fatalf("listener path: %+v", listenerStatus)
	}
	_ = parsed
	if channels, _ := s.ListChannels(); len(channels) != 1 || len(channels[0].Credentials) != 0 {
		t.Fatal("a forged callback created a channel")
	}
	if exchanges := d.exchanges.Load(); exchanges != 0 {
		t.Fatalf("a forged callback reached the token endpoint %d times", exchanges)
	}
}

// One authorization per provider: a second start while one is pending hands the
// first flow back — the same flow id and URL, no second listener, and no second
// device code minted upstream. The device mode is a different flow and gets its
// own.
func TestCodexOAuthStartIsIdempotentWhilePending(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	_, h := codexConsole(t, d)
	first := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	second := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	if first["flow"] != second["flow"] || first["authorization_url"] != second["authorization_url"] {
		t.Fatalf("second start minted a new flow: %+v then %+v", first, second)
	}
	// The device mode is its own flow: the user asked for something else.
	device := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=add_credential&method=device"))
	if device["flow"] == first["flow"] {
		t.Fatal("the device mode was answered with the pending browser flow")
	}
	// The handed-back browser flow is the live one: it still completes.
	done := finishCodexBrowser(t, h, d, first)
	if done["status"] != "complete" {
		t.Fatalf("handed-back flow %+v", done)
	}
	// The older device flow is now stale and cannot append after the target changes.
	deviceDone := oauthCall(t, h, "GET", "/oauth/codex/status?flow="+device["flow"].(string))
	if deviceDone["status"] != "error" || deviceDone["code"] != "credential_rejected" {
		t.Fatalf("stale device flow should not append after browser changed target: %+v", deviceDone)
	}
}

// finishCodexBrowser completes one browser flow the way the console sees it:
// the browser hits the listener, and the flow answers complete.
func finishCodexBrowser(t *testing.T, h http.Handler, _ *codexDouble, started map[string]any) map[string]any {
	t.Helper()
	parsed, err := url.Parse(started["authorization_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	redirect := started["redirect_uri"].(string)
	resp, err := http.Get(redirect + "?code=code-done&state=" + url.QueryEscape(parsed.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)))
}

// An OAuth channel's catalogue is pulled with the channel's own credential and
// written back with the replace semantics.
func TestCodexOAuthChannelModelsFetch(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	done := finishCodexBrowser(t, h, d, started)
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	channelID := int64(done["channel_id"].(float64))
	fetched := oauthCall(t, h, "POST", "/channels/"+strconv.FormatInt(channelID, 10)+"/oauth/models")
	if fetched["ok"] != true {
		t.Fatalf("fetch %+v", fetched)
	}
	stored, err := s.Channel(channelID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(stored.AvailableModels, "gpt-5-codex", "gpt-5") {
		t.Fatalf("catalogue %v", stored.AvailableModels)
	}
}

// The editor re-authorizes a member with operation=reauth on every track, and the
// Codex browser flow is what that is: the channel is named, and the
// authorization is the browser one. A second call is idempotent for the same
// channel, and a reauth of another channel is its own flow.
func TestCodexOAuthReauthChannelIsABrowserFlow(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	first := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	done := finishCodexBrowser(t, h, d, first)
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	channelID := int64(done["channel_id"].(float64))
	path := authorizationFixturePath(t, h, "/oauth/codex/start?operation=reauth&channel_id="+strconv.FormatInt(channelID, 10))
	reauth := oauthCall(t, h, "POST", path)
	if reauth["method"] != "browser" || reauth["authorization_url"] == "" {
		t.Fatalf("reauth start %+v", reauth)
	}
	// The same channel again: the flow in flight comes back.
	again := oauthCall(t, h, "POST", path)
	if again["flow"] != reauth["flow"] {
		t.Fatalf("second reauth minted a new flow: %+v then %+v", reauth, again)
	}
	// Without a channel id there is nothing to bind the authorization to.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=reauth"), nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "已有渠道") {
		t.Fatalf("reauth without a channel: %d %s", w.Code, w.Body.String())
	}
	// An unknown mode is still refused.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", authorizationFixturePath(t, h, "/oauth/codex/start?mode=nonsense"), nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode: %d %s", w.Code, w.Body.String())
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 {
		t.Fatalf("the flows created %d channels", len(channels))
	}
}

// A re-authorization is bound to the channel that started it: signing in as
// somebody else in the browser is refused before anything is rotated, exactly as
// the SIWC track refuses an identity mismatch.
func TestCodexOAuthReauthRejectsForeignAccount(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	first := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	done := finishCodexBrowser(t, h, d, first)
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	channelID := int64(done["channel_id"].(float64))
	before, err := s.Channel(channelID)
	if err != nil {
		t.Fatal(err)
	}
	// The re-authorization of channel A is completed by account B.
	d.account = "acct-somebody-else"
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=reauth&channel_id="+strconv.FormatInt(channelID, 10)))
	parsed, _ := url.Parse(started["authorization_url"].(string))
	pasted := started["redirect_uri"].(string) + "?code=code-x&state=" + url.QueryEscape(parsed.Query().Get("state"))
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/codex/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if status["status"] != "error" || status["code"] != "identity_mismatch" {
		t.Fatalf("status %+v", status)
	}
	if !strings.Contains(status["message"].(string), "不一致") {
		t.Fatalf("message %v", status["message"])
	}
	// Nothing was rotated or created: channel A keeps the credential it had.
	after, err := s.Channel(channelID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OAuth.RefreshToken != before.OAuth.RefreshToken || after.OAuth.AccessToken != before.OAuth.AccessToken {
		t.Fatalf("the credential moved: %+v -> %+v", before.OAuth, after.OAuth)
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 {
		t.Fatalf("the refused reauth left %d channels", len(channels))
	}
}

// A re-authorization of a channel that no longer exists says so, instead of
// completing into a brand-new channel for the account.
func TestCodexOAuthReauthMissingChannelIsRefused(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	first := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	done := finishCodexBrowser(t, h, d, first)
	channelID := int64(done["channel_id"].(float64))
	if err := s.DeleteChannel(channelID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=reauth&channel_id="+strconv.FormatInt(channelID, 10)), nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "已有渠道") {
		t.Fatalf("reauth of a missing channel: %d %s", w.Code, w.Body.String())
	}
}

// The Codex track cancels the same way: the loopback port is released and the
// flow is terminal, so a closed browser tab stops the wait.
func TestCodexOAuthCancelReleasesTheListener(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start"))
	flow := started["flow"].(string)
	port := portOfRedirect(t, started["redirect_uri"].(string))
	cancelled := oauthCall(t, h, "POST", "/oauth/codex/cancel?flow="+flow)
	if cancelled["status"] != "cancelled" {
		t.Fatalf("cancel %+v", cancelled)
	}
	// The loopback port goes back: this track pins 1455/1457, and the next
	// authorization needs it.
	waitForPortRelease(t, port)
	if again := oauthCall(t, h, "POST", "/oauth/codex/cancel?flow="+flow); again["status"] != "cancelled" {
		t.Fatalf("second cancel %+v", again)
	}
	// Nothing completes a cancelled flow, not even the redirect.
	status := oauthCall(t, h, "GET", "/oauth/codex/status?flow="+flow)
	if status["status"] != "cancelled" {
		t.Fatalf("status after cancel %+v", status)
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 || len(channels[0].Credentials) != 0 {
		t.Fatalf("a cancelled flow created %d channels", len(channels))
	}
}
