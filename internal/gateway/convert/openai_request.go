package convert

func decodeOpenAIRequest(root object, p *parser) *irRequest {
	m := p.obj(root, "model messages tools tool_choice temperature top_p max_tokens max_completion_tokens stop stream stream_options")
	r := &irRequest{}
	if v, ok := m["model"]; ok {
		p.required(v)
	}
	parameters(r, m, p, "max_tokens", "stop")
	if v, ok := m["max_completion_tokens"]; ok {
		if r.MaxTokens != nil {
			p.fail(malformed("both token limits supplied"))
		}
		n := p.integer(v)
		if n == 0 {
			p.fail(malformed("token limit must be positive"))
		}
		r.MaxTokens = &n
	}
	if v, ok := m["stream_options"]; ok {
		o := p.obj(v, "include_usage")
		if x, ok := o["include_usage"]; ok {
			p.boolean(x)
		}
	}
	for _, v := range optionalArray(m, "tools", p) {
		t := p.obj(v, "type function")
		if p.str(t["type"]) != "function" {
			p.fail(unsupportedf("tool type"))
		}
		f := p.obj(t["function"], "name description parameters")
		r.Tools = append(r.Tools, decodeTool(f, "parameters", p))
	}
	if v, ok := m["tool_choice"]; ok {
		if s, ok := v.(string); ok {
			r.ToolChoice = &irToolChoice{Mode: s}
		} else {
			t := p.obj(v, "type function")
			if p.str(t["type"]) != "function" {
				p.fail(unsupportedf("tool choice type"))
			}
			f := p.obj(t["function"], "name")
			r.ToolChoice = &irToolChoice{Mode: TCTool, Name: p.required(f["name"])}
		}
	}
	for _, v := range p.arr(m["messages"]) {
		mm := p.obj(v, "role content tool_calls tool_call_id reasoning_content")
		role := p.str(mm["role"])
		msg := irMessage{Role: role}
		if p.hasReasoning(mm["reasoning_content"]) {
			msg.Parts = append(msg.Parts, irPart{Thought: true, Reasoning: &irReasoning{Format: OpenAI, Value: object{"reasoning_content": mm["reasoning_content"]}}})
		}
		if c, ok := mm["content"]; ok && c != nil {
			msg.Parts = append(msg.Parts, openAIText(c, p)...)
		}
		if tc, ok := mm["tool_calls"]; ok && tc != nil {
			if role != RoleAssistant {
				p.fail(malformed("tool_calls require assistant role"))
			}
			for _, v := range p.arr(tc) {
				msg.Parts = append(msg.Parts, irPart{Tool: openAICall(v, p)})
			}
		}
		if role == RoleTool {
			id := p.required(mm["tool_call_id"])
			msg.Parts = []irPart{{ToolResult: &irToolResult{ID: id, Content: textParts(msg.Parts)}}}
		} else if _, ok := mm["tool_call_id"]; ok {
			p.fail(malformed("tool_call_id requires tool role"))
		}
		if role == RoleSystem {
			if len(r.Messages) > 0 {
				p.fail(unsupportedf("system message after conversation start"))
			}
			appendSystem(r, textParts(msg.Parts))
			continue
		}
		r.Messages = append(r.Messages, msg)
	}
	return r
}
func openAIText(v any, p *parser) []irPart {
	if s, ok := v.(string); ok {
		return []irPart{{Text: s}}
	}
	var out []irPart
	for _, v := range p.arr(v) {
		m := p.obj(v, "type text")
		if p.str(m["type"]) != "text" {
			p.fail(unsupportedf("non-text OpenAI content"))
		}
		out = append(out, irPart{Text: p.str(m["text"])})
	}
	return out
}
func openAICall(v any, p *parser) *irToolCall {
	t := p.obj(v, "id type function")
	if p.str(t["type"]) != "function" {
		p.fail(unsupportedf("non-function tool call"))
	}
	f := p.obj(t["function"], "name arguments")
	c := &irToolCall{ID: p.required(t["id"]), Name: p.required(f["name"]), Arguments: p.str(f["arguments"])}
	p.fail(jsonObject(c.Arguments))
	return c
}
