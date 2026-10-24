// Known vectors from CLIProxyAPI (MIT); see SOURCE.md.
package claude

import (
	"encoding/json"
	"strings"
	"testing"
)

const claudeCCH21220BaseBody = `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.test; cc_entrypoint=sdk-cli; cch=00000;"},{"type":"text","text":"system-x"}],"tools":[],"metadata":{"user_id":"meta-x"},"max_tokens":1,"thinking":{"type":"adaptive","display":"omitted"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"output_config":{"effort":"high"},"stream":true}`

func TestSignAnthropicMessagesBody_ClaudeCode21220KnownVectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "base", body: claudeCCH21220BaseBody, want: "7ee87"},
		{name: "model value ignored", body: strings.Replace(claudeCCH21220BaseBody, `"model":"model-a"`, `"model":"model-b"`, 1), want: "7ee87"},
		{name: "max tokens ignored", body: strings.Replace(claudeCCH21220BaseBody, `"max_tokens":1`, `"max_tokens":2`, 1), want: "7ee87"},
		{name: "message changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"text":"x"`, `"text":"y"`, 1), want: "b9cc8"},
		{name: "system changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"system-x"`, `"system-y"`, 1), want: "a30d3"},
		{name: "metadata changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"user_id":"meta-x"`, `"user_id":"meta-y"`, 1), want: "7a89d"},
		{name: "thinking changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"thinking":{"type":"adaptive","display":"omitted"}`, `"thinking":{"type":"disabled"}`, 1), want: "7205c"},
		{name: "context changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`, `"context_management":{"edits":[]}`, 1), want: "05073"},
		{name: "effort changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"effort":"high"`, `"effort":"low"`, 1), want: "12366"},
		{name: "stream changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"stream":true`, `"stream":false`, 1), want: "60400"},
		{name: "tool changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"tools":[]`, `"tools":[{"name":"t","description":"d","input_schema":{"type":"object"}}]`, 1), want: "3d78d"},
		{name: "extra field changes hash", body: strings.Replace(claudeCCH21220BaseBody, `"stream":true}`, `"stream":true,"extra_top":"extra"}`, 1), want: "2d622"},
		{
			name: "field order remains significant",
			body: `{"stream":true,"output_config":{"effort":"high"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"thinking":{"type":"adaptive","display":"omitted"},"max_tokens":1,"metadata":{"user_id":"meta-x"},"tools":[],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.test; cc_entrypoint=sdk-cli; cch=00000;"},{"type":"text","text":"system-x"}],"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"model":"model-a"}`,
			want: "e5b6c",
		},
		{name: "nested model value ignored", body: strings.Replace(claudeCCH21220BaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","model":"a"}`, 1), want: "0601b"},
		{name: "nested max tokens member omitted", body: strings.Replace(claudeCCH21220BaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","max_tokens":2}`, 1), want: "7ee87"},
		{name: "top level fallbacks member omitted", body: strings.Replace(claudeCCH21220BaseBody, `"stream":true}`, `"stream":true,"fallbacks":[{"model":"fallback-a"}]}`, 1), want: "7ee87"},
		{name: "nested fallbacks member omitted", body: strings.Replace(claudeCCH21220BaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","fallbacks":[{"model":"nested-a"}]}`, 1), want: "7ee87"},
		{name: "top level fallback credit token omitted", body: strings.Replace(claudeCCH21220BaseBody, `"stream":true}`, `"stream":true,"fallback_credit_token":"a"}`, 1), want: "7ee87"},
		{name: "nested fallback credit token omitted", body: strings.Replace(claudeCCH21220BaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","fallback_credit_token":"a"}`, 1), want: "7ee87"},
		{name: "trailing dispatch run keeps native comma", body: strings.Replace(claudeCCH21220BaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","max_tokens":999,"fallbacks":[{"model":"fallback-model"}]}`, 1), want: "4589b"},
		{name: "model before trailing dispatch run", body: strings.Replace(claudeCCH21220BaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","model":"nested-model","max_tokens":999,"fallbacks":[{"model":"fallback-model"}],"fallback_credit_token":"not-a-real-token"}`, 1), want: "2d312"},
		{name: "model splits dispatch runs", body: strings.Replace(claudeCCH21220BaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","max_tokens":999,"model":"nested-model","fallbacks":[{"model":"fallback-model"}]}`, 1), want: "0601b"},
		{name: "ordinary nested member remains", body: strings.Replace(claudeCCH21220BaseBody, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","plain":"a"}`, 1), want: "8d74c"},
		{name: "billing block only", body: `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.test; cc_entrypoint=sdk-cli; cch=00000;"}]}`, want: "f2edb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			signed, err := signBody([]byte(tt.body))
			if err != nil {
				t.Fatalf("signBody() error = %v", err)
			}
			if got := claudeCCHFromBody(t, signed); got != tt.want {
				t.Fatalf("cch = %q, want %q\nbody: %s", got, tt.want, signed)
			}
		})
	}
}

func claudeCCHFromBody(t *testing.T, body []byte) string {
	t.Helper()
	var decoded struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.System) == 0 {
		t.Fatal("invalid signed body")
	}
	_, suffix, ok := strings.Cut(decoded.System[0].Text, "cch=")
	if !ok {
		t.Fatal("missing cch")
	}
	return strings.SplitN(suffix, ";", 2)[0]
}
