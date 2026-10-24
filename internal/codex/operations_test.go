package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"torouter/internal/store"
)

type operationsFixture struct {
	ops                                    *Operations
	st                                     *store.Store
	c                                      store.Channel
	identity                               string
	warmups, consumes, reads, cards        atomic.Int32
	status, percent, cardCount, readStatus atomic.Int32
	resetAt                                atomic.Int64
	usageBody                              atomic.Value // Optional synthetic usage response across lifecycle stages.
	now                                    time.Time
}

func newOperationsFixture(t *testing.T, extra http.HandlerFunc) *operationsFixture {
	t.Helper()
	f := &operationsFixture{st: actionTestStore(t), now: time.Now().Truncate(time.Second)}
	f.cardCount.Store(2)
	f.resetAt.Store(f.now.Unix() + 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer a-second" {
			t.Errorf("operation selected wrong credential: %s", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/responses":
			f.warmups.Add(1)
			if extra != nil {
				extra(w, r)
				return
			}
			if status := f.status.Load(); status != 0 {
				w.WriteHeader(int(status))
				return
			}
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		case "/credits/consume":
			f.consumes.Add(1)
			if extra != nil {
				extra(w, r)
				return
			}
			if status := f.status.Load(); status != 0 {
				w.WriteHeader(int(status))
			}
		case "/usage":
			f.reads.Add(1)
			if status := f.readStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				return
			}
			if body, ok := f.usageBody.Load().(string); ok {
				io.WriteString(w, body)
				return
			}
			fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":18000,"reset_at":%d}}}`, f.percent.Load(), f.resetAt.Load())
		case "/credits":
			f.cards.Add(1)
			if status := f.readStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				return
			}
			if f.cardCount.Load() == 0 {
				io.WriteString(w, `{"credits":[],"available_count":0}`)
				return
			}
			fmt.Fprintf(w, `{"available_count":%d,"credits":[{"id":"card","status":"available","expires_at":%d}]}`, f.cardCount.Load(), f.now.Add(12*time.Hour).Unix())
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	f.c = actionTestChannel(t, f.st, "a")
	f.identity, _ = store.CredentialOperationsIdentity(f.c, "second")
	f.ops = NewOperations(NewService(f.st, server.Client(), Endpoints{Backend: server.URL, Usage: server.URL + "/usage", ResetCredits: server.URL + "/credits"}))
	f.ops.now = func() time.Time { return f.now }
	settings, _ := f.st.GetSettings()
	n := 0
	settings.QuotaRefreshSeconds = &n
	if err := f.st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *operationsFixture) save(t *testing.T, cfg store.CredentialOperationsConfig) {
	t.Helper()
	current, err := f.st.Channel(f.c.ID)
	if err != nil {
		t.Fatal(err)
	}
	percent := float64(f.percent.Load())
	snapshot := usageWithWindows(&percent, 18000, f.resetAt.Load())
	snapshot.CapturedAt = f.now
	raw, _ := json.Marshal(snapshot)
	if err := f.st.SaveQuotaCache(current, "second", store.QuotaCache{Snapshot: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ops.PatchConfig(f.c.ID, "second", f.identity, store.CredentialOperationsPatch{Warmup: &cfg.Warmup}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ops.PatchConfig(f.c.ID, "second", f.identity, store.CredentialOperationsPatch{AutoReset: &cfg.AutoReset}); err != nil {
		t.Fatal(err)
	}
}

func (f *operationsFixture) warmupConfig() store.CredentialOperationsConfig {
	cfg := store.DefaultCredentialOperationsConfig()
	cfg.Warmup.Model = "gpt-5-codex"
	return cfg
}

func TestOperationsReportIsLocalAndRequestIDsAreDurable(t *testing.T) {
	f := newOperationsFixture(t, nil)
	report, err := f.ops.Report(f.c.ID, "second")
	if err != nil || !report.Supported || report.Identity != f.identity || report.Config.Warmup.Enabled || report.Config.AutoReset.Enabled {
		t.Fatalf("default: %+v %v", report, err)
	}
	f.ops.tick(context.Background(), f.now)
	if f.reads.Load()+f.cards.Load()+f.warmups.Load()+f.consumes.Load() != 0 {
		t.Fatal("default off or report made network request")
	}
	f.save(t, f.warmupConfig())
	first := uuid.NewString()
	report, err = f.ops.Warmup(context.Background(), f.c.ID, "second", f.identity, first)
	if err != nil || report.LastWarmup.State != "succeeded" {
		t.Fatalf("warmup: %+v %v", report, err)
	}
	if _, err = f.ops.Warmup(context.Background(), f.c.ID, "second", f.identity, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	restarted := NewOperations(f.ops.svc)
	if err := f.st.SetCredentialEnabled(f.c.ID, "second", false); err != nil {
		t.Fatal(err)
	}
	report, err = restarted.Warmup(context.Background(), f.c.ID, "second", f.identity, strings.ToUpper(first))
	if err != nil || report.LastWarmup.RequestID != first || report.LastWarmup.State != "succeeded" || f.warmups.Load() != 2 {
		t.Fatalf("repeated old ID was not original: %+v %v", report, err)
	}
	if _, err := restarted.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, first); !errors.Is(err, store.ErrInvalidCredentialOperation) {
		t.Fatalf("cross-kind ID reused: %v", err)
	}
}

func TestOperationsUnknownBlocksAndAcknowledgeNeverReplays(t *testing.T) {
	f := newOperationsFixture(t, nil)
	f.status.Store(503)
	id := uuid.NewString()
	report, err := f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, id)
	if err != nil || report.LastReset.State != "unknown" {
		t.Fatalf("unknown: %+v %v", report, err)
	}
	if _, err := f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, uuid.NewString()); !errors.Is(err, store.ErrCredentialActionBusy) {
		t.Fatalf("unknown did not block: %v", err)
	}
	if report, err = f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, id); err != nil || report.LastReset.State != "unknown" {
		t.Fatalf("same unknown ID: %+v %v", report, err)
	}
	if _, err = f.ops.RefreshCards(context.Background(), f.c.ID, "second", f.identity); err != nil {
		t.Fatal(err)
	}
	report, err = f.ops.Acknowledge(f.c.ID, "second", f.identity, "reset")
	if err != nil || report.LastReset.State != "acknowledged" {
		t.Fatalf("ack: %+v %v", report, err)
	}
	f.status.Store(0)
	if _, err = f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, id); err != nil {
		t.Fatal(err)
	}
	if f.consumes.Load() != 1 {
		t.Fatal("ack replayed the old request")
	}
	if _, err = f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if f.consumes.Load() != 2 {
		t.Fatal("explicit new request remained blocked")
	}
}

