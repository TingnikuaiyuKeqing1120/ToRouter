package convert

import "encoding/json"

func decodeResponsesRequest(root object, p *parser) *irRequest {
	m := p.obj(root, "model instructions input tools tool_choice max_output_tokens temperature top_p stream store include parallel_tool_calls")
	r := &irRequest{}
	if v, ok := m["model"]; ok {
		p.required(v)
	}
	parameters(r, m, p, "max_output_tokens", "stop")
	if v, ok := m["instructions"]; ok && v != nil {
		r.System = p.str(v)
	}
	if v, ok := m["store"]; ok && p.boolean(v) {
		p.fail(unsupportedf("Responses store=true requires native response storage"))
	}
	if v, ok := m["parallel_tool_calls"]; ok && !p.boolean(v) {
		p.fail(unsupportedf("parallel_tool_calls=false"))
	}
	if v, ok := m["include"]; ok {
		for _, s := range p.strings(v) {
			if s != "reasoning.encrypted_content" {
				p.fail(unsupportedf("include %q", s))
			}
		}
	}
	for _, v := range optionalArray(m, "tools", p) {
		t := p.obj(v, "type name description parameters strict")
		if p.str(t["type"]) != "function" {
			p.fail(unsupportedf("Responses hosted tool"))
		}
		if v, ok := t["strict"]; ok && p.boolean(v) {
			p.fail(unsupportedf("strict function schema"))
		}
		r.Tools = append(r.Tools, decodeTool(t, "parameters", p))
	}
	if v, ok := m["tool_choice"]; ok {
		if s, ok := v.(string); ok {
			r.ToolChoice = &irToolChoice{Mode: s}
		} else {
			t := p.obj(v, "type name")
			if p.str(t["type"]) != "function" {
				p.fail(unsupportedf("Responses tool choice"))
			}
			r.ToolChoice = &irToolChoice{Mode: TCTool, Name: p.required(t["name"])}
		}
	}
	if s, ok := m["input"].(string); ok {
		r.Messages = []irMessage{{Role: RoleUser, Parts: []irPart{{Text: s}}}}
	} else {
		for _, v := range p.arr(m["input"]) {
			item, ok := v.(map[string]any)
			if !ok {
				p.fail(malformed("Responses input item must be object"))
				continue
			}
			typ, _ := item["type"].(string)
			if typ == "" || typ == "message" {
				mm := p.obj(item, "type id status role content")
				role := p.required(mm["role"])
				parts := responsesText(mm["content"], p, role == RoleAssistant)
				if role == RoleSystem || role == "developer" {
					if len(r.Messages) > 0 {
						p.fail(unsupportedf("instruction after conversation start"))
					}
					appendSystem(r, textParts(parts))
					continue
				}
				if role != RoleUser && role != RoleAssistant {
					p.fail(unsupportedf("Responses role %q", role))
				}
				if role == RoleAssistant && len(r.Messages) > 0 && r.Messages[len(r.Messages)-1].Role == RoleAssistant {
					r.Messages[len(r.Messages)-1].Parts = append(r.Messages[len(r.Messages)-1].Parts, parts...)
				} else {
					r.Messages = append(r.Messages, irMessage{Role: role, Parts: parts})
				}
				continue
			}
			part := responsesItem(item, p, true)
			role := RoleAssistant
			if part.ToolResult != nil {
				role = RoleTool
			}
			if len(r.Messages) > 0 && r.Messages[len(r.Messages)-1].Role == role {
				r.Messages[len(r.Messages)-1].Parts = append(r.Messages[len(r.Messages)-1].Parts, part)
			} else {
				r.Messages = append(r.Messages, irMessage{Role: role, Parts: []irPart{part}})
			}
		}
	}
	return r
}

func responsesText(v any, p *parser, assistant bool) []irPart {
	if s, ok := v.(string); ok {
		return []irPart{{Text: s}}
	}
	var parts []irPart
	for _, v := range p.arr(v) {
		m := p.obj(v, "type text annotations logprobs")
		t := p.str(m["type"])
		if t != "input_text" && t != "output_text" {
			p.fail(unsupportedf("Responses content %q", t))
		}
		if t == "output_text" && !assistant {
			p.fail(malformed("output_text requires assistant role"))
		}
		for _, key := range []string{"annotations", "logprobs"} {
			if v, ok := m[key]; ok && v != nil && len(p.arr(v)) > 0 {
				p.fail(unsupportedf("Responses %s", key))
			}
		}
		parts = append(parts, irPart{Text: p.str(m["text"])})
	}
	return parts
}

