package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/compatprofile"
	"torouter/internal/copilot"
	"torouter/internal/marketbridge"
	"torouter/internal/oauthbase"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

// ManagementToolError preserves the existing console response at the HTTP edge.
type ManagementToolError struct {
	Status  int
	Message string
}

func (e *ManagementToolError) Error() string { return e.Message }

// Only callback listener failures carry this marker: their diagnostics are
// controlled locally and no authorization flow or upstream action has begun.
type oauthCallbackBindError struct{ *ManagementToolError }

func (e *oauthCallbackBindError) Unwrap() error { return e.ManagementToolError }

func respondManagementTool(w http.ResponseWriter, out any, err error) {
	if err != nil {
		if pluginError(w, err) {
			return
		}
		var public *ManagementToolError
		if errors.As(err, &public) {
			fail(w, public.Status, public.Message)
		} else {
			fail(w, 500, err.Error())
		}
		return
	}
	respond(w, 200, out)
}

type oauthFlowAccessKey struct{}

// WithOAuthFlowAccess checks existing flow handles before reuse, polling or
// cancellation. The MCP adapter claims only newly created flows and supplies
// its principal ownership check here; ordinary Web requests retain their flow.
func WithOAuthFlowAccess(ctx context.Context, authorize func(string) error) context.Context {
	return context.WithValue(ctx, oauthFlowAccessKey{}, authorize)
}
func checkOAuthFlowAccess(ctx context.Context, flow string) error {
	if authorize, ok := ctx.Value(oauthFlowAccessKey{}).(func(string) error); ok {
		return authorize(flow)
	}
	return nil
}
func oauthProvider(provider string) (string, error) {
	provider = strings.TrimSpace(provider)
	for _, p := range compatprofile.Profiles() {
		if provider == p.ID || provider == p.CredentialType {
			return p.CredentialType, nil
		}
	}
	switch strings.TrimSpace(provider) {
	case "copilot", store.CredentialCopilotOAuth:
		return store.CredentialCopilotOAuth, nil
	case "chatgpt", store.CredentialChatGPTOAuth:
		return store.CredentialChatGPTOAuth, nil
	case "codex", store.CredentialCodexOAuth:
		return store.CredentialCodexOAuth, nil
	case "claude", store.CredentialClaudeOAuth:
		return store.CredentialClaudeOAuth, nil
	case "antigravity", store.CredentialAntigravityOAuth:
		return store.CredentialAntigravityOAuth, nil
	case "devin", store.CredentialDevinOAuth:
		return store.CredentialDevinOAuth, nil
	default:
		return "", &ManagementToolError{Status: 400, Message: "未知的 OAuth 接入方式。"}
	}
}
func (o *oauthTools) ManagementStart(ctx context.Context, provider, mode string, channelID int64) (any, error) {
	return o.ManagementStartCredential(ctx, provider, mode, channelID, "")
}
func (o *oauthTools) ManagementStartCredential(ctx context.Context, provider, mode string, channelID int64, credentialID string) (any, error) {
	provider, err := oauthProvider(provider)
	if err != nil {
		return nil, err
	}
	if credentialID != "" && !store.RequiresIndependentOAuth(provider) {
		return nil, &ManagementToolError{Status: 400, Message: "此接入不支持该精确凭证授权入口。"}
	}
	o, ctx, err = o.nativeView(ctx, provider)
	if err != nil {
		return nil, err
	}
	switch provider {
	case store.CredentialCopilotOAuth:
		if o.copilot != nil {
			return o.startCopilotCore(ctx)
		}
	case store.CredentialChatGPTOAuth:
		if o.siwc != nil {
			return o.startChatGPTCore(ctx, mode, channelID)
		}
	case store.CredentialCodexOAuth:
		if o.codex != nil {
			return o.startCodexCore(ctx, mode, channelID)
		}
	default:
		if o.compatible != nil {
			return o.startCompatibleCore(ctx, mode, channelID, credentialID)
		}
		if o.native != nil {
			return o.startNativeCore(ctx, mode, channelID, credentialID)
		}
	}
	return nil, &ManagementToolError{Status: 503, Message: "此进程没有配置该 OAuth 客户端。"}
}
func (o *oauthTools) ManagementStatus(ctx context.Context, provider, flow string) (any, error) {
	return o.ManagementStatusCallback(ctx, provider, flow, "")
}
func (o *oauthTools) ManagementStatusCallback(ctx context.Context, provider, flow, callback string) (any, error) {
	provider, err := oauthProvider(provider)
	if err != nil {
		return nil, err
	}
	if o.plugins != nil {
		o.book().mu.Lock()
		f := o.book().flows[flow]
		o.book().mu.Unlock()
		if f != nil && f.provider == provider {
			if err = o.plugins.CheckEpoch(provider, f.pluginEpoch); err != nil {
				return nil, err
			}
			ctx = o.flowGuardContext(ctx, f)
		}
	}
	o, ctx, err = o.nativeView(ctx, provider)
	if err != nil {
		return nil, err
	}
	switch provider {
	case store.CredentialCopilotOAuth:
		return o.statusCopilotCore(ctx, flow)
	case store.CredentialChatGPTOAuth:
		return o.statusChatGPTCore(ctx, flow, callback)
	case store.CredentialCodexOAuth:
		return o.statusCodexCore(ctx, flow, callback)
	default:
		if o.compatible != nil {
			return o.statusCompatibleCore(ctx, flow, callback)
		}
		if o.native != nil {
			return o.statusNativeCore(ctx, flow, callback)
		}
		return nil, &ManagementToolError{Status: 503, Message: "此接入尚不可用。"}
	}
}
func (o *oauthTools) ManagementCancel(ctx context.Context, provider, flow string) (any, error) {
	provider, err := oauthProvider(provider)
	if err != nil {
		return nil, err
	}
	return o.cancelCore(ctx, provider, flow)
}
func (o *oauthTools) ManagementModels(ctx context.Context, channelID int64, credentialID string, apply bool) (any, error) {
	return o.modelsCore(ctx, channelID, credentialID, apply)
}
func exactOAuthCredential(channel store.Channel, credentialID string) (store.Channel, error) {
	if credentialID == "" {
		return channel, nil
	}
	for _, credential := range channel.Credentials {
		if credential.ID == credentialID && (credential.OAuth != nil || channel.CredentialType == store.CredentialZenFree && credential.APIKey == "public") {
			channel.Credentials = []store.CredentialItem{credential}
			channel.OAuth = credential.OAuth
			return channel, nil
		}
	}
	return store.Channel{}, &ManagementToolError{Status: 404, Message: "OAuth credential not found"}
}
func (o *oauthTools) startCopilotCore(ctx context.Context) (any, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, flowCallLimit)
	defer cancel()
	// The provider's start lock makes check-and-register atomic for concurrent
	// starts: the loser of a double click finds the winner's flow and spends no
	// upstream call. The pending check happens under it, before the upstream
	// request, and once more under the registration lock.
	defer o.startLock(store.CredentialCopilotOAuth)()
	o.book().mu.Lock()
	if _, f, ok := o.pendingFlowFor(store.CredentialCopilotOAuth, consentTarget(ctx).ChannelID, ""); ok {
		o.book().mu.Unlock()
		if f.accountTarget != consentTarget(ctx) {
			return nil, &ManagementToolError{Status: 409, Message: "该渠道已有另一凭证授权进行中"}
		}
		if err := checkOAuthFlowAccess(ctx, f.id); err != nil {
			return nil, err
		}
		return startPayloadCopilot(f), nil
	}
	o.book().mu.Unlock()
	device, err := copilot.StartDeviceFlow(ctx, o.copilot.Client, o.copilot.Endpoints)
	if err != nil {
		if accessguard.Denied(err) {
			return nil, err
		}
		return nil, &ManagementToolError{Status: 502, Message: "无法启动 GitHub 授权：" + err.Error()}
	}
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	o.book().mu.Lock()
	defer o.book().mu.Unlock()
	o.prune()
	// The same idempotence check under the lock that registers: this start spent
	// a device code, so a concurrent winner is reported instead of registering a
	// second flow for one provider.
	if _, f, ok := o.pendingFlowFor(store.CredentialCopilotOAuth, consentTarget(ctx).ChannelID, ""); ok {
		if f.accountTarget != consentTarget(ctx) {
			return nil, &ManagementToolError{Status: 409, Message: "该渠道已有另一凭证授权进行中"}
		}
		if err := checkOAuthFlowAccess(ctx, f.id); err != nil {
			return nil, err
		}
		return startPayloadCopilot(f), nil
	}
	if len(o.book().flows) >= maxOpenFlows {
		return nil, &ManagementToolError{Status: 429, Message: "等待中的授权过多，请先完成或重新开始已有的授权。"}
	}
	flow := &oauthFlow{
		provider:      store.CredentialCopilotOAuth,
		accountTarget: consentTarget(ctx), expectedChannelID: consentTarget(ctx).ChannelID,
		device:    device,
		interval:  flowInterval(device),
		expiresAt: device.ExpiresAt,
		state:     flowPending,
	}
	flow.id = device.ID
	flow.pluginEpoch = o.epoch
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	o.book().flows[device.ID] = flow
	return startPayloadCopilot(flow), nil
}

