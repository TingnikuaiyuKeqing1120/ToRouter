package store

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestDownstreamManualTagsNormalizeEveryWriteWithoutRewritingLegacyRows(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveTagGroup(TagGroup{ID: "scenario", Name: "Scenario", Type: TagGroupTypeUser}); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []Tag{
		{ID: "scenario:daily", GroupID: "scenario", Name: "Daily"},
		{ID: "billing:custom", GroupID: "scenario", Name: "Opaque custom ID"},
		{ID: "looks-custom", GroupID: "privacy", Name: "Actually builtin group"},
		{ID: "modality:input:image", GroupID: "scenario", Name: "Reassigned builtin ID"},
	} {
		if isBuiltinTag(tag.ID) {
			// Simulate an old corrupt row; current writes now protect builtin identity.
			if err := s.SaveTag(tag); err == nil {
				t.Fatal("moved builtin tag")
			}
			raw, _ := json.Marshal(tag)
			if _, err = s.db.Exec("UPDATE tags SET group_id=?,data=? WHERE id=?", tag.GroupID, string(raw), tag.ID); err != nil {
				t.Fatal(err)
			}
		} else if err := s.SaveTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", Models: []string{"native"}, Tags: []string{"billing:free", "privacy:zdr"}, ModelTags: map[string][]string{"native": {"modality:input:image"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := DownstreamModel{ID: "public", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "native", Enabled: true}},
		Tags: []string{" scenario:daily ", "scenario:daily", "billing:custom", "missing", "looks-custom", "billing:free", "privacy:zdr", "modality:input:image"}, EffectiveTags: []string{"billing:free"}}
	want := []string{"scenario:daily", "billing:custom"}
	assertStored := func(id string) {
		t.Helper()
		var raw string
		if err := s.db.QueryRow("SELECT data FROM downstream_models WHERE id=?", id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var saved DownstreamModel
		if err := json.Unmarshal([]byte(raw), &saved); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(saved.Tags, want) || len(saved.EffectiveTags) != 0 {
			t.Fatalf("invalid tags persisted via %s: %+v", id, saved)
		}
	}
	if err := s.CreateDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	assertStored(m.ID)
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	assertStored(m.ID)
	renamed := m
	renamed.ID = "renamed"
	if err := s.RenameDownstreamModel(m.ID, renamed); err != nil {
		t.Fatal(err)
	}
	assertStored(renamed.ID)
	for _, overwrite := range []bool{false, true} {
		if err := s.ImportDownstreamModelsWithPrices([]DownstreamModel{m}, nil, overwrite); err != nil {
			t.Fatal(err)
		}
		assertStored(m.ID)
	}

	// Legacy rows are not rewritten by storage reads; console projections filter
	// them using the same tag definitions without migrating stored data.
	oldRaw, _ := json.Marshal(m)
	if _, err := s.db.Exec("UPDATE downstream_models SET data=? WHERE id=?", string(oldRaw), m.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListDownstreamModels()
	if err != nil || len(rows) != 1 || !slices.Equal(rows[0].Tags, m.Tags) {
		t.Fatalf("legacy row changed during read = %+v, error %v", rows, err)
	}
	var unchanged string
	if err := s.db.QueryRow("SELECT data FROM downstream_models WHERE id=?", m.ID).Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged != string(oldRaw) {
		t.Fatal("listing rewrote the stored model")
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 || !slices.Equal(channels[0].Tags, c.Tags) || !slices.Equal(channels[0].ModelTags["native"], c.ModelTags["native"]) {
		t.Fatalf("channel tags changed: %+v, error %v", channels, err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestConfigInheritance(t *testing.T) {
	r := ResolveConfig(ModelConfig{Streaming: ptr(false), MaxOutputTokens: ptr(int64(0)), InputModalities: ptr([]string{})})
	if *r.Config.Streaming || *r.Config.MaxOutputTokens != 0 || len(*r.Config.InputModalities) != 0 {
		t.Fatalf("explicit values lost: %+v", r)
	}
	if r.Sources["streaming"] != "上游模型" || r.Sources["max_output_tokens"] != "上游模型" {
		t.Fatal(r.Sources)
	}
	if ResolveConfig(ModelConfig{}).Config.MaxOutputTokens != nil {
		t.Fatal("unknown must remain unset")
	}
	efforts := []string{"low", "medium", "high"}
	r2 := ResolveConfig(ModelConfig{ThinkingEfforts: ptr(efforts)})
	if !slices.Equal(*r2.Config.ThinkingEfforts, efforts) || r2.Sources["thinking_efforts"] != "上游模型" {
		t.Fatalf("thinking efforts inheritance failed: %+v", r2)
	}
}

func TestDownstreamPersistenceAndBindings(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", Models: []string{"real"}, AvailableModels: []string{"disabled"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := DownstreamModel{ID: "public", Enabled: true, Fallback: true, Bindings: []Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	m.Bindings[0].Model = "missing"
	if err := s.SaveDownstreamModel(m); err == nil {
		t.Fatal("accepted missing model")
	}
	m.Bindings[0].Model = "disabled"
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal("disabled models may remain bound", err)
	}
	m.Bindings = append(m.Bindings, m.Bindings[0])
	if err := s.SaveDownstreamModel(m); err == nil {
		t.Fatal("accepted duplicate")
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListDownstreamModels()
	if err != nil || len(rows) != 1 || rows[0].Bindings[0].Model != "disabled" {
		t.Fatalf("%+v %v", rows, err)
	}
	if err := s.DeleteDownstreamModel("public"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDownstreamModel("public"); err != ErrNotFound {
		t.Fatal(err)
	}
}

// The catalogue grows by fetch and shrinks the same way: what the upstream
// returns this time is the whole auto part, so an entry it no longer lists
// disappears instead of accumulating forever. The two things a fetch keeps are
// the owner's manual entries and the channel's enabled models.
func TestCatalogueReplacementRules(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", Models: []string{"enabled-drop", "enabled-keep", "hand"},
		AvailableModels: []string{"enabled-drop", "enabled-keep", "hand", "stale-auto"},
		ManualModels:    []string{"hand"},
		ModelConfigs:    map[string]ModelConfig{"stale-auto": {MaxOutputTokens: ptr(int64(100))}},
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ReplaceCatalogue(c.ID, []string{"enabled-keep", "newly-listed", "hand"})
	if err != nil {
		t.Fatal(err)
	}
	// Rule 3: the auto part is the fetch, so stale-auto is gone; rule 1: the
	// hand-added entry survives even though this fetch did not mention it.
	wantCatalogue := []string{"enabled-keep", "newly-listed", "hand"}
	if !slices.Equal(stored.AvailableModels, wantCatalogue) {
		t.Fatalf("catalogue = %v, want %v", stored.AvailableModels, wantCatalogue)
	}
	// Rule 2: a fetch never deletes an enabled model — the one the upstream
	// dropped stays enabled and routable, it just stops being a catalogue entry.
	if !slices.Equal(stored.Models, c.Models) {
		t.Fatalf("enabled models = %v, want %v", stored.Models, c.Models)
	}
	// A catalogue entry that left takes its per-model config with it, or the row
	// would fail its own validation on the next save.
	if _, ok := stored.ModelConfigs["stale-auto"]; ok {
		t.Fatal("config for a removed model survived")
	}
	reloaded, err := s.ListChannels()
	if err != nil || len(reloaded) != 1 {
		t.Fatal(err)
	}
	if !slices.Equal(reloaded[0].AvailableModels, wantCatalogue) || !slices.Equal(reloaded[0].Models, c.Models) {
		t.Fatalf("replacement did not persist: %+v", reloaded[0])
	}
	// A manual entry that a fetch finally offers itself stays marked manual.
	again, err := s.ReplaceCatalogue(c.ID, []string{"enabled-keep", "newly-listed"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(again.AvailableModels, "hand") {
		t.Fatalf("manual entry dropped by a later fetch: %v", again.AvailableModels)
	}
	// An unknown channel is an error, not a silently created row.
	if _, err := s.ReplaceCatalogue(c.ID+999, []string{"x"}); err != ErrNotFound {
		t.Fatal(err)
	}
}

// Deleting one channel model removes every trace of it — catalogue entry, manual
// marker, enabled entry, config, evidence — and both a catalogue-only entry and
// an enabled one may go: the delete is the operator's decision about their own
// upstream.
func TestDeleteChannelModelRemovesEveryTrace(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	config := ModelConfig{MaxOutputTokens: ptr(int64(64))}
	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", Models: []string{"enabled", "enabled-2"},
		AvailableModels: []string{"enabled", "enabled-2", "catalogue-only", "hand"},
		ManualModels:    []string{"hand"},
		ModelConfigs:    map[string]ModelConfig{"enabled": config, "catalogue-only": config},
		ModelEvidence:   map[string]CapabilityEvidence{"enabled-2": {Source: "manual", APIFormat: FormatOpenAIChatCompletions}},
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteModel(c.ID, "ghost"); err != ErrNotFound {
		t.Fatal("deleting a model the channel does not list")
	}
	if _, err := s.DeleteModel(c.ID+999, "enabled"); err != ErrNotFound {
		t.Fatal("deleting from a channel that does not exist")
	}
	// A catalogue-only entry leaves with its config.
	after, err := s.DeleteModel(c.ID, "catalogue-only")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(after.AvailableModels, "catalogue-only") {
		t.Fatal("catalogue entry survived the delete")
	}
	if _, ok := after.ModelConfigs["catalogue-only"]; ok {
		t.Fatal("config survived the delete")
	}
	// An enabled entry goes from the enabled list too, and carries its config and
	// evidence with it, leaving the row savable.
	after, err = s.DeleteModel(c.ID, "enabled")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(after.Models, "enabled") || slices.Contains(after.AvailableModels, "enabled") {
		t.Fatalf("enabled entry survived: %+v", after)
	}
	if _, ok := after.ModelConfigs["enabled"]; ok {
		t.Fatal("config for an enabled entry survived the delete")
	}
	after, err = s.DeleteModel(c.ID, "enabled-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.ModelEvidence["enabled-2"]; ok {
		t.Fatal("evidence survived the delete")
	}
	// A manual entry can be deleted by hand as well; after that a fetch has
	// nothing left to keep.
	if _, err := s.DeleteModel(c.ID, "hand"); err != nil {
		t.Fatal(err)
	}
	after, err = s.ReplaceCatalogue(c.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.AvailableModels) != 0 || len(after.ManualModels) != 0 {
		t.Fatalf("deleted manual entry resurfaced: %+v", after)
	}
}

// The manual marker is not independent of the catalogue: removing the entry
// removes the marker, and a marker for an entry the channel still lists survives
// a save that keeps the entry.
func TestManualMarkerFollowsItsCatalogueEntry(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", AvailableModels: []string{"hand", "fetched"},
		ManualModels: []string{"hand", "hand", "fetched"},
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.ManualModels, []string{"hand", "fetched"}) {
		t.Fatalf("markers not reconciled: %v", c.ManualModels)
	}
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	if !slices.Equal(rows[0].ManualModels, []string{"hand", "fetched"}) {
		t.Fatalf("markers not persisted: %v", rows[0].ManualModels)
	}
	// An editor that removes the catalogue entry but forgets the marker leaves
	// no phantom: the marker dies with the entry.
	edit := rows[0]
	edit.AvailableModels = []string{"fetched"}
	if err := s.SaveChannel(&edit); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListChannels()
	if !slices.Equal(rows[0].ManualModels, []string{"fetched"}) {
		t.Fatalf("marker outlived its entry: %v", rows[0].ManualModels)
	}
	// A marker for an entry that is not in the catalogue is dropped, not kept.
	edit = rows[0]
	edit.ManualModels = append(slices.Clone(rows[0].ManualModels), "gone")
	if err := s.SaveChannel(&edit); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListChannels()
	if !slices.Equal(rows[0].ManualModels, []string{"fetched"}) {
		t.Fatalf("phantom marker accepted: %v", rows[0].ManualModels)
	}
}

func TestDownstreamModelReorderAndSortOrder(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", Models: []string{"m1", "m2", "m3"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"c-model", "a-model", "b-model"} {
		if err := s.SaveDownstreamModel(DownstreamModel{
			ID:       id,
			Enabled:  true,
			Bindings: []Binding{{ChannelID: c.ID, Model: "m1", Enabled: true}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Default SortOrder is 0; models must be sorted by ID ASC.
	initial, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	wantInitial := []string{"a-model", "b-model", "c-model"}
	gotInitial := []string{initial[0].ID, initial[1].ID, initial[2].ID}
	if !slices.Equal(gotInitial, wantInitial) {
		t.Fatalf("initial order = %v, want %v", gotInitial, wantInitial)
	}

	// Reorder to c, a, b
	if err := s.ReorderDownstreamModels([]string{"c-model", "a-model", "b-model"}); err != nil {
		t.Fatal(err)
	}
	afterReorder, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	wantReordered := []string{"c-model", "a-model", "b-model"}
	gotReordered := []string{afterReorder[0].ID, afterReorder[1].ID, afterReorder[2].ID}
	if !slices.Equal(gotReordered, wantReordered) {
		t.Fatalf("reordered = %v, want %v", gotReordered, wantReordered)
	}
	if afterReorder[0].SortOrder != 1 || afterReorder[1].SortOrder != 2 || afterReorder[2].SortOrder != 3 {
		t.Fatalf("unexpected sort orders: %d, %d, %d", afterReorder[0].SortOrder, afterReorder[1].SortOrder, afterReorder[2].SortOrder)
	}

	// Add a new model with SortOrder 0. It must appear in the 0 segment (first).
	if err := s.SaveDownstreamModel(DownstreamModel{
		ID:       "zero-model",
		Enabled:  true,
		Bindings: []Binding{{ChannelID: c.ID, Model: "m1", Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	withZero, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	wantWithZero := []string{"zero-model", "c-model", "a-model", "b-model"}
	gotWithZero := []string{withZero[0].ID, withZero[1].ID, withZero[2].ID, withZero[3].ID}
	if !slices.Equal(gotWithZero, wantWithZero) {
		t.Fatalf("with zero model = %v, want %v", gotWithZero, wantWithZero)
	}

	// Reorder with a nonexistent ID in the list. Nonexistent IDs should be skipped gracefully.
	if err := s.ReorderDownstreamModels([]string{"b-model", "ghost", "c-model"}); err != nil {
		t.Fatal(err)
	}
	afterPartial, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	// zero-model still has 0, a-model still has 2, b-model has 1, c-model has 3.
	// Order: zero-model (0), b-model (1), a-model (2), c-model (3).
	wantPartial := []string{"zero-model", "b-model", "a-model", "c-model"}
	gotPartial := []string{afterPartial[0].ID, afterPartial[1].ID, afterPartial[2].ID, afterPartial[3].ID}
	if !slices.Equal(gotPartial, wantPartial) {
		t.Fatalf("partial reorder = %v, want %v", gotPartial, wantPartial)
	}
}

func TestDownstreamModelBatchStatus(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", Models: []string{"m1"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		if err := s.SaveDownstreamModel(DownstreamModel{
			ID:       id,
			Enabled:  true,
			Bindings: []Binding{{ChannelID: c.ID, Model: "m1", Enabled: true}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Batch disable m1 and m3
	if err := s.BatchUpdateDownstreamModelsStatus([]string{"m1", "m3", "ghost"}, false); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]bool{}
	for _, m := range rows {
		byID[m.ID] = m.Enabled
	}
	if byID["m1"] != false || byID["m2"] != true || byID["m3"] != false {
		t.Fatalf("unexpected statuses: %v", byID)
	}

	// Batch enable m1
	if err := s.BatchUpdateDownstreamModelsStatus([]string{"m1"}, true); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListDownstreamModels()
	for _, m := range rows {
		byID[m.ID] = m.Enabled
	}
	if byID["m1"] != true || byID["m2"] != true || byID["m3"] != false {
		t.Fatalf("unexpected statuses after re-enabling: %v", byID)
	}
}

func TestDownstreamModelBatchDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", Models: []string{"m1"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		if err := s.SaveDownstreamModel(DownstreamModel{
			ID:       id,
			Enabled:  true,
			Bindings: []Binding{{ChannelID: c.ID, Model: "m1", Enabled: true}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.BatchDeleteDownstreamModels([]string{"m1", "m3", "ghost"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "m2" {
		t.Fatalf("unexpected remaining models: %+v", rows)
	}
}

func TestDownstreamModelImport(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c := Channel{Name: "provider", Protocol: "openai", BaseURL: "http://localhost", APIKey: "test-key", Models: []string{"upstream1", "upstream2"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}

	m1 := DownstreamModel{ID: "m1", DisplayName: "orig 1", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "upstream1", Enabled: true}}}
	m2 := DownstreamModel{ID: "m2", DisplayName: "orig 2", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "upstream1", Enabled: true}}}
	if err := s.SaveDownstreamModel(m1); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(m2); err != nil {
		t.Fatal(err)
	}

	// 1. Merge mode: updates m2, adds m3, preserves m1
	importMerge := []DownstreamModel{
		{ID: "m2", DisplayName: "updated 2", Enabled: false, Bindings: []Binding{{ChannelID: c.ID, Model: "upstream2", Enabled: true}}},
		{ID: "m3", DisplayName: "new 3", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "upstream1", Enabled: true}}},
	}
	if err := s.ImportDownstreamModelsWithPrices(importMerge, nil, false); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 models after merge, got %d", len(rows))
	}
	byID := map[string]DownstreamModel{}
	for _, m := range rows {
		byID[m.ID] = m
	}
	if byID["m1"].DisplayName != "orig 1" {
		t.Errorf("m1 should be preserved: %+v", byID["m1"])
	}
	if byID["m2"].DisplayName != "updated 2" || byID["m2"].Enabled != false {
		t.Errorf("m2 should be updated: %+v", byID["m2"])
	}
	if byID["m3"].DisplayName != "new 3" {
		t.Errorf("m3 should be added: %+v", byID["m3"])
	}

	// 2. Overwrite mode: keeps only imported models (m3 and m4), deletes m1 and m2
	importOverwrite := []DownstreamModel{
		{ID: "m3", DisplayName: "kept 3", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "upstream1", Enabled: true}}},
		{ID: "m4", DisplayName: "new 4", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "upstream2", Enabled: true}}},
	}
	if err := s.ImportDownstreamModelsWithPrices(importOverwrite, nil, true); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 models after overwrite, got %d", len(rows))
	}
	ids := []string{rows[0].ID, rows[1].ID}
	if !slices.Equal(ids, []string{"m3", "m4"}) {
		t.Fatalf("expected m3, m4 after overwrite, got %v", ids)
	}

	// 3. Invalid import must fail and roll back transaction
	invalidImport := []DownstreamModel{
		{ID: "m5", DisplayName: "m5", Enabled: true, Bindings: []Binding{{ChannelID: 99999, Model: "upstream1", Enabled: true}}},
	}
	if err := s.ImportDownstreamModelsWithPrices(invalidImport, nil, false); err == nil {
		t.Fatal("expected error importing model with invalid channel binding")
	}
	rowsAfterFailed, _ := s.ListDownstreamModels()
	if len(rowsAfterFailed) != 2 {
		t.Fatalf("database should remain untouched after failed import: %d", len(rowsAfterFailed))
	}
}
