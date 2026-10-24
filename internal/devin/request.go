package devin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"

	"torouter/internal/nativeoauth"
	"torouter/internal/store"
)

const signaturePrefix = "torouter-devin-state-v1:"

type signatureState struct {
	Data []byte `json:"data"`
	Type string `json:"type"`
}
type prompt struct {
	Role                   int
	Text, Thinking, ToolID string
	Calls                  []call
	Signature              signatureState
}
type call struct{ ID, Name, Arguments string }
type request struct {
	Model     string
	Streaming bool
	Wire      []byte
	Tools     map[string]bool
}
type object map[string]json.RawMessage

func allowed(value object, names string) error {
	known := map[string]bool{}
	for _, name := range strings.Fields(names) {
		known[name] = true
	}
	for name := range value {
		if !known[name] {
			return fmt.Errorf("unsupported field %q", name)
		}
	}
	return nil
}
func text(raw json.RawMessage) (string, error) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", errors.New("expected text")
	}
	return value, nil
}
func obj(raw json.RawMessage) (object, error) {
	var value object
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, errors.New("expected an object")
	}
	return value, nil
}
func unsupported(err error) error {
	return fmt.Errorf("%w: Devin %s", nativeoauth.ErrUnsupportedRequest, err.Error())
}
func decodeSignature(value string) (signatureState, error) {
	if strings.HasPrefix(value, signaturePrefix) {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, signaturePrefix))
		if err != nil {
			return signatureState{}, errors.New("invalid Devin reasoning state")
		}
		var state signatureState
		if json.Unmarshal(raw, &state) != nil || len(state.Data) == 0 {
			return signatureState{}, errors.New("invalid Devin reasoning state")
		}
		return state, nil
	}
	// A foreign opaque carrier is transported without claiming we can decode
	// its vendor semantics. The upstream may accept or reject it.
	if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
		return signatureState{Data: raw}, nil
	}
	return signatureState{Data: []byte(value)}, nil
}
func encodeSignature(state signatureState) string {
	raw, _ := json.Marshal(state)
	return signaturePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func outID(id string) string {
	if safeID.MatchString(id) && !strings.HasPrefix(id, "dvn_") {
		return id
	}
	return "dvn_" + base64.RawURLEncoding.EncodeToString([]byte(id))
}
func inID(id string) string {
	if strings.HasPrefix(id, "dvn_") {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, "dvn_"))
		if err == nil && outID(string(raw)) == id {
			return string(raw)
		}
	}
	return id
}
func stableID(value string) string {
	b := sha256.Sum256([]byte(value))
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
func parseRequest(raw []byte, headers http.Header, oauth store.OAuthCredential) (request, error) {
	fail := func(err error) (request, error) { return request{}, unsupported(err) }
	root, err := obj(raw)
	if err != nil {
		return fail(err)
	}
	if err = allowed(root, "model input instructions stream max_output_tokens temperature top_p tools tool_choice parallel_tool_calls reasoning text store include"); err != nil {
		return fail(err)
	}
	model, err := text(root["model"])
	if err != nil || model == "" {
		return fail(errors.New("requires an exact model ID from its catalogue"))
	}
	result := request{Model: model, Tools: map[string]bool{}}
	if v, ok := root["stream"]; ok && json.Unmarshal(v, &result.Streaming) != nil {
		return fail(errors.New("invalid stream flag"))
	}
	if v, ok := root["store"]; ok {
		var enabled bool
		if json.Unmarshal(v, &enabled) != nil || enabled {
			return fail(errors.New("does not serve stored Responses; send the full context with store:false"))
		}
	}
	if v, ok := root["parallel_tool_calls"]; ok {
		var enabled bool
		if json.Unmarshal(v, &enabled) != nil || !enabled {
			return fail(errors.New("cannot force serial tool calls"))
		}
	}
	if v, ok := root["include"]; ok {
		var names []string
		if json.Unmarshal(v, &names) != nil {
			return fail(errors.New("invalid include selection"))
		}
		for _, name := range names {
			if name != "reasoning.encrypted_content" {
				return fail(errors.New("unsupported include selection"))
			}
		}
	}
	if v, ok := root["text"]; ok {
		config, e := obj(v)
		if e != nil {
			return fail(e)
		}
		if e = allowed(config, "format"); e != nil {
			return fail(e)
		}
		if format, exists := config["format"]; exists {
			f, e := obj(format)
			if e != nil {
				return fail(e)
			}
			if e = allowed(f, "type"); e != nil {
				return fail(e)
			}
			typ, _ := text(f["type"])
			if typ != "text" {
				return fail(errors.New("cannot enforce structured output; no prompt substitution was applied"))
			}
		}
	}
	if v, ok := root["reasoning"]; ok {
		reasoning, e := obj(v)
		if e != nil {
			return fail(e)
		}
		if e = allowed(reasoning, "effort summary"); e != nil {
			return fail(e)
		}
		if summary, exists := reasoning["summary"]; exists {
			value, e := text(summary)
			if e != nil || value != "auto" {
				return fail(errors.New("cannot choose a reasoning summary verbosity"))
			}
		}
		if effort, exists := reasoning["effort"]; exists {
			value, e := text(effort)
			if e != nil {
				return fail(e)
			}
			uid := strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(model), "-priority"), "-fast")
			if value != "" && !strings.HasSuffix(uid, "-"+value) && !strings.HasSuffix(uid, "_"+value) {
				return fail(errors.New("reasoning effort must match the selected exact model variant"))
			}
		}
	}
	system := ""
	if v, ok := root["instructions"]; ok && string(v) != "null" {
		system, err = text(v)
		if err != nil {
			return fail(err)
		}
	}
	var input []json.RawMessage
	if value, e := text(root["input"]); e == nil {
		item, _ := json.Marshal(map[string]any{"role": "user", "content": value})
		input = []json.RawMessage{item}
	} else if json.Unmarshal(root["input"], &input) != nil || len(input) == 0 {
		return fail(errors.New("requires full input history"))
	}
	prompts := []prompt{}
	pending := map[string]bool{}
	assistant := func() *prompt {
		if len(prompts) == 0 || prompts[len(prompts)-1].Role != 2 {
			prompts = append(prompts, prompt{Role: 2})
		}
		return &prompts[len(prompts)-1]
	}
	for _, rawItem := range input {
		item, e := obj(rawItem)
		if e != nil {
			return fail(e)
		}
		typ := "message"
		if v, ok := item["type"]; ok {
			typ, e = text(v)
			if e != nil {
				return fail(e)
			}
		}
		switch typ {
		case "message":
			if e = allowed(item, "type id status role content"); e != nil {
				return fail(e)
			}
			role, e := text(item["role"])
			if e != nil {
				return fail(e)
			}
			content, e := contentText(item["content"])
			if e != nil {
				return fail(e)
			}
			switch role {
			case "system", "developer":
				if len(prompts) != 0 {
					return fail(errors.New("cannot move a mid-conversation instruction"))
				}
				if system != "" {
					system += "\n\n"
				}
				system += content
			case "user":
				if len(pending) > 0 {
					return fail(errors.New("tool calls need their actual results before a new user turn"))
				}
				prompts = append(prompts, prompt{Role: 1, Text: content})
			case "assistant":
				p := assistant()
				p.Text += content
			default:
				return fail(errors.New("unsupported message role"))
			}
		case "function_call":
			if e = allowed(item, "type id status call_id name arguments"); e != nil {
				return fail(e)
			}
			id, e := text(item["call_id"])
			if e != nil || id == "" || pending[id] {
				return fail(errors.New("invalid or duplicate tool call ID"))
			}
			name, e := text(item["name"])
			if e != nil || name == "" {
				return fail(errors.New("missing tool name"))
			}
			args, e := text(item["arguments"])
			if e != nil {
				return fail(errors.New("invalid tool arguments"))
			}
			if _, e = obj([]byte(args)); e != nil {
				return fail(errors.New("tool arguments must be a JSON object"))
			}
			p := assistant()
			p.Calls = append(p.Calls, call{ID: inID(id), Name: name, Arguments: args})
			pending[id] = true
		case "function_call_output":
			if e = allowed(item, "type id status call_id output"); e != nil {
				return fail(e)
			}
			id, e := text(item["call_id"])
			if e != nil || !pending[id] {
				return fail(errors.New("tool result has no matching call"))
			}
			output, e := text(item["output"])
			if e != nil {
				return fail(errors.New("non-text tool results are not represented by this adapter"))
			}
			prompts = append(prompts, prompt{Role: 4, ToolID: inID(id), Text: output})
			delete(pending, id)
		case "reasoning":
			if e = allowed(item, "type id status summary content encrypted_content"); e != nil {
				return fail(e)
			}
			p := assistant()
			for _, key := range []string{"summary", "content"} {
				if v, ok := item[key]; ok {
					var parts []object
					if json.Unmarshal(v, &parts) != nil {
						return fail(errors.New("invalid reasoning text"))
					}
					for _, part := range parts {
						if e = allowed(part, "type text"); e != nil {
							return fail(e)
						}
						typ, _ := text(part["type"])
						expected := "summary_text"
						if key == "content" {
							expected = "reasoning_text"
						}
						if typ != expected {
							return fail(errors.New("unsupported reasoning text part"))
						}
						value, e := text(part["text"])
						if e != nil {
							return fail(e)
						}
						p.Thinking += value
					}
				}
			}
			if v, ok := item["encrypted_content"]; ok && string(v) != "null" {
				value, e := text(v)
				if e != nil {
					return fail(e)
				}
				state, e := decodeSignature(value)
				if e != nil {
					return fail(e)
				}
				if len(p.Signature.Data) > 0 {
					return fail(errors.New("multiple independent reasoning signatures in one assistant turn"))
				}
				p.Signature = state
			}
		default:
			return fail(fmt.Errorf("cannot reconstruct input item %q", typ))
		}
	}
	if len(pending) > 0 {
		return fail(errors.New("tool calls are missing results; no fabricated tool output was inserted"))
	}
	if len(prompts) == 0 || prompts[len(prompts)-1].Role == 2 {
		return fail(errors.New("requires an actual user turn or tool result; no continuation prompt was inserted"))
	}
	choice := "auto"
	if v, ok := root["tool_choice"]; ok {
		choice, err = text(v)
		if err != nil || choice != "auto" && choice != "none" {
			return fail(errors.New("cannot enforce required or named tool choice"))
		}
	}
	wire := message{}.bytes(1, metadata(oauth.AccessToken))
	if system != "" {
		wire = wire.text(2, system)
	}
	for i, p := range prompts {
		raw, _ := json.Marshal(p)
		part := message{}.text(1, stableID(fmt.Sprintf("%s:%d:%s", oauth.Account, i, raw))).integer(2, uint64(p.Role)).text(3, p.Text)
		for _, c := range p.Calls {
			part = part.bytes(6, message{}.text(1, c.ID).text(2, c.Name).text(3, c.Arguments))
		}
		if p.ToolID != "" {
			part = part.text(7, p.ToolID)
		}
		if p.Thinking != "" {
			part = part.text(11, p.Thinking)
		}
		if len(p.Signature.Data) > 0 {
			part = part.bytes(12, p.Signature.Data).text(18, p.Signature.Type)
		}
		wire = wire.bytes(3, part)
	}
	var tools []object
	if v, ok := root["tools"]; ok {
		if json.Unmarshal(v, &tools) != nil {
			return fail(errors.New("invalid tools"))
		}
	}
	for _, tool := range tools {
		if e := allowed(tool, "type name description parameters strict"); e != nil {
			return fail(e)
		}
		kind, _ := text(tool["type"])
		name, e := text(tool["name"])
		if e != nil || kind != "function" || name == "" {
			return fail(errors.New("only named function tools are supported"))
		}
		if v, ok := tool["strict"]; ok {
			var strict bool
			if json.Unmarshal(v, &strict) != nil || strict {
				return fail(errors.New("cannot guarantee strict tool schema enforcement"))
			}
		}
		desc := ""
		if v, ok := tool["description"]; ok {
			desc, e = text(v)
			if e != nil {
				return fail(e)
			}
		}
		schema, e := obj(tool["parameters"])
		if e != nil {
			return fail(errors.New("function parameters must be an explicit object schema"))
		}
		schemaType, _ := text(schema["type"])
		if schemaType != "object" {
			return fail(errors.New("non-object tool root is not reconstructed"))
		}
		if choice == "none" {
			continue
		}
		if result.Tools[name] {
			return fail(errors.New("duplicate tool declaration"))
		}
		result.Tools[name] = true
		wire = wire.bytes(10, message{}.text(1, name).text(2, desc).bytes(3, tool["parameters"]))
	}
	max := uint64(128000)
	if v, ok := root["max_output_tokens"]; ok {
		if json.Unmarshal(v, &max) != nil || max == 0 || max > 1<<31 {
			return fail(errors.New("invalid max_output_tokens"))
		}
	}
	temperature, topP := 1.0, 0.95
	for _, setting := range []struct {
		name   string
		target *float64
		max    float64
	}{{"temperature", &temperature, 2}, {"top_p", &topP, 1}} {
		if v, ok := root[setting.name]; ok {
			if json.Unmarshal(v, setting.target) != nil || math.IsNaN(*setting.target) || *setting.target < 0 || *setting.target > setting.max {
				return fail(fmt.Errorf("invalid %s", setting.name))
			}
		}
	}
	session := headers.Get("X-Session-Id")
	if session == "" {
		for _, p := range prompts {
			if p.Role == 1 {
				session = p.Text
				break
			}
		}
	}
	cascade := stableID(oauth.Account + ":" + session)
	wire = wire.integer(7, 5).bytes(8, message{}.integer(1, 1).integer(2, max).integer(3, 400).decimal(5, temperature).integer(7, 40).decimal(8, topP)).text(16, cascade).text(21, model)
	if len(wire) > maxWire {
		return fail(errors.New("encoded request exceeds size limit"))
	}
	result.Wire = frame(wire)
	return result, nil
}
func contentText(raw json.RawMessage) (string, error) {
	if value, err := text(raw); err == nil {
		return value, nil
	}
	var parts []object
	if json.Unmarshal(raw, &parts) != nil {
		return "", errors.New("unsupported message content")
	}
	var out strings.Builder
	for _, p := range parts {
		if err := allowed(p, "type text annotations"); err != nil {
			return "", err
		}
		typ, _ := text(p["type"])
		if typ != "input_text" && typ != "output_text" {
			return "", errors.New("this adapter currently requires text content")
		}
		value, err := text(p["text"])
		if err != nil {
			return "", err
		}
		out.WriteString(value)
		if v, ok := p["annotations"]; ok && string(v) != "[]" {
			return "", errors.New("cannot preserve message annotations")
		}
	}
	return out.String(), nil
}
