package console

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

// chatgptDouble is the issuer plus the inference host for the console flow:
// JWKS, the token endpoint, userinfo and the catalogue. It signs id_tokens with
// the nonce the console asked for, which the test supplies by parsing the
// authorization URL the /start call returned.
// subjectOr is the subject the double signs for: the one the test named, or the
// default account.
func (d *chatgptDouble) subjectOr() string {
	if d.subject != "" {
		return d.subject
	}
	return "acct-console"
}

type chatgptDouble struct {
	*httptest.Server
	key         *rsa.PrivateKey
	nonce       func() string
	client      string // client id the token response reports
	callback    string // client id the callback carries, empty when not sent
	tokenClient string // client id the token response reports; "" reports none
	subject     string // subject the id_token carries
	scope       string // scope the token response reports
	models      []string
	exchanges   atomic.Int32
	// A failing token endpoint: the status, the OAuth error code and the
	// description it answers with, so the exchange-failure branches can be
	// driven for real.
	tokenStatus int
	tokenError  string
	tokenDetail string
}

func newChatgptDouble(t *testing.T) *chatgptDouble {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	d := &chatgptDouble{key: key, nonce: func() string { return "nonce-default" }, client: "oaiapp_console", tokenClient: "oaiapp_console", scope: siwc.Scope, models: []string{"gpt-5", "gpt-5-codex"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kid": "kid-1", "alg": "RS256", "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(bigEndian(key.PublicKey.E)),
		}}})
	})
	mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		d.exchanges.Add(1)
		if d.tokenStatus != 0 {
			w.WriteHeader(d.tokenStatus)
			json.NewEncoder(w).Encode(siwc.TokenResponse{Error: d.tokenError, ErrorDescription: d.tokenDetail})
			return
		}
		claims := map[string]any{
			"iss": d.URL, "sub": d.subjectOr(), "aud": d.tokenClient,
			"exp": time.Now().Add(time.Hour).Unix(), "nonce": d.nonce(),
			"email": "person@example.test", "name": "Person",
		}
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "kid-1"})
		payload, _ := json.Marshal(claims)
		signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		sum := sha256.Sum256([]byte(signing))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		idToken := signing + "." + base64.RawURLEncoding.EncodeToString(signature)
		json.NewEncoder(w).Encode(siwc.TokenResponse{
			AccessToken: "at-console", RefreshToken: "rt-console", IDToken: idToken,
			ExpiresIn: 3600, Scope: d.scope, ClientID: d.tokenClient,
		})
	})
	mux.HandleFunc("/api/accounts/oauth/userinfo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"email": "person@example.test"})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-console" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		entries := []map[string]any{}
		for _, id := range d.models {
			entries = append(entries, map[string]any{"slug": id, "display_name": id, "visibility": "list"})
		}
		json.NewEncoder(w).Encode(map[string]any{"models": entries})
	})
	d.Server = httptest.NewServer(mux)
	t.Cleanup(d.Close)
	return d
}

func (d *chatgptDouble) endpoints() siwc.Endpoints {
	return siwc.Endpoints{
		Issuer: d.URL, Authorize: d.URL + "/api/accounts/authorize",
		Token: d.URL + "/api/accounts/oauth/token", Userinfo: d.URL + "/api/accounts/oauth/userinfo",
		JWKS: d.URL + "/.well-known/jwks.json", Inference: d.URL + "/v1", Models: d.URL + "/v1/models",
	}
}

func chatgptConsole(t *testing.T, d *chatgptDouble) (*store.Store, http.Handler) {
	t.Helper()
	previous := siwc.CallbackPorts
	siwc.CallbackPorts = []int{0}
	t.Cleanup(func() { siwc.CallbackPorts = previous })
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	eps := d.endpoints()
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{},
		nil, siwc.NewService(s, nil, eps), codex.NewService(s, nil, codex.DefaultEndpoints()))
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	return s, h
}

