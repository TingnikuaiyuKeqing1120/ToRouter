// Package codex implements the community Codex backend track
// (docs/protocol-refs/oauth-openai-codex.md, source: qxcnm/Codex-Manager,
// 2026-10-02): the browser authorization that reuses the Codex CLI's public
// client identity, the device-code login, the token rotation and the backend
// call shape.
//
// This is not an OpenAI-official path. It is the most capable of the three
// tracks — full model catalogue, plan usage windows — and the greyest: it
// borrows the Codex CLI's identity rather than registering an app. The console
// labels it as non-official, and the gateway does not enforce a request profile
// because the backend's capability surface is unverified.
//
// What is verified comes from the protocol ref, which was read off the source.
// Where the ref is silent the code says so.
package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"torouter/internal/accessguard"

	"torouter/internal/oauthbase"
)

// The scope sets, verbatim from the protocol ref. Authorization asks for the
// connectors pair; refresh deliberately does not — "refresh 不带 resource/
// chatgpt scopes——与 SIWC 语义不同，属本路径行为".
const (
	ScopeAuthorize = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	ScopeRefresh   = "openid profile email"
	// ClientID is the Codex CLI's public client id (source: core/src/auth/mod.rs:7).
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// SimplifiedFlow is the codex_cli_simplified_flow flag.
	SimplifiedFlow = "true"
	// AddOrganizations is the id_token_add_organizations flag.
	AddOrganizations = "true"
)

func ClientIDValue() string { return envOr("TOROUTER_CODEX_CLIENT_ID", ClientID) }

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// Endpoints are the Codex bases. Tests inject their own; DefaultEndpoints reads
// the environment first. Issuer and Backend are separate hosts on purpose
// (Codex-Manager core/src/auth/mod.rs, 2026-10-02): the authorization surface
// lives on auth.openai.com — the same issuer SIWC uses — while the inference
// backend stays on chatgpt.com. Pointing the issuer at chatgpt.com makes every
// authorize and device request fail there, which is why the default is the auth
// host and only the backend keeps chatgpt.com.
type Endpoints struct {
	Issuer       string // https://auth.openai.com
	Authorize    string
	Token        string
	DeviceCode   string
	DeviceToken  string
	Backend      string // https://chatgpt.com/backend-api/codex
	Models       string
	Usage        string
	ResetCredits string
	Fallback     string // api.openai.com/v1, the documented fallback base
}

// DefaultEndpoints returns the production bases from the protocol ref.
func DefaultEndpoints() Endpoints {
	issuer := envOr("TOROUTER_CODEX_ISSUER_URL", "https://auth.openai.com")
	backend := envOr("TOROUTER_CODEX_BACKEND_URL", "https://chatgpt.com/backend-api/codex")
	return Endpoints{
		Issuer:       issuer,
		Authorize:    envOr("TOROUTER_CODEX_AUTHORIZE_URL", issuer+"/oauth/authorize"),
		Token:        envOr("TOROUTER_CODEX_TOKEN_URL", issuer+"/oauth/token"),
		DeviceCode:   envOr("TOROUTER_CODEX_DEVICE_CODE_URL", issuer+"/api/accounts/deviceauth/usercode"),
		DeviceToken:  envOr("TOROUTER_CODEX_DEVICE_TOKEN_URL", issuer+"/api/accounts/deviceauth/token"),
		Backend:      backend,
		Models:       envOr("TOROUTER_CODEX_MODELS_URL", backend+"/models"),
		Usage:        envOr("TOROUTER_CODEX_USAGE_URL", strings.TrimSuffix(strings.TrimRight(backend, "/"), "/codex")+"/wham/usage"),
		ResetCredits: envOr("TOROUTER_CODEX_RESET_CREDITS_URL", strings.TrimSuffix(strings.TrimRight(backend, "/"), "/codex")+"/wham/rate-limit-reset-credits"),
		Fallback:     envOr("TOROUTER_CODEX_FALLBACK_URL", "https://api.openai.com/v1"),
	}
}

// AuthRequest is one authorization URL plus the material needed to finish it.
type AuthRequest struct {
	URL         string
	RedirectURI string
	ClientID    string
	State       string
	Verifier    string
}

// CallbackPorts are the loopback ports this track pins: 1455 is the port the
// CLI registers with the issuer (source comment: kept in sync with the issuer's
// allow-list), and 1457 is the fallback for when an earlier authorization still
// holds 1455.
var CallbackPorts = []int{1455, 1457}

