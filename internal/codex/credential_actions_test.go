package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/store"
)

const testRedeemID = "12345678-1234-4234-8234-123456789abc"

func actionTestChannel(t *testing.T, st *store.Store, name string) store.Channel {
	t.Helper()
	c := codexChannel(t, st, "https://chatgpt.com/backend-api/codex")
	c.Name = name
	one, two := *c.OAuth, *c.OAuth
	one.IDToken, two.IDToken = "", ""
	one.AccessToken, one.RefreshToken, one.Account = name+"-first", name+"-refresh-first", name+"-account-first"
	two.AccessToken, two.RefreshToken, two.Account = name+"-second", name+"-refresh-second", name+"-account-second"
	one.ExpiresAt, two.ExpiresAt = time.Now().Add(time.Hour), time.Now().Add(time.Hour)
	c.Credentials = []store.CredentialItem{{ID: "first", Enabled: true, OAuth: &one}, {ID: "second", Enabled: true, OAuth: &two}}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func actionTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func requireActionError(t *testing.T, err error, unknown bool) {
	t.Helper()
	var action *CredentialActionError
	if !errors.As(err, &action) || action.Unknown != unknown {
		t.Fatalf("error=%v, want Unknown=%v", err, unknown)
	}
	if len(err.Error()) > 180 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestResetCreditsParsingPreservesUnknownAndAvailability(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, body := range []string{`{}`, `{"credits":null}`, `{"available_count":null}`, `{"credits":[{"status":"unknown"}]}`, `{"credits":[{}]}`} {
		got, err := parseResetCredits([]byte(body), now)
		if err != nil || got.AvailableCount != nil || got.Credits == nil {
			t.Fatalf("unknown report %s: %+v %v", body, got, err)
		}
	}
	got, err := parseResetCredits([]byte(`{"data":{"credits":[]}}`), now)
	if err != nil || got.AvailableCount == nil || *got.AvailableCount != 0 {
		t.Fatalf("explicit empty: %+v %v", got, err)
	}
	got, err = parseResetCredits([]byte(`{"data":{"availableCount":3,"credits":[
		{"id":"live","status":"available","type":"rate_limit","grantedAt":1799999999000,"expiresAt":"2027-01-15T08:00:01Z"},
		{"id":"expired","status":"available","expires_at":1800000000},
		{"id":"redeemed","status":"available","redeemed_at":1799999999},
		{"id":"unknown","status":"surprise"},
		{"id":"spent","status":"redeemed"}]}}`), now)
	if err != nil || got.AvailableCount == nil || *got.AvailableCount != 3 || len(got.Credits) != 5 {
		t.Fatalf("parse: %+v %v", got, err)
	}
	if !got.Credits[0].Available || *got.Credits[0].GrantedAt != now.Unix()-1 || *got.Credits[0].ExpiresAt != now.Unix()+1 {
		t.Fatalf("live/time: %+v", got.Credits[0])
	}
	for _, credit := range got.Credits[1:] {
		if credit.Available {
			t.Fatalf("unavailable credit marked available: %+v", credit)
		}
	}
	for _, body := range []string{`null`, `[]`, `{`, `{"data":null}`, `{"credits":{}}`, `{"credits":[null]}`, `{"credits":[{"id":123}]}`, `{"credits":[{"expires_at":"bad"}]}`, `{"available_count":-1}`, `{"available_count":"3"}`, `{"available_count":1.5}`} {
		if _, err := parseResetCredits([]byte(body), now); err == nil {
			t.Errorf("accepted damaged report: %s", body)
		}
	}
}

func TestResetCreditCountInUsageReport(t *testing.T) {
	for _, tc := range []struct {
		body string
		want *int
		fail bool
	}{
		{body: `{}`}, {body: `{"rate_limit_reset_credits":{}}`}, {body: `{"rate_limit_reset_credits":{"available_count":null}}`},
		{body: `{"rate_limit_reset_credits":{"available_count":0}}`, want: new(int)},
		{body: `{"rateLimitResetCredits":{"availableCount":2}}`, want: func() *int { n := 2; return &n }()},
		{body: `{"rate_limit_reset_credits":{"available_count":"2"}}`, fail: true},
		{body: `{"rate_limit_reset_credits":[]}`, fail: true},
	} {
		got, err := parseUsage([]byte(tc.body), time.Now())
		if (err != nil) != tc.fail {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if tc.fail {
			continue
		}
		if (got.ResetCreditCount == nil) != (tc.want == nil) || tc.want != nil && *got.ResetCreditCount != *tc.want {
			t.Fatalf("lost unknown/zero/count: %s %+v", tc.body, got)
		}
	}
}

func TestCredentialActionsSelectExactChannelAndMember(t *testing.T) {
	st := actionTestStore(t)
	a, b := actionTestChannel(t, st, "a"), actionTestChannel(t, st, "b")
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+"/"+r.Header.Get("ChatGPT-Account-ID"))
		if r.Header.Get("x-openai-codex-luna-reserve") != "" || r.Header.Get("Originator") == "" {
			t.Error("invalid action headers")
		}
		if r.Method == "GET" {
			if r.Header.Get("Accept") != "application/json" || r.Header.Get("Cache-Control") != "no-cache" {
				t.Error("missing read headers")
			}
			io.WriteString(w, `{"credits":[]}`)
		} else if strings.HasSuffix(r.URL.Path, "/consume") {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["redeem_request_id"] != testRedeemID {
				t.Errorf("consume payload: %v %v", body, err)
			}
			io.WriteString(w, `not a known success body`)
		} else {
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		}
	}))
	defer server.Close()
	svc := NewService(st, server.Client(), Endpoints{Usage: server.URL + "/usage", Backend: server.URL})
	for _, c := range []store.Channel{a, b} {
		if _, err := svc.ResetCreditsForCredential(context.Background(), c, "second"); err != nil {
			t.Fatal(err)
		}
		if err := svc.ConsumeResetCreditForCredential(context.Background(), c, "second", testRedeemID); err != nil {
			t.Fatal(err)
		}
		if err := svc.WarmupForCredential(context.Background(), c, "second", "gpt-5-codex"); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("requests=%d", len(seen))
	}
	for i, s := range seen {
		name := "a"
		if i >= 3 {
			name = "b"
		}
		if s != "Bearer "+name+"-second/"+name+"-account-second" {
			t.Fatalf("wrong identity: %v", seen)
		}
	}
	for _, id := range []string{"", "missing"} {
		if _, err := svc.ResetCreditsForCredential(context.Background(), a, id); err == nil {
			t.Fatal("missing ID fell back")
		}
		if err := svc.ConsumeResetCreditForCredential(context.Background(), a, id, testRedeemID); err == nil {
			t.Fatal("missing ID fell back")
		}
	}
	if len(seen) != 6 {
		t.Fatal("invalid selection made network request")
	}
}

