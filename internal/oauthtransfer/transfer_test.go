package oauthtransfer

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"torouter/internal/accessguard"
	"torouter/internal/store"
)

const fixturePassword = "synthetic-passphrase-for-tests"

func fixtureItem() store.OAuthTransferItem {
	return store.OAuthTransferItem{CredentialType: store.CredentialCodexOAuth, Label: "Fixture credential", Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex", State: "stateless"}},
		OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: "synthetic-client", Account: "synthetic-account",
			AccessToken: "synthetic-access-material", RefreshToken: "synthetic-refresh-material", IDToken: "synthetic-id-material",
			APIBase: "https://chatgpt.com/backend-api/codex", Status: store.OAuthStatusActive, Scopes: "openid profile email offline_access api.connectors.read api.connectors.invoke"}}
}
func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func seed(t *testing.T, s *store.Store) store.Channel {
	t.Helper()
	item := fixtureItem()
	c := store.Channel{Name: "Fixture channel", CredentialType: item.CredentialType, Endpoints: item.Endpoints, Enabled: true,
		Models: []string{"synthetic-model"}, Credentials: []store.CredentialItem{{ID: "fixture-member", Label: item.Label, Enabled: true, OAuth: item.OAuth}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}
func assertPrivate(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fixturePassword, "synthetic-refresh-material", "synthetic-access-material", "synthetic-id-material", "synthetic-account"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private material exposed in public response")
		}
	}
}
func nativeTestFiles(t *testing.T, items []store.OAuthTransferItem) []string {
	t.Helper()
	var out []string
	for _, item := range items {
		f, err := EncodeNativeCredential(item)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f.Content)
	}
	return out
}
func sameTokens(a, b *store.OAuthCredential) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.IDToken == b.IDToken && a.Account == b.Account
}
func TestNativeFileRejectsOldEnvelopeAndInvalidBatch(t *testing.T) {
	if _, err := ParseNativeCredentials(context.Background(), []byte(`{"format":"torouter-oauth-credentials","ciphertext":"old"}`)); !errors.Is(err, ErrFile) {
		t.Fatal("legacy envelope accepted")
	}
	if _, err := ParseNativeCredentialsForType(context.Background(), store.CredentialCodexOAuth, []byte(`[{},{}]`)); !errors.Is(err, ErrFile) {
		t.Fatal("invalid batch accepted")
	}
}
func TestServiceCrossInstanceSelectionAndNoOverwrite(t *testing.T) {
	source, destination := newStore(t), newStore(t)
	channel := seed(t, source)
	original, _ := source.Channel(channel.ID)
	export, err := New(source).Export(context.Background(), []store.OAuthTransferRef{{ChannelID: channel.ID, CredentialID: "fixture-member"}})
	if err != nil || export.Count != 1 {
		t.Fatal("export failed", err)
	}
	if !strings.Contains(export.File, "synthetic-refresh-material") {
		t.Fatal("native export lost material")
	}
	service := New(destination)
	preview, err := service.Prepare(context.Background(), "owner-one", export.File, "")
	if err != nil || len(preview.Rows) != 1 || preview.Rows[0].Status != "new" {
		t.Fatal("prepare failed", err)
	}
	assertPrivate(t, preview)
	if _, err = service.Get(context.Background(), "owner-two", preview.ID); !errors.Is(err, ErrExpired) {
		t.Fatal("other owner accessed file")
	}
	out, err := service.Import(context.Background(), "owner-one", preview.ID, []Selection{{CandidateID: preview.Rows[0].ID, Name: "Imported fixture"}})
	if err != nil || len(out.Results) != 1 || out.Results[0].Added != 1 {
		t.Fatal("import failed", err)
	}
	assertPrivate(t, out)
	got, _ := destination.Channel(out.Results[0].ChannelID)
	if len(got.Models) != 0 || len(got.AvailableModels) != 0 || got.Credentials[0].Operations != nil {
		t.Fatal("unselected configuration migrated")
	}
	if !sameTokens(got.Credentials[0].OAuth, original.Credentials[0].OAuth) {
		t.Fatal("credential material changed")
	}
	before := store.EntityVersion(got)
	again, err := service.Prepare(context.Background(), "owner-one", export.File, "")
	if err != nil || again.Rows[0].Status != "exists" {
		t.Fatal("existing identity was not recognised")
	}
	if _, err = service.Import(context.Background(), "owner-one", again.ID, []Selection{{CandidateID: again.Rows[0].ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("existing identity was overwritten")
	}
	after, _ := destination.Channel(got.ID)
	if store.EntityVersion(after) != before {
		t.Fatal("existing destination changed")
	}
	sourceAfter, _ := source.Channel(channel.ID)
	if store.EntityVersion(sourceAfter) != store.EntityVersion(original) {
		t.Fatal("source mutated")
	}
	service.Cancel("owner-one", preview.ID)
	if _, err = service.Get(context.Background(), "owner-one", preview.ID); !errors.Is(err, ErrExpired) {
		t.Fatal("cancelled file remained usable")
	}
}
func TestServiceRechecksPermissionAndTargetVersion(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	existing := fixtureItem()
	existing.OAuth.Account = "synthetic-other-account"
	existing.OAuth.RefreshToken = "synthetic-other-refresh-material"
	existing.OAuth.AccessToken = "synthetic-other-access-material"
	c := store.Channel{Name: "Destination", CredentialType: existing.CredentialType, Endpoints: existing.Endpoints, Enabled: true,
		Credentials: []store.CredentialItem{{ID: "existing-member", Label: "Existing", Enabled: true, OAuth: existing.OAuth}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	files := nativeTestFiles(t, []store.OAuthTransferItem{fixtureItem()})
	file := files[0]
	service := New(s)
	preview, err := service.Prepare(ctx, "owner", file, store.CredentialCodexOAuth, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := preview.Rows[0]
	if len(row.Targets) != 1 {
		t.Fatal("compatible destination missing")
	}
	target := row.Targets[0]
	selection := []Selection{{CandidateID: row.ID, TargetChannelID: target.ID, ExpectedVersion: target.Version}}
	denied := errors.New("synthetic permission revoked")
	guarded := accessguard.With(ctx, func() error { return denied })
	if _, err = service.Get(guarded, "owner", preview.ID); !errors.Is(err, denied) {
		t.Fatal("revoked read accepted")
	}
	if _, err = service.Import(guarded, "owner", preview.ID, selection); !errors.Is(err, denied) {
		t.Fatal("revoked import accepted")
	}
	c.Name = "Changed destination"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Import(ctx, "owner", preview.ID, selection); !errors.Is(err, ErrConflict) {
		t.Fatal("stale target accepted")
	}
	got, _ := s.Channel(c.ID)
	if len(got.Credentials) != 1 {
		t.Fatal("failed import partially saved")
	}
	session, err := service.lookup("owner", preview.ID)
	if err != nil {
		t.Fatal(err)
	}
	session.cancel()
	if _, err = service.Get(ctx, "owner", preview.ID); !errors.Is(err, ErrExpired) {
		t.Fatal("expired preview accepted")
	}
}

func TestPrepareUsesCompleteFileIdentityEvidence(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			st := newStore(t)
			ch := seed(t, st)
			ch.Credentials[0].OAuth.Subject = "synthetic-complete-user"
			if err := st.SaveChannel(&ch); err != nil {
				t.Fatal(err)
			}
			stable := fixtureItem()
			stable.OAuth.Subject = "synthetic-complete-user"
			stable.OAuth.IDToken = makeSyntheticJWT(t, map[string]any{"sub": "synthetic-complete-user", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": stable.OAuth.Account}})
			stable.OAuth.RefreshToken = "synthetic-rotated-material"
			anonymous := stable
			copyMaterial := *stable.OAuth
			anonymous.OAuth = &copyMaterial
			anonymous.OAuth.Account = ""
			anonymous.OAuth.Subject = ""

			items := []store.OAuthTransferItem{stable, anonymous}
			if reverse {
				items[0], items[1] = items[1], items[0]
			}
			files := nativeTestFiles(t, items)
			service := New(st)
			preview, err := service.PrepareFiles(context.Background(), "owner", files, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range preview.Rows {
				if row.Status != "exists" {
					t.Fatal("anonymous rotated copy lost its stable account evidence")
				}
			}
			if len(preview.Rows) != 1 {
				t.Fatal("same file identity was not merged")
			}
			channels, _ := st.ListChannels()
			if len(channels) != 1 || len(channels[0].Credentials) != 1 {
				t.Fatal("preparation changed the credential pool")
			}
		})
	}
}

func TestPrepareIncompleteIdentityAllowedAsNew(t *testing.T) {
	st := newStore(t)
	seed(t, st) // fixtureItem has Account but no Subject
	item := fixtureItem()
	item.OAuth.RefreshToken = "synthetic-different-material"
	files := nativeTestFiles(t, []store.OAuthTransferItem{item})
	service := New(st)
	preview, err := service.PrepareFiles(context.Background(), "owner", files, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Rows) != 1 || preview.Rows[0].Status != "new" {
		t.Fatalf("incomplete identity without matching material should be allowed as new, got %v", preview.Rows)
	}
}
