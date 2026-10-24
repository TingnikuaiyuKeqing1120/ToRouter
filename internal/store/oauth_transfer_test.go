package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"torouter/internal/compatprofile"
)

func oauthTransferTestItem(t *testing.T, kind, account string) OAuthTransferItem {
	t.Helper()
	updated := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	o := &OAuthCredential{AccessToken: "synthetic-access-" + account, RefreshToken: "synthetic-refresh-" + account, IDToken: "synthetic-id-" + account, ExpiresAt: updated.Add(time.Hour), UpdatedAt: updated, AccountLabel: "Synthetic display label", Status: OAuthStatusActive, StatusReason: "synthetic-preserved-reason"}
	endpoints := []Endpoint{{Format: FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex", State: "stateless"}}
	switch kind {
	case CredentialCopilotOAuth:
		o.Provider = OAuthProviderCopilot
		o.GitHubToken = "synthetic-github-" + account
		o.RefreshToken = ""
		o.IDToken = ""
		o.APIBase = "https://api.githubcopilot.com"
		endpoints = []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: o.APIBase}, {Format: FormatAnthropicMessages, BaseURL: o.APIBase}}
	case CredentialChatGPTOAuth:
		o.Provider = OAuthProviderChatGPT
		o.ClientID = "oaiapp_fixture_" + account
		o.HostID = "urn:uuid:00000000-0000-4000-8000-000000000001"
		o.Subject = "synthetic-subject-" + account
		o.Scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
		o.APIBase = "https://api.openai.com/v1"
		endpoints = []Endpoint{{Format: FormatOpenAIResponses, BaseURL: o.APIBase, State: "stateless"}}
	case CredentialCodexOAuth:
		o.Provider = OAuthProviderCodex
		o.ClientID = "synthetic-codex-client"
		o.Account = "synthetic-account-" + account
		o.Subject = "synthetic-subject-" + account
		o.Scopes = ScopeCodexAuthorize
		o.APIBase = endpoints[0].BaseURL
	default:
		if IsNativeOAuth(kind) {
			c := nativeTestChannel(kind, "synthetic-account-"+account)
			o = c.Credentials[0].OAuth
			endpoints = c.Endpoints
			o.UpdatedAt = updated
			o.IDToken = "synthetic-id-" + account
			o.APIBase = endpoints[0].BaseURL
			o.AccountLabel = "Synthetic native label"
			o.Status = OAuthStatusActive
			o.StatusReason = "synthetic-preserved-reason"
		} else if profile, ok := compatprofile.Lookup(kind); ok {
			state := map[string]any{"type": "oauth", "uid": "synthetic-uid-" + account, "access": "synthetic-plugin-access-" + account, "refresh": "synthetic-plugin-refresh-" + account, "expires": 1900000000000, "unknown": map[string]any{"private": "synthetic-opaque-" + account}}
			switch profile.Package {
			case "qoder":
				state["deviceToken"] = "synthetic-device-" + account
				state["deviceRefresh"] = "synthetic-device-refresh-" + account
			case "workbuddy":
				state["sessionID"] = "00000000-0000-4000-8000-000000000001"
				state["domain"] = "workspace.example.invalid"
				state["enterpriseId"] = "synthetic-enterprise"
			case "minimax":
				state["sessionID"] = "00000000-0000-4000-8000-000000000001"
				state["realUserID"] = state["uid"]
			}
			raw, _ := json.Marshal(state)
			credential, err := CompatibleOAuthCredential(kind, string(raw))
			if err != nil {
				t.Fatal(err)
			}
			o = &credential
			o.AccountLabel = "Synthetic compatible label"
			o.StatusReason = "synthetic-preserved-reason"
			o.UpdatedAt = updated
			endpoints = []Endpoint{{Format: profile.Format(), BaseURL: profile.API, State: "stateless"}}
		} else {
			t.Fatal("unknown transfer fixture type")
		}
	}
	item := OAuthTransferItem{CredentialType: kind, Label: "Synthetic member " + account, Enabled: true, Endpoints: endpoints, OAuth: o}
	if err := ValidateOAuthTransferItem(item); err != nil {
		t.Fatal("valid fixture rejected", kind, err)
	}
	return item
}
func oauthTransferSource(t *testing.T, st *Store, item OAuthTransferItem) Channel {
	t.Helper()
	c := Channel{Name: "Source configuration", CredentialType: item.CredentialType, Enabled: item.Enabled, Models: []string{"source-model"}, AvailableModels: []string{"source-model"}, Endpoints: append([]Endpoint(nil), item.Endpoints...), Credentials: []CredentialItem{{ID: "source-original-member", Label: item.Label, Enabled: item.Enabled, OAuth: item.OAuth}}}
	if c.CredentialType == CredentialCodexOAuth {
		ops := DefaultCredentialOperationsConfig()
		ops.Warmup.Enabled = true
		ops.Warmup.Model = "source-model"
		remaining := float64(20)
		ops.AutoReset.Enabled = true
		ops.AutoReset.Rules = []CredentialResetRule{{WindowSeconds: 3600, RemainingBelow: &remaining, Join: "or"}}
		c.Credentials[0].Operations = &ops
	}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func oauthTransferTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal("fixture store unavailable")
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOAuthTransferCrossInstanceFullMaterialRoundtripAllTypes(t *testing.T) {
	kinds := []string{CredentialCodexOAuth, CredentialChatGPTOAuth, CredentialCopilotOAuth, CredentialClaudeOAuth, CredentialAntigravityOAuth, CredentialDevinOAuth}
	for _, profile := range compatprofile.Profiles() {
		kinds = append(kinds, profile.CredentialType)
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			source := oauthTransferTestStore(t)
			item := oauthTransferTestItem(t, kind, "roundtrip")
			if kind == CredentialDevinOAuth {
				item.Enabled = false
			}
			channel := oauthTransferSource(t, source, item)
			exported, err := source.ExportOAuthCredentials([]OAuthTransferRef{{ChannelID: channel.ID, CredentialID: channel.Credentials[0].ID}})
			if err != nil || len(exported) != 1 {
				t.Fatal("exact export failed", err)
			}
			if !reflect.DeepEqual(exported[0].OAuth, channel.Credentials[0].OAuth) || exported[0].Enabled != channel.Credentials[0].Enabled || exported[0].Label != channel.Credentials[0].Label {
				t.Fatal("export discarded material or member settings")
			}
			// This serialization is only an in-memory transport fixture. File encryption
			// and its password/session boundary are exercised by the owning service.
			raw, err := json.Marshal(exported)
			if err != nil {
				t.Fatal(err)
			}
			var decoded []OAuthTransferItem
			if err = json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			var fields []map[string]any
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"models", "available_models", "operations", "model_configs", "prices", "bindings"} {
				if _, ok := fields[0][name]; ok {
					t.Fatal("source configuration leaked into transfer items")
				}
			}
			destinationDir := t.TempDir()
			destination, err := Open(destinationDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if destination != nil {
					destination.Close()
				}
			}()
			localHost, err := destination.HostID()
			if err != nil {
				t.Fatal(err)
			}
			result, err := destination.ImportOAuthCredentials(OAuthTransferImport{Name: "Imported OAuth", Items: decoded}, nil)
			if err != nil || result.Added != 1 || result.Skipped != 0 {
				t.Fatal("cross-instance import failed", err)
			}
			imported, err := destination.Channel(result.ChannelID)
			if err != nil {
				t.Fatal(err)
			}
			if imported.Credentials[0].ID == channel.Credentials[0].ID || !strings.HasPrefix(imported.Credentials[0].ID, "cred_") {
				t.Fatal("source credential ID was reused")
			}
			if !reflect.DeepEqual(imported.Credentials[0].OAuth, exported[0].OAuth) || imported.Credentials[0].Label != item.Label || imported.Credentials[0].Enabled != item.Enabled {
				t.Fatal("import changed complete authorization material or member state")
			}
			if imported.Enabled != item.Enabled || len(imported.Models) != 0 || len(imported.AvailableModels) != 0 || imported.Credentials[0].Operations != nil {
				t.Fatal("import copied source models or automatic actions")
			}
			if host, err := destination.HostID(); err != nil || host != localHost {
				t.Fatal("migration replaced destination host registration")
			}
			routes, err := destination.ListDownstreamModels()
			if err != nil || len(routes) != 0 {
				t.Fatal("migration created routing")
			}
			var stored string
			if err = destination.db.QueryRow("SELECT data FROM channels WHERE id=?", result.ChannelID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			resultJSON, _ := json.Marshal(result)
			for _, secret := range []string{item.OAuth.AccessToken, item.OAuth.RefreshToken, item.OAuth.IDToken, item.OAuth.GitHubToken, item.OAuth.OrganizationID, item.OAuth.DeviceID, item.OAuth.ProjectID, item.OAuth.PluginState, "synthetic-opaque-roundtrip"} {
				if secret != "" && (strings.Contains(stored, secret) || strings.Contains(string(resultJSON), secret)) {
					t.Fatal("sensitive material escaped at-rest sealing or import result")
				}
			}
			if err = destination.Close(); err != nil {
				t.Fatal(err)
			}
			destination, err = Open(destinationDir)
			if err != nil {
				t.Fatal(err)
			}
			again, err := destination.ExportOAuthCredentials([]OAuthTransferRef{{ChannelID: result.ChannelID, CredentialID: imported.Credentials[0].ID}})
			if err != nil || !reflect.DeepEqual(again, exported) {
				t.Fatal("restart lost complete migration material", err)
			}
			replay, err := destination.ImportOAuthCredentials(OAuthTransferImport{Name: "Replay must not create", Items: decoded}, nil)
			if err != nil || replay.Added != 0 || replay.Skipped != 1 || replay.ChannelID != result.ChannelID {
				t.Fatal("migration replay was not idempotent", err)
			}
			channels, _ := destination.ListChannels()
			if len(channels) != 1 {
				t.Fatal("replay created an empty or duplicate channel")
			}
		})
	}
}

