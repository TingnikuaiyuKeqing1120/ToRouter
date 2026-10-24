package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"torouter/internal/health"
	"torouter/internal/quota"
	route "torouter/internal/router"
	"torouter/internal/store"
)

func runtimePool(t *testing.T, handler http.HandlerFunc) (*Gateway, store.Channel) {
	t.Helper()
	g, s := setup(t)
	settings(t, s, 3, 2)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	c := store.Channel{Name: "pool", Enabled: true, Models: []string{"m", "other"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}, Credentials: []store.CredentialItem{{ID: "a", Label: "first", APIKey: "first-secret", Enabled: true}, {ID: "b", Label: "second", APIKey: "second-secret", Enabled: true}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Models {
		if err := s.SaveDownstreamModel(store.DownstreamModel{ID: m, Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: m, Enabled: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	return g, c
}
func runtimeTurn(g *Gateway, session, model string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	if session != "" {
		r.Header.Set("x-session-id", session)
	}
	serveTestClient(g, w, r)
	return w
}
func quotaFact(c store.Channel, cred store.CredentialItem, available bool) quota.Observation {
	return quota.Observation{ChannelID: c.ID, Generation: c.Generation, CredentialID: cred.ID, Supported: true, Available: &available, Reason: "balance exhausted", CheckedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
}
func TestRuntimeSuccessfulCredentialSurvivesQuotaZeroUntilRealFailure(t *testing.T) {
	var first, second, checks atomic.Int32
	var fail, exhausted atomic.Bool
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-secret" {
			first.Add(1)
			if fail.Load() {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(503)
				return
			}
		} else {
			second.Add(1)
		}
		io.WriteString(w, `{"choices":[]}`)
	})
	g.QuotaCheck = func(ctx context.Context, c store.Channel, cred store.CredentialItem) quota.Observation {
		checks.Add(1)
		return quotaFact(c, cred, cred.ID != "a" || !exhausted.Load())
	}
	if w := runtimeTurn(g, "", "m"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	exhausted.Store(true)
	g.ObserveQuota(c, c.Credentials[0], quotaFact(c, c.Credentials[0], false))
	if w := runtimeTurn(g, "", "m"); w.Code != 200 || first.Load() != 2 || checks.Load() != 1 {
		t.Fatalf("successful path interrupted: code=%d hits=%d checks=%d", w.Code, first.Load(), checks.Load())
	}
	fail.Store(true)
	if w := runtimeTurn(g, "", "m"); w.Code != 200 || first.Load() != 3 || second.Load() != 1 {
		t.Fatalf("failed path not switched: code=%d hits=%d/%d", w.Code, first.Load(), second.Load())
	}
	if w := runtimeTurn(g, "", "m"); w.Code != 200 || first.Load() != 3 || second.Load() != 2 {
		t.Fatal("backup path not retained")
	}
	// A new session must reselect, skip quota zero and use b.
	if w := runtimeTurn(g, "new", "m"); w.Code != 200 || first.Load() != 3 || second.Load() != 3 {
		t.Fatal("new selection used exhausted credential")
	}
}
func TestRuntimeAllCoolingIsNotSentAndModelIsolated(t *testing.T) {
	var calls atomic.Int32
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{"choices":[]}`) })
	for _, cred := range c.Credentials {
		l, _ := g.Breaker.Acquire(combination(route.Attempt{Channel: c, Endpoint: c.Endpoints[0], Model: "m"}, cred.ID))
		g.Breaker.Failure(l)
	}
	if w := runtimeTurn(g, "", "m"); w.Code != 503 || calls.Load() != 0 {
		t.Fatalf("cooldown bypassed: %d %d", w.Code, calls.Load())
	}
	rows, err := g.Store.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 1 || !rows[0].NotSent || rows[0].ActualCost != nil || rows[0].InputTokens != 0 {
		t.Fatalf("missing unpriced rejection: %+v %v", rows, err)
	}
	if w := runtimeTurn(g, "", "other"); w.Code != 200 || calls.Load() != 1 {
		t.Fatal("other model was cooled")
	}
	snapshot := g.RuntimeStatus()
	if len(snapshot.Rows) != 2 || snapshot.ProblemCount != 2 || len(snapshot.Rows[0].Models) != 2 || !snapshot.Rows[0].Models[0].Red || snapshot.Rows[0].Models[0].Model != "m" {
		t.Fatalf("status projection=%+v", snapshot)
	}
}
func TestRuntimeResetAllowsAttemptAndRejectsOldQuota(t *testing.T) {
	var calls, checks atomic.Int32
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(401) })
	c.Credentials = c.Credentials[:1]
	if err := g.Store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	old := quotaFact(c, c.Credentials[0], false)
	g.QuotaCheck = func(ctx context.Context, c store.Channel, cred store.CredentialItem) quota.Observation {
		checks.Add(1)
		return quotaFact(c, cred, false)
	}
	g.ObserveQuota(c, c.Credentials[0], old)
	if w := runtimeTurn(g, "", "m"); w.Code != 503 || calls.Load() != 0 {
		t.Fatal("quota zero must skip new selection")
	}
	if err := g.ResetStatus(c.ID, "a", ""); err != nil {
		t.Fatal(err)
	}
	g.ObserveQuota(c, c.Credentials[0], old)
	if g.RuntimeStatus().ProblemCount != 0 {
		t.Fatal("old quota undid reset")
	}
	before := checks.Load()
	if w := runtimeTurn(g, "", "m"); w.Code != 401 || calls.Load() != 1 || checks.Load() != before {
		t.Fatalf("reset was immediately blocked by quota: code=%d calls=%d checks=%d/%d", w.Code, calls.Load(), checks.Load(), before)
	}
	if g.RuntimeStatus().ProblemCount != 1 {
		t.Fatal("fresh failure after reset not counted")
	}
}
func TestRuntimeResetInvalidatesInFlightFailure(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-finish; w.WriteHeader(500) })
	done := make(chan struct{})
	go func() { runtimeTurn(g, "", "m"); close(done) }()
	<-started
	if err := g.ResetStatus(c.ID, "a", "m"); err != nil {
		t.Fatal(err)
	}
	close(finish)
	<-done
	for k, v := range g.Breaker.Snapshot() {
		if k.CredentialID == "a" && (v.Failures != 0 || v.Red || v.LastRequest != nil) {
			t.Fatalf("old receipt undid reset: %+v", v)
		}
	}
}
func TestRuntimeQuotaIdentityExpiryAndModelReset(t *testing.T) {
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"choices":[]}`) })
	o := quotaFact(c, c.Credentials[0], false)
	g.ObserveQuota(c, c.Credentials[0], o)
	if err := g.ResetStatus(c.ID, "a", "m"); err != nil {
		t.Fatal(err)
	}
	if g.RuntimeStatus().ProblemCount != 1 {
		t.Fatal("model reset cleared credential quota")
	}
	o.CheckedAt = time.Now()
	o.ExpiresAt = time.Now().Add(-time.Second)
	g.ObserveQuota(c, c.Credentials[0], o)
	if g.RuntimeStatus().ProblemCount != 0 {
		t.Fatal("expired quota must be unknown")
	}
	o.CheckedAt = time.Now()
	o.ExpiresAt = time.Now().Add(time.Hour)
	o.Generation++
	g.ObserveQuota(c, c.Credentials[0], o)
	if g.RuntimeStatus().ProblemCount != 0 {
		t.Fatal("wrong generation accepted")
	}
	if g.ResetStatus(c.ID, "a", "missing") == nil || g.ResetStatus(c.ID, "missing", "") == nil || g.ResetStatus(-1, "a", "") == nil {
		t.Fatal("reset accepted unknown object")
	}
}
func TestRuntimeRoundRobinStillRotates(t *testing.T) {
	var first, second atomic.Int32
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-secret" {
			first.Add(1)
		} else {
			second.Add(1)
		}
		io.WriteString(w, `{"choices":[]}`)
	})
	c.DispatchMode = store.DispatchModeRoundRobin
	if err := g.Store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if w := runtimeTurn(g, "", "m"); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if first.Load() != 2 || second.Load() != 2 {
		t.Fatalf("round robin lost: %d/%d", first.Load(), second.Load())
	}
}
func TestRuntimeStatusAggregatesEndpointsAndLastRequest(t *testing.T) {
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"choices":[]}`) })
	c.Endpoints = append(c.Endpoints, store.Endpoint{Format: store.FormatAnthropicMessages, BaseURL: c.Endpoints[0].BaseURL})
	if err := g.Store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	key := combination(route.Attempt{Channel: c, Endpoint: c.Endpoints[1], Model: "m"}, "a")
	l, _ := g.Breaker.Acquire(key)
	g.Breaker.Failure(l)
	g.Breaker.Observe(l, health.LastRequest{At: time.Now(), Status: 503, DurationMS: 12, Error: "upstream returned HTTP 503"})
	v := g.RuntimeStatus()
	if v.ProblemCount != 1 || !v.Rows[0].Red || v.Rows[0].Models[0].LastRequest == nil || v.Rows[0].Models[0].LastRequest.Status != 503 {
		t.Fatalf("multi-interface aggregate=%+v", v)
	}
}

func TestRuntimeCancellationDuringResponseReadDoesNotFailHealth(t *testing.T) {
	started := make(chan struct{})
	g, _ := runtimePool(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[`)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`)).WithContext(ctx)
		serveTestClient(g, httptest.NewRecorder(), req)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled read did not finish")
	}
	if len(failedCombinations(g)) != 0 {
		t.Fatal("cancelled response read cooled combination")
	}
	rows, err := g.Store.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 1 || rows[0].Status != 499 {
		t.Fatalf("cancellation accounting=%+v %v", rows, err)
	}
}

func TestRuntimeFailureRetryReselectsQuotaZero(t *testing.T) {
	for _, status := range []int{429, 503} {
		for _, poolSize := range []int{1, 2} {
			t.Run(fmt.Sprintf("%d/pool%d", status, poolSize), func(t *testing.T) {
				var calls atomic.Int32
				var failed atomic.Bool
				g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if failed.Load() {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(status)
						return
					}
					io.WriteString(w, `{"choices":[]}`)
				})
				c.Credentials = c.Credentials[:poolSize]
				if err := g.Store.SaveChannel(&c); err != nil {
					t.Fatal(err)
				}
				if w := runtimeTurn(g, "", "m"); w.Code != 200 {
					t.Fatal(w.Code)
				}
				for _, cred := range c.Credentials {
					g.ObserveQuota(c, cred, quotaFact(c, cred, false))
				}
				failed.Store(true)
				w := runtimeTurn(g, "", "m")
				if calls.Load() != 2 {
					t.Fatalf("failed successful path bypassed quota on retry: calls=%d HTTP=%d", calls.Load(), w.Code)
				}
			})
		}
	}
}
func TestRuntimeNeutralOutcomeKeepsQuotaZeroSuccessfulPath(t *testing.T) {
	for _, cancelRead := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRead), func(t *testing.T) {
			var calls atomic.Int32
			started := make(chan struct{})
			g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n == 2 {
					w.WriteHeader(400)
					if cancelRead {
						io.WriteString(w, `{"error":`)
						w.(http.Flusher).Flush()
						close(started)
						<-r.Context().Done()
					}
					return
				}
				io.WriteString(w, `{"choices":[]}`)
			})
			c.Credentials = c.Credentials[:1]
			if err := g.Store.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if w := runtimeTurn(g, "", "m"); w.Code != 200 {
				t.Fatal(w.Code)
			}
			g.ObserveQuota(c, c.Credentials[0], quotaFact(c, c.Credentials[0], false))
			if cancelRead {
				reading := make(chan struct{})
				transport := g.Client.Transport
				g.Client.Transport = runtimeTransportFunc(func(r *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(r)
					if err == nil && resp.StatusCode == 400 {
						resp.Body = &runtimeReadSignalBody{ReadCloser: resp.Body, signal: reading}
					}
					return resp, err
				})
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`)).WithContext(ctx)
					serveTestClient(g, httptest.NewRecorder(), r)
				}()
				<-started
				<-reading
				cancel()
				<-done
			} else if w := runtimeTurn(g, "", "m"); w.Code != 400 {
				t.Fatal(w.Code)
			}
			if w := runtimeTurn(g, "", "m"); w.Code != 200 || calls.Load() != 3 {
				t.Fatalf("neutral outcome erased successful credential: code=%d calls=%d", w.Code, calls.Load())
			}
			if len(failedCombinations(g)) != 0 {
				t.Fatal("neutral outcome cooled path")
			}
		})
	}
}

