package convert

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// T025: a cross-protocol tool call must reach the client atomically. Anthropic
// guarantees no global ordering — a thinking block can legally appear between
// tool calls — and a Gemini thought signature may ride a part that arrives
// later. Both converters therefore hold every tool call until the upstream
// message boundary and veto the whole batch when a reasoning carrier was
// stripped anywhere in the message: the client receives either a continuable
// session or an error, never half of one.

// ev builds one Anthropic SSE event; Gemini streams are bare data frames.
func ev(event, data string) string { return "event: " + event + "\ndata: " + data + "\n\n" }

func textBlock(index int, text string) string {
	return ev("content_block_start", `{"type":"content_block_start","index":`+strconv.Itoa(index)+`,"content_block":{"type":"text","text":""}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":`+strconv.Itoa(index)+`,"delta":{"type":"text_delta","text":"`+text+`"}}`) +
		ev("content_block_stop", `{"type":"content_block_stop","index":`+strconv.Itoa(index)+`}`)
}

// toolBlock renders one Anthropic tool_use block: start, argument deltas, stop.
func toolBlock(index int, id, name string, args ...string) string {
	var b strings.Builder
	b.WriteString(ev("content_block_start", `{"type":"content_block_start","index":`+strconv.Itoa(index)+`,"content_block":{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":{}}}`))
	for _, a := range args {
		b.WriteString(ev("content_block_delta", `{"type":"content_block_delta","index":`+strconv.Itoa(index)+`,"delta":{"type":"input_json_delta","partial_json":"`+a+`"}}`))
	}
	b.WriteString(ev("content_block_stop", `{"type":"content_block_stop","index":`+strconv.Itoa(index)+`}`))
	return b.String()
}

// thinkingBlock renders one Anthropic thinking block. A nonempty signature adds
// the signature_delta that turns the block into a continuation carrier.
func thinkingBlock(index int, signature string) string {
	var b strings.Builder
	b.WriteString(ev("content_block_start", `{"type":"content_block_start","index":`+strconv.Itoa(index)+`,"content_block":{"type":"thinking","thinking":""}}`))
	b.WriteString(ev("content_block_delta", `{"type":"content_block_delta","index":`+strconv.Itoa(index)+`,"delta":{"type":"thinking_delta","thinking":"hidden"}}`))
	if signature != "" {
		b.WriteString(ev("content_block_delta", `{"type":"content_block_delta","index":`+strconv.Itoa(index)+`,"delta":{"type":"signature_delta","signature":"`+signature+`"}}`))
	}
	b.WriteString(ev("content_block_stop", `{"type":"content_block_stop","index":`+strconv.Itoa(index)+`}`))
	return b.String()
}

// anthropicMessage wraps content blocks into a complete message lifecycle.
func anthropicMessage(finish string, blocks ...string) string {
	return ev("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"c","content":[],"usage":{"input_tokens":70,"output_tokens":0}}}`) +
		strings.Join(blocks, "") +
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+finish+`"},"usage":{"output_tokens":20}}`) +
		ev("message_stop", `{"type":"message_stop"}`)
}

func assertNoToolCall(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "tool_calls") || strings.Contains(out, "functionCall") {
		t.Fatalf("uncontinuable tool call was emitted:\n%s", out)
	}
}

