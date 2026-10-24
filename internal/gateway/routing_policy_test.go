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

func settings(t *testing.T, s *store.Store, attempts, retries int) {
	t.Helper()
	if err := s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, FailureCooldownSeconds: 1, MaxAttempts: attempts, RetriesPerChannel: retries, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}}); err != nil {
		t.Fatal(err)
	}
}

// T016: a retryable failure is retried on the same channel before any other
// channel is tried, because retrying the channel that owns the state cannot
// lose it.
func TestSameChannelRetriesBeforeFailover(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 3)
	var bad, good atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bad.Add(1); w.WriteHeader(429) }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer second.Close()
	add(t, s, first.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if bad.Load() != 3 || good.Load() != 1 {
		t.Fatalf("same-channel retries then failover expected: bad %d good %d", bad.Load(), good.Load())
	}
	// The throttle answers about the channel's capacity, so the retries that
	// bought the failover count against it: the breaker sees a channel that is
	// answering 429, which is the gateway-level health signal.
	if snap := failedCombinations(g); len(snap) != 1 {
		t.Fatalf("three 429s must count against the throttled channel: %v", snap)
	}
}

// A server-side history reference without a known origin must not be guessed
// between different accounts. Ordinary reasoning is covered separately.
func TestHistoryReferenceWithoutPinIsRefused(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 2)
	var bad, good atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bad.Add(1); w.WriteHeader(429) }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { good.Add(1) }))
	defer second.Close()
	add(t, s, first.URL, "responses", 0)
	add(t, s, second.URL+"/v1", "responses", 1)
	body := `{"model":"test","previous_response_id":"resp_fixture","input":"continue"}`
	w := invoke(g, "/v1/responses", body)
	if w.Code != 409 {
		t.Fatalf("expected refusal, got %d %s", w.Code, w.Body.String())
	}
	if bad.Load() != 0 || good.Load() != 0 {
		t.Fatal("a refused request must not reach any upstream")
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
	if err.Error.Recovery.Code != "state_affinity_unknown" {
		t.Fatalf("recovery = %+v", err.Error.Recovery)
	}
	joined := strings.Join(err.Error.Recovery.Actions, ",")
	for _, want := range []string{"use_single_binding", "handoff", "start_fresh_session"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing action %s in %s", want, joined)
		}
	}
}

// The same refusal seen from the other side: the pin table lost the
// conversation — a restart, an expired entry — and the request arrives carrying
// state with exactly one domain to hold it. When that domain is throttled the
// upstream's own status and message travel, and no recovery is attached: the
// request happening to carry state is not an affinity problem, nothing here
// observed a conflict, and a stateless request sees the very same 429 with the
// very same silence.
func TestStatefulFailureWithoutPinKeepsUpstreamStatus(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var throttled atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		throttled.Add(1)
		w.WriteHeader(429)
	}))
	defer first.Close()
	add(t, s, first.URL, "openai", 0)
	body := `{"model":"test","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"prev","reasoning_content":"earlier reasoning"}]}`
	w := invoke(g, "/v1/chat/completions", body)
	if w.Code != 429 {
		t.Fatalf("the upstream's own status must travel, got %d %s", w.Code, w.Body.String())
	}
	if throttled.Load() != 1 {
		t.Fatalf("the single domain must be spent once, then the request ends: %d calls", throttled.Load())
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
	if !strings.Contains(err.Error.Message, "upstream returned HTTP 429") {
		t.Fatalf("the upstream status must remain visible, got %q", err.Error.Message)
	}
	if err.Error.Recovery.Code != "upstream_rejected" {
		t.Fatalf("a plain upstream error must not be diagnosed as an affinity conflict: %q", err.Error.Recovery.Code)
	}
	for _, action := range err.Error.Recovery.Actions {
		if action == "handoff" || action == "start_fresh_session" {
			t.Fatalf("no affinity advice belongs on a plain throttle: %v", err.Error.Recovery.Actions)
		}
	}
}

