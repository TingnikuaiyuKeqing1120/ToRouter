package siwc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/store"
)

func testEndpoints(server *httptest.Server) Endpoints {
	eps := DefaultEndpoints()
	eps.Issuer = server.URL
	eps.Authorize = server.URL + "/api/accounts/authorize"
	eps.Token = server.URL + "/api/accounts/oauth/token"
	eps.Userinfo = server.URL + "/api/accounts/oauth/userinfo"
	eps.JWKS = server.URL + "/.well-known/jwks.json"
	eps.Inference = server.URL + "/v1"
	eps.Models = server.URL + "/v1/models"
	return eps
}

func testClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func mustAuthorize(t *testing.T, eps Endpoints, clientID, hostID, redirect string, hint IDTokenHint) AuthRequest {
	t.Helper()
	auth, err := AuthorizeURL(eps, clientID, hostID, redirect, hint)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

// The authorization URL carries every parameter the protocol ref names, with
// the values it names: the registration entry point on a first run, the pinned
// loopback redirect, PKCE S256, fresh state and nonce, the full scope set and
// the resource parameter — and agent_name_hint only on that first run.
func TestAuthorizeURLParameters(t *testing.T) {
	eps := DefaultEndpoints()
	redirect := "http://127.0.0.1:1455/auth/callback"
	auth := mustAuthorize(t, eps, DynamicAppID, "urn:uuid:11111111-1111-4111-8111-111111111111", redirect, IDTokenHint{})
	parsed, err := url.Parse(auth.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "auth.openai.com" || parsed.Path != "/api/accounts/authorize" {
		t.Fatalf("authorize URL %q", auth.URL)
	}
	q := parsed.Query()
	want := map[string]string{
		"client_id":             DynamicAppID,
		"response_type":         "code",
		"redirect_uri":          redirect,
		"scope":                 Scope,
		"resource":              Resource,
		"code_challenge_method": "S256",
		"ext_agent_host_id":     "urn:uuid:11111111-1111-4111-8111-111111111111",
		"agent_name_hint":       AgentNameHint,
	}
	for key, value := range want {
		if got := q.Get(key); got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
	// S256: the challenge is the base64url sha256 of the verifier we keep.
	sum := sha256.Sum256([]byte(auth.Verifier))
	if challenge := base64.RawURLEncoding.EncodeToString(sum[:]); q.Get("code_challenge") != challenge {
		t.Fatalf("code_challenge %q is not S256 of the verifier", q.Get("code_challenge"))
	}
	if len(auth.State) < 20 || len(auth.Nonce) < 20 {
		t.Fatal("state/nonce too short")
	}
	// A re-authorization carries the registered client and no name hint.
	stored := mustAuthorize(t, eps, "oaiapp_existing", "urn:uuid:11111111-1111-4111-8111-111111111111", redirect, IDTokenHint{})
	rq := parsed.Query()
	_ = rq
	parsed2, _ := url.Parse(stored.URL)
	q2 := parsed2.Query()
	if q2.Get("client_id") != "oaiapp_existing" {
		t.Fatalf("re-authorization client_id %q", q2.Get("client_id"))
	}
	if q2.Get("agent_name_hint") != "" {
		t.Fatal("re-authorization carried agent_name_hint")
	}
	if q2.Get("state") == q.Get("state") || q2.Get("nonce") == q.Get("nonce") || stored.Verifier == auth.Verifier {
		t.Fatal("state/nonce/verifier were reused")
	}
	// The redirect is pinned: 127.0.0.1 and the fixed path, never localhost.
	for _, bad := range []string{"http://localhost:1455/auth/callback", "http://127.0.0.1:1455/other", "https://127.0.0.1:1455/auth/callback", "http://evil.test/auth/callback"} {
		if _, err := AuthorizeURL(eps, DynamicAppID, "urn:uuid:x", bad, IDTokenHint{}); err == nil {
			t.Fatalf("redirect %q accepted", bad)
		}
	}
}

// The callback arrives on the pinned loopback path, and the paste fallback reads
// the same link back. Anything that is not that path on 127.0.0.1 is refused:
// a pasted link is a link someone else could have crafted.
func TestParseCallbackURL(t *testing.T) {
	full := "http://127.0.0.1:1455/auth/callback?code=abc&state=st-1&client_id=oaiapp_x"
	result, err := ParseCallbackURL(full)
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != "abc" || result.State != "st-1" || result.ClientID != "oaiapp_x" {
		t.Fatalf("%+v", result)
	}
	// A bare query string is what a clipboard paste often yields.
	if result, err = ParseCallbackURL("?code=abc&state=st-1"); err != nil {
		t.Fatal(err)
	}
	if result.Code != "abc" {
		t.Fatalf("bare query %+v", result)
	}
	// The error branch travels too.
	if result, err = ParseCallbackURL(full[:strings.Index(full, "?")] + "?error=access_denied&error_description=no"); err != nil {
		t.Fatal(err)
	}
	if result.Error != "access_denied" || result.Description != "no" {
		t.Fatalf("%+v", result)
	}
	for _, bad := range []string{
		"http://localhost:1455/auth/callback?code=abc",
		"http://127.0.0.1:1455/other?code=abc",
		"https://127.0.0.1:1455/auth/callback?code=abc",
		"http://evil.test/auth/callback?code=abc",
		"http://127.0.0.1:1455/auth/callback",
		"",
	} {
		if _, err := ParseCallbackURL(bad); err == nil {
			t.Fatalf("callback %q accepted", bad)
		}
	}
}

// The listener binds loopback, answers the browser and hands the result to the
// waiting flow.
func TestCallbackListener(t *testing.T) {
	listener, err := ListenCallback()
	if err != nil {
		// `go test ./...` runs the packages in parallel, and another package's
		// OAuth test may hold the pinned pair for its duration; the pinning
		// itself is proven in oauthbase's listener test.
		t.Skipf("pinned ports busy: %v", err)
	}
	defer listener.Close()
	if !strings.HasPrefix(listener.RedirectURI, "http://127.0.0.1:") || !strings.HasSuffix(listener.RedirectURI, CallbackPath) {
		t.Fatalf("redirect %q", listener.RedirectURI)
	}
	waited := make(chan CallbackResult, 1)
	go func() {
		result, err := listener.Wait(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		waited <- result
	}()
	resp, err := testClient().Get(listener.RedirectURI + "?code=code-1&state=st-1&client_id=oaiapp_listen")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("browser page: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	select {
	case result := <-waited:
		if result.Code != "code-1" || result.State != "st-1" || result.ClientID != "oaiapp_listen" {
			t.Fatalf("%+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not deliver the callback")
	}
	// A timeout is not a failure of the flow: the paste fallback outlives it.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := listener.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait after delivery: %v", err)
	}
}

// staticKeys is a keySource over one fixed RSA key: the id_token check is
// about the verification, not about standing up a JWKS server for it.
type staticKeys struct {
	key *rsa.PublicKey
	kid string
}

func (s staticKeys) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if kid != s.kid {
		return nil, errors.New("unknown kid")
	}
	return s.key, nil
}

func signIDToken(t *testing.T, key *rsa.PrivateKey, kid, alg string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func validClaims(nonce, issuer, client string) map[string]any {
	return map[string]any{
		"iss": issuer, "sub": "user-subject-1", "aud": client,
		"exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce,
		"email": "user@example.test", "name": "User",
	}
}

func TestVerifyIDToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := staticKeys{key: &key.PublicKey, kid: "kid-1"}
	issuer := "https://auth.openai.com"
	const nonce, client = "nonce-abc", "oaiapp_verify"
	good := signIDToken(t, key, "kid-1", "RS256", validClaims(nonce, issuer, client))
	claims, err := VerifyIDToken(context.Background(), testClient(), keys, issuer, good, nonce, client)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "user-subject-1" || claims.Email != "user@example.test" {
		t.Fatalf("%+v", claims)
	}
	// A kid the issuer did not publish.
	if _, err := VerifyIDToken(context.Background(), testClient(), keys, issuer, signIDToken(t, key, "kid-2", "RS256", validClaims(nonce, issuer, client)), nonce, client); err == nil {
		t.Fatal("unknown kid accepted")
	}
	// An alg the verifier did not choose.
	if _, err := VerifyIDToken(context.Background(), testClient(), keys, issuer, signIDToken(t, key, "kid-1", "none", validClaims(nonce, issuer, client)), nonce, client); err == nil {
		t.Fatal("alg none accepted")
	}
	// A signature from another key.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyIDToken(context.Background(), testClient(), keys, issuer, signIDToken(t, other, "kid-1", "RS256", validClaims(nonce, issuer, client)), nonce, client); err == nil {
		t.Fatal("foreign signature accepted")
	}
	// Each claim branch.
	branches := map[string]func(map[string]any){
		"nonce":          func(c map[string]any) { c["nonce"] = "other" },
		"missing":        func(c map[string]any) { delete(c, "nonce") },
		"issuer":         func(c map[string]any) { c["iss"] = "https://evil.test" },
		"expired":        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"audience":       func(c map[string]any) { c["aud"] = "oaiapp_other" },
		"no audience":    func(c map[string]any) { delete(c, "aud") },
		"empty audience": func(c map[string]any) { c["aud"] = "" },
		"no subject":     func(c map[string]any) { delete(c, "sub") },
	}
	for name, mutate := range branches {
		claims := validClaims(nonce, issuer, client)
		mutate(claims)
		token := signIDToken(t, key, "kid-1", "RS256", claims)
		if _, err := VerifyIDToken(context.Background(), testClient(), keys, issuer, token, nonce, client); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// A string audience and an array audience both work.
	for _, aud := range []any{client, []any{client}} {
		claims := validClaims(nonce, issuer, client)
		claims["aud"] = aud
		token := signIDToken(t, key, "kid-1", "RS256", claims)
		if _, err := VerifyIDToken(context.Background(), testClient(), keys, issuer, token, nonce, client); err != nil {
			t.Fatalf("audience %v rejected: %v", aud, err)
		}
	}
	// A token that is not a JWT at all.
	if _, err := VerifyIDToken(context.Background(), testClient(), keys, issuer, "not-a-jwt", nonce, client); err == nil {
		t.Fatal("garbage accepted")
	}
}

// The scope gate is an exact-token match: chatgpt.tokens.use is a different
// permission, and a consent that was refused simply lacks the entry.
func TestHasDirectUse(t *testing.T) {
	if !HasDirectUse(Scope) {
		t.Fatal("the documented scope set does not include the direct-use scope")
	}
	if HasDirectUse("openid profile email offline_access resource.invoke") {
		t.Fatal("a refused consent reads as granted")
	}
	if HasDirectUse("chatgpt.tokens.use") {
		t.Fatal("a near-miss scope matched")
	}
	if HasDirectUse("") {
		t.Fatal("empty matched")
	}
}

// fakeTokenServer answers the token endpoint and records the forms it saw.
type fakeTokenServer struct {
	*httptest.Server
	forms    []url.Values
	exchange atomic.Int32
	reply    func(TokenResponse) TokenResponse
}

func newFakeTokenServer(t *testing.T) *fakeTokenServer {
	t.Helper()
	f := &fakeTokenServer{}
	f.reply = func(r TokenResponse) TokenResponse { return r }
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/oauth/token" {
			w.WriteHeader(404)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		f.exchange.Add(1)
		f.forms = append(f.forms, r.PostForm)
		reply := f.reply(TokenResponse{
			AccessToken:  "at-1",
			RefreshToken: "rt-1",
			IDToken:      "id-1",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
			Scope:        Scope,
			ClientID:     "oaiapp_exchange",
		})
		if reply.Error != "" {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(f.Close)
	return f
}

// The exchange carries the PKCE verifier, the resource and no client secret —
// that is the whole point of the public-client flow. It authenticates as the
// client the callback issued, never as the registration entry point.
func TestExchangeCodeSendsVerifierWithoutSecret(t *testing.T) {
	f := newFakeTokenServer(t)
	eps := testEndpoints(f.Server)
	auth := mustAuthorize(t, eps, DynamicAppID, "urn:uuid:host", "http://127.0.0.1:1/auth/callback", IDTokenHint{})
	reply, err := ExchangeCode(context.Background(), testClient(), eps, auth, "code-xyz", "oaiapp_issued")
	if err != nil {
		t.Fatal(err)
	}
	if reply.AccessToken != "at-1" || reply.RefreshToken != "rt-1" || reply.ClientID != "oaiapp_exchange" {
		t.Fatalf("%+v", reply)
	}
	if len(f.forms) != 1 {
		t.Fatalf("forms %d", len(f.forms))
	}
	form := f.forms[0]
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "code-xyz" || form.Get("code_verifier") != auth.Verifier {
		t.Fatalf("form %v", form)
	}
	if form.Get("client_id") != "oaiapp_issued" || form.Get("resource") != Resource {
		t.Fatalf("form %v", form)
	}
	if _, ok := form["client_secret"]; ok {
		t.Fatal("exchange sent a client secret")
	}
	// The registration entry point is not a client an exchange may use.
	for _, issued := range []string{"", DynamicAppID} {
		if _, err := ExchangeCode(context.Background(), testClient(), eps, auth, "code-xyz", issued); err == nil {
			t.Fatalf("the exchange accepted client %q", issued)
		}
	}
	// Every rotation-relevant rejection maps to one error.
	for _, code := range []string{"invalid_grant", "refresh_token_reused", "refresh_token_invalidated"} {
		f.reply = func(TokenResponse) TokenResponse { return TokenResponse{Error: code} }
		if _, err := ExchangeCode(context.Background(), testClient(), eps, auth, "code", "oaiapp_issued"); !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("%s: %v", code, err)
		}
	}
	f.reply = func(TokenResponse) TokenResponse { return TokenResponse{Error: "server_error"} }
	if _, err := ExchangeCode(context.Background(), testClient(), eps, auth, "code", "oaiapp_issued"); err == nil || errors.Is(err, ErrReauthRequired) {
		t.Fatalf("a server error must not read as a dead credential: %v", err)
	}
}

// The catalogue is models[]{slug, visibility}: a slug is what the Responses API
// accepts, so a slug is what a channel binds, and an entry the profile does not
// list is not offered.
func TestDiscoverModelsFiltersVisibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(404)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
			t.Errorf("authorization %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{"slug": "gpt-5", "display_name": "GPT-5", "visibility": "list"},
			{"slug": "gpt-5-codex", "display_name": "GPT-5 Codex", "visibility": "list"},
			{"slug": "hidden-model", "display_name": "Hidden", "visibility": "internal"},
			{"slug": "", "visibility": "list"},
		}})
	}))
	defer server.Close()
	models, err := DiscoverModels(context.Background(), testClient(), server.URL+"/v1/models", "at-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "gpt-5" || models[1] != "gpt-5-codex" {
		t.Fatalf("models %v", models)
	}
}