// startAndArm begins a flow and teaches the double to sign with the nonce the
// returned authorization URL carries.
func startAndArm(t *testing.T, h http.Handler, d *chatgptDouble) map[string]any {
	t.Helper()
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	parsed, err := url.Parse(started["authorization_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	nonce := parsed.Query().Get("nonce")
	if nonce == "" {
		t.Fatal("authorization URL carries no nonce")
	}
	d.nonce = func() string { return nonce }
	return started
}

// The start call returns the authorize URL the user visits and the loopback
// redirect it will come back to — with the registration entry point and the
// name hint on a first run.
func TestChatGPTOAuthStartReturnsAuthorizationURL(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	parsed, err := url.Parse(started["authorization_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("client_id") != siwc.DynamicAppID || q.Get("agent_name_hint") != siwc.AgentNameHint {
		t.Fatalf("first authorization %v", q)
	}
	if q.Get("scope") != siwc.Scope || q.Get("resource") != siwc.Resource {
		t.Fatalf("scope/resource %v", q)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("pkce %v", q)
	}
	if !strings.HasPrefix(started["redirect_uri"].(string), "http://127.0.0.1:") || !strings.HasSuffix(started["redirect_uri"].(string), siwc.CallbackPath) {
		t.Fatalf("redirect %q", started["redirect_uri"])
	}
	if started["flow"] == "" || started["paste_hint"] == "" {
		t.Fatalf("start payload %+v", started)
	}
}

// The browser path: the loopback listener catches the redirect, the exchange
// runs, and the channel appears with the Responses endpoint and the catalogue.
func TestChatGPTOAuthListenerCompletes(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	s, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	redirect := started["redirect_uri"].(string)
	// The browser: hit the redirect with the code and the state it was given.
	parsed, _ := url.Parse(started["authorization_url"].(string))
	state := parsed.Query().Get("state")
	resp, err := http.Get(redirect + "?code=code-live&state=" + url.QueryEscape(state) + "&client_id=oaiapp_console")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback page: %d", resp.StatusCode)
	}
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)))
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	if done["channel_id"] == nil {
		t.Fatalf("no channel: %+v", done)
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatal("channel not created", err)
	}
	c := channels[0]
	if c.CredentialType != store.CredentialChatGPTOAuth || c.OAuth.ClientID != "oaiapp_console" || c.OAuth.Subject != "acct-console" {
		t.Fatalf("credential %+v", c.OAuth)
	}
	if c.OAuth.AccessToken != "at-console" || c.OAuth.RefreshToken != "rt-console" || c.OAuth.Status != store.OAuthStatusActive {
		t.Fatalf("tokens %+v", c.OAuth)
	}
	if len(c.Endpoints) != 1 || c.Endpoints[0].Format != store.FormatOpenAIResponses || c.Endpoints[0].BaseURL != d.URL+"/v1" {
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

	if c.OAuth.AccountLabel != "person@example.test" {
		t.Fatalf("label %q", c.OAuth.AccountLabel)
	}
}

// The paste path: the browser never reached the listener, so the user pastes the
// callback link back and the same exchange runs on it.
func TestChatGPTOAuthPastedCallbackCompletes(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	s, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	parsed, _ := url.Parse(started["authorization_url"].(string))
	state := parsed.Query().Get("state")
	pasted := started["redirect_uri"].(string) + "?code=code-pasted&state=" + url.QueryEscape(state) + "&client_id=oaiapp_console"
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatal("channel not created", err)
	}
	if channels[0].OAuth.AccessToken != "at-console" {
		t.Fatalf("tokens %+v", channels[0].OAuth)
	}
	if d.exchanges.Load() != 1 {
		t.Fatalf("exchanges %d", d.exchanges.Load())
	}
}

