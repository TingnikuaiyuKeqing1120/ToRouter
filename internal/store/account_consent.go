package store

import (
	"time"
)

// AccountOAuthIdentity is independent of short lived tokens and channel location.
func AccountOAuthIdentity(ctype string, o *OAuthCredential) string {
	if o == nil {
		return ""
	}
	switch ctype {
	case CredentialCodexOAuth:
		if o.Account != "" && o.Subject != "" {
			return EntityVersion([]string{ctype, "user_account", o.Subject, o.Account})
		}
		// An account/workspace alone does not identify its user. Incomplete
		// identity only proves equality of the actual authorization material.
		return EntityVersion([]string{ctype, "material", o.AccessToken, o.RefreshToken, o.IDToken})
	case CredentialChatGPTOAuth:
		return EntityVersion([]string{ctype, o.ClientID, o.HostID, o.Subject})
	case CredentialCopilotOAuth:
		if o.Account != "" {
			return EntityVersion([]string{ctype, o.Account})
		}
		return EntityVersion([]string{ctype, o.GitHubToken})
	}
	return ""
}

// CommitAccountConsent adds to a selected pool or replaces one exact member.
// The provider's CredentialLifecycleMutex must be held for reauthorization.
func (s *Store) CommitAccountConsent(ctype string, next OAuthCredential, target PluginConsentTarget, check func() error) (Channel, bool, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	dest, err := s.channel(target.ChannelID)
	if err != nil {
		return Channel{}, false, err
	}
	if dest.CredentialType != ctype || AccountOAuthIdentity(ctype, &next) == "" {
		return Channel{}, false, ErrNativeOAuthIdentity
	}
	index := -1
	if target.CredentialID != "" {
		index = credentialIndex(dest, target.CredentialID)
		if index < 0 || target.Version == "" || AccountOAuthIdentity(ctype, dest.Credentials[index].OAuth) != target.Version {
			return Channel{}, false, ErrNativeOAuthIdentity
		}
		previous := dest.Credentials[index].OAuth
		sameIdentity := AccountOAuthIdentity(ctype, &next) == target.Version
		// Imported GitHub material may not have a verified login yet. The same
		// GitHub token proves this is its authorization, so a successful /user
		// lookup can enrich identity without replacing a different member.
		if ctype == CredentialCopilotOAuth && previous != nil && previous.Account == "" && next.Account != "" && previous.GitHubToken != "" && previous.GitHubToken == next.GitHubToken {
			sameIdentity = true
		}
		if !sameIdentity {
			return Channel{}, false, ErrNativeOAuthIdentity
		}
	} else if target.Version == "" || target.Version != CatalogueSourceVersion(dest) {
		return Channel{}, false, ErrEditConflict
	}
	probe := dest
	probe.OAuth = nil
	probe.APIKey = ""
	probe.Credentials = []CredentialItem{{ID: "validate", Enabled: true, OAuth: &next}}
	if err := ValidateChannel(probe); err != nil {
		return Channel{}, false, err
	}
	rows, err := s.ListChannels()
	if err != nil {
		return Channel{}, false, err
	}
	for _, row := range rows {
		if row.CredentialType != ctype {
			continue
		}
		for _, item := range row.Credentials {
			if AccountOAuthIdentity(ctype, item.OAuth) == AccountOAuthIdentity(ctype, &next) && (index < 0 || row.ID != dest.ID || item.ID != target.CredentialID) {
				return Channel{}, false, &OAuthOwnerConflict{ChannelID: row.ID, CredentialID: item.ID}
			}
		}
	}
	if check != nil {
		if err = check(); err != nil {
			return Channel{}, false, err
		}
	}
	next.Status, next.StatusReason, next.UpdatedAt = OAuthStatusActive, "", time.Now().UTC()
	if index >= 0 {
		dest.Credentials[index].OAuth = &next
	} else {
		id, e := randomToken("cred_")
		if e != nil {
			return Channel{}, false, e
		}
		dest.Credentials = append(dest.Credentials, CredentialItem{ID: id, Enabled: true, OAuth: &next})
	}
	dest.APIKey, dest.OAuth = "", nil
	dest.normalize()
	err = s.saveChannelLocked(&dest, false)
	return dest, index >= 0, err
}