func siwcChannel(t *testing.T, s *store.Store, client, host, subject string) store.Channel {
	t.Helper()
	c := store.Channel{Name: "chatgpt", CredentialType: store.CredentialChatGPTOAuth, Enabled: true,
		Models:    []string{"gpt-5"},
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"}},
		OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderChatGPT, RefreshToken: "rt-stored", IDToken: "id-stored",
			ClientID: client, HostID: host, Subject: subject, Scopes: Scope,
			AccessToken: "at-stored", ExpiresAt: time.Now().Add(-time.Minute).UTC(),
			AccountLabel: "user@example.test", APIBase: "https://api.openai.com/v1", Status: store.OAuthStatusActive,
		}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// G017: two concurrent refreshes must cost one rotation, and the locked-out
// callers re-read the stored row instead of exchanging again.
func TestConcurrentRefreshExchangesOnce(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var rotations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/jwks.json") {
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
				"kid": "kid-1", "alg": "RS256", "kty": "RSA",
				"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(bigInt(key.PublicKey.E)),
			}}})
			return
		}
		if strings.Contains(r.URL.Path, "/oauth/token") {
			rotations.Add(1)
			// Slow enough that the first goroutine is still inside the lock
			// when the others queue up behind it.
			time.Sleep(50 * time.Millisecond)
			json.NewEncoder(w).Encode(TokenResponse{
				AccessToken: "at-rotated", RefreshToken: "rt-rotated", IDToken: "id-rotated",
				ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_rot",
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(server))
	c := siwcChannel(t, s, "oaiapp_rot", "urn:uuid:host", "sub-rot")
	const goroutines = 8
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = svc.Bearer(context.Background(), c)
		}(i)
	}
	close(start)
	wg.Wait()
	if got := rotations.Load(); got != 1 {
		t.Fatalf("rotated %d times for one expiry window", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if tokens[i] != "at-rotated" {
			t.Fatalf("goroutine %d got %q", i, tokens[i])
		}
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.AccessToken != "at-rotated" || stored.OAuth.RefreshToken != "rt-rotated" {
		t.Fatalf("rotation did not land: %+v", stored.OAuth)
	}
	if stored.Generation != c.Generation {
		t.Fatalf("rotation bumped the generation to %d", stored.Generation)
	}
	// The row is fresh now, so nothing exchanges again.
	if _, err = svc.Bearer(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := rotations.Load(); got != 1 {
		t.Fatalf("exchanged again for a fresh token: %d", got)
	}
}

// The access token's one-hour lifecycle decides the skew: a token with time
// left is used as-is, one inside the skew window is exchanged.
func TestBearerSkew(t *testing.T) {
	f := newFakeTokenServer(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	c := siwcChannel(t, s, "oaiapp_skew", "urn:uuid:host", "sub-skew")
	if err = s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "at-soon", ExpiresAt: time.Now().Add(5 * time.Minute).UTC()}); err != nil {
		t.Fatal(err)
	}
	if token, err := svc.Bearer(context.Background(), c); err != nil || token != "at-soon" {
		t.Fatalf("%q %v", token, err)
	}
	if f.exchange.Load() != 0 {
		t.Fatal("exchanged a token with time left")
	}
	if err = s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "at-nearly", ExpiresAt: time.Now().Add(30 * time.Second).UTC()}); err != nil {
		t.Fatal(err)
	}
	if token, err := svc.Bearer(context.Background(), c); err != nil || token != "at-1" {
		t.Fatalf("%q %v", token, err)
	}
	if f.exchange.Load() != 1 {
		t.Fatalf("exchanged %d times", f.exchange.Load())
	}
}

