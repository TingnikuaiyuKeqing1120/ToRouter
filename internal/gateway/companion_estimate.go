package gateway

import (
	"encoding/json"

	"torouter/internal/store"
)

// companionTextUnits counts only incremental, explicit output text. It neither
// retains text nor changes the stream parser's acceptance or billing rules.
func companionTextUnits(obj map[string]json.RawMessage, format string) int64 {
	var units int64
	add := func(raw json.RawMessage) {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return
		}
		for _, r := range text {
			if r < 128 {
				units++
			} else {
				units += 4
			}
		}
	}
	object := func(raw json.RawMessage) map[string]json.RawMessage {
		var value map[string]json.RawMessage
		if json.Unmarshal(raw, &value) != nil {
			return nil
		}
		return value
	}
	array := func(raw json.RawMessage) []map[string]json.RawMessage {
		var value []map[string]json.RawMessage
		if json.Unmarshal(raw, &value) != nil {
			return nil
		}
		return value
	}
	var eventType string
	_ = json.Unmarshal(obj["type"], &eventType)
	switch format {
	case store.FormatOpenAIChatCompletions:
		for _, choice := range array(obj["choices"]) {
			delta := object(choice["delta"])
			for _, field := range []string{"content", "refusal", "reasoning_content", "reasoning"} {
				add(delta[field])
			}
			for _, call := range array(delta["tool_calls"]) {
				add(object(call["function"])["arguments"])
			}
			add(object(delta["function_call"])["arguments"])
		}
	case store.FormatOpenAIResponses:
		switch eventType {
		case "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			add(obj["delta"])
		}
	case store.FormatAnthropicMessages:
		var block map[string]json.RawMessage
		switch eventType {
		case "content_block_delta":
			block = object(obj["delta"])
		case "content_block_start":
			block = object(obj["content_block"])
		}
		var blockType string
		_ = json.Unmarshal(block["type"], &blockType)
		switch {
		case eventType == "content_block_delta" && blockType == "text_delta", eventType == "content_block_start" && blockType == "text":
			add(block["text"])
		case eventType == "content_block_delta" && blockType == "thinking_delta", eventType == "content_block_start" && blockType == "thinking":
			add(block["thinking"])
		case eventType == "content_block_delta" && blockType == "input_json_delta":
			add(block["partial_json"])
		}
	case store.FormatGeminiGenerateContent:
		for _, candidate := range array(obj["candidates"]) {
			for _, part := range array(object(candidate["content"])["parts"]) {
				add(part["text"])
			}
		}
	}
	return units
}
