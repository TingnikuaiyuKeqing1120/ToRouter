package console

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

func validateNativeCredentialDraft(st *store.Store, channel store.Channel) error {
	hasMaterial := func(o *store.OAuthCredential) bool {
		return o != nil && (o.AccessToken != "" || o.RefreshToken != "" || o.IDToken != "" || o.GitHubToken != "" || o.DeviceID != "" || o.OrganizationID != "" || o.ProjectID != "" || o.ClientID != "" || o.PluginState != "")
	}
	if store.RequiresIndependentOAuth(channel.CredentialType) && hasMaterial(channel.OAuth) {
		return errors.New("此接入的凭证只能通过独立授权写入。")
	}
	for _, credential := range channel.Credentials {
		if store.RequiresIndependentOAuth(channel.CredentialType) && (hasMaterial(credential.OAuth) || credential.SourceChannelID > 0) {
			return errors.New("请通过独立授权添加凭证，不复制其它渠道的令牌。")
		}
		if credential.SourceChannelID > 0 {
			source, err := st.Channel(credential.SourceChannelID)
			if err == nil && store.RequiresIndependentOAuth(source.CredentialType) {
				return errors.New("此接入不支持复制令牌，请独立授权。")
			}
		}
	}
	return nil
}

func startPayloadNative(f *oauthFlow) map[string]any {
	redirect, _ := url.Parse(f.nativeAuth.RedirectURI)
	return map[string]any{
		"flow": f.id, "method": "browser", "authorization_url": f.nativeAuth.URL,
		"redirect_uri":  f.nativeAuth.RedirectURI,
		"callback_path": redirect.Path,
		"paste_hint":    "浏览器无法自动跳转时，把地址栏里的完整回调链接粘贴回控制台。",
	}
}

func (o *oauthTools) startNativeCore(ctx context.Context, mode string, channelID int64, credentialID string) (any, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	definition := o.native.Definition()
	identity := ""
	target := nativeoauth.ConsentTarget{}
	poolVersion := ""
	switch strings.TrimSpace(mode) {
	case "", "browser", "add":
		if channelID != 0 || credentialID != "" {
			return nil, &ManagementToolError{Status: 400, Message: "新增授权不接受已有渠道ID。"}
		}
	case "reauth", "add_credential":
		channel, err := o.store.Channel(channelID)
		if err != nil {
			return nil, &ManagementToolError{Status: 404, Message: "重新授权的渠道不存在。"}
		}
		if channel.CredentialType != definition.CredentialType {
			return nil, &ManagementToolError{Status: 400, Message: "渠道接入方式与授权方式不同。"}
		}
		if mode == "add_credential" {
			if credentialID != "" {
				return nil, &ManagementToolError{Status: 400, Message: "新增授权不能引用已有凭证ID。"}
			}
			poolVersion = store.CatalogueSourceVersion(channel)
		} else {
			if credentialID == "" {
				credentialID = channel.PrimaryCredential().ID
			}
			channel, err = exactOAuthCredential(channel, credentialID)
			if err != nil {
				return nil, err
			}
			identity, err = o.native.Identity(channel)
			if err != nil {
				return nil, err
			}
			target = nativeoauth.ConsentTarget{ChannelID: channelID, CredentialID: credentialID, Identity: identity, Generation: channel.Generation}
		}
	default:
		return nil, &ManagementToolError{Status: 400, Message: "该接入只支持浏览器独立授权。"}
	}
	defer o.startLock(definition.CredentialType)()
	o.book().mu.Lock()
	if _, flow, ok := o.pendingFlowFor(definition.CredentialType, channelID, ""); ok {
		o.book().mu.Unlock()
		if flow.expectedSubject != identity || flow.nativePoolVersion != poolVersion || flow.nativeTarget != target {
			return nil, &ManagementToolError{Status: 409, Message: "该渠道的另一个授权仍在进行，请先完成或取消。"}
		}
		if err := checkOAuthFlowAccess(ctx, flow.id); err != nil {
			return nil, err
		}
		if err := accessguard.Check(o.flowGuardContext(ctx, flow)); err != nil {
			return nil, err
		}
		return startPayloadNative(flow), nil
	}
	o.book().mu.Unlock()
	listener, err := oauthbase.ListenCallbackOnPath(definition.CallbackHost, definition.CallbackPath, definition.CallbackPorts...)
	if err != nil {
		return nil, &oauthCallbackBindError{&ManagementToolError{Status: 502, Message: "无法监听授权回调：" + err.Error()}}
	}
	authorization, err := o.native.Start(ctx, listener.RedirectURI)
	if err != nil {
		listener.Close()
		return nil, err
	}
	id, err := newFlowID()
	if err != nil {
		listener.Close()
		return nil, err
	}
	o.book().mu.Lock()
	defer o.book().mu.Unlock()
	o.prune()
	if len(o.book().flows) >= maxOpenFlows {
		listener.Close()
		return nil, &ManagementToolError{Status: 429, Message: "等待中的授权过多，请先完成或取消已有授权。"}
	}
	if err := accessguard.Check(ctx); err != nil {
		listener.Close()
		return nil, err
	}
	flow := &oauthFlow{
		id: id, provider: definition.CredentialType, pluginEpoch: o.epoch,
		nativeAuth: &authorization, listener: listener, state: flowPending,
		expiresAt: time.Now().Add(browserFlowWindow), interval: time.Second,
		expectedChannelID: channelID, expectedSubject: identity,
		nativePoolVersion: poolVersion,
		nativeTarget:      target,
	}
	o.book().flows[id] = flow
	return startPayloadNative(flow), nil
}

