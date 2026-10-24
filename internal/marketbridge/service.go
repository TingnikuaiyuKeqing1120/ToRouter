// Package marketbridge connects installed supplier code to the existing
// credential lifecycle and gateway. Package code never owns a second account DB.
package marketbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"torouter/internal/accessguard"
	"torouter/internal/marketprofile"
	"torouter/internal/nativeoauth"
	"torouter/internal/pluginhost"
	"torouter/internal/store"
)

type Service struct {
	Store                   *store.Store
	Type                    string
	Provider                marketprofile.Provider
	RuntimePath, ModulePath string
	Gates                   *sync.Map
}

func (s *Service) live(ctx context.Context, selected store.Channel) (store.OAuthCredential, string, error) {
	if err := accessguard.Check(ctx); err != nil {
		return store.OAuthCredential{}, "", err
	}
	if err := ctx.Err(); err != nil {
		return store.OAuthCredential{}, "", err
	}
	if selected.CredentialType != s.Type || len(selected.Credentials) != 1 || selected.Credentials[0].OAuth == nil {
		return store.OAuthCredential{}, "", store.ErrNativeOAuthIdentity
	}
	c, err := s.Store.Channel(selected.ID)
	if err != nil {
		return store.OAuthCredential{}, "", err
	}
	item := selected.Credentials[0]
	identity := store.CompatibleOAuthIdentity(s.Type, item.OAuth)
	for _, live := range c.Credentials {
		if c.CredentialType != s.Type || live.ID != item.ID || identity == "" || store.CompatibleOAuthIdentity(s.Type, live.OAuth) != identity {
			continue
		}
		if !c.Enabled || !live.Enabled {
			return store.OAuthCredential{}, "", errors.New("渠道或所选凭证已停用")
		}
		if live.OAuth.Status != "" && live.OAuth.Status != store.OAuthStatusActive {
			return store.OAuthCredential{}, "", errors.New("供应商授权需要重新登录")
		}
		return *live.OAuth, live.ID, nil
	}
	return store.OAuthCredential{}, "", store.ErrNativeOAuthIdentity
}

func (s *Service) Bearer(ctx context.Context, c store.Channel) (string, error) {
	_, _, err := s.live(ctx, c)
	return "plugin-managed", err
}

type session struct {
	s             *Service
	c             store.Channel
	oauth         store.OAuthCredential
	id            string
	worker        *pluginhost.Worker
	release       func()
	callbackError error
}

func (s *Service) session(ctx context.Context, c store.Channel) (*session, error) {
	if s.Gates == nil {
		return nil, errors.New("供应商运行状态未初始化")
	}
	if len(c.Credentials) != 1 {
		return nil, store.ErrNativeOAuthIdentity
	}
	key := fmt.Sprintf("%d/%s", c.ID, c.Credentials[0].ID)
	v, _ := s.Gates.LoadOrStore(key, make(chan struct{}, 1))
	gate := v.(chan struct{})
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	release := func() { once.Do(func() { <-gate }) }
	o, id, err := s.live(ctx, c)
	if err != nil {
		release()
		return nil, err
	}
	op := &session{s: s, c: c, oauth: o, id: id, release: release}
	w, err := pluginhost.StartMarket(ctx, s.RuntimePath, s.ModulePath, s.Provider.ID, json.RawMessage(o.PluginState), op.handle)
	if err != nil {
		if op.callbackError != nil || errors.Is(err, pluginhost.ErrOutcomeUnknown) {
			_ = s.Store.MarkPluginReauthorization(c.ID, id, store.PluginStateVersion(&op.oauth))
		}
		release()
		return nil, err
	}
	op.worker = w
	return op, nil
}

func (op *session) close(err error) {
	defer op.release()
	_ = op.worker.Close()
	if errors.Is(err, pluginhost.ErrOutcomeUnknown) || op.callbackError != nil {
		_ = op.s.Store.MarkPluginReauthorization(op.c.ID, op.id, store.PluginStateVersion(&op.oauth))
	}
}

func (op *session) handle(ctx context.Context, e pluginhost.Event) (json.RawMessage, error) {
	switch e.Name {
	case "network":
		_, _, err := op.s.live(ctx, op.c)
		return json.RawMessage(`{}`), err
	case "auth":
		var event struct {
			Provider string          `json:"provider_id"`
			Value    json.RawMessage `json:"value"`
		}
		if json.Unmarshal(e.Payload, &event) != nil || event.Provider != op.s.Provider.ID {
			return nil, pluginhost.ErrProtocol
		}
		next, err := store.MarketOAuthCredential(op.s.Type, string(event.Value), op.oauth.Account)
		if err == nil {
			next, err = op.s.Store.ReplacePluginState(op.c.ID, op.id, store.PluginStateVersion(&op.oauth), next)
		}
		if err != nil {
			op.callbackError = errors.New("供应商更新后的凭证未保存，请重新授权")
			return nil, op.callbackError
		}
		op.oauth = next
		return json.RawMessage(next.PluginState), nil
	}
	return nil, pluginhost.ErrProtocol
}

func (s *Service) Models(ctx context.Context, c store.Channel) (models []marketprofile.Model, err error) {
	op, err := s.session(ctx, c)
	if err != nil {
		return nil, err
	}
	defer func() { op.close(err) }()
	raw, err := op.worker.Invoke(ctx, "models", nil, op.handle)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &models); err != nil {
		return nil, errors.New("供应商模型目录无效")
	}
	if len(models) > 20000 {
		return nil, errors.New("供应商模型目录过大")
	}
	return models, nil
}

