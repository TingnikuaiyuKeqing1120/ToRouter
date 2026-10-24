package keydiscovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/store"
)

type serviceEpochs struct {
	mu      sync.Mutex
	epoch   map[string]uint64
	enabled map[string]bool
}

func newServiceEpochs() *serviceEpochs {
	f := &serviceEpochs{epoch: map[string]uint64{}, enabled: map[string]bool{}}
	for _, source := range Sources() {
		f.epoch[source.ID] = 1
		f.enabled[source.ID] = true
	}
	return f
}

func (f *serviceEpochs) SourceEpoch(id string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enabled[id] {
		return 0, errors.New("source disabled")
	}
	return f.epoch[id], nil
}

func (f *serviceEpochs) CheckSourceEpoch(id string, epoch uint64) error {
	current, err := f.SourceEpoch(id)
	if err != nil || current != epoch {
		return errors.New("source epoch changed")
	}
	return nil
}

func (f *serviceEpochs) transition(id string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epoch[id]++
	f.enabled[id] = enabled
}

type serviceFixture struct {
	s         *Service
	st        *store.Store
	epochs    *serviceEpochs
	roots     map[string]string
	storeRoot string
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	epochs := newServiceEpochs()
	f := &serviceFixture{s: New(st, epochs, "http://127.0.0.1:19876"), st: st, epochs: epochs, storeRoot: root, roots: map[string]string{}}
	f.s.rootFor = func(id string) (string, error) {
		if root := f.roots[id]; root != "" {
			return root, nil
		}
		return filepath.Join(root, "missing", id), nil
	}
	t.Cleanup(func() {
		f.s.mu.Lock()
		ids := map[string]string{}
		for id, p := range f.s.sessions {
			ids[id] = p.owner
		}
		f.s.mu.Unlock()
		for id, owner := range ids {
			f.s.Cancel(owner, id)
		}
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func serviceWrite(t *testing.T, root, file string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, file), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *serviceFixture) source(t *testing.T, id string, files map[string]any) string {
	t.Helper()
	root := t.TempDir()
	for name, value := range files {
		serviceWrite(t, root, name, value)
	}
	f.roots[id] = root
	return root
}

func serviceMagpie(keys ...map[string]any) map[string]any {
	return map[string]any{"providers": keys}
}

func serviceMagpieKey(key, url, protocol string) map[string]any {
	return map[string]any{"key": key, "keyProtocol": protocol, protocol: url}
}

func (f *serviceFixture) scan(t *testing.T, owner string, ids ...string) Preview {
	t.Helper()
	preview, err := f.s.Begin(context.Background(), owner, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		preview, err = f.s.Scan(context.Background(), owner, preview.ID, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	return preview
}

func serviceNewRows(p Preview) []Row {
	var rows []Row
	for _, g := range p.Groups {
		for _, r := range g.Rows {
			if r.Status == "new" {
				rows = append(rows, r)
			}
		}
	}
	return rows
}

func serviceFindRow(t *testing.T, p Preview, key string) Row {
	t.Helper()
	var found []Row
	for _, g := range p.Groups {
		for _, r := range g.Rows {
			if r.MaskedKey == store.MaskKey(key) {
				found = append(found, r)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("fixture expected exactly one matching public mask, found %d", len(found))
	}
	return found[0]
}

func serviceSaveChannel(t *testing.T, st *store.Store, c store.Channel) store.Channel {
	t.Helper()
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func serviceChannel(t *testing.T, st *store.Store, id int64) store.Channel {
	t.Helper()
	c, err := st.Channel(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func serviceAssertPrivate(t *testing.T, v any, secrets ...string) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		if strings.Contains(string(data), secret) || strings.Contains(string(data), string(encoded[1:len(encoded)-1])) {
			t.Fatal("public response contains private source material")
		}
	}
}

// Triggers observe real persistent writes; no production persistence hook or
// replacement Store is needed to prove one save per selected destination.
func (f *serviceFixture) auditWrites(t *testing.T) func() int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(f.storeRoot, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		"CREATE TABLE discovery_test_write_audit (channel_id INTEGER)",
		"CREATE TRIGGER discovery_test_insert AFTER INSERT ON channels BEGIN INSERT INTO discovery_test_write_audit VALUES (NEW.id); END",
		"CREATE TRIGGER discovery_test_update AFTER UPDATE ON channels BEGIN INSERT INTO discovery_test_write_audit VALUES (NEW.id); END",
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return func() int {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM discovery_test_write_audit").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
}

func TestServiceSourcesNeedNoDirectoryAuthorization(t *testing.T) {
	f := newServiceFixture(t)
	statuses, err := f.s.Sources()
	if err != nil || len(statuses) != 8 {
		t.Fatal("source inventory failed")
	}
	for _, source := range statuses {
		if !source.Enabled {
			t.Fatal("source unexpectedly disabled")
		}
	}
	raw, _ := json.Marshal(statuses)
	for _, field := range []string{"authorized", "files", "default_hint", "root"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatal("source exposes obsolete authorization or path")
		}
	}
	preview, err := f.s.Begin(context.Background(), "owner", []string{"source-magpie"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err = f.s.Scan(context.Background(), "owner", preview.ID, "source-magpie")
	if err != nil || len(preview.Groups) != 0 || preview.Sources[0].Status != "complete" {
		t.Fatal("absent application is not empty result")
	}
}

func TestServicePreviewMergesCompleteKeysAndKeepsMaskCollisionsDistinct(t *testing.T) {
	f := newServiceFixture(t)
	const address = "https://api.example.invalid/v1"
	shared, first, second := "syn-shared-private-tail", "syn-first-private-tail", "syn-second-private-tail"
	if store.MaskKey(shared) != store.MaskKey(first) || store.MaskKey(first) != store.MaskKey(second) {
		t.Fatal("fixtures must have identical masks")
	}
	serviceSaveChannel(t, f.st, store.Channel{Name: "Existing", Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: address}}, Credentials: []store.CredentialItem{{ID: "retained", APIKey: shared, Enabled: true}}})
	claudeRoot := f.source(t, "source-claude", map[string]any{"settings.json": map[string]any{"env": map[string]string{"ANTHROPIC_API_KEY": shared, "ANTHROPIC_BASE_URL": address}}})
	magpieRoot := f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(map[string]any{"name": "private source account label", "key": shared, "anthropic": address, "keys": []map[string]any{{"key": first}, {"key": second}}})})
	p := f.scan(t, "owner", "source-claude", "source-magpie")
	if len(p.Groups) != 1 || len(p.Groups[0].Rows) != 2 {
		t.Fatalf("full-key grouping produced %d groups", len(p.Groups))
	}
	if len(serviceNewRows(p)) != 2 {
		t.Fatal("existing credential visible or masked collision lost")
	}
	for _, row := range p.Groups[0].Rows {
		if row.Status != "new" {
			t.Fatal("existing row visible")
		}
	}
	serviceAssertPrivate(t, p, shared, first, second, claudeRoot, magpieRoot)
	count := f.auditWrites(t)
	if _, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: "not-visible-existing"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("existing row imported: %v", err)
	}
	if count() != 0 {
		t.Fatal("duplicate import wrote the channel")
	}
}

func TestServiceGlobalDiscoveryCreatesDespiteMultipleURLTargets(t *testing.T) {
	f := newServiceFixture(t)
	const address = "https://protocol.example.invalid/v1"
	for _, format := range []string{store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses} {
		serviceSaveChannel(t, f.st, store.Channel{Name: format, Enabled: true, APIKey: "old-" + format, Endpoints: []store.Endpoint{{Format: format, BaseURL: address}}})
	}
	key := "synthetic-new-copy-key"
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey(key, address, "chat"))})
	p := f.scan(t, "owner", "source-magpie")
	row := serviceFindRow(t, p, key)
	if len(p.Groups) != 1 || p.Groups[0].TargetCount != 0 || p.Groups[0].CopyOnly || len(row.Targets) != 0 {
		t.Fatal("URL targets incorrectly restricted by protocol")
	}
	count := f.auditWrites(t)
	copied, err := f.s.Copy(context.Background(), "owner", p.ID, []string{row.ID})
	if err != nil || len(copied.Items) != 1 || copied.Items[0].APIKey != key || copied.Items[0].URL != address {
		t.Fatal("explicit copy lost material")
	}
	created, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID}})
	if err != nil || created.Results[0].Added != 1 {
		t.Fatal("global discovery did not create new channel", err)
	}
	channels, _ := f.st.ListChannels()
	if len(channels) != 3 {
		t.Fatal("global discovery reused existing URL owner")
	}
	if count() != 1 {
		t.Fatal("copy or import changed unexpected persistence")
	}
}

