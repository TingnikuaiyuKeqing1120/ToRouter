package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestUsageCacheReadRefreshFailureAndManualMode(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var calls, status atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if status.Load() != 0 {
			w.WriteHeader(int(status.Load()))
			return
		}
		io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":17}}}`)
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := codexChannel(t, s, server.URL)
	if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "valid", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s, server.Client(), testEndpoints(server))
	empty, err := svc.CachedUsage(c.ID, c.Credentials[0].ID)
	if err != nil || empty.OK || !empty.RefreshDue || empty.RefreshSeconds != 300 || calls.Load() != 0 {
		t.Fatalf("cold GET: %+v %v", empty, err)
	}
	first, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, false)
	if err != nil || !first.OK || first.Cached || calls.Load() != 1 {
		t.Fatalf("initial refresh: %+v %v", first, err)
	}
	for i := 0; i < 3; i++ {
		cached, err := svc.CachedUsage(c.ID, c.Credentials[0].ID)
		if err != nil || !cached.OK || !cached.Cached || cached.RefreshDue || !cached.CapturedAt.Equal(first.CapturedAt) {
			t.Fatalf("cached: %+v %v", cached, err)
		}
	}
	if _, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, false); err != nil || calls.Load() != 1 {
		t.Fatal("fresh automatic call hit upstream")
	}
	status.Store(429)
	failed, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, true)
	if err != nil || !failed.OK || !failed.Stale || failed.Detail == "" || !failed.CapturedAt.Equal(first.CapturedAt) || calls.Load() != 2 {
		t.Fatalf("failed refresh lost snapshot: %+v %v", failed, err)
	}
	// Failed-attempt metadata survives a new service and suppresses immediate retry.
	svc = NewService(s, server.Client(), testEndpoints(server))
	if report, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, false); err != nil || report.Detail == "" || calls.Load() != 2 {
		t.Fatalf("failed-attempt retry storm: %+v %v", report, err)
	}
	settings, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	settings.QuotaRefreshSeconds = &zero
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	manual, err := svc.CachedUsage(c.ID, c.Credentials[0].ID)
	if err != nil || manual.RefreshDue || manual.RefreshSeconds != 0 {
		t.Fatalf("manual mode: %+v %v", manual, err)
	}
	if _, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, false); err != nil || calls.Load() != 2 {
		t.Fatal("manual mode queried automatically")
	}
	status.Store(0)
	if report, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, true); err != nil || !report.OK || report.Detail != "" || calls.Load() != 3 {
		t.Fatalf("manual refresh: %+v %v", report, err)
	}
	// Expiring the snapshot does not enable refresh when setting is zero.
	current, _ := s.Channel(c.ID)
	old := Usage{CapturedAt: time.Now().Add(-time.Hour), Buckets: []UsageBucket{}}
	raw, _ := json.Marshal(old)
	if err := s.SaveQuotaCache(current, c.Credentials[0].ID, store.QuotaCache{Snapshot: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, false); err != nil || calls.Load() != 3 {
		t.Fatal("old cache bypassed manual-only setting")
	}
	seconds := 30
	settings.QuotaRefreshSeconds = &seconds
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, false); err != nil || calls.Load() != 4 {
		t.Fatal("expired cache did not refresh")
	}
}

func TestUsageCacheConcurrentRefreshCoalescesAndGETDoesNotWait(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		io.WriteString(w, `{"rate_limit":{"allowed":true}}`)
	}))
	defer server.Close()
	defer unblock()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := codexChannel(t, s, server.URL)
	if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "valid", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s, server.Client(), testEndpoints(server))
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, true)
			results <- err
		}()
	}
	<-entered
	readDone := make(chan error, 1)
	go func() { _, err := svc.CachedUsage(c.ID, c.Credentials[0].ID); readDone <- err }()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cache GET waited on network lock")
	}
	time.Sleep(50 * time.Millisecond)
	unblock()
	for i := 0; i < 8; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent refreshes issued %d requests", calls.Load())
	}
}

