package console

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"torouter/internal/management"
	"torouter/internal/store"
)

type channelDefinition struct {
	CredentialType string           `json:"credential_type,omitempty"`
	Name           string           `json:"name"`
	BaseURL        string           `json:"base_url"`
	Endpoints      []store.Endpoint `json:"endpoints"`
	APIKey         string           `json:"api_key,omitempty"`
	Models         []string         `json:"models,omitempty"`
	Tags           []string         `json:"tags,omitempty"`
	Enabled        *bool            `json:"enabled,omitempty"`
}
type createChannelInput struct {
	command
	Channel channelDefinition `json:"channel"`
}
type channelPatch struct {
	Name              *string              `json:"name,omitempty"`
	BaseURL           *string              `json:"base_url,omitempty"`
	Endpoints         *[]store.Endpoint    `json:"endpoints,omitempty"`
	Enabled           *bool                `json:"enabled,omitempty"`
	Models            *[]string            `json:"models,omitempty"`
	AvailableModels   *[]string            `json:"available_models,omitempty"`
	ManualModels      *[]string            `json:"manual_models,omitempty"`
	Tags              *[]string            `json:"tags,omitempty"`
	ModelTags         *map[string][]string `json:"model_tags,omitempty"`
	DispatchMode      *string              `json:"dispatch_mode,omitempty"`
	ChainType         *string              `json:"chain_type,omitempty"`
	PreferredEndpoint *string              `json:"preferred_endpoint,omitempty"`
}
type updateChannelInput struct {
	command
	ChannelID       int64        `json:"channel_id"`
	ExpectedVersion string       `json:"expected_version"`
	Patch           channelPatch `json:"patch"`
}
type deleteChannelInput struct {
	maintenanceInput
	ChannelID       int64  `json:"channel_id"`
	ExpectedVersion string `json:"expected_version"`
}
type displayInput struct {
	command
	Patch store.ChannelDisplayPatch `json:"patch"`
}
type modelCreateInput struct {
	command
	Model store.DownstreamModel `json:"model"`
}
type publicModelPatch struct {
	CatalogModelRef *store.CatalogModelRef `json:"catalog_model_ref,omitempty"`
	DisplayName     *string                `json:"display_name,omitempty"`
	Enabled         *bool                  `json:"enabled,omitempty"`
	Fallback        *bool                  `json:"fallback,omitempty"`
	Bindings        *[]store.Binding       `json:"bindings,omitempty"`
	Tags            *[]string              `json:"tags,omitempty"`
	SortOrder       *int                   `json:"sort_order,omitempty"`
}
type publicModelEdit struct {
	ID              string           `json:"id"`
	ExpectedVersion string           `json:"expected_version"`
	Patch           publicModelPatch `json:"patch"`
}
type publicModelsInput struct {
	command
	Items []publicModelEdit `json:"items"`
}
type renameModelInput struct {
	command
	ID              string `json:"id"`
	NewID           string `json:"new_id"`
	ExpectedVersion string `json:"expected_version"`
}
type modelDeleteInput struct {
	maintenanceInput
	ExpectedVersions map[string]string `json:"expected_versions"`
}
type orderInput struct {
	command
	Order []string `json:"order"`
}
type specInput struct {
	command
	ChannelID       int64           `json:"channel_id"`
	Model           string          `json:"model"`
	ExpectedVersion string          `json:"expected_version"`
	Patch           map[string]any  `json:"patch"`
	CatalogModelRef json.RawMessage `json:"catalog_model_ref,omitempty"`
}
type tagPatch struct {
	Name        *string `json:"name,omitempty"`
	GroupID     *string `json:"group_id,omitempty"`
	Color       *string `json:"color,omitempty"`
	Icon        *string `json:"icon,omitempty"`
	Description *string `json:"description,omitempty"`
	SortOrder   *int    `json:"sort_order,omitempty"`
}
type tagInput struct {
	command
	ID              string   `json:"id"`
	ExpectedVersion string   `json:"expected_version"`
	Patch           tagPatch `json:"patch"`
}
type tagDeleteInput struct {
	maintenanceInput
	ID    string `json:"id"`
	Force bool   `json:"force,omitempty"`
}
type tagOrderInput struct {
	command
	GroupID string   `json:"group_id"`
	Order   []string `json:"order"`
}
type tagReferenceInput struct {
	ID string `json:"id"`
}