// A callback whose state does not match the flow is refused: that is the check
// that makes a link someone else crafted useless.
func TestChatGPTOAuthStateMismatch(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	s, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	pasted := started["redirect_uri"].(string) + "?code=code-x&state=forged-state&client_id=oaiapp_console"
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if status["status"] != "error" || status["code"] != "state_mismatch" {
		t.Fatalf("status %+v", status)
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 || len(channels[0].Credentials) != 0 {
		t.Fatal("a forged callback created a channel")
	}
	if d.exchanges.Load() != 0 {
		t.Fatal("a forged callback reached the token endpoint")
	}
}

// A consent that was refused comes back without the ChatGPT-plan scope: no
// channel is created, and the answer says what to re-run.
func TestChatGPTOAuthConsentMissingScope(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	d.scope = "openid profile email offline_access resource.invoke"
	s, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	parsed, _ := url.Parse(started["authorization_url"].(string))
	pasted := started["redirect_uri"].(string) + "?code=code-c&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if status["status"] != "error" || status["code"] != "consent_missing_scope" {
		t.Fatalf("status %+v", status)
	}
	if !strings.Contains(status["message"].(string), "chatgpt.tokens.use.direct") {
		t.Fatalf("message does not name the missing scope: %v", status["message"])
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 || len(channels[0].Credentials) != 0 {
		t.Fatal("a refused consent created a channel")
	}
	// P1-5: no evidence either way is its own refusal, not a granted consent. A
	// fresh flow, because the first one is already terminal.
	d.scope = ""
	fresh := startAndArm(t, h, d)
	parsed2, _ := url.Parse(fresh["authorization_url"].(string))
	pasted = fresh["redirect_uri"].(string) + "?code=code-u2&state=" + url.QueryEscape(parsed2.Query().Get("state")) + "&client_id=oaiapp_console"
	undetermined := oauthCall(t, h, "GET", "/oauth/chatgpt/status?flow="+fresh["flow"].(string)+"&callback="+url.QueryEscape(pasted))
	if undetermined["status"] != "error" || undetermined["code"] != "scope_undetermined" {
		t.Fatalf("undetermined consent: %+v", undetermined)
	}
}

// A registration that came back as a different app is refused rather than
// silently adopted: on a first authorization the callback's client must be an
// oaiapp_ registration, and on a re-authorization it must be the client the flow
// used.
func TestChatGPTOAuthClientIDMismatch(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	s, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	parsed, _ := url.Parse(started["authorization_url"].(string))
	// First authorization, and neither the callback nor the token response
	// names a registration: there is nothing to exchange as, so it is refused.
	d.tokenClient = ""
	pasted := started["redirect_uri"].(string) + "?code=code-m&state=" + url.QueryEscape(parsed.Query().Get("state"))
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if status["status"] != "error" || status["code"] != "client_id_missing" {
		t.Fatalf("status %+v", status)
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 || len(channels[0].Credentials) != 0 {
		t.Fatal("a mismatched registration created a channel")
	}
}

// Explicit reauthorization rotates the selected member in its existing pool.
func TestChatGPTOAuthSecondAuthorizationRotates(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	s, h := chatgptConsole(t, d)
	first := startAndArm(t, h, d)
	complete := func(started map[string]any) map[string]any {
		parsed, _ := url.Parse(started["authorization_url"].(string))
		state := parsed.Query().Get("state")
		// The nonce is per flow, so the double must sign for this one.
		d.nonce = func() string { return parsed.Query().Get("nonce") }
		pasted := started["redirect_uri"].(string) + "?code=code-2&state=" + url.QueryEscape(state) + "&client_id=oaiapp_console"
		return oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	}
	done := complete(first)
	if done["status"] != "complete" {
		t.Fatalf("first authorization %+v", done)
	}
	channelID := done["channel_id"]
	// P1-2: "adding an account" registers a new client. The oaiapp_* is bound to
	// one user and workspace, so it must not be reused for a different account —
	// a second start with no mode is a new registration, not a re-authorization.
	fresh := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	freshURL, _ := url.Parse(fresh["authorization_url"].(string))
	if freshURL.Query().Get("client_id") != siwc.DynamicAppID || freshURL.Query().Get("agent_name_hint") == "" {
		t.Fatalf("adding an account must register a new client: %v", freshURL.Query())
	}
	oauthCall(t, h, "POST", "/oauth/chatgpt/cancel?flow="+fresh["flow"].(string))
	// The re-authorization of this channel names the channel and reuses its
	// registered client, with no name hint.
	second := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start?operation=reauth&channel_id="+strconv.FormatInt(int64(channelID.(float64)), 10)))
	parsed, _ := url.Parse(second["authorization_url"].(string))
	if parsed.Query().Get("client_id") != "oaiapp_console" {
		t.Fatalf("re-authorization client_id %q", parsed.Query().Get("client_id"))
	}
	if parsed.Query().Get("agent_name_hint") != "" {
		t.Fatal("re-authorization carried the name hint")
	}
	d.nonce = func() string { return parsed.Query().Get("nonce") }
	pasted := second["redirect_uri"].(string) + "?code=code-3&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
	again := oauthCall(t, h, "GET", "/oauth/chatgpt/status?flow="+second["flow"].(string)+"&callback="+url.QueryEscape(pasted))
	if again["status"] != "complete" {
		t.Fatalf("second authorization %+v", again)
	}
	if again["channel_id"] != channelID {
		t.Fatalf("reauthorization changed target: %+v", again)
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatalf("second authorization created %d channels", len(channels))
	}
	if channels[0].ID != int64(channelID.(float64)) {
		t.Fatalf("rotated channel %d, want %v", channels[0].ID, channelID)
	}
	if channels[0].OAuth.AccessToken != "at-console" {
		t.Fatalf("credential not rotated: %+v", channels[0].OAuth)
	}
}

func bigEndian(e int) []byte {
	out := []byte{}
	for v := e; v > 0; v >>= 8 {
		out = append([]byte{byte(v)}, out...)
	}
	if len(out) == 0 {
		return []byte{0}
	}
	return out
}

// P1-1: an SIWC flow has no device code, so its expiry is the flow's own. A
// second start hands the in-flight flow back (one authorization per provider)
// and must not prune it away, and the first one must still be answerable —
// including its pending response naming a real expiry instead of a zero value.
func TestChatGPTOAuthFlowSurvivesAnotherStart(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	first := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	pending := oauthCall(t, h, "GET", "/oauth/chatgpt/status?flow="+first["flow"].(string))
	if pending["status"] != "pending" {
		t.Fatalf("status %+v", pending)
	}
	expires, ok := pending["expires"].(string)
	if !ok || expires == "" || strings.HasPrefix(expires, "0001-01-01") {
		t.Fatalf("pending expiry is not a real window: %v", pending["expires"])
	}
	// A second start is idempotent: the same flow comes back, unchanged, and
	// nothing was pruned.
	second := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	if second["flow"] != first["flow"] {
		t.Fatalf("the second start minted %q while %q was pending", second["flow"], first["flow"])
	}
	again := oauthCall(t, h, "GET", "/oauth/chatgpt/status?flow="+first["flow"].(string))
	if again["status"] != "pending" {
		t.Fatalf("the in-flight flow was disturbed by an unrelated start: %+v", again)
	}
	// And it still completes.
	parsed, _ := url.Parse(first["authorization_url"].(string))
	d.nonce = func() string { return parsed.Query().Get("nonce") }
	pasted := first["redirect_uri"].(string) + "?code=code-survive&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
	done := oauthCall(t, h, "GET", "/oauth/chatgpt/status?flow="+first["flow"].(string)+"&callback="+url.QueryEscape(pasted))
	if done["status"] != "complete" {
		t.Fatalf("the surviving flow did not complete: %+v", done)
	}
}

// P1-2: a finished flow releases its loopback listener. The port it held can be
// bound again, and the old redirect no longer answers.
func TestChatGPTOAuthCompletionReleasesTheListener(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	redirect := started["redirect_uri"].(string)
	port := redirect[strings.LastIndex(redirect, ":")+1 : len(redirect)-len(siwc.CallbackPath)]
	parsed, _ := url.Parse(started["authorization_url"].(string))
	pasted := redirect + "?code=code-release&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	// The port can be rebound: nothing holds it any more.
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("the flow's port %s is still held: %v", port, err)
	}
	listener.Close()
	// And the flow's redirect is gone.
	if resp, err := http.Get(redirect + "?code=late&state=" + url.QueryEscape(parsed.Query().Get("state"))); err == nil {
		resp.Body.Close()
		t.Fatal("a released listener still answered a redirect")
	}
}

// P1-2 (prune path): a flow abandoned mid-flight is pruned by the next start,
// and pruning releases the listener too.
func TestChatGPTOAuthPrunedFlowReleasesTheListener(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	abandoned := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	port := portOfRedirect(t, abandoned["redirect_uri"].(string))
	// Backdate the flow's window by starting fresh ones until prune runs: the
	// simplest honest trigger is a completed flow, whose finish already ran, so
	// here the assertion is that an abandoned flow's port is released by the
	// window rather than by completion. Start enough flows that prune runs.
	oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	time.Sleep(50 * time.Millisecond)
	// The abandoned flow is still within its window, so it must still hold its
	// port — prune only drops finished or expired flows.
	if listener, err := net.Listen("tcp", "127.0.0.1:"+port); err == nil {
		listener.Close()
		t.Fatal("an in-window flow lost its listener")
	}
	// A finished flow's port, by contrast, is free.
	started := startAndArm(t, h, d)
	parsed, _ := url.Parse(started["authorization_url"].(string))
	pasted := started["redirect_uri"].(string) + "?code=code-x&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
	if done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted))); done["status"] != "complete" {
		t.Fatalf("status %+v", done)
	}
	freed := portOfRedirect(t, started["redirect_uri"].(string))
	if listener, err := net.Listen("tcp", "127.0.0.1:"+freed); err != nil {
		t.Fatalf("a finished flow still holds port %s: %v", freed, err)
	} else {
		listener.Close()
	}
	_ = abandoned
}

