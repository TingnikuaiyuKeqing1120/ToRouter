package keydiscovery

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"torouter/internal/accessguard"
	"torouter/internal/oauthtransfer"
	"torouter/internal/store"
)

func TestDiscoveryURLSingleTargetAppendPreservesProtocolAndDisabledState(t *testing.T) {
	f := newServiceFixture(t)
	const address = "https://single.example.invalid/custom/v1"
	before := serviceSaveChannel(t, f.st, store.Channel{Name: "Existing disabled", Enabled: false, APIKey: "synthetic-old", Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: address, State: "stateless"}}})
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("synthetic-new", address, "responses"))})
	p := f.scanTarget(t, "owner", before, "source-magpie")
	if len(p.Groups) != 1 || p.Groups[0].TargetCount != 1 || p.Groups[0].CopyOnly || !p.Groups[0].Existing {
		t.Fatal("single URL target not recognized across protocols")
	}
	row := p.Groups[0].Rows[0]
	yes := true
	response, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID, TargetChannelID: before.ID, ExpectedVersion: store.EntityVersion(before), Enabled: &yes}})
	if err != nil || len(response.Results) != 1 || response.Results[0].Added != 1 {
		t.Fatalf("append failed: %v", err)
	}
	after := serviceChannel(t, f.st, before.ID)
	if after.Enabled || !reflect.DeepEqual(after.Endpoints, before.Endpoints) || len(after.Credentials) != 2 {
		t.Fatal("append changed protocol or old disabled state")
	}
	if len(response.Preview.Groups) != 0 {
		t.Fatal("existing credential remained visible")
	}
}

func TestDiscoveryNewChannelSelectionControlsEnabledIconAndOnlySelectedProtocols(t *testing.T) {
	f := newServiceFixture(t)
	const address = "https://selection.example.invalid/v1"
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("synthetic-chat", address, "chat"), serviceMagpieKey("synthetic-responses", address, "responses"))})
	p := f.scan(t, "owner", "source-magpie")
	row := serviceFindRow(t, p, "synthetic-chat")
	no := false
	response, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID, Enabled: &no, DisplayIcon: "deepseek"}})
	if err != nil || len(response.Results) != 1 || response.Results[0].Added != 1 {
		t.Fatalf("new URL import failed: %v", err)
	}
	channel := serviceChannel(t, f.st, response.Results[0].ChannelID)
	if !channel.Enabled || len(channel.Endpoints) != 1 || channel.Endpoints[0].Format != store.FormatOpenAIChatCompletions || !channel.Credentials[0].Enabled {
		t.Fatal("channel checkbox leaked into credential state or selected protocols widened")
	}
	display, err := f.st.GetChannelDisplay()
	if err != nil || len(display.Icons) != 1 {
		t.Fatal("selected icon not persisted")
	}
	for _, icon := range display.Icons {
		if icon != "deepseek" {
			t.Fatal("selected icon changed")
		}
	}
}

func TestDiscoveryAllExistingIncludingDisabledDisappearAndNewSourcesMerge(t *testing.T) {
	f := newServiceFixture(t)
	const address = "https://merge.example.invalid/v1"
	serviceSaveChannel(t, f.st, store.Channel{Name: "Existing", Enabled: false, Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: address}}, Credentials: []store.CredentialItem{{ID: "old", APIKey: "synthetic-old-disabled", Enabled: false}}})
	f.source(t, "source-claude", map[string]any{"settings.json": map[string]any{"env": map[string]any{"ANTHROPIC_API_KEY": "synthetic-new-shared", "ANTHROPIC_BASE_URL": address}}})
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("synthetic-old-disabled", address, "responses"), serviceMagpieKey("synthetic-new-shared", address, "chat"))})
	p := f.scan(t, "owner", "source-claude", "source-magpie")
	if len(p.Groups) != 1 || len(p.Groups[0].Rows) != 1 || len(p.Groups[0].Rows[0].Sources) != 2 || len(p.Groups[0].Rows[0].Formats) != 2 {
		t.Fatal("full key de-dup, disabled filtering or source merge failed")
	}
}