func (o *oauthTools) startChatGPTCore(ctx context.Context, mode string, channelID int64) (any, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	// "Adding an account" and "re-authorizing one" are different flows, and the
	// issuer knows the difference: every oaiapp_* registration is bound to one
	// user and workspace, so a new account must start from the registration
	// entry point again rather than reuse another account's client. Reusing a
	// client is only correct for the channel that already owns it.
	clientID := siwc.DynamicAppID
	var expectedChannelID int64
	var expectedSubject string
	switch strings.TrimSpace(mode) {
	case "", "add":
	case "add_credential":
		expectedChannelID = channelID
	case "reauth":
		id := channelID
		if id < 1 {
			return nil, &ManagementToolError{Status: 400, Message: "重新授权需要指定 channel_id。"}
		}
		channel, err := o.siwc.Store.Channel(id)
		if err != nil {
			return nil, &ManagementToolError{Status: 404, Message: "channel not found"}
		}
		if target := consentTarget(ctx); target.CredentialID != "" {
			channel, err = exactOAuthCredential(channel, target.CredentialID)
			if err != nil {
				return nil, err
			}
		}
		if channel.CredentialType != store.CredentialChatGPTOAuth || channel.OAuth == nil || channel.OAuth.ClientID == "" {
			return nil, &ManagementToolError{Status: 400, Message: "该渠道不是 ChatGPT 授权渠道，无法重新授权。"}
		}
		clientID = channel.OAuth.ClientID
		expectedChannelID, expectedSubject = channel.ID, channel.OAuth.Subject
	default:
		return nil, &ManagementToolError{Status: 400, Message: "mode 必须是 add 或 reauth。"}
	}
	// The provider's start lock makes check-and-register atomic for concurrent
	// starts: the loser of a double click finds the winner's flow and never binds
	// a second listener. The pending check happens under it, before anything is
	// bound, and once more under the registration lock.
	defer o.startLock(store.CredentialChatGPTOAuth)()
	o.book().mu.Lock()
	if _, f, ok := o.pendingFlowFor(store.CredentialChatGPTOAuth, expectedChannelID, ""); ok {
		o.book().mu.Unlock()
		if f.accountTarget != consentTarget(ctx) {
			return nil, &ManagementToolError{Status: 409, Message: "该渠道已有另一凭证授权进行中"}
		}
		if err := checkOAuthFlowAccess(ctx, f.id); err != nil {
			return nil, err
		}
		return startPayloadChatGPT(f), nil
	}
	o.book().mu.Unlock()
	hostID, err := o.siwc.Store.HostID()
	if err != nil {
		return nil, &ManagementToolError{Status: 500, Message: "无法生成本机标识：" + err.Error()}
	}
	listener, err := siwc.ListenCallback()
	if err != nil {
		return nil, &oauthCallbackBindError{&ManagementToolError{Status: 502, Message: "无法监听授权回调端口：" + err.Error()}}
	}
	// A re-authorization hints the account it belongs to, so the issuer can
	// preselect it instead of asking again. The hint is optional: the binding to
	// the expected subject is what actually protects the channel.
	hint := siwc.IDTokenHint{}
	if expectedChannelID != 0 {
		if bound, err := o.siwc.Store.Channel(expectedChannelID); err == nil && bound.OAuth != nil {
			if target := consentTarget(ctx); target.CredentialID != "" {
				bound, _ = exactOAuthCredential(bound, target.CredentialID)
			}
			if bound.OAuth == nil {
				return nil, &ManagementToolError{Status: 409, Message: "所选凭证已变化"}
			}
			hint.IDToken = bound.OAuth.IDToken
			// login_hint comes from the stored token's email claim: advisory, so a
			// missing one is not an error.
			hint.LoginHint = siwc.EmailHintOf(bound.OAuth.IDToken)
		}
	}
	auth, err := siwc.AuthorizeURL(o.siwc.Endpoints, clientID, hostID, listener.RedirectURI, hint)
	if err != nil {
		listener.Close()
		return nil, &ManagementToolError{Status: 500, Message: "无法构造授权链接：" + err.Error()}
	}
	o.book().mu.Lock()
	defer o.book().mu.Unlock()
	o.prune()
	// One authorization per provider, re-checked under the lock that registers:
	// an in-flight flow for the same entry point is the one being completed, so
	// it is handed back and the listener this start just bound is released.
	if _, f, ok := o.pendingFlowFor(store.CredentialChatGPTOAuth, expectedChannelID, ""); ok {
		listener.Close()
		if f.accountTarget != consentTarget(ctx) {
			return nil, &ManagementToolError{Status: 409, Message: "该渠道已有另一凭证授权进行中"}
		}
		if err := checkOAuthFlowAccess(ctx, f.id); err != nil {
			return nil, err
		}
		return startPayloadChatGPT(f), nil
	}
	if len(o.book().flows) >= maxOpenFlows {
		listener.Close()
		return nil, &ManagementToolError{Status: 429, Message: "等待中的授权过多，请先完成或重新开始已有的授权。"}
	}
	flowID, err := newFlowID()
	if err != nil {
		listener.Close()
		return nil, &ManagementToolError{Status: 500, Message: "无法生成授权流程标识：" + err.Error()}
	}
	flow := &oauthFlow{
		provider: store.CredentialChatGPTOAuth, accountTarget: consentTarget(ctx),
		auth:     auth,
		listener: listener,
		// An SIWC flow has no upstream expiry to borrow: the window is the
		// console's own bound on how long a half-finished authorization may
		// hold a loopback port.
		expiresAt:         time.Now().Add(browserFlowWindow),
		expectedChannelID: expectedChannelID,
		expectedSubject:   expectedSubject,
		state:             flowPending,
	}
	flow.id = flowID
	flow.pluginEpoch = o.epoch
	if err := accessguard.Check(ctx); err != nil {
		listener.Close()
		return nil, err
	}
	o.book().flows[flowID] = flow
	return startPayloadChatGPT(flow), nil
}

