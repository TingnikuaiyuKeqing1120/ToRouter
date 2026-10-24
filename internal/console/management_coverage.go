package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"torouter/internal/management"
	"torouter/internal/store"
)

// This is a coverage contract, not another router or tool registry. Exact
// method/path entries deliberately make a new mounted route fail the check.
type managementRouteCoverage struct {
	Route        string   `json:"route"`
	Capabilities []string `json:"capabilities,omitempty"`
	Exception    string   `json:"exception,omitempty"`
	Reason       string   `json:"reason,omitempty"`
}

func managementRouteContracts() []managementRouteCoverage {
	var out []managementRouteCoverage
	add := func(capability string, routes ...string) {
		for _, route := range routes {
			out = append(out, managementRouteCoverage{Route: route, Capabilities: strings.Fields(capability)})
		}
	}
	except := func(kind, reason string, routes ...string) {
		for _, route := range routes {
			out = append(out, managementRouteCoverage{Route: route, Exception: kind, Reason: reason})
		}
	}
	add("entities.search entities.get", "GET /channels", "GET /models", "GET /model-specs", "GET /prices", "GET /tags", "GET /tag-groups", "GET /client-keys", "GET /catalog/models")
	add("plugins.list", "GET /plugins")
	add("plugins.market.list", "GET /plugins/market")
	add("plugins.market.refresh", "POST /plugins/market/refresh")
	add("plugins.market.authorize", "POST /plugins/market/{id}/auth/{provider}/start")
	add("plugins.market.status", "GET /plugins/market/auth/{flow}", "POST /plugins/market/auth/{flow}/complete")
	add("plugins.market.cancel", "DELETE /plugins/market/auth/{flow}")
	except("user_authorization", "安装、更新和卸载外部可执行插件由本机控制台完成；MCP可读取目录并管理已安装供应商的授权和启停。", "POST /plugins/market/install", "DELETE /plugins/market/{id}")
	add("plugins.set", "PUT /plugins/{id}/capabilities/{cap}")
	add("plugins.connect_public", "POST /plugins/{id}/connect")
	add("key_discovery.sources", "GET /key-discovery/sources")
	add("key_discovery.scan", "POST /key-discovery/sessions", "POST /key-discovery/sessions/{id}/scan")
	add("key_discovery.preview", "GET /key-discovery/sessions/{id}")
	add("key_discovery.cancel", "DELETE /key-discovery/sessions/{id}")
	add("key_discovery.import", "POST /key-discovery/sessions/{id}/import")
	add("key_discovery.copy", "POST /key-discovery/sessions/{id}/copy")
	add("oauth_transfer.export", "POST /oauth-transfer/export")
	add("oauth_transfer.prepare", "POST /oauth-transfer/preview")
	add("oauth_transfer.preview", "GET /oauth-transfer/sessions/{id}")
	add("oauth_transfer.cancel", "DELETE /oauth-transfer/sessions/{id}")
	add("oauth_transfer.import", "POST /oauth-transfer/sessions/{id}/import")
	except("user_authorization", "安装并执行兼容代码属于本机控制台权限，不开放为MCP配置操作。", "POST /plugins/compatible/install")
	except("user_authorization", "含认证资料的完整系统备份与恢复由用户在控制台完成；MCP不得读取全量配置或master.key。", "POST /config-backup/export", "POST /config-backup/preview", "POST /config-backup/restore", "POST /config-backup/rollback", "GET /config-backup/rollback")
	add("channel.create", "POST /channels")
	add("credential.move", "POST /channels/{id}/credentials/move")
	add("channel.patch credential.patch model.spec.patch", "PUT /channels/{id}")
	add("channel.delete", "DELETE /channels/{id}")
	add("channel.display.read", "GET /channels/display")
	add("channel.display.patch", "PUT /channels/display")
	add("credential.reorder", "PUT /channels/{channelID}/credential-order")
	add("credential.patch", "PUT /channels/{channelID}/credentials/{credentialID}/enabled")
	add("credential.secret", "GET /channels/{channelID}/credentials/{credentialID}/key")
	base := "/channels/{channelID}/credentials/{credentialID}/operations"
	add("credential.operation.read", "GET "+base)
	add("credential.operation.configure", "PUT "+base)
	add("credential.operation.refresh_reset_cards", "POST "+base+"/refresh-reset-cards")
	add("credential.operation.warmup_credential", "POST "+base+"/warmup")
	add("credential.operation.use_reset_card", "POST "+base+"/consume-reset")
	add("credential.operation.acknowledge_credential_operation", "POST "+base+"/acknowledge")
	add("upstream.discover", "POST /channels/discover")
	add("upstream.probe", "POST /channels/probe", "POST /channels/{id}/probe")
	add("channel.model.delete", "DELETE /channels/{id}/models")
	for _, provider := range []string{"copilot", "chatgpt", "codex", "claude", "antigravity", "devin", "qoder", "qoder-cn", "workbuddy", "workbuddy-ai", "minimax-code", "minimax-code-global"} {
		add("oauth.start", "POST /oauth/"+provider+"/start")
		add("oauth.status", "GET /oauth/"+provider+"/status")
		add("oauth.cancel", "POST /oauth/"+provider+"/cancel")
	}
	add("upstream.oauth_discover", "POST /channels/{id}/oauth/models")
	add("quota.read", "GET /oauth/codex/usage")
	add("quota.refresh", "POST /oauth/codex/usage")
	add("public_model.create", "POST /models")
	add("public_model.patch", "PUT /models/{id}", "PUT /models/batch/status")
	add("public_model.rename", "POST /models/{id}/rename")
	add("public_model.delete", "DELETE /models/{id}", "POST /models/batch-delete")
	add("public_model.reorder", "PUT /models/reorder")
	add("models.backup.import", "POST /models/import")
	add("model.spec.public", "GET /models/specs")
	add("model.spec.patch", "PUT /model-specs")
	add("tag.patch", "POST /tags", "PUT /tags/{id}")
	add("tag.group.patch", "POST /tag-groups", "PUT /tag-groups/{id}")
	add("tag.delete", "DELETE /tags/{id}")
	add("tag.group.delete", "DELETE /tag-groups/{id}")
	add("tag.reorder", "PUT /tags/reorder")
	add("tag.group.reorder", "PUT /tag-groups/reorder")
	add("tag.references", "GET /tags/references")
	add("tag.reset_builtin", "POST /tags/reset-builtin")
	add("prices.update prices.delete", "PUT /prices")
	add("plans.read", "GET /plan-prices")
	add("plans.configure", "PUT /plan-prices/{id}")
	add("plans.stop", "DELETE /plan-prices/{id}")
	add("plans.current_charge usage.amount.preview usage.amount.apply", "PUT /plan-prices/{id}/charges/{month}")
	add("plans.billing_declaration", "PUT /channels/billing")
	add("catalog.sources.read", "GET /catalog/sources")
	add("catalog.sources.update", "PUT /catalog/sources")
	add("catalog.fetch", "POST /catalog/fetch")
	add("catalog.offerings.fetch", "POST /catalog/offerings/fetch")
	add("catalog.matches", "GET /catalog/matches")
	add("catalog.prefill.preview", "POST /catalog/prefill/preview")
	add("catalog.prefill.apply", "POST /catalog/prefill/apply")
	add("usage.backfill.preview", "POST /usage/backfill/preview")
	add("usage.backfill.apply", "POST /usage/backfill/apply")
	add("exchange.read", "GET /exchange-rate")
	add("exchange.refresh", "POST /exchange-rate")
	add("exchange.sources.read", "GET /exchange-rate/sources")
	add("exchange.sources.update", "PUT /exchange-rate/sources")
	add("settings.read", "GET /settings", "GET /settings/quota-refresh")
	add("settings.patch settings.retention", "PUT /settings")
	add("settings.patch", "PUT /settings/quota-refresh", "PUT /settings/oauth-auto-open")
	add("service.connection", "GET /connection", "GET /runtime/port")
	add("app_updates.read", "GET /app-updates")
	add("app_updates.check", "POST /app-updates/check")
	add("app_updates.install", "POST /app-updates/install")
	add("service.port", "PUT /runtime/port")
	add("service.restart_gateway", "POST /restart")
	add("service.stop_gateway", "POST /shutdown")
	add("runtime.status.read", "GET /status")
	add("runtime.status.reset", "POST /status/reset")
	add("quota.refresh", "POST /status/refresh-quota")
	add("quota.read", "GET /quotas")
	add("quota.refresh", "POST /quotas/refresh")
	add("usage.logs usage.detail usage.sensitive_detail", "GET /logs")
	add("usage.summary", "GET /usage")
	add("usage.export", "GET /usage/export")
	add("client_access.create", "POST /client-keys")
	add("client_access.patch", "PUT /client-keys/{id}")
	add("client_access.preview", "POST /client-keys/preview")
	add("client_access.delete", "DELETE /client-keys/{id}")
	add("client_access.secret", "GET /client-keys/{id}/key")
	add("companion.read", "GET /desktop/pet", "GET /companion", "GET /companion/preferences")
	add("companion.preferences", "PUT /companion/preferences")
	add("companion.open_companion", "POST /companion/open", "POST /desktop/launch-pet")
	add("companion.open_console", "POST /desktop/launch-gui", "POST /desktop/open-channel")
	add("companion.close_companion", "POST /desktop/close-pet")
	add("management_access.read", "GET /mcp/status", "GET /mcp/connections")
	except("ui_only", "控制台只读功能目录与MCP tools/list同源；Agent通过tools/list及get_help读取工具说明。", "GET /mcp/tools")
	add("management_access.revoke", "DELETE /mcp/connections/{id}")
	except("user_authorization", "管理授权根的签发、轮换和权限修改须由用户在控制台完成；MCP只能返回操作入口。", "POST /mcp/connections", "PATCH /mcp/connections/{id}", "POST /mcp/connections/{id}/rotate")
	except("ui_only", "此端点是控制台HTTP存活检查；MCP通过协议握手与gateway_overview检查所连实例。", "GET /health")
	sort.Slice(out, func(i, j int) bool { return out[i].Route < out[j].Route })
	return out
}