func TestOAuthTransferBatchAppendKeepsOldPoolMaterialAndConfiguration(t *testing.T) {
	st := oauthTransferTestStore(t)
	one, two := oauthTransferTestItem(t, CredentialCodexOAuth, "one"), oauthTransferTestItem(t, CredentialCodexOAuth, "two")
	c := oauthTransferSource(t, st, one)
	c.Credentials = append(c.Credentials, CredentialItem{ID: "retained-second", Label: "Retained label", Enabled: false, OAuth: two.OAuth})
	c.Enabled = false
	c.Tags = []string{"privacy:zdr", "billing:payg"}
	c.ModelTags = map[string][]string{"source-model": {"billing:payg"}}
	limit := int64(2048)
	c.ModelConfigs = map[string]ModelConfig{"source-model": {MaxOutputTokens: &limit}}
	c.ModelEvidence = map[string]CapabilityEvidence{"source-model": {Source: "manual", APIFormat: FormatOpenAIResponses}}
	c.ModelFormats = map[string][]string{"source-model": {FormatOpenAIResponses}}
	c.ManualModels = []string{"source-model"}
	c.PreferredEndpoint = FormatOpenAIResponses
	c.DispatchMode = DispatchModeRoundRobin
	c.ChainType = ChainTypeCyclic
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDownstreamModel(DownstreamModel{ID: "retained-route", DisplayName: "Retained routing", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "source-model", Enabled: true, Priority: 8}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePrices([]Price{{ChannelID: c.ID, Model: "source-model", Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000000}}}}); err != nil {
		t.Fatal(err)
	}
	before, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	prices, _ := st.GetPrices()
	routes, _ := st.ListDownstreamModels()
	stale := copyOAuthTransferItem(two)
	stale.OAuth.AccessToken = "synthetic-stale-access"
	stale.OAuth.RefreshToken = "synthetic-stale-refresh"
	stale.Label = "Do not replace retained label"
	stale.Enabled = true
	third, fourth := oauthTransferTestItem(t, CredentialCodexOAuth, "third"), oauthTransferTestItem(t, CredentialCodexOAuth, "fourth")
	third.Enabled = false
	guards := 0
	result, err := st.ImportOAuthCredentials(OAuthTransferImport{ChannelID: c.ID, ExpectedVersion: EntityVersion(before), Name: "Do not rename", Items: []OAuthTransferItem{stale, third, fourth}}, func() error { guards++; _, err := st.Channel(c.ID); return err })
	if err != nil || result.Added != 2 || result.Skipped != 1 || guards != 1 {
		t.Fatal("atomic pool append failed", err)
	}
	after, _ := st.Channel(c.ID)
	if len(after.Credentials) != 4 || !reflect.DeepEqual(after.Credentials[:2], before.Credentials) || after.Credentials[2].Enabled || !after.Credentials[3].Enabled {
		t.Fatal("append rewrote old rotation/labels/order/enablement")
	}
	normalized := after
	normalized.Credentials = before.Credentials
	normalized.Generation = before.Generation
	if !reflect.DeepEqual(normalized, before) {
		t.Fatal("append changed non-target fields")
	}
	afterPrices, _ := st.GetPrices()
	afterRoutes, _ := st.ListDownstreamModels()
	if !reflect.DeepEqual(prices, afterPrices) || !reflect.DeepEqual(routes, afterRoutes) {
		t.Fatal("append changed prices/routing")
	}
	for _, member := range after.Credentials[2:] {
		if member.Operations != nil || member.OperationsRevision != 0 {
			t.Fatal("new imported member inherited automatic actions")
		}
	}
	if !reflect.DeepEqual(after.Credentials[2].OAuth, third.OAuth) || !reflect.DeepEqual(after.Credentials[3].OAuth, fourth.OAuth) {
		t.Fatal("append dropped new material")
	}
	// A matching account in a secondary member of another channel still owns it.
	exported, err := st.ExportOAuthCredentials([]OAuthTransferRef{{ChannelID: c.ID, CredentialID: after.Credentials[3].ID}, {ChannelID: c.ID, CredentialID: after.Credentials[1].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if exported[0].OAuth.Account != fourth.OAuth.Account || exported[1].OAuth.Account != two.OAuth.Account {
		t.Fatal("exact export fell back to the primary member")
	}
	skipped, err := st.ImportOAuthCredentials(OAuthTransferImport{Name: "No new channel", Items: exported}, nil)
	if err != nil || skipped.Added != 0 || skipped.Skipped != 2 || skipped.ChannelID != c.ID {
		t.Fatal("global pool deduplication missed secondary members")
	}
}

func TestOAuthTransferEveryLegacyPoolMemberIsValidated(t *testing.T) {
	for _, kind := range []string{CredentialCodexOAuth, CredentialChatGPTOAuth, CredentialCopilotOAuth} {
		t.Run(kind, func(t *testing.T) {
			st := oauthTransferTestStore(t)
			good := oauthTransferTestItem(t, kind, "good")
			bad := oauthTransferTestItem(t, kind, "bad")
			bad.OAuth.Provider = "synthetic-invalid-provider"
			if ValidateOAuthTransferItem(bad) == nil || OAuthTransferIdentity(bad) != "" {
				t.Fatal("invalid secondary authorization produced an identity")
			}
			if _, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{good, bad}}, nil); !errors.Is(err, ErrInvalidOAuthTransfer) {
				t.Fatal("bad second item bypassed primary-only validation", err)
			}
			channels, _ := st.ListChannels()
			if len(channels) != 0 {
				t.Fatal("invalid batch partially wrote the first item")
			}
			// Older SaveChannel validation examines the first member, so exercise the
			// export boundary with a malformed non-primary stored member as well.
			source := oauthTransferSource(t, st, good)
			source.Credentials = append(source.Credentials, CredentialItem{ID: "malformed-second", Enabled: true, OAuth: bad.OAuth})
			if err := st.SaveChannel(&source); err != nil {
				t.Fatal(err)
			}
			if items, err := st.ExportOAuthCredentials([]OAuthTransferRef{{ChannelID: source.ID, CredentialID: source.Credentials[0].ID}, {ChannelID: source.ID, CredentialID: "malformed-second"}}); !errors.Is(err, ErrInvalidOAuthTransfer) || items != nil {
				t.Fatal("export returned a partial or invalid member batch")
			}
			if _, err := st.ExportOAuthCredentials([]OAuthTransferRef{{ChannelID: source.ID, CredentialID: ""}}); !errors.Is(err, ErrInvalidOAuthTransfer) {
				t.Fatal("export guessed primary member")
			}
			if _, err := st.ExportOAuthCredentials([]OAuthTransferRef{{ChannelID: source.ID, CredentialID: "absent"}}); !errors.Is(err, ErrNotFound) {
				t.Fatal("export guessed a missing pool member")
			}
		})
	}
}