func TestDiscoveryCodexOAuthPreservesCompleteMaterialAndExplicitCopy(t *testing.T) {
	f := newServiceFixture(t)
	auth := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"access_token": "synthetic-oauth-access", "refresh_token": "synthetic-oauth-refresh", "id_token": "synthetic-oauth-id", "account_id": "synthetic-account"}, "last_refresh": "2026-01-01T00:00:00Z"}
	f.source(t, "source-codex", map[string]any{"auth.json": auth})
	f.source(t, "source-magpie", map[string]any{"logins.json": []any{map[string]any{"agent": "codex", "auth": auth}}})
	p := f.scan(t, "owner", "source-codex", "source-magpie")
	if len(p.Groups) != 1 || len(p.Groups[0].Rows) != 1 || len(p.Groups[0].Rows[0].Sources) != 2 || p.Groups[0].Rows[0].CredentialType != store.CredentialCodexOAuth {
		t.Fatalf("OAuth material failed to merge: groups=%d", len(p.Groups))
	}
	row := p.Groups[0].Rows[0]
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "synthetic-oauth-") || strings.Contains(string(raw), "synthetic-account") {
		t.Fatal("preview leaked OAuth material")
	}
	if _, err := f.s.Copy(context.Background(), "other", p.ID, []string{row.ID}); !errors.Is(err, ErrExpired) {
		t.Fatal("copy bypassed owner isolation")
	}
	copied, err := f.s.Copy(context.Background(), "owner", p.ID, []string{row.ID})
	if err != nil || len(copied.Items) != 1 {
		t.Fatal("copy failed")
	}
	parsed, err := oauthtransfer.ParseNativeCredentialsForType(context.Background(), store.CredentialCodexOAuth, []byte(copied.Items[0].Content))
	if err != nil || len(parsed) != 1 || parsed[0].OAuth.RefreshToken != "synthetic-oauth-refresh" || parsed[0].OAuth.IDToken != "synthetic-oauth-id" {
		t.Fatal("copy lost refresh/id material")
	}
	imported, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID}})
	if err != nil || len(imported.Results) != 1 || imported.Results[0].Added != 1 {
		t.Fatal("OAuth import failed")
	}
	channel := serviceChannel(t, f.st, imported.Results[0].ChannelID)
	if channel.CredentialType != store.CredentialCodexOAuth || channel.Credentials[0].OAuth.RefreshToken != "synthetic-oauth-refresh" || channel.Credentials[0].APIKey != "" {
		t.Fatal("OAuth flattened into API key")
	}
	if len(imported.Preview.Groups) != 0 {
		t.Fatal("stored OAuth still visible")
	}
}

func TestDiscoveryClaudeTokenWithRelayAndOfflineCredentials(t *testing.T) {
	fixture := &fileFixtures{files: map[string]string{"settings.json": `{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic-relay-token","ANTHROPIC_BASE_URL":"https://relay.example.invalid/v1"}}`, ".credentials.json": `{"claudeAiOauth":{"accessToken":"synthetic-claude-access","refreshToken":"synthetic-claude-refresh","expiresAt":1956528000000}}`}}
	candidates, _, err := readClaude(context.Background(), fixture)
	if err != nil || len(candidates) != 2 || candidates[0].Key != "synthetic-relay-token" || candidates[1].OAuth == nil || candidates[1].OAuth.OAuth.RefreshToken != "synthetic-claude-refresh" {
		t.Fatal("relay token or complete Claude OAuth missing")
	}
}

