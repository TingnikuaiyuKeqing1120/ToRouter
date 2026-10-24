// Package siwc implements the official Sign-in-with-ChatGPT track
// (docs/protocol-refs/oauth-openai-siwc.md): the authorization-code flow with
// PKCE, the loopback callback, the id_token verification, the token rotation
// and the narrow inference profile the gateway enforces on the way out.
//
// Everything here is a preview interface maintained by OpenAI; the endpoint
// table and the parameter lists come from the protocol ref, which was verified
// page by page on 2026-10-02. Where the ref is silent the code says so instead
// of pretending the answer is known.
package siwc

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
	"torouter/internal/accessguard"

	"torouter/internal/oauthbase"
)

// The scope set the flow asks for, verbatim from the protocol ref. The last
// entry is the one that gates ChatGPT-plan inference; a consent that was
// refused comes back without it.
const (
	Scope        = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	ScopeDirect  = "chatgpt.tokens.use.direct"
	Resource     = "https://api.openai.com/v1"
	DynamicAppID = "dynamic_agent_client"
	// AgentNameHint is sent on the first registration only; a re-authorization
	// omits it (protocol ref, 授权流程).
	AgentNameHint = "to-router"
	// CallbackPath is fixed: the ref pins scheme, host and path, and forbids
	// "localhost" as the host.
	CallbackPath = "/auth/callback"
)

// Endpoints are the SIWC bases. Tests inject their own; DefaultEndpoints reads
// the environment first.
type Endpoints struct {
	Issuer    string
	Authorize string
	Token     string
	Revoke    string
	Userinfo  string
	JWKS      string
	Inference string
	Models    string
}

// DefaultEndpoints returns the production bases from the protocol ref's
// endpoint table, each overridable by environment for diagnostics.
func DefaultEndpoints() Endpoints {
	return Endpoints{
		Issuer:    envOr("TOROUTER_SIWC_ISSUER_URL", "https://auth.openai.com"),
		Authorize: envOr("TOROUTER_SIWC_AUTHORIZE_URL", "https://auth.openai.com/api/accounts/authorize"),
		Token:     envOr("TOROUTER_SIWC_TOKEN_URL", "https://auth.openai.com/api/accounts/oauth/token"),
		Revoke:    envOr("TOROUTER_SIWC_REVOKE_URL", "https://auth.openai.com/api/accounts/oauth/revoke"),
		Userinfo:  envOr("TOROUTER_SIWC_USERINFO_URL", "https://auth.openai.com/api/accounts/oauth/userinfo"),
		JWKS:      envOr("TOROUTER_SIWC_JWKS_URL", "https://auth.openai.com/.well-known/jwks.json"),
		Inference: envOr("TOROUTER_SIWC_INFERENCE_URL", "https://api.openai.com/v1"),
		Models:    envOr("TOROUTER_SIWC_MODELS_URL", "https://api.openai.com/v1/models"),
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// EmailHintOf reads the email claim out of a stored id_token, or "" when it has
// none. It is an unverified parse on purpose: the only consumer is the advisory
// login_hint on a re-authorization, where a wrong value costs a form the user
// corrects by hand and a verified parse would cost a JWKS round trip for
// decoration. Signature verification happens in VerifyIDToken, where it matters.
func EmailHintOf(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.Email)
}

// IDTokenHint is the optional account preselect a re-authorization can send:
// the id_token of the account the channel already holds, and — when that token
// carries an email — the login_hint derived from it.
type IDTokenHint struct {
	IDToken   string
	LoginHint string
}

// AuthRequest is one authorization URL plus the material needed to finish it.
// Verifier never leaves the process.
type AuthRequest struct {
	URL         string
	RedirectURI string
	ClientID    string
	HostID      string
	State       string
	Nonce       string
	Verifier    string
}

// AuthorizeURL builds the authorization URL verbatim from the protocol ref:
// the registration entry point or the stored oaiapp_* client, the pinned
// loopback redirect, PKCE S256, a fresh state and nonce every time, the scope
// set and the resource parameter. agent_name_hint rides along on the first
// registration only.
//
// ext_agent_host_id rides along as a query parameter, which is its documented
// transport for this flow.
func AuthorizeURL(eps Endpoints, clientID, hostID, redirectURI string, hint IDTokenHint) (AuthRequest, error) {
	if clientID == "" {
		return AuthRequest{}, errors.New("client id is required")
	}
	if hostID == "" {
		return AuthRequest{}, errors.New("ext_agent_host_id is required")
	}
	if !strings.HasPrefix(redirectURI, "http://127.0.0.1:") || !strings.HasSuffix(redirectURI, CallbackPath) {
		return AuthRequest{}, fmt.Errorf("redirect must be http://127.0.0.1:<port>%s, not %q", CallbackPath, redirectURI)
	}
	state, err := oauthbase.RandomToken(24)
	if err != nil {
		return AuthRequest{}, err
	}
	nonce, err := oauthbase.RandomToken(24)
	if err != nil {
		return AuthRequest{}, err
	}
	pkce, err := oauthbase.NewPKCE()
	if err != nil {
		return AuthRequest{}, err
	}
	verifier, challenge := pkce.Verifier, pkce.Challenge
	query := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {Scope},
		"resource":              {Resource},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"ext_agent_host_id":     {hostID},
	}
	if clientID == DynamicAppID {
		query.Set("agent_name_hint", AgentNameHint)
	}
	// A re-authorization can preselect the account it belongs to. Both hints are
	// optional and advisory: the issuer may ignore them, and the flow's own
	// binding to the expected subject is what actually protects the channel.
	if hint.IDToken != "" {
		query.Set("id_token_hint", hint.IDToken)
	}
	if hint.LoginHint != "" {
		query.Set("login_hint", hint.LoginHint)
	}
	return AuthRequest{
		URL:         eps.Authorize + "?" + query.Encode(),
		RedirectURI: redirectURI,
		ClientID:    clientID,
		HostID:      hostID,
		State:       state,
		Nonce:       nonce,
		Verifier:    verifier,
	}, nil
}