// A credential the issuer rejected is marked and never exchanged again; a
// consent that lost the direct-use scope is marked with its reason.
func TestBearerMarksReauthAndConsent(t *testing.T) {
	f := newFakeTokenServer(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	c := siwcChannel(t, s, "oaiapp_mark", "urn:uuid:host", "sub-mark")
	f.reply = func(TokenResponse) TokenResponse { return TokenResponse{Error: "invalid_grant"} }
	if _, err := svc.Bearer(context.Background(), c); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("%v", err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusReauthRequired || stored.OAuth.StatusReason == "" || !stored.Enabled {
		t.Fatalf("status not recorded: %+v enabled=%v", stored.OAuth, stored.Enabled)
	}
	if _, err = svc.Bearer(context.Background(), c); err == nil || !strings.Contains(err.Error(), "authorization again") {
		t.Fatalf("the mark did not stop the retries: %v", err)
	}
	if f.exchange.Load() != 1 {
		t.Fatalf("exchanged %d times", f.exchange.Load())
	}
	// The consent branch: a rotation whose scope set lost the direct-use entry.
	f.reply = func(r TokenResponse) TokenResponse {
		r.Error = ""
		r.Scope = "openid profile email offline_access resource.invoke"
		return r
	}
	c2 := siwcChannel(t, s, "oaiapp_consent", "urn:uuid:host", "sub-consent")
	if _, err := svc.Bearer(context.Background(), c2); !errors.Is(err, ErrConsentMissingScope) {
		t.Fatalf("%v", err)
	}
	stored, err = s.Channel(c2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusReauthRequired || !strings.Contains(stored.OAuth.StatusReason, "consent_missing_scope") {
		t.Fatalf("consent mark not recorded: %+v", stored.OAuth)
	}
	if stored.OAuth.AccessToken != "at-stored" {
		t.Fatal("a refused rotation replaced the stored token")
	}
}

// An SIWC channel never answers the Copilot identity lookup: its identity is
// the (client, host, account) triple, not a GitHub token.
func TestBearerRejectsForeignChannels(t *testing.T) {
	f := newFakeTokenServer(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	plain := store.Channel{Name: "plain", APIKey: "sk-test", Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://example.test/v1"}}}
	if err = s.SaveChannel(&plain); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Bearer(context.Background(), plain); err == nil {
		t.Fatal("api_key channel answered with a bearer")
	}
}

func bigInt(e int) []byte {
	out := []byte{}
	for v := e; v > 0; v >>= 8 {
		out = append([]byte{byte(v)}, out...)
	}
	if len(out) == 0 {
		return []byte{0}
	}
	return out
}

// fakeSIWC is the issuer plus the inference host: JWKS, userinfo, catalogue.
type fakeSIWC struct {
	*httptest.Server
	key       *rsa.PrivateKey
	models    []string
	userinfo  int
	catalogue int
}

func newFakeSIWC(t *testing.T) *fakeSIWC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSIWC{key: key, models: []string{"gpt-5", "gpt-5-codex"}, userinfo: http.StatusOK, catalogue: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/jwks.json":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
				"kid": "kid-1", "alg": "RS256", "kty": "RSA",
				"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(bigInt(key.PublicKey.E)),
			}}})
		case r.URL.Path == "/api/accounts/oauth/userinfo":
			if f.userinfo != http.StatusOK {
				w.WriteHeader(f.userinfo)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"email": "person@example.test", "name": "Person"})
		case r.URL.Path == "/v1/models":
			if f.catalogue != http.StatusOK {
				w.WriteHeader(f.catalogue)
				return
			}
			entries := []map[string]any{}
			for _, id := range f.models {
				entries = append(entries, map[string]any{"slug": id, "display_name": id, "visibility": "list"})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": entries})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSIWC) loginInput(t *testing.T, auth AuthRequest, tokens TokenResponse, callbackID string) LoginInput {
	t.Helper()
	// The audience is whatever the flow resolved: the token response's own
	// registration, the callback's, or the client the flow was started as.
	audience := firstNonEmpty(tokens.ClientID, callbackID, auth.ClientID)
	tokens.IDToken = signIDToken(t, f.key, "kid-1", "RS256", validClaims(auth.Nonce, f.URL, audience))
	return LoginInput{Auth: auth, CallbackID: callbackID, Tokens: tokens}
}

// A good authorization becomes a channel: the Responses endpoint on the
// documented host, the picker-visible models, and the credential fields the
// gateway rotates with.
func TestCompleteLoginCreatesChannel(t *testing.T) {
	// The channel this login builds points at the local double, so the
	// documented testing escape hatch for the OAuth host allowlist is set
	// explicitly; see store.OAuthHostAllowed.
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeSIWC(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	host, err := s.HostID()
	if err != nil {
		t.Fatal(err)
	}
	auth := mustAuthorize(t, svc.Endpoints, "oaiapp_good", host, "http://127.0.0.1:1/auth/callback", IDTokenHint{})
	in := f.loginInput(t, auth, TokenResponse{
		AccessToken: "at-good", RefreshToken: "rt-good", ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_good",
	}, "oaiapp_good")
	label, err := svc.Account(context.Background(), in.Tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	in.AccountLabel = label
	login, err := svc.CompleteLogin(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := login.Channel
	if c.Name != "ChatGPT plan" {
		t.Fatalf("name %q", c.Name)
	}
	if c.CredentialType != store.CredentialChatGPTOAuth || len(c.Endpoints) != 1 || c.Endpoints[0].Format != store.FormatOpenAIResponses {
		t.Fatalf("channel %+v", c)
	}
	if c.Endpoints[0].BaseURL != svc.Endpoints.Inference {
		t.Fatalf("base %q", c.Endpoints[0].BaseURL)
	}
	if len(c.Models) != 2 || c.Models[0] != "gpt-5" {
		t.Fatalf("models %v", c.Models)
	}
	if c.OAuth.ClientID != "oaiapp_good" || c.OAuth.HostID != host || c.OAuth.Subject != "user-subject-1" {
		t.Fatalf("credential identity %+v", c.OAuth)
	}
	if c.OAuth.RefreshToken != "rt-good" || c.OAuth.AccessToken != "at-good" || c.OAuth.Status != store.OAuthStatusActive {
		t.Fatalf("credential tokens %+v", c.OAuth)
	}
	if !strings.Contains(c.OAuth.Scopes, ScopeDirect) {
		t.Fatalf("scopes %q", c.OAuth.Scopes)
	}
	if c.OAuth.AccountLabel != "person@example.test" {
		t.Fatalf("label %q", c.OAuth.AccountLabel)
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatalf("the constructed channel must validate: %v", err)
	}
	c.Name = "My plan person@example.test"
	if err := s.SaveChannelConfig(&c); err != nil {
		t.Fatal(err)
	}
	in.Tokens.AccessToken = "reauth-access"
	in.Tokens.RefreshToken = "reauth-refresh"
	in.ExpectedChannelID = c.ID
	in.ExpectedSubject = c.OAuth.Subject
	again, err := svc.CompleteLogin(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyLinked || again.ChannelID != c.ID {
		t.Fatal("reauthorization did not reuse the channel")
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != c.Name || stored.OAuth.AccountLabel != "person@example.test" || stored.OAuth.RefreshToken != "reauth-refresh" {
		t.Fatal("reauthorization changed the chosen name or account metadata, or failed to replace the credential")
	}
}

// The branches that refuse a login: a first authorization whose callback did
// not carry the issued client, a registration that came back different from the
// one the flow used, a consent that was refused, an id_token that does not
// belong to this flow, and a scope set nothing evidenced.
func TestCompleteLoginRefusals(t *testing.T) {
	// The channels this test builds point at the local double, so the documented
	// testing escape hatch for the OAuth host allowlist is set explicitly.
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeSIWC(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	host, _ := s.HostID()
	first := mustAuthorize(t, svc.Endpoints, DynamicAppID, host, "http://127.0.0.1:1/auth/callback", IDTokenHint{})
	reauth := mustAuthorize(t, svc.Endpoints, "oaiapp_existing", host, "http://127.0.0.1:1/auth/callback", IDTokenHint{})
	good := TokenResponse{AccessToken: "at-x", RefreshToken: "rt-x", ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_good"}

	// First authorization, callback carries the issued client: accepted, and the
	// channel it returns is one the store takes.
	ok := f.loginInput(t, first, good, "oaiapp_callback")
	accepted, err := svc.CompleteLogin(context.Background(), ok)
	if err != nil {
		t.Fatalf("a well-formed first authorization refused: %v", err)
	}
	if accepted.AlreadyLinked {
		t.Fatal("a first authorization reported already_linked")
	}
	if err := s.SaveChannel(&accepted.Channel); err != nil {
		t.Fatalf("the channel a good login returns does not validate: %v", err)
	}

	// First authorization without the issued client: refused. The registration
	// entry point cannot be exchanged, so there is nothing to fall back to.
	missing := f.loginInput(t, first, TokenResponse{AccessToken: "at-x", RefreshToken: "rt-x", ExpiresIn: 3600, Scope: Scope}, "")
	if _, err := svc.CompleteLogin(context.Background(), missing); err == nil || !strings.Contains(err.Error(), "client_id_missing") {
		t.Fatalf("first authorization without an issued client: %v", err)
	}
	// An issued id that is not an oaiapp registration is refused the same way.
	notRegistered := f.loginInput(t, first, TokenResponse{AccessToken: "at-x", RefreshToken: "rt-x", ExpiresIn: 3600, Scope: Scope}, "some_other_client")
	if _, err := svc.CompleteLogin(context.Background(), notRegistered); err == nil || !strings.Contains(err.Error(), "client_id_missing") {
		t.Fatalf("a non-oaiapp client: %v", err)
	}

	// Re-authorization: the client the flow used is the baseline, and a
	// disagreeing source is refused rather than adopted.
	disagree := f.loginInput(t, reauth, TokenResponse{AccessToken: "at-x", RefreshToken: "rt-x", ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_other"}, "oaiapp_other")
	if _, err := svc.CompleteLogin(context.Background(), disagree); err == nil || !strings.Contains(err.Error(), "client_id_mismatch") {
		t.Fatalf("disagreeing registration: %v", err)
	}
	// A re-authorization whose callback omits the client is fine: the baseline
	// stands, and no client_id_missing is raised for it.
	quiet := f.loginInput(t, reauth, TokenResponse{AccessToken: "at-x", RefreshToken: "rt-x", ExpiresIn: 3600, Scope: Scope}, "")
	if _, err := svc.CompleteLogin(context.Background(), quiet); err != nil {
		t.Fatalf("a quiet callback on a re-authorization refused: %v", err)
	}

	// Refused consent, undetermined consent, foreign nonce.
	refused := f.loginInput(t, first, TokenResponse{AccessToken: "at-x", RefreshToken: "rt-x", ExpiresIn: 3600, Scope: "openid profile email offline_access resource.invoke", ClientID: "oaiapp_good"}, "oaiapp_good")
	if _, err := svc.CompleteLogin(context.Background(), refused); !errors.Is(err, ErrConsentMissingScope) {
		t.Fatalf("refused consent: %v", err)
	}
	undetermined := f.loginInput(t, first, TokenResponse{AccessToken: "at-x", RefreshToken: "rt-x", ExpiresIn: 3600, ClientID: "oaiapp_good"}, "oaiapp_good")
	if _, err := svc.CompleteLogin(context.Background(), undetermined); !errors.Is(err, ErrScopeUndetermined) {
		t.Fatalf("undetermined consent: %v", err)
	}
	foreign := f.loginInput(t, first, good, "oaiapp_good")
	foreign.Tokens.IDToken = signIDToken(t, f.key, "kid-1", "RS256", validClaims("nonce-from-another-flow", f.URL, "oaiapp_good"))
	if _, err := svc.CompleteLogin(context.Background(), foreign); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("foreign nonce: %v", err)
	}
	if channels, _ := s.ListChannels(); len(channels) != 1 {
		t.Fatalf("refused logins stored %d channels", len(channels))
	}
}

// P2-2: a rotation whose write fails once is re-fetched with the live token and
// stored — because the refresh token the exchange used is already dead, keeping
// the stored one would guarantee an invalid_grant on the next attempt.
func TestRotationReFetchesWhenTheWriteFails(t *testing.T) {
	var rotations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/oauth/token") {
			w.WriteHeader(404)
			return
		}
		n := rotations.Add(1)
		json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: fmt.Sprintf("at-attempt-%d", n), RefreshToken: fmt.Sprintf("rt-attempt-%d", n),
			ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_retry",
		})
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(server))
	c := siwcChannel(t, s, "oaiapp_retry", "urn:uuid:host", "sub-retry")
	// Two writes fail: the store's own retry absorbs the first (Q2a), so it is
	// the second that reaches the service and triggers the re-fetch.
	store.FailPersistWrites(2)
	token, err := svc.Bearer(context.Background(), c)
	if err != nil {
		t.Fatalf("the rotation was not recovered: %v", err)
	}
	// The second rotation's token is what got stored, not the first one's.
	if token != "at-attempt-2" {
		t.Fatalf("served %q", token)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.AccessToken != "at-attempt-2" || stored.OAuth.RefreshToken != "rt-attempt-2" {
		t.Fatalf("stored %+v", stored.OAuth)
	}
	if stored.OAuth.Status != store.OAuthStatusActive {
		t.Fatalf("the credential was marked: %+v", stored.OAuth)
	}
	if rotations.Load() != 2 {
		t.Fatalf("rotated %d times", rotations.Load())
	}
}

// The re-fetch failing is fatal and marks the credential, rather than leaving a
// dead refresh token in place.
func TestRotationMarksReauthWhenReFetchFails(t *testing.T) {
	var rotations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/oauth/token") {
			w.WriteHeader(404)
			return
		}
		n := rotations.Add(1)
		if n == 2 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(TokenResponse{Error: "invalid_grant"})
			return
		}
		json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "at-once", RefreshToken: "rt-once", ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_refetch",
		})
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(server))
	c := siwcChannel(t, s, "oaiapp_refetch", "urn:uuid:host", "sub-refetch")
	store.FailPersistWrites(2)
	_, err = svc.Bearer(context.Background(), c)
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err %v", err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusReauthRequired {
		t.Fatalf("status %+v", stored.OAuth)
	}
	if rotations.Load() != 2 {
		t.Fatalf("rotated %d times", rotations.Load())
	}
}

