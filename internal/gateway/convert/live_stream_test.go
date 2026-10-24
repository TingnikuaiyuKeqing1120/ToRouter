package convert

import (
	"strings"
	"testing"
)

// Regression for live traffic: anthropic SSE (full lifecycle) must convert to
// openai chunks. Fixture bytes mirror test/mockup.py streaming response.
func TestAnthropicSSEToOpenAILiveShape(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"ms1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-mock\",\"content\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"stream\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var out strings.Builder
	err := Stream(strings.NewReader(body), &out, "anthropic", "openai", "test-model")
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{"chat.completion.chunk", "\"stream\"", "[DONE]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}
