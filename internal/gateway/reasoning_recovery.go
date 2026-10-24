package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"torouter/internal/store"
)

var reasoningStateParameter = regexp.MustCompile(`^(reasoning_content|thoughtsignature|thought_signature|messages(\[[0-9]+\]|\.[0-9]+)\.reasoning_content|contents(\[[0-9]+\]|\.[0-9]+)\.parts(\[[0-9]+\]|\.[0-9]+)\.thoughtsignature)$`)

// isReasoningCompatibilityRejection recognizes a validation rejection, not a
// general retry permission. The caller must also establish that execution and
// output have not begun, that the request contains removable reasoning, and
// that this logical request has not already used its one recovery attempt.
// In particular, an HTTP 200 stream error must not be passed as an invented 400.
func isReasoningCompatibilityRejection(status int, body []byte) bool {
	if status != 400 && status != 422 {
		return false
	}
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Error == nil {
		return false
	}
	e := envelope.Error
	code := strings.ToLower(e.Code)
	if code == "" {
		code = strings.ToLower(e.Type)
	}
	switch code {
	case "invalid_encrypted_content", "invalid_reasoning_content", "unsupported_reasoning_content",
		"invalid_thinking_signature", "invalid_thought_signature", "invalid_redacted_thinking":
		return true
	}
	// Only known state fields qualify. A generic signature can be an auth
	// signature, and thinking budget / effort errors are not replay-state errors.
	param := strings.ToLower(e.Param)
	switch code {
	case "unsupported_parameter", "unknown_parameter", "unrecognized_request_argument", "invalid_parameter":
		if param != "" {
			return reasoningStateParameter.MatchString(param)
		}
	}
	message := strings.ToLower(e.Message)
	message = strings.NewReplacer("thoughtsignature", "thought signature", "thought_signature", "thought signature", "`", "", "\"", "", "'", "").Replace(message)
	rejected := containsRecoveryPhrase(message, "invalid", "unsupported", "not supported", "unrecognized", "incompatible", "cannot decrypt", "could not decrypt", "failed to verify", "cannot verify", "signature verification failed", "corrupted signature", "malformed signature")
	if !rejected {
		return false
	}
	if containsRecoveryPhrase(message, "thinking signature", "thought signature", "reasoning_content", "encrypted_content", "redacted_thinking") {
		return true
	}
	return strings.Contains(message, "thinking block") &&
		containsRecoveryPhrase(message, "invalid signature", "signature is invalid", "signature verification failed", "corrupted signature", "malformed signature", "unsupported", "not supported")
}