func TestStreamAnthropicToolCallVetoedByLaterThinking(t *testing.T) {
	// The shape Anthropic documents as legal: thinking between tool calls. The
	// call waits for message_stop and the later block vetoes the whole message.
	body := anthropicMessage("tool_use",
		thinkingBlock(0, ""),
		toolBlock(1, "t1", "f", `{\"a\":1}`),
		thinkingBlock(2, "sig"),
	)
	var out strings.Builder
	dropped, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, OpenAI, "m")
	if !errors.Is(err, ErrContinuationState) {
		t.Fatalf("expected refusal, got %v", err)
	}
	assertNoToolCall(t, out.String())
	if strings.Contains(out.String(), "t1") {
		t.Fatalf("tool id leaked:\n%s", out.String())
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestStreamAnthropicToolCallBeforeCarrierIsRefused(t *testing.T) {
	// T025 as filed: the tool call arrives first. Emitting it at its block stop
	// handed the client a call the later thinking block made uncontinuable.
	body := anthropicMessage("tool_use",
		toolBlock(0, "t1", "f", `{}`),
		thinkingBlock(1, "sig"),
	)
	var out strings.Builder
	dropped, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, OpenAI, "m")
	if !errors.Is(err, ErrContinuationState) {
		t.Fatalf("expected refusal, got %v", err)
	}
	assertNoToolCall(t, out.String())
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestStreamAnthropicBuffersEveryToolCall(t *testing.T) {
	// Two calls with a thinking block between them: the veto covers the whole
	// message, not only the blocks that follow the carrier.
	body := anthropicMessage("tool_use",
		toolBlock(0, "t1", "f", `{}`),
		thinkingBlock(1, "sig"),
		toolBlock(2, "t2", "g", `{}`),
	)
	var out strings.Builder
	dropped, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, OpenAI, "m")
	if !errors.Is(err, ErrContinuationState) {
		t.Fatalf("expected refusal, got %v", err)
	}
	assertNoToolCall(t, out.String())
	if s := out.String(); strings.Contains(s, "t1") || strings.Contains(s, "t2") {
		t.Fatalf("buffered tool call leaked:\n%s", s)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestStreamAnthropicSignedThinkingBeforeToolCallIsRefused(t *testing.T) {
	// Carrier before the call is the same strong-continuation state in the
	// other order (STATE-AND-LAYERS rule 2; the unsigned-thinking case is
	// covered by TestStreamRefusesToolCallAfterDroppedReasoning), so the
	// boundary must refuse here too rather than deliver an uncontinuable call.
	body := anthropicMessage("tool_use",
		thinkingBlock(0, "sig"),
		toolBlock(1, "t1", "f", `{}`),
	)
	var out strings.Builder
	if _, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, OpenAI, "m"); !errors.Is(err, ErrContinuationState) {
		t.Fatalf("expected refusal, got %v", err)
	}
	assertNoToolCall(t, out.String())
}

func TestStreamAnthropicToolCallDeliveredAtBoundary(t *testing.T) {
	// No carrier in the message: the call waits for message_stop and then goes
	// out with its content unchanged, after any text and before the finish.
	body := anthropicMessage("tool_use",
		textBlock(0, "Checking"),
		toolBlock(1, "call1", "weather", `{\"city\":`, `\"Paris\"}`),
	)
	var out strings.Builder
	if _, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, OpenAI, "m"); err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	got := decodeOpenAIStream(t, out.String())
	if len(got.calls) != 1 {
		t.Fatalf("expected one tool call, got %v:\n%s", got.calls, out.String())
	}
	if got.calls[0].ID != "call1" || got.calls[0].Name != "weather" || got.calls[0].Arguments != `{"city":"Paris"}` {
		t.Fatalf("tool call altered by buffering: %+v", got.calls[0])
	}
	if got.content != "Checking" {
		t.Fatalf("text lost: %q", got.content)
	}
	want := []string{"content", "tool_calls", "finish", "usage", "done"}
	if strings.Join(got.kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("chunk order = %v, want %v", got.kinds, want)
	}
	if got.finish != FinishToolCalls {
		t.Fatalf("finish = %q", got.finish)
	}
	if got.usage.Input != 70 || got.usage.Output != 20 {
		t.Fatalf("usage = %+v", got.usage)
	}
}