func TestOperationsRestartedRunningIsUnknownAndCannotRetry(t *testing.T) {
	f := newOperationsFixture(t, nil)
	id := uuid.NewString()
	_, err := f.ops.update(f.c, "second", func(s *operationState) error {
		s.LastResetID = id
		s.History[id] = operationEntry{OperationReceipt: OperationReceipt{RequestID: id, State: "running", At: f.now}, Kind: "reset", Owner: f.ops.owner}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	newProcess := NewOperations(f.ops.svc)
	report, err := newProcess.Report(f.c.ID, "second")
	if err != nil || report.LastReset.State != "unknown" {
		t.Fatalf("restart: %+v %v", report, err)
	}
	if _, err := newProcess.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, uuid.NewString()); !errors.Is(err, store.ErrCredentialActionBusy) {
		t.Fatalf("restarted running replayed: %v", err)
	}
	if _, err := newProcess.Acknowledge(f.c.ID, "second", f.identity, "reset"); err != nil {
		t.Fatal(err)
	}
	if f.consumes.Load() != 0 {
		t.Fatal("restart/ack sent POST")
	}
}

func TestOperationsConcurrentActionsAndConfigEditPreserveReceipt(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newOperationsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	cfg := f.warmupConfig()
	f.save(t, cfg)
	id := uuid.NewString()
	done := make(chan error, 1)
	go func() { _, err := f.ops.Warmup(context.Background(), f.c.ID, "second", f.identity, id); done <- err }()
	<-entered
	if _, err := f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, uuid.NewString()); !errors.Is(err, store.ErrCredentialActionBusy) {
		t.Fatalf("concurrent reset admitted: %v", err)
	}
	report, err := f.ops.Warmup(context.Background(), f.c.ID, "second", f.identity, id)
	if err != nil || report.LastWarmup.State != "running" {
		t.Fatalf("running duplicate: %+v %v", report, err)
	}
	cfg.Warmup.DailyTimes = []string{"11:37"}
	f.save(t, cfg)
	if err := f.st.SetCredentialEnabled(f.c.ID, "second", false); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	report, err = f.ops.Report(f.c.ID, "second")
	if err != nil || len(report.Config.Warmup.DailyTimes) != 1 || report.Config.Warmup.DailyTimes[0] != "11:37" || report.LastWarmup.State != "succeeded" || f.warmups.Load() != 1 {
		t.Fatalf("completion overwrote config or lost receipt: %+v %v", report, err)
	}
}