func TestServiceNewImportBatchesByDestinationAndReplayDoesNotWrite(t *testing.T) {
	f := newServiceFixture(t)
	first, second, third := "new-first-private-1111", "new-second-private-2222", "res-third-private-3333"
	root := f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(
		map[string]any{"key": first, "chat": "https://new.example.invalid/v1", "keys": []map[string]any{{"key": second}}},
		serviceMagpieKey(third, "https://other.example.invalid/v1", "responses"),
	)})
	p := f.scan(t, "owner", "source-magpie")
	items := []Selection{}
	for _, row := range serviceNewRows(p) {
		items = append(items, Selection{CandidateID: row.ID, Name: "Selected import"})
	}
	if len(items) != 3 {
		t.Fatal("source keys missing")
	}
	count := f.auditWrites(t)
	response, err := f.s.Import(context.Background(), "owner", p.ID, items)
	if err != nil || response.OperationStatus() != "completed" || len(response.Results) != 2 || count() != 2 {
		t.Fatalf("batch import: %+v %v", response.Results, err)
	}
	channels, err := f.st.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 {
		t.Fatalf("created %d channels instead of one per endpoint set", len(channels))
	}
	keys := map[string]bool{}
	for _, channel := range channels {
		if channel.Name != "Selected import" || !channel.Enabled || len(channel.Models) != 0 {
			t.Fatal("new channel gained unselected settings or routes")
		}
		for _, credential := range channel.Credentials {
			keys[credential.APIKey] = true
			if !credential.Enabled {
				t.Fatal("new credential disabled")
			}
		}
	}
	if len(keys) != 3 || !keys[first] || !keys[second] || !keys[third] {
		t.Fatal("new import lost complete key values")
	}
	models, err := f.st.ListDownstreamModels()
	if err != nil || len(models) != 0 {
		t.Fatal("import created downstream routing")
	}
	if len(serviceNewRows(response.Preview)) != 0 {
		t.Fatal("imported rows not marked existing")
	}
	serviceAssertPrivate(t, response, first, second, third, root)
	if replay, err := f.s.Import(context.Background(), "owner", p.ID, items); err != nil || len(replay.Results) != 2 {
		t.Fatalf("replay should return completed receipt: %v", err)
	}
	if count() != 2 {
		t.Fatal("replay performed a persistent write")
	}
}

