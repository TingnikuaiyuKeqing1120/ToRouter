package console

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"torouter/internal/gateway"
	"torouter/internal/management"
	"torouter/internal/quota"
	"torouter/internal/store"
)

type runtimePatch struct {
	MaxAttempts            *int                 `json:"max_attempts,omitempty"`
	RetriesPerChannel      *int                 `json:"retries_per_channel,omitempty"`
	ReasoningPolicy        *string              `json:"reasoning_policy,omitempty"`
	FailureCooldownSeconds *int                 `json:"failure_cooldown_seconds,omitempty"`
	RequestDefaultsEnabled *bool                `json:"request_defaults_enabled,omitempty"`
	RequestDefaults        *store.ModelDefaults `json:"request_defaults,omitempty"`
	OAuthAutoOpen          *bool                `json:"oauth_auto_open,omitempty"`
	QuotaRefreshSeconds    *int                 `json:"quota_refresh_seconds,omitempty"`
	DefaultLaunchMode      *string              `json:"default_launch_mode,omitempty"`
}
type runtimeSettingsInput struct {
	command
	ExpectedVersion string       `json:"expected_version"`
	Patch           runtimePatch `json:"patch"`
}
type retentionInput struct {
	maintenanceInput
	Days            int    `json:"days"`
	ExpectedVersion string `json:"expected_version"`
}
type portInput struct {
	maintenanceInput
	Port int `json:"port"`
}
type runtimeTargetInput struct {
	command
	statusTarget
}
type quotaSnapshotRefreshInput struct {
	command
	quotaRefreshInput
}
type openChannelInput struct {
	command
	store.QuotaTarget
}
type clientCreateInput struct {
	command
	Name  string                `json:"name"`
	Scope *store.ClientKeyScope `json:"scope,omitempty"`
}
type clientEditInput struct {
	command
	ID              int64                 `json:"id"`
	ExpectedVersion string                `json:"expected_version"`
	Name            *string               `json:"name,omitempty"`
	Scope           *store.ClientKeyScope `json:"scope,omitempty"`
}
type clientDeleteInput struct {
	maintenanceInput
	ID              int64  `json:"id"`
	ExpectedVersion string `json:"expected_version"`
}
type clientIDInput struct {
	ID int64 `json:"id"`
}
type scopeInput struct {
	Scope store.ClientKeyScope `json:"scope"`
}
type publicSpecInput struct {
	APIFormat string `json:"api_format,omitempty"`
}
type companionPatchInput struct {
	command
	Patch store.CompanionPreferencesPatch `json:"patch"`
}
type importInput struct {
	maintenanceInput
	Mode   string                  `json:"mode"`
	Models []store.DownstreamModel `json:"models"`
	Prices []store.Price           `json:"prices"`
}

