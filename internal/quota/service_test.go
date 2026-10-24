package quota

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func balanceResponse(body string, code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func quotaFixture(t *testing.T, transport roundTripFunc) (*Service, *store.Store, store.Channel) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := store.Channel{Name: "DeepSeek", Enabled: true, CredentialType: store.CredentialAPIKey,
		Models: []string{"a"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.deepseek.com/v1"}},
		Credentials: []store.CredentialItem{{ID: "one", APIKey: "synthetic-secret", Enabled: true}}}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return New(st, nil, &http.Client{Transport: transport}), st, c
}

func TestDeepSeekTargetRequiresOfficialDestination(t *testing.T) {
	s := New(nil, nil, nil)
	cred := store.CredentialItem{APIKey: "synthetic"}
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://api.deepseek.com", true}, {"https://API.DEEPSEEK.COM:443/v1", true}, {"https://api.deepseek.com/anthropic", true},
		{"http://api.deepseek.com", false}, {"https://api.deepseek.com.evil.test", false}, {"https://evil.test/api.deepseek.com", false},
		{"https://api.deepseek.com@evil.test", false}, {"https://user@api.deepseek.com", false}, {"https://api.deepseek.com:444", false},
		{"https://api.deepseek.com/?secret=x", false}, {"https://api.deepseek.com/#x", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			c := store.Channel{Models: []string{"deepseek-chat"}, BaseURL: tc.url}
			if got := s.Supported(c, cred); got != tc.want {
				t.Fatalf("supported=%v want %v", got, tc.want)
			}
		})
	}
	c := store.Channel{Endpoints: []store.Endpoint{{BaseURL: "https://api.deepseek.com"}, {BaseURL: "https://third-party.test"}}}
	if s.Supported(c, cred) {
		t.Fatal("mixed third-party destinations accepted")
	}
	c.CredentialType = store.CredentialChatGPTOAuth
	if s.Supported(c, store.CredentialItem{OAuth: &store.OAuthCredential{}}) {
		t.Fatal("SIWC treated as Codex")
	}
}

func TestDeepSeekBalanceKnownAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       *bool
		invalid    bool
	}{
		{"positive", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0.01"}]}`, boolValue(true), false},
		{"zero", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0.000"}]}`, boolValue(false), false},
		{"negative", `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"-2"}]}`, boolValue(false), false},
		{"one_currency", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"-2"},{"currency":"USD","total_balance":"1"}]}`, boolValue(true), false},
		{"disabled", `{"is_available":false,"balance_infos":[{"currency":"USD","total_balance":"1"}]}`, boolValue(false), false},
		{"missing", `{"is_available":true}`, nil, false},
		{"unknown_amount", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"nan"}]}`, nil, false},
		{"unknown_currency", `{"is_available":true,"balance_infos":[{"currency":"EUR","total_balance":"1"}]}`, nil, false},
		{"zero_unknown", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0"},{"currency":"USD"}]}`, nil, false},
		{"no_flag", `{"balance_infos":[{"currency":"CNY","total_balance":"0"}]}`, nil, false},
		{"invalid", `{"is_available":`, nil, true},
		{"invalid_type", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":0}]}`, nil, true},
		{"exponent_bound", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"1e999999999"}]}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBalance([]byte(tc.body))
			if (err != nil) != tc.invalid || (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
func boolValue(b bool) *bool { return &b }

func TestQuotaCacheExpiryFailureAndManualRefresh(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	s, st, c := quotaFixture(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != "https://api.deepseek.com/user/balance" || r.Header.Get("Authorization") != "Bearer synthetic-secret" {
			t.Errorf("unexpected request destination or credential")
		}
		if fail.Load() {
			return nil, errors.New("synthetic-secret raw-response must not leave transport")
		}
		return balanceResponse(`{"is_available":false}`, 200), nil
	})
	cred := c.Credentials[0]
	first := s.Check(context.Background(), c, cred, false)
	if first.Available == nil || *first.Available || first.Reason != "余额不足" || calls.Load() != 1 {
		t.Fatalf("initial %+v calls %d", first, calls.Load())
	}
	*first.Available = true
	if got := s.Check(context.Background(), c, cred, false); got.Available == nil || *got.Available || calls.Load() != 1 {
		t.Fatalf("cache %+v", got)
	}
	s.mu.Lock()
	e := s.entries[cacheKey{c.ID, cred.ID}]
	e.observation.CheckedAt = time.Now().Add(-time.Hour)
	e.observation.ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if got := s.Cached(c, cred); got.Available != nil || got.Reason != "" || calls.Load() != 1 {
		t.Fatalf("expired %+v", got)
	}
	fail.Store(true)
	got := s.Check(context.Background(), c, cred, false)
	if got.Available != nil || got.Error == "" || strings.Contains(got.Error, "synthetic-secret") || calls.Load() != 2 {
		t.Fatalf("failed %+v", got)
	}
	settings, err := st.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	settings.QuotaRefreshSeconds = &zero
	if err := st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	s.Reset(c.ID, cred.ID)
	if got := s.Check(context.Background(), c, cred, false); got.Available != nil || calls.Load() != 2 {
		t.Fatalf("manual mode %+v", got)
	}
	fail.Store(false)
	if got := s.Check(context.Background(), c, cred, true); got.Available == nil || *got.Available || calls.Load() != 3 {
		t.Fatalf("force %+v", got)
	}
}

