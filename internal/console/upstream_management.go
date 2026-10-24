package console

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"torouter/internal/accessguard"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

// UpstreamManagementDraft contains only the connection fields supported by the
// existing discovery/probe pipeline. It cannot import a credential identity or
// redirect an already stored secret to a different destination.
type UpstreamManagementDraft struct {
	BaseURL   string           `json:"base_url,omitempty"`
	APIKey    string           `json:"api_key"`
	Endpoints []store.Endpoint `json:"endpoints"`
}
type UpstreamManagementInput struct {
	ChannelID     int64                    `json:"channel_id,omitempty"`
	Draft         *UpstreamManagementDraft `json:"draft,omitempty"`
	CredentialIDs []string                 `json:"credential_ids,omitempty"`
	Formats       []string                 `json:"formats,omitempty"`
	Model         string                   `json:"model,omitempty"`
	Apply         bool                     `json:"apply,omitempty"`
	Confirmed     bool                     `json:"confirmed,omitempty"`
}

func (u *upstreamTools) managementChannel(in UpstreamManagementInput) (store.Channel, error) {
	if in.ChannelID > 0 {
		if in.Draft != nil {
			return store.Channel{}, &ManagementToolError{Status: 400, Message: "存储渠道只能探测既有地址；新地址请提供独立草稿和本次输入的新密钥。"}
		}
		channel, err := u.store.Channel(in.ChannelID)
		if err != nil {
			return store.Channel{}, &ManagementToolError{Status: 404, Message: "channel not found"}
		}
		seen := map[string]bool{}
		for _, id := range in.CredentialIDs {
			found := false
			for _, credential := range channel.Credentials {
				if id == credential.ID {
					found = true
					break
				}
			}
			if !found || seen[id] {
				return store.Channel{}, &ManagementToolError{Status: 400, Message: "请指定该渠道中准确且不重复的凭证 ID。"}
			}
			seen[id] = true
		}
		return channel, nil
	}
	if in.ChannelID < 0 || in.Draft == nil {
		return store.Channel{}, &ManagementToolError{Status: 400, Message: "请指定 channel_id 或连接草稿。"}
	}
	if len(in.CredentialIDs) > 0 || in.Apply {
		return store.Channel{}, &ManagementToolError{Status: 400, Message: "草稿不能引用已存凭证或写入模型目录。"}
	}
	if strings.TrimSpace(in.Draft.APIKey) == "" {
		return store.Channel{}, &ManagementToolError{Status: 400, Message: "新连接草稿必须提供本次输入的 API Key。"}
	}
	channel := store.Channel{BaseURL: in.Draft.BaseURL, APIKey: in.Draft.APIKey, Endpoints: in.Draft.Endpoints, CredentialType: store.CredentialAPIKey, Enabled: true}
	channel.Normalize()
	return channel, nil
}
func (u *upstreamTools) Discover(ctx context.Context, in UpstreamManagementInput) (any, error) {
	channel, err := u.managementChannel(in)
	if err != nil {
		return nil, err
	}
	expectedVersion := store.EntityVersion(channel)
	if channel.CredentialType != store.CredentialAPIKey {
		return nil, &ManagementToolError{Status: 400, Message: "OAuth 渠道请使用 OAuth 模型目录操作。"}
	}
	if len(in.CredentialIDs) > 1 {
		return nil, &ManagementToolError{Status: 400, Message: "一次模型发现请指定一个凭证。"}
	}
	if len(in.CredentialIDs) == 1 {
		for _, credential := range channel.Credentials {
			if credential.ID == in.CredentialIDs[0] {
				channel.Credentials = []store.CredentialItem{credential}
				channel.APIKey = credential.APIKey
				break
			}
		}
	}
	req, err := u.prepareDraft(draftRequest{Channel: channel, Formats: in.Formats, Apply: in.Apply}, false)
	if err != nil {
		return nil, err
	}
	if accessguard.Present(ctx) {
		req.expectedVersion = expectedVersion
	}
	return u.discoverCore(ctx, req)
}
func (u *upstreamTools) Probe(ctx context.Context, in UpstreamManagementInput) (any, error) {
	channel, err := u.managementChannel(in)
	if err != nil {
		return nil, err
	}
	if in.Apply {
		return nil, &ManagementToolError{Status: 400, Message: "探测不能写入模型目录。"}
	}
	if in.ChannelID > 0 {
		return u.probeStoredCore(ctx, channel.ID, storedProbeRequest{Model: in.Model, Confirmed: in.Confirmed, Formats: in.Formats, CredentialIDs: in.CredentialIDs, RequireExactCredentials: true})
	}
	req, err := u.prepareDraft(draftRequest{Channel: channel, Formats: in.Formats, Model: in.Model, Confirmed: in.Confirmed}, true)
	if err != nil {
		return nil, err
	}
	return u.probeCore(ctx, req)
}
func probeIdentity(destination, identity store.Channel) store.Channel {
	destination.OAuth = identity.OAuth
	destination.APIKey = identity.APIKey
	destination.Credentials = identity.Credentials
	return destination
}
func (u *upstreamTools) prepareDraft(body draftRequest, probe bool) (draftRequest, error) {
	c := body.Channel
	if c.ID != 0 {
		channels, err := u.store.ListChannels()
		if err != nil {
			return body, &ManagementToolError{Status: 500, Message: "cannot read channel"}
		}
		found := false
		for _, old := range channels {
			if old.ID == c.ID {
				found = true
				hasFreshKey := false
				for _, cr := range c.Credentials {
					if strings.TrimSpace(cr.APIKey) != "" {
						hasFreshKey = true
						break
					}
				}
				if !hasFreshKey && strings.TrimSpace(c.APIKey) != "" {
					hasFreshKey = true
				}
				if !hasFreshKey {
					// A saved key may only be reused when the destination is
					// unchanged. With endpoints that means every endpoint's
					// format and the address it is reached at, not just the first
					// one — and an endpoint that declares no address of its own
					// is reached at the channel default, so re-pointing the
					// default changes the destination too.
					if !sameDestinations(c, old) {
						return body, &ManagementToolError{Status: 400, Message: "连接地址或协议已改变，请重新输入密钥后测试/拉取。"}
					}
					if len(c.Credentials) == 0 && len(old.Credentials) > 0 {
						c.Credentials = old.Credentials
					} else {
						for i := range c.Credentials {
							if c.Credentials[i].APIKey == "" {
								for _, oc := range old.Credentials {
									if oc.ID == c.Credentials[i].ID {
										c.Credentials[i].APIKey = oc.APIKey
										break
									}
								}
							}
						}
					}
				}
				c.Normalize()
				break
			}
		}
		if !found {
			return body, &ManagementToolError{Status: 404, Message: "channel not found"}
		}
	}
	body.Channel = c
	validation := c
	validation.Name = "draft"
	validation.Models = nil
	validation.AvailableModels = nil
	validation.ModelFormats = nil
	if err := store.ValidateChannel(validation); err != nil {
		return body, &ManagementToolError{Status: 400, Message: err.Error()}
	}
	// An empty credential type is a row written before the field existed:
	// it is an api_key channel (see Channel.normalize).
	if c.CredentialType != "" && c.CredentialType != store.CredentialAPIKey {
		// Discover and probe authenticate with an API key, and an OAuth channel
		// has none: its credential is refreshed and injected per request by the
		// gateway, and its model catalogue is pulled by the authorization flow.
		// Saying so beats sending a keyless request and reporting a 401 the
		// owner cannot act on. Wiring the token service into these two tools
		// belongs with the OAuth console UI.
		return body, &ManagementToolError{Status: 400, Message: "OAuth 渠道暂不支持获取/测试，请直接发请求验证；模型目录随授权流程自动拉取。"}
	}
	if probe && (!body.Confirmed || strings.TrimSpace(body.Model) == "" || len(body.Model) > 512) {
		return body, &ManagementToolError{Status: 400, Message: "请选择模型并确认测试可能产生上游费用。"}
	}
	return body, nil
}

