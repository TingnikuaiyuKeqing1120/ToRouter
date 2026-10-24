package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"torouter/internal/store"
)

func TestLocalOperationsManageWithoutService(t *testing.T) {
	st := actionTestStore(t)
	c := actionTestChannel(t, st, "fixture")
	local := NewLocalOperations(st)
	report, err := local.Report(c.ID, "second")
	if err != nil || !report.Supported || local.svc != nil {
		t.Fatalf("local report: %+v %v", report, err)
	}
	used := 25.0
	usage := usageWithWindows(&used, 18000, time.Now().Add(time.Hour).Unix())
	usage.CapturedAt = time.Now().UTC()
	raw, _ := json.Marshal(usage)
	if err := st.SaveQuotaCache(c, "second", store.QuotaCache{Snapshot: raw}); err != nil {
		t.Fatal(err)
	}
	warmup := store.DefaultCredentialOperationsConfig().Warmup
	warmup.Model, warmup.Enabled = "gpt-5-codex", true
	report, err = local.PatchConfig(c.ID, "second", report.Identity, store.CredentialOperationsPatch{Warmup: &warmup})
	if err != nil || !report.Config.Warmup.Enabled {
		t.Fatalf("local cached-usage configuration: %+v %v", report, err)
	}
	requestID := uuid.NewString()
	if _, err := local.update(c, "second", func(s *operationState) error {
		s.LastResetID = requestID
		s.History[requestID] = operationEntry{OperationReceipt: OperationReceipt{RequestID: requestID, State: "unknown", At: time.Now()}, Kind: "reset", Owner: local.owner}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report, err = local.Acknowledge(c.ID, "second", report.Identity, "reset")
	if err != nil || report.LastReset == nil || report.LastReset.State != "acknowledged" {
		t.Fatalf("local acknowledge: %+v %v", report, err)
	}
	before, err := st.LoadCredentialOperationState(c.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, network := range []func() (OperationReport, error){
		func() (OperationReport, error) { return local.RefreshCards(ctx, c.ID, "second", report.Identity) },
		func() (OperationReport, error) {
			return local.Warmup(ctx, c.ID, "second", report.Identity, uuid.NewString())
		},
		func() (OperationReport, error) {
			return local.ConsumeReset(ctx, c.ID, "second", report.Identity, uuid.NewString())
		},
	} {
		if _, err := network(); !errors.Is(err, errOperationsUnavailable) {
			t.Fatalf("local network action did not reject unloaded service: %v", err)
		}
	}
	local.tick(ctx, time.Now())
	local.tickCredential(ctx, c, "second", report.Identity, operationConfig(c, "second"), time.Now(), time.Minute)
	done := make(chan struct{})
	go func() { local.Run(ctx); local.RunUntil(ctx, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("local worker did not return without a service")
	}
	after, err := st.LoadCredentialOperationState(c.ID, "second")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("local network method claimed or changed an action: %v", err)
	}
}

func TestOperationsWithServicePreservesRunningOwnerInLocalView(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newOperationsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	defer close(release)
	f.save(t, f.warmupConfig())
	local := NewLocalOperations(f.st)
	local.now = func() time.Time { return f.now }
	local.tickInterval = time.Hour
	bound := local.WithService(f.ops.svc)
	if local.svc != nil || bound.svc != f.ops.svc || bound.store != local.store || bound.owner != local.owner || bound.now() != local.now() || bound.tickInterval != local.tickInterval {
		t.Fatal("WithService mutated the local view or lost its owner/store/clock")
	}
	done := make(chan error, 1)
	go func() {
		_, err := bound.Warmup(context.Background(), f.c.ID, "second", f.identity, uuid.NewString())
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("bound action ended before HTTP: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("bound action did not start")
	}
	report, err := local.Report(f.c.ID, "second")
	if err != nil || report.LastWarmup == nil || report.LastWarmup.State != "running" {
		t.Fatalf("local view misclassified the shared running owner: %+v %v", report, err)
	}
	if _, err := local.Acknowledge(f.c.ID, "second", f.identity, "warmup"); !errors.Is(err, store.ErrInvalidCredentialOperation) {
		t.Fatalf("local view acknowledged an in-process running action: %v", err)
	}
	release <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bound action did not finish")
	}
	report, err = local.Report(f.c.ID, "second")
	if err != nil || report.LastWarmup == nil || report.LastWarmup.State != "succeeded" {
		t.Fatalf("local view lost completed shared receipt: %+v %v", report, err)
	}
}
