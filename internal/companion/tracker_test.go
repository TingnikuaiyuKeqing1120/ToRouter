package companion

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReportedUsageRetryAndTerminalLifecycle(t *testing.T) {
	tm := time.Unix(1000, 0)
	tracker := New()
	tracker.now = func() time.Time { return tm }
	r := tracker.Begin("model", true, tm)
	r.Attempt(1, "primary")
	if a := tracker.Snapshot().Active[0]; a.OutputTokens != nil || a.FirstByteMS != nil || a.TokensPerSecond != nil {
		t.Fatal("missing measurements must stay null")
	}
	tm = tm.Add(100 * time.Millisecond)
	r.FirstByte()
	r.Output(0)
	tm = tm.Add(time.Second)
	r.Output(10)
	a := tracker.Snapshot().Active[0]
	if *a.OutputTokens != 10 || *a.FirstByteMS != 100 || *a.TokensPerSecond != 10 {
		t.Fatalf("reported metrics: %+v", a)
	}
	*a.OutputTokens = 900
	if *tracker.Snapshot().Active[0].OutputTokens != 10 {
		t.Fatal("active snapshot aliases mutable state")
	}
	r.Retrying()
	r.Attempt(2, "backup")
	a = tracker.Snapshot().Active[0]
	if a.Attempt != 2 || a.Phase != "retrying" || a.OutputTokens != nil || a.FirstByteMS != nil {
		t.Fatalf("attempt reset: %+v", a)
	}
	r.FirstByte()
	tm = tm.Add(time.Second)
	r.Output(5)
	r.Finish(200, "success", "")
	r.Finish(500, "error", "secret diagnostic")
	s := tracker.Snapshot()
	if s.ActiveCount != 0 || len(s.Recent) != 1 || s.Sequence != 1 || s.Recent[0].Outcome != "success" || *s.Recent[0].OutputTokens != 5 {
		t.Fatalf("final lifecycle: %+v", s)
	}
	tm = tm.Add(31 * time.Second)
	if len(tracker.Snapshot().Recent) != 0 {
		t.Fatal("expired events remained visible")
	}
}

func TestTextEstimateCalibratesAndResumesAfterUsage(t *testing.T) {
	tm := time.Unix(3000, 0)
	tracker := New()
	tracker.now = func() time.Time { return tm }
	r := tracker.Begin("model", true, tm)
	r.Attempt(1, "channel")
	r.FirstByte()
	tm = tm.Add(time.Second)
	check := func(tokens int64, source string) {
		t.Helper()
		a := tracker.Snapshot().Active[0]
		if a.OutputTokens == nil || *a.OutputTokens != tokens || a.OutputTokensSource != source || a.TokensPerSecond == nil || *a.TokensPerSecond != float64(tokens) {
			t.Fatalf("want %d %s, got %+v", tokens, source, a)
		}
	}
	r.TextUnits(20)
	check(5, "estimated")
	r.Output(1)
	check(1, "reported")
	r.TextUnits(1)
	check(2, "estimated")
	r.TextUnits(3)
	check(2, "estimated")
	r.TextUnits(4)
	check(3, "estimated")
	r.Output(0)
	check(0, "reported")
	r.TextUnits(5)
	check(2, "estimated")
	r.Output(-1)
	r.TextUnits(-1)
	check(2, "estimated")
	r.Attempt(2, "backup")
	a := tracker.Snapshot().Active[0]
	if a.OutputTokens != nil || a.OutputTokensSource != "" || a.FirstByteMS != nil || a.TokensPerSecond != nil {
		t.Fatalf("retry retained metrics: %+v", a)
	}
	r.TextUnits(1)
	r.Finish(200, "success", "")
	done := tracker.Snapshot().Recent[0]
	if done.OutputTokens == nil || *done.OutputTokens != 1 || done.OutputTokensSource != "estimated" || done.TokensPerSecond != nil {
		t.Fatalf("unreported final estimate/reset: %+v", done)
	}
}

func TestTextEstimateIsChunkIndependentAndContainsNoText(t *testing.T) {
	tracker := New()
	whole := tracker.Begin("whole", true, time.Now())
	split := tracker.Begin("split", true, time.Now())
	whole.TextUnits(17)
	for range 17 {
		split.TextUnits(1)
	}
	s := tracker.Snapshot()
	if *s.Active[0].OutputTokens != 5 || *s.Active[1].OutputTokens != 5 {
		t.Fatalf("chunk-dependent rounding: %+v", s.Active)
	}
	// The observation API takes numeric units; its public JSON exposes no text
	// fields or internal estimate accumulator.
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "text") || strings.Contains(string(b), "units") {
		t.Fatalf("unexpected text/accumulator field: %s", b)
	}
	nonstream := tracker.Begin("nonstream", false, time.Now())
	nonstream.TextUnits(8)
	if a := tracker.Snapshot().Active[2]; a.OutputTokens != nil || a.OutputTokensSource != "" {
		t.Fatalf("nonstream invented estimate: %+v", a)
	}
}

func TestNonStreamingHasNoInventedSpeedAndErrorsAreSafe(t *testing.T) {
	tm := time.Unix(2000, 0)
	tracker := New()
	tracker.now = func() time.Time { return tm }
	r := tracker.Begin("model", false, tm)
	r.Attempt(1, "channel")
	tm = tm.Add(3 * time.Second)
	r.Output(40)
	r.FirstByte()
	tm = tm.Add(time.Second)
	r.Finish(500, "error", "url with a secret")
	done := tracker.Snapshot().Recent[0]
	if done.TokensPerSecond != nil || done.ErrorCode != "upstream_error" {
		t.Fatalf("unsafe/invented result: %+v", done)
	}
}

func TestConcurrentTrackingBoundsVisibleRowsAndKeepsTotal(t *testing.T) {
	tracker := New()
	requests := make([]*Request, 80)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			requests[i] = tracker.Begin(fmt.Sprint(i), true, time.Now())
			requests[i].Attempt(1, "channel")
			requests[i].FirstByte()
			requests[i].Output(int64(i))
			_ = tracker.Snapshot()
		}(i)
	}
	wg.Wait()
	s := tracker.Snapshot()
	if s.ActiveCount != 80 || len(s.Active) != 16 {
		t.Fatalf("active bounds/count: %+v", s)
	}
	for _, r := range requests {
		wg.Add(1)
		go func(r *Request) { defer wg.Done(); r.Finish(499, "cancelled", "client_cancelled") }(r)
	}
	wg.Wait()
	s = tracker.Snapshot()
	if s.ActiveCount != 0 || len(s.Recent) != 32 || s.Sequence != 80 {
		t.Fatalf("completed bounds/count: %+v", s)
	}
}
