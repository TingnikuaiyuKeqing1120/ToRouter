package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

func TestCodexUsageSnapshotContract(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		if fail.Load() {
			w.WriteHeader(429)
			io.WriteString(w, "synthetic-private-token")
			return
		}
		if r.Header.Get("Authorization") == "Bearer second-private-token" {
			if r.Header.Get("ChatGPT-Account-ID") != "second-account" {
				t.Error("secondary request used the primary account")
			}
			io.WriteString(w, `{"plan_type":"second-plan","rate_limit":{"primary_window":{"used_percent":77}}}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-private-token" || r.Header.Get("ChatGPT-Account-ID") != "test-account" {
			t.Error("primary request used wrong credential")
		}
		io.WriteString(w, `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":0,"limit_window_seconds":7200,"reset_at":1900000000}},"additional_rate_limits":{"review":{"primary_window":{"used_percent":null}}}}`)
	}))
	defer upstream.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	eps := codex.DefaultEndpoints()
	eps.Usage = upstream.URL + "/backend-api/wham/usage"
	svc := codex.NewService(s, upstream.Client(), eps)
	c := codex.ChannelFor(codex.LoginInput{Account: "test-account", APIBase: upstream.URL, Tokens: codex.TokenResponse{AccessToken: "synthetic-private-token", RefreshToken: "test-refresh", ExpiresIn: 3600}})
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	secondary := *c.OAuth
	secondary.Account, secondary.AccessToken, secondary.RefreshToken = "second-account", "second-private-token", "second-refresh-token"
	c.Credentials = append(c.Credentials, store.CredentialItem{ID: "second", Enabled: true, OAuth: &secondary})
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, nil, nil, svc)
	defer ReleaseOAuthListeners(h)
	path := "/oauth/codex/usage?channel_id=" + strconv.FormatInt(c.ID, 10) + "&credential_id=" + c.Credentials[0].ID
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, invalid := range []struct {
			path   string
			status int
		}{
			{"/oauth/codex/usage?channel_id=" + strconv.FormatInt(c.ID, 10), 400},
			{"/oauth/codex/usage?credential_id=" + c.Credentials[0].ID, 400},
			{"/oauth/codex/usage?channel_id=" + strconv.FormatInt(c.ID, 10) + "&credential_id=removed", 404},
		} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, invalid.path, nil))
			if w.Code != invalid.status {
				t.Fatalf("%s %s: %d %s", method, invalid.path, w.Code, w.Body.String())
			}
		}
	}
	empty := oauthCall(t, h, http.MethodGet, path)
	if empty["ok"] != false || empty["refresh_due"] != true {
		t.Fatalf("cache miss: %+v", empty)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response %d headers=%v", w.Code, w.Header())
	}
	var got struct {
		OK bool `json:"ok"`
		codex.Usage
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.CapturedAt.IsZero() || time.Since(got.CapturedAt) > time.Minute || got.PlanType != "plus" || len(got.Buckets) != 2 {
		t.Fatalf("snapshot: %s", w.Body.String())
	}
	if p := got.Buckets[0].Primary; p == nil || p.UsedPercent == nil || *p.UsedPercent != 0 || *p.WindowSeconds != 7200 {
		t.Fatalf("zero/dynamic window: %s", w.Body.String())
	}
	if got.Buckets[1].Primary.UsedPercent != nil {
		t.Fatal("unknown value became zero")
	}
	if strings.Contains(w.Body.String(), "window_minutes") || strings.Contains(w.Body.String(), "synthetic-private-token") {
		t.Fatalf("old contract or credential leak: %s", w.Body.String())
	}
	secondPath := "/oauth/codex/usage?channel_id=" + strconv.FormatInt(c.ID, 10) + "&credential_id=second"
	second := oauthCall(t, h, http.MethodPost, secondPath)
	if second["ok"] != true || second["plan_type"] != "second-plan" || second["refresh_seconds"] != float64(300) {
		t.Fatalf("secondary report: %+v", second)
	}
	if primary := oauthCall(t, h, http.MethodGet, path); primary["plan_type"] != "plus" || primary["captured_at"] != got.CapturedAt.Format(time.RFC3339Nano) {
		t.Fatalf("secondary overwrote primary: %+v", primary)
	}
	fail.Store(true)
	failure := oauthCall(t, h, http.MethodPost, path+"&force=1")
	if failure["ok"] != true || failure["stale"] != true || failure["buckets"] == nil || strings.Contains(failure["detail"].(string), "synthetic-private-token") {
		t.Fatalf("failure: %+v", failure)
	}
	if failure["captured_at"] != got.CapturedAt.Format(time.RFC3339Nano) {
		t.Fatal("failed refresh replaced snapshot time")
	}
	cached := oauthCall(t, h, http.MethodGet, path)
	if cached["cached"] != true || cached["refresh_due"] != false || cached["detail"] != failure["detail"] {
		t.Fatalf("cached failure metadata: %+v", cached)
	}
	after, err := s.Channel(c.ID)
	if err != nil || after.Enabled != c.Enabled || after.OAuth.Status != c.OAuth.Status {
		t.Fatalf("quota failure altered routing: %+v %v", after, err)
	}
}

