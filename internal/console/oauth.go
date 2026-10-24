package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"torouter/internal/accessguard"

	"github.com/go-chi/chi/v5"
	"torouter/internal/codex"
	"torouter/internal/compatprofile"
	"torouter/internal/copilot"
	"torouter/internal/marketbridge"
	"torouter/internal/nativeoauth"
	"torouter/internal/oauthbase"
	"torouter/internal/pluginhost"
	"torouter/internal/plugins"
	"torouter/internal/siwc"
	"torouter/internal/store"
	"torouter/internal/zenfree"
)

// The console half of the Copilot OAuth track: a device flow the user completes
// in their browser, and a channel that appears when they do. The user-facing
// surface is two endpoints; everything else (exchange, refresh, catalogue) is
// the copilot service's job.
//
// The flow is client-paced: /status performs at most one GitHub poll per
// interval, which keeps the polling cadence with the caller and needs no
// background goroutine that outlives the console request that started it.
const (
	flowPending  = "pending"
	flowComplete = "complete"
	flowError    = "error"
	// flowCancelled is the terminal state a user-initiated cancel leaves. Nothing
	// revives it: the browser tab is gone, so no redirect and no code exchange
	// can follow, and answering "pending" forever is what made a closed tab look
	// like a hung console.
	flowCancelled = "cancelled"
	maxOpenFlows  = 8 // one person authorizes one account at a time
	flowCallLimit = 30 * time.Second
	// browserFlowWindow is how long a browser authorization stays answerable,
	// and therefore how long it may hold its loopback port. Both browser tracks
	// use it (the SIWC track and the Codex one). Ten minutes, not thirty: an
	// authorization code is short-lived (the SIWC reference reports ~600s), so a
	// flow kept alive past that can only invite pasting a callback whose code
	// the issuer has already expired — the honest answer becomes "start again",
	// and a shorter window asks for it sooner.
	browserFlowWindow = 10 * time.Minute
)

// oauthTools runs both OAuth tracks. The console-facing surface is the same
// two-step shape per track — start, then status until it ends — so the flow
// bookkeeping lives here once and the per-track work is dispatched by provider.
type oauthTools struct {
	owner      *oauthTools
	plugins    *plugins.Manager
	epoch      uint64
	copilot    *copilot.Service
	siwc       *siwc.Service
	codex      *codex.Service
	native     *nativeoauth.Service
	public     *zenfree.Service
	compatible *pluginhost.Service
	market     *marketbridge.Service
	store      *store.Store
	mu         sync.Mutex
	flows      map[string]*oauthFlow
	// starting serializes one provider's start calls. "One authorization per
	// provider" is a check-then-act without it: two concurrent starts both pass
	// the pending check, both run their upstream work (a device code minted, a
	// listener bound) and both register a flow. The lock is per provider, so a
	// slow issuer for one track still cannot hold another track's bookkeeping
	// — o.book().mu is never held across a network call.
	starting sync.Map // provider -> *sync.Mutex
}

