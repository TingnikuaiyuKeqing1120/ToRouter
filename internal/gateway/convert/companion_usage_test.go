package convert

import "testing"

func TestReportedOutputDistinguishesMissingFromZero(t *testing.T) {
	cases := []struct {
		name, format, body string
		count              int64
		known              bool
	}{
		{"chat input only", OpenAIChatCompletions, `{"usage":{"prompt_tokens":8}}`, 0, false},
		{"chat zero", OpenAIChatCompletions, `{"usage":{"completion_tokens":0}}`, 0, true},
		{"responses nested", OpenAIResponses, `{"response":{"usage":{"input_tokens":8,"output_tokens":4}}}`, 4, true},
		{"anthropic start", AnthropicMessages, `{"message":{"usage":{"input_tokens":8}}}`, 0, false},
		{"anthropic delta", AnthropicMessages, `{"usage":{"output_tokens":5}}`, 5, true},
		{"gemini thinking", GeminiGenerateContent, `{"usageMetadata":{"candidatesTokenCount":4,"thoughtsTokenCount":3}}`, 7, true},
		{"malformed count", OpenAIChatCompletions, `{"usage":{"completion_tokens":"4"}}`, 0, false},
		{"negative count", OpenAIChatCompletions, `{"usage":{"completion_tokens":-1}}`, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, known := ReportedOutputTokens([]byte(c.body), c.format)
			if known != c.known || (known && n != c.count) {
				t.Fatalf("got (%d,%v), want (%d,%v)", n, known, c.count, c.known)
			}
		})
	}
}
