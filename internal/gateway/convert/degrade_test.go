package convert

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// T119 supersedes T014: preserve reasoning by default; explicitly record
// authorized loss when a target cannot express opaque state.

func TestResponseDropsAnthropicThinking(t *testing.T) {
	body := `{"id":"m1","type":"message","role":"assistant","model":"claude-x","content":[` +
		`{"type":"thinking","thinking":"secret reasoning","signature":"sig"},` +
		`{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`
	out, dropped, err := ResponseWithOptions([]byte(body), Anthropic, OpenAI, "m", Options{AllowReasoningDrop: true})
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "reasoning_state_dropped" {
		t.Fatalf("dropped = %v", dropped)
	}
	if strings.Contains(string(out), "secret reasoning") || strings.Contains(string(out), "sig") {
		t.Fatalf("reasoning leaked: %s", out)
	}
	if !strings.Contains(string(out), "answer") {
		t.Fatalf("answer lost: %s", out)
	}
}

func TestResponseDropsOmittedThinkingBlock(t *testing.T) {
	// display:"omitted" leaves an empty thinking field; the signature still
	// makes it a carrier the target cannot express.
	body := `{"id":"m1","type":"message","role":"assistant","model":"claude-x","content":[` +
		`{"type":"thinking","thinking":"","signature":"sig"},` +
		`{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`
	_, dropped, err := ResponseWithOptions([]byte(body), Anthropic, OpenAI, "m", Options{AllowReasoningDrop: true})
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "reasoning_state_dropped" {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestResponseRefusesSignedToolCall(t *testing.T) {
	// A signed functionCall is continuation state: the client must send the
	// signature back next turn or the upstream rejects it. Stripping it would
	// hand back a tool call that can never be validly continued, so the whole
	// conversion is refused instead of degraded.
	body := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"text":"done"},` +
		`{"functionCall":{"name":"f","args":{"a":1}},"thoughtSignature":"sig"}]},"finishReason":"STOP"}]}`
	if _, _, err := ResponseWithInfo([]byte(body), Gemini, OpenAI, "m"); err == nil {
		t.Fatal("signed tool call must not be silently stripped")
	} else if !errors.Is(err, ErrContinuationState) {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestResponseConvertsUnsignedToolCall(t *testing.T) {
	// Without a signature there is no continuation obligation, so the tool
	// call converts normally.
	body := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"f","args":{"a":1}}}]},"finishReason":"STOP"}]}`
	out, dropped, err := ResponseWithInfo([]byte(body), Gemini, OpenAI, "m")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
	if !strings.Contains(string(out), `"f"`) {
		t.Fatalf("tool call lost: %s", out)
	}
}

