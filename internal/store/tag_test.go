package store

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTagStore_BuiltinSeeding(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	groups, err := st.ListTagGroups()
	if err != nil {
		t.Fatalf("ListTagGroups: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("expected 3 builtin groups, got %d", len(groups))
	}
	groupIDs := []string{groups[0].ID, groups[1].ID, groups[2].ID}
	if !slices.Contains(groupIDs, "billing") || !slices.Contains(groupIDs, "privacy") || !slices.Contains(groupIDs, "modality") {
		t.Fatalf("unexpected group IDs: %v", groupIDs)
	}

	tags, err := st.ListTags()
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != len(DefaultTags) {
		t.Fatalf("expected %d default tags, got %d", len(DefaultTags), len(tags))
	}

	// 验证内置组不可删除
	if err := st.DeleteTagGroup("billing"); err == nil {
		t.Fatalf("expected error deleting builtin group, got nil")
	}
	// 验证内置标签不可删除
	if err := st.DeleteTag("billing:free", false); err == nil {
		t.Fatalf("expected error deleting builtin tag, got nil")
	}
}

func TestTagStore_UserCustomTagsAndGroups(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// 1. 创建自定义组
	customGroup := TagGroup{
		ID:          "scenario",
		Name:        "使用场景",
		Type:        TagGroupTypeUser,
		Description: "自定义场景标签组",
		SortOrder:   10,
	}
	if err := st.SaveTagGroup(customGroup); err != nil {
		t.Fatalf("SaveTagGroup: %v", err)
	}

	// 2. 创建自定义标签
	customTag := Tag{
		ID:          "scenario:daily",
		GroupID:     "scenario",
		Name:        "日常测试",
		Color:       "emerald",
		Description: "日常轻量测试",
		SortOrder:   1,
	}
	if err := st.SaveTag(customTag); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	// 检查列表
	tags, err := st.ListTags()
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	found := false
	for _, tag := range tags {
		if tag.ID == "scenario:daily" && tag.Name == "日常测试" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created custom tag not found in ListTags")
	}

	// 3. 组内有标签时不可直接删组
	if err := st.DeleteTagGroup("scenario"); err == nil {
		t.Fatalf("expected error deleting group with active tags")
	}

	// 4. 删除自定义标签
	if err := st.DeleteTag("scenario:daily", false); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}

	// 5. 组内无标签后可以删组
	if err := st.DeleteTagGroup("scenario"); err != nil {
		t.Fatalf("DeleteTagGroup after emptied: %v", err)
	}
}

