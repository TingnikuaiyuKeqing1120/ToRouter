package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"torouter/internal/accessguard"
)

func TestOperationsRunUntilStopsWhileIdle(t *testing.T) {
	f := newOperationsFixture(t, nil)
	f.ops.tickInterval = time.Hour
	started := make(chan struct{}, 1)
	f.ops.now = func() time.Time { started <- struct{}{}; return f.now }
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { f.ops.RunUntil(context.Background(), stop); close(done) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not wake idle ticker wait")
	}
	if f.reads.Load()+f.cards.Load()+f.warmups.Load()+f.consumes.Load() != 0 {
		t.Fatal("disabled operation rules performed network work")
	}
}

func TestOperationsRunUntilStopPreservesInFlightResponseAndReceipt(t *testing.T) {
	entered, release, cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f := newOperationsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		select {
		case <-release:
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		case <-r.Context().Done():
			close(cancelled)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(release)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled, cfg.Warmup.Mode = true, "daily"
	cfg.Warmup.DailyTimes = []string{f.now.Format("15:04")}
	f.save(t, cfg)
	f.ops.tickInterval = time.Millisecond
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { f.ops.RunUntil(ctx, stop); close(done) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled action did not start")
	}
	close(stop)
	select {
	case <-done:
		t.Fatal("worker returned before HTTP response and receipt completed")
	case <-cancelled:
		t.Fatal("stop cancelled in-flight HTTP")
	case <-time.After(25 * time.Millisecond):
	}
	// Send rather than close so the deferred close also cleans up failed tests.
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after released response")
	}
	report, err := f.ops.Report(f.c.ID, "second")
	if err != nil || report.LastWarmup == nil || report.LastWarmup.State != "succeeded" || f.warmups.Load() != 1 || f.reads.Load() != 1 {
		t.Fatalf("stop lost receipt or started another tick: %+v %v reads=%d warmups=%d", report, err, f.reads.Load(), f.warmups.Load())
	}
	select {
	case <-cancelled:
		t.Fatal("request was cancelled before it returned")
	default:
	}
}

func TestOperationsTickRejectsExpiredAccessBeforeNetwork(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled, cfg.Warmup.Mode = true, "daily"
	cfg.Warmup.DailyTimes = []string{f.now.Format("15:04")}
	f.save(t, cfg)
	ctx := accessguard.With(context.Background(), func() error { return errors.New("fixture epoch expired") })
	f.ops.tick(ctx, f.now)
	if f.reads.Load()+f.cards.Load()+f.warmups.Load()+f.consumes.Load() != 0 {
		t.Fatal("expired epoch started network work")
	}
}