func TestOAuthTransferSIWCRegistrationPreservedAndSourceDomainScoped(t *testing.T) {
	st := oauthTransferTestStore(t)
	original := oauthTransferTestItem(t, CredentialChatGPTOAuth, "same-user")
	created, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{original}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.Channel(created.ChannelID)
	moved := copyOAuthTransferItem(original)
	moved.OAuth.ClientID = "oaiapp_other_registration"
	moved.OAuth.HostID = "urn:uuid:00000000-0000-4000-8000-000000000002"
	moved.OAuth.AccessToken = "synthetic-other-access"
	moved.OAuth.RefreshToken = "synthetic-other-refresh"
	moved.OAuth.Scopes = "openid profile"
	moved.OAuth.APIBase = "HTTPS://API.OPENAI.COM:443/v2"
	moved.Label = "Do not replace current owner"
	if OAuthTransferIdentity(moved) != OAuthTransferIdentity(original) {
		t.Fatal("registration/path/default-port/scope differences invented a new account")
	}
	skipped, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{moved}}, nil)
	if err != nil || skipped.Skipped != 1 || skipped.Added != 0 {
		t.Fatal("same Subject replaced or duplicated SIWC owner", err)
	}
	after, _ := st.Channel(created.ChannelID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration overwrote an existing SIWC registration/rotation")
	}
	otherDomain := copyOAuthTransferItem(moved)
	otherDomain.OAuth.APIBase = "https://auth.openai.com/v1"
	otherDomain.Endpoints = []Endpoint{{Format: FormatOpenAIResponses, BaseURL: otherDomain.OAuth.APIBase, State: "stateless"}}
	if OAuthTransferIdentity(otherDomain) == OAuthTransferIdentity(original) {
		t.Fatal("same Subject crossed provider API authority")
	}
	distinct, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{otherDomain}}, nil)
	if err != nil || distinct.Added != 1 {
		t.Fatal("different source domain was conflated", err)
	}
	incomplete := copyOAuthTransferItem(original)
	incomplete.OAuth.Subject = ""
	if ValidateOAuthTransferItem(incomplete) == nil {
		t.Fatal("SIWC without stable Subject accepted")
	}
}