func TestRequestAttachesGemini3Bypass(t *testing.T) {
	// Cross-protocol tool history aimed at Gemini 3 gets the provider
	// documented dummy signature, and the adaptation is reported so the
	// gateway can record it.
	body := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`
	out, adapted, err := RequestWithInfo([]byte(body), OpenAI, Gemini, "gemini-3-pro")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(adapted) != 1 || adapted[0] != "thought_signature_bypass" {
		t.Fatalf("adapted = %v", adapted)
	}
	// Assert the real shape, not a substring: the signature must sit on the
	// part next to functionCall, and our own decoder must accept what we built.
	var wire struct {
		Contents []struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	var part map[string]json.RawMessage
	for _, c := range wire.Contents {
		for _, p := range c.Parts {
			if _, ok := p["functionCall"]; ok {
				part = p
			}
		}
	}
	if part == nil {
		t.Fatalf("no functionCall part: %s", out)
	}
	if _, ok := part["thoughtSignature"]; !ok {
		t.Fatalf("signature not on the part: %s", out)
	}
	var call map[string]json.RawMessage
	if err := json.Unmarshal(part["functionCall"], &call); err != nil {
		t.Fatal(err)
	}
	if _, ok := call["thoughtSignature"]; ok {
		t.Fatalf("signature nested inside functionCall: %s", out)
	}
	// A request that arrives carrying a thoughtSignature is state and is still
	// refused on the way in; the decoder is deliberately strict about that, so
	// there is no round trip to assert here — the shape checks above are the
	// contract.
	// Parallel calls: only the first call of a step carries the signature.
	body2 := `{"messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},` +
		`{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]}]}`
	out2, _, err := RequestWithInfo([]byte(body2), OpenAI, Gemini, "gemini-3-pro")
	if err != nil {
		t.Fatalf("parallel conversion failed: %v", err)
	}
	var wire2 struct {
		Contents []struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(out2, &wire2); err != nil {
		t.Fatal(err)
	}
	signed := 0
	for _, c := range wire2.Contents {
		for _, p := range c.Parts {
			if _, ok := p["thoughtSignature"]; ok {
				signed++
			}
		}
	}
	if signed != 1 {
		t.Fatalf("expected exactly one signature for parallel calls, got %d: %s", signed, out2)
	}
	// A Gemini 2.x target gets no bypass.
	_, adapted, err = RequestWithInfo([]byte(body), OpenAI, Gemini, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(adapted) != 0 || strings.Contains(string(mustJSON(t, body, OpenAI, Gemini, "gemini-2.5-flash")), "skip_thought") {
		t.Fatalf("unexpected bypass for 2.x: %v", adapted)
	}
}

func mustJSON(t *testing.T, body, from, to, model string) []byte {
	t.Helper()
	out, _, err := RequestWithInfo([]byte(body), from, to, model)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestResponsePreservesGeminiThoughtPart(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"text":"inner monologue","thought":true},` +
		`{"text":"answer"}]},"finishReason":"STOP"}]}`
	out, dropped, err := ResponseWithInfo([]byte(body), Gemini, OpenAI, "m")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
	s := string(out)
	if !strings.Contains(s, `"reasoning_content":"inner monologue"`) {
		t.Fatalf("thought text was not preserved: %s", s)
	}
	if !strings.Contains(s, "answer") {
		t.Fatalf("answer lost: %s", s)
	}
}

func TestResponsePreservesOpenAIReasoningContent(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":` +
		`{"role":"assistant","content":"answer","reasoning_content":"plain reasoning"},"finish_reason":"stop"}]}`
	out, dropped, err := ResponseWithInfo([]byte(body), OpenAI, Anthropic, "m")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
	s := string(out)
	if strings.Contains(s, "plain reasoning") {
		t.Fatalf("reasoning leaked: %s", s)
	}
	if !strings.Contains(s, attachmentPrefix) || !strings.Contains(s, "answer") {
		t.Fatalf("answer lost: %s", s)
	}
}

func TestStreamDropsAnthropicThinking(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hidden\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var out strings.Builder
	dropped, err := StreamWithOptions(strings.NewReader(body), &out, Anthropic, OpenAI, "m", Options{AllowReasoningDrop: true})
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "reasoning_state_dropped" {
		t.Fatalf("dropped = %v", dropped)
	}
	s := out.String()
	if strings.Contains(s, "hidden") || strings.Contains(s, "sig\"") {
		t.Fatalf("reasoning leaked: %s", s)
	}
	if !strings.Contains(s, "answer") || !strings.Contains(s, "[DONE]") {
		t.Fatalf("output broken: %s", s)
	}
}

func TestStreamPreservesOpenAIReasoningContent(t *testing.T) {
	body := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"hidden\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var out strings.Builder
	dropped, err := StreamWithInfo(strings.NewReader(body), &out, OpenAI, Anthropic, "m")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
	s := out.String()
	if strings.Contains(s, "hidden") {
		t.Fatalf("reasoning leaked: %s", s)
	}
	if !strings.Contains(s, attachmentPrefix) || !strings.Contains(s, "answer") || !strings.Contains(s, "message_stop") {
		t.Fatalf("output broken: %s", s)
	}
}