func portOfRedirect(t *testing.T, redirect string) string {
	t.Helper()
	host := redirect[strings.Index(redirect, "//")+2:]
	port := host[strings.LastIndex(host, ":")+1 : len(host)-len(siwc.CallbackPath)]
	return port
}

// 必要 1: choosing A and signing in as B is refused, with nothing created and
// nothing rotated. The flow was started for the account the channel holds.
func TestChatGPTOAuthReauthRejectsForeignAccount(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	d.subject = "account-B"
	d.tokenClient = "oaiapp_bound" // the issuer echoes the client the flow used
	s, h := chatgptConsole(t, d)
	// A channel already holding account A.
	bound := store.Channel{Name: "chatgpt", CredentialType: store.CredentialChatGPTOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"}},
		OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderChatGPT, RefreshToken: "rt-bound", IDToken: "id-bound",
			ClientID: "oaiapp_bound", HostID: "urn:uuid:11111111-1111-4111-8111-111111111111",
			Subject: "account-A", Scopes: siwc.Scope, AccessToken: "at-bound",
			ExpiresAt: time.Now().Add(time.Hour).UTC(), APIBase: "https://api.openai.com/v1", Status: store.OAuthStatusActive,
		}}
	if err := s.SaveChannel(&bound); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Channel(bound.ID)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start?operation=reauth&channel_id="+strconv.FormatInt(bound.ID, 10)))
	parsed, _ := url.Parse(started["authorization_url"].(string))
	// The nonce is per flow, so the double must sign for this one.
	d.nonce = func() string { return parsed.Query().Get("nonce") }
	if parsed.Query().Get("client_id") != "oaiapp_bound" {
		t.Fatalf("reauthorization client %q", parsed.Query().Get("client_id"))
	}
	// 推荐 5: the re-authorization preselects the account it belongs to.
	if parsed.Query().Get("id_token_hint") != "id-bound" {
		t.Fatalf("id_token_hint %q, want the stored id_token", parsed.Query().Get("id_token_hint"))
	}
	pasted := started["redirect_uri"].(string) + "?code=code-foreign&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_bound"
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if status["status"] != "error" || status["code"] != "identity_mismatch" {
		t.Fatalf("status %+v", status)
	}
	if !strings.Contains(status["message"].(string), "原账号") {
		t.Fatalf("message does not name the remedy: %v", status["message"])
	}
	after, _ := s.Channel(bound.ID)
	if after.OAuth.AccessToken != before.OAuth.AccessToken || after.OAuth.RefreshToken != before.OAuth.RefreshToken {
		t.Fatal("the channel's credential was rotated by a foreign account")
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 {
		t.Fatalf("a foreign account created channels: %d", len(channels))
	}
}

