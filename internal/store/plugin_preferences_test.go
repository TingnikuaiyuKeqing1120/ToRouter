package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func pluginPreferenceDefaults() map[string]map[string]bool {
	return map[string]map[string]bool{
		"codex": {"connect": true}, "copilot": {"connect": false}, "chatgpt": {"connect": false},
	}
}

func pluginPreferenceLegacyTypes() map[string]string {
	return map[string]string{"codex": CredentialCodexOAuth, "copilot": CredentialCopilotOAuth, "chatgpt": CredentialChatGPTOAuth}
}

func pluginPreferenceTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPluginPreferencesDefaultsAndCopies(t *testing.T) {
	s := pluginPreferenceTestStore(t)
	got, err := s.GetPluginPreferences()
	if err != nil || got.Plugins == nil || len(got.Plugins) != 0 {
		t.Fatalf("uninitialized preferences: %+v, %v", got, err)
	}
	defaults := pluginPreferenceDefaults()
	got, err = s.InitPluginPreferences(defaults, pluginPreferenceLegacyTypes())
	if err != nil || !reflect.DeepEqual(got.Plugins, defaults) {
		t.Fatalf("defaults: %+v, %v", got, err)
	}
	defaults["codex"]["connect"] = false
	defaults["external"] = map[string]bool{"connect": true}
	got.Plugins["copilot"]["connect"] = true
	delete(got.Plugins, "codex")
	got, err = s.GetPluginPreferences()
	if err != nil || !reflect.DeepEqual(got.Plugins, pluginPreferenceDefaults()) {
		t.Fatalf("caller mutated stored preferences: %+v, %v", got, err)
	}
	got.Plugins["chatgpt"]["connect"] = true
	got, err = s.SetPluginCapability("codex", "connect", false)
	if err != nil || got.Plugins["chatgpt"]["connect"] {
		t.Fatalf("get result aliased stored preferences: %+v, %v", got, err)
	}
	got.Plugins["codex"]["connect"] = true
	got, err = s.GetPluginPreferences()
	if err != nil || got.Plugins["codex"]["connect"] {
		t.Fatalf("set result aliased stored preferences: %+v, %v", got, err)
	}
}

func TestPluginPreferencesLegacyInheritanceAndReopen(t *testing.T) {
	for id, credentialType := range pluginPreferenceLegacyTypes() {
		t.Run(id, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			provider, base := OAuthProviderCodex, "https://chatgpt.com/backend-api/codex"
			if id == "copilot" {
				provider, base = OAuthProviderCopilot, "https://api.githubcopilot.com"
			}
			if id == "chatgpt" {
				provider, base = OAuthProviderChatGPT, "https://api.openai.com/v1"
			}
			channel := Channel{Name: "Synthetic OAuth", CredentialType: credentialType, Enabled: false,
				Models:    []string{"synthetic-upstream"},
				Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: base}},
				OAuth: &OAuthCredential{Provider: provider, RefreshToken: "synthetic-refresh", GitHubToken: "synthetic-github",
					ClientID: "oaiapp_synthetic", HostID: "synthetic-host", Status: OAuthStatusActive},
			}
			if id == "copilot" {
				channel.Endpoints[0].Format = FormatOpenAIChatCompletions
			}
			if err = s.SaveChannel(&channel); err != nil {
				t.Fatal(err)
			}
			// Include a real binding and compare stored JSON byte-for-byte.
			model := DownstreamModel{ID: "synthetic-model", Bindings: []Binding{{ChannelID: channel.ID, Model: "synthetic-upstream", Enabled: true}}}
			if err = s.SaveDownstreamModel(model); err != nil {
				t.Fatal(err)
			}
			snapshot := func() []string {
				t.Helper()
				out := []string{}
				for _, query := range []string{"SELECT data FROM channels ORDER BY id", "SELECT data FROM downstream_models ORDER BY id", "SELECT data FROM settings ORDER BY id"} {
					rows, e := s.db.Query(query)
					if e != nil {
						t.Fatal(e)
					}
					for rows.Next() {
						var raw string
						if e = rows.Scan(&raw); e != nil {
							t.Fatal(e)
						}
						out = append(out, raw)
					}
					e = rows.Err()
					rows.Close()
					if e != nil {
						t.Fatal(e)
					}
				}
				return out
			}
			before := snapshot()
			defaults := map[string]map[string]bool{id: {"connect": false, "discover": false}}
			got, err := s.InitPluginPreferences(defaults, pluginPreferenceLegacyTypes())
			if err != nil || !got.Plugins[id]["connect"] || got.Plugins[id]["discover"] {
				t.Fatalf("disabled legacy channel inheritance: %+v, %v", got, err)
			}
			if _, err = s.SetPluginCapability(id, "connect", false); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("preferences rewrote channel, binding or settings data")
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defaults[id]["connect"] = true
			got, err = s.InitPluginPreferences(defaults, pluginPreferenceLegacyTypes())
			if err != nil || got.Plugins[id]["connect"] {
				t.Fatalf("explicit false lost after reopen: %+v, %v", got, err)
			}
		})
	}
}

