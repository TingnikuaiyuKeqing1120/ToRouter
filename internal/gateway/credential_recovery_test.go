package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"torouter/internal/store"
)

func TestHalfOpenCredentialFailoverRecovers(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			var rejected, healthy atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer rejected-key" {
					rejected.Add(1)
					w.WriteHeader(status)
					return
				}
				healthy.Add(1)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"content":"recovered"}}]}`)
			}))
			defer upstream.Close()
			g, s := setup(t)
			channel := store.Channel{
				Name: "recovery", Enabled: true, Models: []string{"up"},
				Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: upstream.URL}},
				Credentials: []store.CredentialItem{
					{ID: "rejected", APIKey: "rejected-key", Enabled: true},
					{ID: "healthy", APIKey: "healthy-key", Enabled: true},
				},
			}
			if err := s.SaveChannel(&channel); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDownstreamModel(store.DownstreamModel{
				ID: "recovery", Enabled: true, Fallback: true,
				Bindings: []store.Binding{{ChannelID: channel.ID, Model: "up", Enabled: true}},
			}); err != nil {
				t.Fatal(err)
			}
			settings, err := s.GetSettings()
			if err != nil {
				t.Fatal(err)
			}
			settings.RetriesPerChannel = 2
			if err := s.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			for range 1 {
				lease, ok := g.Breaker.Acquire(testCombination(t, g, channel.ID))
				if !ok {
					t.Fatal("could not seed channel cooldown")
				}
				g.Breaker.Failure(lease.WithRetryAfter(0))
			}
			// Exercise the production breaker through its first recovery probe.
			// Avoid a separate clock API used only by this cross-package test.

			if got := healthPhase(g, channel.ID); got != "half_open" {
				t.Fatalf("expected recovery probe to be available, got %q", got)
			}
			body := `{"model":"recovery","messages":[{"role":"user","content":"hello"}]}`
			got := invoke(g, "/v1/chat/completions", body)
			if got.Code != http.StatusOK || rejected.Load() != 1 || healthy.Load() != 1 {
				t.Fatalf("recovery status=%d rejected=%d healthy=%d breaker=%v", got.Code, rejected.Load(), healthy.Load(), g.Breaker.Snapshot())
			}
			if state := g.Breaker.Snapshot()[combinationForCredential(t, g, channel.ID, "healthy")]; state.Failures != 0 || state.Probing {
				t.Fatalf("successful backup did not close breaker: %+v", state)
			}
			if next := invoke(g, "/v1/chat/completions", body); next.Code != http.StatusOK || healthy.Load() != 2 {
				t.Fatalf("next request could not use recovered channel: status=%d healthy=%d", next.Code, healthy.Load())
			}
		})
	}
}
