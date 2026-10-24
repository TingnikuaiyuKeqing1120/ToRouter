package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"torouter/internal/accessguard"
	"torouter/internal/marketprofile"
	"torouter/internal/pluginmarket"
	"torouter/internal/store"
)

func TestMarketSameVersionReinstallRetiresRuntime(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pkg := "github:synthetic/supplier"
	id := marketprofile.PackageID(pkg)
	typ := marketprofile.CredentialType(pkg, "fixture")
	write := func(revision string) string {
		dir := filepath.Join(root, "plugins", "market", "packages", id, "versions", revision)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		module := filepath.Join(dir, "index.mjs")
		if err := os.WriteFile(module, []byte("// synthetic module"), 0600); err != nil {
			t.Fatal(err)
		}
		record := map[string]any{"id": id, "package": pkg, "name": "Synthetic supplier", "version": "1.0.0", "module": module, "runtime": "synthetic-runtime", "providers": []marketprofile.Provider{{ID: "fixture", Name: "Fixture", Methods: []marketprofile.AuthMethod{{Type: "api", Label: "Key"}}}}}
		b, _ := json.Marshal([]any{record})
		if err := os.WriteFile(filepath.Join(root, "plugins", "market", "active.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		return module
	}
	firstModule := write("first")
	m, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.AttachMarket(pluginmarket.New(root)); err != nil {
		t.Fatal(err)
	}
	first, oldCtx, err := m.Native(context.Background(), typ)
	if err != nil || first.Market.ModulePath != firstModule {
		t.Fatal("initial runtime unavailable")
	}
	before, _ := m.Snapshot()
	nextModule := write("second-same-version")
	if err = m.AttachMarket(pluginmarket.New(root)); err != nil {
		t.Fatal(err)
	}
	next, _, err := m.Native(context.Background(), typ)
	if err != nil || next == first || next.Market.ModulePath != nextModule {
		t.Fatal("reinstall retained old runtime")
	}
	if accessguard.Check(oldCtx) == nil {
		t.Fatal("old account handle remained authorized after code replacement")
	}
	after, _ := m.Snapshot()
	if after.Version == before.Version {
		t.Fatal("install replacement did not invalidate stale UI mutation version")
	}
	if _, err = m.SetCapability(context.Background(), typ, "connect", false, before.Version); err != ErrConflict {
		t.Fatal("stale pre-update snapshot changed new runtime")
	}
}

func TestMarketReloadFindsRestoredChannelWithoutInstalledPackage(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.AttachMarket(pluginmarket.New(root)); err != nil {
		t.Fatal(err)
	}
	typ := marketprofile.CredentialType("@fixture/not-installed", "fixture")
	o, err := store.MarketOAuthCredential(typ, `{"type":"api","key":"synthetic-restored-key"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	initial := store.Channel{Name: "Restored supplier", CredentialType: typ, Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://example.invalid/v1"}}}
	if _, _, err = st.CommitMarketConsent(initial, o, store.PluginConsentTarget{}, nil); err != nil {
		t.Fatal(err)
	}
	if err = m.Reload(); err != nil {
		t.Fatal(err)
	}
	list, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.CredentialType == typ {
			if len(item.Capabilities[0].AffectedChannelIDs) != 1 || item.Capabilities[0].Available {
				t.Fatal("restored account has wrong availability")
			}
			return
		}
	}
	t.Fatal("restored market account missing from management without app restart")
}
