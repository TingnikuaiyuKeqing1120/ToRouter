package gateway

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"torouter/internal/companion"
	"torouter/internal/store"
)

func TestCompanionTextUnitsProtocolFields(t *testing.T) {
	cases := []struct {
		name, format, data string
		want               int64
	}{
		{"chat text", store.FormatOpenAIChatCompletions, `{"choices":[{"delta":{"content":"abcd中"}},{"delta":{"content":"文"}}]}`, 12},
		{"chat reasoning and refusal", store.FormatOpenAIChatCompletions, `{"choices":[{"delta":{"refusal":"no","reasoning_content":"思","reasoning":"想"}}]}`, 10},
		{"chat tools", store.FormatOpenAIChatCompletions, `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"ignored","arguments":"{}"}}],"function_call":{"arguments":"null"}}}]}`, 6},
		{"chat complete body", store.FormatOpenAIChatCompletions, `{"choices":[{"message":{"content":"ignored"}}]}`, 0},
		{"anthropic text", store.FormatAnthropicMessages, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"abcd中"}}`, 8},
		{"anthropic thinking", store.FormatAnthropicMessages, `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"秘密"}}`, 8},
		{"anthropic tool", store.FormatAnthropicMessages, `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`, 2},
		{"anthropic initial text", store.FormatAnthropicMessages, `{"type":"content_block_start","content_block":{"type":"text","text":"hi"}}`, 2},
		{"anthropic initial thinking", store.FormatAnthropicMessages, `{"type":"content_block_start","content_block":{"type":"thinking","thinking":"想"}}`, 4},
		{"anthropic private fields", store.FormatAnthropicMessages, `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"secret"}}`, 0},
		{"anthropic unknown block", store.FormatAnthropicMessages, `{"type":"content_block_delta","delta":{"type":"unknown","text":"ignored"}}`, 0},
		{"anthropic unknown event", store.FormatAnthropicMessages, `{"type":"unknown","delta":{"type":"text_delta","text":"ignored"}}`, 0},
		{"anthropic redacted", store.FormatAnthropicMessages, `{"type":"content_block_start","content_block":{"type":"redacted_thinking","data":"secret"}}`, 0},
		{"gemini", store.FormatGeminiGenerateContent, `{"candidates":[{"content":{"parts":[{"text":"hi"},{"text":"想","thought":true},{"thoughtSignature":"secret"},{"inlineData":{"data":"secret"}}]}}]}`, 6},
		{"unknown format", "unknown", `{"choices":[{"delta":{"content":"ignored"}}]}`, 0},
		{"responses final text", store.FormatOpenAIResponses, `{"type":"response.output_text.done","text":"ignored"}`, 0},
		{"responses encrypted", store.FormatOpenAIResponses, `{"type":"response.reasoning.delta","delta":{"encrypted_content":"ignored"}}`, 0},
		{"heartbeat", store.FormatAnthropicMessages, `{"type":"ping","text":"ignored"}`, 0},
		{"invalid text type", store.FormatOpenAIChatCompletions, `{"choices":[{"delta":{"content":{"text":"ignored"}}}]}`, 0},
	}
	for _, typ := range []string{"response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta"} {
		cases = append(cases, struct {
			name, format, data string
			want               int64
		}{typ, store.FormatOpenAIResponses, `{"type":"` + typ + `","delta":"abc中"}`, 7})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.data), &obj); err != nil {
				t.Fatal(err)
			}
			if got := companionTextUnits(obj, tc.format); got != tc.want {
				t.Fatalf("units=%d want %d", got, tc.want)
			}
		})
	}
}

func TestCompanionObserverEstimateCalibrationAndPassthrough(t *testing.T) {
	tracker := companion.New()
	r := tracker.Begin("model", true, time.Now())
	u := store.Usage{}
	wire := ": heartbeat\r\n\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"abcd中\"}}],\"usage\":{\"prompt_tokens\":10}}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"completion_tokens\":1}}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n" +
		"data: [DONE]\n\n"
	o := newObserver(iotest.OneByteReader(strings.NewReader(wire)), store.FormatOpenAIChatCompletions, &u)
	var observed []companion.LiveRequest
	record := func() { observed = append(observed, tracker.Snapshot().Active[0]) }
	o.onTextUnits = func(n int64) { r.TextUnits(n); record() }
	o.onOutput = func(n int64) { r.Output(n); record() }
	got, err := io.ReadAll(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wire {
		t.Fatal("SSE bytes changed")
	}
	if !o.complete || len(observed) != 4 {
		t.Fatalf("events=%+v complete=%v", observed, o.complete)
	}
	for i, want := range []int64{2, 4, 1, 3} {
		if observed[i].OutputTokens == nil || *observed[i].OutputTokens != want {
			t.Fatalf("event %d got %+v want %d", i, observed[i], want)
		}
	}
	if observed[2].OutputTokensSource != "reported" || observed[3].OutputTokensSource != "estimated" {
		t.Fatal("usage did not calibrate source")
	}
	if u.InputTokens != 10 || u.OutputTokens != 1 {
		t.Fatalf("estimate contaminated billing: %+v", u)
	}
	r.Finish(200, "success", "")
	done := tracker.Snapshot().Recent[0]
	if done.OutputTokensSource != "estimated" || *done.OutputTokens != 3 {
		t.Fatalf("final estimate lost: %+v", done)
	}
	b, _ := json.Marshal(tracker.Snapshot())
	for _, secret := range []string{"abcd", "hello", "world"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("snapshot retained response text")
		}
	}
}

func TestCompanionObserverInputOnlyAndInitialAnthropicUsage(t *testing.T) {
	tracker := companion.New()
	r := tracker.Begin("model", true, time.Now())
	u := store.Usage{}
	o := newObserver(nil, store.FormatAnthropicMessages, &u)
	o.onOutput, o.onTextUnits = r.Output, r.TextUnits
	if err := o.observe([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`)); err != nil {
		t.Fatal(err)
	}
	if got := tracker.Snapshot().Active[0]; got.OutputTokens != nil || got.OutputTokensSource != "" {
		t.Fatalf("input-only invented output: %+v", got)
	}
	for _, data := range []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"abcdefgh"}}`,
	} {
		if err := o.observe([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if got := tracker.Snapshot().Active[0]; *got.OutputTokens != 3 || got.OutputTokensSource != "estimated" {
		t.Fatalf("initial count froze estimate: %+v", got)
	}
	if err := o.observe([]byte(`{"type":"message_delta","usage":{"output_tokens":2}}`)); err != nil {
		t.Fatal(err)
	}
	if got := tracker.Snapshot().Active[0]; *got.OutputTokens != 2 || got.OutputTokensSource != "reported" {
		t.Fatalf("final usage failed to calibrate: %+v", got)
	}
}

func TestCompanionObserverTextSplitsPreserveEstimate(t *testing.T) {
	for _, chunks := range [][]string{{"abc中de文f"}, {"a", "b", "c", "中", "d", "e", "文", "f"}} {
		tracker := companion.New()
		r := tracker.Begin("model", true, time.Now())
		u := store.Usage{}
		o := newObserver(nil, store.FormatOpenAIResponses, &u)
		o.onTextUnits = r.TextUnits
		for _, chunk := range chunks {
			data, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": chunk})
			if err := o.observe(data); err != nil {
				t.Fatal(err)
			}
		}
		got := tracker.Snapshot().Active[0]
		if *got.OutputTokens != 4 || got.OutputTokensSource != "estimated" || u.OutputTokens != 0 {
			t.Fatalf("split estimate: %+v usage=%+v", got, u)
		}
	}
}