// P2: a key rotation must not read as an outage. A kid the cache has never seen
// forces a refetch even inside the TTL, and a known kid is served from it.
func TestJWKSUnknownKidForcesRefetch(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	current := map[string]*rsa.PublicKey{"kid-1": &key.PublicKey}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			w.WriteHeader(404)
			return
		}
		fetches.Add(1)
		entries := []map[string]any{}
		for kid, public := range current {
			entries = append(entries, map[string]any{
				"kid": kid, "alg": "RS256", "kty": "RSA",
				"n": base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(bigInt(public.E)),
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": entries})
	}))
	defer server.Close()
	eps := DefaultEndpoints()
	eps.JWKS = server.URL + "/.well-known/jwks.json"
	cache := newJWKSCache(eps.JWKS, testClient())
	ctx := context.Background()
	if _, err := cache.Key(ctx, "kid-1"); err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("fetches %d", fetches.Load())
	}
	// A known kid inside the TTL is served from the cache.
	if _, err := cache.Key(ctx, "kid-1"); err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("a known kid refetched: %d", fetches.Load())
	}
	// The issuer rotates: the new kid is unknown, so the fetch bypasses the TTL
	// even though the hour has not run out.
	current["kid-2"] = &rotated.PublicKey
	if _, err := cache.Key(ctx, "kid-2"); err != nil {
		t.Fatalf("the rotated key was not reachable: %v", err)
	}
	if fetches.Load() != 2 {
		t.Fatalf("fetches %d, want 2", fetches.Load())
	}
	// A kid the issuer never published is still a refusal.
	if _, err := cache.Key(ctx, "kid-absent"); err == nil {
		t.Fatal("an unpublished kid resolved")
	}
}