// A client that identifies its session gets pinned on the first successful
// turn, and every later turn follows the pin instead of re-rolling the dice.
func TestSessionPinRoutesConsistently(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var a, b atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"from-a","reasoning_content":"r"}}]}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"from-b"}}]}`)
	}))
	defer second.Close()
	add(t, s, first.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	turn := func(withState bool) (int32, int32) {
		beforeA, beforeB := a.Load(), b.Load()
		msg := `{"model":"test","metadata":{"user_id":"user_hash_session_abc123"},"messages":[{"role":"user","content":"hi"}`
		if withState {
			msg += `,{"role":"assistant","content":"prev","reasoning_content":"earlier reasoning"}`
		}
		msg += `]}`
		w := invoke(g, "/v1/chat/completions", msg)
		if w.Code != 200 {
			t.Fatalf("turn failed: %d %s", w.Code, w.Body.String())
		}
		return a.Load() - beforeA, b.Load() - beforeB
	}
	// First turn: no state, first eligible candidate, pin recorded.
	hitA, hitB := turn(false)
	if hitA+hitB != 1 {
		t.Fatalf("first turn hit %d/%d", hitA, hitB)
	}
	// Later turns carry state and must land on the same channel every time.
	for i := 0; i < 4; i++ {
		hitA, hitB = turn(true)
		if hitA+hitB != 1 {
			t.Fatalf("stateful turn %d hit %d/%d", i, hitA, hitB)
		}
	}
	total := a.Load() + b.Load()
	if total != 5 {
		t.Fatalf("expected 5 upstream calls, got %d", total)
	}
	// The pinned channel served every stateful turn: one of the two counters
	// advanced by 5, the other by 0.
	if a.Load() != 5 && b.Load() != 5 {
		t.Fatalf("turns were split across channels: a=%d b=%d", a.Load(), b.Load())
	}
}

// A pin is a continuation domain, not just a channel: two bindings on the same
// channel with different upstream models are different domains, and a
// conversation must never drift from one to the other.
func TestPinSeparatesModelsOnOneChannel(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "model-x") {
			hitsA.Add(1)
		} else {
			hitsB.Add(1)
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok","reasoning_content":"r"}}]}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "multi", APIKey: "k", Models: []string{"model-x", "model-y"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	// Same channel, two upstream models: two continuation domains.
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{
		{ChannelID: c.ID, Model: "model-x", Enabled: true},
		{ChannelID: c.ID, Model: "model-y", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	turn := func() {
		w := invoke(g, "/v1/chat/completions",
			`{"model":"test","metadata":{"user_id":"user_hash_session_dom1"},"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != 200 {
			t.Fatalf("turn failed: %d %s", w.Code, w.Body.String())
		}
	}
	turn()
	firstA, firstB := hitsA.Load(), hitsB.Load()
	if firstA+firstB != 1 {
		t.Fatalf("first turn hit %d/%d", firstA, firstB)
	}
	for i := 0; i < 4; i++ {
		turn()
	}
	// One model served all five turns; the other was never touched.
	if (hitsA.Load() != 5 || hitsB.Load() != 0) && (hitsB.Load() != 5 || hitsA.Load() != 0) {
		t.Fatalf("conversation drifted between models on one channel: x=%d y=%d", hitsA.Load(), hitsB.Load())
	}
}

