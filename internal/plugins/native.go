package plugins

import (
	"context"
	"log/slog"
	"net/http"

	"torouter/internal/accessguard"
	"torouter/internal/antigravity"
	"torouter/internal/claude"
	"torouter/internal/codex"
	"torouter/internal/compatprofile"
	"torouter/internal/copilot"
	"torouter/internal/devin"
	"torouter/internal/marketbridge"
	"torouter/internal/nativeoauth"
	"torouter/internal/pluginhost"
	"torouter/internal/siwc"
	"torouter/internal/store"
	"torouter/internal/zenfree"
)

type Runtime interface {
	Bearer(context.Context, store.Channel) (string, error)
}

type Executor interface {
	Execute(*http.Client, *http.Request, store.Channel) (*http.Response, error)
}

// NativeRuntime shares the existing services and their credential locks across
// management, requests and background operations, including after re-enabling.
type NativeRuntime struct {
	Runtime
	Copilot    *copilot.Service
	SIWC       *siwc.Service
	Codex      *codex.Service
	Native     *nativeoauth.Service
	Public     *zenfree.Service
	Compatible *pluginhost.Service
	Market     *marketbridge.Service
	Operations *codex.Operations
	background func(context.Context, <-chan struct{})
}

func (r *NativeRuntime) Run(ctx context.Context, stop <-chan struct{}) {
	if r.background != nil {
		r.background(ctx, stop)
	} else if r.Operations != nil {
		r.Operations.RunUntil(ctx, stop)
	}
}

// The captured access context prevents an old resolved handle from being used
// after an off/on cycle, even if its caller supplies another request context.
// Check only before the operation: a rotated token already returned by the
// provider must still be durably stored by the existing service.
type resolvedRuntime struct {
	Runtime
	access context.Context
}

// A returned authentication refusal belongs to the exact request already sent.
// Keep provider feedback available through the access wrapper without requiring
// other providers to implement Codex's access-only credential handling.
func (r resolvedRuntime) RejectAccessOnly(channel store.Channel, credentialID, token string) error {
	if feedback, ok := r.Runtime.(interface {
		RejectAccessOnly(store.Channel, string, string) error
	}); ok {
		return feedback.RejectAccessOnly(channel, credentialID, token)
	}
	return nil
}

func (r resolvedRuntime) Bearer(ctx context.Context, channel store.Channel) (string, error) {
	guarded := accessguard.With(ctx, func() error {
		if err := accessguard.Check(ctx); err != nil {
			return err
		}
		return accessguard.Check(r.access)
	})
	if err := guarded.Err(); err != nil {
		return "", err
	}
	if err := accessguard.Check(guarded); err != nil {
		return "", err
	}
	return r.Runtime.Bearer(guarded, channel)
}

func (r resolvedRuntime) Execute(client *http.Client, req *http.Request, channel store.Channel) (*http.Response, error) {
	parent := req.Context()
	ctx := accessguard.With(parent, func() error {
		if err := accessguard.Check(parent); err != nil {
			return err
		}
		return accessguard.Check(r.access)
	})
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	if driver, ok := r.Runtime.(Executor); ok {
		return driver.Execute(client, req, channel)
	}
	return accessguard.Do(client, req)
}

func New(st *store.Store, compatible ...*pluginhost.Installation) (*Manager, error) {
	m, err := NewInjected(st, nil, nil, nil)
	if err == nil && len(compatible) > 0 {
		m.compatible = compatible[0]
	}
	return m, err
}

// Non-nil injected services are an explicit initial enable choice. Existing
// persisted choices always win, and wrappers/operations are still made lazily.
func NewInjected(st *store.Store, copilotSvc *copilot.Service, siwcSvc *siwc.Service, codexSvc *codex.Service) (*Manager, error) {
	return newManager(st, copilotSvc, siwcSvc, codexSvc, nil, nil)
}

// NewWithNativeServices injects known native drivers without changing their
// persisted capability choices. It does not register arbitrary provider code.
func NewWithNativeServices(st *store.Store, services map[string]*nativeoauth.Service) (*Manager, error) {
	return newManager(st, nil, nil, nil, services, nil)
}

func NewWithZenFreeService(st *store.Store, service *zenfree.Service) (*Manager, error) {
	if service == nil || service.Store != st {
		return nil, ErrInvalid
	}
	return newManager(st, nil, nil, nil, nil, service)
}

