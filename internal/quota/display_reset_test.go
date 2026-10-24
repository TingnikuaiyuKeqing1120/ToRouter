package quota_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/gateway"
	"torouter/internal/quota"
	"torouter/internal/store"
)

type resetTransport func(*http.Request) (*http.Response, error)

func (f resetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCodexDisplayResetDoesNotPromoteLateCacheToGatewayFact(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := store.Channel{Name: "Codex example", CredentialType: store.CredentialCodexOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		Credentials: []store.CredentialItem{{ID: "first", Enabled: true, OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderCodex, ClientID: codex.ClientID, Account: "synthetic-account",
			AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour), Status: store.OAuthStatusActive}}}}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	cred := c.Credentials[0]
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var available atomic.Bool
	client := &http.Client{Transport: resetTransport(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		body := `{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_after_seconds":3600}}}`
		if available.Load() {
			body = `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"reset_after_seconds":3600}}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	codexSvc := codex.NewService(st, client, codex.Endpoints{Usage: "https://usage.test/usage"})
	svc := quota.New(st, codexSvc, client)
	g := gateway.NewWithOAuth(st, nil, nil, codexSvc)
	defer g.Client.CloseIdleConnections()
	assertRed := func(want bool) {
		t.Helper()
		rows := g.RuntimeStatus().Rows
		if len(rows) != 1 || rows[0].Red != want {
			t.Fatalf("gateway red=%v rows=%+v", want, rows)
		}
	}
	owner := make(chan quota.Observation, 1)
	go func() { _, o := svc.RefreshSnapshot(context.Background(), c, cred, true); owner <- o }()
	<-entered
	if err := g.ResetStatus(c.ID, cred.ID, ""); err != nil {
		t.Fatal(err)
	}
	svc.Reset(c.ID, cred.ID)
	// A concurrent non-forced reader may wait for the owner, but its cached
	// report must remain display-only even though it started in the new epoch.
	joined := make(chan quota.Observation, 1)
	go func() { _, o := svc.RefreshSnapshot(context.Background(), c, cred, false); joined <- o }()
	close(release)
	for _, o := range []quota.Observation{<-owner, <-joined} {
		if o.Available != nil || !o.CheckedAt.IsZero() {
			t.Fatalf("late response became observation %+v", o)
		}
		g.ObserveQuota(c, cred, o)
	}
	assertRed(false)
	display, cached := svc.RefreshSnapshot(context.Background(), c, cred, false)
	if len(display.Windows) != 1 || display.Windows[0].RemainingPercent == nil || *display.Windows[0].RemainingPercent != 0 || cached.Available != nil || !cached.CheckedAt.IsZero() || calls.Load() != 1 {
		t.Fatalf("cached response display=%+v observation=%+v calls=%d", display, cached, calls.Load())
	}
	g.ObserveQuota(c, cred, cached)
	assertRed(false)
	if o := svc.Cached(c, cred); o.Available != nil {
		t.Fatalf("late cache republished internally %+v", o)
	}
	// A genuine new query remains a routing fact, in both directions.
	_, fresh := svc.RefreshSnapshot(context.Background(), c, cred, true)
	if fresh.Available == nil || *fresh.Available || fresh.CheckedAt.IsZero() || calls.Load() != 2 {
		t.Fatalf("new exhausted query %+v calls=%d", fresh, calls.Load())
	}
	g.ObserveQuota(c, cred, fresh)
	assertRed(true)
	available.Store(true)
	_, fresh = svc.RefreshSnapshot(context.Background(), c, cred, true)
	if fresh.Available == nil || !*fresh.Available || calls.Load() != 3 {
		t.Fatalf("new recovery query %+v calls=%d", fresh, calls.Load())
	}
	g.ObserveQuota(c, cred, fresh)
	assertRed(false)
	// The gateway's original Check path keeps the fresh positive fact without
	// an extra query; the change only excludes reused display reports.
	checked := svc.Check(context.Background(), c, cred, false)
	if checked.Available == nil || !*checked.Available || calls.Load() != 3 {
		t.Fatalf("routing check %+v calls=%d", checked, calls.Load())
	}
}