func TestOAuthTransferOpaqueFallbacksDoNotGuessAccountsFromLabels(t *testing.T) {
	for _, kind := range []string{CredentialCopilotOAuth, CredentialDevinOAuth, CredentialCodexOAuth} {
		t.Run(kind, func(t *testing.T) {
			st := oauthTransferTestStore(t)
			first := oauthTransferTestItem(t, kind, "fallback")
			if kind != CredentialCopilotOAuth {
				first.OAuth.Account = ""
				first.OAuth.Subject = ""
			}
			created, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{first}}, nil)
			if err != nil || created.Added != 1 {
				t.Fatal(err)
			}
			same := copyOAuthTransferItem(first)
			same.Label = "Different display name"
			same.OAuth.AccountLabel = "Other display label"
			skipped, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{same}}, nil)
			if err != nil || skipped.Skipped != 1 {
				t.Fatal("identical full token/session not deduplicated")
			}
			other := copyOAuthTransferItem(first)
			switch kind {
			case CredentialCopilotOAuth:
				other.OAuth.GitHubToken += "-different"
			case CredentialDevinOAuth:
				other.OAuth.AccessToken += "-different"
			case CredentialCodexOAuth:
				other.OAuth.RefreshToken += "-different"
			}
			distinct, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{other}}, nil)
			if err != nil || distinct.Added != 1 {
				t.Fatal("display label was guessed as account identity", err)
			}
		})
	}
	// A token-only Codex copy also matches an existing stable account carrying
	// that exact refresh token, even though its primary identity uses Account.
	st := oauthTransferTestStore(t)
	stable := oauthTransferTestItem(t, CredentialCodexOAuth, "stable")
	if _, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{stable}}, nil); err != nil {
		t.Fatal(err)
	}
	anonymous := copyOAuthTransferItem(stable)
	anonymous.OAuth.Account = ""
	anonymous.OAuth.Subject = ""
	if result, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{anonymous}}, nil); err != nil || result.Skipped != 1 {
		t.Fatal("fallback failed to compare the complete existing token")
	}
	// A skipped account copy can still establish that its complete token-only
	// copy later in the same batch belongs to that already-owned account.
	rotated := copyOAuthTransferItem(stable)
	rotated.OAuth.RefreshToken += "-rotated"
	anonymousRotated := copyOAuthTransferItem(rotated)
	anonymousRotated.OAuth.Account = ""
	anonymousRotated.OAuth.Subject = ""
	if result, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{rotated, anonymousRotated}}, nil); err != nil || result.Added != 0 || result.Skipped != 2 {
		t.Fatal("skipped account copy lost its complete token identity")
	}
	if result, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{anonymousRotated, rotated}}, nil); err != nil || result.Added != 0 || result.Skipped != 2 {
		t.Fatal("anonymous-first order bypassed stable account ownership")
	}
	if len(OAuthTransferIdentityAliases(rotated)) < 2 || len(OAuthTransferIdentityAliases(anonymousRotated)) == 0 {
		t.Fatal("validated alias evidence unavailable")
	}
	invalid := copyOAuthTransferItem(rotated)
	invalid.OAuth.Provider = "invalid"
	if OAuthTransferIdentityAliases(invalid) != nil {
		t.Fatal("invalid material yielded alias evidence")
	}
	for _, reverse := range []bool{false, true} {
		empty := oauthTransferTestStore(t)
		batch := []OAuthTransferItem{rotated, anonymousRotated}
		if reverse {
			batch[0], batch[1] = batch[1], batch[0]
		}
		if result, err := empty.ImportOAuthCredentials(OAuthTransferImport{Items: batch}, nil); err != nil || result.Added != 1 || result.Skipped != 1 {
			t.Fatal("new alias component was not imported once in both orders")
		}
		channels, err := empty.ListChannels()
		if err != nil || len(channels) != 1 || channels[0].Credentials[0].OAuth.Account != rotated.OAuth.Account {
			t.Fatal("anonymous-first order discarded the available stable account identity")
		}
	}
}

