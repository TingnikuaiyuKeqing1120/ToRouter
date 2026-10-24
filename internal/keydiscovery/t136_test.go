package keydiscovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"torouter/internal/store"
)

func TestT136GlobalAlwaysCreatesAndReplays(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			f := newServiceFixture(t)
			const address = "https://t136.example.invalid/v1"
			for i := 0; i < count; i++ {
				serviceSaveChannel(t, f.st, store.Channel{Name: "Existing", Enabled: false, APIKey: "synthetic-existing-" + string(rune('a'+i)), Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: address}}})
			}
			f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("synthetic-t136-new", address, "chat"))})
			p := f.scan(t, "owner", "source-magpie")
			row := p.Groups[0].Rows[0]
			if p.Groups[0].CopyOnly || len(row.Targets) != 0 {
				t.Fatal("global discovery inferred existing destination")
			}
			selected := []Selection{{CandidateID: row.ID, Name: "New selected group", DisplayIcon: "codex"}}
			result, err := f.s.Import(context.Background(), "owner", p.ID, selected)
			if err != nil || result.Results[0].Added != 1 {
				t.Fatal("creation failed", err)
			}
			id := result.Results[0].ChannelID
			replay, err := f.s.Import(context.Background(), "owner", p.ID, selected)
			if err != nil || replay.Results[0].ChannelID != id {
				t.Fatal("retry did not reuse receipt", err)
			}
			all, _ := f.st.ListChannels()
			if len(all) != count+1 {
				t.Fatal("replay duplicated or append reused existing")
			}
			created := serviceChannel(t, f.st, id)
			if !created.Enabled || created.Name != "New selected group" || len(created.Models) != 0 {
				t.Fatal("new channel defaults incorrect")
			}
			for _, c := range all {
				if c.ID != id && (c.Enabled || len(c.Credentials) != 1) {
					t.Fatal("existing channel was modified")
				}
			}
			reopened, err := store.Open(f.storeRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			persisted, err := reopened.Channel(id)
			if err != nil || len(persisted.Credentials) != 1 {
				t.Fatal("created channel was not persisted")
			}
		})
	}
}

func TestT136ScopedDiscoveryCannotSwitchTargetAndFiltersType(t *testing.T) {
	f := newServiceFixture(t)
	target, err := store.BuiltinChannel("Target", store.CredentialCodexOAuth)
	if err != nil {
		t.Fatal(err)
	}
	serviceSaveChannel(t, f.st, target)
	channels, _ := f.st.ListChannels()
	target = channels[0]
	other := target
	other.ID = 0
	other.Name = "Other"
	other = serviceSaveChannel(t, f.st, other)
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("synthetic-api", target.Endpoints[0].BaseURL, "responses")), "logins.json": []any{map[string]any{"agent": "codex", "auth": map[string]any{"tokens": map[string]any{"access_token": "synthetic-scoped-access", "refresh_token": "synthetic-scoped-refresh", "account_id": "synthetic-scoped-workspace"}}}}})
	p := f.scanTarget(t, "owner", target, "source-magpie")
	if p.Target == nil || p.Target.ID != target.ID || len(p.Groups) != 1 || len(p.Groups[0].Rows) != 1 || p.Groups[0].Rows[0].CredentialType != store.CredentialCodexOAuth {
		t.Fatal("scoped preview leaked another type or lost target")
	}
	id := p.Groups[0].Rows[0].ID
	if _, err = f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: id, TargetChannelID: other.ID}}); !errors.Is(err, ErrConflict) {
		t.Fatal("selection switched target", err)
	}
	result, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: id}})
	if err != nil || result.Results[0].ChannelID != target.ID || result.Results[0].Added != 1 {
		t.Fatal("fixed empty target append failed", err)
	}
	if len(serviceChannel(t, f.st, other.ID).Credentials) != 0 || len(serviceChannel(t, f.st, target.ID).Credentials) != 1 {
		t.Fatal("append wrote another target")
	}
}

