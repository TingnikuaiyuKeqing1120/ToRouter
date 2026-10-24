package convert

import "encoding/json"

func decodeResponse(b []byte, protocol string) (*irResponse, error) {
	p := &parser{}
	root := decodeRoot(b, p)
	r := &irResponse{}
	switch protocol {
	case Responses:
		r = decodeResponsesResponse(root, p)
	case OpenAI:
		m := p.obj(root, "id object created model choices usage system_fingerprint service_tier")
		r.ID = p.optstr(m, "id")
		cs := p.arr(m["choices"])
		if len(cs) != 1 {
			p.fail(unsupportedf("exactly one choice required"))
		}
		for _, v := range cs {
			c := p.obj(v, "index message finish_reason logprobs")
			if v, ok := c["index"]; ok && p.integer(v) != 0 {
				p.fail(unsupportedf("choice index"))
			}
			if c["logprobs"] != nil {
				p.fail(unsupportedf("logprobs"))
			}
			msg := p.obj(c["message"], "role content tool_calls refusal annotations reasoning_content")
			if p.str(msg["role"]) != RoleAssistant {
				p.fail(malformed("response role must be assistant"))
			}
			if msg["refusal"] != nil {
				p.fail(unsupportedf("refusal"))
			}
			if p.hasReasoning(msg["reasoning_content"]) {
				// Plaintext reasoning from an OpenAI-format upstream. The target
				// format may have no field for it; drop and record.
				r.Parts = append(r.Parts, irPart{Thought: true, Reasoning: &irReasoning{Format: OpenAI, Value: object{"reasoning_content": msg["reasoning_content"]}}})
			}
			if v, ok := msg["annotations"]; ok && len(p.arr(v)) > 0 {
				p.fail(unsupportedf("annotations"))
			}
			if v, ok := msg["content"]; ok && v != nil {
				r.Parts = append(r.Parts, openAIText(v, p)...)
			}
			for _, v := range optionalArray(msg, "tool_calls", p) {
				r.Parts = append(r.Parts, irPart{Tool: openAICall(v, p)})
			}
			r.Finish = finishIn(p.str(c["finish_reason"]), protocol, p)
		}
	case Anthropic:
		m := p.obj(root, "id type role model content stop_reason stop_sequence usage")
		r.ID = p.optstr(m, "id")
		if v, ok := m["type"]; ok && p.str(v) != "message" {
			p.fail(malformed("expected message"))
		}
		if p.str(m["role"]) != RoleAssistant {
			p.fail(malformed("response role must be assistant"))
		}
		r.Parts = anthropicParts(m["content"], p, false, true)
		r.Finish = finishIn(p.str(m["stop_reason"]), protocol, p)
		if v, ok := m["stop_sequence"]; ok && v != nil {
			r.StopSequence = p.str(v)
		}
	case Gemini:
		m := p.obj(root, "candidates usageMetadata modelVersion responseId")
		r.ID = p.optstr(m, "responseId")
		cs := p.arr(m["candidates"])
		if len(cs) != 1 {
			p.fail(unsupportedf("exactly one candidate required"))
		}
		for _, v := range cs {
			c := p.obj(v, "index content finishReason")
			if v, ok := c["index"]; ok && p.integer(v) != 0 {
				p.fail(unsupportedf("candidate index"))
			}
			content := p.obj(c["content"], "role parts")
			if v, ok := content["role"]; ok && p.str(v) != "model" {
				p.fail(malformed("expected model role"))
			}
			r.Parts = geminiParts(content["parts"], p, 0, false, true)
			r.Finish = finishIn(p.str(c["finishReason"]), protocol, p)
		}
	}
	r.Usage, r.HasUsage = extractUsage(root, protocol, p, true)
	if r.Finish == FinishStop && hasTools(r.Parts) {
		r.Finish = FinishToolCalls
	}
	seen := map[string]bool{}
	for _, t := range calls(r.Parts) {
		if seen[t.ID] {
			p.fail(malformed("duplicate tool id"))
		}
		seen[t.ID] = true
	}
	if p.err != nil {
		return nil, p.err
	}
	r.Dropped = p.dropped
	return r, nil
}
func finishIn(s, protocol string, p *parser) string {
	switch protocol {
	case OpenAI:
		switch s {
		case "stop":
			return FinishStop
		case "length":
			return FinishLength
		case "tool_calls":
			return FinishToolCalls
		}
	case Anthropic:
		switch s {
		case "end_turn", "stop_sequence":
			return FinishStop
		case "max_tokens":
			return FinishLength
		case "tool_use":
			return FinishToolCalls
		}
	case Gemini:
		switch s {
		case "STOP":
			return FinishStop
		case "MAX_TOKENS":
			return FinishLength
		}
	}
	p.fail(unsupportedf("%s finish reason %q", protocol, s))
	return ""
}
func finishOut(f, to, stop string) string {
	switch to {
	case OpenAI:
		return f
	case Anthropic:
		if stop != "" && f == FinishStop {
			return "stop_sequence"
		}
		return map[string]string{FinishStop: "end_turn", FinishLength: "max_tokens", FinishToolCalls: "tool_use"}[f]
	case Gemini:
		if f == FinishLength {
			return "MAX_TOKENS"
		}
		return "STOP"
	}
	return ""
}

