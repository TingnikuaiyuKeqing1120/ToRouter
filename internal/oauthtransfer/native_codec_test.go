package oauthtransfer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"torouter/internal/accessguard"
	"torouter/internal/antigravity"
	"torouter/internal/compatprofile"
	"torouter/internal/store"
)

func TestNativeSupportedFormatsPreserveMaterial(t *testing.T) {
	fixtures := []struct{ kind, raw string }{
		{store.CredentialCodexOAuth, `{"tokens":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","id_token":"synthetic-id","account_id":"synthetic-workspace"},"extension":{"keep":"opaque"}}`},
		{store.CredentialClaudeOAuth, `{"claudeAiOauth":{"accessToken":"synthetic-access","refreshToken":"synthetic-refresh","expiresAt":1900000000000,"scopes":["user:profile","user:inference"],"subscriptionType":"synthetic-plan","refreshTokenExpiresAt":2000000000000},"mcpOAuth":{"keep":"opaque"}}`},
		{store.CredentialAntigravityOAuth, `{"email":"fixture@example.invalid","token":{"refresh_token":"synthetic-refresh","access_token":"synthetic-access","project_id":"synthetic-project","expiry_timestamp":1900000000000,"extension":"opaque"}}`},
		{store.CredentialChatGPTOAuth, `{"clientId":"oaiapp_fixture","hostId":"urn:uuid:00000000-0000-4000-8000-000000000001","sub":"synthetic-subject","access":"synthetic-access","refresh":"synthetic-refresh","idToken":"synthetic-id","expires":"2030-01-01T00:00:00Z","scopes":["openid","profile","email","offline_access","resource.invoke","chatgpt.tokens.use.direct"],"extension":"opaque"}`},
		{store.CredentialCopilotOAuth, `{"github.com":{"oauth_token":"synthetic-github","user":"synthetic-user","extension":"opaque"}}`},
		{store.CredentialDevinOAuth, `{"type":"api","key":"synthetic-session","metadata":{"email":"fixture@example.invalid","plan":"synthetic-plan"}}`},
	}
	for _, p := range compatprofile.Profiles() {
		raw := `{"type":"oauth","access":"synthetic-access","refresh":"synthetic-refresh","expires":1900000000000,"uid":"synthetic-user","realUserID":"synthetic-user","extension":"opaque"}`
		fixtures = append(fixtures, struct{ kind, raw string }{p.CredentialType, raw})
	}
	for _, f := range fixtures {
		t.Run(f.kind, func(t *testing.T) {
			items, err := ParseNativeCredentialsForType(context.Background(), f.kind, []byte(f.raw))
			if err != nil || len(items) != 1 {
				t.Fatalf("parse %v", err)
			}
			file, err := EncodeNativeCredential(items[0])
			if err != nil {
				t.Fatal(err)
			}
			again, err := ParseNativeCredentialsForType(context.Background(), f.kind, []byte(file.Content))
			if err != nil || len(again) != 1 {
				t.Fatalf("roundtrip %v", err)
			}
			a, b := items[0].OAuth, again[0].OAuth
			if a.AccessToken != b.AccessToken || a.RefreshToken != b.RefreshToken || a.IDToken != b.IDToken || a.GitHubToken != b.GitHubToken || a.ClientID != b.ClientID || a.HostID != b.HostID || a.Account != b.Account || a.Subject != b.Subject || a.ProjectID != b.ProjectID || a.Scopes != b.Scopes || !a.ExpiresAt.Equal(b.ExpiresAt) {
				t.Fatal("native roundtrip changed genuine authorization material")
			}
			if strings.Contains(f.raw, "opaque") && !strings.Contains(file.Content, "opaque") {
				t.Fatal("opaque source field lost")
			}
			if store.IsCompatibleOAuth(f.kind) && a.PluginState != b.PluginState {
				t.Fatal("complete plugin state changed")
			}
		})
	}
}
func TestNativeRefreshOnlyPreviewCompletesOnlyWhenConfirmed(t *testing.T) {
	for _, kind := range []string{store.CredentialCodexOAuth, store.CredentialClaudeOAuth, store.CredentialAntigravityOAuth} {
		t.Run(kind, func(t *testing.T) {
			raw := `{"refresh_token":"synthetic-refresh","id_token":"synthetic-id"}`
			if kind == store.CredentialClaudeOAuth {
				raw = `{"claudeAiOauth":{"accessToken":"synthetic-access","refreshToken":"synthetic-refresh"}}`
			}
			calls := 0
			svc := New(newStore(t), func(ctx context.Context, k string, o store.OAuthCredential) (store.OAuthCredential, error) {
				calls++
				if k != kind || o.RefreshToken != "synthetic-refresh" {
					t.Fatal("completion changed source binding")
				}
				o.AccessToken = "synthetic-access"
				o.Account = "synthetic-account"
				o.Subject = "synthetic-subject"
				o.DeviceID = strings.Repeat("a", 64)
				o.ProjectID = "synthetic-project"
				o.ExpiresAt = time.Now().Add(time.Hour)
				return o, nil
			})
			preview, err := svc.Prepare(context.Background(), "owner", raw, kind)
			if err != nil || len(preview.Rows) != 1 || calls != 0 {
				t.Fatalf("offline preview %v", err)
			}
			result, err := svc.Import(context.Background(), "owner", preview.ID, []Selection{{CandidateID: preview.Rows[0].ID}})
			if err != nil || len(result.Results) != 1 || result.Results[0].Added != 1 || calls != 1 {
				t.Fatalf("confirmed completion %v %#v", err, result.Results)
			}
		})
	}
}
func TestNativeMultiFileIssuesAndZip(t *testing.T) {
	st := newStore(t)
	ch := seed(t, st)
	svc := New(st)
	raw := nativeTestFiles(t, []store.OAuthTransferItem{fixtureItem()})[0]
	preview, err := svc.PrepareFiles(context.Background(), "owner", []string{raw, `{"secret":"not-a-credential"}`}, store.CredentialCodexOAuth)
	if err != nil || len(preview.Rows) != 1 || len(preview.Errors) != 1 || preview.Errors[0].File != 2 {
		t.Fatalf("bad file feedback missing %v", err)
	}
	second := *ch.Credentials[0].OAuth
	second.RefreshToken = "synthetic-second-refresh"
	second.AccessToken = "synthetic-second-access"
	second.Account = "synthetic-second-account"
	ch.Credentials = append(ch.Credentials, store.CredentialItem{ID: "second", Enabled: true, OAuth: &second})
	if err := st.SaveChannel(&ch); err != nil {
		t.Fatal(err)
	}
	exported, err := svc.Export(context.Background(), []store.OAuthTransferRef{{ChannelID: ch.ID, CredentialID: ch.Credentials[0].ID}, {ChannelID: ch.ID, CredentialID: "second"}})
	if err != nil || exported.Encoding != "base64" || exported.MIMEType != "application/zip" {
		t.Fatal("zip export missing", err)
	}
	b, err := base64.StdEncoding.DecodeString(exported.File)
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil || len(z.File) != 2 || z.File[0].Name == z.File[1].Name {
		t.Fatal("each selected credential must have an independent native file")
	}
}
func TestNativeAmbiguousPluginRequiresTypeAndMiniMaxRequiresRealIdentity(t *testing.T) {
	raw := []byte(`{"type":"oauth","access":"synthetic-access","refresh":"synthetic-refresh","expires":1900000000000,"uid":"synthetic-user"}`)
	if _, err := ParseNativeCredentialsForType(context.Background(), "", raw); err == nil {
		t.Fatal("ambiguous provider was guessed")
	}
	if _, err := ParseNativeCredentialsForType(context.Background(), compatprofile.MiniMaxCredential, raw); err == nil {
		t.Fatal("missing real provider ID was fabricated")
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["realUserID"] = "synthetic-user"
	raw, _ = json.Marshal(m)
	if _, err := ParseNativeCredentialsForType(context.Background(), compatprofile.MiniMaxCredential, raw); err != nil {
		t.Fatal("complete Magpie MiniMax state rejected", err)
	}
}

func TestCopilotAppsTokensExportWithoutDuplicatingOneCredential(t *testing.T) {
	raw := []byte(`{"github.com:synthetic-client":{"oauth_token":"synthetic-github","user":"synthetic-user"},"github.com:other-client":{"oauth_token":"synthetic-other"}}`)
	items, err := ParseNativeCredentialsForType(context.Background(), store.CredentialCopilotOAuth, raw)
	if err != nil || len(items) != 2 {
		t.Fatal("apps tokens omitted", err)
	}
	for _, item := range items {
		file, err := EncodeNativeCredential(item)
		if err != nil || file.Filename != "apps.json" {
			t.Fatal("app native filename missing", err)
		}
		again, err := ParseNativeCredentialsForType(context.Background(), store.CredentialCopilotOAuth, []byte(file.Content))
		if err != nil || len(again) != 1 || again[0].OAuth.GitHubToken != item.OAuth.GitHubToken {
			t.Fatal("one credential export duplicated material", err)
		}
	}
}
func TestConfirmedImportRetainsRotatedMaterialAfterPermissionFailure(t *testing.T) {
	st := newStore(t)
	source := fixtureItem()
	existing := seed(t, st)
	raw := `{"tokens":{"refresh_token":"synthetic-pending-refresh","id_token":"synthetic-id"}}`
	calls := 0
	denyNext := false
	svc := New(st, func(ctx context.Context, k string, o store.OAuthCredential) (store.OAuthCredential, error) {
		calls++
		o.AccessToken = "synthetic-rotated-access"
		o.RefreshToken = "synthetic-rotated-refresh"
		denyNext = true
		return o, nil
	})
	preview, err := svc.Prepare(context.Background(), "owner", raw, store.CredentialCodexOAuth, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := preview.Rows[0].Targets[0]
	guarded := accessguard.With(context.Background(), func() error {
		if denyNext {
			denyNext = false
			return errors.New("synthetic permission failure")
		}
		return nil
	})
	first, err := svc.Import(guarded, "owner", preview.ID, []Selection{{CandidateID: preview.Rows[0].ID, TargetChannelID: target.ID, ExpectedVersion: target.Version}})
	if err != nil || first.Results[0].Status != "failed" || calls != 1 {
		t.Fatal("stale target should fail after rotation", err)
	}
	refreshed, err := svc.Get(context.Background(), "owner", preview.ID)
	if err != nil {
		t.Fatal(err)
	}
	target = refreshed.Rows[0].Targets[0]
	second, err := svc.Import(context.Background(), "owner", preview.ID, []Selection{{CandidateID: refreshed.Rows[0].ID, TargetChannelID: target.ID, ExpectedVersion: target.Version}})
	if err != nil || second.Results[0].Added != 1 || calls != 1 {
		t.Fatal("retry reused stale refresh material", err)
	}
	saved, _ := st.Channel(target.ID)
	if len(saved.Credentials) != 2 || saved.Credentials[1].OAuth.RefreshToken != "synthetic-rotated-refresh" || saved.Credentials[0].OAuth.RefreshToken != source.OAuth.RefreshToken {
		t.Fatal("rotation cache or existing member corrupted")
	}
}

func TestNativeTypeMismatchAndWorkspaceBoundary(t *testing.T) {
	raw := []byte(`{"type":"antigravity","access_token":"synthetic-access","refresh_token":"synthetic-refresh"}`)
	if _, err := ParseNativeCredentialsForType(context.Background(), store.CredentialCodexOAuth, raw); err == nil {
		t.Fatal("explicit provider mismatch accepted")
	}
	jwt := makeSyntheticJWT(t, map[string]any{"sub": "synthetic-same-user"})
	batch := []map[string]any{{"tokens": map[string]any{"access_token": "synthetic-first", "refresh_token": "synthetic-first-refresh", "id_token": jwt}, "meta": map[string]any{"chatgptAccountId": "synthetic-workspace-one"}}, {"tokens": map[string]any{"access_token": "synthetic-second", "refresh_token": "synthetic-second-refresh", "id_token": jwt}, "meta": map[string]any{"chatgptAccountId": "synthetic-workspace-two"}}}
	encoded, _ := json.Marshal(batch)
	svc := New(newStore(t))
	preview, err := svc.Prepare(context.Background(), "owner", string(encoded), store.CredentialCodexOAuth)
	if err != nil || len(preview.Rows) != 2 {
		t.Fatal("distinct workspaces were collapsed", err)
	}
}

func TestNativeExportUsesRotatedTokenAndExpiryInSourceAliases(t *testing.T) {
	for _, seconds := range []bool{false, true} {
		raw := `{"email":"fixture@example.invalid","token":{"accessToken":"synthetic-old-access","refreshToken":"synthetic-old-refresh","expiry_timestamp":1900000000000,"opaque":"keep"}}`
		if seconds {
			raw = strings.ReplaceAll(raw, "1900000000000", "1900000000")
		}
		items, err := ParseNativeCredentialsForType(context.Background(), store.CredentialAntigravityOAuth, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		o := items[0].OAuth
		o.AccessToken = "synthetic-new-access"
		o.RefreshToken = "synthetic-new-refresh"
		o.ExpiresAt = time.Unix(2000000000, 0)
		file, err := EncodeNativeCredential(items[0])
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseNativeCredentialsForType(context.Background(), store.CredentialAntigravityOAuth, []byte(file.Content))
		if err != nil || !again[0].OAuth.ExpiresAt.Equal(o.ExpiresAt) || again[0].OAuth.RefreshToken != o.RefreshToken || strings.Contains(file.Content, "synthetic-old") || !strings.Contains(file.Content, "keep") {
			t.Fatal("source token aliases retained stale material", err)
		}
	}
}

func TestAntigravityServiceRetriesTransientRefreshWithoutLosingMaterial(t *testing.T) {
	tokenCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls++
			r.ParseForm()
			if r.Form.Get("refresh_token") != "synthetic-refresh" {
				t.Error("retry lost original refresh token")
			}
			if tokenCalls == 1 {
				http.Error(w, "synthetic transient", 503)
				return
			}
			fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-rotated-refresh","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"email":"fixture@example.invalid","id":"synthetic-subject"}`)
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"cloudaicompanionProject":"synthetic-project"}`)
		default:
			t.Errorf("unexpected import endpoint %s", r.URL.Path)
			http.Error(w, "invalid", 400)
		}
	}))
	defer srv.Close()
	driver := antigravity.NewDriver(srv.Client(), antigravity.Endpoints{TokenURL: srv.URL + "/token", UserInfoURL: srv.URL + "/userinfo", APIBase: srv.URL, LoadBase: srv.URL})
	svc := New(newStore(t), func(ctx context.Context, kind string, c store.OAuthCredential) (store.OAuthCredential, error) {
		return driver.CompleteImport(ctx, c)
	})
	preview, err := svc.Prepare(context.Background(), "owner", `{"type":"antigravity","refresh_token":"synthetic-refresh"}`, store.CredentialAntigravityOAuth)
	if err != nil {
		t.Fatal(err)
	}
	selected := []Selection{{CandidateID: preview.Rows[0].ID}}
	first, err := svc.Import(context.Background(), "owner", preview.ID, selected)
	if err != nil || len(first.Results) != 1 || first.Results[0].Status != "failed" || tokenCalls != 1 {
		t.Fatal("transient failure should remain failed without saving", err)
	}
	if !strings.Contains(first.Results[0].Message, "检查网络") || strings.Contains(first.Results[0].Message, "目标或授权已变化") {
		t.Fatal("provider completion failure should offer a retry instead of reporting a target conflict")
	}
	second, err := svc.Import(context.Background(), "owner", preview.ID, selected)
	if err != nil || second.Results[0].Added != 1 || tokenCalls != 2 {
		t.Fatal("retry did not reach provider and save", err)
	}
	saved, _ := svc.store.Channel(second.Results[0].ChannelID)
	if saved.Credentials[0].OAuth.RefreshToken != "synthetic-rotated-refresh" || saved.Credentials[0].OAuth.ProjectID != "synthetic-project" {
		t.Fatal("retry lost complete material")
	}
}
