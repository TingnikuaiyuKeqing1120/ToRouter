package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"torouter/internal/health"
	"torouter/internal/store"
)

func TestCredentialManagerDispatchModes(t *testing.T) {
	c := store.Channel{ID: 1, Credentials: []store.CredentialItem{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}, {ID: "c", Enabled: true}}}
	for _, chain := range []string{store.ChainTypeUnidirectional, store.ChainTypeCyclic} {
		for _, mode := range []string{store.DispatchModeFailover, store.DispatchModeRoundRobin} {
			cm := NewCredentialManager()
			c.ChainType = chain
			c.DispatchMode = mode
			k := health.Key{ChannelID: 1, Model: "m"}
			if got := cm.Order(c, k, "")[0].ID; got != "a" {
				t.Fatal(got)
			}
			k.CredentialID = "b"
			cm.Selected(k)
			want := "a"
			if mode == store.DispatchModeRoundRobin {
				want = "c"
			} else if chain == store.ChainTypeCyclic {
				want = "b"
			}
			if got := cm.Order(c, k, "")[0].ID; got != want {
				t.Fatalf("%s/%s got %s want %s", chain, mode, got, want)
			}
			if got := cm.Order(c, k, "b"); len(got) != 2 || got[0].ID != "c" || got[1].ID != "a" {
				t.Fatalf("failover order=%v", got)
			}
		}
	}
}

func TestGatewayIntraChannelFailoverOn429(t *testing.T) {
	var key1Hits atomic.Int64
	var key2Hits atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "Bearer sk-key-1" {
			key1Hits.Add(1)
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"Rate limit exceeded"}}`))
			return
		}
		if auth == "Bearer sk-key-2" {
			key2Hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[{"message":{"content":"success from key 2"}}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch := &store.Channel{
		Name:         "multi-key-channel",
		Enabled:      true,
		DispatchMode: store.DispatchModeFailover,
		ChainType:    store.ChainTypeUnidirectional,
		Endpoints:    []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: ts.URL}},
		Credentials: []store.CredentialItem{
			{ID: "c1", Label: "Primary Key", APIKey: "sk-key-1", Enabled: true},
			{ID: "c2", Label: "Backup Key", APIKey: "sk-key-2", Enabled: true},
		},
		Models: []string{"gpt-test"},
	}
	if err := s.SaveChannel(ch); err != nil {
		t.Fatal(err)
	}

	dm := store.DownstreamModel{
		ID:       "gpt-test",
		Enabled:  true,
		Fallback: true,
		Bindings: []store.Binding{{ChannelID: ch.ID, Model: "gpt-test", Enabled: true}},
	}
	if err := s.SaveDownstreamModel(dm); err != nil {
		t.Fatal(err)
	}

	gw := New(s)
	reqBody := `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	serveTestClient(gw, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after intra-channel failover, got %d: %s", rec.Code, rec.Body.String())
	}
	respBody, _ := io.ReadAll(rec.Body)
	if !bytes.Contains(respBody, []byte("success from key 2")) {
		t.Fatalf("expected response from key 2, got: %s", string(respBody))
	}

	if key1Hits.Load() != 1 {
		t.Fatalf("expected 1 hit on key 1, got %d", key1Hits.Load())
	}
	if key2Hits.Load() != 1 {
		t.Fatalf("expected 1 hit on key 2, got %d", key2Hits.Load())
	}

	// Subsequent request while key 1 is in 60s cooldown should go directly to key 2!
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req2.Header.Set("Content-Type", "application/json")

	serveTestClient(gw, rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected second request 200 OK, got %d", rec2.Code)
	}
	// Key 1 should still be at 1 hit (skipped due to cooldown)
	if key1Hits.Load() != 1 {
		t.Fatalf("expected key 1 to be skipped while cooling down, got %d hits", key1Hits.Load())
	}
	if key2Hits.Load() != 2 {
		t.Fatalf("expected key 2 to receive the second request, got %d hits", key2Hits.Load())
	}
}
