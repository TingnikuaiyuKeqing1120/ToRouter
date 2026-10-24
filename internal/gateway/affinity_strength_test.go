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

// chatServer stands up an OpenAI Chat Completions upstream that always answers,
// counting the hits.
func chatServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(up.Close)
	return up
}

func nativeHistoryServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("history request reached %s", r.URL.Path)
		}
		hits.Add(1)
		completedResponses(w, r)
	}))
	t.Cleanup(up.Close)
	return up
}

func addNativeHistory(t *testing.T, s *store.Store, base string, priority int) {
	t.Helper()
	c := store.Channel{Name: "history-fixture", APIKey: "synthetic", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: base}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, priority)
}

func historyTurn(g *Gateway, session, body string) *httptest.ResponseRecorder {
	w, _ := responsesSessionTurn(g, session, body)
	return w
}

// channelIDByURL finds the channel that declares an endpoint at base, the way
// the tests arrange them: one endpoint per channel.
func channelIDByURL(t *testing.T, s *store.Store, base string) int64 {
	t.Helper()
	channels, err := s.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range channels {
		for _, ep := range c.Endpoints {
			if ep.BaseURL == base {
				return c.ID
			}
		}
	}
	t.Fatalf("no channel declares %s", base)
	return 0
}

// setChannelEnabled takes a channel out of rotation, or puts it back. The
// enable flag is not part of the continuation domain, so the generation — and
// every pin recorded at it — survives the round trip.
func setChannelEnabled(t *testing.T, s *store.Store, base string, enabled bool) int64 {
	t.Helper()
	id := channelIDByURL(t, s, base)
	channels, err := s.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range channels {
		if c.ID != id {
			continue
		}
		c.Enabled = enabled
		if err := s.SaveChannel(&c); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t.Fatalf("channel %d disappeared", id)
	return 0
}

// messagesTurn posts an Anthropic Messages request that names its session in a
// header, the way a client identifies a conversation on that protocol. The
// thinking block is the protocol's own state carrier, so it rides the inbound
// body untouched.
func messagesTurn(g *Gateway, session, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-session-id", session)
	w := httptest.NewRecorder()
	serveTestClient(g, w, req)
	return w
}

// The session key and the carrier fingerprint are two independent pin keys, and
// a request can carry both. When they resolve to different continuation domains
// the gateway refuses instead of guessing which one continues the conversation:
// both keys describe the same conversation, so one of the two pins is stale, and
// which one is not visible from the request.
func TestSessionAndCarrierPinsThatDisagreeAreRefused(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	first := nativeHistoryServer(t, &hitsA)
	second := nativeHistoryServer(t, &hitsB)
	addNativeHistory(t, s, first.URL, 0)
	addNativeHistory(t, s, second.URL+"/v1", 1)
	carrier := `{"type":"compaction","encrypted_content":"carrier-value"}`
	// Turn one: the session carries no state and is pinned on the only
	// priority-0 binding.
	if w := historyTurn(g, "conflict", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("turn one hits = %d/%d", hitsA.Load(), hitsB.Load())
	}
	// The carrier gets its own pin on the other channel: with the first channel
	// out of rotation, the request that carries it is served by the only
	// remaining candidate and records its fingerprint there.
	setChannelEnabled(t, s, first.URL, false)
	if w := invoke(g, "/v1/responses", `{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},`+carrier+`]}`); w.Code != 200 {
		t.Fatalf("carrier turn failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("carrier turn hits = %d/%d", hitsA.Load(), hitsB.Load())
	}
	setChannelEnabled(t, s, first.URL, true)
	// Turn three carries both keys, and they disagree.
	w := historyTurn(g, "conflict", `{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},`+carrier+`]}`)
	if w.Code != 409 {
		t.Fatalf("disagreeing pins must be refused, got %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("a refused request must not reach any upstream: %d/%d", hitsA.Load(), hitsB.Load())
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
	if err.Error.Recovery.Code != "state_affinity_conflict" {
		t.Fatalf("recovery code = %q", err.Error.Recovery.Code)
	}
	joined := strings.Join(err.Error.Recovery.Actions, ",")
	for _, want := range []string{"handoff", "start_fresh_session"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing action %s in %s", want, joined)
		}
	}
	// The two pins name continuation domains, which can differ in the model, the
	// endpoint format or the generation alone — "different channels" would claim
	// a dimension the request does not distinguish. And neither pin heals by
	// waiting: the conversation keeps both domains until the client sorts it
	// out, so a retry_later would only send it back into the same conflict.
	if strings.Contains(joined, "retry_later") {
		t.Fatalf("a conflict that waiting cannot resolve must not advise retry_later: %s", joined)
	}
	if !strings.Contains(err.Error.Message, "continuation domain") {
		t.Fatalf("the refusal must name the domain conflict, not a channel mismatch: %q", err.Error.Message)
	}
}

// The wording matters most where the channel never differed: a configuration
// edit bumps the channel's generation, and the two keys hold a pin recorded on
// either side of it. Same channel, same endpoint, same model — the conflict is
// the generation alone, so a refusal naming "different channels" would describe
// a move that never happened.
func TestPinConflictCanDifferInGenerationAlone(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hits atomic.Int32
	up := nativeHistoryServer(t, &hits)
	c := store.Channel{Name: "rotating", APIKey: "key-one", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, 0)
	carrier := `{"type":"compaction","encrypted_content":"carrier-value"}`
	// Turn one: the session is pinned at the channel's first generation.
	if w := historyTurn(g, "gen-conflict", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	// The operator rotates the credential. The id stays; the generation moves,
	// and the session pin still names the domain the conversation started on.
	c.APIKey = "key-two"
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 2 {
		t.Fatalf("rotating a credential must bump the generation, got %d", c.Generation)
	}
	// A request that names no session carries the carrier and is served by the
	// channel's current domain, so its fingerprint is pinned there.
	if w := invoke(g, "/v1/responses", `{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},`+carrier+`]}`); w.Code != 200 {
		t.Fatalf("carrier turn failed: %d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("both turns belong to the same channel: hits %d", hits.Load())
	}
	// Turn three carries both keys. They agree on the channel, the endpoint and
	// the model, and disagree on the generation alone — still a conflict.
	w := historyTurn(g, "gen-conflict", `{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},`+carrier+`]}`)
	if w.Code != 409 {
		t.Fatalf("pins that differ in generation alone must be refused, got %d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("a refused request must not reach any upstream: hits %d", hits.Load())
	}
	var err struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &err); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(err.Error.Message, "continuation domain") {
		t.Fatalf("the refusal must name the domain that moved, not a channel that never left: %q", err.Error.Message)
	}
}

// A soft pin has no irreplaceable history: the pinned domain leads and the
// remaining candidates follow, so it wins even against a higher-priority channel.
func TestSoftPinPrefersPinnedDomain(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	add(t, s, chatServer(t, &hitsA).URL, "openai", 0)
	// Turn one: A is the only candidate, so the session is pinned there.
	if w := sessionTurn(g, "soft-lead", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("turn one hits = %d/%d", hitsA.Load(), hitsB.Load())
	}
	// B would win an unpinned pick from now on.
	add(t, s, chatServer(t, &hitsB).URL, "openai", -1)
	if w := sessionTurn(g, "soft-lead", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn two failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 2 || hitsB.Load() != 0 {
		t.Fatalf("the pinned domain must lead, not the higher-priority channel: %d/%d", hitsA.Load(), hitsB.Load())
	}
}

// A soft pin that cannot be followed is not a wall: when the pinned channel is
// cooling down the remaining candidates still get their turn, and the success
// path re-pins the session onto the domain that answered, so the conversation
// moves as a whole instead of splitting.
func TestSoftPinFallsBackAndRepinsWhenPinnedChannelIsCooling(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	first := chatServer(t, &hitsA)
	add(t, s, first.URL, "openai", 0)
	if w := sessionTurn(g, "soft-cooldown", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 {
		t.Fatalf("turn one hits = %d", hitsA.Load())
	}
	aID := channelIDByURL(t, s, first.URL)
	recoveryLease, _ := g.Breaker.Acquire(testCombination(t, g, aID))
	add(t, s, chatServer(t, &hitsB).URL, "openai", 1)
	// The pinned channel's breaker is open: the pinned domain is a candidate on
	// paper only.
	for i := 0; i < 1; i++ {
		lease, _ := g.Breaker.Acquire(testCombination(t, g, aID))
		g.Breaker.Failure(lease)
	}
	if w := sessionTurn(g, "soft-cooldown", `{"model":"test","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"previous","reasoning_content":"carried-thinking"}]}`); w.Code != 200 {
		t.Fatalf("turn two must fall back: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("turn two hits = %d/%d", hitsA.Load(), hitsB.Load())
	}
	// The pinned channel is healthy again, and the session now follows the
	// domain that answered: the old one is never revisited.
	g.Breaker.Success(recoveryLease)
	if w := sessionTurn(g, "soft-cooldown", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn three failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 2 {
		t.Fatalf("the pin must have moved with the fallback: %d/%d", hitsA.Load(), hitsB.Load())
	}
}

// The other way a soft pin stops guiding: the pinned domain is reachable but
// keeps answering 503, and once its retries are spent the next candidate takes
// over — and becomes the pin.
func TestSoftPinFallsBackWhenRetryableErrorsAreExhausted(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 2)
	var calls atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() == 1 {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer a.Close()
	var hitsB atomic.Int32
	add(t, s, a.URL, "openai", 0)
	add(t, s, chatServer(t, &hitsB).URL, "openai", 1)
	if w := sessionTurn(g, "soft-retries", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("turn one hits = %d/%d", calls.Load(), hitsB.Load())
	}
	if w := sessionTurn(g, "soft-retries", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn two must fall back after the retries are spent: %d %s", w.Code, w.Body.String())
	}
	// One same-channel retry, then the failover: the pinned channel was asked
	// twice, the spare once.
	if calls.Load() != 3 || hitsB.Load() != 1 {
		t.Fatalf("turn two hits = %d/%d", calls.Load(), hitsB.Load())
	}
	if w := sessionTurn(g, "soft-retries", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn three failed: %d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 3 || hitsB.Load() != 2 {
		t.Fatalf("the pin must have moved with the fallback: %d/%d", calls.Load(), hitsB.Load())
	}
}

// The last way a soft pin stops guiding: the endpoint world it names is gone.
// The operator drops the chat endpoint the session was pinned on — the
// declaration set moves and the generation moves with it. The old domain is
// no longer among the candidates. A stateless turn is
// portable: it can use the remaining Responses endpoint by conversion, and
// success re-pins the session onto that new endpoint domain.
func TestSoftPinRoutesUnpinnedAndRepinsWhenPinnedEndpointIsGone(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB, hitsC atomic.Int32
	var responsesHits atomic.Int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		if strings.HasSuffix(r.URL.Path, "/responses") {
			responsesHits.Add(1)
			io.WriteString(w, `{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upA.Close()
	// The remaining Responses endpoint can serve stateless Chat by conversion.
	pinned := store.Channel{Name: "chat-then-responses", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: upA.URL}, {Format: store.FormatOpenAIResponses, BaseURL: upA.URL}}}
	if err := s.SaveChannel(&pinned); err != nil {
		t.Fatal(err)
	}
	bind(t, s, pinned.ID, 0)
	add(t, s, chatServer(t, &hitsB).URL, "openai", 1)
	// Turn one: the channel is the only priority-0 candidate, so the session is
	// pinned on its chat endpoint.
	if w := sessionTurn(g, "soft-gone", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("turn one hits = %d/%d", hitsA.Load(), hitsB.Load())
	}
	// The operator drops the chat endpoint. The declaration set moved, so the
	// channel keeps its id and bumps its generation: the domain the pin names is
	// gone from the candidates by the endpoint and by the generation alike.
	pinned.Endpoints = []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: upA.URL}}
	if err := s.SaveChannel(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned.Generation != 2 {
		t.Fatalf("dropping an endpoint must bump the generation, got %d", pinned.Generation)
	}
	// Turn two: no candidate matches the pinned domain, so the order stands and
	// the request routes unpinned onto A's remaining convertible endpoint.
	if w := sessionTurn(g, "soft-gone", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn two must route as unpinned: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 2 || hitsB.Load() != 0 || responsesHits.Load() != 1 {
		t.Fatalf("turn two must use the remaining Responses endpoint: a=%d b=%d responses=%d", hitsA.Load(), hitsB.Load(), responsesHits.Load())
	}
	// A channel routing would now prefer joins, so only the migrated pin keeps
	// turn three on the domain that answered turn two.
	add(t, s, chatServer(t, &hitsC).URL, "openai", -1)
	if w := sessionTurn(g, "soft-gone", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn three failed: %d %s", w.Code, w.Body.String())
	}
	if hitsB.Load() != 0 || hitsC.Load() != 0 || hitsA.Load() != 3 || responsesHits.Load() != 2 {
		t.Fatalf("the pin must have moved with the fallback: a=%d b=%d c=%d", hitsA.Load(), hitsB.Load(), hitsC.Load())
	}
}

// A soft pin falls back on failures that belong to the channel — it cooling
// down, its retryable errors spent. It does not fall back on one the upstream
// may already have executed: a 500 stops the request wherever it lands, pinned
// or not, because resending it on another channel would spend the inference
// twice (see status_grading_test.go).
func TestSoftPinDoesNotFallOverAnAmbiguousServerError(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	var calls atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hitsA.Add(1)
		if calls.Load() == 1 {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	add(t, s, failing.URL, "openai", 0)
	if w := sessionTurn(g, "soft-ambiguous", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 {
		t.Fatalf("turn one hits = %d", hitsA.Load())
	}
	add(t, s, chatServer(t, &hitsB).URL, "openai", 1)
	w := sessionTurn(g, "soft-ambiguous", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 502 {
		t.Fatalf("an ambiguous 5xx must stop the request, got %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 2 || hitsB.Load() != 0 {
		t.Fatalf("the spare channel must stay untouched: %d/%d", hitsA.Load(), hitsB.Load())
	}
	if code, _ := recoveryOf(t, w.Body.Bytes()); code != "upstream_may_have_processed" {
		t.Fatalf("recovery code = %q", code)
	}
}

// A hard pin has state to protect: the candidates collapse to the pinned domain
// and no other channel is tried, whatever the domain costs. The request below
// depends on previous_response_id, so only its original account and endpoint
// can reconstruct that history.
func TestHardPinDoesNotFailOverToAnotherChannel(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	first := nativeHistoryServer(t, &hitsA)
	second := nativeHistoryServer(t, &hitsB)
	addNativeHistory(t, s, first.URL, 0)
	addNativeHistory(t, s, second.URL, 1)
	// Turn one carries no state, so the pin is soft: A is the only priority-0
	// channel and serves it.
	if w := historyTurn(g, "hard", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("turn one hits = %d/%d", hitsA.Load(), hitsB.Load())
	}
	// The pinned channel leaves. Turn two brings the state it produced.
	setChannelEnabled(t, s, first.URL, false)
	w := historyTurn(g, "hard", `{"model":"test","previous_response_id":"resp_native","input":"continue"}`)
	if w.Code != 503 {
		t.Fatalf("a hard pin must not fail over, got %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("the spare channel must stay untouched: %d/%d", hitsA.Load(), hitsB.Load())
	}
	if code, _ := recoveryOf(t, w.Body.Bytes()); code != "state_affinity_unavailable" {
		t.Fatalf("recovery code = %q", code)
	}
}

// The pinned domain is among the candidates but not reachable right now: either
// the channel's breaker is open, or the domain's retries are spent. That is a
// health signal about the channel, so the client is told the conversation cannot
// continue where it lives — 503 with the pinned-unavailable recovery and its
// retry_later — rather than being handed the upstream's own status.
func TestHardPinUnavailableAfterFailureReportsStateAffinity(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	first := nativeHistoryServer(t, &hitsA)
	addNativeHistory(t, s, first.URL, 0)
	addNativeHistory(t, s, nativeHistoryServer(t, &hitsB).URL+"/v1", 1)
	if w := historyTurn(g, "hard-cooling", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	aID := channelIDByURL(t, s, first.URL)
	for i := 0; i < 1; i++ {
		lease, _ := g.Breaker.Acquire(testCombination(t, g, aID))
		g.Breaker.Failure(lease)
	}
	w := historyTurn(g, "hard-cooling",
		`{"model":"test","previous_response_id":"resp_native","input":"continue"}`)
	if w.Code != 503 {
		t.Fatalf("want 503, got %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("a cooling pinned channel must not reach an upstream: %d/%d", hitsA.Load(), hitsB.Load())
	}
	code, actions := recoveryOf(t, w.Body.Bytes())
	if code != "state_affinity_unavailable" {
		t.Fatalf("recovery code = %q", code)
	}
	if joined := strings.Join(actions, ","); !strings.Contains(joined, "retry_later") {
		t.Fatalf("a cooling channel may recover, so retry_later must be advised: %s", joined)
	}
	if !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("message = %s", w.Body.String())
	}
}

// The same recovery when the pinned domain answers and is simply failing: its
// retry budget is spent without a success, and the client still hears that the
// conversation cannot continue where it lives — not the upstream's own 429.
func TestHardPinSpendsItsRetriesThenReportsStateAffinity(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var calls atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() == 1 {
			io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer a.Close()
	var hitsB atomic.Int32
	addNativeHistory(t, s, a.URL, 0)
	addNativeHistory(t, s, nativeHistoryServer(t, &hitsB).URL+"/v1", 1)
	if w := historyTurn(g, "hard-retries", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	w := historyTurn(g, "hard-retries",
		`{"model":"test","previous_response_id":"resp_native","input":"continue"}`)
	if w.Code != 503 {
		t.Fatalf("want 503, got %d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 2 || hitsB.Load() != 0 {
		t.Fatalf("the pinned domain must be spent and nothing else tried: %d/%d", calls.Load(), hitsB.Load())
	}
	if code, _ := recoveryOf(t, w.Body.Bytes()); code != "state_affinity_unavailable" {
		t.Fatalf("recovery code = %q", code)
	}
}

// A hard pin followed by a plain upstream error is that error and nothing else.
// The pin is correct and the fixture returns a plain credential rejection, 401.
// The client still sees that status and its diagnostic, with no affinity
// advice attached: nothing here observed a conflict, and a handoff suggested
// over a credential the operator has to rotate anyway is advice about a problem
// the request does not have.
func TestHardPinPlainUpstreamErrorKeepsItsOwnStatusAndMessage(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("a converted request reached %s", r.URL.Path)
		}
		if hitsA.Add(1) == 1 {
			io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer first.Close()
	addNativeHistory(t, s, first.URL, 0)
	addNativeHistory(t, s, nativeHistoryServer(t, &hitsB).URL, 1)
	// Turn one carries no state, so the pin is soft: A is the only priority-0
	// channel and serves it.
	if w := historyTurn(g, "hard-401", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("turn one hits = %d/%d", hitsA.Load(), hitsB.Load())
	}
	// Turn two brings the state A produced, so the pin is hard and the
	// candidates collapse to A — which answers 401.
	w := historyTurn(g, "hard-401", `{"model":"test","previous_response_id":"resp_native","input":"continue"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("the upstream's own status must travel, got %d %s", w.Code, w.Body.String())
	}
	if hitsA.Load() != 2 || hitsB.Load() != 0 {
		t.Fatalf("a deterministic 4xx must be spent once with no failover: %d/%d", hitsA.Load(), hitsB.Load())
	}
	var err struct {
		Error struct {
			Message    string `json:"message"`
			Diagnostic struct {
				Code string `json:"code"`
			} `json:"diagnostic"`
			Recovery struct {
				Code    string   `json:"code"`
				Actions []string `json:"actions"`
			} `json:"recovery"`
		} `json:"error"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &err); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(err.Error.Message, "upstream returned HTTP 401") || err.Error.Diagnostic.Code != "upstream_rejected" {
		t.Fatalf("status detail and diagnostic must travel: %+v", err.Error)
	}
	if err.Error.Recovery.Code != "upstream_rejected" || len(err.Error.Recovery.Actions) != 0 {
		t.Fatalf("a plain upstream rejection must carry its diagnostic without affinity advice: %+v", err.Error.Recovery)
	}
	// Credential rejection cools only the attempted credential combination.
	if snap := failedCombinations(g); len(snap) != 1 {
		t.Fatalf("credential rejection must cool only its combination: %v", snap)
	}
}