func TestOAuthTransferTargetVersionRevocationAndValidationAreAtomicAndSafe(t *testing.T) {
	st := oauthTransferTestStore(t)
	item := oauthTransferTestItem(t, CredentialClaudeOAuth, "target")
	created, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{item}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.Channel(created.ChannelID)
	incoming := oauthTransferTestItem(t, CredentialClaudeOAuth, "incoming")
	input := OAuthTransferImport{ChannelID: before.ID, ExpectedVersion: EntityVersion(before), Items: []OAuthTransferItem{incoming}}
	stale := input
	stale.ExpectedVersion = "stale"
	if _, err := st.ImportOAuthCredentials(stale, nil); !errors.Is(err, ErrEditConflict) {
		t.Fatal("old target version accepted")
	}
	wrong := input
	wrong.Items = []OAuthTransferItem{oauthTransferTestItem(t, CredentialAntigravityOAuth, "wrong")}
	if _, err := st.ImportOAuthCredentials(wrong, nil); !errors.Is(err, ErrInvalidOAuthTransfer) {
		t.Fatal("heterogeneous target accepted")
	}
	if _, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{incoming, wrong.Items[0]}}, nil); !errors.Is(err, ErrInvalidOAuthTransfer) {
		t.Fatal("heterogeneous new destination accepted")
	}
	changed := copyOAuthTransferItem(incoming)
	changed.Endpoints[0].State = "stateless"
	wrong.Items = []OAuthTransferItem{changed}
	if _, err := st.ImportOAuthCredentials(wrong, nil); !errors.Is(err, ErrInvalidOAuthTransfer) {
		t.Fatal("different endpoint declaration accepted")
	}
	denied := errors.New("synthetic-private-token in synthetic-private-path")
	if _, err := st.ImportOAuthCredentials(input, func() error { return denied }); !errors.Is(err, denied) || !errors.Is(err, ErrOAuthTransferDenied) || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("revocation bypassed or private error surfaced")
	}
	after, _ := st.Channel(before.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed append changed target")
	}
	if _, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{incoming}}, func() error { return denied }); !errors.Is(err, ErrOAuthTransferDenied) {
		t.Fatal("new destination bypassed guard")
	}
	channels, _ := st.ListChannels()
	if len(channels) != 1 {
		t.Fatal("denied migration created a channel")
	}
	bad := copyOAuthTransferItem(incoming)
	bad.Endpoints[0].BaseURL = "https://synthetic-private-token@unapproved.example.invalid/private"
	if err := ValidateOAuthTransferItem(bad); !errors.Is(err, ErrInvalidOAuthTransfer) || strings.Contains(err.Error(), "synthetic-private-token") {
		t.Fatal("validation error exposed endpoint credentials")
	}
	public := OAuthTransferItem{CredentialType: CredentialZenFree, Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://opencode.ai/zen/v1"}}, OAuth: &OAuthCredential{AccessToken: "public"}}
	if ValidateOAuthTransferItem(public) == nil {
		t.Fatal("public credential accepted as OAuth migration")
	}
	emptyRefs := []OAuthTransferRef{}
	if _, err := st.ExportOAuthCredentials(emptyRefs); !errors.Is(err, ErrInvalidOAuthTransfer) {
		t.Fatal("empty export accepted")
	}
}

