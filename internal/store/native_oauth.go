package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrNativeOAuthIdentity = errors.New("OAuth 凭证身份已变化，请重新读取渠道")

// AppendNativeOAuthCredential is the native authorization commit boundary.
// Ordinary channel drafts cannot create OAuth materials or duplicate owners.
func (s *Store) AppendNativeOAuthCredential(id int64, expected string, credential CredentialItem, check func() error) (Channel, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return Channel{}, err
	}
	if expected == "" || CatalogueSourceVersion(c) != expected {
		return Channel{}, ErrEditConflict
	}
	if !IsNativeOAuth(c.CredentialType) || credential.OAuth == nil || credential.ID == "" {
		return Channel{}, ErrNativeOAuthIdentity
	}
	if err = ValidateNativeOAuthCredential(c.CredentialType, *credential.OAuth); err != nil {
		return Channel{}, err
	}
	for _, existing := range c.Credentials {
		if existing.ID == credential.ID {
			return Channel{}, ErrNativeOAuthIdentity
		}
	}
	rows, err := s.ListChannels()
	if err != nil {
		return Channel{}, err
	}
	identity := NativeOAuthAccountIdentity(c.CredentialType, *credential.OAuth)
	for _, row := range rows {
		if row.CredentialType == c.CredentialType {
			for _, item := range row.Credentials {
				if item.OAuth != nil && NativeOAuthAccountIdentity(row.CredentialType, *item.OAuth) == identity {
					return Channel{}, ErrNativeOAuthIdentity
				}
			}
		}
	}
	if check != nil {
		if err = check(); err != nil {
			return Channel{}, err
		}
	}
	credential.Label = fmt.Sprintf("凭据 %d", len(c.Credentials)+1)
	c.Credentials = append(c.Credentials, credential)
	err = s.saveChannelLocked(&c, false)
	return c, err
}

func IsNativeOAuth(credentialType string) bool {
	return credentialType == CredentialClaudeOAuth || credentialType == CredentialAntigravityOAuth || credentialType == CredentialDevinOAuth
}

func ValidateNativeOAuthCredential(credentialType string, oauth OAuthCredential) error {
	provider := ""
	switch credentialType {
	case CredentialClaudeOAuth:
		provider = OAuthProviderClaude
		if strings.TrimSpace(oauth.RefreshToken) == "" || strings.TrimSpace(oauth.ClientID) == "" || strings.TrimSpace(oauth.DeviceID) == "" || oauth.Account == "" && oauth.Subject == "" {
			return errors.New("Claude 授权缺少刷新令牌、客户端、稳定账号或设备信息")
		}
	case CredentialAntigravityOAuth:
		provider = OAuthProviderAntigravity
		if strings.TrimSpace(oauth.RefreshToken) == "" || strings.TrimSpace(oauth.ClientID) == "" || strings.TrimSpace(oauth.ProjectID) == "" || oauth.Account == "" && oauth.Subject == "" {
			return errors.New("Antigravity 授权缺少刷新令牌、客户端、稳定账号或项目")
		}
	case CredentialDevinOAuth:
		provider = OAuthProviderDevin
		if oauth.RefreshToken != "" || !oauth.ExpiresAt.IsZero() {
			return errors.New("Devin session 不声明刷新令牌或计划到期时间")
		}
	default:
		return errors.New("不支持的原生 OAuth 接入")
	}
	if oauth.Provider != provider || strings.TrimSpace(oauth.AccessToken) == "" {
		return errors.New("OAuth 提供方或访问令牌无效")
	}
	if oauth.Status != "" && oauth.Status != OAuthStatusActive && oauth.Status != OAuthStatusReauthRequired {
		return errors.New("OAuth 状态无效")
	}
	return nil
}

// NativeOAuthAccountIdentity identifies an actual account, never its display
// label. Without a provider account ID, Devin can only deduplicate the same
// session material; a different session is not proof of the same account.
func NativeOAuthAccountIdentity(credentialType string, oauth OAuthCredential) string {
	if !IsNativeOAuth(credentialType) {
		return ""
	}
	identity := []string{credentialType, oauth.Provider}
	if oauth.Subject != "" {
		identity = append(identity, "subject", oauth.Subject)
	} else if oauth.Account != "" {
		identity = append(identity, "account", oauth.Account)
	} else {
		if credentialType != CredentialDevinOAuth || oauth.AccessToken == "" {
			return ""
		}
		identity = append(identity, "session", oauth.AccessToken)
	}
	return EntityVersion(identity)
}