// startLock serializes the starts of one provider and returns its release.
func (o *oauthTools) startLock(provider string) func() {
	value, _ := o.book().starting.LoadOrStore(provider, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// oauthFlow is one authorization in progress. Its mutex serializes the polls of
// a single flow: two concurrent /status calls must not both complete the same
// device code into two channels.
type oauthFlow struct {
	nativeTarget         nativeoauth.ConsentTarget
	existingChannelID    int64
	existingCredentialID string
	accountTarget        store.PluginConsentTarget
	market               *marketConsent
	compatibleAuth       *pluginhost.Authorization
	compatibleTarget     store.PluginConsentTarget
	compatibleResult     chan compatibleConsentResult
	compatibleCancel     context.CancelFunc
	pluginEpoch          uint64
	nativeAuth           *nativeoauth.Authorization
	nativePoolVersion    string
	mu                   sync.Mutex
	id                   string // the handle /status polls with; the map key
	provider             string
	// expiresAt is when this flow stops being answerable, independent of
	// anything the upstream says. An SIWC flow has no device code to expire, so
	// its window is the console's own bound; reading a device expiry here would
	// be a zero value that prunes every SIWC flow on the next start.
	expiresAt time.Time
	// Copilot: the device flow, paced by the console's own polls.
	device   copilot.DeviceFlow
	interval time.Duration
	lastPoll time.Time
	// SIWC: the authorization the user completes in a browser, and the loopback
	// listener that catches the redirect. Paste is the fallback the user drives.
	auth      siwc.AuthRequest
	listener  *siwc.CallbackListener
	delivered bool // the listener already handed its result over
	// expectedChannelID and expectedSubject bind a re-authorization to the
	// account that started it: the user may sign in as anybody in the browser,
	// and rotating channel A's credential with account B's tokens would move a
	// registered client to a different user without anybody noticing.
	expectedChannelID int64
	expectedSubject   string
	// Codex: either the browser authorization above, or a device-code login the
	// console paces with its own polls. The device flow gets its own field name
	// because the Copilot track already uses `device` for its device code.
	codexDevice   codex.DeviceFlow
	codexMode     string // "browser" or "device"
	state         string
	channelID     int64
	alreadyLinked bool     // the account was already linked; no channel was created
	degraded      []string // steps that failed without failing the login
	errCode       string
	errMessage    string
}

// finish releases everything a pending flow holds. It is the single exit for a
// flow that leaves pending, so its loopback listener is released exactly once —
// whether the flow completed, failed, or was pruned.
func (f *oauthFlow) finish() {
	if f.market != nil {
		f.market.cancel()
		if f.market.worker != nil {
			_ = f.market.worker.Close()
		}
	}
	if f.compatibleCancel != nil {
		f.compatibleCancel()
		f.compatibleCancel = nil
	}
	if f.compatibleAuth != nil {
		f.compatibleAuth.Close()
	}
	f.compatibleResult = nil
	if f.listener != nil {
		f.listener.Close()
		f.listener = nil
	}
}

// oauthRoutes mounts the OAuth surface on r and hands back the flow
// bookkeeping it built, so the console's handler can carry it: a test needs a
// way to end flows a half-finished authorization left pending.
func oauthRoutes(r chi.Router, s *store.Store, copilotSvc *copilot.Service, siwcSvc *siwc.Service, codexSvc *codex.Service) *oauthTools {
	o := &oauthTools{copilot: copilotSvc, siwc: siwcSvc, codex: codexSvc, store: s, flows: map[string]*oauthFlow{}}
	mountOAuthRoutes(r, o)
	return o
}

func mountOAuthRoutes(r chi.Router, o *oauthTools) {
	providers := []string{"claude", "antigravity", "devin"}
	for _, p := range compatprofile.Profiles() {
		providers = append(providers, p.ID)
	}
	for _, provider := range providers {
		r.Post("/oauth/"+provider+"/start", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
			_ = id
			o.startAuthorizationHTTP(w, r, provider)
		})
		r.Get("/oauth/"+provider+"/status", func(w http.ResponseWriter, r *http.Request) {
			out, err := o.ManagementStatusCallback(r.Context(), provider, r.URL.Query().Get("flow"), r.URL.Query().Get("callback"))
			respondManagementTool(w, out, err)
		})
		r.Post("/oauth/"+provider+"/cancel", o.cancel)
	}
	r.Post("/oauth/copilot/start", o.startCopilot)
	r.Get("/oauth/copilot/status", o.statusCopilot)
	r.Post("/oauth/chatgpt/start", o.startChatGPT)
	r.Get("/oauth/chatgpt/status", o.statusChatGPT)
	r.Post("/oauth/codex/start", o.startCodex)
	r.Get("/oauth/codex/status", o.statusCodex)
	r.Get("/oauth/codex/usage", o.codexUsage)
	r.Post("/oauth/codex/usage", o.codexUsage)
	// One route per provider, like every other OAuth route above: chi gives the
	// static children (/oauth/chatgpt/…) priority over a {provider} sibling, so a
	// shared cancel route would never be reached.
	r.Post("/oauth/copilot/cancel", o.cancel)
	r.Post("/oauth/chatgpt/cancel", o.cancel)
	r.Post("/oauth/codex/cancel", o.cancel)
	r.Post("/channels/{id}/oauth/models", o.fetchOAuthModels)
}

// startCopilot begins a device flow and returns the code the user types at
// github.com/login/device. Nothing is stored but the flow itself: the channel
// only exists once GitHub has answered.
func (o *oauthTools) startCopilot(w http.ResponseWriter, r *http.Request) {
	o.startAuthorizationHTTP(w, r, "copilot")
}

// statusCopilot reports one flow: pending until GitHub says otherwise, complete
// with the channel that was created, or error with the branch that ended it.
func (o *oauthTools) statusCopilot(w http.ResponseWriter, r *http.Request) {
	out, err := o.ManagementStatusCallback(r.Context(), "copilot", r.URL.Query().Get("flow"), r.URL.Query().Get("callback"))
	respondManagementTool(w, out, err)
}

// completeFlow turns an authorized GitHub token into a channel: exchange it for
// a Copilot bearer, read the account, pull the catalogue, save. First version
// imports the whole picker-enabled catalogue — the owner trims it in the channel
// editor afterwards — and prefers the api base the account reports.
//
// The identity check comes first, before anything is exchanged. One GitHub
// token is one channel: a second authorization of an account that is already
// linked must not create a parallel channel, because two channels sharing a
// token refresh the same rotation against each other and the loser's requests
// die on a token the other rotation invalidated (good-finds G017, applied
// across channels). The existing channel is reported instead, and no upstream
// call is spent on the answer.
func (o *oauthTools) completeFlow(ctx context.Context, f *oauthFlow, githubToken string) {
	if flowAccessRevoked(ctx, f) {
		return
	}
	existing, err := o.copilot.Store.ChannelByGitHubToken(githubToken)
	if f.accountTarget.ChannelID > 0 {
		err = store.ErrNotFound
	}
	switch {
	case err == nil:
		f.state = flowComplete
		f.channelID = existing.ID
		f.alreadyLinked = true
		f.errMessage = "这个 GitHub 账号已经连接到 to-router；沿用既有渠道，未创建重复渠道。"
		return
	case !errors.Is(err, store.ErrNotFound):
		f.state, f.errCode, f.errMessage = flowError, "lookup_failed", "无法检查既有渠道："+err.Error()
		return
	}
	login, err := o.copilot.CompleteLogin(ctx, githubToken)
	if err != nil {
		if failFlowAccess(f, err) {
			return
		}
		switch {
		case errors.Is(err, copilot.ErrUnsubscribed):
			f.state, f.errCode, f.errMessage = flowError, "unsubscribed", "这个 GitHub 账号没有生效的 Copilot 订阅，无法创建渠道。订阅后重新授权即可。"
		case errors.Is(err, copilot.ErrReauthRequired):
			f.state, f.errCode, f.errMessage = flowError, "reauth_required", "GitHub 拒绝了授权令牌，请重新开始授权流程。"
		default:
			f.state, f.errCode, f.errMessage = flowError, "login_failed", "授权成功，但初始化账号失败："+err.Error()
		}
		f.finish()
		return
	}
	channel := store.Channel{
		Name:           "GitHub Copilot",
		CredentialType: store.CredentialCopilotOAuth,
		Enabled:        true,
		Models:         login.Models,
		ModelFormats:   login.ModelFormats,
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIChatCompletions, BaseURL: login.APIBase},
			{Format: store.FormatAnthropicMessages, BaseURL: login.APIBase},
		},
		OAuth: &store.OAuthCredential{
			Provider:     store.OAuthProviderCopilot,
			GitHubToken:  login.GitHubToken,
			AccessToken:  login.Bearer.Token,
			ExpiresAt:    login.Bearer.ExpiresAt,
			AccountLabel: login.Account,
			APIBase:      login.APIBase,
			Status:       store.OAuthStatusActive,
			UpdatedAt:    time.Now().UTC(),
		},
	}
	if channel.OAuth != nil {
		channel.OAuth.Account = login.Identity
	}
	if o.commitAccountFlow(ctx, f, *channel.OAuth) {
		f.degraded = login.Degraded
		return
	}
	// The allowlist speaks here too: an account reporting an api base outside
	// the documented hosts is refused by the store, and the refusal travels
	// back to the caller instead of being silently re-pointed.
	if flowAccessRevoked(ctx, f) {
		return
	}
	if err := o.copilot.Store.SaveChannel(&channel); err != nil {
		f.state, f.errCode, f.errMessage = flowError, "channel_rejected", "授权成功，但创建渠道被拒绝："+err.Error()
		f.finish()
		return
	}
	f.state = flowComplete
	f.channelID = channel.ID
	f.degraded = login.Degraded
}