func managementRouteIssues(routes chi.Routes, operations []management.Operation, contracts []managementRouteCoverage) []string {
	var issues []string
	caps := map[string]bool{}
	for _, op := range operations {
		caps[op.Capability] = true
	}
	registered := map[string]managementRouteCoverage{}
	for _, row := range contracts {
		if _, exists := registered[row.Route]; exists {
			issues = append(issues, "duplicate route contract: "+row.Route)
		}
		registered[row.Route] = row
		if row.Exception != "" {
			if (row.Exception != "ui_only" && row.Exception != "user_authorization") || row.Reason == "" || len(row.Capabilities) != 0 {
				issues = append(issues, "invalid route exception: "+row.Route)
			}
		} else if len(row.Capabilities) == 0 {
			issues = append(issues, "route needs capability or explicit exception: "+row.Route)
		}
		for _, cap := range row.Capabilities {
			if !caps[cap] {
				issues = append(issues, fmt.Sprintf("%s: missing MCP capability %s", row.Route, cap))
			}
		}
	}
	mounted := map[string]bool{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		mounted[key] = true
		if _, ok := registered[key]; !ok {
			issues = append(issues, "unregistered mounted route: "+key)
		}
		return nil
	})
	if err != nil {
		issues = append(issues, "walk console: "+err.Error())
	}
	for key := range registered {
		if !mounted[key] {
			issues = append(issues, "contract no longer mounted: "+key)
		}
	}
	sort.Strings(issues)
	return issues
}

