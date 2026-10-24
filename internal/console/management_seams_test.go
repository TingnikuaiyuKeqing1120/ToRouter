package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

func TestManagementOAuthSharesFlowAndOwnership(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	_, s := probeStore(t)
	d := newOAuthDouble(t)
	o := &oauthTools{store: s, copilot: d.service(s), flows: map[string]*oauthFlow{}}
	ctx := context.Background()
	out, err := o.ManagementStart(ctx, "copilot", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	flow := out.(map[string]any)["flow"].(string)
	denied := errors.New("flow belongs to another principal")
	foreign := WithOAuthFlowAccess(ctx, func(string) error { return denied })
	if _, err = o.ManagementStart(foreign, "copilot", "", 0); !errors.Is(err, denied) {
		t.Fatalf("existing start bypassed owner: %v", err)
	}
	if d.deviceCodes.Load() != 1 {
		t.Fatal("foreign start minted another device code")
	}
	if _, err = o.ManagementStatus(foreign, "copilot", flow); !errors.Is(err, denied) {
		t.Fatalf("foreign poll: %v", err)
	}
	if _, err = o.ManagementCancel(foreign, "copilot", flow); !errors.Is(err, denied) {
		t.Fatalf("foreign cancel: %v", err)
	}
	if _, err = o.ManagementStatus(ctx, "codex", flow); err == nil {
		t.Fatal("wrong provider accepted the flow")
	}
	if d.polls.Load() != 0 {
		t.Fatal("refused access contacted issuer")
	}
	owned := WithOAuthFlowAccess(ctx, func(id string) error {
		if id != flow {
			return denied
		}
		return nil
	})
	out, err = o.ManagementStatus(owned, "copilot", flow)
	if err != nil {
		t.Fatal(err)
	}
	answer := out.(map[string]any)
	if answer["status"] != flowComplete {
		t.Fatalf("completion: %v", out)
	}
	channels, _ := s.ListChannels()
	if len(channels) != 1 || channels[0].OAuth == nil {
		t.Fatal("direct status did not persist the authorized channel")
	}
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"gho_console", "console-jwt", "access_token", "refresh_token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("summary leaked %s", secret)
		}
	}
	if _, err = o.ManagementStatus(owned, "copilot", flow); err != nil {
		t.Fatal(err)
	}
	if d.exchangeCalls.Load() != 1 {
		t.Fatal("completed flow exchanged twice")
	}
	// The shared models operation keeps read-only and apply semantics distinct.
	d.models = []string{"replacement"}
	if _, err = o.ManagementModels(ctx, channels[0].ID, "missing", false); err == nil {
		t.Fatal("unknown catalogue credential fell back to default")
	}
	if _, err = o.ManagementModels(ctx, channels[0].ID, channels[0].Credentials[0].ID, false); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := s.Channel(channels[0].ID)
	if slices.Contains(unchanged.AvailableModels, "replacement") {
		t.Fatal("read-only model discovery wrote the catalogue")
	}
	if _, err = o.ManagementModels(ctx, channels[0].ID, "", true); err != nil {
		t.Fatal(err)
	}
	updated, _ := s.Channel(channels[0].ID)
	if !slices.Equal(updated.AvailableModels, []string{"replacement"}) {
		t.Fatalf("catalogue not applied: %v", updated.AvailableModels)
	}
}

