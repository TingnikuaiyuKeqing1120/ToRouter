// Package copilot implements the GitHub Copilot OAuth track: the device flow
// that authorizes an account, the exchange that turns the GitHub token into a
// short-lived Copilot bearer token, the model catalogue, and the refresh
// scheduling that keeps that bearer token fresh.
//
// The wire behaviour is documented verbatim in docs/protocol-refs/
// oauth-github-copilot.md (source: cc-switch's copilot_auth.rs, 2026-10-02).
// copilot_internal/* is an undocumented interface that can change without
// notice; the 403/404 defensive branches in that source exist for a reason.
// Evidence levels: client_id, scope, device-flow grant type and error branches,
// the exchange endpoint and its Authorization/Editor-*/User-Agent triple, the
// 60s refresh skew and the fixed request headers are from that document; the
// exact editor-version / editor-plugin-version values are not stated in it and
// are environment-overridable for that reason.
package copilot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"torouter/internal/accessguard"
	"torouter/internal/store"
)

// ClientID is the public VS Code Copilot Chat client id the device flow runs
// as. It is public by construction — every Copilot client ships it.
func ClientID() string { return clientIDValue }

// Scope is the only permission the login asks for.
const Scope = "read:user"

// UserAgent is the GitHubCopilotChat UA the device flow presents.
func UserAgent() string { return userAgentValue }

// EditorVersion and EditorPluginVersion are the VS Code disguise triple the
// copilot_internal endpoints expect (with UserAgent). Evidence: named in the
// protocol ref, values not stated there — hence env-overridable.
func EditorVersion() string { return editorVersionValue }
func EditorPluginVersion() string {
	return editorPluginVersionValue
}

// IntegrationID is the copilot-integration-id header the model API expects.
const IntegrationID = "vscode-chat"

// APIVersion is the x-github-api-version pin the model API expects
// (2025-10-01, per the protocol ref).
const APIVersion = "2025-10-01"

var (
	clientIDValue            = envOr("TOROUTER_COPILOT_CLIENT_ID", "Iv1.b507a08c87ecfe98")
	userAgentValue           = envOr("TOROUTER_COPILOT_USER_AGENT", "GitHubCopilotChat/0.38.2")
	editorVersionValue       = envOr("TOROUTER_COPILOT_EDITOR_VERSION", "vscode/1.107.0")
	editorPluginVersionValue = envOr("TOROUTER_COPILOT_EDITOR_PLUGIN_VERSION", "copilot-chat/0.38.2")
)

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// Endpoints are the three HTTP bases the track talks to. The gateway, the
// console and tests all inject their own; DefaultEndpoints reads the
// environment first, which is what lets a test double replace github.com
// entirely.
type Endpoints struct {
	GitHub   string // device flow and token polling
	API      string // copilot_internal/* and the account lookup
	ModelAPI string // fallback model API base
}

// DefaultEndpoints returns the production bases, each overridable by
// environment for diagnostics.
func DefaultEndpoints() Endpoints {
	return Endpoints{
		GitHub:   envOr("TOROUTER_COPILOT_GITHUB_URL", "https://github.com"),
		API:      envOr("TOROUTER_COPILOT_API_URL", "https://api.github.com"),
		ModelAPI: envOr("TOROUTER_COPILOT_MODEL_API_URL", "https://api.githubcopilot.com"),
	}
}

// DeviceFlow is one GitHub device authorization in progress. ID is the opaque
// handle the console polls with; DeviceCode is the poll secret and never
// leaves the process.
type DeviceFlow struct {
	ID              string
	DeviceCode      string
	UserCode        string
	VerificationURI string
	Interval        int
	ExpiresAt       time.Time
}

// StartDeviceFlow asks GitHub for a device code: POST /login/device/code with
// client_id and scope (protocol ref step 1).
func StartDeviceFlow(ctx context.Context, client *http.Client, eps Endpoints) (DeviceFlow, error) {
	form := url.Values{"client_id": {ClientID()}, "scope": {Scope}}
	var reply struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	if err := postForm(ctx, client, join(eps.GitHub, "/login/device/code"), form, &reply); err != nil {
		return DeviceFlow{}, err
	}
	if reply.Error != "" {
		return DeviceFlow{}, fmt.Errorf("device flow start refused: %s", reply.Error)
	}
	if reply.DeviceCode == "" || reply.UserCode == "" || reply.VerificationURI == "" {
		return DeviceFlow{}, errors.New("device flow start returned an incomplete reply")
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return DeviceFlow{}, err
	}
	expires := time.Now().Add(15 * time.Minute)
	if reply.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second)
	}
	return DeviceFlow{
		ID:              hex.EncodeToString(id),
		DeviceCode:      reply.DeviceCode,
		UserCode:        reply.UserCode,
		VerificationURI: reply.VerificationURI,
		Interval:        reply.Interval,
		ExpiresAt:       expires,
	}, nil
}

