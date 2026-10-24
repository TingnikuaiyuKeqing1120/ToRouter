package pluginhost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/compatprofile"
	"torouter/internal/nativeoauth"
	"torouter/internal/store"
	"torouter/internal/streamreply"
)

type Service struct {
	Store   *store.Store
	Profile compatprofile.Profile
	launch  func(context.Context, compatprofile.Profile, string) (*Worker, error)
	gates   sync.Map
	blocked sync.Map // state versions with an unacknowledged rotation
}

func NewService(st *store.Store, install *Installation, p compatprofile.Profile) *Service {
	return &Service{Store: st, Profile: p, launch: install.Launch}
}

func check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return accessguard.Check(ctx)
}

func (s *Service) acquire(ctx context.Context, id int64, credentialID string) (func(), error) {
	key := fmt.Sprintf("%d/%s", id, credentialID)
	value, _ := s.gates.LoadOrStore(key, make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case gate <- struct{}{}:
		lock := s.Store.CredentialLifecycleMutex(id)
		for !lock.TryLock() {
			select {
			case <-ctx.Done():
				<-gate
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		var once sync.Once
		return func() { once.Do(func() { lock.Unlock(); <-gate }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) live(ctx context.Context, selected store.Channel) (store.OAuthCredential, string, error) {
	if err := check(ctx); err != nil {
		return store.OAuthCredential{}, "", err
	}
	if selected.CredentialType != s.Profile.CredentialType || len(selected.Credentials) != 1 || selected.Credentials[0].OAuth == nil {
		return store.OAuthCredential{}, "", store.ErrNativeOAuthIdentity
	}
	c, err := s.Store.Channel(selected.ID)
	if err != nil {
		return store.OAuthCredential{}, "", err
	}
	item := selected.Credentials[0]
	identity := store.CompatibleOAuthIdentity(s.Profile.CredentialType, item.OAuth)
	if identity == "" || c.CredentialType != s.Profile.CredentialType {
		return store.OAuthCredential{}, "", store.ErrNativeOAuthIdentity
	}
	for _, live := range c.Credentials {
		if live.ID != item.ID || store.CompatibleOAuthIdentity(c.CredentialType, live.OAuth) != identity {
			continue
		}
		if !c.Enabled || !live.Enabled {
			return store.OAuthCredential{}, "", errors.New("渠道或所选凭证已停用")
		}
		if live.OAuth.Status != "" && live.OAuth.Status != store.OAuthStatusActive {
			return store.OAuthCredential{}, "", errors.New("授权需要重新登录")
		}
		key := fmt.Sprintf("%d/%s", c.ID, live.ID)
		if version, ok := s.blocked.Load(key); ok {
			if version == store.PluginStateVersion(live.OAuth) {
				return store.OAuthCredential{}, "", errors.New("插件续期结果未能确认，请重新独立授权")
			}
			s.blocked.CompareAndDelete(key, version)
		}
		return *live.OAuth, live.ID, nil
	}
	return store.OAuthCredential{}, "", store.ErrNativeOAuthIdentity
}

// Bearer never exports the plugin's access material. Executor is the sole
// sender and obtains current state again under the exact credential gate.
func (s *Service) Bearer(ctx context.Context, c store.Channel) (string, error) {
	_, _, err := s.live(ctx, c)
	return "plugin-managed", err
}

type networkEvent struct {
	Origin, Path, Method string
}

func (e networkEvent) inference(p compatprofile.Profile) bool {
	switch p.Package {
	case "qoder":
		return e.Path == "/algo/api/v2/service/pro/sse/agent_chat_generation"
	case "workbuddy":
		return e.Path == "/v2/chat/completions"
	case "minimax":
		return e.Path == "/mavis/api/v1/llm/v1/messages"
	}
	return false
}
func (e networkEvent) rotation(p compatprofile.Profile) bool {
	switch p.Package {
	case "qoder":
		return e.Path == "/api/v1/jobToken/refresh" || e.Path == "/api/v1/deviceToken/refresh"
	case "workbuddy":
		return e.Path == "/v2/plugin/auth/token/refresh"
	case "minimax":
		// Outside consent this endpoint is used only for refresh_token.
		return e.Path == "/oauth2/token"
	}
	return false
}

func approvedNetwork(profile compatprofile.Profile, e networkEvent, consent bool) bool {
	if profile.Package == "minimax" {
		if e.Origin == profile.Web && e.Method == "POST" && (e.Path == "/oauth2/token" || consent && e.Path == "/oauth2/device/code") {
			return true
		}
		if consent {
			return e.Origin == profile.OpenAPI && e.Method == "GET" && e.Path == "/v1/api/user/info"
		}
		return e.Origin == origin(profile.API) && (e.Method == "GET" && e.Path == "/mavis/api/v1/models" || e.Method == "POST" && e.inference(profile))
	}
	if profile.Package == "workbuddy" {
		if e.Origin != profile.OpenAPI {
			return false
		}
		if consent {
			return e.Method == "POST" && e.Path == "/v2/plugin/auth/state" ||
				e.Method == "GET" && (e.Path == "/v2/plugin/auth/token" || e.Path == "/v2/plugin/login/account" || e.Path == "/v2/plugin/accounts")
		}
		return e.Method == "POST" && (e.rotation(profile) || e.inference(profile)) || e.Method == "GET" && e.Path == "/v3/config"
	}
	if profile.Package != "qoder" {
		return false
	}
	if e.Origin == profile.OpenAPI {
		if consent {
			return e.Method == "GET" && (e.Path == "/api/v1/deviceToken/poll" || e.Path == "/api/v1/userinfo") ||
				e.Method == "POST" && e.Path == "/api/v1/me/jobToken"
		}
		if e.Method == "POST" && e.rotation(profile) {
			return true
		}
	}
	return !consent && e.Origin == profile.API &&
		(e.Method == "POST" && e.inference(profile) || e.Method == "GET" && e.Path == "/algo/api/v2/model/list")
}

func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

type operationSession struct {
	service         *Service
	selected        store.Channel
	credentialID    string
	oauth           store.OAuthCredential
	release         func()
	inferenceIssued bool
	rotationPending bool
	callbackError   error
}

func (s *Service) session(ctx context.Context, c store.Channel) (*operationSession, *Worker, error) {
	if len(c.Credentials) != 1 {
		return nil, nil, store.ErrNativeOAuthIdentity
	}
	release, err := s.acquire(ctx, c.ID, c.Credentials[0].ID)
	if err != nil {
		return nil, nil, err
	}
	o, id, err := s.live(ctx, c)
	if err != nil {
		release()
		return nil, nil, err
	}
	w, err := s.launch(ctx, s.Profile, o.PluginState)
	if err != nil {
		release()
		return nil, nil, err
	}
	return &operationSession{service: s, selected: c, credentialID: id, oauth: o, release: release}, w, nil
}

func (op *operationSession) handle(ctx context.Context, e Event) (out json.RawMessage, err error) {
	defer func() {
		if err != nil && op.callbackError == nil {
			op.callbackError = err
		}
	}()
	switch e.Name {
	case "network":
		var n networkEvent
		if json.Unmarshal(e.Payload, &n) != nil || !approvedNetwork(op.service.Profile, n, false) || op.inferenceIssued {
			return nil, errors.New("插件请求不在已验证的网络范围内")
		}
		if op.callbackError != nil {
			return nil, op.callbackError
		}
		live, _, err := op.service.live(ctx, op.selected)
		if err != nil {
			return nil, err
		}
		if store.PluginStateVersion(&live) != store.PluginStateVersion(&op.oauth) {
			return nil, store.ErrEditConflict
		}
		if n.rotation(op.service.Profile) {
			op.rotationPending = true
		}
		if n.inference(op.service.Profile) {
			if op.rotationPending {
				return nil, errors.New("插件续期未持久化，不能发送推理请求")
			}
			op.inferenceIssued = true
			// Verified adapters do no more auth/control calls after sending
			// inference. Release rotation ownership; parallel streams are OK.
			op.release()
		}
		return json.RawMessage(`{}`), nil
	case "auth":
		if op.inferenceIssued {
			return nil, errors.New("推理已发送，不能再由旧快照改写凭证")
		}
		var a struct {
			ProviderID string          `json:"provider_id"`
			Mode       string          `json:"mode"`
			Value      json.RawMessage `json:"value"`
		}
		if json.Unmarshal(e.Payload, &a) != nil || a.ProviderID != op.service.Profile.ID || (a.Mode != "replace" && a.Mode != "merge") {
			return nil, ErrProtocol
		}
		next, err := store.CompatibleOAuthCredential(op.service.Profile.CredentialType, string(a.Value))
		if err != nil {
			return nil, err
		}
		next, err = op.service.Store.ReplacePluginState(op.selected.ID, op.credentialID, store.PluginStateVersion(&op.oauth), next)
		if err != nil {
			return nil, err
		}
		op.oauth, op.rotationPending = next, false
		return json.RawMessage(next.PluginState), nil
	default:
		return nil, ErrProtocol
	}
}

func (op *operationSession) finish() {
	defer op.release()
	if op.rotationPending {
		// A spent refresh token without a durable acknowledgement cannot be
		// retried by the next request or after restarting the application.
		version := store.PluginStateVersion(&op.oauth)
		op.service.blocked.Store(fmt.Sprintf("%d/%s", op.selected.ID, op.credentialID), version)
		_ = op.service.Store.MarkPluginReauthorization(op.selected.ID, op.credentialID, version)
	}
}

func (s *Service) Models(ctx context.Context, c store.Channel) ([]nativeoauth.Model, error) {
	op, w, err := s.session(ctx, c)
	if err != nil {
		return nil, err
	}
	defer w.Close()
	defer op.finish()
	raw, err := w.Invoke(ctx, "models", nil, op.handle)
	if op.callbackError != nil {
		return nil, op.callbackError
	}
	if err != nil {
		return nil, err
	}
	var data map[string]struct {
		ID, Name string
	}
	if json.Unmarshal(raw, &data) != nil || len(data) == 0 || len(data) > 20000 {
		return nil, errors.New("插件未返回有效的账号模型目录")
	}
	models := make([]nativeoauth.Model, 0, len(data))
	for id, value := range data {
		if strings.TrimSpace(id) == "" || value.ID != "" && value.ID != id {
			return nil, errors.New("插件模型标识不一致")
		}
		models = append(models, nativeoauth.Model{ID: id, Name: value.Name, Format: s.Profile.Format()})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

type streamHead struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	RetryAfter  string `json:"retry_after"`
	SignIn      string `json:"sign_in"`
}

type streamResult struct {
	response *http.Response
	err      error
}

type responseBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *responseBody) Close() error {
	b.cancel()
	return b.PipeReader.Close()
}

func (s *Service) Execute(_ *http.Client, req *http.Request, c store.Channel) (*http.Response, error) {
	base, _ := url.Parse(s.Profile.API)
	suffix := "/chat/completions"
	if s.Profile.Format() == store.FormatAnthropicMessages {
		suffix = "/messages"
	}
	if req == nil || req.URL == nil || req.Body == nil || req.URL.User != nil || req.Method != "POST" ||
		origin(req.URL.String()) != origin(s.Profile.API) ||
		(req.URL.Path != base.Path+suffix && !(s.Profile.Package == "qoder" && req.URL.Path == "/v1"+suffix)) {
		return nil, fmt.Errorf("%w: 兼容插件请求端点无效", nativeoauth.ErrUnsupportedRequest)
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 12<<20+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 12<<20 || !json.Valid(body) {
		return nil, fmt.Errorf("%w: 兼容插件请求大小或 JSON 无效", nativeoauth.ErrUnsupportedRequest)
	}
	assemble := false
	if s.Profile.Package == "workbuddy" {
		var obj map[string]json.RawMessage
		var streaming bool
		if json.Unmarshal(body, &obj) != nil || obj == nil {
			return nil, fmt.Errorf("%w: 请求必须为 JSON 对象", nativeoauth.ErrUnsupportedRequest)
		}
		if raw, ok := obj["stream"]; ok && json.Unmarshal(raw, &streaming) != nil {
			return nil, fmt.Errorf("%w: stream 必须为布尔值", nativeoauth.ErrUnsupportedRequest)
		}
		assemble = !streaming
		if assemble {
			obj["stream"] = json.RawMessage("true")
			body, _ = json.Marshal(obj)
		}
	}
	ctx, cancel := context.WithCancel(req.Context())
	op, w, err := s.session(ctx, c)
	if err != nil {
		cancel()
		return nil, err
	}
	if _, err = w.Invoke(ctx, "validate", json.RawMessage(body), nil); err != nil {
		cancel()
		w.Close()
		op.finish()
		var refusal *RemoteError
		if errors.As(err, &refusal) {
			return nil, fmt.Errorf("%w: %v", nativeoauth.ErrUnsupportedRequest, err)
		}
		return nil, err
	}
	reader, writer := io.Pipe()
	first := make(chan streamResult, 1)
	finished := make(chan struct{})
	go func() {
		defer cancel()
		defer close(finished)
		defer w.Close()
		defer op.finish()
		started := false
		headers := map[string]string{}
		for _, name := range []string{"anthropic-version", "anthropic-beta"} {
			if value := req.Header.Get(name); value != "" {
				headers[name] = value
			}
		}
		_, runErr := w.Invoke(ctx, "fetch", map[string]any{"url": req.URL.String(), "body": base64.StdEncoding.EncodeToString(body), "headers": headers}, func(eventCtx context.Context, e Event) (json.RawMessage, error) {
			switch e.Name {
			case "head":
				if op.callbackError != nil {
					return nil, op.callbackError
				}
				var h streamHead
				if started || json.Unmarshal(e.Payload, &h) != nil || h.Status < 200 || h.Status > 599 {
					return nil, ErrProtocol
				}
				if h.SignIn == "expired" {
					_ = s.Store.MarkPluginReauthorization(c.ID, op.credentialID, store.PluginStateVersion(&op.oauth))
				}
				started = true
				headers := http.Header{"Content-Type": {h.ContentType}}
				if h.RetryAfter != "" {
					headers.Set("Retry-After", h.RetryAfter)
				}
				first <- streamResult{response: &http.Response{StatusCode: h.Status, Header: headers, Body: &responseBody{reader, cancel}, Request: req}}
				return json.RawMessage(`{}`), nil
			case "chunk":
				if !started {
					return nil, ErrProtocol
				}
				var chunk struct{ Data string }
				if json.Unmarshal(e.Payload, &chunk) != nil {
					return nil, ErrProtocol
				}
				data, err := base64.StdEncoding.DecodeString(chunk.Data)
				if err != nil || len(data) > 65536 {
					return nil, ErrProtocol
				}
				_, err = writer.Write(data)
				return json.RawMessage(`{}`), err
			default:
				return op.handle(eventCtx, e)
			}
		})
		if op.callbackError != nil {
			runErr = op.callbackError
		}
		if !started {
			if runErr == nil {
				runErr = errors.New("插件未返回响应头")
			}
			first <- streamResult{err: runErr}
		}
		_ = writer.CloseWithError(runErr)
	}()
	// Unblock backpressure if the request owner disappears without reading.
	go func() {
		select {
		case <-finished:
		case <-ctx.Done():
			select {
			case <-finished:
				return
			default:
				_ = reader.CloseWithError(ctx.Err())
			}
		}
	}()
	select {
	case result := <-first:
		if result.err == nil && assemble && result.response.StatusCode >= 200 && result.response.StatusCode < 300 {
			resp := result.response
			defer resp.Body.Close()
			data, err := streamreply.AssembleChat(resp.Body, resp.Header.Get("Content-Type"))
			if err != nil {
				return nil, fmt.Errorf("上游响应已开始，无法完整汇总且未重发: %w", err)
			}
			resp.Body = io.NopCloser(bytes.NewReader(data))
			resp.ContentLength = int64(len(data))
			resp.Header.Set("Content-Type", "application/json")
		}
		return result.response, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type Authorization struct {
	URL, Method, Instructions string
	UserCode                  string
	worker                    *Worker
	handle                    string
}

func (a *Authorization) Close() {
	if a != nil && a.worker != nil {
		_ = a.worker.Close()
	}
}

func (s *Service) consentHandler(ctx context.Context, e Event) (json.RawMessage, error) {
	var n networkEvent
	if e.Name != "network" || json.Unmarshal(e.Payload, &n) != nil || !approvedNetwork(s.Profile, n, true) {
		return nil, ErrProtocol
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

func (s *Service) Start(ctx context.Context) (*Authorization, error) {
	if err := check(ctx); err != nil {
		return nil, err
	}
	w, err := s.launch(ctx, s.Profile, "")
	if err != nil {
		return nil, err
	}
	raw, err := w.Invoke(ctx, "authorize", map[string]any{"method": 0}, s.consentHandler)
	var result struct {
		Flow, URL, Method, Instructions string
		UserCode                        string `json:"user_code"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil || !s.Profile.AuthorizationURL(result.URL) || result.Flow == "" || result.Method != "auto" {
		w.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("授权页不在已验证的地区范围内，请确认地区或更新兼容层")
	}
	return &Authorization{URL: result.URL, Method: result.Method, Instructions: result.Instructions, UserCode: result.UserCode, worker: w, handle: result.Flow}, nil
}

func (s *Service) Complete(ctx context.Context, a *Authorization, target store.PluginConsentTarget) (store.Channel, bool, error) {
	next, err := s.Exchange(ctx, a)
	if err != nil {
		return store.Channel{}, false, err
	}
	return s.Store.CommitPluginConsent(s.Profile.CredentialType, next, target, func() error { return check(ctx) })
}

// Exchange keeps returned consent private. Console polling performs the
// authorization-checked Store commit, never an unobserved background write.
func (s *Service) Exchange(ctx context.Context, a *Authorization) (store.OAuthCredential, error) {
	if a == nil || a.worker == nil {
		return store.OAuthCredential{}, errors.New("授权流程已结束")
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	raw, err := a.worker.Invoke(ctx, "complete", map[string]any{"flow": a.handle}, s.consentHandler)
	if err != nil {
		return store.OAuthCredential{}, err
	}
	var state map[string]json.RawMessage
	if json.Unmarshal(raw, &state) != nil || string(state["type"]) != `"success"` {
		return store.OAuthCredential{}, compatprofile.ErrState
	}
	state["type"] = json.RawMessage(`"oauth"`)
	raw, _ = json.Marshal(state)
	return store.CompatibleOAuthCredential(s.Profile.CredentialType, string(raw))
}
