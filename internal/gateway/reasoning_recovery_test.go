package gateway

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"torouter/internal/store"
)

func TestReasoningCompatibilityRejection(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"encrypted code", 400, `{"error":{"code":"invalid_encrypted_content","message":"sensitive payload"}}`, true},
		{"thinking signature code", 422, `{"error":{"code":"invalid_thinking_signature"}}`, true},
		{"gemini signature", 400, `{"error":{"status":"INVALID_ARGUMENT","message":"Invalid thought signature in functionCall part"}}`, true},
		{"anthropic signature", 400, `{"error":{"type":"invalid_request_error","message":"Invalid signature in thinking block"}}`, true},
		{"quoted anthropic signature", 400, "{\"error\":{\"message\":\"Invalid `signature` in `thinking` block\"}}", true},
		{"signature verification", 400, `{"error":{"message":"Signature verification failed for thinking block"}}`, true},
		{"snake gemini signature", 400, `{"error":{"message":"thought_signature is invalid"}}`, true},
		{"reasoning field rejected", 400, `{"error":{"code":"unsupported_parameter","param":"messages[2].reasoning_content"}}`, true},
		{"reasoning words", 400, `{"error":{"message":"Invalid reasoning_content in assistant message"}}`, true},
		{"generic", 400, `{"error":{"message":"Bad request"}}`, false},
		{"generic422", 422, `{"error":{"message":"validation failed"}}`, false},
		{"mention", 400, `{"error":{"message":"Error thinking about your invalid request"}}`, false},
		{"tool arguments", 400, `{"error":{"message":"Invalid tool arguments: thinking is required"}}`, false},
		{"nested parameter", 400, `{"error":{"code":"unsupported_parameter","param":"messages[1].tool_calls[0].function.arguments.reasoning_content","message":"unsupported reasoning_content"}}`, false},
		{"thinking configuration parameter", 400, `{"error":{"code":"invalid_parameter","param":"thinking.budget_tokens","message":"invalid thinking budget"}}`, false},
		{"thinking budget", 400, `{"error":{"message":"Invalid thinking budget_tokens"}}`, false},
		{"missing required thinking", 400, `{"error":{"message":"Expected thinking block before tool_use"}}`, false},
		{"auth signature", 400, `{"error":{"code":"invalid_signature","message":"Invalid authentication signature"}}`, false},
		{"not an error envelope", 400, `{"message":"invalid thought signature","code":"invalid_encrypted_content"}`, false},
		{"nested user data", 400, `{"error":{"message":"invalid input","data":{"code":"invalid_encrypted_content"}}}`, false},
		{"401", 401, `{"error":{"code":"invalid_encrypted_content"}}`, false},
		{"429", 429, `{"error":{"code":"invalid_encrypted_content"}}`, false},
		{"500", 500, `{"error":{"code":"invalid_encrypted_content"}}`, false},
		{"timeout", 408, `{"error":{"code":"invalid_encrypted_content"}}`, false},
		{"not json", 400, `invalid thinking signature`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReasoningCompatibilityRejection(tt.status, []byte(tt.body)); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStripReasoningPreservesContextAndTools(t *testing.T) {
	for _, tt := range []struct {
		name, format, body, want string
		removed                  []string
	}{
		{"chat", store.FormatOpenAIChatCompletions,
			`{"messages":[{"role":"user","content":"question"},{"role":"assistant","reasoning_content":"secret","content":null,"tool_calls":[{"id":"call","function":{"name":"f","arguments":"{\"reasoning_content\":\"keep\"}"}}]},{"role":"tool","tool_call_id":"call","content":"result","reasoning_content":"tool payload"}],"reasoning_content":"root extension"}`,
			`{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":null,"tool_calls":[{"id":"call","function":{"name":"f","arguments":"{\"reasoning_content\":\"keep\"}"}}]},{"role":"tool","tool_call_id":"call","content":"result","reasoning_content":"tool payload"}],"reasoning_content":"root extension"}`,
			[]string{"messages.reasoning_content"}},
		{"messages", store.FormatAnthropicMessages,
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret","signature":"secret"},{"type":"redacted_thinking","data":"secret"},{"type":"text","text":"answer"},{"type":"tool_use","id":"call","name":"f","input":{"type":"thinking","thinking":"keep"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"result"}]}]}`,
			`{"messages":[{"role":"assistant","content":[{"type":"text","text":"answer"},{"type":"tool_use","id":"call","name":"f","input":{"type":"thinking","thinking":"keep"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"result"}]}]}`,
			[]string{"messages.content.thinking", "messages.content.redacted_thinking"}},
		{"gemini", store.FormatGeminiGenerateContent,
			`{"contents":[{"role":"model","parts":[{"thought":true,"text":"secret"},{"text":"answer","thoughtSignature":"secret"},{"functionCall":{"name":"f","args":{"thoughtSignature":"keep","thought":true}},"thoughtSignature":"secret"}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"thoughtSignature":"keep"}}}]}],"cachedContent":"cache-history"}`,
			`{"contents":[{"role":"model","parts":[{"text":"answer"},{"functionCall":{"name":"f","args":{"thoughtSignature":"keep","thought":true}}}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"thoughtSignature":"keep"}}}]}],"cachedContent":"cache-history"}`,
			[]string{"contents.parts.thought", "contents.parts.thoughtSignature"}},
		{"responses", store.FormatOpenAIResponses,
			`{"input":[{"type":"reasoning","encrypted_content":"secret"},{"type":"compaction","encrypted_content":"only-history"},{"type":"item_reference","id":"history"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","call_id":"call","name":"f","arguments":"{\"type\":\"reasoning\"}"},{"type":"function_call_output","call_id":"call","output":"result"},{"type":"agent_message","content":"only-agent-history"}],"previous_response_id":"prev","conversation":"conversation"}`,
			`{"input":[{"type":"compaction","encrypted_content":"only-history"},{"type":"item_reference","id":"history"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","call_id":"call","name":"f","arguments":"{\"type\":\"reasoning\"}"},{"type":"function_call_output","call_id":"call","output":"result"},{"type":"agent_message","content":"only-agent-history"}],"previous_response_id":"prev","conversation":"conversation"}`,
			[]string{"input.reasoning"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := []byte(tt.body)
			before := bytes.Clone(original)
			got, removed, err := stripReasoning(original, tt.format)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryJSONEqual(t, got, []byte(tt.want))
			if !reflect.DeepEqual(removed, tt.removed) {
				t.Fatalf("removed %v, want %v", removed, tt.removed)
			}
			if !bytes.Equal(original, before) {
				t.Fatal("mutated original input")
			}
		})
	}
}

func TestStripReasoningEmptyAssistant(t *testing.T) {
	for _, tt := range []struct{ name, format, body, want string }{
		{"chat missing content", store.FormatOpenAIChatCompletions, `{"messages":[{"role":"user","content":"q"},{"role":"assistant","reasoning_content":"secret"}]}`, `{"messages":[{"role":"user","content":"q"}]}`},
		{"chat null content", store.FormatOpenAIChatCompletions, `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"reasoning_content":"secret"}]}`, `{"messages":[{"role":"user","content":"q"}]}`},
		{"chat existing empty text", store.FormatOpenAIChatCompletions, `{"messages":[{"role":"assistant","content":"","reasoning_content":"secret"}]}`, `{"messages":[{"role":"assistant","content":""}]}`},
		{"chat unknown body preserved", store.FormatOpenAIChatCompletions, `{"messages":[{"role":"assistant","content":{"custom":"body"},"reasoning_content":"secret"}]}`, `{"messages":[{"role":"assistant","content":{"custom":"body"}}]}`},
		{"chat empty array removed", store.FormatOpenAIChatCompletions, `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[ ],"reasoning_content":"secret"}]}`, `{"messages":[{"role":"user","content":"q"}]}`},
		{"messages", store.FormatAnthropicMessages, `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"thinking","thinking":"secret"}]}]}`, `{"messages":[{"role":"user","content":"q"}]}`},
		{"gemini", store.FormatGeminiGenerateContent, `{"contents":[{"role":"user","parts":[{"text":"q"}]},{"role":"model","parts":[{"thought":true,"text":"secret"}]}]}`, `{"contents":[{"role":"user","parts":[{"text":"q"}]}]}`},
		{"gemini marked call", store.FormatGeminiGenerateContent, `{"contents":[{"role":"model","parts":[{"thought":true,"thoughtSignature":"secret","functionCall":{"name":"f","args":{}}}]}]}`, `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]}]}`},
		{"responses external history", store.FormatOpenAIResponses, `{"previous_response_id":"prev","input":[{"type":"reasoning","encrypted_content":"secret"}]}`, `{"previous_response_id":"prev","input":[]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := stripReasoning([]byte(tt.body), tt.format)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryJSONEqual(t, got, []byte(tt.want))
		})
	}
}

func TestStripReasoningRejectsEmptyRequestAndUnsafeMessages(t *testing.T) {
	for _, tt := range []struct{ format, body string }{
		{store.FormatOpenAIChatCompletions, `{"messages":[{"role":"assistant","reasoning_content":"secret"}]}`},
		{store.FormatAnthropicMessages, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret"}]}]}`},
		{store.FormatGeminiGenerateContent, `{"contents":[{"role":"model","parts":[{"thought":true,"text":"secret"}]}]}`},
		{store.FormatOpenAIResponses, `{"input":[{"type":"reasoning","encrypted_content":"secret"}]}`},
		{store.FormatOpenAIChatCompletions, `{"messages":[{"role":"user","content":"q"},{"role":"assistant","reasoning_content":"secret","agent_message":{"content":"keep"}}]}`},
		{store.FormatAnthropicMessages, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret"}],"agent_message":{"content":"keep"}}]}`},
		{store.FormatAnthropicMessages, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret","agent_message":{"content":"keep"}}]}]}`},
		{store.FormatOpenAIResponses, `{"input":[{"type":"reasoning","encrypted_content":"secret","agent_message":{"content":"keep"}}]}`},
		{store.FormatOpenAIResponses, `{"previous_response_id":0,"input":[{"type":"reasoning","encrypted_content":"secret"}]}`},
	} {
		got, removed, err := stripReasoning([]byte(tt.body), tt.format)
		if err == nil {
			t.Fatalf("expected safe refusal for %s", tt.body)
		}
		if !bytes.Equal(got, []byte(tt.body)) || len(removed) != 0 {
			t.Fatal("failed strip returned partial modifications")
		}
	}
}

func TestStripReasoningDoesNotTouchUniqueHistory(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"compaction","encrypted_content":"only-history"}],"previous_response_id":"prev","conversation":{"id":"conversation"}}`,
		`{"input":[{"type":"item_reference","id":"only-history"}]}`,
		`{"input":[{"type":"agent_message","content":"only-history"}]}`,
		`{"input":[{"type":"function_call_output","call_id":"remote-call","output":"only-result"}],"previous_response_id":"prev"}`,
	} {
		got, removed, err := stripReasoning([]byte(body), store.FormatOpenAIResponses)
		if err != nil || len(removed) != 0 || !bytes.Equal(got, []byte(body)) {
			t.Fatalf("unique history touched: %s %v %v", got, removed, err)
		}
	}
}

func TestStripReasoningNoopAndNumberIntegrity(t *testing.T) {
	for _, body := range []string{
		` {"messages":[{"role":"assistant","content":"keep"}],"n":9007199254740993} `,
		`{"messages":[{"role":"tool","content":{"reasoning_content":"keep"},"reasoning_content":"keep"}]}`,
		`{"messages":[{"role":"assistant","content":"keep","reasoning_content":null}]}`,
		`{"messages":[{"role":"assistant","content":"keep","reasoning_content":""}]}`,
	} {
		got, removed, err := stripReasoning([]byte(body), store.FormatOpenAIChatCompletions)
		if err != nil || len(removed) != 0 || !bytes.Equal(got, []byte(body)) {
			t.Fatalf("noop changed: %s %v %v", got, removed, err)
		}
	}
	got, _, err := stripReasoning([]byte(`{"messages":[{"role":"assistant","content":"keep","reasoning_content":"remove"}],"n":9007199254740993}`), store.FormatOpenAIChatCompletions)
	if err != nil || !bytes.Contains(got, []byte(`9007199254740993`)) {
		t.Fatalf("number changed: %s %v", got, err)
	}
	for _, body := range []string{`null`, `[]`, `{`, `{"messages":{}}`} {
		if _, _, err := stripReasoning([]byte(body), store.FormatOpenAIChatCompletions); err == nil {
			t.Fatalf("invalid request accepted: %s", body)
		}
	}
}

func assertRecoveryJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
