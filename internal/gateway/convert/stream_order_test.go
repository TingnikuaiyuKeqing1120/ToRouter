package convert

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func assertStreamToolTextOrder(t *testing.T, source, from, to string) {
	t.Helper()
	var out bytes.Buffer
	err := Stream(strings.NewReader(source), &out, from, to, "m")
	if to == OpenAI {
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unrepresentable Chat ordering accepted: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "[DONE]") {
			t.Fatal("unsupported ordering ended successfully")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	parts := assembleClientStream(t, out.Bytes(), to)
	if len(parts) != 4 || parts[0].Tool == nil || parts[0].Tool.ID != "call-a" || parts[1].Text != "after-a" || parts[2].Tool == nil || parts[2].Tool.ID != "call-b" || parts[3].Text != "after-b" {
		t.Fatalf("tool/text content lost or reordered: %+v\n%s", parts, out.String())
	}
}

func TestStreamPendingToolsRetainFollowingText(t *testing.T) {
	fixtures := map[string]string{
		Anthropic: anthropicMessage("tool_use", toolBlock(0, "call-a", "f", `{}`), textBlock(1, "after-a"), toolBlock(2, "call-b", "g", `{}`), textBlock(3, "after-b")),
		Gemini:    sse(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call-a","name":"f","args":{}}},{"text":"after-a"},{"functionCall":{"id":"call-b","name":"g","args":{}}},{"text":"after-b"}]},"finishReason":"STOP"}]}`),
	}
	for from, source := range fixtures {
		for _, to := range fourProtocols {
			if from == to {
				continue
			}
			t.Run(from+"_"+to, func(t *testing.T) { assertStreamToolTextOrder(t, source, from, to) })
		}
	}
}

func TestResponsesStreamRetainsTextAfterFunctionItems(t *testing.T) {
	var source bytes.Buffer
	e, _ := newEmitter(Responses, &source, "m")
	e.id = "r"
	for _, part := range []irPart{{Tool: &irToolCall{ID: "call-a", Name: "f", Arguments: `{}`}}, {Text: "after-a"}, {Tool: &irToolCall{ID: "call-b", Name: "g", Arguments: `{}`}}, {Text: "after-b"}} {
		var err error
		if part.Tool != nil {
			err = e.toolRaw(*part.Tool, "")
		} else {
			err = e.text(part.Text)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := e.end(FinishToolCalls, ""); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{OpenAI, Anthropic, Gemini} {
		t.Run(to, func(t *testing.T) { assertStreamToolTextOrder(t, source.String(), Responses, to) })
	}
}

func TestStreamBridgeReasoningPositionIncludesEmittedText(t *testing.T) {
	source := anthropicMessage("tool_use", textBlock(0, "prefix"), toolBlock(1, "call-a", "f", `{}`), thinkingBlock(2, "original-signature"), toolBlock(3, "call-b", "g", `{}`))
	opts := Options{Scope: "synthetic-scope", SigningKey: []byte("synthetic-key"), Bridge: NewReasoningBridge(8, 0)}
	var wire bytes.Buffer
	if _, err := StreamWithOptions(strings.NewReader(source), &wire, Anthropic, OpenAI, "m", opts); err != nil {
		t.Fatal(err)
	}
	parts := assembleClientStream(t, wire.Bytes(), OpenAI)
	request := requestFromResponseParts(t, parts, OpenAI)
	back, _, err := RequestWithOptions(request, OpenAI, Anthropic, "m", opts)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decodeRequest(back, Anthropic)
	if err != nil {
		t.Fatal(err)
	}
	got := restored.Messages[1].Parts
	if len(got) != 4 || got[0].Text != "prefix" || got[1].Tool == nil || got[1].Tool.ID != "call-a" || got[2].Reasoning == nil || got[2].Reasoning.Value["signature"] != "original-signature" || got[3].Tool == nil || got[3].Tool.ID != "call-b" {
		t.Fatalf("reasoning moved across preceding text/tool: %s", back)
	}
}

func TestStreamBridgeConsecutiveReasoningRetainsOrder(t *testing.T) {
	for _, leading := range []bool{false, true} {
		name := "reasoning_first"
		if leading {
			name = "leading_text"
		}
		t.Run(name, func(t *testing.T) {
			var blocks []string
			index := 0
			if leading {
				blocks = append(blocks, textBlock(index, "prefix"))
				index++
			}
			blocks = append(blocks, thinkingBlock(index, "signature-first"), thinkingBlock(index+1, "signature-second"), toolBlock(index+2, "call-a", "f", `{}`))
			opts := Options{Scope: "synthetic-scope", SigningKey: []byte("synthetic-key"), Bridge: NewReasoningBridge(8, 0)}
			var wire bytes.Buffer
			if _, err := StreamWithOptions(strings.NewReader(anthropicMessage("tool_use", blocks...)), &wire, Anthropic, OpenAI, "m", opts); err != nil {
				t.Fatal(err)
			}
			request := requestFromResponseParts(t, assembleClientStream(t, wire.Bytes(), OpenAI), OpenAI)
			back, _, err := RequestWithOptions(request, OpenAI, Anthropic, "m", opts)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := decodeRequest(back, Anthropic)
			if err != nil {
				t.Fatal(err)
			}
			parts := restored.Messages[1].Parts
			if len(parts) != index+3 || leading && parts[0].Text != "prefix" || parts[index].Reasoning == nil || parts[index].Reasoning.Value["signature"] != "signature-first" || parts[index+1].Reasoning == nil || parts[index+1].Reasoning.Value["signature"] != "signature-second" || parts[index+2].Tool == nil || parts[index+2].Tool.ID != "call-a" {
				t.Fatalf("consecutive reasoning reordered: %s", back)
			}
		})
	}
}
