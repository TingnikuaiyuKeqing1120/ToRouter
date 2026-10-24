package convert

import "testing"

// The Responses API reports usage with its own schema — input_tokens /
// output_tokens, cache tokens nested under input_tokens_details (cached_tokens,
// cache_write_tokens) and reasoning tokens under output_tokens_details —
// verified against the official Responses reference on 2026-10-02. Reading it
// with the Chat Completions schema reported no usage at all, so every attempt
// against a Responses endpoint was booked as estimated (T024).
func TestResponsesUsage(t *testing.T) {
	cases := []struct {
		name            string
		b               string
		in, out, cr, cw int64
		ok              bool
	}{
		// Total input includes both cache categories: 100 - 30 - 5 = 65.
		{"response object", `{"id":"resp_1","object":"response","output":[],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":5},"output_tokens_details":{"reasoning_tokens":7}}}`, 65, 20, 30, 5, true},
		// response.completed carries the whole response object, so usage nests
		// one level below the event.
		{"completed event", `{"type":"response.completed","sequence_number":13,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":37,"output_tokens":11,"total_tokens":48,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`, 37, 11, 0, 0, true},
		{"event without usage", `{"type":"response.output_text.delta","delta":"hi","sequence_number":1}`, 0, 0, 0, 0, false},
		{"usage null", `{"usage":null}`, 0, 0, 0, 0, false},
		{"negative count", `{"usage":{"input_tokens":-1,"output_tokens":2}}`, 0, 0, 0, 0, false},
		{"fractional count", `{"usage":{"input_tokens":1.5,"output_tokens":2}}`, 0, 0, 0, 0, false},
	}
	for _, c := range cases {
		in, out, cr, cw, ok := ParseUsage([]byte(c.b), OpenAIResponses)
		if in != c.in || out != c.out || cr != c.cr || cw != c.cw || ok != c.ok {
			t.Errorf("%s: got %d %d %d %d %v", c.name, in, out, cr, cw, ok)
		}
	}
}

// Strict mode is the conversion path's contract: refuse what the IR cannot
// carry rather than drop it silently. Reasoning tokens are already a subset
// of output_tokens; accept their counter without adding it to output again.
func TestResponsesUsageStrict(t *testing.T) {
	valid := `{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":3}}}`
	p := &parser{}
	u, ok := extractUsage(decodeRoot([]byte(valid), p), OpenAIResponses, p, true)
	if p.err != nil || !ok || u.Input != 4 || u.Output != 20 || u.CacheRead != 4 || u.CacheWrite != 2 {
		t.Fatalf("valid body: %+v %v %v", u, ok, p.err)
	}
	for _, c := range []struct{ name, b string }{
		{"unknown top-level field", `{"usage":{"input_tokens":10,"output_tokens":20,"cached_tokens":3}}`},
		{"unknown input detail", `{"usage":{"input_tokens":10,"output_tokens":20,"input_tokens_details":{"reasoning_tokens":3}}}`},
		{"cached above input", `{"usage":{"input_tokens":10,"output_tokens":20,"input_tokens_details":{"cached_tokens":11}}}`},
	} {
		p := &parser{}
		extractUsage(decodeRoot([]byte(c.b), p), OpenAIResponses, p, true)
		if p.err == nil {
			t.Errorf("%s accepted in strict mode", c.name)
		}
	}
}

// Callers that only know the wire family keep working, and the full endpoint
// format names the gateway passes in map onto the same schemas — except for
// Responses, which is the whole point of the split.
func TestUsageAcceptsFamiliesAndEndpointFormats(t *testing.T) {
	chat := `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`
	for _, p := range []string{OpenAI, OpenAIChatCompletions} {
		if in, out, cr, cw, ok := ParseUsage([]byte(chat), p); !ok || in != 70 || out != 20 || cr != 30 || cw != 0 {
			t.Errorf("chat schema under %q: %d %d %d %d %v", p, in, out, cr, cw, ok)
		}
	}
	anthropic := `{"usage":{"input_tokens":70,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}`
	for _, p := range []string{Anthropic, AnthropicMessages} {
		if in, out, cr, cw, ok := ParseUsage([]byte(anthropic), p); !ok || in != 70 || out != 20 || cr != 30 || cw != 5 {
			t.Errorf("anthropic schema under %q: %d %d %d %d %v", p, in, out, cr, cw, ok)
		}
	}
	gemini := `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":30}}`
	for _, p := range []string{Gemini, GeminiGenerateContent} {
		if in, out, cr, cw, ok := ParseUsage([]byte(gemini), p); !ok || in != 70 || out != 20 || cr != 30 || cw != 0 {
			t.Errorf("gemini schema under %q: %d %d %d %d %v", p, in, out, cr, cw, ok)
		}
	}
	if _, _, _, _, ok := ParseUsage([]byte(`{}`), OpenAIResponses); ok {
		t.Error("responses schema accepted a chat body")
	}
	if _, _, _, _, ok := ParseUsage([]byte(chat), OpenAIResponses); ok {
		t.Error("responses schema accepted a chat body")
	}
}
