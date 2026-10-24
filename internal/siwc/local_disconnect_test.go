package siwc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/store"
)

func TestLocalDisconnectWaitsForRefreshWithoutRevoke(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseRefresh := func() { once.Do(func() { close(release) }) }
	var rotations, revokes atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/oauth/token" {
			revokes.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		if rotations.Add(1) != 1 {
			t.Error("local delete interfered with the first durable rotation")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		close(entered)
		<-release
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "fixture-rotated-access", RefreshToken: "fixture-rotated-refresh", ExpiresIn: 3600, Scope: Scope})
	}))
	defer issuer.Close()
	defer releaseRefresh()
	eps := testEndpoints(issuer)
	eps.Revoke = issuer.URL + "/api/accounts/oauth/revoke"
	svc := NewService(st, testClient(), eps)
	c := siwcChannel(t, st, "oaiapp_fixture", "fixture-host", "fixture-subject")
	refreshDone := make(chan error, 1)
	go func() {
		token, err := svc.Bearer(context.Background(), c)
		if err == nil && token != "fixture-rotated-access" {
			err = errors.New("rotation did not return the expected fixture access token")
		}
		refreshDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation did not start")
	}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- svc.LocalDisconnectChannelAtVersion(context.Background(), c.ID, "") }()
	select {
	case err := <-deleteDone:
		t.Fatalf("local deletion did not wait for rotation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := st.Channel(c.ID); err != nil {
		t.Fatalf("channel disappeared while rotation held its lifecycle: %v", err)
	}
	releaseRefresh()
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatalf("rotation failed before local deletion: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rotation did not finish")
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local deletion did not finish")
	}
	if _, err := st.Channel(c.ID); !errors.Is(err, store.ErrNotFound) || rotations.Load() != 1 || revokes.Load() != 0 {
		t.Fatalf("local deletion/revoke boundary: channel=%v rotations=%d revokes=%d", err, rotations.Load(), revokes.Load())
	}
}

func TestLocalDisconnectPreservesPermissionAndVersionChecks(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var requests atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer issuer.Close()
	eps := testEndpoints(issuer)
	eps.Revoke = issuer.URL
	svc := NewService(st, testClient(), eps)
	c := siwcChannel(t, st, "oaiapp_fixture", "fixture-host", "fixture-subject")
	version := store.EntityVersion(c)
	if err := svc.LocalDisconnectChannelAtVersion(context.Background(), c.ID, "fixture-stale-version"); !errors.Is(err, store.ErrEditConflict) {
		t.Fatalf("local deletion ignored version: %v", err)
	}
	denied := accessguard.With(context.Background(), func() error { return errors.New("fixture permission denied") })
	if err := svc.LocalDisconnectChannelAtVersion(denied, c.ID, version); !accessguard.Denied(err) {
		t.Fatalf("local deletion ignored caller permissions: %v", err)
	}
	if _, err := st.Channel(c.ID); err != nil {
		t.Fatalf("failed local deletion changed channel: %v", err)
	}
	if err := svc.LocalDisconnectChannelAtVersion(context.Background(), c.ID, version); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Channel(c.ID); !errors.Is(err, store.ErrNotFound) || requests.Load() != 0 {
		t.Fatalf("local deletion sent network work: channel=%v requests=%d", err, requests.Load())
	}
}
