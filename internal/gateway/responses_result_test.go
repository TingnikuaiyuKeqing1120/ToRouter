package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/plugins"
	"torouter/internal/store"
)

func TestResponsesJSONAndBufferedTerminalSemantics(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		cases := []struct {
			status, details, errorText, health string
			usageStatus                        int
			pin                                bool
		}{
			{"completed", "", "", "", 200, true},
			{"incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`, "upstream response incomplete: max_output_tokens", "half_open", 200, false},
			{"incomplete", `,"incomplete_details":{"reason":"content_filter"}`, "upstream response incomplete: content_filter", "half_open", 200, false},
			{"failed", `,"error":{"code":"server_error","message":"private payload"}`, "upstream response failed", "cooling", 502, false},
			{"cancelled", "", "upstream response cancelled", "half_open", 200, false},
			{"queued", "", "", "half_open", 200, false},
			{"in_progress", "", "", "half_open", 200, false},
		}
		for _, tc := range cases {
			if buffered && (tc.status == "cancelled" || tc.status == "queued" || tc.status == "in_progress") {
				// These are legal native JSON/background statuses, not SSE
				// terminal event types the SIWC profile can reassemble.
				continue
			}
			name := tc.status + tc.errorText + "/native-json"
			if buffered {
				name = tc.status + tc.errorText + "/siwc-buffered"
			}
			t.Run(name, func(t *testing.T) {
				g, s := setup(t)
				settings(t, s, 3, 3)
				result := `{"id":"resp_native","object":"response","status":"` + tc.status + `","output":[],"usage":{"input_tokens":5,"output_tokens":2}` + tc.details + `}`
				var primary atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primary.Add(1)
					if buffered {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, sseEvent("response."+tc.status, `{"type":"response.`+tc.status+`","response":`+result+`}`))
					} else {
						io.WriteString(w, result)
					}
				}))
				t.Cleanup(up.Close)
				var id int64
				if buffered {
					t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
					auth, _ := mockAuth(t, http.StatusOK)
					c := addChatGPTChannel(t, s, up.URL)
					if _, err := s.SetPluginCapability("chatgpt", "connect", true); err != nil {
						t.Fatal(err)
					}
					manager, err := plugins.NewInjected(s, nil, siwcService(t, s, auth.URL), nil)
					if err != nil {
						t.Fatal(err)
					}
					g = NewWithPlugins(s, manager)
					id = c.ID
				} else {
					addFormat(t, s, up.URL, store.FormatOpenAIResponses)
					id = channelIDByURL(t, s, up.URL)
				}
				_, spare := addResponsesChannel(t, s, completedResponses, 1)
				for i := 0; i < 2; i++ {
					lease, _ := g.Breaker.Acquire(testCombination(t, g, id))
					g.Breaker.Failure(lease.WithRetryAfter(0))
				}
				body := `{"model":"test","input":"hello"}`
				w, r := responsesSessionTurn(g, "result-session", body)
				if w.Code != 200 || strings.TrimSpace(w.Body.String()) != result || primary.Load() != 1 || spare.Load() != 0 {
					t.Errorf("native terminal must survive without resend: HTTP=%d requests=%d/%d body=%s", w.Code, primary.Load(), spare.Load(), w.Body.String())
				}
				rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
				if err != nil || len(rows) != 1 || rows[0].Status != tc.usageStatus || rows[0].Error != tc.errorText || rows[0].Estimated || rows[0].InputTokens != 5 || rows[0].OutputTokens != 2 {
					t.Errorf("usage=%+v error=%v", rows, err)
				}
				key, _ := sessionKey(r, store.FormatOpenAIResponses, true, []byte(body), 0, "test")
				if _, pinned := g.affinity.lookup(key); pinned != tc.pin {
					t.Errorf("learned pin=%v want=%v", pinned, tc.pin)
				}
				if health := healthPhase(g, id); health != tc.health {
					t.Errorf("health=%q want=%q", health, tc.health)
				}
			})
		}
	}
}

func TestChatGPTBufferRejectsUnterminatedFinalEvent(t *testing.T) {
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_native\",\"status\":\"completed\",\"output\":[]}}"
	if result, err := bufferChatGPT(strings.NewReader(stream), "text/event-stream"); err == nil {
		t.Fatalf("unterminated final event was assembled as success: %s", result)
	}
}