// PollVerdict is the outcome of one device-flow poll. GitHub answers every
// poll with 200 plus either a token or an error code, so the verdict, not the
// status, is what the caller paces itself on.
type PollVerdict int

const (
	// PollPending: authorization_pending — the user has not entered the code.
	PollPending PollVerdict = iota
	// PollSlowDown: slow_down — GitHub wants a longer interval.
	PollSlowDown
	// PollComplete: authorized; GitHubToken carries the token to exchange.
	PollComplete
	// PollFailed: expired_token, access_denied, or a transport failure. Code is
	// the machine-readable branch for the terminal ones.
	PollFailed
)

// PollResult is one poll's verdict. GitHubToken is only set for PollComplete.
type PollResult struct {
	Verdict     PollVerdict
	GitHubToken string
	Code        string
	Err         error
}

// PollIntervalSlowDown is how much a slow_down answer adds to the interval
// (RFC 8628 §3.5).
const PollIntervalSlowDown = 5

// PollDeviceFlow polls POST /login/oauth/access_token once, with the device
// grant. It never sleeps: the caller owns the pacing.
func PollDeviceFlow(ctx context.Context, client *http.Client, eps Endpoints, flow DeviceFlow) PollResult {
	form := url.Values{
		"client_id":   {ClientID()},
		"device_code": {flow.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	var reply struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := postForm(ctx, client, join(eps.GitHub, "/login/oauth/access_token"), form, &reply); err != nil {
		return PollResult{Verdict: PollFailed, Code: "network", Err: err}
	}
	switch reply.Error {
	case "":
		if reply.AccessToken == "" {
			return PollResult{Verdict: PollFailed, Code: "invalid_reply", Err: errors.New("device poll returned no token")}
		}
		return PollResult{Verdict: PollComplete, GitHubToken: reply.AccessToken}
	case "authorization_pending":
		return PollResult{Verdict: PollPending}
	case "slow_down":
		return PollResult{Verdict: PollSlowDown}
	default:
		return PollResult{Verdict: PollFailed, Code: reply.Error, Err: fmt.Errorf("device flow failed: %s", reply.Error)}
	}
}

// CopilotToken is one exchange result: a JWT and the instant it dies.
type CopilotToken struct {
	Token     string
	ExpiresAt time.Time
}

// ErrUnsubscribed means GitHub answered the exchange with 403: this account has
// no Copilot subscription, so the credential can never serve a request.
var ErrUnsubscribed = errors.New("github copilot: no active subscription")

// ErrReauthRequired means GitHub answered with 401: the GitHub token itself is
// gone (revoked or expired).
var ErrReauthRequired = errors.New("github copilot: github token rejected; re-authorization required")

// ExchangeToken trades the GitHub token for a Copilot bearer token:
// GET /copilot_internal/v2/token with the VS Code triple (protocol ref step 2).
// The token is a JWT valid for a short while; the response also carries
// refresh_in, which is not used for scheduling — the expiry is.
func ExchangeToken(ctx context.Context, client *http.Client, eps Endpoints, githubToken string) (CopilotToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, join(eps.API, "/copilot_internal/v2/token"), nil)
	if err != nil {
		return CopilotToken{}, err
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Editor-Version", EditorVersion())
	req.Header.Set("Editor-Plugin-Version", EditorPluginVersion())
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("Accept", "application/json")
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return CopilotToken{}, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return CopilotToken{}, readErr
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return CopilotToken{}, ErrUnsubscribed
	case http.StatusUnauthorized:
		return CopilotToken{}, ErrReauthRequired
	default:
		return CopilotToken{}, fmt.Errorf("copilot token exchange returned HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return CopilotToken{}, err
	}
	if reply.Token == "" || reply.ExpiresAt <= 0 {
		return CopilotToken{}, errors.New("copilot token exchange returned an incomplete reply")
	}
	return CopilotToken{Token: reply.Token, ExpiresAt: time.Unix(reply.ExpiresAt, 0).UTC()}, nil
}

// CopilotUserProfile is the subset of GET /copilot_internal/user the gateway
// uses: the dynamic model API base and the plan name. The quota fields
// (quota_snapshots and friends) are deliberately not modelled — the Copilot
// quota is a monthly premium-interactions budget, not a rate window, and
// to-router reports tokens, not budget.
type CopilotUserProfile struct {
	Endpoints struct {
		API string `json:"api"`
	} `json:"endpoints"`
	CopilotPlan string `json:"copilot_plan"`
}

// FetchCopilotUser reads the account profile. It sits on api.github.com next to
// the token exchange, so it authenticates the way the other copilot_internal
// call does — with the GitHub token (inference: the protocol ref names the
// endpoint but not its scheme; matching its neighbour is the best available
// evidence). Its endpoints.api is the preferred model API base; the caller
// falls back to Endpoints.ModelAPI when it is empty.
func FetchCopilotUser(ctx context.Context, client *http.Client, eps Endpoints, githubToken string) (CopilotUserProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, join(eps.API, "/copilot_internal/user"), nil)
	if err != nil {
		return CopilotUserProfile{}, err
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Editor-Version", EditorVersion())
	req.Header.Set("Editor-Plugin-Version", EditorPluginVersion())
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("Accept", "application/json")
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return CopilotUserProfile{}, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return CopilotUserProfile{}, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return CopilotUserProfile{}, fmt.Errorf("copilot user profile returned HTTP %d", resp.StatusCode)
	}
	var profile CopilotUserProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return CopilotUserProfile{}, err
	}
	return profile, nil
}

