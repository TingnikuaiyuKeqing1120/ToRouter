package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestT136AccessOnlyBearerNeverRefreshes(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	c, _ := store.BuiltinChannel("Access only", store.CredentialCodexOAuth)
	c.Credentials = []store.CredentialItem{{ID: "synthetic-access-only", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: ClientIDValue(), Account: "synthetic-account", AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(10 * time.Second)}}}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, server.Client(), Endpoints{Token: server.URL})
	if got, e := svc.Bearer(context.Background(), c); e != nil || got != "synthetic-access" {
		t.Fatal("valid access-only bearer rejected", e)
	}
	c.Credentials[0].OAuth.ExpiresAt = time.Now().Add(-time.Second)
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Bearer(context.Background(), c); !errors.Is(err, ErrReauthRequired) {
		t.Fatal("expired access-only bearer not rejected", err)
	}
	if sends.Load() != 0 {
		t.Fatal("access-only bearer attempted refresh")
	}
	live, _ := st.Channel(c.ID)
	if live.Credentials[0].OAuth.Status != store.OAuthStatusReauthRequired || !live.Enabled {
		t.Fatal("expiry status or enabled choice lost")
	}
}

func TestT136MoveWaitsForInFlightRefresh(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "synthetic-rotated-access", RefreshToken: "synthetic-rotated-refresh", ExpiresIn: 3600})
	}))
	defer server.Close()
	a, _ := store.BuiltinChannel("Source", store.CredentialCodexOAuth)
	b, _ := store.BuiltinChannel("Target", store.CredentialCodexOAuth)
	a.Credentials = []store.CredentialItem{{ID: "synthetic-movable", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: ClientIDValue(), Account: "synthetic-moving-account", AccessToken: "synthetic-old-access", RefreshToken: "synthetic-old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}}}
	if err = st.SaveChannel(&a); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveChannel(&b); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, server.Client(), Endpoints{Token: server.URL})
	refreshed := make(chan error, 1)
	go func() { _, e := svc.Bearer(context.Background(), a); refreshed <- e }()
	<-entered
	moved := make(chan error, 1)
	go func() {
		_, _, e := st.MoveCredentials(a.ID, b.ID, []string{a.Credentials[0].ID}, a.Generation, b.Generation, nil)
		moved <- e
	}()
	select {
	case e := <-moved:
		close(release)
		t.Fatal("move passed an active refresh", e)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if e := <-refreshed; e != nil {
		t.Fatal(e)
	}
	if e := <-moved; e != nil {
		t.Fatal(e)
	}
	source, _ := st.Channel(a.ID)
	target, _ := st.Channel(b.ID)
	if len(source.Credentials) != 0 || len(target.Credentials) != 1 || target.Credentials[0].OAuth.RefreshToken != "synthetic-rotated-refresh" {
		t.Fatal("move lost freshly rotated credentials")
	}
}

func TestT136AccessOnlyRejectionFencesExactSentCredential(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, _ := store.BuiltinChannel("Unknown expiry", store.CredentialCodexOAuth)
	c.Credentials = []store.CredentialItem{
		{ID: "rejected", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: ClientIDValue(), Subject: "synthetic-user-a", Account: "synthetic-workspace", AccessToken: "synthetic-rejected"}},
		{ID: "other", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: ClientIDValue(), Subject: "synthetic-user-b", Account: "synthetic-workspace", AccessToken: "synthetic-other"}},
		{ID: "renewable", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: ClientIDValue(), Subject: "synthetic-user-c", Account: "synthetic-workspace", AccessToken: "synthetic-renewable", RefreshToken: "synthetic-refresh"}},
	}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, nil, Endpoints{})
	if err = svc.RejectAccessOnly(c, "rejected", "synthetic-wrong-token"); !errors.Is(err, store.ErrEditConflict) {
		t.Fatal("unrelated bearer was quarantined", err)
	}
	if err = svc.RejectAccessOnly(c, "renewable", "synthetic-renewable"); !errors.Is(err, store.ErrEditConflict) {
		t.Fatal("renewable credential was quarantined", err)
	}
	if err = svc.RejectAccessOnly(c, "rejected", "synthetic-rejected"); err != nil {
		t.Fatal(err)
	}
	live, _ := st.Channel(c.ID)
	if live.Credentials[0].OAuth.Status != store.OAuthStatusReauthRequired || live.Credentials[1].OAuth.Status == store.OAuthStatusReauthRequired || !live.Enabled || !live.Credentials[0].Enabled {
		t.Fatal("rejection affected other members or enabled choices")
	}
	selected := live
	selected.OAuth = live.Credentials[0].OAuth
	if _, err = svc.Bearer(context.Background(), selected); err == nil {
		t.Fatal("rejected bearer remained usable")
	}
	// A late 401 for the old token cannot quarantine newer consent.
	live.Credentials[0].OAuth.AccessToken = "synthetic-new-consent"
	live.Credentials[0].OAuth.Status = store.OAuthStatusActive
	if err = st.SaveChannel(&live); err != nil {
		t.Fatal(err)
	}
	if err = svc.RejectAccessOnly(c, "rejected", "synthetic-rejected"); !errors.Is(err, store.ErrEditConflict) {
		t.Fatal("late response quarantined new consent", err)
	}
	live, _ = st.Channel(c.ID)
	if live.Credentials[0].OAuth.Status != store.OAuthStatusActive {
		t.Fatal("late response changed new consent")
	}
	// Membership changes also fence a response even if the same token returns.
	target, _ := store.BuiltinChannel("Moved", store.CredentialCodexOAuth)
	if err = st.SaveChannel(&target); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.MoveCredentials(live.ID, target.ID, []string{"rejected"}, live.Generation, target.Generation, nil); err != nil {
		t.Fatal(err)
	}
	if err = svc.RejectAccessOnly(live, "rejected", "synthetic-new-consent"); !errors.Is(err, store.ErrEditConflict) {
		t.Fatal("late response followed moved member", err)
	}
}