// pollFailure maps a terminal device-flow branch onto the code and message the
// console shows.
func pollFailure(result copilot.PollResult) (string, string) {
	switch result.Code {
	case "expired_token":
		return "expired_token", "授权码已过期，请重新开始授权。"
	case "access_denied":
		return "access_denied", "已在 GitHub 上拒绝本次授权。"
	case "network":
		return "network", "无法连接 GitHub：" + result.Err.Error()
	default:
		return result.Code, "授权失败：" + result.Err.Error()
	}
}

// flowInterval is the pacing the flow honours between polls. GitHub always
// sends one; a zero is taken literally so a test double controls the cadence,
// which is safe because only the caller's own polls drive it.
func flowInterval(device copilot.DeviceFlow) time.Duration {
	return time.Duration(device.Interval) * time.Second
}

// prune drops flows that are finished or whose window has passed, so the map
// cannot grow without bound on a process that lives for weeks. Every flow it
// drops is finished first, so its loopback listener is released.
func (o *oauthTools) prune() {
	for id, f := range o.book().flows {
		f.mu.Lock()
		if o.plugins != nil && f.state == flowPending && o.plugins.CheckEpoch(f.provider, f.pluginEpoch) != nil {
			f.state, f.errMessage = flowCancelled, "插件启用状态已变化，请重新授权。"
			f.finish()
		}
		done := f.state != flowPending || time.Now().After(f.expiresAt)
		if done {
			f.finish()
		}
		f.mu.Unlock()
		if done {
			delete(o.book().flows, id)
		}
	}
}

// cancel ends a pending flow at the user's request. The user closed the browser
// tab — or simply changed their mind — and the console must stop waiting for a
// redirect that will never arrive: the listener (and its pinned port) is
// released immediately, and the flow is marked cancelled, a terminal state
// nothing revives. Cancelling a flow the console has already pruned is the same
// answer, so a double cancel is harmless.
//
// The cancel is local by design: a Copilot device code and a Codex device code
// stay valid at the upstream until they expire, and revoking them would need an
// endpoint neither ref documents. What ends is this console's wait, and the
// upstream's own expiry collects the rest.
func (o *oauthTools) cancel(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	provider := parts[len(parts)-2]
	out, err := o.ManagementCancel(r.Context(), provider, r.URL.Query().Get("flow"))
	respondManagementTool(w, out, err)
}

