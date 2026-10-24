package console

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"torouter/internal/accessguard"
	"torouter/internal/marketbridge"
	"torouter/internal/marketprofile"
	"torouter/internal/pluginhost"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

func marketCodeAllowed(w http.ResponseWriter, r *http.Request) bool {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	host, parseErr := url.Parse("http://" + r.Host)
	if err != nil || net.ParseIP(peer) == nil || !net.ParseIP(peer).IsLoopback() || parseErr != nil ||
		(host.Hostname() != "localhost" && (net.ParseIP(host.Hostname()) == nil || !net.ParseIP(host.Hostname()).IsLoopback())) {
		fail(w, 403, "插件安装和卸载请在本机控制台操作。")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
		fail(w, 403, "请求来源不符。")
		return false
	}
	return true
}

func marketError(w http.ResponseWriter, err error) {
	if pluginError(w, err) {
		return
	}
	var remote *pluginhost.RemoteError
	if errors.As(err, &remote) {
		fail(w, 400, Redact(firstLine(remote.Message)))
		return
	}
	var public *ManagementToolError
	if errors.As(err, &public) {
		fail(w, public.Status, public.Message)
		return
	}
	// Installer/host errors are concise controlled messages; path-bearing OS
	// errors are not appropriate for a public console response.
	fail(w, 400, Redact(firstLine(err.Error())))
}

