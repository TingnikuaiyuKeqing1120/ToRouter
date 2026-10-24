package codex

import (
	"context"
	"errors"
	"time"

	"torouter/internal/store"
)

func sameUsageIdentity(a, b *store.OAuthCredential) bool {
	if a == nil || b == nil || a.Provider != b.Provider || a.ClientID != b.ClientID || a.Account != b.Account || a.Subject != b.Subject {
		return false
	}
	// Without a stable account/subject, only the same renewable credential is
	// evidence of identity. Empty identity fields never establish a match.
	return a.Account != "" || a.Subject != "" || a.RefreshToken != "" && a.RefreshToken == b.RefreshToken
}

func usageCredential(c store.Channel, id string) (*store.OAuthCredential, error) {
	if c.CredentialType == store.CredentialCodexOAuth {
		for _, cred := range c.Credentials {
			if cred.ID == id && cred.OAuth != nil {
				return cred.OAuth, nil
			}
		}
	}
	return nil, errors.New("Codex credential is no longer available")
}

// UsageForCredential is a bounded passive read of a specific pool member. It
// shares token rotation's lifecycle lock and never substitutes the main account.
// It returns a fresh snapshot with source-bucket provenance intact.
func (svc *Service) UsageForCredential(ctx context.Context, c store.Channel, credID string) (Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	lock := svc.lockFor(c.ID)
	if err := lockUsage(ctx, lock); err != nil {
		return Usage{}, err
	}
	defer lock.Unlock()
	snapshot, _, err := svc.usageForCredentialLocked(ctx, c, credID)
	return snapshot, err
}

// The caller holds lockFor(c.ID). The returned channel binds the receipt to
// the live member after any token rotation and before the upstream read.
func (svc *Service) usageForCredentialLocked(ctx context.Context, c store.Channel, credID string) (Usage, store.Channel, error) {
	expected, err := usageCredential(c, credID)
	if err != nil {
		return Usage{}, c, err
	}
	c.OAuth = expected
	token, err := svc.bearerCredentialLocked(ctx, c, usageHTTPClient(svc.Client), credID)
	if err != nil {
		if ctx.Err() != nil {
			return Usage{}, c, ctx.Err()
		}
		return Usage{}, c, errors.New("could not obtain the selected Codex credential; check account authorization")
	}
	current, err := svc.Store.Channel(c.ID)
	if err != nil || current.Generation != c.Generation {
		return Usage{}, c, errors.New("Codex account changed during quota read")
	}
	oauth, err := usageCredential(current, credID)
	if err != nil || oauth.Provider != expected.Provider || oauth.ClientID != expected.ClientID || oauth.Account != expected.Account || oauth.Subject != expected.Subject || oauth.AccessToken != token {
		return Usage{}, c, errors.New("Codex account changed during quota read")
	}
	account := AccountHeaderID(token, oauth.IDToken, oauth.Account, oauth.Subject)
	snapshot, err := FetchUsage(ctx, svc.Client, svc.Endpoints, token, account)
	return snapshot, current, err
}
