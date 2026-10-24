package nativeoauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

var ErrCredentialUnavailable = errors.New("原生授权凭证不可用，请重新读取渠道")

// Service owns one native provider's lifecycle. The plugin manager caches this
// instance, so inference, management, reauthorization and deletion share locks.
type Service struct {
	Store      *store.Store
	driver     Driver
	definition Definition
	mu         sync.Mutex
	locks      map[int64]*sync.Mutex
	completeMu sync.Mutex
}

func NewService(st *store.Store, driver Driver) *Service {
	s := &Service{Store: st, driver: driver, locks: map[int64]*sync.Mutex{}}
	if driver != nil {
		s.definition = driver.Definition()
		s.definition.Endpoints = append([]store.Endpoint(nil), s.definition.Endpoints...)
		s.definition.CallbackPorts = append([]int(nil), s.definition.CallbackPorts...)
	}
	return s
}

func (s *Service) Definition() Definition {
	d := s.definition
	d.Endpoints = append([]store.Endpoint(nil), d.Endpoints...)
	d.CallbackPorts = append([]int(nil), d.CallbackPorts...)
	return d
}

func (s *Service) ready() error {
	if s.Store == nil || s.driver == nil || !store.IsNativeOAuth(s.definition.CredentialType) || len(s.definition.Endpoints) == 0 {
		return ErrCredentialUnavailable
	}
	return nil
}

func (s *Service) lockFor(id int64) *sync.Mutex { return s.Store.CredentialLifecycleMutex(id) }

func checkAccess(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return accessguard.Check(ctx)
}

func (s *Service) selected(channel store.Channel) (store.CredentialItem, error) {
	if channel.CredentialType != s.definition.CredentialType || channel.ID <= 0 || len(channel.Credentials) != 1 || channel.Credentials[0].ID == "" || channel.Credentials[0].OAuth == nil {
		return store.CredentialItem{}, ErrCredentialUnavailable
	}
	return channel.Credentials[0], nil
}

// Identity is the snapshot a reauthorization flow captures for one exact
// selected member. Complete rejects an ambiguous match in a multi-member pool.
func (s *Service) Identity(channel store.Channel) (string, error) {
	credential, err := s.selected(channel)
	if err != nil {
		return "", err
	}
	return store.NativeOAuthIdentity(channel.CredentialType, credential.OAuth), nil
}

func (s *Service) liveCredential(ctx context.Context, selected store.Channel) (store.Channel, store.CredentialItem, error) {
	if err := checkAccess(ctx); err != nil {
		return store.Channel{}, store.CredentialItem{}, err
	}
	selectedCredential, err := s.selected(selected)
	if err != nil {
		return store.Channel{}, store.CredentialItem{}, err
	}
	live, err := s.Store.Channel(selected.ID)
	if err != nil {
		return store.Channel{}, store.CredentialItem{}, err
	}
	if live.CredentialType != s.definition.CredentialType {
		return store.Channel{}, store.CredentialItem{}, store.ErrNativeOAuthIdentity
	}
	for _, credential := range live.Credentials {
		if credential.ID != selectedCredential.ID || credential.OAuth == nil {
			continue
		}
		if store.NativeOAuthIdentity(live.CredentialType, credential.OAuth) != store.NativeOAuthIdentity(selected.CredentialType, selectedCredential.OAuth) {
			return store.Channel{}, store.CredentialItem{}, store.ErrNativeOAuthIdentity
		}
		if err := store.ValidateNativeOAuthCredential(live.CredentialType, *credential.OAuth); err != nil {
			return store.Channel{}, store.CredentialItem{}, err
		}
		if credential.OAuth.Status != "" && credential.OAuth.Status != store.OAuthStatusActive {
			return store.Channel{}, store.CredentialItem{}, errors.New("授权需要重新登录")
		}
		return live, credential, nil
	}
	return store.Channel{}, store.CredentialItem{}, store.ErrNativeOAuthIdentity
}