// 必要 1: a re-authorization is bound to the account that started it. Signing in
// as a different user in the browser is refused before anything is created or
// rotated.
func TestCompleteLoginRejectsIdentityMismatch(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeSIWC(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	host, _ := s.HostID()
	reauth := mustAuthorize(t, svc.Endpoints, "oaiapp_bound", host, "http://127.0.0.1:1/auth/callback", IDTokenHint{IDToken: "stored-id-token"})
	// The flow asks for account A.
	in := f.loginInput(t, reauth, TokenResponse{AccessToken: "at-x", RefreshToken: "rt-x", ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_bound"}, "oaiapp_bound")
	in.ExpectedSubject = "account-A"
	_, err = svc.CompleteLogin(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "identity_mismatch") {
		t.Fatalf("the signed-in account did not have to match: %v", err)
	}
	if channels, _ := s.ListChannels(); len(channels) != 0 {
		t.Fatal("an identity mismatch created a channel")
	}
	// The same flow with the expected subject succeeds, and a channel with no
	// recorded subject is not bound at all (it could never be re-authorized
	// otherwise).
	in.ExpectedSubject = ""
	login, err := svc.CompleteLogin(context.Background(), in)
	if err != nil {
		t.Fatalf("an unbound flow refused: %v", err)
	}
	_ = login
}

// 必要 2: a rotation writes the granted scope set in the same statement as the
// tokens, so the stored set never describes the previous consent.
func TestRotationWritesGrantedScopes(t *testing.T) {
	var rotations atomic.Int32
	f := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/oauth/token") {
			w.WriteHeader(404)
			return
		}
		rotations.Add(1)
		json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "at-rotated", RefreshToken: "rt-rotated", ExpiresIn: 3600, Scope: Scope,
		})
	}))
	defer f.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f))
	c := siwcChannel(t, s, "oaiapp_scopes", "urn:uuid:host", "sub-scopes")
	before, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before.OAuth.Scopes, ScopeDirect) {
		t.Fatalf("the stored scope set lost the direct scope: %q", before.OAuth.Scopes)
	}
	token, err := svc.Bearer(context.Background(), c)
	if err != nil || token != "at-rotated" {
		t.Fatalf("%q %v", token, err)
	}
	after, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OAuth.Scopes != "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct" {
		t.Fatalf("scopes not written with the rotation: %q", after.OAuth.Scopes)
	}
}

