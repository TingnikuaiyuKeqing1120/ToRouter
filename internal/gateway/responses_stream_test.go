package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestResponsesStreamTerminalSemantics(t *testing.T) {
	begin := sseEvent("response.created", `{"type":"response.created","response":{"id":"resp_native","status":"in_progress"}}`) +
		sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","delta":"partial answer"}`)
	cases := []struct {
		name, suffix, errorText, health string
		status                          int
		pin, nativeTerminal             bool
	}{
		{"completed", sseEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_native","status":"completed","usage":{"input_tokens":5,"output_tokens":2}}}`), "", "", 200, true, true},
		{"completed plus DONE", sseEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_native","status":"completed"}}`) + "data: [DONE]\n\n", "", "", 200, true, true},
		{"max output tokens", sseEvent("response.incomplete", `{"type":"response.incomplete","response":{"id":"resp_native","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":2}}}`), "upstream response incomplete: max_output_tokens", "half_open", 200, false, true},
		{"content filter", sseEvent("response.incomplete", `{"type":"response.incomplete","response":{"id":"resp_native","status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`), "upstream response incomplete: content_filter", "half_open", 200, false, true},
		{"unknown incomplete reason", sseEvent("response.incomplete", `{"type":"response.incomplete","response":{"id":"resp_native","status":"incomplete","incomplete_details":{"reason":"secret user payload"}}}`), "upstream response incomplete: unknown", "half_open", 200, false, true},
		{"failed", sseEvent("response.failed", `{"type":"response.failed","response":{"id":"resp_native","status":"failed","error":{"code":"server_error","message":"private payload"}}}`), "upstream response failed", "cooling", 502, false, true},
		{"explicit error", sseEvent("error", `{"type":"error","code":"server_error","message":"private payload"}`), "upstream stream error", "cooling", 502, false, true},
		{"EOF between events", "", "stream interrupted", "cooling", 502, false, false},
		{"EOF inside event", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":", "stream interrupted", "cooling", 502, false, false},
		{"DONE without Responses terminal", "data: [DONE]\n\n", "stream interrupted", "cooling", 502, false, false},
		{"other protocol terminal", sseEvent("message_stop", `{"type":"message_stop"}`), "stream interrupted", "cooling", 502, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 3)
			stream := begin + tc.suffix
			id, primary := addResponsesChannel(t, s, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, stream)
			}, 0)
			_, spare := addResponsesChannel(t, s, completedResponses, 1)
			// Two prior line failures make one new failure observable as a
			// cooldown. A logical incomplete result must leave them untouched;
			// only a fully completed response establishes success.
			for i := 0; i < 2; i++ {
				lease, _ := g.Breaker.Acquire(testCombination(t, g, id))
				g.Breaker.Failure(lease.WithRetryAfter(0))
			}
			body := `{"model":"test","stream":true,"input":"hello"}`
			w, r := responsesSessionTurn(g, "terminal-session", body)
			if w.Code != 200 || primary.Load() != 1 || spare.Load() != 0 {
				t.Fatalf("committed stream must never resend: HTTP=%d requests=%d/%d", w.Code, primary.Load(), spare.Load())
			}
			rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
			if err != nil || len(rows) != 1 || rows[0].Status != tc.status || rows[0].Error != tc.errorText {
				t.Fatalf("usage=%+v error=%v, want status=%d error=%q", rows, err, tc.status, tc.errorText)
			}
			key, _ := sessionKey(r, store.FormatOpenAIResponses, true, []byte(body), 0, "test")
			if _, pinned := g.affinity.lookup(key); pinned != tc.pin {
				t.Fatalf("learned pin=%v want=%v", pinned, tc.pin)
			}
			if state := healthPhase(g, id); state != tc.health {
				t.Fatalf("health=%q want=%q", state, tc.health)
			}
			if tc.nativeTerminal {
				if w.Body.String() != stream {
					t.Fatalf("native terminal changed or duplicate gateway error added: %q", w.Body.String())
				}
			} else if !strings.Contains(w.Body.String(), "interrupted") {
				t.Fatalf("EOF must report a stream interruption: %q", w.Body.String())
			}
			if tc.name == "max output tokens" && (rows[0].Estimated || rows[0].InputTokens != 5 || rows[0].OutputTokens != 2) {
				t.Fatalf("incomplete response's actual usage must survive: %+v", rows[0])
			}
		})
	}
}
