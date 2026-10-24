package accessguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGuardChecksEveryRedirectWithoutMutatingClient(t *testing.T) {
	var hits atomic.Int32
	var allowed atomic.Bool
	allowed.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			allowed.Store(false)
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	client := &http.Client{}
	ctx := With(context.Background(), func() error {
		if !allowed.Load() {
			return errors.New("authorization revoked")
		}
		return nil
	})
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/start", nil)
	response, err := Do(client, r)
	if response != nil {
		response.Body.Close()
	}
	if !Denied(err) || hits.Load() != 1 {
		t.Fatalf("redirect passed revoked authorization: hits=%d err=%v", hits.Load(), err)
	}
	if client.Transport != nil || client.CheckRedirect != nil {
		t.Fatal("shared client mutated")
	}
	unguarded, _ := http.NewRequest("GET", server.URL+"/second", nil)
	response, err = Do(client, unguarded)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if hits.Load() != 2 {
		t.Fatal("unguarded behavior changed")
	}
}

func TestNilGuardAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Check(ctx); err != nil {
		t.Fatal("a missing gate changed existing caller behavior")
	}
	guarded := With(ctx, func() error { return nil })
	if err := Check(guarded); Denied(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost classification: %v", err)
	}
}