func (o *oauthTools) statusNativeCore(ctx context.Context, id, pasted string) (any, error) {
	definition := o.native.Definition()
	o.book().mu.Lock()
	flow := o.book().flows[id]
	o.book().mu.Unlock()
	if flow == nil || flow.nativeAuth == nil || flow.provider != definition.CredentialType {
		return nil, &ManagementToolError{Status: 404, Message: "授权流程不存在或已过期，请重新开始。"}
	}
	if err := checkOAuthFlowAccess(ctx, id); err != nil {
		return nil, err
	}
	flow.mu.Lock()
	defer flow.mu.Unlock()
	ctx = o.flowGuardContext(ctx, flow)
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if flow.state != flowPending {
		return flowPayload(flow), nil
	}
	if time.Now().After(flow.expiresAt) {
		flow.state, flow.errMessage = flowError, "授权流程已过期，请重新开始。"
		flow.finish()
		return flowPayload(flow), nil
	}
	var callback oauthbase.CallbackResult
	if strings.TrimSpace(pasted) != "" {
		parsed, err := oauthbase.ParseCallbackURLOnPath(pasted, definition.CallbackHost, definition.CallbackPath)
		if err != nil {
			return nil, &ManagementToolError{Status: 400, Message: "请粘贴本次授权的完整回调链接。"}
		}
		callback = parsed
	} else if !flow.delivered {
		select {
		case callback = <-flow.listener.Result():
			flow.delivered = true
		default:
			return flowPayload(flow), nil
		}
	} else {
		return flowPayload(flow), nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, flowCallLimit)
	defer cancel()
	var channel store.Channel
	var linked bool
	var err error
	if flow.nativePoolVersion != "" {
		channel, linked, err = o.native.CompleteIntoPool(requestCtx, *flow.nativeAuth, callback, flow.expectedChannelID, flow.nativePoolVersion)
	} else {
		channel, linked, err = o.native.Complete(requestCtx, *flow.nativeAuth, callback, flow.nativeTarget)
	}
	if err != nil {
		if accessguard.Denied(err) {
			return nil, err
		}
		flow.state, flow.errCode = flowError, "authorization_failed"
		flow.errMessage = Redact(firstLine(err.Error(), "授权未完成，请重新开始。"), callback.Code, pasted)
		if errors.Is(err, store.ErrNativeOAuthIdentity) {
			flow.errMessage = "授权账号与所选凭证不同，或凭证已变化；请重新读取后授权。"
		}
		recordOAuthOwnerConflict(flow, err)
	} else {
		flow.state, flow.channelID, flow.alreadyLinked = flowComplete, channel.ID, linked
		flow.errMessage = "授权已完成，可在渠道中获取并选择启用模型。"
	}
	flow.finish()
	return flowPayload(flow), nil
}