// 必要 3: a successful refresh that returns no replacement refresh token is a
// dead credential, not a credential with a stale token behind it.
func TestRefreshWithoutReplacementTokenMarksReauth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/oauth/token") {
			w.WriteHeader(404)
			return
		}
		// No refresh_token in the reply.
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "at-only", ExpiresIn: 3600, Scope: Scope})
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(server))
	c := siwcChannel(t, s, "oaiapp_norefresh", "urn:uuid:host", "sub-norefresh")
	if _, err := svc.Bearer(context.Background(), c); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err %v", err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusReauthRequired || stored.OAuth.AccessToken != "at-stored" {
		t.Fatalf("the credential was stored or not marked: %+v", stored.OAuth)
	}
}

// 必要 3: the terminal error set, and invalid_client as its own configuration
// failure rather than a re-authorization prompt.
func TestRefreshTerminalErrors(t *testing.T) {
	for _, code := range []string{"invalid_grant", "refresh_token_reused", "refresh_token_invalidated", "invalid_refresh_token", "token_expired", "refresh_token_expired"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(TokenResponse{Error: code})
		}))
		s, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		svc := NewService(s, testClient(), testEndpoints(server))
		c := siwcChannel(t, s, "oaiapp_terminal", "urn:uuid:host", "sub-terminal")
		_, err = svc.Bearer(context.Background(), c)
		if !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("%s: %v", code, err)
		}
		stored, _ := s.Channel(c.ID)
		if stored.OAuth.Status != store.OAuthStatusReauthRequired {
			t.Fatalf("%s: not marked", code)
		}
		s.Close()
		server.Close()
	}
	// invalid_client is a configuration problem: it stops the refresh and is
	// marked, but the reason says the client is wrong rather than the consent.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(TokenResponse{Error: "invalid_client"})
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(server))
	c := siwcChannel(t, s, "oaiapp_client", "urn:uuid:host", "sub-client")
	if _, err := svc.Bearer(context.Background(), c); !errors.Is(err, ErrClientMisconfigured) {
		t.Fatalf("err %v", err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusReauthRequired || !strings.Contains(stored.OAuth.StatusReason, "client id") {
		t.Fatalf("status %+v", stored.OAuth)
	}
}

// 1: a re-authorization of a channel the issuer had rejected clears the mark and
// makes the credential usable again — the rotation path alone would leave the
// gateway refusing a channel the owner just re-authorized.
func TestReauthorizationClearsReauthRequired(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeSIWC(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	host, _ := s.HostID()
	c := siwcChannel(t, s, "oaiapp_reauth", host, "user-subject-1")
	if err := s.SetOAuthStatus(c.ID, store.OAuthStatusReauthRequired, "invalid_grant", false); err != nil {
		t.Fatal(err)
	}
	// The channel is unusable until something writes "active" back.
	if _, err := svc.Bearer(context.Background(), c); err == nil {
		t.Fatal("a rejected credential answered a bearer")
	}
	// A re-authorization of the same account.
	auth := mustAuthorize(t, svc.Endpoints, "oaiapp_reauth", host, "http://127.0.0.1:1/auth/callback", IDTokenHint{})
	in := f.loginInput(t, auth, TokenResponse{
		AccessToken: "at-reauthorized", RefreshToken: "rt-reauthorized",
		ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_reauth",
	}, "oaiapp_reauth")
	in.ExpectedSubject = "user-subject-1"
	login, err := svc.CompleteLogin(context.Background(), in)
	if err != nil {
		t.Fatalf("the re-authorization refused: %v", err)
	}
	if !login.AlreadyLinked || login.ChannelID != c.ID {
		t.Fatalf("the re-authorization did not reuse the channel: %+v", login)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusActive || stored.OAuth.StatusReason != "" {
		t.Fatalf("the mark survived the re-authorization: %+v", stored.OAuth)
	}
	if stored.OAuth.AccessToken != "at-reauthorized" || stored.OAuth.RefreshToken != "rt-reauthorized" {
		t.Fatalf("tokens not written: %+v", stored.OAuth)
	}
	if !strings.Contains(stored.OAuth.Scopes, ScopeDirect) {
		t.Fatalf("scopes not written: %q", stored.OAuth.Scopes)
	}
	// And the credential is usable again.
	if token, err := svc.Bearer(context.Background(), stored); err != nil || token != "at-reauthorized" {
		t.Fatalf("%q %v", token, err)
	}
}

// 3: the re-fetch after a failed write goes through the same validation as a
// normal refresh — a second exchange that returns no replacement refresh token
// fails the credential rather than being stored.
func TestRefetchWithoutReplacementTokenMarksReauth(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var rotations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/oauth/token") {
			w.WriteHeader(404)
			return
		}
		n := rotations.Add(1)
		if n == 1 {
			json.NewEncoder(w).Encode(TokenResponse{AccessToken: "at-once", RefreshToken: "rt-once", ExpiresIn: 3600, Scope: Scope})
			return
		}
		// The re-fetch succeeds but issues no replacement refresh token.
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "at-second", ExpiresIn: 3600, Scope: Scope})
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(server))
	c := siwcChannel(t, s, "oaiapp_refetch", "urn:uuid:host", "sub-refetch")
	store.FailPersistWrites(2)
	_, err = svc.Bearer(context.Background(), c)
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err %v", err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusReauthRequired || stored.OAuth.AccessToken != "at-stored" {
		t.Fatalf("the dead credential was stored or not marked: %+v", stored.OAuth)
	}
	if rotations.Load() != 2 {
		t.Fatalf("rotated %d times", rotations.Load())
	}
}