func TestOperationsAcceptedResetReadFailureDoesNotBecomeFailed(t *testing.T) {
	f := newOperationsFixture(t, nil)
	f.readStatus.Store(500)
	id := uuid.NewString()
	report, err := f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, id)
	if err != nil || report.LastReset.State != "succeeded" || !strings.Contains(report.LastReset.Detail, "刷新") || report.ResetCardsError == "" {
		t.Fatalf("reconciliation: %+v %v", report, err)
	}
	if _, err = f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, id); err != nil || f.consumes.Load() != 1 {
		t.Fatal("failed read caused replay")
	}
}

func TestOperationsDailyClaimsPersistAndDoNotCatchUp(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	cfg.Warmup.Mode = "daily"
	cfg.Warmup.DailyTimes = []string{f.now.Format("15:04")}
	f.save(t, cfg)
	f.ops.tick(context.Background(), f.now)
	f.ops.tick(context.Background(), f.now.Add(20*time.Second))
	newProcess := NewOperations(f.ops.svc)
	newProcess.now = f.ops.now
	newProcess.tick(context.Background(), f.now)
	if f.warmups.Load() != 1 || f.reads.Load() != 1 {
		t.Fatal("daily repeated or queried unnecessary quota")
	}
	f.now = f.now.AddDate(0, 0, 1).Add(time.Hour)
	newProcess.tick(context.Background(), f.now)
	if f.warmups.Load() != 1 {
		t.Fatal("daily caught up a past time")
	}
	f.now = f.now.Add(-time.Hour)
	newProcess.tick(context.Background(), f.now)
	if f.warmups.Load() != 2 {
		t.Fatal("next day did not trigger")
	}
}

func TestOperationsStaleScheduledConfigAndPastMinuteCannotStart(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	cfg.Warmup.Mode = "daily"
	cfg.Warmup.DailyTimes = []string{f.now.Format("15:04")}
	f.save(t, cfg)
	old, _ := f.st.Channel(f.c.ID)
	off := cfg
	off.Warmup.Enabled = false
	f.save(t, off)
	f.ops.tickCredential(context.Background(), old, "second", f.identity, cfg, f.now, time.Minute)
	if f.warmups.Load() != 0 {
		t.Fatal("stale daily configuration started an action after disabled")
	}
	f.save(t, cfg)
	past := f.now
	f.now = f.now.Add(time.Minute)
	current, _ := f.st.Channel(f.c.ID)
	f.ops.tickCredential(context.Background(), current, "second", f.identity, cfg, past, time.Minute)
	if f.warmups.Load() != 0 {
		t.Fatal("queued daily action caught up a past minute")
	}
}

