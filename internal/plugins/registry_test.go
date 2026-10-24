package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/copilot"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(testStore(t))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func toggle(t *testing.T, m *Manager, id string, enabled bool) List {
	t.Helper()
	before, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	after, err := m.SetCapability(context.Background(), id, "connect", enabled, before.Version)
	if err != nil {
		t.Fatal(err)
	}
	return after
}

func TestRegistryLazyDefaultsAndConcurrentFactory(t *testing.T) {
	m := testManager(t)
	var factories atomic.Int32
	for _, e := range m.ordered {
		original := e.factory
		e.factory = func() *NativeRuntime { factories.Add(1); return original() }
	}
	list, err := m.Snapshot()
	if err != nil || len(list.Items) != 21 || !m.Enabled(store.CredentialAPIKey) || !m.Enabled(store.CredentialCodexOAuth) || !m.Enabled(store.CredentialClaudeOAuth) || !m.Enabled(store.CredentialAntigravityOAuth) || m.Enabled(store.CredentialCopilotOAuth) || m.Enabled(store.CredentialChatGPTOAuth) || m.Enabled(store.CredentialZenFree) || m.Enabled(store.CredentialDevinOAuth) {
		t.Fatalf("defaults: %+v, %v", list, err)
	}
	for _, id := range []string{"qoder", "qoder-cn", "workbuddy", "workbuddy-ai", "minimax-code", "minimax-code-global"} {
		e := m.entries[id]
		if e == nil || e.info.Implementation != "compatible" || e.enabled {
			t.Fatal("compatible provider must require explicit enable")
		}
	}
	for _, e := range m.ordered {
		if m.Existing(e.credentialType) != nil {
			t.Fatal("snapshot constructed a service")
		}
	}
	if _, _, err = m.Resolve(context.Background(), store.CredentialCopilotOAuth); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled resolve: %v", err)
	}
	if factories.Load() != 0 {
		t.Fatal("disabled resolve called factory")
	}
	var wg sync.WaitGroup
	results := make(chan *NativeRuntime, 20)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, err := m.Native(context.Background(), store.CredentialCodexOAuth)
			if err != nil {
				t.Error(err)
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	first := m.Existing(store.CredentialCodexOAuth)
	for r := range results {
		if r != first || r.Codex == nil || r.Operations == nil {
			t.Fatal("concurrent native resolution did not share its service")
		}
	}
	if factories.Load() != 1 {
		t.Fatalf("factory calls: %d", factories.Load())
	}
	for _, info := range list.Items {
		if !info.Featured || (info.Implementation != "native" && info.Implementation != "compatible") || info.Version == "" || len(info.Capabilities) != 1 || info.Capabilities[0].AffectedChannelIDs == nil {
			t.Fatalf("invalid public info: %+v", info)
		}
		if info.Capabilities[0].Available != (info.Implementation == "native") {
			t.Fatal("missing runtime must affect only compatible providers")
		}
		if info.Capabilities[0].ID == "connect" && m.byType[info.CredentialType] != m.entries[info.ID] {
			t.Fatalf("credential type metadata missing or incorrect: %+v", info)
		}
	}
}

func TestRegistryInjectedDefaultsAndPersistedFalse(t *testing.T) {
	st := testStore(t)
	cp := copilot.NewService(st, nil, copilot.Endpoints{})
	sw := siwc.NewService(st, nil, siwc.Endpoints{})
	cx := codex.NewService(st, nil, codex.Endpoints{})
	m, err := NewInjected(st, cp, sw, cx)
	if err != nil {
		t.Fatal(err)
	}
	for _, credentialType := range []string{store.CredentialCopilotOAuth, store.CredentialChatGPTOAuth, store.CredentialCodexOAuth} {
		if !m.Enabled(credentialType) || m.Existing(credentialType) != nil {
			t.Fatalf("injected initial state: %s", credentialType)
		}
	}
	r, _, err := m.Native(context.Background(), store.CredentialCopilotOAuth)
	if err != nil || r.Copilot != cp {
		t.Fatalf("injected instance not reused: %v", err)
	}
	toggle(t, m, "copilot", false)
	m, err = NewInjected(st, cp, sw, cx)
	if err != nil || m.Enabled(store.CredentialCopilotOAuth) {
		t.Fatalf("injected default replaced persisted false: %v", err)
	}
}