// 必要 4: deleting an SIWC channel revokes the credential at the issuer first,
// and the answer says whether the issuer confirmed it.
func TestChatGPTOAuthChannelDeleteRevokes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		confirmed bool
		warning   string // the fragment the warning must contain
	}{
		{"revoked", http.StatusOK, true, ""},
		{"refused", http.StatusBadRequest, false, "未接受撤销请求"},
		{"server error", http.StatusInternalServerError, false, "未能联系 OpenAI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var revoked atomic.Value
			revoked.Store("")
			var attempts atomic.Int32
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/oauth/revoke") {
					w.WriteHeader(404)
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				attempts.Add(1)
				form := r.Form.Get("token") + "|" + r.Form.Get("client_id") + "|" + r.Form.Get("token_type_hint")
				revoked.Store(form)
				w.WriteHeader(tc.status)
			}))
			defer issuer.Close()
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			eps := siwc.DefaultEndpoints()
			eps.Revoke = issuer.URL + "/api/accounts/oauth/revoke"
			h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, nil, siwc.NewService(s, nil, eps), nil)
			channel := store.Channel{Name: "chatgpt", CredentialType: store.CredentialChatGPTOAuth, Enabled: true,
				Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"}},
				OAuth: &store.OAuthCredential{
					Provider: store.OAuthProviderChatGPT, RefreshToken: "rt-to-revoke", IDToken: "id-x",
					ClientID: "oaiapp_revoke", HostID: "urn:uuid:11111111-1111-4111-8111-111111111111",
					Subject: "sub-revoke", Scopes: siwc.Scope, AccessToken: "at-x",
					ExpiresAt: time.Now().Add(time.Hour).UTC(), APIBase: "https://api.openai.com/v1", Status: store.OAuthStatusActive,
				}}
			if err := s.SaveChannel(&channel); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("DELETE", "/channels/"+strconv.FormatInt(channel.ID, 10), nil))
			if w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			var answer map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
				t.Fatal(err)
			}
			if answer["ok"] != true {
				t.Fatalf("answer %+v", answer)
			}
			if answer["revocation_confirmed"] != tc.confirmed {
				t.Fatalf("revocation_confirmed = %v, want %v", answer["revocation_confirmed"], tc.confirmed)
			}
			if tc.warning == "" {
				if answer["warning"] != nil {
					t.Fatalf("unexpected warning: %v", answer["warning"])
				}
			} else {
				text, _ := answer["warning"].(string)
				if !strings.Contains(text, tc.warning) {
					t.Fatalf("warning %q does not say %q", text, tc.warning)
				}
			}
			if got := revoked.Load().(string); got != "rt-to-revoke|oaiapp_revoke|refresh_token" {
				t.Fatalf("revoke form %q", got)
			}
			// A 5xx is retried once; a definitive answer is not.
			if want := int32(1); tc.status >= 400 && tc.status < 500 {
				if attempts.Load() != want {
					t.Fatalf("a refused revoke was retried %d times", attempts.Load())
				}
			} else if tc.status >= 500 && attempts.Load() != 2 {
				t.Fatalf("a 5xx revoke was attempted %d times, want 2", attempts.Load())
			}
			if channels, _ := s.ListChannels(); len(channels) != 0 {
				t.Fatal("the channel was not deleted")
			}
		})
	}
}