func TestTagStore_ReferencesAndCascadeDetach(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.SaveTagGroup(TagGroup{ID: "custom", Name: "Custom", Type: TagGroupTypeUser}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"custom:test", "custom:keep"} {
		if err := st.SaveTag(Tag{ID: id, GroupID: "custom", Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	// 创建渠道
	ch := Channel{
		Name:    "Test Channel",
		APIKey:  "sk-test-key",
		Models:  []string{"model-1"},
		Enabled: true,
		Tags:    []string{"billing:free", "custom:test"},
		ModelTags: map[string][]string{
			"model-1": {"custom:test", "privacy:zdr"},
		},
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "http://example.com"}},
	}
	if err := st.SaveChannel(&ch); err != nil {
		t.Fatalf("SaveChannel: %v", err)
	}
	chID := ch.ID

	// 创建下游模型
	dm := DownstreamModel{
		ID:          "dm-1",
		DisplayName: "Downstream 1",
		Enabled:     true,
		Tags:        []string{"custom:test", "custom:keep"},
		Bindings: []Binding{
			{ChannelID: chID, Model: "model-1", Priority: 1, Enabled: true},
		},
	}
	if err := st.SaveDownstreamModel(dm); err != nil {
		t.Fatalf("SaveDownstreamModel: %v", err)
	}

	// 查询引用
	refs, err := st.GetTagReferences("custom:test")
	if err != nil {
		t.Fatalf("GetTagReferences: %v", err)
	}
	if len(refs.ChannelNames) != 1 || refs.ChannelNames[0] != "Test Channel" {
		t.Fatalf("expected Test Channel in references, got %v", refs.ChannelNames)
	}
	if len(refs.DownstreamModels) != 1 || refs.DownstreamModels[0] != "Downstream 1" {
		t.Fatalf("expected Downstream 1 in references, got %v", refs.DownstreamModels)
	}

	// 级联解绑
	if err := st.DeleteTag("custom:test", true); err != nil {
		t.Fatalf("DeleteTag(force=true): %v", err)
	}

	// 验证解绑结果
	chUpdated, err := st.Channel(chID)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if slices.Contains(chUpdated.Tags, "custom:test") {
		t.Fatalf("expected custom:test removed from channel tags, got %v", chUpdated.Tags)
	}
	if slices.Contains(chUpdated.ModelTags["model-1"], "custom:test") {
		t.Fatalf("expected custom:test removed from channel model tags, got %v", chUpdated.ModelTags["model-1"])
	}

	dms, err := st.ListDownstreamModels()
	if err != nil {
		t.Fatalf("ListDownstreamModels: %v", err)
	}
	if slices.Contains(dms[0].Tags, "custom:test") {
		t.Fatalf("expected custom:test removed from downstream model tags, got %v", dms[0].Tags)
	}
	// 确认未受影响的其它标签仍然保留
	if !slices.Contains(dms[0].Tags, "custom:keep") {
		t.Fatalf("expected custom:keep preserved, got %v", dms[0].Tags)
	}
}

func TestDeleteTagPreservesConcurrentSpecAndModelCommit(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveTagGroup(TagGroup{ID: "custom", Name: "Custom", Type: TagGroupTypeUser}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"custom:remove", "custom:keep", "custom:model-keep"} {
		if err := s.SaveTag(Tag{ID: id, GroupID: "custom", Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	c := Channel{Name: "tagged", APIKey: "synthetic", Enabled: true, Models: []string{"native"}, Tags: []string{"custom:remove", "custom:keep"}, ModelTags: map[string][]string{"native": {"custom:remove", "custom:model-keep"}}, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}, ModelConfigs: map[string]ModelConfig{"native": {MaxOutputTokens: ptr(int64(1))}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := DownstreamModel{ID: "public", Tags: []string{"custom:remove", "custom:keep"}, Bindings: []Binding{{ChannelID: c.ID, Model: "native", Enabled: true}}}
	if err = s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	s.channelSaveMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.channelSaveMu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- s.DeleteTag("custom:remove", true) }()
	// Wait for the actual cascade to reach the shared save lock. This establishes
	// ordering without a scheduler delay or a production synchronization hook.
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		stacks := make([]byte, 1<<20)
		n := runtime.Stack(stacks, true)
		for _, stack := range strings.Split(string(stacks[:n]), "\n\n") {
			if strings.Contains(stack, ".DeleteTag(") && (strings.Contains(stack, "sync.(*Mutex).Lock(") || strings.Contains(stack, "sync.(*Mutex).lockSlow(")) {
				blocked = true
				break
			}
		}
		if blocked {
			break
		}
		runtime.Gosched()
	}
	if !blocked {
		t.Fatal("cascade did not reach shared channel save lock")
	}
	// Commit the same model-only update SaveModelSpecs writes while owning its
	// mutex. The queued cascade must preserve this newer row, including ref.
	current, err := s.channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.ModelConfigs["native"] = ModelConfig{MaxOutputTokens: ptr(int64(99)), Streaming: ptr(false)}
	current.ModelCatalogRefs = map[string]CatalogModelRef{"native": {Source: "builtin", ID: "new-reference"}}
	if err = s.writeChannelInPlace(&current); err != nil {
		t.Fatal(err)
	}
	m.DisplayName, m.SortOrder, m.Enabled = "concurrent edit", 23, true
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = validateAndSaveDownstreamModelInTx(tx, m); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.channelSaveMu.Unlock()
	locked = false
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cascade did not finish")
	}
	saved, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *saved.ModelConfigs["native"].MaxOutputTokens != 99 || saved.ModelConfigs["native"].Streaming == nil || *saved.ModelConfigs["native"].Streaming || saved.ModelCatalogRefs["native"].ID != "new-reference" {
		t.Fatal("tag detach overwrote the newer model specification")
	}
	if slices.Contains(saved.Tags, "custom:remove") || slices.Contains(saved.ModelTags["native"], "custom:remove") || !slices.Contains(saved.Tags, "custom:keep") || !slices.Contains(saved.ModelTags["native"], "custom:model-keep") {
		t.Fatal("tag detach lost unrelated tags or retained target")
	}
	if saved.APIKey != c.APIKey || saved.Generation != c.Generation {
		t.Fatal("tag detach changed credential or continuation generation")
	}
	models, err := s.ListDownstreamModels()
	if err != nil || len(models) != 1 || models[0].DisplayName != "concurrent edit" || models[0].SortOrder != 23 || !models[0].Enabled || slices.Contains(models[0].Tags, "custom:remove") || !slices.Contains(models[0].Tags, "custom:keep") {
		t.Fatal("tag deletion overwrote a newer public model edit", models, err)
	}
}
