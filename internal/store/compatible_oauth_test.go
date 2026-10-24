package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"torouter/internal/compatprofile"
)

func pluginFixture(t *testing.T, account, access string) OAuthCredential {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"type": "oauth", "uid": account, "access": access, "refresh": "synthetic-refresh", "expires": 1900000000000,
		"deviceToken": "synthetic-device", "deviceRefresh": "synthetic-device-refresh", "unknown": map[string]any{"nested": "synthetic-private-field"}})
	o, err := CompatibleOAuthCredential(compatprofile.QoderCredential, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPluginStateSealedCASRestartAndDraft(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	o := pluginFixture(t, "synthetic-account", "synthetic-access")
	c, linked, err := st.CommitPluginConsent(compatprofile.QoderCredential, o, PluginConsentTarget{}, nil)
	if err != nil || linked {
		t.Fatalf("initial consent: %v", err)
	}
	draft, _ := st.Channel(c.ID)
	id, generation, stateVersion := c.Credentials[0].ID, c.Generation, PluginStateVersion(c.OAuth)
	var raw string
	if err = st.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(c.OAuth.Public())
	for _, secret := range []string{"synthetic-access", "synthetic-refresh", "synthetic-device", "synthetic-private-field"} {
		if strings.Contains(raw, secret) || strings.Contains(string(public), secret) {
			t.Fatal("plugin material escaped the encrypted state")
		}
	}
	next := pluginFixture(t, "synthetic-account", "synthetic-rotated")
	catalogueVersion := CatalogueSourceVersion(c)
	saved, err := st.ReplacePluginState(c.ID, id, stateVersion, next)
	if err != nil {
		t.Fatal(err)
	}
	rotated, _ := st.Channel(c.ID)
	if CatalogueSourceVersion(rotated) != catalogueVersion {
		t.Fatal("ordinary plugin rotation invalidated its own model fetch or pending pool append")
	}
	if _, err = st.ReplacePluginState(c.ID, id, stateVersion, o); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("stale worker accepted: %v", err)
	}
	draft.Name = "Edited display name"
	if err = st.SaveChannelConfig(&draft); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	live, err := st.Channel(c.ID)
	if err != nil || live.OAuth.PluginState != saved.PluginState || live.Generation != generation || live.Name != draft.Name {
		t.Fatalf("restart/config/rotation invariant failed: %v", err)
	}
	other := pluginFixture(t, "different-account", "synthetic-other")
	if _, err = st.ReplacePluginState(c.ID, id, PluginStateVersion(live.OAuth), other); !errors.Is(err, ErrNativeOAuthIdentity) {
		t.Fatalf("cross-account write: %v", err)
	}
}

func TestPluginConsentPoolDedupGuardAndExactReauth(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first, _, err := st.CommitPluginConsent(compatprofile.QoderCredential, pluginFixture(t, "one", "synthetic-one"), PluginConsentTarget{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := first
	pool, _, err := st.CommitPluginConsent(compatprofile.QoderCredential, pluginFixture(t, "two", "synthetic-two"),
		PluginConsentTarget{ChannelID: first.ID, Version: CatalogueSourceVersion(first)}, nil)
	if err != nil || len(pool.Credentials) != 2 {
		t.Fatalf("append: %v", err)
	}
	if err = st.SaveChannelConfig(&stale); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("stale draft removed new member: %v", err)
	}
	again, linked, err := st.CommitPluginConsent(compatprofile.QoderCredential, pluginFixture(t, "two", "synthetic-reauth"), PluginConsentTarget{}, nil)
	if err != nil || !linked || again.ID != pool.ID || len(again.Credentials) != 2 {
		t.Fatalf("duplicated owner: %v", err)
	}
	target := PluginConsentTarget{ChannelID: pool.ID, CredentialID: pool.Credentials[1].ID, Version: CompatibleOAuthIdentity(pool.CredentialType, pool.Credentials[1].OAuth)}
	refreshed, err := st.ReplacePluginState(pool.ID, target.CredentialID, PluginStateVersion(again.Credentials[1].OAuth), pluginFixture(t, "two", "synthetic-rotation-during-consent"))
	if err != nil {
		t.Fatal(err)
	}
	reauthorized, _, err := st.CommitPluginConsent(compatprofile.QoderCredential, pluginFixture(t, "two", "synthetic-new-consent"), target, nil)
	if err != nil || !strings.Contains(reauthorized.Credentials[1].OAuth.PluginState, "synthetic-new-consent") {
		t.Fatalf("ordinary rotation invalidated independent reauthorization: %v", err)
	}
	if _, err = st.ReplacePluginState(pool.ID, target.CredentialID, PluginStateVersion(&refreshed), pluginFixture(t, "two", "synthetic-late-worker")); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("old rotation overwrote new consent: %v", err)
	}
	if _, _, err = st.CommitPluginConsent(compatprofile.QoderCredential, pluginFixture(t, "three", "synthetic-wrong-account"), target, nil); !errors.Is(err, ErrNativeOAuthIdentity) {
		t.Fatalf("reauthorization replaced the original account: %v", err)
	}
	target.Version = CompatibleOAuthIdentity(pool.CredentialType, pool.Credentials[0].OAuth)
	if _, _, err = st.CommitPluginConsent(compatprofile.QoderCredential, pluginFixture(t, "two", "synthetic-wrong-target"), target, nil); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("wrong owner accepted for reauthorization: %v", err)
	}
	denied := errors.New("disabled during consent")
	if _, _, err = st.CommitPluginConsent(compatprofile.QoderCredential, pluginFixture(t, "three", "synthetic-three"), PluginConsentTarget{}, func() error { return denied }); !errors.Is(err, denied) {
		t.Fatal("revoked consent saved")
	}
	rows, _ := st.ListChannels()
	if len(rows) != 1 || len(rows[0].Credentials) != 2 {
		t.Fatal("consent wrote unexpected owners")
	}
}
