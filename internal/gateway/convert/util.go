package convert

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type object = map[string]any

func unsupportedf(f string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(f, a...))
}
func malformed(f string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(f, a...))
}
func strictJSON(b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(dst); err != nil {
		return malformed("%v", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return malformed("trailing JSON")
	}
	return nil
}

type parser struct {
	err     error
	dropped []string
}

func (p *parser) fail(e error) {
	if p.err == nil {
		p.err = e
	}
}

// drop records a reasoning carrier that is being stripped because the target
// format cannot express it. Every strip must be recorded so the gateway can
// report it; silent loss is never acceptable.
func (p *parser) drop(kind string) {
	for _, k := range p.dropped {
		if k == kind {
			return
		}
	}
	p.dropped = append(p.dropped, kind)
}
func (p *parser) obj(v any, allowed string) object {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		p.fail(malformed("expected object"))
		return object{}
	}
	for k := range m {
		found := false
		for _, field := range strings.Fields(allowed) {
			if k == field {
				found = true
				break
			}
		}
		if !found {
			p.fail(unsupportedf("field %q", k))
		}
	}
	return m
}
func (p *parser) arr(v any) []any {
	a, ok := v.([]any)
	if !ok {
		p.fail(malformed("expected array"))
	}
	return a
}
func (p *parser) str(v any) string {
	s, ok := v.(string)
	if !ok {
		p.fail(malformed("expected string"))
	}
	return s
}
func (p *parser) required(v any) string {
	s := p.str(v)
	if s == "" {
		p.fail(malformed("required nonempty string"))
	}
	return s
}
func (p *parser) optstr(m object, k string) string {
	if v, ok := m[k]; ok {
		return p.str(v)
	}
	return ""
}

// Reasoning is nullable on OpenAI-format responses and deltas. Null and empty
// strings carry no continuation state; other types are malformed, not lossy.
func (p *parser) hasReasoning(v any) bool {
	if v == nil {
		return false
	}
	s, ok := v.(string)
	if !ok {
		p.fail(malformed("reasoning_content must be string or null"))
		return false
	}
	return s != ""
}
func (p *parser) boolean(v any) bool {
	b, ok := v.(bool)
	if !ok {
		p.fail(malformed("expected boolean"))
	}
	return b
}
func (p *parser) integer(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		p.fail(malformed("expected integer"))
		return 0
	}
	i, e := n.Int64()
	if e != nil || i < 0 {
		p.fail(malformed("invalid nonnegative integer"))
	}
	return i
}
func (p *parser) number(v any) float64 {
	n, ok := v.(json.Number)
	if !ok {
		p.fail(malformed("expected number"))
		return 0
	}
	f, e := n.Float64()
	if e != nil {
		p.fail(malformed("invalid number"))
	}
	return f
}
func (p *parser) strings(v any) []string {
	var out []string
	for _, x := range p.arr(v) {
		out = append(out, p.str(x))
	}
	return out
}
func (p *parser) args(v any) string {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		p.fail(malformed("tool arguments must be object"))
		return "{}"
	}
	b, e := json.Marshal(m)
	if e != nil {
		p.fail(e)
	}
	return string(b)
}
func jsonObject(s string) error {
	var m any
	if e := strictJSON([]byte(s), &m); e != nil {
		return e
	}
	if o, ok := m.(map[string]any); !ok || o == nil {
		return malformed("arguments must be JSON object")
	}
	return nil
}
func rawObject(s string) json.RawMessage { return json.RawMessage(s) }
func stableID(kind string, parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return fmt.Sprintf("%s_%x", kind, h[:12])
}
func toolID(turn, index int, name string) string {
	return stableID("call", strconv.Itoa(turn), strconv.Itoa(index), name)
}
func textParts(parts []irPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Tool == nil && p.ToolResult == nil && !p.Thought && p.Reasoning == nil {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
func calls(parts []irPart) []irToolCall {
	var out []irToolCall
	for _, p := range parts {
		if p.Tool != nil {
			out = append(out, *p.Tool)
		}
	}
	return out
}
func hasTools(parts []irPart) bool { return len(calls(parts)) > 0 }
func decodeRoot(b []byte, p *parser) object {
	var v any
	if e := strictJSON(b, &v); e != nil {
		p.fail(e)
		return object{}
	}
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		p.fail(malformed("root must be object"))
		return object{}
	}
	return m
}
func appendSystem(r *irRequest, s string) {
	if r.System != "" {
		r.System += "\n\n"
	}
	r.System += s
}