// ListenRedirect binds the loopback listener for this track and returns the
// redirect URI the issuer will send the code to. The Codex track pins
// http://localhost:<port>/auth/callback — the opposite of SIWC's 127.0.0.1
// requirement — and its listener binds 127.0.0.1 with the IPv6 loopback as the
// fallback (protocol ref, 与官方 SIWC 是两条互不兼容的路). The port is pinned to
// the CLI's 1455 with 1457 behind it.
func ListenRedirect() (*oauthbase.CallbackListener, string, error) {
	listener, err := oauthbase.ListenCallbackOn("localhost", CallbackPorts...)
	if err != nil {
		return nil, "", err
	}
	return listener, listener.RedirectURI, nil
}

// AuthorizeURL builds the authorization URL verbatim from the protocol ref: the
// Codex CLI's public client, the localhost redirect, PKCE S256, a fresh state,
// the connector scope set — and no resource parameter, no nonce (neither the CLI
// nor Codex-Manager sends one; the id_token is not checked against a nonce on
// this path), which is one of the ways this path is incompatible with SIWC.
func AuthorizeURL(eps Endpoints, redirectURI string) (AuthRequest, error) {
	state, err := oauthbase.RandomToken(24)
	if err != nil {
		return AuthRequest{}, err
	}
	pkce, err := oauthbase.NewPKCE()
	if err != nil {
		return AuthRequest{}, err
	}
	query := url.Values{
		"client_id":                  {ClientIDValue()},
		"response_type":              {"code"},
		"redirect_uri":               {redirectURI},
		"scope":                      {ScopeAuthorize},
		"state":                      {state},
		"code_challenge":             {pkce.Challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {AddOrganizations},
		"codex_cli_simplified_flow":  {SimplifiedFlow},
		"originator":                 {OriginatorValue()},
	}
	return AuthRequest{
		URL:         eps.Authorize + "?" + query.Encode(),
		RedirectURI: redirectURI,
		ClientID:    ClientIDValue(),
		State:       state,
		Verifier:    pkce.Verifier,
	}, nil
}

// ExchangeCode trades the authorization code for tokens. There is no client
// secret: this is a public CLI client, so the PKCE verifier is the proof.
func ExchangeCode(ctx context.Context, client *http.Client, eps Endpoints, auth AuthRequest, code string) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {auth.RedirectURI},
		"client_id":     {auth.ClientID},
		"code_verifier": {auth.Verifier},
	}
	return tokenRequest(ctx, client, eps, form)
}

// RefreshToken rotates the refresh token. The scope set is the narrow one the
// protocol ref names — "openid profile email", without the connector scopes —
// and there is no resource parameter. Both are asserted in tests because
// getting either wrong fails the exchange in a way that looks like a dead
// credential.
func RefreshToken(ctx context.Context, client *http.Client, eps Endpoints, refreshToken string) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {ClientIDValue()},
		"scope":         {ScopeRefresh},
	}
	return tokenRequest(ctx, client, eps, form)
}

