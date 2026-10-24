package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

func testClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func testEndpoints(server *httptest.Server) Endpoints {
	return Endpoints{
		Issuer: server.URL, Authorize: server.URL + "/oauth/authorize",
		Token: server.URL + "/oauth/token", DeviceCode: server.URL + "/api/accounts/deviceauth/usercode",
		DeviceToken: server.URL + "/api/accounts/deviceauth/token",
		Backend:     server.URL + "/backend-api/codex", Models: server.URL + "/backend-api/codex/models",
		Usage: server.URL + "/backend-api/wham/usage", Fallback: server.URL + "/v1",
	}
}

func mustIDToken(t *testing.T, account, email string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": account, "email": email, "chatgpt_account_id": account, "exp": time.Now().Add(time.Hour).Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

// The authorization URL carries every parameter the protocol ref names, with the
// values it names — and none of the SIWC ones.
func TestAuthorizeURLParameters(t *testing.T) {
	eps := DefaultEndpoints()
	auth, err := AuthorizeURL(eps, "http://localhost:1455/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(auth.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The authorization surface is the auth issuer, not the backend host.
	if parsed.Scheme != "https" || parsed.Host != "auth.openai.com" || parsed.Path != "/oauth/authorize" {
		t.Fatalf("authorize URL %q", auth.URL)
	}
	if eps.Issuer != "https://auth.openai.com" || eps.Backend != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("default endpoints %+v", eps)
	}
	q := parsed.Query()
	want := map[string]string{
		"client_id":                  ClientID,
		"response_type":              "code",
		"redirect_uri":               "http://localhost:1455/auth/callback",
		"scope":                      ScopeAuthorize,
		"code_challenge_method":      "S256",
		"id_token_add_organizations": AddOrganizations,
		"codex_cli_simplified_flow":  SimplifiedFlow,
		"originator":                 Originator,
	}
	for key, value := range want {
		if got := q.Get(key); got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
	// The S256 challenge is the sha256 of the verifier the flow keeps.
	if q.Get("code_challenge") == "" || len(q.Get("code_challenge")) < 40 {
		t.Fatalf("code_challenge %q", q.Get("code_challenge"))
	}
	// None of the SIWC parameters: no resource, no agent name hint, and the
	// scope set has neither resource.invoke nor the direct-use scope.
	if q.Get("resource") != "" || q.Get("agent_name_hint") != "" {
		t.Fatalf("SIWC parameters leaked into the Codex URL: %v", q)
	}
	if strings.Contains(q.Get("scope"), "resource.invoke") || strings.Contains(q.Get("scope"), "chatgpt.tokens.use.direct") {
		t.Fatalf("scope %q", q.Get("scope"))
	}
	if !strings.Contains(q.Get("scope"), "api.connectors.read") || !strings.Contains(q.Get("scope"), "api.connectors.invoke") {
		t.Fatalf("scope %q", q.Get("scope"))
	}
	// No nonce: neither the CLI nor Codex-Manager sends one, and the id_token is
	// not checked against one on this path.
	if _, ok := q["nonce"]; ok {
		t.Fatalf("nonce leaked into the Codex URL: %v", q)
	}
	// Fresh state per flow, and a fresh verifier with it.
	second, err := AuthorizeURL(eps, "http://localhost:1455/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	parsed2, _ := url.Parse(second.URL)
	if parsed2.Query().Get("state") == q.Get("state") {
		t.Fatal("state reused")
	}
	if second.Verifier == auth.Verifier {
		t.Fatal("verifier reused")
	}
}

// The loopback listener for this track answers on localhost, and a pasted
// callback is accepted only for that host — the SIWC rule is the opposite.
func TestCallbackListenerOnLocalhost(t *testing.T) {
	// The pair is pinned to the CLI's 1455 with 1457 behind it. The binding
	// mechanism (try in order, fall back, name the ports when all are taken) is
	// proven in oauthbase, because `go test ./...` runs the packages in parallel
	// and the production pair may be held by another package's OAuth test.
	if len(CallbackPorts) != 2 || CallbackPorts[0] != 1455 || CallbackPorts[1] != 1457 {
		t.Fatalf("callback ports %v, want [1455 1457]", CallbackPorts)
	}
	listener, redirect, err := ListenRedirect()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if !strings.HasPrefix(redirect, "http://localhost:") || !strings.HasSuffix(redirect, oauthbase.CallbackPath) {
		t.Fatalf("redirect %q", redirect)
	}
	waited := make(chan oauthbase.CallbackResult, 1)
	go func() {
		result, err := listener.Wait(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		waited <- result
	}()
	resp, err := http.Get(redirect + "?code=code-local&state=st-local")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case result := <-waited:
		if result.Code != "code-local" || result.State != "st-local" {
			t.Fatalf("%+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not deliver the callback")
	}
	// The pasted-callback parser accepts this track's host and refuses SIWC's.
	if _, err := oauthbase.ParseCallbackURLOn(redirect+"?code=c&state=s", "localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := oauthbase.ParseCallbackURLOn("http://127.0.0.1:1455/auth/callback?code=c&state=s", "localhost"); err == nil {
		t.Fatal("the SIWC host was accepted for a Codex flow")
	}
}

// fakeCodex is the issuer plus the backend for the whole track.
type fakeCodex struct {
	*httptest.Server
	refreshScope atomic.Value // last refresh form's scope, for the assertion
	refreshCount atomic.Int32
	deviceReply  func() map[string]any
	models       []string
	usage        string
	// The device poll's JSON body and the device exchange's form, recorded so
	// the tests can assert the fields the protocol ref names.
	pollBody     atomic.Value // map[string]string
	tokenForm    atomic.Value // url.Values of the last token request
	pollStatuses []int        // statuses the poll answers with, in order
	pollIndex    atomic.Int32
	// The catalogue request's shape, recorded for the header and query
	// assertions: the URL with its query and the headers as sent.
	modelsURL    atomic.Value // string
	modelsHeader atomic.Value // http.Header
}

func newFakeCodex(t *testing.T) *fakeCodex {
	t.Helper()
	f := &fakeCodex{models: []string{"gpt-5-codex", "gpt-5"}}
	f.deviceReply = func() map[string]any {
		return map[string]any{"device_auth_id": "da-1", "user_code": "CODE-XY", "interval": 0, "expires_in": 900}
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			f.tokenForm.Store(r.PostForm)
			if got := r.PostForm.Get("grant_type"); got == "refresh_token" {
				f.refreshScope.Store(r.PostForm.Get("scope"))
				f.refreshCount.Add(1)
				json.NewEncoder(w).Encode(TokenResponse{
					AccessToken: "at-refreshed", RefreshToken: "rt-refreshed", IDToken: mustIDToken(t, "acct-1", "person@example.test"),
					ExpiresIn: 3600, Scope: ScopeRefresh,
				})
				return
			}
			json.NewEncoder(w).Encode(TokenResponse{
				AccessToken: "at-live", RefreshToken: "rt-live", IDToken: mustIDToken(t, "acct-1", "person@example.test"),
				ExpiresIn: 3600, Scope: ScopeAuthorize,
			})
		case "/api/accounts/deviceauth/usercode":
			json.NewEncoder(w).Encode(f.deviceReply())
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.pollBody.Store(body)
			if status := f.pollStatus(); status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"authorization_code": "device-code-1", "code_verifier": "device-verifier-1"})
		case "/backend-api/codex/models":
			f.modelsURL.Store(r.URL.String())
			f.modelsHeader.Store(r.Header.Clone())
			data := []map[string]any{}
			for _, id := range f.models {
				data = append(data, map[string]any{"id": id, "slug": id})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": data})
		case "/backend-api/wham/usage":
			if got := r.Header.Get("Origin"); got != "" {
				t.Error("passive usage must not require Origin")
			}
			if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-1" {
				t.Errorf("account id header %q", got)
			}
			io.WriteString(w, f.usage)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// pollStatus is the status the next device poll answers with: the scripted one
// while any remain, then 200.
func (f *fakeCodex) pollStatus() int {
	if int(f.pollIndex.Load()) < len(f.pollStatuses) {
		status := f.pollStatuses[f.pollIndex.Add(1)-1]
		return status
	}
	return http.StatusOK
}

// The exchange sends no client secret and the refresh asks for exactly the
// narrow scope set the protocol ref names — the two details that make this path
// differ from SIWC.
func TestExchangeAndRefreshScope(t *testing.T) {
	f := newFakeCodex(t)
	eps := testEndpoints(f.Server)
	auth, err := AuthorizeURL(eps, "http://localhost:1455/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := ExchangeCode(context.Background(), testClient(), eps, auth, "code-1")
	if err != nil {
		t.Fatal(err)
	}
	if reply.AccessToken != "at-live" || reply.RefreshToken != "rt-live" {
		t.Fatalf("%+v", reply)
	}
	rotated, err := RefreshToken(context.Background(), testClient(), eps, "rt-live")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.AccessToken != "at-refreshed" {
		t.Fatalf("%+v", rotated)
	}
	if got := f.refreshScope.Load(); got != ScopeRefresh {
		t.Fatalf("refresh scope %q, want %q", got, ScopeRefresh)
	}
	// Every dead-credential branch maps to one error.
	for _, code := range []string{"invalid_grant", "refresh_token_reused", "refresh_token_invalidated"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(TokenResponse{Error: code})
		}))
		if _, err := RefreshToken(context.Background(), testClient(), testEndpoints(server), "rt"); !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("%s: %v", code, err)
		}
		server.Close()
	}
}

// The device-code path: the code request, the poll that returns the
// server-minted pair, and the exchange that spends it. The 404 branch means the
// server has not enabled device auth.
func TestDeviceCodeFlow(t *testing.T) {
	f := newFakeCodex(t)
	eps := testEndpoints(f.Server)
	flow, err := StartDeviceFlow(context.Background(), testClient(), eps)
	if err != nil {
		t.Fatal(err)
	}
	if flow.UserCode != "CODE-XY" || flow.DeviceAuthID != "da-1" {
		t.Fatalf("%+v", flow)
	}
	if flow.Verification != f.URL+"/codex/device" {
		t.Fatalf("verification %q", flow.Verification)
	}
	result := PollDeviceFlow(context.Background(), testClient(), eps, flow)
	if result.Verdict != DeviceComplete || result.AuthorizationCode != "device-code-1" || result.Verifier != "device-verifier-1" {
		t.Fatalf("%+v", result)
	}
	// The poll names the user code, not a client id: the pair is what the server
	// matches the pending login against.
	body, _ := f.pollBody.Load().(map[string]string)
	if body["user_code"] != "CODE-XY" || body["device_auth_id"] != "da-1" || body["client_id"] != "" {
		t.Fatalf("poll body %v", body)
	}
	tokens, err := ExchangeDeviceCode(context.Background(), testClient(), eps, result.AuthorizationCode, result.Verifier)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "at-live" {
		t.Fatalf("%+v", tokens)
	}
	// The exchange carries the device flow's own callback on the issuer.
	form, _ := f.tokenForm.Load().(url.Values)
	if got := form.Get("redirect_uri"); got != f.URL+"/deviceauth/callback" {
		t.Fatalf("device exchange redirect_uri %q", got)
	}
	// Pending, declined, expired and disabled.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The user-code request itself is what a server without device auth
		// answers 404 for.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	notEnabled := testEndpoints(server)
	if _, err := StartDeviceFlow(context.Background(), testClient(), notEnabled); !errors.Is(err, ErrDeviceAuthDisabled) {
		t.Fatalf("device auth disabled: %v", err)
	}
	pending := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/accounts/deviceauth/usercode" {
			json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "d", "user_code": "U"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
	}))
	defer pending.Close()
	peps := testEndpoints(pending)
	device, err := StartDeviceFlow(context.Background(), testClient(), peps)
	if err != nil {
		t.Fatal(err)
	}
	if got := PollDeviceFlow(context.Background(), testClient(), peps, device); got.Verdict != DevicePending {
		t.Fatalf("pending: %+v", got)
	}
	declined := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/accounts/deviceauth/usercode" {
			json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "d", "user_code": "U"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
	}))
	defer declined.Close()
	deps := testEndpoints(declined)
	device, err = StartDeviceFlow(context.Background(), testClient(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := PollDeviceFlow(context.Background(), testClient(), deps, device); got.Verdict != DeviceFailed || got.Code != "access_denied" {
		t.Fatalf("declined: %+v", got)
	}
	// A 403 or 404 on the poll means "not authorized yet", not a failure: the
	// flow stays answerable and the caller keeps polling to the flow's own
	// deadline. Treating either as terminal would kill every login where the
	// user is still typing the code.
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		f.pollStatuses = []int{status, status, http.StatusOK}
		f.pollIndex.Store(0)
		verdicts := []DeviceVerdict{}
		for i := 0; i < 3; i++ {
			verdicts = append(verdicts, PollDeviceFlow(context.Background(), testClient(), eps, flow).Verdict)
		}
		if verdicts[0] != DevicePending || verdicts[1] != DevicePending || verdicts[2] != DeviceComplete {
			t.Fatalf("poll %d answered %v, want pending, pending, complete", status, verdicts)
		}
	}
}

