package siwc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"torouter/internal/accessguard"

	"torouter/internal/store"
)

// refreshSkew is how long before expiry an access token is exchanged for a new
// one. The access token lives an hour, so a minute of margin covers the
// exchange round trip with room to spare and keeps a long-lived process far
// away from the expiry boundary.
const refreshSkew = 60 * time.Second

// Service owns the SIWC credential lifecycle for one process: it hands the
// gateway fresh access tokens, it authorizes new accounts for the console, and
// it records what the issuer says about a credential.
type Service struct {
	Store     *store.Store
	Client    *http.Client
	Endpoints Endpoints

	jwks      *jwksCache
	mu        sync.Mutex
	lifecycle map[int64]*sync.Mutex
}

func NewService(st *store.Store, client *http.Client, eps Endpoints) *Service {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{Store: st, Client: client, Endpoints: eps, jwks: newJWKSCache(eps.JWKS, client), lifecycle: map[int64]*sync.Mutex{}}
}

// lifecycleLock returns the lock that serializes every write to one channel's
// credential. It covers more than the refresh path, because a rotating
// credential has several writers with different lifetimes:
//
//   - the refresh a request triggers (Bearer),
//   - a re-authorization the console completes (ReplaceChatGPTAuthorization),
//   - the revoke and local delete a channel removal performs (DisconnectChannel).
//
// Without one lock across all three, the operations interleave in ways the user
// cannot see and the store cannot detect: a delete that revokes the refresh
// token it read before a concurrent refresh rotated it leaves a live credential
// the issuer still honours, and a rotation that lands after a re-authorization
// overwrites the credential the owner just authorized. Per-channel keeps
// unrelated channels concurrent; the critical section is the store write plus
// the network call it depends on, which is what has to be atomic with it.
func (svc *Service) lifecycleLock(id int64) *sync.Mutex {
	return svc.Store.CredentialLifecycleMutex(id)
}

// Bearer returns an access token that will still be valid for at least
// refreshSkew, rotating it when it is not.
//
// The lock and the double-check under it are the whole point (good-finds G017,
// which the protocol ref makes concrete with refresh_token_reused /
// refresh_token_invalidated): a rotation is single-use, two concurrent
// refreshes for one account destroy each other, and the failure is permanent
// rather than transient. The lock is per channel; the stored row — not the
// caller's snapshot — decides whether a rotation is still needed, because the
// snapshot may predate one that finished while this caller waited.
func (svc *Service) Bearer(ctx context.Context, c store.Channel) (string, error) {
	if err := usable(c); err != nil {
		return "", err
	}
	if time.Until(c.OAuth.ExpiresAt) > refreshSkew {
		return c.OAuth.AccessToken, nil
	}
	lock := svc.lifecycleLock(c.ID)
	lock.Lock()
	defer lock.Unlock()
	fresh, err := svc.Store.Channel(c.ID)
	if err != nil {
		return "", err
	}
	var targetOAuth *store.OAuthCredential
	targetCredID := ""
	for _, cred := range fresh.Credentials {
		if cred.OAuth != nil && (cred.OAuth.ClientID == c.OAuth.ClientID && cred.OAuth.HostID == c.OAuth.HostID && cred.OAuth.Subject == c.OAuth.Subject) {
			targetOAuth = cred.OAuth
			targetCredID = cred.ID
			break
		}
	}
	if targetOAuth == nil {
		return "", store.ErrNativeOAuthIdentity
	}
	selected := fresh
	selected.OAuth = targetOAuth
	if err = usable(selected); err != nil {
		return "", err
	}
	if time.Until(targetOAuth.ExpiresAt) > refreshSkew {
		return targetOAuth.AccessToken, nil
	}
	tokens, err := RefreshToken(ctx, svc.Client, svc.Endpoints, targetOAuth.RefreshToken, targetOAuth.ClientID)
	if err != nil {
		svc.markFailure(c.ID, targetCredID, err)
		return "", err
	}
	if err = svc.validateRefreshResult(c.ID, targetCredID, tokens, targetOAuth); err != nil {
		return "", err
	}
	// Preserve the already-issued replacement after a successful single-use
	// rotation, even if management access was revoked during that response.
	// The guard continues to block later network sends/re-fetches.
	// The exchange just invalidated the refresh token it used (rotation is
	// single-use). From here on the only credential that can produce another
	// rotation is the one in tokens, so a failed write is retried with it:
	// the store still holds the previous token, and that token is already dead —
	// the next attempt would answer invalid_grant and the account would need a
	// full re-authorization. Re-fetching with the live token is the difference
	// between one slow request and a dead credential.
	storeTokens := store.OAuthTokens{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		IDToken:      tokens.IDToken,
		ExpiresAt:    ExpiryOf(tokens),
		Scopes:       tokens.Scope,
	}
	if targetCredID != "" {
		err = svc.Store.ReplaceOAuthTokensForCredential(c.ID, targetCredID, storeTokens)
	} else {
		err = svc.Store.ReplaceOAuthTokens(c.ID, storeTokens)
	}
	if err != nil {
		retried, retryErr := RefreshToken(ctx, svc.Client, svc.Endpoints, tokens.RefreshToken, targetOAuth.ClientID)
		if retryErr != nil {
			svc.markFailure(c.ID, targetCredID, retryErr)
			return "", fmt.Errorf("the rotated credential could not be stored and could not be re-fetched; re-authorize this account: %w", retryErr)
		}
		// The re-fetch is a refresh too, so it answers to the same checks: a
		// response that cannot produce the next rotation is a dead credential
		// wherever it came from.
		if err = svc.validateRefreshResult(c.ID, targetCredID, retried, targetOAuth); err != nil {
			return "", err
		}
		retriedTokens := store.OAuthTokens{
			AccessToken:  retried.AccessToken,
			RefreshToken: retried.RefreshToken,
			IDToken:      retried.IDToken,
			ExpiresAt:    ExpiryOf(retried),
			Scopes:       retried.Scope,
		}
		if targetCredID != "" {
			err = svc.Store.ReplaceOAuthTokensForCredential(c.ID, targetCredID, retriedTokens)
		} else {
			err = svc.Store.ReplaceOAuthTokens(c.ID, retriedTokens)
		}
		if err != nil {
			svc.markFailure(c.ID, targetCredID, err)
			return "", err
		}
		return retried.AccessToken, nil
	}
	return tokens.AccessToken, nil
}