func TestServiceAppendManyKeysSavesOnceAndPreservesConfiguration(t *testing.T) {
	f := newServiceFixture(t)
	const address = "https://append.example.invalid/v1"
	limit := int64(4096)
	before := serviceSaveChannel(t, f.st, store.Channel{Name: "Keep channel name", Enabled: false, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: address}},
		Credentials: []store.CredentialItem{{ID: "old-disabled", Label: "Keep disabled label", APIKey: "old-disabled-private", Enabled: false}, {ID: "old-enabled", Label: "Keep enabled label", APIKey: "old-enabled-private", Enabled: true}},
		Models:      []string{"kept-model"}, AvailableModels: []string{"kept-model", "catalog-model"}, ManualModels: []string{"kept-model"}, Tags: []string{"privacy:zdr"}, ModelTags: map[string][]string{"kept-model": {"billing:payg"}},
		ModelConfigs: map[string]store.ModelConfig{"kept-model": {MaxOutputTokens: &limit}}, ModelFormats: map[string][]string{"kept-model": {store.FormatOpenAIChatCompletions}, "catalog-model": {store.FormatOpenAIChatCompletions}},
		DispatchMode: store.DispatchModeRoundRobin, ChainType: store.ChainTypeCyclic, PreferredEndpoint: store.FormatOpenAIChatCompletions})
	route := store.DownstreamModel{ID: "retained-route", DisplayName: "Retained", Enabled: true, Bindings: []store.Binding{{ChannelID: before.ID, Model: "kept-model", Priority: 3, Enabled: true}}}
	if err := f.st.SaveDownstreamModel(route); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SavePrices([]store.Price{{ChannelID: before.ID, Model: "kept-model", Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "USD", UnitTokens: 1000000}}}}); err != nil {
		t.Fatal(err)
	}
	prices, err := f.st.GetPrices()
	if err != nil {
		t.Fatal(err)
	}
	routes, err := f.st.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(map[string]any{"key": "add-first-private-1111", "chat": address, "keys": []map[string]any{{"key": "add-second-private-2222"}}})})
	p := f.scanTarget(t, "owner", before, "source-magpie")
	items := []Selection{}
	for _, row := range serviceNewRows(p) {
		items = append(items, Selection{CandidateID: row.ID, TargetChannelID: before.ID, ExpectedVersion: store.EntityVersion(before), Name: "Must not rename"})
	}
	if len(items) != 2 {
		t.Fatal("expected two new keys")
	}
	count := f.auditWrites(t)
	response, err := f.s.Import(context.Background(), "owner", p.ID, items)
	if err != nil || len(response.Results) != 1 || response.Results[0].Added != 2 || len(response.Results[0].CandidateIDs) != 2 || count() != 1 {
		t.Fatalf("append batching: %+v %v", response.Results, err)
	}
	after := serviceChannel(t, f.st, before.ID)
	if len(after.Credentials) != len(before.Credentials)+2 || !reflect.DeepEqual(after.Credentials[:len(before.Credentials)], before.Credentials) {
		t.Fatal("append replaced, reordered or altered existing credentials")
	}
	comparison := after
	comparison.Credentials = before.Credentials
	comparison.Generation = before.Generation
	if !reflect.DeepEqual(comparison, before) {
		t.Fatal("append changed non-credential channel configuration")
	}
	afterPrices, _ := f.st.GetPrices()
	afterRoutes, _ := f.st.ListDownstreamModels()
	if !reflect.DeepEqual(prices, afterPrices) || !reflect.DeepEqual(routes, afterRoutes) {
		t.Fatal("append changed pricing or routing")
	}
	if _, err = f.s.Import(context.Background(), "owner", p.ID, items); err != nil || count() != 1 {
		t.Fatal("append replay wrote or duplicated credentials")
	}
}

