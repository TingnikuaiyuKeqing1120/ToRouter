package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

type TagGroupType string

const (
	TagGroupTypeBuiltin TagGroupType = "builtin"
	TagGroupTypeUser    TagGroupType = "user"
)

type TagGroup struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Type        TagGroupType `json:"type"`
	Description string       `json:"description"`
	SortOrder   int          `json:"sort_order"`
}

type Tag struct {
	ID          string `json:"id"`
	GroupID     string `json:"group_id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Icon        string `json:"icon,omitempty"`
	Description string `json:"description"`
	SortOrder   int    `json:"sort_order"`
}

type TagWithGroup struct {
	Tag
	GroupName string       `json:"group_name"`
	GroupType TagGroupType `json:"group_type"`
}

type TagReferences struct {
	TagID            string   `json:"tag_id"`
	ChannelNames     []string `json:"channel_names"`
	DownstreamModels []string `json:"downstream_models"`
	ClientKeyNames   []string `json:"client_key_names"`
}

var DefaultTagGroups = []TagGroup{
	{
		ID:          "billing",
		Name:        "计费模式",
		Type:        TagGroupTypeBuiltin,
		Description: "区分渠道与模型的商业付费模式（免费、订阅套餐、按量付费）",
		SortOrder:   1,
	},
	{
		ID:          "privacy",
		Name:        "数据隐私",
		Type:        TagGroupTypeBuiltin,
		Description: "数据留存策略与训练政策（零保留、不训练、参与训练）",
		SortOrder:   2,
	},
	{
		ID:          "modality",
		Name:        "模态支持",
		Type:        TagGroupTypeBuiltin,
		Description: "输入与输出模态能力声明（文本、图像、音频等）",
		SortOrder:   3,
	},
}

var DefaultTags = []Tag{
	// billing
	{ID: "billing:free", GroupID: "billing", Name: "免费 / 赠额", Color: "emerald", Description: "免费渠道、试用赠额或社区公益接口", SortOrder: 1},
	{ID: "billing:plan", GroupID: "billing", Name: "订阅套餐", Color: "violet", Description: "月费/季费订阅包（如 Copilot, ChatGPT Plus 等）", SortOrder: 2},
	{ID: "billing:payg", GroupID: "billing", Name: "按量付费", Color: "amber", Description: "官方商业 API，按 Token/调用量扣费", SortOrder: 3},
	{ID: "billing:unknown", GroupID: "billing", Name: "未知计费", Color: "slate", Description: "未明确计费模式", SortOrder: 4},

	// privacy
	{ID: "privacy:zdr", GroupID: "privacy", Name: "零保留 (ZDR)", Color: "emerald", Description: "Zero Data Retention 商业隐私承诺，数据不落地上游", SortOrder: 1},
	{ID: "privacy:log_no_train", GroupID: "privacy", Name: "留存不训练", Color: "sky", Description: "上游留存请求日志用于安全排障，但明确不用于模型训练", SortOrder: 2},
	{ID: "privacy:train", GroupID: "privacy", Name: "参与训练", Color: "rose", Description: "用户数据可能被上游用于模型优化与训练", SortOrder: 3},
	{ID: "privacy:unknown", GroupID: "privacy", Name: "未知隐私", Color: "slate", Description: "未明确隐私条款", SortOrder: 4},

	{ID: "modality:unknown", GroupID: "modality", Name: "未知模态", Color: "slate", SortOrder: 10},

	// modality (输入)
	{ID: "modality:input:text", GroupID: "modality", Name: "文本输入", Color: "sky", Description: "支持自然语言文本输入", SortOrder: 1},
	{ID: "modality:input:image", GroupID: "modality", Name: "图像输入", Color: "emerald", Description: "支持图片/视觉输入", SortOrder: 2},
	{ID: "modality:input:audio", GroupID: "modality", Name: "音频输入", Color: "violet", Description: "支持语音/音频输入 (ASR/分析)", SortOrder: 3},
	{ID: "modality:input:video", GroupID: "modality", Name: "视频输入", Color: "amber", Description: "支持视频帧输入", SortOrder: 4},
	{ID: "modality:input:file", GroupID: "modality", Name: "文件输入", Color: "indigo", Description: "支持文档/PDF文件输入", SortOrder: 5},

	// modality (输出)
	{ID: "modality:output:text", GroupID: "modality", Name: "文本输出", Color: "sky", Description: "支持文本生成输出", SortOrder: 6},
	{ID: "modality:output:image", GroupID: "modality", Name: "图像输出", Color: "emerald", Description: "支持图像生成输出", SortOrder: 7},
	{ID: "modality:output:audio", GroupID: "modality", Name: "音频输出", Color: "violet", Description: "支持语音音频合成输出 (TTS)", SortOrder: 8},
	{ID: "modality:output:video", GroupID: "modality", Name: "视频输出", Color: "amber", Description: "支持视频生成输出", SortOrder: 9},
}

func isBuiltinGroup(id string) bool {
	for _, g := range DefaultTagGroups {
		if g.ID == id {
			return true
		}
	}
	return false
}

func isBuiltinTag(id string) bool {
	for _, t := range DefaultTags {
		if t.ID == id {
			return true
		}
	}
	return false
}

func (s *Store) initTags() error {
	schema := `
CREATE TABLE IF NOT EXISTS tag_groups (
	id TEXT PRIMARY KEY,
	type TEXT NOT NULL,
	data TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tags (
	id TEXT PRIMARY KEY,
	group_id TEXT NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tags_group ON tags(group_id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("init tag schema: %w", err)
	}

	// 检查并初始化默认组
	for _, g := range DefaultTagGroups {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM tag_groups WHERE id=?", g.ID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			raw, _ := json.Marshal(g)
			if _, err := s.db.Exec("INSERT INTO tag_groups(id, type, data) VALUES(?, ?, ?)", g.ID, string(g.Type), string(raw)); err != nil {
				return err
			}
		}
	}

	// 检查并初始化默认标签
	for _, t := range DefaultTags {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM tags WHERE id=?", t.ID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			raw, _ := json.Marshal(t)
			if _, err := s.db.Exec("INSERT INTO tags(id, group_id, data) VALUES(?, ?, ?)", t.ID, t.GroupID, string(raw)); err != nil {
				return err
			}
		}
	}

	groups, err := s.ListTagGroups()
	if err != nil {
		return err
	}
	for _, group := range groups {
		unknown := Tag{ID: UnknownTagID(group.ID), GroupID: group.ID, Name: "未知", Color: "slate", SortOrder: 1000}
		raw, _ := json.Marshal(unknown)
		if _, err = s.db.Exec("INSERT OR IGNORE INTO tags(id,group_id,data) VALUES(?,?,?)", unknown.ID, group.ID, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListTagGroups() ([]TagGroup, error) {
	rows, err := s.db.Query("SELECT data FROM tag_groups")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []TagGroup
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var g TagGroup
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	slices.SortStableFunc(groups, func(a, b TagGroup) int {
		if a.SortOrder != b.SortOrder {
			return a.SortOrder - b.SortOrder
		}
		return strings.Compare(a.ID, b.ID)
	})
	return groups, nil
}

// ReorderTagGroups requires the complete current group set, exactly once.
func (s *Store) ReorderTagGroups(order []string) error {
	return s.reorderTagRecords("tag_groups", "", order)
}

// ReorderTags changes presentation order inside one group without rewriting identities or references.
func (s *Store) ReorderTags(groupID string, order []string) error {
	return s.reorderTagRecords("tags", groupID, order)
}

func (s *Store) reorderTagRecords(table, groupID string, order []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := "SELECT id, data FROM tag_groups"
	var args []any
	if table == "tags" {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM tag_groups WHERE id=?", groupID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return ErrNotFound
		}
		query = "SELECT id, data FROM tags WHERE group_id=?"
		args = append(args, groupID)
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	records := make(map[string]map[string]json.RawMessage)
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal([]byte(raw), &fields); err != nil {
			rows.Close()
			return err
		}
		if fields == nil {
			rows.Close()
			return NewConfigurationError("tag ordering record must be an object")
		}
		records[id] = fields
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(order) != len(records) {
		return NewConfigurationError("order must contain every current ID exactly once")
	}
	seen := make(map[string]bool, len(order))
	for _, id := range order {
		if _, ok := records[id]; !ok || seen[id] {
			return NewConfigurationError("order contains an unknown or duplicate ID")
		}
		seen[id] = true
	}
	for index, id := range order {
		fields := records[id]
		fields["sort_order"], _ = json.Marshal(index + 1)
		raw, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE "+table+" SET data=? WHERE id=?", string(raw), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SaveTagGroup(group TagGroup) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	return s.saveTagGroupLocked(group)
}

func (s *Store) saveTagGroupLocked(group TagGroup) error {
	group.ID = strings.TrimSpace(group.ID)
	group.Name = strings.TrimSpace(group.Name)
	if group.ID == "" || len(group.ID) > 64 || strings.ContainsAny(group.ID, " \t\r\n:/\\") {
		return NewConfigurationError("tag group ID must be 1..64 non-empty alphanumeric characters without colons or slashes")
	}
	if group.Name == "" || len(group.Name) > 100 {
		return NewConfigurationError("tag group name must be 1..100 characters")
	}

	var existingRaw string
	var existingType string
	err := s.db.QueryRow("SELECT type, data FROM tag_groups WHERE id=?", group.ID).Scan(&existingType, &existingRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if errors.Is(err, sql.ErrNoRows) {
		if group.Type == "" {
			group.Type = TagGroupTypeUser
		}
		raw, _ := json.Marshal(group)
		tx, e := s.db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec("INSERT INTO tag_groups(id, type, data) VALUES(?, ?, ?)", group.ID, string(group.Type), string(raw)); e != nil {
			return e
		}
		unknown := Tag{ID: UnknownTagID(group.ID), GroupID: group.ID, Name: "未知", Color: "slate", SortOrder: 1000}
		data, _ := json.Marshal(unknown)
		if _, e = tx.Exec("INSERT INTO tags(id,group_id,data) VALUES(?,?,?)", unknown.ID, group.ID, string(data)); e != nil {
			return e
		}
		return tx.Commit()
	}

	// 存在时更新
	if existingType == string(TagGroupTypeBuiltin) {
		group.Type = TagGroupTypeBuiltin // 保护内置组类型
	} else if group.Type == "" {
		group.Type = TagGroupType(existingType)
	}
	raw, _ := json.Marshal(group)
	_, err = s.db.Exec("UPDATE tag_groups SET data=? WHERE id=?", string(raw), group.ID)
	return err
}

func (s *Store) DeleteTagGroup(id string) error { return s.DeleteTagGroupAtVersion(id, "") }

func (s *Store) DeleteTagGroupAtVersion(id string, expected string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if expected != "" {
		state, err := s.tagDeletionStateLocked(id, true)
		if err != nil {
			return err
		}
		if state.Version != expected {
			return ErrEditConflict
		}
	}
	id = strings.TrimSpace(id)
	if isBuiltinGroup(id) {
		return NewConfigurationError("cannot delete builtin tag group")
	}

	var tagCount int
	if err := s.db.QueryRow("SELECT count(*) FROM tags WHERE group_id=? AND id<>?", id, UnknownTagID(id)).Scan(&tagCount); err != nil {
		return err
	}
	if tagCount > 0 {
		return NewConfigurationError(fmt.Sprintf("tag group has %d tags, please delete its tags first", tagCount))
	}

	keys, err := s.ListClientKeys()
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, ok := key.Scope.TagRules[id]; ok {
			return NewConfigurationError("tag group is referenced by a client key")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	refs, changes, err := tagReferenceChanges(tx, UnknownTagID(id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if len(refs.ClientKeyNames) > 0 {
		return &TagInUseError{References: refs}
	}
	for _, change := range changes {
		if _, err = tx.Exec("UPDATE "+change.table+" SET data=? WHERE id=?", change.data, change.id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM tags WHERE id=?", UnknownTagID(id)); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM tag_groups WHERE id=?", id)
	if err != nil {
		return err
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) ListTags() ([]Tag, error) {
	rows, err := s.db.Query("SELECT data FROM tags")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var t Tag
		if err := json.Unmarshal([]byte(raw), &t); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	slices.SortStableFunc(tags, func(a, b Tag) int {
		if a.GroupID != b.GroupID {
			return strings.Compare(a.GroupID, b.GroupID)
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder - b.SortOrder
		}
		return strings.Compare(a.ID, b.ID)
	})
	return tags, nil
}

func (s *Store) SaveTag(tag Tag) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	return s.saveTagLocked(tag)
}

func (s *Store) saveTagLocked(tag Tag) error {
	tag.ID = strings.TrimSpace(tag.ID)
	tag.GroupID = strings.TrimSpace(tag.GroupID)
	tag.Name = strings.TrimSpace(tag.Name)
	if tag.ID == "" || len(tag.ID) > 100 || strings.ContainsAny(tag.ID, " \t\r\n/\\") {
		return NewConfigurationError("tag ID must be 1..100 characters without whitespace or slashes")
	}
	if tag.GroupID == "" {
		return NewConfigurationError("group_id is required")
	}
	if tag.Name == "" || len(tag.Name) > 100 {
		return NewConfigurationError("tag name must be 1..100 characters")
	}
	if tag.Color == "" {
		tag.Color = "slate"
	}

	// 验证 group 存在
	var groupCount int
	if err := s.db.QueryRow("SELECT count(*) FROM tag_groups WHERE id=?", tag.GroupID).Scan(&groupCount); err != nil {
		return err
	}
	if groupCount == 0 {
		return NewConfigurationError("tag group does not exist")
	}

	var existingRaw string
	err := s.db.QueryRow("SELECT data FROM tags WHERE id=?", tag.ID).Scan(&existingRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if errors.Is(err, sql.ErrNoRows) {
		raw, _ := json.Marshal(tag)
		_, err = s.db.Exec("INSERT INTO tags(id, group_id, data) VALUES(?, ?, ?)", tag.ID, tag.GroupID, string(raw))
		return err
	}

	var existing Tag
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return err
	}
	if (isBuiltinTag(tag.ID) || tag.ID == UnknownTagID(existing.GroupID)) && tag.GroupID != existing.GroupID {
		return NewConfigurationError("cannot move protected unknown tag")
	}
	if tag.GroupID != existing.GroupID {
		return s.moveTagLocked(existing, tag)
	}
	// 存在时更新
	raw, _ := json.Marshal(tag)
	_, err = s.db.Exec("UPDATE tags SET group_id=?, data=? WHERE id=?", tag.GroupID, string(raw), tag.ID)
	return err
}

// TagInUseError carries the references observed inside the deletion transaction.
type TagInUseError struct{ References TagReferences }

func (e *TagInUseError) Error() string { return "tag is in use" }

func (s *Store) DeleteTag(id string, force bool) error { return s.DeleteTagAtVersion(id, force, "") }

func (s *Store) DeleteTagAtVersion(id string, force bool, expected string) error {
	id = strings.TrimSpace(id)
	if isBuiltinTag(id) {
		return NewConfigurationError("cannot delete builtin core tag")
	}
	// Keep channel read-modify-write operations outside SQL transactions from
	// replaying old specification or credential snapshots over this tag edit.
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if expected != "" {
		state, err := s.tagDeletionStateLocked(id, false)
		if err != nil {
			return err
		}
		if state.Version != expected {
			return ErrEditConflict
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow("SELECT count(*) FROM tags WHERE id=?", id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	var groupID string
	if err = tx.QueryRow("SELECT group_id FROM tags WHERE id=?", id).Scan(&groupID); err != nil {
		return err
	}
	if id == UnknownTagID(groupID) {
		return NewConfigurationError("cannot delete protected unknown tag")
	}
	refs, changes, err := tagReferenceChanges(tx, id)
	if err != nil {
		return err
	}
	if len(refs.ClientKeyNames) > 0 || len(changes) != 0 && !force {
		return &TagInUseError{References: refs}
	}
	for _, change := range changes {
		if _, err = tx.Exec("UPDATE "+change.table+" SET data=? WHERE id=?", change.data, change.id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM tags WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// Only tag fields are rewritten, preserving sealed credentials and all other
// fields in the current transaction snapshot, including model edits.
type tagReferenceChange struct{ table, id, data string }

func tagReferenceChanges(tx *sql.Tx, tagID string) (TagReferences, []tagReferenceChange, error) {
	refs := TagReferences{TagID: tagID, ChannelNames: []string{}, DownstreamModels: []string{}, ClientKeyNames: []string{}}
	var changes []tagReferenceChange
	var groupID string
	if err := tx.QueryRow("SELECT group_id FROM tags WHERE id=?", tagID).Scan(&groupID); err != nil {
		return refs, nil, err
	}
	groupTags := map[string]bool{}
	groupRows, err := tx.Query("SELECT id FROM tags WHERE group_id=?", groupID)
	if err != nil {
		return refs, nil, err
	}
	for groupRows.Next() {
		var id string
		if err = groupRows.Scan(&id); err != nil {
			groupRows.Close()
			return refs, nil, err
		}
		groupTags[id] = true
	}
	err = groupRows.Err()
	groupRows.Close()
	if err != nil {
		return refs, nil, err
	}
	replaceDeletedTag := func(ids []string, tagID, groupID string) []string {
		out := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == tagID })
		for _, id := range out {
			if groupTags[id] {
				return out
			}
		}
		if tagID == UnknownTagID(groupID) {
			return out
		}
		return append(out, UnknownTagID(groupID))
	}
	for _, table := range []string{"channels", "downstream_models"} {
		rows, err := tx.Query("SELECT id, data FROM " + table + " ORDER BY id")
		if err != nil {
			return refs, nil, err
		}
		for rows.Next() {
			var id, raw string
			if err = rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return refs, nil, err
			}
			var fields map[string]json.RawMessage
			var value struct {
				Name        string              `json:"name"`
				DisplayName string              `json:"display_name"`
				Tags        []string            `json:"tags"`
				ModelTags   map[string][]string `json:"model_tags"`
			}
			if err = json.Unmarshal([]byte(raw), &fields); err != nil {
				rows.Close()
				return refs, nil, err
			}
			if err = json.Unmarshal([]byte(raw), &value); err != nil {
				rows.Close()
				return refs, nil, err
			}
			changed := false
			if slices.Contains(value.Tags, tagID) {
				value.Tags = replaceDeletedTag(value.Tags, tagID, groupID)
				fields["tags"], _ = json.Marshal(value.Tags)
				changed = true
			}
			if table == "channels" {
				modelChanged := false
				for model, tags := range value.ModelTags {
					if slices.Contains(tags, tagID) {
						value.ModelTags[model] = replaceDeletedTag(tags, tagID, groupID)
						modelChanged = true
					}
				}
				if modelChanged {
					fields["model_tags"], _ = json.Marshal(value.ModelTags)
					changed = true
				}
			}
			if !changed {
				continue
			}
			if table == "channels" {
				refs.ChannelNames = append(refs.ChannelNames, value.Name)
			} else {
				name := value.DisplayName
				if name == "" {
					name = id
				}
				refs.DownstreamModels = append(refs.DownstreamModels, name)
			}
			data, err := json.Marshal(fields)
			if err != nil {
				rows.Close()
				return refs, nil, err
			}
			changes = append(changes, tagReferenceChange{table: table, id: id, data: string(data)})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return refs, nil, err
		}
	}
	rows, err := tx.Query("SELECT name,scope FROM client_keys")
	if err != nil {
		return refs, nil, err
	}
	for rows.Next() {
		var name, raw string
		if err = rows.Scan(&name, &raw); err != nil {
			rows.Close()
			return refs, nil, err
		}
		scope, e := decodeClientScope(raw)
		if e != nil {
			rows.Close()
			return refs, nil, e
		}
		for _, ids := range scope.TagRules {
			if slices.Contains(ids, tagID) {
				refs.ClientKeyNames = append(refs.ClientKeyNames, name)
				break
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return refs, nil, err
	}
	return refs, changes, nil
}

func (s *Store) ResetBuiltinTags() error { return s.ResetBuiltinTagsAtVersion("") }

func (s *Store) ResetBuiltinTagsAtVersion(expected string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if expected != "" {
		tags, e := s.ListTags()
		if e != nil {
			return e
		}
		groups, e := s.ListTagGroups()
		if e != nil {
			return e
		}
		if EntityVersion(map[string]any{"tags": tags, "groups": groups}) != expected {
			return ErrEditConflict
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, g := range DefaultTagGroups {
		raw, _ := json.Marshal(g)
		if _, err := tx.Exec("INSERT OR REPLACE INTO tag_groups(id, type, data) VALUES(?, ?, ?)", g.ID, string(g.Type), string(raw)); err != nil {
			return err
		}
	}
	for _, t := range DefaultTags {
		raw, _ := json.Marshal(t)
		if _, err := tx.Exec("INSERT OR REPLACE INTO tags(id, group_id, data) VALUES(?, ?, ?)", t.ID, t.GroupID, string(raw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetTagReferences 查找标签在所有渠道与下游模型中的引用情况
func (s *Store) GetTagReferences(tagID string) (TagReferences, error) {
	tagID = strings.TrimSpace(tagID)
	refs := TagReferences{
		TagID:            tagID,
		ChannelNames:     []string{},
		DownstreamModels: []string{},
		ClientKeyNames:   []string{},
	}

	channels, err := s.ListChannels()
	if err != nil {
		return refs, err
	}
	for _, ch := range channels {
		matched := false
		if slices.Contains(ch.Tags, tagID) {
			matched = true
		} else {
			for _, mtags := range ch.ModelTags {
				if slices.Contains(mtags, tagID) {
					matched = true
					break
				}
			}
		}
		if matched {
			refs.ChannelNames = append(refs.ChannelNames, ch.Name)
		}
	}

	models, err := s.ListDownstreamModels()
	if err != nil {
		return refs, err
	}
	for _, m := range models {
		if slices.Contains(m.Tags, tagID) {
			name := m.DisplayName
			if name == "" {
				name = m.ID
			}
			refs.DownstreamModels = append(refs.DownstreamModels, name)
		}
	}

	keys, err := s.ListClientKeys()
	if err != nil {
		return refs, err
	}
	for _, key := range keys {
		for _, ids := range key.Scope.TagRules {
			if slices.Contains(ids, tagID) {
				refs.ClientKeyNames = append(refs.ClientKeyNames, key.Name)
				break
			}
		}
	}
	return refs, nil
}
