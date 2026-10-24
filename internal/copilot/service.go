package copilot

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

// refreshSkew is how long before expiry a bearer token is exchanged for a new
// one. Protocol ref step 2 ("提前 60s 刷新"): the exchange takes a round trip
// and the debounce below means the first request after the threshold pays it,
// not the request that finds the token already dead.
const refreshSkew = 60 * time.Second

// Service owns the Copilot credential lifecycle for one process: it hands the
// gateway fresh bearer tokens and it records what GitHub says about an account.
// One instance is shared by the gateway and the console, so there is exactly
// one lock domain per channel.
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

// Bearer returns a Copilot bearer token that will still be valid for at least
// refreshSkew, exchanging a new one when it is not.
//
// The double-check under the lock is the whole point (good-finds G017): a
// rotation is single-use, so two concurrent exchanges for one account would
// permanently destroy the credential — GitHub invalidates the first token when
// the second is issued. The gateway is inherently concurrent (every forwarded
// request plus any console work calls this), so the lock is per channel and the
// stored row, not the caller's snapshot, decides whether an exchange is needed.
func (svc *Service) Bearer(ctx context.Context, c store.Channel) (string, error) {
	if err := usable(c); err != nil {
		return "", err
	}
	if time.Until(c.OAuth.ExpiresAt) > refreshSkew {
		return c.OAuth.AccessToken, nil
	}
	lock := svc.lockFor(c.ID)
	lock.Lock()
	defer lock.Unlock()
	// Someone may have refreshed while this caller waited for the lock: the row
	// is the only truth about what token is current.
	fresh, err := svc.Store.Channel(c.ID)
	if err != nil {
		return "", err
	}
	var targetOAuth *store.OAuthCredential
	targetCredID := ""
	for _, cred := range fresh.Credentials {
		if cred.OAuth != nil && (cred.OAuth.GitHubToken != "" && cred.OAuth.GitHubToken == c.OAuth.GitHubToken) {
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
	token, err := ExchangeToken(ctx, svc.Client, svc.Endpoints, targetOAuth.GitHubToken)
	if err != nil {
		svc.markFailure(c.ID, targetCredID, err)
		return "", err
	}
	// Preserve the actual token already issued by a successful request. The
	// optional guard stops later sends instead of discarding this result.
	if targetCredID != "" {
		if err = svc.Store.ReplaceCopilotTokenForCredential(c.ID, targetCredID, token.Token, token.ExpiresAt); err != nil {
			return "", err
		}
	} else {
		if err = svc.Store.ReplaceCopilotToken(c.ID, token.Token, token.ExpiresAt); err != nil {
			return "", err
		}
	}
	return token.Token, nil
}

// usable reports whether the credential may be exchanged at all, naming the
// recorded failure when it may not.
func usable(c store.Channel) error {
	if c.CredentialType != store.CredentialCopilotOAuth || c.OAuth == nil {
		return fmt.Errorf("channel %d carries no copilot oauth credential", c.ID)
	}
	switch c.OAuth.Status {
	case store.OAuthStatusUnsubscribed:
		return fmt.Errorf("channel %d: this GitHub account has no Copilot subscription (%s)", c.ID, c.OAuth.StatusReason)
	case store.OAuthStatusReauthRequired:
		return fmt.Errorf("channel %d: %s (re-run the Copilot authorization to replace the credential)", c.ID, c.OAuth.StatusReason)
	case "", store.OAuthStatusActive:
		return nil
	default:
		return fmt.Errorf("channel %d: oauth credential has unknown status %q", c.ID, c.OAuth.Status)
	}
}

// markFailure turns an exchange failure that describes the account into a
// stored status. 403 disables the channel — an account without a subscription
// cannot serve requests, and leaving it enabled would keep paying for failed
// exchanges. 401 only marks: the credential is the operator's to replace, and
// the status already stops the retries. Wire failures record nothing: they say
// nothing about the account.
func (svc *Service) markFailure(id int64, credentialID string, err error) {
	switch {
	case errors.Is(err, ErrUnsubscribed):
		_ = svc.Store.SetOAuthCredentialStatus(id, credentialID, store.OAuthStatusUnsubscribed,
			"GitHub reports no active Copilot subscription for this account; the channel was disabled. Subscribe, then authorize again.", true)
	case errors.Is(err, ErrReauthRequired):
		_ = svc.Store.SetOAuthCredentialStatus(id, credentialID, store.OAuthStatusReauthRequired,
			"GitHub rejected the stored token (revoked or expired). Run the Copilot authorization again to replace the credential.", false)
	}
}

func (svc *Service) lockFor(id int64) *sync.Mutex { return svc.Store.CredentialLifecycleMutex(id) }

// Login is everything the console needs to turn a freshly authorized GitHub
// token into a channel.
type Login struct {
	Identity     string // verified GitHub login; never a plan or display fallback
	GitHubToken  string
	Bearer       CopilotToken
	Account      string
	APIBase      string
	Models       []string
	ModelFormats map[string][]string
	// Degraded names the steps that failed without failing the login, so the
	// caller can say what is missing instead of pretending everything worked.
	Degraded []string
}

// CompleteLogin performs the whole post-authorization chain: exchange the
// GitHub token for a bearer, read the account (for the label and the dynamic
// API base) and pull the model catalogue.
//
// The api base prefers copilot_internal/user's endpoints.api and falls back to
// Endpoints.ModelAPI (protocol ref step 3). The label prefers the GitHub login
// and falls back to the Copilot plan.
//
// What fails the login and what degrades it are different questions. The
// exchange is the authorization itself, so its 403/401 ends the flow. The
// profile, by contrast, only supplies decoration — a display name and a nicer
// api base — so a flaky account call must not throw away an authorization the
// user just completed in their browser; the flow continues on the documented
// default base and reports the degradation (P2-2). Only the catalogue is still
// fatal: a channel without models cannot serve anything, and re-running the
// device flow is a smaller cost than a channel that silently does nothing.
func (svc *Service) CompleteLogin(ctx context.Context, githubToken string) (Login, error) {
	bearer, err := ExchangeToken(ctx, svc.Client, svc.Endpoints, githubToken)
	if err != nil {
		return Login{}, err
	}
	if err := accessguard.Check(ctx); err != nil {
		return Login{}, err
	}
	login := Login{GitHubToken: githubToken, Bearer: bearer, APIBase: svc.Endpoints.ModelAPI}
	profile, profileErr := FetchCopilotUser(ctx, svc.Client, svc.Endpoints, githubToken)
	if err := accessguard.Check(ctx); err != nil {
		return Login{}, err
	}
	if profileErr != nil {
		login.Degraded = append(login.Degraded, "account_profile")
	} else {
		if profile.Endpoints.API != "" {
			login.APIBase = profile.Endpoints.API
		}
		login.Account = profile.CopilotPlan
	}
	if name, err := GitHubLogin(ctx, svc.Client, svc.Endpoints, githubToken); err == nil && name != "" {
		login.Account = name
		login.Identity = name
	}
	if err := accessguard.Check(ctx); err != nil {
		return Login{}, err
	}
	return login, nil
}
