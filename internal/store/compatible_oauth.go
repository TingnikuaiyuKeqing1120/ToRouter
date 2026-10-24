package store

import (
	"errors"
	"fmt"
	"time"

	"torouter/internal/compatprofile"
)

func IsCompatibleOAuth(credentialType string) bool {
	if IsMarketOAuth(credentialType) {
		return true
	}
	_, ok := compatprofile.Lookup(credentialType)
	return ok
}

// RequiresIndependentOAuth excludes raw credential writes/copies in normal
// configuration APIs. Authorization and explicit migration own those writes.
func RequiresIndependentOAuth(credentialType string) bool {
	return credentialType == CredentialCodexOAuth || credentialType == CredentialChatGPTOAuth || credentialType == CredentialCopilotOAuth || IsNativeOAuth(credentialType) || IsCompatibleOAuth(credentialType)
}

func CompatibleOAuthCredential(credentialType, raw string) (OAuthCredential, error) {
	p, ok := compatprofile.Lookup(credentialType)
	if !ok {
		return OAuthCredential{}, compatprofile.ErrState
	}
	state, account, err := p.State(raw)
	if err != nil {
		return OAuthCredential{}, err
	}
	return OAuthCredential{Provider: p.ID, Account: account, APIBase: p.API, PluginState: state, Status: OAuthStatusActive}, nil
}

func validateCompatibleCredential(credentialType string, o OAuthCredential) error {
	if IsMarketOAuth(credentialType) {
		return validateMarketCredential(credentialType, o)
	}
	next, err := CompatibleOAuthCredential(credentialType, o.PluginState)
	if err != nil {
		return err
	}
	if o.Provider != next.Provider || o.Account != next.Account || o.APIBase != next.APIBase ||
		o.AccessToken != "" || o.RefreshToken != "" || o.IDToken != "" || o.GitHubToken != "" ||
		o.ClientID != "" || o.HostID != "" || o.Subject != "" || o.OrganizationID != "" || o.DeviceID != "" || o.ProjectID != "" {
		return compatprofile.ErrState
	}
	if o.Status != "" && o.Status != OAuthStatusActive && o.Status != OAuthStatusReauthRequired {
		return compatprofile.ErrState
	}
	return nil
}

// CompatibleOAuthIdentity is stable through token rotation. StateVersion is
// separate and private; CAS prevents stale workers from overwriting new consent.
func CompatibleOAuthIdentity(credentialType string, o *OAuthCredential) string {
	if o == nil || validateCompatibleCredential(credentialType, *o) != nil {
		return ""
	}
	return EntityVersion([]string{credentialType, o.Provider, o.Account, o.APIBase})
}

func PluginStateVersion(o *OAuthCredential) string {
	if o == nil || o.PluginState == "" {
		return ""
	}
	return EntityVersion(o.PluginState)
}

func validateCompatibleChannel(c Channel) error {
	if IsMarketOAuth(c.CredentialType) {
		return validateMarketChannel(c)
	}
	if len(c.Endpoints) != 1 {
		return errors.New("兼容接入需要独立授权凭证和固定端点")
	}
	p, _ := compatprofile.Lookup(c.CredentialType)
	if c.Endpoints[0].Format != p.Format() || c.Endpoints[0].BaseURL != p.API {
		return errors.New("兼容接入端点不符合已验证协议")
	}
	if c.Endpoints[0].State != "" && c.Endpoints[0].State != "stateless" {
		return errors.New("此兼容接入只支持完整上下文请求，不提供服务端会话")
	}
	for _, item := range c.Credentials {
		if item.APIKey != "" || item.OAuth == nil {
			return compatprofile.ErrState
		}
		if err := validateCompatibleCredential(c.CredentialType, *item.OAuth); err != nil {
			return err
		}
	}
	return nil
}

// ReplacePluginState acknowledges the complete state only after a durable write.
// Call after the provider returns rotation material even if access was revoked
// meanwhile. The exact member, stable account and previous state must still match.
func (s *Store) ReplacePluginState(id int64, credentialID, expected string, next OAuthCredential) (OAuthCredential, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return OAuthCredential{}, err
	}
	i := credentialIndex(c, credentialID)
	if i < 0 || expected == "" || PluginStateVersion(c.Credentials[i].OAuth) != expected {
		return OAuthCredential{}, ErrEditConflict
	}
	if err = validateCompatibleCredential(c.CredentialType, next); err != nil {
		return OAuthCredential{}, err
	}
	old := c.Credentials[i].OAuth
	if CompatibleOAuthIdentity(c.CredentialType, old) != CompatibleOAuthIdentity(c.CredentialType, &next) {
		return OAuthCredential{}, ErrNativeOAuthIdentity
	}
	// Normal rotation cannot change presentation or an independent status mark.
	next.AccountLabel, next.Status, next.StatusReason = old.AccountLabel, old.Status, old.StatusReason
	next.UpdatedAt = time.Now().UTC()
	c.Credentials[i].OAuth = &next
	c.normalize()
	if err = s.persistRow(&c); err != nil {
		if err = s.persistRow(&c); err != nil {
			return OAuthCredential{}, fmt.Errorf("插件续期结果未能保存，请检查本地存储: %w", err)
		}
	}
	return next, nil
}