func (o *oauthTools) startCodexCore(ctx context.Context, mode string, channelID int64) (any, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	mode = strings.TrimSpace(mode)
	// A re-authorization names its channel, and the console's editor sends it as
	// mode=reauth on every track: the authorization itself is still the browser
	// one, so "reauth" is the browser arm with a channel bound to it. The
	// binding is loaded and checked here — the channel must exist and be a Codex
	// channel, exactly as the SIWC track checks its own — because completeCodex
	// later refuses an authorization signed by another account, and that refusal
	// needs the channel to still be the one that started the flow.
	expectedChannelID := consentTarget(ctx).ChannelID
	if mode == "reauth" {
		id := channelID
		if id < 1 {
			return nil, &ManagementToolError{Status: 400, Message: "重新授权需要指定 channel_id。"}
		}
		channel, err := o.codex.Store.Channel(id)
		if err != nil {
			return nil, &ManagementToolError{Status: 404, Message: "channel not found"}
		}
		if channel.CredentialType != store.CredentialCodexOAuth || channel.OAuth == nil || channel.OAuth.ClientID == "" {
			return nil, &ManagementToolError{Status: 400, Message: "该渠道不是 Codex 授权渠道，无法重新授权。"}
		}
		expectedChannelID = channel.ID
		mode = "browser"
	}
	if mode == "" {
		mode = "browser"
	}
	if mode != "browser" && mode != "device" {
		return nil, &ManagementToolError{Status: 400, Message: "mode 必须是 browser 或 device。"}
	}
	// The provider's start lock makes check-and-register atomic for concurrent
	// starts: the loser of a double click finds the winner's flow and never binds
	// a second listener. The pending check happens under it, before anything is
	// minted or bound, and once more under the registration lock.
	defer o.startLock(store.CredentialCodexOAuth)()
	o.book().mu.Lock()
	if _, f, ok := o.pendingFlowFor(store.CredentialCodexOAuth, expectedChannelID, mode); ok {
		o.book().mu.Unlock()
		if f.accountTarget != consentTarget(ctx) {
			return nil, &ManagementToolError{Status: 409, Message: "该渠道已有另一凭证授权进行中"}
		}
		if err := checkOAuthFlowAccess(ctx, f.id); err != nil {
			return nil, err
		}
		return startPayloadCodex(f), nil
	}
	o.book().mu.Unlock()
	var device codex.DeviceFlow
	if mode == "device" {
		ctx, cancel := context.WithTimeout(ctx, flowCallLimit)
		started, err := codex.StartDeviceFlow(ctx, o.codex.Client, o.codex.Endpoints)
		cancel()
		if err != nil {
			switch {
			case accessguard.Denied(err):
				return nil, err
			case errors.Is(err, codex.ErrDeviceAuthDisabled):
				return nil, &ManagementToolError{Status: 400, Message: "该服务器未启用设备码登录，请改用浏览器授权。"}
			default:
				return nil, &ManagementToolError{Status: 502, Message: "无法启动设备码授权：" + err.Error()}
			}
		}
		device = started
	}
	var listener *siwc.CallbackListener
	var auth siwc.AuthRequest
	if mode == "browser" {
		bound, redirect, err := codex.ListenRedirect()
		if err != nil {
			return nil, &oauthCallbackBindError{&ManagementToolError{Status: 502, Message: "无法监听授权回调端口：" + err.Error()}}
		}
		built, err := codex.AuthorizeURL(o.codex.Endpoints, redirect)
		if err != nil {
			bound.Close()
			return nil, &ManagementToolError{Status: 500, Message: "无法构造授权链接：" + err.Error()}
		}
		listener = bound
		auth = siwc.AuthRequest{URL: built.URL, RedirectURI: built.RedirectURI, ClientID: built.ClientID, State: built.State, Verifier: built.Verifier}
	}
	o.book().mu.Lock()
	defer o.book().mu.Unlock()
	o.prune()
	// One authorization per provider, re-checked under the lock that registers:
	// an in-flight flow for the same mode and channel is the one being
	// completed, so it is handed back and whatever this start minted is dropped.
	if _, f, ok := o.pendingFlowFor(store.CredentialCodexOAuth, expectedChannelID, mode); ok {
		if listener != nil {
			listener.Close()
		}
		if f.accountTarget != consentTarget(ctx) {
			return nil, &ManagementToolError{Status: 409, Message: "该渠道已有另一凭证授权进行中"}
		}
		if err := checkOAuthFlowAccess(ctx, f.id); err != nil {
			return nil, err
		}
		return startPayloadCodex(f), nil
	}
	if len(o.book().flows) >= maxOpenFlows {
		if listener != nil {
			listener.Close()
		}
		return nil, &ManagementToolError{Status: 429, Message: "等待中的授权过多，请先完成或重新开始已有的授权。"}
	}
	flowID, err := newFlowID()
	if err != nil {
		if listener != nil {
			listener.Close()
		}
		return nil, &ManagementToolError{Status: 500, Message: "无法生成授权流程标识：" + err.Error()}
	}
	flow := &oauthFlow{accountTarget: consentTarget(ctx), provider: store.CredentialCodexOAuth, pluginEpoch: o.epoch, codexMode: mode, state: flowPending, expectedChannelID: expectedChannelID}
	if err := accessguard.Check(ctx); err != nil {
		if listener != nil {
			listener.Close()
		}
		return nil, err
	}
	if mode == "device" {
		flow.codexDevice = device
		flow.interval = flowIntervalOf(device.Interval)
		flow.expiresAt = device.ExpiresAt
		flow.id = flowID
		o.book().flows[flowID] = flow
		return startPayloadCodex(flow), nil
	}
	flow.auth = auth
	flow.listener = listener
	flow.expiresAt = time.Now().Add(browserFlowWindow)
	flow.id = flowID
	o.book().flows[flowID] = flow
	return startPayloadCodex(flow), nil
}

