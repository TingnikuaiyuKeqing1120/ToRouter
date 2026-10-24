package convert

import "io"

func streamAnthropic(r io.Reader, e *emitter) error {
	sr := newSSEReader(r)
	started, messageDeltas := false, false
	active := -1
	next := 0
	kind := ""
	skip := false
	var reasoning object
	var tool irToolCall
	initial, fragments := "", false
	finish, stop := "", ""
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
		typ := p.str(root["type"])
		if event != "" && event != typ {
			p.fail(malformed("SSE event/type mismatch"))
		}
		text := ""
		switch typ {
		case "ping":
			p.obj(root, "type")
		case "error":
			return malformed("upstream Anthropic error event")
		case "message_start":
			m := p.obj(root, "type message")
			if started {
				p.fail(malformed("duplicate message_start"))
			}
			msg := p.obj(m["message"], "id type role model content stop_reason stop_sequence usage")
			e.id = p.required(msg["id"])
			if p.str(msg["type"]) != "message" || p.str(msg["role"]) != RoleAssistant {
				p.fail(malformed("invalid message_start"))
			}
			if len(p.arr(msg["content"])) != 0 || msg["stop_reason"] != nil || msg["stop_sequence"] != nil {
				p.fail(malformed("nonempty message_start"))
			}
			started = true
		case "content_block_start":
			m := p.obj(root, "type index content_block")
			i := int(p.integer(m["index"]))
			if !started || messageDeltas || active != -1 || i != next {
				p.fail(malformed("invalid content block start ordering"))
			}
			active = i
			next++
			b, ok := m["content_block"].(map[string]any)
			if !ok {
				p.fail(malformed("missing content block"))
				b = object{}
			}
			kind = p.str(b["type"])
			switch kind {
			case "text":
				p.obj(b, "type text")
				text = p.str(b["text"])
			case "tool_use":
				p.obj(b, "type id name input")
				tool = irToolCall{ID: p.required(b["id"]), Name: p.required(b["name"])}
				initial = p.args(b["input"])
				fragments = false
			case "thinking", "redacted_thinking":
				// Reasoning carrier: the target format has no place for it.
				// Skip the block and record the loss (T014).
				p.obj(b, "type thinking signature data")
				skip = true
				reasoning = object{}
				for k, v := range b {
					reasoning[k] = v
				}
			default:
				p.fail(unsupportedf("Anthropic stream block %q", kind))
			}
		case "content_block_delta":
			m := p.obj(root, "type index delta")
			if !started || messageDeltas || active < 0 || int(p.integer(m["index"])) != active {
				p.fail(malformed("delta outside active block"))
			}
			d, ok := m["delta"].(map[string]any)
			if !ok {
				p.fail(malformed("missing block delta"))
				d = object{}
			}
			switch d["type"] {
			case "text_delta":
				p.obj(d, "type text")
				if kind != "text" {
					p.fail(malformed("text delta in tool block"))
				}
				text = p.str(d["text"])
			case "input_json_delta":
				p.obj(d, "type partial_json")
				if kind != "tool_use" {
					p.fail(malformed("tool delta in tool block"))
				}
				if initial != "{}" {
					p.fail(unsupportedf("nonempty initial input plus JSON deltas"))
				}
				s := p.str(d["partial_json"])
				fragments = true
				if len(tool.Arguments)+len(s) > maxLineLen {
					p.fail(ErrLineLimit)
				} else {
					tool.Arguments += s
				}
			case "thinking_delta", "signature_delta", "data_delta":
				if !skip {
					p.fail(unsupportedf("Anthropic delta type %v", d["type"]))
				} else {
					key := map[string]string{"thinking_delta": "thinking", "signature_delta": "signature", "data_delta": "data"}[p.str(d["type"])]
					p.obj(d, "type "+key)
					s := p.str(d[key])
					previous, _ := reasoning[key].(string)
					if len(previous)+len(s) > maxLineLen {
						return ErrLineLimit
					}
					reasoning[key] = previous + s
				}
			default:
				p.fail(unsupportedf("Anthropic delta type %v", d["type"]))
			}
		case "content_block_stop":
			m := p.obj(root, "type index")
			if !started || messageDeltas || active < 0 || int(p.integer(m["index"])) != active {
				p.fail(malformed("stop outside active block"))
			}
			if kind == "tool_use" {
				if !fragments {
					tool.Arguments = initial
				}
				p.fail(jsonObject(tool.Arguments))
				// Hold the call until message_stop: a thinking block can still
				// arrive and veto the whole message (T025).
				e.bufferTool(tool)
			}
			if skip {
				if err := e.reasoning(irPart{Thought: true, Reasoning: &irReasoning{Format: Anthropic, Value: reasoning}}); err != nil {
					return err
				}
			}
			active = -1
			kind = ""
			skip = false
		case "message_delta":
			m := p.obj(root, "type delta usage")
			if !started || active >= 0 {
				p.fail(malformed("invalid message_delta ordering"))
			}
			d := p.obj(m["delta"], "stop_reason stop_sequence")
			// Message deltas may split finish metadata and cumulative usage over
			// multiple events. Only fields present in a delta replace prior values.
			if v, ok := d["stop_reason"]; ok {
				finish = ""
				if v != nil {
					finish = finishIn(p.str(v), Anthropic, p)
				}
			}
			if v, ok := d["stop_sequence"]; ok {
				stop = ""
				if v != nil {
					stop = p.str(v)
				}
			}
			messageDeltas = true
		case "message_stop":
			p.obj(root, "type")
			if !started || !messageDeltas || active >= 0 || finish == "" {
				p.fail(malformed("premature message_stop"))
			}
			if p.err != nil {
				return p.err
			}
			if err := e.flushTools(); err != nil {
				return err
			}
			return e.end(finish, stop)
		default:
			p.fail(unsupportedf("Anthropic stream event %q", typ))
		}
		u, has := extractUsage(root, Anthropic, p, true)
		if p.err != nil {
			return p.err
		}
		if err := e.use(u, has); err != nil {
			return err
		}
		if started {
			if err := e.start(); err != nil {
				return err
			}
		}
		if err := e.textIfNonempty(text); err != nil {
			return err
		}
	}
}
func (e *emitter) textIfNonempty(s string) error {
	if s == "" {
		return nil
	}
	return e.text(s)
}