func TestDiscoveryOpenCodeReadsDefaultConfigAndSeparateDataAuth(t *testing.T) {
	f := newServiceFixture(t)
	config := t.TempDir()
	data := t.TempDir()
	t.Setenv("OPENCODE_CONFIG_DIR", config)
	t.Setenv("XDG_DATA_HOME", data)
	if err := os.Mkdir(filepath.Join(data, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	serviceWrite(t, config, "opencode.json", map[string]any{"provider": map[string]any{"custom": map[string]any{"npm": "@ai-sdk/openai-compatible", "options": map[string]any{"baseURL": "https://custom.example.invalid/v1", "apiKey": "synthetic-config-key"}}}})
	serviceWrite(t, filepath.Join(data, "opencode"), "auth.json", map[string]any{"deepseek": map[string]any{"type": "api", "key": "synthetic-auth-key"}})
	f.s.rootFor = nil
	p := f.scan(t, "owner", "source-opencode")
	if len(p.Groups) != 2 {
		t.Fatal("OpenCode separate default roots not read")
	}
	var row Row
	for _, group := range p.Groups {
		if group.URLDisplay == "https://api.deepseek.com/v1" {
			row = group.Rows[0]
		}
	}
	if row.ID == "" {
		t.Fatal("preset URL not retained")
	}
	serviceWrite(t, filepath.Join(data, "opencode"), "auth.json", map[string]any{"deepseek": map[string]any{"type": "api", "key": "synthetic-mutated"}})
	if _, err := f.s.Copy(context.Background(), "owner", p.ID, []string{row.ID}); !errors.Is(err, ErrConflict) {
		t.Fatal("copy ignored changed data credential file")
	}
}

func TestDiscoveryZCodeExplicitConfigFolder(t *testing.T) {
	f := newServiceFixture(t)
	alternate := t.TempDir()
	f.source(t, "source-zcode", map[string]any{"setting.json": map[string]any{"configFolder": alternate}})
	if err := os.WriteFile(filepath.Join(alternate, "provider_config.json"), []byte(zcodeFixture("anthropic-messages", nil)), 0600); err != nil {
		t.Fatal(err)
	}
	p := f.scan(t, "owner", "source-zcode")
	if len(p.Groups) != 1 {
		t.Fatal("ZCode configured folder not read")
	}
}

func TestDiscoveryManagerAndCCSwitchOAuthStorageFormats(t *testing.T) {
	t.Run("manager tokens table", func(t *testing.T) {
		r := &gatewayFakeResources{t: t, queries: map[string][][]string{"codexmanager.db:" + gatewayManagerOAuthSQL: {{"synthetic-manager-access", "synthetic-manager-refresh", "synthetic-manager-id", "synthetic-workspace", "", "disabled"}}}}
		cs, _, err := readGatewayOAuth(context.Background(), r, "source-codex-manager")
		if err != nil || len(cs) != 1 || cs[0].OAuth == nil || cs[0].OAuth.OAuth.RefreshToken != "synthetic-manager-refresh" || cs[0].OAuth.OAuth.Account != "synthetic-workspace" || !cs[0].Disabled {
			t.Fatal("manager OAuth columns lost")
		}
	})
	t.Run("ccswitch refresh only", func(t *testing.T) {
		r := &fileFixtures{files: map[string]string{"codex_oauth_auth.json": `{"accounts":{"local-id":{"account_id":"local-id","chatgpt_account_id":"synthetic-workspace","refresh_token":"synthetic-cc-refresh","id_token":"synthetic-cc-id"}}}`}}
		cs, _, err := readGatewayOAuth(context.Background(), r, "source-ccswitch")
		if err != nil || len(cs) != 1 || cs[0].OAuth == nil || cs[0].OAuth.OAuth.RefreshToken != "synthetic-cc-refresh" || cs[0].OAuth.OAuth.Account != "synthetic-workspace" || cs[0].OAuth.OAuth.AccessToken != "" {
			t.Fatal("CCSwitch refresh-only material lost or fabricated")
		}
	})
}

func TestDiscoveryCopyRejectsAccessRevocationAndFileMutation(t *testing.T) {
	f := newServiceFixture(t)
	root := f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("synthetic-copy-secret", "https://copy.example.invalid/v1", "chat"))})
	p := f.scan(t, "owner", "source-magpie")
	row := p.Groups[0].Rows[0]
	ctx := accessguard.With(context.Background(), func() error { return errors.New("synthetic permission revoked") })
	if _, err := f.s.Copy(ctx, "owner", p.ID, []string{row.ID}); err == nil {
		t.Fatal("copy bypassed revoked sensitive permission")
	}
	serviceWrite(t, root, "providers.json", serviceMagpie())
	if _, err := f.s.Copy(context.Background(), "owner", p.ID, []string{row.ID}); !errors.Is(err, ErrConflict) {
		t.Fatal("copy ignored source mutation")
	}
}

func TestDiscoveryCompletionKeepsRotatedMaterialAfterStoreConflict(t *testing.T) {
	f := newServiceFixture(t)
	f.source(t, "source-codex", map[string]any{"auth.json": map[string]any{"tokens": map[string]any{"access_token": "synthetic-old-access", "refresh_token": "synthetic-old-refresh", "account_id": "synthetic-workspace"}}})
	p := f.scan(t, "owner", "source-codex")
	row := p.Groups[0].Rows[0]
	calls := 0
	f.s.complete = func(ctx context.Context, items []store.OAuthTransferItem) ([]store.OAuthTransferItem, error) {
		calls++
		result := append([]store.OAuthTransferItem(nil), items...)
		material := *result[0].OAuth
		material.AccessToken = "synthetic-rotated-access"
		material.RefreshToken = "synthetic-rotated-refresh"
		result[0].OAuth = &material
		f.epochs.transition("source-codex", false)
		return result, nil
	}
	response, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID}})
	if err != nil || calls != 1 || len(response.Results) != 1 || response.Results[0].Status != "failed" {
		t.Fatal("completion conflict lost partial receipt", err)
	}
	session, _ := f.s.lookup("owner", p.ID)
	cached := session.sources["source-codex"].candidates[0].OAuth.OAuth
	if cached.RefreshToken != "synthetic-rotated-refresh" || cached.AccessToken != "synthetic-rotated-access" {
		t.Fatal("failed save discarded rotated material")
	}
	channels, _ := f.st.ListChannels()
	if len(channels) != 0 {
		t.Fatal("source invalidation persisted material")
	}

}

