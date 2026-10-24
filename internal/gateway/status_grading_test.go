package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/store"
)

// A POST inference request that comes back 500, 502 or 504 was not proven
// unprocessed: an upstream gateway reports a request it already ran as one of
// its own timeouts, and 5xx is where those land. The status is graded exactly
// like the ambiguous transport failures — spent once, never resent, never
// failed over — while the breaker still counts it, because a channel answering
// 5xx is what the gateway-level health signal is for.
func TestAmbiguousServerErrorIsSpentOnceAndNotFailedOver(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 3)
			var first, backup atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first.Add(1)
				w.WriteHeader(status)
			}))
			defer up.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backup.Add(1)
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer second.Close()
			add(t, s, up.URL, "openai", 0)
			add(t, s, second.URL+"/v1", "openai", 1)
			w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
			if w.Code != 502 {
				t.Fatalf("an ambiguous 5xx must answer 502, got %d %s", w.Code, w.Body.String())
			}
			if first.Load() != 1 || backup.Load() != 0 {
				t.Fatalf("an ambiguous 5xx must be attempted exactly once and never fail over: first %d backup %d", first.Load(), backup.Load())
			}
			if code, _ := recoveryOf(t, w.Body.Bytes()); code != "upstream_may_have_processed" {
				t.Fatalf("recovery code = %q", code)
			}
			rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Status != 502 || !strings.Contains(rows[0].Error, "may have been processed") {
				t.Fatalf("usage = %+v", rows)
			}
			// Nothing is resent, but the channel's answer still counts against
			// its breaker: 5xx is a gateway-level health signal.
			if snap := failedCombinations(g); len(snap) != 1 {
				t.Fatalf("breaker snapshot = %v, want one counted failure", snap)
			}
		})
	}
}

// The other side of the grading: a 501 or a 505 is the upstream answering about
// the request or about what it supports, which another attempt, here or on
// another channel, reproduces exactly. It is spent once, it does not fail over,
// the breaker is released without a failure (a missing capability is not an
// unhealthy channel), and the upstream's own status travels.
func TestCapabilityServerErrorIsSpentOnceWithoutCounting(t *testing.T) {
	for _, status := range []int{http.StatusNotImplemented, http.StatusHTTPVersionNotSupported} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 3)
			var first, backup atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first.Add(1)
				w.WriteHeader(status)
			}))
			defer up.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backup.Add(1)
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer second.Close()
			add(t, s, up.URL, "openai", 0)
			add(t, s, second.URL+"/v1", "openai", 1)
			w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
			if w.Code != status {
				t.Fatalf("the upstream's own status must travel, got %d %s", w.Code, w.Body.String())
			}
			if first.Load() != 1 || backup.Load() != 0 {
				t.Fatalf("a deterministic 5xx must be attempted exactly once and never fail over: first %d backup %d", first.Load(), backup.Load())
			}
			if !strings.Contains(w.Body.String(), "would fail the same way") {
				t.Fatalf("unexplained refusal: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "may have been processed") {
				t.Fatalf("a deterministic failure must not be graded ambiguous: %s", w.Body.String())
			}
			if snap := failedCombinations(g); len(snap) != 0 {
				t.Fatalf("a capability signal must not count against the channel: %v", snap)
			}
			rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Status != status {
				t.Fatalf("usage = %+v", rows)
			}
		})
	}
}