// pendingFlowFor returns the one pending flow of a provider that matches the
// wanted channel and mode, or nil. One authorization per provider at a time: a
// second start must not mint a second URL (and a second loopback listener)
// while one is still answerable — the user clicked twice, or reloaded the page
// and clicked again, and either way the authorization in flight is the one they
// are completing. The match keeps the idempotence honest: a re-authorization of
// another channel, or the other Codex mode, is a different flow and gets its
// own. The caller holds o.book().mu.
func (o *oauthTools) pendingFlowFor(provider string, channelID int64, mode string) (string, *oauthFlow, bool) {
	o.prune()
	for id, f := range o.book().flows {
		f.mu.Lock()
		match := f.provider == provider && f.state == flowPending &&
			time.Now().Before(f.expiresAt) &&
			f.expectedChannelID == channelID &&
			(provider != store.CredentialCodexOAuth || f.codexMode == mode)
		f.mu.Unlock()
		if match {
			return id, f, true
		}
	}
	return "", nil, false
}

// startPayloadCopilot is the start answer a pending Copilot flow hands back, so
// a repeated start and the first one look the same to the caller.
func startPayloadCopilot(f *oauthFlow) map[string]any {
	return map[string]any{
		"flow":             f.id,
		"method":           "device",
		"user_code":        f.device.UserCode,
		"verification_uri": f.device.VerificationURI,
		"expires":          f.device.ExpiresAt,
		"interval":         f.interval.Seconds(),
	}
}

// startPayloadChatGPT is the same for a pending SIWC browser flow.
func startPayloadChatGPT(f *oauthFlow) map[string]any {
	return map[string]any{
		"flow":              f.id,
		"method":            "browser",
		"authorization_url": f.auth.URL,
		"redirect_uri":      f.auth.RedirectURI,
		"callback_path":     siwc.CallbackPath,
		"paste_hint":        "浏览器无法自动跳转时，把地址栏里的完整链接粘贴回控制台。",
	}
}

// startPayloadCodex is the same for a pending Codex flow, browser or device.
func startPayloadCodex(f *oauthFlow) map[string]any {
	if f.codexMode == "device" {
		return map[string]any{
			"flow": f.id, "method": "device",
			"user_code": f.codexDevice.UserCode, "verification_uri": f.codexDevice.Verification,
			"expires": f.codexDevice.ExpiresAt, "interval": f.interval.Seconds(),
		}
	}
	return map[string]any{
		"flow": f.id, "method": "browser",
		"authorization_url": f.auth.URL,
		"redirect_uri":      f.auth.RedirectURI,
		"callback_path":     oauthbase.CallbackPath,
		"paste_hint":        "浏览器无法自动跳转时，把地址栏里的完整链接粘贴回控制台。",
		"non_official":      "此轨复用 Codex CLI 的公共客户端身份，非 OpenAI 官方支持路径。",
	}
}

// startChatGPT opens a Sign-in-with-ChatGPT authorization: it builds the
// authorize URL, binds the loopback callback and returns both. The user
// completes the flow in their browser; /status watches for the redirect, and the
// paste fallback covers the machines where the browser cannot reach the
// listener.
func (o *oauthTools) startChatGPT(w http.ResponseWriter, r *http.Request) {
	o.startAuthorizationHTTP(w, r, "chatgpt")
}

// statusChatGPT drives one SIWC flow. It consumes the loopback result once,
// then accepts a pasted callback link for as long as the flow lives, and only
// then runs the exchange — the network round trips belong to the request that
// asked for the answer, not to a background goroutine.
func (o *oauthTools) statusChatGPT(w http.ResponseWriter, r *http.Request) {
	out, err := o.ManagementStatusCallback(r.Context(), "chatgpt", r.URL.Query().Get("flow"), r.URL.Query().Get("callback"))
	respondManagementTool(w, out, err)
}