func TestDiscoveryDefaultRootsFollowConfiguredToolLocations(t *testing.T) {
	home := t.TempDir()
	config := t.TempDir()
	data := t.TempDir()
	app := t.TempDir()
	codex := t.TempDir()
	claude := t.TempDir()
	openCode := t.TempDir()
	database := filepath.Join(t.TempDir(), "custom-manager.sqlite")
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("APPDATA", app)
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("OPENCODE_CONFIG_DIR", openCode)
	t.Setenv("CODEXMANAGER_DB_PATH", database)
	wants := map[string]string{"source-codex": codex, "source-claude": claude, "source-opencode": openCode, "source-magpie": filepath.Join(config, "magpie"), "source-zcode": filepath.Join(home, ".zcode", "v2"), "source-ccswitch": filepath.Join(home, ".cc-switch"), "source-codex-manager": filepath.Dir(database)}
	for id, want := range wants {
		got, err := defaultRoot(id)
		if err != nil || got != want {
			t.Fatalf("configured source root not honored: %s", id)
		}
	}
	if openCodeDataRoot() != filepath.Join(data, "opencode") {
		t.Fatal("OpenCode data root ignored XDG_DATA_HOME")
	}
}

func TestDiscoveryFailedCompletionKeepsReturnedPartialMaterial(t *testing.T) {
	f := newServiceFixture(t)
	f.source(t, "source-codex", map[string]any{"auth.json": map[string]any{"tokens": map[string]any{"refresh_token": "synthetic-old-refresh", "account_id": "synthetic-workspace"}}})
	p := f.scan(t, "owner", "source-codex")
	row := p.Groups[0].Rows[0]
	f.s.complete = func(ctx context.Context, items []store.OAuthTransferItem) ([]store.OAuthTransferItem, error) {
		result := append([]store.OAuthTransferItem(nil), items...)
		material := *result[0].OAuth
		material.RefreshToken = "synthetic-partially-rotated-refresh"
		result[0].OAuth = &material
		return result, errors.New("synthetic-profile-not-ready")
	}
	response, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: row.ID}})
	if err != nil || len(response.Results) != 1 || response.Results[0].Status != "failed" || len(response.Preview.Groups) != 1 {
		t.Fatal("failed completion lost safe receipt")
	}
	copied, err := f.s.Copy(context.Background(), "owner", p.ID, []string{response.Preview.Groups[0].Rows[0].ID})
	if err != nil || len(copied.Items) != 1 || !strings.Contains(copied.Items[0].Content, "synthetic-partially-rotated-refresh") {
		t.Fatal("failed profile enrichment lost refreshed material")
	}
	channels, _ := f.st.ListChannels()
	if len(channels) != 0 {
		t.Fatal("failed enrichment persisted incomplete channel")
	}
}

func TestDiscoveryGroupIDAndNewChannelChoicesSurviveOtherGroupImport(t *testing.T) {
	f := newServiceFixture(t)
	const firstURL = "https://a-group.example.invalid/v1"
	const secondURL = "https://b-group.example.invalid/v1"
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("synthetic-first", firstURL, "chat"), serviceMagpieKey("synthetic-second", secondURL, "chat"))})
	p := f.scan(t, "owner", "source-magpie")
	if len(p.Groups) != 2 {
		t.Fatal("fixture needs two URL cards")
	}
	first, second := p.Groups[0], p.Groups[1]
	if first.URLDisplay != firstURL || second.URLDisplay != secondURL {
		t.Fatal("fixture URL order changed")
	}
	choices := map[string]bool{first.ID: true, second.ID: false}
	result, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: first.Rows[0].ID}})
	if err != nil || len(result.Preview.Groups) != 1 || result.Preview.Groups[0].ID != second.ID {
		t.Fatal("remaining URL card changed identity after earlier group disappeared")
	}
	remaining := result.Preview.Groups[0]
	enabled := choices[remaining.ID]
	result, err = f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: remaining.Rows[0].ID, Enabled: &enabled, DisplayIcon: "deepseek"}})
	if err != nil || len(result.Results) != 1 {
		t.Fatal("remaining group import failed")
	}
	channel := serviceChannel(t, f.st, result.Results[0].ChannelID)
	if !channel.Enabled {
		t.Fatal("global creation must be enabled independent of obsolete enabled input")
	}
}