// 1: the lifecycle lock serializes a delete against a refresh, so the revoke
// sees the token as it stands rather than the one an earlier snapshot carried.
// Interleaving them by hand: a refresh rotates rt0 into rt1, then the delete
// must revoke rt1 — revoking rt0 would report "revoked" while the newer token
// stays live at the issuer.
func TestDisconnectRevokesTheCurrentRefreshToken(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var rotations atomic.Int32
	var revoked atomic.Value
	revoked.Store("")
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/oauth/revoke"):
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			revoked.Store(r.Form.Get("token"))
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/oauth/token"):
			n := rotations.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			json.NewEncoder(w).Encode(TokenResponse{
				AccessToken: fmt.Sprintf("at-%d", n), RefreshToken: fmt.Sprintf("rt-%d", n),
				ExpiresIn: 3600, Scope: Scope,
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer issuer.Close()
	eps := testEndpoints(issuer)
	eps.Revoke = issuer.URL + "/api/accounts/oauth/revoke"
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), eps)
	c := siwcChannel(t, s, "oaiapp_lifecycle", "urn:uuid:host", "sub-lifecycle")
	// A refresh rotates the stored token first.
	if _, err := svc.Bearer(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.RefreshToken != "rt-1" {
		t.Fatalf("the rotation did not land: %q", stored.OAuth.RefreshToken)
	}
	// The disconnect re-reads inside the lock and revokes what is stored now.
	if err := svc.DisconnectChannel(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	if got := revoked.Load().(string); got != "rt-1" {
		t.Fatalf("the revoke used %q, want the current token", got)
	}
	if _, err := s.Channel(c.ID); err != store.ErrNotFound {
		t.Fatalf("the channel survived the disconnect: %v", err)
	}
}

// 1: the same lock holds for a re-authorization landing while an old session
// rotates: the final stored credential is the re-authorized one, and the stale
// rotation does not overwrite it.
func TestReauthorizationWinsOverConcurrentRotation(t *testing.T) {
	testReauthorizationWithInFlightRefresh(t)
}

// 2: a code exchange with no refresh token is refused before anything is
// stored — the credential could not be renewed, so it would only look
// configured for an hour.
func TestCompleteLoginRejectsMissingRefreshToken(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeSIWC(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	host, _ := s.HostID()
	auth := mustAuthorize(t, svc.Endpoints, DynamicAppID, host, "http://127.0.0.1:1/auth/callback", IDTokenHint{})
	in := f.loginInput(t, auth, TokenResponse{
		AccessToken: "at-only", ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_norenew",
	}, "oaiapp_norenew")
	_, err = svc.CompleteLogin(context.Background(), in)
	if !errors.Is(err, ErrRenewableSessionMissing) {
		t.Fatalf("err %v", err)
	}
	if channels, _ := s.ListChannels(); len(channels) != 0 {
		t.Fatal("an unrenewable credential was stored")
	}
}

// The rejection's detail is the issuer's own words after the sentinel, and a
// bare sentinel carries none — repeating it beside the console's own advice
// would say the same thing twice.
func TestDetailOf(t *testing.T) {
	wrapped := fmt.Errorf("%w: invalid_grant: the code has already been redeemed", ErrReauthRequired)
	if got := DetailOf(wrapped); got != "invalid_grant: the code has already been redeemed" {
		t.Fatalf("wrapped detail %q", got)
	}
	client := fmt.Errorf("%w: unauthorized_client: client is not registered", ErrClientMisconfigured)
	if got := DetailOf(client); got != "unauthorized_client: client is not registered" {
		t.Fatalf("client detail %q", got)
	}
	for name, err := range map[string]error{
		"bare sentinel":    ErrReauthRequired,
		"bare client":      ErrClientMisconfigured,
		"self-referential": fmt.Errorf("%w: %s", ErrReauthRequired, ErrReauthRequired.Error()),
		"foreign error":    errors.New("token endpoint returned HTTP 500"),
	} {
		if got := DetailOf(err); got != "" {
			t.Fatalf("%s detail %q, want empty", name, got)
		}
	}
}

// 3: the bound branch writes the scope set the login was judged on, not the
// response's own — a callback-only evidence case must not persist the old set.
func TestReauthorizationWritesResolvedScopes(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeSIWC(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(f.Server))
	host, _ := s.HostID()
	c := siwcChannel(t, s, "oaiapp_scopes", host, "user-subject-1")
	if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "at-old", ExpiresAt: time.Now().Add(time.Hour).UTC(), Scopes: "openid profile"}); err != nil {
		t.Fatal(err)
	}
	auth := mustAuthorize(t, svc.Endpoints, "oaiapp_scopes", host, "http://127.0.0.1:1/auth/callback", IDTokenHint{})
	in := f.loginInput(t, auth, TokenResponse{
		AccessToken: "at-new", RefreshToken: "rt-new", ExpiresIn: 3600, ClientID: "oaiapp_scopes",
		// The response reports no scope set; the callback is the only evidence.
	}, "oaiapp_scopes")
	in.CallbackScope = Scope
	login, err := svc.CompleteLogin(context.Background(), in)
	if err != nil {
		t.Fatalf("the reauthorization refused: %v", err)
	}
	if !login.AlreadyLinked {
		t.Fatal("the reauthorization did not reuse the channel")
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.OAuth.Scopes, ScopeDirect) {
		t.Fatalf("the stored scope set is the old one: %q", stored.OAuth.Scopes)
	}
}

// The token endpoint's refusal is logged for diagnosing an authorization that
// just worked in the browser and still failed here — and the log carries no
// secret: the status, the issuer's error and description, and a summary of the
// request (client id prefix, redirect, resource, verifier length).
func TestTokenRequestLogsASanitizedSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(TokenResponse{Error: "invalid_grant", ErrorDescription: "the code has already been redeemed"})
	}))
	defer server.Close()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-secret-value"},
		"client_id":     {"oaiapp_1234567890abcdef"},
		"redirect_uri":  {"http://127.0.0.1:1455/auth/callback"},
		"code_verifier": {"verifier-secret-value"},
	}
	if _, err := tokenRequest(context.Background(), testClient(), testEndpoints(server), form); err == nil {
		t.Fatal("the refusal was not reported")
	}
	log := buf.String()
	for _, want := range []string{"invalid_grant", "the code has already been redeemed", "oaiapp_12345", "http://127.0.0.1:1455/auth/callback", "code_verifier_len=21"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log %q lacks %q", log, want)
		}
	}
	// The code and the verifier themselves never appear.
	for _, secret := range []string{"code-secret-value", "verifier-secret-value"} {
		if strings.Contains(log, secret) {
			t.Fatalf("log %q leaks %q", log, secret)
		}
	}
}