func TestPluginPreferencesAppendAndExactPatchPreserveUnknown(t *testing.T) {
	s := pluginPreferenceTestStore(t)
	initial := `{"plugins":{"retired":{"custom":false},"codex":{"connect":false,"custom":true}},"future":{"value":7}}`
	if _, err := s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?)", pluginPreferencesKey, initial); err != nil {
		t.Fatal(err)
	}
	defaults := pluginPreferenceDefaults()
	defaults["new-plugin"] = map[string]bool{"connect": true}
	defaults["codex"]["discover"] = false
	got, err := s.InitPluginPreferences(defaults, pluginPreferenceLegacyTypes())
	if err != nil || got.Plugins["codex"]["connect"] || !got.Plugins["codex"]["custom"] || !got.Plugins["new-plugin"]["connect"] {
		t.Fatalf("append: %+v, %v", got, err)
	}
	want := got.Plugins
	want["copilot"]["connect"] = true
	got, err = s.SetPluginCapability("copilot", "connect", true)
	if err != nil || !reflect.DeepEqual(got.Plugins, want) {
		t.Fatalf("exact patch: %+v, %v", got, err)
	}
	if value, exists := got.Plugins["retired"]["custom"]; !exists || value {
		t.Fatal("unknown plugin choice lost")
	}
	var raw string
	if err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", pluginPreferencesKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &fields); err != nil || string(fields["future"]) != `{"value":7}` {
		t.Fatalf("unknown resource field lost: %v", err)
	}
}

func TestPluginPreferencesConcurrentInitAndPatches(t *testing.T) {
	s := pluginPreferenceTestStore(t)
	const count = 40
	start, results := make(chan struct{}), make(chan error, count*2)
	for i := 0; i < count; i++ {
		go func(i int) {
			<-start
			_, err := s.SetPluginCapability("shared", fmt.Sprintf("cap-%d", i), true)
			results <- err
		}(i)
		go func(i int) {
			<-start
			_, err := s.InitPluginPreferences(map[string]map[string]bool{fmt.Sprintf("plugin-%d", i): {"connect": false}, "shared": {fmt.Sprintf("cap-%d", i): false}}, nil)
			results <- err
		}(i)
	}
	close(start)
	for i := 0; i < count*2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetPluginPreferences()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if !got.Plugins["shared"][fmt.Sprintf("cap-%d", i)] {
			t.Fatalf("lost capability patch %d", i)
		}
		if value, exists := got.Plugins[fmt.Sprintf("plugin-%d", i)]["connect"]; !exists || value {
			t.Fatalf("lost initialized plugin %d", i)
		}
	}
}

