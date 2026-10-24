package convert

import (
	"encoding/json"
	"strings"
)

func optionalArray(m object, k string, p *parser) []any {
	if v, ok := m[k]; ok {
		return p.arr(v)
	}
	return nil
}
func parameters(r *irRequest, m object, p *parser, maxKey, stopKey string) {
	if v, ok := m["stream"]; ok {
		r.Stream = p.boolean(v)
	}
	if v, ok := m["temperature"]; ok {
		f := p.number(v)
		if f < 0 || f > 2 {
			p.fail(unsupportedf("temperature outside [0,2]"))
		}
		r.Temperature = &f
	}
	if v, ok := m["top_p"]; ok {
		f := p.number(v)
		if f < 0 || f > 1 {
			p.fail(malformed("top_p outside [0,1]"))
		}
		r.TopP = &f
	}
	if v, ok := m[maxKey]; ok {
		n := p.integer(v)
		if n == 0 {
			p.fail(malformed("token limit must be positive"))
		}
		r.MaxTokens = &n
	}
	if v, ok := m[stopKey]; ok {
		if s, ok := v.(string); ok && stopKey == "stop" {
			r.Stop = []string{s}
		} else {
			r.Stop = p.strings(v)
		}
	}
}
func decodeTool(m object, schemaKey string, p *parser) irTool {
	t := irTool{Name: p.required(m["name"]), Description: p.optstr(m, "description"), Schema: object{"type": "object", "properties": object{}}}
	if v, ok := m[schemaKey]; ok {
		s, ok := v.(map[string]any)
		if !ok || s == nil {
			p.fail(malformed("tool schema must be object"))
		} else {
			t.Schema = s
		}
	}
	return t
}
func decodeRequest(b []byte, protocol string) (*irRequest, error) {
	p := &parser{}
	root := decodeRoot(b, p)
	var r *irRequest
	switch protocol {
	case Responses:
		r = decodeResponsesRequest(root, p)
	case OpenAI:
		r = decodeOpenAIRequest(root, p)
	case Anthropic:
		r = decodeAnthropicRequest(root, p)
	case Gemini:
		r = decodeGeminiRequest(root, p)
	}
	if p.err != nil {
		return nil, p.err
	}
	if err := validateRequest(r); err != nil {
		return nil, err
	}
	return r, nil
}
func decodeAnthropicRequest(root object, p *parser) *irRequest {
	m := p.obj(root, "model system messages tools tool_choice max_tokens temperature top_p stop_sequences stream context_management")
	r := &irRequest{}
	if v, ok := m["context_management"]; ok {
		context := p.obj(v, "edits")
		edits := p.arr(context["edits"])
		// Claude's explicit keep-all edit preserves every supplied thinking
		// block, which is already the converter's behavior. Only this exact
		// strategy is a no-op here; cleanup/compaction is never inferred.
		if len(edits) != 1 {
			p.fail(unsupportedf("context_management requires exactly one keep-all thinking edit"))
		} else {
			edit := p.obj(edits[0], "type keep")
			if edit["type"] != "clear_thinking_20251015" || edit["keep"] != "all" {
				p.fail(unsupportedf("context_management supports only clear_thinking_20251015 with keep=all"))
			}
		}
	}
	if v, ok := m["model"]; ok {
		p.required(v)
	}
	parameters(r, m, p, "max_tokens", "stop_sequences")
	if v, ok := m["system"]; ok {
		r.System = textParts(anthropicText(v, p))
	}
	for _, v := range optionalArray(m, "tools", p) {
		t := p.obj(v, "name description input_schema")
		r.Tools = append(r.Tools, decodeTool(t, "input_schema", p))
	}
	if v, ok := m["tool_choice"]; ok {
		t := p.obj(v, "type name")
		mode := p.str(t["type"])
		if mode == "any" {
			mode = TCRequired
		}
		r.ToolChoice = &irToolChoice{Mode: mode}
		if mode == TCTool {
			r.ToolChoice.Name = p.required(t["name"])
		} else if _, ok := t["name"]; ok {
			p.fail(malformed("name only valid for named tool choice"))
		}
	}
	for _, v := range p.arr(m["messages"]) {
		mm := p.obj(v, "role content")
		role := p.str(mm["role"])
		if role != RoleUser && role != RoleAssistant {
			p.fail(unsupportedf("Anthropic role %q", role))
		}
		r.Messages = append(r.Messages, irMessage{Role: role, Parts: anthropicParts(mm["content"], p, true, true)})
	}
	return r
}
func anthropicText(v any, p *parser) []irPart {
	if s, ok := v.(string); ok {
		return []irPart{{Text: s}}
	}
	var parts []irPart
	for _, v := range p.arr(v) {
		m := p.obj(v, "type text")
		if p.str(m["type"]) != "text" {
			p.fail(unsupportedf("non-text content"))
		}
		parts = append(parts, irPart{Text: p.str(m["text"])})
	}
	return parts
}

