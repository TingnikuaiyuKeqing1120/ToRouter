package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/store"
)

// T021 follow-up: the pin's fourth dimension is the channel's configuration
// generation. Rotating a credential or moving a base URL keeps the channel id
// but swaps the continuation domain underneath it — the state a session carries
// was produced by the previous account or endpoint — so a pin recorded at the
// old generation is no longer servable and the request is refused instead of
// being sent to the wrong world.

func TestCredentialRotationInvalidatesThePin(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "rotating", APIKey: "key-one", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, 0)
	// Turn one pins the session at the channel's first generation.
	if w := historyTurn(g, "gen-rotate", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("turn one hits = %d", hits.Load())
	}
	// The operator points the same channel at another account. The id is
	// unchanged; the continuation domain is not.
	c.APIKey = "key-two"
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	// Turn two carries state and follows the pin. The pinned generation has no
	// candidate left, so the gateway refuses rather than guessing.
	w := historyTurn(g, "gen-rotate",
		`{"model":"test","previous_response_id":"resp_native","input":"continue"}`)
	if w.Code != 503 {
		t.Fatalf("pinned domain changed, want 503, got %d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("a request whose pinned domain changed must not reach any upstream: hits %d", hits.Load())
	}
	var err struct {
		Error struct {
			Message  string `json:"message"`
			Recovery struct {
				Code    string   `json:"code"`
				Actions []string `json:"actions"`
			} `json:"recovery"`
		} `json:"error"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &err); e != nil {
		t.Fatal(e)
	}
	// The channel kept its id and only its configuration moved on, so the
	// advice is not "retry when it recovers": pointing the channel back bumps
	// the generation again, and polling would 503 forever.
	if err.Error.Recovery.Code != "state_domain_changed" {
		t.Fatalf("recovery = %+v", err.Error.Recovery)
	}
	if !strings.Contains(err.Error.Message, "reconfigured") {
		t.Fatalf("message = %q", err.Error.Message)
	}
	if joined := strings.Join(err.Error.Recovery.Actions, ","); strings.Contains(joined, "retry_later") {
		t.Fatalf("a reconfigured channel must not be polled: %s", joined)
	}
}

// A channel that simply left is the other side of the same refusal: the pinned
// channel is not among the candidates at all, its configuration untouched. It
// may come back exactly as it was, so "retry when it recovers" stays honest.
func TestPinnedChannelGoneStillAdvisesRetryLater(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var gone, spare atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gone.Add(1)
		io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spare.Add(1)
		io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
	}))
	defer second.Close()
	c := store.Channel{Name: "leaving", APIKey: "key-one", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: first.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, 0)
	other := store.Channel{Name: "spare", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: second.URL}}}
	if err := s.SaveChannel(&other); err != nil {
		t.Fatal(err)
	}
	bind(t, s, other.ID, 1)
	// Turn one pins the session on the only priority-0 binding.
	if w := historyTurn(g, "gen-gone", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if gone.Load() != 1 || spare.Load() != 0 {
		t.Fatalf("turn one hits = %d/%d", gone.Load(), spare.Load())
	}
	// The operator takes the channel out of rotation. Nothing about its
	// configuration changed, so the pin it holds is still valid — it just has
	// no candidate right now.
	c.Enabled = false
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	w := historyTurn(g, "gen-gone",
		`{"model":"test","previous_response_id":"resp_native","input":"continue"}`)
	if w.Code != 503 {
		t.Fatalf("pinned channel gone, want 503, got %d %s", w.Code, w.Body.String())
	}
	if gone.Load() != 1 || spare.Load() != 0 {
		t.Fatalf("a request whose pinned channel is gone must not reach any upstream: gone %d spare %d", gone.Load(), spare.Load())
	}
	var err struct {
		Error struct {
			Recovery struct {
				Code    string   `json:"code"`
				Actions []string `json:"actions"`
			} `json:"recovery"`
		} `json:"error"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &err); e != nil {
		t.Fatal(e)
	}
	if err.Error.Recovery.Code != "state_affinity_unavailable" {
		t.Fatalf("recovery = %+v", err.Error.Recovery)
	}
	if joined := strings.Join(err.Error.Recovery.Actions, ","); !strings.Contains(joined, "retry_later") {
		t.Fatalf("a channel that may return unchanged must still advise retrying: %s", joined)
	}
}

// The generation counts configuration-domain changes only: renaming a channel
// leaves the state namespace alone, so the pin keeps working.
func TestRenameDoesNotInvalidateThePin(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "before", APIKey: "key-one", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, 0)
	if w := historyTurn(g, "gen-rename", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	c.Name = "after"
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	w := historyTurn(g, "gen-rename",
		`{"model":"test","previous_response_id":"resp_native","input":"continue"}`)
	if w.Code != 200 {
		t.Fatalf("a rename must not break the pin: %d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("both turns must land on the same channel: hits %d", hits.Load())
	}
}