func (u *upstreamTools) discoverCore(ctx context.Context, req draftRequest) (result toolResult, err error) {
	c := req.Channel
	formats, message := requestedFormats(req.Formats)
	if message != "" {
		return result, &ManagementToolError{Status: 400, Message: message}
	}
	if len(formats) == 0 {
		// An unnamed discover answers for everything the channel declares: a
		// catalogue is per channel, and every facade that answers is evidence
		// about what it serves.
		formats = declaredFormats(c)
	}
	if len(formats) == 0 {
		return result, &ManagementToolError{Status: 400, Message: "渠道未声明任何端点。"}
	}
	start := time.Now()
	result = toolResult{}
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	entries := make([]formatResult, 0, len(formats))
	planned := make([]formatResult, 0, len(formats))
	for _, format := range formats {
		planned = append(planned, formatResult{Format: format})
	}
	for _, format := range formats {
		if err := accessguard.Check(ctx); err != nil {
			return blockedToolResult(result, entries, planned), err
		}
		began := time.Now()
		models, status, d := u.fetchModels(ctx, c.APIKey, familyOf(format), baseForFormat(c, format))
		if d != nil && d.Code == "access_revoked" && status == 0 {
			return blockedToolResult(result, entries, planned), blockedUpstreamError(ctx)
		}
		entry := formatResult{Format: format, OK: d == nil, Models: models, DurationMS: time.Since(began).Milliseconds(), UpstreamStatus: status, Diagnostic: d}
		entries = append(entries, entry)
		if err := accessguard.Check(ctx); err != nil {
			return blockedToolResult(result, entries, planned), err
		}
	}
	if len(formats) == 1 {
		// One format keeps the flat answer the console's model list and its
		// existing clients read: ok, models, diagnostic.
		result.OK = entries[0].OK
		result.Models = entries[0].Models
		result.UpstreamStatus = entries[0].UpstreamStatus
		result.Diagnostic = entries[0].Diagnostic
	} else {
		result.Formats = entries
		result.OK = true
		for _, entry := range entries {
			if !entry.OK {
				result.OK = false
			}
		}
	}
	if !result.OK {
		return result, nil
	}
	if req.Apply && c.ID != 0 {
		// The fetch is the authority on the auto part of the catalogue from here
		// on, so it is written back rather than unioned into what the console
		// happened to hold. A model the upstream no longer lists stops being a
		// catalogue entry; the owner's manual additions and the channel's enabled
		// models are kept. The fetched set is the union across the formats that
		// answered: one channel, one catalogue, and every facade that works is
		// evidence about what it serves.
		ids := []string{}
		seen := map[string]bool{}
		for _, entry := range entries {
			for _, m := range entry.Models {
				if !seen[m.ID] {
					seen[m.ID] = true
					ids = append(ids, m.ID)
				}
			}
		}
		if err := accessguard.Check(ctx); err != nil {
			return blockedToolResult(result, entries, planned), err
		}
		var updated store.Channel
		var err error
		if req.expectedVersion != "" {
			updated, err = u.store.ReplaceCatalogueAtVersion(c.ID, ids, req.expectedVersion, func() error { return accessguard.Check(ctx) })
		} else {
			updated, err = u.store.ReplaceCatalogue(c.ID, ids)
		}
		if accessguard.Denied(err) {
			return blockedToolResult(result, entries, planned), err
		}
		if errors.Is(err, store.ErrEditConflict) {
			return result, err
		}
		if err != nil {
			result.OK = false
			result.Models = nil
			result.Catalogue = nil
			result.Formats = nil
			result.Diagnostic = &diagnostic{Source: "gateway", Code: "catalogue_write", Message: "模型目录已获取，但写入渠道失败。", Suggestion: "重试获取，或手动勾选模型后保存渠道。"}
			return result, nil
		}
		result.Catalogue = updated.AvailableModels
	}
	return result, nil
}

