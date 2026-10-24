package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func operationsStoreFixture(t *testing.T) (*Store, Channel) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := Channel{Name: "operations", CredentialType: CredentialCodexOAuth, Enabled: true, Models: []string{"model"},
		Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		Credentials: []CredentialItem{
			{ID: "one/%name", Enabled: true, OAuth: &OAuthCredential{Provider: OAuthProviderCodex, ClientID: "client", Account: "account-one", AccessToken: "private-one", RefreshToken: "refresh-one", ExpiresAt: time.Now().Add(time.Hour)}},
			{ID: "two", Enabled: true, OAuth: &OAuthCredential{Provider: OAuthProviderCodex, ClientID: "client", Account: "account-two", AccessToken: "private-two", RefreshToken: "refresh-two", ExpiresAt: time.Now().Add(time.Hour)}},
		},
	}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestCredentialOperationsNarrowWritesPreserveLatestConfiguration(t *testing.T) {
	s, c := operationsStoreFixture(t)
	stale, _ := s.Channel(c.ID)
	identity, _ := CredentialOperationsIdentity(c, "two")
	cfg := DefaultCredentialOperationsConfig()
	cfg.Warmup.Model = "model"
	if err := s.PatchCredentialOperationsConfig(c.ID, "two", identity, CredentialOperationsPatch{Warmup: &cfg.Warmup}); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Channel(c.ID)
	if current.Generation != c.Generation {
		t.Fatal("operation options changed routing generation")
	}
	current.Credentials[1].OAuth.AccessToken = "rotated-latest"
	current.Credentials[1].OAuth.RefreshToken = "rotated-refresh"
	if err := s.SaveChannel(&current); err != nil {
		t.Fatal(err)
	}
	stale.Name = "renamed"
	if err := s.SaveChannelConfig(&stale); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCredentialOrder(c.ID, []string{"two", "one/%name"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Channel(c.ID)
	if got.Name != "renamed" || got.Credentials[0].ID != "two" || got.Credentials[0].OAuth.AccessToken != "rotated-latest" || !reflect.DeepEqual(got.Credentials[0].Operations, &cfg) {
		t.Fatal("narrow edit overwrote latest identity/configuration")
	}
	if err := s.SetCredentialEnabled(c.ID, "two", false); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Channel(c.ID)
	for _, ids := range [][]string{{"two"}, {"two", "two"}, {"missing", "one/%name"}} {
		if err := s.SetCredentialOrder(c.ID, ids); !errors.Is(err, ErrInvalidCredentialOperation) {
			t.Fatalf("bad order accepted: %v", err)
		}
	}
	if err := s.SetCredentialEnabled(c.ID, "one/%name", false); !errors.Is(err, ErrInvalidCredentialOperation) {
		t.Fatal("last enabled credential silently disabled")
	}
	after, _ := s.Channel(c.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed narrow update modified channel")
	}
}

func TestCredentialOperationsClaimsFenceSettingsButKeepReceipts(t *testing.T) {
	s, c := operationsStoreFixture(t)
	identity, _ := CredentialOperationsIdentity(c, "two")
	write := func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"receipt":"sent"}`), nil }
	if _, err := s.ClaimCredentialOperationState(c, "two", write); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultCredentialOperationsConfig()
	cfg.Warmup.Model = "model"
	if err := s.PatchCredentialOperationsConfig(c.ID, "two", identity, CredentialOperationsPatch{Warmup: &cfg.Warmup}); err != nil {
		t.Fatal(err)
	}
	called := false
	if _, err := s.ClaimCredentialOperationState(c, "two", func(json.RawMessage) (json.RawMessage, error) { called = true; return nil, nil }); !errors.Is(err, ErrCredentialIdentityChanged) || called {
		t.Fatal("stale config claim reached callback")
	}
	if err := s.SetCredentialEnabled(c.ID, "two", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateCredentialOperationState(c, "two", write); err != nil {
		t.Fatal("disable discarded sent receipt", err)
	}
	raw, err := s.LoadCredentialOperationState(c.ID, "two")
	if err != nil || string(raw) != `{"receipt":"sent"}` {
		t.Fatalf("missing receipt: %s %v", raw, err)
	}
	changed, _ := s.Channel(c.ID)
	changed.Credentials[1].OAuth.Account = "other-account"
	if err := s.SaveChannel(&changed); err != nil {
		t.Fatal(err)
	}
	if changed.Credentials[1].Operations != nil {
		t.Fatal("replacement identity inherited automatic options")
	}
	if _, err := s.UpdateCredentialOperationState(c, "two", write); !errors.Is(err, ErrCredentialIdentityChanged) {
		t.Fatal("old receipt crossed identities")
	}
	if raw, err = s.LoadCredentialOperationState(c.ID, "two"); err != nil || len(raw) != 0 {
		t.Fatal("replacement identity inherited old state")
	}
	if err := s.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM meta WHERE key LIKE 'codex_credential_operations:%'").Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted channel retained operation records")
	}
}

func TestCredentialOperationsValidationAndRollback(t *testing.T) {
	s, c := operationsStoreFixture(t)
	identity, _ := CredentialOperationsIdentity(c, "two")
	before, _ := s.Channel(c.ID)
	cfg := DefaultCredentialOperationsConfig()
	cfg.Warmup.Enabled = true
	if err := s.PatchCredentialOperationsConfig(c.ID, "two", identity, CredentialOperationsPatch{Warmup: &cfg.Warmup}); !errors.Is(err, ErrInvalidCredentialOperation) {
		t.Fatal("enabled warmup without model accepted")
	}
	cfg.Warmup.Model = "missing-model"
	if err := s.PatchCredentialOperationsConfig(c.ID, "two", identity, CredentialOperationsPatch{Warmup: &cfg.Warmup}); !errors.Is(err, ErrInvalidCredentialOperation) {
		t.Fatal("disabled model accepted")
	}
	cfg.Warmup.Model = "model"
	if err := s.PatchCredentialOperationsConfig(c.ID, "two", "stale", CredentialOperationsPatch{Warmup: &cfg.Warmup}); !errors.Is(err, ErrCredentialIdentityChanged) {
		t.Fatal("stale UI identity accepted")
	}
	after, _ := s.Channel(c.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("invalid config modified channel")
	}
	_, err := s.UpdateCredentialOperationState(c, "two", func(json.RawMessage) (json.RawMessage, error) { return nil, errors.New("abort") })
	if err == nil {
		t.Fatal("callback error ignored")
	}
	if raw, err := s.LoadCredentialOperationState(c.ID, "two"); err != nil || len(raw) > 0 {
		t.Fatal("failed callback committed")
	}
	if _, err := s.UpdateCredentialOperationState(c, "two", func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"ok":true}`), nil }); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", credentialOperationKey(c.ID, "two")).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"account-two", "private-two", "refresh-two"} {
		if strings.Contains(raw, secret) {
			t.Fatal("ledger contains credential identity/secrets")
		}
	}
}

func TestCredentialOperationsReorderKeepsQuotaForSameIdentity(t *testing.T) {
	s, c := operationsStoreFixture(t)
	record := QuotaCache{Snapshot: json.RawMessage(`{"plan_type":"plus"}`), LastAttemptAt: time.Now()}
	if err := s.SaveQuotaCache(c, "two", record); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCredentialOrder(c.ID, []string{"two", "one/%name"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCredentialEnabled(c.ID, "two", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadQuotaCache(c.ID, "two")
	if err != nil || string(got.Snapshot) != string(record.Snapshot) {
		t.Fatal("order/toggle erased unchanged account quota")
	}
	if err := s.SaveQuotaCache(c, "two", record); err == nil {
		t.Fatal("stale in-flight writer accepted")
	}
}
