package convert

import (
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

func (e *emitter) responseText(s string) error {
	e.responseBytes += len(s)
	if e.responseBytes > maxLineLen-4096 {
		return ErrLineLimit
	}
	if !e.responseTextOpen {
		i := len(e.responseOutput)
		id := stableID("item", e.id, strconv.Itoa(i))
		item := object{"type": "message", "id": id, "role": "assistant", "status": "in_progress", "content": []any{}}
		if err := e.event("response.output_item.added", object{"type": "response.output_item.added", "output_index": i, "item": item}); err != nil {
			return err
		}
		if err := e.event("response.content_part.added", object{"type": "response.content_part.added", "output_index": i, "item_id": id, "content_index": 0, "part": object{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
		e.responseTextOpen = true
		e.responseTextIndex = i
		e.responseOutput = append(e.responseOutput, item)
	}
	if e.responseTextValue.Len()+len(s) > maxLineLen {
		return ErrLineLimit
	}
	e.responseTextValue.WriteString(s)
	item := e.responseOutput[e.responseTextIndex].(object)
	return e.event("response.output_text.delta", object{"type": "response.output_text.delta", "output_index": e.responseTextIndex, "item_id": item["id"], "content_index": 0, "delta": s})
}
func (e *emitter) closeResponseText() error {
	if !e.responseTextOpen {
		return nil
	}
	i := e.responseTextIndex
	item := e.responseOutput[i].(object)
	text := e.responseTextValue.String()
	part := object{"type": "output_text", "text": text, "annotations": []any{}}
	item["status"] = "completed"
	item["content"] = []any{part}
	if err := e.event("response.output_text.done", object{"type": "response.output_text.done", "output_index": i, "item_id": item["id"], "content_index": 0, "text": text}); err != nil {
		return err
	}
	if err := e.event("response.content_part.done", object{"type": "response.content_part.done", "output_index": i, "item_id": item["id"], "content_index": 0, "part": part}); err != nil {
		return err
	}
	if err := e.event("response.output_item.done", object{"type": "response.output_item.done", "output_index": i, "item": item}); err != nil {
		return err
	}
	e.responseTextOpen = false
	e.responseTextValue.Reset()
	return nil
}

func (e *emitter) responseItem(item object, text string) error {
	encoded, err := json.Marshal(item)
	if err != nil {
		return err
	}
	e.responseBytes += len(encoded)
	if e.responseBytes > maxLineLen-4096 {
		return ErrLineLimit
	}
	if err := e.closeResponseText(); err != nil {
		return err
	}
	if err := e.start(); err != nil {
		return err
	}
	i := len(e.responseOutput)
	id := stableID("item", e.id, strconv.Itoa(i))
	if original, ok := item["id"].(string); ok && original != "" {
		id = original
	}
	item["id"] = id
	item["status"] = "completed"
	initial := object{}
	for k, v := range item {
		initial[k] = v
	}
	initial["status"] = "in_progress"
	if item["type"] == "message" {
		initial["content"] = []any{}
	}
	if item["type"] == "function_call" {
		initial["arguments"] = ""
	}
	if err := e.event("response.output_item.added", object{"type": "response.output_item.added", "output_index": i, "item": initial}); err != nil {
		return err
	}
	if item["type"] == "message" {
		if err := e.event("response.content_part.added", object{"type": "response.content_part.added", "output_index": i, "item_id": id, "content_index": 0, "part": object{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
		if err := e.event("response.output_text.delta", object{"type": "response.output_text.delta", "output_index": i, "item_id": id, "content_index": 0, "delta": text}); err != nil {
			return err
		}
		if err := e.event("response.output_text.done", object{"type": "response.output_text.done", "output_index": i, "item_id": id, "content_index": 0, "text": text}); err != nil {
			return err
		}
		if err := e.event("response.content_part.done", object{"type": "response.content_part.done", "output_index": i, "item_id": id, "content_index": 0, "part": item["content"].([]any)[0]}); err != nil {
			return err
		}
	}
	if item["type"] == "function_call" {
		if err := e.event("response.function_call_arguments.delta", object{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": id, "delta": item["arguments"]}); err != nil {
			return err
		}
		if err := e.event("response.function_call_arguments.done", object{"type": "response.function_call_arguments.done", "output_index": i, "item_id": id, "arguments": item["arguments"]}); err != nil {
			return err
		}
	}
	if err := e.event("response.output_item.done", object{"type": "response.output_item.done", "output_index": i, "item": item}); err != nil {
		return err
	}
	e.responseOutput = append(e.responseOutput, item)
	return nil
}

func (e *emitter) reasoning(part irPart) error {
	if len(e.pending) > 0 {
		e.pending = append(e.pending, part)
		return nil
	}
	if token := attachmentFromPart(part); token != "" {
		restored, err := openAttachment(token, e.opts)
		if err != nil {
			return err
		}
		for _, p := range restored {
			if p.Reasoning != nil {
				if err := e.reasoning(p); err != nil {
					return err
				}
			} else if p.Tool != nil {
				e.pending = append(e.pending, p)
			} else {
				if err := e.text(p.Text); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if part.Reasoning == nil && part.Signature == "" {
		return nil
	}
	// A tool-ID bridge needs its tool before it can store a bounded attachment.
	if (e.to == OpenAI || e.to == Gemini) && opaqueReasoning(part) {
		index := e.prefixParts + len(e.reasoningPending)
		if part.BridgePosition == nil {
			part.BridgePosition = &index
		}
		e.reasoningPending = append(e.reasoningPending, part)
		return nil
	}
	parts, drops, err := prepareResponseReasoning([]irPart{part}, e.to, e.opts)
	if err != nil {
		return err
	}
	for _, d := range drops {
		e.drop(d)
	}
	for _, p := range parts {
		if err := e.nativeReasoning(p); err != nil {
			return err
		}
	}
	return nil
}
func (e *emitter) nativeReasoning(part irPart) error {
	if part.Reasoning == nil {
		return nil
	}
	if e.to != OpenAI || !e.prefixReasoning {
		e.prefixParts++
		e.prefixReasoning = true
	}
	if err := e.start(); err != nil {
		return err
	}
	switch e.to {
	case OpenAI:
		return e.event("", e.chunk(object{"reasoning_content": reasoningText(part.Reasoning)}, nil))
	case Responses:
		return e.responseItem(part.Reasoning.Value, "")
	case Gemini:
		return e.event("", object{"responseId": e.id, "modelVersion": e.model, "candidates": []any{object{"index": 0, "content": object{"role": "model", "parts": []any{part.Reasoning.Value}}}}})
	case Anthropic:
		i := e.block
		e.block++
		if err := e.event("content_block_start", object{"type": "content_block_start", "index": i, "content_block": part.Reasoning.Value}); err != nil {
			return err
		}
		return e.event("content_block_stop", object{"type": "content_block_stop", "index": i})
	}
	return ErrUnknownProtocol
}
func (e *emitter) emitToolPart(part irPart) error {
	parts := append(e.reasoningPending, part)
	e.reasoningPending = nil
	out, drops, err := prepareResponseReasoning(parts, e.to, e.opts)
	if err != nil {
		return err
	}
	for _, d := range drops {
		e.drop(d)
	}
	for _, p := range out {
		if p.Reasoning != nil {
			if err := e.nativeReasoning(p); err != nil {
				return err
			}
		}
		if p.Tool != nil {
			if err := e.toolRaw(*p.Tool, p.Signature); err != nil {
				return err
			}
		}
	}
	return nil
}

type responsesStreamItem struct {
	emitted  bool
	deferred bool
	final    object
	part     *irPart
	text     strings.Builder
	item     object
	args     string
	done     bool
}

func streamResponses(r io.Reader, e *emitter) error {
	sr := newSSEReader(r)
	items := map[int]*responsesStreamItem{}
	created := false
	budget := 0
	predecessorsEmitted := func(index int) bool {
		for i := 0; i < index; i++ {
			previous := items[i]
			if previous == nil || !previous.done || !previous.emitted {
				return false
			}
		}
		return true
	}
	for {
		event, data, err := sr.next()
		if err != nil {
			if err == io.EOF {
				return ErrTruncated
			}
			return err
		}
		p := &parser{}
		root := decodeRoot([]byte(data), p)
		typ := p.required(root["type"])
		if event != "" && event != typ {
			return malformed("Responses SSE event/type mismatch")
		}
		if p.err != nil {
			return p.err
		}
		switch typ {
		case "response.created", "response.in_progress":
			m := p.obj(root, "type sequence_number response")
			response, ok := m["response"].(map[string]any)
			if !ok {
				return malformed("missing response")
			}
			if typ == "response.created" && created {
				return malformed("duplicate response.created")
			}
			if created && response["id"] != e.id {
				return malformed("changed Responses response id")
			}
			if !created {
				e.id = p.required(response["id"])
				created = true
			}
			if output, ok := response["output"].([]any); ok && len(output) > 0 {
				return unsupportedf("nonempty initial Responses output")
			}
			if err := e.start(); err != nil {
				return err
			}
		case "response.output_item.added":
			m := p.obj(root, "type sequence_number output_index item")
			i := int(p.integer(m["output_index"]))
			if !created || i > 1024 || items[i] != nil {
				return malformed("Responses output item ordering")
			}
			item, ok := m["item"].(map[string]any)
			if !ok {
				return malformed("missing output item")
			}
			switch item["type"] {
			case "message":
				p.obj(item, "type id role status content")
				if p.str(item["role"]) != RoleAssistant {
					return malformed("expected assistant")
				}
				if len(p.arr(item["content"])) > 0 {
					return unsupportedf("nonempty initial message content")
				}
			case "function_call":
				p.obj(item, "type id call_id name arguments status")
				p.required(item["call_id"])
				p.required(item["name"])
			case "reasoning":
				responsesItem(item, p, false)
			default:
				return unsupportedf("Responses output item type %v", item["type"])
			}
			budget += len(data)
			if budget > maxLineLen {
				return ErrLineLimit
			}
			args, _ := item["arguments"].(string)
			items[i] = &responsesStreamItem{item: item, args: args}
		case "response.output_text.delta", "response.function_call_arguments.delta":
			m := p.obj(root, "type sequence_number output_index item_id content_index delta logprobs")
			i := int(p.integer(m["output_index"]))
			item := items[i]
			if item == nil || item.done {
				return malformed("delta outside active Responses item")
			}
			if id, ok := m["item_id"]; ok && p.str(id) != item.item["id"] {
				return malformed("changed Responses item id")
			}
			s := p.str(m["delta"])
			budget += len(s)
			if budget > maxLineLen {
				return ErrLineLimit
			}
			if typ == "response.output_text.delta" {
				if item.item["type"] != "message" {
					return malformed("text delta outside message")
				}
				if v, ok := m["logprobs"]; ok && v != nil && len(p.arr(v)) > 0 {
					return unsupportedf("Responses logprobs")
				}
				if item.text.Len()+len(s) > maxLineLen {
					return ErrLineLimit
				}
				item.text.WriteString(s)
				if item.deferred || !predecessorsEmitted(i) {
					item.deferred = true
				} else {
					if err := e.text(s); err != nil {
						return err
					}
					item.emitted = true
				}
			} else {
				if item.item["type"] != "function_call" {
					return malformed("arguments outside function call")
				}
				if len(item.args)+len(s) > maxLineLen {
					return ErrLineLimit
				}
				item.args += s
			}
		case "response.output_item.done":
			m := p.obj(root, "type sequence_number output_index item")
			i := int(p.integer(m["output_index"]))
			state := items[i]
			if state == nil || state.done {
				return malformed("Responses done outside active item")
			}
			item, ok := m["item"].(map[string]any)
			if !ok {
				return malformed("missing completed item")
			}
			if item["id"] != state.item["id"] || item["type"] != state.item["type"] {
				return malformed("changed Responses item")
			}
			if item["type"] == "message" {
				msg := p.obj(item, "type id role status content")
				parts := responsesText(msg["content"], p, true)
				if textParts(parts) != state.text.String() {
					return malformed("Responses text disagrees with deltas")
				}
				if !state.deferred && predecessorsEmitted(i) {
					state.emitted = true
				} else {
					state.deferred = true
				}
			} else {
				part := responsesItem(item, p, false)
				if part.Tool != nil && (part.Tool.ID != state.item["call_id"] || part.Tool.Name != state.item["name"]) {
					return malformed("changed Responses function call identity")
				}
				if part.Tool != nil && state.args != "" && state.args != part.Tool.Arguments {
					return malformed("Responses arguments disagree with deltas")
				}
				index := i
				part.BridgePosition = &index
				state.part = &part
				if part.Reasoning != nil && predecessorsEmitted(i) {
					if p.err != nil {
						return p.err
					}
					if err := e.reasoning(part); err != nil {
						return err
					}
					state.emitted = true
				}
			}
			state.done = true
			state.final = item
		case "response.completed":
			m := p.obj(root, "type sequence_number response")
			response, ok := m["response"].(map[string]any)
			if !ok || !created {
				return malformed("Responses terminal without response")
			}
			if response["id"] != e.id {
				return malformed("changed Responses terminal id")
			}
			for _, state := range items {
				if !state.done {
					return ErrTruncated
				}
			}
			output := p.arr(response["output"])
			if len(output) != len(items) {
				return malformed("Responses terminal output disagrees with completed items")
			}
			for i, v := range output {
				state := items[i]
				item, ok := v.(map[string]any)
				if state == nil || !ok || !reflect.DeepEqual(item, state.final) {
					return malformed("Responses terminal item disagrees with completed item")
				}
			}
			decoded := decodeResponsesResponse(response, p)
			u, has := extractUsage(response, Responses, p, true)
			if p.err != nil {
				return p.err
			}
			if err := e.use(u, has); err != nil {
				return err
			}
			finish := FinishStop
			if hasTools(decoded.Parts) {
				finish = FinishToolCalls
			}
			indices := make([]int, 0, len(items))
			for i := range items {
				indices = append(indices, i)
			}
			sort.Ints(indices)
			for _, i := range indices {
				state := items[i]
				if state.deferred && state.item["type"] == "message" {
					e.pending = append(e.pending, irPart{Text: state.text.String()})
				}
				if part := state.part; part != nil && !state.emitted {
					e.pending = append(e.pending, *part)
				}
			}
			if err := e.flushTools(); err != nil {
				return err
			}
			return e.end(finish, "")
		case "response.incomplete", "response.failed", "error":
			return unsupportedf("upstream Responses terminal %s", typ)
		case "response.content_part.added", "response.content_part.done":
			m := p.obj(root, "type sequence_number output_index item_id content_index part")
			part := p.obj(m["part"], "type text annotations logprobs")
			responsesText([]any{part}, p, true)
		case "response.output_text.done":
			p.obj(root, "type sequence_number output_index item_id content_index text logprobs")
		case "response.function_call_arguments.done":
			p.obj(root, "type sequence_number output_index item_id arguments")
		case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
			m := p.obj(root, "type sequence_number output_index item_id summary_index part")
			part := p.obj(m["part"], "type text")
			if p.str(part["type"]) != "summary_text" {
				return unsupportedf("reasoning summary type")
			}
			p.str(part["text"])
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			p.obj(root, "type sequence_number output_index item_id summary_index content_index delta")
		case "response.reasoning_summary_text.done", "response.reasoning_text.done":
			p.obj(root, "type sequence_number output_index item_id summary_index content_index text")
		default:
			return unsupportedf("Responses stream event %q", typ)
		}
		if p.err != nil {
			return p.err
		}
	}
}