func destinationSet(c store.Channel) (map[string]bool, error) {
	out := map[string]bool{}
	for _, ep := range c.Endpoints {
		u, err := url.Parse(c.EndpointBaseURL(ep))
		if err != nil || u.Hostname() == "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" {
			return nil, invalid("请填写有效HTTP(S)目标地址，不能包含用户信息。")
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		out[strings.ToLower(u.Scheme+"://"+u.Hostname()+":"+port)] = true
	}
	return out, nil
}
func sameCredentialDestinations(old, next store.Channel) bool {
	a, err := destinationSet(old)
	if err != nil {
		return false
	}
	b, err := destinationSet(next)
	if err != nil {
		return false
	}
	for dst := range b {
		if !a[dst] {
			return false
		}
	}
	return true
}
func checkChannelDestination(ctx context.Context, p management.Principal, old, next store.Channel) error {
	if sameCredentialDestinations(old, next) {
		return nil
	}
	for _, cred := range next.Credentials {
		if cred.APIKey != "" || cred.OAuth != nil {
			return p.Require(ctx, management.Sensitive)
		}
	}
	if next.APIKey != "" || next.OAuth != nil {
		return p.Require(ctx, management.Sensitive)
	}
	return nil
}

func (t *managementTools) configOperations() []management.Operation {
	ops := []management.Operation{
		typedOperation[createChannelInput]("create_channel", "channel.create", management.Manage, "创建API Key或空内置账号渠道；凭证通过独立授权添加。明确的新凭证仅用于本次配置目标。", false, func(ctx context.Context, p management.Principal, in createChannelInput) (any, error) {
			v := in.Channel
			c := store.Channel{Name: v.Name, BaseURL: v.BaseURL, Endpoints: v.Endpoints, APIKey: v.APIKey, Models: v.Models, AvailableModels: v.Models, ManualModels: v.Models, Tags: v.Tags, Enabled: true, CredentialType: store.CredentialAPIKey}
			if v.CredentialType != "" && v.CredentialType != store.CredentialAPIKey {
				var err error
				c, err = store.BuiltinChannel(v.Name, v.CredentialType)
				if err != nil {
					return nil, err
				}
			}
			if v.Enabled != nil {
				c.Enabled = *v.Enabled
			}
			if _, err := destinationSet(c); err != nil {
				return nil, err
			}
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			if err := t.store.SaveChannelConfig(&c); err != nil {
				return nil, err
			}
			return map[string]any{"channel": safeManagedChannel(c), "version": store.EntityVersion(c)}, nil
		}),
		typedOperation[updateChannelInput]("update_channel", "channel.patch", management.Manage, "按读取版本修改渠道明确字段；凭证与规格有独立工具。旧秘密改投新地址需要敏感授权。", false, func(ctx context.Context, p management.Principal, in updateChannelInput) (any, error) {
			updated, err := t.store.PatchChannelConfig(in.ChannelID, in.ExpectedVersion, func(c *store.Channel) error {
				old := *c
				raw := rawArgument(ctx, "patch")
				if err := store.MergeFields(c, raw, "name", "base_url", "endpoints", "enabled", "models", "available_models", "manual_models", "tags", "model_tags", "dispatch_mode", "chain_type", "preferred_endpoint"); err != nil {
					return err
				}
				if err := checkChannelDestination(ctx, p, old, *c); err != nil {
					return err
				}
				return p.Require(ctx, management.Manage)
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"channel": safeManagedChannel(updated), "version": store.EntityVersion(updated)}, nil
		}),
		typedOperation[deleteChannelInput]("delete_channel", "channel.delete", management.Maintain, "删除一个渠道。首次返回具体预览；带preview_id以相同目标执行，历史用量保留。", false, func(ctx context.Context, p management.Principal, in deleteChannelInput) (any, error) {
			c, err := t.store.Channel(in.ChannelID)
			if err != nil {
				return nil, err
			}
			if in.ExpectedVersion == "" || store.EntityVersion(c) != in.ExpectedVersion {
				return nil, store.ErrEditConflict
			}
			args := map[string]any{"channel_id": in.ChannelID, "expected_version": in.ExpectedVersion}
			ready, result, err := t.prepareMaintenance(ctx, p, "delete_channel", in.PreviewID, args, c, map[string]any{"channel_id": c.ID, "name": c.Name, "effect": "删除渠道与凭证；保留历史用量及已支付套餐记录"})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			warning, err := t.disconnectManaged(ctx, c, in.ExpectedVersion)
			return map[string]any{"deleted": err == nil, "channel_id": c.ID, "warning": warning}, err
		}),
		typedOperation[emptyInput]("get_channel_display", "channel.display.read", management.Read, "读取渠道图标与显示排序；不代表调用顺序。", true, func(_ context.Context, _ management.Principal, _ emptyInput) (any, error) {
			return t.store.GetChannelDisplay()
		}),
		typedOperation[displayInput]("update_channel_display", "channel.display.patch", management.Manage, "修改显示排序和图标。图标只接受既有内置标识或受限图片内容。", false, func(ctx context.Context, p management.Principal, in displayInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			return t.store.PatchChannelDisplay(in.Patch)
		}),
		typedOperation[modelCreateInput]("create_public_model", "public_model.create", management.Manage, "创建公开模型与明确上游绑定；同ID已存在会拒绝。", false, func(ctx context.Context, p management.Principal, in modelCreateInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			if err := t.store.CreateDownstreamModel(in.Model); err != nil {
				return nil, err
			}
			return map[string]any{"id": in.Model.ID}, nil
		}),
		typedOperation[publicModelsInput]("update_public_models", "public_model.patch", management.Manage, "按版本原子修改1..100个公开模型；绑定数组明确替换，渠道显示顺序不影响此处路由。", false, func(ctx context.Context, p management.Principal, in publicModelsInput) (any, error) {
			edits := []store.ModelEdit{}
			for i, item := range in.Items {
				raw := rawArgument(ctx, "items", strconv.Itoa(i), "patch")
				edits = append(edits, store.ModelEdit{ID: item.ID, ExpectedVersion: item.ExpectedVersion, Patch: raw})
			}
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			rows, err := t.store.PatchPublicModels(edits)
			return map[string]any{"models": rows, "atomic": true}, err
		}),
		typedOperation[renameModelInput]("rename_public_model", "public_model.rename", management.Manage, "重命名公开ID并原子更新引用；历史日志保持原归属。", false, func(ctx context.Context, p management.Principal, in renameModelInput) (any, error) {
			rows, err := t.store.ListDownstreamModels()
			if err != nil {
				return nil, err
			}
			for _, m := range rows {
				if m.ID == in.ID {
					if in.ExpectedVersion == "" {
						return nil, store.ErrEditConflict
					}
					m.ID = in.NewID
					if err = p.Require(ctx, management.Manage); err != nil {
						return nil, err
					}
					err = t.store.RenameDownstreamModelAtVersion(in.ID, m, in.ExpectedVersion)
					return map[string]any{"id": in.NewID}, err
				}
			}
			return nil, store.ErrNotFound
		}),
		typedOperation[modelDeleteInput]("delete_public_models", "public_model.delete", management.Maintain, "明确ID与版本的原子批量删除；先预览引用变化，再执行。", false, func(ctx context.Context, p management.Principal, in modelDeleteInput) (any, error) {
			rows, err := t.store.ListDownstreamModels()
			if err != nil {
				return nil, err
			}
			selected := []store.DownstreamModel{}
			for _, m := range rows {
				if expected, ok := in.ExpectedVersions[m.ID]; ok {
					if expected == "" || expected != store.EntityVersion(m) {
						return nil, store.ErrEditConflict
					}
					selected = append(selected, m)
				}
			}
			if len(selected) == 0 || len(selected) != len(in.ExpectedVersions) {
				return nil, store.ErrNotFound
			}
			ready, result, err := t.prepareMaintenance(ctx, p, "delete_public_models", in.PreviewID, in.ExpectedVersions, selected, map[string]any{"models": selected, "count": len(selected)})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			err = t.store.DeletePublicModelsAtVersions(in.ExpectedVersions)
			return map[string]any{"deleted": len(selected), "atomic": true}, err
		}),
		typedOperation[orderInput]("reorder_public_models", "public_model.reorder", management.Manage, "按完整ID集合调整公开模型显示顺序。", false, func(ctx context.Context, p management.Principal, in orderInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			return map[string]any{"order": in.Order}, t.store.ReorderDownstreamModels(in.Order)
		}),
		typedOperation[specInput]("update_model_specification", "model.spec.patch", management.Manage, "按渠道版本修改一个上游模型的规格；只改指定字段，null可将可空规格设为未知。", false, func(ctx context.Context, p management.Principal, in specInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			raw := rawArgument(ctx, "patch")
			return t.store.PatchModelSpecification(in.ChannelID, in.Model, in.ExpectedVersion, raw, in.CatalogModelRef)
		}),
	}
	for _, group := range []bool{false, true} {
		name, capability := "update_tag", "tag.patch"
		if group {
			name, capability = "update_tag_group", "tag.group.patch"
		}
		ops = append(ops, typedOperation[tagInput](name, capability, management.Manage, "按版本修改标签或标签组；新建用expected_version=absent。标签组类型不可变，未知标签受保护。", false, func(ctx context.Context, p management.Principal, in tagInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			raw := rawArgument(ctx, "patch")
			return t.store.PatchTagRecord(in.ID, in.ExpectedVersion, raw, group)
		}))
	}
	ops = append(ops,
		typedOperation[tagReferenceInput]("get_tag_references", "tag.references", management.Read, "查看标签被哪些渠道、模型、调用Key引用。", true, func(_ context.Context, _ management.Principal, in tagReferenceInput) (any, error) {
			return t.store.GetTagReferences(in.ID)
		}),
		typedOperation[orderInput]("reorder_tag_groups", "tag.group.reorder", management.Manage, "按完整组ID集合排序，不改变标签身份与引用。", false, func(ctx context.Context, p management.Principal, in orderInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			return map[string]any{"order": in.Order}, t.store.ReorderTagGroups(in.Order)
		}),
		typedOperation[tagOrderInput]("reorder_tags", "tag.reorder", management.Manage, "按指定组的完整标签ID集合排序。", false, func(ctx context.Context, p management.Principal, in tagOrderInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			return map[string]any{"order": in.Order}, t.store.ReorderTags(in.GroupID, in.Order)
		}),
	)
	for _, group := range []bool{false, true} {
		name, capability := "delete_tag", "tag.delete"
		if group {
			name, capability = "delete_tag_group", "tag.group.delete"
		}
		ops = append(ops, typedOperation[tagDeleteInput](name, capability, management.Maintain, "删除标签或标签组；先预览。引用中的客户端权限不能自动放宽。", false, func(ctx context.Context, p management.Principal, in tagDeleteInput) (any, error) {
			kind := "tag"
			if group {
				kind = "tag_group"
			}
			rows, err := t.entities(kind)
			if err != nil {
				return nil, err
			}
			var found *managedEntity
			for _, row := range rows {
				if row.Ref.ID == in.ID {
					v := row
					found = &v
				}
			}
			if found == nil {
				return nil, store.ErrNotFound
			}
			state, err := t.store.TagDeletionState(in.ID, group)
			if err != nil {
				return nil, err
			}
			args := map[string]any{"id": in.ID, "force": in.Force}
			ready, result, err := t.prepareMaintenance(ctx, p, name, in.PreviewID, args, state.Version, state)
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			if group {
				err = t.store.DeleteTagGroupAtVersion(in.ID, state.Version)
			} else {
				err = t.store.DeleteTagAtVersion(in.ID, in.Force, state.Version)
			}
			return map[string]any{"deleted": in.ID}, err
		}))
	}
	for i := range ops {
		if ops[i].Permission == management.Maintain {
			ops[i].Destructive = true
		}
	}
	return ops
}

func (t *managementTools) disconnectManaged(ctx context.Context, c store.Channel, version string) (string, error) {
	if t.services.plugins != nil {
		removed, _, notice, err := disconnectPluginChannel(ctx, t.store, t.services.plugins, c, version)
		if removed || err != nil {
			return notice, err
		}
	}
	switch c.CredentialType {
	case store.CredentialCodexOAuth:
		if t.services.codex != nil {
			return "", t.services.codex.DisconnectChannelAtVersion(c.ID, version)
		}
	case store.CredentialChatGPTOAuth:
		if t.services.siwc != nil {
			return t.services.siwc.DisconnectChannelAtVersion(ctx, c.ID, version)
		}
	}
	return "", t.store.DeleteChannelAtVersion(c.ID, version)
}

// Keep the field whitelist visible in tool schemas rather than exposing a free
// form patch. The store still validates it independently at its write boundary.
func restrictPatchSchema(op *management.Operation, field string, typ map[string]any) {
	if props, ok := op.InputSchema["properties"].(map[string]any); ok {
		props[field] = typ
	}
}

// Exact integers are represented as IDs, not inferred from display names.
func numericID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, invalid("请使用检索返回的精确ID")
	}
	return id, nil
}
func modelName(c store.Channel, name string) bool {
	return slices.Contains(c.Models, name) || slices.Contains(c.AvailableModels, name)
}