// anthropicParts decodes text, function tools and native reasoning. results
// enables tool results; thinking enables opaque thinking blocks. Native
// reasoning stays in the IR until the explicit target adaptation step.
func anthropicParts(v any, p *parser, results, thinking bool) []irPart {
	if s, ok := v.(string); ok {
		return []irPart{{Text: s}}
	}
	var parts []irPart
	for _, v := range p.arr(v) {
		raw, ok := v.(map[string]any)
		if !ok {
			p.fail(malformed("content block must be object"))
			continue
		}
		switch raw["type"] {
		case "text":
			m := p.obj(v, "type text")
			parts = append(parts, irPart{Text: p.str(m["text"])})
		case "tool_use":
			m := p.obj(v, "type id name input")
			parts = append(parts, irPart{Tool: &irToolCall{ID: p.required(m["id"]), Name: p.required(m["name"]), Arguments: p.args(m["input"])}})
		case "tool_result":
			if !results {
				p.fail(unsupportedf("tool result in assistant response"))
			}
			m := p.obj(v, "type tool_use_id content is_error")
			if e, ok := m["is_error"]; ok && p.boolean(e) {
				p.fail(unsupportedf("is_error tool results"))
			}
			parts = append(parts, irPart{ToolResult: &irToolResult{ID: p.required(m["tool_use_id"]), Content: textParts(anthropicText(m["content"], p))}})
		case "thinking", "redacted_thinking":
			if !thinking {
				p.fail(unsupportedf("Anthropic block type %v", raw["type"]))
				continue
			}
			if raw["type"] == "thinking" {
				p.obj(v, "type thinking signature")
				p.str(raw["thinking"])
				if v, ok := raw["signature"]; ok {
					p.str(v)
				}
			} else {
				p.obj(v, "type data")
				p.required(raw["data"])
			}
			parts = append(parts, irPart{Thought: true, Reasoning: &irReasoning{Format: Anthropic, Value: raw}})
		default:
			p.fail(unsupportedf("Anthropic block type %v", raw["type"]))
		}
	}
	return parts
}
func decodeGeminiRequest(root object, p *parser) *irRequest {
	m := p.obj(root, "contents systemInstruction tools toolConfig generationConfig")
	r := &irRequest{}
	if v, ok := m["systemInstruction"]; ok {
		s := p.obj(v, "role parts")
		if role, ok := s["role"]; ok {
			if p.str(role) != "system" {
				p.fail(unsupportedf("systemInstruction role"))
			}
		}
		r.System = textParts(geminiParts(s["parts"], p, -1, false, false))
		for _, part := range geminiParts(s["parts"], p, -1, false, false) {
			if part.Tool != nil || part.ToolResult != nil || part.Reasoning != nil || part.Signature != "" {
				p.fail(unsupportedf("non-text system instruction"))
			}
		}
	}
	if v, ok := m["generationConfig"]; ok {
		g := p.obj(v, "maxOutputTokens temperature topP stopSequences")
		n := object{}
		for k, v := range g {
			switch k {
			case "maxOutputTokens":
				n["max_tokens"] = v
			case "topP":
				n["top_p"] = v
			case "stopSequences":
				n["stop"] = v
			default:
				n[k] = v
			}
		}
		parameters(r, n, p, "max_tokens", "stop")
	}
	for _, v := range optionalArray(m, "tools", p) {
		t := p.obj(v, "functionDeclarations")
		for _, v := range p.arr(t["functionDeclarations"]) {
			f := p.obj(v, "name description parameters parametersJsonSchema")
			if _, a := f["parameters"]; a {
				if _, b := f["parametersJsonSchema"]; b {
					p.fail(malformed("two schemas supplied"))
				}
			}
			key := "parameters"
			if _, ok := f["parametersJsonSchema"]; ok {
				key = "parametersJsonSchema"
			}
			tool := decodeTool(f, key, p)
			if schema, ok := f["parameters"]; ok {
				tool.Schema = geminiSchema(schema, p)
			}
			r.Tools = append(r.Tools, tool)
		}
	}
	if v, ok := m["toolConfig"]; ok {
		t := p.obj(v, "functionCallingConfig")
		f := p.obj(t["functionCallingConfig"], "mode allowedFunctionNames")
		mode := p.str(f["mode"])
		switch mode {
		case "AUTO":
			mode = TCAuto
		case "NONE":
			mode = TCNone
		case "ANY":
			mode = TCRequired
		default:
			p.fail(unsupportedf("Gemini tool choice mode %q", mode))
		}
		r.ToolChoice = &irToolChoice{Mode: mode}
		if v, ok := f["allowedFunctionNames"]; ok {
			names := p.strings(v)
			if len(names) != 1 || mode != TCRequired {
				p.fail(unsupportedf("allowedFunctionNames requires ANY with one name"))
			} else {
				r.ToolChoice.Mode = TCTool
				r.ToolChoice.Name = names[0]
			}
		}
	}
	for i, v := range p.arr(m["contents"]) {
		c := p.obj(v, "role parts")
		role := p.optstr(c, "role")
		switch role {
		case "", "user":
			role = RoleUser
		case "model":
			role = RoleAssistant
		default:
			p.fail(unsupportedf("Gemini role %q", role))
		}
		r.Messages = append(r.Messages, irMessage{Role: role, Parts: geminiParts(c["parts"], p, i, true, true)})
	}
	return r
}