func (u *upstreamTools) probeCore(ctx context.Context, req draftRequest) (result toolResult, err error) {
	c, model := req.Channel, req.Model
	formats, message := requestedFormats(req.Formats)
	if message != "" {
		return result, &ManagementToolError{Status: 400, Message: message}
	}
	declared := declaredFormats(c)
	if len(declared) == 0 {
		return result, &ManagementToolError{Status: 400, Message: "渠道未声明任何端点。"}
	}
	if c.ModelFormats != nil {
		allowed := c.ModelOutboundFormats(model)
		if len(allowed) == 0 {
			return result, &ManagementToolError{Status: 400, Message: "该模型没有已知可用协议，请刷新模型目录或配置模型协议。"}
		}
		for _, format := range formats {
			if !slices.Contains(allowed, format) {
				return result, &ManagementToolError{Status: 400, Message: "该模型未声明此协议，请选择其实际协议。"}
			}
		}
		if len(formats) == 0 {
			formats = allowed[:1]
		}
	}
	if len(formats) == 0 && len(declared) > 0 {
		// A probe is a paid generation, so an unnamed one tests the first
		// declared endpoint only: the console's per-model test promises one
		// request, not one per format. Naming formats is how the other facades
		// are asked.
		formats = declared[:1]
	}
	if len(formats) == 0 {
		return result, &ManagementToolError{Status: 400, Message: "渠道未声明任何端点。"}
	}
	start := time.Now()
	result = toolResult{Model: model}
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()
	credsToTest := selectCredentialsToTest(c, req.CredentialIDs)
	entries := make([]formatResult, 0, len(credsToTest)*len(formats))
	planned := plannedProbeResults(credsToTest, formats)
	for _, cred := range credsToTest {
		if err := accessguard.Check(ctx); err != nil {
			return blockedToolResult(result, entries, planned), err
		}
		key := cred.APIKey
		if key == "" {
			key = c.PrimaryKey()
		}
		label := cred.Label
		if label == "" {
			label = cred.ID
		}
		for _, format := range formats {
			if err := accessguard.Check(ctx); err != nil {
				return blockedToolResult(result, entries, planned), err
			}
			began := time.Now()
			status, d := u.probeFormat(ctx, key, format, baseForFormat(c, format), model)
			if d != nil && d.Code == "access_revoked" && status == 0 {
				return blockedToolResult(result, entries, planned), blockedUpstreamError(ctx)
			}
			entries = append(entries, formatResult{
				Format:          format,
				CredentialID:    cred.ID,
				CredentialLabel: label,
				OK:              d == nil,
				DurationMS:      time.Since(began).Milliseconds(),
				UpstreamStatus:  status,
				Diagnostic:      d,
			})
			if err := accessguard.Check(ctx); err != nil {
				return blockedToolResult(result, entries, planned), err
			}
		}
	}
	if len(entries) == 1 {
		// One format keeps the flat answer the console's model-list test button
		// and its existing clients read.
		result.OK = entries[0].OK
		result.UpstreamStatus = entries[0].UpstreamStatus
		result.Diagnostic = entries[0].Diagnostic
		return result, nil
	}
	result.Formats = entries
	result.OK = true
	for _, entry := range entries {
		if !entry.OK {
			result.OK = false
		}
	}
	return result, nil
}