func TestCredentialActionsRejectDisabledStaleAndUnknownModel(t *testing.T) {
	st := actionTestStore(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, `{"credits":[]}`) }))
	defer server.Close()
	c := actionTestChannel(t, st, "a")
	svc := NewService(st, server.Client(), Endpoints{ResetCredits: server.URL, Backend: server.URL})
	if err := svc.WarmupForCredential(context.Background(), c, "second", "unlisted"); err == nil {
		t.Fatal("unlisted model accepted")
	}
	if err := svc.WarmupForCredential(context.Background(), c, "second", ""); err == nil {
		t.Fatal("empty model accepted")
	}
	if err := svc.ConsumeResetCreditForCredential(context.Background(), c, "second", "not-a-uuid"); err == nil {
		t.Fatal("bad request ID accepted")
	}
	for _, channelDisabled := range []bool{false, true} {
		current, _ := st.Channel(c.ID)
		current.Enabled = !channelDisabled
		current.Credentials[1].Enabled = channelDisabled
		if err := st.SaveChannel(&current); err != nil {
			t.Fatal(err)
		}
		if err := svc.ConsumeResetCreditForCredential(context.Background(), current, "second", testRedeemID); err == nil {
			t.Fatal("disabled write accepted")
		}
		if err := svc.WarmupForCredential(context.Background(), current, "second", "gpt-5-codex"); err == nil {
			t.Fatal("disabled warmup accepted")
		}
		if _, err := svc.ResetCreditsForCredential(context.Background(), current, "second"); err != nil {
			t.Fatalf("disabled manual read: %v", err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("disabled POST/unlisted model sent: %d", requests.Load())
	}
	current, _ := st.Channel(c.ID)
	current.Enabled = true
	current.Credentials[1].Enabled = true
	current.Credentials[1].OAuth.Account = "replaced-account"
	if err := st.SaveChannel(&current); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResetCreditsForCredential(context.Background(), c, "second"); err == nil {
		t.Fatal("stale identity accepted")
	}
	if err := svc.ConsumeResetCreditForCredential(context.Background(), c, "second", testRedeemID); err == nil {
		t.Fatal("stale write accepted")
	}
	if requests.Load() != 2 {
		t.Fatal("stale identity made request")
	}
}

func TestCredentialActionsRotateOnlySelectedCredential(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	st := actionTestStore(t)
	c := actionTestChannel(t, st, "a")
	c.Credentials[1].OAuth.ExpiresAt = time.Now().Add(-time.Minute)
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	var refreshes, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "a-refresh-second" {
				t.Error("wrong refresh credential")
			}
			json.NewEncoder(w).Encode(TokenResponse{AccessToken: "rotated", RefreshToken: "new-refresh", ExpiresIn: 3600})
			return
		}
		posts.Add(1)
		if r.Header.Get("Authorization") != "Bearer rotated" || r.Header.Get("ChatGPT-Account-ID") != "a-account-second" {
			t.Error("lost identity after refresh")
		}
	}))
	defer server.Close()
	svc := NewService(st, server.Client(), Endpoints{Token: server.URL + "/token", ResetCredits: server.URL + "/credits"})
	for i := 0; i < 2; i++ {
		if err := svc.ConsumeResetCreditForCredential(context.Background(), c, "second", testRedeemID); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := st.Channel(c.ID)
	if refreshes.Load() != 1 || posts.Load() != 2 || current.Credentials[0].OAuth.RefreshToken != "a-refresh-first" || current.Credentials[1].OAuth.RefreshToken != "new-refresh" {
		t.Fatal("rotation was duplicated or changed the primary")
	}
}

