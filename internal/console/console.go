package console

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"torouter/internal/antigravity"
	"torouter/internal/claude"
	"torouter/internal/codex"
	"torouter/internal/companion"
	"torouter/internal/copilot"
	"torouter/internal/health"
	"torouter/internal/keydiscovery"
	"torouter/internal/management"
	"torouter/internal/oauthtransfer"
	"torouter/internal/plugins"
	"torouter/internal/quota"
	"torouter/internal/siwc"
	"torouter/internal/store"
	"torouter/internal/updater"
)

// Hooks connect the console to the running process. They are optional so tests
// and other embedders can pass an empty value.
type Hooks struct {
	Shutdown           func()
	Restart            func()
	Companion          func() companion.Snapshot
	OpenCompanion      func() error
	OpenConsole        func() error
	OpenChannelConsole func(int64, string) error
	CloseCompanion     func() error
	CompanionRunning   func() bool
	RuntimeStatus      func() health.StatusSnapshot
	ResetStatus        func(channelID int64, credentialID, model string) error
	RefreshQuota       func(context.Context, store.Channel, store.CredentialItem) error
	CodexOperations    *codex.Operations
	Quotas             *quota.Service
	ObserveQuota       func(store.Channel, store.CredentialItem, quota.Observation)
	UpdateStatus       func() updater.Status
	CheckUpdate        func(context.Context) (updater.Status, error)
	PrepareUpdate      func(context.Context, string, string, string) (UpdateAccepted, func(), error)
}

// oauthHandlers carries the per-track services the console needs outside the
// authorization routes, including credential lifecycle on channel deletion.
type oauthHandlers struct {
	copilot   *copilot.Service
	siwc      *siwc.Service
	codex     *codex.Service
	plugins   *plugins.Manager
	discovery *keydiscovery.Service
	transfer  *oauthtransfer.Service
}

// disconnectChannel removes a channel, retiring its credential at the issuer
// first where there is a way to. removed reports a lifecycle-managed local
// deletion, independently from the optional remote revocation confirmation.
func disconnectChannel(parent context.Context, handlers oauthHandlers, id int64) (removed bool, confirmed *bool, notice string, err error) {
	var st *store.Store
	if handlers.siwc != nil {
		st = handlers.siwc.Store
	} else if handlers.codex != nil {
		st = handlers.codex.Store
	} else {
		return false, nil, "", nil
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	channel, err := st.Channel(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil, "", store.ErrNotFound
		}
		return false, nil, "", err
	}
	if channel.CredentialType == store.CredentialCodexOAuth && handlers.codex != nil {
		return true, nil, "", handlers.codex.DisconnectChannel(id)
	}
	if channel.CredentialType != store.CredentialChatGPTOAuth || handlers.siwc == nil {
		return false, nil, "", nil
	}
	err = handlers.siwc.DisconnectChannel(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil, "", store.ErrNotFound
	}
	if err != nil {
		if _, readErr := st.Channel(id); readErr == nil {
			// A guard can stop the service before its local delete. Do not
			// report a successful deletion merely because revocation failed.
			return true, nil, "", err
		} else if !errors.Is(readErr, store.ErrNotFound) {
			return true, nil, "", readErr
		}
		confirmed := false
		// Which kind of failure decides the wording: the issuer was reached and
		// refused, or it could not be reached at all. Both leave the remote
		// revocation unconfirmed, and the owner is told which one happened.
		warning := "已在本地删除该渠道，远端撤销未确认。"
		if errors.Is(err, siwc.ErrRevokeRefused) {
			warning = "已在本地删除该渠道，但 OpenAI 未接受撤销请求，远端撤销未确认；如有顾虑请在 ChatGPT 账号设置中移除该应用授权。"
		} else {
			warning = "已在本地删除该渠道，但未能联系 OpenAI 或服务暂时不可用，远端撤销未确认；如有顾虑请在 ChatGPT 账号设置中移除该应用授权。"
		}
		return true, &confirmed, warning, nil
	}
	wasRevoked := true
	return true, &wasRevoked, "", nil
}

