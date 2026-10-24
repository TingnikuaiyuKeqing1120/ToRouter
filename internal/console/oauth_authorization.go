package console

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"torouter/internal/accessguard"
	"torouter/internal/compatprofile"
	"torouter/internal/store"
)

type accountConsentKey struct{}

func consentTarget(ctx context.Context) store.PluginConsentTarget {
	v, _ := ctx.Value(accountConsentKey{}).(store.PluginConsentTarget)
	return v
}

func (o *oauthTools) ManagementAuthorize(ctx context.Context, provider, operation, method string, channelID int64, credentialID string) (any, error) {
	ctype, err := oauthProvider(provider)
	if err != nil {
		return nil, err
	}
	if operation != "add_credential" && operation != "reauth" {
		return nil, &ManagementToolError{Status: 400, Message: "operation 必须为 add_credential 或 reauth"}
	}
	if method == "" {
		if ctype == store.CredentialCopilotOAuth {
			method = "device"
		} else {
			method = "browser"
		}
	}
	profile, isCompatible := compatprofile.Lookup(ctype)
	deviceAllowed := ctype == store.CredentialCopilotOAuth || ctype == store.CredentialCodexOAuth || isCompatible && (profile.Package == "qoder" || profile.Package == "minimax")
	if method != "browser" && method != "device" || ctype == store.CredentialCopilotOAuth && method != "device" || !deviceAllowed && method != "browser" {
		return nil, &ManagementToolError{Status: 400, Message: "该接入不支持所选登录方式"}
	}
	channel, err := o.store.Channel(channelID)
	if err != nil {
		return nil, &ManagementToolError{Status: 404, Message: "请选择已有渠道"}
	}
	if channel.CredentialType != ctype {
		return nil, &ManagementToolError{Status: 400, Message: "渠道接入方式不同"}
	}
	target := store.PluginConsentTarget{ChannelID: channelID, Version: store.CatalogueSourceVersion(channel)}
	if operation == "reauth" {
		if credentialID == "" {
			return nil, &ManagementToolError{Status: 400, Message: "重新授权必须选择具体 credential_id"}
		}
		exact, e := exactOAuthCredential(channel, credentialID)
		if e != nil {
			return nil, e
		}
		target.CredentialID = credentialID
		target.Version = store.AccountOAuthIdentity(ctype, exact.OAuth)
	} else if credentialID != "" {
		return nil, &ManagementToolError{Status: 400, Message: "添加凭证不能指定 credential_id"}
	}
	if ctype == store.CredentialCodexOAuth || ctype == store.CredentialChatGPTOAuth || ctype == store.CredentialCopilotOAuth {
		o.book().mu.Lock()
		for _, f := range o.book().flows {
			f.mu.Lock()
			conflict := f.provider == ctype && f.expectedChannelID == channelID && f.state == flowPending && f.accountTarget != target
			f.mu.Unlock()
			if conflict {
				o.book().mu.Unlock()
				return nil, &ManagementToolError{Status: 409, Message: "该渠道已有另一授权进行中"}
			}
		}
		o.book().mu.Unlock()
		ctx = context.WithValue(ctx, accountConsentKey{}, target)
		view, next, e := o.nativeView(ctx, ctype)
		if e != nil {
			return nil, e
		}
		o, ctx = view, next
		switch ctype {
		case store.CredentialCopilotOAuth:
			if o.copilot != nil {
				return o.startCopilotCore(ctx)
			}
		case store.CredentialChatGPTOAuth:
			if o.siwc != nil {
				return o.startChatGPTCore(ctx, operation, channelID)
			}
		case store.CredentialCodexOAuth:
			if o.codex != nil {
				return o.startCodexCore(ctx, method, channelID)
			}
		}
		return nil, &ManagementToolError{Status: 503, Message: "授权服务未就绪"}
	}
	return o.ManagementStartCredential(ctx, provider, operation, channelID, credentialID)
}

func (o *oauthTools) startAuthorizationHTTP(w http.ResponseWriter, r *http.Request, provider string) {
	q := r.URL.Query()
	if q.Has("mode") {
		fail(w, 400, "请使用 operation 和 method 参数")
		return
	}
	id, _ := strconv.ParseInt(q.Get("channel_id"), 10, 64)
	out, err := o.ManagementAuthorize(r.Context(), provider, q.Get("operation"), q.Get("method"), id, q.Get("credential_id"))
	respondManagementTool(w, out, err)
}

func (o *oauthTools) commitAccountFlow(ctx context.Context, f *oauthFlow, oauth store.OAuthCredential) bool {
	target := f.accountTarget
	if target.ChannelID == 0 {
		return false
	}
	lock := o.store.CredentialLifecycleMutex(target.ChannelID)
	lock.Lock()
	defer lock.Unlock()
	channel, _, err := o.store.CommitAccountConsent(f.provider, oauth, target, func() error { return accessguard.Check(ctx) })
	if err != nil {
		f.state, f.errCode, f.errMessage = flowError, "credential_rejected", err.Error()
		if errors.Is(err, store.ErrNotFound) {
			f.errCode, f.errMessage = "channel_missing", "授权目标渠道或凭证已不存在，请刷新后重试。"
		} else if errors.Is(err, store.ErrNativeOAuthIdentity) {
			f.errCode, f.errMessage = "identity_mismatch", "授权签出的账号与当前凭证绑定身份不一致，已拒绝。"
		}
		recordOAuthOwnerConflict(f, err)
	} else {
		f.state, f.channelID = flowComplete, channel.ID
	}
	f.finish()
	return true
}

func recordOAuthOwnerConflict(f *oauthFlow, err error) {
	var conflict *store.OAuthOwnerConflict
	if errors.As(err, &conflict) {
		f.existingChannelID = conflict.ChannelID
		f.existingCredentialID = conflict.CredentialID
		f.errCode = "already_linked"
		f.errMessage = conflict.Error()
	}
}