// scopesOf prefers the response's own scope field and falls back to the stored
// one: a refresh that does not echo the scope set has not revoked it.
func scopesOf(tokens TokenResponse, stored *store.OAuthCredential) string {
	if strings.TrimSpace(tokens.Scope) != "" {
		return tokens.Scope
	}
	if stored != nil {
		return stored.Scopes
	}
	return ""
}

// validateRefreshResult decides whether a refresh response is a credential the
// channel can keep using. Both rules are termination conditions rather than
// retries, and they apply wherever a refresh response comes from — the normal
// path and the re-fetch after a failed write alike, because a response that
// cannot produce the *next* rotation is a dead credential in either place.
func (svc *Service) validateRefreshResult(id int64, credentialID string, tokens TokenResponse, current *store.OAuthCredential) error {
	if tokens.RefreshToken == "" {
		// A successful refresh always returns a new refresh token (the rotation
		// is the point), so a response without one is a credential that is
		// already gone: the token it used was consumed, and keeping it would
		// only guarantee an invalid_grant on the next attempt.
		_ = svc.Store.SetOAuthCredentialStatus(id, credentialID, store.OAuthStatusReauthRequired,
			"The refresh succeeded but returned no replacement refresh token; the credential can no longer be rotated. Run the ChatGPT authorization again.", false)
		return ErrReauthRequired
	}
	if !HasDirectUse(scopesOf(tokens, current)) {
		// The consent that once granted ChatGPT-plan usage no longer does: the
		// credential can sign in but cannot infer, so it is marked rather than
		// left to fail on the wire (protocol ref, 资格 — errors-and-recovery).
		reason := "consent_missing_scope: the granted scope set no longer includes " + ScopeDirect
		_ = svc.Store.SetOAuthCredentialStatus(id, credentialID, store.OAuthStatusReauthRequired, reason, false)
		return ErrConsentMissingScope
	}
	return nil
}