func TestOperationsNaturalRecoveryPersistsAndClaimsOnce(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	f.save(t, cfg)
	f.percent.Store(100)
	f.ops.tick(context.Background(), f.now)
	if f.warmups.Load() != 0 {
		t.Fatal("exhaustion immediately preheated")
	}
	newProcess := NewOperations(f.ops.svc)
	newProcess.now = f.ops.now
	f.percent.Store(0)
	f.now = f.now.Add(time.Minute)
	newProcess.tick(context.Background(), f.now)
	if f.warmups.Load() != 0 {
		t.Fatal("recovered quota before latest reset time triggered")
	}
	f.now = f.now.Add(time.Minute)
	newProcess.tick(context.Background(), f.now)
	if f.warmups.Load() != 1 {
		t.Fatal("persisted natural reset did not preheat")
	}
	f.now = f.now.Add(time.Minute)
	newProcess.tick(context.Background(), f.now)
	if f.warmups.Load() != 1 {
		t.Fatal("same natural recovery preheated twice")
	}
}

func TestOperationsExpiredCardRuleDoesNotStarveNaturalRecovery(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	days := 1.0
	cfg.AutoReset.Enabled = true
	cfg.AutoReset.Rules = []store.CredentialResetRule{{ExpiresWithinDays: &days, Join: "and"}}
	f.save(t, cfg)
	// A prior reset already claimed this continuously matching card condition.
	_, err := f.ops.update(f.c, "second", func(s *operationState) error { s.ResetLatched = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	f.percent.Store(100)
	f.ops.tick(context.Background(), f.now)
	f.percent.Store(0)
	f.now = f.now.Add(2 * time.Minute)
	f.ops.tick(context.Background(), f.now)
	if f.consumes.Load() != 0 || f.warmups.Load() != 1 {
		t.Fatal("latched card condition hid natural recovery or consumed again")
	}
}

func TestOperationsAutomaticResetLatchSurvivesRestartAndUnknownReads(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	n := 20.0
	cfg.AutoReset.Enabled = true
	cfg.AutoReset.Rules = []store.CredentialResetRule{{WindowSeconds: 18000, RemainingBelow: &n, Join: "and"}}
	f.save(t, cfg)
	f.percent.Store(90)
	f.ops.tick(context.Background(), f.now)
	f.ops.tick(context.Background(), f.now.Add(30*time.Second))
	if f.consumes.Load() != 1 {
		t.Fatalf("first match: %d", f.consumes.Load())
	}
	newProcess := NewOperations(f.ops.svc)
	newProcess.now = f.ops.now
	newProcess.tick(context.Background(), f.now.Add(time.Minute))
	if f.consumes.Load() != 1 {
		t.Fatal("restart cleared reset latch")
	}
	f.readStatus.Store(500)
	newProcess.tick(context.Background(), f.now.Add(2*time.Minute))
	f.readStatus.Store(0)
	newProcess.tick(context.Background(), f.now.Add(3*time.Minute))
	if f.consumes.Load() != 1 {
		t.Fatal("unknown observation rearmed latch")
	}
	f.percent.Store(50)
	newProcess.tick(context.Background(), f.now.Add(4*time.Minute))
	f.percent.Store(90)
	newProcess.tick(context.Background(), f.now.Add(5*time.Minute))
	if f.consumes.Load() != 2 {
		t.Fatal("new low-quota situation did not rearm")
	}
}

func TestOperationsAutomaticResetRequiresAvailableCardAndEnabledIdentity(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	n := 20.0
	cfg.AutoReset.Enabled = true
	cfg.AutoReset.Rules = []store.CredentialResetRule{{WindowSeconds: 18000, RemainingBelow: &n, Join: "and"}}
	f.save(t, cfg)
	f.percent.Store(90)
	f.cardCount.Store(0)
	f.ops.tick(context.Background(), f.now)
	if f.consumes.Load() != 0 {
		t.Fatal("no-card automatic reset sent POST")
	}
	if err := f.st.SetCredentialEnabled(f.c.ID, "second", false); err != nil {
		t.Fatal(err)
	}
	before := f.reads.Load() + f.cards.Load()
	f.ops.tick(context.Background(), f.now.Add(time.Minute))
	if f.reads.Load()+f.cards.Load() != before {
		t.Fatal("disabled credential queried upstream")
	}
	current, _ := f.st.Channel(f.c.ID)
	current.Credentials[1].Enabled = true
	current.Credentials[1].OAuth.Account = "replacement"
	if err := f.st.SaveChannel(&current); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ops.ConsumeReset(context.Background(), f.c.ID, "second", f.identity, uuid.NewString()); !errors.Is(err, store.ErrCredentialIdentityChanged) {
		t.Fatalf("stale identity accepted: %v", err)
	}
}

func TestOperationsAutomaticReadsUseConfiguredCadence(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	n := 20.0
	cfg.AutoReset.Enabled = true
	cfg.AutoReset.Rules = []store.CredentialResetRule{{WindowSeconds: 18000, RemainingBelow: &n, Join: "and"}}
	f.save(t, cfg)
	f.ops.tick(context.Background(), f.now)
	f.ops.tick(context.Background(), f.now.Add(30*time.Second))
	f.ops.tick(context.Background(), f.now.Add(59*time.Second))
	if f.reads.Load() != 1 || f.cards.Load() != 1 {
		t.Fatal("manual quota mode polled more often than 60 seconds")
	}
	f.ops.tick(context.Background(), f.now.Add(time.Minute))
	if f.reads.Load() != 2 {
		t.Fatal("manual quota mode did not autonomously refresh")
	}
	settings, _ := f.st.GetSettings()
	interval := 300
	settings.QuotaRefreshSeconds = &interval
	if err := f.st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	f.ops.tick(context.Background(), f.now.Add(359*time.Second))
	if f.reads.Load() != 2 {
		t.Fatal("configured quota interval ignored")
	}
	f.ops.tick(context.Background(), f.now.Add(360*time.Second))
	if f.reads.Load() != 3 {
		t.Fatal("configured quota interval did not refresh")
	}
}

func TestOperationsReenabledWarmupDoesNotAdoptOldWatch(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	f.save(t, cfg)
	f.percent.Store(100)
	f.ops.tick(context.Background(), f.now)
	cfg.Warmup.Enabled = false
	f.save(t, cfg)
	cfg.Warmup.Enabled = true
	f.save(t, cfg)
	f.percent.Store(0)
	f.now = f.now.Add(2 * time.Minute)
	f.resetAt.Store(f.now.Add(5 * time.Hour).Unix())
	f.ops.tick(context.Background(), f.now)
	if f.warmups.Load() != 0 {
		t.Fatal("re-enabled plan adopted its old exhaustion watch")
	}
}

func TestOperationsRunCancellationWaitsForReceipt(t *testing.T) {
	entered := make(chan struct{})
	f := newOperationsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	})
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	cfg.Warmup.Mode = "daily"
	cfg.Warmup.DailyTimes = []string{f.now.Format("15:04")}
	f.save(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.ops.Run(ctx); close(done) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background action did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not wait for cancelled action")
	}
	report, err := f.ops.Report(f.c.ID, "second")
	if err != nil || report.LastWarmup.State != "unknown" || f.warmups.Load() != 1 {
		t.Fatalf("shutdown receipt: %+v %v", report, err)
	}
}