func newManager(st *store.Store, copilotSvc *copilot.Service, siwcSvc *siwc.Service, codexSvc *codex.Service, nativeServices map[string]*nativeoauth.Service, publicService *zenfree.Service) (*Manager, error) {
	if st == nil {
		return nil, ErrInvalid
	}
	nativeCopy := make(map[string]*nativeoauth.Service, len(nativeServices))
	for credentialType, service := range nativeServices {
		if service == nil || service.Store != st || service.Definition().CredentialType != credentialType || !store.IsNativeOAuth(credentialType) {
			return nil, ErrInvalid
		}
		nativeCopy[credentialType] = service
	}
	nativeServices = nativeCopy
	m := &Manager{store: st, entries: map[string]*entry{}, byType: map[string]*entry{}, localOperations: codex.NewLocalOperations(st)}
	register := func(id, name, description, icon, credentialType string, enabled, background bool, factory func() *NativeRuntime) {
		e := &entry{info: Info{ID: id, Name: name, Description: description, Icon: icon, Implementation: "native", Featured: true, Version: "1", CredentialType: credentialType},
			credentialType: credentialType, capability: "connect", enabled: enabled, epoch: 1, changed: make(chan struct{}), background: background, factory: factory}
		m.entries[id], m.byType[credentialType] = e, e
		m.ordered = append(m.ordered, e)
	}
	register("codex", "Codex", "通过 Codex OAuth 连接模型", "codex", store.CredentialCodexOAuth, true, true, func() *NativeRuntime {
		if codexSvc == nil {
			codexSvc = codex.NewService(st, nil, codex.DefaultEndpoints())
		}
		return &NativeRuntime{Runtime: codexSvc, Codex: codexSvc, Operations: m.localOperations.WithService(codexSvc)}
	})
	register("copilot", "GitHub Copilot", "通过 GitHub Copilot OAuth 连接模型", "githubcopilot", store.CredentialCopilotOAuth, copilotSvc != nil, false, func() *NativeRuntime {
		if copilotSvc == nil {
			copilotSvc = copilot.NewService(st, nil, copilot.DefaultEndpoints())
		}
		return &NativeRuntime{Runtime: copilotSvc, Copilot: copilotSvc}
	})
	register("chatgpt", "ChatGPT", "通过 ChatGPT SIWC 授权连接模型", "openai", store.CredentialChatGPTOAuth, siwcSvc != nil, false, func() *NativeRuntime {
		if siwcSvc == nil {
			siwcSvc = siwc.NewService(st, nil, siwc.DefaultEndpoints())
		}
		return &NativeRuntime{Runtime: siwcSvc, SIWC: siwcSvc}
	})
	register("claude", "Claude 订阅", "独立授权，使用订阅的原生请求身份；模型列表为维护目录", "claude", store.CredentialClaudeOAuth, true, false, func() *NativeRuntime {
		service := nativeServices[store.CredentialClaudeOAuth]
		if service == nil {
			service = nativeoauth.NewService(st, claude.NewDriver(nil, claude.Endpoints{}))
		}
		return &NativeRuntime{Runtime: service, Native: service}
	})
	register("antigravity", "Antigravity", "独立 Google 授权，连接账号可用模型", "antigravity", store.CredentialAntigravityOAuth, true, false, func() *NativeRuntime {
		service := nativeServices[store.CredentialAntigravityOAuth]
		if service == nil {
			service = nativeoauth.NewService(st, antigravity.NewDriver(nil, antigravity.DefaultEndpoints))
		}
		return &NativeRuntime{Runtime: service, Native: service}
	})
	register("opencode-zen-free", "OpenCode Zen Free", "公共免费接入，按模型选择协议；需客户端提供其要求的工具", "opencode", store.CredentialZenFree, false, false, func() *NativeRuntime {
		if publicService == nil {
			publicService = zenfree.NewService(st, nil, zenfree.Endpoints{})
		}
		return &NativeRuntime{Runtime: publicService, Public: publicService}
	})
	register("devin", "Devin", "独立授权订阅账号，支持文本、函数工具和完整上下文；不支持托管工具或严格结构化输出", "devin", store.CredentialDevinOAuth, false, false, func() *NativeRuntime {
		service := nativeServices[store.CredentialDevinOAuth]
		if service == nil {
			service = nativeoauth.NewService(st, devin.NewDriver(nil, devin.Endpoints{}))
		}
		return &NativeRuntime{Runtime: service, Native: service}
	})
	for _, profile := range compatprofile.Profiles() {
		register(profile.ID, profile.Name, "通过固定兼容包独立登录；需要 Node.js 24，不读取原工具账号", profile.Icon, profile.CredentialType, false, false, func() *NativeRuntime {
			service := pluginhost.NewService(st, m.compatible, profile)
			return &NativeRuntime{Runtime: service, Compatible: service}
		})
		m.entries[profile.ID].info.Implementation = "compatible"
		m.entries[profile.ID].info.Version = profile.Version
	}
	m.registerSources()
	defaults, legacy := map[string]map[string]bool{}, map[string]string{}
	for _, e := range m.ordered {
		defaults[e.info.ID] = map[string]bool{e.capability: e.enabled}
		if e.capability == "connect" {
			legacy[e.info.ID] = e.credentialType
		}
	}
	preferences, err := st.InitPluginPreferences(defaults, legacy)
	if err != nil {
		return nil, err
	}
	for _, e := range m.ordered {
		e.enabled = preferences.Plugins[e.info.ID][e.capability]
	}
	st.OnConfigRestore(func() {
		if err := m.Reload(); err != nil {
			slog.Error("failed to reload native plugins on config restore", "error", err)
		}
	})
	return m, nil
}