func TestDiscoveryOpenCodeAuthUsesExplicitProviderRelayURL(t *testing.T) {
	f := newServiceFixture(t)
	config := t.TempDir()
	data := t.TempDir()
	const relay = "https://relay.example.invalid/custom/v1"
	t.Setenv("OPENCODE_CONFIG_DIR", config)
	t.Setenv("XDG_DATA_HOME", data)
	if err := os.Mkdir(filepath.Join(data, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	serviceWrite(t, config, "opencode.json", map[string]any{"provider": map[string]any{"openai": map[string]any{"options": map[string]any{"baseURL": relay}}}})
	serviceWrite(t, filepath.Join(data, "opencode"), "auth.json", map[string]any{"openai": map[string]any{"type": "api", "key": "synthetic-relay-auth"}})
	f.s.rootFor = nil
	p := f.scan(t, "owner", "source-opencode")
	if len(p.Groups) != 1 || p.Groups[0].URLDisplay != relay || len(p.Groups[0].Rows) != 1 {
		t.Fatal("OpenCode relay key was redirected to a vendor preset")
	}
	copied, err := f.s.Copy(context.Background(), "owner", p.ID, []string{p.Groups[0].Rows[0].ID})
	if err != nil || len(copied.Items) != 1 || copied.Items[0].URL != relay || copied.Items[0].APIKey != "synthetic-relay-auth" {
		t.Fatal("copy lost configured OpenCode key destination")
	}
	imported, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: p.Groups[0].Rows[0].ID}})
	if err != nil || len(imported.Results) != 1 || imported.Results[0].Added != 1 {
		t.Fatal("relay import failed")
	}
	channel := serviceChannel(t, f.st, imported.Results[0].ChannelID)
	if len(channel.Endpoints) != 1 || channel.Endpoints[0].BaseURL != relay {
		t.Fatal("stored key was sent to an unrelated default URL")
	}
}

func discoveryCodexJWT(t *testing.T, account string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"sub": "synthetic-shared-user", "exp": 2000000000, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func TestDiscoveryCodexStrongIdentityMergesFullAndRefreshOnlyWithoutRotation(t *testing.T) {
	for _, completeFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "complete-first", false: "complete-later"}[completeFirst], func(t *testing.T) {
			f := newServiceFixture(t)
			jwt := discoveryCodexJWT(t, "synthetic-shared-workspace")
			full := map[string]any{"tokens": map[string]any{"access_token": "synthetic-current-access", "refresh_token": "synthetic-current-refresh", "id_token": jwt, "account_id": "synthetic-shared-workspace"}}
			partial := map[string]any{"tokens": map[string]any{"refresh_token": "synthetic-current-refresh", "id_token": jwt, "account_id": "synthetic-shared-workspace"}}
			sources := []string{"source-codex", "source-ccswitch"}
			if completeFirst {
				f.source(t, "source-codex", map[string]any{"auth.json": full})
				f.source(t, "source-ccswitch", map[string]any{"codex_oauth_auth.json": map[string]any{"accounts": map[string]any{"local-only-id": map[string]any{"refresh_token": "synthetic-current-refresh", "id_token": jwt, "chatgpt_account_id": "synthetic-shared-workspace"}}}})
			} else {
				f.source(t, "source-codex", map[string]any{"auth.json": partial})
				f.source(t, "source-magpie", map[string]any{"logins.json": []any{map[string]any{"agent": "codex", "auth": full}}})
				sources = []string{"source-codex", "source-magpie"}
			}
			refreshes := 0
			transfer := oauthtransfer.New(f.st, func(ctx context.Context, kind string, o store.OAuthCredential) (store.OAuthCredential, error) {
				refreshes++
				o.RefreshToken = "synthetic-unnecessary-rotation"
				o.AccessToken = "synthetic-new-access"
				return o, nil
			})
			f.s.complete = transfer.CompleteImportItems
			p := f.scan(t, "owner", sources...)
			if len(p.Groups) != 1 || len(p.Groups[0].Rows) != 1 || len(p.Groups[0].Rows[0].Sources) != 2 {
				t.Fatal("strong user/workspace identity did not merge full and refresh-only copies")
			}
			result, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: p.Groups[0].Rows[0].ID}})
			if err != nil || len(result.Results) != 1 || result.Results[0].Added != 1 || refreshes != 0 {
				t.Fatal("complete representative was unnecessarily rotated or not imported")
			}
			channel := serviceChannel(t, f.st, result.Results[0].ChannelID)
			if len(channel.Credentials) != 1 || channel.Credentials[0].OAuth.RefreshToken != "synthetic-current-refresh" || channel.Credentials[0].OAuth.AccessToken != "synthetic-current-access" {
				t.Fatal("stored credential lost complete representative")
			}
		})
	}
}

