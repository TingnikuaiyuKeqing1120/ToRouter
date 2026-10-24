package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"torouter/internal/store"
)

const reasoningProbePrefixLimit = 64 << 10

// probeReasoningRejection inspects at most the first complete non-comment SSE
// event. It never reads on after a created/start/content event to find an error.
// A rejection is a narrow validation-error observation, not general permission
// to replay: the caller still applies policy, removable-state and once-only
// limits. Neither a 2xx status, EOF nor lack of visible text proves no execution.
//
// replay always contains every byte consumed here, including transport read-
// ahead, and delegates Close to body. An observed read error is returned and is
// also replayed after its prefix; callers must not discard it as an empty stream.
// Reaching the byte limit simply abandons detection. Waiting for the first
// event remains subject to the original body's cancellation/read deadline.
func probeReasoningRejection(body io.ReadCloser, format string) (replay io.ReadCloser, rejection []byte, err error) {
	switch format {
	case store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent:
	default:
		return body, nil, nil
	}
	prefix := make([]byte, 0, 4096)
	lineStart, scan := 0, 0
	event := ""
	var data []string
	nonComment, unknownField := false, false
	buffer := make([]byte, 4096)
	for len(prefix) < reasoningProbePrefixLimit {
		readSize := min(len(buffer), reasoningProbePrefixLimit-len(prefix))
		n, readErr := body.Read(buffer[:readSize])
		prefix = append(prefix, buffer[:n]...)
		if readErr != nil {
			return reasoningProbeReplay(body, prefix, readErr), nil, readErr
		}
		if n == 0 {
			// A non-progressing reader should not make this optional probe loop.
			return reasoningProbeReplay(body, prefix, nil), nil, nil
		}
		for scan < len(prefix) {
			if prefix[scan] != '\n' {
				scan++
				continue
			}
			line := strings.TrimSuffix(string(prefix[lineStart:scan]), "\r")
			scan++
			lineStart = scan
			if line == "" {
				if nonComment {
					replay = reasoningProbeReplay(body, prefix, nil)
					if !unknownField {
						rejection = reasoningProbeEvent(event, strings.Join(data, "\n"), format)
					}
					return replay, rejection, nil
				}
				continue // Comment-only records are keepalive, not response activity.
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			nonComment = true
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event = value
			case "data":
				data = append(data, value)
			case "id", "retry":
			default:
				unknownField = true
			}
		}
	}
	return reasoningProbeReplay(body, prefix, nil), nil, nil
}

func reasoningProbeEvent(event, data, format string) []byte {
	var root map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &root) != nil || root == nil {
		return nil
	}
	typ := recoveryString(root["type"])
	if event != "" && event != "error" && event != "response.failed" {
		return nil
	}
	if typ != "" && typ != "error" && typ != "response.failed" {
		return nil
	}
	if event != "" && typ != "" && event != typ {
		return nil
	}
	if reasoningProbeActivity(root) {
		return nil
	}
	rawError := root["error"]
	if event == "response.failed" || typ == "response.failed" {
		if format != store.FormatOpenAIResponses {
			return nil
		}
		var response map[string]json.RawMessage
		if json.Unmarshal(root["response"], &response) != nil || response == nil || reasoningProbeActivity(response) {
			return nil
		}
		if status := recoveryString(response["status"]); status != "" && status != "failed" {
			return nil
		}
		rawError = response["error"]
	} else if _, hasResponse := root["response"]; hasResponse {
		return nil // Do not infer a generic error's nested response is unexecuted.
	}
	var upstreamError map[string]json.RawMessage
	if json.Unmarshal(rawError, &upstreamError) != nil || upstreamError == nil {
		return nil
	}
	// Normalize only recognized error fields. Gemini's numeric error.code is an
	// HTTP-style code, not a structured reasoning code; its message can still
	// explicitly identify a thought-signature validation rejection.
	normalized := map[string]string{}
	for _, key := range []string{"code", "type", "param", "message"} {
		if value := recoveryString(upstreamError[key]); value != "" {
			normalized[key] = value
		}
	}
	envelope, marshalErr := json.Marshal(map[string]any{"error": normalized})
	if marshalErr != nil {
		return nil
	}
	// The event guard above is the narrow stream validation-rejection boundary;
	// 400 selects the existing validation classifier, not the HTTP stream status.
	if !isReasoningCompatibilityRejection(400, envelope) {
		return nil
	}
	return envelope
}

// Only inspect protocol activity paths. Never interpret an error's message or
// a nested function argument as execution evidence. Empty output arrays are
// normal in response.failed, whereas any usage object, delta or tool activity
// makes a replay unsafe even when it contains no final text yet.
func reasoningProbeActivity(obj map[string]json.RawMessage) bool {
	for _, key := range []string{"usage", "usage_metadata", "usageMetadata", "input_tokens", "output_tokens", "prompt_tokens", "completion_tokens", "delta", "tool_calls", "tool_call", "function_call", "function_call_output", "functionCall", "functionResponse", "tool_use", "tool_result", "output_index", "content_block"} {
		if raw, ok := obj[key]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true
		}
	}
	for _, key := range []string{"output", "content", "message", "completion", "text", "output_text", "reasoning_content", "choices", "candidates", "parts"} {
		if raw, ok := obj[key]; ok {
			trimmed := bytes.TrimSpace(raw)
			if bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
				continue
			}
			var list []json.RawMessage
			if json.Unmarshal(raw, &list) == nil && len(list) == 0 {
				continue
			}
			return true
		}
	}
	return false
}

func reasoningProbeReplay(body io.ReadCloser, prefix []byte, readErr error) io.ReadCloser {
	readers := []io.Reader{bytes.NewReader(prefix)}
	if readErr != nil {
		readers = append(readers, &reasoningProbeReadError{err: readErr})
	}
	readers = append(readers, body)
	return &reasoningProbeReadCloser{Reader: io.MultiReader(readers...), body: body}
}

type reasoningProbeReadCloser struct {
	io.Reader
	body io.ReadCloser
}

func (r *reasoningProbeReadCloser) Close() error { return r.body.Close() }

type reasoningProbeReadError struct{ err error }

func (r *reasoningProbeReadError) Read([]byte) (int, error) {
	if r.err == nil {
		return 0, io.EOF
	}
	err := r.err
	r.err = nil
	return 0, err
}