func TestUsageCachePoolIsolationAndRotationSharesLifecycle(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "second-refresh" {
				t.Error("rotated wrong member")
			}
			json.NewEncoder(w).Encode(TokenResponse{AccessToken: "second-current", RefreshToken: "second-rotated", ExpiresIn: 3600})
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer first-current":
			if r.Header.Get("ChatGPT-Account-ID") != "first-account" {
				t.Error("first account header mismatch")
			}
			io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":11}}}`)
		case "Bearer second-current":
			if r.Header.Get("ChatGPT-Account-ID") != "second-account" {
				t.Error("second account header mismatch")
			}
			io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":77}}}`)
		default:
			t.Error("unexpected credential")
			w.WriteHeader(401)
		}
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	one, two := *c.OAuth, *c.OAuth
	one.Account, one.IDToken, one.AccessToken, one.ExpiresAt = "first-account", "", "first-current", time.Now().Add(time.Hour)
	two.Account, two.IDToken, two.RefreshToken = "second-account", "", "second-refresh"
	c.Credentials = []store.CredentialItem{{ID: "one", OAuth: &one, Enabled: true}, {ID: "two", OAuth: &two, Enabled: true}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s, server.Client(), Endpoints{Usage: server.URL + "/usage", Token: server.URL + "/token"})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = svc.RefreshUsage(context.Background(), c.ID, "two", true)
			} else {
				_, err = svc.UsageForCredential(context.Background(), c, "two")
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if exchanges.Load() != 1 {
		t.Fatalf("display/passive reads rotated %d times", exchanges.Load())
	}
	if _, err := svc.RefreshUsage(context.Background(), c.ID, "one", true); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]float64{"one": 11, "two": 77} {
		got, err := svc.CachedUsage(c.ID, id)
		if err != nil || !got.OK || got.Buckets[0].Primary == nil || *got.Buckets[0].Primary.UsedPercent != want {
			t.Fatalf("%s quota: %+v %v", id, got, err)
		}
	}
	current, _ := s.Channel(c.ID)
	if current.Credentials[0].OAuth.RefreshToken != one.RefreshToken || current.Credentials[1].OAuth.RefreshToken != "second-rotated" {
		t.Fatal("rotation changed sibling credential")
	}
}

func TestUsageCacheRejectsReceiptAfterSelectedIdentityChanges(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"plan_type":"old-identity","rate_limit":{"allowed":true}}`)
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "current", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s, server.Client(), Endpoints{Usage: server.URL})
	done := make(chan error, 1)
	go func() { _, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, true); done <- err }()
	<-entered
	live, _ := s.Channel(c.ID)
	live.Credentials[0].OAuth.Account = "new-identity"
	if err := s.SaveChannel(&live); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("accepted old identity's in-flight receipt")
	}
	got, err := svc.CachedUsage(c.ID, c.Credentials[0].ID)
	if err != nil || got.OK {
		t.Fatalf("old receipt became visible: %+v %v", got, err)
	}
}

func TestUsageCacheAnonymousRotationAndReauthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			json.NewEncoder(w).Encode(TokenResponse{AccessToken: "anonymous-current", RefreshToken: "anonymous-rotated", ExpiresIn: 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer anonymous-current" {
			t.Error("usage did not use rotated anonymous credential")
		}
		io.WriteString(w, `{"rate_limit":{"allowed":true}}`)
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	c.Credentials[0].OAuth.Account, c.Credentials[0].OAuth.Subject, c.Credentials[0].OAuth.IDToken = "", "", ""
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s, server.Client(), Endpoints{Token: server.URL + "/token", Usage: server.URL + "/usage"})
	if got, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, true); err != nil || !got.OK {
		t.Fatalf("anonymous rotation lost receipt: %+v %v", got, err)
	}
	if got, err := svc.CachedUsage(c.ID, c.Credentials[0].ID); err != nil || !got.OK {
		t.Fatalf("anonymous receipt not cached: %+v %v", got, err)
	}
	old, _ := s.Channel(c.ID)
	if err := s.ReplaceCodexAuthorization(c.ID, store.AuthorizationResult{AccessToken: "new-authorization", RefreshToken: "new-anonymous-identity", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.CachedUsage(c.ID, c.Credentials[0].ID); err != nil || got.OK {
		t.Fatalf("anonymous reauthorization inherited old quota: %+v %v", got, err)
	}
	if err := s.SaveQuotaCache(old, c.Credentials[0].ID, store.QuotaCache{}); err == nil {
		t.Fatal("old authorization's receipt accepted")
	}
}

func TestUsageCachePreservesServerGeneratedGeneralProvenance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"rate_limit":{"limit_id":"vendor-main","general":false,"allowed":true},"additional_rate_limits":{"rate_limit":{"general":true,"allowed":false}}}`)
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "current", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s, server.Client(), Endpoints{Usage: server.URL})
	if _, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := svc.CachedUsage(c.ID, c.Credentials[0].ID)
	if err != nil || !got.OK || len(got.Buckets) != 2 || got.Buckets[0].ID != "vendor-main" || !got.Buckets[0].General || got.Buckets[1].ID != "rate_limit" || got.Buckets[1].General {
		t.Fatalf("cached source provenance lost or forged: %+v %v", got, err)
	}
}
