package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/copilot"
	"torouter/internal/marketbridge"
	"torouter/internal/nativeoauth"
	"torouter/internal/pluginhost"
	"torouter/internal/plugins"
	"torouter/internal/siwc"
	"torouter/internal/store"
	"torouter/internal/zenfree"
)

const diagnosticBodyLimit = 2 << 20

type upstreamTools struct {
	plugins *plugins.Manager
	store   *store.Store
	client  *http.Client
	// The three OAuth token services, the same instances the authorization
	// routes and the gateway hold, so one channel has exactly one refresh lock
	// no matter which side of the process refreshes it. A stored probe
	// authenticates with the credential the channel already keeps, and for an
	// OAuth channel that credential only exists behind these services.
	copilot    *copilot.Service
	siwc       *siwc.Service
	codex      *codex.Service
	native     *nativeoauth.Service
	public     *zenfree.Service
	compatible *pluginhost.Service
	market     *marketbridge.Service
}
type discoveredModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

// formatResult is one API format's answer inside a multi-format discover or
// probe. The formats of one channel live at different paths with different
// shapes, so they can disagree: a base that serves Chat Completions need not
// serve Messages, and each format's answer carries its own outcome, duration
// and diagnostic instead of one shared verdict.
type formatResult struct {
	Format          string            `json:"format"`
	CredentialID    string            `json:"credential_id,omitempty"`
	CredentialLabel string            `json:"credential_label,omitempty"`
	OK              bool              `json:"ok"`
	Models          []discoveredModel `json:"models,omitempty"`
	DurationMS      int64             `json:"duration_ms"`
	UpstreamStatus  int               `json:"upstream_status,omitempty"`
	Diagnostic      *diagnostic       `json:"diagnostic,omitempty"`
}
type toolResult struct {
	OK             bool              `json:"ok"`
	Models         []discoveredModel `json:"models"`
	Model          string            `json:"model,omitempty"`
	DurationMS     int64             `json:"duration_ms"`
	UpstreamStatus int               `json:"upstream_status,omitempty"`
	Diagnostic     *diagnostic       `json:"diagnostic,omitempty"`
	// Catalogue is the channel's model list as it stands after the fetch was
	// written back (ReplaceCatalogue). It is only present on an applied
	// discover: what the console should show from here on, so it does not have
	// to re-derive which entries came from the upstream.
	Catalogue []string `json:"catalogue,omitempty"`
	// Formats carries one entry per requested API format. A call naming several
	// formats answers per format — which one works, how long it took, what went
	// wrong — while a call naming one keeps the flat fields above, which is what
	// the console's per-model test and its existing clients read.
	Formats []formatResult `json:"formats,omitempty"`
}

func upstreamRoutes(r chi.Router, s *store.Store, handlers oauthHandlers) *upstreamTools {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	transport.MaxIdleConnsPerHost = 2
	u := &upstreamTools{store: s, client: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, copilot: handlers.copilot, siwc: handlers.siwc, codex: handlers.codex, plugins: handlers.plugins}
	r.Post("/channels/discover", u.discover)
	r.Post("/channels/probe", u.probe)
	// The stored-channel probe: the credential is the channel's own, which for
	// an OAuth channel lives only on this side of the wire, so it cannot travel
	// in a draft payload the way the probe above asks for one.
	r.Post("/channels/{id}/probe", u.probeStored)
	r.Delete("/channels/{id}/models", u.deleteModel)
	return u
}

// deleteModel removes one model from a stored channel: the catalogue entry, and
// with it the manual marker and the enabled entry when there is one. The model
// travels as a query parameter because model ids routinely contain slashes,
// which a path segment cannot carry.
func (u *upstreamTools) deleteModel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		fail(w, 400, "invalid channel id")
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		fail(w, 400, "model is required")
		return
	}
	updated, err := u.store.DeleteModel(id, model)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(w, 404, "channel or model not found")
			return
		}
		fail(w, 500, "delete failed")
		return
	}
	respond(w, 200, publicChannel(updated))
}

type draftRequest struct {
	Channel   store.Channel `json:"channel"`
	Model     string        `json:"model"`
	Confirmed bool          `json:"confirmed"`
	// Formats names the API formats the call answers for. Empty means the tool's
	// own default: discover covers everything the channel declares, probe tests
	// the first declared endpoint only, because a probe is a paid generation.
	Formats       []string `json:"formats"`
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// Apply asks discover to write the fetched catalogue back to the stored
	// channel, replacing the auto part of it. Without it discover stays what it
	// was: a read-only look at the upstream.
	Apply bool `json:"apply"`
	// Management discovery binds application to the full original stored row.
	expectedVersion string
}