func TestPluginPreferencesRejectInvalidInputAndCorruption(t *testing.T) {
	s := pluginPreferenceTestStore(t)
	for _, name := range []string{"", "*", " ", "Codex", "a.b", "a/b", "a\n", strings.Repeat("a", 65)} {
		if _, err := s.SetPluginCapability(name, "connect", true); !errors.Is(err, ErrInvalidPluginPreferences) {
			t.Fatalf("invalid plugin accepted: %q: %v", name, err)
		}
		if _, err := s.SetPluginCapability("codex", name, true); !errors.Is(err, ErrInvalidPluginPreferences) {
			t.Fatalf("invalid capability accepted: %q: %v", name, err)
		}
		if _, err := s.InitPluginPreferences(map[string]map[string]bool{name: {"connect": true}}, nil); !errors.Is(err, ErrInvalidPluginPreferences) {
			t.Fatalf("invalid default plugin accepted: %q: %v", name, err)
		}
		if _, err := s.InitPluginPreferences(map[string]map[string]bool{"codex": {name: true}}, nil); !errors.Is(err, ErrInvalidPluginPreferences) {
			t.Fatalf("invalid default capability accepted: %q: %v", name, err)
		}
		if _, err := s.InitPluginPreferences(nil, map[string]string{name: CredentialCodexOAuth}); !errors.Is(err, ErrInvalidPluginPreferences) {
			t.Fatalf("invalid legacy plugin accepted: %q: %v", name, err)
		}
	}
	for _, raw := range []string{`{`, `null`, `{}`, `{"plugins":null}`, `{"plugins":{"codex":null}}`, `{"plugins":{"codex":{"connect":null}}}`, `{"plugins":{"codex":{"connect":"false"}}}`, `{"plugins":{"*":{"connect":true}}}`} {
		if _, err := s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", pluginPreferencesKey, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetPluginPreferences(); !errors.Is(err, ErrInvalidPluginPreferences) {
			t.Fatalf("corruption not reported by get: %v", err)
		}
		if _, err := s.InitPluginPreferences(pluginPreferenceDefaults(), nil); !errors.Is(err, ErrInvalidPluginPreferences) {
			t.Fatalf("corruption not reported by init: %v", err)
		}
		if _, err := s.SetPluginCapability("codex", "connect", true); !errors.Is(err, ErrInvalidPluginPreferences) {
			t.Fatalf("corruption not reported by set: %v", err)
		}
		var after string
		if err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", pluginPreferencesKey).Scan(&after); err != nil || after != raw {
			t.Fatalf("corrupted resource changed: %v", err)
		}
	}
}

func TestPluginPreferencesDatabaseErrorsAreReturned(t *testing.T) {
	s := pluginPreferenceTestStore(t)
	before, err := s.InitPluginPreferences(pluginPreferenceDefaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_plugin_preferences BEFORE UPDATE ON meta
		WHEN OLD.key='plugin_preferences' BEGIN SELECT RAISE(ABORT,'synthetic write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetPluginCapability("codex", "connect", false); err == nil {
		t.Fatal("set write failure swallowed")
	}
	if _, err = s.InitPluginPreferences(map[string]map[string]bool{"new-plugin": {"connect": true}}, nil); err == nil {
		t.Fatal("init write failure swallowed")
	}
	after, err := s.GetPluginPreferences()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed write changed preferences: %v", err)
	}
	if _, err := s.db.Exec("DROP TABLE channels"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitPluginPreferences(map[string]map[string]bool{"new-plugin": {"connect": false}}, map[string]string{"new-plugin": CredentialCodexOAuth}); err == nil {
		t.Fatal("channel query failure swallowed")
	}
	if _, err := s.db.Exec("DROP TABLE meta"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPluginPreferences(); err == nil {
		t.Fatal("get query failure swallowed")
	}
	if _, err := s.SetPluginCapability("codex", "connect", true); err == nil {
		t.Fatal("set query failure swallowed")
	}
}