// The id_token is parsed for the account identity, not verified: the token
// arrived from the token endpoint over TLS in exchange for a code this process
// minted, and the ref does not document a JWKS for this path.
func TestIDClaimsAccountIdentity(t *testing.T) {
	token := mustIDToken(t, "acct-9", "nine@example.test")
	claims, err := ParseIDClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AccountID != "acct-9" || claims.Email != "nine@example.test" {
		t.Fatalf("%+v", claims)
	}
	if AccountOf(claims) != "acct-9" {
		t.Fatalf("account %q", AccountOf(claims))
	}
	// No account id: the subject stands in.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": "sub-only"})
	claims, err = ParseIDClaims(header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".")
	if err != nil {
		t.Fatal(err)
	}
	if AccountOf(claims) != "sub-only" {
		t.Fatalf("account %q", AccountOf(claims))
	}
	if _, err := ParseIDClaims("not-a-jwt"); err == nil {
		t.Fatal("garbage accepted")
	}
}

// The rejection's detail is the issuer's own words after the sentinel, and a
// bare sentinel carries none - repeating it beside the console's own advice
// would say the same thing twice.
func TestDetailOf(t *testing.T) {
	wrapped := fmt.Errorf("%w: invalid_grant: the refresh token expired", ErrReauthRequired)
	if got := DetailOf(wrapped); got != "invalid_grant: the refresh token expired" {
		t.Fatalf("wrapped detail %q", got)
	}
	for name, err := range map[string]error{
		"bare sentinel":    ErrReauthRequired,
		"self-referential": fmt.Errorf("%w: %s", ErrReauthRequired, ErrReauthRequired.Error()),
		"foreign error":    errors.New("token endpoint returned HTTP 500"),
	} {
		if got := DetailOf(err); got != "" {
			t.Fatalf("%s detail %q, want empty", name, got)
		}
	}
}

