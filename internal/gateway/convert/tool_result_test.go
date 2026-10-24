package convert

import (
	"encoding/json"
	"reflect"
	"testing"
)

func resultRequest(from string, result any) object {
	switch from {
	case Gemini:
		return object{"generationConfig": object{"maxOutputTokens": 128}, "contents": []any{
			object{"role": "model", "parts": []any{object{"functionCall": object{"id": "t1", "name": "f", "args": object{}}}}},
			object{"role": "user", "parts": []any{object{"functionResponse": object{"id": "t1", "name": "f", "response": result}}}},
		}}
	case Anthropic:
		return object{"max_tokens": 128, "messages": []any{
			object{"role": "assistant", "content": []any{object{"type": "tool_use", "id": "t1", "name": "f", "input": object{}}}},
			object{"role": "user", "content": []any{object{"type": "tool_result", "tool_use_id": "t1", "content": result}}},
		}}
	default:
		return object{"max_tokens": 128, "messages": []any{
			object{"role": "assistant", "tool_calls": []any{object{"id": "t1", "type": "function", "function": object{"name": "f", "arguments": "{}"}}}},
			object{"role": "tool", "tool_call_id": "t1", "content": result},
		}}
	}
}

func convertedResult(t *testing.T, from, to string, result any) any {
	t.Helper()
	source, err := json.Marshal(resultRequest(from, result))
	if err != nil {
		t.Fatal(err)
	}
	converted, err := Request(source, from, to, "m")
	if err != nil {
		t.Fatal(err)
	}
	var root object
	if err := json.Unmarshal(converted, &root); err != nil {
		t.Fatal(err)
	}
	if to == Gemini {
		part := root["contents"].([]any)[1].(object)["parts"].([]any)[0].(object)
		return part["functionResponse"].(object)["response"]
	}
	message := root["messages"].([]any)[1].(object)
	if to == Anthropic {
		return message["content"].([]any)[0].(object)["content"]
	}
	return message["content"]
}

func TestGeminiStructuredToolResultsPreserveBusinessFields(t *testing.T) {
	results := []object{
		{"output": "success"},
		{"output": object{"record": []any{"x", nil, float64(2)}}},
		{"output": "success", "status": "ok", "error": object{"code": float64(0)}},
		{},
	}
	for _, to := range []string{OpenAI, Anthropic} {
		for _, result := range results {
			content := convertedResult(t, Gemini, to, result)
			text, ok := content.(string)
			if !ok {
				t.Fatalf("%s result content is not text: %#v", to, content)
			}
			var got any
			if err := json.Unmarshal([]byte(text), &got); err != nil || !reflect.DeepEqual(got, result) {
				t.Fatalf("%s structured result changed: content=%q expected=%#v err=%v", to, text, result, err)
			}
		}
	}
}

func TestTextToolResultsUseExplicitGeminiWrapper(t *testing.T) {
	texts := []string{"sunny", "", `{"output":"success"}`, `{"output":{"nested":[1,null]},"status":"ok"}`}
	for _, from := range []string{OpenAI, Anthropic} {
		for _, text := range texts {
			got := convertedResult(t, from, Gemini, text)
			want := object{"output": text}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s text interpreted as JSON: got=%#v want=%#v", from, got, want)
			}
			// The native protocol has no provenance marker: do not guess that
			// a one-key output object is a text wrapper on the way back.
			back := convertedResult(t, Gemini, from, got)
			var decoded any
			if err := json.Unmarshal([]byte(back.(string)), &decoded); err != nil || !reflect.DeepEqual(decoded, want) {
				t.Fatalf("wrapper lost on return: %q want=%#v err=%v", back, want, err)
			}
		}
	}
}
