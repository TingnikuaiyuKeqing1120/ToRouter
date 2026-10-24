package codex

import (
	"context"
	"encoding/json"
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

func TestUsageForCredentialSelectsPoolMemberWithoutAccountFallback(t *testing.T) {
	for _, blank := range []bool{false, true} {
		t.Run(map[bool]string{false: "named", true: "empty_account"}[blank], func(t *testing.T) {
			var seenMu sync.Mutex
			var seen []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenMu.Lock()
				seen = append(seen, r.Header.Get("Authorization")+"/"+r.Header.Get("ChatGPT-Account-ID"))
				seenMu.Unlock()
				io.WriteString(w, `{"rate_limit":{"allowed":true}}`)
			}))
			defer server.Close()
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			c := codexChannel(t, st, "https://chatgpt.com/backend-api/codex")
			one, two := *c.OAuth, *c.OAuth
			one.AccessToken, one.RefreshToken, one.IDToken, one.ExpiresAt = "first-access", "first-refresh", "", time.Now().Add(time.Hour)
			two.AccessToken, two.RefreshToken, two.IDToken, two.ExpiresAt = "second-access", "second-refresh", "", time.Now().Add(time.Hour)
			two.Account = "second-account"
			if blank {
				one.Account = ""
				two.Account = ""
			}
			c.Credentials = []store.CredentialItem{{ID: "first", OAuth: &one, Enabled: true}, {ID: "second", OAuth: &two, Enabled: true}}
			if err := st.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			svc := NewService(st, server.Client(), Endpoints{Usage: server.URL})
			for _, id := range []string{"second", "first"} {
				if _, err := svc.UsageForCredential(context.Background(), c, id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.UsageForCredential(context.Background(), c, "removed"); err == nil {
				t.Fatal("missing credential fell back to primary")
			}
			if len(seen) != 2 || !strings.HasPrefix(seen[0], "Bearer second-access/") || !strings.HasPrefix(seen[1], "Bearer first-access/") {
				t.Fatalf("wrong pool credential: %v", seen)
			}
			if !blank && seen[0] != "Bearer second-access/second-account" {
				t.Fatalf("wrong account header: %v", seen)
			}
			// Matching empty account strings are not evidence. Even an unchanged
			// pool ID must reject a replaced anonymous renewable credential.
			current, err := st.Channel(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			current.Credentials[1].OAuth.RefreshToken = "replacement-refresh"
			current.Credentials[1].OAuth.Account = "replacement-account"
			if blank {
				current.Credentials[1].OAuth.Account = ""
			}
			if err := st.SaveChannel(&current); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.UsageForCredential(context.Background(), c, "second"); err == nil {
				t.Fatal("replaced account accepted stale caller")
			}
			if len(seen) != 2 {
				t.Fatal("replaced identity made network query")
			}
		})
	}
}

func TestUsageForCredentialSharesRotationWithoutTouchingPrimary(t *testing.T) {
	var exchanges, reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "secondary-refresh" {
				t.Error("rotated wrong credential")
			}
			json.NewEncoder(w).Encode(TokenResponse{AccessToken: "secondary-current", RefreshToken: "secondary-rotated", ExpiresIn: 3600})
			return
		}
		reads.Add(1)
		if r.Header.Get("Authorization") != "Bearer secondary-current" || r.Header.Get("ChatGPT-Account-ID") != "secondary-account" {
			t.Error("queried wrong account")
		}
		io.WriteString(w, `{"rate_limit":{"allowed":false}}`)
	}))
	defer server.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := codexChannel(t, st, "https://chatgpt.com/backend-api/codex")
	secondary := *c.OAuth
	secondary.IDToken = ""
	secondary.Account = "secondary-account"
	secondary.RefreshToken = "secondary-refresh"
	c.Credentials = append(c.Credentials, store.CredentialItem{ID: "secondary", OAuth: &secondary, Enabled: true})
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, server.Client(), Endpoints{Token: server.URL + "/token", Usage: server.URL + "/usage"})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.UsageForCredential(context.Background(), c, "secondary"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if exchanges.Load() != 1 || reads.Load() != 6 {
		t.Fatalf("exchanges=%d reads=%d", exchanges.Load(), reads.Load())
	}
	current, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.OAuth.RefreshToken != "rt-stored" || current.Credentials[1].OAuth.RefreshToken != "secondary-rotated" {
		t.Fatal("rotation wrote to primary instead of selected member")
	}
}