// TokenResponse is the token endpoint's reply. ExpiresIn is seconds. Error and
// ErrorDescription are the OAuth failure body, zero on success.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	// ClientID is echoed by some issuers; the track's own constant is the
	// default when it is absent.
	ClientID         string `json:"client_id"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (r TokenResponse) Code() string { return r.Error }

// ErrReauthRequired covers the token-endpoint answers that say the credential is
// finished.
var ErrReauthRequired = errors.New("codex: credential rejected; re-authorization required")

// DetailOf is the issuer's own error code and description a rejection carried,
// or "" when it carried none. The wrapped error keeps the sentinel first, so the
// detail is what follows it; the console appends it to its fixed advice. A bare
// sentinel — or a detail that is the sentinel itself — carries nothing to add,
// and repeating it beside the console's own advice would say the same thing
// twice.
func DetailOf(err error) string {
	text := err.Error()
	prefix := ErrReauthRequired.Error()
	if !strings.HasPrefix(text, prefix) {
		return ""
	}
	detail := strings.TrimSpace(strings.TrimPrefix(text, prefix))
	detail = strings.TrimSpace(strings.TrimPrefix(detail, ":"))
	if detail == "" || detail == prefix {
		return ""
	}
	return detail
}

func tokenRequest(ctx context.Context, client *http.Client, eps Endpoints, form url.Values) (TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, eps.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return TokenResponse{}, readErr
	}
	var reply TokenResponse
	if json.Unmarshal(body, &reply) != nil {
		return TokenResponse{}, fmt.Errorf("token endpoint returned HTTP %d with an unreadable body", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch reply.Code() {
		case "invalid_grant", "refresh_token_reused", "refresh_token_invalidated":
			// The description rides along on the wrapped error: the console
			// shows it beside the fixed advice, and an issuer that says why
			// ("refresh token expired") saves the owner a guess.
			return TokenResponse{}, fmt.Errorf("%w: %s", ErrReauthRequired, errorDetail(reply))
		}
		if reply.Error != "" {
			return TokenResponse{}, fmt.Errorf("token endpoint returned HTTP %d: %s", resp.StatusCode, errorDetail(reply))
		}
		return TokenResponse{}, fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	if reply.AccessToken == "" {
		return TokenResponse{}, errors.New("token endpoint returned no access token")
	}
	return reply, nil
}

// errorDetail renders the issuer's machine-readable error with its description
// when it sent one.
func errorDetail(reply TokenResponse) string {
	if desc := strings.TrimSpace(reply.ErrorDescription); desc != "" {
		return fmt.Sprintf("%s: %s", reply.Error, desc)
	}
	return reply.Error
}

// IDClaims is the subset of the id_token this track reads. The signature is
// verified when the issuer publishes a JWKS (chatgpt.com has an OIDC surface,
// so it normally does) and skipped otherwise — the claims are used for the
// account label and the identity, and a track whose issuer has no keys should
// not be blocked by a verifier it cannot satisfy.
type IDClaims struct {
	Subject   string
	Email     string
	Name      string
	AccountID string
	Expiry    time.Time
	Verified  bool
}

// ParseIDClaims reads the claims without verifying the signature. The token was
// just received from the token endpoint over TLS in exchange for a code this
// process minted, so the claims are trusted as delivered; what they are used
// for (a label, an identity) does not need the signature, and the ref does not
// document this issuer's JWKS for this path.
func ParseIDClaims(idToken string) (IDClaims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return IDClaims{}, errors.New("id_token is not a JWT")
	}
	payload, err := base64RawURL(parts[1])
	if err != nil {
		return IDClaims{}, err
	}
	var claims struct {
		Subject   string `json:"sub"`
		Email     string `json:"email"`
		Name      string `json:"name"`
		AccountID string `json:"chatgpt_account_id"`
		OrgID     string `json:"organization_id"`
		Expiry    int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return IDClaims{}, err
	}
	id := firstNonEmpty(claims.AccountID, claims.OrgID, claims.Subject)
	return IDClaims{Subject: claims.Subject, Email: claims.Email, Name: claims.Name, AccountID: id, Expiry: time.Unix(claims.Expiry, 0).UTC()}, nil
}

// AccountOf resolves the account identity a channel is pinned to: the id_token's
// account id when it has one, the subject otherwise.
func AccountOf(claims IDClaims) string { return firstNonEmpty(claims.AccountID, claims.Subject) }

// ChatGPTAccountIDOf reads the chatgpt_account_id claim out of one of this
// track's tokens — the access token or the id token — without verifying the
// signature (ParseIDClaims's trust argument applies: the token arrived over TLS
// from the token endpoint in exchange for a code this process minted). The
// claim sits at the top level of the payload or inside the
// https://api.openai.com/auth object, which is where ChatGPT puts it
// (Codex-Manager core/src/auth/mod.rs extract_chatgpt_account_id;
// CLIProxyAPI internal/auth/codex/jwt_parser.go reads the nested one, both
// 2026-10-02).
//
// The subject is deliberately not a fallback: it is a user id, and a backend
// asked to scope a catalogue to a user id answers 400 rather than another
// account's models. extract_chatgpt_user_id is a separate function for that
// reason, and so is this split here.
func ChatGPTAccountIDOf(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64RawURL(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		AccountID string `json:"chatgpt_account_id"`
		Auth      struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return firstNonEmpty(claims.AccountID, claims.Auth.AccountID)
}

// AccountHeaderID resolves the ChatGPT-Account-ID value a backend request
// carries, in the order the references use: the access token's claim first —
// Codex-Manager resolves the account scope from the access token before the id
// token (service/src/agent_identity.rs resolve_agent_identity_binding) — then
// the id token's, then the account the channel is pinned to, unless that
// account is the subject: AccountOf falls back to the subject for identity,
// and an identity is not an account id. An empty result means the header is
// omitted, which is what the references do when the account cannot be named —
// omitting beats sending a value the backend will reject.
func AccountHeaderID(accessToken, idToken, account, subject string) string {
	if id := ChatGPTAccountIDOf(accessToken); id != "" {
		return id
	}
	if id := ChatGPTAccountIDOf(idToken); id != "" {
		return id
	}
	if account != "" && account != subject {
		return account
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// DeviceFlow is one device-code login in progress.
type DeviceFlow struct {
	DeviceAuthID string
	UserCode     string
	Verification string
	Interval     int
	ExpiresAt    time.Time
}

// StartDeviceFlow asks the issuer for a device code. A 404 means this server
// has not enabled device auth (protocol ref), which is a different failure from
// a network one and is reported as such.
func StartDeviceFlow(ctx context.Context, client *http.Client, eps Endpoints) (DeviceFlow, error) {
	payload, _ := json.Marshal(map[string]string{"client_id": ClientIDValue()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, eps.DeviceCode, strings.NewReader(string(payload)))
	if err != nil {
		return DeviceFlow{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgentValue())
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return DeviceFlow{}, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return DeviceFlow{}, readErr
	}
	if resp.StatusCode == http.StatusNotFound {
		return DeviceFlow{}, ErrDeviceAuthDisabled
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DeviceFlow{}, fmt.Errorf("device authorization returned HTTP %d", resp.StatusCode)
	}
	var reply struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     int    `json:"interval"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return DeviceFlow{}, err
	}
	if reply.DeviceAuthID == "" || reply.UserCode == "" {
		return DeviceFlow{}, errors.New("device authorization returned an incomplete reply")
	}
	expires := time.Now().Add(15 * time.Minute)
	if reply.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second)
	}
	return DeviceFlow{
		DeviceAuthID: reply.DeviceAuthID,
		UserCode:     reply.UserCode,
		Verification: eps.Issuer + "/codex/device",
		Interval:     reply.Interval,
		ExpiresAt:    expires,
	}, nil
}

