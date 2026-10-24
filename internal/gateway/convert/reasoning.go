package convert

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Options is shared by requests, responses and streams. Scope is an opaque,
// trusted authorization boundary supplied by the gateway, never by the client.
// Source identifies the actual upstream continuation domain. A source change
// is not an authorization failure and does not prove state incompatibility.
type Options struct {
	AllowReasoningDrop bool
	SigningKey         []byte
	Scope              string
	Source             string
	Bridge             *ReasoningBridge
}

var ErrReasoningState = errors.New("convert: reasoning attachment unavailable or outside access scope")

const attachmentPrefix = "to-router:reasoning:v1:"
const bridgeToolPrefix = "call_trb_"

var attachmentKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

type reasoningEnvelope struct {
	Scope  string   `json:"scope"`
	Source string   `json:"source"`
	Parts  []irPart `json:"parts"`
}

// ReasoningBridge holds only reasoning attachments and their tool linkage. It
// never stores prompts or full conversations. There is no background goroutine.
// TTL, entry count and aggregate bytes are bounded; expired/missing handles fail
// closed instead of guessing which previous state belongs to a call.
type ReasoningBridge struct {
	mu       sync.Mutex
	entries  map[string]bridgeEntry
	capacity int
	ttl      time.Duration
	bytes    int
}
type bridgeEntry struct {
	value   []byte
	expires time.Time
}