// CallbackResult, the loopback listener and the pasted-callback parser are
// shared with the other ChatGPT-family tracks; only the host a flow pins is a
// per-track decision, so the primitives live in internal/oauthbase and this
// track pins 127.0.0.1 through them.
type CallbackResult = oauthbase.CallbackResult

// CallbackPorts are the loopback ports this track pins: 1455 is the port the
// reference's clients register with the issuer (the CLI source comment says it
// is kept in sync with the issuer's allow-list), and 1457 is the fallback for
// when an earlier authorization still holds 1455.
var CallbackPorts = []int{1455, 1457}

// ListenCallback binds the loopback listener on 127.0.0.1: the SIWC ref pins
// scheme, host and path, and forbids "localhost" as the host. The port is the
// pinned pair, not a random one, because the redirect the issuer allow-lists
// must land on the port the flow was built with.
func ListenCallback() (*CallbackListener, error) {
	return oauthbase.ListenCallbackOn("127.0.0.1", CallbackPorts...)
}

// CallbackListener is the loopback half of the flow.
type CallbackListener = oauthbase.CallbackListener

// ParseCallbackURL reads a pasted callback for a flow pinned to 127.0.0.1.
func ParseCallbackURL(raw string) (CallbackResult, error) {
	return oauthbase.ParseCallbackURLOn(raw, "127.0.0.1")
}