func (o *oauthTools) statusCopilotCore(ctx context.Context, id string) (any, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, &ManagementToolError{Status: 400, Message: "缺少 flow 参数。"}
	}
	o.book().mu.Lock()
	f, ok := o.book().flows[id]
	o.book().mu.Unlock()
	if !ok || f.provider != store.CredentialCopilotOAuth {
		return nil, &ManagementToolError{Status: 404, Message: "授权流程不存在或已过期，请重新开始。"}
	}
	if err := checkOAuthFlowAccess(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ctx = o.flowGuardContext(ctx, f)
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if f.state != flowPending || time.Now().Before(f.lastPoll.Add(f.interval)) {
		return flowPayload(f), nil
	}
	ctx, cancel := context.WithTimeout(ctx, flowCallLimit)
	defer cancel()
	result := copilot.PollDeviceFlow(ctx, o.copilot.Client, o.copilot.Endpoints, f.device)
	if accessguard.Denied(result.Err) {
		return nil, result.Err
	}
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	switch result.Verdict {
	case copilot.PollPending:
		f.lastPoll = time.Now()
	case copilot.PollSlowDown:
		f.interval += copilot.PollIntervalSlowDown * time.Second
		f.lastPoll = time.Now()
	case copilot.PollFailed:
		code, message := pollFailure(result)
		f.state, f.errCode, f.errMessage = flowError, code, message
	default:
		o.completeFlow(ctx, f, result.GitHubToken)
	}
	return flowPayload(f), nil
}

