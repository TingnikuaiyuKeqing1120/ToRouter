package store

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestTagOrderAtomicAndReferences(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"first", "empty"} {
		if err := s.SaveTagGroup(TagGroup{ID: id, Name: id, Type: TagGroupTypeUser, SortOrder: 30}); err != nil {
			t.Fatal(err)
		}
	}
	for index, id := range []string{"first:a", "first:b"} {
		if err := s.SaveTag(Tag{ID: id, GroupID: "first", Name: id, Color: "rose", Icon: "custom", Description: "keep", SortOrder: 10 + index}); err != nil {
			t.Fatal(err)
		}
	}
	c := Channel{Name: "reference", Enabled: true, Models: []string{"native"}, APIKey: "synthetic", Tags: []string{"first:a"}, ModelTags: map[string][]string{"native": {"first:b"}}, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(DownstreamModel{ID: "public", Enabled: true, Tags: []string{"first:a"}, Bindings: []Binding{{ChannelID: c.ID, Model: "native", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	beforeChannel, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeModel, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	beforeTags, err := s.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]string{nil, {"first:b"}, {"first:b", "first:b"}, {"first:b", "billing:free"}, {"first:b", "ghost"}, {"first:b", "first:a", "first:a"}} {
		if err := s.ReorderTags("first", order); err == nil {
			t.Fatalf("accepted invalid order %v", order)
		}
		after, err := s.ListTags()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(beforeTags, after) {
			t.Fatal("rejected order changed tags")
		}
	}
	if err := s.ReorderTags("missing", []string{}); err == nil {
		t.Fatal("accepted missing group")
	}
	if err := s.ReorderTags("empty", []string{"empty:unknown"}); err != nil {
		t.Fatal(err)
	}
	beforeTags, _ = s.ListTags()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_tag_order BEFORE UPDATE ON tags WHEN NEW.id='first:a' BEGIN SELECT RAISE(ABORT, 'write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReorderTags("first", []string{"first:b", "first:a", "first:unknown"}); err == nil {
		t.Fatal("expected write failure")
	}
	afterFailed, _ := s.ListTags()
	if !reflect.DeepEqual(beforeTags, afterFailed) {
		t.Fatal("transaction partially saved tag order")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_tag_order"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReorderTags("first", []string{"first:b", "first:a", "first:unknown"}); err != nil {
		t.Fatal(err)
	}
	afterTags, _ := s.ListTags()
	var ids []string
	for _, tag := range afterTags {
		if tag.GroupID == "first" {
			ids = append(ids, tag.ID)
			if tag.ID != "first:unknown" && (tag.Color != "rose" || tag.Icon != "custom" || tag.Description != "keep") {
				t.Fatal("lost metadata")
			}
		}
	}
	if !reflect.DeepEqual(ids, []string{"first:b", "first:a", "first:unknown"}) {
		t.Fatalf("unexpected order %v", ids)
	}
	afterChannel, _ := s.Channel(c.ID)
	afterModel, _ := s.ListDownstreamModels()
	if !reflect.DeepEqual(beforeChannel, afterChannel) || !reflect.DeepEqual(beforeModel, afterModel) {
		t.Fatal("reorder changed referenced objects")
	}
	if err := s.DeleteTag("billing:free", true); err == nil {
		t.Fatal("lost core protection")
	}
	groups, _ := s.ListTagGroups()
	var groupIDs []string
	for _, group := range groups {
		groupIDs = append(groupIDs, group.ID)
	}
	slices.Reverse(groupIDs)
	for _, invalid := range [][]string{groupIDs[:len(groupIDs)-1], append(slices.Clone(groupIDs[:len(groupIDs)-1]), groupIDs[0]), append(slices.Clone(groupIDs[:len(groupIDs)-1]), "ghost")} {
		if err := s.ReorderTagGroups(invalid); err == nil {
			t.Fatal("accepted invalid group order")
		}
		actual, _ := s.ListTagGroups()
		if !reflect.DeepEqual(groups, actual) {
			t.Fatal("rejected group order changed rows")
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_group_order BEFORE UPDATE ON tag_groups WHEN NEW.id='billing' BEGIN SELECT RAISE(ABORT, 'write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReorderTagGroups(groupIDs); err == nil {
		t.Fatal("expected group write failure")
	}
	unchanged, _ := s.ListTagGroups()
	if !reflect.DeepEqual(groups, unchanged) {
		t.Fatal("transaction partially saved group order")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_group_order"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReorderTagGroups(groupIDs); err != nil {
		t.Fatal(err)
	}
	reordered, _ := s.ListTagGroups()
	for index, group := range reordered {
		if group.ID != groupIDs[index] || group.SortOrder != index+1 {
			t.Fatal("group order not saved")
		}
	}
	// Only sort_order is rewritten, even for extra stored metadata.
	var raw string
	if err := s.db.QueryRow("SELECT data FROM tags WHERE id='first:a'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	fields["extra"] = "retained"
	encoded, _ := json.Marshal(fields)
	if _, err := s.db.Exec("UPDATE tags SET data=? WHERE id='first:a'", string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReorderTags("first", []string{"first:a", "first:b", "first:unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT data FROM tags WHERE id='first:a'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["extra"] != "retained" {
		t.Fatal("lost extra metadata")
	}
}