// responseParts renders response parts; response encoding never attaches a
// Gemini signature bypass (the bypass exists so an upstream accepts a trace,
// not so a client does).
func responseParts(parts []irPart, to string) []any {
	out, _ := encodeParts(parts, to, false)
	return out
}

func encodeResponse(r *irResponse, to, model string) ([]byte, error) {
	if to == Responses {
		return encodeResponsesResponse(r, model)
	}
	if r.StopSequence != "" && to != Anthropic {
		return nil, unsupportedf("matched stop sequence not representable in %s", to)
	}
	if r.Usage.CacheWrite > 0 && to == Gemini {
		return nil, unsupportedf("cache-write usage not representable in %s", to)
	}
	id := r.ID
	if id == "" {
		b, _ := json.Marshal(r.Parts)
		id = stableID("resp", string(b))
	}
	m := object{}
	switch to {
	case OpenAI:
		seen := false
		for _, part := range r.Parts {
			if part.Tool != nil {
				seen = true
			} else if seen && part.Text != "" {
				return nil, unsupportedf("interleaved text after tool call")
			}
		}
		msg := object{"role": "assistant", "content": textParts(r.Parts)}
		if reasoning := chatReasoning(r.Parts); reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		if ts := calls(r.Parts); len(ts) > 0 {
			msg["tool_calls"] = openAICalls(ts)
			if msg["content"] == "" {
				msg["content"] = nil
			}
		}
		m = object{"id": id, "object": "chat.completion", "created": 0, "model": model, "choices": []any{object{"index": 0, "message": msg, "finish_reason": finishOut(r.Finish, to, r.StopSequence)}}}
	case Anthropic:
		var stop any
		if r.StopSequence != "" {
			stop = r.StopSequence
		}
		m = object{"id": id, "type": "message", "role": "assistant", "model": model, "content": responseParts(r.Parts, to), "stop_reason": finishOut(r.Finish, to, r.StopSequence), "stop_sequence": stop, "usage": usageOut(r.Usage, to)}
	case Gemini:
		m = object{"responseId": id, "modelVersion": model, "candidates": []any{object{"index": 0, "content": object{"role": "model", "parts": responseParts(r.Parts, to)}, "finishReason": finishOut(r.Finish, to, "")}}}
	}
	if r.HasUsage {
		key := "usage"
		if to == Gemini {
			key = "usageMetadata"
		}
		m[key] = usageOut(r.Usage, to)
	}
	return json.Marshal(m)
}