// TokenResponse is the token endpoint's reply. ExpiresIn is seconds. Error and
// ErrorDescription are the OAuth failure body, zero on success.
type TokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	ClientID         string `json:"client_id"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Code reports the machine-readable error code, if any.
func (r TokenResponse) Code() string { return r.Error }

// ExchangeCode trades the authorization code for tokens. There is no client
// secret (protocol ref, 授权流程): the PKCE verifier is the proof. Which
// registered client the exchange reports is checked by the caller against the
// callback's (see resolveIssuedClientID), not here.
//
// issuedClientID is the client the exchange must authenticate as, and it is NOT
// auth.ClientID: dynamic_agent_client is the registration entry point, not a
// client a code exchange may use (the first authorization's callback issues the
// real oaiapp_* id). A caller that passes the entry point here gets a refusal
// from the issuer, so the resolution lives in resolveIssuedClientID.
func ExchangeCode(ctx context.Context, client *http.Client, eps Endpoints, auth AuthRequest, code, issuedClientID string) (TokenResponse, error) {
	if issuedClientID == "" || issuedClientID == DynamicAppID {
		return TokenResponse{}, errors.New("the code exchange requires the issued oaiapp_ client id, not the registration entry point")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {auth.RedirectURI},
		"client_id":     {issuedClientID},
		"code_verifier": {auth.Verifier},
		"resource":      {Resource},
	}
	return tokenRequest(ctx, client, eps, form)
}

// RefreshToken rotates the refresh token. The response carries a new one and
// re-arms its 30 days; a rotation that did not issue one means the credential
// is already gone, and the caller marks it rather than retrying.
func RefreshToken(ctx context.Context, client *http.Client, eps Endpoints, refreshToken, clientID string) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"resource":      {Resource},
	}
	return tokenRequest(ctx, client, eps, form)
}

// ErrClientMisconfigured means the issuer did not recognize the client: a
// configuration problem rather than a credential one, so the fix is not a fresh
// authorization.
var ErrClientMisconfigured = errors.New("siwc: the issuer did not recognize this client id; the channel's client configuration is wrong")

// ErrReauthRequired covers every token-endpoint answer that says the credential
// is finished: invalid_grant, the two rotation failure codes the protocol ref
// names as the reason refresh must be serialized, and the expiry synonyms.
var ErrReauthRequired = errors.New("siwc: credential rejected; re-authorization required")

// ErrRenewableSessionMissing means the code exchange produced no refresh
// token: the consent did not include offline_access, so the credential cannot
// be rotated and is not worth storing. It is the exchange-side twin of the
// refresh-side invariant that a successful rotation always returns a
// replacement refresh token.
var ErrRenewableSessionMissing = errors.New("siwc: the authorization did not grant offline_access, so the credential cannot be renewed; re-run the authorization and accept all permissions")

// DetailOf is the issuer's own error code and description a token-endpoint
// rejection carried, or "" when it carried none. The wrapped error keeps the
// sentinel first, so the detail is what follows it; the console appends it to
// its fixed advice instead of swallowing the issuer's own words. A bare
// sentinel carries no detail of its own and answers empty.
func DetailOf(err error) string {
	text := err.Error()
	for _, sentinel := range []error{ErrReauthRequired, ErrClientMisconfigured} {
		prefix := sentinel.Error()
		if !strings.HasPrefix(text, prefix) {
			continue
		}
		detail := strings.TrimSpace(strings.TrimPrefix(text, prefix))
		detail = strings.TrimSpace(strings.TrimPrefix(detail, ":"))
		// A bare sentinel — or a detail that is the sentinel itself — carries
		// nothing to add beside the console's own advice.
		if detail == "" || detail == prefix {
			return ""
		}
		return detail
	}
	return ""
}

// ErrScopeUndetermined means neither the token response nor the callback said
// which scopes were granted. The consent cannot be judged, so the login is
// refused rather than assumed.
var ErrScopeUndetermined = errors.New("siwc: the granted scope set could not be determined; re-run the authorization and check the consent page")

// ErrConsentMissingScope means the consent did not include
// chatgpt.tokens.use.direct: the user can sign in without granting ChatGPT-plan
// usage, and the resulting token is useless for inference (protocol ref,
// 资格 — errors-and-recovery).
var ErrConsentMissingScope = errors.New("siwc: consent is missing chatgpt.tokens.use.direct; re-run the authorization with the full scope set")

func tokenRequest(ctx context.Context, client *http.Client, eps Endpoints, form url.Values) (TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, eps.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// ext_agent_host_id is not sent here: the documented transport is the
	// authorize query parameter (AuthorizeURL sends it), and the token endpoint
	// is not documented to read it.
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
		// An invalid_grant here is the failure with the fewest clues: the browser
		// half of the flow just succeeded, the code was minted seconds ago, and
		// the issuer often answers with a bare error code. This log exists for
		// diagnosing that after the fact, and it deliberately carries no secret:
		// the status, the issuer's own error and description, and a summary of
		// the request — the client id's prefix, the redirect and resource (both
		// identifiers, not credentials) and the verifier's length rather than its
		// value. The refresh token, the code and the verifier itself never
		// appear.
		slog.Info("token endpoint refused the request",
			"status", resp.StatusCode,
			"error", reply.Error,
			"error_description", reply.ErrorDescription,
			"raw_body", firstN(string(body), 500),
			"grant_type", form.Get("grant_type"),
			"client_id_prefix", firstN(form.Get("client_id"), 12),
			"redirect_uri", form.Get("redirect_uri"),
			"resource", form.Get("resource"),
			"code_verifier_len", len(form.Get("code_verifier")),
		)
		switch reply.Code() {
		case "invalid_grant", "refresh_token_reused", "refresh_token_invalidated",
			"invalid_refresh_token", "token_expired", "refresh_token_expired":
			// The credential is finished: retrying would meet the same answer, and
			// the channel must be marked rather than rotated against a dead token.
			// The issuer's description rides along so the console can show why.
			return TokenResponse{}, fmt.Errorf("%w: %s", ErrReauthRequired, errorDetail(reply))
		case "invalid_client":
			// Not a dead credential but a broken configuration — a client id the
			// issuer does not recognize. Retrying the refresh cannot fix that, and
			// "re-authorize" is the wrong advice, so it carries its own message.
			return TokenResponse{}, fmt.Errorf("%w: %s", ErrClientMisconfigured, errorDetail(reply))
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

// firstN is the first n characters of s, for logging an identifier's prefix.
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// errorDetail renders the issuer's machine-readable error with its description
// when it sent one.
func errorDetail(reply TokenResponse) string {
	if desc := strings.TrimSpace(reply.ErrorDescription); desc != "" {
		return fmt.Sprintf("%s: %s", reply.Error, desc)
	}
	return reply.Error
}

// HasDirectUse reports whether the granted scope set includes the ChatGPT-plan
// usage scope. The check is exact-token: "chatgpt.tokens.use" is a different
// permission.
func HasDirectUse(scopes string) bool {
	for _, s := range strings.Fields(scopes) {
		if s == ScopeDirect {
			return true
		}
	}
	return false
}

// DiscoverModels lists the model catalogue and keeps the entries the profile
// exposes (visibility=="list", protocol ref 端点表). Slugs are what the
// Responses API accepts, so slugs are what a channel binds.
func DiscoverModels(ctx context.Context, client *http.Client, modelsURL, accessToken string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
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
		return nil, fmt.Errorf("model catalogue returned HTTP %d", resp.StatusCode)
	}
	// The catalogue is models[]{slug, display_name, visibility}: a slug is what
	// the Responses API accepts, so a slug is what a channel binds, and an entry
	// the profile does not list (visibility != "list") is not offered.
	var reply struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, err
	}
	slugs := []string{}
	seen := map[string]bool{}
	for _, m := range reply.Models {
		if m.Slug == "" || m.Visibility != "list" || seen[m.Slug] {
			continue
		}
		seen[m.Slug] = true
		slugs = append(slugs, m.Slug)
	}
	return slugs, nil
}

// ApplyRequestHeaders stamps the bearer token onto an inference request. The
// SIWC profile adds no client disguise: it is a first-party API, not a
// reverse-engineered one.
func ApplyRequestHeaders(h http.Header, accessToken string) {
	h.Set("Authorization", "Bearer "+accessToken)
}
