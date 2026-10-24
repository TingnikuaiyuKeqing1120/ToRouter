package store

import "strings"

// ValidateOAuthTransferMaterial accepts offline materials awaiting the provider's
// confirm-import enrichment. Persistent channels still use ValidateChannel.
func ValidateOAuthTransferMaterial(item OAuthTransferItem) error {
	if item.OAuth == nil {
		return ErrInvalidOAuthTransfer
	}
	o := item.OAuth
	if item.CredentialType == CredentialClaudeOAuth {
		if o.Provider != OAuthProviderClaude || strings.TrimSpace(o.AccessToken) == "" || strings.TrimSpace(o.RefreshToken) == "" {
			return ErrInvalidOAuthTransfer
		}
		return nil
	}
	if item.CredentialType == CredentialCodexOAuth {
		if o.Provider != OAuthProviderCodex || strings.TrimSpace(o.RefreshToken) == "" && (strings.TrimSpace(o.AccessToken) == "" || strings.TrimSpace(o.Account) == "") || strings.TrimSpace(o.ClientID) == "" {
			return ErrInvalidOAuthTransfer
		}
		return nil
	}
	if item.CredentialType == CredentialAntigravityOAuth {
		if o.Provider != OAuthProviderAntigravity || strings.TrimSpace(o.RefreshToken) == "" {
			return ErrInvalidOAuthTransfer
		}
		return nil
	}
	return ValidateOAuthTransferItem(item)
}

// Material identity preserves exact refresh material for offline previews; it
// does not promote missing profile data into a provider account identity.
func OAuthTransferMaterialIdentity(item OAuthTransferItem) string {
	aliases := OAuthTransferMaterialAliases(item)
	if len(aliases) == 0 {
		return ""
	}
	return aliases[0]
}
func OAuthTransferMaterialAliases(item OAuthTransferItem) []string {
	if ValidateOAuthTransferMaterial(item) != nil {
		return nil
	}
	aliases := oauthTransferAliases(item)
	if item.CredentialType == CredentialClaudeOAuth || item.CredentialType == CredentialAntigravityOAuth || item.CredentialType == CredentialCodexOAuth {
		o := item.OAuth
		materialType, token := "refresh", o.RefreshToken
		if token == "" {
			materialType, token = "access", o.AccessToken
		}
		material := EntityVersion([]string{item.CredentialType, o.Provider, materialType, token})
		if len(aliases) == 0 {
			return []string{material}
		}
		aliases = append(aliases, material)
	}
	return aliases
}

func OAuthTransferMaterialIdentityAliases(item OAuthTransferItem) []string {
	return OAuthTransferMaterialAliases(item)
}
