package console

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/management"
	"torouter/internal/pluginhost"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

func pluginError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, pluginhost.ErrNotInstalled):
		fail(w, 503, "兼容层未安装或文件需要修复，请在本机插件库安装后重试。")
	case errors.Is(err, plugins.ErrDisabled):
		fail(w, 503, "插件已停用或启用状态已变化，请在插件库启用并重新发起操作。")
	case errors.Is(err, plugins.ErrConflict):
		fail(w, 409, "插件列表已变化，请刷新后重试。")
	case errors.Is(err, plugins.ErrInvalid):
		fail(w, 400, "无效的插件或能力。")
	default:
		return false
	}
	return true
}

func pluginRoutes(r chi.Router, manager *plugins.Manager) {
	r.Post("/plugins/compatible/install", func(w http.ResponseWriter, r *http.Request) {
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		host, parseErr := url.Parse("http://" + r.Host)
		if err != nil || net.ParseIP(peer) == nil || !net.ParseIP(peer).IsLoopback() || parseErr != nil ||
			(host.Hostname() != "localhost" && (net.ParseIP(host.Hostname()) == nil || !net.ParseIP(host.Hostname()).IsLoopback())) {
			fail(w, 403, "安装兼容代码只能在本机控制台执行。")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
			fail(w, 403, "安装请求来源不符。")
			return
		}
		var in struct {
			RuntimePath string `json:"runtime_path"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := manager.InstallCompatible(r.Context(), in.RuntimePath)
		if err != nil {
			// OS errors may contain a private installation/home path.
			fail(w, 400, "兼容层安装失败。请确认已安装 Node.js 24，路径指向 node 可执行文件，且数据目录可写。")
			return
		}
		respond(w, 200, out)
	})
	r.Post("/plugins/{id}/connect", func(w http.ResponseWriter, r *http.Request) {
		out, err := connectPublicPlugin(r.Context(), manager, chi.URLParam(r, "id"))
		respondManagementTool(w, out, err)
	})
	r.Get("/plugins", func(w http.ResponseWriter, r *http.Request) {
		out, err := manager.Snapshot()
		respondManagementTool(w, out, err)
	})
	r.Put("/plugins/{id}/capabilities/{cap}", func(w http.ResponseWriter, r *http.Request) {
		id, err := url.PathUnescape(chi.URLParam(r, "id"))
		if err != nil {
			fail(w, 400, "无效的插件标识。")
			return
		}
		var body struct {
			Enabled         *bool  `json:"enabled"`
			ExpectedVersion string `json:"expected_version"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Enabled == nil {
			fail(w, 400, "enabled 必须是 true 或 false")
			return
		}
		out, err := manager.SetCapability(r.Context(), id, chi.URLParam(r, "cap"), *body.Enabled, body.ExpectedVersion)
		respondManagementTool(w, out, err)
	})
}

type pluginCapabilityInput struct {
	command
	PluginID        string `json:"plugin_id"`
	Capability      string `json:"capability"`
	Enabled         *bool  `json:"enabled"`
	ExpectedVersion string `json:"expected_version"`
}

type publicPluginInput struct {
	command
	PluginID string `json:"plugin_id"`
}

func connectPublicPlugin(ctx context.Context, manager *plugins.Manager, id string) (any, error) {
	if id != "opencode-zen-free" {
		return nil, plugins.ErrInvalid
	}
	runtime, ctx, err := manager.Native(ctx, store.CredentialZenFree)
	if err != nil {
		return nil, err
	}
	c, linked, err := runtime.Public.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"channel_id": c.ID, "already_linked": linked}, nil
}

func (t *managementTools) pluginOperations() []management.Operation {
	return append(t.marketOperations(), []management.Operation{
		typedOperation[publicPluginInput]("connect_public_plugin", "plugins.connect_public", management.Manage, "建立已启用公共接入插件的渠道。无需OAuth，不扫描文件，不自动启用模型；重复调用返回既有渠道。", false, func(ctx context.Context, p management.Principal, in publicPluginInput) (any, error) {
			return connectPublicPlugin(ctx, t.services.plugins, in.PluginID)
		}),
		typedOperation[emptyInput]("list_plugins", "plugins.list", management.Read, "列出插件能力、启用状态、影响渠道和版本；不创建服务或发送网络请求。", true, func(context.Context, management.Principal, emptyInput) (any, error) {
			return t.services.plugins.Snapshot()
		}),
		typedOperation[pluginCapabilityInput]("set_plugin_capability", "plugins.set", management.Manage, "先读取插件列表和影响渠道，再以其expected_version修改一个能力开关。停用不删除渠道或凭证；旧授权流程停止。", false, func(ctx context.Context, p management.Principal, in pluginCapabilityInput) (any, error) {
			if in.Enabled == nil {
				return nil, invalid("enabled 必须是 true 或 false")
			}
			return t.services.plugins.SetCapability(ctx, in.PluginID, in.Capability, *in.Enabled, in.ExpectedVersion)
		}),
	}...)
}

// Views share only bookkeeping. Service fields never mutate on the shared
// handler, so simultaneous requests keep their original runtime and locks.
func (o *oauthTools) book() *oauthTools {
	if o.owner != nil {
		return o.owner
	}
	return o
}