func TestT136CopilotOfficialFilesDeduplicateAndRespectFingerprint(t *testing.T) {
	f := newServiceFixture(t)
	f.source(t, "source-copilot", map[string]any{"apps.json": map[string]any{"github.com:synthetic-app": map[string]any{"oauth_token": "synthetic-github-token", "user": "synthetic-user", "githubAppId": "synthetic-app"}}, "hosts.json": map[string]any{"github.com": map[string]any{"oauth_token": "synthetic-github-token", "user": "synthetic-user"}}})
	p := f.scan(t, "owner", "source-copilot")
	if len(p.Groups) != 1 || len(p.Groups[0].Rows) != 1 || p.Groups[0].Rows[0].CredentialType != store.CredentialCopilotOAuth {
		t.Fatal("Copilot files not recognized or duplicate token exposed")
	}
	result, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: p.Groups[0].Rows[0].ID}})
	if err != nil || result.Results[0].Added != 1 {
		t.Fatal("Copilot import failed", err)
	}
	all, _ := f.st.ListChannels()
	if len(all) != 1 || all[0].Credentials[0].OAuth.GitHubToken != "synthetic-github-token" {
		t.Fatal("Copilot token did not persist as Github material")
	}
}

func TestT136MixedTypesPartialFailureRetriesOnlyPendingGroup(t *testing.T) {
	f := newServiceFixture(t)
	const address = "https://chatgpt.com/backend-api/codex"
	f.source(t, "source-magpie", map[string]any{"providers.json": serviceMagpie(serviceMagpieKey("synthetic-mixed-api", address, "responses")), "logins.json": []any{map[string]any{"agent": "codex", "auth": map[string]any{"tokens": map[string]any{"access_token": "synthetic-mixed-access", "refresh_token": "synthetic-mixed-refresh", "account_id": "synthetic-mixed-workspace"}}}}})
	p := f.scan(t, "owner", "source-magpie")
	if len(p.Groups) != 2 || p.Groups[0].ID == p.Groups[1].ID {
		t.Fatal("same URL with different types was mixed")
	}
	selections := []Selection{}
	for _, group := range p.Groups {
		selections = append(selections, Selection{CandidateID: group.Rows[0].ID})
	}
	calls := 0
	f.s.complete = func(_ context.Context, items []store.OAuthTransferItem) ([]store.OAuthTransferItem, error) {
		calls++
		if calls == 1 {
			return items, errors.New("synthetic temporary failure")
		}
		return items, nil
	}
	first, err := f.s.Import(context.Background(), "owner", p.ID, selections)
	if err != nil || first.OperationStatus() != "partial" || len(first.Preview.Groups) != 1 {
		t.Fatal("partial failure lost pending group", err)
	}
	second, err := f.s.Import(context.Background(), "owner", p.ID, selections)
	if err != nil || second.OperationStatus() != "completed" || len(second.Results) != 2 || calls != 2 {
		t.Fatal("retry did not finish pending group", err)
	}
	channels, _ := f.st.ListChannels()
	if len(channels) != 2 {
		t.Fatal("retry duplicated completed channel")
	}
	for _, c := range channels {
		if len(c.Credentials) != 1 {
			t.Fatal("retry duplicated credentials")
		}
	}
}

func TestT136CopilotSourceFingerprintAndDefaultRoot(t *testing.T) {
	f := newServiceFixture(t)
	root := f.source(t, "source-copilot", map[string]any{"hosts.json": map[string]any{"github.com": map[string]any{"oauth_token": "synthetic-original-token"}}})
	p := f.scan(t, "owner", "source-copilot")
	if err := os.WriteFile(filepath.Join(root, "hosts.json"), []byte(`{"github.com":{"oauth_token":"synthetic-changed-token"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Import(context.Background(), "owner", p.ID, []Selection{{CandidateID: p.Groups[0].Rows[0].ID}}); !errors.Is(err, ErrConflict) {
		t.Fatal("modified Copilot source imported", err)
	}
	channels, _ := f.st.ListChannels()
	if len(channels) != 0 {
		t.Fatal("source conflict persisted credentials")
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	resolved, err := defaultRoot("source-copilot")
	if err != nil || resolved != filepath.Join(xdg, "github-copilot") {
		t.Fatal("official absolute XDG override not respected")
	}
}