func TestCredentialActionAnonymousRotationKeepsProvenLineage(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	st := actionTestStore(t)
	c := actionTestChannel(t, st, "anonymous")
	c.Credentials[1].OAuth.Account = ""
	c.Credentials[1].OAuth.Subject = ""
	c.Credentials[1].OAuth.ExpiresAt = time.Now().Add(-time.Minute)
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			json.NewEncoder(w).Encode(TokenResponse{AccessToken: "new-anonymous-access", RefreshToken: "new-anonymous-refresh", ExpiresIn: 3600})
			return
		}
		posts.Add(1)
		if r.Header.Get("Authorization") != "Bearer new-anonymous-access" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Error("anonymous refresh changed identity")
		}
	}))
	defer server.Close()
	svc := NewService(st, server.Client(), Endpoints{Token: server.URL + "/token", ResetCredits: server.URL + "/credits"})
	if err := svc.ConsumeResetCreditForCredential(context.Background(), c, "second", testRedeemID); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatal("anonymous refresh did not execute the selected action")
	}
	// Once that proven refresh scope ends, an old anonymous snapshot cannot
	// establish identity for another action merely by matching empty fields.
	if err := svc.ConsumeResetCreditForCredential(context.Background(), c, "second", testRedeemID); err == nil {
		t.Fatal("stale anonymous identity accepted")
	}
	if posts.Load() != 1 {
		t.Fatal("stale anonymous action reached upstream")
	}
}

func TestConsumeResetCreditSinglePOSTAndUnknownOutcomes(t *testing.T) {
	for _, status := range []int{200, 204, 307, 400, 401, 408, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			st := actionTestStore(t)
			c := actionTestChannel(t, st, "a")
			var posts, target atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					target.Add(1)
					return
				}
				posts.Add(1)
				w.Header().Set("Location", "/target")
				w.WriteHeader(status)
				io.WriteString(w, "secret a-second")
			}))
			defer server.Close()
			svc := NewService(st, server.Client(), Endpoints{ResetCredits: server.URL + "/credits"})
			err := svc.ConsumeResetCreditForCredential(context.Background(), c, "second", testRedeemID)
			if status < 300 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireActionError(t, err, status == 408 || status == 429 || status >= 500)
			}
			if posts.Load() != 1 || target.Load() != 0 {
				t.Fatalf("replayed or redirected POST: %d/%d", posts.Load(), target.Load())
			}
		})
	}
	st := actionTestStore(t)
	c := actionTestChannel(t, st, "a")
	var posts atomic.Int32
	client := &http.Client{Transport: actionRoundTripper(func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		return nil, errors.New("secret reflected transport error")
	})}
	svc := NewService(st, client, Endpoints{ResetCredits: "https://example.invalid/credits"})
	requireActionError(t, svc.ConsumeResetCreditForCredential(context.Background(), c, "second", testRedeemID), true)
	if posts.Load() != 1 {
		t.Fatal("retried transport failure")
	}
}

