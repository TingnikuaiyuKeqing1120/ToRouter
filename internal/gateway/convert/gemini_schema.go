package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Gemini's parameters is a provider Schema, not parametersJsonSchema. Map only
// its documented subset; unknown keywords (including propertyOrdering, whose
// generation-order contract has no JSON Schema equivalent) must not leak into
// another protocol or disappear silently.
func geminiSchema(v any, p *parser) object {
	m := p.obj(v, "type format title description nullable enum maxItems minItems properties required minProperties maxProperties minLength maxLength pattern example anyOf default items minimum maximum")
	out := object{}
	if _, ok := m["type"]; !ok {
		p.fail(malformed("Gemini parameters schema requires type"))
	}
	nullable := false
	for k, v := range m {
		field := &parser{}
		switch k {
		case "type":
			typ := strings.ToLower(field.str(v))
			switch typ {
			case "string", "number", "integer", "boolean", "array", "object", "null":
				out[k] = typ
			default:
				field.fail(unsupportedf("Gemini schema type %q", typ))
			}
		case "nullable":
			nullable = field.boolean(v)
		case "properties":
			properties, ok := v.(object)
			if !ok || properties == nil {
				field.fail(malformed("expected object"))
				break
			}
			converted := object{}
			for name, schema := range properties {
				converted[name] = geminiSchema(schema, field)
			}
			out[k] = converted
		case "items":
			out[k] = geminiSchema(v, field)
		case "anyOf":
			variants := field.arr(v)
			if len(variants) == 0 {
				field.fail(malformed("anyOf must not be empty"))
			}
			converted := make([]any, 0, len(variants))
			for _, schema := range variants {
				converted = append(converted, geminiSchema(schema, field))
			}
			out[k] = converted
		case "maxItems", "minItems", "minProperties", "maxProperties", "minLength", "maxLength":
			// ProtoJSON encodes int64 constraints as strings, and also accepts
			// JSON numbers. JSON Schema requires nonnegative integer numbers.
			if text, ok := v.(string); ok {
				v = json.Number(text)
			}
			out[k] = field.integer(v)
		case "minimum", "maximum":
			field.number(v)
			out[k] = v
		case "enum", "required":
			values := field.strings(v)
			if values == nil {
				values = []string{}
			}
			if k == "enum" && len(values) == 0 {
				field.fail(malformed("enum must not be empty"))
			}
			out[k] = values
		case "format", "title", "description", "pattern":
			out[k] = field.str(v)
		case "example":
			out["examples"] = []any{v}
		case "default":
			out[k] = v
		}
		if field.err != nil {
			p.fail(fmt.Errorf("Gemini parameters %s: %w", k, field.err))
		}
	}
	if nullable && out["type"] != "null" {
		// An outer union also makes null valid when enum/anyOf constrain the
		// original schema. Merely adding null to type would still reject it.
		return object{"anyOf": []any{out, object{"type": "null"}}}
	}
	return out
}
