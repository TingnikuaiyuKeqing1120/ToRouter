// Package streamreply assembles native SSE into the same protocol's JSON reply.
// It does not translate protocols or invent a missing terminal event.
package streamreply

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"torouter/internal/store"
)

// This reducer only reconstructs the same protocol. It does not convert a
// response through another protocol, drop reasoning or rename client tools.
func Assemble(input io.Reader, contentType, format string) ([]byte, error) {
	return assemble(input, contentType, format, false)
}

// AssembleChat also accepts providers that end a complete Chat response with
// normal EOF after every choice's finish_reason. A truncated frame, read error
// or unfinished choice still fails; no DONE marker or usage is synthesized.
func AssembleChat(input io.Reader, contentType string) ([]byte, error) {
	return assemble(input, contentType, store.FormatOpenAIChatCompletions, true)
}

func assemble(input io.Reader, contentType, format string, completedChatEOF bool) ([]byte, error) {
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return nil, errors.New("expected an SSE response")
	}
	limited := &io.LimitedReader{R: input, N: 16<<20 + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	state := &reducer{format: format, body: map[string]any{}, choices: map[int]map[string]any{}, calls: map[int]map[int]map[string]any{}, blocks: map[int]map[string]any{}, inputs: map[int]string{}, open: map[int]bool{}}
	var data []string
	event := ""
	dispatch := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		name := event
		event = ""
		if raw == "[DONE]" {
			if format != store.FormatOpenAIChatCompletions {
				return errors.New("unexpected DONE")
			}
			state.terminal = true
			return nil
		}
		if state.terminal {
			return errors.New("data after terminal event")
		}
		var value map[string]any
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil || value == nil {
			return errors.New("malformed SSE JSON")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return errors.New("trailing SSE JSON")
		}
		if value["error"] != nil || name == "error" || value["type"] == "error" {
			return errors.New("upstream stream reported an error")
		}
		return state.apply(name, value)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data = append(data, value)
		case "event":
			event = value
		case "id", "retry":
		default:
			return nil, errors.New("unknown SSE field")
		}
	}
	if limited.N == 0 {
		return nil, errors.New("stream exceeds 16 MiB")
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(data) > 0 || event != "" {
		return nil, errors.New("truncated SSE event")
	}
	if !state.terminal && !(completedChatEOF && state.completedChat()) {
		return nil, errors.New("stream ended without terminal event")
	}
	return state.finish()
}

func (r *reducer) completedChat() bool {
	if len(r.choices) == 0 {
		return false
	}
	for _, choice := range r.choices {
		reason, ok := choice["finish_reason"].(string)
		if !ok || reason == "" {
			return false
		}
	}
	return true
}

type reducer struct {
	format   string
	body     map[string]any
	choices  map[int]map[string]any
	calls    map[int]map[int]map[string]any
	blocks   map[int]map[string]any
	inputs   map[int]string
	open     map[int]bool
	terminal bool
}

func object(value any) map[string]any { m, _ := value.(map[string]any); return m }
func position(value any) (int, error) {
	if value == nil {
		return 0, nil
	}
	n, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("invalid stream index")
	}
	i, err := n.Int64()
	if err != nil || i < 0 || i > 65535 {
		return 0, errors.New("invalid stream index")
	}
	return int(i), nil
}
func appendText(target map[string]any, key string, value any) error {
	if value == nil {
		return nil
	}
	s, ok := value.(string)
	if !ok {
		return errors.New("non-text stream delta")
	}
	old, _ := target[key].(string)
	target[key] = old + s
	return nil
}
func ordered(values map[int]map[string]any) []any {
	keys := make([]int, 0, len(values))
	for i := range values {
		keys = append(keys, i)
	}
	sort.Ints(keys)
	out := make([]any, 0, len(keys))
	for _, i := range keys {
		out = append(out, values[i])
	}
	return out
}
func merge(target map[string]any, source map[string]any) {
	for k, v := range source {
		target[k] = v
	}
}

