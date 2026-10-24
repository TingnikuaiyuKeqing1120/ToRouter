package app

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRestartReadinessDoesNotFollowRedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte(`{"pid":123,"port":456}`)) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := restartClient()
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, redirect.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-To-Router-Restart", "local-test-nonce")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 0 || resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatal("readiness followed a redirect outside the chosen loopback endpoint")
	}
}