func TestOAuthTransferConcurrentReplayWritesOneOwner(t *testing.T) {
	st := oauthTransferTestStore(t)
	item := oauthTransferTestItem(t, compatprofile.MiniMaxCredential, "concurrent")
	const count = 4
	var wg sync.WaitGroup
	results := make(chan OAuthTransferImportResult, count)
	failures := make(chan error, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{item}}, nil)
			results <- result
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	added, skipped, id := 0, 0, int64(0)
	for result := range results {
		added += result.Added
		skipped += result.Skipped
		if id != 0 && id != result.ChannelID {
			t.Fatal("concurrent replay selected multiple owners")
		}
		id = result.ChannelID
	}
	channels, _ := st.ListChannels()
	if added != 1 || skipped != count-1 || len(channels) != 1 || len(channels[0].Credentials) != 1 {
		t.Fatal("concurrent replay duplicated owner or material")
	}
}

func TestOAuthTransferInheritedEndpointRetainsExistingOwnership(t *testing.T) {
	st := oauthTransferTestStore(t)
	item := oauthTransferTestItem(t, CredentialCodexOAuth, "inherited")
	created, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{item}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the stored ciphertext while representing a current endpoint
	// through the supported channel-level address inheritance contract.
	var raw string
	if err := st.db.QueryRow("SELECT data FROM channels WHERE id=?", created.ChannelID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored Channel
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	stored.BaseURL = stored.Endpoints[0].BaseURL
	stored.Endpoints[0].BaseURL = ""
	stored.Credentials[0].OAuth.APIBase = ""
	changed, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("UPDATE channels SET data=? WHERE id=?", string(changed), created.ChannelID); err != nil {
		t.Fatal(err)
	}
	result, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{item}}, nil)
	if err != nil || result.Added != 0 || result.Skipped != 1 || result.ChannelID != created.ChannelID {
		t.Fatal("inherited endpoint lost existing account ownership")
	}
	exported, err := st.ExportOAuthCredentials([]OAuthTransferRef{{ChannelID: created.ChannelID, CredentialID: stored.Credentials[0].ID}})
	if err != nil || len(exported) != 1 || exported[0].Endpoints[0].BaseURL != stored.BaseURL || exported[0].OAuth.APIBase != "" {
		t.Fatal("export lost effective endpoint or changed original OAuth material")
	}
	channels, err := st.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatal("inherited endpoint replay created a second owner")
	}
}
