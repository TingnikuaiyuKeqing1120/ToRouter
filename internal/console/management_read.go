package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"torouter/internal/gateway"
	"torouter/internal/management"
	"torouter/internal/store"
)

type entityRef struct {
	Kind         string `json:"kind"`
	ID           string `json:"id,omitempty"`
	ChannelID    int64  `json:"channel_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	Model        string `json:"model,omitempty"`
	Source       string `json:"source,omitempty"`
}
type managedEntity struct {
	Ref     entityRef `json:"ref"`
	Name    string    `json:"name"`
	Enabled *bool     `json:"enabled,omitempty"`
	Tags    []string  `json:"tags,omitempty"`
	Version string    `json:"version"`
	Data    any       `json:"data,omitempty"`
}
type searchInput struct {
	Kind            string              `json:"kind,omitempty"`
	Query           string              `json:"query,omitempty"`
	ChannelID       int64               `json:"channel_id,omitempty"`
	Source          string              `json:"source,omitempty"`
	IncludeDisabled bool                `json:"include_disabled,omitempty"`
	TagGroups       map[string][]string `json:"tag_groups,omitempty"`
	Limit           int                 `json:"limit,omitempty"`
	Cursor          string              `json:"cursor,omitempty"`
}
type getInput struct {
	Refs   []entityRef `json:"refs"`
	Fields []string    `json:"fields,omitempty"`
}
type searchCursor struct {
	Offset   int    `json:"offset"`
	Snapshot string `json:"snapshot"`
	Filter   string `json:"filter"`
}

func refKey(ref entityRef) string { b, _ := json.Marshal(ref); return string(b) }
func boolValue(v bool) *bool      { return &v }
func flattenModelTags(groups map[string][]string) []string {
	out := []string{}
	for _, ids := range groups {
		out = append(out, ids...)
	}
	sort.Strings(out)
	return out
}

func safeManagedChannel(c store.Channel) map[string]any {
	out := asObject(publicChannel(c))
	delete(out, "oauth")
	if creds, ok := out["credentials"].([]any); ok {
		for _, v := range creds {
			if cred, ok := v.(map[string]any); ok {
				delete(cred, "oauth_info")
				delete(cred, "api_key")
				delete(cred, "oauth")
			}
		}
	}
	out["redacted_fields"] = []string{"api_key", "oauth", "credentials.oauth_info"}
	return out
}

func (t *managementTools) entities(kind string) ([]managedEntity, error) {
	out := []managedEntity{}
	add := func(ref entityRef, name string, enabled *bool, tags []string, version any, data any) {
		out = append(out, managedEntity{Ref: ref, Name: name, Enabled: enabled, Tags: tags, Version: store.EntityVersion(version), Data: data})
	}
	channels, err := t.store.ListChannels()
	if err != nil {
		return nil, err
	}
	tags, err := t.store.ListTags()
	if err != nil {
		return nil, err
	}
	groups, err := t.store.ListTagGroups()
	if err != nil {
		return nil, err
	}
	byID := map[int64]store.Channel{}
	for _, c := range channels {
		byID[c.ID] = c
	}
	if kind == "" || kind == "channel" || kind == "credential" || kind == "upstream_model" {
		for _, c := range channels {
			if kind == "" || kind == "channel" {
				add(entityRef{Kind: "channel", ChannelID: c.ID}, c.Name, boolValue(c.Enabled), c.Tags, c, safeManagedChannel(c))
			}
			if kind == "" || kind == "credential" {
				for _, cred := range c.Credentials {
					label := cred.Label
					if label == "" {
						label = cred.ID
					}
					add(entityRef{Kind: "credential", ChannelID: c.ID, CredentialID: cred.ID}, label, boolValue(c.Enabled && cred.Enabled), nil, c, map[string]any{"id": cred.ID, "label": label, "channel_name": c.Name, "enabled": cred.Enabled, "has_key": cred.APIKey != "", "has_oauth": cred.OAuth != nil, "operations": cred.Operations, "channel_version": store.EntityVersion(c)})
				}
			}
			if kind == "" || kind == "upstream_model" {
				ids := slices.Clone(c.AvailableModels)
				for _, id := range c.Models {
					if !slices.Contains(ids, id) {
						ids = append(ids, id)
					}
				}
				for _, id := range ids {
					add(entityRef{Kind: "upstream_model", ChannelID: c.ID, Model: id}, id, boolValue(c.Enabled && slices.Contains(c.Models, id)), flattenModelTags(store.ResolveModelTags(c, id, tags, groups)), c, map[string]any{"channel_name": c.Name, "specs": c.ModelConfigs[id], "catalog_model_ref": c.ModelCatalogRefs[id], "channel_version": store.EntityVersion(c)})
				}
			}
		}
	}
	if kind == "" || kind == "public_model" {
		models, err := t.store.ListDownstreamModels()
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			data := asObject(m)
			data["effective_tags"] = gateway.ComputeEffectiveTags(m, byID, tags, groups)
			add(entityRef{Kind: "public_model", ID: m.ID}, m.DisplayName, boolValue(m.Enabled), m.Tags, m, data)
		}
	}
	if kind == "" || kind == "tag" {
		for _, v := range tags {
			add(entityRef{Kind: "tag", ID: v.ID}, v.Name, nil, nil, v, v)
		}
	}
	if kind == "" || kind == "tag_group" {
		for _, v := range groups {
			add(entityRef{Kind: "tag_group", ID: v.ID}, v.Name, nil, nil, v, v)
		}
	}
	if kind == "" || kind == "catalog" {
		rows, err := t.store.ListCatalogItems("", "")
		if err != nil {
			return nil, err
		}
		for _, v := range rows {
			add(entityRef{Kind: "catalog", Source: v.Source, ID: v.ID}, v.Name, nil, nil, v, v)
		}
	}
	if kind == "" || kind == "price" {
		rows, err := t.store.GetPrices()
		if err != nil {
			return nil, err
		}
		for _, v := range rows {
			add(entityRef{Kind: "price", ChannelID: v.ChannelID, Model: v.Model}, v.DisplayName, nil, nil, v, v)
		}
	}
	if kind == "" || kind == "client_key" {
		rows, err := t.store.ListClientKeys()
		if err != nil {
			return nil, err
		}
		for _, v := range rows {
			add(entityRef{Kind: "client_key", ID: strconv.FormatInt(v.ID, 10)}, v.Name, nil, nil, v, v)
			out[len(out)-1].Version = store.ClientKeyVersion(v)
		}
	}
	if kind == "" || kind == "settings" {
		v, err := t.store.GetSettings()
		if err != nil {
			return nil, err
		}
		safe := store.SafeSettings(v)
		add(entityRef{Kind: "settings"}, "系统设置", nil, nil, safe, safe)
	}
	if kind != "" && !slices.Contains([]string{"channel", "credential", "upstream_model", "public_model", "tag", "tag_group", "catalog", "price", "client_key", "settings"}, kind) {
		return nil, invalid("kind无效；可用channel/credential/upstream_model/public_model/tag/tag_group/catalog/price/client_key/settings")
	}
	sort.Slice(out, func(i, j int) bool { return refKey(out[i].Ref) < refKey(out[j].Ref) })
	return out, nil
}

func (t *managementTools) readOperations() []management.Operation {
	ops := t.baseReadOperations()
	ops = append(ops, typedOperation[searchInput]("search_entities", "entities.search", management.Read, "按类型、名称/ID、渠道和标签条件检索；默认启用对象、20项。结果含精确ref与version，has_more表示仍有结果。", true, func(ctx context.Context, p management.Principal, in searchInput) (any, error) {
		if in.Limit == 0 {
			in.Limit = 20
		}
		if in.Limit < 1 || in.Limit > 100 {
			return nil, invalid("limit须为1..100")
		}
		if len(in.TagGroups) > 0 {
			tags, err := t.store.ListTags()
			if err != nil {
				return nil, err
			}
			groups, err := t.store.ListTagGroups()
			if err != nil {
				return nil, err
			}
			for group, ids := range in.TagGroups {
				if !slices.ContainsFunc(groups, func(g store.TagGroup) bool { return g.ID == group }) {
					return nil, invalid("标签组不存在：" + group)
				}
				for _, id := range ids {
					if !slices.ContainsFunc(tags, func(tag store.Tag) bool { return tag.ID == id && tag.GroupID == group }) {
						return nil, invalid("标签不属于指定组：" + id)
					}
				}
			}
		}
		rows, err := t.entities(in.Kind)
		if err != nil {
			return nil, err
		}
		filtered := []managedEntity{}
		for _, row := range rows {
			if !in.IncludeDisabled && row.Enabled != nil && !*row.Enabled {
				continue
			}
			if in.ChannelID != 0 && row.Ref.ChannelID != in.ChannelID {
				continue
			}
			if in.Source != "" && row.Ref.Source != in.Source {
				continue
			}
			if in.Query != "" && !strings.Contains(strings.ToLower(row.Name+" "+refKey(row.Ref)), strings.ToLower(in.Query)) {
				continue
			}
			match := true
			for _, allowed := range in.TagGroups {
				found := false
				for _, id := range allowed {
					if slices.Contains(row.Tags, id) {
						found = true
					}
				}
				if !found {
					match = false
				}
			}
			if !match {
				continue
			}
			row.Data = nil
			filtered = append(filtered, row)
		}
		filter := in
		filter.Cursor = ""
		signature := store.EntityVersion(filter)
		snapshot := store.EntityVersion(filtered)
		offset := 0
		if in.Cursor != "" {
			raw, e := base64.RawURLEncoding.DecodeString(in.Cursor)
			var cursor searchCursor
			if e != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Offset < 0 || cursor.Offset > len(filtered) {
				return nil, invalid("无效cursor")
			}
			if cursor.Snapshot != snapshot || cursor.Filter != signature {
				return nil, store.ErrEditConflict
			}
			offset = cursor.Offset
		}
		end := min(offset+in.Limit, len(filtered))
		next := ""
		if end < len(filtered) {
			b, _ := json.Marshal(searchCursor{end, snapshot, signature})
			next = base64.RawURLEncoding.EncodeToString(b)
		}
		return map[string]any{"items": filtered[offset:end], "total": len(filtered), "has_more": end < len(filtered), "next_cursor": next, "filter": filter}, nil
	}))
	ops = append(ops, typedOperation[getInput]("get_entities", "entities.get", management.Read, "按search返回的精确ref批量读详情与当前version；最多100项。密钥必须用专门敏感读取工具。", true, func(ctx context.Context, p management.Principal, in getInput) (any, error) {
		if len(in.Refs) < 1 || len(in.Refs) > 100 {
			return nil, invalid("refs须为1..100项")
		}
		cache := map[string]map[string]managedEntity{}
		out := []managedEntity{}
		for _, ref := range in.Refs {
			if cache[ref.Kind] == nil {
				rows, e := t.entities(ref.Kind)
				if e != nil {
					return nil, e
				}
				cache[ref.Kind] = map[string]managedEntity{}
				for _, row := range rows {
					cache[ref.Kind][refKey(row.Ref)] = row
				}
			}
			row, ok := cache[ref.Kind][refKey(ref)]
			if !ok {
				return nil, management.NewError("not_found", fmt.Sprintf("对象不存在：%s", refKey(ref)))
			}
			if len(in.Fields) > 0 {
				fields := asObject(row.Data)
				selection := map[string]any{}
				for _, name := range in.Fields {
					v, exists := fields[name]
					if !exists {
						return nil, invalid("不存在或不可读取的字段：" + name)
					}
					selection[name] = v
				}
				row.Data = selection
			}
			out = append(out, row)
		}
		return map[string]any{"items": out}, nil
	}))
	return ops
}
