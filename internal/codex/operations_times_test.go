package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestOperationsEnabledWarmupSurvivesFiveHourQualificationLifecycle(t *testing.T) {
	for _, mode := range []string{"reset", "daily"} {
		t.Run(mode, func(t *testing.T) {
			f := newOperationsFixture(t, nil)
			start := time.Date(2026, 10, 12, 8, 30, 0, 0, time.Local)
			f.now = start
			cfg := f.warmupConfig()
			cfg.Warmup.Enabled = true
			cfg.Warmup.Mode = mode
			cfg.Warmup.DailyTimes = []string{"08:30", "17:00"}
			f.save(t, cfg)
			ctx := context.Background()
			var expectedWarmups int32
			setUsage := func(plan string, five bool, reset *int64, weeklyUsed int, weeklyReset int64) {
				t.Helper()
				week := map[string]any{"used_percent": weeklyUsed, "limit_window_seconds": 604800, "reset_at": weeklyReset}
				rate := map[string]any{"primary_window": week}
				if five {
					window := map[string]any{"used_percent": 20, "limit_window_seconds": 18000}
					if reset != nil {
						window["reset_at"] = *reset
					}
					rate = map[string]any{"primary_window": window, "secondary_window": week}
				}
				body, err := json.Marshal(map[string]any{"plan_type": plan, "rate_limit": rate})
				if err != nil {
					t.Fatal(err)
				}
				f.usageBody.Store(string(body))
			}
			assertPreserved := func(plan string, five bool) {
				t.Helper()
				r, err := f.ops.Report(f.c.ID, "second")
				if err != nil || r.Identity != f.identity || !sameOperationConfig(r.Config, cfg) {
					t.Fatalf("configuration or identity changed: %+v, %v", r, err)
				}
				u, err := f.ops.svc.CachedUsage(f.c.ID, "second")
				if err != nil || !u.OK || u.Usage == nil || u.PlanType != plan || (generalWindows(*u.Usage)[18000] != nil) != five {
					t.Fatalf("unexpected live usage evidence: %+v, %v", u, err)
				}
				if got := f.warmups.Load(); got != expectedWarmups {
					t.Fatalf("at %s warmup POSTs=%d, want %d", f.now, got, expectedWarmups)
				}
			}
			restart := func() {
				// Recreate both process-owned services, retaining only the fixture's durable store.
				f.ops = NewOperations(NewService(f.st, f.ops.svc.Client, f.ops.svc.Endpoints))
				f.ops.now = func() time.Time { return f.now }
			}

			reset := start.Add(-time.Minute).Unix()
			setUsage("plus", true, &reset, 0, start.AddDate(0, 0, 7).Unix())
			f.ops.tick(ctx, f.now)
			expectedWarmups++
			assertPreserved("plus", true)

			// A different plan loses its five-hour window for three complete days.
			setUsage("free", false, nil, 0, start.AddDate(0, 0, 7).Unix())
			for day := 1; day <= 3; day++ {
				f.now = start.AddDate(0, 0, day)
				f.ops.tick(ctx, f.now)
				assertPreserved("free", false)
				if day == 2 {
					restart()
					assertPreserved("free", false)
				}
			}

			// Return after a missed daily point. A window with no reset time is valid
			// for the next daily point, but cannot start a natural-reset cycle.
			f.now = start.AddDate(0, 0, 4).Add(time.Minute)
			setUsage("pro", true, nil, 0, f.now.AddDate(0, 0, 7).Unix())
			reads := f.reads.Load()
			f.ops.tick(ctx, f.now)
			if mode == "daily" {
				if f.reads.Load() != reads {
					t.Fatal("daily mode polled outside a configured point")
				}
				if _, err := f.ops.svc.RefreshUsage(ctx, f.c.ID, "second", true); err != nil {
					t.Fatal(err)
				}
			}
			assertPreserved("pro", true)
			f.now = start.AddDate(0, 0, 4).Add(8*time.Hour + 30*time.Minute)
			f.ops.tick(ctx, f.now)
			if mode == "daily" {
				expectedWarmups++
			}
			assertPreserved("pro", true)

			// Restored qualification must still respect the exhausted long window.
			f.now = start.AddDate(0, 0, 5)
			reset = f.now.Add(-time.Minute).Unix()
			longReset := f.now.Add(time.Hour).Unix()
			setUsage("pro", true, &reset, 100, longReset)
			f.ops.tick(ctx, f.now)
			assertPreserved("pro", true)
			f.now = f.now.Add(time.Hour)
			f.ops.tick(ctx, f.now)
			if mode == "reset" {
				expectedWarmups++
			}
			assertPreserved("pro", true)
			f.now = start.AddDate(0, 0, 5).Add(8*time.Hour + 30*time.Minute)
			f.ops.tick(ctx, f.now)
			if mode == "daily" {
				expectedWarmups++
			}
			assertPreserved("pro", true)
			restart()
			f.ops.tick(ctx, f.now)
			assertPreserved("pro", true) // Neither claim repeats after restart.

			cfg.Warmup.Enabled = false
			if _, err := f.ops.PatchConfig(f.c.ID, "second", f.identity, store.CredentialOperationsPatch{Warmup: &cfg.Warmup}); err != nil {
				t.Fatal(err)
			}
			f.now = start.AddDate(0, 0, 6)
			reads = f.reads.Load()
			f.ops.tick(ctx, f.now)
			if f.reads.Load() != reads {
				t.Fatal("manually disabled warmup still polled")
			}
			assertPreserved("pro", true)

			cfg.Warmup.Enabled = true
			if _, err := f.ops.PatchConfig(f.c.ID, "second", f.identity, store.CredentialOperationsPatch{Warmup: &cfg.Warmup}); err != nil {
				t.Fatal(err)
			}
			reset = f.now.Add(-time.Minute).Unix()
			setUsage("pro", true, &reset, 0, f.now.AddDate(0, 0, 7).Unix())
			f.status.Store(http.StatusInternalServerError)
			f.ops.tick(ctx, f.now)
			expectedWarmups++
			r, err := f.ops.Report(f.c.ID, "second")
			if err != nil || r.LastWarmup == nil || r.LastWarmup.State != "unknown" {
				t.Fatalf("missing unknown receipt: %+v, %v", r, err)
			}
			unknownID := r.LastWarmup.RequestID
			f.status.Store(0)
			restart()
			f.now = start.AddDate(0, 0, 7)
			reset = f.now.Add(-time.Minute).Unix()
			setUsage("pro", true, &reset, 0, f.now.AddDate(0, 0, 7).Unix())
			f.ops.tick(ctx, f.now)
			assertPreserved("pro", true) // A later due window cannot bypass unknown outcome.
			r, err = f.ops.Report(f.c.ID, "second")
			if err != nil || r.LastWarmup == nil || r.LastWarmup.RequestID != unknownID || r.LastWarmup.State != "unknown" {
				t.Fatalf("unknown receipt changed after restart: %+v, %v", r, err)
			}
		})
	}
}