func TestServiceInvalidationRejectsReadScanAndImportWithoutWrites(t *testing.T) {
	for _, scenario := range []string{"source-disable", "source-restart", "session-expire"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServiceFixture(t)
			f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("invalidated-private-key", "https://invalidate.example.invalid/v1", "chat"))})
			p := f.scan(t, "owner", "source-magpie")
			row := serviceNewRows(p)[0]
			want := ErrConflict
			switch scenario {
			case "source-disable":
				f.epochs.transition("source-magpie", false)
			case "source-restart":
				f.epochs.transition("source-magpie", false)
				f.epochs.transition("source-magpie", true)
			case "session-expire":
				private, err := f.s.lookup("owner", p.ID)
				if err != nil {
					t.Fatal(err)
				}
				expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				private.mu.Lock()
				private.ctx = expired
				private.expires = time.Now().Add(-time.Second)
				private.mu.Unlock()
				want = ErrExpired
			}
			count := f.auditWrites(t)
			if _, err := f.s.Get(context.Background(), "owner", p.ID); !errors.Is(err, want) {
				t.Fatalf("Get: %v", err)
			}
			if _, err := f.s.Scan(context.Background(), "owner", p.ID, "source-magpie"); !errors.Is(err, want) {
				t.Fatalf("Scan: %v", err)
			}
			if _, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID}}); !errors.Is(err, want) {
				t.Fatalf("Import: %v", err)
			}
			if count() != 0 {
				t.Fatal("invalidated operation wrote to Store")
			}
		})
	}
}

