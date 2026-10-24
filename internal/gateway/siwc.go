package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"torouter/internal/store"
)

// The SIWC profile is not the general OpenAI API: it accepts one endpoint, it
// keeps no server-side state, and it refuses a fixed list of request
// parameters (docs/protocol-refs/oauth-openai-siwc.md, Preview 限制). A
// ChatGPT-plan credential is not a general-purpose API key, and a request that
// silently lost its parameters would be a different request than the one the
// client sent — so the gateway enforces the profile on the way out and records
// what it stripped.

// omitParams is the omit list verbatim. Each stripped name is reported as a
// degradation, so a caller can tell why a parameter it set had no effect.
var omitParams = []string{
	"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata",
	"moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier",
	"temperature", "top_logprobs", "top_p", "truncation", "user",
}

// ErrServerStateUnsupported is returned when a request names server-side state
// the profile does not have. Refusing is the point: dropping
// previous_response_id would silently answer a different question than the one
// asked, and every other stateless failure in this gateway is visible.
var ErrServerStateUnsupported = errors.New("the chatgpt_oauth profile has no server-side state")

// ProfileRequestBody rewrites a Responses request for the SIWC profile:
// forces stream and store:false, strips the omit list, and refuses a request
// that carries server-side state. stripped names every parameter removed, so
// the caller can record the degradation.
func ProfileRequestBody(body []byte) (out []byte, stripped, adapted []string, err error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, nil, nil, errors.New("request must be a JSON object")
	}
	if _, ok := obj["previous_response_id"]; ok {
		return nil, nil, nil, ErrServerStateUnsupported
	}
	if _, ok := obj["conversation"]; ok {
		return nil, nil, nil, ErrServerStateUnsupported
	}
	for _, param := range omitParams {
		if _, ok := obj[param]; ok {
			delete(obj, param)
			stripped = append(stripped, param)
		}
	}
	adapted, err = adaptSystemInput(obj)
	if err != nil {
		return nil, nil, nil, err
	}
	obj["stream"] = json.RawMessage("true")
	obj["store"] = json.RawMessage("false")
	out, err = json.Marshal(obj)
	if err != nil {
		return nil, nil, nil, err
	}
	return out, stripped, adapted, nil
}

// adaptSystemInput rewrites a {type:"message", role:"system"} input item into a
// developer message. The profile rejects the system item outright, and the
// documented substitutes are instructions or a developer item — so this is an
// adaptation with a basis, not a guess: the developer role is the Responses
// API's own name for instructions the model follows, and the item's text is
// carried across unchanged.
//
// The boundary is recorded: the caller sees siwc_adapt:system_to_developer in
// the usage row, because a consumer that treats "who spoke" as meaningful would
// see a different speaker than the one the client wrote. Items that are not a
// plain system message are left alone, and an item whose shape cannot be read
// fails the request rather than being silently rewritten.
func adaptSystemInput(obj map[string]json.RawMessage) ([]string, error) {
	raw, ok := obj["input"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		// A string input is a shorthand the profile accepts as-is; only the
		// array form can carry a system item.
		return nil, nil
	}
	adapted := []string{}
	for i, item := range items {
		var shape struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(item, &shape); err != nil {
			return nil, errors.New("input item could not be read")
		}
		if shape.Type != "message" || shape.Role != "system" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, errors.New("system input item could not be read")
		}
		fields["role"] = json.RawMessage(`"developer"`)
		rewritten, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		items[i] = rewritten
		adapted = append(adapted, "system_to_developer")
	}
	if len(adapted) == 0 {
		return nil, nil
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	obj["input"] = out
	return adapted, nil
}

// strippedDegradations names each omitted parameter the way the usage record
// reports losses, so a caller can tell why a parameter it set had no effect.
func strippedDegradations(params []string) []string {
	out := make([]string, 0, len(params))
	for _, p := range params {
		out = append(out, "siwc_omit:"+p)
	}
	return out
}

// adaptedDegradations names the rewrites the profile forced, the same way.
func adaptedDegradations(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "siwc_adapt:"+name)
	}
	return out
}

// bufferChatGPT reads the profile's mandatory SSE stream and reassembles the
// terminal Response object, for a client that asked for a non-streaming answer
// on a channel that can only stream.
//
// The upstream already ran the request by the time this reads anything, so a
// failure here is reported the way every other post-commit failure is: the
// attempt ends, nothing is resent, and the breaker hears about the line.
func bufferChatGPT(body io.Reader, contentType string) ([]byte, error) {
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return nil, errors.New("the chatgpt_oauth profile did not return an SSE stream")
	}
	usage := store.Usage{}
	observer := newObserver(io.LimitReader(body, maxBody+1), store.FormatOpenAIResponses, &usage)
	raw, err := io.ReadAll(observer)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBody {
		return nil, errors.New("upstream stream exceeds 16 MiB")
	}
	if observer.responseTerminal == "error" {
		return nil, errors.New("upstream stream error")
	}
	if !observer.complete || len(observer.responseBody) == 0 {
		return nil, errors.New("upstream stream ended without a completion event")
	}
	return observer.responseBody, nil
}

// siwcStatelessRecovery explains a refusal the SIWC profile forces: the request
// named state the endpoint does not keep, so it cannot be served here at all —
// not now, and not on a retry.
func siwcStatelessRecovery() *recovery {
	return &recovery{
		Code:    "siwc_profile_stateless",
		Actions: []string{"start_fresh_session", "handoff"},
		Detail:  "the chatgpt_oauth profile keeps no server-side state: previous_response_id and conversation cannot be served, send the full conversation in input instead",
	}
}