func Catalogue(name, credentialType string, models []marketprofile.Model) (store.Channel, error) {
	c := store.Channel{Name: name, CredentialType: credentialType, Enabled: true, ModelFormats: map[string][]string{}}
	seenFormat, seenModel := map[string]bool{}, map[string]bool{}
	for _, m := range models {
		if strings.TrimSpace(m.ID) == "" || m.API == "" {
			continue
		}
		switch m.Format {
		case store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent:
		default:
			continue
		}
		if !seenModel[m.ID] {
			c.AvailableModels = append(c.AvailableModels, m.ID)
			seenModel[m.ID] = true
		}
		if !contains(c.ModelFormats[m.ID], m.Format) {
			c.ModelFormats[m.ID] = append(c.ModelFormats[m.ID], m.Format)
		}
		if !seenFormat[m.Format] {
			c.Endpoints = append(c.Endpoints, store.Endpoint{Format: m.Format, BaseURL: m.API, State: "stateless"})
			seenFormat[m.Format] = true
		}
	}
	if len(c.Endpoints) == 0 {
		return c, errors.New("供应商没有返回当前可接入的模型协议，请检查登录权限或更新插件")
	}
	return c, nil
}
func contains(a []string, b string) bool {
	for _, s := range a {
		if s == b {
			return true
		}
	}
	return false
}

type responseBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *responseBody) Close() error { b.cancel(); return b.PipeReader.Close() }

type result struct {
	response *http.Response
	err      error
}

func (s *Service) Execute(_ *http.Client, req *http.Request, c store.Channel) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Body == nil || req.Method != "POST" {
		return nil, nativeoauth.ErrUnsupportedRequest
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, (12<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 12<<20 || !json.Valid(body) {
		return nil, nativeoauth.ErrUnsupportedRequest
	}
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	format := ""
	switch {
	case strings.HasSuffix(req.URL.Path, "/chat/completions"):
		format = store.FormatOpenAIChatCompletions
	case strings.HasSuffix(req.URL.Path, "/responses"):
		format = store.FormatOpenAIResponses
	case strings.HasSuffix(req.URL.Path, "/messages"):
		format = store.FormatAnthropicMessages
	case strings.Contains(req.URL.Path, ":generateContent") || strings.Contains(req.URL.Path, ":streamGenerateContent"):
		format = store.FormatGeminiGenerateContent
		parts := strings.Split(req.URL.Path, "/models/")
		if len(parts) == 2 {
			payload.Model = strings.Split(parts[1], ":")[0]
		}
	}
	if format == "" || !contains(c.ModelOutboundFormats(payload.Model), format) {
		return nil, fmt.Errorf("%w: 模型未声明此协议，请刷新模型目录", nativeoauth.ErrUnsupportedRequest)
	}
	ctx, cancel := context.WithCancel(req.Context())
	op, err := s.session(ctx, c)
	if err != nil {
		cancel()
		return nil, err
	}
	reader, writer := io.Pipe()
	first := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		var runErr error
		defer close(finished)
		defer cancel()
		defer func() { op.close(runErr) }()
		started := false
		headers := map[string]string{}
		for _, key := range []string{"anthropic-version", "anthropic-beta", "content-type"} {
			if v := req.Header.Get(key); v != "" {
				headers[key] = v
			}
		}
		_, runErr = op.worker.Invoke(ctx, "fetch", map[string]any{"url": req.URL.String(), "method": req.Method, "body": base64.StdEncoding.EncodeToString(body), "headers": headers, "model_id": payload.Model, "format": format}, func(eventCtx context.Context, e pluginhost.Event) (json.RawMessage, error) {
			switch e.Name {
			case "head":
				var h struct {
					Status  int               `json:"status"`
					Headers map[string]string `json:"headers"`
				}
				if started || json.Unmarshal(e.Payload, &h) != nil || h.Status < 200 || h.Status > 599 {
					return nil, pluginhost.ErrProtocol
				}
				started = true
				outHeaders := http.Header{}
				for key, value := range h.Headers {
					switch strings.ToLower(key) {
					case "content-type", "retry-after", "request-id", "x-request-id":
						outHeaders.Set(key, value)
					}
				}
				first <- result{response: &http.Response{StatusCode: h.Status, Header: outHeaders, Body: &responseBody{reader, cancel}, Request: req}}
				return json.RawMessage(`{}`), nil
			case "chunk":
				var chunk struct {
					Data string `json:"data"`
				}
				if !started || json.Unmarshal(e.Payload, &chunk) != nil {
					return nil, pluginhost.ErrProtocol
				}
				b, err := base64.StdEncoding.DecodeString(chunk.Data)
				if err != nil || len(b) > 65536 {
					return nil, pluginhost.ErrProtocol
				}
				_, err = writer.Write(b)
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
				runErr = errors.New("供应商未返回响应")
			}
			first <- result{err: runErr}
		}
		_ = writer.CloseWithError(runErr)
	}()
	go func() {
		select {
		case <-finished:
		case <-ctx.Done():
			_ = reader.CloseWithError(ctx.Err())
		}
	}()
	select {
	case r := <-first:
		if r.err != nil {
			cancel()
			_ = reader.Close()
		}
		return r.response, r.err
	case <-ctx.Done():
		cancel()
		_ = reader.Close()
		return nil, ctx.Err()
	}
}