func TestDiscoveryCodexSharedRefreshNeverMergesDifferentWorkspaces(t *testing.T) {
	for _, stable := range []bool{true, false} {
		t.Run(map[bool]string{true: "strong-identities", false: "no-stable-user"}[stable], func(t *testing.T) {
			f := newServiceFixture(t)
			auth := func(account string) map[string]any {
				tokens := map[string]any{"access_token": "synthetic-shared-access", "refresh_token": "synthetic-shared-refresh", "account_id": account}
				if stable {
					tokens["id_token"] = discoveryCodexJWT(t, account)
				}
				return map[string]any{"tokens": tokens}
			}
			f.source(t, "source-codex", map[string]any{"auth.json": auth("synthetic-workspace-a")})
			f.source(t, "source-magpie", map[string]any{"logins.json": []any{map[string]any{"agent": "codex", "auth": auth("synthetic-workspace-b")}}})
			p := f.scan(t, "owner", "source-codex", "source-magpie")
			if len(p.Groups) != 1 || len(p.Groups[0].Rows) != 2 {
				t.Fatal("different workspaces collapsed by shared refresh token")
			}
			parsed, err := oauthtransfer.ParseNativeCredentialsForType(context.Background(), store.CredentialCodexOAuth, mustDiscoveryJSON(t, auth("synthetic-workspace-a")))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.st.ImportOAuthCredentials(store.OAuthTransferImport{Items: parsed}, nil); err != nil {
				t.Fatal(err)
			}
			updated, err := f.s.Get(context.Background(), "owner", p.ID)
			if err != nil || len(updated.Groups) != 1 || len(updated.Groups[0].Rows) != 1 {
				t.Fatal("existing workspace hid another workspace sharing refresh material")
			}
		})
	}
}

func mustDiscoveryJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDiscoverySelectedDisabledOAuthCreatesEnabledMemberInDisabledChannel(t *testing.T) {
	f := newServiceFixture(t)
	root := f.source(t, "source-codex-manager", nil)
	db, err := sql.Open("sqlite", filepath.Join(root, "codexmanager.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE accounts (id TEXT, chatgpt_account_id TEXT, workspace_id TEXT, status TEXT)",
		"CREATE TABLE tokens (account_id TEXT, access_token TEXT, refresh_token TEXT, id_token TEXT)",
		"INSERT INTO accounts VALUES ('synthetic-local-id','synthetic-workspace','','disabled')",
		"INSERT INTO tokens VALUES ('synthetic-local-id','synthetic-access','synthetic-refresh','synthetic-id')",
	} {
		if _, err = db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	preview := f.scan(t, "owner", "source-codex-manager")
	if len(preview.Groups) != 1 || len(preview.Groups[0].Rows) != 1 || !preview.Groups[0].Rows[0].Disabled {
		t.Fatal("source disabled status was not retained in preview")
	}
	no := false
	result, err := f.s.Import(context.Background(), "owner", preview.ID, []Selection{{CandidateID: preview.Groups[0].Rows[0].ID, Enabled: &no}})
	if err != nil || len(result.Results) != 1 || result.Results[0].Added != 1 {
		t.Fatal("selected source-disabled OAuth could not be imported")
	}
	channel := serviceChannel(t, f.st, result.Results[0].ChannelID)
	if !channel.Enabled || len(channel.Credentials) != 1 || !channel.Credentials[0].Enabled || channel.Credentials[0].OAuth.RefreshToken != "synthetic-refresh" {
		t.Fatal("member enablement inherited source status or changed URL channel choice")
	}
}