func TestServiceOwnerIsolationCancelAndAccessGuard(t *testing.T) {
	f := newServiceFixture(t)
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("owner-private-key", "https://owner.example.invalid/v1", "chat"))})
	p := f.scan(t, "owner", "source-magpie")
	row := serviceNewRows(p)[0]
	if _, err := f.s.Get(context.Background(), "different-owner", p.ID); !errors.Is(err, ErrExpired) {
		t.Fatal("different owner read the session")
	}
	if _, err := f.s.Scan(context.Background(), "different-owner", p.ID, "source-magpie"); !errors.Is(err, ErrExpired) {
		t.Fatal("different owner scanned the session")
	}
	if _, err := f.s.Import(context.Background(), "different-owner", p.ID, []Selection{{CandidateID: row.ID}}); !errors.Is(err, ErrExpired) {
		t.Fatal("different owner imported a key")
	}
	if err := f.s.Cancel("different-owner", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Get(context.Background(), "owner", p.ID); err != nil {
		t.Fatal("different owner canceled another session")
	}
	denied := errors.New("caller permission revoked")
	ctx := accessguard.With(context.Background(), func() error { return denied })
	if _, err := f.s.Begin(ctx, "owner", []string{"source-magpie"}); !errors.Is(err, denied) {
		t.Fatal("Begin ignored caller permission")
	}
	if _, err := f.s.Get(ctx, "owner", p.ID); !errors.Is(err, denied) {
		t.Fatal("Get ignored caller permission")
	}
	if _, err := f.s.Scan(ctx, "owner", p.ID, "source-magpie"); !errors.Is(err, denied) {
		t.Fatal("Scan ignored caller permission")
	}
	if _, err := f.s.Import(ctx, "owner", p.ID, []Selection{{CandidateID: row.ID}}); !errors.Is(err, denied) {
		t.Fatal("Import ignored caller permission")
	}
	if err := f.s.Cancel("owner", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Get(context.Background(), "owner", p.ID); !errors.Is(err, ErrExpired) {
		t.Fatal("canceled preview remained accessible")
	}
	if _, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID}}); !errors.Is(err, ErrExpired) {
		t.Fatal("canceled preview imported")
	}
	channels, _ := f.st.ListChannels()
	if len(channels) != 0 {
		t.Fatal("rejected operation imported keys")
	}
}

func TestServiceFileMutationAfterPreviewRejectsImport(t *testing.T) {
	for _, mutation := range []string{"changed-read-file", "created-missing-file", "removed-read-file"} {
		t.Run(mutation, func(t *testing.T) {
			f := newServiceFixture(t)
			root := f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("preview-private-key", "https://preview.example.invalid/v1", "chat"))})
			p := f.scan(t, "owner", "source-magpie")
			row := serviceNewRows(p)[0]
			switch mutation {
			case "changed-read-file":
				serviceWrite(t, root, "providers.json", serviceMagpie(serviceMagpieKey("changed-private-key", "https://preview.example.invalid/v1", "chat")))
			case "created-missing-file":
				serviceWrite(t, root, "settings.json", map[string]int{"port": 3425})
			case "removed-read-file":
				if err := os.Remove(filepath.Join(root, "providers.json")); err != nil {
					t.Fatal(err)
				}
			}
			count := f.auditWrites(t)
			if _, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID}}); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed source imported: %v", err)
			}
			if count() != 0 {
				t.Fatal("changed source wrote a channel")
			}
		})
	}
}