func responsesItem(item object, p *parser, results bool) irPart {
	switch item["type"] {
	case "function_call":
		m := p.obj(item, "type id status call_id name arguments")
		t := &irToolCall{ID: p.required(m["call_id"]), Name: p.required(m["name"]), Arguments: p.str(m["arguments"])}
		p.fail(jsonObject(t.Arguments))
		return irPart{Tool: t}
	case "function_call_output":
		m := p.obj(item, "type id status call_id output")
		if !results {
			p.fail(unsupportedf("tool result in response"))
		}
		return irPart{ToolResult: &irToolResult{ID: p.required(m["call_id"]), Content: p.str(m["output"])}}
	case "reasoning":
		m := p.obj(item, "type id status summary content encrypted_content")
		for _, key := range []string{"summary", "content"} {
			if v, ok := m[key]; ok && v != nil {
				for _, v := range p.arr(v) {
					s := p.obj(v, "type text")
					typ := p.str(s["type"])
					if typ != "summary_text" && typ != "reasoning_text" {
						p.fail(unsupportedf("reasoning content type %q", typ))
					}
					p.str(s["text"])
				}
			}
		}
		if v, ok := m["encrypted_content"]; ok && v != nil {
			p.str(v)
		}
		return irPart{Thought: true, Reasoning: &irReasoning{Format: Responses, Value: m}}
	default:
		p.fail(unsupportedf("Responses item type %v", item["type"]))
		return irPart{}
	}
}

func responsesItems(parts []irPart, role string) []any {
	var out []any
	for _, part := range parts {
		switch {
		case part.Reasoning != nil:
			out = append(out, part.Reasoning.Value)
		case part.Tool != nil:
			t := part.Tool
			out = append(out, object{"type": "function_call", "call_id": t.ID, "name": t.Name, "arguments": t.Arguments})
		case part.ToolResult != nil:
			t := part.ToolResult
			out = append(out, object{"type": "function_call_output", "call_id": t.ID, "output": t.Content})
		default:
			typ := "input_text"
			if role == RoleAssistant {
				typ = "output_text"
			}
			out = append(out, object{"type": "message", "role": role, "content": []any{object{"type": typ, "text": part.Text}}})
		}
	}
	return out
}

func encodeResponsesRequest(r *irRequest, model string) ([]byte, error) {
	if len(r.Stop) > 0 {
		return nil, unsupportedf("Responses stop sequences")
	}
	m := object{"model": model, "stream": r.Stream, "store": false, "include": []string{"reasoning.encrypted_content"}}
	if r.System != "" {
		m["instructions"] = r.System
	}
	var input []any
	for _, msg := range r.Messages {
		input = append(input, responsesItems(msg.Parts, msg.Role)...)
	}
	m["input"] = input
	if r.MaxTokens != nil {
		m["max_output_tokens"] = *r.MaxTokens
	}
	if r.Temperature != nil {
		m["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		m["top_p"] = *r.TopP
	}
	if len(r.Tools) > 0 {
		var ts []any
		for _, t := range r.Tools {
			ts = append(ts, object{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Schema})
		}
		m["tools"] = ts
	}
	if t := r.ToolChoice; t != nil {
		if t.Mode == TCTool {
			m["tool_choice"] = object{"type": "function", "name": t.Name}
		} else {
			m["tool_choice"] = t.Mode
		}
	}
	return json.Marshal(m)
}

func decodeResponsesResponse(root object, p *parser) *irResponse {
	m := p.obj(root, "id object created_at completed_at model output status error incomplete_details usage instructions max_output_tokens parallel_tool_calls previous_response_id reasoning store temperature text tool_choice tools top_p truncation metadata user service_tier background conversation prompt_cache_key prompt_cache_retention safety_identifier max_tool_calls")
	// Known metadata is not history. References and incomplete/failed terminal
	// states must never be silently reported as a successful generated answer.
	for _, key := range []string{"previous_response_id", "conversation"} {
		if m[key] != nil {
			p.fail(unsupportedf("Responses %s", key))
		}
	}
	r := &irResponse{ID: p.optstr(m, "id"), Finish: FinishStop}
	status := p.str(m["status"])
	if status != "completed" {
		p.fail(unsupportedf("Responses terminal status %q", status))
	}
	if m["error"] != nil {
		p.fail(malformed("Responses error"))
	}
	for _, v := range p.arr(m["output"]) {
		item, ok := v.(map[string]any)
		if !ok {
			p.fail(malformed("output item must be object"))
			continue
		}
		if item["type"] == "message" {
			msg := p.obj(item, "type id status role content")
			if p.str(msg["role"]) != RoleAssistant {
				p.fail(malformed("response role must be assistant"))
			}
			r.Parts = append(r.Parts, responsesText(msg["content"], p, true)...)
		} else {
			r.Parts = append(r.Parts, responsesItem(item, p, false))
		}
	}
	return r
}

func encodeResponsesResponse(r *irResponse, model string) ([]byte, error) {
	if r.StopSequence != "" {
		return nil, unsupportedf("Responses matched stop sequence")
	}
	status := "completed"
	var incomplete any
	if r.Finish == FinishLength {
		status = "incomplete"
		incomplete = object{"reason": "max_output_tokens"}
	}
	id := r.ID
	if id == "" {
		b, _ := json.Marshal(r.Parts)
		id = stableID("resp", string(b))
	}
	out := responsesItems(r.Parts, RoleAssistant)
	if out == nil {
		out = []any{}
	}
	for i, v := range out {
		item := v.(object)
		if _, ok := item["id"]; !ok {
			item["id"] = stableID("item", id, string(rune(i)))
		}
		item["status"] = "completed"
	}
	m := object{"id": id, "object": "response", "created_at": 0, "model": model, "status": status, "error": nil, "incomplete_details": incomplete, "output": out}
	if r.HasUsage {
		m["usage"] = usageOut(r.Usage, Responses)
	}
	return json.Marshal(m)
}