func nativeOAuthDomainIdentity(credentialType string, oauth OAuthCredential) string {
	return EntityVersion([]string{NativeOAuthAccountIdentity(credentialType, oauth), oauth.ClientID, oauth.APIBase, oauth.OrganizationID, oauth.DeviceID, oauth.ProjectID})
}

// NativeOAuthIdentity identifies the stable provider domain. Callers pair it
// with an exact credential ID; an ambiguous pool match is never a first-item fallback.
func NativeOAuthIdentity(credentialType string, oauth *OAuthCredential) string {
	if !IsNativeOAuth(credentialType) || oauth == nil {
		return ""
	}
	return nativeOAuthDomainIdentity(credentialType, *oauth)
}

// ReplaceNativeOAuthCredential checks the exact live member under the same lock
// as configuration writes, and persists tokens/private fields in one row write.
// The caller owns the provider lifecycle lock and checks authorization before
// sending; an already-issued rotation is saved without a post-response gate.
func (s *Store) ReplaceNativeOAuthCredential(id int64, credentialID, expectedIdentity string, oauth OAuthCredential, reauthorize bool) (Channel, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	channel, err := s.channel(id)
	if err != nil {
		return Channel{}, err
	}
	index := credentialIndex(channel, credentialID)
	if index < 0 || expectedIdentity == "" || NativeOAuthIdentity(channel.CredentialType, channel.Credentials[index].OAuth) != expectedIdentity {
		return Channel{}, ErrNativeOAuthIdentity
	}
	previous := *channel.Credentials[index].OAuth
	if err := ValidateNativeOAuthCredential(channel.CredentialType, oauth); err != nil {
		return Channel{}, err
	}
	if reauthorize {
		if NativeOAuthAccountIdentity(channel.CredentialType, previous) != NativeOAuthAccountIdentity(channel.CredentialType, oauth) {
			return Channel{}, ErrNativeOAuthIdentity
		}
		oauth.Status, oauth.StatusReason = OAuthStatusActive, ""
	} else if nativeOAuthDomainIdentity(channel.CredentialType, previous) != nativeOAuthDomainIdentity(channel.CredentialType, oauth) {
		return Channel{}, ErrNativeOAuthIdentity
	}
	oauth.UpdatedAt = time.Now().UTC()
	channel.Credentials[index].OAuth = &oauth
	channel.normalize()
	if nativeOAuthDomainIdentity(channel.CredentialType, previous) != nativeOAuthDomainIdentity(channel.CredentialType, oauth) {
		channel.Generation++
	}
	if err = s.persistRow(&channel); err != nil {
		if err = s.persistRow(&channel); err != nil {
			return Channel{}, fmt.Errorf("已取得的新令牌未能保存，请检查本地存储: %w", err)
		}
	}
	return channel, nil
}

func validateNativeChannel(channel Channel) error {
	format := ""
	switch channel.CredentialType {
	case CredentialClaudeOAuth:
		format = FormatAnthropicMessages
	case CredentialAntigravityOAuth:
		format = FormatGeminiGenerateContent
	case CredentialDevinOAuth:
		format = FormatOpenAIResponses
	case CredentialZenFree:
		if channel.ModelFormats == nil {
			return errors.New("Zen Free 需要逐模型协议目录")
		}
	default:
		return errors.New("不支持的原生接入")
	}

	for _, credential := range channel.Credentials {
		if channel.CredentialType == CredentialZenFree {
			if credential.APIKey != "public" || credential.OAuth != nil {
				return errors.New("Zen Free 仅使用公共凭证 public")
			}
		} else {
			if credential.APIKey != "" || credential.OAuth == nil {
				return errors.New("原生 OAuth 接入需要独立授权凭证")
			}
			if err := ValidateNativeOAuthCredential(channel.CredentialType, *credential.OAuth); err != nil {
				return err
			}
		}
	}
	for _, endpoint := range channel.Endpoints {
		if format != "" && endpoint.Format != format || channel.CredentialType == CredentialZenFree && endpoint.Format == FormatGeminiGenerateContent {
			return errors.New("端点协议不符合该接入的能力声明")
		}
		if !OAuthHostAllowed(channel.CredentialType, endpoint.BaseURL) || !strings.HasPrefix(endpoint.BaseURL, "https://") && !EndpointRelaxedForDiagnostics(endpoint.BaseURL) {
			return errors.New("原生接入端点必须使用已核实提供方的 HTTPS 地址")
		}
	}
	return nil
}