type runtimeTransportFunc func(*http.Request) (*http.Response, error)

func (f runtimeTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type runtimeReadSignalBody struct {
	io.ReadCloser
	once   sync.Once
	signal chan struct{}
}

func (b *runtimeReadSignalBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.signal) })
	return b.ReadCloser.Read(p)
}

func TestRuntimeTransientRetryReselectsQuotaZero(t *testing.T) {
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"choices":[]}`) })
	c.Credentials = c.Credentials[:1]
	if err := g.Store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if w := runtimeTurn(g, "", "m"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	g.ObserveQuota(c, c.Credentials[0], quotaFact(c, c.Credentials[0], false))
	var attempts atomic.Int32
	g.Client.Transport = runtimeTransportFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	})
	if w := runtimeTurn(g, "", "m"); w.Code != 502 || attempts.Load() != 1 {
		t.Fatalf("transient retry bypassed zero quota: code=%d attempts=%d", w.Code, attempts.Load())
	}
}
func TestRuntimePathsBoundedAndRemovedConfigurationReclaimed(t *testing.T) {
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"choices":[]}`) })
	if _, err := g.reconcileRuntime(); err != nil {
		t.Fatal(err)
	}
	k := combination(route.Attempt{Channel: c, Endpoint: c.Endpoints[0], Model: "m"}, "a")
	lease, _ := g.Breaker.Acquire(k)
	for i := 0; i < maxSuccessfulPaths+2; i++ {
		g.rememberPath(fmt.Sprintf("scope%d", i), k)
	}
	g.runtimeMu.Lock()
	count := len(g.runtimeLocked().paths)
	g.runtimeMu.Unlock()
	if count > maxSuccessfulPaths || count < maxSuccessfulPaths/2 {
		t.Fatalf("path capacity=%d", count)
	}
	if _, ok := g.previousPath("scope0"); ok {
		t.Fatal("oldest entry not evicted")
	}
	if got, ok := g.previousPath(fmt.Sprintf("scope%d", maxSuccessfulPaths+1)); !ok || got != k {
		t.Fatal("newest path lost")
	}
	// Status reads retain an active attempt on the current configuration.
	_ = g.RuntimeStatus()
	if !g.Breaker.Failure(lease.WithRetryAfter(0)) {
		t.Fatal("status read invalidated current active lease")
	}
	if err := g.ResetStatus(c.ID, "a", ""); err != nil {
		t.Fatal(err)
	}
	g.ObserveQuota(c, c.Credentials[0], quotaFact(c, c.Credentials[0], false))
	g.credentials().Selected(k)
	oldChannel := c
	oldCred := c.Credentials[0]
	oldObservation := quotaFact(c, oldCred, false)
	c.Credentials[0].APIKey = "replacement-key"
	if err := g.Store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	_ = g.RuntimeStatus()
	g.rememberPath("late", k)
	g.ObserveQuota(oldChannel, oldCred, oldObservation)
	g.Breaker.Failure(lease)
	if len(g.Breaker.Snapshot()) != 0 {
		t.Fatal("old health survived generation change")
	}
	g.runtimeMu.Lock()
	state := g.runtimeLocked()
	if len(state.paths) != 0 || len(state.quotas) != 0 || len(state.resets) != 0 || len(state.bypass) != 0 {
		t.Fatalf("obsolete config retained: paths=%d quota=%d reset=%d bypass=%d", len(state.paths), len(state.quotas), len(state.resets), len(state.bypass))
	}
	g.runtimeMu.Unlock()
	// Exercise the same cleanup for deleting the entity, rather than only generation changes.
	newKey := combination(route.Attempt{Channel: c, Endpoint: c.Endpoints[0], Model: "m"}, "a")
	current, _ := g.Breaker.Acquire(newKey)
	g.Breaker.Failure(current)
	g.rememberPath("new", newKey)
	g.ObserveQuota(c, c.Credentials[0], quotaFact(c, c.Credentials[0], false))
	if err := g.ResetStatus(c.ID, "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.Store.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	_ = g.RuntimeStatus()
	g.runtimeMu.Lock()
	state = g.runtimeLocked()
	if len(state.paths) != 0 || len(state.quotas) != 0 || len(state.resets) != 0 || len(state.bypass) != 0 || len(state.validQuotas) != 0 {
		t.Fatal("deleted entity retained runtime state")
	}
	g.runtimeMu.Unlock()
	if len(g.Breaker.Snapshot()) != 0 {
		t.Fatal("deleted entity retained health")
	}
}