func NewReasoningBridge(capacity int, ttl time.Duration) *ReasoningBridge {
	if capacity < 1 {
		capacity = 512
	}
	if capacity > 4096 {
		capacity = 4096
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if ttl > 24*time.Hour {
		ttl = 24 * time.Hour
	}
	return &ReasoningBridge{entries: map[string]bridgeEntry{}, capacity: capacity, ttl: ttl}
}
func scopeDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func signingKey(opts Options) []byte {
	if len(opts.SigningKey) > 0 {
		return opts.SigningKey
	}
	return attachmentKey
}
func sealAttachment(parts []irPart, opts Options) (string, error) {
	b, err := json.Marshal(reasoningEnvelope{Scope: scopeDigest(opts.Scope), Source: opts.Source, Parts: parts})
	if err != nil {
		return "", err
	}
	if len(b) > maxLineLen {
		return "", ErrLineLimit
	}
	h := hmac.New(sha256.New, signingKey(opts))
	h.Write(b)
	return attachmentPrefix + base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil)), nil
}
func openAttachment(s string, opts Options) ([]irPart, error) {
	if !strings.HasPrefix(s, attachmentPrefix) {
		return nil, ErrReasoningState
	}
	data, mac, ok := strings.Cut(strings.TrimPrefix(s, attachmentPrefix), ".")
	if !ok {
		return nil, ErrReasoningState
	}
	b, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil || len(b) > maxLineLen {
		return nil, ErrReasoningState
	}
	sig, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		return nil, ErrReasoningState
	}
	h := hmac.New(sha256.New, signingKey(opts))
	h.Write(b)
	if !hmac.Equal(sig, h.Sum(nil)) {
		return nil, ErrReasoningState
	}
	var env reasoningEnvelope
	if strictJSON(b, &env) != nil || env.Scope != scopeDigest(opts.Scope) {
		return nil, ErrReasoningState
	}
	return env.Parts, nil
}
func (b *ReasoningBridge) put(value string, scope string) (string, error) {
	if b == nil || scope == "" {
		return "", ErrReasoningState
	}
	if len(value) > maxLineLen {
		return "", ErrLineLimit
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for k, v := range b.entries {
		if !now.Before(v.expires) {
			delete(b.entries, k)
			b.bytes -= len(v.value)
		}
	}
	id := bridgeToolPrefix + strings.TrimPrefix(stableID("", scope, value), "_")
	if _, ok := b.entries[id]; ok {
		return id, nil
	}
	for len(b.entries) >= b.capacity || b.bytes+len(value) > 64<<20 {
		oldest := ""
		var deadline time.Time
		for k, v := range b.entries {
			if oldest == "" || v.expires.Before(deadline) {
				oldest = k
				deadline = v.expires
			}
		}
		if oldest == "" {
			return "", ErrReasoningState
		}
		b.bytes -= len(b.entries[oldest].value)
		delete(b.entries, oldest)
	}
	b.entries[id] = bridgeEntry{value: []byte(value), expires: now.Add(b.ttl)}
	b.bytes += len(value)
	return id, nil
}
func (b *ReasoningBridge) get(id string, opts Options) ([]irPart, error) {
	if b == nil || opts.Scope == "" {
		return nil, ErrReasoningState
	}
	b.mu.Lock()
	v, ok := b.entries[id]
	if ok && !time.Now().Before(v.expires) {
		delete(b.entries, id)
		b.bytes -= len(v.value)
		ok = false
	}
	b.mu.Unlock()
	if !ok {
		return nil, ErrReasoningState
	}
	return openAttachment(string(v.value), opts)
}

func normalizeProtocol(s string) string {
	switch s {
	case OpenAIChatCompletions:
		return OpenAI
	case OpenAIResponses:
		return Responses
	case AnthropicMessages:
		return Anthropic
	case GeminiGenerateContent:
		return Gemini
	}
	return s
}
func uniqueKinds(kinds []string) []string {
	var out []string
	for _, k := range kinds {
		found := false
		for _, v := range out {
			found = found || k == v
		}
		if !found {
			out = append(out, k)
		}
	}
	return out
}
func chatReasoning(parts []irPart) string {
	var b strings.Builder
	for _, part := range parts {
		if r := part.Reasoning; r != nil && r.Format == OpenAI {
			s, _ := r.Value["reasoning_content"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

func attachmentFromPart(part irPart) string {
	if part.Reasoning != nil {
		for _, key := range []string{"signature", "data", "encrypted_content"} {
			if s, ok := part.Reasoning.Value[key].(string); ok && strings.HasPrefix(s, attachmentPrefix) {
				return s
			}
		}
	}
	return ""
}
func unwrapParts(parts []irPart, opts Options) ([]irPart, error) {
	var out []irPart
	var visible *irPart
	for _, part := range parts {
		if visible != nil {
			previous := visible
			visible = nil
			if sameVisiblePart(*previous, part) {
				continue
			}
		}
		if token := attachmentFromPart(part); token != "" {
			restored, err := openAttachment(token, opts)
			if err != nil {
				return nil, err
			}
			out = append(out, restored...)
			if len(restored) == 1 && restored[0].Reasoning == nil {
				copy := restored[0]
				visible = &copy
			}
		} else {
			out = append(out, part)
		}
	}
	return out, nil
}

func sameVisiblePart(a, b irPart) bool {
	if a.Tool != nil && b.Tool != nil {
		return *a.Tool == *b.Tool
	}
	return a.Tool == nil && b.Tool == nil && a.Reasoning == nil && b.Reasoning == nil && a.Text == b.Text
}

func restoreBridgePositions(parts []irPart) []irPart {
	var normal []irPart
	var attached []irPart
	for _, p := range parts {
		if p.BridgePosition != nil {
			attached = append(attached, p)
		} else {
			normal = append(normal, p)
		}
	}
	for _, p := range attached {
		index := *p.BridgePosition
		p.BridgePosition = nil
		if index < 0 {
			index = 0
		}
		if index > len(normal) {
			index = len(normal)
		}
		normal = append(normal, irPart{})
		copy(normal[index+1:], normal[index:])
		normal[index] = p
	}
	return normal
}

func prepareRequestReasoning(r *irRequest, to string, opts Options) ([]string, error) {
	var drops []string
	ids := map[string]string{}
	for i := range r.Messages {
		msg := &r.Messages[i]
		parts, err := unwrapParts(msg.Parts, opts)
		if err != nil {
			return nil, err
		}
		msg.Parts = parts
		var expanded []irPart
		for _, part := range msg.Parts {
			if t := part.Tool; t != nil && strings.HasPrefix(t.ID, bridgeToolPrefix) {
				original, err := opts.Bridge.get(t.ID, opts)
				if err != nil {
					return nil, err
				}
				if len(original) == 0 || original[len(original)-1].Tool == nil {
					return nil, ErrReasoningState
				}
				prev := original[len(original)-1].Tool
				if t.Name != prev.Name || !equalArguments(t.Arguments, prev.Arguments) {
					return nil, ErrReasoningState
				}
				ids[t.ID] = prev.ID
				expanded = append(expanded, original...)
			} else {
				expanded = append(expanded, part)
			}
		}
		msg.Parts = restoreBridgePositions(expanded)
		for j := range msg.Parts {
			part := &msg.Parts[j]
			if t := part.ToolResult; t != nil {
				if id, ok := ids[t.ID]; ok {
					t.ID = id
				}
			}
		}
		adaptedParts, partsDrops, err := adaptReasoning(msg.Parts, to, opts, false)
		msg.Parts = adaptedParts
		if err != nil {
			return nil, err
		}
		drops = append(drops, partsDrops...)
	}
	if err := validateRequest(r); err != nil {
		return nil, err
	}
	return uniqueKinds(drops), nil
}

func prepareResponseReasoning(parts []irPart, to string, opts Options) ([]irPart, []string, error) {
	return adaptReasoning(parts, to, opts, true)
}

func reasoningText(r *irReasoning) string {
	switch r.Format {
	case OpenAI:
		s, _ := r.Value["reasoning_content"].(string)
		return s
	case Anthropic:
		s, _ := r.Value["thinking"].(string)
		return s
	case Gemini:
		s, _ := r.Value["text"].(string)
		return s
	case Responses:
		var b strings.Builder
		for _, key := range []string{"content", "summary"} {
			if a, ok := r.Value[key].([]any); ok {
				for _, v := range a {
					if m, ok := v.(map[string]any); ok {
						s, _ := m["text"].(string)
						b.WriteString(s)
					}
				}
			}
		}
		return b.String()
	}
	return ""
}
func opaqueReasoning(part irPart) bool {
	if part.Signature != "" {
		return true
	}
	if r := part.Reasoning; r != nil {
		for _, key := range []string{"signature", "data", "encrypted_content", "thoughtSignature"} {
			if s, ok := r.Value[key].(string); ok && s != "" {
				return true
			}
		}
	}
	return false
}

func adaptReasoning(parts []irPart, to string, opts Options, response bool) ([]irPart, []string, error) {
	var out []irPart
	var drops []string
	var pending []irPart
	for position, part := range parts {
		foreign := part.Reasoning != nil && part.Reasoning.Format != to || part.Signature != "" && to != Gemini
		if response && (to == OpenAI || to == Gemini) && opts.Bridge != nil && opts.Scope != "" {
			if foreign && opaqueReasoning(part) && part.Tool == nil && part.Reasoning != nil {
				if part.BridgePosition == nil {
					index := position
					part.BridgePosition = &index
				}
				pending = append(pending, part)
				continue
			}
			if part.Tool != nil && (len(pending) > 0 || foreign && opaqueReasoning(part)) {
				original := part
				copied := *part.Tool
				original.Tool = &copied
				token, err := sealAttachment(append(pending, original), opts)
				if err != nil {
					return nil, nil, err
				}
				id, err := opts.Bridge.put(token, opts.Scope)
				if err != nil {
					return nil, nil, err
				}
				call := *part.Tool
				call.ID = id
				part.Tool = &call
				part.Signature = ""
				pending = nil
				out = append(out, part)
				continue
			}
		}
		if !foreign {
			out = append(out, part)
			continue
		}
		if response && (to == Anthropic || to == Responses) {
			token, err := sealAttachment([]irPart{part}, opts)
			if err != nil {
				return nil, nil, err
			}
			value := object{"type": "redacted_thinking", "data": token}
			if to == Responses {
				value = object{"type": "reasoning", "summary": []any{}, "encrypted_content": token}
			}
			out = append(out, irPart{Thought: true, Reasoning: &irReasoning{Format: to, Value: value}})
			if part.Reasoning == nil {
				copy := part
				copy.Signature = ""
				out = append(out, copy)
			}
			continue
		}
		if opaqueReasoning(part) {
			if !opts.AllowReasoningDrop {
				return nil, nil, fmt.Errorf("%w: %w", ErrContinuationState, unsupportedf("reasoning state cannot be represented in %s; enable protocol conversion reasoning drop in settings", to))
			}
			drops = append(drops, "reasoning_state_dropped")
			if part.Reasoning == nil {
				part.Signature = ""
				out = append(out, part)
			}
			continue
		}
		if part.Reasoning != nil {
			text := reasoningText(part.Reasoning)
			switch to {
			case OpenAI:
				part.Reasoning = &irReasoning{Format: to, Value: object{"reasoning_content": text}}
			case Anthropic:
				part.Reasoning = &irReasoning{Format: to, Value: object{"type": "thinking", "thinking": text}}
			case Responses:
				part.Reasoning = &irReasoning{Format: to, Value: object{"type": "reasoning", "summary": []any{}, "content": []any{object{"type": "reasoning_text", "text": text}}}}
			case Gemini:
				part.Reasoning = &irReasoning{Format: to, Value: object{"text": text, "thought": true}}
				part.Text = text
			}
		}
		out = append(out, part)
	}
	if len(pending) > 0 {
		if !opts.AllowReasoningDrop {
			return nil, nil, fmt.Errorf("%w: %w", ErrContinuationState, unsupportedf("reasoning attachment has no following tool to carry its reference"))
		}
		drops = append(drops, "reasoning_state_dropped")
	}
	return out, uniqueKinds(drops), nil
}

// stripCacheHints only touches known Anthropic cache_control positions. Tool
// schemas, arguments and JSON-looking user text are never traversed as hints.
func stripCacheHints(body []byte, from string) ([]byte, []string, error) {
	if from != Anthropic {
		return body, nil, nil
	}
	p := &parser{}
	root := decodeRoot(body, p)
	if p.err != nil {
		return nil, nil, p.err
	}
	removed := false
	strip := func(v any) {
		if m, ok := v.(map[string]any); ok {
			if _, ok := m["cache_control"]; ok {
				delete(m, "cache_control")
				removed = true
			}
		}
	}
	strip(root)
	for _, key := range []string{"system", "tools"} {
		if a, ok := root[key].([]any); ok {
			for _, v := range a {
				strip(v)
			}
		}
	}
	if a, ok := root["messages"].([]any); ok {
		for _, v := range a {
			if msg, ok := v.(map[string]any); ok {
				if blocks, ok := msg["content"].([]any); ok {
					for _, b := range blocks {
						strip(b)
						if block, ok := b.(map[string]any); ok && block["type"] == "tool_result" {
							if a, ok := block["content"].([]any); ok {
								for _, v := range a {
									strip(v)
								}
							}
						}
					}
				}
			}
		}
	}
	if !removed {
		return body, nil, nil
	}
	b, err := json.Marshal(root)
	return b, []string{"cache_control_removed"}, err
}

// PrepareRequestWithOptions restores router transport on native passthrough
// paths. Unrelated native fields, blocks and settings are preserved. Requests
// without an actual router carrier remain byte-for-byte intact.
func PrepareRequestWithOptions(body []byte, protocol string, opts Options) ([]byte, []string, error) {
	if !bytes.Contains(body, []byte(attachmentPrefix)) && !bytes.Contains(body, []byte(bridgeToolPrefix)) {
		return body, nil, nil
	}
	protocol = normalizeProtocol(protocol)
	p := &parser{}
	root := decodeRoot(body, p)
	if p.err != nil {
		return nil, nil, p.err
	}
	changed := false
	var drops []string
	ids := map[string]string{}
	nativeParts := func(parts []irPart) ([]any, error) {
		adapted, loss, err := adaptReasoning(parts, protocol, opts, false)
		if err != nil {
			return nil, err
		}
		drops = append(drops, loss...)
		if protocol == Responses {
			return responsesItems(adapted, RoleAssistant), nil
		}
		out, _ := encodeParts(adapted, protocol, false)
		return out, nil
	}
	restoreArray := func(raw []any) ([]any, error) {
		var out []any
		var visible *irPart
		for _, v := range raw {
			m, ok := v.(map[string]any)
			if !ok {
				out = append(out, v)
				continue
			}
			if visible != nil {
				prev := visible
				visible = nil
				if rawMatchesPart(m, *prev, protocol) {
					continue
				}
			}
			token := ""
			for _, key := range []string{"signature", "data", "encrypted_content"} {
				if s, ok := m[key].(string); ok && strings.HasPrefix(s, attachmentPrefix) {
					token = s
				}
			}
			if token != "" {
				restored, err := openAttachment(token, opts)
				if err != nil {
					return nil, err
				}
				encoded, err := nativeParts(restored)
				if err != nil {
					return nil, err
				}
				out = append(out, encoded...)
				changed = true
				if len(restored) == 1 && restored[0].Reasoning == nil {
					copy := restored[0]
					visible = &copy
				}
				continue
			}
			id, name, args := rawTool(m, protocol)
			if strings.HasPrefix(id, bridgeToolPrefix) {
				original, err := opts.Bridge.get(id, opts)
				if err != nil {
					return nil, err
				}
				if len(original) == 0 || original[len(original)-1].Tool == nil {
					return nil, ErrReasoningState
				}
				call := original[len(original)-1].Tool
				if call.Name != name || !equalArguments(call.Arguments, args) {
					return nil, ErrReasoningState
				}
				ids[id] = call.ID
				changed = true
				for _, part := range original {
					position := part.BridgePosition
					part.BridgePosition = nil
					encoded, err := nativeParts([]irPart{part})
					if err != nil {
						return nil, err
					}
					if position != nil && *position >= 0 && *position <= len(out) {
						index := *position
						out = append(out[:index], append(encoded, out[index:]...)...)
					} else {
						out = append(out, encoded...)
					}
				}
				continue
			}
			switch protocol {
			case Anthropic:
				if m["type"] == "tool_result" {
					if id, ok := m["tool_use_id"].(string); ok {
						if old, ok := ids[id]; ok {
							m["tool_use_id"] = old
							changed = true
						} else if strings.HasPrefix(id, bridgeToolPrefix) {
							return nil, ErrReasoningState
						}
					}
				}
			case Responses:
				if m["type"] == "function_call_output" {
					if id, ok := m["call_id"].(string); ok {
						if old, ok := ids[id]; ok {
							m["call_id"] = old
							changed = true
						} else if strings.HasPrefix(id, bridgeToolPrefix) {
							return nil, ErrReasoningState
						}
					}
				}
			case Gemini:
				if result, ok := m["functionResponse"].(map[string]any); ok {
					if id, ok := result["id"].(string); ok {
						if old, ok := ids[id]; ok {
							result["id"] = old
							changed = true
						} else if strings.HasPrefix(id, bridgeToolPrefix) {
							return nil, ErrReasoningState
						}
					}
				}
			}
			out = append(out, v)
		}
		return out, nil
	}
	switch protocol {
	case Anthropic, Gemini:
		key, content := "messages", "content"
		if protocol == Gemini {
			key, content = "contents", "parts"
		}
		if messages, ok := root[key].([]any); ok {
			for _, v := range messages {
				if m, ok := v.(map[string]any); ok {
					if raw, ok := m[content].([]any); ok {
						out, err := restoreArray(raw)
						if err != nil {
							return nil, nil, err
						}
						m[content] = out
					}
				}
			}
		}
	case Responses:
		if raw, ok := root["input"].([]any); ok {
			out, err := restoreArray(raw)
			if err != nil {
				return nil, nil, err
			}
			root["input"] = out
		}
	case OpenAI:
		if messages, ok := root["messages"].([]any); ok {
			for _, v := range messages {
				m, ok := v.(map[string]any)
				if !ok {
					continue
				}
				if calls, ok := m["tool_calls"].([]any); ok {
					for i, raw := range calls {
						call, ok := raw.(map[string]any)
						if !ok {
							continue
						}
						id, _ := call["id"].(string)
						if !strings.HasPrefix(id, bridgeToolPrefix) {
							continue
						}
						original, err := opts.Bridge.get(id, opts)
						if err != nil {
							return nil, nil, err
						}
						if len(original) == 0 || original[len(original)-1].Tool == nil {
							return nil, nil, ErrReasoningState
						}
						prev := original[len(original)-1].Tool
						fn, ok := call["function"].(map[string]any)
						if !ok {
							return nil, nil, ErrReasoningState
						}
						args, _ := fn["arguments"].(string)
						if fn["name"] != prev.Name || !equalArguments(args, prev.Arguments) {
							return nil, nil, ErrReasoningState
						}
						adapted, loss, err := adaptReasoning(original, protocol, opts, false)
						if err != nil {
							return nil, nil, err
						}
						drops = append(drops, loss...)
						reasoning := chatReasoning(adapted)
						if reasoning != "" {
							if existing, ok := m["reasoning_content"].(string); ok && existing != "" && existing != reasoning {
								return nil, nil, ErrReasoningState
							}
							m["reasoning_content"] = reasoning
						}
						calls[i] = openAICalls([]irToolCall{*prev})[0]
						ids[id] = prev.ID
						changed = true
					}
				}
				if m["role"] == "tool" {
					if id, ok := m["tool_call_id"].(string); ok {
						if old, ok := ids[id]; ok {
							m["tool_call_id"] = old
							changed = true
						} else if strings.HasPrefix(id, bridgeToolPrefix) {
							return nil, nil, ErrReasoningState
						}
					}
				}
			}
		}
	default:
		return nil, nil, ErrUnknownProtocol
	}
	if !changed {
		return body, nil, nil
	}
	out, err := json.Marshal(root)
	return out, uniqueKinds(drops), err
}

func equalArguments(a, b string) bool {
	var av, bv any
	return strictJSON([]byte(a), &av) == nil && strictJSON([]byte(b), &bv) == nil && reflect.DeepEqual(av, bv)
}
func rawTool(m object, protocol string) (string, string, string) {
	var id, name, args string
	switch protocol {
	case Anthropic:
		if m["type"] != "tool_use" {
			return "", "", ""
		}
		id, _ = m["id"].(string)
		name, _ = m["name"].(string)
		b, _ := json.Marshal(m["input"])
		args = string(b)
	case Responses:
		if m["type"] != "function_call" {
			return "", "", ""
		}
		id, _ = m["call_id"].(string)
		name, _ = m["name"].(string)
		args, _ = m["arguments"].(string)
	case Gemini:
		f, ok := m["functionCall"].(map[string]any)
		if !ok {
			return "", "", ""
		}
		id, _ = f["id"].(string)
		name, _ = f["name"].(string)
		b, _ := json.Marshal(f["args"])
		args = string(b)
	}
	return id, name, args
}
func rawMatchesPart(m object, part irPart, protocol string) bool {
	if part.Tool != nil {
		id, name, args := rawTool(m, protocol)
		return id == part.Tool.ID && name == part.Tool.Name && equalArguments(args, part.Tool.Arguments)
	}
	if protocol == Anthropic {
		return m["type"] == "text" && m["text"] == part.Text
	}
	if protocol == Gemini {
		return m["text"] == part.Text
	}
	return false
}
