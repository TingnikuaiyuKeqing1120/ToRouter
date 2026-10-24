package console

import (
	"context"
	"errors"
	"net/http"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

// Used only after the owner confirms adding a refresh-only native credential.
// Discovery and preview never rotate the source tool's token.
func completeCodexImport(ctx context.Context, client *http.Client, endpoints codex.Endpoints, value store.OAuthCredential) (store.OAuthCredential, error) {
	tokens, err := codex.RefreshToken(ctx, client, endpoints, value.RefreshToken)
	if err != nil {
		return value, errors.New("Codex 未能补齐访问令牌，请核对原始凭证")
	}
	value.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		value.RefreshToken = tokens.RefreshToken
	}
	if tokens.IDToken != "" {
		value.IDToken = tokens.IDToken
	}
	if claims, err := codex.ParseIDClaims(value.IDToken); err == nil {
		if claims.Subject != "" {
			value.Subject = claims.Subject
		}
		if claims.AccountID != "" {
			value.Account = claims.AccountID
		}
	}
	if account := codex.ChatGPTAccountIDOf(value.AccessToken); account != "" {
		value.Account = account
	}
	value.ClientID = codex.ClientIDValue()
	value.Provider = store.OAuthProviderCodex
	value.Status = store.OAuthStatusActive
	value.ExpiresAt = codex.ExpiryOf(tokens)
	value.UpdatedAt = time.Now().UTC()
	return value, nil
}