func (u *upstreamTools) probeStoredCore(ctx context.Context, id int64, body storedProbeRequest) (result toolResult, err error) {
	if id < 1 {
		return result, &ManagementToolError{Status: 400, Message: "invalid channel id"}
	}
	channel, err := u.store.Channel(id)
	if err != nil {
		return result, &ManagementToolError{Status: 404, Message: "channel not found"}
	}
	if u.plugins != nil && channel.CredentialType != store.CredentialAPIKey {
		runtime, guarded, resolveErr := u.plugins.Native(ctx, channel.CredentialType)
		if resolveErr != nil {
			if errors.Is(resolveErr, plugins.ErrDisabled) {
				return toolResult{OK: false, Model: body.Model, Diagnostic: &diagnostic{Source: "gateway", Code: "plugin_disabled", Message: "该接入插件已停用。", Suggestion: "在插件库启用对应的直连能力后重试。"}}, nil
			}
			return result, resolveErr
		}
		view := *u
		view.copilot, view.siwc, view.codex, view.native = runtime.Copilot, runtime.SIWC, runtime.Codex, runtime.Native
		view.public = runtime.Public
		view.compatible = runtime.Compatible
		view.market = runtime.Market
		u, ctx = &view, guarded
	}
	if body.RequireExactCredentials {
		for _, id := range body.CredentialIDs {
			found := false
			for _, credential := range channel.Credentials {
				if credential.ID == id {
					found = true
					break
				}
			}
			if !found {
				return result, &ManagementToolError{Status: 400, Message: "指定的凭证已不存在，请重新读取渠道。"}
			}
		}
	}
	if !body.Confirmed {
		// The same paid-generation confirmation the draft probe asks for: the
		// request costs money at the upstream whoever is holding the credential.
		return result, &ManagementToolError{Status: 400, Message: "请确认测试：探测会向上游发送一次付费生成请求。"}
	}
	model := strings.TrimSpace(body.Model)
	if model == "" || len(model) > 512 {
		return result, &ManagementToolError{Status: 400, Message: "请填写要测试的模型 ID。"}
	}
	formats, message := requestedFormats(body.Formats)
	if message != "" {
		return result, &ManagementToolError{Status: 400, Message: message}
	}
	// The declaration under test is the draft's when the editor sent one: the
	// probe must answer for the endpoints the editor shows, so a re-pointed
	// endpoint is tested where it now points before anything is saved. The
	// credential stays the stored row's — storedBearer and the api_key check
	// below both read the row the path names — and a draft endpoint is checked
	// by the same allowlist and format re-check a stored one is, because a real
	// credential is sent to whatever address is named here.
	underTest := channel
	if body.Channel != nil {
		underTest.BaseURL = body.Channel.BaseURL
		underTest.Endpoints = body.Channel.Endpoints
	}
	declared := declaredFormats(underTest)
	if len(declared) == 0 {
		return result, &ManagementToolError{Status: 400, Message: "渠道未声明任何端点。"}
	}
	if underTest.ModelFormats != nil {
		allowed := underTest.ModelOutboundFormats(model)
		if len(allowed) == 0 {
			return result, &ManagementToolError{Status: 400, Message: "该模型没有已知可用协议，请刷新模型目录或配置模型协议。"}
		}
		if len(formats) == 0 {
			formats = allowed[:1]
		}
	}
	if len(formats) == 0 && len(declared) > 0 {
		// A probe is a paid generation, so an unnamed one tests the first
		// declared endpoint only, exactly like the draft probe.
		formats = declared[:1]
	}
	if len(formats) == 0 {
		return result, &ManagementToolError{Status: 400, Message: "渠道未声明任何端点。"}
	}
	if channel.CredentialType == store.CredentialAPIKey && channel.PrimaryKey() == "" {
		// Nothing to authenticate with and nothing to ask the caller for: the
		// key is write-only, so the answer is to re-enter it in the editor.
		return result, &ManagementToolError{Status: 400, Message: "该渠道没有已保存的密钥；请在渠道编辑中重新输入密钥后测试。"}
	}
	start := time.Now()
	result = toolResult{Model: model}
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()
	credsToTest := selectCredentialsToTest(channel, body.CredentialIDs)
	entries := make([]formatResult, 0, len(credsToTest)*len(formats))
	planned := plannedProbeResults(credsToTest, formats)
	for _, cred := range credsToTest {
		if err := accessguard.Check(ctx); err != nil {
			return blockedToolResult(result, entries, planned), err
		}
		label := cred.Label
		if label == "" {
			label = cred.ID
		}
		credChan := channel
		credChan.OAuth = cred.OAuth
		credChan.APIKey = cred.APIKey
		credChan.Credentials = []store.CredentialItem{cred}
		credential, d := u.storedBearer(ctx, credChan)
		if err := accessguard.Check(ctx); err != nil {
			return blockedToolResult(result, entries, planned), err
		}
		if d != nil {
			for _, format := range formats {
				entries = append(entries, formatResult{
					Format:          format,
					CredentialID:    cred.ID,
					CredentialLabel: label,
					OK:              false,
					Diagnostic:      d,
				})
			}
			continue
		}
		for _, format := range formats {
			if err := accessguard.Check(ctx); err != nil {
				return blockedToolResult(result, entries, planned), err
			}
			began := time.Now()
			status, d := u.storedProbeFormat(ctx, probeIdentity(underTest, credChan), credential, format, model)
			if d != nil && d.Code == "access_revoked" && status == 0 {
				return blockedToolResult(result, entries, planned), blockedUpstreamError(ctx)
			}
			entries = append(entries, formatResult{
				Format:          format,
				CredentialID:    cred.ID,
				CredentialLabel: label,
				OK:              d == nil,
				DurationMS:      time.Since(began).Milliseconds(),
				UpstreamStatus:  status,
				Diagnostic:      d,
			})
			if err := accessguard.Check(ctx); err != nil {
				return blockedToolResult(result, entries, planned), err
			}
		}
	}
	if len(entries) == 1 {
		// One format keeps the flat answer the console's per-model test button
		// and its existing clients read.
		result.OK = entries[0].OK
		result.UpstreamStatus = entries[0].UpstreamStatus
		result.Diagnostic = entries[0].Diagnostic
		return result, nil
	}
	result.Formats = entries
	result.OK = true
	for _, entry := range entries {
		if !entry.OK {
			result.OK = false
		}
	}
	return result, nil
}