// The catalogue prefers slugs, and the usage windows are the 5h/7d pair.
func TestModelsAndUsage(t *testing.T) {
	f := newFakeCodex(t)
	f.usage = `{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":1800},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`
	eps := testEndpoints(f.Server)
	models, err := DiscoverModels(context.Background(), testClient(), eps.Models, "at-live", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "gpt-5-codex" {
		t.Fatalf("models %v", models)
	}
	usage, err := FetchUsage(context.Background(), testClient(), eps, "at-live", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Buckets) != 1 || usage.Buckets[0].Primary == nil || *usage.Buckets[0].Primary.UsedPercent != 12 || *usage.Buckets[0].Primary.WindowSeconds != 18000 {
		t.Fatalf("primary %+v", usage)
	}
	if usage.Buckets[0].Secondary == nil || *usage.Buckets[0].Secondary.WindowSeconds != 604800 {
		t.Fatalf("secondary %+v", usage)
	}
}

// The catalogue request is the Codex CLI's, field for field: the client_version
// query naming the CLI release, the bearer, the ChatGPT-Account-ID the
// catalogue is scoped to, the originator, the CLI's user agent and an Accept.
// The account id is the field whose absence made the backend answer 400, so it
// is asserted by name — and the two headers neither the CLI nor the two Go
// reimplementations sends on this endpoint (session-id, Origin/Referer) are
// asserted absent, so nobody "fixes" a future 400 by guessing them back in.
func TestDiscoverModelsRequestShape(t *testing.T) {
	f := newFakeCodex(t)
	eps := testEndpoints(f.Server)
	if _, err := DiscoverModels(context.Background(), testClient(), eps.Models, "at-live", "acct-9"); err != nil {
		t.Fatal(err)
	}
	gotURL, _ := f.modelsURL.Load().(string)
	parsed, err := url.Parse(gotURL)
	if err != nil {
		t.Fatalf("models URL %q: %v", gotURL, err)
	}
	if parsed.Path != "/backend-api/codex/models" {
		t.Fatalf("path %q", parsed.Path)
	}
	if v := parsed.Query().Get("client_version"); v != ClientVersionValue() {
		t.Fatalf("client_version %q, want %q", v, ClientVersionValue())
	}
	header, _ := f.modelsHeader.Load().(http.Header)
	if header == nil {
		t.Fatal("no models request recorded")
	}
	want := map[string]string{
		"Authorization":      "Bearer at-live",
		"ChatGPT-Account-ID": "acct-9",
		"originator":         OriginatorValue(),
		"User-Agent":         UserAgentValue(),
		"Accept":             "application/json",
	}
	for name, value := range want {
		if got := header.Get(name); got != value {
			t.Fatalf("%s = %q, want %q", name, got, value)
		}
	}
	for _, name := range []string{"session-id", "Origin", "Referer"} {
		if got := header.Get(name); got != "" {
			t.Fatalf("%s = %q, want absent", name, got)
		}
	}
}

// A token pair that names no account sends no account header: the references
// omit the header rather than send a value the backend will reject, and the
// subject is not an account id.
func TestDiscoverModelsOmitsAccountHeaderWhenUnknown(t *testing.T) {
	f := newFakeCodex(t)
	eps := testEndpoints(f.Server)
	if _, err := DiscoverModels(context.Background(), testClient(), eps.Models, "at-live", ""); err != nil {
		t.Fatal(err)
	}
	header, _ := f.modelsHeader.Load().(http.Header)
	if got := header.Get("ChatGPT-Account-ID"); got != "" {
		t.Fatalf("account header %q, want absent", got)
	}
	// The rest of the client disguise still rides: an omission of one field is
	// not a reason to send the request bare.
	if got := header.Get("originator"); got != OriginatorValue() {
		t.Fatalf("originator %q", got)
	}
}

// An operator who pinned the models URL with its own query keeps it: the
// client_version is appended only when the URL does not already carry one.
func TestModelsURLWithVersion(t *testing.T) {
	got := modelsURLWithVersion("https://chatgpt.com/backend-api/codex/models")
	if !strings.Contains(got, "client_version="+ClientVersionValue()) {
		t.Fatalf("version not appended: %q", got)
	}
	pinned := "https://chatgpt.com/backend-api/codex/models?client_version=0.1.0"
	if got := modelsURLWithVersion(pinned); got != pinned {
		t.Fatalf("pinned query overwritten: %q", got)
	}
}

// The account id claim is read from the top level or from the
// https://api.openai.com/auth object, which is where ChatGPT puts it. The
// subject is not an account id, and a token that does not carry the claim
// yields an empty answer rather than a guess.
func TestChatGPTAccountIDOf(t *testing.T) {
	jwt := func(t *testing.T, payload map[string]any) string {
		t.Helper()
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		return header + "." + base64.RawURLEncoding.EncodeToString(encoded) + "."
	}
	cases := map[string]struct {
		token string
		want  string
	}{
		"top level claim": {jwt(t, map[string]any{"sub": "user-1", "chatgpt_account_id": "acct-top"}), "acct-top"},
		"nested claim":    {jwt(t, map[string]any{"sub": "user-1", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-nested"}}), "acct-nested"},
		"top level wins":  {jwt(t, map[string]any{"chatgpt_account_id": "acct-top", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-nested"}}), "acct-top"},
		"subject only":    {jwt(t, map[string]any{"sub": "user-1"}), ""},
		"not a jwt":       {"at-opaque", ""},
		"empty":           {"", ""},
	}
	for name, tc := range cases {
		if got := ChatGPTAccountIDOf(tc.token); got != tc.want {
			t.Fatalf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// The header value resolution order is the references': the access token's
// claim first, the id token's next, and the pinned account last — unless the
// pinned account is the subject, which is a user id the backend would reject.
func TestAccountHeaderIDResolution(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	topLevel := func(account string) string {
		payload, _ := json.Marshal(map[string]any{"sub": "user-1", "chatgpt_account_id": account})
		return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
	}
	cases := []struct {
		name             string
		access, id       string
		account, subject string
		want             string
	}{
		{"access token claim wins", topLevel("acct-access"), topLevel("acct-id"), "acct-pinned", "user-1", "acct-access"},
		{"id token claim next", "at-opaque", topLevel("acct-id"), "acct-pinned", "user-1", "acct-id"},
		{"pinned account last", "at-opaque", "at-opaque", "acct-pinned", "user-1", "acct-pinned"},
		{"subject is not an account", "at-opaque", "at-opaque", "user-1", "user-1", ""},
		{"nothing names the account", "at-opaque", "at-opaque", "", "", ""},
	}
	for _, tc := range cases {
		if got := AccountHeaderID(tc.access, tc.id, tc.account, tc.subject); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A refused catalogue is logged with the shape that produced it: the URL with
// its query, the status, the body's first characters and every header the
// request carried. The bearer appears as a prefix only — the record exists to
// name the missing field, never to carry the credential.
func TestDiscoverModelsFailureLogsTheReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"detail":"missing ChatGPT-Account-ID header","hint":"scope the catalogue to an account"}`)
	}))
	defer server.Close()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)

	_, err := DiscoverModels(context.Background(), testClient(), server.URL+"/backend-api/codex/models", "at-secret-bearer", "acct-1")
	if err == nil {
		t.Fatal("the refusal was not reported")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error %q carries no status", err)
	}
	log := buf.String()
	for _, want := range []string{
		"model catalogue refused the request",
		"status=400",
		"missing ChatGPT-Account-ID header",
		"scope the catalogue to an account",
		"client_version",
		"account", // the header report names the account id field
		"acct-1",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log %q lacks %q", log, want)
		}
	}
	// The bearer itself never appears — only its prefix.
	if strings.Contains(log, "at-secret-bearer") {
		t.Fatalf("log %q leaks the access token", log)
	}
}

func codexChannel(t *testing.T, s *store.Store, base string) store.Channel {
	t.Helper()
	c := store.Channel{Name: "codex", CredentialType: store.CredentialCodexOAuth, Enabled: true,
		Models:    []string{"gpt-5-codex"},
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: base}},
		OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderCodex, RefreshToken: "rt-stored", IDToken: mustIDToken(t, "acct-1", "p@example.test"),
			ClientID: ClientID, Account: "acct-1", Scopes: ScopeAuthorize,
			AccessToken: "at-stored", ExpiresAt: time.Now().Add(-time.Minute).UTC(),
			AccountLabel: "p@example.test", APIBase: base, Status: store.OAuthStatusActive,
		}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func newService(t *testing.T, f *fakeCodex) (*Service, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return NewService(s, testClient(), testEndpoints(f.Server)), s
}

// G017: two concurrent rotations cost one refresh, and the locked-out callers
// read the stored row instead of exchanging again.
func TestConcurrentRotationExchangesOnce(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeCodex(t)
	svc, s := newService(t, f)
	c := codexChannel(t, s, f.URL+"/backend-api/codex")
	const goroutines = 6
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
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if tokens[i] != "at-refreshed" {
			t.Fatalf("goroutine %d got %q", i, tokens[i])
		}
	}
	if got := f.refreshCount.Load(); got != 1 {
		t.Fatalf("rotated %d times for one expiry window", got)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.RefreshToken != "rt-refreshed" || stored.Generation != c.Generation {
		t.Fatalf("rotation damaged the credential: %+v gen=%d", stored.OAuth, stored.Generation)
	}
}

// The skew and the race recovery: a token with time left is used, and a caller
// whose refresh token was already rotated while it waited adopts the stored one.
func TestBearerSkewAndRaceRecovery(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeCodex(t)
	svc, s := newService(t, f)
	c := codexChannel(t, s, f.URL+"/backend-api/codex")
	if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "at-soon", ExpiresAt: time.Now().Add(5 * time.Minute).UTC()}); err != nil {
		t.Fatal(err)
	}
	if token, err := svc.Bearer(context.Background(), c); err != nil || token != "at-soon" {
		t.Fatalf("%q %v", token, err)
	}
	if f.refreshCount.Load() != 0 {
		t.Fatal("rotated a token with time left")
	}
	// A stored token with life left, rotated behind the caller's back: the
	// caller's refresh token is already dead, so it adopts the stored one and
	// spends nothing.
	if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "at-adopted", RefreshToken: "rt-new", ExpiresAt: time.Now().Add(30 * time.Minute).UTC()}); err != nil {
		t.Fatal(err)
	}
	if token, err := svc.Bearer(context.Background(), c); err != nil || token != "at-adopted" {
		t.Fatalf("%q %v", token, err)
	}
	if f.refreshCount.Load() != 0 {
		t.Fatalf("rotated %d times with a stale snapshot and a healthy stored token", f.refreshCount.Load())
	}
	// A stored token inside the skew is not adoptable: rotation is single-use, so
	// the caller's refresh token is dead and the only usable one is the stored
	// refresh token — which means rotating with it, not handing back a token that
	// expires mid-request.
	if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "at-nearly", RefreshToken: "rt-newer", ExpiresAt: time.Now().Add(30 * time.Second).UTC()}); err != nil {
		t.Fatal(err)
	}
	if token, err := svc.Bearer(context.Background(), c); err != nil || token != "at-refreshed" {
		t.Fatalf("%q %v", token, err)
	}
	if f.refreshCount.Load() != 1 {
		t.Fatalf("rotated %d times", f.refreshCount.Load())
	}
}

// A rejection marks the credential and stops the retries.
func TestBearerMarksReauth(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	f := newFakeCodex(t)
	svc, s := newService(t, f)
	c := codexChannel(t, s, f.URL+"/backend-api/codex")
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(TokenResponse{Error: "invalid_grant"})
	}))
	defer rejected.Close()
	svc.Endpoints = testEndpoints(rejected)
	if _, err := svc.Bearer(context.Background(), c); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("%v", err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusReauthRequired || stored.OAuth.StatusReason == "" {
		t.Fatalf("status %+v", stored.OAuth)
	}
	if _, err := svc.Bearer(context.Background(), c); err == nil || !strings.Contains(err.Error(), "authorization again") {
		t.Fatalf("the mark did not stop the retries: %v", err)
	}
}

// The identity lookup is by account id, and by the refresh token's hash when the
// token carries no account.
func TestChannelByCodexAccount(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	found, err := s.ChannelByCodexAccount("acct-1", "")
	if err != nil || found.ID != c.ID {
		t.Fatalf("account lookup: %v", err)
	}
	// No account id: the refresh token's hash stands in.
	noAccount := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	noAccount.OAuth.Account = ""
	noAccount.OAuth.RefreshToken = "rt-unique"
	if err := s.SaveChannel(&noAccount); err != nil {
		t.Fatal(err)
	}
	found, err = s.ChannelByCodexAccount("", "rt-unique")
	if err != nil || found.ID != noAccount.ID {
		t.Fatalf("refresh-token lookup: %v", err)
	}
	if _, err := s.ChannelByCodexAccount("acct-missing", ""); err != store.ErrNotFound {
		t.Fatalf("unknown account: %v", err)
	}
}

// A channel assembled from an authorization validates and keeps the identity the
// pin is made of.
func TestChannelFor(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	tokens := TokenResponse{AccessToken: "at", RefreshToken: "rt", IDToken: mustIDToken(t, "acct-2", "two@example.test"), ExpiresIn: 3600, Scope: ScopeAuthorize}
	claims, err := ParseIDClaims(tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	channel := ChannelFor(LoginInput{Tokens: tokens, Claims: claims, Account: AccountOf(claims), APIBase: "https://chatgpt.com/backend-api/codex", Models: []string{"gpt-5-codex"}, Label: "two@example.test"})
	if channel.CredentialType != store.CredentialCodexOAuth || channel.OAuth.Provider != store.OAuthProviderCodex {
		t.Fatalf("%+v", channel)
	}
	if channel.OAuth.Account != "acct-2" || channel.OAuth.ClientID != ClientID {
		t.Fatalf("identity %+v", channel.OAuth)
	}
	if len(channel.Endpoints) != 1 || channel.Endpoints[0].Format != store.FormatOpenAIResponses || channel.Endpoints[0].BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("endpoints %+v", channel.Endpoints)
	}
	if channel.Name != "Codex backend" {
		t.Fatalf("name %q", channel.Name)
	}
	if channel.OAuth.AccountLabel != "two@example.test" {
		t.Fatalf("account label %q", channel.OAuth.AccountLabel)
	}
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.SaveChannel(&channel); err != nil {
		t.Fatalf("the assembled channel must validate: %v", err)
	}
	channel.Name = "My Codex two@example.test"
	if err := s.SaveChannelConfig(&channel); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s, testClient(), DefaultEndpoints())
	if err := svc.ReauthorizeChannel(channel.ID, LoginInput{Account: "acct-2", Tokens: TokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh"}}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Channel(channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != channel.Name || stored.OAuth.AccountLabel != "two@example.test" || stored.OAuth.RefreshToken != "new-refresh" {
		t.Fatal("reauthorization changed the chosen name or account metadata, or failed to replace the credential")
	}
}

// P2-3: a rotation whose write fails is re-fetched with the live token and
// stored — the refresh token the exchange used is already dead, so keeping the
// stored one would guarantee an invalid_grant on the next attempt.
func TestRotationReFetchesWhenTheWriteFails(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var rotations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/oauth/token") {
			w.WriteHeader(404)
			return
		}
		n := rotations.Add(1)
		json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: fmt.Sprintf("at-attempt-%d", n), RefreshToken: fmt.Sprintf("rt-attempt-%d", n),
			ExpiresIn: 3600, Scope: ScopeRefresh, ClientID: "app_retry",
		})
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(server))
	c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	// Two writes fail: the store's own retry absorbs the first, so it is the
	// second that reaches the service and triggers the re-fetch.
	store.FailPersistWrites(2)
	token, err := svc.Bearer(context.Background(), c)
	if err != nil {
		t.Fatalf("the rotation was not recovered: %v", err)
	}
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
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
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
			AccessToken: "at-once", RefreshToken: "rt-once", ExpiresIn: 3600, Scope: ScopeRefresh, ClientID: "app_refetch",
		})
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(server))
	c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	store.FailPersistWrites(2)
	_, err = svc.Bearer(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "re-authorize") {
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
