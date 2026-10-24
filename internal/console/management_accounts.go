package console

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"torouter/internal/management"
	"torouter/internal/store"
)

type credentialRef struct {
	ChannelID    int64  `json:"channel_id"`
	CredentialID string `json:"credential_id"`
}
type credentialChange struct {
	ID                 string  `json:"id"`
	Label              *string `json:"label,omitempty"`
	APIKey             *string `json:"api_key,omitempty"`
	Enabled            *bool   `json:"enabled,omitempty"`
	SourceChannelID    int64   `json:"source_channel_id,omitempty"`
	SourceCredentialID string  `json:"source_credential_id,omitempty"`
}
type credentialsInput struct {
	command
	ChannelID       int64              `json:"channel_id"`
	ExpectedVersion string             `json:"expected_version"`
	Items           []credentialChange `json:"items"`
}
type credentialDeleteInput struct {
	maintenanceInput
	credentialRef
	ExpectedVersion string `json:"expected_version"`
}
type credentialOrderInput struct {
	command
	ChannelID     int64    `json:"channel_id"`
	CredentialIDs []string `json:"credential_ids"`
}
type credentialActionInput struct {
	command
	credentialRef
	ExpectedIdentity string `json:"expected_identity"`
	Kind             string `json:"kind,omitempty"`
}
type automationInput struct {
	command
	credentialRef
	ExpectedIdentity string                          `json:"expected_identity"`
	Patch            store.CredentialOperationsPatch `json:"patch"`
}
type oauthStartInput struct {
	command
	Provider     string `json:"provider"`
	Operation    string `json:"operation"`
	Method       string `json:"method,omitempty"`
	ChannelID    int64  `json:"channel_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
}
type oauthFlowInput struct {
	command
	Provider string `json:"provider"`
	Flow     string `json:"flow"`
	Callback string `json:"callback,omitempty"`
}
type discoverInput struct {
	command
	UpstreamManagementInput
}
type oauthModelsInput struct {
	command
	credentialRef
	Apply bool `json:"apply,omitempty"`
}

func credentialIn(c store.Channel, id string) (store.CredentialItem, error) {
	for _, v := range c.Credentials {
		if v.ID == id {
			return v, nil
		}
	}
	return store.CredentialItem{}, store.ErrNotFound
}
func (t *managementTools) accountOperations() []management.Operation {
	ops := []management.Operation{
		typedOperation[credentialsInput]("update_credentials", "credential.patch", management.Manage, "原子添加或编辑同一渠道的明确凭证。指定新ID即新增；省略字段保持，空密钥不代表保留。source_channel_id仅复制API Key，MCP暂不开放OAuth重复关联。", false, func(ctx context.Context, p management.Principal, in credentialsInput) (any, error) {
			if len(in.Items) < 1 || len(in.Items) > 100 {
				return nil, invalid("items须为1..100项")
			}
			c, err := t.store.PatchChannelConfig(in.ChannelID, in.ExpectedVersion, func(c *store.Channel) error {
				seen := map[string]bool{}
				for _, item := range in.Items {
					if item.ID == "" || seen[item.ID] {
						return invalid("凭证ID须明确且不重复")
					}
					seen[item.ID] = true
					idx := slices.IndexFunc(c.Credentials, func(v store.CredentialItem) bool { return v.ID == item.ID })
					v := store.CredentialItem{ID: item.ID, Label: item.ID, Enabled: true}
					if idx >= 0 {
						v = c.Credentials[idx]
					}
					if item.Label != nil {
						v.Label = *item.Label
					}
					if item.Enabled != nil {
						v.Enabled = *item.Enabled
					}
					if item.APIKey != nil {
						if c.CredentialType != store.CredentialAPIKey {
							return invalid("OAuth凭证仅通过授权流程更新")
						}
						v.APIKey = *item.APIKey
					}
					if item.SourceChannelID != 0 {
						if c.CredentialType != store.CredentialAPIKey {
							return invalid("MCP暂不开放OAuth账号复制关联；请使用正常授权流程，现有凭证仍可管理。")
						}
						if item.APIKey != nil {
							return invalid("新密钥与来源关联不能同时指定")
						}
						src, e := t.store.Channel(item.SourceChannelID)
						if e != nil {
							return e
						}
						if src.CredentialType != c.CredentialType {
							return invalid("来源凭证接入方式必须一致")
						}
						from, e := credentialIn(src, item.SourceCredentialID)
						if e != nil {
							return e
						}
						if !sameCredentialDestinations(src, *c) {
							if e = p.Require(ctx, management.Sensitive); e != nil {
								return e
							}
						}
						v.APIKey = from.APIKey
						v.OAuth = from.OAuth
					}
					if idx < 0 {
						c.Credentials = append(c.Credentials, v)
					} else {
						c.Credentials[idx] = v
					}
				}
				if len(c.Credentials) > 0 {
					c.APIKey = c.Credentials[0].APIKey
					c.OAuth = c.Credentials[0].OAuth
				}
				return p.Require(ctx, management.Manage)
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"channel": safeManagedChannel(c), "version": store.EntityVersion(c), "atomic": true}, nil
		}),
		typedOperation[credentialDeleteInput]("delete_credential", "credential.delete", management.Maintain, "删除一个凭证；先预览再执行，空内置渠道可保留。", false, func(ctx context.Context, p management.Principal, in credentialDeleteInput) (any, error) {
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			v, err := credentialIn(c, in.CredentialID)
			if err != nil {
				return nil, err
			}
			if len(c.Credentials) < 2 && c.CredentialType == store.CredentialAPIKey {
				return nil, invalid("最后一个凭证请停用，或删除整个渠道。")
			}
			if store.EntityVersion(c) != in.ExpectedVersion {
				return nil, store.ErrEditConflict
			}
			ready, result, err := t.prepareMaintenance(ctx, p, "delete_credential", in.PreviewID, map[string]any{"target": in.credentialRef, "version": in.ExpectedVersion}, c, map[string]any{"channel": c.Name, "credential": v.Label})
			if err != nil || !ready {
				return result, err
			}
			updated, err := t.store.PatchChannelConfig(in.ChannelID, in.ExpectedVersion, func(c *store.Channel) error {
				c.Credentials = slices.DeleteFunc(c.Credentials, func(v store.CredentialItem) bool { return v.ID == in.CredentialID })
				c.APIKey, c.OAuth = "", nil
				if len(c.Credentials) > 0 {
					c.APIKey = c.Credentials[0].APIKey
					c.OAuth = c.Credentials[0].OAuth
				}
				return p.Require(ctx, management.Maintain)
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"channel": safeManagedChannel(updated), "version": store.EntityVersion(updated)}, nil
		}),
		typedOperation[credentialOrderInput]("reorder_credentials", "credential.reorder", management.Manage, "按完整凭证ID列表调整实际调用顺序。", false, func(ctx context.Context, p management.Principal, in credentialOrderInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			return map[string]any{"order": in.CredentialIDs}, t.store.SetCredentialOrder(in.ChannelID, in.CredentialIDs)
		}),
		typedOperation[credentialRef]("read_credential_key", "credential.secret", management.Sensitive, "明确读取一个API Key凭证的完整密钥；不支持读取OAuth令牌。", true, func(ctx context.Context, p management.Principal, in credentialRef) (any, error) {
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			v, err := credentialIn(c, in.CredentialID)
			if err != nil {
				return nil, err
			}
			if c.CredentialType != store.CredentialAPIKey {
				return nil, invalid("OAuth访问／刷新令牌不开放")
			}
			if err = p.Require(ctx, management.Sensitive); err != nil {
				return nil, err
			}
			return map[string]any{"channel_id": c.ID, "credential_id": v.ID, "api_key": v.APIKey}, nil
		}),
		typedOperation[credentialRef]("read_credential_identity", "credential.identity", management.Sensitive, "明确读取OAuth凭证的账号信息，不返回访问或刷新令牌。", true, func(ctx context.Context, p management.Principal, in credentialRef) (any, error) {
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			v, err := credentialIn(c, in.CredentialID)
			if err != nil {
				return nil, err
			}
			if v.OAuth == nil {
				return nil, invalid("此凭证没有OAuth账号信息")
			}
			if err = p.Require(ctx, management.Sensitive); err != nil {
				return nil, err
			}
			return v.OAuth.Public(), nil
		}),
		typedOperation[credentialRef]("get_credential_operations", "credential.operation.read", management.Read, "读取指定Codex凭证的自动配置、重置卡缓存和最近操作回执，不发送预热。", true, func(ctx context.Context, _ management.Principal, in credentialRef) (any, error) {
			operations, ctx, resolveErr := t.services.operationService(ctx, false, t.operations)
			if resolveErr != nil {
				return nil, resolveErr
			}
			v, err := operations.Report(in.ChannelID, in.CredentialID)
			return redactAccountResult(v), err
		}),
		typedOperation[automationInput]("configure_credential_automation", "credential.operation.configure", management.Manage, "更新指定凭证的自动预热或自动重置分支；保留其他配置。", false, func(ctx context.Context, p management.Principal, in automationInput) (any, error) {
			operations, ctx, resolveErr := t.services.operationService(ctx, false, t.operations)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			v, err := operations.PatchConfig(in.ChannelID, in.CredentialID, in.ExpectedIdentity, in.Patch)
			return redactAccountResult(v), err
		}),
	}
	for _, action := range []string{"refresh_reset_cards", "warmup_credential", "use_reset_card", "acknowledge_credential_operation"} {
		ops = append(ops, typedOperation[credentialActionInput](action, "credential.operation."+action, management.Manage, "明确目标的凭证操作。预热／用卡会消耗额度；未知结果先核对，不更换request_id重复发送。", false, func(ctx context.Context, p management.Principal, in credentialActionInput) (any, error) {
			operations, ctx, resolveErr := t.services.operationService(ctx, action != "acknowledge_credential_operation", t.operations)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			var v any
			var err error
			switch action {
			case "refresh_reset_cards":
				v, err = operations.RefreshCards(ctx, in.ChannelID, in.CredentialID, in.ExpectedIdentity)
			case "warmup_credential":
				v, err = operations.Warmup(ctx, in.ChannelID, in.CredentialID, in.ExpectedIdentity, in.RequestID)
			case "use_reset_card":
				v, err = operations.ConsumeReset(ctx, in.ChannelID, in.CredentialID, in.ExpectedIdentity, in.RequestID)
			case "acknowledge_credential_operation":
				v, err = operations.Acknowledge(in.ChannelID, in.CredentialID, in.ExpectedIdentity, in.Kind)
			}
			return credentialActionResult(v, action), err
		}))
	}
	ops = append(ops,
		typedOperation[oauthStartInput]("start_oauth", "oauth.start", management.Manage, "开始用户交互授权；返回链接或设备码。用户在提供商页面完成登录，不读取OAuth令牌。", false, func(ctx context.Context, p management.Principal, in oauthStartInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			if t.oauth == nil {
				return nil, invalid("OAuth服务未就绪")
			}
			result, err := t.oauth.ManagementAuthorize(t.flowContext(ctx, p), in.Provider, in.Operation, in.Method, in.ChannelID, in.CredentialID)
			if err != nil {
				return nil, err
			}
			if flow, ok := asObject(result)["flow"].(string); ok {
				t.flowOwners.Store(flow, p.ID)
			}
			return result, nil
		}),
		typedOperation[oauthFlowInput]("get_oauth_status", "oauth.status", management.Manage, "查询本连接发起的授权流程；授权完成时会关联账号。可提供用户复制的回调地址。", false, func(ctx context.Context, p management.Principal, in oauthFlowInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			v, err := t.oauth.ManagementStatusCallback(t.flowContext(ctx, p), in.Provider, in.Flow, in.Callback)
			return oauthFlowResult(v), err
		}),
		typedOperation[oauthFlowInput]("cancel_oauth", "oauth.cancel", management.Manage, "取消本连接发起且尚未完成的授权流程。", false, func(ctx context.Context, p management.Principal, in oauthFlowInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			return t.oauth.ManagementCancel(t.flowContext(ctx, p), in.Provider, in.Flow)
		}),
		typedOperation[discoverInput]("discover_upstream_models", "upstream.discover", management.Manage, "发现API Key渠道模型，可明确apply到目录。草稿必须本次提供新密钥，不能改投已存秘密。", false, func(ctx context.Context, p management.Principal, in discoverInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			result, err := t.upstream.Discover(ctx, in.UpstreamManagementInput)
			return upstreamActionResult(result), err
		}),
		typedOperation[discoverInput]("probe_upstream_model", "upstream.probe", management.Manage, "对明确模型、凭证、协议发送一次测试请求，可能消耗额度。结果按凭证/协议分别报告。", false, func(ctx context.Context, p management.Principal, in discoverInput) (any, error) {
			if in.Model == "" || len(in.Formats) == 0 {
				return nil, invalid("请明确model和formats")
			}
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			in.Confirmed = true
			result, err := t.upstream.Probe(ctx, in.UpstreamManagementInput)
			return upstreamActionResult(result), err
		}),
		typedOperation[oauthModelsInput]("discover_oauth_models", "upstream.oauth_discover", management.Manage, "读取指定OAuth凭证的模型目录；apply=true才保存到渠道目录。", false, func(ctx context.Context, p management.Principal, in oauthModelsInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			result, err := t.oauth.ManagementModels(ctx, in.ChannelID, in.CredentialID, in.Apply)
			return upstreamActionResult(result), err
		}),
	)
	for i := range ops {
		if strings.HasPrefix(ops[i].Capability, "upstream.") || strings.HasPrefix(ops[i].Capability, "oauth.") || strings.Contains(ops[i].Capability, "operation.refresh") || strings.Contains(ops[i].Capability, "operation.warmup") || strings.Contains(ops[i].Capability, "operation.use_") {
			ops[i].External = true
		}
		if ops[i].Permission == management.Maintain {
			ops[i].Destructive = true
		}
	}
	return ops
}
func (t *managementTools) flowContext(ctx context.Context, p management.Principal) context.Context {
	ctx = context.WithValue(ctx, oauthContinuationKey{}, func(current context.Context) error { return p.Require(current, management.Manage) })
	return WithOAuthFlowAccess(ctx, func(flow string) error {
		owner, ok := t.flowOwners.Load(flow)
		if !ok || owner != p.ID {
			return management.NewError("forbidden", fmt.Sprintf("授权流程不属于当前管理连接；请在原控制台或连接中继续（%s）。", flow))
		}
		return p.Require(ctx, management.Manage)
	})
}

func redactAccountResult(v any) any {
	if v == nil {
		return nil
	}
	data := asObject(v)
	var clean func(any)
	clean = func(value any) {
		switch obj := value.(type) {
		case map[string]any:
			for k, child := range obj {
				switch k {
				case "api_key", "access_token", "refresh_token", "github_token", "id_token", "account", "subject", "email", "github_login":
					delete(obj, k)
				default:
					clean(child)
				}
			}
		case []any:
			for _, child := range obj {
				clean(child)
			}
		}
	}
	clean(data)
	return data
}

func oauthFlowResult(v any) any {
	result := redactAccountResult(v)
	if obj, ok := result.(map[string]any); ok {
		obj["flow_status"] = obj["status"]
		if obj["status"] == "error" {
			obj["status"] = "failed"
		}
	}
	return result
}

func credentialActionResult(v any, action string) any {
	result := redactAccountResult(v)
	obj, ok := result.(map[string]any)
	if !ok {
		return result
	}
	key := ""
	if action == "warmup_credential" {
		key = "last_warmup"
	}
	if action == "use_reset_card" {
		key = "last_reset"
	}
	if key != "" {
		receipt, ok := obj[key].(map[string]any)
		if !ok {
			obj["status"] = "unknown"
		} else {
			switch receipt["state"] {
			case "succeeded":
				obj["status"] = "completed"
			case "failed":
				obj["status"] = "failed"
			default:
				obj["status"] = "unknown"
			}
		}
	}
	if action == "refresh_reset_cards" {
		if message, ok := obj["reset_cards_error"].(string); ok && message != "" {
			obj["status"] = "failed"
		}
	}
	return obj
}
func upstreamActionResult(v any) any {
	if v == nil {
		return nil
	}
	obj := asObject(v)
	if outcome, ok := v.(management.OperationOutcome); ok && outcome.OperationStatus() != "" {
		obj["status"] = outcome.OperationStatus()
		return obj
	}
	if ok, exists := obj["ok"].(bool); exists && !ok {
		obj["status"] = "failed"
		if rows, ok := obj["formats"].([]any); ok {
			for _, item := range rows {
				if row, ok := item.(map[string]any); ok && row["ok"] == true {
					obj["status"] = "partial"
					break
				}
			}
		}
	}
	return obj
}