func TestRuntimeReconciliationKeepsCatalogueBindings(t *testing.T) {
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"choices":[]}`) })
	c.AvailableModels = []string{"catalogue-only"}
	if err := g.Store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := g.Store.SaveDownstreamModel(store.DownstreamModel{ID: "catalogue-only", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "catalogue-only", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if w := runtimeTurn(g, "", "catalogue-only"); w.Code != 200 {
		t.Fatalf("valid catalogue binding rejected: %d %s", w.Code, w.Body.String())
	}
	found := false
	for _, m := range g.RuntimeStatus().Rows[0].Models {
		if m.Model == "catalogue-only" {
			found = true
		}
	}
	if !found {
		t.Fatal("observed catalogue model missing from projection")
	}
	if err := g.ResetStatus(c.ID, "a", "catalogue-only"); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeStatusDoesNotExpandDiscoveryCatalogue(t *testing.T) {
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"choices":[]}`) })
	for i := 0; i < 10000; i++ {
		c.AvailableModels = append(c.AvailableModels, fmt.Sprintf("discovered-%d", i))
	}
	if err := g.Store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	status := g.RuntimeStatus()
	if len(status.Rows) != 2 {
		t.Fatal("credential rows changed")
	}
	for _, row := range status.Rows {
		if len(row.Models) != 2 {
			t.Fatalf("discovery catalogue expanded into status: %d models", len(row.Models))
		}
	}
	if len(g.Breaker.Snapshot()) != 0 {
		t.Fatal("status read precreated combination records")
	}
}