// geminiParts retains thought and thoughtSignature on their native part.
// Only explicit target adaptation may remove them, with a recorded loss.
func geminiParts(v any, p *parser, turn int, results, thinking bool) []irPart {
	var out []irPart
	for i, v := range p.arr(v) {
		m := p.obj(v, "text functionCall functionResponse thoughtSignature thought")
		content := 0
		for _, k := range []string{"text", "functionCall", "functionResponse"} {
			if _, ok := m[k]; ok {
				content++
			}
		}
		if content != 1 {
			p.fail(malformed("Gemini part must have exactly one content field"))
		}
		_, signed := m["thoughtSignature"]
		thought := false
		if v, ok := m["thought"]; ok {
			thought = p.boolean(v)
		}
		signature := ""
		if signed {
			signature = p.str(m["thoughtSignature"])
		}

		if v, ok := m["text"]; ok {
			part := irPart{Text: p.str(v), Thought: thought, Signature: signature}
			if thought {
				part.Reasoning = &irReasoning{Format: Gemini, Value: m}
			}
			out = append(out, part)
		}
		if v, ok := m["functionCall"]; ok {
			f := p.obj(v, "id name args")
			name := p.required(f["name"])
			id := p.optstr(f, "id")
			if id == "" {
				id = toolID(turn, i, name)
			}
			out = append(out, irPart{Tool: &irToolCall{ID: id, Name: name, Arguments: p.args(f["args"])}, Signature: signature})
		}
		if v, ok := m["functionResponse"]; ok {
			if !results {
				p.fail(unsupportedf("functionResponse in response"))
			}
			f := p.obj(v, "id name response")
			name := p.required(f["name"])
			content := p.args(f["response"])
			out = append(out, irPart{ToolResult: &irToolResult{ID: p.optstr(f, "id"), Name: name, Content: content, Structured: true}})
		}
	}
	return out
}
func validateRequest(r *irRequest) error {
	if len(r.Messages) == 0 {
		return malformed("messages cannot be empty")
	}
	names := map[string]bool{}
	for _, t := range r.Tools {
		if names[t.Name] {
			return malformed("duplicate tool name %q", t.Name)
		}
		names[t.Name] = true
	}
	if tc := r.ToolChoice; tc != nil {
		switch tc.Mode {
		case TCNone, TCAuto, TCRequired:
		case TCTool:
			if !names[tc.Name] {
				return malformed("unknown selected tool %q", tc.Name)
			}
		default:
			return unsupportedf("tool choice %q", tc.Mode)
		}
		if len(r.Tools) == 0 && tc.Mode != TCNone {
			return malformed("tool choice without tools")
		}
	}
	known := map[string]*irToolCall{}
	resolved := map[string]bool{}
	var order []string
	for _, m := range r.Messages {
		if m.Role != RoleUser && m.Role != RoleAssistant && m.Role != RoleTool {
			return unsupportedf("role %q", m.Role)
		}
		if len(m.Parts) == 0 {
			return malformed("message without content")
		}
		for _, part := range m.Parts {
			if t := part.Tool; t != nil {
				if m.Role != RoleAssistant {
					return malformed("tool call requires assistant role")
				}
				if _, ok := known[t.ID]; ok {
					return malformed("duplicate tool call id %q", t.ID)
				}
				known[t.ID] = t
				order = append(order, t.ID)
			}
			if t := part.ToolResult; t != nil {
				if m.Role == RoleAssistant {
					return malformed("tool result requires user/tool role")
				}
				if t.ID == "" {
					for _, id := range order {
						if !resolved[id] && known[id].Name == t.Name {
							t.ID = id
							break
						}
					}
				}
				c, ok := known[t.ID]
				if !ok || resolved[t.ID] {
					return malformed("unmatched tool result %q", t.ID)
				}
				if t.Name != "" && t.Name != c.Name {
					return malformed("tool result name mismatch")
				}
				t.Name = c.Name
				resolved[t.ID] = true
			}
		}
	}
	return nil
}
func encodeRequest(r *irRequest, to, model string) ([]byte, []string, error) {
	if to == Responses {
		out, err := encodeResponsesRequest(r, model)
		return out, nil, err
	}
	if to == Anthropic && r.Temperature != nil && *r.Temperature > 1 {
		return nil, nil, unsupportedf("Anthropic temperature above 1")
	}
	if to == OpenAI && len(r.Stop) > 4 {
		return nil, nil, unsupportedf("OpenAI supports at most four stops")
	}
	// Anthropic requires an explicit limit; inventing one would silently change
	// the caller's budget. The gateway must supply it before conversion.
	if to == Anthropic && r.MaxTokens == nil {
		return nil, nil, unsupportedf("Anthropic requires max_tokens")
	}
	bypass := to == Gemini && isGemini3(model)
	adapted := []string{}
	m := object{}
	if to != Gemini {
		m["model"] = model
		m["stream"] = r.Stream
	}
	if r.System != "" {
		switch to {
		case Anthropic:
			m["system"] = r.System
		case Gemini:
			m["systemInstruction"] = object{"parts": []any{object{"text": r.System}}}
		}
	}
	var msgs []any
	if to == OpenAI && r.System != "" {
		msgs = append(msgs, object{"role": "system", "content": r.System})
	}
	for _, msg := range r.Messages {
		if to == OpenAI {
			var normal []irPart
			flush := func() {
				if len(normal) > 0 {
					o := object{"role": msg.Role, "content": textParts(normal)}
					if reasoning := chatReasoning(normal); reasoning != "" {
						o["reasoning_content"] = reasoning
					}
					if ts := calls(normal); len(ts) > 0 {
						o["tool_calls"] = openAICalls(ts)
						if o["content"] == "" {
							o["content"] = nil
						}
					}
					msgs = append(msgs, o)
					normal = nil
				}
			}
			seenTool := false
			for _, part := range msg.Parts {
				if part.ToolResult != nil {
					flush()
					t := part.ToolResult
					msgs = append(msgs, object{"role": "tool", "tool_call_id": t.ID, "content": t.Content})
				} else {
					if part.Tool != nil {
						seenTool = true
					} else if seenTool && part.Text != "" {
						return nil, nil, unsupportedf("text after tool call cannot retain OpenAI ordering")
					}
					normal = append(normal, part)
				}
			}
			flush()
		} else {
			role := msg.Role
			if role == RoleTool {
				role = RoleUser
			}
			if to == Gemini && role == RoleAssistant {
				role = "model"
			}
			key := "content"
			if to == Gemini {
				key = "parts"
			}
			parts, used := encodeParts(msg.Parts, to, bypass)
			if used {
				adapted = append(adapted, "thought_signature_bypass")
			}
			if len(msgs) > 0 {
				prev := msgs[len(msgs)-1].(object)
				if prev["role"] == role {
					prev[key] = append(prev[key].([]any), parts...)
					continue
				}
			}
			msgs = append(msgs, object{"role": role, key: parts})
		}
	}
	key := "messages"
	if to == Gemini {
		key = "contents"
	}
	m[key] = msgs
	if len(r.Tools) > 0 {
		var ts []any
		for _, t := range r.Tools {
			f := object{"name": t.Name}
			if t.Description != "" {
				f["description"] = t.Description
			}
			switch to {
			case OpenAI:
				f["parameters"] = t.Schema
				ts = append(ts, object{"type": "function", "function": f})
			case Anthropic:
				f["input_schema"] = t.Schema
				ts = append(ts, f)
			case Gemini:
				f["parametersJsonSchema"] = t.Schema
				ts = append(ts, f)
			}
		}
		if to == Gemini {
			m["tools"] = []any{object{"functionDeclarations": ts}}
		} else {
			m["tools"] = ts
		}
	}
	if tc := r.ToolChoice; tc != nil {
		switch to {
		case OpenAI:
			if tc.Mode == TCTool {
				m["tool_choice"] = object{"type": "function", "function": object{"name": tc.Name}}
			} else {
				m["tool_choice"] = tc.Mode
			}
		case Anthropic:
			t := object{"type": tc.Mode}
			if tc.Mode == TCRequired {
				t["type"] = "any"
			}
			if tc.Mode == TCTool {
				t["name"] = tc.Name
			}
			m["tool_choice"] = t
		case Gemini:
			mode := map[string]string{TCNone: "NONE", TCAuto: "AUTO", TCRequired: "ANY", TCTool: "ANY"}[tc.Mode]
			f := object{"mode": mode}
			if tc.Mode == TCTool {
				f["allowedFunctionNames"] = []string{tc.Name}
			}
			m["toolConfig"] = object{"functionCallingConfig": f}
		}
	}
	cfg := m
	maxKey, topKey, stopKey := "max_tokens", "top_p", "stop"
	if to == Anthropic {
		stopKey = "stop_sequences"
	}
	if to == Gemini {
		cfg = object{}
		maxKey, topKey, stopKey = "maxOutputTokens", "topP", "stopSequences"
	}
	if r.MaxTokens != nil {
		cfg[maxKey] = *r.MaxTokens
	}
	if r.Temperature != nil {
		cfg["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		cfg[topKey] = *r.TopP
	}
	if len(r.Stop) > 0 {
		cfg[stopKey] = r.Stop
	}
	if to == Gemini && len(cfg) > 0 {
		m["generationConfig"] = cfg
	}
	if to == OpenAI && r.Stream {
		m["stream_options"] = object{"include_usage": true}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return out, adapted, nil
}

// isGemini3 reports whether the target model is in the Gemini 3 family, whose
// function calling validates thought signatures. Name-based, like the gateway's
// own pre-check: a false negative costs an upstream 400, a false positive adds
// a documented dummy signature.
func isGemini3(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "gemini-3")
}
func openAICalls(ts []irToolCall) []any {
	out := make([]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, object{"id": t.ID, "type": "function", "function": object{"name": t.Name, "arguments": t.Arguments}})
	}
	return out
}