// usable reports whether the credential may be exchanged at all, naming the
// recorded failure when it may not.
func usable(c store.Channel) error {
	if c.CredentialType != store.CredentialChatGPTOAuth || c.OAuth == nil {
		return fmt.Errorf("channel %d carries no chatgpt oauth credential", c.ID)
	}
	switch c.OAuth.Status {
	case store.OAuthStatusReauthRequired:
		return fmt.Errorf("channel %d: %s (run the ChatGPT authorization again to replace the credential)", c.ID, c.OAuth.StatusReason)
	case "", store.OAuthStatusActive:
		return nil
	default:
		return fmt.Errorf("channel %d: oauth credential has unknown status %q", c.ID, c.OAuth.Status)
	}
}

// markFailure turns a token-endpoint rejection into a stored status. The
// enabled flag is left alone on purpose: the credential is the operator's to
// replace, and the status already stops the retries.
func (svc *Service) markFailure(id int64, credentialID string, err error) {
	switch {
	case errors.Is(err, ErrReauthRequired):
		_ = svc.Store.SetOAuthCredentialStatus(id, credentialID, store.OAuthStatusReauthRequired,
			"OpenAI rejected the refresh token (invalid_grant, an expired token, or a rotation failure). Run the ChatGPT authorization again to replace the credential.", false)
	case errors.Is(err, ErrClientMisconfigured):
		// The channel is not left usable: a client the issuer does not recognize
		// fails every refresh, so the status says so without promising that a new
		// authorization will fix it.
		_ = svc.Store.SetOAuthCredentialStatus(id, credentialID, store.OAuthStatusReauthRequired,
			"OpenAI did not recognize this channel's client id; the client configuration is wrong. Delete the channel and authorize again to register a new client.", false)
	}
}

func (svc *Service) lockFor(id int64) *sync.Mutex { return svc.lifecycleLock(id) }

// LoginInput is one completed authorization: the callback the browser (or the
// user's paste) delivered, plus the material the flow issued.
type LoginInput struct {
	Detached      bool // validation only; selected-pool consent is committed by the caller
	Auth          AuthRequest
	Callback      CallbackResult
	CallbackID    string // client id the callback carried, if any
	CallbackScope string // scope set the callback carried, if any
	// ExpectedSubject binds a re-authorization to the account the channel
	// already holds; empty for a fresh "add account" flow.
	ExpectedSubject   string
	ExpectedChannelID int64 // bound reauthorization must not recreate a deleted channel
	Tokens            TokenResponse
	Degraded          []string
	AccountLabel      string
	APIBase           string
}

// Login is what the console needs to finish: the channel row to store and what
// to say about it. AlreadyLinked reports an account that already had a channel:
// the service rotated that channel's credential instead of returning a row to
// save, and ChannelID names it.
type Login struct {
	Channel       store.Channel
	AlreadyLinked bool
	ChannelID     int64
	Degraded      []string
}

// ExpiryOf is when a token response's access token dies. A response that does
// not say is treated as the documented one hour rather than "already expired",
// which would rotate on every request.
func ExpiryOf(tokens TokenResponse) time.Time {
	if tokens.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).UTC()
	}
	return time.Now().Add(time.Hour).UTC()
}

