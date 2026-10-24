package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
	"torouter/internal/accessguard"

	"torouter/internal/store"
)

// refreshSkew is how long before expiry an access token is exchanged for a new
// one. The ref names refresh_ahead_secs rather than a value; 60s covers the
// exchange round trip with room to spare.
const refreshSkew = 60 * time.Second

// Service owns the Codex credential lifecycle for one process.
type Service struct {
	Store     *store.Store
	Client    *http.Client
	Endpoints Endpoints

	mu      sync.Mutex
	refresh map[int64]*sync.Mutex
}

func NewService(st *store.Store, client *http.Client, eps Endpoints) *Service {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{Store: st, Client: client, Endpoints: eps, refresh: map[int64]*sync.Mutex{}}
}

// Bearer returns an access token that will still be valid for at least
// refreshSkew, rotating it when it is not.
//
// The lock and the double-check under it are the same G017 discipline the other
// tracks use: the ref records TOKEN_REFRESH_LOCKS — one mutex per account — and
// the race recovery that compares the stored refresh token against the one the
// caller holds. A rotation is single-use, so two concurrent refreshes destroy
// each other, and the failure is permanent rather than transient.
func (svc *Service) Bearer(ctx context.Context, c store.Channel) (string, error) {
	if err := usable(c); err != nil {
		return "", err
	}
	if c.OAuth.RefreshToken == "" && (c.OAuth.ExpiresAt.IsZero() || time.Now().Before(c.OAuth.ExpiresAt)) && c.OAuth.AccessToken != "" {
		return c.OAuth.AccessToken, nil
	}
	if time.Until(c.OAuth.ExpiresAt) > refreshSkew {
		return c.OAuth.AccessToken, nil
	}
	lock := svc.lockFor(c.ID)
	lock.Lock()
	defer lock.Unlock()
	return svc.bearerLocked(ctx, c, svc.Client)
}

// bearerLocked shares rotation with read-only account operations. The caller
// must hold lockFor(c.ID), including until any resulting snapshot is captured.
func (svc *Service) bearerLocked(ctx context.Context, c store.Channel, client *http.Client) (string, error) {
	return svc.bearerCredentialLocked(ctx, c, client, "")
}

// An explicit credential ID never falls back to another member of the pool.
// The empty ID retains the inference/display caller's identity-based lookup.
func (svc *Service) bearerCredentialLocked(ctx context.Context, c store.Channel, client *http.Client, credID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fresh, err := svc.Store.Channel(c.ID)
	if err != nil {
		return "", err
	}
	if c.OAuth == nil || fresh.CredentialType != store.CredentialCodexOAuth {
		return "", errors.New("Codex credential is no longer available")
	}
	// Inference may hold the selected snapshot from just before a usage read
	// rotated its refresh token. Establish its pool ID from that snapshot's
	// nonempty token and complete identity, then match the live member by ID
	// and stable identity. A Subject alone must never select another pool ID.
	expectedID := credID
	if expectedID == "" && c.OAuth.RefreshToken != "" {
		for _, selected := range c.Credentials {
			if selected.OAuth != nil && selected.OAuth.RefreshToken == c.OAuth.RefreshToken && sameUsageIdentity(selected.OAuth, c.OAuth) {
				expectedID = selected.ID
				break
			}
		}
	}
	var targetOAuth *store.OAuthCredential
	targetCredID := ""
	for _, cred := range fresh.Credentials {
		matches := cred.OAuth != nil && ((c.OAuth.Account != "" && cred.OAuth.Account == c.OAuth.Account) || (c.OAuth.RefreshToken != "" && cred.OAuth.RefreshToken == c.OAuth.RefreshToken) || (c.OAuth.RefreshToken == "" && c.OAuth.AccessToken != "" && cred.OAuth.AccessToken == c.OAuth.AccessToken))
		if expectedID != "" {
			matches = cred.ID == expectedID && sameUsageIdentity(cred.OAuth, c.OAuth)
		}
		if matches {
			targetOAuth = cred.OAuth
			targetCredID = cred.ID
			break
		}
	}
	if targetOAuth == nil {
		return "", errors.New("Codex credential is no longer available")
	}
	selected := fresh
	selected.OAuth = targetOAuth
	if err = usable(selected); err != nil {
		return "", err
	}
	if time.Until(targetOAuth.ExpiresAt) > refreshSkew {
		return targetOAuth.AccessToken, nil
	}
	if targetOAuth.RefreshToken == "" {
		if targetOAuth.AccessToken != "" && (targetOAuth.ExpiresAt.IsZero() || time.Now().Before(targetOAuth.ExpiresAt)) {
			return targetOAuth.AccessToken, nil
		}
		_ = svc.Store.SetOAuthCredentialStatus(c.ID, targetCredID, store.OAuthStatusReauthRequired, "访问令牌已到期且无刷新凭证，请重新授权", false)
		return "", ErrReauthRequired
	}
	tokens, err := RefreshToken(ctx, client, svc.Endpoints, targetOAuth.RefreshToken)
	if err != nil {
		if credID == "" {
			svc.markFailure(c.ID, targetCredID, err)
		}
		return "", err
	}
	// Preserve a successful rotation even when management access was revoked
	// during its response: this is already-issued material, not a new action.
	// The optional guard still blocks later re-fetch/network requests.
	// The exchange just invalidated the refresh token it used (rotation is
	// single-use). From here on the only credential that can produce another
	// rotation is the one in tokens, so a failed write is retried with it: the
	// store still holds the previous token, and that token is already dead — the
	// next attempt would answer invalid_grant and the account would need a full
	// re-authorization. Re-fetching with the live one is the difference between
	// one slow request and a dead credential (the SIWC track's discipline).
	storeTokens := store.OAuthTokens{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		IDToken:      tokens.IDToken,
		ExpiresAt:    ExpiryOf(tokens),
	}
	if targetCredID != "" {
		err = svc.Store.ReplaceOAuthTokensForCredential(c.ID, targetCredID, storeTokens)
	} else {
		err = svc.Store.ReplaceOAuthTokens(c.ID, storeTokens)
	}
	if err != nil {
		retried, retryErr := RefreshToken(ctx, client, svc.Endpoints, tokens.RefreshToken)
		if retryErr != nil {
			if credID == "" {
				svc.markFailure(c.ID, targetCredID, retryErr)
			}
			return "", fmt.Errorf("the rotated credential could not be stored and could not be re-fetched; re-authorize this account: %w", retryErr)
		}
		retriedTokens := store.OAuthTokens{
			AccessToken:  retried.AccessToken,
			RefreshToken: retried.RefreshToken,
			IDToken:      retried.IDToken,
			ExpiresAt:    ExpiryOf(retried),
		}
		if targetCredID != "" {
			err = svc.Store.ReplaceOAuthTokensForCredential(c.ID, targetCredID, retriedTokens)
		} else {
			err = svc.Store.ReplaceOAuthTokens(c.ID, retriedTokens)
		}
		if err != nil {
			if credID == "" {
				svc.markFailure(c.ID, targetCredID, err)
			}
			return "", err
		}
		return retried.AccessToken, nil
	}
	return tokens.AccessToken, nil
}