func mergeCredential(previous, next store.OAuthCredential) store.OAuthCredential {
	result := previous
	for _, field := range []struct {
		dst   *string
		value string
	}{
		{&result.Provider, next.Provider}, {&result.AccessToken, next.AccessToken}, {&result.RefreshToken, next.RefreshToken},
		{&result.IDToken, next.IDToken}, {&result.ClientID, next.ClientID}, {&result.HostID, next.HostID},
		{&result.Account, next.Account}, {&result.Subject, next.Subject}, {&result.Scopes, next.Scopes},
		{&result.OrganizationID, next.OrganizationID}, {&result.DeviceID, next.DeviceID}, {&result.ProjectID, next.ProjectID}, {&result.APIBase, next.APIBase},
	} {
		if field.value != "" {
			*field.dst = field.value
		}
	}
	if !next.ExpiresAt.IsZero() {
		result.ExpiresAt = next.ExpiresAt
	}
	return result
}

// credentialLocked refreshes at most once after rereading the exact live pool
// member. The token response is persisted without a post-response access check.
func (s *Service) credentialLocked(ctx context.Context, selected store.Channel) (store.OAuthCredential, error) {
	live, credential, err := s.liveCredential(ctx, selected)
	if err != nil {
		return store.OAuthCredential{}, err
	}
	oauth := *credential.OAuth
	if oauth.RefreshToken == "" || !oauth.ExpiresAt.IsZero() && time.Until(oauth.ExpiresAt) > time.Minute {
		return oauth, nil
	}
	if err := checkAccess(ctx); err != nil {
		return store.OAuthCredential{}, err
	}
	next, err := s.driver.Refresh(ctx, oauth)
	if err != nil {
		return store.OAuthCredential{}, err
	}
	if next.AccessToken == "" {
		return store.OAuthCredential{}, errors.New("提供方未返回新的访问令牌")
	}
	next = mergeCredential(oauth, next)
	updated, err := s.Store.ReplaceNativeOAuthCredential(live.ID, credential.ID, store.NativeOAuthIdentity(live.CredentialType, &oauth), next, false)
	if err != nil {
		return store.OAuthCredential{}, err
	}
	for _, item := range updated.Credentials {
		if item.ID == credential.ID && item.OAuth != nil {
			return *item.OAuth, nil
		}
	}
	return store.OAuthCredential{}, ErrCredentialUnavailable
}

func (s *Service) Bearer(ctx context.Context, channel store.Channel) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	lock := s.lockFor(channel.ID)
	lock.Lock()
	defer lock.Unlock()
	oauth, err := s.credentialLocked(ctx, channel)
	return oauth.AccessToken, err
}

func (s *Service) Models(ctx context.Context, channel store.Channel) ([]Model, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	lock := s.lockFor(channel.ID)
	lock.Lock()
	defer lock.Unlock()
	oauth, err := s.credentialLocked(ctx, channel)
	if err != nil {
		return nil, err
	}
	if err := checkAccess(ctx); err != nil {
		return nil, err
	}
	return s.driver.Models(ctx, oauth)
}

func (s *Service) Execute(client *http.Client, req *http.Request, channel store.Channel) (*http.Response, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if req == nil || req.URL == nil || !store.OAuthHostAllowed(s.definition.CredentialType, req.URL.String()) || req.URL.Scheme != "https" && !store.EndpointRelaxedForDiagnostics(req.URL.String()) {
		return nil, errors.New("原生接入请求目标无效")
	}
	lock := s.lockFor(channel.ID)
	lock.Lock()
	oauth, err := s.credentialLocked(req.Context(), channel)
	lock.Unlock()
	if err != nil {
		return nil, err
	}
	if err := checkAccess(req.Context()); err != nil {
		return nil, err
	}
	return s.driver.Execute(client, req, oauth)
}