func New(s *store.Store, origin string, hooks Hooks) http.Handler {
	m, err := plugins.New(s)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fail(w, 503, "无法初始化插件配置") })
	}
	return NewWithPlugins(s, origin, hooks, m)
}

// consoleHandler is the console's router plus the OAuth flow bookkeeping it
// mounts. The return type stays http.Handler for every caller; the concrete
// type exists so ReleaseOAuthListeners can end flows a test left pending.
type consoleHandler struct {
	http.Handler
	oauth      *oauthTools
	management *management.Server
}

// ReleaseOAuthListeners ends every flow the handler still holds, which releases
// its loopback listener. Production calls it after admitted HTTP handlers have
// exited and before closing the store. Tests also use it because a pending or
// failed-but-unpruned flow would hold the pinned callback port (1455/1457) for
// the rest of the package run and make the next test fail to bind.
func ReleaseOAuthListeners(h http.Handler) {
	if c, ok := h.(consoleHandler); ok {
		c.oauth.mu.Lock()
		var flows []*oauthFlow
		for _, f := range c.oauth.flows {
			flows = append(flows, f)
		}
		c.oauth.mu.Unlock()
		for _, f := range flows {
			f.mu.Lock()
			f.finish()
			f.mu.Unlock()
		}
	}
}

// NewWithOAuth injects the OAuth token services. The gateway is constructed with
// the same instances (see internal/app), so a channel has exactly one refresh
// lock no matter which side of the process refreshes it.
func NewWithOAuth(s *store.Store, origin string, hooks Hooks, copilotSvc *copilot.Service, siwcSvc *siwc.Service, codexSvc *codex.Service) http.Handler {
	m, err := plugins.NewInjected(s, copilotSvc, siwcSvc, codexSvc)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fail(w, 503, "无法初始化插件配置") })
	}
	return NewWithPlugins(s, origin, hooks, m)
}

func NewWithPlugins(s *store.Store, origin string, hooks Hooks, manager *plugins.Manager) http.Handler {
	if manager == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fail(w, 503, "无法初始化插件配置") })
	}
	if hooks.Quotas == nil {
		hooks.Quotas = quota.NewWithPlugins(s, manager, nil)
	}
	var oauth *oauthTools
	var mgmt *management.Server
	transfer := oauthtransfer.New(s, func(ctx context.Context, kind string, credential store.OAuthCredential) (store.OAuthCredential, error) {
		switch kind {
		case store.CredentialCodexOAuth:
			return completeCodexImport(ctx, nil, codex.DefaultEndpoints(), credential)
		case store.CredentialClaudeOAuth:
			return claude.NewDriver(nil, claude.Endpoints{}).CompleteImport(ctx, credential)
		case store.CredentialAntigravityOAuth:
			return antigravity.NewDriver(nil, antigravity.DefaultEndpoints).CompleteImport(ctx, credential)
		default:
			return credential, nil
		}
	})
	services := oauthHandlers{plugins: manager, discovery: keydiscovery.New(s, manager, origin, transfer.CompleteImportItems), transfer: transfer}
	build := func(r chi.Router, upstream *upstreamTools) {
		oauth = &oauthTools{store: s, plugins: manager, flows: map[string]*oauthFlow{}}
		manager.OnChange(func(provider string, enabled bool) {
			if !enabled {
				oauth.CancelProviderFlows(provider)
			}
		})
		mountOAuthRoutes(r, oauth)
		pluginRoutes(r, manager)
		marketRoutes(r, manager, oauth)
		keyDiscoveryRoutes(r, services.discovery)
		oauthTransferRoutes(r, services.transfer)
		configBackupRoutes(r, s, hooks, manager)
		tools := newManagementTools(s, origin, hooks, services, oauth, upstream)
		mgmt = tools.server
		mgmt.AdminRoutes(r)
	}
	handler := newConsoleRouter(s, origin, hooks, services, build)
	return consoleHandler{Handler: handler, oauth: oauth, management: mgmt}
}

