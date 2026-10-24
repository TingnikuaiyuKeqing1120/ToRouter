// Package claude implements native subscription OAuth and Anthropic Messages.
// The pinned request profile is adapted from CLIProxyAPI; see SOURCE.md.
package claude

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

const (
	ClientID       = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	RedirectURI    = "http://localhost:54545/callback"
	Scope          = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	maxControlBody = 1 << 20
)

type Endpoints struct {
	AuthorizeURL string
	TokenURL     string
	RefreshURL   string
	ProfileURL   string
	RolesURL     string
	APIBase      string
}

type Driver struct {
	client    *http.Client
	endpoints Endpoints
}

var _ nativeoauth.Driver = (*Driver)(nil)

func NewDriver(client *http.Client, endpoints Endpoints) *Driver {
	if endpoints.AuthorizeURL == "" {
		endpoints.AuthorizeURL = "https://claude.ai/oauth/authorize"
	}
	if endpoints.TokenURL == "" {
		endpoints.TokenURL = "https://platform.claude.com/v1/oauth/token"
	}
	if endpoints.RefreshURL == "" {
		endpoints.RefreshURL = endpoints.TokenURL
	}
	if endpoints.ProfileURL == "" {
		endpoints.ProfileURL = "https://api.anthropic.com/api/oauth/profile"
	}
	if endpoints.RolesURL == "" {
		endpoints.RolesURL = "https://api.anthropic.com/api/oauth/claude_cli/roles"
	}
	if endpoints.APIBase == "" {
		endpoints.APIBase = "https://api.anthropic.com"
	}
	return &Driver{client: client, endpoints: endpoints}
}

func (d *Driver) Definition() nativeoauth.Definition {
	return nativeoauth.Definition{ID: "claude", Name: "Claude", CredentialType: store.CredentialClaudeOAuth, Provider: "claude", CallbackHost: "localhost", CallbackPath: "/callback", CallbackPorts: []int{54545}, Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: d.endpoints.APIBase}}}
}