func usable(c store.Channel) error {
	if c.CredentialType != store.CredentialCodexOAuth || c.OAuth == nil {
		return fmt.Errorf("channel %d carries no codex oauth credential", c.ID)
	}
	switch c.OAuth.Status {
	case store.OAuthStatusReauthRequired:
		return fmt.Errorf("channel %d: %s (run the Codex authorization again to replace the credential)", c.ID, c.OAuth.StatusReason)
	case "", store.OAuthStatusActive:
		return nil
	default:
		return fmt.Errorf("channel %d: oauth credential has unknown status %q", c.ID, c.OAuth.Status)
	}
}

// markFailure records a credential the issuer rejected. The enabled flag is left
// alone: replacing the credential is the operator's decision, and the status
// already stops the retries.
//
// Known boundary: this hears only from the refresh path. A 401 from the backend
// on an inference request does not feed back here — the gateway treats it as the
// upstream's own answer and passes it through — so a credential the issuer
// rejected for inference alone is not marked from that signal. The refresh is
// the loop that exists, and it runs at least once an hour per channel.
func (svc *Service) markFailure(id int64, credentialID string, err error) {
	if errors.Is(err, ErrReauthRequired) {
		_ = svc.Store.SetOAuthCredentialStatus(id, credentialID, store.OAuthStatusReauthRequired,
			"The Codex issuer rejected the refresh token (invalid_grant or a rotation failure). Run the Codex authorization again to replace the credential.", false)
	}
}

func (svc *Service) lockFor(id int64) *sync.Mutex { return svc.Store.CredentialLifecycleMutex(id) }

// ReauthorizeChannel installs completed consent under the same lifecycle lock
// as refresh and disconnect. Re-reading after acquiring it keeps an old
// refresh's success or failure from undoing the new authorization.
func (svc *Service) ReauthorizeChannel(id int64, in LoginInput) error {
	return svc.ReauthorizeChannelContext(context.Background(), id, in)
}

func (svc *Service) ReauthorizeChannelContext(ctx context.Context, id int64, in LoginInput) error {
	lock := svc.lockFor(id)
	lock.Lock()
	defer lock.Unlock()
	c, err := svc.Store.Channel(id)
	if err != nil {
		return err
	}
	if c.CredentialType != store.CredentialCodexOAuth || c.OAuth == nil {
		return errors.New("identity_mismatch: the channel no longer holds a Codex account")
	}
	if in.Tokens.AccessToken == "" || in.Tokens.RefreshToken == "" {
		return errors.New("the authorization returned no renewable credential")
	}
	clientID := in.Tokens.ClientID
	if clientID == "" {
		clientID = ClientIDValue()
	}
	if clientID != c.OAuth.ClientID || in.Account != c.OAuth.Account ||
		(in.Account == "" && in.Tokens.RefreshToken != c.OAuth.RefreshToken) {
		return errors.New("identity_mismatch: this authorization does not establish the channel's current account")
	}
	if err := accessguard.Check(ctx); err != nil {
		return err
	}
	return svc.Store.ReplaceCodexAuthorization(id, store.AuthorizationResult{
		AccessToken: in.Tokens.AccessToken, RefreshToken: in.Tokens.RefreshToken,
		IDToken: in.Tokens.IDToken, ExpiresAt: ExpiryOf(in.Tokens), Scopes: in.Tokens.Scope,
	})
}