// GitHubLogin returns the account's login for the console's channel label.
// Evidence: standard GitHub REST, not part of the Copilot protocol ref — the
// caller treats a failure as cosmetic and keeps a fallback label.
func GitHubLogin(ctx context.Context, client *http.Client, eps Endpoints, githubToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, join(eps.API, "/user"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/json")
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github user returned HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply); err != nil {
		return "", err
	}
	return reply.Login, nil
}

// DiscoverModels lists the model catalogue at apiBase and keeps only the
// entries the Copilot client itself would show (model_picker_enabled=true,
// protocol ref step 4). Names are sorted for a stable channel import.
func DiscoverModels(ctx context.Context, client *http.Client, apiBase, copilotToken string) ([]string, map[string][]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, join(apiBase, "/models"), nil)
	if err != nil {
		return nil, nil, err
	}
	ApplyRequestHeaders(req.Header, copilotToken)
	req.Header.Set("Accept", "application/json")
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return nil, nil, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("copilot model catalogue returned HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Data []struct {
			ID                 string          `json:"id"`
			Name               string          `json:"name"`
			Vendor             string          `json:"vendor"`
			SupportedEndpoints json.RawMessage `json:"supported_endpoints"`
			ModelPickerEnabled bool            `json:"model_picker_enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, nil, err
	}
	ids := []string{}
	formats := map[string][]string{}
	seen := map[string]bool{}
	for _, m := range reply.Data {
		if m.ID == "" || !m.ModelPickerEnabled || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		ids = append(ids, m.ID)
		protocols := []string{}
		if m.SupportedEndpoints == nil {
			protocols = append(protocols, store.FormatOpenAIChatCompletions)
		} else {
			var endpoints []string
			if err := json.Unmarshal(m.SupportedEndpoints, &endpoints); err != nil {
				return nil, nil, err
			}
			for _, endpoint := range endpoints {
				format := ""
				switch endpoint {
				case "/chat/completions":
					format = store.FormatOpenAIChatCompletions
				case "/v1/messages", "/messages":
					format = store.FormatAnthropicMessages
				}
				if format != "" && !slices.Contains(protocols, format) {
					protocols = append(protocols, format)
				}
			}
		}
		formats[m.ID] = protocols
	}
	return ids, formats, nil
}

// ApplyRequestHeaders stamps the fixed Copilot client headers onto an upstream
// request: the bearer token plus the VS Code disguise triple and the API
// version pin (protocol ref step 3). The gateway calls this on every
// copilot_oauth outbound request, exactly as cc-switch's proxy does.
func ApplyRequestHeaders(h http.Header, copilotToken string) {
	h.Set("Authorization", "Bearer "+copilotToken)
	h.Set("copilot-integration-id", IntegrationID)
	h.Set("editor-version", EditorVersion())
	h.Set("editor-plugin-version", EditorPluginVersion())
	h.Set("x-github-api-version", APIVersion)
	h.Set("User-Agent", UserAgent())
}

func postForm(ctx context.Context, client *http.Client, target string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent())
	resp, err := accessguard.Do(client, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d", target, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return err
	}
	return nil
}

// join appends a path to a base URL, keeping the base's own path prefix.
func join(base, path string) string {
	return strings.TrimRight(base, "/") + path
}
