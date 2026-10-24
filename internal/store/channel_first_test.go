package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"torouter/internal/compatprofile"
)

func TestT136EmptyBuiltinPools(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	types := []string{CredentialCodexOAuth, CredentialChatGPTOAuth, CredentialCopilotOAuth, CredentialClaudeOAuth, CredentialAntigravityOAuth, CredentialDevinOAuth, CredentialZenFree}
	for _, p := range compatprofile.Profiles() {
		types = append(types, p.CredentialType)
	}
	for _, ctype := range types {
		for range 2 {
			c, e := BuiltinChannel("Synthetic pool", ctype)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.SaveChannelConfig(&c); e != nil {
				t.Fatalf("%s: %v", ctype, e)
			}
			got, e := s.Channel(c.ID)
			if e != nil {
				t.Fatal(e)
			}
			if !got.Enabled {
				t.Fatal("new pool disabled")
			}
			if ctype == CredentialZenFree {
				if got.PrimaryKey() != "public" {
					t.Fatal("missing public credential")
				}
			} else if len(got.Credentials) != 0 || got.OAuth != nil {
				t.Fatal("empty pool acquired credentials")
			}
		}
	}
}

func consentFixture(account string) OAuthCredential {
	return OAuthCredential{Provider: OAuthProviderCodex, Account: account, Subject: "synthetic-user-" + account, ClientID: "synthetic-client", AccessToken: "synthetic-access-" + account, RefreshToken: "synthetic-refresh-" + account, ExpiresAt: time.Now().Add(time.Hour), Status: OAuthStatusActive}
}
func poolFixture(t *testing.T, s *Store, name string) Channel {
	t.Helper()
	c, _ := BuiltinChannel(name, CredentialCodexOAuth)
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}
func addConsentFixture(t *testing.T, s *Store, c Channel, account string) Channel {
	t.Helper()
	next, _, err := s.CommitAccountConsent(c.CredentialType, consentFixture(account), PluginConsentTarget{ChannelID: c.ID, Version: CatalogueSourceVersion(c)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestT136ExactConsentAndStaleDraft(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	c := poolFixture(t, s, "Pool")
	stale := c
	c = addConsentFixture(t, s, c, "one")
	c = addConsentFixture(t, s, c, "two")
	if err := s.SaveChannelConfig(&stale); !errors.Is(err, ErrEditConflict) {
		t.Fatal("stale draft removed members", err)
	}
	other := poolFixture(t, s, "Other")
	if _, _, err := s.CommitAccountConsent(c.CredentialType, consentFixture("two"), PluginConsentTarget{ChannelID: other.ID, Version: CatalogueSourceVersion(other)}, nil); err == nil {
		t.Fatal("duplicate account crossed pools")
	}
	target := PluginConsentTarget{ChannelID: c.ID, CredentialID: c.Credentials[1].ID, Version: AccountOAuthIdentity(c.CredentialType, c.Credentials[1].OAuth)}
	next := consentFixture("two")
	next.AccessToken = "synthetic-new-consent"
	updated, _, err := s.CommitAccountConsent(c.CredentialType, next, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Credentials[0].OAuth.AccessToken != c.Credentials[0].OAuth.AccessToken || updated.Credentials[1].OAuth.AccessToken != next.AccessToken {
		t.Fatal("reauth changed another member")
	}
	draft := c
	draft.Name = "Edited"
	if err = s.SaveChannelConfig(&draft); err != nil {
		t.Fatal(err)
	}
	live, _ := s.Channel(c.ID)
	if live.Credentials[1].OAuth.AccessToken != next.AccessToken {
		t.Fatal("ordinary save restored stale tokens")
	}
	next.Account = "foreign"
	if _, _, err = s.CommitAccountConsent(c.CredentialType, next, target, nil); err == nil {
		t.Fatal("foreign reauth accepted")
	}
}

func TestT136MoveAtomicPreservesWholeMemberAndLateReceipt(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	defer s.Close()
	source := poolFixture(t, s, "Source")
	target := poolFixture(t, s, "Target")
	source = addConsentFixture(t, s, source, "one")
	source = addConsentFixture(t, s, source, "two")
	target = addConsentFixture(t, s, target, "three")
	source.Credentials[0].Label = "Synthetic note"
	cfg := DefaultCredentialOperationsConfig()
	source.Credentials[0].Operations = &cfg
	if err := s.SaveChannel(&source); err != nil {
		t.Fatal(err)
	}
	before := source
	raw := json.RawMessage(`{"history":{"request":{"state":"running"}}}`)
	if _, err := s.UpdateCredentialOperationState(source, source.Credentials[0].ID, func(json.RawMessage) (json.RawMessage, error) { return raw, nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MoveCredentials(source.ID, target.ID, []string{source.Credentials[1].ID, source.Credentials[0].ID}, source.Generation-1, target.Generation, nil); !errors.Is(err, ErrEditConflict) {
		t.Fatal("stale move accepted", err)
	}
	a, b, err := s.MoveCredentials(source.ID, target.ID, []string{source.Credentials[1].ID, source.Credentials[0].ID}, source.Generation, target.Generation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Credentials) != 0 || a.OAuth != nil || !a.Enabled || len(b.Credentials) != 3 || EntityVersion(b.Credentials[1:]) != EntityVersion(before.Credentials) {
		t.Fatal("move lost order, state, or empty source")
	}
	loaded, err := s.LoadCredentialOperationState(b.ID, b.Credentials[1].ID)
	if err != nil || string(loaded) != string(raw) {
		t.Fatal("move lost pending receipt", err, string(loaded))
	}
	finished := json.RawMessage(`{"history":{"request":{"state":"succeeded"}}}`)
	if _, err = s.UpdateCredentialOperationState(before, b.Credentials[1].ID, func(json.RawMessage) (json.RawMessage, error) { return finished, nil }); err != nil {
		t.Fatal("late receipt lost", err)
	}
	if _, err = s.ClaimCredentialOperationState(before, b.Credentials[1].ID, func(json.RawMessage) (json.RawMessage, error) { return finished, nil }); err == nil {
		t.Fatal("late action claim followed moved member")
	}
	loaded, err = s.LoadCredentialOperationState(b.ID, b.Credentials[1].ID)
	if err != nil || string(loaded) != string(finished) {
		t.Fatal("target receipt not completed", err)
	}
	if _, _, err = s.MoveCredentials(b.ID, a.ID, []string{b.Credentials[1].ID}, b.Generation, a.Generation, func() error { return errors.New("revoked") }); err == nil {
		t.Fatal("revoked move succeeded")
	}
	live, _ := s.Channel(b.ID)
	if len(live.Credentials) != 3 {
		t.Fatal("failed move partially persisted")
	}
}

func TestT136MoveRollsBackBothRows(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	a := addConsentFixture(t, s, poolFixture(t, s, "A"), "one")
	b := poolFixture(t, s, "B")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_move BEFORE UPDATE ON channels WHEN NEW.id=2 BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MoveCredentials(a.ID, b.ID, []string{a.Credentials[0].ID}, a.Generation, b.Generation, nil); err == nil {
		t.Fatal("injected failure ignored")
	}
	a, _ = s.Channel(a.ID)
	b, _ = s.Channel(b.ID)
	if len(a.Credentials) != 1 || len(b.Credentials) != 0 {
		t.Fatal("move partially persisted")
	}
}

func TestT136LateReceiptFollowsMoveAfterSourceDeletion(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := addConsentFixture(t, s, poolFixture(t, s, "Source"), "moving-account")
	target := poolFixture(t, s, "Target")
	cid := source.Credentials[0].ID
	running := json.RawMessage(`{"history":{"request":{"state":"running"}}}`)
	if _, err = s.ClaimCredentialOperationState(source, cid, func(json.RawMessage) (json.RawMessage, error) { return running, nil }); err != nil {
		t.Fatal(err)
	}
	_, target, err = s.MoveCredentials(source.ID, target.ID, []string{cid}, source.Generation, target.Generation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteChannel(source.ID); err != nil {
		t.Fatal(err)
	}
	finished := json.RawMessage(`{"history":{"request":{"state":"succeeded"}}}`)
	if _, err = s.UpdateCredentialOperationState(source, cid, func(raw json.RawMessage) (json.RawMessage, error) {
		if string(raw) != string(running) {
			t.Fatal("late completion did not read the moved receipt")
		}
		return finished, nil
	}); err != nil {
		t.Fatal("source deletion discarded completion", err)
	}
	if _, err = s.ClaimCredentialOperationState(source, cid, func(json.RawMessage) (json.RawMessage, error) {
		t.Fatal("deleted source accepted a new action")
		return nil, nil
	}); !errors.Is(err, ErrNotFound) {
		t.Fatal("claim no longer rejects deleted source", err)
	}
	loaded, err := s.LoadCredentialOperationState(target.ID, cid)
	if err != nil || string(loaded) != string(finished) {
		t.Fatal("target receipt did not complete", err, string(loaded))
	}
}

func TestT136CodexConsentSeparatesUsersInSameWorkspace(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pool := poolFixture(t, s, "Shared workspace")
	a := consentFixture("shared-workspace")
	a.Subject = "synthetic-user-a"
	pool, _, err = s.CommitAccountConsent(pool.CredentialType, a, PluginConsentTarget{ChannelID: pool.ID, Version: CatalogueSourceVersion(pool)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultCredentialOperationsConfig()
	pool.Credentials[0].Operations = &cfg
	if err = s.SaveChannel(&pool); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Subject = "synthetic-user-b"
	b.AccessToken = "synthetic-user-b-access"
	b.RefreshToken = "synthetic-user-b-refresh"
	pool, _, err = s.CommitAccountConsent(pool.CredentialType, b, PluginConsentTarget{ChannelID: pool.ID, Version: CatalogueSourceVersion(pool)}, nil)
	if err != nil {
		t.Fatal("second user in the same workspace was treated as the first", err)
	}
	before := EntityVersion(pool.Credentials)
	if _, _, err = s.CommitAccountConsent(pool.CredentialType, b, PluginConsentTarget{ChannelID: pool.ID, CredentialID: pool.Credentials[0].ID, Version: AccountOAuthIdentity(pool.CredentialType, pool.Credentials[0].OAuth)}, nil); !errors.Is(err, ErrNativeOAuthIdentity) {
		t.Fatal("reauth changed the user behind a workspace", err)
	}
	live, _ := s.Channel(pool.ID)
	if EntityVersion(live.Credentials) != before || live.Credentials[0].Operations == nil {
		t.Fatal("refused reauth changed consent or operations")
	}
}

func TestT136CopilotImportedTokenCanAcquireVerifiedIdentity(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, _ := BuiltinChannel("Imported Copilot", CredentialCopilotOAuth)
	c.Credentials = []CredentialItem{{ID: "imported", Enabled: true, OAuth: &OAuthCredential{Provider: OAuthProviderCopilot, GitHubToken: "synthetic-imported-github"}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	target := PluginConsentTarget{ChannelID: c.ID, CredentialID: c.Credentials[0].ID, Version: AccountOAuthIdentity(c.CredentialType, c.Credentials[0].OAuth)}
	next := *c.Credentials[0].OAuth
	next.Account = "synthetic-verified-login"
	next.AccessToken = "synthetic-copilot-access"
	foreign := next
	foreign.GitHubToken = "synthetic-unproven-github"
	if _, _, err = s.CommitAccountConsent(c.CredentialType, foreign, target, nil); !errors.Is(err, ErrNativeOAuthIdentity) {
		t.Fatal("unrelated token acquired imported identity", err)
	}
	updated, _, err := s.CommitAccountConsent(c.CredentialType, next, target, nil)
	if err != nil {
		t.Fatal("same imported token could not acquire verified identity", err)
	}
	if updated.Credentials[0].ID != "imported" || updated.Credentials[0].OAuth.Account != next.Account || updated.Credentials[0].OAuth.AccessToken != next.AccessToken {
		t.Fatal("identity enrichment changed member or lost authorization")
	}
}