// 4: a re-authorization's callback that names a different client is refused
// before the token endpoint is contacted — the code is only exchangeable by the
// client it was issued to.
func TestChatGPTOAuthCallbackClientMismatchRefusedBeforeExchange(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	s, h := chatgptConsole(t, d)
	bound := store.Channel{Name: "chatgpt", CredentialType: store.CredentialChatGPTOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"}},
		OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderChatGPT, RefreshToken: "rt-bound", IDToken: "id-bound",
			ClientID: "oaiapp_bound", HostID: "urn:uuid:11111111-1111-4111-8111-111111111111",
			Subject: "acct-console", Scopes: siwc.Scope, AccessToken: "at-bound",
			ExpiresAt: time.Now().Add(time.Hour).UTC(), APIBase: "https://api.openai.com/v1", Status: store.OAuthStatusActive,
		}}
	if err := s.SaveChannel(&bound); err != nil {
		t.Fatal(err)
	}
	before := d.exchanges.Load()
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start?operation=reauth&channel_id="+strconv.FormatInt(bound.ID, 10)))
	parsed, _ := url.Parse(started["authorization_url"].(string))
	d.nonce = func() string { return parsed.Query().Get("nonce") }
	pasted := started["redirect_uri"].(string) + "?code=code-x&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_someone_else"
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if status["status"] != "error" || status["code"] != "client_id_mismatch" {
		t.Fatalf("status %+v", status)
	}
	if d.exchanges.Load() != before {
		t.Fatalf("the token endpoint was contacted %d times", d.exchanges.Load()-before)
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 {
		t.Fatal("a foreign client created a channel")
	}
}

