package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

func TestBalanceDisplayExactDecimalAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []Balance
	}{
		{"zero", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0.000"}]}`, []Balance{{"CNY", "0.000"}}},
		{"exact_multiple_currencies", `{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"12345678901234567890.123456789"},{"currency":"USD","total_balance":"0.01"}]}`, []Balance{{"CNY", "12345678901234567890.123456789"}, {"USD", "0.01"}}},
		{"missing", `{"is_available":true}`, nil},
		{"duplicate_currency", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"1"},{"currency":"CNY","total_balance":"2"}]}`, nil},
		{"invalid", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"NaN"},{"currency":"USD","total_balance":"1e9"},{"currency":"EUR","total_balance":"3"}]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := parseBalanceSnapshot([]byte(tc.body))
			if err != nil || len(got) != len(tc.want) {
				t.Fatalf("got %v error %v", got, err)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

func TestBalanceSnapshotReadOnlyFailureExpiryAndCredentialIsolation(t *testing.T) {
	var calls atomic.Int32
	var failed atomic.Bool
	s, st, c := quotaFixture(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		if failed.Load() {
			return nil, errors.New("synthetic-secret")
		}
		return balanceResponse(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0"}]}`, 200), nil
	})
	c.Credentials = append(c.Credentials, store.CredentialItem{ID: "two", Enabled: true, APIKey: c.Credentials[0].APIKey})
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	cred := c.Credentials[0]
	if v := s.Snapshot(c, cred); !v.Stale || v.CheckedAt != nil || len(v.Balances) != 0 || calls.Load() != 0 {
		t.Fatalf("empty %+v", v)
	}
	v, observation := s.RefreshSnapshot(context.Background(), c, cred, true)
	if v.Stale || len(v.Balances) != 1 || v.Balances[0].Total != "0" || observation.Available == nil || *observation.Available {
		t.Fatalf("zero %+v %+v", v, observation)
	}
	v.Balances[0].Total = "999"
	if got := s.Snapshot(c, cred); got.Balances[0].Total != "0" || calls.Load() != 1 {
		t.Fatalf("cache %+v", got)
	}
	if got := s.Snapshot(c, c.Credentials[1]); !got.Stale || len(got.Balances) != 0 || calls.Load() != 1 {
		t.Fatalf("same account merged %+v", got)
	}
	s.mu.Lock()
	s.entries[cacheKey{c.ID, cred.ID}].observation.ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if got := s.Snapshot(c, cred); !got.Stale || got.Balances[0].Total != "0" {
		t.Fatalf("expired %+v", got)
	}
	failed.Store(true)
	v, observation = s.RefreshSnapshot(context.Background(), c, cred, true)
	if !v.Stale || v.Error == "" || len(v.Balances) != 0 || observation.Available != nil {
		t.Fatalf("failed %+v %+v", v, observation)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "synthetic-secret") {
		t.Fatal("secret in display")
	}
	if err := st.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(c, cred); !got.Stale || len(got.Balances) != 0 {
		t.Fatalf("deleted identity %+v", got)
	}
}

func TestCodexDisplayReusesPersistedCacheAndRefresh(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := store.Channel{Name: "Codex", CredentialType: store.CredentialCodexOAuth, Enabled: true,
		Endpoints:   []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		Credentials: []store.CredentialItem{{ID: "first", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: codex.ClientID, Account: "synthetic-account", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour), Status: store.OAuthStatusActive}}}}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var block atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		if block.Load() {
			close(entered)
			<-release
			return balanceResponse(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_after_seconds":3600}}}`, 200), nil
		}
		return balanceResponse(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":3600}},"code_review_rate_limit":{"primary_window":{}}}`, 200), nil
	})}
	codexSvc := codex.NewService(st, client, codex.Endpoints{Usage: "https://usage.test/usage"})
	s := New(st, codexSvc, client)
	if got := s.Snapshot(c, c.Credentials[0]); !got.Stale || calls.Load() != 0 {
		t.Fatalf("empty %+v", got)
	}
	v, o := s.RefreshSnapshot(context.Background(), c, c.Credentials[0], true)
	if v.Stale || len(v.Windows) != 2 || v.Windows[0].RemainingPercent == nil || *v.Windows[0].RemainingPercent != 75 || v.Windows[1].RemainingPercent != nil || o.Available == nil || !*o.Available || calls.Load() != 1 {
		t.Fatalf("fresh %+v %+v calls %d", v, o, calls.Load())
	}
	// A new quota service still reads Codex's existing persisted display record.
	s = New(st, codexSvc, client)
	if got := s.Snapshot(c, c.Credentials[0]); got.Stale || len(got.Windows) != 2 || calls.Load() != 1 {
		t.Fatalf("cached %+v", got)
	}
	_, _ = s.RefreshSnapshot(context.Background(), c, c.Credentials[0], false)
	if calls.Load() != 1 {
		t.Fatal("fresh display cache queried upstream again")
	}
	raw, _ := json.Marshal(v)
	for _, secret := range []string{"synthetic-account", "synthetic-access", "synthetic-refresh"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("identity exposed")
		}
	}
	block.Store(true)
	done := make(chan Observation, 1)
	go func() {
		_, observation := s.RefreshSnapshot(context.Background(), c, c.Credentials[0], true)
		done <- observation
	}()
	<-entered
	s.Reset(c.ID, c.Credentials[0].ID)
	close(release)
	if got := <-done; got.Available != nil || !got.CheckedAt.IsZero() {
		t.Fatalf("reset revived observation %+v", got)
	}
	if got := s.Cached(c, c.Credentials[0]); got.Available != nil {
		t.Fatalf("reset revived cache %+v", got)
	}
	_, second := s.RefreshSnapshot(context.Background(), c, c.Credentials[0], false)
	if second.Available != nil {
		t.Fatalf("cached late response revived unavailable observation: %+v", second)
	}
}
