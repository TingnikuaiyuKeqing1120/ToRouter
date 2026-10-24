package oauthtransfer

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

type rawAccountPayload struct {
	Tokens *struct {
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`

	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`

	Meta *struct {
		Label                 string `json:"label"`
		Issuer                string `json:"issuer"`
		Note                  string `json:"note"`
		Status                string `json:"status"`
		WorkspaceID           string `json:"workspaceId"`
		WorkspaceIDSnake      string `json:"workspace_id"`
		ChatGPTAccountID      string `json:"chatgptAccountId"`
		ChatGPTAccountIDSnake string `json:"chatgpt_account_id"`
	} `json:"meta"`

	Credentials *struct {
		AuthMode string `json:"auth_mode"`
	} `json:"credentials"`

	AuthMode string `json:"auth_mode"`
}

func parseSingleNative(raw []byte) (store.OAuthTransferItem, bool) {
	var p rawAccountPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return store.OAuthTransferItem{}, false
	}

	// Skip special agent identities that do not use standard OAuth
	if p.Credentials != nil && strings.Contains(strings.ToLower(p.Credentials.AuthMode), "agent") {
		return store.OAuthTransferItem{}, false
	}
	if strings.Contains(strings.ToLower(p.AuthMode), "agent") {
		return store.OAuthTransferItem{}, false
	}

	var accessToken, refreshToken, idToken string
	isCodexManager := p.Meta != nil
	if p.Tokens != nil {
		accessToken = strings.TrimSpace(p.Tokens.AccessToken)
		refreshToken = strings.TrimSpace(p.Tokens.RefreshToken)
		idToken = strings.TrimSpace(p.Tokens.IDToken)
	}
	if accessToken == "" && p.AccessToken != "" {
		accessToken = strings.TrimSpace(p.AccessToken)
	}
	if refreshToken == "" && p.RefreshToken != "" {
		refreshToken = strings.TrimSpace(p.RefreshToken)
	}
	if idToken == "" && p.IDToken != "" {
		idToken = strings.TrimSpace(p.IDToken)
	}

	if refreshToken == "" && accessToken == "" {
		return store.OAuthTransferItem{}, false
	}

	var subject, account string
	if idToken != "" {
		if claims, err := codex.ParseIDClaims(idToken); err == nil {
			subject = claims.Subject
			if claims.AccountID != "" && claims.AccountID != claims.Subject {
				account = claims.AccountID
			}
		}
	}
	if acctFromAccess := codex.ChatGPTAccountIDOf(accessToken); acctFromAccess != "" {
		account = acctFromAccess
	}
	if account == "" && idToken != "" {
		if acctFromID := codex.ChatGPTAccountIDOf(idToken); acctFromID != "" {
			account = acctFromID
		}
	}
	if isCodexManager && p.Meta != nil {
		ws := p.Meta.WorkspaceID
		if ws == "" {
			ws = p.Meta.WorkspaceIDSnake
		}
		acct := p.Meta.ChatGPTAccountID
		if acct == "" {
			acct = p.Meta.ChatGPTAccountIDSnake
		}
		if acct != "" {
			account = strings.TrimSpace(acct)
		} else if ws != "" {
			account = strings.TrimSpace(ws)
		}
	} else if account == "" {
		cand := ""
		if p.Tokens != nil && p.Tokens.AccountID != "" {
			cand = strings.TrimSpace(p.Tokens.AccountID)
		} else if p.AccountID != "" {
			cand = strings.TrimSpace(p.AccountID)
		}
		if cand != "" && cand != subject {
			account = cand
		}
	}

	label := ""
	if isCodexManager && p.Meta != nil {
		cand := strings.TrimSpace(p.Meta.Label)
		note := strings.TrimSpace(p.Meta.Note)
		validCand := cand != "" && !strings.Contains(cand, "@") && cand != subject && cand != account
		validNote := note != "" && !strings.Contains(note, "@") && note != subject && note != account
		if validCand && validNote && cand != note {
			label = cand + " (" + note + ")"
		} else if validCand {
			label = cand
		} else if validNote {
			label = note
		}
	}

	enabled := true
	if isCodexManager && p.Meta != nil {
		if strings.EqualFold(p.Meta.Status, "disabled") || strings.EqualFold(p.Meta.Status, "inactive") {
			enabled = false
		}
	}

	expiresAt := time.Time{}
	if claims, err := codex.ParseIDClaims(accessToken); err == nil && !claims.Expiry.IsZero() {
		expiresAt = claims.Expiry
	}
	if idToken != "" {
		if claims, err := codex.ParseIDClaims(idToken); err == nil && !claims.Expiry.IsZero() && expiresAt.IsZero() {
			expiresAt = claims.Expiry
		}
	}

	item := store.OAuthTransferItem{
		CredentialType: store.CredentialCodexOAuth,
		Label:          label,
		Enabled:        enabled,
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex", State: "stateless"},
		},
		OAuth: &store.OAuthCredential{
			Provider:     store.OAuthProviderCodex,
			ClientID:     codex.ClientID,
			Account:      account,
			Subject:      subject,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			IDToken:      idToken,
			Status:       store.OAuthStatusActive,
			ExpiresAt:    expiresAt,
			Scopes:       codex.ScopeAuthorize,
			APIBase:      "https://chatgpt.com/backend-api/codex",
		},
	}
	return item, true
}

// ParseNativeCredentials parses external credential files:
// - Codex auth.json (tokens object or flat object)
// - Codex-Manager export (single account object or array of account objects)
// It extracts the genuine OAuth material and bounds limits.
func ParseNativeCredentials(ctx context.Context, raw []byte) ([]store.OAuthTransferItem, error) {
	return ParseNativeCredentialsForType(ctx, "", raw)
}