func usageWithWindows(percent *float64, seconds int64, reset int64) Usage {
	return Usage{Buckets: []UsageBucket{{General: true, Primary: &UsageWindow{UsedPercent: percent, WindowSeconds: &seconds, ResetAt: &reset}}}}
}

func TestOperationsNaturalResetUsesFiveHourDeadlineAndWaitsForLongQuota(t *testing.T) {
	now := time.Unix(1800000000, 0)
	used, zero := 100.0, 0.0
	week := int64(604800)
	early, late := now.Unix()+60, now.Unix()+120
	u := usageWithWindows(&zero, 18000, early) // The five-hour allowance need not be exhausted.
	u.Buckets[0].Secondary = &UsageWindow{UsedPercent: &used, WindowSeconds: &week, ResetAt: &late}
	reset, due, known := naturalWarmupTime(u)
	if !known || reset != early || due != late {
		t.Fatal("long depleted window did not postpone preheat")
	}
	u.Buckets[0].Secondary.UsedPercent = nil
	if _, _, known = naturalWarmupTime(u); known {
		t.Fatal("unknown long window treated as available")
	}
	u.Buckets[0].Secondary.UsedPercent = &zero
	if reset, due, known = naturalWarmupTime(u); !known || due != early || reset != early {
		t.Fatal("available long window unnecessarily postponed preheat")
	}
	weekly := usageWithWindows(&zero, week, early)
	if _, _, known = naturalWarmupTime(weekly); known || dailyWarmupAvailable(weekly, now) {
		t.Fatal("weekly-only credential accepted for automatic preheat")
	}
	u.Buckets[0].Primary.ResetAt = nil
	if _, _, known = naturalWarmupTime(u); known {
		t.Fatal("unknown reset time invented a new window")
	}
}

