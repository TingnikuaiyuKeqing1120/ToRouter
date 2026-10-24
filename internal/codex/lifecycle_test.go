package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestReauthorizationPreservesCurrentAccount(t *testing.T) {
	for _, mismatch := range []string{"account", "unknown_account", "client", "missing_refresh"} {
		t.Run(mismatch, func(t *testing.T) {
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			svc := NewService(s, testClient(), DefaultEndpoints())
			c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
			before, err := s.Channel(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			in := LoginInput{Account: c.OAuth.Account, Tokens: TokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}}
			switch mismatch {
			case "account":
				in.Account = "another-account"
			case "unknown_account":
				in.Account = ""
			case "client":
				in.Tokens.ClientID = "another-client"
			case "missing_refresh":
				in.Tokens.RefreshToken = ""
			}
			if err := svc.ReauthorizeChannel(c.ID, in); err == nil {
				t.Fatal("reauthorization accepted a different or unproven account/client or incomplete credential")
			}
			got, err := s.Channel(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, before) {
				t.Fatal("refused authorization altered the stored credential")
			}
		})
	}
}

func TestDisconnectWaitsForInFlightRefresh(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseRefresh := func() { once.Do(func() { close(release) }) }
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "rt-stored" {
			t.Error("refresh did not use stored credential")
			w.WriteHeader(400)
			return
		}
		close(entered)
		<-release
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", ExpiresIn: 3600})
	}))
	defer issuer.Close()
	defer releaseRefresh()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := NewService(s, testClient(), testEndpoints(issuer))
	c := codexChannel(t, s, "https://chatgpt.com/backend-api/codex")
	refreshDone := make(chan error, 1)
	go func() { _, err := svc.Bearer(context.Background(), c); refreshDone <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh never reached token endpoint")
	}
	disconnectDone := make(chan error, 1)
	go func() { disconnectDone <- svc.DisconnectChannel(c.ID) }()
	var deleteErr error
	finished := false
	select {
	case deleteErr = <-disconnectDone:
		finished = true
	case <-time.After(100 * time.Millisecond):
	}
	releaseRefresh()
	if err := <-refreshDone; err != nil {
		t.Fatalf("in-flight rotation lost its stored result: %v", err)
	}
	if !finished {
		deleteErr = <-disconnectDone
	}
	if deleteErr != nil {
		t.Fatalf("disconnect failed: %v", deleteErr)
	}
	if _, err := s.Channel(c.ID); err != store.ErrNotFound {
		t.Fatalf("deleted channel survived: %v", err)
	}
}