func TestOperationsMultipleDailyTimesSurviveRestartIndividually(t *testing.T) {
	f := newOperationsFixture(t, nil)
	f.now = time.Date(2026, 10, 18, 8, 30, 0, 0, time.Local)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	cfg.Warmup.Mode = "daily"
	cfg.Warmup.DailyTimes = []string{"08:30", "17:00"}
	f.save(t, cfg)
	f.ops.tick(context.Background(), f.now)
	f.ops.tick(context.Background(), f.now)
	if f.warmups.Load() != 1 {
		t.Fatal("morning point not claimed once")
	}
	restarted := NewOperations(f.ops.svc)
	restarted.now = f.ops.now
	restarted.tick(context.Background(), f.now)
	f.now = f.now.Add(8*time.Hour + 30*time.Minute)
	restarted.tick(context.Background(), f.now)
	restarted.tick(context.Background(), f.now)
	if f.warmups.Load() != 2 {
		t.Fatalf("independent evening point count=%d", f.warmups.Load())
	}
	f.now = f.now.AddDate(0, 0, 1).Add(time.Minute)
	restarted.tick(context.Background(), f.now)
	if f.warmups.Load() != 2 {
		t.Fatal("missed daily time caught up later")
	}
}

func TestOperationsUnspentFiveHourWindowStartsNextWindowOnce(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	f.save(t, cfg)
	f.percent.Store(20)
	settings, _ := f.st.GetSettings()
	interval := 300
	settings.QuotaRefreshSeconds = &interval
	f.st.SaveSettings(settings)
	f.ops.tick(context.Background(), f.now)
	f.now = f.now.Add(110 * time.Second) // The known deadline overrides a longer refresh cadence.
	f.ops.tick(context.Background(), f.now)
	if f.warmups.Load() != 1 {
		t.Fatal("unspent expired window did not start next cycle")
	}
	restarted := NewOperations(f.ops.svc)
	restarted.now = f.ops.now
	restarted.tick(context.Background(), f.now.Add(6*time.Minute))
	if f.warmups.Load() != 1 {
		t.Fatal("same window repeated after restart")
	}
	f.resetAt.Store(f.now.Add(5 * time.Hour).Unix())
	f.now = f.now.Add(5*time.Hour + time.Second)
	restarted.tick(context.Background(), f.now)
	if f.warmups.Load() != 2 {
		t.Fatal("second five-hour cycle did not start")
	}
}

func TestOperationsFiveHourQualificationRecheckedAtDailyExecution(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	cfg.Warmup.Mode = "daily"
	cfg.Warmup.DailyTimes = []string{f.now.Format("15:04")}
	f.save(t, cfg)
	transport := f.ops.svc.Client.Transport
	f.ops.svc.Client = &http.Client{Transport: actionRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/usage" {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%d}}}`, f.now.Add(24*time.Hour).Unix()))), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})}
	f.ops.tick(context.Background(), f.now)
	if f.warmups.Load() != 0 {
		t.Fatal("weekly-only account preheated automatically")
	}
	if _, err := f.ops.PatchConfig(f.c.ID, "second", f.identity, store.CredentialOperationsPatch{Warmup: &cfg.Warmup}); err == nil {
		t.Fatal("missing five-hour qualification accepted at save")
	}
	cfg.Warmup.Enabled = false
	if _, err := f.ops.PatchConfig(f.c.ID, "second", f.identity, store.CredentialOperationsPatch{Warmup: &cfg.Warmup}); err != nil {
		t.Fatal("could not disable unsupported existing automation", err)
	}
}

func TestOperationsUnavailableDailyWarmupDoesNotBlockExpiryReset(t *testing.T) {
	f := newOperationsFixture(t, nil)
	cfg := f.warmupConfig()
	cfg.Warmup.Enabled = true
	cfg.Warmup.Mode = "daily"
	cfg.Warmup.DailyTimes = []string{f.now.Format("15:04")}
	days := 1.0
	cfg.AutoReset = store.CredentialAutoResetConfig{Enabled: true, Rules: []store.CredentialResetRule{{ExpiresWithinDays: &days, Join: "or"}}}
	f.save(t, cfg)
	transport := f.ops.svc.Client.Transport
	f.ops.svc.Client = &http.Client{Transport: actionRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/usage" {
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable")), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})}
	f.ops.tick(context.Background(), f.now)
	if f.warmups.Load() != 0 || f.consumes.Load() != 1 {
		t.Fatal("unavailable daily warmup suppressed independent expiry rule")
	}
}
