package codex

import (
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestOperationsRegressionDisableAutomaticDuringLifecycleWait(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	cfg.Warmup.Mode = "daily"
	cfg.Warmup.DailyTimes = []string{f.now.Format("15:04")}
	f.save(t, cfg)
	lock := f.ops.svc.lockFor(f.c.ID)
	lock.Lock()
	done := make(chan struct{})
	current, _ := f.st.Channel(f.c.ID)
	go func() {
		_, _ = f.ops.execute(context.Background(), f.c.ID, "second", f.identity, uuid.NewString(), "warmup", &scheduleClaim{config: cfg, revision: scheduleRevision(current, "second"), apply: func(*operationState) error { return nil }})
		close(done)
	}()
	claimed := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		r, e := f.ops.Report(f.c.ID, "second")
		if e == nil && r.LastWarmup != nil && r.LastWarmup.State == "running" {
			claimed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !claimed {
		lock.Unlock()
		<-done
		t.Fatal("no persistent claim")
	}
	cfg.Warmup.Enabled = false
	f.save(t, cfg)
	lock.Unlock()
	<-done
	r, _ := f.ops.Report(f.c.ID, "second")
	t.Logf("auto_enabled=%v warmup_POSTs=%d receipt=%+v", r.Config.Warmup.Enabled, f.warmups.Load(), r.LastWarmup)
	if f.warmups.Load() != 0 {
		t.Fatal("automatic warmup was sent after automation was disabled while queued")
	}
}
func TestOperationsRegressionChannelReenabledAdoptsOldExhaustionWatch(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	f.save(t, cfg)
	f.percent.Store(100)
	f.ops.tick(context.Background(), f.now)
	c, _ := f.st.Channel(f.c.ID)
	c.Enabled = false
	if e := f.st.SaveChannelConfig(&c); e != nil {
		t.Fatal(e)
	}
	c.Enabled = true
	if e := f.st.SaveChannelConfig(&c); e != nil {
		t.Fatal(e)
	}
	f.percent.Store(0)
	f.now = f.now.Add(2 * time.Minute)
	f.resetAt.Store(f.now.Add(5 * time.Hour).Unix())
	f.ops.tick(context.Background(), f.now)
	t.Logf("warmup_POSTs_after_reenable=%d", f.warmups.Load())
	if f.warmups.Load() != 0 {
		t.Fatal("reenabled channel inherited pre-disable exhaustion watch")
	}
}

func TestOperationsRegressionOldNonmatchingReadRearmsNewResetClaim(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	threshold := 20.0
	cfg.AutoReset.Enabled = true
	cfg.AutoReset.Rules = []store.CredentialResetRule{{WindowSeconds: 18000, RemainingBelow: &threshold, Join: "and"}}
	f.save(t, cfg)
	f.percent.Store(50)
	_, e := f.ops.update(f.c, "second", func(s *operationState) error { s.ResetLatched = true; return nil })
	if e != nil {
		t.Fatal(e)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var intercepted atomic.Bool
	transport := f.ops.svc.Client.Transport
	f.ops.svc.Client = &http.Client{Transport: actionRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/credits" && intercepted.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return transport.RoundTrip(r)
	})}
	tickDone := make(chan struct{})
	go func() { f.ops.tick(context.Background(), f.now); close(tickDone) }()
	<-entered           // The usage sample was 50%; its following cards GET holds the lifecycle lock.
	f.percent.Store(90) // Quota is now low before the new manual reset is claimed.
	resetDone := make(chan error, 1)
	go func() {
		_, e := f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, uuid.NewString())
		resetDone <- e
	}()
	claimed := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		r, e := f.ops.Report(f.c.ID, "second")
		if e == nil && r.LastReset != nil && r.LastReset.State == "running" {
			claimed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !claimed {
		close(release)
		<-tickDone
		<-resetDone
		t.Fatal("manual reset not claimed")
	}
	close(release)
	<-tickDone
	if e := <-resetDone; e != nil {
		t.Fatal(e)
	}
	raw, _ := f.st.LoadCredentialOperationState(f.c.ID, "second")
	state, _ := decodeOperationState(raw)
	t.Logf("after manual reset: latch=%v consume_POSTs=%d", state.ResetLatched, f.consumes.Load())
	f.ops.tick(context.Background(), f.now.Add(time.Minute))
	t.Logf("after same low-quota condition: consume_POSTs=%d", f.consumes.Load())
	if f.consumes.Load() != 1 {
		t.Fatal("pre-claim nonmatching sample rearmed reset and caused another automatic consume")
	}
}

func TestOperationsRegressionExpiryOnlyRuleDoesNotDependOnUsageEndpoint(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	days := 1.0
	cfg.AutoReset.Enabled = true
	cfg.AutoReset.Rules = []store.CredentialResetRule{{ExpiresWithinDays: &days, Join: "or"}}
	f.save(t, cfg)
	transport := f.ops.svc.Client.Transport
	f.ops.svc.Client = &http.Client{Transport: actionRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/usage" {
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable")), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})}
	// Prove the independent card endpoint provides a known true expiry condition.
	c, _ := f.st.Channel(f.c.ID)
	cards, e := f.ops.svc.ResetCreditsForCredential(context.Background(), c, "second")
	if e != nil {
		t.Fatal(e)
	}
	matched, known := resetRulesMatch(cfg.AutoReset.Rules, Usage{}, cards, f.now)
	if !matched || !known {
		t.Fatal("expiry condition not known true")
	}
	f.ops.tick(context.Background(), f.now)
	t.Logf("expiry_match=%v known=%v cards_reads=%d consume_POSTs=%d", matched, known, f.cards.Load(), f.consumes.Load())
	if f.consumes.Load() != 1 {
		t.Fatal("known expiry-only rule was suppressed by unrelated usage read failure")
	}
}

func TestOperationsRegressionUnrelatedCredentialConfigPreservesRecoveryWatch(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	f.save(t, cfg)
	f.percent.Store(100)
	f.ops.tick(context.Background(), f.now)
	c, _ := f.st.Channel(f.c.ID)
	identity, e := store.CredentialOperationsIdentity(c, "first")
	if e != nil {
		t.Fatal(e)
	}
	unrelated := store.DefaultCredentialOperationsConfig()
	unrelated.Warmup.Model = "gpt-5-codex"
	if _, e = f.ops.PatchConfig(f.c.ID, "first", identity, store.CredentialOperationsPatch{Warmup: &unrelated.Warmup}); e != nil {
		t.Fatal(e)
	}
	f.percent.Store(0)
	f.now = f.now.Add(2 * time.Minute)
	f.ops.tick(context.Background(), f.now)
	t.Logf("untouched credential warmup_POSTs=%d", f.warmups.Load())
	if f.warmups.Load() != 1 {
		t.Fatal("another credential's config edit erased this credential's recovery watch")
	}
}