type managementFieldCoverage struct {
	Type         string `json:"type"`
	Field        string `json:"field"`
	Access       string `json:"access"`
	Capability   string `json:"capability,omitempty"`
	SchemaParent string `json:"schema_parent,omitempty"`
	InputField   string `json:"input_field,omitempty"`
	Reason       string `json:"reason"`
}

func managementFieldContracts() []managementFieldCoverage {
	var out []managementFieldCoverage
	add := func(typ, fields, access, capability, parent, reason string) {
		for _, field := range strings.Fields(fields) {
			out = append(out, managementFieldCoverage{Type: typ, Field: field, Access: access, Capability: capability, SchemaParent: parent, Reason: reason})
		}
	}
	add("Channel", "name base_url endpoints enabled models available_models manual_models tags model_tags dispatch_mode chain_type preferred_endpoint", "writable", "channel.patch", "patch", "显式patch省略保持；目的地址变化另查已有秘密使用权限。")
	add("Channel", "model_formats", "readonly", "entities.get", "", "接入实现获取的逐模型协议；与目录原子保存，普通编辑不能覆盖。")
	add("Channel", "id generation operations_revision protocol", "readonly", "entities.get", "", "稳定身份、运行版本及规范化协议投影由存储维护，不作为普通patch。")
	add("Channel", "api_key credentials", "dedicated", "credential.patch", "", "凭证池由独立精确凭证操作维护；已存秘密默认不返回。")
	add("Channel", "model_configs model_catalog_refs", "dedicated", "model.spec.patch", "", "按精确上游模型修改规格与资料引用。")
	add("Channel", "model_evidence", "readonly", "entities.get", "", "资料与探测证据是来源记录，不作为普通配置覆盖。")
	add("Channel", "credential_type", "dedicated", "channel.create", "", "新建渠道选择固定内置接入类型。")
	add("Channel", "oauth", "user_authorization", "oauth.start", "", "授权材料通过独立授权流程建立，禁止直接注入令牌。")
	add("Settings", "gateway_key", "authorization_root", "", "", "管理员主密钥不能经MCP读取或改写。")
	add("Settings", "prices", "dedicated", "prices.update", "", "价格身份、版本与删除有独立领域操作。")
	add("Settings", "retention_days", "dedicated", "settings.retention", "", "保存保留策略会清理历史，需维护预览。")
	add("Settings", "max_attempts retries_per_channel failure_cooldown_seconds request_defaults_enabled request_defaults reasoning_policy oauth_auto_open quota_refresh_seconds default_launch_mode", "writable", "settings.patch", "patch", "业务运行设置使用明确字段patch，不含授权根。")
	add("ModelConfig", "output_token_field native_thinking max_output_tokens context_window max_input_tokens output_limit streaming input_modalities output_modalities thinking_efforts", "dedicated", "model.spec.patch", "", "规格map由Store.PatchModelSpecification显式白名单和类型校验；测试另比对真实白名单。")
	add("DownstreamModel", "display_name enabled fallback sort_order bindings tags catalog_model_ref", "writable", "public_model.patch", "items[].patch", "公开模型patch必须在实际输入schema显式提供字段。")
	add("DownstreamModel", "id", "dedicated", "public_model.rename", "", "公开ID通过原子重命名同步引用，不能作为普通patch。")
	add("DownstreamModel", "effective_tags", "readonly", "entities.get", "", "继承标签按绑定及组规则派生，不接受直接写入。")
	add("CredentialItem", "id label enabled api_key", "writable", "credential.patch", "items[]", "稳定ID明确指定成员；API Key只写，敏感读取另授权。")
	add("CredentialItem", "source_channel_id", "writable", "credential.patch", "items[]", "用户批准MCP首版暂不开放OAuth复制；source_channel_id仅可复制API Key。正常授权、重新授权和现有OAuth凭证池管理保留。Web旧复制能力的共享续期修复另行处理。")
	add("CredentialItem", "oauth", "user_authorization", "oauth.start", "", "OAuth材料只能由授权流程建立或重新授权。")
	add("CredentialItem", "oauth_info", "dedicated", "credential.identity", "", "账号摘要仅明确敏感读取，不是可写授权材料。")
	add("CredentialItem", "has_key has_oauth masked_key operations_revision", "readonly", "entities.get", "", "存在性、掩码和配置版本由服务端派生。")
	add("CredentialItem", "operations", "dedicated", "credential.operation.configure", "", "自动操作配置用精确身份与专用操作维护。")
	add("Endpoint", "format base_url state", "writable", "channel.patch", "patch.endpoints[]", "端点声明仍走现有渠道校验。")
	add("Binding", "channel_id model priority enabled", "writable", "public_model.patch", "items[].patch.bindings[]", "绑定数组明确替换，身份与引用由领域校验。")
	add("CapabilityEvidence", "source verified_at api_format", "readonly", "entities.get", "", "来源证据不是用户声明的能力证明。")
	add("CatalogModelRef", "source id offering_key", "dedicated", "model.spec.patch", "", "精确资料引用可在规格工具设定，匹配和预填复用同一资料身份。")
	add("ModelDefaults", "max_output_tokens temperature thinking", "writable", "settings.patch", "patch.request_defaults", "仅设置请求默认值；零/未知语义由原校验保留。")
	add("ThinkingDefault", "mode effort budget enabled", "writable", "settings.patch", "patch.request_defaults.thinking", "推理默认值随明确默认配置提交。")
	add("NativeThinking", "supported mandatory modes", "dedicated", "model.spec.patch", "", "模型原生推理说明由规格类型校验。")
	add("NativeThinkingMode", "id api_format control output_token_field efforts default_effort disabled_efforts budget_min budget_max", "dedicated", "model.spec.patch", "", "规格嵌套类型字段受MergeFields递归检查与ValidateModelConfig约束。")
	add("OAuthCredential", "provider github_token refresh_token id_token client_id host_id subject account scopes access_token expires_at account_label api_base status status_reason updated_at organization_id device_id project_id plugin_state", "user_authorization", "oauth.start", "", "内部OAuth授权/身份/轮转状态由现有服务管理；访问和刷新令牌及完整插件状态无通用读取/写入工具。")
	add("OAuthInfo", "provider account_label api_base status status_reason expires_at updated_at", "readonly", "credential.identity", "", "授权的去令牌摘要；只能在明确身份读取中查看，不能反写。")
	add("CredentialOperationsConfig", "warmup auto_reset", "writable", "credential.operation.configure", "patch", "自动操作配置独立于账号凭证。")
	add("CredentialWarmupConfig", "enabled mode model daily_times", "writable", "credential.operation.configure", "patch.warmup", "自动预热沿用额度前提和原有调度规则。")
	add("CredentialAutoResetConfig", "enabled rules", "writable", "credential.operation.configure", "patch.auto_reset", "自动重置配置沿用现行规则。")
	add("CredentialResetRule", "window_seconds remaining_below expires_within_days join", "writable", "credential.operation.configure", "patch.auto_reset.rules[]", "精确窗口与条件组合由原配置校验。")
	add("PriceEntry", "channel_id model display_name origin selection", "writable", "prices.update", "edits[].price", "按价格身份及版本维护完整现行计价声明。")
	add("PriceSelection", "mode table ref", "writable", "prices.update", "edits[].price.selection", "手动表或资料引用使用当前唯一模型。")
	add("PriceTable", "currency unit_tokens rates rules uncovered_conditions", "writable", "prices.update", "edits[].price.selection.table", "币种、单位、条件和未知覆盖沿用现行价格校验。")
	add("PriceRates", "input output cache_read cache_write", "writable", "prices.update", "edits[].price.selection.table.rates", "指针保留未知与零价格的区别。")
	add("CatalogPriceRef", "source id offering_key quote_key", "writable", "prices.update", "edits[].price.selection.ref", "资料价格必须使用精确区域/报价身份。")
	add("PriceRule", "draft id enabled when action", "writable", "prices.update", "edits[].price.selection.table.rules[]", "条件价格声明复用现行价格类型及校验。")
	add("PriceRuleWhen", "input_range time_window weekdays date_set timezone", "writable", "prices.update", "edits[].price.selection.table.rules[].when", "条件交集按现行计价规则解释。")
	add("PriceInputRange", "from_tokens to_tokens", "writable", "prices.update", "edits[].price.selection.table.rules[].when.input_range", "输入区间边界保留现行单位。")
	add("PriceTimeWindow", "start end all_day timezone", "writable", "prices.update", "edits[].price.selection.table.rules[].when.time_window", "时段按声明时区解释。")
	add("PriceRuleAction", "rate_changes multiplier", "writable", "prices.update", "edits[].price.selection.table.rules[].action", "完整单价/倍数声明由领域校验。")
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type == out[j].Type {
			return out[i].Field < out[j].Field
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// Recurse from the current domain roots. A new nested JSON field/type is also
// unknown until it receives an explicit disposition; no wildcard registration.
func managementCoverageTypes() map[string]reflect.Type {
	out := map[string]reflect.Type{}
	var visit func(reflect.Type)
	visit = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Array || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || typ.PkgPath() == "time" || typ.PkgPath() != "torouter/internal/store" {
			return
		}
		if _, seen := out[typ.Name()]; seen {
			return
		}
		out[typ.Name()] = typ
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.IsExported() && field.Tag.Get("json") != "-" {
				visit(field.Type)
			}
		}
	}
	for _, root := range []any{store.Channel{}, store.Settings{}, store.ModelConfig{}, store.DownstreamModel{}, store.CredentialItem{}} {
		visit(reflect.TypeOf(root))
	}
	return out
}