func saveCopilot(t *testing.T, st *store.Store) store.Channel {
	t.Helper()
	c := store.Channel{Name: "Synthetic channel", CredentialType: store.CredentialCopilotOAuth, Enabled: false,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.githubcopilot.com"}},
		OAuth:     &store.OAuthCredential{Provider: store.OAuthProviderCopilot, GitHubToken: "synthetic-github-secret", AccessToken: "synthetic-old-bearer", Status: store.OAuthStatusActive},
	}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRegistryLegacySnapshotPrivacyAndVersion(t *testing.T) {
	st := testStore(t)
	c := saveCopilot(t, st)
	m, err := New(st)
	if err != nil || !m.Enabled(store.CredentialCopilotOAuth) || m.Existing(store.CredentialCopilotOAuth) != nil {
		t.Fatalf("legacy initialization: %v", err)
	}
	list, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Snapshot()
	if err != nil || list.Version != again.Version {
		t.Fatalf("snapshot version is unstable: %v", err)
	}
	var affected []int64
	for _, info := range list.Items {
		if info.ID == "copilot" {
			affected = info.Capabilities[0].AffectedChannelIDs
		}
	}
	if !reflect.DeepEqual(affected, []int64{c.ID}) {
		t.Fatalf("disabled existing channel omitted: %v", affected)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{c.Name, c.OAuth.GitHubToken, c.OAuth.AccessToken, "defaultEnabled", "default_enabled"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("snapshot includes private/internal data: %s", secret)
		}
	}
	saveCopilot(t, st)
	newList, err := m.Snapshot()
	if err != nil || newList.Version == list.Version {
		t.Fatalf("affected IDs did not change version: %v", err)
	}
	if _, err = m.SetCapability(context.Background(), "copilot", "connect", false, list.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale affected IDs accepted: %v", err)
	}
}

func TestRegistrySwitchEpochAccessGuardAndReuse(t *testing.T) {
	m := testManager(t)
	var allowed atomic.Bool
	allowed.Store(true)
	denied := errors.New("synthetic caller denied")
	parent := accessguard.With(context.Background(), func() error {
		if !allowed.Load() {
			return denied
		}
		return nil
	})
	r, guarded, err := m.Native(parent, store.CredentialCodexOAuth)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := m.Resolve(parent, store.CredentialCodexOAuth)
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := m.Epoch(store.CredentialCodexOAuth)
	allowed.Store(false)
	if err = accessguard.Check(guarded); !errors.Is(err, denied) {
		t.Fatalf("original access guard overwritten: %v", err)
	}
	if _, err = resolved.Bearer(context.Background(), store.Channel{}); !errors.Is(err, denied) {
		t.Fatalf("captured caller guard bypassed: %v", err)
	}
	allowed.Store(true)
	var notices []bool
	m.OnChange(func(credentialType string, enabled bool) {
		if m.Enabled(credentialType) != enabled || m.Existing(credentialType) != r {
			t.Error("callback state or runtime mismatch")
		}
		notices = append(notices, enabled)
	})
	toggle(t, m, "codex", false)
	if m.Existing(store.CredentialCodexOAuth) != r || !errors.Is(m.CheckEpoch(store.CredentialCodexOAuth, oldEpoch), ErrDisabled) {
		t.Fatal("disabled instance or epoch invalidation lost")
	}
	toggle(t, m, "codex", true)
	again, _, err := m.Native(context.Background(), store.CredentialCodexOAuth)
	if err != nil || again != r || again.Codex != r.Codex || again.Operations != r.Operations {
		t.Fatalf("re-enable created another credential lock domain: %v", err)
	}
	if err = accessguard.Check(guarded); !errors.Is(err, ErrDisabled) {
		t.Fatalf("old guarded context revived: %v", err)
	}
	if _, err = resolved.Bearer(context.Background(), store.Channel{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("old runtime revived with a fresh context: %v", err)
	}
	epoch := m.Epoch(store.CredentialCodexOAuth)
	toggle(t, m, "codex", true)
	if m.Epoch(store.CredentialCodexOAuth) != epoch || !reflect.DeepEqual(notices, []bool{false, true}) {
		t.Fatal("no-op switch changed epoch or emitted a change")
	}
}

func TestRegistryRejectsConflictsUnknownCapabilityAndCaller(t *testing.T) {
	m := testManager(t)
	before, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("synthetic caller denied")
	ctx := accessguard.With(context.Background(), func() error { return denied })
	for _, tc := range []struct {
		id, cap, version string
		ctx              context.Context
		want             error
	}{
		{"codex", "connect", "", context.Background(), ErrConflict},
		{"codex", "connect", "stale", context.Background(), ErrConflict},
		{"unknown", "connect", before.Version, context.Background(), ErrInvalid},
		{"codex", "unknown", before.Version, context.Background(), ErrInvalid},
		{"codex", "connect", before.Version, ctx, denied},
	} {
		if _, err := m.SetCapability(tc.ctx, tc.id, tc.cap, false, tc.version); !errors.Is(err, tc.want) {
			t.Fatalf("unexpected rejection %s/%s: %v", tc.id, tc.cap, err)
		}
	}
	after, err := m.Snapshot()
	if err != nil || !reflect.DeepEqual(before, after) || m.Epoch(store.CredentialCodexOAuth) != 1 {
		t.Fatalf("rejected write changed state: %v", err)
	}
	if _, _, err := m.Native(ctx, store.CredentialCodexOAuth); !errors.Is(err, denied) || m.Existing(store.CredentialCodexOAuth) != nil {
		t.Fatalf("denied caller created runtime: %v", err)
	}
	if _, err := New(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil store accepted: %v", err)
	}
}

func TestRegistryFailedStorageWriteKeepsRuntimeState(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER reject_plugin_preferences BEFORE UPDATE ON meta
		WHEN OLD.key='plugin_preferences' BEGIN SELECT RAISE(ABORT,'synthetic write failure'); END`); err != nil {
		t.Fatal(err)
	}
	before, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var notices int
	m.OnChange(func(string, bool) { notices++ })
	if _, err = m.SetCapability(context.Background(), "codex", "connect", false, before.Version); err == nil {
		t.Fatal("storage write failure swallowed")
	}
	after, err := m.Snapshot()
	if err != nil || !reflect.DeepEqual(before, after) || !m.Enabled(store.CredentialCodexOAuth) || m.Epoch(store.CredentialCodexOAuth) != 1 || notices != 0 {
		t.Fatalf("failed write changed runtime, epoch or notifications: %v", err)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
		var zero T
		return zero
	}
}

func TestRegistryWorkerStopsWithoutCancellationAndNeverOverlaps(t *testing.T) {
	m := testManager(t)
	toggle(t, m, "codex", false)
	started := make(chan context.Context, 3)
	stopped := make(chan bool, 2)
	release := make(chan struct{})
	var active, calls, factories atomic.Int32
	e := m.byType[store.CredentialCodexOAuth]
	e.factory = func() *NativeRuntime {
		factories.Add(1)
		return &NativeRuntime{background: func(ctx context.Context, stop <-chan struct{}) {
			if active.Add(1) != 1 {
				t.Error("background workers overlap")
			}
			defer active.Add(-1)
			call := calls.Add(1)
			started <- ctx
			select {
			case <-ctx.Done():
				return
			case <-stop:
				stopped <- ctx.Err() == nil
				if call == 1 {
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}
		}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	if m.Existing(store.CredentialCodexOAuth) != nil || factories.Load() != 0 {
		t.Fatal("disabled worker created runtime")
	}
	toggle(t, m, "codex", true)
	firstCtx := receive(t, started)
	toggle(t, m, "codex", false)
	if !receive(t, stopped) || firstCtx.Err() != nil {
		t.Fatal("disable canceled an in-flight HTTP context")
	}
	toggle(t, m, "codex", true)
	if calls.Load() != 1 || active.Load() != 1 {
		t.Fatal("new worker started before the previous one finished")
	}
	close(release)
	secondCtx := receive(t, started)
	if err := accessguard.Check(firstCtx); !errors.Is(err, ErrDisabled) || accessguard.Check(secondCtx) != nil {
		t.Fatalf("worker epoch guards: %v", err)
	}
	cancel()
	receive(t, done)
	if factories.Load() != 1 || active.Load() != 0 {
		t.Fatal("worker was recreated or not drained")
	}
}

func TestRegistryInFlightTokenStillPersistsAfterDisable(t *testing.T) {
	st := testStore(t)
	channel := saveCopilot(t, st)
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		select {
		case <-req.Context().Done():
			t.Error("disable canceled the token request")
			return
		case <-release:
		}
		fmt.Fprintf(w, `{"token":"synthetic-new-bearer","expires_at":%d}`, time.Now().Add(time.Hour).Unix())
	}))
	defer srv.Close()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	svc := copilot.NewService(st, srv.Client(), copilot.Endpoints{API: srv.URL})
	m, err := NewInjected(st, svc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime, guarded, err := m.Resolve(context.Background(), store.CredentialCopilotOAuth)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		token, e := runtime.Bearer(guarded, channel)
		if e == nil && token != "synthetic-new-bearer" {
			e = errors.New("wrong rotated token")
		}
		result <- e
	}()
	receive(t, entered)
	toggle(t, m, "copilot", false)
	unblock()
	if err = receive(t, result); err != nil {
		t.Fatalf("in-flight token result rejected: %v", err)
	}
	fresh, err := st.Channel(channel.ID)
	if err != nil || fresh.OAuth.AccessToken != "synthetic-new-bearer" {
		t.Fatalf("issued token not persisted: %v", err)
	}
	if _, err = runtime.Bearer(guarded, channel); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled handle allowed a later request: %v", err)
	}
}
