package console

import (
	"context"
	"errors"
	"strings"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/compatprofile"
	"torouter/internal/pluginhost"
	"torouter/internal/store"
)

type compatibleConsentResult struct {
	oauth store.OAuthCredential
	err   error
}
type oauthContinuationKey struct{}

func startPayloadCompatible(f *oauthFlow) map[string]any {
	out := map[string]any{"flow": f.id, "method": "browser", "authorization_url": f.compatibleAuth.URL,
		"expires": f.expiresAt, "interval": 2, "instructions": "在浏览器完成授权后，此处会自动确认。"}
	if f.compatibleAuth.UserCode != "" {
		out["method"] = "device"
		out["user_code"], out["verification_uri"] = f.compatibleAuth.UserCode, f.compatibleAuth.URL
	}
	return out
}

func (o *oauthTools) startCompatibleCore(ctx context.Context, mode string, channelID int64, credentialID string) (any, error) {
	provider := o.compatible.Profile.CredentialType
	target := store.PluginConsentTarget{}
	switch strings.TrimSpace(mode) {
	case "", "add", "browser":
		if channelID != 0 || credentialID != "" {
			return nil, &ManagementToolError{Status: 400, Message: "新增授权不接受已有渠道或凭证。"}
		}
	case "reauth", "add_credential":
		channel, err := o.store.Channel(channelID)
		if err != nil || channel.CredentialType != provider {
			return nil, &ManagementToolError{Status: 400, Message: "所选渠道接入方式不匹配。"}
		}
		target.ChannelID = channelID
		if mode == "add_credential" {
			if credentialID != "" {
				return nil, &ManagementToolError{Status: 400, Message: "新增凭证不能指定已有凭证。"}
			}
			target.Version = store.CatalogueSourceVersion(channel)
		} else {
			if credentialID == "" {
				if len(channel.Credentials) != 1 {
					return nil, &ManagementToolError{Status: 400, Message: "请选择需要重新授权的具体凭证。"}
				}
				credentialID = channel.Credentials[0].ID
			}
			exact, err := exactOAuthCredential(channel, credentialID)
			if err != nil {
				return nil, err
			}
			target.CredentialID, target.Version = credentialID, store.CompatibleOAuthIdentity(provider, exact.OAuth)
		}
	default:
		return nil, &ManagementToolError{Status: 400, Message: "此兼容接入只支持独立浏览器授权。"}
	}
	defer o.startLock(provider)()
	o.book().mu.Lock()
	if _, f, ok := o.pendingFlowFor(provider, channelID, ""); ok {
		o.book().mu.Unlock()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.compatibleAuth == nil || f.compatibleTarget != target {
			return nil, &ManagementToolError{Status: 409, Message: "已有其他授权进行中，请先完成或取消。"}
		}
		if err := checkOAuthFlowAccess(ctx, f.id); err != nil {
			return nil, err
		}
		return startPayloadCompatible(f), nil
	}
	o.book().mu.Unlock()
	auth, err := o.compatible.Start(ctx)
	if err != nil {
		return nil, err
	}
	id, err := newFlowID()
	if err != nil {
		auth.Close()
		return nil, err
	}
	o.book().mu.Lock()
	defer o.book().mu.Unlock()
	o.prune()
	if len(o.book().flows) >= maxOpenFlows {
		auth.Close()
		return nil, &ManagementToolError{Status: 429, Message: "等待中的授权过多，请先完成或取消。"}
	}
	if err = accessguard.Check(ctx); err != nil {
		auth.Close()
		return nil, err
	}
	f := &oauthFlow{id: id, provider: provider, pluginEpoch: o.epoch, compatibleAuth: auth, compatibleTarget: target,
		expectedChannelID: channelID, state: flowPending, expiresAt: time.Now().Add(browserFlowWindow), interval: 2 * time.Second}
	o.book().flows[id] = f
	return startPayloadCompatible(f), nil
}

func (o *oauthTools) statusCompatibleCore(ctx context.Context, id, callback string) (any, error) {
	if callback != "" {
		return nil, &ManagementToolError{Status: 400, Message: "此授权会自动确认，无需粘贴回调。"}
	}
	o.book().mu.Lock()
	f := o.book().flows[id]
	o.book().mu.Unlock()
	if f == nil || f.compatibleAuth == nil || f.provider != o.compatible.Profile.CredentialType {
		return nil, &ManagementToolError{Status: 404, Message: "授权流程不存在或已过期。"}
	}
	if err := checkOAuthFlowAccess(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if flowAccessRevoked(ctx, f) || f.state != flowPending {
		return flowPayload(f), nil
	}
	if time.Now().After(f.expiresAt) {
		f.state, f.errCode, f.errMessage = flowError, "expired_token", "授权已过期，请重新开始。"
		f.finish()
		return flowPayload(f), nil
	}
	if f.compatibleResult == nil {
		// Only the exchange polls in the background. It never commits an account.
		// MCP supplies a fresh permission check, not a cancelled HTTP context.
		background, cancel := context.WithDeadline(context.Background(), f.expiresAt)
		actor, _ := ctx.Value(oauthContinuationKey{}).(func(context.Context) error)
		background = accessguard.With(background, func() error {
			if actor != nil {
				if err := actor(context.Background()); err != nil {
					return err
				}
			}
			if o.plugins != nil {
				return o.plugins.CheckEpoch(f.provider, f.pluginEpoch)
			}
			return nil
		})
		results := make(chan compatibleConsentResult, 1)
		f.compatibleResult, f.compatibleCancel = results, cancel
		auth := f.compatibleAuth
		go func() {
			next, err := o.compatible.Exchange(background, auth)
			results <- compatibleConsentResult{next, err}
		}()
	}
	select {
	case result := <-f.compatibleResult:
		if result.err == nil {
			channel, linked, err := o.store.CommitPluginConsent(f.provider, result.oauth, f.compatibleTarget, func() error { return accessguard.Check(ctx) })
			if err == nil {
				f.state, f.channelID, f.alreadyLinked = flowComplete, channel.ID, linked
				f.errMessage = "授权完成，可获取并选择启用模型。"
			} else {
				result.err = err
			}
		}
		if result.err != nil {
			f.state, f.errCode, f.errMessage = flowError, "authorization_failed", "授权未完成，请确认所选地区与账号并重试。"
			var remote *pluginhost.RemoteError
			if errors.As(result.err, &remote) {
				// The private worker has already redacted every state secret.
				f.errMessage = "提供方未完成授权：" + Redact(firstLine(remote.Message))
			}
			if errors.Is(result.err, compatprofile.ErrState) {
				f.errMessage = "提供方未返回完整的受支持凭证，请检查账号地区或更新兼容层。"
			}
			if errors.Is(result.err, store.ErrEditConflict) {
				f.errMessage = "所选凭证或渠道已变化，请重新读取后授权。"
			}
			if errors.Is(result.err, store.ErrNativeOAuthIdentity) {
				f.errMessage = "返回账号与所选凭证不一致，或存在重复归属；请使用原账号重新授权，其他账号请通过添加凭证接入。"
			}
			recordOAuthOwnerConflict(f, result.err)
		}
		f.finish()
	default:
	}
	return flowPayload(f), nil
}
