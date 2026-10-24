package oauthtransfer

import (
	"context"
	"torouter/internal/store"
)

type Completer func(context.Context, string, store.OAuthCredential) (store.OAuthCredential, error)

func CompleteImportItems(ctx context.Context, items []store.OAuthTransferItem, complete Completer) ([]store.OAuthTransferItem, error) {
	out := append([]store.OAuthTransferItem(nil), items...)
	for i, item := range items {
		if err := store.ValidateOAuthTransferMaterial(item); err != nil {
			return out, err
		}
		o := *item.OAuth
		needs := item.CredentialType == store.CredentialCodexOAuth && o.AccessToken == "" || item.CredentialType == store.CredentialClaudeOAuth && (o.Account == "" || o.DeviceID == "" || o.ClientID == "") || item.CredentialType == store.CredentialAntigravityOAuth && (o.AccessToken == "" || o.ProjectID == "" || o.ClientID == "" || o.Account == "" && o.Subject == "")
		if needs {
			if complete == nil {
				return out, ErrInvalid
			}
			var err error
			next, completeErr := complete(ctx, item.CredentialType, o)
			candidate := item
			candidate.OAuth = &next
			if store.ValidateOAuthTransferMaterial(candidate) == nil {
				o = next
				item = candidate
				out[i] = item
			}
			err = completeErr
			if err != nil {
				return out, err
			}
		}
		item.OAuth = &o
		if store.ValidateOAuthTransferItem(item) != nil {
			return out, ErrInvalid
		}
		out[i] = item
	}
	return out, nil
}
func (s *Service) CompleteImportItems(ctx context.Context, items []store.OAuthTransferItem) ([]store.OAuthTransferItem, error) {
	return CompleteImportItems(ctx, items, s.complete)
}
