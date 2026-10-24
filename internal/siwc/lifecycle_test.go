package siwc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"torouter/internal/store"
)

// Exercise the token endpoint with a real HTTP request held behind a barrier;
// a fake issuer returning 404 never reaches the rotation race being tested.
func testReauthorizationWithInFlightRefresh(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name := "late_success"
		if rejected {
			name = "late_rejection"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeSIWC(t)
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseRefresh := func() { once.Do(func() { close(release) }) }
			rotator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" {
					t.Error("refresh endpoint did not receive a refresh grant")
					w.WriteHeader(400)
					return
				}
				close(entered)
				<-release
				if rejected {
					w.WriteHeader(400)
					json.NewEncoder(w).Encode(TokenResponse{Error: "invalid_grant"})
					return
				}
				json.NewEncoder(w).Encode(TokenResponse{AccessToken: "old-operation-access", RefreshToken: "old-operation-refresh", ExpiresIn: 3600, Scope: Scope})
			}))
			defer rotator.Close()
			defer releaseRefresh()
			eps := testEndpoints(f.Server)
			eps.Token = rotator.URL
			svc := NewService(s, testClient(), eps)
			host, err := s.HostID()
			if err != nil {
				t.Fatal(err)
			}
			c := siwcChannel(t, s, "oaiapp_serial", host, "user-subject-1")
			auth := mustAuthorize(t, eps, "oaiapp_serial", host, "http://127.0.0.1:1/auth/callback", IDTokenHint{})
			in := f.loginInput(t, auth, TokenResponse{AccessToken: "reauth-access", RefreshToken: "reauth-refresh", ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_serial"}, "oaiapp_serial")
			in.ExpectedSubject = "user-subject-1"
			refreshDone := make(chan error, 1)
			go func() { _, err := svc.Bearer(context.Background(), c); refreshDone <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("refresh never reached the token endpoint")
			}
			loginDone := make(chan error, 1)
			go func() { _, err := svc.CompleteLogin(context.Background(), in); loginDone <- err }()
			var loginErr error
			finished := false
			select {
			case loginErr = <-loginDone:
				finished = true
			case <-time.After(300 * time.Millisecond):
			}
			releaseRefresh()
			if err := <-refreshDone; rejected && !errors.Is(err, ErrReauthRequired) || !rejected && err != nil {
				t.Fatalf("refresh result: %v", err)
			}
			if !finished {
				loginErr = <-loginDone
			}
			if loginErr != nil {
				t.Fatalf("authorization failed: %v", loginErr)
			}
			got, err := s.Channel(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.OAuth.RefreshToken != "reauth-refresh" || got.OAuth.AccessToken != "reauth-access" || got.OAuth.Status != store.OAuthStatusActive || got.OAuth.StatusReason != "" {
				t.Fatal("late old refresh displaced or invalidated the reauthorized credential")
			}
			if got.OAuth.Subject != c.OAuth.Subject || got.OAuth.ClientID != c.OAuth.ClientID || got.OAuth.HostID != c.OAuth.HostID {
				t.Fatal("reauthorization changed account ownership")
			}
			bearer, err := svc.Bearer(context.Background(), got)
			if err != nil || bearer != "reauth-access" {
				t.Fatalf("reauthorized Bearer unusable: %v", err)
			}
		})
	}
}

func TestReauthorizationWaitsForInFlightDisconnect(t *testing.T) {
	f := newFakeSIWC(t)
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseRevoke := func() { once.Do(func() { close(release) }) }
	revoker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("token") != "rt-stored" {
			t.Error("revoke did not use current credential")
			w.WriteHeader(400)
			return
		}
		close(entered)
		<-release
		w.WriteHeader(200)
	}))
	defer revoker.Close()
	defer releaseRevoke()
	eps := testEndpoints(f.Server)
	eps.Revoke = revoker.URL
	svc := NewService(s, testClient(), eps)
	host, err := s.HostID()
	if err != nil {
		t.Fatal(err)
	}
	c := siwcChannel(t, s, "oaiapp_delete", host, "user-subject-1")
	auth := mustAuthorize(t, eps, "oaiapp_delete", host, "http://127.0.0.1:1/auth/callback", IDTokenHint{})
	in := f.loginInput(t, auth, TokenResponse{AccessToken: "reauth-access", RefreshToken: "reauth-refresh", ExpiresIn: 3600, Scope: Scope, ClientID: "oaiapp_delete"}, "oaiapp_delete")
	in.ExpectedSubject, in.ExpectedChannelID = "user-subject-1", c.ID
	disconnectDone := make(chan error, 1)
	go func() { disconnectDone <- svc.DisconnectChannel(context.Background(), c.ID) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect never reached revoke endpoint")
	}
	loginDone := make(chan error, 1)
	go func() { _, err := svc.CompleteLogin(context.Background(), in); loginDone <- err }()
	var loginErr error
	finished := false
	select {
	case loginErr = <-loginDone:
		finished = true
	case <-time.After(300 * time.Millisecond):
	}
	releaseRevoke()
	if err := <-disconnectDone; err != nil {
		t.Fatalf("disconnect failed: %v", err)
	}
	if !finished {
		loginErr = <-loginDone
	}
	if !errors.Is(loginErr, store.ErrNotFound) {
		t.Fatalf("reauthorization completed against a deleted channel: %v", loginErr)
	}
	if rows, err := s.ListChannels(); err != nil || len(rows) != 0 {
		t.Fatal("reauthorization recreated the deleted channel")
	}
}
