package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

const keepAllThinkingContext = `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`

func withContextManagement(t *testing.T, body []byte, context string) []byte {
	t.Helper()
	var root object
	if err := strictJSON(body, &root); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := strictJSON([]byte(context), &value); err != nil {
		t.Fatal(err)
	}
	root["context_management"] = value
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnthropicKeepAllThinkingContextDoesNotChangeConversion(t *testing.T) {
	body := fourFixtureRequest(t, Anthropic)
	for _, to := range []string{OpenAI, Gemini, Responses} {
		t.Run(to, func(t *testing.T) {
			baseline, drops, err := RequestWithInfo(body, Anthropic, to, "m")
			if err != nil || len(drops) != 0 {
				t.Fatalf("baseline: %v %v", err, drops)
			}
			out, drops, err := RequestWithInfo(withContextManagement(t, body, keepAllThinkingContext), Anthropic, to, "m")
			if err != nil || len(drops) != 0 {
				t.Fatalf("keep-all conversion: %v %v", err, drops)
			}
			if !bytes.Equal(out, baseline) {
				t.Fatalf("keep-all changed messages, tools or results:\n%s\n%s", out, baseline)
			}
		})
	}
}

func TestAnthropicKeepAllContextPreservesEveryReasoningAttachment(t *testing.T) {
	parts := []irPart{
		{Thought: true, Reasoning: &irReasoning{Format: Responses, Value: object{"type": "reasoning", "id": "reason-first", "summary": []any{}, "encrypted_content": "opaque-first"}}},
		{Thought: true, Reasoning: &irReasoning{Format: Responses, Value: object{"type": "reasoning", "id": "reason-second", "summary": []any{}, "encrypted_content": "opaque-second"}}},
		{Tool: &irToolCall{ID: "call-original", Name: "f", Arguments: `{"literal":"all tool material"}`}},
	}
	opts := Options{Scope: "synthetic-scope", SigningKey: []byte("synthetic-key")}
	source, err := encodeResponse(&irResponse{ID: "r", Parts: parts, Finish: FinishToolCalls}, Responses, "m")
	if err != nil {
		t.Fatal(err)
	}
	wire, drops, err := ResponseWithOptions(source, Responses, Anthropic, "m", opts)
	if err != nil || len(drops) != 0 {
		t.Fatalf("client response: %v %v", err, drops)
	}
	decoded, err := decodeResponse(wire, Anthropic)
	if err != nil {
		t.Fatal(err)
	}
	request := requestFromResponseParts(t, decoded.Parts, Anthropic)
	baseline, _, err := RequestWithOptions(request, Anthropic, Responses, "m", opts)
	if err != nil {
		t.Fatal(err)
	}
	out, drops, err := RequestWithOptions(withContextManagement(t, request, keepAllThinkingContext), Anthropic, Responses, "m", opts)
	if err != nil || len(drops) != 0 || !bytes.Equal(out, baseline) {
		t.Fatalf("keep-all changed attachment/tool history: %v %v\n%s", err, drops, out)
	}
	restored, err := decodeRequest(out, Responses)
	if err != nil {
		t.Fatal(err)
	}
	assistant := restored.Messages[1].Parts
	if len(assistant) != 3 || assistant[0].Reasoning == nil || assistant[0].Reasoning.Value["encrypted_content"] != "opaque-first" || assistant[1].Reasoning == nil || assistant[1].Reasoning.Value["encrypted_content"] != "opaque-second" || assistant[2].Tool == nil || assistant[2].Tool.ID != "call-original" || assistant[2].Tool.Arguments != `{"literal":"all tool material"}` {
		t.Fatalf("reasoning or original tool changed: %s", out)
	}
}

func TestAnthropicContextManagementRejectsOtherStrategies(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"keep exact input"}]}`)
	for _, context := range []string{
		`{"edits":[]}`,
		`{"edits":[{"type":"clear_thinking_20251015","keep":1}]}`,
		`{"edits":[{"type":"clear_thinking_20251015","keep":0}]}`,
		`{"edits":[{"type":"clear_thinking_20251015","keep":"last"}]}`,
		`{"edits":[{"type":"clear_thinking_20251015"}]}`,
		`{"edits":[{"type":"clear_tool_uses_20250919","keep":"all"}]}`,
		`{"edits":[{"type":"unknown_edit","keep":"all"}]}`,
		`{"edits":[{"type":"clear_thinking_20251015","keep":"all","unknown":true}]}`,
		`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}],"unknown":true}`,
		`{"edits":[{"type":"clear_thinking_20251015","keep":"all"},{"type":"clear_thinking_20251015","keep":"all"}]}`,
		`{"edits":[{"type":"clear_thinking_20251015","keep":"all"},{"type":"clear_tool_uses_20250919"}]}`,
	} {
		t.Run(context, func(t *testing.T) {
			_, _, err := RequestWithOptions(withContextManagement(t, body, context), Anthropic, OpenAI, "m", Options{AllowReasoningDrop: true})
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("unsupported context strategy accepted/misclassified: %v", err)
			}
		})
	}
}