func (r *reducer) apply(event string, v map[string]any) error {
	switch r.format {
	case store.FormatOpenAIResponses:
		typ, _ := v["type"].(string)
		if event != "" && typ != event {
			return errors.New("response event/type mismatch")
		}
		switch typ {
		case "response.failed", "response.cancelled", "response.error":
			return errors.New("response did not complete")
		case "response.completed", "response.incomplete":
			final := object(v["response"])
			if final == nil {
				return errors.New("terminal response missing")
			}
			if final["status"] != strings.TrimPrefix(typ, "response.") {
				return errors.New("terminal response status mismatch")
			}
			r.body = final
			r.terminal = true
		}
		return nil
	case store.FormatAnthropicMessages:
		return r.message(event, v)
	case store.FormatOpenAIChatCompletions:
		return r.chat(v)
	default:
		return errors.New("unsupported stream protocol")
	}
}
func (r *reducer) chat(v map[string]any) error {
	for k, value := range v {
		if k != "choices" && k != "object" {
			r.body[k] = value
		}
	}
	raw, ok := v["choices"]
	if !ok {
		return nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return errors.New("invalid choices")
	}
	for _, part := range parts {
		p := object(part)
		if p == nil {
			return errors.New("invalid choice")
		}
		i, err := position(p["index"])
		if err != nil {
			return err
		}
		choice := r.choices[i]
		if choice == nil {
			choice = map[string]any{"index": i, "message": map[string]any{"role": "assistant", "content": nil}, "finish_reason": nil}
			r.choices[i] = choice
			r.calls[i] = map[int]map[string]any{}
		}
		msg := object(choice["message"])
		for k, value := range p {
			if k != "delta" && k != "index" && k != "finish_reason" && k != "logprobs" {
				choice[k] = value
			}
		}
		delta := object(p["delta"])
		if p["delta"] != nil && delta == nil {
			return errors.New("invalid chat delta")
		}
		for key, value := range delta {
			switch key {
			case "role":
				msg[key] = value
			case "content", "reasoning_content", "reasoning", "refusal":
				if err := appendText(msg, key, value); err != nil {
					return err
				}
			case "function_call":
				f := object(value)
				if f == nil {
					return errors.New("invalid function delta")
				}
				target := object(msg[key])
				if target == nil {
					target = map[string]any{}
					msg[key] = target
				}
				for k, v := range f {
					if k != "name" && k != "arguments" {
						return errors.New("unknown function delta")
					}
					if err := appendText(target, k, v); err != nil {
						return err
					}
				}
			case "tool_calls":
				calls, ok := value.([]any)
				if !ok {
					return errors.New("invalid tool deltas")
				}
				for _, raw := range calls {
					call := object(raw)
					if call == nil {
						return errors.New("invalid tool delta")
					}
					n, err := position(call["index"])
					if err != nil {
						return err
					}
					target := r.calls[i][n]
					if target == nil {
						target = map[string]any{"type": "function", "function": map[string]any{}}
						r.calls[i][n] = target
					}
					for k, v := range call {
						switch k {
						case "index":
						case "id", "type":
							target[k] = v
						case "function":
							f := object(v)
							if f == nil {
								return errors.New("invalid tool function")
							}
							for name, text := range f {
								if name != "name" && name != "arguments" {
									return errors.New("unknown function delta")
								}
								if err := appendText(object(target["function"]), name, text); err != nil {
									return err
								}
							}
						default:
							return errors.New("unknown tool delta")
						}
					}
				}
			default:
				return fmt.Errorf("cannot preserve chat delta %q in a non-stream response", key)
			}
		}
		if p["finish_reason"] != nil {
			choice["finish_reason"] = p["finish_reason"]
		}
		if logs := object(p["logprobs"]); logs != nil {
			target := object(choice["logprobs"])
			if target == nil {
				target = map[string]any{}
				choice["logprobs"] = target
			}
			for k, v := range logs {
				if list, ok := v.([]any); ok {
					old, _ := target[k].([]any)
					target[k] = append(old, list...)
				} else {
					target[k] = v
				}
			}
		}
	}
	return nil
}
func (r *reducer) message(event string, v map[string]any) error {
	typ, _ := v["type"].(string)
	if event != "" && event != typ {
		return errors.New("message event/type mismatch")
	}
	switch typ {
	case "ping":
	case "message_start":
		if len(r.body) != 0 {
			return errors.New("duplicate message_start")
		}
		r.body = object(v["message"])
		if r.body == nil {
			return errors.New("missing message")
		}
		if initial, ok := r.body["content"].([]any); ok {
			for i, value := range initial {
				block := object(value)
				if block == nil {
					return errors.New("invalid initial content block")
				}
				r.blocks[i] = block
			}
		}
	case "content_block_start":
		i, err := position(v["index"])
		if err != nil {
			return err
		}
		block := object(v["content_block"])
		if block == nil || r.blocks[i] != nil {
			return errors.New("invalid content block")
		}
		r.blocks[i] = block
		r.open[i] = true
	case "content_block_delta":
		i, err := position(v["index"])
		if err != nil {
			return err
		}
		block, delta := r.blocks[i], object(v["delta"])
		if block == nil || delta == nil || !r.open[i] {
			return errors.New("delta without block")
		}
		switch delta["type"] {
		case "text_delta":
			return appendText(block, "text", delta["text"])
		case "thinking_delta":
			return appendText(block, "thinking", delta["thinking"])
		case "signature_delta":
			return appendText(block, "signature", delta["signature"])
		case "input_json_delta":
			if initial := object(block["input"]); len(initial) != 0 {
				return errors.New("streamed input would replace existing tool input")
			}
			text, ok := delta["partial_json"].(string)
			if !ok {
				return errors.New("invalid tool input delta")
			}
			r.inputs[i] += text
		case "citations_delta":
			citation, ok := delta["citation"]
			if !ok {
				return errors.New("invalid citation delta")
			}
			list, _ := block["citations"].([]any)
			block["citations"] = append(list, citation)
		default:
			return errors.New("unsupported message delta")
		}
	case "content_block_stop":
		i, err := position(v["index"])
		if err != nil {
			return err
		}
		if !r.open[i] {
			return errors.New("stop without block")
		}
		delete(r.open, i)
	case "message_delta":
		merge(r.body, object(v["delta"]))
		usage := object(r.body["usage"])
		if usage == nil {
			usage = map[string]any{}
		}
		merge(usage, object(v["usage"]))
		r.body["usage"] = usage
	case "message_stop":
		if r.body["stop_reason"] == nil || r.body["stop_reason"] == "" || len(r.open) != 0 {
			return errors.New("message_stop without stop reason")
		}
		r.terminal = true
	default:
		return fmt.Errorf("unsupported message event %q", typ)
	}
	return nil
}
func (r *reducer) finish() ([]byte, error) {
	switch r.format {
	case store.FormatOpenAIChatCompletions:
		if len(r.choices) == 0 {
			return nil, errors.New("no completed chat choice")
		}
		for i, c := range r.choices {
			if c["finish_reason"] == nil {
				return nil, errors.New("unfinished chat choice")
			}
			if len(r.calls[i]) > 0 {
				object(c["message"])["tool_calls"] = ordered(r.calls[i])
			}
		}
		r.body["object"] = "chat.completion"
		r.body["choices"] = ordered(r.choices)
	case store.FormatAnthropicMessages:
		for i, raw := range r.inputs {
			var value any
			d := json.NewDecoder(bytes.NewBufferString(raw))
			d.UseNumber()
			if d.Decode(&value) != nil {
				return nil, errors.New("malformed streamed tool input")
			}
			var extra any
			if d.Decode(&extra) != io.EOF {
				return nil, errors.New("trailing tool input")
			}
			r.blocks[i]["input"] = value
		}
		r.body["content"] = ordered(r.blocks)
	}
	return json.Marshal(r.body)
}
