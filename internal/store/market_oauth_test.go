package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"torouter/internal/marketprofile"
)

func TestMarketCredentialStoreRotationReopenAndConsent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	typ := marketprofile.CredentialType("@fixture/supplier", "fixture")
	o, err := MarketOAuthCredential(typ, `{"type":"oauth","access":"synthetic-original","refresh":"synthetic-refresh","tenant":{"id":"synthetic-tenant"}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	initial := Channel{Name: "Synthetic supplier", CredentialType: typ, Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid/v1", State: "stateless"}}, AvailableModels: []string{"model"}, ModelFormats: map[string][]string{"model": {FormatOpenAIChatCompletions}}}
	c, _, err := s.CommitMarketConsent(initial, o, PluginConsentTarget{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := c.Generation
	next, err := MarketOAuthCredential(typ, `{"type":"oauth","access":"synthetic-rotated","refresh":"synthetic-refresh-2","tenant":{"id":"synthetic-tenant"}}`, o.Account)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplacePluginState(c.ID, c.Credentials[0].ID, PluginStateVersion(c.PrimaryOAuth()), next); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplacePluginState(c.ID, c.Credentials[0].ID, PluginStateVersion(c.PrimaryOAuth()), o); !errors.Is(err, ErrEditConflict) {
		t.Fatal("stale worker overwrote refreshed state")
	}
	current, _ := s.Channel(c.ID)
	if current.Generation != before || CompatibleOAuthIdentity(typ, current.PrimaryOAuth()) != CompatibleOAuthIdentity(typ, &o) {
		t.Fatal("rotation changed stable consent identity")
	}
	public, _ := json.Marshal(current.PrimaryOAuth().Public())
	if strings.Contains(string(public), "synthetic-") {
		t.Fatal("public OAuth info leaked opaque state")
	}
	if _, _, err = s.CommitMarketConsent(initial, next, PluginConsentTarget{ChannelID: c.ID, CredentialID: c.Credentials[0].ID, Version: CompatibleOAuthIdentity(typ, current.PrimaryOAuth())}, nil); err != nil {
		t.Fatal(err)
	}
	current, _ = s.Channel(c.ID)
	if current.Generation <= before {
		t.Fatal("explicit reauthorization did not invalidate old context")
	}
	if _, err = s.InitPluginPreferences(map[string]map[string]bool{typ: {"connect": false}}, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// No installed-plugin registry is needed to read/restore opaque credentials.
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.Channel(c.ID)
	if err != nil || loaded.PrimaryOAuth().PluginState != next.PluginState {
		t.Fatal("market consent did not reopen unchanged")
	}
	preferences, err := s.GetPluginPreferences()
	if err != nil || preferences.Plugins[typ]["connect"] {
		t.Fatal("market preference lost")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.db*"))
	for _, file := range files {
		raw, _ := os.ReadFile(file)
		if strings.Contains(string(raw), "synthetic-rotated") {
			t.Fatal("opaque credential persisted in plaintext")
		}
	}
}