func TestRuntimePaymentRequiredFailsOnceThenReselectsQuota(t *testing.T) {
	var first, second, checks atomic.Int32
	var exhausted atomic.Bool
	g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-secret" {
			first.Add(1)
			if exhausted.Load() {
				w.WriteHeader(http.StatusPaymentRequired)
				return
			}
		} else {
			second.Add(1)
		}
		io.WriteString(w, `{"choices":[]}`)
	})
	g.QuotaCheck = func(ctx context.Context, ch store.Channel, cred store.CredentialItem) quota.Observation {
		checks.Add(1)
		return quotaFact(ch, cred, cred.ID != "a" || !exhausted.Load())
	}
	if w := runtimeTurn(g, "", "m"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	exhausted.Store(true)
	if w := runtimeTurn(g, "", "m"); w.Code != http.StatusPaymentRequired || first.Load() != 2 || second.Load() != 0 || checks.Load() != 1 {
		t.Fatalf("402 was resent or changed: status=%d hits=%d/%d queries=%d", w.Code, first.Load(), second.Load(), checks.Load())
	}
	status := g.RuntimeStatus()
	if status.ProblemCount != 1 || !status.Rows[0].Red || status.Rows[0].Failures != 1 || status.Rows[0].Models[0].LastRequest == nil || status.Rows[0].Models[0].LastRequest.Status != 402 {
		t.Fatalf("402 did not mark the combination unavailable: %+v", status)
	}
	g.runtimeMu.Lock()
	paths := len(g.runtimeLocked().paths)
	g.runtimeMu.Unlock()
	if paths != 0 {
		t.Fatal("402 kept the successful path")
	}
	key := combination(route.Attempt{Channel: c, Endpoint: c.Endpoints[0], Model: "m"}, "a")
	if !waitFor(context.Background(), g.Breaker.Remaining(key)) {
		t.Fatal("cooldown wait interrupted")
	}
	if w := runtimeTurn(g, "", "m"); w.Code != 200 || first.Load() != 2 || second.Load() != 1 || checks.Load() != 3 {
		t.Fatalf("new selection bypassed quota: status=%d hits=%d/%d queries=%d", w.Code, first.Load(), second.Load(), checks.Load())
	}
}

