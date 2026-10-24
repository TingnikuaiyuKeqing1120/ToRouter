package convert

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Bounds both physical lines and assembled events, including multi-line data.
type sseReader struct{ br *bufio.Reader }

func newSSEReader(r io.Reader) *sseReader { return &sseReader{bufio.NewReaderSize(r, 64<<10)} }
func (s *sseReader) line() (string, error) {
	var b []byte
	for {
		v, e := s.br.ReadSlice('\n')
		if len(b)+len(v) > maxLineLen {
			return "", ErrLineLimit
		}
		b = append(b, v...)
		if e == bufio.ErrBufferFull {
			continue
		}
		if e != nil {
			if e == io.EOF && len(b) > 0 {
				return "", ErrTruncated
			}
			return "", e
		}
		return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
	}
}
func (s *sseReader) next() (string, string, error) {
	var data []string
	event := ""
	size := 0
	pending := false
	for {
		line, e := s.line()
		if e != nil {
			if e == io.EOF && pending {
				e = ErrTruncated
			}
			return "", "", e
		}
		if line == "" {
			if len(data) > 0 {
				return event, strings.Join(data, "\n"), nil
			}
			event = ""
			pending = false
			size = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		pending = true
		size += len(line)
		if size > maxLineLen {
			return "", "", ErrLineLimit
		}
		k, v, _ := strings.Cut(line, ":")
		v = strings.TrimPrefix(v, " ")
		switch k {
		case "data":
			data = append(data, v)
		case "event":
			event = v
		case "id", "retry":
		default:
			return "", "", unsupportedf("SSE field %q", k)
		}
	}
}

type emitter struct {
	prefixParts       int
	prefixText        bool
	prefixReasoning   bool
	responseBytes     int
	responseTextIndex int
	responseTextOpen  bool
	responseTextValue strings.Builder
	sequence          int
	opts              Options
	reasoningPending  []irPart
	responseOutput    []any
	to                string
	w                 io.Writer
	model, id         string
	started           bool
	block, toolIndex  int
	usage             irUsage
	hasUsage          bool
	dropped           []string
	// pending keeps tools and subsequent state in native order until the
	// message terminal. Earlier text is still emitted incrementally.
	pending []irPart
}

// drop records a lossy adaptation during streaming and, on the first one,
// writes an SSE comment so the client can see it. A comment line is legal
// anywhere in an SSE stream and clients ignore it; doing it here rather than
// after the stream ends is the whole point, because end() always calls start()
// and any "before the body" test would never fire.
func (e *emitter) drop(kind string) {
	for _, k := range e.dropped {
		if k == kind {
			return
		}
	}
	e.dropped = append(e.dropped, kind)
	if len(e.dropped) == 1 {
		_ = e.comment("x-to-router-degraded: " + kind)
	}
}

// comment writes an SSE comment line. Clients ignore comment lines, which makes
// this the only way to report a mid-stream degradation after headers are sent.
func (e *emitter) comment(text string) error {
	return e.raw(":" + text + "\n\n")
}

func newEmitter(to string, w io.Writer, model string) (*emitter, error) {
	return &emitter{to: to, w: w, model: model, id: "chatcmpl-converted"}, nil
}
func (e *emitter) raw(s string) error {
	n, err := io.WriteString(e.w, s)
	if err != nil {
		return err
	}
	if n != len(s) {
		return io.ErrShortWrite
	}
	if f, ok := e.w.(interface{ Flush() }); ok {
		f.Flush()
	}
	return nil
}
func (e *emitter) event(name string, v any) error {
	if e.to == Responses {
		if m, ok := v.(map[string]any); ok {
			m["sequence_number"] = e.sequence
			e.sequence++
		}
	}
	b, err := json.Marshal(v)
	if len(b) > maxLineLen-1024 {
		return ErrLineLimit
	}
	if err != nil {
		return err
	}
	prefix := ""
	if name != "" {
		prefix = "event: " + name + "\n"
	}
	return e.raw(prefix + "data: " + string(b) + "\n\n")
}
func (e *emitter) chunk(delta object, finish any) object {
	return object{"id": e.id, "object": "chat.completion.chunk", "created": 0, "model": e.model, "choices": []any{object{"index": 0, "delta": delta, "finish_reason": finish}}}
}
func (e *emitter) start() error {
	if e.started {
		return nil
	}
	e.started = true
	switch e.to {
	case Responses:
		return e.event("response.created", object{"type": "response.created", "response": object{"id": e.id, "object": "response", "model": e.model, "status": "in_progress", "output": []any{}}})
	case OpenAI:
		return e.event("", e.chunk(object{"role": "assistant"}, nil))
	case Anthropic:
		return e.event("message_start", object{"type": "message_start", "message": object{"id": e.id, "type": "message", "role": "assistant", "model": e.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usageOut(e.usage, Anthropic)}})
	}
	return nil
}
func (e *emitter) text(s string) error {
	if s != "" && len(e.pending) > 0 {
		if e.to == OpenAI {
			return unsupportedf("text after tool call cannot retain Chat ordering")
		}
		e.pending = append(e.pending, irPart{Text: s})
		return nil
	}
	if err := e.start(); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	if e.to != OpenAI || !e.prefixText {
		e.prefixParts++
		e.prefixText = true
	}
	switch e.to {
	case Responses:
		return e.responseText(s)
	case OpenAI:
		return e.event("", e.chunk(object{"content": s}, nil))
	case Gemini:
		return e.event("", object{"modelVersion": e.model, "responseId": e.id, "candidates": []any{object{"index": 0, "content": object{"role": "model", "parts": []any{object{"text": s}}}}}})
	case Anthropic:
		i := e.block
		e.block++
		if err := e.event("content_block_start", object{"type": "content_block_start", "index": i, "content_block": object{"type": "text", "text": ""}}); err != nil {
			return err
		}
		if err := e.event("content_block_delta", object{"type": "content_block_delta", "index": i, "delta": object{"type": "text_delta", "text": s}}); err != nil {
			return err
		}
		return e.event("content_block_stop", object{"type": "content_block_stop", "index": i})
	}
	return nil
}
func (e *emitter) tool(t irToolCall) error { return e.emitToolPart(irPart{Tool: &t}) }
func (e *emitter) toolRaw(t irToolCall, signature string) error {
	if err := jsonObject(t.Arguments); err != nil {
		return err
	}
	if t.ID == "" || t.Name == "" {
		return malformed("incomplete streamed tool")
	}
	if err := e.start(); err != nil {
		return err
	}
	switch e.to {
	case Responses:
		return e.responseItem(object{"type": "function_call", "call_id": t.ID, "name": t.Name, "arguments": t.Arguments}, "")
	case OpenAI:
		i := e.toolIndex
		e.toolIndex++
		return e.event("", e.chunk(object{"tool_calls": []any{object{"index": i, "id": t.ID, "type": "function", "function": object{"name": t.Name, "arguments": t.Arguments}}}}, nil))
	case Gemini:
		// Client direction is always honest: the Gemini 3 signature bypass
		// exists for the gateway→upstream request path only (see encodeRequest),
		// and a fabricated provider-native signature here would fake state the
		// upstream never produced.
		parts, _ := encodeParts([]irPart{{Tool: &t, Signature: signature}}, Gemini, false)
		return e.event("", object{"modelVersion": e.model, "responseId": e.id, "candidates": []any{object{"index": 0, "content": object{"role": "model", "parts": parts}}}})
	case Anthropic:
		i := e.block
		e.block++
		if err := e.event("content_block_start", object{"type": "content_block_start", "index": i, "content_block": object{"type": "tool_use", "id": t.ID, "name": t.Name, "input": object{}}}); err != nil {
			return err
		}
		if err := e.event("content_block_delta", object{"type": "content_block_delta", "index": i, "delta": object{"type": "input_json_delta", "partial_json": t.Arguments}}); err != nil {
			return err
		}
		return e.event("content_block_stop", object{"type": "content_block_stop", "index": i})
	}
	return nil
}

// bufferTool holds a tool call until the message boundary instead of sending
// it immediately, so a reasoning carrier dropped later in the same message can
// still veto it.
func (e *emitter) bufferTool(t irToolCall) { e.pending = append(e.pending, irPart{Tool: &t}) }

// flushTools releases every held tool call at the message boundary, in arrival
// order. The complete batch is adapted before emitting its first tool, so an
// unsupported attachment cannot hand back a partially continuable tool batch.
func (e *emitter) flushTools() error {
	if len(e.pending) == 0 {
		return nil
	}
	batch := append(e.reasoningPending, e.pending...)
	e.pending = nil
	e.reasoningPending = nil
	seenTool := false
	for i := range batch {
		part := &batch[i]
		if part.Reasoning != nil && part.BridgePosition == nil {
			position := e.prefixParts + i
			part.BridgePosition = &position
		}
		if part.Tool != nil {
			seenTool = true
		} else if e.to == OpenAI && seenTool && part.Reasoning == nil && !part.Thought && part.Text != "" {
			return unsupportedf("text after tool call cannot retain Chat ordering")
		}
	}
	parts, drops, err := prepareResponseReasoning(batch, e.to, e.opts)
	if err != nil {
		return err
	}
	for _, d := range drops {
		e.drop(d)
	}
	for _, part := range parts {
		if part.Reasoning != nil {
			if err := e.nativeReasoning(part); err != nil {
				return err
			}
		}
		if part.Tool != nil {
			if err := e.toolRaw(*part.Tool, part.Signature); err != nil {
				return err
			}
		}
		if part.Reasoning == nil && part.Tool == nil && !part.Thought {
			if err := e.text(part.Text); err != nil {
				return err
			}
		}
	}

	e.pending = nil
	return nil
}
func (e *emitter) use(u irUsage, ok bool) error {
	if u.CacheWrite > 0 && e.to == Gemini {
		return unsupportedf("cache-write usage cannot be represented")
	}
	if ok {
		mergeUsage(&e.usage, u)
		e.hasUsage = true
	}
	return nil
}
func (e *emitter) end(finish, stop string) error {
	if e.to == Responses {
		if err := e.closeResponseText(); err != nil {
			return err
		}
	}
	if len(e.reasoningPending) > 0 {
		parts, drops, err := prepareResponseReasoning(e.reasoningPending, e.to, e.opts)
		if err != nil {
			return err
		}
		for _, d := range drops {
			e.drop(d)
		}
		for _, part := range parts {
			if err := e.nativeReasoning(part); err != nil {
				return err
			}
		}
		e.reasoningPending = nil
	}
	if stop != "" && e.to != Anthropic {
		return unsupportedf("matched stop sequence cannot be represented")
	}
	if err := e.start(); err != nil {
		return err
	}
	switch e.to {
	case Responses:
		status, typ := "completed", "response.completed"
		var incomplete any
		if finish == FinishLength {
			status, typ = "incomplete", "response.incomplete"
			incomplete = object{"reason": "max_output_tokens"}
		}
		output := e.responseOutput
		if output == nil {
			output = []any{}
		}
		response := object{"id": e.id, "object": "response", "model": e.model, "status": status, "output": output, "error": nil, "incomplete_details": incomplete}
		if e.hasUsage {
			response["usage"] = usageOut(e.usage, Responses)
		}
		return e.event(typ, object{"type": typ, "response": response})
	case OpenAI:
		if err := e.event("", e.chunk(object{}, finish)); err != nil {
			return err
		}
		if e.hasUsage {
			if err := e.event("", object{"id": e.id, "object": "chat.completion.chunk", "created": 0, "model": e.model, "choices": []any{}, "usage": usageOut(e.usage, OpenAI)}); err != nil {
				return err
			}
		}
		return e.raw("data: [DONE]\n\n")
	case Anthropic:
		var seq any
		if stop != "" {
			seq = stop
		}
		if err := e.event("message_delta", object{"type": "message_delta", "delta": object{"stop_reason": finishOut(finish, Anthropic, stop), "stop_sequence": seq}, "usage": usageOut(e.usage, Anthropic)}); err != nil {
			return err
		}
		return e.event("message_stop", object{"type": "message_stop"})
	case Gemini:
		m := object{"modelVersion": e.model, "responseId": e.id, "candidates": []any{object{"index": 0, "finishReason": finishOut(finish, Gemini, "")}}}
		if e.hasUsage {
			m["usageMetadata"] = usageOut(e.usage, Gemini)
		}
		return e.event("", m)
	}
	return fmt.Errorf("%w", ErrUnknownProtocol)
}