func plannedProbeResults(credentials []store.CredentialItem, formats []string) []formatResult {
	planned := make([]formatResult, 0, len(credentials)*len(formats))
	for _, credential := range credentials {
		label := credential.Label
		if label == "" {
			label = credential.ID
		}
		for _, format := range formats {
			planned = append(planned, formatResult{Format: format, CredentialID: credential.ID, CredentialLabel: label})
		}
	}
	return planned
}

func blockedToolResult(result toolResult, completed, planned []formatResult) toolResult {
	result.OK = false
	result.Formats = append([]formatResult(nil), completed...)
	for _, pending := range planned[len(completed):] {
		pending.Diagnostic = &diagnostic{Source: "gateway", Code: "not_sent", Message: "后续步骤已被阻止，未向上游发送请求。", Suggestion: "先读取回执及当前状态；不要重发已完成的步骤。"}
		result.Formats = append(result.Formats, pending)
	}
	result.Diagnostic = &diagnostic{Source: "gateway", Code: "blocked", Message: "操作已停止；已收到的逐项响应已保留，后续发送或写入已被阻止。", Suggestion: "恢复授权后读取原 request_id 的回执，只处理明确未发送的后续项。"}
	return result
}

func blockedUpstreamError(ctx context.Context) error {
	if err := accessguard.Check(ctx); err != nil {
		return err
	}
	return &ManagementToolError{Status: 403, Message: "本次上游发送因管理授权检查而停止。"}
}

// OperationStatus distinguishes already received responses from the remaining
// blocked work. No response means a connection failure can still be unknown.
func (result toolResult) OperationStatus() string {
	if result.Diagnostic == nil || result.Diagnostic.Code != "blocked" {
		return ""
	}
	unknown := false
	received := false
	for _, entry := range result.Formats {
		if entry.Diagnostic != nil && entry.Diagnostic.Code == "not_sent" {
			continue
		}
		if entry.Diagnostic != nil && entry.Diagnostic.Source == "network" {
			unknown = true
			continue
		}
		if entry.UpstreamStatus > 0 {
			received = true
			continue
		}
		unknown = true
	}
	if unknown {
		return "unknown"
	}
	if received {
		return "partial"
	}
	return "failed"
}