// The two axes come apart on a resource-exhaustion 5xx. A 507 — or a 509, the
// bandwidth flavour of the same thing — proves nothing about the request being
// resendable: another attempt, here or on another channel, meets the same
// exhausted resource, so it is spent once and never failed over like any other
// deterministic answer. What it is about is the channel's health, so the breaker
// counts it: an upstream that is out of storage or bandwidth is exactly the
// gateway-level health signal, and releasing it would leave a channel that
// cannot serve anything looking perfectly healthy.
func TestResourceExhaustedStatusIsSpentOnceButCounts(t *testing.T) {
	for _, status := range []int{http.StatusInsufficientStorage, 509} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 3)
			var first, backup atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first.Add(1)
				w.WriteHeader(status)
			}))
			defer up.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backup.Add(1)
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer second.Close()
			add(t, s, up.URL, "openai", 0)
			add(t, s, second.URL+"/v1", "openai", 1)
			w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
			if w.Code != status {
				t.Fatalf("the upstream's own status must travel, got %d %s", w.Code, w.Body.String())
			}
			if first.Load() != 1 || backup.Load() != 0 {
				t.Fatalf("a resource-exhaustion 5xx must be attempted exactly once and never fail over: first %d backup %d", first.Load(), backup.Load())
			}
			// The message owns both halves: the request is not what failed, and
			// waiting it out or re-sending it changes nothing.
			if !strings.Contains(w.Body.String(), "out of a resource") || !strings.Contains(w.Body.String(), "would meet the same exhaustion") {
				t.Fatalf("unexplained refusal: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "may have been processed") {
				t.Fatalf("a deterministic answer must not be graded ambiguous: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "not about its health") {
				t.Fatalf("an exhausted resource is about the channel's health: %s", w.Body.String())
			}
			// The breaker counts it even though the request is not resent.
			if snap := failedCombinations(g); len(snap) != 1 {
				t.Fatalf("an exhausted resource must count against the channel: %v", snap)
			}
			rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Status != status {
				t.Fatalf("usage = %+v", rows)
			}
		})
	}
}

// The grade is two axes, and every pairing the table allows is worth a row:
// safe-with-failure (429/503), ambiguous-with-failure (408 and the 500/502/504
// family), deterministic-with-failure (401/402/403/507/509) and deterministic-with-neutral
// (other 4xx, 501/505 and any other 5xx the grading does not name). The transport
// grader is the other producer of the same two axes: a DNS or dial failure is
// safe and counts, a read or write failure is ambiguous and counts — both
// asserted where the wire is exercised, in retry_grading_test.go.
func TestGradeStatusSplitsReplayFromHealth(t *testing.T) {
	cases := []struct {
		status int
		replay replayDecision
		health healthImpact
	}{
		{http.StatusTooManyRequests, replayAllowed, healthFailure},
		{http.StatusServiceUnavailable, replayAllowed, healthFailure},
		{http.StatusInternalServerError, replayAmbiguous, healthFailure},
		{http.StatusBadGateway, replayAmbiguous, healthFailure},
		{http.StatusGatewayTimeout, replayAmbiguous, healthFailure},
		{http.StatusRequestTimeout, replayAmbiguous, healthFailure},
		{http.StatusInsufficientStorage, replayDeterministic, healthFailure},
		{509, replayDeterministic, healthFailure},
		{http.StatusNotImplemented, replayDeterministic, healthNeutral},
		{http.StatusHTTPVersionNotSupported, replayDeterministic, healthNeutral},
		{http.StatusLoopDetected, replayDeterministic, healthNeutral},
		{http.StatusNetworkAuthenticationRequired, replayDeterministic, healthNeutral},
		{http.StatusBadRequest, replayDeterministic, healthNeutral},
		{http.StatusUnauthorized, replayDeterministic, healthFailure},
		{http.StatusPaymentRequired, replayDeterministic, healthFailure},
		{http.StatusForbidden, replayDeterministic, healthFailure},
		{http.StatusNotFound, replayDeterministic, healthNeutral},
		{http.StatusUnprocessableEntity, replayDeterministic, healthNeutral},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("HTTP %d", tc.status), func(t *testing.T) {
			if got := gradeStatus(tc.status); got.replay != tc.replay || got.health != tc.health {
				t.Fatalf("gradeStatus(%d) = %v/%v, want %v/%v", tc.status, got.replay, got.health, tc.replay, tc.health)
			}
		})
	}
}