func containsRecoveryPhrase(text string, phrases ...string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

type recoveryObject = map[string]json.RawMessage

// stripReasoning removes only native replay-state carriers in the selected
// schema. It never descends into text, function arguments or tool results.
// Removed contains fixed field categories, never values from the request.
//
// A message made empty by removing all of its thinking blocks is omitted; no
// replacement assistant text is invented. Existing Chat empty text and all
// tool calls remain. If omitting a message would discard unknown sibling
// payload, or all input would disappear, the operation fails atomically.
// Responses may retain an empty input when a server-side history reference is
// still present. Errors return the original bytes and no removal claims.
func stripReasoning(body []byte, format string) ([]byte, []string, error) {
	var root recoveryObject
	if json.Unmarshal(body, &root) != nil || root == nil {
		return body, nil, errors.New("reasoning recovery requires a JSON request object")
	}
	var field string
	switch format {
	case store.FormatOpenAIChatCompletions, store.FormatAnthropicMessages:
		field = "messages"
	case store.FormatGeminiGenerateContent:
		field = "contents"
	case store.FormatOpenAIResponses:
		field = "input"
	default:
		return body, nil, errors.New("reasoning recovery does not support this request format")
	}
	raw, exists := root[field]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return body, nil, nil
	}
	if format == store.FormatOpenAIResponses && len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '"' {
		return body, nil, nil // Responses also accepts a plain input string.
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return body, nil, errors.New("reasoning recovery requires a native input list")
	}
	removed := []string{}
	mark := func(path string) {
		for _, prior := range removed {
			if prior == path {
				return
			}
		}
		removed = append(removed, path)
	}
	kept := make([]json.RawMessage, 0, len(items))
	for _, rawItem := range items {
		var item recoveryObject
		if json.Unmarshal(rawItem, &item) != nil || item == nil {
			kept = append(kept, rawItem)
			continue
		}
		changed, omit := false, false
		switch format {
		case store.FormatOpenAIChatCompletions:
			if recoveryString(item["role"]) == "assistant" {
				if recoveryPresent(item["reasoning_content"]) {
					delete(item, "reasoning_content")
					mark("messages.reasoning_content")
					changed = true
					omit = !recoveryChatContent(item["content"]) && !recoveryNonemptyArray(item["tool_calls"]) && !recoveryPresent(item["function_call"])
					if omit && !recoveryOnlyKeys(item, "role", "content", "name") {
						return body, nil, errors.New("reasoning recovery cannot omit an assistant message with other payload")
					}
				}
			}
		case store.FormatAnthropicMessages:
			if recoveryString(item["role"]) != "assistant" {
				break
			}
			var blocks []json.RawMessage
			if json.Unmarshal(item["content"], &blocks) != nil {
				break // String content and unknown extensions are left untouched.
			}
			remaining := make([]json.RawMessage, 0, len(blocks))
			for _, block := range blocks {
				var obj recoveryObject
				_ = json.Unmarshal(block, &obj)
				typ := recoveryString(obj["type"])
				if typ == "thinking" || typ == "redacted_thinking" {
					if !recoveryOnlyKeys(obj, "type", "thinking", "signature", "data", "cache_control") {
						return body, nil, errors.New("reasoning recovery cannot discard a thinking block with other payload")
					}
					mark("messages.content." + typ)
					changed = true
					continue
				}
				remaining = append(remaining, block)
			}
			if changed {
				item["content"], _ = json.Marshal(remaining)
				omit = len(remaining) == 0
				if omit && !recoveryOnlyKeys(item, "role", "content") {
					return body, nil, errors.New("reasoning recovery cannot omit an assistant message with other payload")
				}
			}
		case store.FormatGeminiGenerateContent:
			var parts []json.RawMessage
			if json.Unmarshal(item["parts"], &parts) != nil {
				break
			}
			remaining := make([]json.RawMessage, 0, len(parts))
			for _, rawPart := range parts {
				var part recoveryObject
				if json.Unmarshal(rawPart, &part) != nil || part == nil {
					remaining = append(remaining, rawPart)
					continue
				}
				partChanged := false
				if bytes.Equal(bytes.TrimSpace(part["thought"]), []byte("true")) {
					mark("contents.parts.thought")
					changed = true
					// A real call/result or unknown payload must survive even if
					// an upstream attached a thought marker to the same part.
					if recoveryOnlyKeys(part, "thought", "thoughtSignature", "text") {
						if _, ok := part["thoughtSignature"]; ok {
							mark("contents.parts.thoughtSignature")
						}
						continue
					}
					delete(part, "thought")
					partChanged = true
				}
				if _, ok := part["thoughtSignature"]; ok {
					delete(part, "thoughtSignature")
					mark("contents.parts.thoughtSignature")
					changed, partChanged = true, true
				}
				if partChanged {
					if len(part) == 0 {
						continue
					}
					rawPart, _ = json.Marshal(part)
				}
				remaining = append(remaining, rawPart)
			}
			if changed {
				item["parts"], _ = json.Marshal(remaining)
				omit = len(remaining) == 0
				if omit && !recoveryOnlyKeys(item, "role", "parts") {
					return body, nil, errors.New("reasoning recovery cannot omit a content message with other payload")
				}
			}
		case store.FormatOpenAIResponses:
			if recoveryString(item["type"]) == "reasoning" {
				if !recoveryOnlyKeys(item, "type", "id", "summary", "content", "encrypted_content", "status") {
					return body, nil, errors.New("reasoning recovery cannot discard a reasoning item with other payload")
				}
				mark("input.reasoning")
				omit = true
			}
		}
		if omit {
			continue
		}
		if changed {
			rawItem, _ = json.Marshal(item)
		}
		kept = append(kept, rawItem)
	}
	if len(removed) == 0 {
		return body, nil, nil
	}
	var conversation recoveryObject
	_ = json.Unmarshal(root["conversation"], &conversation)
	hasHistory := format == store.FormatOpenAIResponses && (recoveryString(root["previous_response_id"]) != "" || recoveryString(root["conversation"]) != "" || recoveryString(conversation["id"]) != "")
	if len(kept) == 0 && !hasHistory {
		return body, nil, errors.New("reasoning recovery would leave no input or history")
	}
	root[field], _ = json.Marshal(kept)
	result, err := json.Marshal(root)
	if err != nil {
		return body, nil, errors.New("reasoning recovery could not encode the request")
	}
	return result, removed, nil
}

func recoveryString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func recoveryNonemptyArray(raw json.RawMessage) bool {
	var values []json.RawMessage
	return json.Unmarshal(raw, &values) == nil && len(values) > 0
}

func recoveryChatContent(raw json.RawMessage) bool {
	// Unknown content shapes still carry payload. Preserve upstream validation
	// rather than silently deleting a possible message body.
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return false
	}
	if value[0] == '[' {
		return recoveryNonemptyArray(value)
	}
	return true
}

func recoveryPresent(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return len(value) != 0 && !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte(`""`))
}

func recoveryOnlyKeys(obj recoveryObject, keys ...string) bool {
	for key := range obj {
		found := false
		for _, allowed := range keys {
			if key == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