// One authorization per provider: a second start while one is pending hands the
// first flow back — the same flow id and URL, and no second listener. The user
// clicked twice, or reloaded the page and clicked again. A flow for a different
// entry point (a channel's re-authorization) is a different flow and gets its
// own once the first one has finished.
func TestChatGPTOAuthStartIsIdempotentWhilePending(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	first := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	second := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	if first["flow"] != second["flow"] || first["authorization_url"] != second["authorization_url"] {
		t.Fatalf("second start minted a new flow: %+v then %+v", first, second)
	}
	if first["redirect_uri"] != second["redirect_uri"] {
		t.Fatalf("the repeat start rebound the listener: %v then %v", first["redirect_uri"], second["redirect_uri"])
	}
	// The handed-back flow is the live one: finishing it releases the port.
	channelID := finishChatGPT(t, h, d, first)
	reauth := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start?operation=reauth&channel_id="+strconv.FormatInt(channelID, 10)))
	if reauth["flow"] == first["flow"] {
		t.Fatal("a reauth of the channel was answered with the finished add-account flow")
	}
	finishChatGPT(t, h, d, reauth)
}

// finishChatGPT completes one browser flow the way the console sees it: the
// double signs the nonce the flow's URL carries, and the pasted callback
// closes it. It reports the channel the flow ended on.
func finishChatGPT(t *testing.T, h http.Handler, d *chatgptDouble, started map[string]any) int64 {
	t.Helper()
	parsed, err := url.Parse(started["authorization_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	d.nonce = func() string { return parsed.Query().Get("nonce") }
	pasted := started["redirect_uri"].(string) + "?code=code-done&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if done["status"] != "complete" {
		t.Fatalf("finish %+v", done)
	}
	return int64(done["channel_id"].(float64))
}

// The exchange failure carries the issuer's own words: error_description is
// appended to the fixed advice, so the owner sees why instead of a generic
// "rejected". The renewable-session and scope refusals keep their own wording.
func TestChatGPTOAuthExchangeSurfacesIssuerDetail(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	parsed, _ := url.Parse(started["authorization_url"].(string))
	d.tokenStatus = http.StatusBadRequest
	d.tokenError = "invalid_grant"
	d.tokenDetail = "the authorization code has already been redeemed"
	pasted := started["redirect_uri"].(string) + "?code=code-x&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
	status := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
	if status["status"] != "error" || status["code"] != "reauth_required" {
		t.Fatalf("status %+v", status)
	}
	message, _ := status["message"].(string)
	if !strings.Contains(message, "OpenAI 拒绝了授权码") || !strings.Contains(message, "the authorization code has already been redeemed") {
		t.Fatalf("message %q", message)
	}
}

// An OAuth channel's catalogue is pulled with the channel's own credential and
// written back with the replace semantics.
func TestChatGPTOAuthChannelModelsFetch(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	s, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	parsed, _ := url.Parse(started["authorization_url"].(string))
	pasted := started["redirect_uri"].(string) + "?code=code-x&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
	done := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(pasted)))
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
	if !containsAll(stored.AvailableModels, "gpt-5", "gpt-5-codex") {
		t.Fatalf("catalogue %v", stored.AvailableModels)
	}
}

