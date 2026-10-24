package console

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"torouter/internal/store"
)

func TestCRUDRedactsAndPreservesSecret(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	body := `{"name":"a","protocol":"openai","base_url":"http://localhost:9999","api_key":"upstream-sensitive","models":["test"],"enabled":true}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/channels", strings.NewReader(body)))
	if w.Code != 200 || strings.Contains(w.Body.String(), "upstream-sensitive") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	body = strings.Replace(body, "upstream-sensitive", "", 1)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PUT", "/channels/1", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	rows, _ := s.ListChannels()
	if rows[0].APIKey != "upstream-sensitive" {
		t.Fatal("empty edit lost key")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/channels", nil))
	var data []map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &data); err != nil || data[0]["has_key"] != true {
		t.Fatal("redaction metadata", err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/channels", strings.NewReader(body+`{}`)))
	if w.Code != 400 {
		t.Fatal("accepted trailing JSON")
	}
}

// The initial failure cooldown travels through the console API like every other
// settings field: GET hands out the effective value, PUT accepts a legal one
// and refuses an out-of-range one.
func TestSettingsFailureCooldownRoundTripsThroughAPI(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17888", Hooks{})
	get := func() map[string]any {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
		if w.Code != 200 {
			t.Fatalf("GET /settings = %d %s", w.Code, w.Body.String())
		}
		var data map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &data); e != nil {
			t.Fatal(e)
		}
		return data
	}
	if v := get()["failure_cooldown_seconds"]; v != float64(store.DefaultFailureCooldownSeconds) {
		t.Fatalf("default cooldown = %v, want %d", v, store.DefaultFailureCooldownSeconds)
	}
	v := get()
	delete(v, "prices")
	put := func(seconds int) *httptest.ResponseRecorder {
		v["failure_cooldown_seconds"] = seconds
		raw, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("PUT", "/settings", strings.NewReader(string(raw))))
		return rec
	}
	if rec := put(45); rec.Code != 200 {
		t.Fatalf("PUT /settings = %d %s", rec.Code, rec.Body.String())
	}
	if got := get()["failure_cooldown_seconds"]; got != float64(45) {
		t.Fatalf("saved cooldown = %v, want 45", got)
	}
	if rec := put(301); rec.Code != 400 {
		t.Fatalf("accepted an out-of-range cooldown: %d %s", rec.Code, rec.Body.String())
	}
}

// The OAuth auto-open switch reaches the console page as a plain JSON bool that
// is always resolved: nil (a row from before the switch existed) must not
// travel as null, which the page would have to interpret as off.
func TestSettingsOAuthAutoOpenRoundTripsThroughAPI(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17887", Hooks{})
	get := func() map[string]any {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
		if w.Code != 200 {
			t.Fatalf("GET /settings = %d %s", w.Code, w.Body.String())
		}
		var data map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &data); e != nil {
			t.Fatal(e)
		}
		return data
	}
	if v := get()["oauth_auto_open"]; v != store.DefaultOAuthAutoOpen {
		t.Fatalf("default auto-open = %v, want %t", v, store.DefaultOAuthAutoOpen)
	}
	v := get()
	delete(v, "prices")
	put := func(open bool) *httptest.ResponseRecorder {
		v["oauth_auto_open"] = open
		raw, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("PUT", "/settings", strings.NewReader(string(raw))))
		return rec
	}
	if rec := put(false); rec.Code != 200 {
		t.Fatalf("PUT /settings = %d %s", rec.Code, rec.Body.String())
	}
	if got := get()["oauth_auto_open"]; got != false {
		t.Fatalf("saved auto-open = %v, want false", got)
	}
	if rec := put(true); rec.Code != 200 {
		t.Fatalf("PUT /settings = %d %s", rec.Code, rec.Body.String())
	}
	if got := get()["oauth_auto_open"]; got != true {
		t.Fatalf("saved auto-open = %v, want true", got)
	}
}

// The console's two views ask the channel list for one credential type each:
// the API key view, and a card per OAuth provider. Without the parameter every
// channel is listed, which is what any other client gets.
func TestChannelListFiltersByCredentialType(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	save := func(name string, c store.Channel) {
		t.Helper()
		c.Name = name
		c.Enabled = true
		if err := s.SaveChannel(&c); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	save("plain", store.Channel{APIKey: "k", CredentialType: store.CredentialAPIKey,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.example.com"}}})
	// A row written before credential types existed reads back as api_key, so
	// the default type must cover it.
	save("legacy", store.Channel{APIKey: "k",
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://legacy.example.com"}}})
	save("copilot", store.Channel{CredentialType: store.CredentialCopilotOAuth,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.githubcopilot.com"}},
		OAuth:     &store.OAuthCredential{Provider: store.OAuthProviderCopilot, GitHubToken: "gho_x", AccountLabel: "octocat", Status: store.OAuthStatusActive}})
	save("chatgpt", store.Channel{CredentialType: store.CredentialChatGPTOAuth,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://api.openai.com"}},
		OAuth:     &store.OAuthCredential{Provider: store.OAuthProviderChatGPT, RefreshToken: "rt", ClientID: "oaiapp_x", HostID: "urn:uuid:host"}})
	save("codex", store.Channel{CredentialType: store.CredentialCodexOAuth,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		OAuth:     &store.OAuthCredential{Provider: store.OAuthProviderCodex, RefreshToken: "rt", ClientID: "app_x", Account: "acct-1"}})
	h := New(s, "http://127.0.0.1:17888", Hooks{})
	names := func(query string) []string {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/channels"+query, nil))
		if w.Code != 200 {
			t.Fatalf("GET /channels%s = %d %s", query, w.Code, w.Body.String())
		}
		var rows []struct {
			Name           string `json:"name"`
			CredentialType string `json:"credential_type"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, row := range rows {
			out = append(out, row.Name)
		}
		return out
	}
	if got := names(""); !reflect.DeepEqual(got, []string{"plain", "legacy", "copilot", "chatgpt", "codex"}) {
		t.Fatalf("unfiltered list = %v", got)
	}
	for query, want := range map[string][]string{
		"?credential_type=api_key":       {"plain", "legacy"},
		"?credential_type=copilot_oauth": {"copilot"},
		"?credential_type=chatgpt_oauth": {"chatgpt"},
		"?credential_type=codex_oauth":   {"codex"},
	} {
		if got := names(query); !reflect.DeepEqual(got, want) {
			t.Fatalf("GET /channels%s = %v, want %v", query, got, want)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/channels?credential_type=not_a_type", nil))
	if w.Code != 400 {
		t.Fatalf("unknown credential type: %d %s", w.Code, w.Body.String())
	}
}

// Rule 2, channel side: a save that turns a model back on while the channel's
// catalogue no longer lists it succeeds and answers with a note; the same models
// re-saved while nothing changed are not a re-enable and carry none. The note
// reports what the last fetch did not say, not a verdict on the model.
func TestChannelSaveReportsReenabledModel(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	c := store.Channel{Name: "prov", Protocol: "openai", BaseURL: "http://localhost:9999", APIKey: "upstream-secret", Enabled: true,
		Models: []string{"live", "retired"}, AvailableModels: []string{"live"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	save := func(models ...string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"name": c.Name, "endpoints": []map[string]any{{"format": "openai_chat_completions", "base_url": c.Base()}}, "models": models, "available_models": c.AvailableModels, "enabled": true})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("PUT", "/channels/"+fmt.Sprint(c.ID), strings.NewReader(string(body))))
		if w.Code != 200 {
			t.Fatalf("save %d %s", w.Code, w.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Disabling the model first: no note, and the catalogue is left alone.
	if answer := save("live"); answer["notice"] != nil {
		t.Fatalf("plain save carried a notice: %v", answer["notice"])
	}
	// Re-enabling it while the catalogue does not list it: a note, not a refusal.
	answer := save("live", "retired")
	notice, _ := answer["notice"].(string)
	if !strings.Contains(notice, "retired") || !strings.Contains(notice, "可能") {
		t.Fatalf("re-enable notice = %q", notice)
	}
	if answer["name"] != "prov" || answer["has_key"] != true {
		t.Fatalf("notice answer lost the channel shape: %v", answer)
	}
	// Saving the same enabled list again is not a re-enable: no note.
	if again := save("live", "retired"); again["notice"] != nil {
		t.Fatalf("unchanged save carried a notice: %v", again["notice"])
	}
	// Enabling a model the catalogue does not list is reported the same way
	// whether it was disabled before or never enabled at all.
	if listed := save("live", "newly-typed"); listed["notice"] == nil {
		t.Fatal("enabling a model outside the catalogue must be reported")
	}
}

func TestSaveChannelRefusesOAuthCopyFromSourceChannel(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})

	ch1 := store.Channel{
		Name:           "Copilot 1",
		CredentialType: store.CredentialCopilotOAuth,
		Enabled:        true,
		Endpoints:      []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.githubcopilot.com"}},
		OAuth: &store.OAuthCredential{
			Provider:     store.OAuthProviderCopilot,
			GitHubToken:  "gho_token1",
			AccountLabel: "octocat",
			Status:       store.OAuthStatusActive,
		},
		Credentials: []store.CredentialItem{
			{
				ID:      "cred_1",
				Label:   "主号",
				Enabled: true,
			},
		},
	}
	if err := s.SaveChannel(&ch1); err != nil {
		t.Fatal(err)
	}

	ch2 := store.Channel{
		Name:           "Copilot 2",
		CredentialType: store.CredentialCopilotOAuth,
		Enabled:        true,
		Endpoints:      []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.githubcopilot.com"}},
		OAuth: &store.OAuthCredential{
			Provider:     store.OAuthProviderCopilot,
			GitHubToken:  "gho_token2",
			AccountLabel: "monalisa",
			Status:       store.OAuthStatusActive,
		},
	}
	if err := s.SaveChannel(&ch2); err != nil {
		t.Fatal(err)
	}

	before1, _ := s.Channel(ch1.ID)
	before2, _ := s.Channel(ch2.ID)
	// Ordinary edits cannot copy credential material from another pool.
	body, _ := json.Marshal(map[string]any{
		"name":            ch1.Name,
		"credential_type": ch1.CredentialType,
		"endpoints":       []map[string]any{{"format": "openai_chat_completions", "base_url": "https://api.githubcopilot.com"}},
		"enabled":         true,
		"dispatch_mode":   "round_robin",
		"chain_type":      "cyclic",
		"credentials": []map[string]any{
			{"id": "cred_1", "label": "主号", "enabled": true},
			{"id": "cred_2", "label": "副号", "enabled": true, "source_channel_id": ch2.ID},
		},
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PUT", fmt.Sprintf("/channels/%d", ch1.ID), strings.NewReader(string(body))))
	if w.Code != 400 {
		t.Fatalf("PUT /channels accepted OAuth copy: %d %s", w.Code, w.Body.String())
	}

	// Verify DB state
	loaded, err := s.Channel(ch1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Credentials) != 1 || store.EntityVersion(loaded) != store.EntityVersion(before1) {
		t.Fatalf("rejected copy changed target, credentials=%d", len(loaded.Credentials))
	}
	if loaded.Credentials[0].OAuth == nil || loaded.Credentials[0].OAuth.GitHubToken != "gho_token1" {
		t.Fatalf("expected cred 1 to have token 'gho_token1', got %+v", loaded.Credentials[0].OAuth)
	}
	untouched, _ := s.Channel(ch2.ID)
	if store.EntityVersion(untouched) != store.EntityVersion(before2) {
		t.Fatal("rejected copy modified source")
	}

	// Verify GET response
	wGet := httptest.NewRecorder()
	h.ServeHTTP(wGet, httptest.NewRequest("GET", "/channels", nil))
	if wGet.Code != 200 {
		t.Fatalf("GET /channels failed: %d %s", wGet.Code, wGet.Body.String())
	}
	var channels []struct {
		ID          int64                  `json:"id"`
		Credentials []store.CredentialItem `json:"credentials"`
	}
	if err := json.Unmarshal(wGet.Body.Bytes(), &channels); err != nil {
		t.Fatal(err)
	}
	var ch1Public *struct {
		ID          int64                  `json:"id"`
		Credentials []store.CredentialItem `json:"credentials"`
	}
	for i := range channels {
		if channels[i].ID == ch1.ID {
			ch1Public = &channels[i]
			break
		}
	}
	if ch1Public == nil {
		t.Fatalf("ch1 not found in GET /channels: %s", wGet.Body.String())
	}
	if len(ch1Public.Credentials) != 1 {
		t.Fatalf("expected unchanged 1 credential in GET response, got %d", len(ch1Public.Credentials))
	}
	if !ch1Public.Credentials[0].HasOAuth || ch1Public.Credentials[0].OAuthInfo == nil || ch1Public.Credentials[0].OAuthInfo.AccountLabel != "octocat" {
		t.Fatalf("expected cred 0 to have octocat account label, got %+v", ch1Public.Credentials[0])
	}

}
