package gateway

import (
	"testing"

	"torouter/internal/store"
)

func TestDetectStateCarriers(t *testing.T) {
	cases := []struct {
		name      string
		protocol  string
		responses bool
		body      string
		kinds     []string
		oldest    string
	}{
		{"anthropic thinking", "anthropic", false,
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"sig-1"}]}]}`,
			[]string{"anthropic.thinking"}, "sig-1"},
		{"anthropic keeps the oldest of many", "anthropic", false,
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","signature":"sig-1"}]},{"role":"assistant","content":[{"type":"thinking","signature":"sig-2"}]}]}`,
			[]string{"anthropic.thinking"}, "sig-1"},
		{"anthropic redacted", "anthropic", false,
			`{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"blob"}]}]}`,
			[]string{"anthropic.redacted_thinking"}, "blob"},
		{"openai reasoning content", "openai", false,
			`{"messages":[{"role":"assistant","content":"a","reasoning_content":"r1"}]}`,
			[]string{"openai.reasoning_content"}, "r1"},
		{"responses cursor is stateful but not a fingerprint", "openai", true,
			`{"previous_response_id":"resp_1","input":[]}`,
			[]string{"openai.previous_response_id"}, ""},
		{"responses encrypted reasoning is the fingerprint source", "openai", true,
			`{"input":[{"type":"reasoning","encrypted_content":"enc-1"}]}`,
			[]string{"openai.encrypted_reasoning"}, "enc-1"},
		{"responses prefers the earliest encrypted item", "openai", true,
			`{"input":[{"type":"reasoning","encrypted_content":"enc-1"},{"type":"reasoning","encrypted_content":"enc-2"}]}`,
			[]string{"openai.encrypted_reasoning"}, "enc-1"},
		{"gemini signature", "gemini", false,
			`{"contents":[{"role":"model","parts":[{"text":"t","thoughtSignature":"tsig"}]}]}`,
			[]string{"gemini.thought_signature"}, "tsig"},
		{"gemini thought flag", "gemini", false,
			`{"contents":[{"role":"model","parts":[{"text":"t","thought":true}]}]}`,
			[]string{"gemini.thought"}, ""},
		{"portable request", "openai", false,
			`{"messages":[{"role":"user","content":"hi"}]}`, nil, ""},
		{"not json", "openai", false, `not json`, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectState(c.protocol, c.responses, []byte(c.body))
			if len(got.kinds) != len(c.kinds) {
				t.Fatalf("kinds = %v, want %v", got.kinds, c.kinds)
			}
			for i := range c.kinds {
				if got.kinds[i] != c.kinds[i] {
					t.Fatalf("kinds = %v, want %v", got.kinds, c.kinds)
				}
			}
			if got.oldest != c.oldest {
				t.Fatalf("oldest = %q, want %q", got.oldest, c.oldest)
			}
			if got.present() != (len(c.kinds) > 0) {
				t.Fatalf("present = %v", got.present())
			}
		})
	}
}

func TestDetectStateConversation(t *testing.T) {
	contains := func(kinds []string, want string) bool {
		for _, k := range kinds {
			if k == want {
				return true
			}
		}
		return false
	}
	// Present: stateful on its own, listed as its own kind, but the server-side
	// handle must not become an affinity fingerprint.
	got := detectState("openai", true, []byte(`{"conversation":"conv_1","input":[]}`))
	if !got.present() {
		t.Fatal("conversation alone must mark the request stateful")
	}
	if !contains(got.kinds, "openai.conversation") {
		t.Fatalf("kinds = %v, want openai.conversation", got.kinds)
	}
	if got.oldest != "" {
		t.Fatalf("oldest = %q, want empty", got.oldest)
	}
	// Absent or empty: nothing changes.
	for _, body := range []string{`{"input":[]}`, `{"conversation":"","input":[]}`} {
		if got := detectState("openai", true, []byte(body)); got.present() {
			t.Fatalf("%s: present = true, want false", body)
		}
	}
	// Alongside the replayable carrier: both detected, fingerprint stays the
	// carrier's value.
	got = detectState("openai", true, []byte(`{"conversation":"conv_1","input":[{"type":"reasoning","encrypted_content":"enc-1"}]}`))
	if !contains(got.kinds, "openai.conversation") || !contains(got.kinds, "openai.encrypted_reasoning") {
		t.Fatalf("kinds = %v, want both openai.conversation and openai.encrypted_reasoning", got.kinds)
	}
	if got.oldest != "enc-1" {
		t.Fatalf("oldest = %q, want enc-1", got.oldest)
	}
}

