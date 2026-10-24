package convert

import (
	"io"
	"sort"
)

func streamOpenAI(r io.Reader, e *emitter) error {
	sr := newSSEReader(r)
	finish := ""
	tools := map[int]*irToolCall{}
	started := false
	for {
		name, data, err := sr.next()
		if err != nil {
			if err == io.EOF {
				return ErrTruncated
			}
			return err
		}
		if name != "" && name != "message" {
			return unsupportedf("OpenAI SSE event %q", name)
		}
		if data == "[DONE]" {
			if !started || finish == "" {
				return ErrTruncated
			}
			keys := make([]int, 0, len(tools))
			for k := range tools {
				keys = append(keys, k)
			}
			sort.Ints(keys)
			// Validate all calls before emitting any. JSON fragments are never sent
			// to Gemini until the entire argument object has been received.
			for _, k := range keys {
				t := tools[k]
				if t.ID == "" || t.Name == "" {
					return malformed("incomplete tool call")
				}
				if err := jsonObject(t.Arguments); err != nil {
					return err
				}
			}
			seen := map[string]bool{}
			for _, k := range keys {
				t := tools[k]
				if seen[t.ID] {
					return malformed("duplicate tool id")
				}
				seen[t.ID] = true
				if err := e.tool(*t); err != nil {
					return err
				}
			}
			return e.end(finish, "")
		}
		p := &parser{}
		root := decodeRoot([]byte(data), p)
		m := p.obj(root, "id object created model choices usage system_fingerprint service_tier")
		if !started {
			if id, ok := m["id"]; ok {
				e.id = p.required(id)
			}
		}
		u, has := extractUsage(m, OpenAI, p, true)
		cs := p.arr(m["choices"])
		if len(cs) > 1 {
			p.fail(unsupportedf("multiple choices"))
		}
		var text string
		for _, v := range cs {
			if finish != "" {
				p.fail(malformed("choice after finish"))
			}
			c := p.obj(v, "index delta finish_reason logprobs")
			if p.integer(c["index"]) != 0 {
				p.fail(unsupportedf("choice index"))
			}
			if c["logprobs"] != nil {
				p.fail(unsupportedf("logprobs"))
			}
			d := p.obj(c["delta"], "role content tool_calls refusal reasoning_content")
			if v, ok := d["role"]; ok && p.str(v) != RoleAssistant {
				p.fail(malformed("expected assistant role"))
			}
			if d["refusal"] != nil {
				p.fail(unsupportedf("refusal"))
			}
			if p.hasReasoning(d["reasoning_content"]) {
				// Plaintext reasoning delta; the target format may have no field
				// for it. Drop and record (T014).
				if err := e.reasoning(irPart{Thought: true, Reasoning: &irReasoning{Format: OpenAI, Value: object{"reasoning_content": d["reasoning_content"]}}}); err != nil {
					return err
				}
			}
			if v, ok := d["content"]; ok && v != nil {
				text = p.str(v)
			}
			for _, v := range optionalArray(d, "tool_calls", p) {
				t := p.obj(v, "index id type function")
				idx := p.integer(t["index"])
				if idx > 1024 {
					p.fail(unsupportedf("tool index above 1024"))
					continue
				}
				k := int(idx)
				call := tools[k]
				if call == nil {
					call = &irToolCall{}
					tools[k] = call
				}
				if v, ok := t["id"]; ok {
					id := p.required(v)
					if call.ID != "" && call.ID != id {
						p.fail(malformed("changed tool id"))
					}
					call.ID = id
				}
				if v, ok := t["type"]; ok && p.str(v) != "function" {
					p.fail(unsupportedf("tool type"))
				}
				if v, ok := t["function"]; ok {
					f := p.obj(v, "name arguments")
					if v, ok := f["name"]; ok {
						call.Name += p.str(v)
					}
					if v, ok := f["arguments"]; ok {
						s := p.str(v)
						if len(call.Arguments)+len(s) > maxLineLen {
							p.fail(ErrLineLimit)
						} else {
							call.Arguments += s
						}
					}
				}
			}
			if v, ok := c["finish_reason"]; ok && v != nil {
				finish = finishIn(p.str(v), OpenAI, p)
			}
		}
		if p.err != nil {
			return p.err
		}
		started = true
		if err := e.use(u, has); err != nil {
			return err
		}
		if err := e.start(); err != nil {
			return err
		}
		if err := e.text(text); err != nil {
			return err
		}
	}
}
