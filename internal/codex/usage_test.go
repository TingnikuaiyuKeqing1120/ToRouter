package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestUsageDefaultURL(t *testing.T) {
	t.Setenv("TOROUTER_CODEX_USAGE_URL", "")
	t.Setenv("TOROUTER_CODEX_BACKEND_URL", "")
	if got := DefaultEndpoints().Usage; got != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("usage URL %q", got)
	}
	t.Setenv("TOROUTER_CODEX_BACKEND_URL", "http://127.0.0.1:1234/backend-api/codex/")
	if got := DefaultEndpoints().Usage; got != "http://127.0.0.1:1234/backend-api/wham/usage" {
		t.Fatalf("custom backend usage URL %q", got)
	}
	t.Setenv("TOROUTER_CODEX_USAGE_URL", "http://127.0.0.1:4321/custom-usage")
	if got := DefaultEndpoints().Usage; got != "http://127.0.0.1:4321/custom-usage" {
		t.Fatalf("explicit usage URL %q", got)
	}
}

func TestUsageCancellationIncludesLifecycleWait(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := NewService(st, nil, Endpoints{})
	lock := svc.lockFor(123)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := svc.Usage(ctx, store.Channel{ID: 123}); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unexpected wait error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("usage stayed queued after deadline")
	}
}

func TestUsageRefreshRejectsRedirectAndRedactsFailure(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	var mode atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 0 {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"rt-stored synthetic-secret"}`)
	}))
	defer issuer.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := codexChannel(t, s, issuer.URL)
	svc := NewService(s, issuer.Client(), testEndpoints(issuer))
	if _, err := svc.Usage(context.Background(), c); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() != 0 {
		t.Fatal("refresh token forwarded on redirect")
	}
	mode.Store(1)
	if _, err := svc.Usage(context.Background(), c); err == nil || strings.Contains(err.Error(), "rt-stored") || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("refresh detail leaked: %v", err)
	}
	got, err := s.Channel(c.ID)
	if err != nil || got.OAuth.Status != store.OAuthStatusReauthRequired || !got.Enabled {
		t.Fatalf("refresh failure semantics changed: %+v %v", got, err)
	}
}

func TestUsageOptionalWindowsAndObjectBuckets(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	u, err := parseUsage([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":{"reset_after_seconds":30}},"additional_rate_limits":{"weekly":{"limit_name":"Weekly","allowed":false,"rate_limit":{"allowed":true,"primary_window":{"reset_at":0,"reset_after_seconds":30}}},"empty":null}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Buckets) != 2 || u.Buckets[0].Primary != nil || u.Buckets[0].Allowed != nil || u.Buckets[0].Secondary.UsedPercent != nil || *u.Buckets[0].Secondary.ResetAt != now.Unix()+30 {
		t.Fatalf("missing values or relative reset: %+v", u)
	}
	extra := u.Buckets[1]
	if extra.ID != "weekly" || extra.Name != "Weekly" || *extra.Allowed || *extra.Primary.ResetAt != 0 {
		t.Fatalf("object bucket: %+v", extra)
	}
	for _, body := range []string{`{}`, `{"rate_limit":null,"additional_rate_limits":null}`} {
		u, err = parseUsage([]byte(body), now)
		if err != nil || u.Buckets == nil || len(u.Buckets) != 0 {
			t.Fatalf("empty snapshot: %+v %v", u, err)
		}
	}
}

func TestUsageFailuresAreBoundedAndDoNotExposeUpstream(t *testing.T) {
	secret := "synthetic-credential-never-display"
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"html", 200, "<html>" + secret + "</html>"},
		{"wrong_shape", 200, `["` + secret + `"]`},
		{"oversize", 200, `{"ignored":"` + strings.Repeat("x", usageBodyLimit) + secret + `"}`},
		{"unauthorized", 401, secret},
		{"rate_limited", 429, secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer s.Close()
			_, err := FetchUsage(context.Background(), s.Client(), Endpoints{Usage: s.URL}, secret, "account")
			if err == nil || len(err.Error()) > 160 || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "<html>") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	if _, err := FetchUsage(context.Background(), source.Client(), Endpoints{Usage: source.URL}, secret, "account"); err == nil {
		t.Fatal("accepted redirect")
	}
	if redirected.Load() != 0 {
		t.Fatal("forwarded account credential on redirect")
	}
}

func TestUsageAndBearerShareRefreshAndCurrentAccount(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var exchanges, reads atomic.Int32
	access := mustIDToken(t, "claim-account", "test@example.test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			exchanges.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "rt-stored" {
				t.Error("used stale refresh credential")
			}
			json.NewEncoder(w).Encode(TokenResponse{AccessToken: access, RefreshToken: "rotated-refresh", IDToken: mustIDToken(t, "id-token-account", "test@example.test"), ExpiresIn: 3600})
			return
		}
		reads.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+access || r.Header.Get("ChatGPT-Account-ID") != "claim-account" {
			t.Error("usage did not use current access claim")
		}
		io.WriteString(w, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":100}}}`)
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := NewService(s, server.Client(), testEndpoints(server))
	c := codexChannel(t, s, server.URL)
	// A stale caller snapshot with a still-valid token must not win over storage.
	stale := c
	copyOAuth := *c.OAuth
	stale.OAuth = &copyOAuth
	stale.OAuth.AccessToken = "stale-access"
	stale.OAuth.ExpiresAt = time.Now().Add(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if _, err := svc.Usage(context.Background(), stale); err != nil {
					t.Error(err)
				}
			} else if _, err := svc.Bearer(context.Background(), c); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if exchanges.Load() != 1 || reads.Load() != 4 {
		t.Fatalf("refresh=%d reads=%d", exchanges.Load(), reads.Load())
	}
	current, err := s.Channel(c.ID)
	if err != nil || !current.Enabled || current.OAuth.Status != store.OAuthStatusActive {
		t.Fatalf("quota altered channel state: %+v %v", current, err)
	}
}

