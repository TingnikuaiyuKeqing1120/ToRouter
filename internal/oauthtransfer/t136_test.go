package oauthtransfer

import (
	"context"
	"errors"
	"testing"
	"torouter/internal/store"
)

func TestT136CodexExplicitAccessOnlyAndRefreshOnly(t *testing.T) {
	for _, raw := range []string{`{"access_token":"synthetic-only-access","account_id":"synthetic-workspace"}`, `{"tokens":{"access_token":"synthetic-only-access","account_id":"synthetic-workspace"}}`} {
		parsed, err := ParseNativeCredentialsForType(context.Background(), store.CredentialCodexOAuth, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		o := parsed[0].OAuth
		if o.RefreshToken != "" || !o.ExpiresAt.IsZero() || o.Subject != "" || o.Account != "synthetic-workspace" {
			t.Fatal("access-only fabricated refresh, expiry, or identity")
		}
		calls := 0
		completed, err := CompleteImportItems(context.Background(), parsed, func(context.Context, string, store.OAuthCredential) (store.OAuthCredential, error) {
			calls++
			return store.OAuthCredential{}, errors.New("unexpected refresh")
		})
		if err != nil || calls != 0 || completed[0].OAuth.AccessToken != "synthetic-only-access" {
			t.Fatal("access-only attempted refresh", err)
		}
	}
	if _, err := ParseNativeCredentialsForType(context.Background(), store.CredentialCodexOAuth, []byte(`{"access_token":"synthetic-no-account"}`)); err == nil {
		t.Fatal("access-only accepted without request account")
	}
	parsed, err := ParseNativeCredentialsForType(context.Background(), store.CredentialCodexOAuth, []byte(`{"refresh_token":"synthetic-refresh-only"}`))
	if err != nil || parsed[0].OAuth.AccessToken != "" {
		t.Fatal("refresh-only parsing failed", err)
	}
}

func TestT136NativePreviewBindsEmptyTargetAndRejectsSwitchOrLateEdit(t *testing.T) {
	st := newStore(t)
	target, err := store.BuiltinChannel("Target", store.CredentialCodexOAuth)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.SaveChannel(&target); err != nil {
		t.Fatal(err)
	}
	other := target
	other.ID = 0
	other.Name = "Other"
	if err = st.SaveChannel(&other); err != nil {
		t.Fatal(err)
	}
	svc := New(st)
	raw := `{"access_token":"synthetic-access-only","account_id":"synthetic-account-only"}`
	p, err := svc.Prepare(context.Background(), "owner", raw, store.CredentialCodexOAuth, target.ID)
	if err != nil || p.Target == nil || p.Target.ID != target.ID || len(p.Rows[0].Targets) != 1 {
		t.Fatal("preview lost fixed target", err)
	}
	if _, err = svc.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: p.Rows[0].ID, TargetChannelID: other.ID}}); !errors.Is(err, ErrConflict) {
		t.Fatal("native selection changed target", err)
	}
	items := []Selection{{CandidateID: p.Rows[0].ID}}
	result, err := svc.Import(context.Background(), "owner", p.ID, items)
	if err != nil || result.Results[0].ChannelID != target.ID || result.Results[0].Added != 1 {
		t.Fatal("fixed target import failed", err)
	}
	replay, err := svc.Import(context.Background(), "owner", p.ID, items)
	if err != nil || replay.Results[0].ChannelID != target.ID {
		t.Fatal("native replay lost receipt", err)
	}
	saved, _ := st.Channel(target.ID)
	unchanged, _ := st.Channel(other.ID)
	if len(saved.Credentials) != 1 || len(unchanged.Credentials) != 0 || saved.Credentials[0].OAuth.RefreshToken != "" {
		t.Fatal("native import corrupted target or material")
	}
	next, err := svc.Prepare(context.Background(), "owner", `{"refresh_token":"synthetic-late"}`, store.CredentialCodexOAuth, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved.Name = "Edited"
	if err = st.SaveChannel(&saved); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Import(context.Background(), "owner", next.ID, []Selection{{CandidateID: next.Rows[0].ID}}); !errors.Is(err, ErrConflict) {
		t.Fatal("late import ignored target edit", err)
	}
}