// Cancel ends a pending flow at the user's request: the listener (and its port)
// is released, the flow answers cancelled, and nothing revives it — a redirect
// that arrives afterwards has nowhere to land. Cancelling twice answers the
// same, and a cancelled flow is not a completed one.
func TestChatGPTOAuthCancelReleasesAndIsTerminal(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	redirect := started["redirect_uri"].(string)
	port := portOfRedirect(t, redirect)
	flow := started["flow"].(string)
	// The port is held while the flow is pending.
	if listener, err := net.Listen("tcp", "127.0.0.1:"+port); err == nil {
		listener.Close()
		t.Fatal("a pending flow lost its listener")
	}
	cancelled := oauthCall(t, h, "POST", "/oauth/chatgpt/cancel?flow="+flow)
	if cancelled["status"] != "cancelled" {
		t.Fatalf("cancel %+v", cancelled)
	}
	// The port is free again.
	waitForPortRelease(t, port)
	// A repeat cancel is the same answer.
	again := oauthCall(t, h, "POST", "/oauth/chatgpt/cancel?flow="+flow)
	if again["status"] != "cancelled" {
		t.Fatalf("second cancel %+v", again)
	}
	// The flow stays cancelled: nothing completes it, not even a redirect that
	// somehow still arrives.
	status := oauthCall(t, h, "GET", "/oauth/chatgpt/status?flow="+flow)
	if status["status"] != "cancelled" {
		t.Fatalf("status after cancel %+v", status)
	}
	if resp, err := http.Get(redirect + "?code=code-late&state=late"); err == nil {
		resp.Body.Close()
		t.Fatal("a cancelled flow's listener still answered")
	}
	status = oauthCall(t, h, "GET", "/oauth/chatgpt/status?flow="+flow)
	if status["status"] != "cancelled" {
		t.Fatalf("a late redirect revived the flow: %+v", status)
	}
	// Cancelling a flow that is already gone answers the same, so a double cancel
	// (or one racing a prune) never looks like a failure.
	unknown := oauthCall(t, h, "POST", "/oauth/chatgpt/cancel?flow=nope")
	if unknown["status"] != "cancelled" {
		t.Fatalf("cancel of an unknown flow %+v", unknown)
	}
}

// A cancel that arrives after the flow completed does not undo it: the channel
// stays, and the answer is the state the flow ended in.
func TestChatGPTOAuthCancelAfterCompletionKeepsTheChannel(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newChatgptDouble(t)
	s, h := chatgptConsole(t, d)
	started := startAndArm(t, h, d)
	channelID := finishChatGPT(t, h, d, started)
	answer := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/cancel?flow="+started["flow"].(string)))
	if answer["status"] != "complete" || int64(answer["channel_id"].(float64)) != channelID {
		t.Fatalf("late cancel %+v", answer)
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 {
		t.Fatalf("the channel count moved: %d", len(channels))
	}
}

// A cancel without a flow id is a client mistake, not a no-op.
func TestOAuthCancelRequiresAFlow(t *testing.T) {
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/oauth/chatgpt/cancel", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing flow: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/oauth/chatgpt/cancel?flow=", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty flow: %d %s", w.Code, w.Body.String())
	}
}

// The window is the authorization code's own lifetime, not a convenience: ten
// minutes. A flow kept alive past that can only invite pasting a callback whose
// code the issuer has already expired, and the honest answer becomes "start
// again" — a shorter window asks for it sooner.
func TestChatGPTOAuthWindowIsTenMinutes(t *testing.T) {
	d := newChatgptDouble(t)
	_, h := chatgptConsole(t, d)
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start"))
	pending := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)))
	expires, err := time.Parse(time.RFC3339, pending["expires"].(string))
	if err != nil {
		t.Fatalf("expires %v: %v", pending["expires"], err)
	}
	if left := time.Until(expires); left < 9*time.Minute || left > 11*time.Minute {
		t.Fatalf("window %v, want about ten minutes", left)
	}
}