func TestServiceOneSourceFailureRetainsOthersAndCanImportThem(t *testing.T) {
	f := newServiceFixture(t)
	key := "claude-surviving-private-key"
	f.source(t, "source-claude", map[string]any{"settings.json": map[string]any{"env": map[string]string{"ANTHROPIC_API_KEY": key, "ANTHROPIC_BASE_URL": "https://surviving.example.invalid"}}})
	badRoot := f.source(t, "source-magpie", nil)
	if err := os.WriteFile(filepath.Join(badRoot, "providers.json"), []byte(`malformed private source document`), 0600); err != nil {
		t.Fatal(err)
	}
	p := f.scan(t, "owner", "source-claude", "source-magpie")
	if len(serviceNewRows(p)) != 1 {
		t.Fatal("one failing source erased another source's result")
	}
	statuses := map[string]string{}
	for _, source := range p.Sources {
		statuses[source.ID] = source.Status
	}
	if statuses["source-claude"] != "complete" || statuses["source-magpie"] != "error" {
		t.Fatalf("wrong partial scan status: %v", statuses)
	}
	row := serviceFindRow(t, p, key)
	result, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID}})
	if err != nil || len(result.Results) != 1 || result.Results[0].Added != 1 {
		t.Fatalf("surviving source import: %+v %v", result.Results, err)
	}
	serviceAssertPrivate(t, result, key, badRoot, "private source document")
}

func TestServiceTargetEditedAfterPreviewRejectsImport(t *testing.T) {
	f := newServiceFixture(t)
	const address = "https://changed-target.example.invalid/v1"
	target := serviceSaveChannel(t, f.st, store.Channel{Name: "Original target", Enabled: true, APIKey: "original-target-private-key", Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: address}}})
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("new-target-private-key", address, "chat"))})
	p := f.scanTarget(t, "owner", target, "source-magpie")
	row := serviceFindRow(t, p, "new-target-private-key")
	if len(row.Targets) != 1 || row.Targets[0].ID != target.ID {
		t.Fatal("expected one matching destination")
	}
	selected := Selection{CandidateID: row.ID, TargetChannelID: target.ID, ExpectedVersion: row.Targets[0].Version}
	target.Name = "Owner changed the target"
	target = serviceSaveChannel(t, f.st, target)
	count := f.auditWrites(t)
	if _, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{selected}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale target preview was accepted: %v", err)
	}
	if count() != 0 || !reflect.DeepEqual(serviceChannel(t, f.st, target.ID), target) {
		t.Fatal("stale import altered the newly edited target")
	}
}

func TestServiceProtocolBindingsStayInsideTheirURLGroup(t *testing.T) {
	f := newServiceFixture(t)
	const chatURL = "https://chat-group.example.invalid/v1"
	const responseURL = "https://response-group.example.invalid/v1"
	chatKey, responseKey := "cht-group-private-1111", "res-group-private-2222"
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(map[string]any{
		"key": chatKey, "keyProtocol": "chat", "chat": chatURL, "responses": responseURL,
		"keys": []map[string]any{{"key": responseKey, "protocol": "responses"}},
	})})
	p := f.scan(t, "owner", "source-magpie")
	if len(p.Groups) != 2 {
		t.Fatal("different source URLs were combined")
	}
	for _, group := range p.Groups {
		if len(group.Rows) != 1 {
			t.Fatal("key leaked into an undeclared URL group")
		}
		row := group.Rows[0]
		switch group.URLDisplay {
		case chatURL:
			if row.MaskedKey != store.MaskKey(chatKey) || !reflect.DeepEqual(row.Formats, []string{store.FormatOpenAIChatCompletions}) {
				t.Fatal("chat URL received another key or protocol")
			}
		case responseURL:
			if row.MaskedKey != store.MaskKey(responseKey) || !reflect.DeepEqual(row.Formats, []string{store.FormatOpenAIResponses}) {
				t.Fatal("responses URL received another key or protocol")
			}
		default:
			t.Fatal("unexpected URL group")
		}
	}
}

func (f *serviceFixture) scanTarget(t *testing.T, owner string, target store.Channel, ids ...string) Preview {
	t.Helper()
	p, err := f.s.Begin(context.Background(), owner, ids, BeginOptions{TargetChannelID: target.ID, CredentialType: target.CredentialType})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		p, err = f.s.Scan(context.Background(), owner, p.ID, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	return p
}
