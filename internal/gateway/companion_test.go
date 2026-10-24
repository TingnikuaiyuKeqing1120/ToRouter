package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/companion"
	"torouter/internal/store"
)

func waitCompanion(t *testing.T, live *companion.Tracker, accept func(companion.Snapshot) bool) companion.Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v := live.Snapshot()
		if accept(v) {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("companion did not reach expected state")
	return companion.Snapshot{}
}

func TestCompanionObservesLiveStreamWithoutChangingBytesOrUsage(t *testing.T) {
	g, s := setup(t)
	g.Live = companion.New()
	steps := make(chan struct{}, 3)
	defer close(steps)
	parts := []string{
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7}}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"abcdefgh\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5}}\n\n",
		"data: [DONE]\n\n",
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, part := range parts {
			if i > 0 {
				select {
				case <-steps:
				case <-r.Context().Done():
					return
				}
			}
			io.WriteString(w, part)
			http.NewResponseController(w).Flush()
		}
	}))
	defer up.Close()
	add(t, s, up.URL, "openai", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		serveTestClient(g, w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true,"messages":[]}`)).WithContext(ctx))
		close(done)
	}()
	first := waitCompanion(t, g.Live, func(v companion.Snapshot) bool { return v.ActiveCount == 1 && v.Active[0].FirstByteMS != nil })
	if first.Active[0].OutputTokens != nil || first.Active[0].TokensPerSecond != nil {
		t.Fatal("input-only usage invented an output count")
	}
	steps <- struct{}{}
	estimate := waitCompanion(t, g.Live, func(v companion.Snapshot) bool {
		return v.ActiveCount == 1 && v.Active[0].OutputTokensSource == "estimated"
	})
	if *estimate.Active[0].OutputTokens != 2 {
		t.Fatalf("text estimate = %+v", estimate.Active[0])
	}
	steps <- struct{}{}
	reported := waitCompanion(t, g.Live, func(v companion.Snapshot) bool {
		return v.ActiveCount == 1 && v.Active[0].OutputTokensSource == "reported"
	})
	if *reported.Active[0].OutputTokens != 5 {
		t.Fatal("actual usage did not calibrate the estimate")
	}
	steps <- struct{}{}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not finish")
	}
	if w.Code != 200 || w.Body.String() != strings.Join(parts, "") {
		t.Fatal("observation changed response bytes/status")
	}
	final := g.Live.Snapshot()
	if final.ActiveCount != 0 || final.Sequence != 1 || len(final.Recent) != 1 || final.Recent[0].Outcome != "success" {
		t.Fatalf("final lifecycle = %+v", final)
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 1 || rows[0].InputTokens != 7 || rows[0].OutputTokens != 5 || rows[0].Estimated {
		t.Fatalf("persisted usage changed: %+v %v", rows, err)
	}
}

func TestCompanionRetryIsOneLogicalCompletion(t *testing.T) {
	g, s := setup(t)
	g.Live = companion.New()
	settings(t, s, 2, 2)
	var calls atomic.Int32
	release := make(chan struct{})
	defer close(release)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	}))
	defer up.Close()
	add(t, s, up.URL, "openai", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		serveTestClient(g, w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[]}`)).WithContext(ctx))
		close(done)
	}()
	waitCompanion(t, g.Live, func(v companion.Snapshot) bool {
		return v.ActiveCount == 1 && v.Active[0].Phase == "retrying" && v.Active[0].Attempt == 1 && v.Sequence == 0
	})
	v := waitCompanion(t, g.Live, func(v companion.Snapshot) bool { return v.ActiveCount == 1 && v.Active[0].Attempt == 2 })
	if v.Active[0].Phase != "retrying" || len(v.Recent) != 0 {
		t.Fatal("intermediate retry became a final failure")
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("retry did not complete")
	}
	v = g.Live.Snapshot()
	if w.Code != 200 || v.Sequence != 1 || len(v.Recent) != 1 || v.Recent[0].Outcome != "success" || v.Recent[0].TokensPerSecond != nil {
		t.Fatalf("bad retry outcome: %+v", v)
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("upstream attempts must remain separate: %+v %v", rows, err)
	}
	for _, row := range rows {
		if row.Status == 200 && row.Error != "" {
			t.Fatal("successful retry retained the previous attempt's error")
		}
		if row.Status == 429 && row.Error == "" {
			t.Fatal("resetting the retry removed the recorded failure")
		}
	}
}

func TestCompanionBackgroundResponsesAreAcceptedNotCompleted(t *testing.T) {
	for _, state := range []string{"queued", "in_progress"} {
		t.Run(state, func(t *testing.T) {
			g, s := setup(t)
			g.Live = companion.New()
			body := `{"id":"resp_background","object":"response","status":"` + state + `","output":[]}`
			addResponsesChannel(t, s, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }, 0)
			w := invoke(g, "/v1/responses", `{"model":"test","input":"hello","background":true}`)
			v := g.Live.Snapshot()
			if w.Code != 200 || w.Body.String() != body || v.ActiveCount != 0 || v.Sequence != 1 || len(v.Recent) != 1 || v.Recent[0].Outcome != "accepted" || v.Recent[0].ErrorCode != "" {
				t.Fatalf("background request misclassified: %+v", v)
			}
		})
	}
}

func TestCompanionNativeHTTP200FailureDoesNotCelebrate(t *testing.T) {
	for _, terminal := range []string{"failed", "incomplete"} {
		t.Run(terminal, func(t *testing.T) {
			g, s := setup(t)
			g.Live = companion.New()
			stream := sseEvent("response."+terminal, `{"type":"response.`+terminal+`","response":{"status":"`+terminal+`","error":{"message":"private stream text"}}}`)
			addResponsesChannel(t, s, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, stream)
			}, 0)
			w := invoke(g, "/v1/responses", `{"model":"test","stream":true,"input":"private request"}`)
			v := g.Live.Snapshot()
			if w.Code != 200 || w.Body.String() != stream || v.Sequence != 1 || len(v.Recent) != 1 || v.Recent[0].Outcome != "error" || strings.Contains(v.Recent[0].ErrorCode, "private") {
				t.Fatalf("native failure observation wrong: %+v", v)
			}
		})
	}
}

func TestCompanionClientCancellationClearsActiveWithoutSuccess(t *testing.T) {
	g, s := setup(t)
	g.Live = companion.New()
	entered := make(chan struct{})
	stop := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer func() { close(stop); up.Close() }()
	add(t, s, up.URL, "openai", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		serveTestClient(g, httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true,"messages":[]}`)).WithContext(ctx))
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	v := g.Live.Snapshot()
	if v.ActiveCount != 0 || v.Sequence != 1 || len(v.Recent) != 1 || v.Recent[0].Outcome != "cancelled" || v.Recent[0].Status != 499 {
		t.Fatalf("bad cancelled lifecycle: %+v", v)
	}
}