func (o *oauthTools) statusChatGPTCore(ctx context.Context, id, callbackURL string) (any, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, &ManagementToolError{Status: 400, Message: "缺少 flow 参数。"}
	}
	o.book().mu.Lock()
	f, ok := o.book().flows[id]
	o.book().mu.Unlock()
	if !ok || f.provider != store.CredentialChatGPTOAuth {
		return nil, &ManagementToolError{Status: 404, Message: "授权流程不存在或已过期，请重新开始。"}
	}
	if err := checkOAuthFlowAccess(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ctx = o.flowGuardContext(ctx, f)
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if f.state != flowPending {
		return flowPayload(f), nil
	}
	var callback siwc.CallbackResult
	switch pasted := strings.TrimSpace(callbackURL); {
	case pasted != "":
		parsed, err := siwc.ParseCallbackURL(pasted)
		if err != nil {
			return flowPayload(f), nil
		}
		callback = parsed
	case !f.delivered:
		// Non-blocking: the listener hands over its result once and the flow
		// keeps polling until then, so a slow browser cannot wedge the console.
		select {
		case result := <-f.listener.Result():
			f.delivered = true
			callback = result
		default:
			return flowPayload(f), nil
		}
	default:
		return flowPayload(f), nil
	}
	o.completeChatGPT(ctx, f, callback)
	return flowPayload(f), nil
}

