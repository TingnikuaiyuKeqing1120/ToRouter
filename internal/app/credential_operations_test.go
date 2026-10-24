package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

func TestShutdownCancelsCredentialAutomationBeforeStoreClose(t *testing.T) {
	s := isolatedServer(t)
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/usage" {
			io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1800000000}}}`)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	cfg := store.DefaultCredentialOperationsConfig()
	cfg.Warmup = store.CredentialWarmupConfig{Enabled: true, Mode: "daily", DailyTimes: []string{time.Now().Format("15:04")}, Model: "model"}
	c := store.Channel{Name: "cancel-action", CredentialType: store.CredentialCodexOAuth, Enabled: true, Models: []string{"model"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}}, Credentials: []store.CredentialItem{{ID: "one", Enabled: true, Operations: &cfg, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: "client", Account: "synthetic-account", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour)}}}}
	if err := s.Store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	manager, err := plugins.NewInjected(s.Store, nil, nil, codex.NewService(s.Store, upstream.Client(), codex.Endpoints{Backend: upstream.URL, Usage: upstream.URL + "/usage"}))
	if err != nil {
		t.Fatal(err)
	}
	s.plugins = manager
	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic action not started")
	}
	s.Shutdown()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("automation kept server alive after cancellation")
	}
	st, err := store.Open(s.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	raw, err := st.LoadCredentialOperationState(c.ID, "one")
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		History map[string]struct {
			State string `json:"state"`
		} `json:"history"`
	}
	if err = json.Unmarshal(raw, &state); err != nil || len(state.History) != 1 {
		t.Fatalf("final receipt missing: %v", err)
	}
	for _, r := range state.History {
		if r.State != "unknown" {
			t.Fatalf("cancelled POST outcome=%s", r.State)
		}
	}
}
