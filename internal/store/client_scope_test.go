package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestClientScopePersistenceSnapshotsAndEmptyRules(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	channel := Channel{Name: "source", Models: []string{"native"}, APIKey: "synthetic", Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}}
	if err = s.SaveChannel(&channel); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveDownstreamModel(DownstreamModel{ID: "public", Bindings: []Binding{{ChannelID: channel.ID, Model: "native", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	scope := ClientKeyScope{ModelMode: "selected", ModelIDs: []string{"public"}, TagRules: map[string][]string{"billing": {}}}
	key, token, err := s.CreateClientKeyWithScope("scoped", scope)
	if err != nil {
		t.Fatal(err)
	}
	scope.ModelIDs[0] = "mutated"
	scope.TagRules["billing"] = []string{"billing:free"}
	if key.Scope.AllowsTags(map[string][]string{"billing": {"billing:free"}}) {
		t.Fatal("restricted empty tag rule passed")
	}
	if !key.Scope.AllowsModel("public") || key.Scope.AllowsModel("mutated") {
		t.Fatal("scope snapshot shared caller memory")
	}
	authenticated, err := s.AuthenticateClientKey(token)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(authenticated.Scope, key.Scope) {
		t.Fatal("authentication omitted scope")
	}
	authenticated.Scope.ModelIDs[0] = "mutated"
	authenticated.Scope.TagRules["billing"] = []string{"billing:free"}
	keys, err := s.ListClientKeys()
	if err != nil || len(keys) != 1 || !reflect.DeepEqual(keys[0].Scope, key.Scope) {
		t.Fatal("authentication result mutated persisted scope", err)
	}
	empty := ClientKeyScope{ModelMode: "selected", TagRules: map[string][]string{}}
	updated, err := s.UpdateClientKey(key.ID, "renamed", empty)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Scope.AllowsModel("public") || !updated.Scope.AllowsTags(map[string][]string{}) {
		t.Fatal("selected empty confused with unrestricted tags")
	}
	revealed, err := s.RevealClientKey(key.ID)
	if err != nil || revealed != token {
		t.Fatal("editing changed key material", err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.AuthenticateClientKey(token)
	if err != nil || !reflect.DeepEqual(got.Scope, updated.Scope) {
		t.Fatal("scope not durable", err)
	}
	all := DefaultClientKeyScope()
	if !all.AllowsModel("future") || !all.AllowsTags(nil) {
		t.Fatal("all default is restricted")
	}
}

func TestClientScopeRejectsMalformedAndMissingReferences(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, scope := range []ClientKeyScope{{}, {ModelMode: "broken"}, {ModelMode: "all", ModelIDs: []string{"ghost"}}, {ModelMode: "selected", ModelIDs: []string{"ghost"}}, {ModelMode: "all", TagRules: map[string][]string{"modality": {}}}, {ModelMode: "all", TagRules: map[string][]string{"ghost": {}}}, {ModelMode: "all", TagRules: map[string][]string{"billing": {"privacy:zdr"}}}, {ModelMode: "all", TagRules: map[string][]string{"billing": {"billing:missing"}}}} {
		if _, _, err = s.CreateClientKeyWithScope("invalid", scope); err == nil {
			t.Fatalf("accepted malformed scope %+v", scope)
		}
	}
	k, token, err := s.CreateClientKey("legacy internal entry")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `{}`, `{"model_mode":"all"}`, `{"model_mode":"all","model_ids":[],"tag_rules":null}`, `{"model_mode":"broken","model_ids":[],"tag_rules":{}}`, `invalid`} {
		if _, err = s.db.Exec("UPDATE client_keys SET scope=? WHERE id=?", raw, k.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.AuthenticateClientKey(token); err == nil {
			t.Fatalf("authenticated damaged scope %s", raw)
		}
	}
}

func TestClientScopeTagGroupResolutionAndValidation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SaveTagGroup(TagGroup{ID: "custom", Name: "custom"}); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []Tag{{ID: "opaque-a", GroupID: "custom", Name: "a"}, {ID: "billing:looks-like-billing", GroupID: "custom", Name: "b"}} {
		if err = s.SaveTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	tags, _ := s.ListTags()
	groups, _ := s.ListTagGroups()
	channel := Channel{Tags: []string{"billing:plan", "privacy:zdr", "opaque-a"}, ModelTags: map[string][]string{"native": {"privacy:unknown", "opaque-a", "billing:looks-like-billing"}}}
	effective := ResolveModelTags(channel, "native", tags, groups)
	if !reflect.DeepEqual(effective["billing"], []string{"billing:plan"}) || !reflect.DeepEqual(effective["privacy"], []string{"privacy:unknown"}) || len(effective["custom"]) != 2 {
		t.Fatalf("bad per-group defaults %v", effective)
	}
	channel.Tags = []string{"billing:payg", "privacy:train"}
	effective = ResolveModelTags(channel, "native", tags, groups)
	if effective["billing"][0] != "billing:payg" || effective["privacy"][0] != "privacy:unknown" {
		t.Fatal("default change overwrote explicit group", effective)
	}
	channel.ModelTags["native"] = []string{"opaque-a"}
	effective = ResolveModelTags(channel, "native", tags, groups)
	if effective["privacy"][0] != "privacy:train" {
		t.Fatal("restore follow did not use latest default")
	}
	channel.ModelTags["native"] = nil
	channel.Tags = nil
	effective = ResolveModelTags(channel, "native", tags, groups)
	for _, group := range []string{"billing", "privacy", "custom"} {
		if !reflect.DeepEqual(effective[group], []string{UnknownTagID(group)}) {
			t.Fatal("missing default unknown", effective)
		}
	}
	scope := ClientKeyScope{ModelMode: "all", TagRules: map[string][]string{"billing": {"billing:free", "billing:plan"}, "privacy": {"privacy:zdr"}}}
	if !scope.AllowsTags(map[string][]string{"billing": {"billing:plan"}, "privacy": {"privacy:zdr"}}) || scope.AllowsTags(effective) {
		t.Fatal("same group OR / cross group AND broken")
	}
	if !reflect.DeepEqual(scope.MismatchedTagGroups(effective), []string{"billing", "privacy"}) {
		t.Fatal("mismatch groups unstable")
	}
	channel.ModelTags["native"] = []string{"ghost"}
	if scope.AllowsTags(ResolveModelTags(channel, "native", tags, groups)) {
		t.Fatal("invalid tag granted access")
	}
	c := Channel{Name: "validation", Models: []string{"native"}, APIKey: "synthetic", Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}}
	for _, ids := range [][]string{{"billing:free", "billing:plan"}, {"privacy:zdr", "privacy:train"}, {"custom:unknown", "opaque-a"}, {"missing"}} {
		c.ModelTags = map[string][]string{"native": ids}
		if err = s.SaveChannel(&c); err == nil {
			t.Fatalf("accepted invalid object tags %v", ids)
		}
	}
	c.ModelTags = map[string][]string{"native": {"opaque-a", "billing:looks-like-billing"}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal("custom multi select rejected", err)
	}
}

func TestClientScopeReferencesProtectTagsAndPublicModelTransactions(t *testing.T) {
	s, c, m := operationFixture(t)
	scope := ClientKeyScope{ModelMode: "selected", ModelIDs: []string{m.ID}, TagRules: map[string][]string{"scenario": {"scenario:original"}}}
	key, token, err := s.CreateClientKeyWithScope("restricted", scope)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := s.GetTagReferences("scenario:original")
	if err != nil || !slices.Contains(refs.ClientKeyNames, "restricted") {
		t.Fatal("missing key references", err)
	}
	if err = s.DeleteTag("scenario:original", true); err == nil {
		t.Fatal("force deletion widened key permissions")
	}
	if err = s.SaveTagGroup(TagGroup{ID: "other", Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTag(Tag{ID: "scenario:original", GroupID: "other", Name: "moved"}); err == nil {
		t.Fatal("moved referenced tag")
	}
	if err = s.DeleteTag("other:unknown", true); err == nil {
		t.Fatal("deleted protected unknown")
	}
	if err = s.SaveTag(Tag{ID: "other:unknown", GroupID: "scenario", Name: "moved"}); err == nil {
		t.Fatal("moved protected unknown")
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_key_scope BEFORE UPDATE OF scope ON client_keys BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	m.ID = "new"
	if err = s.RenameDownstreamModel("old", m); err == nil {
		t.Fatal("rename ignored failed key reference update")
	}
	models, _ := s.ListDownstreamModels()
	if len(models) != 1 || models[0].ID != "old" {
		t.Fatal("failed rename was not atomic")
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_key_scope"); err != nil {
		t.Fatal(err)
	}
	if err = s.RenameDownstreamModel("old", m); err != nil {
		t.Fatal(err)
	}
	got, err := s.AuthenticateClientKey(token)
	if err != nil || got.ID != key.ID || !got.Scope.AllowsModel("new") || got.Scope.AllowsModel("old") {
		t.Fatal("rename omitted key scope", err)
	}
	if err = s.DeleteDownstreamModel("new"); err != nil {
		t.Fatal(err)
	}
	got, err = s.AuthenticateClientKey(token)
	if err != nil || got.Scope.ModelMode != "selected" || len(got.Scope.ModelIDs) != 0 || got.Scope.AllowsModel("future") {
		t.Fatal("deletion widened selected scope", err)
	}
	// Last independent tag deletion must not resume following channel defaults.
	c.Tags = []string{"scenario:edited"}
	c.ModelTags = map[string][]string{"native": {"other:unknown"}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTag(Tag{ID: "custom-opaque", GroupID: "other", Name: "temporary"}); err != nil {
		t.Fatal(err)
	}
	c.ModelTags["native"] = []string{"custom-opaque"}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteTag("custom-opaque", true); err != nil {
		t.Fatal(err)
	}
	c, err = s.Channel(c.ID)
	if err != nil || !reflect.DeepEqual(c.ModelTags["native"], []string{"other:unknown"}) {
		t.Fatal("deletion reset model group to follow", err)
	}
}

func TestTagScopesStartupBackupAndImportConversionPreserveOtherFields(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := Channel{Name: "legacy", Models: []string{"native"}, APIKey: "synthetic-secret", Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}, Tags: []string{"billing:free"}, ModelTags: map[string][]string{"native": {"privacy:zdr"}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	key, token, err := s.CreateClientKeyWithScope("backup", ClientKeyScope{ModelMode: "selected", TagRules: map[string][]string{"billing": {}}})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = s.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	fields["tags"] = json.RawMessage(`["billing:free","billing:plan"]`)
	fields["model_tags"] = json.RawMessage(`{"native":["privacy:zdr","privacy:unknown"]}`)
	fields["extension"] = json.RawMessage(`{"keep":42}`)
	original, _ := json.Marshal(fields)
	if _, err = s.db.Exec("UPDATE channels SET data=? WHERE id=?", string(original), c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DELETE FROM meta WHERE key='tag_scopes_v1'"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Channel(c.ID)
	if err != nil || got.PrimaryKey() != c.PrimaryKey() || !reflect.DeepEqual(got.Tags, []string{"billing:unknown"}) || !reflect.DeepEqual(got.ModelTags["native"], []string{"privacy:unknown"}) {
		t.Fatal("conversion lost fields or guessed a tag", err)
	}
	var migrated string
	if err = s.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	json.Unmarshal([]byte(migrated), &after)
	delete(fields, "tags")
	delete(fields, "model_tags")
	delete(after, "tags")
	delete(after, "model_tags")
	if !reflect.DeepEqual(fields, after) {
		t.Fatal("conversion rewrote unrelated raw fields")
	}
	report, err := s.TagScopeMigrationSummary()
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Backup  string   `json:"backup"`
		Changes []string `json:"changes"`
	}
	if err = json.Unmarshal(report, &summary); err != nil || len(summary.Changes) != 2 {
		t.Fatal("missing conversion report", err)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(summary.Backup), "master.key")); err != nil {
		t.Fatal("backup omitted master key", err)
	}
	backup, err := sql.Open("sqlite", summary.Backup)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var saved string
	if err = backup.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&saved); err != nil || saved != string(original) {
		t.Fatal("backup does not preserve pre-conversion source", err)
	}
	gotKey, err := s.AuthenticateClientKey(token)
	if err != nil || !reflect.DeepEqual(gotKey.Scope, key.Scope) {
		t.Fatal("migration changed key scope", err)
	}
	imported := Channel{Tags: []string{"billing:free", "billing:payg"}, ModelTags: map[string][]string{"native": {"privacy:zdr", "privacy:train"}}}
	changes, err := s.NormalizeImportedChannelTags(&imported)
	if err != nil || len(changes) != 2 || imported.Tags[0] != "billing:unknown" || imported.ModelTags["native"][0] != "privacy:unknown" {
		t.Fatal("import boundary conversion failed", err)
	}
	changes, err = s.NormalizeImportedChannelTags(&imported)
	if err != nil || len(changes) != 0 {
		t.Fatal("conversion not idempotent", err)
	}
	if !errors.Is(s.DeleteDownstreamModel("missing"), ErrNotFound) {
		t.Fatal("delete missing contract changed")
	}
}

func TestPublicModelUnknownValidationAndImportBackup(t *testing.T) {
	s, _, m := operationFixture(t)
	key, token, err := s.CreateClientKeyWithScope("keep scope", ClientKeyScope{ModelMode: "selected", ModelIDs: []string{m.ID}, TagRules: map[string][]string{"scenario": {}}})
	if err != nil {
		t.Fatal(err)
	}
	m.Tags = []string{"scenario:unknown", "scenario:original"}
	if err = s.SaveDownstreamModel(m); err == nil {
		t.Fatal("public model accepted unknown plus specific")
	}
	if err = s.ImportDownstreamModelsWithPrices([]DownstreamModel{m}, nil, true); err != nil {
		t.Fatal(err)
	}
	models, err := s.ListDownstreamModels()
	if err != nil || len(models) != 1 || !reflect.DeepEqual(models[0].Tags, []string{"scenario:unknown"}) {
		t.Fatal("import did not normalize ambiguous custom facts", err)
	}
	got, err := s.AuthenticateClientKey(token)
	if err != nil || !reflect.DeepEqual(got.Scope, key.Scope) {
		t.Fatal("import changed scope", err)
	}
	var dbPath string
	if err = s.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&dbPath); err != nil {
		t.Fatal(err)
	}
	sources, err := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "backups", "tag-scope-import-*", "source.json"))
	if err != nil || len(sources) != 1 {
		t.Fatal("missing import source backup", err)
	}
	raw, err := os.ReadFile(sources[0])
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Models  []DownstreamModel `json:"models"`
		Changes []string          `json:"changes"`
	}
	if err = json.Unmarshal(raw, &source); err != nil || len(source.Changes) != 1 || !reflect.DeepEqual(source.Models[0].Tags, m.Tags) {
		t.Fatal("source backup lost original ambiguity", err)
	}
	// A later startup conversion also covers existing public-model rows.
	if _, err = s.db.Exec("UPDATE downstream_models SET data=? WHERE id=?", string(mustJSON(t, m)), m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DELETE FROM meta WHERE key='tag_scopes_v1'"); err != nil {
		t.Fatal(err)
	}
	if err = s.upgradeTagScopes(filepath.Dir(dbPath)); err != nil {
		t.Fatal(err)
	}
	models, err = s.ListDownstreamModels()
	if err != nil || !reflect.DeepEqual(models[0].Tags, []string{"scenario:unknown"}) {
		t.Fatal("startup missed public model conflicts", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUnknownGroupsAtomicLifecycleAndEmptyKeyReference(t *testing.T) {
	s, c, _ := operationFixture(t)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_unknown BEFORE INSERT ON tags WHEN NEW.id='atomic:unknown' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTagGroup(TagGroup{ID: "atomic", Name: "atomic"}); err == nil {
		t.Fatal("ignored unknown creation failure")
	}
	groups, err := s.ListTagGroups()
	if err != nil || slices.ContainsFunc(groups, func(g TagGroup) bool { return g.ID == "atomic" }) {
		t.Fatal("group persisted without protected unknown", err)
	}
	if err = s.SaveTagGroup(TagGroup{ID: "empty", Name: "empty"}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteTagGroup("empty"); err != nil {
		t.Fatal("unknown alone prevents group deletion", err)
	}
	if err = s.SaveTagGroup(TagGroup{ID: "restricted", Name: "restricted"}); err != nil {
		t.Fatal(err)
	}
	k, _, err := s.CreateClientKeyWithScope("empty group restriction", ClientKeyScope{ModelMode: "all", TagRules: map[string][]string{"restricted": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteTagGroup("restricted"); err == nil {
		t.Fatal("deleted empty key tag restriction")
	}
	if err = s.DeleteClientKey(k.ID); err != nil {
		t.Fatal(err)
	}
	c.ModelTags = map[string][]string{"native": {"restricted:unknown"}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteTagGroup("restricted"); err != nil {
		t.Fatal("unreferenced authorization group could not be removed", err)
	}
	saved, err := s.Channel(c.ID)
	if err != nil || slices.Contains(saved.ModelTags["native"], "restricted:unknown") {
		t.Fatal("group deletion left model unknown reference", err)
	}
	if !errors.Is(s.DeleteTagGroup("missing"), ErrNotFound) {
		t.Fatal("missing tag group contract changed")
	}
}

func TestClientScopeAndTagDeletionSerialize(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SaveTagGroup(TagGroup{ID: "race", Name: "race"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTag(Tag{ID: "race:tag", GroupID: "race", Name: "race"}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	created := make(chan error, 1)
	deleted := make(chan error, 1)
	go func() {
		<-start
		_, _, e := s.CreateClientKeyWithScope("racing key", ClientKeyScope{ModelMode: "all", TagRules: map[string][]string{"race": {"race:tag"}}})
		created <- e
	}()
	go func() { <-start; deleted <- s.DeleteTag("race:tag", true) }()
	close(start)
	createErr, deleteErr := <-created, <-deleted
	if createErr == nil && deleteErr == nil {
		t.Fatal("concurrent writes stored dangling key scope")
	}
	keys, err := s.ListClientKeys()
	if err != nil {
		t.Fatal(err)
	}
	tags, err := s.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) > 0 && !slices.ContainsFunc(tags, func(tag Tag) bool { return tag.ID == "race:tag" }) {
		t.Fatal("key reference passed concurrent tag deletion")
	}
}

func TestMoveModelOverrideRetainsIndependentUnknownAndIsAtomic(t *testing.T) {
	s, c, m := operationFixture(t)
	if err := s.SaveTagGroup(TagGroup{ID: "target", Name: "target"}); err != nil {
		t.Fatal(err)
	}
	c.Tags = []string{"scenario:edited"}
	c.ModelTags = map[string][]string{"native": {"scenario:original"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	tags, _ := s.ListTags()
	groups, _ := s.ListTagGroups()
	before := ResolveModelTags(c, "native", tags, groups)
	scope := ClientKeyScope{ModelMode: "all", TagRules: map[string][]string{"scenario": {"scenario:edited"}}}
	if scope.AllowsTags(before) {
		t.Fatal("fixture should exclude model override")
	}
	moved := Tag{ID: "scenario:original", GroupID: "target", Name: "moved"}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_move BEFORE UPDATE ON channels BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTag(moved); err == nil {
		t.Fatal("move ignored failed model override write")
	}
	tags, _ = s.ListTags()
	for _, tag := range tags {
		if tag.ID == moved.ID && tag.GroupID != "scenario" {
			t.Fatal("failed move partly changed identity")
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_move"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTag(moved); err != nil {
		t.Fatal("unrestricted public/model reference blocked group move", err)
	}
	saved, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(saved.ModelTags["native"], "scenario:unknown") || !slices.Contains(saved.ModelTags["native"], moved.ID) {
		t.Fatal("move discarded override independence")
	}
	tags, _ = s.ListTags()
	effective := ResolveModelTags(saved, "native", tags, groups)
	if scope.AllowsTags(effective) || !reflect.DeepEqual(effective["scenario"], []string{"scenario:unknown"}) {
		t.Fatal("move silently resumed channel defaults", effective)
	}
	if saved.PrimaryKey() != c.PrimaryKey() || saved.Generation != c.Generation {
		t.Fatal("move rewrote credential or continuation generation")
	}
	models, _ := s.ListDownstreamModels()
	if len(models) != 1 || !reflect.DeepEqual(models[0], m) {
		t.Fatal("move rewrote public model facts")
	}
	// Moving into a single-select group must reject conflicting new facts.
	saved.ModelTags["native"] = append(saved.ModelTags["native"], "billing:plan")
	if err = s.SaveChannel(&saved); err != nil {
		t.Fatal(err)
	}
	moved.GroupID = "billing"
	if err = s.SaveTag(moved); err == nil {
		t.Fatal("group move introduced contradictory billing facts")
	}
}

func TestOldClientKeyColumnUpgradePreservesIdentity(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, token, err := s.CreateClientKey("old identity")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("ALTER TABLE client_keys DROP COLUMN scope"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.AuthenticateClientKey(token)
	if err != nil || got.ID != key.ID || !reflect.DeepEqual(got.Scope, DefaultClientKeyScope()) {
		t.Fatal("old key did not retain unrestricted identity", err)
	}
	revealed, err := s.RevealClientKey(key.ID)
	if err != nil || revealed != token {
		t.Fatal("old key sealed secret changed", err)
	}
}

func TestOverwriteModelImportRetainsOnlySurvivingSelectedReferences(t *testing.T) {
	s, _, a := operationFixture(t)
	b := a
	b.ID = "retained"
	if err := s.CreateDownstreamModel(b); err != nil {
		t.Fatal(err)
	}
	mixed, _, err := s.CreateClientKeyWithScope("mixed", ClientKeyScope{ModelMode: "selected", ModelIDs: []string{a.ID, b.ID}, TagRules: map[string][]string{"billing": {}}})
	if err != nil {
		t.Fatal(err)
	}
	removed, _, err := s.CreateClientKeyWithScope("removed", ClientKeyScope{ModelMode: "selected", ModelIDs: []string{a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	all, _, err := s.CreateClientKey("all")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ImportDownstreamModelsWithPrices([]DownstreamModel{b}, nil, true); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListClientKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		switch key.ID {
		case mixed.ID:
			if !reflect.DeepEqual(key.Scope.ModelIDs, []string{b.ID}) || key.Scope.ModelMode != "selected" || !reflect.DeepEqual(key.Scope.TagRules, mixed.Scope.TagRules) {
				t.Fatal("overwrite did not retain exactly the surviving selection", key.Scope)
			}
		case removed.ID:
			if key.Scope.ModelMode != "selected" || len(key.Scope.ModelIDs) != 0 || key.Scope.AllowsModel(b.ID) {
				t.Fatal("removed selection became unrestricted", key.Scope)
			}
		case all.ID:
			if !reflect.DeepEqual(key.Scope, all.Scope) {
				t.Fatal("overwrite changed all-model permission")
			}
		}
		if _, err = s.ValidateClientKeyScope(key.Scope); err != nil {
			t.Fatal("overwrite left uneditable scope", err)
		}
	}
	if err = s.CreateDownstreamModel(a); err != nil {
		t.Fatal(err)
	}
	keys, err = s.ListClientKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.ID != all.ID && key.Scope.AllowsModel(a.ID) {
			t.Fatal("recreating overwritten public ID restored deleted permission")
		}
	}
}

func TestOverwriteModelImportPriceFailureRollsBackSelectedReferences(t *testing.T) {
	s, _, a := operationFixture(t)
	b := a
	b.ID = "retained"
	if err := s.CreateDownstreamModel(b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateClientKeyWithScope("mixed", ClientKeyScope{ModelMode: "selected", ModelIDs: []string{a.ID, b.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateClientKeyWithScope("removed", ClientKeyScope{ModelMode: "selected", ModelIDs: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	before := operationSnapshot(t, s)
	beforeKeys, err := s.ListClientKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_import_price BEFORE UPDATE ON settings BEGIN SELECT RAISE(ABORT,'injected price write'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportDownstreamModelsWithPrices([]DownstreamModel{b}, []Price{operationPrice(b.ID, 0)}, true); err == nil {
		t.Fatal("import ignored failed price write")
	}
	afterKeys, err := s.ListClientKeys()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeKeys, afterKeys) || !reflect.DeepEqual(before, operationSnapshot(t, s)) {
		t.Fatal("failed import partly changed models, prices or Key selections")
	}
}

func TestDeleteUnknownOnlyGroupCleansAllObjectsAtomically(t *testing.T) {
	for _, stage := range []string{"channel", "public model", "unknown", "group", "success"} {
		t.Run(stage, func(t *testing.T) {
			s, c, m := operationFixture(t)
			if err := s.SaveTagGroup(TagGroup{ID: "retired", Name: "retired"}); err != nil {
				t.Fatal(err)
			}
			c.Tags = []string{"retired:unknown", "scenario:edited"}
			c.ModelTags = map[string][]string{"native": {"retired:unknown", "scenario:original"}, "other": {"retired:unknown"}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			c2 := c
			c2.ID = 0
			c2.Name = "second channel"
			if err := s.SaveChannel(&c2); err != nil {
				t.Fatal(err)
			}
			m.Tags = []string{"retired:unknown", "scenario:original"}
			if err := s.SaveDownstreamModel(m); err != nil {
				t.Fatal(err)
			}
			m2 := m
			m2.ID = "second-public"
			if err := s.CreateDownstreamModel(m2); err != nil {
				t.Fatal(err)
			}
			before := operationSnapshot(t, s)
			beforeGroups, err := s.ListTagGroups()
			if err != nil {
				t.Fatal(err)
			}
			trigger := map[string]string{
				"channel":      `CREATE TRIGGER fail_group_cleanup BEFORE UPDATE ON channels WHEN NEW.id=2 BEGIN SELECT RAISE(ABORT,'injected channel write'); END`,
				"public model": `CREATE TRIGGER fail_group_cleanup BEFORE UPDATE ON downstream_models BEGIN SELECT RAISE(ABORT,'injected model write'); END`,
				"unknown":      `CREATE TRIGGER fail_group_cleanup BEFORE DELETE ON tags WHEN OLD.id='retired:unknown' BEGIN SELECT RAISE(ABORT,'injected tag delete'); END`,
				"group":        `CREATE TRIGGER fail_group_cleanup BEFORE DELETE ON tag_groups WHEN OLD.id='retired' BEGIN SELECT RAISE(ABORT,'injected group delete'); END`,
			}[stage]
			if trigger != "" {
				if _, err = s.db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			err = s.DeleteTagGroup("retired")
			if stage != "success" {
				if err == nil || !strings.Contains(err.Error(), "injected") {
					t.Fatal("group deletion did not reach injected failure", err)
				}
				afterGroups, e := s.ListTagGroups()
				if e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(before, operationSnapshot(t, s)) || !reflect.DeepEqual(beforeGroups, afterGroups) {
					t.Fatal("failed group deletion partly changed tags, group or objects")
				}
				return
			}
			if err != nil {
				t.Fatal("unknown references prevented complete group deletion", err)
			}
			channels, err := s.ListChannels()
			if err != nil {
				t.Fatal(err)
			}
			if len(channels) != 2 {
				t.Fatal("group cleanup lost channels")
			}
			for _, saved := range channels {
				expected := c
				if saved.ID == c2.ID {
					expected = c2
				}
				expected.Tags = []string{"scenario:edited"}
				expected.ModelTags = map[string][]string{"native": {"scenario:original"}, "other": {}}
				if !reflect.DeepEqual(saved, expected) {
					t.Fatal("group deletion changed unrelated channel fields")
				}
			}
			models, err := s.ListDownstreamModels()
			if err != nil || len(models) != 2 {
				t.Fatal("group cleanup lost models", err)
			}
			for _, saved := range models {
				expected := m
				if saved.ID == m2.ID {
					expected = m2
				}
				expected.Tags = []string{"scenario:original"}
				if !reflect.DeepEqual(saved, expected) {
					t.Fatal("group cleanup changed unrelated public model fields")
				}
			}
			groups, err := s.ListTagGroups()
			if err != nil || slices.ContainsFunc(groups, func(g TagGroup) bool { return g.ID == "retired" }) {
				t.Fatal("group not removed", err)
			}
			tags, err := s.ListTags()
			if err != nil || slices.ContainsFunc(tags, func(tag Tag) bool { return tag.ID == "retired:unknown" }) {
				t.Fatal("unknown not removed", err)
			}
		})
	}
}