func TestSessionKeyIsNamespaced(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"user_hash_session_abc"}}`)
	a, kind := sessionKey(nil, store.FormatAnthropicMessages, false, body, 1, "claude-x")
	b, _ := sessionKey(nil, store.FormatAnthropicMessages, false, body, 1, "claude-x")
	if a == "" || a != b {
		t.Fatalf("same inputs must give the same key: %q vs %q", a, b)
	}
	if kind != "user_metadata" {
		t.Fatalf("kind = %q", kind)
	}
	// Different client, different model, different header value: all different keys.
	other, _ := sessionKey(nil, store.FormatAnthropicMessages, false, body, 2, "claude-x")
	if other == a {
		t.Fatal("different client keys must not share a pin")
	}
	otherModel, _ := sessionKey(nil, store.FormatAnthropicMessages, false, body, 1, "claude-y")
	if otherModel == a {
		t.Fatal("different downstream models must not share a pin")
	}
	// The raw identifier must not appear in the key.
	if a == "s:uid:abc" || len(a) < 20 {
		t.Fatalf("key does not look hashed: %q", a)
	}
	if k, _ := sessionKey(nil, store.FormatOpenAIChatCompletions, false, []byte(`{"messages":[]}`), 1, "m"); k != "" {
		t.Fatal("a client with no identifier must yield no key")
	}
}

func TestCarrierFingerprintIsNamespacedByModel(t *testing.T) {
	a := carrierFingerprint(1, "claude-x", store.FormatAnthropicMessages, "sig-1")
	b := carrierFingerprint(1, "claude-x", store.FormatAnthropicMessages, "sig-1")
	c := carrierFingerprint(1, "claude-y", store.FormatAnthropicMessages, "sig-1")
	d := carrierFingerprint(2, "claude-x", store.FormatAnthropicMessages, "sig-1")
	if a == "" || a != b || a == c || a == d {
		t.Fatalf("fingerprint namespacing broken: %q %q %q %q", a, b, c, d)
	}
	if carrierFingerprint(1, "claude-x", store.FormatAnthropicMessages, "") != "" {
		t.Fatal("empty carrier must not fingerprint")
	}
}

func TestStateRequiresOriginOnlyForHistoryDependencies(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, body  string
		responses, wantOrigin bool
	}{
		{"responses cursor", "openai", `{"previous_response_id":"resp-1","input":[]}`, true, true},
		{"responses conversation", "openai", `{"conversation":"conv-1","input":[]}`, true, true},
		{"responses conversation object", "openai", `{"conversation":{"id":"conv-1"},"input":[]}`, true, true},
		{"responses item reference", "openai", `{"input":[{"type":"item_reference","id":"item-1"}]}`, true, true},
		{"responses shorthand reference", "openai", `{"input":[{"id":"item-1"}]}`, true, true},
		{"responses compaction", "openai", `{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, true, true},
		{"responses compaction unknown payload", "openai", `{"input":[{"type":"compaction","content":"history"}]}`, true, true},
		{"gemini cached content", "gemini", `{"cachedContent":"cachedContents/history","contents":[{"parts":[{"text":"continue"}]}]}`, false, true},
		{"chat visible reasoning", "openai", `{"messages":[{"role":"assistant","reasoning_content":"thought","content":"answer"}]}`, false, false},
		{"messages thinking signature", "anthropic", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"thought","signature":"sig"}]}]}`, false, false},
		{"messages redacted thinking", "anthropic", `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`, false, false},
		{"responses encrypted reasoning", "openai", `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, true, false},
		{"responses visible reasoning", "openai", `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"thought"}]}]}`, true, false},
		{"gemini thinking signature", "gemini", `{"contents":[{"role":"model","parts":[{"text":"thought","thought":true,"thoughtSignature":"sig"}]}]}`, false, false},
		{"mixed reasoning and history", "openai", `{"previous_response_id":"resp-1","input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := detectState(tc.protocol, tc.responses, []byte(tc.body))
			if !state.present() {
				t.Fatalf("state carrier not detected: %+v", state)
			}
			if state.requiresOrigin() != tc.wantOrigin {
				t.Fatalf("requiresOrigin=%v, want %v: %+v", state.requiresOrigin(), tc.wantOrigin, state)
			}
		})
	}
	if (stateInfo{}).requiresOrigin() {
		t.Fatal("a stateless request must not require its origin")
	}
}

func TestDetectGeminiCachedContent(t *testing.T) {
	got := detectState("gemini", false, []byte(`{"cachedContent":"cachedContents/history"}`))
	if len(got.kinds) != 1 || got.kinds[0] != "gemini.cached_content" || got.oldest != "" || !got.requiresOrigin() {
		t.Fatalf("cached content state=%+v", got)
	}
	withThought := detectState("gemini", false, []byte(`{"cachedContent":"cachedContents/history","contents":[{"role":"model","parts":[{"thoughtSignature":"sig","functionCall":{"name":"f","args":{}}}]}]}`))
	if !withThought.requiresOrigin() || withThought.oldest != "sig" || len(withThought.kinds) != 2 {
		t.Fatalf("cached content must not replace thought fingerprint: %+v", withThought)
	}
	for _, body := range []string{
		`{}`, `{"cachedContent":""}`, `{"cachedContent":null}`,
		`{"contents":[{"parts":[{"functionCall":{"name":"f","args":{"cachedContent":"application-data"}}}]}]}`,
		`{"contents":[{"parts":[{"functionResponse":{"name":"f","response":{"cachedContent":"application-data"}}}]}]}`,
	} {
		if got := detectState("gemini", false, []byte(body)); got.present() || got.requiresOrigin() {
			t.Fatalf("non-carrier detected: %s %+v", body, got)
		}
	}
	if got := detectState("openai", false, []byte(`{"cachedContent":"not-in-this-schema"}`)); got.present() {
		t.Fatalf("cachedContent detected outside Gemini: %+v", got)
	}
}
