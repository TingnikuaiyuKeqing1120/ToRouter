package store

import (
	"errors"
	"sort"
	"strings"

	"torouter/internal/marketprofile"
)

func IsMarketOAuth(credentialType string) bool {
	_, _, ok := marketprofile.ParseCredentialType(credentialType)
	return ok
}

// MarketOAuthCredential stores a supplier's complete auth object. Account is a
// local consent identity, not a guessed email/tenant identity. It is preserved
// for reauthorization and rotation; independent logins remain independent.
func MarketOAuthCredential(credentialType, raw, account string) (OAuthCredential, error) {
	_, provider, ok := marketprofile.ParseCredentialType(credentialType)
	if !ok {
		return OAuthCredential{}, errors.New("无效的市场供应商")
	}
	state, err := marketprofile.State(raw)
	if err != nil {
		return OAuthCredential{}, err
	}
	if account == "" {
		account, err = randomToken("consent_")
	}
	if err != nil {
		return OAuthCredential{}, err
	}
	return OAuthCredential{Provider: provider, Account: account, PluginState: state, Status: OAuthStatusActive}, nil
}

func validateMarketCredential(credentialType string, o OAuthCredential) error {
	_, provider, ok := marketprofile.ParseCredentialType(credentialType)
	if !ok || o.Provider != provider || strings.TrimSpace(o.Account) == "" || len(o.Account) > 256 ||
		o.AccessToken != "" || o.RefreshToken != "" || o.IDToken != "" || o.GitHubToken != "" || o.APIBase != "" ||
		o.ClientID != "" || o.HostID != "" || o.Subject != "" || o.OrganizationID != "" || o.DeviceID != "" || o.ProjectID != "" {
		return errors.New("市场供应商凭证结构无效")
	}
	if _, err := marketprofile.State(o.PluginState); err != nil {
		return err
	}
	if o.Status != "" && o.Status != OAuthStatusActive && o.Status != OAuthStatusReauthRequired {
		return errors.New("市场供应商凭证状态无效")
	}
	return nil
}

func validateMarketChannel(c Channel) error {
	if len(c.Credentials) == 0 || len(c.Endpoints) == 0 {
		return errors.New("市场接入需要供应商凭证和模型端点")
	}
	for _, e := range c.Endpoints {
		if e.State != "" && e.State != "stateless" {
			return errors.New("市场接入目前使用完整上下文请求")
		}
	}
	for _, item := range c.Credentials {
		if item.APIKey != "" || item.OAuth == nil {
			return errors.New("市场接入凭证无效")
		}
		if err := validateMarketCredential(c.CredentialType, *item.OAuth); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CommitMarketConsent(initial Channel, next OAuthCredential, target PluginConsentTarget, check func() error) (Channel, bool, error) {
	if !IsMarketOAuth(initial.CredentialType) {
		return Channel{}, false, errors.New("无效的市场供应商")
	}
	return s.commitPluginConsent(initial.CredentialType, next, target, check, &initial)
}

// Refresh endpoint and model declarations as one configuration change. A new
// supplier model can introduce another protocol without a second manual save.
func (s *Store) ReplaceMarketCatalogueAtVersion(id int64, next Channel, expected string, check func() error) (Channel, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return c, err
	}
	if !IsMarketOAuth(c.CredentialType) || c.CredentialType != next.CredentialType || expected == "" || CatalogueSourceVersion(c) != expected {
		return c, ErrEditConflict
	}
	if check != nil {
		if err = check(); err != nil {
			return c, err
		}
	}
	c.Endpoints = next.Endpoints
	c.ModelFormats = next.ModelFormats
	c.AvailableModels = append([]string{}, next.AvailableModels...)
	for _, model := range c.ManualModels {
		if !containsString(c.AvailableModels, model) {
			c.AvailableModels = append(c.AvailableModels, model)
		}
	}
	sort.Strings(c.AvailableModels)
	pruneModelEntries(&c)
	err = s.saveChannelLocked(&c, false)
	return c, err
}