func TestOperationsResetRulesThreeValuedConditions(t *testing.T) {
	now := time.Unix(1800000000, 0)
	used := 90.0
	threshold, days := 20.0, 1.0
	expiry := now.Add(12 * time.Hour).Unix()
	count := 1
	u := usageWithWindows(&used, 18000, now.Unix()+60)
	cards := ResetCredits{AvailableCount: &count, Credits: []ResetCredit{{Status: "available", Available: true, ExpiresAt: &expiry}}}
	rule := store.CredentialResetRule{WindowSeconds: 18000, RemainingBelow: &threshold, ExpiresWithinDays: &days, Join: "and"}
	if match, known := resetRulesMatch([]store.CredentialResetRule{rule}, u, cards, now); !match || !known {
		t.Fatal("AND condition failed")
	}
	rule.WindowSeconds = 604800
	if match, known := resetRulesMatch([]store.CredentialResetRule{rule}, u, cards, now); match || known {
		t.Fatal("missing specified window guessed another window")
	}
	rule.Join = "or"
	if match, known := resetRulesMatch([]store.CredentialResetRule{rule}, u, cards, now); !match || !known {
		t.Fatal("known OR true did not win")
	}
	threshold = 10
	rule = store.CredentialResetRule{WindowSeconds: 18000, RemainingBelow: &threshold, Join: "and"}
	if match, known := resetRulesMatch([]store.CredentialResetRule{rule}, u, cards, now); match || !known {
		t.Fatal("remaining threshold was not strict")
	}
	threshold = 20
	rules := []store.CredentialResetRule{{WindowSeconds: 604800, RemainingBelow: &threshold, Join: "and"}, rule}
	if match, known := resetRulesMatch(rules, u, cards, now); !match || !known {
		t.Fatal("groups did not combine with OR")
	}
	rule = store.CredentialResetRule{ExpiresWithinDays: &days, Join: "and"}
	if match, known := resetRulesMatch([]store.CredentialResetRule{rule}, u, ResetCredits{}, now); match || known {
		t.Fatal("unknown cards inferred an empty list")
	}
	if !hasAvailableResetCredit(cards, now) {
		t.Fatal("valid available credit lost")
	}
	cards.Credits[0].RedeemedAt = &expiry
	if hasAvailableResetCredit(cards, now) {
		t.Fatal("redeemed card usable")
	}
	raw, _ := json.Marshal(operationState{ResetLatched: true})
	state, err := decodeOperationState(raw)
	if err != nil || !state.ResetLatched {
		t.Fatal("state did not survive JSON storage")
	}
}
