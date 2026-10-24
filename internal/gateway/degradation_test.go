package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestDegradationMergeIsASetInDirectionOrder(t *testing.T) {
	got := mergeDegradations("thought_signature_bypass,siwc_omit:temperature", []string{"thought_signature", "thought_signature_bypass", "thought_signature", ""})
	want := []string{"thought_signature_bypass", "siwc_omit:temperature", "thought_signature"}
	if !reflect.DeepEqual(strings.Split(got, ","), want) {
		t.Fatalf("merged=%q want=%v", got, want)
	}
}

func TestRequestAndResponseDegradationsAreRetained(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, losses := range []string{"neither", "request", "response", "both"} {
			name := losses + "/nonstream"
			if streaming {
				name = losses + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				g, s := setup(t)
				settings(t, s, 3, 1)
				policy, _ := s.GetSettings()
				policy.ReasoningPolicy = store.ReasoningAvailability
				if err := s.SaveSettings(policy); err != nil {
					t.Fatal(err)
				}
				requestLoss := losses == "request" || losses == "both"
				responseLoss := losses == "response" || losses == "both"
				var outbound string
				parts := `{"text":"answer"}`
				if responseLoss {
					parts = `{"text":"answer","thoughtSignature":"synthetic-signature"}`
				}
				response := `{"candidates":[{"content":{"role":"model","parts":[` + parts + `]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					outbound = string(data)
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: "+response+"\n\n")
					} else {
						io.WriteString(w, response)
					}
				}))
				t.Cleanup(up.Close)
				c := store.Channel{Name: "gemini", APIKey: "synthetic", Models: []string{"gemini-3-pro"}, Enabled: true,
					Endpoints: []store.Endpoint{{Format: store.FormatGeminiGenerateContent, BaseURL: up.URL}}}
				if err := s.SaveChannel(&c); err != nil {
					t.Fatal(err)
				}
				if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "gemini-3-pro", Enabled: true}}}); err != nil {
					t.Fatal(err)
				}
				messages := `[{"role":"user","content":"hello"}]`
				if requestLoss {
					messages = `[{"role":"user","content":"hello"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"first","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"second","arguments":"{}"}}]}]`
				}
				body, err := json.Marshal(map[string]any{"model": "test", "stream": streaming, "messages": json.RawMessage(messages)})
				if err != nil {
					t.Fatal(err)
				}
				w := invoke(g, "/v1/chat/completions", string(body))
				if w.Code != 200 || strings.Contains(w.Body.String(), "interrupted") {
					t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
				}
				if strings.Contains(outbound, "skip_thought_signature_validator") != requestLoss {
					t.Fatalf("request adaptation=%v want=%v", strings.Contains(outbound, "skip_thought_signature_validator"), requestLoss)
				}
				want := []string{}
				if requestLoss {
					want = append(want, "thought_signature_bypass")
				}
				if responseLoss {
					want = append(want, "reasoning_state_dropped")
				}
				rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
				if err != nil || len(rows) != 1 || rows[0].Degraded != strings.Join(want, ",") {
					t.Fatalf("recorded losses=%+v error=%v want=%q", rows, err, strings.Join(want, ","))
				}
				if !streaming && w.Header().Get("X-To-Router-Degraded") != strings.Join(want, ",") {
					t.Fatalf("response header must describe response loss: %q", w.Header().Get("X-To-Router-Degraded"))
				}
			})
		}
	}
}
