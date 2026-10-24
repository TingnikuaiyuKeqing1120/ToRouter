package console

import (
	"context"
	"strings"

	"torouter/internal/catalog"
	"torouter/internal/management"
	"torouter/internal/store"
)

type upstreamDeleteInput struct {
	maintenanceInput
	ChannelID       int64  `json:"channel_id"`
	Model           string `json:"model"`
	ExpectedVersion string `json:"expected_version"`
}
type managementRevokeInput struct {
	maintenanceInput
	ID              int64  `json:"id"`
	ExpectedVersion string `json:"expected_version"`
}
type auditInput struct {
	BeforeID int64 `json:"before_id,omitempty"`
	Limit    int   `json:"limit,omitempty"`
}
type presetInput struct {
	Query string `json:"query,omitempty"`
}

func (t *managementTools) maintenanceOperations() []management.Operation {
	return []management.Operation{
		typedOperation[upstreamDeleteInput]("delete_upstream_model", "channel.model.delete", management.Maintain, "从渠道删除指定上游模型及其规格/标签/目录记录，先预览再执行。", false, func(ctx context.Context, p management.Principal, in upstreamDeleteInput) (any, error) {
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			if !modelName(c, in.Model) {
				return nil, store.ErrNotFound
			}
			if in.ExpectedVersion != store.EntityVersion(c) {
				return nil, store.ErrEditConflict
			}
			args := map[string]any{"channel_id": in.ChannelID, "model": in.Model, "expected_version": in.ExpectedVersion}
			ready, result, err := t.prepareMaintenance(ctx, p, "delete_upstream_model", in.PreviewID, args, c, map[string]any{"channel": c.Name, "model": in.Model, "effect": "移除启用/目录条目和该上游规格标签；公开绑定保留失效提示"})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			updated, err := t.store.DeleteModelAtVersion(in.ChannelID, in.Model, in.ExpectedVersion)
			return map[string]any{"channel": safeManagedChannel(updated), "version": store.EntityVersion(updated)}, err
		}),
		typedOperation[maintenanceInput]("reset_builtin_tags", "tag.reset_builtin", management.Maintain, "预览并恢复内置标签定义；自定义标签不删除。", false, func(ctx context.Context, p management.Principal, in maintenanceInput) (any, error) {
			tags, err := t.store.ListTags()
			if err != nil {
				return nil, err
			}
			groups, err := t.store.ListTagGroups()
			if err != nil {
				return nil, err
			}
			snapshot := map[string]any{"tags": tags, "groups": groups}
			ready, result, err := t.prepareMaintenance(ctx, p, "reset_builtin_tags", in.PreviewID, "reset_builtin", snapshot, map[string]any{"restored_tags": store.DefaultTags, "restored_groups": store.DefaultTagGroups})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			err = t.store.ResetBuiltinTagsAtVersion(store.EntityVersion(snapshot))
			return map[string]any{"restored": err == nil}, err
		}),
		typedOperation[emptyInput]("get_management_connections", "management_access.read", management.Read, "查询MCP管理接入名称、权限、启停及最近使用，不返回管理密钥。授权签发和提权只在控制台完成。", true, func(_ context.Context, p management.Principal, _ emptyInput) (any, error) {
			rows, err := t.store.ListManagementConnections()
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, row := range rows {
				value := asObject(row)
				value["version"] = store.ManagementConnectionVersion(row)
				value["current"] = row.ID == p.ID
				out = append(out, value)
			}
			return map[string]any{"connections": out, "endpoint": t.origin + "/mcp", "console_url": t.origin + "/#/mcp"}, nil
		}),
		typedOperation[managementRevokeInput]("revoke_management_connection", "management_access.revoke", management.Maintain, "预览后撤销明确的MCP接入；不签发、不提权。当前连接会在回执写出后失效。", false, func(ctx context.Context, p management.Principal, in managementRevokeInput) (any, error) {
			rows, err := t.store.ListManagementConnections()
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				if row.ID == in.ID {
					if store.ManagementConnectionVersion(row) != in.ExpectedVersion {
						return nil, store.ErrEditConflict
					}
					args := map[string]any{"id": in.ID, "expected_version": in.ExpectedVersion}
					ready, result, err := t.prepareMaintenance(ctx, p, "revoke_management_connection", in.PreviewID, args, store.ManagementConnectionVersion(row), map[string]any{"id": row.ID, "name": row.Name, "current": row.ID == p.ID})
					if err != nil || !ready {
						return result, err
					}
					if err = p.Require(ctx, management.Maintain); err != nil {
						return nil, err
					}
					if row.ID == p.ID {
						err = management.AfterResponse(ctx, func() { _ = t.store.RevokeManagementConnection(in.ID, in.ExpectedVersion) })
						return map[string]any{"accepted": err == nil, "connection_will_close": true}, err
					}
					err = t.store.RevokeManagementConnection(in.ID, in.ExpectedVersion)
					return map[string]any{"revoked": err == nil}, err
				}
			}
			return nil, store.ErrNotFound
		}),
		typedOperation[auditInput]("get_management_audit", "management_access.audit", management.Read, "分页查看当前连接的敏感读取审计；只含目标标识，不含秘密值。", true, func(ctx context.Context, p management.Principal, in auditInput) (any, error) {
			return t.server.Audit(ctx, p, in.BeforeID, in.Limit)
		}),
		typedOperation[presetInput]("get_channel_presets", "channel.preset", management.Read, "查询添加渠道页同源供应商模板、默认端点与提示，可用名称或ID检索。", true, func(_ context.Context, _ management.Principal, in presetInput) (any, error) {
			out := []catalog.ChannelPreset{}
			for _, v := range catalog.ChannelPresets() {
				if in.Query == "" || strings.Contains(strings.ToLower(v.ID+v.Name+v.Label), strings.ToLower(in.Query)) {
					out = append(out, v)
				}
			}
			return map[string]any{"presets": out}, nil
		}),
	}
}