func (o *oauthTools) statusCodexCore(ctx context.Context, id, callbackURL string) (any, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, &ManagementToolError{Status: 400, Message: "缺少 flow 参数。"}
	}
	o.book().mu.Lock()
	f, ok := o.book().flows[id]
	o.book().mu.Unlock()
	if !ok || f.provider != store.CredentialCodexOAuth {
		return nil, &ManagementToolError{Status: 404, Message: "授权流程不存在或已过期，请重新开始。"}
	}
	if err := checkOAuthFlowAccess(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ctx = o.flowGuardContext(ctx, f)
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if f.state != flowPending {
		return flowPayload(f), nil
	}
	if f.codexMode == "device" {
		if time.Now().Before(f.lastPoll.Add(f.interval)) {
			return flowPayload(f), nil
		}
		// The poll is bounded like every other console call: a device flow can
		// live for minutes, and a request that hangs on it must not.
		pollCtx, pollCancel := context.WithTimeout(ctx, flowCallLimit)
		result := codex.PollDeviceFlow(pollCtx, o.codex.Client, o.codex.Endpoints, f.codexDevice)
		pollCancel()
		if accessguard.Denied(result.Err) {
			return nil, result.Err
		}
		if err := accessguard.Check(ctx); err != nil {
			return nil, err
		}
		f.lastPoll = time.Now()
		switch result.Verdict {
		case codex.DevicePending:
			return flowPayload(f), nil
		case codex.DeviceComplete:
			o.completeCodex(ctx, f, codex.LoginInput{DeviceCode: result.AuthorizationCode, DeviceVerifier: result.Verifier})
		default:
			code, message := deviceFailure(result)
			f.state, f.errCode, f.errMessage = flowError, code, message
			f.finish()
		}
		return flowPayload(f), nil
	}
	var callback siwc.CallbackResult
	switch pasted := strings.TrimSpace(callbackURL); {
	case pasted != "":
		parsed, err := oauthbase.ParseCallbackURLOn(pasted, "localhost")
		if err != nil {
			return flowPayload(f), nil
		}
		callback = parsed
	case !f.delivered:
		select {
		case result := <-f.listener.Result():
			f.delivered = true
			callback = result
		default:
			return flowPayload(f), nil
		}
	default:
		return flowPayload(f), nil
	}
	o.completeCodex(ctx, f, codex.LoginInput{Code: callback.Code, State: callback.State, CallbackError: callback.Error, CallbackDescription: callback.Description})
	return flowPayload(f), nil
}

func (o *oauthTools) cancelCore(ctx context.Context, provider, id string) (any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, &ManagementToolError{Status: 400, Message: "缺少 flow 参数。"}
	}
	o.book().mu.Lock()
	f, ok := o.book().flows[id]
	o.book().mu.Unlock()
	if !ok {
		// Finished, pruned, or never ours: the user asked for it to be over, and
		// it is. Reporting that (rather than 404) keeps a double cancel, or a
		// cancel racing a completion, from looking like a failure.
		return map[string]any{"status": flowCancelled, "message": "授权已取消。"}, nil
	}
	if f.provider != provider {
		return nil, &ManagementToolError{Status: 404, Message: "授权流程不存在或已过期，请重新开始。"}
	}
	if err := checkOAuthFlowAccess(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == flowPending {
		// Only a pending flow is cancelled. One that already ended reports the
		// state it ended in: a completed authorization is not undone by a late
		// cancel, and the channel it created stays.
		f.state = flowCancelled
		f.errMessage = "授权已取消。"
		f.finish()
	}
	return flowPayload(f), nil
}