// encodeParts renders parts for protocol `to`. When bypass is set, tool parts
// carry the provider-documented dummy thought signature; used reports whether
// any part needed it, so the caller can record the adaptation.
func encodeParts(parts []irPart, to string, bypass bool) ([]any, bool) {
	out := make([]any, 0, len(parts))
	used, signed := false, false
	for _, part := range parts {
		if part.Reasoning != nil {
			out = append(out, part.Reasoning.Value)
			continue
		}
		if part.Thought {
			continue
		}
		switch {
		case part.Tool != nil:
			t := part.Tool
			if to == Anthropic {
				out = append(out, object{"type": "tool_use", "id": t.ID, "name": t.Name, "input": rawObject(t.Arguments)})
			} else {
				call := object{"name": t.Name, "args": rawObject(t.Arguments)}
				wirePart := object{"functionCall": call}
				if to == Gemini {
					call["id"] = t.ID
					// The signature sits on the part, next to functionCall, not
					// inside it. Google's shape is
					//   {"functionCall": {...}, "thoughtSignature": "..."}
					// and only the first call of a step strictly needs one.
					if bypass && !signed && part.Signature == "" {
						wirePart["thoughtSignature"] = thoughtSignatureBypass
						used = true
						signed = true
					}
				}
				if part.Signature != "" {
					wirePart["thoughtSignature"] = part.Signature
				}
				out = append(out, wirePart)
			}
		case part.ToolResult != nil:
			t := part.ToolResult
			if to == Anthropic {
				out = append(out, object{"type": "tool_result", "tool_use_id": t.ID, "content": t.Content})
			} else {
				var response any = object{"output": t.Content}
				if t.Structured {
					response = rawObject(t.Content)
				}
				fr := object{"name": t.Name, "response": response}
				if to == Gemini {
					fr["id"] = t.ID
				}
				out = append(out, object{"functionResponse": fr})
			}
		case to == Anthropic:
			out = append(out, object{"type": "text", "text": part.Text})
		default:
			out = append(out, object{"text": part.Text})
		}
	}
	return out, used
}