func marketRoutes(r chi.Router, m *plugins.Manager, o *oauthTools) {
	r.Get("/plugins/market", func(w http.ResponseWriter, r *http.Request) {
		out, err := m.MarketCatalogue(r.Context(), r.URL.Query().Get("q"))
		if err != nil {
			marketError(w, err)
			return
		}
		respond(w, 200, out)
	})
	r.Post("/plugins/market/refresh", func(w http.ResponseWriter, r *http.Request) {
		query:=r.URL.Query().Get("q")
		if err := m.RefreshMarket(r.Context(),query); err != nil {
			marketError(w, err)
			return
		}
		out, err := m.MarketCatalogue(r.Context(), query)
		respondManagementTool(w, out, err)
	})
	r.Post("/plugins/market/install", func(w http.ResponseWriter, r *http.Request) {
		if !marketCodeAllowed(w, r) {
			return
		}
		var in struct {
			Package string `json:"package"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := m.InstallMarket(r.Context(), in.Package)
		if err != nil {
			marketError(w, err)
			return
		}
		respond(w, 200, out)
	})
	r.Delete("/plugins/market/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !marketCodeAllowed(w, r) {
			return
		}
		if err := m.RemoveMarket(r.Context(), chi.URLParam(r, "id")); err != nil {
			marketError(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	r.Post("/plugins/market/{id}/auth/{provider}/start", func(w http.ResponseWriter, r *http.Request) {
		var in marketStartInput
		if !decode(w, r, &in) {
			return
		}
		out, err := o.startMarketCore(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "provider"), in)
		if err != nil {
			marketError(w, err)
			return
		}
		respond(w, 200, out)
	})
	r.Get("/plugins/market/auth/{flow}", func(w http.ResponseWriter, r *http.Request) {
		out, err := o.statusMarketCore(r.Context(), chi.URLParam(r, "flow"), nil, false)
		if err != nil {
			marketError(w, err)
			return
		}
		respond(w, 200, out)
	})
	r.Post("/plugins/market/auth/{flow}/complete", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := o.statusMarketCore(r.Context(), chi.URLParam(r, "flow"), &in.Code, false)
		if err != nil {
			marketError(w, err)
			return
		}
		respond(w, 200, out)
	})
	r.Delete("/plugins/market/auth/{flow}", func(w http.ResponseWriter, r *http.Request) {
		out, err := o.statusMarketCore(r.Context(), chi.URLParam(r, "flow"), nil, true)
		if err != nil {
			marketError(w, err)
			return
		}
		respond(w, 200, out)
	})
}

type marketStartInput struct {
	Method       int               `json:"method"`
	Inputs       map[string]string `json:"inputs"`
	ChannelID    int64             `json:"channel_id,omitempty"`
	CredentialID string            `json:"credential_id,omitempty"`
}
type marketConsentResult struct {
	state  json.RawMessage
	models []marketprofile.Model
	err    error
}
type marketConsent struct {
	worker  *pluginhost.Worker
	service *marketbridge.Service
	auth    marketprofile.Authorization
	target  store.PluginConsentTarget
	account string
	ctx     context.Context
	cancel  context.CancelFunc
	result  chan marketConsentResult
	started bool
}

func (o *oauthTools) startMarketCore(ctx context.Context, id, provider string, in marketStartInput) (any, error) {
	if o.plugins == nil {
		return nil, plugins.ErrInvalid
	}
	svc, ctx, err := o.plugins.MarketProvider(ctx, id, provider)
	if err != nil {
		return nil, err
	}
	valid := false
	for _, method := range svc.Provider.Methods {
		if method.Index == in.Method {
			valid = true
		}
	}
	if !valid {
		return nil, &ManagementToolError{400, "请选择供应商提供的登录方式。"}
	}
	target := store.PluginConsentTarget{}
	account := ""
	if in.ChannelID < 0 || in.ChannelID == 0 && in.CredentialID != "" {
		return nil, store.ErrEditConflict
	}
	if in.ChannelID > 0 {
		c, err := o.store.Channel(in.ChannelID)
		if err != nil || c.CredentialType != svc.Type {
			return nil, &ManagementToolError{400, "所选渠道接入方式不匹配。"}
		}
		target.ChannelID = in.ChannelID
		target.Version = store.CatalogueSourceVersion(c)
		if in.CredentialID != "" {
			exact, err := exactOAuthCredential(c, in.CredentialID)
			if err != nil {
				return nil, err
			}
			target.CredentialID = in.CredentialID
			target.Version = store.CompatibleOAuthIdentity(c.CredentialType, exact.OAuth)
			account = exact.OAuth.Account
		}
	}
	defer o.startLock(svc.Type)()
	o.book().mu.Lock()
	o.prune()
	if len(o.book().flows) >= maxOpenFlows {
		o.book().mu.Unlock()
		return nil, &ManagementToolError{429, "请先完成或取消现有授权。"}
	}
	o.book().mu.Unlock()
	epoch := o.plugins.Epoch(svc.Type)
	background, cancel := context.WithTimeout(context.Background(), browserFlowWindow)
	actor, _ := ctx.Value(oauthContinuationKey{}).(func(context.Context) error)
	background = accessguard.With(background, func() error {
		if actor != nil {
			if err := actor(context.Background()); err != nil {
				return err
			}
		}
		return o.plugins.CheckEpoch(svc.Type, epoch)
	})
	m := &marketConsent{service: svc, target: target, account: account, ctx: background, cancel: cancel, result: make(chan marketConsentResult, 1)}
	w, err := pluginhost.StartMarket(ctx, svc.RuntimePath, svc.ModulePath, provider, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	m.worker = w
	raw, err := w.Invoke(ctx, "authorize", map[string]any{"method": in.Method, "inputs": in.Inputs}, marketConsentHandler(provider, new(json.RawMessage)))
	if err != nil {
		cancel()
		_ = w.Close()
		return nil, err
	}
	if err = json.Unmarshal(raw, &m.auth); err != nil {
		cancel()
		_ = w.Close()
		return nil, errors.New("供应商授权响应无效")
	}
	flowID, err := newFlowID()
	if err != nil {
		cancel()
		_ = w.Close()
		return nil, err
	}
	f := &oauthFlow{id: flowID, provider: svc.Type, pluginEpoch: epoch, state: flowPending, expiresAt: time.Now().Add(browserFlowWindow), market: m}
	o.book().mu.Lock()
	o.book().flows[flowID] = f
	o.book().mu.Unlock()
	if len(m.auth.State) > 0 {
		m.start(m.auth.State, nil)
	} else if m.auth.Method == "auto" {
		m.start(nil, nil)
	}
	go func() {
		<-background.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.state == flowPending {
			f.state = flowError
			f.errMessage = "授权已到期或被取消，请重新开始。"
			f.finish()
		}
	}()
	return marketFlowPayload(f), nil
}

func marketConsentHandler(provider string, state *json.RawMessage) pluginhost.Handler {
	return func(ctx context.Context, e pluginhost.Event) (json.RawMessage, error) {
		if e.Name == "network" {
			return json.RawMessage(`{}`), accessguard.Check(ctx)
		}
		if e.Name != "auth" {
			return nil, pluginhost.ErrProtocol
		}
		var event struct {
			Provider string          `json:"provider_id"`
			Value    json.RawMessage `json:"value"`
		}
		if json.Unmarshal(e.Payload, &event) != nil || event.Provider != provider {
			return nil, pluginhost.ErrProtocol
		}
		next, err := marketprofile.State(string(event.Value))
		if err != nil {
			return nil, err
		}
		*state = json.RawMessage(next)
		return *state, nil
	}
}

// A background callback only acquires credentials. The foreground status call
// still checks the current owner/permission and plugin epoch before committing.
func (m *marketConsent) start(state json.RawMessage, code *string) {
	if m.started {
		return
	}
	m.started = true
	go func() {
		result := marketConsentResult{state: state}
		handler := marketConsentHandler(m.service.Provider.ID, &result.state)
		if len(result.state) == 0 {
			input := map[string]any{}
			if code != nil {
				input["code"] = *code
			}
			result.state, result.err = m.worker.Invoke(m.ctx, "complete", input, handler)
		}
		if result.err == nil {
			var raw json.RawMessage
			raw, result.err = m.worker.Invoke(m.ctx, "models", nil, handler)
			if result.err == nil {
				result.err = json.Unmarshal(raw, &result.models)
			}
		}
		m.result <- result
	}()
}

func (o *oauthTools) statusMarketCore(ctx context.Context, id string, code *string, cancel bool) (any, error) {
	if err := checkOAuthFlowAccess(ctx, id); err != nil {
		return nil, err
	}
	o.book().mu.Lock()
	f := o.book().flows[id]
	o.book().mu.Unlock()
	if f == nil || f.market == nil {
		return nil, &ManagementToolError{404, "授权流程不存在或已过期。"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != flowPending {
		return marketFlowPayload(f), nil
	}
	if cancel {
		f.state = flowCancelled
		f.finish()
		return marketFlowPayload(f), nil
	}
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if err := o.plugins.CheckEpoch(f.provider, f.pluginEpoch); err != nil {
		f.state = flowError
		f.errMessage = "插件状态已变化，请重新开始授权。"
		f.finish()
		return marketFlowPayload(f), nil
	}
	if code != nil {
		f.market.start(nil, code)
	}
	select {
	case result := <-f.market.result:
		if result.err == nil {
			initial, err := marketbridge.Catalogue(f.market.service.Provider.Name, f.provider, result.models)
			var credential store.OAuthCredential
			if err == nil {
				credential, err = store.MarketOAuthCredential(f.provider, string(result.state), f.market.account)
			}
			if err == nil {
				var c store.Channel
				c, _, err = o.store.CommitMarketConsent(initial, credential, f.market.target, func() error {
					if err := accessguard.Check(ctx); err != nil {
						return err
					}
					return o.plugins.CheckEpoch(f.provider, f.pluginEpoch)
				})
				if err == nil {
					f.state = flowComplete
					f.channelID = c.ID
				}
			}
			result.err = err
		}
		if result.err != nil {
			f.state = flowError
			f.errMessage = "供应商授权未完成，请检查登录方式或更新插件后重试。"
			var remote *pluginhost.RemoteError
			if errors.As(result.err, &remote) {
				f.errMessage = Redact(firstLine(remote.Message))
			}
			if errors.Is(result.err, store.ErrEditConflict) {
				f.errMessage = "渠道或凭证已变化，请重新打开后授权。"
			}
		}
		f.finish()
	default:
	}
	return marketFlowPayload(f), nil
}
func marketFlowPayload(f *oauthFlow) map[string]any {
	out := map[string]any{"flow_id": f.id, "status": "pending"}
	switch f.state {
	case flowComplete:
		out["status"] = "success"
		out["channel_id"] = f.channelID
	case flowError:
		out["status"] = "error"
		out["error"] = f.errMessage
	case flowCancelled:
		out["status"] = "cancelled"
	default:
		out["url"] = f.market.auth.URL
		out["instructions"] = f.market.auth.Instructions
		out["method"] = f.market.auth.Method
		if f.market.started {
			out["method"] = "auto"
		}
	}
	return out
}