func TestRuntimeQuotaRefreshDuringRetryWaitPreventsResend(t *testing.T) {
	for _, kind := range []string{"429", "503", "dial"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			status := http.StatusTooManyRequests
			if kind == "503" {
				status = http.StatusServiceUnavailable
			}
			g, c := runtimePool(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) > 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, `{"choices":[]}`)
			})
			c.Credentials = c.Credentials[:1]
			if err := g.Store.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if w := runtimeTurn(g, "", "m"); w.Code != 200 {
				t.Fatal(w.Code)
			}
			if kind == "dial" {
				status = 502
				g.Client.Transport = runtimeTransportFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
				})
			}
			key := combination(route.Attempt{Channel: c, Endpoint: c.Endpoints[0], Model: "m"}, "a")
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { result <- runtimeTurn(g, "", "m") }()
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()
			for g.Breaker.Remaining(key) == 0 {
				select {
				case got := <-result:
					t.Fatalf("retry finished before controlled refresh: %d", got.Code)
				case <-deadline.C:
					t.Fatal("failure never entered cooldown")
				case <-poll.C:
				}
			}
			// This accepted observation arrives after the retry decision and before its
			// timer expires; the actual retry must perform selection again.
			g.ObserveQuota(c, c.Credentials[0], quotaFact(c, c.Credentials[0], false))
			select {
			case got := <-result:
				if got.Code != status || calls.Load() != 2 {
					t.Fatalf("wait-time quota refresh ignored: status=%d calls=%d", got.Code, calls.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("retry wait did not finish")
			}
		})
	}
}