func TestDeepSeekErrorsAreNotZeroAndRejectRedirect(t *testing.T) {
	for _, code := range []int{302, 307, 401, 402, 429, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var leaked atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
			defer target.Close()
			s, _, c := quotaFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.deepseek.com" {
					leaked.Add(1)
				}
				resp := balanceResponse("synthetic-secret raw error", code)
				resp.Header.Set("Location", target.URL)
				return resp, nil
			})
			got := s.Check(context.Background(), c, c.Credentials[0], true)
			if got.Available != nil || got.Error == "" || strings.Contains(got.Error, "synthetic-secret") || leaked.Load() != 0 {
				t.Fatalf("error %+v leaked %d", got, leaked.Load())
			}
		})
	}
}

func TestQuotaConcurrentCheckAndResetDoNotReviveOldFact(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, _, c := quotaFixture(t, func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return balanceResponse(`{"is_available":false}`, 200), nil
	})
	cred := c.Credentials[0]
	first := make(chan Observation, 1)
	go func() { first <- s.Check(context.Background(), c, cred, true) }()
	<-entered
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Check(context.Background(), c, cred, false) }()
	}
	// Waiters are permitted to finish after Reset. The old owner is never
	// allowed to publish its negative fact into the replacement epoch.
	s.Reset(c.ID, cred.ID)
	close(release)
	got := <-first
	if got.Available != nil {
		t.Fatalf("old in-flight result survived reset: %+v", got)
	}
	wg.Wait()
	// A second reset deliberately removes any genuine post-reset query made
	// by a late waiter; no prior writer remains to recreate it.
	s.Reset(c.ID, cred.ID)
	if got := s.Cached(c, cred); got.Available != nil {
		t.Fatalf("cache revived: %+v", got)
	}
}

func TestQuotaConcurrentForceCoalesces(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, _, c := quotaFixture(t, func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return balanceResponse(`{"is_available":false}`, 200), nil
	})
	first := make(chan Observation, 1)
	go func() { first <- s.Check(context.Background(), c, c.Credentials[0], true) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.Check(ctx, c, c.Credentials[0], true); got.Available != nil || got.Error == "" {
		t.Fatalf("canceled waiter %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent check started another request")
	}
	close(release)
	if got := <-first; got.Available == nil || *got.Available {
		t.Fatalf("owner %+v", got)
	}
}

func TestQuotaConfigChangeAndDeletedEntityInvalidate(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s, st, c := quotaFixture(t, func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return balanceResponse(`{"is_available":false}`, 200), nil
	})
	cred := c.Credentials[0]
	done := make(chan Observation, 1)
	go func() { done <- s.Check(context.Background(), c, cred, true) }()
	<-entered
	live, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	live.APIKey = "replacement-secret"
	live.Credentials[0].APIKey = live.APIKey
	if err := st.SaveChannel(&live); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := <-done; got.Available != nil {
		t.Fatalf("old config published: %+v", got)
	}
	if got := s.Cached(live, live.Credentials[0]); got.Available != nil {
		t.Fatalf("old config reused %+v", got)
	}
	if err := st.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.lastPrune = time.Time{}
	s.mu.Unlock()
	_ = s.Cached(live, live.Credentials[0])
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) != 0 {
		t.Fatal("deleted channel cache not pruned")
	}
}