func (o *oauthTools) modelsCore(ctx context.Context, id int64, credentialID string, apply bool) (any, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if id < 1 {
		return nil, &ManagementToolError{Status: 400, Message: "invalid channel id"}
	}
	channel, err := o.store.Channel(id)
	if err != nil {
		return nil, &ManagementToolError{Status: 404, Message: "channel not found"}
	}
	if channel.CredentialType != store.CredentialAPIKey {
		o, ctx, err = o.nativeView(ctx, channel.CredentialType)
		if err != nil {
			return nil, err
		}
	}
	expectedVersion := store.CatalogueSourceVersion(channel)
	channel, err = exactOAuthCredential(channel, credentialID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var models []string
	var modelFormats map[string][]string
	var marketCatalogue *store.Channel
	secrets := oauthSecrets(channel)
	if channel.CredentialType != store.CredentialAPIKey && channel.CredentialType != store.CredentialZenFree && channel.OAuth == nil {
		return nil, &ManagementToolError{Status: 400, Message: "该渠道没有 OAuth 凭证。"}
	}
	switch channel.CredentialType {
	case store.CredentialZenFree:
		if o.public == nil {
			return nil, &ManagementToolError{Status: 503, Message: "公共接入插件尚不可用。"}
		}
		found, fetchErr := o.public.Models(ctx)
		err = fetchErr
		modelFormats = map[string][]string{}
		for _, model := range found {
			models = append(models, model.ID)
			modelFormats[model.ID] = []string{model.Format}
		}
	case store.CredentialCopilotOAuth:
		if o.copilot == nil {
			return nil, &ManagementToolError{Status: 503, Message: "此进程没有配置 Copilot 客户端。"}
		}
		bearer, bearerErr := o.copilot.Bearer(ctx, channel)
		if bearerErr != nil {
			if accessguard.Denied(bearerErr) {
				return nil, bearerErr
			}
			return map[string]any{"ok": false, "detail": Redact("无法刷新 Copilot 凭证："+bearerErr.Error(), oauthSecrets(channel)...)}, nil
		}
		secrets = append(secrets, bearer)
		// The account's own api base, the one the authorization stored.
		base := channel.OAuth.APIBase
		if base == "" {
			base = o.copilot.Endpoints.ModelAPI
		}
		models, modelFormats, err = copilot.DiscoverModels(ctx, o.copilot.Client, base, bearer)
	case store.CredentialChatGPTOAuth:
		if o.siwc == nil {
			return nil, &ManagementToolError{Status: 503, Message: "此进程没有配置 ChatGPT 客户端。"}
		}
		token, tokenErr := o.siwc.Bearer(ctx, channel)
		if tokenErr != nil {
			if accessguard.Denied(tokenErr) {
				return nil, tokenErr
			}
			return map[string]any{"ok": false, "detail": Redact("无法刷新 ChatGPT 凭证："+tokenErr.Error(), oauthSecrets(channel)...)}, nil
		}
		secrets = append(secrets, token)
		models, err = siwc.DiscoverModels(ctx, o.siwc.Client, o.siwc.Endpoints.Models, token)
	case store.CredentialCodexOAuth:
		if o.codex == nil {
			return nil, &ManagementToolError{Status: 503, Message: "此进程没有配置 Codex 客户端。"}
		}
		token, tokenErr := o.codex.Bearer(ctx, channel)
		if tokenErr != nil {
			if accessguard.Denied(tokenErr) {
				return nil, tokenErr
			}
			return map[string]any{"ok": false, "detail": Redact("无法刷新 Codex 凭证："+tokenErr.Error(), oauthSecrets(channel)...)}, nil
		}
		secrets = append(secrets, token)
		// The catalogue is account-scoped, so the pull carries the same
		// ChatGPT-Account-ID the authorization's own pull did: the fresh access
		// token's claim first, the stored id token's next, the channel's pinned
		// account last when it is not the subject.
		accountID := codex.AccountHeaderID(token, channel.OAuth.IDToken, channel.OAuth.Account, channel.OAuth.Subject)
		models, err = codex.DiscoverModels(ctx, o.codex.Client, o.codex.Endpoints.Models, token, accountID)
	case store.CredentialClaudeOAuth, store.CredentialAntigravityOAuth, store.CredentialDevinOAuth:
		if o.native == nil {
			return nil, &ManagementToolError{Status: 503, Message: "此接入尚不可用。"}
		}
		if len(channel.Credentials) != 1 {
			credential := channel.PrimaryCredential()
			channel.Credentials = []store.CredentialItem{credential}
			channel.OAuth = credential.OAuth
		}
		found, fetchErr := o.native.Models(ctx, channel)
		err = fetchErr
		for _, model := range found {
			models = append(models, model.ID)
		}
	default:
		if o.market != nil {
			if len(channel.Credentials) != 1 {
				credential := channel.PrimaryCredential()
				channel.Credentials, channel.OAuth = []store.CredentialItem{credential}, credential.OAuth
			}
			found, fetchErr := o.market.Models(ctx, channel)
			err = fetchErr
			if err == nil {
				next, catalogueErr := marketbridge.Catalogue(channel.Name, channel.CredentialType, found)
				err = catalogueErr
				marketCatalogue = &next
				models = next.AvailableModels
				modelFormats = next.ModelFormats
			}
			break
		}
		if !store.IsCompatibleOAuth(channel.CredentialType) || o.compatible == nil {
			return nil, &ManagementToolError{Status: 400, Message: "该渠道不是可用的 OAuth 渠道；API Key 渠道请在编辑页用「从上游获取模型」。"}
		}
		if len(channel.Credentials) != 1 {
			credential := channel.PrimaryCredential()
			channel.Credentials, channel.OAuth = []store.CredentialItem{credential}, credential.OAuth
		}
		found, fetchErr := o.compatible.Models(ctx, channel)
		err = fetchErr
		for _, model := range found {
			models = append(models, model.ID)
		}
	}
	if err != nil {
		if accessguard.Denied(err) {
			return nil, err
		}
		return map[string]any{"ok": false, "detail": Redact(err.Error(), secrets...)}, nil
	}
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if !apply {
		return map[string]any{"ok": true, "fetched": len(models), "models": models, "model_formats": modelFormats}, nil
	}
	var updated store.Channel
	if marketCatalogue != nil {
		updated, err = o.store.ReplaceMarketCatalogueAtVersion(id, *marketCatalogue, expectedVersion, func() error { return accessguard.Check(ctx) })
	} else if modelFormats != nil {
		updated, err = o.store.ReplaceProviderCatalogueAtVersion(id, modelFormats, expectedVersion, func() error { return accessguard.Check(ctx) })
	} else if accessguard.Present(ctx) {
		updated, err = o.store.ReplaceCatalogueAtVersion(id, models, expectedVersion, func() error { return accessguard.Check(ctx) })
	} else {
		updated, err = o.store.ReplaceCatalogue(id, models)
	}
	if errors.Is(err, store.ErrEditConflict) || accessguard.Denied(err) {
		return nil, err
	}
	if err != nil {
		return map[string]any{"ok": false, "detail": "模型已获取，但写入渠道失败：" + err.Error()}, nil
	}
	return map[string]any{
		"ok": true,
		// fetched is this pull's own count, which is what the console reports;
		// catalogue is what the channel lists afterwards (fetched plus manual).
		"fetched":       len(models),
		"models":        updated.Models,
		"catalogue":     updated.AvailableModels,
		"manual":        updated.ManualModels,
		"model_formats": updated.ModelFormats,
	}, nil
}

func flowPayload(f *oauthFlow) any {
	switch f.state {
	case flowComplete:
		answer := map[string]any{"status": flowComplete, "channel_id": f.channelID}
		if f.alreadyLinked {
			// The authorization succeeded and the channel already existed, so
			// nothing was created. The caller needs to say so, or the user
			// looks for a channel that was never made.
			answer["already_linked"] = true
			answer["message"] = f.errMessage
		}
		if len(f.degraded) > 0 {
			answer["degraded"] = f.degraded
			answer["message"] = "渠道已创建，但账号信息拉取失败（显示名与动态接口地址使用默认值）。"
		}
		return answer
	case flowCancelled:
		return map[string]any{"status": flowCancelled, "message": firstLine(f.errMessage, "授权已取消。")}
	case flowError:
		return map[string]any{"status": flowError, "code": f.errCode, "message": f.errMessage, "existing_channel_id": f.existingChannelID, "existing_credential_id": f.existingCredentialID}
	default:
		return map[string]any{
			"status":   flowPending,
			"expires":  f.expiresAt,
			"interval": f.interval.Seconds(),
		}
	}
}