func TestUsageBucketGeneralProvenanceCannotBeForgedByExtraID(t *testing.T) {
	u, err := parseUsage([]byte(`{"rate_limit":{"limit_id":"vendor-main","general":false,"allowed":true},"code_review_rate_limit":{"limit_id":"rate_limit","general":true,"allowed":false},"additional_rate_limits":{"rate_limit":{"general":true,"allowed":false}}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Buckets) != 3 || !u.Buckets[0].General || u.Buckets[1].General || u.Buckets[2].General {
		t.Fatalf("wrong source provenance: %+v", u.Buckets)
	}
	// The server-generated provenance must survive JSON display/cache transport.
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var displayed Usage
	if err := json.Unmarshal(raw, &displayed); err != nil {
		t.Fatal(err)
	}
	if len(displayed.Buckets) != 3 || displayed.Buckets[0].ID != "vendor-main" || !displayed.Buckets[0].General || displayed.Buckets[1].General || displayed.Buckets[2].General {
		t.Fatalf("display lost or forged source provenance: %s", raw)
	}
}

func TestUsageForCredentialRotationKeepsBearerSnapshotBoundToSubjectAndPoolID(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "secondary-refresh" {
				t.Error("rotated a different pool member")
			}
			json.NewEncoder(w).Encode(TokenResponse{AccessToken: "secondary-current", RefreshToken: "secondary-rotated", ExpiresIn: 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer secondary-current" {
			t.Error("usage read a different pool member")
		}
		io.WriteString(w, `{"rate_limit":{"allowed":true}}`)
	}))
	defer server.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := codexChannel(t, st, "https://chatgpt.com/backend-api/codex")
	secondary := *c.OAuth
	secondary.Account, secondary.IDToken, secondary.Subject = "", "", "stable-secondary-subject"
	secondary.RefreshToken = "secondary-refresh"
	c.Credentials = append(c.Credentials, store.CredentialItem{ID: "secondary", OAuth: &secondary, Enabled: true})
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, server.Client(), Endpoints{Token: server.URL + "/token", Usage: server.URL + "/usage"})
	if _, err := svc.UsageForCredential(context.Background(), c, "secondary"); err != nil {
		t.Fatal(err)
	}
	// The gateway selected this credential before the auxiliary usage read.
	// Its local snapshot intentionally still contains the expired old token.
	selected := c
	selected.OAuth = c.Credentials[1].OAuth
	selected.Credentials = []store.CredentialItem{c.Credentials[1]}
	if token, err := svc.Bearer(context.Background(), selected); err != nil || token != "secondary-current" {
		t.Fatalf("quota rotation broke the selected inference credential: token=%q error=%v", token, err)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("rotated twice after a successful usage refresh: %d", exchanges.Load())
	}
	// A subject on another pool ID is insufficient. Removing the selected ID
	// must fail even when a live sibling has that same subject and account.
	live, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	live.Credentials[1].ID = "replacement-id"
	if err := st.SaveChannel(&live); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Bearer(context.Background(), selected); err == nil {
		t.Fatal("old selected ID fell back to a sibling with the same subject")
	}
	// Conversely, reusing the ID for another subject must not establish the
	// original account, even though both Account fields remain empty.
	live.Credentials[1].ID = "secondary"
	live.Credentials[1].OAuth.Subject = "different-subject"
	if err := st.SaveChannel(&live); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Bearer(context.Background(), selected); err == nil {
		t.Fatal("empty account matched a replaced subject")
	}
}
