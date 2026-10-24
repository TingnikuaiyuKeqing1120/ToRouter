package store

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"torouter/internal/compatprofile"
)

// Credential types. A channel without the field is an api_key channel: every
// row written before OAuth existed reads back that way.
const (
	CredentialAPIKey       = "api_key"
	CredentialCopilotOAuth = "copilot_oauth"
	// CredentialChatGPTOAuth is the official Sign-in-with-ChatGPT track
	// (docs/protocol-refs/oauth-openai-siwc.md). Its profile is narrow: the
	// Responses endpoint only, no server-side state, and a fixed omit list the
	// gateway enforces on the way out.
	CredentialChatGPTOAuth = "chatgpt_oauth"
	// CredentialCodexOAuth is the community Codex backend track
	// (docs/protocol-refs/oauth-openai-codex.md): it reuses the Codex CLI's
	// public client identity against chatgpt.com. Not OpenAI-official; the
	// console labels it as such and the capability surface is unverified, so
	// the gateway passes requests through instead of enforcing a profile.
	CredentialCodexOAuth       = "codex_oauth"
	CredentialClaudeOAuth      = "claude_oauth"
	CredentialAntigravityOAuth = "antigravity_oauth"
	CredentialDevinOAuth       = "devin_oauth"
	CredentialZenFree          = "zen_free"
)

// OAuth credential status values. "active" and "" both mean usable; the other
// two are the failure marks the gateway and the console read.
const (
	OAuthStatusActive         = "active"
	OAuthStatusUnsubscribed   = "unsubscribed"
	OAuthStatusReauthRequired = "reauth_required"
)

// OAuth providers.
const (
	OAuthProviderCopilot     = "copilot"
	OAuthProviderChatGPT     = "chatgpt"
	OAuthProviderCodex       = "codex"
	OAuthProviderClaude      = "claude"
	OAuthProviderAntigravity = "antigravity"
	OAuthProviderDevin       = "devin"
)

