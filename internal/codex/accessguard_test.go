package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"torouter/internal/accessguard"
)

func TestRevokedManagementPreservesAlreadyIssuedRotationWithoutNextSend(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		allowed.Store(false)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "synthetic-new-access", RefreshToken: "synthetic-new-refresh", ExpiresIn: 3600})
	}))
	defer upstream.Close()
	f := newFakeCodex(t)
	svc, st := newService(t, f)
	svc.Client = upstream.Client()
	svc.Endpoints.Token = upstream.URL
	c := codexChannel(t, st, "https://chatgpt.com/backend-api/codex")
	ctx := accessguard.With(context.Background(), func() error {
		if !allowed.Load() {
			return errors.New("authorization revoked")
		}
		return nil
	})
	token, err := svc.Bearer(ctx, c)
	if err != nil || token != "synthetic-new-access" {
		t.Fatalf("already-issued rotation lost: %q %v", token, err)
	}
	fresh, err := st.Channel(c.ID)
	if err != nil || fresh.OAuth.RefreshToken != "synthetic-new-refresh" {
		t.Fatalf("rotated credential not durable: %+v %v", fresh.OAuth != nil, err)
	}
	if _, err = DiscoverModels(ctx, svc.Client, upstream.URL, token, "acct-1"); !accessguard.Denied(err) || hits.Load() != 1 {
		t.Fatalf("later send after revoke: hits=%d err=%v", hits.Load(), err)
	}
}

func TestRevokedManagementActionIsKnownNotSent(t *testing.T) {
	st := actionTestStore(t)
	c := actionTestChannel(t, st, "guarded")
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(204) }))
	defer upstream.Close()
	svc := NewService(st, upstream.Client(), Endpoints{ResetCredits: upstream.URL + "/credits"})
	ctx := accessguard.With(context.Background(), func() error { return errors.New("authorization revoked") })
	err := svc.ConsumeResetCreditForCredential(ctx, c, "second", testRedeemID)
	requireActionError(t, err, false)
	if hits.Load() != 0 {
		t.Fatal("revoked consuming action reached upstream")
	}
}
