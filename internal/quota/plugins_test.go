package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

func pluginCodexChannel(t *testing.T, st *store.Store) store.Channel {
	t.Helper()
	c := store.Channel{Name: "Fixture Codex", Enabled: true, CredentialType: store.CredentialCodexOAuth,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		Credentials: []store.CredentialItem{{ID: "fixture", Enabled: true, OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderCodex, ClientID: codex.ClientID, Account: "synthetic-account",
			AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour), Status: store.OAuthStatusActive}}}}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func setCodexPlugin(t *testing.T, m *plugins.Manager, enabled bool) {
	t.Helper()
	list, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetCapability(context.Background(), "codex", "connect", enabled, list.Version); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledPluginSnapshotReadsPersistedCacheWithoutRuntime(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := pluginCodexChannel(t, st)
	if _, err := st.SetPluginCapability("codex", "connect", false); err != nil {
		t.Fatal(err)
	}
	used, seconds := 25.0, int64(18000)
	u := codex.Usage{CapturedAt: time.Now().UTC(), Buckets: []codex.UsageBucket{{General: true, Primary: &codex.UsageWindow{UsedPercent: &used, WindowSeconds: &seconds}}}}
	raw, _ := json.Marshal(u)
	if err := st.SaveQuotaCache(c, "fixture", store.QuotaCache{Snapshot: raw}); err != nil {
		t.Fatal(err)
	}
	m, err := plugins.New(st)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s := NewWithPlugins(st, m, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return balanceResponse(`{}`, 200), nil
	})})
	for i := 0; i < 2; i++ {
		v := s.Snapshot(c, c.Credentials[0])
		if v.Enabled || v.NextRefreshAt != nil || len(v.Windows) != 1 || v.Windows[0].RemainingPercent == nil || *v.Windows[0].RemainingPercent != 75 || !strings.Contains(v.Error, "插件已停用") {
			t.Fatalf("disabled snapshot: %+v", v)
		}
	}
	v, o := s.RefreshSnapshot(context.Background(), c, c.Credentials[0], true)
	if v.Enabled || len(v.Windows) != 1 || v.NextRefreshAt != nil || o.Available != nil || o.Reason != "" || !o.CheckedAt.IsZero() || !strings.Contains(o.Error, "插件已停用") {
		t.Fatalf("disabled refresh: %+v %+v", v, o)
	}
	o = s.Check(context.Background(), c, c.Credentials[0], true)
	if o.Available != nil || o.Reason != "" || !o.CheckedAt.IsZero() || !strings.Contains(o.Error, "插件已停用") || calls.Load() != 0 || m.Existing(store.CredentialCodexOAuth) != nil {
		t.Fatalf("disabled check created runtime/network/fact: %+v calls=%d", o, calls.Load())
	}
	live, _ := st.Channel(c.ID)
	if live.Credentials[0].OAuth.Status != store.OAuthStatusActive || live.Credentials[0].OAuth.AccessToken != "synthetic-access" {
		t.Fatal("disabled quota read changed OAuth credential")
	}
}

func TestPluginQuotaSharesServiceAndRejectsDisabledNetwork(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := pluginCodexChannel(t, st)
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return balanceResponse(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`, 200), nil
	})}
	svc := codex.NewService(st, client, codex.Endpoints{Usage: "https://usage.test/usage"})
	m, err := plugins.NewInjected(st, nil, nil, svc)
	if err != nil {
		t.Fatal(err)
	}
	s := NewWithPlugins(st, m, client)
	v, o := s.RefreshSnapshot(context.Background(), c, c.Credentials[0], true)
	if v.Error != "" || o.Available == nil || !*o.Available || calls.Load() != 1 || m.Existing(c.CredentialType).Codex != svc {
		t.Fatalf("shared refresh: %+v %+v calls=%d", v, o, calls.Load())
	}
	o = s.Check(context.Background(), c, c.Credentials[0], true)
	if o.Error != "" || o.Available == nil || !*o.Available || calls.Load() != 2 {
		t.Fatalf("shared check: %+v calls=%d", o, calls.Load())
	}
	setCodexPlugin(t, m, false)
	v, o = s.RefreshSnapshot(context.Background(), c, c.Credentials[0], true)
	if v.Enabled || v.NextRefreshAt != nil || len(v.Windows) != 1 || o.Available != nil || !strings.Contains(o.Error, "插件已停用") {
		t.Fatalf("disabled refresh after enable: %+v %+v", v, o)
	}
	for _, force := range []bool{false, true} {
		if got := s.Check(context.Background(), c, c.Credentials[0], force); got.Available != nil || !strings.Contains(got.Error, "插件已停用") {
			t.Fatalf("disabled force=%v: %+v", force, got)
		}
	}
	if got := s.Cached(c, c.Credentials[0]); got.Available != nil || calls.Load() != 2 || m.Existing(c.CredentialType).Codex != svc {
		t.Fatalf("disabled cache/service: %+v calls=%d", got, calls.Load())
	}
}

func TestPluginQuotaLeavesAPIKeyBalanceAvailable(t *testing.T) {
	var calls atomic.Int32
	old, st, c := quotaFixture(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return balanceResponse(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.00"}]}`, 200), nil
	})
	m, err := plugins.New(st)
	if err != nil {
		t.Fatal(err)
	}
	setCodexPlugin(t, m, false)
	s := NewWithPlugins(st, m, old.client)
	v, o := s.RefreshSnapshot(context.Background(), c, c.Credentials[0], true)
	if !v.Enabled || o.Available == nil || !*o.Available || len(v.Balances) != 1 || v.Balances[0].Total != "12.00" || calls.Load() != 1 {
		t.Fatalf("API key balance: %+v %+v calls=%d", v, o, calls.Load())
	}
}

func TestPluginQuotaDiscardsAvailabilityFromExpiredEpoch(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "check", true: "refresh"}[refresh], func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			c := pluginCodexChannel(t, st)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				close(entered)
				<-release
				return balanceResponse(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`, 200), nil
			})}
			svc := codex.NewService(st, client, codex.Endpoints{Usage: "https://usage.test/usage"})
			m, err := plugins.NewInjected(st, nil, nil, svc)
			if err != nil {
				t.Fatal(err)
			}
			s := NewWithPlugins(st, m, client)
			done := make(chan Observation, 1)
			go func() {
				if refresh {
					_, o := s.RefreshSnapshot(context.Background(), c, c.Credentials[0], true)
					done <- o
				} else {
					done <- s.Check(context.Background(), c, c.Credentials[0], true)
				}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("quota request did not start")
			}
			setCodexPlugin(t, m, false)
			setCodexPlugin(t, m, true)
			close(release)
			select {
			case o := <-done:
				if o.Available != nil || o.Reason != "" || !o.CheckedAt.IsZero() || !o.ExpiresAt.IsZero() || !strings.Contains(o.Error, "启用状态已变化") || calls.Load() != 1 {
					t.Fatalf("expired epoch published quota fact: %+v calls=%d", o, calls.Load())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("quota request did not finish")
			}
			if o := s.Cached(c, c.Credentials[0]); o.Available != nil {
				t.Fatalf("expired epoch populated cached availability: %+v", o)
			}
		})
	}
}