// The request side stays strict: carriers in a client request are state.
func TestRequestPreservesPlaintextRejectsUnrepresentableOpaque(t *testing.T) {
	cases := []struct{ p, b string }{
		{OpenAI, `{"messages":[{"role":"assistant","content":"x","reasoning_content":"s"}]}`},
		{Anthropic, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"s"}]}]}`},
		{Anthropic, `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"s"}]}]}`},
		{Gemini, `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"x","args":{}},"thoughtSignature":"s"}]}]}`},
		{Gemini, `{"contents":[{"role":"model","parts":[{"text":"t","thought":true}]}]}`},
	}
	for i, c := range cases {
		to := Gemini
		if c.p == Gemini {
			to = OpenAI
		}
		if _, err := Request([]byte(c.b), c.p, to, "m"); (err == nil) != (i == 0 || i == 4) {
			t.Errorf("case %d incorrect reasoning preservation decision", i)
		}
	}
}

func TestStreamReportsDegradationInline(t *testing.T) {
	// The comment must appear in the stream itself. Checking for it after the
	// stream ends never fires, because end() always calls start().
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"sig\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var out strings.Builder
	dropped, err := StreamWithOptions(strings.NewReader(body), &out, Anthropic, OpenAI, "m", Options{AllowReasoningDrop: true})
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "reasoning_state_dropped" {
		t.Fatalf("dropped = %v", dropped)
	}
	got := out.String()
	if !strings.Contains(got, ":x-to-router-degraded: reasoning_state_dropped") {
		t.Fatalf("no inline degradation comment:\n%s", got)
	}
	if !strings.Contains(got, "answer") || !strings.Contains(got, "[DONE]") {
		t.Fatalf("output broken:\n%s", got)
	}
}

// A streamed tool call that follows a stripped reasoning carrier must be
// refused, not handed over: the client could never legally continue it.
func TestStreamPreservesPlainReasoningWithToolCall(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"hidden\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hidden\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"f\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var out strings.Builder
	_, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, OpenAI, "m")
	if err != nil {
		t.Fatalf("expected refusal, got %v", err)
	}
	if !strings.Contains(out.String(), "tool_calls") || !strings.Contains(out.String(), "reasoning_content") {
		t.Fatalf("plaintext reasoning or tool call was lost:\n%s", out.String())
	}
}

// A tool call with no reasoning in front of it streams normally.
func TestStreamAllowsToolCallWithoutReasoning(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"f\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var out strings.Builder
	if _, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, OpenAI, "m"); err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if !strings.Contains(out.String(), "tool_calls") {
		t.Fatalf("tool call missing:\n%s", out.String())
	}
}

// T023: the provider-documented dummy signature belongs to the gateway→upstream
// request path only. A tool call streamed back to the client must not wear it,
// and the conversion must not book an adaptation it did not perform.
func TestStreamToGeminiCarriesNoBypass(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"f\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var out strings.Builder
	dropped, err := StreamWithInfo(strings.NewReader(body), &out, Anthropic, Gemini, "gemini-3-pro")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if strings.Contains(out.String(), thoughtSignatureBypass) {
		t.Fatalf("bypass signature in a client stream:\n%s", out.String())
	}
	if len(dropped) != 0 {
		t.Fatalf("client direction booked an adaptation: %v", dropped)
	}
	// Assert the real shape, not a substring: the call survives and the part
	// carries no signature, the way a real Gemini 3 response would.
	var parts []map[string]json.RawMessage
	for _, line := range strings.Split(out.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var frame struct {
			Candidates []struct {
				Content struct {
					Parts []map[string]json.RawMessage `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatal(err)
		}
		for _, c := range frame.Candidates {
			parts = append(parts, c.Content.Parts...)
		}
	}
	if len(parts) != 1 {
		t.Fatalf("expected one part, got %d:\n%s", len(parts), out.String())
	}
	if _, ok := parts[0]["functionCall"]; !ok {
		t.Fatalf("functionCall lost: %v", parts[0])
	}
	if _, ok := parts[0]["thoughtSignature"]; ok {
		t.Fatalf("signature on the part: %v", parts[0])
	}
	// The request direction keeps the bypass, unchanged by this fix.
	_, adapted, err := RequestWithInfo([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`), OpenAI, Gemini, "gemini-3-pro")
	if err != nil {
		t.Fatalf("request conversion failed: %v", err)
	}
	if len(adapted) != 1 || adapted[0] != "thought_signature_bypass" {
		t.Fatalf("adapted = %v", adapted)
	}
}
