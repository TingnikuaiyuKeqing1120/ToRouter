package copilot

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"torouter/internal/store"
)

func newService(t *testing.T, f *fakeGitHub) (*Service, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return NewService(s, testClient(), f.endpoints()), s
}

func expiredCopilotChannel(t *testing.T, s *store.Store) store.Channel {
	t.Helper()
	c := store.Channel{Name: "copilot", CredentialType: store.CredentialCopilotOAuth, Enabled: true,
		Models:    []string{"gpt-5"},
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.githubcopilot.com"}},
		OAuth: &store.OAuthCredential{
			Provider:     store.OAuthProviderCopilot,
			GitHubToken:  "gho_authorized",
			AccessToken:  "stale-bearer",
			ExpiresAt:    time.Now().Add(-time.Minute).UTC(),
			AccountLabel: "octocat",
			Status:       store.OAuthStatusActive,
			APIBase:      "https://api.githubcopilot.com",
		}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// G017: a rotation is single-use, and the gateway forwards many requests at
// once, so concurrent refreshes must funnel through one exchange. Everything
// after the first waits on the channel's lock, re-reads the row and finds the
// fresh token; the credential must not be exchanged twice, and no goroutine may
// end up holding a token the store does not have.
func TestConcurrentRefreshExchangesOnce(t *testing.T) {
	f := newFakeGitHub(t)
	svc, s := newService(t, f)
	c := expiredCopilotChannel(t, s)
	const goroutines = 8
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = svc.Bearer(context.Background(), c)
		}(i)
	}
	close(start)
	wg.Wait()
	if got := f.exchanges.Load(); got != 1 {
		t.Fatalf("exchanged %d times for one rotation", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if tokens[i] != "copilot-jwt" {
			t.Fatalf("goroutine %d got %q", i, tokens[i])
		}
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.AccessToken != "copilot-jwt" {
		t.Fatalf("stored token %q", stored.OAuth.AccessToken)
	}
	if stored.OAuth.GitHubToken != "gho_authorized" || stored.Generation != c.Generation {
		t.Fatal("rotation damaged the credential or the generation")
	}
	// The row is fresh now, so a later request must not exchange again.
	if _, err = svc.Bearer(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := f.exchanges.Load(); got != 1 {
		t.Fatalf("exchanged again for a fresh token: %d", got)
	}
}

// Two channels refresh independently: the lock is per credential, not one
// global gate that would serialize the whole gateway.
func TestRefreshLocksArePerChannel(t *testing.T) {
	f := newFakeGitHub(t)
	svc, s := newService(t, f)
	first := expiredCopilotChannel(t, s)
	second := store.Channel{Name: "copilot-two", CredentialType: store.CredentialCopilotOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: "https://api.githubcopilot.com"}},
		OAuth: &store.OAuthCredential{
			Provider: store.OAuthProviderCopilot, GitHubToken: "gho_authorized", AccessToken: "stale-two",
			ExpiresAt: time.Now().Add(-time.Minute).UTC(), Status: store.OAuthStatusActive,
		}}
	if err := s.SaveChannel(&second); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, c := range []store.Channel{first, second} {
		wg.Add(1)
		go func(c store.Channel) {
			defer wg.Done()
			if _, err := svc.Bearer(context.Background(), c); err != nil {
				t.Error(err)
			}
		}(c)
	}
	wg.Wait()
	if got := f.exchanges.Load(); got != 2 {
		t.Fatalf("exchanged %d times for two rotations", got)
	}
}

// A channel whose credential GitHub already refused must fail the request with
// the reason, and never exchange again.
func TestReauthRequiredStatusFailsBeforeExchange(t *testing.T) {
	f := newFakeGitHub(t)
	svc, s := newService(t, f)
	c := expiredCopilotChannel(t, s)
	if err := s.SetOAuthStatus(c.ID, store.OAuthStatusReauthRequired, "github token revoked", false); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Bearer(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "re-run the Copilot authorization") {
		t.Fatalf("err %v", err)
	}
	if f.exchanges.Load() != 0 {
		t.Fatal("exchanged with a rejected token")
	}
}

// 403 during a refresh: the account has no subscription, so the credential is
// marked and the member is disabled; independent members remain available.
func TestUnsubscribedExchangeDisablesOnlyMember(t *testing.T) {
	f := newFakeGitHub(t)
	f.exchange = 403
	svc, s := newService(t, f)
	c := expiredCopilotChannel(t, s)
	other := store.CredentialItem{ID: "second", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCopilot, GitHubToken: "synthetic-other-github", AccessToken: "synthetic-other-access", ExpiresAt: time.Now().Add(time.Hour)}}
	c.Credentials = append(c.Credentials, other)
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Bearer(context.Background(), c)
	if !errors.Is(err, ErrUnsubscribed) {
		t.Fatalf("err %v", err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Enabled || stored.Credentials[0].Enabled || !stored.Credentials[1].Enabled {
		t.Fatal("subscription failure changed another member or channel choice")
	}
	if stored.OAuth.Status != store.OAuthStatusUnsubscribed || stored.OAuth.StatusReason == "" {
		t.Fatalf("status not recorded: %+v", stored.OAuth)
	}
	if stored.OAuth.AccessToken != "stale-bearer" {
		t.Fatal("failed exchange replaced the token")
	}
}

// 401 during a refresh: the GitHub token is dead. The credential is marked for
// re-authorization; the enabled flag is the operator's to change.
func TestRejectedGitHubTokenMarksReauth(t *testing.T) {
	f := newFakeGitHub(t)
	f.exchange = 401
	svc, s := newService(t, f)
	c := expiredCopilotChannel(t, s)
	_, err := svc.Bearer(context.Background(), c)
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err %v", err)
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusReauthRequired || !stored.Enabled {
		t.Fatalf("status not recorded: %+v enabled=%v", stored.OAuth, stored.Enabled)
	}
	// The next request fails on the mark instead of hammering GitHub.
	if _, err = svc.Bearer(context.Background(), c); err == nil || !strings.Contains(err.Error(), "re-run") {
		t.Fatalf("err %v", err)
	}
	if f.exchanges.Load() != 1 {
		t.Fatalf("exchanged %d times", f.exchanges.Load())
	}
}

// An exchange that fails on the wire (5xx, refused connection) leaves the
// credential alone: the failure is this request's, not the account's, so a
// later attempt may still succeed.
func TestWireFailureLeavesCredentialUsable(t *testing.T) {
	f := newFakeGitHub(t)
	svc, s := newService(t, f)
	c := expiredCopilotChannel(t, s)
	f.Close()
	_, err := svc.Bearer(context.Background(), c)
	if err == nil {
		t.Fatal("exchange through a closed server succeeded")
	}
	stored, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuth.Status != store.OAuthStatusActive || stored.OAuth.AccessToken != "stale-bearer" || !stored.Enabled {
		t.Fatalf("wire failure damaged the credential: %+v enabled=%v", stored.OAuth, stored.Enabled)
	}
}

// A fresh token is used as-is; the 60-second skew is what decides "fresh", so a
// token one minute from expiry is exchanged early.
func TestRefreshHappensSixtySecondsBeforeExpiry(t *testing.T) {
	f := newFakeGitHub(t)
	svc, s := newService(t, f)
	c := expiredCopilotChannel(t, s)
	if err := s.ReplaceCopilotToken(c.ID, "soon", time.Now().Add(5*time.Minute).UTC()); err != nil {
		t.Fatal(err)
	}
	token, err := svc.Bearer(context.Background(), c)
	if err != nil || token != "soon" {
		t.Fatalf("token %q err %v", token, err)
	}
	if f.exchanges.Load() != 0 {
		t.Fatal("exchanged a token with time left")
	}
	if err := s.ReplaceCopilotToken(c.ID, "nearly", time.Now().Add(30*time.Second).UTC()); err != nil {
		t.Fatal(err)
	}
	if token, err = svc.Bearer(context.Background(), c); err != nil || token != "copilot-jwt" {
		t.Fatalf("token %q err %v", token, err)
	}
	if f.exchanges.Load() != 1 {
		t.Fatalf("exchanged %d times", f.exchanges.Load())
	}
}

func TestBearerRejectsNonCopilotChannels(t *testing.T) {
	f := newFakeGitHub(t)
	svc, s := newService(t, f)
	c := store.Channel{Name: "plain", APIKey: "sk-test", Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://example.test/v1"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Bearer(context.Background(), c); err == nil {
		t.Fatal("api_key channel answered with a bearer")
	}
	c2 := store.Channel{Name: "no-cred", CredentialType: store.CredentialCopilotOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.githubcopilot.com"}}}
	if _, err := svc.Bearer(context.Background(), c2); err == nil {
		t.Fatal("channel without a credential answered")
	}
}

// A failed profile call degrades the login instead of rejecting it: the account
// label is empty, the documented default api base is used, and the degraded
// step is named so the caller can say what is missing.
func TestCompleteLoginSurvivesProfileFailure(t *testing.T) {
	f := newFakeGitHub(t)
	f.profile = http.StatusInternalServerError
	svc, _ := newService(t, f)
	login, err := svc.CompleteLogin(context.Background(), "gho_authorized")
	if err != nil {
		t.Fatalf("profile failure rejected the login: %v", err)
	}
	if login.APIBase != svc.Endpoints.ModelAPI {
		t.Fatalf("api base %q, want the documented default %q", login.APIBase, svc.Endpoints.ModelAPI)
	}
	if login.Account != "" {
		t.Fatalf("label %q, want empty", login.Account)
	}
	if len(login.Degraded) != 1 || login.Degraded[0] != "account_profile" {
		t.Fatalf("degraded %v", login.Degraded)
	}
	if login.Bearer.Token != "copilot-jwt" || login.GitHubToken != "gho_authorized" {
		t.Fatalf("login lost its tokens: %+v", login)
	}
}