// CompleteLogin verifies the authorization and resolves it into a channel:
// verify the id_token, agree on the registered client id, check the consent and
// pull the model catalogue.
//
// The order matters. The id_token is verified before anything is stored,
// because its subject is the account identity a session pins to; the client id
// is agreed before the credential is written, because a registration that came
// back with a different id is a different app and not one to silently adopt.
func (svc *Service) CompleteLogin(ctx context.Context, in LoginInput) (Login, error) {
	// Which client this code belongs to is settled before anything is verified:
	// the id_token's audience is that client and the exchange authenticates as
	// it, so a wrong answer here poisons everything downstream.
	clientID, err := resolveIssuedClientID(in)
	if err != nil {
		return Login{}, err
	}
	claims, err := VerifyIDToken(ctx, svc.Client, svc.jwks, svc.Endpoints.Issuer, in.Tokens.IDToken, in.Auth.Nonce, clientID)
	if err != nil {
		return Login{}, fmt.Errorf("id_token verification failed: %w", err)
	}
	// A re-authorization is bound to the account that started it, and the check
	// runs before anything is created or rotated: the user can sign in as
	// anybody in the browser, and rotating channel A's credential with account
	// B's tokens would move a registered client to a different user with no
	// visible failure. A channel with no recorded subject cannot be bound, so an
	// empty expectation lets the reauthorization through — the alternative would
	// be a channel that can never be re-authorized.
	if in.ExpectedSubject != "" && claims.Subject != in.ExpectedSubject {
		return Login{}, fmt.Errorf("identity_mismatch: this authorization is for account %q but the channel holds %q", claims.Subject, in.ExpectedSubject)
	}
	hostID := in.Auth.HostID
	if hostID == "" {
		return Login{}, errors.New("the flow did not carry this host's ext_agent_host_id")
	}
	if in.Tokens.RefreshToken == "" {
		// The same invariant the refresh side enforces, from the other end: a
		// credential whose rotation cannot be renewed is not one the gateway can
		// keep serving. The scope set already asks for offline_access, so an
		// exchange that comes back without one is a consent that declined it, and
		// storing it would produce a channel that looks configured and dies in an
		// hour. Checked before anything is written.
		return Login{}, ErrRenewableSessionMissing
	}
	scopes, evidenced := grantedScopes(in)
	if !evidenced {
		// No evidence about the consent at all. Assuming granted == requested
		// would make a refused consent look like a working credential, so this
		// is a refusal with its own reason rather than a guess.
		return Login{}, ErrScopeUndetermined
	}
	if !HasDirectUse(scopes) {
		// Consent refused: the protocol ref's errors-and-recovery says the
		// token simply lacks the scope, and the fix is a full re-run. The
		// credential is not stored at all — a channel that cannot infer would
		// look configured.
		return Login{}, ErrConsentMissingScope
	}
	// One account is one channel. The identity is (client, host, subject) and
	// it is checked here, after the id_token has been verified, because the
	// subject only exists once it has: two channels sharing a refresh token
	// would race each other's rotations (G017 across channels). The existing
	// channel is rotated in place — the fresh tokens are already in hand — and
	// nothing new is created.
	var models []string
	if !in.Detached {
		var existing store.Channel
		if in.ExpectedChannelID != 0 {
			existing, err = svc.Store.Channel(in.ExpectedChannelID)
		} else {
			existing, err = svc.Store.ChannelByChatGPTIdentity(clientID, hostID, claims.Subject)
		}
		if err == nil {
			lock := svc.lifecycleLock(existing.ID)
			lock.Lock()
			defer lock.Unlock()
			// Re-read while owning the same lifecycle as refresh and disconnect;
			// the earlier lookup cannot establish the channel's current ownership.
			existing, err = svc.Store.Channel(existing.ID)
			if err != nil {
				return Login{}, err
			}
			if existing.CredentialType != store.CredentialChatGPTOAuth || existing.OAuth == nil ||
				existing.OAuth.ClientID != clientID || existing.OAuth.HostID != hostID ||
				existing.OAuth.Subject != claims.Subject {
				return Login{}, errors.New("identity_mismatch: the channel no longer holds this authorization's account")
			}
			// A re-authorization is not a rotation: it writes the new credential and
			// clears the failure marks, so a channel that was reauth_required becomes
			// usable again in the same statement (see ReplaceChatGPTAuthorization).
			if err := accessguard.Check(ctx); err != nil {
				return Login{}, err
			}
			if err := svc.Store.ReplaceChatGPTAuthorization(existing.ID, store.AuthorizationResult{
				AccessToken:  in.Tokens.AccessToken,
				RefreshToken: in.Tokens.RefreshToken,
				IDToken:      in.Tokens.IDToken,
				ExpiresAt:    ExpiryOf(in.Tokens),
				// The resolved set, not the response's own: what the login was
				// judged on is what gets stored, so a callback-only evidence case
				// does not verify one scope set and persist another.
				Scopes: scopes,
			}); err != nil {
				return Login{}, fmt.Errorf("the existing channel could not be updated: %w", err)
			}
			return Login{AlreadyLinked: true, ChannelID: existing.ID, Degraded: in.Degraded}, nil
		} else if in.ExpectedChannelID != 0 || !errors.Is(err, store.ErrNotFound) {
			return Login{}, err
		}
		models, err = DiscoverModels(ctx, svc.Client, svc.Endpoints.Models, in.Tokens.AccessToken)
		if err != nil {
			return Login{}, fmt.Errorf("model catalogue unavailable: %w", err)
		}
	}
	label := in.AccountLabel
	if label == "" {
		label = firstNonEmpty(claims.Email, claims.Name, claims.Subject)
	}
	apiBase := in.APIBase
	if apiBase == "" {
		apiBase = svc.Endpoints.Inference
	}
	cred := &store.OAuthCredential{
		Provider:     store.OAuthProviderChatGPT,
		RefreshToken: in.Tokens.RefreshToken,
		IDToken:      in.Tokens.IDToken,
		ClientID:     clientID,
		HostID:       hostID,
		Subject:      claims.Subject,
		Scopes:       scopes,
		AccessToken:  in.Tokens.AccessToken,
		ExpiresAt:    ExpiryOf(in.Tokens),
		AccountLabel: label,
		APIBase:      apiBase,
		Status:       store.OAuthStatusActive,
		UpdatedAt:    time.Now().UTC(),
	}
	channel := store.Channel{
		Name:           "ChatGPT plan",
		CredentialType: store.CredentialChatGPTOAuth,
		Enabled:        true,
		Models:         models,
		Endpoints:      []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: apiBase}},
		OAuth:          cred,
	}
	return Login{Channel: channel, Degraded: in.Degraded}, nil
}