func TestCodexGeneralOnlyAndWindowExpiry(t *testing.T) {
	now := time.Now()
	used := func(f float64) *float64 { return &f }
	past, future := now.Add(-time.Second).Unix(), now.Add(time.Hour).Unix()
	for _, tc := range []struct {
		name    string
		buckets []codex.UsageBucket
		want    *bool
	}{
		{"unknown", nil, nil},
		{"specialist", []codex.UsageBucket{{ID: "rate_limit", Allowed: boolValue(false)}}, nil},
		{"allowed", []codex.UsageBucket{{General: true, Allowed: boolValue(true)}, {LimitReached: boolValue(true)}}, boolValue(true)},
		{"not_allowed", []codex.UsageBucket{{General: true, Allowed: boolValue(false)}}, boolValue(false)},
		{"reached", []codex.UsageBucket{{General: true, LimitReached: boolValue(true)}}, boolValue(false)},
		{"secondary", []codex.UsageBucket{{General: true, Primary: &codex.UsageWindow{UsedPercent: used(20)}, Secondary: &codex.UsageWindow{UsedPercent: used(100), ResetAt: &future}}}, boolValue(false)},
		{"expired", []codex.UsageBucket{{General: true, Allowed: boolValue(false), Primary: &codex.UsageWindow{UsedPercent: used(100), ResetAt: &past}}}, nil},
		{"unknown_window", []codex.UsageBucket{{General: true, Primary: &codex.UsageWindow{}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := codexAvailability(codex.Usage{Buckets: tc.buckets}, now)
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaCodexPoolServiceUsesOnlyFreshGeneralBucket(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oauth := func(account, token string) *store.OAuthCredential {
		return &store.OAuthCredential{
			Provider: store.OAuthProviderCodex, ClientID: codex.ClientID, Account: account,
			AccessToken: token, RefreshToken: "refresh-" + account, ExpiresAt: time.Now().Add(time.Hour), Status: store.OAuthStatusActive}
	}
	c := store.Channel{Name: "Codex", CredentialType: store.CredentialCodexOAuth, Enabled: true,
		Endpoints:   []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		Credentials: []store.CredentialItem{{ID: "first", OAuth: oauth("account-a", "access-a"), Enabled: true}, {ID: "second", OAuth: oauth("account-b", "access-b"), Enabled: true}}}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			if r.Header.Get("ChatGPT-Account-ID") != "account-a" {
				t.Error("wrong primary identity")
			}
			return balanceResponse(`{"rate_limit":{"limit_id":"vendor-main","allowed":true,"primary_window":{"used_percent":9}},"code_review_rate_limit":{"allowed":false},"additional_rate_limits":{"rate_limit":{"allowed":false}},"credits":{"balance":"0"}}`, 200), nil
		case "Bearer access-b":
			if r.Header.Get("ChatGPT-Account-ID") != "account-b" {
				t.Error("wrong secondary identity")
			}
			return balanceResponse(`{"rate_limit":{"allowed":false}}`, 200), nil
		default:
			t.Error("unexpected pool credential")
			return balanceResponse(`{}`, 500), nil
		}
	})}
	s := New(st, codex.NewService(st, client, codex.Endpoints{Usage: "https://usage.test/usage"}), client)
	first := s.Check(context.Background(), c, c.Credentials[0], false)
	second := s.Check(context.Background(), c, c.Credentials[1], false)
	if first.Available == nil || !*first.Available || second.Available == nil || *second.Available || second.Reason != "额度不足" {
		t.Fatalf("wrong credential observations: first=%+v second=%+v", first, second)
	}
	if got := s.Check(context.Background(), c, c.Credentials[0], false); got.Available == nil || !*got.Available || calls.Load() != 2 {
		t.Fatalf("pool cache mixed: %+v calls %d", got, calls.Load())
	}
}

func TestQuotaNetworkBudgetsAndHardCacheBound(t *testing.T) {
	var force atomic.Bool
	s, _, c := quotaFixture(t, func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		limit := 2 * time.Second
		if force.Load() {
			limit = 20 * time.Second
		}
		if !ok || time.Until(deadline) > limit || time.Until(deadline) < limit-time.Second {
			t.Errorf("unexpected network budget: %v %v", deadline, ok)
		}
		return balanceResponse(`{"is_available":false}`, 200), nil
	})
	s.mu.Lock()
	s.lastPrune = time.Now()
	for i := 0; i < maxEntries; i++ {
		s.entries[cacheKey{int64(i + 100), "removed"}] = &cacheEntry{observation: Observation{CheckedAt: time.Now().Add(-time.Hour)}}
	}
	s.mu.Unlock()
	_ = s.Check(context.Background(), c, c.Credentials[0], false)
	s.mu.Lock()
	size := len(s.entries)
	s.mu.Unlock()
	if size != maxEntries {
		t.Fatalf("unbounded cache size %d", size)
	}
	force.Store(true)
	_ = s.Check(context.Background(), c, c.Credentials[0], true)
}
