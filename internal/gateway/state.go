package gateway

import "encoding/json"

// Inspect the raw inbound schema for carried reasoning, history dependencies
// and tool activity before routing. Carried reasoning alone is not a reason
// to forbid another target: the converter preserves supported material and
// the upstream determines whether it accepts that material. External or
// compacted history remains dependent on its original source. See
// docs/design/state-and-layers.md and docs/design/protocol-conversion.md.

// stateInfo describes the provider-native state a request carries. kinds names
// the carriers; oldest is the first replayable value, used as an affinity
// fingerprint when the client gave no session identifier of its own. Such a
// fingerprint is a routing preference, not proof that all carriers require
// that source; requiresOrigin makes the narrower history distinction.
type stateInfo struct {
	kinds  []string
	oldest string
}

func (s stateInfo) present() bool { return len(s.kinds) > 0 }

// requiresOrigin distinguishes external/compacted history from carried
// reasoning. History dependencies need their original source to reconstruct
// context. Thinking/signatures alone may be sent to another permitted target;
// whether they are accepted is established by conversion and the upstream.
func (s stateInfo) requiresOrigin() bool {
	for _, kind := range s.kinds {
		switch kind {
		case "openai.previous_response_id", "openai.conversation", "openai.item_reference", "openai.encrypted_compaction", "gemini.cached_content":
			return true
		}
	}
	return false
}

// detectState scans an inbound body for provider-native state carriers. An
// empty result means no known continuation carrier was found; it does not
// establish that every request field is convertible.
func detectState(protocol string, responses bool, body []byte) stateInfo {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return stateInfo{}
	}
	var info stateInfo
	add := func(kind, value string) {
		for _, x := range info.kinds {
			if x == kind {
				return
			}
		}
		info.kinds = append(info.kinds, kind)
		if info.oldest == "" && value != "" {
			info.oldest = value
		}
	}
	switch protocol {
	case "anthropic":
		for _, m := range objects(root["messages"]) {
			for _, b := range objects(m["content"]) {
				switch b["type"] {
				case "thinking":
					if sig, ok := b["signature"].(string); ok {
						add("anthropic.thinking", sig)
					} else {
						add("anthropic.thinking", "")
					}
				case "redacted_thinking":
					if d, ok := b["data"].(string); ok {
						add("anthropic.redacted_thinking", d)
					} else {
						add("anthropic.redacted_thinking", "")
					}
				}
			}
		}
	case "openai":
		if responses {
			// previous_response_id is a rolling cursor: it changes every turn,
			// so it marks the request as stateful but must never be used as a
			// fingerprint. The replayable carrier is the encrypted reasoning
			// item, whose value stays in the history from the turn it appeared.
			if s, ok := root["previous_response_id"].(string); ok && s != "" {
				add("openai.previous_response_id", "")
			}
			// conversation is a server-side session handle, mutually exclusive
			// with previous_response_id. Like the cursor it marks the request
			// stateful but its value must not fingerprint: only the channel that
			// created the handle can resume it.
			if s, ok := root["conversation"].(string); ok && s != "" {
				add("openai.conversation", "")
			} else if conversation, ok := root["conversation"].(map[string]any); ok {
				if id, ok := conversation["id"].(string); ok && id != "" {
					add("openai.conversation", "")
				}
			}
			for _, item := range objects(root["input"]) {
				switch item["type"] {
				case "reasoning":
					if v, ok := item["encrypted_content"].(string); ok && v != "" {
						add("openai.encrypted_reasoning", v)
					} else {
						add("openai.reasoning", "")
					}
				case "compaction":
					if v, ok := item["encrypted_content"].(string); ok && v != "" {
						add("openai.encrypted_compaction", v)
					} else {
						// Its contents remain history even when this detector
						// cannot inspect the compaction payload's shape.
						add("openai.encrypted_compaction", "")
					}
				case "item_reference":
					if id, ok := item["id"].(string); ok && id != "" {
						add("openai.item_reference", "")
					}
				case nil:
					// The reference type is optional in the Responses input union.
					// A complete inline message may also carry an id, so only the
					// id-only shorthand refers to an item stored on the server.
					if id, ok := item["id"].(string); ok && id != "" && len(item) == 1 {
						add("openai.item_reference", "")
					}
				}
			}
		}
		for _, m := range objects(root["messages"]) {
			if v, ok := m["reasoning_content"].(string); ok && v != "" {
				add("openai.reasoning_content", v)
			}
		}
	case "gemini":
		// cachedContent can be the only copy of the prompt prefix, rather
		// than an optional cache hint. Do not fingerprint its external ID.
		if name, ok := root["cachedContent"].(string); ok && name != "" {
			add("gemini.cached_content", "")
		}
		for _, c := range objects(root["contents"]) {
			for _, p := range objects(c["parts"]) {
				if v, ok := p["thoughtSignature"].(string); ok && v != "" {
					add("gemini.thought_signature", v)
				}
				if b, ok := p["thought"].(bool); ok && b {
					add("gemini.thought", "")
				}
			}
		}
	}
	return info
}

// requestHasToolCalls reports whether the conversation contains tool activity.
func requestHasToolCalls(protocol string, body []byte) bool {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return false
	}
	switch protocol {
	case "anthropic":
		for _, m := range objects(root["messages"]) {
			for _, b := range objects(m["content"]) {
				switch b["type"] {
				case "tool_use", "tool_result", "server_tool_use":
					return true
				}
			}
		}
	case "openai":
		for _, m := range objects(root["messages"]) {
			if len(objects(m["tool_calls"])) > 0 {
				return true
			}
			if m["role"] == "tool" {
				return true
			}
		}
	case "gemini":
		for _, c := range objects(root["contents"]) {
			for _, p := range objects(c["parts"]) {
				if _, ok := p["functionCall"]; ok {
					return true
				}
				if _, ok := p["functionResponse"]; ok {
					return true
				}
			}
		}
	}
	return false
}

func objects(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