// Account fetches the display label from the userinfo endpoint. It is
// decoration: a failure here must not cost the user their authorization, so the
// caller records the degradation and moves on.
func (svc *Service) Account(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.Endpoints.Userinfo, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := accessguard.Do(svc.Client, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("userinfo returned HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply); err != nil {
		return "", err
	}
	return firstNonEmpty(reply.Email, reply.Name), nil
}

// resolveIssuedClientID decides which client id the code exchange must
// authenticate as, and refuses the combinations that would silently adopt a
// different app registration.
//
// The three sources are consulted in the order the official flow gives them:
// the token response's own registration, the callback's, and the client the
// authorization was started as. The first two are evidence about the client this
// code belongs to; the third is the baseline. Any source that appears must agree
// with the baseline, because a code is only exchangeable by the client it was
// issued to.
//
// The one case where the baseline is not usable is the first authorization: it
// starts from dynamic_agent_client, which is the registration entry point and
// not a client an exchange may authenticate as. There the callback MUST carry
// the issued oaiapp_* id, and its absence is a refusal rather than a fallback.
func resolveIssuedClientID(in LoginInput) (string, error) {
	baseline := strings.TrimSpace(in.Auth.ClientID)
	reported := firstNonEmpty(strings.TrimSpace(in.Tokens.ClientID), strings.TrimSpace(in.CallbackID))
	if baseline == "" || baseline == DynamicAppID {
		if reported == "" {
			return "", errors.New("client_id_missing: the callback did not carry the issued oaiapp client id")
		}
		if !strings.HasPrefix(reported, "oaiapp_") {
			return "", fmt.Errorf("client_id_missing: the issued client id %q is not an oaiapp registration", reported)
		}
		return reported, nil
	}
	if reported != "" && reported != baseline {
		return "", fmt.Errorf("client_id_mismatch: the authorization used %q but the flow reports %q", baseline, reported)
	}
	return baseline, nil
}

// grantedScopes decides what consent the login actually has. The order is the
// token response's own scope field, then the callback's; when neither carries
// one there is no evidence either way, and guessing that granted equals
// requested would make a refused consent look like a working credential — so
// "undetermined" is a refusal, not a granted.
func grantedScopes(in LoginInput) (string, bool) {
	if scopes := strings.TrimSpace(in.Tokens.Scope); scopes != "" {
		return scopes, true
	}
	if scopes := strings.TrimSpace(in.CallbackScope); scopes != "" {
		return scopes, true
	}
	return "", false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// RevokeCredential asks the issuer to retire a refresh token
// (POST {issuer}/api/accounts/oauth/revoke, form token=<refresh_token> +
// token_type_hint=refresh_token + client_id=<issued>). It is best-effort by
// design and reports which kind of answer it got:
//
//   - nil — the issuer answered 2xx, which is the only thing that counts as a
//     confirmed revocation. (A 4xx does not: the token may still be live and the
//     issuer merely refused this revoke, e.g. for the wrong client.)
//   - error — a network failure, a 4xx, or a 5xx after one bounded retry: the
//     caller must say the revoke is unconfirmed rather than claim a clean
//     removal. Either way it deletes the local row.
//
// The retry is one extra attempt with a short backoff, because a single
// transient failure is the common case and the caller's only other option is to
// tell the owner "maybe revoked".
//
// Copilot and Codex deliberately have no equivalent call: Copilot has no revoke
// endpoint and Codex's is not documented for this path, so those tracks delete
// the local row only (see the console's delete handler).
// DisconnectChannel retires an SIWC channel's credential and removes the
// channel, under the channel's lifecycle lock. Both halves matter:
//
//   - The revoke uses the credential as it stands *now*, re-read inside the
//     lock. Revoking the token an earlier snapshot carried would report
//     "revoked" while a concurrent refresh's newer token stays live at the
//     issuer — a renewable session the owner believes is gone.
//   - The delete happens inside the same lock, so a refresh that starts after
//     the disconnect sees no channel rather than resurrecting a deleted one.
//
// The local row is removed even when the revoke cannot be confirmed: the
// operator asked for the channel to go, and the answer reports what the issuer
// said (see the console's delete handler for the wording). Copilot and Codex
// have no remote revoke call; Codex also serializes its local lifecycle.
func (svc *Service) DisconnectChannel(ctx context.Context, id int64) error {
	return svc.disconnectChannelAtVersion(ctx, id, "")
}

func (svc *Service) DisconnectChannelAtVersion(ctx context.Context, id int64, expected string) (string, error) {
	err := svc.disconnectChannelAtVersion(ctx, id, expected)
	if err != nil {
		if _, readErr := svc.Store.Channel(id); errors.Is(readErr, store.ErrNotFound) {
			return "已在本地删除，远端撤销未确认。", nil
		}
	}
	return "", err
}

// LocalDisconnectChannelAtVersion removes only local state while sharing the
// existing credential lifecycle. It never contacts the issuer to revoke tokens.
func (svc *Service) LocalDisconnectChannelAtVersion(ctx context.Context, id int64, expected string) error {
	lock := svc.lifecycleLock(id)
	lock.Lock()
	defer lock.Unlock()
	if err := accessguard.Check(ctx); err != nil {
		return err
	}
	return svc.Store.DeleteChannelAtVersion(id, expected)
}

func (svc *Service) disconnectChannelAtVersion(ctx context.Context, id int64, expected string) error {
	lock := svc.lifecycleLock(id)
	lock.Lock()
	defer lock.Unlock()
	channel, err := svc.Store.Channel(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrNotFound
		}
		return err
	}
	if expected != "" && store.EntityVersion(channel) != expected {
		return store.ErrEditConflict
	}
	if channel.CredentialType != store.CredentialChatGPTOAuth || channel.OAuth == nil {
		// Not an SIWC channel: nothing to revoke, so the delete is the whole job.
		if err := accessguard.Check(ctx); err != nil {
			return err
		}
		return svc.Store.DeleteChannelAtVersion(id, expected)
	}
	revokeErr := svc.RevokeCredential(ctx, *channel.OAuth)
	if err := accessguard.Check(ctx); err != nil {
		return err
	}
	deleteErr := svc.Store.DeleteChannelAtVersion(id, expected)
	if deleteErr != nil {
		return deleteErr
	}
	return revokeErr
}

func (svc *Service) RevokeCredential(ctx context.Context, credential store.OAuthCredential) error {
	form := url.Values{
		"token":           {credential.RefreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {credential.ClientID},
	}
	body := form.Encode()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := accessguard.Check(ctx); err != nil {
			return err
		}
		if attempt > 0 {
			// A short fixed backoff: this is a delete path, so the total added
			// latency is bounded and small.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(revokeRetryBackoff):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, svc.Endpoints.Revoke, strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, err := accessguard.Do(svc.Client, req)
		if err != nil {
			if accessguard.Denied(err) {
				return err
			}
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// The issuer refused the revoke: not a confirmed retirement, and not
			// worth a retry — the answer is about the request, not the wire.
			return fmt.Errorf("%w: HTTP %d", ErrRevokeRefused, resp.StatusCode)
		}
		lastErr = fmt.Errorf("revoke returned HTTP %d", resp.StatusCode)
	}
	return lastErr
}

// ErrRevokeRefused means the issuer was reached and declined the revoke: the
// token may still be live, and the caller says so distinctly from a revoke it
// could not even attempt.
var ErrRevokeRefused = errors.New("siwc: the issuer refused the revoke request")

// revokeRetryBackoff is the pause between a revoke's two attempts.
const revokeRetryBackoff = 300 * time.Millisecond