func TestUsageFailureDoesNotChangeChannelState(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, status := range []int{401, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, "synthetic-secret")
			}))
			defer server.Close()
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			c := codexChannel(t, s, server.URL)
			if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "current", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			svc := NewService(s, server.Client(), testEndpoints(server))
			if _, err := svc.Usage(context.Background(), c); err == nil {
				t.Fatal("accepted upstream failure")
			}
			got, err := s.Channel(c.ID)
			if err != nil || got.Enabled != c.Enabled || got.OAuth.Status != c.OAuth.Status || got.OAuth.StatusReason != c.OAuth.StatusReason {
				t.Fatalf("usage failure changed route/auth state: %+v %v", got, err)
			}
		})
	}
}

func TestUsageLifecycleOrdersReauthorizationAndDeletion(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, action := range []string{"reauthorize", "delete"} {
		t.Run(action, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
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
			if err := s.ReplaceOAuthTokens(c.ID, store.OAuthTokens{AccessToken: "before", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			svc := NewService(s, server.Client(), testEndpoints(server))
			readDone := make(chan error, 1)
			go func() { _, err := svc.Usage(context.Background(), c); readDone <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("usage did not start")
			}
			changed := make(chan error, 1)
			go func() {
				if action == "delete" {
					changed <- svc.DisconnectChannel(c.ID)
				} else {
					changed <- svc.ReauthorizeChannel(c.ID, LoginInput{Account: c.OAuth.Account, Tokens: TokenResponse{AccessToken: "after", RefreshToken: "after-refresh", ExpiresIn: 3600}})
				}
			}()
			select {
			case err := <-changed:
				t.Fatalf("lifecycle passed in-flight snapshot: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			unblock()
			if err := <-readDone; err != nil {
				t.Fatal(err)
			}
			if err := <-changed; err != nil {
				t.Fatal(err)
			}
			got, err := s.Channel(c.ID)
			if action == "delete" {
				if err != store.ErrNotFound {
					t.Fatalf("delete: %v", err)
				}
				if _, err := svc.Usage(context.Background(), c); err == nil {
					t.Fatal("stale deleted channel accepted")
				}
			} else if err != nil || got.OAuth.AccessToken != "after" {
				t.Fatalf("reauth lost: %+v %v", got, err)
			}
		})
	}
}

func TestUsageRawContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") != "no-cache" || r.Header.Get("Pragma") != "no-cache" {
			t.Error("usage read must bypass caches")
		}
		if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("ChatGPT-Account-ID") != "acct-test" {
			t.Error("missing account credential")
		}
		for name := range r.Header {
			if strings.Contains(strings.ToLower(name), "reserve") || name == "Origin" || name == "Referer" {
				t.Errorf("unexpected passive usage header %s", name)
			}
		}
		io.WriteString(w, `{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":1900000000},"secondary_window":{"used_percent":12.5,"limit_window_seconds":604800}},"additional_rate_limits":[{"limit_id":"spark","limit_name":"Spark","rate_limit":{"allowed":false,"primary_window":{"used_percent":0,"limit_window_seconds":0,"reset_at":0}}}],"code_review_rate_limit":{"primary_window":{"used_percent":null}}}`)
	}))
	defer server.Close()
	u, err := FetchUsage(context.Background(), server.Client(), Endpoints{Usage: server.URL}, "test-access", "acct-test")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(u)
	var got map[string]any
	json.Unmarshal(b, &got)
	if got["plan_type"] != "plus" {
		t.Fatalf("missing plan: %s", b)
	}
	buckets, ok := got["buckets"].([]any)
	if !ok || len(buckets) != 3 {
		t.Fatalf("buckets: %s", b)
	}
	main := buckets[0].(map[string]any)
	if main["allowed"] != true || main["limit_reached"] != false {
		t.Fatalf("permission rewritten: %s", b)
	}
	p := main["primary"].(map[string]any)
	if p["used_percent"] != float64(100) || p["window_seconds"] != float64(18000) || p["reset_at"] != float64(1900000000) {
		t.Fatalf("raw units: %s", b)
	}
	if main["secondary"].(map[string]any)["used_percent"] != 12.5 {
		t.Fatalf("fraction lost: %s", b)
	}
	byID := map[string]map[string]any{}
	for _, bucket := range buckets {
		v := bucket.(map[string]any)
		byID[v["id"].(string)] = v
	}
	zero := byID["spark"]["primary"].(map[string]any)
	if zero["used_percent"] != float64(0) || zero["window_seconds"] != float64(0) || zero["reset_at"] != float64(0) || byID["spark"]["allowed"] != false {
		t.Fatalf("zero lost: %s", b)
	}
	unknown := byID["code_review_rate_limit"]["primary"].(map[string]any)
	if unknown["used_percent"] != nil {
		t.Fatalf("unknown fabricated: %s", b)
	}
}