func newConsoleRouter(s *store.Store, origin string, hooks Hooks, services oauthHandlers, mount func(chi.Router, *upstreamTools)) http.Handler {
	r := chi.NewRouter()
	companionRoutes(r, s, hooks)
	quotaDisplayRoutes(r, s, hooks)
	statusRoutes(r, s, hooks)
	channelDisplayRoutes(r, s)
	operations := hooks.CodexOperations
	if operations == nil && services.codex != nil {
		operations = codex.NewOperations(services.codex)
	}
	credentialOperationRoutes(r, s, operations, services.plugins)
	r.Get("/connection", func(w http.ResponseWriter, r *http.Request) {
		port := 0
		if parsed, err := url.Parse(origin); err == nil {
			port, _ = strconv.Atoi(parsed.Port())
		}
		respond(w, 200, map[string]any{"origin": origin, "port": port})
	})
	clientKeyRoutes(r, s)
	channelCredentialKeyRoutes(r, s)
	modelRoutes(r, s)
	catalogRoutes(r, s)
	tagRoutes(r, s)
	upstream := upstreamRoutes(r, s, services)
	mount(r, upstream)
	r.Get("/runtime/port", func(w http.ResponseWriter, r *http.Request) {
		port, err := s.Port()
		if err != nil {
			fail(w, 500, "cannot read the configured port")
			return
		}
		if port == 0 {
			// Nothing configured yet: report the port actually in use rather
			// than 0, which would make the console show a nonsense value.
			if parsed, parseErr := url.Parse(origin); parseErr == nil {
				port, _ = strconv.Atoi(parsed.Port())
			}
		}
		respond(w, 200, map[string]any{"port": port, "origin": origin})
	})
	r.Put("/runtime/port", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Port int `json:"port"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Port < 1 || body.Port > 65535 {
			fail(w, 400, "端口必须是 1..65535")
			return
		}
		if err := s.SavePort(body.Port); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]any{"port": body.Port, "restart_required": true})
	})
	if hooks.Shutdown != nil {
		r.Post("/shutdown", func(w http.ResponseWriter, r *http.Request) {
			stopAfterResponse(w, r, hooks.Shutdown)
		})
	}
	if hooks.Restart != nil {
		r.Post("/restart", func(w http.ResponseWriter, r *http.Request) {
			stopAfterResponse(w, r, hooks.Restart)
		})
	}
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/channels", func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.ListChannels()
		if err != nil {
			fail(w, 500, "cannot read channels")
			return
		}
		// The console shows one credential type at a time — the API key view,
		// and one card per OAuth provider — so each view asks for exactly the
		// slice it renders instead of filtering client-side. Absent means every
		// channel, which is what any other client gets.
		if want := r.URL.Query().Get("credential_type"); want != "" {
			switch want {
			case store.CredentialAPIKey, store.CredentialCopilotOAuth, store.CredentialChatGPTOAuth, store.CredentialCodexOAuth, store.CredentialClaudeOAuth, store.CredentialAntigravityOAuth, store.CredentialDevinOAuth, store.CredentialZenFree:
			default:
				if !store.IsCompatibleOAuth(want) {
					fail(w, 400, "invalid credential_type")
					return
				}
			}
			kept := make([]store.Channel, 0, len(rows))
			for _, c := range rows {
				// A row written before credential types existed reads back as
				// api_key, so the default type covers the legacy channels too.
				if c.CredentialType == want {
					kept = append(kept, c)
				}
			}
			rows = kept
		}
		prices, err := s.GetPrices()
		if err != nil {
			fail(w, 500, "cannot read associated prices")
			return
		}
		out := []any{}
		for _, c := range rows {
			out = append(out, publicChannelWithPrices(c, prices))
		}
		respond(w, 200, out)
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		c := store.Channel{Enabled: true}
		if !decode(w, r, &c) {
			return
		}
		if r.Method == "POST" && c.CredentialType != "" && c.CredentialType != store.CredentialAPIKey && c.ModelFormats != nil {
			fail(w, 400, "逐模型协议由接入插件获取，不能在普通渠道创建中设置。")
			return
		}
		if err := validateNativeCredentialDraft(s, c); err != nil {
			fail(w, 400, err.Error())
			return
		}
		if r.Method == "POST" && c.CredentialType != "" && c.CredentialType != store.CredentialAPIKey {
			factory, err := store.BuiltinChannel(c.Name, c.CredentialType)
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			factory.Enabled = c.Enabled
			c = factory
		}

		// An OAuth credential is write-only in one direction: it enters through
		// the authorization flow and nowhere else. The console echoes a
		// token-free summary of it, so an empty GitHub token on the way in
		// means "unchanged" (the same rule an empty APIKey follows), and a
		// non-empty one is refused outright — a channel whose credential can be
		// pasted through this API would have no single place where a rotation
		// could keep it consistent.
		if c.OAuth != nil && strings.TrimSpace(c.OAuth.GitHubToken) != "" {
			fail(w, 400, "OAuth 凭证只能通过授权流程写入，API 不能直接修改；更换账号请重新授权。")
			return
		}
		// previous is the stored channel a PUT replaces, empty for a create;
		// the re-enable notice compares against it after the save lands.
		var previous store.Channel
		if r.Method == "PUT" {
			id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
			if err != nil || id < 1 {
				fail(w, 400, "invalid channel id")
				return
			}
			c.ID = id
			rows, err := s.ListChannels()
			if err != nil {
				fail(w, 500, "cannot read channels")
				return
			}
			found := false
			for _, old := range rows {
				if old.ID == id {
					found = true
					previous = old
					if old.CredentialType != "" && old.CredentialType != store.CredentialAPIKey {
						if c.ModelFormats != nil && !reflect.DeepEqual(c.ModelFormats, old.ModelFormats) {
							fail(w, 400, "逐模型协议由接入插件获取，不能直接修改。")
							return
						}
						c.ModelFormats = old.ModelFormats
					}
					if c.APIKey == "" {
						c.APIKey = old.APIKey
					}
					// The empty-token case of the write-only rule above: keep
					// what the store already has.
					if c.OAuth == nil || c.OAuth.GitHubToken == "" {
						c.OAuth = old.OAuth
					}
					// Preserve credentials secrets when empty
					for i := range c.Credentials {
						for _, oldCred := range old.Credentials {
							if c.Credentials[i].ID == oldCred.ID {
								if c.Credentials[i].APIKey == "" {
									c.Credentials[i].APIKey = oldCred.APIKey
								}
								if c.Credentials[i].OAuth == nil {
									c.Credentials[i].OAuth = oldCred.OAuth
								}
								break
							}
						}
						if c.Credentials[i].OAuth == nil && c.Credentials[i].SourceChannelID > 0 {
							if src, err := s.Channel(c.Credentials[i].SourceChannelID); err == nil && src.PrimaryOAuth() != nil {
								copied := *src.PrimaryOAuth()
								c.Credentials[i].OAuth = &copied
								if c.Credentials[i].OAuthInfo == nil {
									info := src.PrimaryOAuth().Public()
									c.Credentials[i].OAuthInfo = &info
								}
							}
						}
					}
					if c.Credentials == nil && len(old.Credentials) > 0 {
						c.Credentials = old.Credentials
					}
					break
				}
			}
			if !found {
				fail(w, 404, "channel not found")
				return
			}
		} else {
			c.ID = 0
			for i := range c.Credentials {
				if c.Credentials[i].OAuth == nil && c.Credentials[i].SourceChannelID > 0 {
					if src, err := s.Channel(c.Credentials[i].SourceChannelID); err == nil && src.PrimaryOAuth() != nil {
						copied := *src.PrimaryOAuth()
						c.Credentials[i].OAuth = &copied
						if c.Credentials[i].OAuthInfo == nil {
							info := src.PrimaryOAuth().Public()
							c.Credentials[i].OAuthInfo = &info
						}
					}
				}
			}
		}
		if r.Method == "PUT" && c.Credentials != nil && len(c.Credentials) == 0 {
			c.APIKey, c.OAuth = "", nil
		}
		if err := s.SaveChannelConfig(&c); err != nil {
			if errors.Is(err, store.ErrEditConflict) {
				fail(w, 409, "渠道凭证或连接已变化，请重新读取后保存。")
				return
			}
			fail(w, 400, err.Error())
			return
		}
		// A save that turns a model back on while the catalogue no longer lists it
		// is answered with a note, not a refusal: the enable is the operator's
		// call, and the catalogue only reports the last fetch (channelReenableNotice).
		if notice := channelReenableNotice(previous, c); notice != "" {
			answer := map[string]any{}
			raw, _ := json.Marshal(publicChannel(c))
			_ = json.Unmarshal(raw, &answer)
			answer["notice"] = notice
			respond(w, 200, answer)
			return
		}
		respond(w, 200, publicChannel(c))
	}
	credentialMoveRoutes(r, s)
	r.Post("/channels", save)
	r.Put("/channels/{id}", save)
	r.Delete("/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "invalid channel id")
			return
		}
		// Deleting a channel also retires its credential where the issuer offers
		// a way to. The local row goes either way — the operator asked for it to
		// go — and a revoke that could not be confirmed is reported instead of
		// being claimed as clean. The handlers are the constructor's parameters
		// captured by this closure, so two consoles in one process never share
		// (or race over) one set of services.
		var removed bool
		var confirmed *bool
		var notice string
		var revokeErr error
		if services.plugins != nil {
			channel, readErr := s.Channel(id)
			if readErr != nil {
				fail(w, 404, "channel not found")
				return
			}
			removed, confirmed, notice, revokeErr = disconnectPluginChannel(r.Context(), s, services.plugins, channel, "")
		} else {
			removed, confirmed, notice, revokeErr = disconnectChannel(r.Context(), services, id)
		}
		if revokeErr != nil {
			if pluginError(w, revokeErr) {
				return
			}
			if errors.Is(revokeErr, store.ErrNotFound) {
				fail(w, 404, "channel not found")
			} else {
				fail(w, 500, "delete failed")
			}
			return
		}
		if !removed {
			// No revoke applied to this track: the plain delete is the whole job.
			if err = s.DeleteChannel(id); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					fail(w, 404, "channel not found")
				} else {
					fail(w, 500, "delete failed")
				}
				return
			}
		}
		answer := map[string]any{"ok": true}
		if confirmed != nil {
			answer["revocation_confirmed"] = *confirmed
		}
		if notice != "" {
			answer["warning"] = notice
		}
		respond(w, 200, answer)
	})
	exchangeRoutes(r, s)
	planPricesRoutes(r, s)
	r.Get("/prices", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.GetPrices()
		if err != nil {
			fail(w, 500, "cannot read prices")
			return
		}
		respond(w, 200, v)
	})
	r.Put("/prices", func(w http.ResponseWriter, r *http.Request) {
		var v []store.Price
		if !decode(w, r, &v) {
			return
		}
		if v == nil {
			fail(w, 400, "prices must be an array")
			return
		}
		if err := s.SavePrices(v); err != nil {
			fail(w, 400, err.Error())
			return
		}
		v, err := s.GetPrices()
		if err != nil {
			fail(w, 500, "cannot read saved prices")
			return
		}
		respond(w, 200, v)
	})
	quotaRefreshRoutes(r, s)
	appUpdateRoutes(r, hooks)
	oauthAutoOpenRoutes(r, s)
	r.Get("/settings", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.GetSettings()
		if err != nil {
			fail(w, 500, "cannot read settings")
			return
		}
		respond(w, 200, v)
	})
	r.Put("/settings", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			store.Settings
			Prices json.RawMessage `json:"prices"`
		}
		if !decode(w, r, &body) {
			return
		}
		if len(body.Prices) != 0 {
			fail(w, 400, "prices must be saved through the price API")
			return
		}
		v := body.Settings
		if err := s.SaveSettings(v); err != nil {
			fail(w, 400, err.Error())
			return
		}
		if err := s.Cleanup(); err != nil {
			fail(w, 500, "settings saved but retention cleanup failed")
			return
		}
		v, err := s.GetSettings()
		if err != nil {
			fail(w, 500, "cannot read saved settings")
			return
		}
		respond(w, 200, v)
	})
	r.Get("/logs", func(w http.ResponseWriter, r *http.Request) {
		f, err := filter(r)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		if f.Limit == 0 {
			f.Limit = 200
		}
		rows, err := s.ListUsage(f)
		if err != nil {
			fail(w, 500, "cannot read logs")
			return
		}
		respond(w, 200, rows)
	})
	r.Get("/usage/export", usageExportHandler(s))
	r.Get("/usage", func(w http.ResponseWriter, r *http.Request) {
		f, err := filter(r)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		summary, err := s.SummarizeUsage(r.Context(), f)
		if err != nil {
			fail(w, 500, "cannot read usage")
			return
		}
		respond(w, 200, summary)
	})
	return r
}

// channelReenableNotice names the models a save turns back on while the
// channel's catalogue no longer lists them. Like the downstream-side notice
// (downstreamReenableNotice) it is a statement about the last fetch, not a
// verdict on the model: the catalogue said nothing about it this time, so the
// wording covers a rename, a retirement, and a fetch that did not see it, and
// the enable goes through either way.
func channelReenableNotice(previous, updated store.Channel) string {
	if previous.ID == 0 {
		// A brand-new channel has no enabled-model history to compare against.
		return ""
	}
	missing := []string{}
	for _, m := range updated.Models {
		if slices.Contains(previous.Models, m) || slices.Contains(updated.AvailableModels, m) {
			continue
		}
		if !slices.Contains(missing, m) {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "上游当前目录中没有模型 " + strings.Join(missing, "、") + "（上游可能已下线或已更名，也可能这次未拉取到）；已照常启用，请自行确认可用性。"
}

func maskKey(k string) string {
	return store.MaskKey(k)
}

// publicChannel is what the console shows: the channel with its credential
// replaced by the flags that say one exists. An OAuth channel additionally
// carries a token-free summary (account, status, expiry, api base) so the UI
// can say why it is disabled or when it was last refreshed.
func publicChannel(c store.Channel) any {
	return publicChannelWithPrices(c, nil)
}

func publicChannelWithPrices(c store.Channel, prices []store.Price) any {
	// Empty inventories are lists in the management contract. A newly
	// authorized/public channel has no selected models, not an absent model
	// property; keep Web and MCP projections consistent.
	if c.Models == nil {
		c.Models = []string{}
	}
	if c.AvailableModels == nil {
		c.AvailableModels = []string{}
	}
	has := false
	var oauth *store.OAuthInfo
	if len(c.Credentials) > 0 {
		creds := make([]store.CredentialItem, len(c.Credentials))
		for i, cred := range c.Credentials {
			creds[i] = cred
			creds[i].HasKey = cred.APIKey != ""
			if cred.APIKey != "" {
				creds[i].MaskedKey = maskKey(cred.APIKey)
			}
			creds[i].APIKey = ""
			if cred.OAuth != nil {
				creds[i].HasOAuth = true
				info := cred.OAuth.Public()
				creds[i].OAuthInfo = &info
				creds[i].OAuth = nil
			}
		}
		c.Credentials = creds
		has = c.Credentials[0].HasKey
		if c.Credentials[0].HasOAuth {
			oauth = c.Credentials[0].OAuthInfo
		}
	} else if c.APIKey != "" {
		has = true
		c.Credentials = []store.CredentialItem{{
			ID:        "cred_default",
			Label:     "默认凭据",
			HasKey:    true,
			MaskedKey: maskKey(c.APIKey),
			Enabled:   true,
		}}
	} else if c.OAuth != nil {
		info := c.OAuth.Public()
		oauth = &info
		c.Credentials = []store.CredentialItem{{
			ID:        "cred_default",
			Label:     "默认凭据",
			HasOAuth:  true,
			OAuthInfo: &info,
			Enabled:   true,
		}}
	}
	c.APIKey = ""
	c.OAuth = nil
	return struct {
		store.Channel
		AssociatedPrices []store.Price    `json:"associated_prices"`
		HasKey           bool             `json:"has_key"`
		HasOAuth         bool             `json:"has_oauth"`
		OAuth            *store.OAuthInfo `json:"oauth,omitempty"`
	}{c, channelPrices(prices, c.ID), has, oauth != nil, oauth}
}

func channelPrices(prices []store.Price, id int64) []store.Price {
	out := []store.Price{}
	for _, p := range prices {
		if p.ChannelID == id {
			out = append(out, p)
		}
	}
	return out
}

// Chi matches RawPath when present; otherwise URL.Path is already decoded.
// Unescaping only the former also preserves literal percent sequences in IDs.
func decodedPathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if r.URL.RawPath != "" {
		var err error
		id, err = url.PathUnescape(id)
		if err != nil {
			fail(w, 400, "invalid escaped ID")
			return "", false
		}
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid JSON body: "+err.Error())
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, 400, "body must contain one JSON object")
		return false
	}
	return true
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"message": message}})
}
func filter(r *http.Request) (store.UsageFilter, error) {
	f := store.UsageFilter{}
	q := r.URL.Query()
	for _, entry := range []struct {
		key string
		dst *time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if text := q.Get(entry.key); text != "" {
			v, err := time.Parse(time.RFC3339, text)
			if err != nil {
				v, err = time.Parse("2006-01-02", text)
				if err != nil {
					return f, errors.New("invalid date")
				}
				if entry.key == "to" {
					v = v.Add(24*time.Hour - time.Millisecond)
				}
			}
			*entry.dst = v
		}
	}
	if !f.From.IsZero() && !f.To.IsZero() && f.From.After(f.To) {
		return f, errors.New("from must not be later than to")
	}
	f.Model = q.Get("model")
	f.RequestID = q.Get("request_id")
	f.Timezone = q.Get("timezone")
	if _, err := store.UsageLocation(f.Timezone); err != nil {
		return f, err
	}
	if v := q.Get("channel_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return f, errors.New("invalid channel_id")
		}
		f.ChannelID = n
	}
	for _, entry := range []struct {
		key string
		dst *int
	}{{"limit", &f.Limit}, {"status", &f.Status}} {
		if v := q.Get(entry.key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return f, errors.New("invalid " + entry.key)
			}
			*entry.dst = n
		}
	}
	return f, nil
}

// emptyControlBody consumes a bounded, optional empty JSON object. An unread
// POST body can reset a Connection: close response even without a shutdown.
func emptyControlBody(w http.ResponseWriter, r *http.Request) bool {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
	var body []byte
	var err error
	if r.Body != nil {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	}
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		fail(w, status, "invalid control request body")
		return false
	}
	if strings.TrimSpace(string(body)) != "" {
		var parameters map[string]json.RawMessage
		if json.Unmarshal(body, &parameters) != nil || parameters == nil || len(parameters) != 0 {
			fail(w, http.StatusBadRequest, "control request body must be an empty JSON object")
			return false
		}
	}
	return true
}

// stopAfterResponse flushes the complete acknowledgment before scheduling a stop.
func stopAfterResponse(w http.ResponseWriter, r *http.Request, action func()) {
	if !emptyControlBody(w, r) {
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len("{\"ok\":true}\n")))
	respond(w, 200, map[string]bool{"ok": true})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		action()
	}()
}

// ManagementHandler is mounted outside the Web cookie boundary. Its own
// middleware accepts only independent management credentials.
func ManagementHandler(h http.Handler) http.Handler {
	if c, ok := h.(consoleHandler); ok && c.management != nil {
		return c.management.Handler()
	}
	return http.NotFoundHandler()
}