func TestCodexUsageLockWaitFailureKeepsSnapshotAndRefreshSchedule(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"rate_limit":{"allowed":true}}`)
	}))
	defer upstream.Close()
	defer unblock()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := codex.ChannelFor(codex.LoginInput{Account: "one", Tokens: codex.TokenResponse{AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresIn: 3600}})
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"two", "three", "four"} {
		member := *c.OAuth
		member.Account = id
		c.Credentials = append(c.Credentials, store.CredentialItem{ID: id, OAuth: &member, Enabled: true})
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	old := codex.Usage{CapturedAt: time.Now().Add(-10 * time.Minute), PlanType: "old-four", Buckets: []codex.UsageBucket{{ID: "vendor-main", General: true}}}
	raw, _ := json.Marshal(old)
	record := store.QuotaCache{Snapshot: raw, LastAttemptAt: old.CapturedAt}
	if err := s.SaveQuotaCache(c, "four", record); err != nil {
		t.Fatal(err)
	}
	svc := codex.NewService(s, upstream.Client(), codex.Endpoints{Usage: upstream.URL})
	h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, nil, nil, svc)
	defer ReleaseOAuthListeners(h)
	done := make(chan error, 1)
	go func() { _, err := svc.RefreshUsage(context.Background(), c.ID, c.Credentials[0].ID, true); done <- err }()
	defer func() {
		unblock()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	<-entered
	for _, seconds := range []int{60, 0} {
		if _, err := s.PatchQuotaRefresh(seconds); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"two", "three", "four"} {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			path := "/oauth/codex/usage?channel_id=" + strconv.FormatInt(c.ID, 10) + "&credential_id=" + id + "&force=1"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil).WithContext(ctx))
			cancel()
			if w.Code != 200 {
				t.Fatalf("wait failure %d %s", w.Code, w.Body.String())
			}
			var got codex.UsageReport
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Detail == "" || got.RefreshSeconds != seconds || got.RefreshDue || got.LastAttemptAt == nil {
				t.Fatalf("wait failure lacks scheduling metadata: %s", w.Body.String())
			}
			if seconds == 0 {
				if got.NextRefreshAt != nil {
					t.Fatal("manual mode scheduled automatic retry")
				}
			} else if got.NextRefreshAt == nil || !got.NextRefreshAt.After(time.Now()) || got.NextRefreshAt.Sub(*got.LastAttemptAt) != time.Duration(seconds)*time.Second {
				t.Fatalf("bad retry time: %s", w.Body.String())
			}
			if id == "four" {
				if !got.OK || !got.Stale || got.PlanType != "old-four" || !got.CapturedAt.Equal(old.CapturedAt) {
					t.Fatalf("wait failure discarded cached snapshot: %s", w.Body.String())
				}
			} else if got.OK || got.Usage != nil {
				t.Fatalf("wait failure inherited sibling snapshot: %s", w.Body.String())
			}
		}
	}
	after, err := s.LoadQuotaCache(c.ID, "four")
	if err != nil || after.Detail != "" || !after.LastAttemptAt.Equal(record.LastAttemptAt) {
		t.Fatalf("cancelled waiter wrote cache attempt metadata: %+v %v", after, err)
	}
}