// T015: the fallback switch is real. Turning it off keeps same-channel retries
// but never reaches the second binding.
func TestFallbackDisabledTriesOneChannel(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 2)
	var bad, good atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bad.Add(1); w.WriteHeader(503) }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { good.Add(1) }))
	defer second.Close()
	add(t, s, first.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	rows, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	m := rows[0]
	m.Fallback = false
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	if w.Code != 503 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if good.Load() != 0 {
		t.Fatal("fallback was disabled but a second channel was tried")
	}
	if bad.Load() != 2 {
		t.Fatalf("same-channel retries still apply: bad %d", bad.Load())
	}
	var body struct {
		Error struct {
			Recovery struct{ Code string } `json:"recovery"`
		} `json:"error"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	if body.Error.Recovery.Code != "fallback_disabled" {
		t.Fatalf("recovery = %+v", body.Error.Recovery)
	}
}

// T013: cross-protocol tool history aimed at Gemini 3 gets the provider
// documented dummy signature, and the adaptation is recorded. Refusing instead
// is no longer necessary: Google documents this bypass for exactly the case of
// transferring a trace from a model that produced no signatures.
func TestGemini3ToolCallGetsDocumentedBypass(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var seen atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.Store(string(b))
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "g", APIKey: "k", Models: []string{"gemini-3-pro", "gemini-2.5-flash"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatGeminiGenerateContent, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "gemini-3-pro", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"test","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`
	w := invoke(g, "/v1/chat/completions", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	got, _ := seen.Load().(string)
	if !strings.Contains(got, "skip_thought_signature_validator") {
		t.Fatalf("bypass signature missing upstream: %s", got)
	}
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Degraded, "thought_signature_bypass") {
		t.Fatalf("usage did not record the adaptation: %+v", rows)
	}
	// A Gemini 2.x target carries no bypass.
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "gemini-2.5-flash", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if w = invoke(g, "/v1/chat/completions", body); w.Code != 200 {
		t.Fatalf("2.x should route: %d %s", w.Code, w.Body.String())
	}
	got, _ = seen.Load().(string)
	if strings.Contains(got, "skip_thought_signature_validator") {
		t.Fatalf("bypass attached to a 2.x target: %s", got)
	}
}

// The tool identifier carries a bounded, authenticated attachment reference,
// preserving otherwise opaque reasoning for the next turn.
func TestToolCallWithReasoningIsPreserved(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"c","content":[`+
			`{"type":"thinking","thinking":"secret","signature":"sig"},`+
			`{"type":"tool_use","id":"t1","name":"f","input":{}}],"stop_reason":"tool_use"}`)
	}))
	defer up.Close()
	add(t, s, up.URL, "anthropic", 0)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "call_trb_") || !strings.Contains(w.Body.String(), "tool_calls") || strings.Contains(w.Body.String(), "secret") || w.Header().Get("X-To-Router-Degraded") != "" {
		t.Fatalf("tool bridge did not preserve opaque state: %s", w.Body.String())
	}
}

// T014: a stripped reasoning carrier is reported three ways — response header,
// log (asserted indirectly through the usage row) and the usage record — and
// the answer itself survives.
func TestStrippedReasoningIsReported(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	policy, _ := s.GetSettings()
	policy.ReasoningPolicy = store.ReasoningAvailability
	if err := s.SaveSettings(policy); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"c","content":[{"type":"thinking","thinking":"secret","signature":"sig"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
	}))
	defer up.Close()
	add(t, s, up.URL, "anthropic", 0)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("X-To-Router-Degraded"), "reasoning_state_dropped") {
		t.Fatalf("missing degraded header: %q", w.Header().Get("X-To-Router-Degraded"))
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("reasoning leaked: %s", w.Body.String())
	}
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Degraded != "reasoning_state_dropped" || !strings.Contains(string(rows[0].Diagnostic), "reasoning_state_dropped") {
		t.Fatalf("usage degraded = %+v", rows)
	}
}

// A request without state may fail over freely; this is the control case for
// the tests above. The first channel answers 503, the status that still grades
// as the channel's own failure — a 500 would be ambiguous and stop the request
// instead (see status_grading_test.go).
func TestPortableRequestFailsOverSilently(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var bad, good atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bad.Add(1); w.WriteHeader(503) }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer second.Close()
	add(t, s, first.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	if w.Code != 200 || bad.Load() != 1 || good.Load() != 1 {
		t.Fatalf("%d bad %d good %d", w.Code, bad.Load(), good.Load())
	}
	if strings.Contains(w.Body.String(), "recovery") {
		t.Fatalf("portable request must not carry recovery: %s", w.Body.String())
	}
}

// A deterministic conversion failure is not transient: asking the same channel
// again only spends money, so it must not be retried per channel.
func TestDeterministicConversionFailureIsNotRetried(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 3) // three attempts per channel available
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"c","content":[`+
			`{"type":"thinking","thinking":"secret","signature":"sig"},`+
			`{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`)
	}))
	defer up.Close()
	add(t, s, up.URL, "anthropic", 0)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 400 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("deterministic failure was retried %d times", hits.Load())
	}
}
