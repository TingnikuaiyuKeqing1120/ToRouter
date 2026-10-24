package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"torouter/internal/store"
)

// T018: a transport failure is graded by whether the upstream may already have
// processed the request. Transient failures never reached the upstream and stay
// safe to repeat; ambiguous ones may have been executed there, so they are not
// resent — on the same channel or any other.

// fakeTimeout is how http.Client.Timeout surfaces: a *url.Error whose inner
// error answers Timeout() but matches no sentinel.
type fakeTimeout struct{}

func (fakeTimeout) Error() string {
	return "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
}
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func TestTransportErrorIsGradedByIdempotency(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want errClass
	}{
		{"dns failure", &url.Error{Op: "Post", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}, errTransient},
		{"connection refused", &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, errTransient},
		{"dial timeout", &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}}, errTransient},
		{"read timeout after connect", &url.Error{Op: "Post", Err: &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}}, errAmbiguous},
		{"connection reset", &url.Error{Op: "Post", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}, errAmbiguous},
		{"bare connection reset", syscall.ECONNRESET, errAmbiguous},
		{"write error", &url.Error{Op: "Post", Err: &net.OpError{Op: "write", Net: "tcp", Err: errors.New("broken pipe")}}, errAmbiguous},
		{"bare deadline exceeded", os.ErrDeadlineExceeded, errAmbiguous},
		{"client timeout awaiting headers", &url.Error{Op: "Post", Err: fakeTimeout{}}, errAmbiguous},
		// The safe default: an unrecognized failure is not resent.
		{"unknown failure", errors.New("boom"), errAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTransportError(tc.err); got != tc.want {
				t.Fatalf("classifyTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The synthetic cases above mirror the shapes Go produces; this one is the real
// thing a closed port yields on this platform.
func TestRefusedDialIsGradedTransient(t *testing.T) {
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	_, err := net.DialTimeout("tcp", strings.TrimPrefix(gone.URL, "http://"), time.Second)
	if err == nil {
		t.Fatal("expected the closed port to refuse the dial")
	}
	if class := classifyTransportError(err); class != errTransient {
		t.Fatalf("a real refused dial graded %v, want transient", class)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"1", time.Second, true}, {"0", 0, true}, {"120", 120 * time.Second, true},
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0, true}, {"-3", 0, false}, {"soon", 0, false}, {"", 0, false},
		{"9223372036854775807", time.Duration(1<<63 - 1), true},
	}
	for _, tc := range cases {
		if got, ok := parseRetryAfter(tc.value); ok != tc.ok || got != tc.want {
			t.Fatalf("%q => %v/%v want %v/%v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
	far := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(far); !ok || d < 119*time.Minute || d > 2*time.Hour {
		t.Fatalf("date clipped: %v %v", d, ok)
	}
}
func TestFailureCooldownFromSettings(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{{0, 10 * time.Second}, {-5, 10 * time.Second}, {1, time.Second}, {45, 45 * time.Second}, {300, 300 * time.Second}} {
		if got := failureCooldown(store.Settings{FailureCooldownSeconds: tc.seconds}); got != tc.want {
			t.Fatalf("%d => %v want %v", tc.seconds, got, tc.want)
		}
	}
}

// A transient transport failure keeps the T016 behaviour: same-channel retries,
// then failover to the next candidate.
func TestRefusedUpstreamIsRetriedThenFailedOver(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 2)
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	goneURL := gone.URL
	gone.Close()
	var good atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer second.Close()
	add(t, s, goneURL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	start := time.Now()
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if good.Load() != 1 {
		t.Fatalf("failover to the second channel expected: good %d", good.Load())
	}
	// A retry on the dead channel means at least one 200 ms backoff was served;
	// an immediate failover would return in a few milliseconds.
	if elapsed < backoffFloor() {
		t.Fatalf("same-channel retries expected, but the request finished in %v", elapsed)
	}
	// The transient failures never reached the upstream, but they are still the
	// channel's wire failing: the breaker counts them, which is what makes a
	// dead channel cool down instead of absorbing every request's retries.
	aID := channelIDByURL(t, s, goneURL)
	if state := healthPhase(g, aID); state == "" {
		t.Fatalf("a transient transport failure must count against the channel: %v", g.Breaker.Snapshot())
	}
}

// An ambiguous failure is attempted exactly once and never fails over: the
// upstream may have executed the request, and another channel could repeat it.
func TestAmbiguousTimeoutIsAttemptedOnce(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 3)
	var hang, other atomic.Int32
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hang.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); up.Close() }()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer second.Close()
	add(t, s, up.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	// A client shorter than the retry budget: any resend would add attempts, any
	// failover would add a hit on the second channel.
	g.Client = &http.Client{Timeout: 300 * time.Millisecond}
	start := time.Now()
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("no resend and no failover expected, but the request took %v", elapsed)
	}
	if hang.Load() != 1 || other.Load() != 0 {
		t.Fatalf("ambiguous failure must be attempted exactly once and not fail over: hang %d other %d", hang.Load(), other.Load())
	}
	t.Logf("counts hang=%d other=%d code=%d body=%s", hang.Load(), other.Load(), w.Code, w.Body.String())
	if w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "may have") {
		t.Fatalf("unexplained refusal: %s", w.Body.String())
	}
	code, actions := recoveryOf(t, w.Body.Bytes())
	if code != "upstream_may_have_processed" {
		t.Fatalf("recovery code = %q", code)
	}
	// No wait makes an execution that may already have happened safe, so
	// retry_later is not among the ways forward: the decision to spend the
	// request again stays with the client, taken with the risk in hand.
	joined := strings.Join(actions, ",")
	for _, want := range []string{"handoff", "start_fresh_session", "manual_retry_if_acceptable"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing action %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, "retry_later") {
		t.Fatalf("waiting cannot undo an execution that may already have happened: %s", joined)
	}
	var detail struct {
		Error struct {
			Recovery struct {
				Detail string `json:"detail"`
			} `json:"recovery"`
		} `json:"error"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &detail); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(detail.Error.Recovery.Detail, "manual retry") {
		t.Fatalf("the detail must hand the duplicate-execution risk to the client: %q", detail.Error.Recovery.Detail)
	}
	// The exchange broke mid-flight, which is the channel's health: the breaker
	// counts it even though the request is never resent.
	if len(failedCombinations(g)) != 1 {
		t.Fatalf("an ambiguous transport failure must count against the channel: %v", g.Breaker.Snapshot())
	}
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != 502 || !strings.Contains(rows[0].Error, "may have") {
		t.Fatalf("usage = %+v", rows)
	}
}

// A 429 carrying Retry-After is waited out for the asked delay — capped — in
// place of the exponential guess, and the status stays retryable.
func TestRetryAfterReplacesBackoff(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 2)
	var throttled atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		throttled.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer second.Close()
	add(t, s, first.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	start := time.Now()
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if throttled.Load() != 2 {
		t.Fatalf("retries per channel = 2 expected: %d", throttled.Load())
	}
	// One whole second of Retry-After, replacing a 200 ms first backoff: the
	// wait is observable from the outside.
	if elapsed < time.Second {
		t.Fatalf("Retry-After was not honored: finished in %v", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the wait ran long: %v", elapsed)
	}
}

// The ceiling is a setting that travels into the wait: an upstream asking for
// 120 s on a 429 holds the request for the configured cap, not for the ask.
func TestRetryAfterAskIsNotCappedBySetting(t *testing.T) {
	g, s := setup(t)
	if err := s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: 2, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}, FailureCooldownSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	var throttled atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		throttled.Add(1)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer second.Close()
	add(t, s, first.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	start := time.Now()
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if throttled.Load() != 2 {
		t.Fatalf("retries per channel = 2 expected: %d", throttled.Load())
	}
	// The cap, not the ask: an uncapped 120 s would still be waiting, and an
	// ignored header would have failed over in the 200 ms backoff instead.
	if elapsed < 2*time.Second {
		t.Fatalf("the upstream 2 s delay was not waited out: %v", elapsed)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("the wait added a second local backoff: %v", elapsed)
	}
}

// A 408 is graded ambiguous: the server gave up waiting but may already have
// run the request. It is not resent, it does not fail over, and the upstream's
// own status travels.
func TestRequestTimeoutIsNotResent(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 2)
	var gaveUp, other atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gaveUp.Add(1); w.WriteHeader(http.StatusRequestTimeout) }))
	defer up.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer second.Close()
	add(t, s, up.URL, "openai", 0)
	add(t, s, second.URL+"/v1", "openai", 1)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("the upstream's own status must travel: %d %s", w.Code, w.Body.String())
	}
	if gaveUp.Load() != 1 || other.Load() != 0 {
		t.Fatalf("408 must not be resent or failed over: gaveUp %d other %d", gaveUp.Load(), other.Load())
	}
	if code, _ := recoveryOf(t, w.Body.Bytes()); code != "upstream_may_have_processed" {
		t.Fatalf("recovery code = %q", code)
	}
}

// backoffFloor is the smallest wait a first same-channel retry costs, used as a
// lower bound that distinguishes "retried" from "failing over immediately".
func backoffFloor() time.Duration { return 150 * time.Millisecond }

func recoveryOf(t *testing.T, body []byte) (string, []string) {
	t.Helper()
	var err struct {
		Error struct {
			Recovery struct {
				Code    string   `json:"code"`
				Actions []string `json:"actions"`
			} `json:"recovery"`
		} `json:"error"`
	}
	if e := json.Unmarshal(body, &err); e != nil {
		t.Fatal(e)
	}
	return err.Error.Recovery.Code, err.Error.Recovery.Actions
}