// ErrDeviceAuthDisabled is the 404 the issuer answers when the server has not
// enabled device auth: the track is unusable there, and the caller says so
// rather than offering a login that cannot finish.
var ErrDeviceAuthDisabled = errors.New("codex: this server has not enabled device authorization")

// DeviceVerdict is one device poll's outcome.
type DeviceVerdict int

const (
	// DevicePending: the user has not entered the code yet.
	DevicePending DeviceVerdict = iota
	// DeviceComplete: authorized; AuthorizationCode and Verifier are the ones
	// the server minted, and they go straight to the token endpoint.
	DeviceComplete
	// DeviceFailed: declined, expired, or a transport failure. Code names the
	// branch for the terminal ones.
	DeviceFailed
)

// DevicePollResult is one poll's verdict.
type DevicePollResult struct {
	Verdict           DeviceVerdict
	AuthorizationCode string
	Verifier          string
	Code              string
	Err               error
}

// PollDeviceFlow polls the device token endpoint once. The verifier is generated
// server-side and returned here (protocol ref), which is why this flow does not
// use a local PKCE pair.
//
// A 403 or 404 from the poll is "not authorized yet", not a failure: the server
// answers both while the code has not been entered (Codex-Manager treats them as
// authorization_pending), and the caller keeps polling until the flow's own
// window closes. Anything else that is not an answer is a real failure.
func PollDeviceFlow(ctx context.Context, client *http.Client, eps Endpoints, flow DeviceFlow) DevicePollResult {
	payload, _ := json.Marshal(map[string]string{"device_auth_id": flow.DeviceAuthID, "user_code": flow.UserCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, eps.DeviceToken, strings.NewReader(string(payload)))
	if err != nil {
		return DevicePollResult{Verdict: DeviceFailed, Code: "request", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgentValue())
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return DevicePollResult{Verdict: DeviceFailed, Code: "network", Err: err}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return DevicePollResult{Verdict: DeviceFailed, Code: "network", Err: readErr}
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		// The code has not been entered yet: keep polling rather than ending the
		// flow on an answer that only means "not yet".
		return DevicePollResult{Verdict: DevicePending}
	}
	var reply struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
		Error             string `json:"error"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return DevicePollResult{Verdict: DeviceFailed, Code: "invalid_reply", Err: fmt.Errorf("device poll returned HTTP %d with an unreadable body", resp.StatusCode)}
	}
	if reply.AuthorizationCode != "" {
		return DevicePollResult{Verdict: DeviceComplete, AuthorizationCode: reply.AuthorizationCode, Verifier: reply.CodeVerifier}
	}
	switch reply.Error {
	case "", "authorization_pending", "slow_down":
		return DevicePollResult{Verdict: DevicePending}
	case "expired_token", "access_denied":
		return DevicePollResult{Verdict: DeviceFailed, Code: reply.Error, Err: fmt.Errorf("device authorization failed: %s", reply.Error)}
	default:
		return DevicePollResult{Verdict: DeviceFailed, Code: reply.Error, Err: fmt.Errorf("device authorization failed: %s", reply.Error)}
	}
}

// ExchangeDeviceCode trades the server-minted pair for tokens. The redirect is
// the device flow's own callback on the issuer, which is what the CLI sends
// (Codex-Manager core/src/auth/mod.rs): the token endpoint expects it even
// though the user never visited it.
func ExchangeDeviceCode(ctx context.Context, client *http.Client, eps Endpoints, code, verifier string) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {strings.TrimSuffix(eps.Issuer, "/") + "/deviceauth/callback"},
		"client_id":     {ClientIDValue()},
		"code_verifier": {verifier},
	}
	return tokenRequest(ctx, client, eps, form)
}

// Model is one entry of the backend's catalogue.
type Model struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// DiscoverModels lists the backend catalogue. Slugs are what the Responses API
// accepts, so a slug wins when the entry has one.
//
// The request is the Codex CLI's own catalogue call, cross-checked against the
// two Go reimplementations (openai/codex codex-api/src/endpoint/models.rs
// request_url; Codex-Manager crates/service/src/account/account_models.rs
// fetch_account_models_async; CLIProxyAPI cmd/fetch_codex_models/main.go — all
// 2026-10-02): the client_version query naming the CLI release, the bearer, the
// ChatGPT-Account-ID the catalogue is scoped to, the originator, the CLI's
// user agent and an Accept. The account id is the field that was missing: the
// answer without it is a 400 that names nothing, which is what this call
// shipped until the account id was plumbed through from the login.
//
// session-id is per conversation and belongs to the responses turn, not the
// catalogue — none of the three sends it here, and neither do we. Origin and
// Referer are browser surfaces neither the catalogue nor passive usage needs.
func DiscoverModels(ctx context.Context, client *http.Client, modelsURL, accessToken, accountID string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURLWithVersion(modelsURL), nil)
	if err != nil {
		return nil, err
	}
	ApplyRequestHeaders(req.Header, accessToken, accountID, "")
	req.Header.Set("Accept", "application/json")
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode != http.StatusOK {
		// A 400 from this endpoint is a missing or wrong field, and the body
		// usually names it — until now the operator got the status alone. The
		// log carries the URL with its query, the status, the body's first 300
		// characters and every header the request carried; credentials appear
		// only as a prefix, so the record diagnoses without leaking.
		slog.Warn("model catalogue refused the request",
			"url", req.URL.String(),
			"status", resp.StatusCode,
			"body", firstN(string(body), 300),
			"headers", headerReport(req.Header),
		)
		return nil, fmt.Errorf("model catalogue returned HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, err
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, m := range reply.Models {
		id := firstNonEmpty(m.Slug, m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// modelsURLWithVersion appends the client_version query the catalogue expects.
// A URL that already carries one — an operator who pinned the endpoint with
// its own query — keeps it: the value is theirs, not ours to overwrite.
func modelsURLWithVersion(modelsURL string) string {
	parsed, err := url.Parse(modelsURL)
	if err != nil {
		return modelsURL
	}
	query := parsed.Query()
	if query.Get("client_version") != "" {
		return modelsURL
	}
	query.Set("client_version", ClientVersionValue())
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// headerReport names every header the request carried, with the credentials
// reduced to a prefix: the log exists to show which fields were present, not
// what they held. originator, the user agent and Accept are not secrets and
// ride verbatim.
func headerReport(h http.Header) map[string]string {
	report := make(map[string]string, len(h))
	for name, values := range h {
		if len(values) == 0 {
			report[name] = "<absent>"
			continue
		}
		report[name] = headerValueReport(name, values[0])
	}
	return report
}

func headerValueReport(name, value string) string {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization":
		return "bearer:" + firstN(strings.TrimPrefix(value, "Bearer "), 8)
	case "Chatgpt-Account-Id":
		return firstN(value, 8)
	default:
		return value
	}
}

// firstN bounds a value's length for a log attribute, rune-safely so a
// multi-byte body snippet is not cut mid-character.
func firstN(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func base64RawURL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