// OAuthCredential is the OAuth half of a channel's credential, sealed field by
// field the way APIKey is (see sealOAuthFields). AccessToken is short-lived and
// rotates; the long-lived token is the identity the sessions are pinned to
// (GitHubToken for Copilot, RefreshToken for SIWC), so only it — plus the
// per-track identity fields — feeds the continuation generation. A bearer or
// access rotation must never invalidate a pin.
//
// The two tracks deliberately do not share the long-lived slot: Copilot's
// GitHub token and SIWC's refresh token play the same role in different
// protocols, and a field named after one of them would be silently reused by
// the other.
type OAuthCredential struct {
	Provider    string `json:"provider"`
	GitHubToken string `json:"github_token"`
	// SIWC material (docs/protocol-refs/oauth-openai-siwc.md).
	RefreshToken   string `json:"refresh_token"`
	IDToken        string `json:"id_token"`
	ClientID       string `json:"client_id"` // the registered oaiapp_* client, not dynamic_agent_client
	HostID         string `json:"host_id"`   // urn:uuid, generated once per host and reused
	Subject        string `json:"subject"`   // account subject from the id_token
	Account        string `json:"account"`   // account id (Codex pins identity to it)
	Scopes         string `json:"scopes"`    // space-separated, as the token response reports them
	OrganizationID string `json:"organization_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
	// PluginState is the complete opaque state of a verified external adapter.
	// It is sealed as one value and is never included in OAuthInfo.
	PluginState string `json:"plugin_state,omitempty"`

	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountLabel string    `json:"account_label,omitempty"`
	APIBase      string    `json:"api_base,omitempty"`
	Status       string    `json:"status,omitempty"`
	StatusReason string    `json:"status_reason,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// OAuthInfo is the credential without its secrets: what the console shows and
// round-trips on save. A client can never supply a real GitHub token through
// the API, so an OAuthCredential whose GitHubToken is empty means "unchanged"
// (see the console save path), not "cleared".
type OAuthInfo struct {
	Provider     string    `json:"provider"`
	AccountLabel string    `json:"account_label,omitempty"`
	APIBase      string    `json:"api_base,omitempty"`
	Status       string    `json:"status,omitempty"`
	StatusReason string    `json:"status_reason,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (o OAuthCredential) Public() OAuthInfo {
	return OAuthInfo{Provider: o.Provider, AccountLabel: o.AccountLabel, APIBase: o.APIBase, Status: o.Status, StatusReason: o.StatusReason, ExpiresAt: o.ExpiresAt, UpdatedAt: o.UpdatedAt}
}

// oauthHostAllowlist is the ROADMAP hard rule — "不把 OAuth 凭证发往普通自定义
// 上游" — made concrete per credential type: only the hosts the issuer itself
// documents may ever receive the token. SaveChannel enforces it and the gateway
// re-checks it per request, so a row edited directly in the database cannot aim
// a credential anywhere else.
//
// Copilot returns a per-account api base from copilot_internal/user
// (endpoints.api). Individual accounts get their own segment,
// api.individual.githubcopilot.com, so it is a member alongside the shared host:
// refusing it would make every Individual authorization fail at channel
// creation. Observation carried with it (unverified, external sources
// 2026-10-02): the segmented host may not serve newer models' Responses API —
// which is one more reason copilot_oauth channels declare only the two
// documented facades and never the Responses format.
var oauthHostAllowlist = map[string][]string{
	CredentialCopilotOAuth: {"api.githubcopilot.com", "api.individual.githubcopilot.com", "api.github.com"},
	// SIWC: the OIDC issuer two hosts and the one inference endpoint the
	// profile allows (docs/protocol-refs/oauth-openai-siwc.md, 端点表).
	CredentialChatGPTOAuth: {"api.openai.com", "auth.openai.com"},
	// The Codex track has two hosts: its authorization surface is the auth
	// issuer (auth.openai.com — the same one SIWC uses) and its inference
	// backend lives on chatgpt.com. Both are allow-listed, because the
	// credential travels to each for a different part of the flow.
	CredentialCodexOAuth: {"auth.openai.com", "chatgpt.com"},
	// Native subscription profiles verified against the fixed CPA/Magpie
	// sources. Token/control-plane URLs belong to their drivers, not channels.
	CredentialClaudeOAuth:      {"api.anthropic.com"},
	CredentialAntigravityOAuth: {"daily-cloudcode-pa.googleapis.com", "daily-cloudcode-pa.sandbox.googleapis.com", "cloudcode-pa.googleapis.com"},
	CredentialDevinOAuth:       {"server.codeium.com"},
	CredentialZenFree:          {"opencode.ai"},
}

// extraAllowedHostsEnv is a deliberately awkward escape hatch for tests and
// local diagnostics: pointing an OAuth credential at a host that is not on the
// allowlist is exactly what the rule forbids, so it takes an explicit
// environment variable and an operator who means it. Empty by default.
//
// What it suspends, all at once: the host allowlist (for every credential type,
// including ones not implemented yet such as chatgpt_oauth) and the https
// requirement on OAuth endpoints — a diagnostic double is plain HTTP on
// loopback, so both would block it. Those two rules are one decision: "this
// credential may leave for this host", and the variable says the operator has
// made it deliberately. Nothing in the process sets it on its own.
const extraAllowedHostsEnv = "TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS"

// OAuthHostAllowed reports whether baseURL's host may receive this credential
// type's token. Comparison is on the hostname only — scheme, port and path are
// the endpoint declaration's business — and it is exact, so a lookalike such as
// api.githubcopilot.com.evil.test is not a member.
//
// The escape hatch is deliberately not type-scoped: a host named there receives
// every credential type, chatgpt_oauth included, because the variable is one
// operator decision about one host rather than a per-track setting.
func OAuthHostAllowed(credentialType, baseURL string) bool {
	// Market code owns its network destinations. No real token is placed on
	// this URL by the gateway; the installed supplier's loader authenticates.
	if IsMarketOAuth(credentialType) {
		return validBaseURL(baseURL)
	}
	if profile, ok := compatprofile.Lookup(credentialType); ok {
		u, err := url.Parse(baseURL)
		return err == nil && u.User == nil && u.Scheme+"://"+u.Host == profile.API || extraHost(baseURL)
	}
	return hostAllowed(credentialType, baseURL) || extraHost(baseURL)
}

// extraHost reports whether baseURL's host was named in the escape-hatch
// environment variable.
func extraHost(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, candidate := range strings.Split(os.Getenv(extraAllowedHostsEnv), ",") {
		if candidate = strings.ToLower(strings.TrimSpace(candidate)); candidate != "" && candidate == host {
			return true
		}
	}
	return false
}

// hostAllowed reports whether baseURL's host is on the credential type's
// production allowlist.
func hostAllowed(credentialType, baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range oauthHostAllowlist[credentialType] {
		if allowed == host {
			return true
		}
	}
	return false
}

// endpointRelaxedForDiagnostics reports whether an endpoint declaration is
// aimed at a diagnostic host, where the https rule is suspended along with the
// allowlist: a host named in the escape hatch is by definition not the
// production issuer. Without the variable both rules apply unchanged.
// EndpointRelaxedForDiagnostics reports whether an endpoint declaration is
// aimed at a diagnostic host, where the https rule is suspended along with the
// allowlist. Exported because the gateway's runtime re-check applies the same
// two rules SaveChannel does.
func EndpointRelaxedForDiagnostics(baseURL string) bool { return extraHost(baseURL) }

// sealedOAuthFields are the credential's secrets, sealed one field at a time.
// A new track's secret belongs here and in openOAuth, nowhere else: a field
// that is only half-wired would land in the database in plaintext.
func sealOAuthFields(s *Store, o OAuthCredential) (OAuthCredential, error) {
	var err error
	for _, field := range []*string{&o.GitHubToken, &o.RefreshToken, &o.IDToken, &o.AccessToken, &o.OrganizationID, &o.DeviceID, &o.ProjectID, &o.PluginState} {
		if *field, err = s.seal(*field); err != nil {
			return OAuthCredential{}, err
		}
	}
	return o, nil
}

func openOAuthFields(s *Store, o OAuthCredential) (OAuthCredential, error) {
	fields := []*string{&o.GitHubToken, &o.RefreshToken, &o.IDToken, &o.AccessToken}
	// Optional provider fields may be absent, including in rows saved before
	// those providers existed. Only present ciphertext is decrypted; the four
	// original sealed slots and all nonempty values still authenticate strictly.
	for _, field := range []*string{&o.OrganizationID, &o.DeviceID, &o.ProjectID, &o.PluginState} {
		if *field != "" {
			fields = append(fields, field)
		}
	}
	var err error
	for _, field := range fields {
		if *field, err = s.open(*field); err != nil {
			return OAuthCredential{}, err
		}
	}
	return o, nil
}

// OAuthTokens is a rotation payload: the access token plus whatever long-lived
// material the issuer issued beside it. An empty RefreshToken keeps the stored
// one, which is what Copilot rotations do (they issue none); SIWC always issues
// a new one, and a rotation that did not would mean the credential is already
// gone.
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	// Scopes is the granted scope set the issuer reported, written in the same
	// statement as the tokens: a rotation that changed the consent must not
	// leave the stored set describing the previous one.
	Scopes string
}

// ReplaceOAuthTokens swaps in a freshly exchanged access token, leaving the rest
// of the credential — the GitHub or refresh token that authorizes the next
// exchange, the account label, the status — untouched. This is the atomic
// replace G017 requires of a rotation: the new token lands as one write, and a
// concurrent console edit cannot see a half-updated credential.
//
// The write is retried once before giving up. A rotation that was already paid
// for with an upstream round trip must not be thrown away by one transient
// SQLite failure (a busy database, a locked WAL): the exchange succeeded, and
// dropping its result means the next request pays again. The retry sits inside
// the same channelSaveMu critical section, so it stays atomic — either the new
// token lands or the old row is exactly as it was, never a half-written one,
// and the generation does not move either way. If both attempts fail the error
// travels to the caller, the stored token stays the previous one, and the next
// request re-exchanges.
func (s *Store) ReplaceOAuthTokens(id int64, tokens OAuthTokens) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return err
	}
	var target *OAuthCredential
	for i := range c.Credentials {
		if c.Credentials[i].OAuth != nil {
			target = c.Credentials[i].OAuth
			break
		}
	}
	if target == nil {
		if c.OAuth == nil {
			return errors.New("channel carries no oauth credential")
		}
		target = c.OAuth
	}
	target.AccessToken = tokens.AccessToken
	target.ExpiresAt = tokens.ExpiresAt
	target.UpdatedAt = time.Now().UTC()
	if tokens.RefreshToken != "" {
		target.RefreshToken = tokens.RefreshToken
	}
	if tokens.IDToken != "" {
		target.IDToken = tokens.IDToken
	}
	if tokens.Scopes != "" {
		target.Scopes = tokens.Scopes
	}
	c.normalize()
	err = s.persistRow(&c)
	if err == nil {
		return nil
	}
	slog.Error("oauth token rotation write failed, retrying once", "channel_id", id, "error", err)
	if retryErr := s.persistRow(&c); retryErr != nil {
		return fmt.Errorf("rotated token could not be stored (old token kept): %w", retryErr)
	}
	return nil
}

// ReplaceOAuthTokensForCredential updates the OAuth tokens for a specific credential in the channel pool.
func (s *Store) ReplaceOAuthTokensForCredential(id int64, credID string, tokens OAuthTokens) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return err
	}
	found := false
	for i := range c.Credentials {
		if c.Credentials[i].ID == credID && c.Credentials[i].OAuth != nil {
			found = true
			c.Credentials[i].OAuth.AccessToken = tokens.AccessToken
			c.Credentials[i].OAuth.ExpiresAt = tokens.ExpiresAt
			c.Credentials[i].OAuth.UpdatedAt = time.Now().UTC()
			if tokens.RefreshToken != "" {
				c.Credentials[i].OAuth.RefreshToken = tokens.RefreshToken
			}
			if tokens.IDToken != "" {
				c.Credentials[i].OAuth.IDToken = tokens.IDToken
			}
			if tokens.Scopes != "" {
				c.Credentials[i].OAuth.Scopes = tokens.Scopes
			}
			break
		}
	}
	if !found {
		return errors.New("credential not found or carries no oauth credential")
	}
	c.normalize()
	err = s.persistRow(&c)
	if err == nil {
		return nil
	}
	slog.Error("oauth token rotation write failed, retrying once", "channel_id", id, "error", err)
	if retryErr := s.persistRow(&c); retryErr != nil {
		return fmt.Errorf("rotated token could not be stored (old token kept): %w", retryErr)
	}
	return nil
}

// AuthorizationResult is a completed re-authorization: everything the channel
// needs to become usable again, in one write. It is deliberately not
// OAuthTokens — a rotation refreshes an access token, while a re-authorization
// is a new credential from a new consent, and the difference shows in what
// happens to the status.
type AuthorizationResult struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	Scopes       string
}

// ReplaceChatGPTAuthorization writes a re-authorization's result and clears the
// failure marks, in one statement. The status reset is the point: a channel
// marked reauth_required stays unusable until something writes "active" back,
// and a re-authorization that only swapped the tokens would leave the owner
// looking at a credential the gateway still refuses. The identity fields — the
// registered client, the host id, the subject — are not touched: those name the
// account, and a re-authorization of the same account must not look like a new
// one to a session pin.
func (s *Store) ReplaceChatGPTAuthorization(id int64, result AuthorizationResult) error {
	return s.replaceAuthorization(id, CredentialChatGPTOAuth, result)
}

// ReplaceCodexAuthorization installs completed consent and clears failure
// marks. The caller owns the service lifecycle and checks the current identity.
func (s *Store) ReplaceCodexAuthorization(id int64, result AuthorizationResult) error {
	return s.replaceAuthorization(id, CredentialCodexOAuth, result)
}

func (s *Store) replaceAuthorization(id int64, credentialType string, result AuthorizationResult) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return err
	}
	if c.CredentialType != credentialType || c.OAuth == nil {
		return errors.New("channel carries no oauth credential")
	}
	c.OAuth.AccessToken = result.AccessToken
	c.OAuth.RefreshToken = result.RefreshToken
	c.OAuth.IDToken = result.IDToken
	c.OAuth.ExpiresAt = result.ExpiresAt
	if result.Scopes != "" {
		c.OAuth.Scopes = result.Scopes
	}
	c.OAuth.Status = OAuthStatusActive
	c.OAuth.StatusReason = ""
	c.OAuth.UpdatedAt = time.Now().UTC()
	if len(c.Credentials) > 0 && c.Credentials[0].OAuth != nil {
		c.Credentials[0].OAuth = c.OAuth
	}
	err = s.persistRow(&c)
	if err == nil {
		return nil
	}
	slog.Error("re-authorization write failed, retrying once", "channel_id", id, "error", err)
	if retryErr := s.persistRow(&c); retryErr != nil {
		return fmt.Errorf("the re-authorized credential could not be stored: %w", retryErr)
	}
	return nil
}

// ReplaceCopilotToken is the Copilot-shaped rotation: a bearer token and
// nothing else, because Copilot's exchange issues no long-lived material.
func (s *Store) ReplaceCopilotToken(id int64, token string, expiresAt time.Time) error {
	return s.ReplaceOAuthTokens(id, OAuthTokens{AccessToken: token, ExpiresAt: expiresAt})
}

// ReplaceCopilotTokenForCredential rotates the Copilot bearer token for a specific credential in the pool.
func (s *Store) ReplaceCopilotTokenForCredential(id int64, credID string, token string, expiresAt time.Time) error {
	return s.ReplaceOAuthTokensForCredential(id, credID, OAuthTokens{AccessToken: token, ExpiresAt: expiresAt})
}

// metaOAuthHostID is the meta row holding this host's ext_agent_host_id.
const metaOAuthHostID = "oauth_host_id"

// HostID returns this host's ext_agent_host_id, generating and persisting one
// the first time it is asked for.
//
// The value is a urn:uuid that is generated once and reused forever: the SIWC
// reference describes it as a per-host identifier that is stable across
// sessions, and a host that changed it would look like a new device to the
// issuer on every restart — re-prompting consent for an app that was already
// authorized. It lives in the private meta table rather than Settings because
// the console round-trips Settings whole: anything the UI does not know about
// would be zeroed by the next settings save.
//
// The reference is explicit that it is not a credential and that no private key
// possession is proven — a uuid is enough, and a jwk-thumbprint or did:key
// would claim more than we do.
func (s *Store) HostID() (string, error) {
	var existing string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", metaOAuthHostID).Scan(&existing)
	if err == nil && existing != "" {
		return existing, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id, err := newHostID()
	if err != nil {
		return "", err
	}
	// DO NOTHING, then read back: two concurrent first calls would otherwise
	// both generate and both consider their own value the host's, and a host
	// that changed identity on every boot would look like a new device to the
	// issuer. The loser of the insert adopts the winner's row.
	if _, err = s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING", metaOAuthHostID, id); err != nil {
		return "", err
	}
	var stored string
	if err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", metaOAuthHostID).Scan(&stored); err != nil {
		return "", err
	}
	return stored, nil
}

// newHostID mints a urn:uuid from crypto/rand. The uuid format is hand-rolled
// rather than pulled from a dependency: the only consumer is an identifier the
// issuer treats as opaque.
func newHostID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ChannelByChatGPTIdentity finds the channel that holds this SIWC account: the
// same registered client, on this host, for the same subject. It exists for the
// same reason ChannelByGitHubToken does — one account is one channel, because
// two channels sharing a refresh token race each other's rotations and the
// loser's requests die on a token the other rotation invalidated.
//
// All three fields are compared in plaintext: none of them is a secret. The
// client id and host id are identifiers the operator can read, and the subject
// is an opaque account id, so hashing them would buy nothing.
func (s *Store) ChannelByChatGPTIdentity(clientID, hostID, subject string) (Channel, error) {
	if clientID == "" || hostID == "" || subject == "" {
		return Channel{}, ErrNotFound
	}
	rows, err := s.ListChannels()
	if err != nil {
		return Channel{}, err
	}
	for _, c := range rows {
		if c.CredentialType != CredentialChatGPTOAuth || c.OAuth == nil {
			continue
		}
		if c.OAuth.ClientID == clientID && c.OAuth.HostID == hostID && c.OAuth.Subject == subject {
			return c, nil
		}
	}
	return Channel{}, ErrNotFound
}

// ChannelByCodexAccount finds the channel that holds this Codex account. The
// identity is the account id from the id_token; when the token carries none,
// the hash of the refresh token stands in, because an account that cannot be
// named still must not be authorized twice — two channels sharing a refresh
// token race each other's rotations and the loser dies on a token the other
// rotation invalidated (good-finds G017, across channels).
func (s *Store) ChannelByCodexAccount(account, refreshToken string) (Channel, error) {
	if account == "" && refreshToken == "" {
		return Channel{}, ErrNotFound
	}
	want := tokenHash(refreshToken)
	rows, err := s.ListChannels()
	if err != nil {
		return Channel{}, err
	}
	for _, c := range rows {
		if c.CredentialType != CredentialCodexOAuth || c.OAuth == nil {
			continue
		}
		if account != "" {
			if c.OAuth.Account == account {
				return c, nil
			}
			continue
		}
		if c.OAuth.RefreshToken != "" && subtle.ConstantTimeCompare(tokenHash(c.OAuth.RefreshToken), want) == 1 {
			return c, nil
		}
	}
	return Channel{}, ErrNotFound
}

// SetOAuthStatus records a failure the upstream reported about the credential
// itself. disable is written in the same statement so a channel marked
// unsubscribed is never left selectable between the two writes. Neither the
// status nor the flag touches the continuation generation: the credential's
// identity did not move.
func (s *Store) SetOAuthStatus(id int64, status, reason string, disable bool) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return err
	}
	if c.OAuth == nil {
		return errors.New("channel carries no oauth credential")
	}
	c.OAuth.Status = status
	c.OAuth.StatusReason = reason
	c.OAuth.UpdatedAt = time.Now().UTC()
	for i := range c.Credentials {
		if c.Credentials[i].OAuth != nil {
			c.Credentials[i].OAuth.Status = status
			c.Credentials[i].OAuth.StatusReason = reason
			c.Credentials[i].OAuth.UpdatedAt = time.Now().UTC()
		}
	}
	if disable {
		c.Enabled = false
	}
	return s.persistRow(&c)
}

// Channel reads one stored channel the way ListChannels returns it: normalized,
// with its credential decrypted.
func (s *Store) Channel(id int64) (Channel, error) {
	return s.channel(id)
}

// ChannelByGitHubToken finds the channel that was authorized with this GitHub
// token, or ErrNotFound when none was. It exists for the identity rule: one
// GitHub account is one channel, because two channels sharing a token refresh
// the same rotation against each other and the loser's requests die on a token
// the other rotation already invalidated (good-finds G017, across channels).
//
// Both sides are hashed before comparison, so the secret never sits in a local
// that an error path or a log line could quote, and the compare is
// constant-time.
func (s *Store) ChannelByGitHubToken(githubToken string) (Channel, error) {
	if githubToken == "" {
		return Channel{}, ErrNotFound
	}
	want := tokenHash(githubToken)
	rows, err := s.ListChannels()
	if err != nil {
		return Channel{}, err
	}
	for _, c := range rows {
		if c.CredentialType != CredentialCopilotOAuth || c.OAuth == nil || c.OAuth.GitHubToken == "" {
			continue
		}
		if subtle.ConstantTimeCompare(tokenHash(c.OAuth.GitHubToken), want) == 1 {
			return c, nil
		}
	}
	return Channel{}, ErrNotFound
}

// writeRow is the single statement that lands one channel row: it seals the
// credential fields exactly the way SaveChannel does and updates the stored
// JSON. Callers hold channelSaveMu. Store.persistRow points here in production;
// tests substitute that field, or store.FailPersistWrites from another package,
// to inject a write failure on demand (see ReplaceOAuthTokens' retry).
// persistFailures is how many upcoming row writes the test seam fails.
var persistFailures atomic.Int64

// FailPersistWrites makes the next n channel-row writes fail with an injected
// error. It exists so packages outside store can exercise a rotation's
// write-failure path; production never calls it, and it fails exactly n writes.
func FailPersistWrites(n int) { persistFailures.Store(int64(n)) }

func (s *Store) writeRow(c *Channel) error {
	// A test seam for callers outside this package: the SIWC rotation's
	// write-failure path needs to be drivable from its own package, and a
	// counter consulted here is the narrowest way to do it. Nothing in the
	// process sets it, and it fails exactly n writes and no more.
	if left := persistFailures.Load(); left > 0 && persistFailures.CompareAndSwap(left, left-1) {
		return errors.New("injected write failure")
	}
	row, err := s.sealChannel(*c)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if c.ID == 0 {
		return errors.New("persist requires an existing channel id")
	}
	res, err := s.db.Exec("UPDATE channels SET data=? WHERE id=?", string(raw), c.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// sealChannel returns a copy of c whose credential fields are encrypted, ready
// to be marshalled into a row. The caller's copy is left in plaintext.
func (s *Store) sealChannel(c Channel) (Channel, error) {
	row := c
	// Clean Break: Credentials is the single persistent store for secrets.
	// Legacy top-level fields are not written to SQLite.
	row.APIKey = ""
	row.OAuth = nil
	row.Protocol = ""
	if len(row.Credentials) > 0 {
		sealedCreds := make([]CredentialItem, len(row.Credentials))
		for i, cred := range row.Credentials {
			sealedCreds[i] = cred
			sealedCreds[i].HasKey = false
			sealedCreds[i].HasOAuth = false
			sealedCreds[i].MaskedKey = ""
			sealedCreds[i].OAuthInfo = nil

			if cred.APIKey != "" {
				sKey, err := s.seal(cred.APIKey)
				if err != nil {
					return Channel{}, err
				}
				sealedCreds[i].APIKey = sKey
			}
			if cred.OAuth != nil {
				sealedO, err := sealOAuthFields(s, *cred.OAuth)
				if err != nil {
					return Channel{}, err
				}
				sealedCreds[i].OAuth = &sealedO
			}
		}
		row.Credentials = sealedCreds
	}
	return row, nil
}

// openChannel finishes reading one stored row: it normalizes the legacy shapes
// and decrypts the credential fields. Every read path goes through it, so a
// sealed field cannot exist on one path only.
func (s *Store) openChannel(id int64, raw string) (Channel, error) {
	var c Channel
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Channel{}, err
	}
	c.ID = id
	if c.APIKey != "" {
		key, err := s.open(c.APIKey)
		if err != nil {
			return Channel{}, err
		}
		c.APIKey = key
	}
	if c.OAuth != nil {
		cred, err := openOAuthFields(s, *c.OAuth)
		if err != nil {
			return Channel{}, err
		}
		c.OAuth = &cred
	}
	for i := range c.Credentials {
		if c.Credentials[i].APIKey != "" {
			key, err := s.open(c.Credentials[i].APIKey)
			if err != nil {
				return Channel{}, err
			}
			c.Credentials[i].APIKey = key
		}
		if c.Credentials[i].OAuth != nil {
			cred, err := openOAuthFields(s, *c.Credentials[i].OAuth)
			if err != nil {
				return Channel{}, err
			}
			c.Credentials[i].OAuth = &cred
		}
	}
	c.normalize()
	return c, nil
}