func (s *Service) Start(ctx context.Context, redirect string) (Authorization, error) {
	if err := s.ready(); err != nil {
		return Authorization{}, err
	}
	if err := checkAccess(ctx); err != nil {
		return Authorization{}, err
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Hostname() != s.definition.CallbackHost || u.Path != s.definition.CallbackPath || u.RawQuery != "" || u.Fragment != "" {
		return Authorization{}, errors.New("授权回调地址不符合该接入的固定声明")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return Authorization{}, errors.New("授权回调端口无效")
	}
	if len(s.definition.CallbackPorts) > 0 {
		allowed := false
		for _, registered := range s.definition.CallbackPorts {
			allowed = allowed || port == registered
		}
		if !allowed {
			return Authorization{}, errors.New("授权回调端口未注册")
		}
	}
	return s.driver.Begin(ctx, redirect)
}

func (s *Service) matchingCredential(channel store.Channel, target ConsentTarget) (store.CredentialItem, error) {
	var matches []store.CredentialItem
	if channel.CredentialType == s.definition.CredentialType && target.Identity != "" && target.CredentialID != "" && target.Generation == channel.Generation {
		for _, credential := range channel.Credentials {
			if credential.ID == target.CredentialID && credential.OAuth != nil && store.NativeOAuthIdentity(channel.CredentialType, credential.OAuth) == target.Identity {
				matches = append(matches, credential)
			}
		}
	}
	if len(matches) != 1 {
		return store.CredentialItem{}, store.ErrNativeOAuthIdentity
	}
	return matches[0], nil
}

func (s *Service) replaceAuthorization(channel store.Channel, credential store.CredentialItem, oauth store.OAuthCredential) (store.Channel, bool, error) {
	if err := store.ValidateNativeOAuthCredential(channel.CredentialType, oauth); err != nil {
		return store.Channel{}, false, err
	}
	if store.NativeOAuthAccountIdentity(channel.CredentialType, *credential.OAuth) != store.NativeOAuthAccountIdentity(channel.CredentialType, oauth) {
		return store.Channel{}, false, store.ErrNativeOAuthIdentity
	}
	if channel.CredentialType == store.CredentialClaudeOAuth && credential.OAuth.DeviceID != "" {
		// A fresh consent flow has no existing-account context when it mints
		// a device ID. Keep this account's established local device identity.
		oauth.DeviceID = credential.OAuth.DeviceID
	}
	oauth = mergeCredential(*credential.OAuth, oauth)
	updated, err := s.Store.ReplaceNativeOAuthCredential(channel.ID, credential.ID, store.NativeOAuthIdentity(channel.CredentialType, credential.OAuth), oauth, true)
	return updated, true, err
}

func (s *Service) Complete(ctx context.Context, authorization Authorization, callback oauthbase.CallbackResult, target ConsentTarget) (store.Channel, bool, error) {
	return s.complete(ctx, authorization, callback, target, 0, "")
}

// CompleteIntoPool attaches a newly authorized identity directly to its owner.
// It never creates a temporary channel and copies a rotating token out of it.
func (s *Service) CompleteIntoPool(ctx context.Context, authorization Authorization, callback oauthbase.CallbackResult, channelID int64, expected string) (store.Channel, bool, error) {
	if channelID < 1 || expected == "" {
		return store.Channel{}, false, store.ErrEditConflict
	}
	return s.complete(ctx, authorization, callback, ConsentTarget{}, channelID, expected)
}

func (s *Service) complete(ctx context.Context, authorization Authorization, callback oauthbase.CallbackResult, target ConsentTarget, poolID int64, poolVersion string) (store.Channel, bool, error) {
	if err := s.ready(); err != nil {
		return store.Channel{}, false, err
	}
	if authorization.State == "" || callback.State != authorization.State || callback.Code == "" || callback.Error != "" || target.ChannelID < 0 {
		return store.Channel{}, false, errors.New("授权回调无效或未获同意，请重新开始")
	}
	s.completeMu.Lock()
	defer s.completeMu.Unlock()
	if poolID > 0 {
		target, err := s.Store.Channel(poolID)
		if err != nil {
			return store.Channel{}, false, err
		}
		if target.CredentialType != s.definition.CredentialType || store.CatalogueSourceVersion(target) != poolVersion {
			return store.Channel{}, false, store.ErrEditConflict
		}
	}
	if target.ChannelID > 0 {
		lock := s.lockFor(target.ChannelID)
		lock.Lock()
		defer lock.Unlock()
		channel, err := s.Store.Channel(target.ChannelID)
		if err != nil {
			return store.Channel{}, false, err
		}
		credential, err := s.matchingCredential(channel, target)
		if err != nil {
			return store.Channel{}, false, err
		}
		if err := checkAccess(ctx); err != nil {
			return store.Channel{}, false, err
		}
		oauth, err := s.driver.Exchange(ctx, authorization, callback)
		if err != nil {
			return store.Channel{}, false, err
		}
		if err := checkAccess(ctx); err != nil {
			return store.Channel{}, false, err
		}
		return s.replaceAuthorization(channel, credential, oauth)
	}
	if err := checkAccess(ctx); err != nil {
		return store.Channel{}, false, err
	}
	oauth, err := s.driver.Exchange(ctx, authorization, callback)
	if err != nil {
		return store.Channel{}, false, err
	}
	if err := store.ValidateNativeOAuthCredential(s.definition.CredentialType, oauth); err != nil {
		return store.Channel{}, false, err
	}
	identity := store.NativeOAuthAccountIdentity(s.definition.CredentialType, oauth)
	channels, err := s.Store.ListChannels()
	if err != nil {
		return store.Channel{}, false, err
	}
	var matches []struct {
		channel    store.Channel
		credential store.CredentialItem
	}
	for _, channel := range channels {
		if channel.CredentialType != s.definition.CredentialType {
			continue
		}
		for _, credential := range channel.Credentials {
			if credential.OAuth != nil && identity == store.NativeOAuthAccountIdentity(channel.CredentialType, *credential.OAuth) {
				matches = append(matches, struct {
					channel    store.Channel
					credential store.CredentialItem
				}{channel, credential})
			}
		}
	}
	if len(matches) > 1 {
		return store.Channel{}, false, store.ErrNativeOAuthIdentity
	}
	if len(matches) == 1 {
		match := matches[0]
		if poolID > 0 {
			return store.Channel{}, false, &store.OAuthOwnerConflict{ChannelID: match.channel.ID, CredentialID: match.credential.ID}
		}
		lock := s.lockFor(match.channel.ID)
		lock.Lock()
		defer lock.Unlock()
		if err := checkAccess(ctx); err != nil {
			return store.Channel{}, false, err
		}
		// A concurrent deletion/configuration change must never turn this into
		// creation of another channel, or redirect the result to pool member 1.
		return s.replaceAuthorization(match.channel, match.credential, oauth)
	}
	if err := checkAccess(ctx); err != nil {
		return store.Channel{}, false, err
	}
	oauth.AccountLabel = s.definition.Name
	oauth.Status, oauth.StatusReason, oauth.UpdatedAt = store.OAuthStatusActive, "", time.Now().UTC()
	if poolID > 0 {
		lock := s.lockFor(poolID)
		lock.Lock()
		defer lock.Unlock()
		id, err := oauthbase.RandomToken(12)
		if err != nil {
			return store.Channel{}, false, err
		}
		updated, err := s.Store.AppendNativeOAuthCredential(poolID, poolVersion, store.CredentialItem{ID: "cred_" + id, Enabled: true, OAuth: &oauth}, func() error { return checkAccess(ctx) })
		return updated, false, err
	}
	channel := store.Channel{Name: strings.TrimSpace(s.definition.Name), Enabled: true, CredentialType: s.definition.CredentialType,
		Endpoints:   append([]store.Endpoint(nil), s.definition.Endpoints...),
		Credentials: []store.CredentialItem{{ID: "cred_default", Label: "默认凭据", Enabled: true, OAuth: &oauth}},
	}
	if err := s.Store.SaveChannel(&channel); err != nil {
		return store.Channel{}, false, err
	}
	return channel, false, nil
}

func (s *Service) DisconnectChannelAtVersion(ctx context.Context, id int64, expected string) error {
	if err := s.ready(); err != nil {
		return err
	}
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()
	if err := checkAccess(ctx); err != nil {
		return err
	}
	channel, err := s.Store.Channel(id)
	if err != nil {
		return err
	}
	if channel.CredentialType != s.definition.CredentialType {
		return ErrCredentialUnavailable
	}
	return s.Store.DeleteChannelAtVersion(id, expected)
}