func (d *Driver) Begin(ctx context.Context, redirectURI string) (nativeoauth.Authorization, error) {
	if err := ctx.Err(); err != nil {
		return nativeoauth.Authorization{}, err
	}
	if err := accessguard.Check(ctx); err != nil {
		return nativeoauth.Authorization{}, err
	}
	if redirectURI != RedirectURI {
		return nativeoauth.Authorization{}, errors.New("Claude OAuth requires its registered localhost callback")
	}
	pkce, err := oauthbase.NewPKCE()
	if err != nil {
		return nativeoauth.Authorization{}, err
	}
	state, err := oauthbase.RandomToken(32)
	if err != nil {
		return nativeoauth.Authorization{}, err
	}
	u, err := url.Parse(d.endpoints.AuthorizeURL)
	if err != nil {
		return nativeoauth.Authorization{}, errors.New("invalid Claude authorization endpoint")
	}
	q := u.Query()
	for key, value := range map[string]string{"code": "true", "client_id": ClientID, "response_type": "code", "redirect_uri": redirectURI, "scope": Scope, "code_challenge": pkce.Challenge, "code_challenge_method": "S256", "state": state} {
		q.Set(key, value)
	}
	u.RawQuery = q.Encode()
	return nativeoauth.Authorization{URL: u.String(), State: state, Verifier: pkce.Verifier, RedirectURI: redirectURI}, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Account      struct {
		UUID string `json:"uuid"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
}

func (d *Driver) Exchange(ctx context.Context, auth nativeoauth.Authorization, cb oauthbase.CallbackResult) (store.OAuthCredential, error) {
	if cb.Error != "" {
		return store.OAuthCredential{}, errors.New("Claude authorization was rejected")
	}
	code, fragment, _ := strings.Cut(cb.Code, "#")
	state := cb.State
	if fragment != "" {
		if state != "" && !sameState(state, fragment) {
			return store.OAuthCredential{}, errors.New("Claude callback state mismatch")
		}
		state = fragment
	}
	if auth.State == "" || !sameState(auth.State, state) || code == "" || auth.Verifier == "" || auth.RedirectURI != RedirectURI {
		return store.OAuthCredential{}, errors.New("invalid Claude callback state or authorization material")
	}
	// Preserve the native control-plane field order.
	payload := struct {
		GrantType    string `json:"grant_type"`
		Code         string `json:"code"`
		RedirectURI  string `json:"redirect_uri"`
		ClientID     string `json:"client_id"`
		CodeVerifier string `json:"code_verifier"`
		State        string `json:"state"`
	}{"authorization_code", code, auth.RedirectURI, ClientID, auth.Verifier, auth.State}
	var tr tokenResponse
	if err := d.control(ctx, http.MethodPost, d.endpoints.TokenURL, "", payload, &tr); err != nil {
		return store.OAuthCredential{}, err
	}
	credential, err := applyToken(store.OAuthCredential{Provider: "claude", ClientID: ClientID, Scopes: Scope, APIBase: d.endpoints.APIBase}, tr)
	if err != nil {
		return store.OAuthCredential{}, err
	}
	device, err := newDeviceID()
	if err != nil {
		return credential, err
	}
	credential.DeviceID = device
	d.inspect(ctx, &credential)
	return credential, nil
}

func sameState(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func applyToken(old store.OAuthCredential, tr tokenResponse) (store.OAuthCredential, error) {
	if strings.TrimSpace(tr.AccessToken) == "" || tr.ExpiresIn <= 0 || tr.ExpiresIn > 31536000 || (tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer")) {
		return old, errors.New("invalid Claude token response")
	}
	old.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		old.RefreshToken = tr.RefreshToken
	}
	if tr.Scope != "" {
		old.Scopes = tr.Scope
	}
	if tr.Account.UUID != "" {
		old.Account = tr.Account.UUID
		old.Subject = tr.Account.UUID
	}
	if tr.Organization.UUID != "" {
		old.OrganizationID = tr.Organization.UUID
	}
	old.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	old.UpdatedAt = time.Now().UTC()
	old.Status = store.OAuthStatusActive
	old.StatusReason = ""
	return old, nil
}

func (d *Driver) Refresh(ctx context.Context, old store.OAuthCredential) (store.OAuthCredential, error) {
	if old.RefreshToken == "" {
		return old, errors.New("Claude refresh token is missing; authorize again")
	}
	clientID := old.ClientID
	if clientID == "" {
		clientID = ClientID
	}
	payload := struct {
		ClientID     string `json:"client_id"`
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}{clientID, "refresh_token", old.RefreshToken, Scope}
	var tr tokenResponse
	if err := d.control(ctx, http.MethodPost, d.endpoints.RefreshURL, "", payload, &tr); err != nil {
		return old, err
	}
	credential, err := applyToken(old, tr)
	if err != nil {
		return old, err
	}
	// Refresh preserves the known account/device identity. Profile enrichment
	// is part of consent, not a second network dependency after token rotation.
	return credential, nil
}

func (d *Driver) inspect(ctx context.Context, credential *store.OAuthCredential) {
	var profile struct {
		Account struct {
			UUID string `json:"uuid"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if d.control(ctx, http.MethodGet, d.endpoints.ProfileURL, credential.AccessToken, nil, &profile) == nil {
		if profile.Account.UUID != "" {
			credential.Account = profile.Account.UUID
			credential.Subject = profile.Account.UUID
		}
		if profile.Organization.UUID != "" {
			credential.OrganizationID = profile.Organization.UUID
		}
	}
	// Claude Code queries roles after login. It is evidence only, never a model
	// entitlement inferred from this response or from the static catalog.
	_ = d.control(ctx, http.MethodGet, d.endpoints.RolesURL, credential.AccessToken, nil, nil)
}

// A static CPA maintenance list is not an account entitlement probe.
func (d *Driver) Models(ctx context.Context, _ store.OAuthCredential) ([]nativeoauth.Model, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	return append([]nativeoauth.Model(nil), maintainedModels...), nil
}

type wireError struct {
	operation string
	cause     error
}

func (e *wireError) Error() string { return "Claude " + e.operation + " failed" }
func (e *wireError) Unwrap() error { return e.cause }

func safeError(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if accessguard.Denied(err) {
		return err
	}
	return &wireError{operation: op, cause: err}
}

func (d *Driver) control(ctx context.Context, method, endpoint, token string, payload any, out any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return errors.New("invalid Claude control request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Claude control endpoint")
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", profileControlUA)
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	req.Header.Set("Connection", "close")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Cache-Control", "no-cache")
	}
	client, err := d.profileClient(d.client, false)
	if err != nil {
		return err
	}
	response, err := accessguard.Do(client, req)
	if err != nil {
		return safeError(ctx, "OAuth request", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Claude OAuth returned HTTP %d", response.StatusCode)
	}
	reader, err := decodedBody(response)
	if err != nil {
		return safeError(ctx, "OAuth response decoding", err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maxControlBody+1))
	if err != nil {
		return safeError(ctx, "OAuth response reading", err)
	}
	if len(raw) > maxControlBody {
		return errors.New("Claude OAuth response exceeds size limit")
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return errors.New("invalid Claude OAuth JSON response")
	}
	return nil
}

func validDeviceID(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}