func (t *managementTools) runtimeOperations() []management.Operation {
	ops := []management.Operation{
		typedOperation[emptyInput]("get_quota_snapshots", "quota.read", management.Read, "读取所有支持凭证的安全额度快照与刷新间隔；只读本机缓存，不访问供应商。", true, func(_ context.Context, _ management.Principal, _ emptyInput) (any, error) {
			return listQuotas(t.store, t.hooks.Quotas)
		}),
		typedOperation[quotaSnapshotRefreshInput]("refresh_quota_snapshot", "quota.refresh", management.Manage, "刷新精确凭证安全快照；force仅明确点击时使用，false遵循配置间隔与手动模式。", false, func(ctx context.Context, p management.Principal, in quotaSnapshotRefreshInput) (any, error) {
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			cred, err := credentialIn(c, in.CredentialID)
			if err != nil {
				return nil, err
			}
			if !quota.Supported(c, cred) {
				return nil, invalid("此凭证不支持额度查询")
			}
			if err = p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			v, observation := t.hooks.Quotas.RefreshSnapshot(ctx, c, cred, in.Force)
			if t.hooks.ObserveQuota != nil {
				t.hooks.ObserveQuota(c, cred, observation)
			}
			return v, nil
		}),
		typedOperation[openChannelInput]("open_channel_console", "companion.open_console", management.Manage, "打开精确渠道和凭证的本机控制台页面。", false, func(ctx context.Context, p management.Principal, in openChannelInput) (any, error) {
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			if _, err = credentialIn(c, in.CredentialID); err != nil {
				return nil, err
			}
			if t.hooks.OpenChannelConsole == nil {
				return nil, invalid("此宿主不支持打开渠道")
			}
			if err = p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			err = t.hooks.OpenChannelConsole(in.ChannelID, in.CredentialID)
			return map[string]bool{"requested": err == nil}, err
		}),
		typedOperation[emptyInput]("get_runtime_status", "runtime.status.read", management.Read, "查询渠道/凭证/模型运行状态，不主动调用模型。", true, func(_ context.Context, _ management.Principal, _ emptyInput) (any, error) {
			if t.hooks.RuntimeStatus == nil {
				return nil, invalid("此宿主不支持运行状态")
			}
			return redactAccountResult(t.hooks.RuntimeStatus()), nil
		}),
		typedOperation[runtimeTargetInput]("reset_runtime_status", "runtime.status.reset", management.Manage, "手动恢复明确渠道+凭证，可进一步限定模型；不改变配置启停。", false, func(ctx context.Context, p management.Principal, in runtimeTargetInput) (any, error) {
			if t.hooks.ResetStatus == nil {
				return nil, invalid("此宿主不支持恢复状态")
			}
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			if _, err = credentialIn(c, in.CredentialID); err != nil {
				return nil, err
			}
			if in.Model != "" && !modelName(c, in.Model) {
				return nil, store.ErrNotFound
			}
			if err = p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			err = t.hooks.ResetStatus(in.ChannelID, in.CredentialID, in.Model)
			return map[string]any{"restored": err == nil}, err
		}),
		typedOperation[credentialRef]("get_credential_quota", "quota.read", management.Read, "读取指定凭证现有额度缓存；未知不当0，不主动探测。", true, func(_ context.Context, _ management.Principal, in credentialRef) (any, error) {
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			cred, err := credentialIn(c, in.CredentialID)
			if err != nil {
				return nil, err
			}
			if !quota.Supported(c, cred) {
				return nil, invalid("此凭证不支持额度查询")
			}
			return t.hooks.Quotas.Snapshot(c, cred), nil
		}),
		typedOperation[runtimeTargetInput]("refresh_credential_quota", "quota.refresh", management.Manage, "明确刷新已适配凭证额度并更新网关观察状态；查询失败不当额度0。", false, func(ctx context.Context, p management.Principal, in runtimeTargetInput) (any, error) {
			if t.hooks.RefreshQuota == nil {
				return nil, invalid("此宿主不支持刷新额度")
			}
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			v, err := credentialIn(c, in.CredentialID)
			if err != nil {
				return nil, err
			}
			if in.Model != "" || !quota.Supported(c, v) {
				return nil, invalid("此凭证不支持额度查询")
			}
			if err = p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			err = t.hooks.RefreshQuota(ctx, c, v)
			if err != nil {
				return nil, err
			}
			return t.hooks.Quotas.Snapshot(c, v), nil
		}),
		typedOperation[publicSpecInput]("get_public_model_specifications", "model.spec.public", management.Read, "查询公开模型派生规格、绑定差异和价格；未知/不一致保持未知。", true, func(_ context.Context, _ management.Principal, in publicSpecInput) (any, error) {
			return (&gateway.Gateway{Store: t.store}).ReadModelSpecs(in.APIFormat)
		}),
		typedOperation[emptyInput]("get_settings", "settings.read", management.Read, "读取可管理业务设置和version；永不返回管理员主密钥。", true, func(_ context.Context, _ management.Principal, _ emptyInput) (any, error) {
			v, err := t.store.GetSettings()
			safe := store.SafeSettings(v)
			return map[string]any{"settings": safe, "version": store.EntityVersion(safe)}, err
		}),
		typedOperation[runtimeSettingsInput]("update_settings", "settings.patch", management.Manage, "只修改指定运行设置；管理员主密钥不可读写，日志保留策略另用维护工具。", false, func(ctx context.Context, p management.Principal, in runtimeSettingsInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			raw := rawArgument(ctx, "patch")
			v, err := t.store.PatchRuntimeSettings(in.ExpectedVersion, raw)
			if err != nil {
				return nil, err
			}
			safe := store.SafeSettings(v)
			return map[string]any{"settings": safe, "version": store.EntityVersion(safe)}, nil
		}),
		typedOperation[retentionInput]("configure_log_retention", "settings.retention", management.Maintain, "预览并保存日志保留天数，同时按现行规则清理过期数据。", false, func(ctx context.Context, p management.Principal, in retentionInput) (any, error) {
			if in.Days < 1 || in.Days > 3650 {
				return nil, invalid("days须为1..3650")
			}
			v, err := t.store.GetSettings()
			if err != nil {
				return nil, err
			}
			if in.ExpectedVersion != store.EntityVersion(store.SafeSettings(v)) {
				return nil, store.ErrEditConflict
			}
			args := map[string]any{"days": in.Days, "expected_version": in.ExpectedVersion}
			ready, result, err := t.prepareMaintenance(ctx, p, "configure_log_retention", in.PreviewID, args, store.SafeSettings(v), map[string]any{"retention_days": in.Days, "effect": "过期日志、管理工件与过期回执结果将按保留规则清理"})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(map[string]int{"retention_days": in.Days})
			if _, err = t.store.PatchRuntimeSettings(in.ExpectedVersion, raw); err != nil {
				return nil, err
			}
			err = t.store.Cleanup()
			return map[string]any{"retention_days": in.Days, "cleaned": err == nil}, err
		}),
		typedOperation[emptyInput]("get_runtime_connection", "service.connection", management.Read, "读取实际监听地址与下次启动端口，二者不同代表需要重启。", true, func(_ context.Context, _ management.Principal, _ emptyInput) (any, error) {
			port, err := t.store.Port()
			return map[string]any{"origin": t.origin, "configured_port": port, "mcp_endpoint": t.origin + "/mcp"}, err
		}),
		typedOperation[portInput]("set_runtime_port", "service.port", management.Maintain, "保存下次启动端口，先预览；不会自动重启或静默改客户端地址。", false, func(ctx context.Context, p management.Principal, in portInput) (any, error) {
			if in.Port < 1 || in.Port > 65535 {
				return nil, invalid("port须为1..65535")
			}
			old, err := t.store.Port()
			if err != nil {
				return nil, err
			}
			ready, result, err := t.prepareMaintenance(ctx, p, "set_runtime_port", in.PreviewID, in.Port, old, map[string]any{"before": old, "after": in.Port, "restart_required": true})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			err = t.store.SavePortIfCurrent(in.Port, &old)
			return map[string]any{"configured_port": in.Port, "restart_required": true}, err
		}),
		typedOperation[clientCreateInput]("create_client_key", "client_access.create", management.Manage, "为模型调用创建独立Key；默认结果仅身份，完整Key需敏感读取或到控制台领取。", false, func(ctx context.Context, p management.Principal, in clientCreateInput) (any, error) {
			scope := store.DefaultClientKeyScope()
			if in.Scope != nil {
				scope = *in.Scope
			}
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			key, _, err := t.store.CreateClientKeyWithScope(in.Name, scope)
			return map[string]any{"client_key": key, "next": "在控制台领取，或使用read_client_key（需敏感权限）"}, err
		}),
		typedOperation[clientEditInput]("update_client_key", "client_access.patch", management.Manage, "按版本修改客户端Key名称/访问范围；省略保持，不自动放宽权限。", false, func(ctx context.Context, p management.Principal, in clientEditInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			return t.store.PatchClientKey(in.ID, in.ExpectedVersion, in.Name, in.Scope)
		}),
		typedOperation[scopeInput]("preview_client_scope", "client_access.preview", management.Read, "预览公开模型许可与上游标签组合的实际匹配结果。", true, func(_ context.Context, _ management.Principal, in scopeInput) (any, error) {
			return evaluateClientScope(t.store, in.Scope)
		}),
		typedOperation[clientIDInput]("read_client_key", "client_access.secret", management.Sensitive, "明确读取指定客户端调用Key的完整密钥；它不能调用管理MCP。", true, func(ctx context.Context, p management.Principal, in clientIDInput) (any, error) {
			if err := p.Require(ctx, management.Sensitive); err != nil {
				return nil, err
			}
			key, err := t.store.RevealClientKey(in.ID)
			return map[string]any{"id": in.ID, "key": key}, err
		}),
		typedOperation[clientDeleteInput]("revoke_client_key", "client_access.delete", management.Maintain, "预览并撤销客户端模型调用Key，不能以空列表撤销全部。", false, func(ctx context.Context, p management.Principal, in clientDeleteInput) (any, error) {
			rows, err := t.store.ListClientKeys()
			if err != nil {
				return nil, err
			}
			for _, key := range rows {
				if key.ID == in.ID {
					if store.ClientKeyVersion(key) != in.ExpectedVersion {
						return nil, store.ErrEditConflict
					}
					ready, result, err := t.prepareMaintenance(ctx, p, "revoke_client_key", in.PreviewID, in.ID, store.ClientKeyVersion(key), map[string]any{"id": key.ID, "name": key.Name})
					if err != nil || !ready {
						return result, err
					}
					if err = p.Require(ctx, management.Maintain); err != nil {
						return nil, err
					}
					err = t.store.DeleteClientKeyAtVersion(in.ID, in.ExpectedVersion)
					return map[string]any{"revoked": in.ID}, err
				}
			}
			return nil, store.ErrNotFound
		}),
		typedOperation[emptyInput]("export_model_backup", "models.backup.export", management.Read, "导出公开模型与关联价格（非整库备份）；返回有期限资源。", true, func(ctx context.Context, p management.Principal, _ emptyInput) (any, error) {
			models, err := t.store.ListDownstreamModels()
			if err != nil {
				return nil, err
			}
			prices, err := t.store.GetPrices()
			if err != nil {
				return nil, err
			}
			selected := []store.Price{}
			for _, price := range prices {
				if price.ChannelID == 0 {
					for _, m := range models {
						if m.ID == price.Model {
							selected = append(selected, price)
							break
						}
					}
				}
			}
			raw, _ := json.MarshalIndent(map[string]any{"version": 2, "models": models, "prices": selected}, "", "  ")
			return t.server.PutArtifact(ctx, p, "model-backup.json", "application/json", raw)
		}),
		typedOperation[importInput]("import_model_backup", "models.backup.import", management.Maintain, "预览并导入模型及关联价格，mode为merge/overwrite；不包含渠道、密钥或整库。", false, func(ctx context.Context, p management.Principal, in importInput) (any, error) {
			if in.Mode != "merge" && in.Mode != "overwrite" {
				return nil, invalid("mode须为merge或overwrite")
			}
			models, err := t.store.ListDownstreamModels()
			if err != nil {
				return nil, err
			}
			prices, err := t.store.GetPrices()
			if err != nil {
				return nil, err
			}
			snapshot := map[string]any{"models": models, "prices": prices}
			args := map[string]any{"mode": in.Mode, "models": in.Models, "prices": in.Prices}
			ready, result, err := t.prepareMaintenance(ctx, p, "import_model_backup", in.PreviewID, args, snapshot, map[string]any{"mode": in.Mode, "models": len(in.Models), "prices": len(in.Prices)})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			err = t.store.ImportDownstreamModelsWithPricesChecked(in.Models, in.Prices, in.Mode == "overwrite", store.EntityVersion(snapshot))
			return map[string]any{"imported": len(in.Models)}, err
		}),
		typedOperation[emptyInput]("get_companion", "companion.read", management.Read, "读取桌宠状态、偏好及今日用量，能力由当前宿主决定。", true, func(ctx context.Context, _ management.Principal, _ emptyInput) (any, error) {
			pref, err := t.store.CompanionPreferences()
			if err != nil {
				return nil, err
			}
			out := map[string]any{"preferences": pref, "can_open": t.hooks.OpenCompanion != nil}
			if t.hooks.CompanionRunning != nil {
				out["running"] = t.hooks.CompanionRunning()
			}
			if t.hooks.Companion != nil {
				snapshot := t.hooks.Companion()
				out["snapshot"] = snapshot
				now := snapshot.ServerTime
				day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
				out["today"], err = t.store.UsageTotalsBetween(ctx, day, day.AddDate(0, 0, 1))
			}
			return out, err
		}),
		typedOperation[companionPatchInput]("configure_companion", "companion.preferences", management.Manage, "增量修改桌宠主题、动画暂停或精确额度关注列表。", false, func(ctx context.Context, p management.Principal, in companionPatchInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			if err := validateQuotaTargets(t.store, in.Patch.QuotaTargets); err != nil {
				return nil, invalid(err.Error())
			}
			return t.store.PatchCompanionPreferences(in.Patch)
		}),
	}
	for _, action := range []string{"restart_gateway", "stop_gateway"} {
		ops = append(ops, typedOperation[maintenanceInput](action, "service."+action, management.Maintain, "明确预览后重启或停止网关；回复只表示已接受，后续需重新连接确认运行状态。", false, func(ctx context.Context, p management.Principal, in maintenanceInput) (any, error) {
			fn := t.hooks.Restart
			if action == "stop_gateway" {
				fn = t.hooks.Shutdown
			}
			if fn == nil {
				return nil, invalid("此宿主不支持服务控制")
			}
			port, err := t.store.Port()
			if err != nil {
				return nil, err
			}
			ready, result, err := t.prepareMaintenance(ctx, p, action, in.PreviewID, action, port, map[string]any{"action": action, "configured_port": port, "disconnects": true})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			err = management.AfterResponse(ctx, fn)
			return map[string]any{"accepted": err == nil, "action": action, "next": "重新连接实际地址检查完成状态，不盲目重发。"}, err
		}))
	}
	for _, action := range []string{"open_companion", "close_companion", "open_console"} {
		ops = append(ops, typedOperation[command](action, "companion."+action, management.Manage, "操作当前宿主的桌面窗口；不支持GUI时明确拒绝。", false, func(ctx context.Context, p management.Principal, _ command) (any, error) {
			fn := t.hooks.OpenCompanion
			if action == "close_companion" {
				fn = t.hooks.CloseCompanion
			}
			if action == "open_console" {
				fn = t.hooks.OpenConsole
			}
			if fn == nil {
				return nil, invalid("此宿主不支持桌面窗口")
			}
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			err := fn()
			return map[string]any{"requested": err == nil}, err
		}))
	}
	for i := range ops {
		if ops[i].Permission == management.Maintain {
			ops[i].Destructive = true
		}
		if strings.Contains(ops[i].Name, "refresh_") {
			ops[i].External = true
		}
	}
	return ops
}