func (o *oauthTools) nativeView(ctx context.Context, provider string) (*oauthTools, context.Context, error) {
	if o.plugins == nil {
		return o, ctx, nil
	}
	runtime, guarded, err := o.plugins.Native(ctx, provider)
	if err != nil {
		return nil, ctx, err
	}
	epoch := o.plugins.Epoch(provider)
	if err := accessguard.Check(guarded); err != nil {
		return nil, guarded, err
	}
	return &oauthTools{owner: o.book(), plugins: o.plugins, epoch: epoch, store: o.store, copilot: runtime.Copilot, siwc: runtime.SIWC, codex: runtime.Codex, native: runtime.Native, public: runtime.Public, compatible: runtime.Compatible, market: runtime.Market}, guarded, nil
}

// A status request belongs to the flow's original epoch, even when it obtains
// a runtime after an off/on cycle. Retain its caller's authorization as well.
func (o *oauthTools) flowGuardContext(ctx context.Context, flow *oauthFlow) context.Context {
	if o.plugins == nil {
		return ctx
	}
	return accessguard.With(ctx, func() error {
		if err := accessguard.Check(ctx); err != nil {
			return err
		}
		return o.plugins.CheckEpoch(flow.provider, flow.pluginEpoch)
	})
}

// Do not wait for a poll holding f.mu across HTTP: disabling must let that
// response return and persist a rotated credential. Epoch checks reject every
// later send; the pending flow releases its listener once the poll is done.
func (o *oauthTools) CancelProviderFlows(provider string) {
	cutoff := o.plugins.Epoch(provider)
	collect := func() []*oauthFlow {
		var pending []*oauthFlow
		for _, f := range o.book().flows {
			if f.provider == provider && f.pluginEpoch < cutoff {
				pending = append(pending, f)
			}
		}
		return pending
	}
	cancelFlows := func(pending []*oauthFlow) {
		for _, f := range pending {
			cancel := func() {
				if f.state == flowPending {
					f.state, f.errMessage = flowCancelled, "插件已停用，请在插件库启用后重新授权。"
					f.finish()
				}
			}
			if f.mu.TryLock() {
				cancel()
				f.mu.Unlock()
			} else {
				go func() { f.mu.Lock(); defer f.mu.Unlock(); cancel() }()
			}
		}
	}
	if o.book().mu.TryLock() {
		pending := collect()
		o.book().mu.Unlock()
		cancelFlows(pending)
		return
	}
	go func() {
		o.book().mu.Lock()
		pending := collect()
		o.book().mu.Unlock()
		cancelFlows(pending)
	}()
}

func disconnectPluginChannel(ctx context.Context, st *store.Store, manager *plugins.Manager, channel store.Channel, version string) (bool, *bool, string, error) {
	provider := channel.CredentialType
	if store.IsNativeOAuth(provider) || store.IsCompatibleOAuth(provider) {
		if runtime := manager.Existing(provider); runtime != nil && runtime.Native != nil {
			return true, nil, "", runtime.Native.DisconnectChannelAtVersion(ctx, channel.ID, version)
		}
		if err := accessguard.Check(ctx); err != nil {
			return true, nil, "", err
		}
		return true, nil, "", st.DeleteChannelAtVersion(channel.ID, version)
	}
	if provider != store.CredentialCodexOAuth && provider != store.CredentialChatGPTOAuth {
		return false, nil, "", nil
	}
	if err := accessguard.Check(ctx); err != nil {
		return true, nil, "", err
	}
	runtime := manager.Existing(provider)
	if manager.Enabled(provider) {
		var err error
		runtime, ctx, err = manager.Native(ctx, provider)
		if err != nil {
			return true, nil, "", err
		}
		if provider == store.CredentialChatGPTOAuth {
			if version == "" {
				return disconnectChannel(ctx, oauthHandlers{siwc: runtime.SIWC}, channel.ID)
			}
			notice, err := runtime.SIWC.DisconnectChannelAtVersion(ctx, channel.ID, version)
			confirmed := err == nil && notice == ""
			return true, &confirmed, notice, err
		}
		return true, nil, "", runtime.Codex.DisconnectChannelAtVersion(channel.ID, version)
	}
	var err error
	if runtime == nil {
		// No service has ever run, so there is no in-flight credential lock.
		err = st.DeleteChannelAtVersion(channel.ID, version)
	} else if provider == store.CredentialCodexOAuth {
		err = runtime.Codex.DisconnectChannelAtVersion(channel.ID, version)
	} else {
		err = runtime.SIWC.LocalDisconnectChannelAtVersion(ctx, channel.ID, version)
	}
	if provider == store.CredentialChatGPTOAuth {
		confirmed := false
		return true, &confirmed, "插件已停用，仅在本地删除，未尝试远端撤销。", err
	}
	return true, nil, "", err
}

func (h oauthHandlers) operationService(ctx context.Context, remote bool, fallback *codex.Operations) (*codex.Operations, context.Context, error) {
	if h.plugins == nil {
		if fallback == nil {
			return nil, ctx, &ManagementToolError{Status: 503, Message: "凭证操作不可用"}
		}
		return fallback, ctx, nil
	}
	if remote {
		runtime, guarded, err := h.plugins.Native(ctx, store.CredentialCodexOAuth)
		if err != nil {
			return nil, ctx, err
		}
		return runtime.Operations, guarded, nil
	}
	return h.plugins.LocalOperations(store.CredentialCodexOAuth), ctx, nil
}