// completeChatGPT exchanges an authorization code and turns it into a channel.
// The identity check comes first: one account is one channel, because two
// channels sharing a refresh token race each other's rotations and the loser's
// requests die on a token the other rotation invalidated (good-finds G017,
// applied across channels).
func (o *oauthTools) completeChatGPT(ctx context.Context, f *oauthFlow, callback siwc.CallbackResult) {
	if flowAccessRevoked(ctx, f) {
		return
	}
	if callback.State != f.auth.State {
		f.state, f.errCode, f.errMessage = flowError, "state_mismatch", "回调链接的 state 与本次授权不符，授权已被拒绝。这通常是伪造的回调。"
		f.finish()
		return
	}
	if callback.Error != "" {
		f.state, f.errCode, f.errMessage = flowError, callback.Error, "授权未完成："+firstLine(callback.Description, callback.Error)
		f.finish()
		return
	}
	// A re-authorization is pinned to the client it was started with, so a
	// callback naming a different one is refused before the token endpoint is
	// contacted: the code was issued to one client and is only exchangeable by
	// it. A first authorization is the exception — it starts from the
	// registration entry point, and accepting the new oaiapp_* the callback
	// issues is the whole point.
	// Presence-only breadcrumb for diagnosing exchange failures: which fields
	// the issuer actually sent back. Values stay out of the log.
	slog.Info("oauth callback received", "provider", "chatgpt",
		"reauth", f.expectedChannelID != 0, "has_code", callback.Code != "", "has_error", callback.Error != "",
		"has_client_id", callback.ClientID != "", "has_scope", callback.Scope != "")
	if f.auth.ClientID != "" && f.auth.ClientID != siwc.DynamicAppID && callback.ClientID != "" && callback.ClientID != f.auth.ClientID {
		f.state, f.errCode, f.errMessage = flowError, "client_id_mismatch", "回调返回的 client_id 与本次授权不一致，授权已被拒绝。"
		f.finish()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, flowCallLimit)
	defer cancel()
	// The exchange authenticates as the client the code was issued to. On a
	// first authorization that is the oaiapp_* the callback carries (the
	// registration entry point may not be exchanged); on a re-authorization it
	// is the client the flow already used.
	issued := firstNonEmpty(callback.ClientID, f.auth.ClientID)
	tokens, err := siwc.ExchangeCode(ctx, o.siwc.Client, o.siwc.Endpoints, f.auth, callback.Code, issued)
	if err != nil {
		if failFlowAccess(f, err) {
			return
		}
		switch {
		case errors.Is(err, siwc.ErrReauthRequired):
			// The fixed advice plus whatever the issuer said: a description
			// ("refresh token expired") tells the owner what actually happened
			// instead of leaving them to guess.
			f.state, f.errCode, f.errMessage = flowError, "reauth_required", "OpenAI 拒绝了授权码，请重新开始授权。"+detailSuffix(siwc.DetailOf(err))
		case errors.Is(err, siwc.ErrClientMisconfigured):
			f.state, f.errCode, f.errMessage = flowError, "client_misconfigured", "OpenAI 不识别此渠道的 client 配置，请删除渠道后重新授权注册新客户端。"+detailSuffix(siwc.DetailOf(err))
		case strings.Contains(err.Error(), "oaiapp"):
			// The first authorization's callback did not carry the registered
			// client, so there is nothing the exchange could authenticate as.
			f.state, f.errCode, f.errMessage = flowError, "client_id_missing", "回调没有携带本次授权签发的 oaiapp_* client_id，无法完成注册。请重新授权。"
		default:
			f.state, f.errCode, f.errMessage = flowError, "exchange_failed", "换取令牌失败："+err.Error()
		}
		f.finish()
		return
	}
	if flowAccessRevoked(ctx, f) {
		return
	}
	degraded := []string{}
	label, err := o.siwc.Account(ctx, tokens.AccessToken)
	if flowAccessRevoked(ctx, f) {
		return
	}
	if err != nil {
		// The label is decoration: an authorization must not be thrown away
		// because a cosmetic call failed.
		degraded = append(degraded, "account_profile")
	}
	login, err := o.siwc.CompleteLogin(ctx, siwc.LoginInput{
		Auth: f.auth, Callback: callback, CallbackID: callback.ClientID, CallbackScope: callback.Scope,
		Tokens: tokens, Degraded: degraded, AccountLabel: label, ExpectedSubject: f.expectedSubject, ExpectedChannelID: f.expectedChannelID,
		Detached: f.accountTarget.ChannelID > 0,
	})
	if err != nil {
		if failFlowAccess(f, err) {
			return
		}
		switch {
		case errors.Is(err, store.ErrNotFound):
			f.state, f.errCode, f.errMessage = flowError, "channel_missing", "重新授权的渠道已不存在，请刷新渠道列表后重试。"
		case errors.Is(err, siwc.ErrScopeUndetermined):
			f.state, f.errCode, f.errMessage = flowError, "scope_undetermined", "无法确定本次授权授予了哪些权限。请重新授权，并在确认页逐项确认权限后重试。"
		case errors.Is(err, siwc.ErrRenewableSessionMissing):
			f.state, f.errCode, f.errMessage = flowError, "offline_access_missing", "授权未包含 offline_access，凭证将无法自动续期。请重新授权并在确认页同意全部权限。"
		case errors.Is(err, siwc.ErrConsentMissingScope):
			f.state, f.errCode, f.errMessage = flowError, "consent_missing_scope", "授权未包含 ChatGPT 额度使用权限（chatgpt.tokens.use.direct）。请重新授权并在确认页同意全部权限。"
		case strings.Contains(err.Error(), "client_id_mismatch"):
			f.state, f.errCode, f.errMessage = flowError, "client_id_mismatch", "回调返回的 client_id 与本次授权不一致，授权已被拒绝。"
		case strings.Contains(err.Error(), "client_id_missing"):
			f.state, f.errCode, f.errMessage = flowError, "client_id_missing", "回调没有携带本次授权签发的 oaiapp_* client_id，无法完成注册。请重新授权。"
		case strings.Contains(err.Error(), "identity_mismatch"):
			f.state, f.errCode, f.errMessage = flowError, "identity_mismatch", "授权签出的账号与当前渠道绑定的账号不一致，已拒绝。请用原账号重新授权，或改「添加账号 / 工作区」。"
		case strings.Contains(err.Error(), "id_token"):
			f.state, f.errCode, f.errMessage = flowError, "id_token_invalid", "id_token 校验失败："+err.Error()
		default:
			f.state, f.errCode, f.errMessage = flowError, "login_failed", "授权成功，但初始化账号失败："+err.Error()
		}
		// The listener has nothing left to catch: the authorization is over, one
		// way or another, and the port goes back now rather than when a later
		// start happens to prune this flow.
		f.finish()
		return
	}
	if login.Channel.OAuth != nil && o.commitAccountFlow(ctx, f, *login.Channel.OAuth) {
		f.degraded = login.Degraded
		return
	}
	if login.AlreadyLinked {
		// The account was already connected: the service rotated the existing
		// channel's credential instead of returning a row to save.
		f.state, f.channelID, f.alreadyLinked = flowComplete, login.ChannelID, true
		f.degraded = login.Degraded
		f.errMessage = "这个 ChatGPT 账号已经连接到 to-router；已更新既有渠道的凭证，未创建重复渠道。"
		f.finish()
		return
	}
	if flowAccessRevoked(ctx, f) {
		return
	}
	if err := o.siwc.Store.SaveChannel(&login.Channel); err != nil {
		f.state, f.errCode, f.errMessage = flowError, "channel_rejected", "授权成功，但创建渠道被拒绝："+err.Error()
		// The authorization is over and nothing was stored: the listener has
		// nothing left to catch, so it is released now rather than at the next
		// prune.
		f.finish()
		return
	}
	f.state = flowComplete
	f.channelID = login.Channel.ID
	f.degraded = login.Degraded
	f.finish()
}