// MarkPluginReauthorization quarantines only the exact state whose refresh
// outcome is unknown. It cannot invalidate consent saved by another operation.
func (s *Store) MarkPluginReauthorization(id int64, credentialID, expected string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return err
	}
	i := credentialIndex(c, credentialID)
	if i < 0 || expected == "" || !IsCompatibleOAuth(c.CredentialType) || PluginStateVersion(c.Credentials[i].OAuth) != expected {
		return ErrEditConflict
	}
	c.Credentials[i].OAuth.Status = OAuthStatusReauthRequired
	c.Credentials[i].OAuth.StatusReason = "当前授权不可用，请重新独立授权"
	c.normalize()
	if err = s.persistRow(&c); err != nil {
		err = s.persistRow(&c)
	}
	return err
}

type PluginConsentTarget struct {
	ChannelID    int64
	CredentialID string // empty means append to the selected pool
	Version      string // stable owner identity for reauth; catalogue version for append
}

// CommitPluginConsent commits a new independent login without temporary owner
// channels or copied tokens. The guard runs under the same lock as config writes.
func (s *Store) CommitPluginConsent(credentialType string, next OAuthCredential, target PluginConsentTarget, check func() error) (Channel, bool, error) {
	if target.ChannelID > 0 {
		lock := s.CredentialLifecycleMutex(target.ChannelID)
		lock.Lock()
		defer lock.Unlock()
	}
	return s.commitPluginConsent(credentialType, next, target, check, nil)
}

func (s *Store) commitPluginConsent(credentialType string, next OAuthCredential, target PluginConsentTarget, check func() error, initial *Channel) (Channel, bool, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if err := validateCompatibleCredential(credentialType, next); err != nil {
		return Channel{}, false, err
	}
	if target.ChannelID < 0 || target.ChannelID == 0 && (target.CredentialID != "" || target.Version != "") {
		return Channel{}, false, ErrEditConflict
	}
	var dest Channel
	var err error
	if target.ChannelID > 0 {
		dest, err = s.channel(target.ChannelID)
		if err != nil {
			return Channel{}, false, err
		}
		if dest.CredentialType != credentialType || target.Version == "" {
			return Channel{}, false, ErrEditConflict
		}
		if target.CredentialID == "" && CatalogueSourceVersion(dest) != target.Version {
			return Channel{}, false, ErrEditConflict
		}
	}
	rows, err := s.ListChannels()
	if err != nil {
		return Channel{}, false, err
	}
	matchChannel, matchIndex := -1, -1
	identity := CompatibleOAuthIdentity(credentialType, &next)
	for ci, c := range rows {
		if c.CredentialType != credentialType {
			continue
		}
		for i, item := range c.Credentials {
			if CompatibleOAuthIdentity(credentialType, item.OAuth) == identity {
				if matchIndex >= 0 {
					return Channel{}, false, ErrNativeOAuthIdentity
				}
				matchChannel, matchIndex = ci, i
			}
		}
	}
	if target.CredentialID != "" {
		i := credentialIndex(dest, target.CredentialID)
		if i < 0 || CompatibleOAuthIdentity(credentialType, dest.Credentials[i].OAuth) != target.Version {
			return Channel{}, false, ErrEditConflict
		}
		if CompatibleOAuthIdentity(credentialType, dest.Credentials[i].OAuth) != identity ||
			matchIndex < 0 || rows[matchChannel].ID != dest.ID || matchIndex != i {
			return Channel{}, false, ErrNativeOAuthIdentity
		}
	}
	if check != nil {
		if err = check(); err != nil {
			return Channel{}, false, err
		}
	}
	next.Status, next.StatusReason, next.UpdatedAt = OAuthStatusActive, "", time.Now().UTC()
	if matchIndex >= 0 {
		if target.ChannelID > 0 && (rows[matchChannel].ID != target.ChannelID || target.CredentialID == "") {
			return Channel{}, false, &OAuthOwnerConflict{ChannelID: rows[matchChannel].ID, CredentialID: rows[matchChannel].Credentials[matchIndex].ID}
		}
		dest = rows[matchChannel]
		next.AccountLabel = dest.Credentials[matchIndex].OAuth.AccountLabel
		dest.Credentials[matchIndex].OAuth = &next
		if IsMarketOAuth(credentialType) {
			dest.Generation++
		}
		dest.normalize()
		err = s.persistRow(&dest)
		return dest, true, err
	}
	p, _ := compatprofile.Lookup(credentialType)
	next.AccountLabel = p.Name
	if target.ChannelID == 0 {
		dest = Channel{Name: p.Name, CredentialType: credentialType, Enabled: true, Endpoints: []Endpoint{{Format: p.Format(), BaseURL: p.API, State: "stateless"}}}
		if initial != nil {
			dest = *initial
			dest.CredentialType = credentialType
			next.AccountLabel = dest.Name
		}
	}
	id, err := randomToken("cred_")
	if err != nil {
		return Channel{}, false, err
	}
	dest.Credentials = append(dest.Credentials, CredentialItem{ID: id, Label: fmt.Sprintf("凭据 %d", len(dest.Credentials)+1), Enabled: true, OAuth: &next})
	err = s.saveChannelLocked(&dest, false)
	return dest, false, err
}
