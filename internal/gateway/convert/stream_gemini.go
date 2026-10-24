package convert

import "io"

// Gemini streamGenerateContent?alt=sse sends delta parts, followed by a
// candidate finishReason. Unlike OpenAI/Anthropic, it has no extra sentinel.
func streamGemini(r io.Reader, e *emitter) error {
	sr := newSSEReader(r)
	finish := ""
	toolCount := 0
	seen := map[string]bool{}
	for {
		event, data, err := sr.next()
		if err != nil {
			if err == io.EOF && finish != "" {
				if err := e.flushTools(); err != nil {
					return err
				}
				return e.end(finish, "")
			}
			if err == io.EOF {
				return ErrTruncated
			}
			return err
		}
		if event != "" && event != "message" {
			return unsupportedf("Gemini SSE event %q", event)
		}
		p := &parser{}
		root := decodeRoot([]byte(data), p)
		m := p.obj(root, "candidates usageMetadata modelVersion responseId")
		if !e.started {
			if v, ok := m["responseId"]; ok {
				e.id = p.required(v)
			}
		}
		var parts []irPart
		if v, ok := m["candidates"]; ok {
			cs := p.arr(v)
			if len(cs) > 1 {
				p.fail(unsupportedf("multiple Gemini candidates"))
			}
			for _, v := range cs {
				c := p.obj(v, "index content finishReason")
				if finish != "" {
					p.fail(malformed("candidate after finishReason"))
				}
				if v, ok := c["index"]; ok && p.integer(v) != 0 {
					p.fail(unsupportedf("candidate index"))
				}
				if v, ok := c["content"]; ok {
					content := p.obj(v, "role parts")
					if v, ok := content["role"]; ok && p.str(v) != "model" {
						p.fail(malformed("expected model role"))
					}
					parts = geminiParts(content["parts"], p, 0, false, true)
					if p.err != nil {
						return p.err
					}
					for _, k := range p.dropped {
						e.drop(k)
					}
					for i := range parts {
						if t := parts[i].Tool; t != nil {
							raw := content["parts"].([]any)[i].(map[string]any)["functionCall"].(map[string]any)
							if _, ok := raw["id"]; !ok {
								t.ID = toolID(0, toolCount, t.Name)
							}
							toolCount++
							if seen[t.ID] {
								p.fail(unsupportedf("repeated/fragmented Gemini functionCall"))
							}
							seen[t.ID] = true
						}
					}
				}
				if v, ok := c["finishReason"]; ok {
					finish = finishIn(p.str(v), Gemini, p)
					if finish == FinishStop && toolCount > 0 {
						finish = FinishToolCalls
					}
				}
			}
		}
		u, has := extractUsage(m, Gemini, p, true)
		if p.err != nil {
			return p.err
		}
		if err := e.use(u, has); err != nil {
			return err
		}
		for _, part := range parts {
			if part.Reasoning != nil {
				if err := e.reasoning(part); err != nil {
					return err
				}
				continue
			}
			if part.Tool != nil {
				// Hold the call until the candidate ends: a signature may still
				// ride a later part and must be able to veto it (T025).
				e.pending = append(e.pending, part)
			} else {
				if part.Signature != "" {
					if err := e.reasoning(part); err != nil {
						return err
					}
				}
				if err := e.text(part.Text); err != nil {
					return err
				}
			}
		}
	}
}