func TestStreamAnthropicTruncatedAfterToolCallEmitsNothing(t *testing.T) {
	// The message never reached its boundary, so no tool call may be handed
	// over: the client gets the truncation error, not half a conversation.
	body := ev("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"c","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`) +
		toolBlock(0, "t1", "f", `{}`)
	var out strings.Builder
	if _, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, OpenAI, "m"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("expected truncation, got %v", err)
	}
	assertNoToolCall(t, out.String())
}

func TestStreamGeminiFunctionCallVetoedByLaterSignature(t *testing.T) {
	// The signature rides a later part instead of the call's own part: the
	// call must wait for the candidate to end and then be vetoed.
	body := sse(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}}}]}}]}`) +
		sse(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"note","thoughtSignature":"sig"}]}}]}`) +
		sse(`{"candidates":[{"index":0,"finishReason":"STOP"}]}`)
	var out strings.Builder
	dropped, err := StreamWithInfo(strings.NewReader(body), &out, Gemini, OpenAI, "m")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected refusal, got %v", err)
	}
	assertNoToolCall(t, out.String())
	if strings.Contains(out.String(), "weather") {
		t.Fatalf("tool name leaked:\n%s", out.String())
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestStreamGeminiSignedFunctionCallStillRefused(t *testing.T) {
	// The documented Gemini shape: the signature rides the same part as the
	// call. A signed call is continuation state, so the boundary refuses it
	// exactly as the non-streaming path does.
	body := sse(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"sig"}]},"finishReason":"STOP"}]}`)
	var out strings.Builder
	dropped, err := StreamWithInfo(strings.NewReader(body), &out, Gemini, OpenAI, "m")
	if !errors.Is(err, ErrContinuationState) {
		t.Fatalf("expected refusal, got %v", err)
	}
	assertNoToolCall(t, out.String())
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestStreamGeminiFunctionCallDeliveredAtBoundary(t *testing.T) {
	// No carrier in the candidate: the call waits for the candidate to end and
	// then goes out with its content unchanged, after any text.
	var out strings.Builder
	if _, err := StreamWithInfo(strings.NewReader(sourceStream(Gemini)), &out, Gemini, OpenAI, "m"); err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	got := decodeOpenAIStream(t, out.String())
	if len(got.calls) != 1 || got.calls[0].Name != "weather" || got.calls[0].Arguments != `{"city":"Paris"}` {
		t.Fatalf("tool call altered by buffering: %+v", got.calls)
	}
	if got.content != "Checking" {
		t.Fatalf("text lost: %q", got.content)
	}
	want := []string{"content", "tool_calls", "finish", "usage", "done"}
	if strings.Join(got.kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("chunk order = %v, want %v", got.kinds, want)
	}
	if got.finish != FinishToolCalls {
		t.Fatalf("finish = %q", got.finish)
	}
	// Gemini input excludes cached tokens: 100 prompt minus 30 cached.
	if got.usage.Input != 70 || got.usage.Output != 20 || got.usage.CacheRead != 30 {
		t.Fatalf("usage = %+v", got.usage)
	}
}

func TestStreamGeminiThoughtPartWithoutToolCallStillStreams(t *testing.T) {
	// Reasoning without a tool call is degraded, not refused (T014): the
	// thought part is stripped and recorded, the answer goes out.
	body := sse(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"inner","thought":true},{"text":"answer"}]},"finishReason":"STOP"}]}`)
	var out strings.Builder
	dropped, err := StreamWithInfo(strings.NewReader(body), &out, Gemini, OpenAI, "m")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
	s := out.String()
	if !strings.Contains(s, `"reasoning_content":"inner"`) {
		t.Fatalf("reasoning was not preserved:\n%s", s)
	}
	if !strings.Contains(s, "answer") || !strings.Contains(s, "[DONE]") {
		t.Fatalf("output broken:\n%s", s)
	}
}

func TestStreamGeminiTruncatedAfterFunctionCallEmitsNothing(t *testing.T) {
	// No finishReason means no candidate end, so the held call is discarded
	// with the truncation error instead of being delivered.
	body := sse(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]}}]}`)
	var out strings.Builder
	if _, err := StreamWithInfo(strings.NewReader(body), &out, Gemini, OpenAI, "m"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("expected truncation, got %v", err)
	}
	assertNoToolCall(t, out.String())
}

// openAIStream is a decoded OpenAI SSE stream: the kinds of chunk it carried in
// arrival order plus the content, tool calls, finish reason and usage it ends
// with.
type openAIStream struct {
	kinds   []string
	content string
	calls   []irToolCall
	finish  string
	usage   irUsage
}

func decodeOpenAIStream(t *testing.T, s string) openAIStream {
	t.Helper()
	var out openAIStream
	for _, line := range strings.Split(s, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			out.kinds = append(out.kinds, "done")
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				out.kinds = append(out.kinds, "content")
				out.content += ch.Delta.Content
			}
			for _, tc := range ch.Delta.ToolCalls {
				out.kinds = append(out.kinds, "tool_calls")
				out.calls = append(out.calls, irToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			if ch.FinishReason != "" {
				out.kinds = append(out.kinds, "finish")
				out.finish = ch.FinishReason
			}
		}
		if c.Usage != nil {
			out.kinds = append(out.kinds, "usage")
			in, o, cr, _, ok := parseOpenAIUsage([]byte(data))
			if !ok {
				t.Fatalf("usage chunk without usage: %s", data)
			}
			out.usage = irUsage{Input: in, Output: o, CacheRead: cr, HasInput: true, HasOutput: true, HasCacheRead: cr > 0}
		}
	}
	return out
}