// newFlowID mints the opaque handle a console polls with.
func newFlowID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func firstLine(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// completeCodex turns an authorization into a channel. The identity check comes
// first — one account is one channel, because two channels sharing a refresh
// token race each other's rotations (good-finds G017, across channels) — and the
// account id from the id_token is what that identity is made of. When the token
// carries no id_token the account cannot be named, so identity falls back to the
// refresh token's hash rather than blocking the login.
func (o *oauthTools) completeCodex(ctx context.Context, f *oauthFlow, in codex.LoginInput) {
	if flowAccessRevoked(ctx, f) {
		return
	}
	// The callback is checked before anything is exchanged, exactly as the
	// ChatGPT track does: a link that is not this flow's, or an error the
	// issuer returned, must never reach the token endpoint.
	if in.CallbackError != "" {
		if in.CallbackError == "access_denied" {
			f.state, f.errCode, f.errMessage = flowError, "access_denied", "已在授权页面拒绝本次授权。"
		} else {
			f.state, f.errCode, f.errMessage = flowError, in.CallbackError, "授权未完成："+firstLine(in.CallbackDescription, in.CallbackError)
		}
		f.finish()
		return
	}
	if in.Code != "" && in.State != "" && in.State != f.auth.State {
		f.state, f.errCode, f.errMessage = flowError, "state_mismatch", "回调链接的 state 与本次授权不符，授权已被拒绝。这通常是伪造的回调。"
		f.finish()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, flowCallLimit)
	defer cancel()
	if in.DeviceCode != "" {
		tokens, err := codex.ExchangeDeviceCode(ctx, o.codex.Client, o.codex.Endpoints, in.DeviceCode, in.DeviceVerifier)
		if err != nil {
			codexFailed(f, err)
			return
		}
		in.Tokens = tokens
	} else {
		tokens, err := codex.ExchangeCode(ctx, o.codex.Client, o.codex.Endpoints, codexAuthOf(f), in.Code)
		if err != nil {
			codexFailed(f, err)
			return
		}
		in.Tokens = tokens
	}
	if flowAccessRevoked(ctx, f) {
		return
	}
	claims, err := codex.ParseIDClaims(in.Tokens.IDToken)
	if err != nil {
		claims = codex.IDClaims{}
	}
	account := codex.AccountOf(claims)
	if f.accountTarget.ChannelID > 0 {
		channel := codex.ChannelFor(codex.LoginInput{Tokens: in.Tokens, Claims: claims, Account: account, APIBase: o.codex.Endpoints.Backend, Label: "Codex"})
		o.commitAccountFlow(ctx, f, *channel.OAuth)
		return
	}
	// A re-authorization is bound to the channel that started it. The user may
	// sign in as anybody in the browser, and rotating channel A's credential
	// with account B's tokens would move a channel to a different account with
	// no visible failure — the same rule the SIWC track enforces, against this
	// track's identity (the id_token's account id). Checked before anything is
	// rotated or created.
	var existing store.Channel
	var findErr error
	if f.expectedChannelID != 0 {
		existing, findErr = o.codex.Store.Channel(f.expectedChannelID)
	} else {
		existing, findErr = o.codex.Store.ChannelByCodexAccount(account, in.Tokens.RefreshToken)
	}
	if findErr == nil {
		// The account is already connected: rotate its credential in place
		// rather than building a second channel that would race the first one's
		// refreshes.
		in.Account = account
		if rotateErr := o.codex.ReauthorizeChannelContext(ctx, existing.ID, in); rotateErr != nil {
			if failFlowAccess(f, rotateErr) {
				return
			}
			switch {
			case errors.Is(rotateErr, store.ErrNotFound):
				f.state, f.errCode, f.errMessage = flowError, "channel_missing", "重新授权的渠道已不存在，请刷新渠道列表后重试。"
			case strings.Contains(rotateErr.Error(), "identity_mismatch"):
				f.state, f.errCode, f.errMessage = flowError, "identity_mismatch", "授权签出的账号与当前渠道绑定的账号不一致，已拒绝。请用原账号重新授权，或改「添加账号 / 工作区」。"
			default:
				f.state, f.errCode, f.errMessage = flowError, "rotate_failed", "授权成功，但更新既有渠道失败："+rotateErr.Error()
			}
			f.finish()
			return
		}
		f.state, f.channelID, f.alreadyLinked = flowComplete, existing.ID, true
		f.errMessage = "这个 Codex 账号已经连接到 to-router；已更新既有渠道的凭证，未创建重复渠道。"
		f.finish()
		return
	} else if f.expectedChannelID != 0 && errors.Is(findErr, store.ErrNotFound) {
		f.state, f.errCode, f.errMessage = flowError, "channel_missing", "重新授权的渠道已不存在，请刷新渠道列表后重试。"
		f.finish()
		return
	} else if !errors.Is(findErr, store.ErrNotFound) {
		f.state, f.errCode, f.errMessage = flowError, "lookup_failed", "无法检查既有渠道："+findErr.Error()
		f.finish()
		return
	}
	label := firstNonEmpty(claims.Email, claims.Name, account, "codex")
	// The catalogue is scoped to an account, and the account id comes from the
	// tokens themselves before the channel's pinned identity: the access token's
	// claim first (Codex-Manager's order), the id token's next, and the pinned
	// account last when it is not the subject. Without the header the backend
	// answers 400 and the login dies at the last step with nothing said.
	accountID := codex.AccountHeaderID(in.Tokens.AccessToken, in.Tokens.IDToken, claims.AccountID, claims.Subject)
	models, err := codex.DiscoverModels(ctx, o.codex.Client, o.codex.Endpoints.Models, in.Tokens.AccessToken, accountID)
	if err != nil {
		if failFlowAccess(f, err) {
			return
		}
		f.state, f.errCode, f.errMessage = flowError, "catalog_failed", "授权成功，但模型目录拉取失败："+err.Error()
		f.finish()
		return
	}
	channel := codex.ChannelFor(codex.LoginInput{
		Tokens: in.Tokens, Claims: claims, Account: account, APIBase: o.codex.Endpoints.Backend,
		Models: models, Label: label,
	})
	if flowAccessRevoked(ctx, f) {
		return
	}
	if err := o.codex.Store.SaveChannel(&channel); err != nil {
		f.state, f.errCode, f.errMessage = flowError, "channel_rejected", "授权成功，但创建渠道被拒绝："+err.Error()
		f.finish()
		return
	}
	f.state = flowComplete
	f.channelID = channel.ID
	f.finish()
}

// codexFailed records a failed exchange on the flow.
func codexFailed(f *oauthFlow, err error) {
	if failFlowAccess(f, err) {
		return
	}
	switch {
	case errors.Is(err, codex.ErrReauthRequired):
		// The fixed advice plus the issuer's own error and description.
		f.state, f.errCode, f.errMessage = flowError, "reauth_required", "Codex 拒绝了授权，请重新开始授权流程。"+detailSuffix(codex.DetailOf(err))
	default:
		f.state, f.errCode, f.errMessage = flowError, "exchange_failed", "换取令牌失败："+err.Error()
	}
	f.finish()
}

// Revocation is a local authorization failure, not provider denial or a
// malformed OAuth reply. Already submitted refresh rotations remain durable
// inside the services, but this guard prevents a new account from being saved.
func failFlowAccess(f *oauthFlow, err error) bool {
	if !accessguard.Denied(err) {
		return false
	}
	f.state, f.errCode, f.errMessage = flowError, "access_revoked", "管理授权已撤销或权限已改变，授权完成操作已停止；未保存新账号。"
	if errors.Is(err, plugins.ErrDisabled) {
		f.errCode, f.errMessage = "plugin_disabled", "插件已停用或重新启用，请在插件库启用后重新授权。"
	}
	f.finish()
	return true
}
func flowAccessRevoked(ctx context.Context, f *oauthFlow) bool {
	return failFlowAccess(f, accessguard.Check(ctx))
}

// detailSuffix parents an issuer's own error detail behind a fixed-advice
// message; an empty detail changes nothing.
func detailSuffix(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return "（" + strings.TrimSpace(detail) + "）"
}

// codexAuthOf rebuilds the authorization material the browser flow stored, so
// the exchange can present the same verifier.
func codexAuthOf(f *oauthFlow) codex.AuthRequest {
	return codex.AuthRequest{RedirectURI: f.auth.RedirectURI, ClientID: f.auth.ClientID, State: f.auth.State, Verifier: f.auth.Verifier}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// startCodex opens a Codex authorization in one of two modes: the browser
// authorization (the Codex CLI's own flow, on a localhost redirect) or a device
// code (for a machine where no browser is available). mode defaults to browser.
//
// Both mode arms do their upstream work before taking the flow lock — the
// device-code request and the authorize-URL construction — so a slow or
// unreachable issuer cannot hold every other flow's bookkeeping hostage, and
// nothing is registered until the flow can actually proceed.
func (o *oauthTools) startCodex(w http.ResponseWriter, r *http.Request) {
	o.startAuthorizationHTTP(w, r, "codex")
}

// flowIntervalOf turns a device-flow interval in seconds into a pacing duration.
func flowIntervalOf(seconds int) time.Duration {
	if seconds <= 0 {
		return 5 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

// statusCodex drives one Codex flow: the device-code branch is polled, the
// browser branch consumes the listener once and then accepts a pasted link. The
// network round trips belong to the request that asked for the answer, not to a
// background goroutine.
func (o *oauthTools) statusCodex(w http.ResponseWriter, r *http.Request) {
	out, err := o.ManagementStatusCallback(r.Context(), "codex", r.URL.Query().Get("flow"), r.URL.Query().Get("callback"))
	respondManagementTool(w, out, err)
}

// deviceFailure maps a terminal device poll onto the code and message the
// console shows. The "not authorized yet" answers (403/404) never reach here:
// they are pending, not failures.
func deviceFailure(result codex.DevicePollResult) (string, string) {
	switch result.Code {
	case "expired_token":
		return "expired_token", "设备码已过期，请重新开始授权。"
	case "access_denied":
		return "access_denied", "已在验证页面拒绝本次授权。"
	default:
		return "device_failed", "设备码授权失败：" + firstLine(result.Err.Error())
	}
}

// fetchOAuthModels pulls one OAuth channel's model catalogue with the channel's
// own credential: each track's documented catalogue endpoint, reached with the
// token the track's service refreshes. The catalogue write goes through the same
// ReplaceCatalogue the api-key discover uses, so the fetched part replaces the
// fetched part and the owner's manual entries survive.
//
// Probe stays refused on these channels: it sends a paid generation, and a
// catalogue pull is the free half of what the console offers elsewhere.
func (o *oauthTools) fetchOAuthModels(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	out, err := o.ManagementModels(r.Context(), id, "", true)
	respondManagementTool(w, out, err)
}

// codexUsage reports a timestamped, account-scoped display snapshot. Missing
// windows remain unknown; a failed read is distinct from an empty report.
func (o *oauthTools) codexUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, err := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
	if err != nil || id < 1 {
		fail(w, 400, "缺少或非法的 channel_id。")
		return
	}
	credentialID := r.URL.Query().Get("credential_id")
	if strings.TrimSpace(credentialID) == "" {
		fail(w, 400, "缺少 credential_id。")
		return
	}
	channel, err := o.store.Channel(id)
	if err != nil {
		fail(w, 404, "channel not found")
		return
	}
	if channel.CredentialType != store.CredentialCodexOAuth {
		fail(w, 400, "该渠道不是 Codex 轨。")
		return
	}
	found := false
	for _, credential := range channel.Credentials {
		if credential.ID == credentialID && credential.OAuth != nil {
			found = true
			break
		}
	}
	if !found {
		fail(w, 404, "Codex credential not found")
		return
	}
	var usage codex.UsageReport
	if r.Method == http.MethodPost {
		view, guarded, resolveErr := o.nativeView(r.Context(), store.CredentialCodexOAuth)
		if resolveErr != nil {
			respondManagementTool(w, nil, resolveErr)
			return
		}
		usage, err = view.codex.RefreshUsage(guarded, channel.ID, credentialID, r.URL.Query().Get("force") == "1")
	} else {
		usage, err = codex.CachedUsageFromStore(o.store, channel.ID, credentialID)
	}
	if err != nil {
		if pluginError(w, err) {
			return
		}
		// A cancelled waiter never owns the lifecycle lock and must not write
		// attempt metadata over the request that is still running. Preserve its
		// member's current snapshot and schedule only this response's next try.
		usage, _ = codex.CachedUsageFromStore(o.store, channel.ID, credentialID)
		refresh, readErr := o.store.GetQuotaRefresh()
		if readErr != nil {
			fail(w, 500, "额度缓存读取或刷新失败。")
			return
		}
		usage.RefreshSeconds = refresh.QuotaRefreshSeconds
		usage.Detail = "额度缓存读取或刷新失败。"
		usage.Stale = usage.OK
		usage.RefreshDue = false
		attemptedAt := time.Now().UTC()
		usage.LastAttemptAt = &attemptedAt
		usage.NextRefreshAt = nil
		if usage.RefreshSeconds > 0 {
			next := attemptedAt.Add(time.Duration(usage.RefreshSeconds) * time.Second)
			usage.NextRefreshAt = &next
		}
		respond(w, 200, usage)
		return
	}
	respond(w, 200, usage)
}