type managementHelpExample struct {
	Scenario  string         `json:"scenario"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

func managementHelpExamples() []managementHelpExample {
	// Versions/IDs are illustrative values obtained from a preceding read, never
	// values a client should copy blindly. No example invokes a real upstream.
	const version = "<读取结果中的version>"
	scope := map[string]any{"model_mode": "all", "model_ids": []any{}, "tag_rules": map[string]any{"billing": []any{"billing:free"}, "privacy": []any{"privacy:zdr"}}}
	return []managementHelpExample{
		{"查看当前工具参数", "get_help", map[string]any{"tool": "update_channel"}},
		{"读取一个明确月份的用量", "get_usage_summary", map[string]any{"filter": map[string]any{"from": "2026-10-01T00:00:00+08:00", "to": "2026-11-01T00:00:00+08:00", "timezone": "Asia/Shanghai"}}},
		{"先定位准确渠道", "search_entities", map[string]any{"kind": "channel", "query": "备用", "limit": 20}},
		{"读取渠道与版本", "get_entities", map[string]any{"refs": []any{map[string]any{"kind": "channel", "channel_id": 1}}}},
		{"添加独立备用Key", "update_credentials", map[string]any{"request_id": "example-add-credential", "channel_id": 1, "expected_version": version, "items": []any{map[string]any{"id": "backup", "label": "备用凭证", "api_key": "<本次提供的新密钥>", "enabled": true}}}},
		{"预览免费且ZDR的访问范围", "preview_client_scope", map[string]any{"scope": scope}},
		{"保存已检查的访问范围", "update_client_key", map[string]any{"request_id": "example-update-scope", "id": 1, "expected_version": version, "scope": scope}},
		{"预览资料库预填", "preview_catalog_prefill", map[string]any{"request_id": "example-prefill-preview", "request": map[string]any{"targets": []any{map[string]any{"kind": "upstream", "channel_id": 1, "model": "example-model"}}, "groups": []any{"capabilities"}, "force_overwrite": false}}},
		{"应用已检查的资料预填", "apply_catalog_prefill", map[string]any{"request_id": "example-prefill-apply", "preview_id": "<预览返回的preview_id>"}},
		{"读取异常状态", "get_runtime_status", map[string]any{}},
		{"恢复明确凭证", "reset_runtime_status", map[string]any{"request_id": "example-reset-status", "channel_id": 1, "credential_id": "cred_default"}},
		{"读取已有自动预热配置", "get_credential_operations", map[string]any{"channel_id": 1, "credential_id": "cred_default"}},
		{"修改自动预热分支", "configure_credential_automation", map[string]any{"request_id": "example-automation", "channel_id": 1, "credential_id": "cred_default", "expected_identity": "<读取结果中的identity>", "patch": map[string]any{"warmup": map[string]any{"enabled": true, "mode": "daily", "model": "example-model", "daily_times": []any{"09:00"}}}}},
	}
}

func managementReferenceJSON(operations []management.Operation) ([]byte, error) {
	doc := struct {
		FormatVersion int                       `json:"format_version"`
		Routes        []managementRouteCoverage `json:"routes"`
		Fields        []managementFieldCoverage `json:"fields"`
		Tools         []management.Operation    `json:"tools"`
		Examples      []managementHelpExample   `json:"examples"`
	}{1, managementRouteContracts(), managementFieldContracts(), operations, managementHelpExamples()}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