type actionRoundTripper func(*http.Request) (*http.Response, error)

func (f actionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWarmupRequiresSuccessfulResponsesTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, stream     string
		success, unknown bool
	}{
		{"completed", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", true, false},
		{"done", "event: response.done\ndata: {\"response\":{\"status\":\"completed\"}}\n\n", true, false},
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", false, false},
		{"incomplete", "data: {\"type\":\"response.incomplete\"}\n\n", false, false},
		{"error", "data: {\"type\":\"error\",\"message\":\"secret\"}\n\n", false, false},
		{"eof", "data: {\"type\":\"response.created\"}\n\n", false, true},
		{"http200empty", "", false, true},
		{"done_marker", "data: [DONE]\n\n", false, true},
		{"missing_status", "data: {\"type\":\"response.completed\"}\n\n", false, true},
		{"contradiction", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\"}}\n\n", false, false},
		{"partial_terminal", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n", false, true},
		{"damaged", "data: secret\n\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := actionTestStore(t)
			c := actionTestChannel(t, st, "a")
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if r.Method != "POST" || r.URL.Path != "/responses" || r.Header.Get("Accept") != "text/event-stream" {
					t.Error("wrong warmup request")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["model"] != "gpt-5-codex" || body["instructions"] != "" || body["stream"] != true || body["store"] != false {
					t.Errorf("wrong warmup payload: %v", body)
				}
				input, _ := json.Marshal(body["input"])
				if string(input) != `[{"content":[{"text":"hi","type":"input_text"}],"role":"user","type":"message"}]` {
					t.Errorf("wrong warmup input: %s", input)
				}
				io.WriteString(w, tc.stream)
			}))
			defer server.Close()
			svc := NewService(st, server.Client(), Endpoints{Backend: server.URL})
			err := svc.WarmupForCredential(context.Background(), c, "second", "gpt-5-codex")
			if tc.success {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireActionError(t, err, tc.unknown)
			}
			if posts.Load() != 1 {
				t.Fatal("warmup replayed")
			}
		})
	}
}

func TestResetCreditsReadBoundsAndRedaction(t *testing.T) {
	st := actionTestStore(t)
	c := actionTestChannel(t, st, "a")
	for _, body := range []string{`{"credits":[{"id":"a-second","type":"a-second","status":"a-second"}]}`, `{"ignored":"` + strings.Repeat("x", usageBodyLimit) + `"}`, `{"credits":"secret"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		svc := NewService(st, server.Client(), Endpoints{ResetCredits: server.URL})
		got, err := svc.ResetCreditsForCredential(context.Background(), c, "second")
		server.Close()
		if strings.Contains(body, `"id"`) {
			raw, _ := json.Marshal(got)
			if err != nil || strings.Contains(string(raw), "a-second") || !strings.Contains(string(raw), "[redacted]") {
				t.Fatalf("reflection leak: %s %v", raw, err)
			}
		} else if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe read error: %v", err)
		}
	}
}

func TestResetCreditsEndpointDefaultsAndRejectsRedirect(t *testing.T) {
	t.Setenv("TOROUTER_CODEX_BACKEND_URL", "")
	t.Setenv("TOROUTER_CODEX_RESET_CREDITS_URL", "")
	if got := DefaultEndpoints().ResetCredits; got != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
		t.Fatalf("default endpoint: %s", got)
	}
	if got := (Endpoints{Usage: "http://example.test/wham/usage"}).resetCreditsURL(); got != "http://example.test/wham/rate-limit-reset-credits" {
		t.Fatalf("derived endpoint: %s", got)
	}
	st := actionTestStore(t)
	c := actionTestChannel(t, st, "a")
	var source, target atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			target.Add(1)
			return
		}
		source.Add(1)
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer server.Close()
	svc := NewService(st, server.Client(), Endpoints{ResetCredits: server.URL + "/credits"})
	if _, err := svc.ResetCreditsForCredential(context.Background(), c, "second"); err == nil {
		t.Fatal("redirect accepted")
	}
	if source.Load() != 1 || target.Load() != 0 {
		t.Fatal("GET forwarded credentials through redirect")
	}
}

func TestCredentialActionCancellationIncludesLockWait(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := NewService(st, nil, Endpoints{})
	lock := svc.lockFor(42)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = svc.ConsumeResetCreditForCredential(ctx, store.Channel{ID: 42}, "selected", testRedeemID)
	requireActionError(t, err, false)
	if time.Since(start) > time.Second {
		t.Fatal("action outlived lock budget")
	}
}