func TestManagementUpstreamExactCredentialAndDestination(t *testing.T) {
	_, s := probeStore(t)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer key-two" {
			t.Errorf("selected credential not used: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"data":[{"id":"fresh"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer up.Close()
	c := store.Channel{Name: "two keys", Enabled: true, Models: []string{"old"}, AvailableModels: []string{"old", "manual"}, ManualModels: []string{"manual"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}, Credentials: []store.CredentialItem{{ID: "one", APIKey: "key-one", Enabled: true}, {ID: "two", APIKey: "key-two", Enabled: true}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	u := &upstreamTools{store: s, client: up.Client()}
	ctx := context.Background()
	in := UpstreamManagementInput{ChannelID: c.ID, CredentialIDs: []string{"two"}, Apply: true}
	out, err := u.Discover(ctx, in)
	if err != nil || !out.(toolResult).OK {
		t.Fatalf("discover: %v %v", out, err)
	}
	updated, _ := s.Channel(c.ID)
	if !slices.Equal(updated.AvailableModels, []string{"fresh", "manual"}) || !slices.Equal(updated.Models, c.Models) {
		t.Fatal("apply changed the wrong catalogue/model fields")
	}
	in.Apply = false
	in.Model = "fresh"
	in.Confirmed = true
	out, err = u.Probe(ctx, in)
	if err != nil || !out.(toolResult).OK {
		t.Fatalf("probe: %v %v", out, err)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected probe count")
	}
	for _, invalid := range []UpstreamManagementInput{
		{ChannelID: c.ID, CredentialIDs: []string{"missing"}, Model: "fresh", Confirmed: true},
		{ChannelID: c.ID, CredentialIDs: []string{"two", "two"}, Model: "fresh", Confirmed: true},
		{ChannelID: c.ID, Draft: &UpstreamManagementDraft{BaseURL: "https://different.example", APIKey: "new-key"}, Model: "fresh", Confirmed: true},
		{ChannelID: c.ID, Model: "fresh"},
		{Draft: &UpstreamManagementDraft{BaseURL: "https://different.example"}, Model: "fresh", Confirmed: true},
	} {
		if _, err = u.Probe(ctx, invalid); err == nil {
			t.Fatalf("unsafe input accepted: %+v", invalid)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("refused input sent a probe or fell back to the first credential")
	}
}

func TestManagementDraftSendsOnlyFreshKey(t *testing.T) {
	_, s := probeStore(t)
	up := newProbeUpstream(t)
	u := &upstreamTools{store: s, client: up.Client()}
	out, err := u.Probe(context.Background(), UpstreamManagementInput{Draft: &UpstreamManagementDraft{APIKey: "fresh-draft-key", Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}}, Model: "a", Confirmed: true})
	if err != nil || !out.(toolResult).OK {
		t.Fatalf("fresh draft: %v %v", out, err)
	}
	hits := up.recorded()
	if len(hits) != 1 || hits[0].header.Get("Authorization") != "Bearer fresh-draft-key" {
		t.Fatal("draft did not authenticate with its own new key")
	}
}

func TestManagementStoredProbeUsesSelectedOAuthAccount(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	_, s := probeStore(t)
	up := newProbeUpstream(t)
	first := &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: "app_test", AccessToken: "access-first", RefreshToken: "refresh-first", Account: "account-first", Status: store.OAuthStatusActive, ExpiresAt: time.Now().Add(time.Hour)}
	second := &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: "app_test", AccessToken: "access-second", RefreshToken: "refresh-second", Account: "account-second", Status: store.OAuthStatusActive, ExpiresAt: time.Now().Add(time.Hour)}
	c := store.Channel{Name: "multi OAuth", CredentialType: store.CredentialCodexOAuth, Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL + "/backend-api/codex"}}, Credentials: []store.CredentialItem{{ID: "first", OAuth: first, Enabled: true}, {ID: "second", OAuth: second, Enabled: true}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	u := &upstreamTools{store: s, client: up.Client(), codex: codex.NewService(s, nil, codex.DefaultEndpoints())}
	out, err := u.Probe(context.Background(), UpstreamManagementInput{ChannelID: c.ID, CredentialIDs: []string{"second"}, Model: "gpt-5", Confirmed: true})
	if err != nil || !out.(toolResult).OK {
		t.Fatalf("OAuth probe: %v %v", out, err)
	}
	hits := up.recorded()
	if len(hits) != 1 || hits[0].header.Get("Authorization") != "Bearer access-second" || hits[0].header.Get("ChatGPT-Account-ID") != "account-second" {
		t.Fatalf("credential/account mismatch: %+v", hits)
	}
}