// DisconnectChannel has no remote revoke on this track. The local delete still
// participates in the lifecycle so an in-flight operation cannot write to a
// channel the operator has removed.
func (svc *Service) DisconnectChannel(id int64) error { return svc.DisconnectChannelAtVersion(id, "") }

func (svc *Service) DisconnectChannelAtVersion(id int64, expected string) error {
	lock := svc.lockFor(id)
	lock.Lock()
	defer lock.Unlock()
	return svc.Store.DeleteChannelAtVersion(id, expected)
}

// ExpiryOf is when a token response's access token dies. A response that does
// not say is treated as an hour: inference, not documentation — the ref does not
// state this issuer's access-token lifetime, and "already expired" would rotate
// on every single request, which is the more expensive of the two mistakes.
func ExpiryOf(tokens TokenResponse) time.Time {
	if tokens.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).UTC()
	}
	return time.Now().Add(time.Hour).UTC()
}

// LoginInput is one completed authorization: what the flow produced. Either the
// browser path's code or the device path's server-minted pair, never both.
type LoginInput struct {
	Code                string
	State               string
	CallbackError       string
	CallbackDescription string
	DeviceCode          string
	DeviceVerifier      string
	Tokens              TokenResponse
	Claims              IDClaims
	Account             string // the account id the channel is pinned to
	APIBase             string
	Models              []string
	Label               string
	Degraded            []string
	AlreadyID           int64 // non-zero when the account already had a channel
}

// ChannelFor assembles the channel row for an authorization, without saving it.
// The caller decides between this and rotating an existing channel.
func ChannelFor(in LoginInput) store.Channel {
	label := in.Label
	if label == "" {
		label = "codex"
	}
	base := in.APIBase
	if base == "" {
		base = DefaultEndpoints().Backend
	}
	// The client id is a constant on this track; the response echoes it only
	// sometimes, so the constant is the default.
	clientID := in.Tokens.ClientID
	if clientID == "" {
		clientID = ClientIDValue()
	}
	return store.Channel{
		Name:           "Codex backend",
		CredentialType: store.CredentialCodexOAuth,
		Enabled:        true,
		Models:         in.Models,
		Endpoints:      []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: base}},
		OAuth: &store.OAuthCredential{
			Provider:     store.OAuthProviderCodex,
			RefreshToken: in.Tokens.RefreshToken,
			IDToken:      in.Tokens.IDToken,
			ClientID:     clientID,
			Subject:      in.Claims.Subject,
			Account:      in.Account,
			Scopes:       in.Tokens.Scope,
			AccessToken:  in.Tokens.AccessToken,
			ExpiresAt:    ExpiryOf(in.Tokens),
			AccountLabel: label,
			APIBase:      base,
			Status:       store.OAuthStatusActive,
			UpdatedAt:    time.Now().UTC(),
		},
	}
}

// Usage reads the current credential and shares Bearer's rotation lifecycle.
// Keeping the lock through the bounded read orders the snapshot before any
// reauthorization or deletion, without adding a persistent quota cache.
func (svc *Service) Usage(ctx context.Context, c store.Channel) (Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	lock := svc.lockFor(c.ID)
	// Unlike inference's refresh wait, this auxiliary read has a total budget,
	// including time spent behind another lifecycle operation.
	if err := lockUsage(ctx, lock); err != nil {
		return Usage{}, err
	}
	defer lock.Unlock()
	return svc.usageLocked(ctx, c.ID)
}

func (svc *Service) usageLocked(ctx context.Context, id int64) (Usage, error) {
	c, err := svc.Store.Channel(id)
	if err != nil {
		return Usage{}, errors.New("Codex account is no longer available")
	}
	// A usage-triggered refresh must not forward the refresh-token form through
	// a redirect either. Inference/auth callers retain their existing client.
	token, err := svc.bearerLocked(ctx, c, usageHTTPClient(svc.Client))
	if err != nil {
		// The refresh path keeps its existing status semantics, but an issuer's
		// raw error body must not leak into this auxiliary display endpoint.
		if ctx.Err() != nil {
			return Usage{}, ctx.Err()
		}
		return Usage{}, errors.New("could not obtain a current Codex credential; check account authorization")
	}
	current, err := svc.Store.Channel(c.ID)
	if err != nil || current.OAuth == nil {
		return Usage{}, errors.New("Codex account is no longer available")
	}
	account := AccountHeaderID(token, current.OAuth.IDToken, current.OAuth.Account, current.OAuth.Subject)
	return FetchUsage(ctx, svc.Client, svc.Endpoints, token, account)
}

func lockUsage(ctx context.Context, lock *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lock.TryLock() {
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if lock.TryLock() {
				return nil
			}
		}
	}
}