func (u *upstreamTools) draft(w http.ResponseWriter, r *http.Request) (draftRequest, bool) {
	var body draftRequest
	if !decode(w, r, &body) {
		return draftRequest{}, false
	}
	out, err := u.prepareDraft(body, strings.HasSuffix(r.URL.Path, "/probe"))
	if err != nil {
		respondManagementTool(w, nil, err)
		return body, false
	}
	return out, true
}

// familyOf is the wire family an API format belongs to: the request shape, the
// auth header and the version prefix all follow the family, so two formats of
// one family are the same client with a different URL.
func familyOf(format string) string {
	switch format {
	case store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses:
		return "openai"
	case store.FormatAnthropicMessages:
		return "anthropic"
	case store.FormatGeminiGenerateContent:
		return "gemini"
	}
	return ""
}

// baseForFormat is where a format's endpoint lives: the endpoint's own address,
// or the channel-level default when it declares none. A format the channel does
// not declare is asked at the channel default, the only address it could live
// at; an empty one is reported as an invalid address rather than guessed.
func baseForFormat(c store.Channel, format string) string {
	if e, ok := c.EndpointFor(format); ok {
		return c.EndpointBaseURL(e)
	}
	return strings.TrimSpace(c.BaseURL)
}

// endpoint builds one request URL: the base a format lives at, the version
// prefix its family uses, and the path the tool appends. A base that is not
// there yet is an error, not a relative URL the HTTP client would reject with a
// message about schemes.
func endpoint(base, family, suffix string) (*url.URL, error) {
	if strings.TrimSpace(base) == "" {
		return nil, errNoAddress
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	version := "/v1"
	if family == "gemini" {
		version = "/v1beta"
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, version) {
		parsed.Path += version
	}
	parsed.Path += suffix
	parsed.RawPath = ""
	return parsed, nil
}

// requestedFormats validates the API formats a tool call names: known names,
// no duplicates, nothing dropped silently — a silently dropped format is an
// answer the caller cannot see. It returns no default: what an unnamed request
// means differs between the two tools, so each caller decides.
func requestedFormats(requested []string) ([]string, string) {
	formats := []string{}
	for _, f := range requested {
		f = strings.TrimSpace(f)
		if familyOf(f) == "" {
			return nil, "未知 API 格式：" + f
		}
		if !slices.Contains(formats, f) {
			formats = append(formats, f)
		}
	}
	return formats, ""
}

// declaredFormats is what the channel serves, deduplicated, in declaration
// order.
func declaredFormats(c store.Channel) []string {
	formats := []string{}
	for _, f := range c.Formats() {
		if !slices.Contains(formats, f) {
			formats = append(formats, f)
		}
	}
	return formats
}

func (u *upstreamTools) request(ctx context.Context, family, apiKey, method string, target *url.URL, body []byte, sse bool) ([]byte, int, *diagnostic) {
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, &diagnostic{Source: "gateway", Code: "request", Message: "无法构造上游请求。", Suggestion: "检查连接地址。"}
	}
	if sse {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch family {
	case "openai":
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case "anthropic":
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		req.Header.Set("x-goog-api-key", apiKey)
	}
	return u.send(ctx, req, apiKey)
}

// send performs one upstream request and reads its answer under the diagnostic
// limit. secret is the credential the request carried, and it is never logged:
// it exists so an error that quotes it — a transport message, an upstream error
// body — is masked in the diagnostic's raw field. Redirects are not followed
// (see the client in upstreamRoutes), so a credential cannot ride along to a
// third-party host the upstream names.
func (u *upstreamTools) send(ctx context.Context, req *http.Request, secret string) ([]byte, int, *diagnostic) {
	return u.sendUsing(ctx, req, secret, nil)
}

func (u *upstreamTools) sendUsing(ctx context.Context, req *http.Request, secret string, execute func(*http.Request) (*http.Response, error)) ([]byte, int, *diagnostic) {
	var resp *http.Response
	var err error
	if execute != nil {
		resp, err = execute(req)
	} else {
		resp, err = accessguard.Do(u.client, req)
	}
	if err != nil {
		if errors.Is(err, nativeoauth.ErrUnsupportedRequest) {
			return nil, 0, &diagnostic{Source: "gateway", Code: "unsupported_request", Message: Redact(firstLine(err.Error()), secret), Suggestion: "该接入无法保留此请求，请改用已支持的模型或协议。"}
		}
		if accessguard.Denied(err) {
			return nil, 0, &diagnostic{Source: "gateway", Code: "access_revoked", Message: "管理授权已撤销或权限已改变，后续请求已停止。", Suggestion: "在控制台检查授权后重新读取操作结果；不要盲目重发已发送的请求。"}
		}
		raw, truncated := safeRaw([]byte(err.Error()), secret)
		return nil, 0, &diagnostic{Source: "network", Code: "connection", Message: "无法连接上游、连接超时或请求已取消。", Suggestion: "检查地址、网络、代理和证书；这不一定是密钥问题。", Raw: raw, Truncated: truncated}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, diagnosticBodyLimit+1))
	if err != nil {
		return nil, resp.StatusCode, &diagnostic{Source: "network", Code: "read", Message: "上游响应读取失败。", Suggestion: "检查网络后重试。", UpstreamStatus: resp.StatusCode}
	}
	if len(raw) > diagnosticBodyLimit {
		return nil, resp.StatusCode, &diagnostic{Source: "upstream", Code: "response_limit", Message: "上游响应超过 2 MiB 安全上限。", Suggestion: "缩小模型列表范围或手动添加模型。", UpstreamStatus: resp.StatusCode, Truncated: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, upstreamDiagnostic(resp.StatusCode, raw, secret)
	}
	return raw, resp.StatusCode, nil
}
func malformedDiagnostic(raw []byte, status int, key string) *diagnostic {
	d := &diagnostic{Source: "upstream", Code: "invalid_response", Message: "上游返回成功状态，但响应格式不符合当前协议。", Suggestion: "检查 API 地址和协议；网页或空响应不算模型可用。", UpstreamStatus: status}
	d.Raw, d.Truncated = safeRaw(raw, key)
	return d
}

// fetchModels pulls one format's model catalogue: the models endpoint at the
// base that format lives at, paged the way its family pages and parsed the way
// its family answers. It returns the discovered models, the last upstream
// status, and the reason it stopped early, if it stopped early.
func (u *upstreamTools) fetchModels(ctx context.Context, apiKey, family, base string) ([]discoveredModel, int, *diagnostic) {
	target, err := endpoint(base, family, "/models")
	if err != nil {
		return nil, 0, &diagnostic{Source: "gateway", Code: "url", Message: "无效地址。"}
	}
	models := map[string]discoveredModel{}
	collectedModels := func() []discoveredModel {
		out := make([]discoveredModel, 0, len(models))
		for _, model := range models {
			out = append(out, model)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	seen := map[string]bool{}
	status := 0
	for page := 0; page < 20; page++ {
		raw, code, d := u.request(ctx, family, apiKey, "GET", target, nil, false)
		if d != nil && d.Code == "access_revoked" && code == 0 {
			if page == 0 {
				return nil, 0, d
			}
			// The next page was blocked before sending. Retain the completed
			// pages and their last real HTTP status; do not apply a partial list.
			d.Message = "后续分页请求未发送；已保留之前收到的模型页，本次目录未应用。"
			return collectedModels(), status, d
		}
		status = code
		if d != nil {
			return nil, status, d
		}
		var envelope struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			Models []struct {
				Name        string   `json:"name"`
				DisplayName string   `json:"displayName"`
				Methods     []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
			Next    string `json:"nextPageToken"`
		}
		if json.Unmarshal(raw, &envelope) != nil || (family == "gemini" && envelope.Models == nil) || (family != "gemini" && envelope.Data == nil) {
			return nil, status, malformedDiagnostic(raw, status, apiKey)
		}
		if family == "gemini" {
			for _, m := range envelope.Models {
				supported := len(m.Methods) == 0
				for _, method := range m.Methods {
					if method == "generateContent" {
						supported = true
					}
				}
				if supported {
					id := strings.TrimPrefix(m.Name, "models/")
					if id != "" {
						models[id] = discoveredModel{id, m.DisplayName}
					}
				}
			}
		} else {
			for _, m := range envelope.Data {
				if m.ID != "" {
					models[m.ID] = discoveredModel{m.ID, m.DisplayName}
				}
			}
		}
		if len(models) > 10000 {
			return nil, status, &diagnostic{Source: "gateway", Code: "model_limit", Message: "模型列表超过 10000 条。", Suggestion: "请手动添加需要的模型。"}
		}
		next := ""
		parameter := "after_id"
		if family == "gemini" {
			next = envelope.Next
			parameter = "pageToken"
		} else if envelope.HasMore {
			next = envelope.LastID
			if next == "" && len(envelope.Data) > 0 {
				next = envelope.Data[len(envelope.Data)-1].ID
			}
			if next == "" {
				return nil, status, malformedDiagnostic(raw, status, apiKey)
			}
		}
		if next == "" {
			return collectedModels(), status, nil
		}
		if seen[next] {
			return nil, status, &diagnostic{Source: "upstream", Code: "pagination_loop", Message: "上游重复返回同一个分页标记。", Suggestion: "请手动添加模型，或联系提供商检查分页接口。"}
		}
		seen[next] = true
		q := target.Query()
		q.Set(parameter, next)
		target.RawQuery = q.Encode()
	}
	return nil, status, &diagnostic{Source: "gateway", Code: "page_limit", Message: "模型列表超过 20 页上限。", Suggestion: "请手动添加所需模型。"}
}
func (u *upstreamTools) discover(w http.ResponseWriter, r *http.Request) {
	req, ok := u.draft(w, r)
	if !ok {
		return
	}
	out, err := u.discoverCore(r.Context(), req)
	respondManagementTool(w, out, err)
}

// probeFormat sends one model's text request to one format's endpoint and reads
// the answer the way that family shapes it. A success status with the wrong
// shape is a failure: the point of the probe is that the facade really serves
// the model, not that some URL answered 200.
func (u *upstreamTools) probeFormat(ctx context.Context, apiKey, format, base, model string) (int, *diagnostic) {
	suffix, body, d := probeBody(format, model)
	if d != nil {
		return 0, d
	}
	target, err := endpoint(base, familyOf(format), suffix)
	if err != nil {
		return 0, &diagnostic{Source: "gateway", Code: "url", Message: "无效地址。"}
	}
	b, _ := json.Marshal(body)
	raw, status, d := u.request(ctx, familyOf(format), apiKey, "POST", target, b, format == store.FormatOpenAIResponses)
	if d != nil {
		return status, d
	}
	return status, probeAnswer(format, raw, status, apiKey)
}

// probeBody is the one text a probe sends to one format's endpoint: the path the
// format lives at and the fixed "reply OK" body. A probe is not a capability
// test — it is the cheapest request that proves the facade serves the model, so
// the same short prompt goes to every format and the Responses shape asks for a
// bounded answer with streaming off.
func probeBody(format, model string) (string, map[string]any, *diagnostic) {
	switch format {
	case store.FormatAnthropicMessages:
		return "/messages", chatBody(model), nil
	case store.FormatOpenAIResponses:
		// The shape the backend-api validates, mirroring Codex-Manager's warmup
		// request (account_warmup.rs): input as a message list with an explicit
		// type ("Input must be a list" for a bare string), store false ("Store
		// must be set to false"), streamed and read as SSE until
		// response.completed (2026-10-04, live 400s + CM source). The official
		// Responses API accepts the same shape, so one body serves the SIWC
		// track too.
		return "/responses", map[string]any{"model": model, "instructions": "", "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}}, "stream": true, "store": false}, nil
	case store.FormatGeminiGenerateContent:
		if strings.ContainsAny(model, "/\\?#") {
			return "", nil, &diagnostic{Source: "gateway", Code: "model_id", Message: "Gemini 模型 ID 含有无效字符。", Suggestion: "填写 models/ 后的模型名。"}
		}
		return "/models/" + model + ":generateContent", map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hi"}}}}, "generationConfig": map[string]int{"maxOutputTokens": 64}}, nil
	default:
		return "/chat/completions", chatBody(model), nil
	}
}

func chatBody(model string) map[string]any {
	return map[string]any{"model": model, "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "max_tokens": 64, "stream": false}
}

// probeAnswer reads the answer one format's probe got. A success status with the
// wrong shape is a failure: the point of the probe is that the facade really
// serves the model, not that some URL answered 200.
func probeAnswer(format string, raw []byte, status int, key string) *diagnostic {
	if format == store.FormatOpenAIResponses {
		return responsesAnswer(raw, status, key)
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil || payload["error"] != nil {
		return malformedDiagnostic(raw, status, key)
	}
	field, required := "choices", "message"
	switch format {
	case store.FormatAnthropicMessages:
		field, required = "content", "type"
	case store.FormatGeminiGenerateContent:
		field, required = "candidates", "content"
	}
	var entries []json.RawMessage
	if json.Unmarshal(payload[field], &entries) != nil || len(entries) == 0 {
		return malformedDiagnostic(raw, status, key)
	}
	for _, entry := range entries {
		var item map[string]json.RawMessage
		if json.Unmarshal(entry, &item) != nil || item == nil {
			return malformedDiagnostic(raw, status, key)
		}
		if len(item[required]) == 0 || string(item[required]) == "null" {
			return malformedDiagnostic(raw, status, key)
		}
	}
	return nil
}

// responsesAnswer reads a Responses API answer. The Codex backend answers JSON
// when the caller does not stream and the SIWC profile answers SSE whatever the
// caller asked for, so both shapes count: a non-empty output array, or a
// completion event in the stream. An error body or an error event is a failure
// even on a 2xx — the request was not served, which is what the probe is asking
// about.
func responsesAnswer(raw []byte, status int, key string) *diagnostic {
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, "data:") || strings.Contains(text, "\ndata:") {
		completed := false
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var event map[string]json.RawMessage
			if json.Unmarshal([]byte(payload), &event) != nil {
				continue
			}
			if event["error"] != nil || string(event["type"]) == `"error"` {
				return malformedDiagnostic(raw, status, key)
			}
			switch string(event["type"]) {
			case `"response.completed"`, `"response.done"`:
				completed = true
			}
		}
		if !completed {
			return malformedDiagnostic(raw, status, key)
		}
		return nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil || payload["error"] != nil {
		return malformedDiagnostic(raw, status, key)
	}
	var output []json.RawMessage
	if json.Unmarshal(payload["output"], &output) != nil || len(output) == 0 {
		return malformedDiagnostic(raw, status, key)
	}
	return nil
}
func selectCredentialsToTest(c store.Channel, requestedIDs []string) []store.CredentialItem {
	if len(requestedIDs) > 0 {
		var selected []store.CredentialItem
		for _, id := range requestedIDs {
			id = strings.TrimSpace(id)
			for _, cr := range c.Credentials {
				if cr.ID == id {
					selected = append(selected, cr)
					break
				}
			}
		}
		if len(selected) > 0 {
			return selected
		}
	}
	primary := c.PrimaryCredential()
	if primary.ID == "" {
		primary.ID = "cred_default"
	}
	if primary.Label == "" {
		primary.Label = "默认凭据"
	}
	return []store.CredentialItem{primary}
}

func (u *upstreamTools) probe(w http.ResponseWriter, r *http.Request) {
	req, ok := u.draft(w, r)
	if !ok {
		return
	}
	out, err := u.probeCore(r.Context(), req)
	respondManagementTool(w, out, err)
}

// storedProbeRequest is the body of the stored-channel probe: which model to
// test, the confirmation that a paid generation is acceptable, the formats to
// answer for, and — when the test comes from the editor rather than an account
// card — the draft the editor is showing. There is no credential in it: the
// credential is the stored row's, named by the path.
type storedProbeRequest struct {
	Model                   string   `json:"model"`
	Confirmed               bool     `json:"confirmed"`
	Formats                 []string `json:"formats"`
	CredentialIDs           []string `json:"credential_ids,omitempty"`
	RequireExactCredentials bool     `json:"-"`
	// Channel is the editor's draft. Only its endpoints — and the channel
	// default they fall back to — are read, because the probe has to answer for
	// the configuration the editor shows: a re-pointed endpoint must be tested
	// at its new address before anything is saved, not at the address the row
	// still holds. Everything the draft says about the credential (its
	// credential_type, api_key, models) is ignored: the credential is resolved
	// from the stored row the path names, so a draft cannot name one it does not
	// own. Absent — an account card's probe, or an older client — means the
	// stored row's own endpoints, which is what this probe has always tested.
	Channel *store.Channel `json:"channel"`
}

// probeStored tests one model on a saved channel with the credential that
// channel already holds: the stored api key, or the OAuth token its track's
// service refreshes. The draft probe cannot do this — it authenticates with a
// key the caller types, and an OAuth channel's credential exists only on this
// side of the wire — so this is how a channel is tested after authorization.
//
// The destination is the editor's draft when it sent one (see storedProbeRequest)
// and the stored row's otherwise; the credential is the stored row's either way.
//
// The answer has the same shape as the draft probe's: flat for one format, per
// format for several.
func (u *upstreamTools) probeStored(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		fail(w, 400, "invalid channel id")
		return
	}
	var body storedProbeRequest
	if !decode(w, r, &body) {
		return
	}
	out, err := u.probeStoredCore(r.Context(), id, body)
	respondManagementTool(w, out, err)
}

// storedBearer resolves the credential a stored channel authenticates with. An
// OAuth credential goes through the same service call the gateway makes, so the
// probe is answered with the token the gateway would use — refreshed or not.
// A failure is an answer rather than a refusal: nothing was sent, so the caller
// gets the reason the credential could not be resolved, desensitized.
func (u *upstreamTools) storedBearer(ctx context.Context, c store.Channel) (string, *diagnostic) {
	if store.IsCompatibleOAuth(c.CredentialType) {
		return u.oauthBearer(ctx, c)
	}
	switch c.CredentialType {
	case store.CredentialCopilotOAuth, store.CredentialChatGPTOAuth, store.CredentialCodexOAuth, store.CredentialClaudeOAuth, store.CredentialAntigravityOAuth, store.CredentialDevinOAuth:
		return u.oauthBearer(ctx, c)
	}
	if c.APIKey != "" {
		return c.APIKey, nil
	}
	return c.PrimaryKey(), nil
}

// oauthBearer resolves one OAuth channel's credential through its track's
// service, which refreshes it when it has to. The service is the same instance
// the gateway holds, so the probe is answered with the token the gateway would
// use and a rotation is not duplicated across two clients.
func (u *upstreamTools) oauthBearer(ctx context.Context, c store.Channel) (string, *diagnostic) {
	if c.OAuth == nil {
		return "", authDiagnostic("该渠道没有 OAuth 凭证。")
	}
	track := ""
	var token string
	var err error
	switch c.CredentialType {
	case store.CredentialCopilotOAuth:
		track = "Copilot"
		if u.copilot == nil {
			return "", authDiagnostic("无法解析 Copilot 凭证：此进程没有配置 Copilot 客户端。")
		}
		token, err = u.copilot.Bearer(ctx, c)
	case store.CredentialChatGPTOAuth:
		track = "ChatGPT"
		if u.siwc == nil {
			return "", authDiagnostic("无法解析 ChatGPT 凭证：此进程没有配置 ChatGPT 客户端。")
		}
		token, err = u.siwc.Bearer(ctx, c)
	case store.CredentialCodexOAuth:
		track = "Codex"
		if u.codex == nil {
			return "", authDiagnostic("无法解析 Codex 凭证：此进程没有配置 Codex 客户端。")
		}
		token, err = u.codex.Bearer(ctx, c)
	case store.CredentialClaudeOAuth, store.CredentialAntigravityOAuth, store.CredentialDevinOAuth:
		if u.native == nil {
			return "", authDiagnostic("此原生接入尚不可用。")
		}
		track = u.native.Definition().Name
		token, err = u.native.Bearer(ctx, c)
	default:
		if u.market != nil && store.IsMarketOAuth(c.CredentialType) {
			track = u.market.Provider.Name
			token, err = u.market.Bearer(ctx, c)
		} else if store.IsCompatibleOAuth(c.CredentialType) && u.compatible != nil {
			track = u.compatible.Profile.Name
			token, err = u.compatible.Bearer(ctx, c)
		} else {
			return "", authDiagnostic("此接入尚不可用，请检查插件库。")
		}
	}
	if err != nil {
		return "", authDiagnostic("无法解析或刷新 "+track+" 凭证："+firstLine(err.Error()), oauthSecrets(c)...)
	}
	return token, nil
}

// authDiagnostic is the answer a credential that cannot be resolved gets: the
// reason, with the credential masked before it travels. A token endpoint's
// refusal quotes the endpoint's own words, and those words can quote the
// credential the endpoint refused — so the credential is masked by exact
// substring, the way an upstream error body is, not only by shape pattern.
func authDiagnostic(message string, secrets ...string) *diagnostic {
	return &diagnostic{Source: "gateway", Code: "auth", Message: Redact(message, secrets...), Suggestion: "重新授权该渠道；若持续失败，请检查账号状态后重试。"}
}

// oauthSecrets is every credential the stored OAuth row holds: the bearer, the
// refresh token and the track's own authorization token. A refresh failure
// travels the token endpoint's own description, which is free text that can
// quote any of the three, so all of them are masked rather than the one the
// caller happens to guess at.
func oauthSecrets(c store.Channel) []string {
	if c.OAuth == nil {
		return nil
	}
	out := []string{}
	for _, secret := range []string{c.OAuth.AccessToken, c.OAuth.RefreshToken, c.OAuth.GitHubToken} {
		if secret != "" && !slices.Contains(out, secret) {
			out = append(out, secret)
		}
	}
	return out
}

// storedProbeFormat sends one stored channel's probe for one format. Where the
// request goes and what it authenticates with follow the credential type, the
// same split gateway.upstreamRequest makes: an OAuth credential decides the path
// family and the header set, an api key rides the generic branch. Anything the
// gateway would refuse to build is refused here with its wording, so a probe
// never proves a destination the gateway does not use.
func (u *upstreamTools) storedProbeFormat(ctx context.Context, c store.Channel, credential, format, model string) (int, *diagnostic) {
	if c.ModelFormats != nil && !slices.Contains(c.ModelOutboundFormats(model), format) {
		return 0, &diagnostic{Source: "gateway", Code: "request", Message: "该模型未声明此协议，请刷新模型目录并选择其实际协议。"}
	}
	suffix, body, d := probeBody(format, model)
	if d != nil {
		return 0, d
	}
	if c.CredentialType == store.CredentialZenFree {
		body["tools"] = zenfree.ProbeTools(format)
		if format == store.FormatAnthropicMessages {
			body["tool_choice"] = map[string]any{"type": "none"}
		} else {
			body["tool_choice"] = "none"
		}
	}
	target, err := storedProbeTarget(c.CredentialType, format, baseForFormat(c, format), suffix)
	if err != nil {
		// An address that is not there, or that names no usable scheme, is the
		// destination missing rather than a declaration the credential has no
		// business at: the draft probe's "无效地址。", not a refusal about the
		// credential. This holds for a draft endpoint too — the address is
		// whoever named it.
		if errors.Is(err, errNoAddress) {
			return 0, &diagnostic{Source: "gateway", Code: "url", Message: "无效地址。"}
		}
		return 0, &diagnostic{Source: "gateway", Code: "request", Message: Redact(err.Error()), Suggestion: "在渠道编辑中为该渠道声明受支持的端点；凭证不会发往此处。"}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(b))
	if err != nil {
		return 0, &diagnostic{Source: "gateway", Code: "request", Message: "无法构造上游请求。", Suggestion: "检查连接地址。"}
	}
	req.Header.Set("Content-Type", "application/json")
	if format == store.FormatOpenAIResponses || c.CredentialType == store.CredentialChatGPTOAuth {
		// Responses probes stream (the backend-api wants stream:true and
		// answers in SSE), and the SIWC profile answers with SSE whatever the
		// client asked for — either way the Accept is SSE.
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	// The credential type, not the endpoint format, decides the header set —
	// the same rule gateway.upstreamRequest follows.
	switch c.CredentialType {
	case store.CredentialCopilotOAuth:
		copilot.ApplyRequestHeaders(req.Header, credential)
	case store.CredentialChatGPTOAuth:
		siwc.ApplyRequestHeaders(req.Header, credential)
	case store.CredentialCodexOAuth:
		codex.ApplyRequestHeaders(req.Header, credential, accountOf(c), "")
	case store.CredentialClaudeOAuth, store.CredentialAntigravityOAuth, store.CredentialDevinOAuth:
		req.Header.Set("Authorization", "Bearer "+credential)
	default:
		switch format {
		case store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses:
			req.Header.Set("Authorization", "Bearer "+credential)
		case store.FormatAnthropicMessages:
			req.Header.Set("x-api-key", credential)
			req.Header.Set("Authorization", "Bearer "+credential)
			req.Header.Set("anthropic-version", "2023-06-01")
		case store.FormatGeminiGenerateContent:
			req.Header.Set("x-goog-api-key", credential)
		}
	}
	send := func(req *http.Request) (*http.Response, error) { return accessguard.Do(u.client, req) }
	if u.native != nil && store.IsNativeOAuth(c.CredentialType) {
		send = func(req *http.Request) (*http.Response, error) { return u.native.Execute(u.client, req, c) }
	} else if u.public != nil && c.CredentialType == store.CredentialZenFree {
		send = func(req *http.Request) (*http.Response, error) { return u.public.Execute(u.client, req, c) }
	} else if u.market != nil && store.IsMarketOAuth(c.CredentialType) {
		send = func(req *http.Request) (*http.Response, error) { return u.market.Execute(u.client, req, c) }
	} else if u.compatible != nil && store.IsCompatibleOAuth(c.CredentialType) {
		send = func(req *http.Request) (*http.Response, error) { return u.compatible.Execute(u.client, req, c) }
	}
	raw, status, d := u.sendUsing(ctx, req, credential, send)
	if d != nil {
		return status, d
	}
	return status, probeAnswer(format, raw, status, credential)
}

// accountOf is the account an OAuth credential is pinned to, which is what the
// Codex backend scopes its answers by. An api_key channel has none and never
// reaches the call that uses it.
func accountOf(c store.Channel) string {
	if c.OAuth == nil {
		return ""
	}
	return c.OAuth.Account
}

// storedProbeTarget is where one stored probe goes. The OAuth branches mirror
// gateway.upstreamRequest path for path — Copilot's two facades off the API base,
// the Codex backend's /responses with no /v1, the SIWC profile's Responses
// endpoint on the generic branch — and each re-checks the host allowlist and the
// scheme the way the gateway's runtime half does, because a row can be edited
// behind the console's back. The api_key branch is the generic one.
func storedProbeTarget(credentialType, format, base, suffix string) (*url.URL, error) {
	if store.IsNativeOAuth(credentialType) || store.IsCompatibleOAuth(credentialType) || credentialType == store.CredentialZenFree {
		if err := oauthEndpointAllowed(credentialType, base); err != nil {
			return nil, err
		}
		return endpoint(base, familyOf(format), suffix)
	}
	switch credentialType {
	case store.CredentialCopilotOAuth:
		// Copilot serves the two documented facades directly off the API base:
		// /chat/completions for the OpenAI shape and /v1/messages for the
		// Anthropic one — the OpenAI side takes no /v1 prefix here.
		//
		// The facade is checked before the host, unlike the gateway's order,
		// because a format the channel does not declare has no base of its own
		// to check: naming the facade is the actionable answer.
		if format != store.FormatOpenAIChatCompletions && format != store.FormatAnthropicMessages {
			return nil, errors.New("copilot_oauth channel declares an unsupported endpoint format")
		}
		if err := oauthEndpointAllowed(credentialType, base); err != nil {
			return nil, err
		}
		if format == store.FormatAnthropicMessages {
			return endpointPath(base, "/v1"+suffix)
		}
		return endpointPath(base, suffix)
	case store.CredentialCodexOAuth:
		if format != store.FormatOpenAIResponses {
			// The gateway's own wording, verbatim: one rule, one sentence,
			// wherever it is enforced.
			return nil, fmt.Errorf("the codex backend serves only the responses endpoint, not %s", format)
		}
		if err := oauthEndpointAllowed(credentialType, base); err != nil {
			return nil, err
		}
		return endpointPath(base, "/responses")
	case store.CredentialChatGPTOAuth:
		if format != store.FormatOpenAIResponses {
			return nil, fmt.Errorf("the chatgpt_oauth profile serves only the responses endpoint, not %s", format)
		}
		if err := oauthEndpointAllowed(credentialType, base); err != nil {
			return nil, err
		}
		return endpoint(base, familyOf(format), suffix)
	}
	// The generic branch is the api_key one, and it re-checks the address itself
	// — that there is one, and that it names a scheme the HTTP client can use —
	// the way the OAuth branches do through their allowlist. A row edited behind
	// the console's back can name a base the client would refuse with an English
	// transport error; the probe answers the draft probe's "无效地址。" instead.
	if err := usableAddress(base); err != nil {
		return nil, err
	}
	return endpoint(base, familyOf(format), suffix)
}

// errNoAddress is a base that is not there, or that names no scheme the HTTP
// client can use: the destination a tool would need does not exist. Both tools
// answer it as "无效地址。" rather than passing the English error through, so an
// owner who left an address empty gets a diagnosis they can act on.
var errNoAddress = errors.New("no base URL for this endpoint")

// usableAddress reports whether a base is a destination a request can be built
// for: it is there, and it names http or https. Anything else is errNoAddress.
func usableAddress(base string) error {
	if strings.TrimSpace(base) == "" {
		return errNoAddress
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errNoAddress
	}
	return nil
}

// endpointPath appends one path to a base as it stands, with no version prefix:
// the facades that serve off the API base itself rather than under /v1.
func endpointPath(base, suffix string) (*url.URL, error) {
	if strings.TrimSpace(base) == "" {
		return nil, errNoAddress
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("unsupported scheme")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + suffix
	parsed.RawPath = ""
	return parsed, nil
}

// oauthEndpointAllowed is the console's half of the gateway's runtime re-check:
// an OAuth credential may only be sent to a host on its credential type's
// allowlist, over https. SaveChannel enforces both when the channel is written;
// a probe is where the rule is asked again, because a probe sends a real
// credential on a real generation.
func oauthEndpointAllowed(credentialType, baseURL string) error {
	if !store.OAuthHostAllowed(credentialType, baseURL) {
		return fmt.Errorf("endpoint %s is not on the %s host allowlist; to-router will not send the OAuth credential there", baseURL, credentialType)
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && !store.EndpointRelaxedForDiagnostics(baseURL)) {
		return fmt.Errorf("endpoint %s is not https; to-router will not send the %s credential over a cleartext connection", baseURL, credentialType)
	}
	return nil
}

// sameDestinations reports whether two channel declarations describe the same
// destinations: same formats, same addresses each endpoint is reached at (an
// endpoint without its own address rides on its channel default), order
// ignored. Both sides are resolved first, so a row whose endpoints inherit the
// default compares equal to the stored one that carries it expanded.
func sameDestinations(a, b store.Channel) bool {
	resolved := func(c store.Channel) map[string]string {
		out := map[string]string{}
		for _, e := range c.Endpoints {
			out[e.Format] = strings.TrimRight(c.EndpointBaseURL(e), "/")
		}
		return out
	}
	left, right := resolved(a), resolved(b)
	if len(left) != len(right) {
		return false
	}
	for format, base := range left {
		other, ok := right[format]
		if !ok || other != base {
			return false
		}
	}
	return true
}
