package convert

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const nativeGeminiSchema = `{
  "type":"OBJECT", "title":"payload", "minProperties":"1", "maxProperties":"5",
  "properties":{
    "name":{"type":"STRING","nullable":true,"enum":["ok","ready"],"minLength":"2","maxLength":"5","pattern":"^[a-z]+$","description":"name","default":"ok","example":"ready"},
    "list":{"type":"ARRAY","minItems":"1","maxItems":"3","items":{"type":"OBJECT","properties":{"n":{"type":"INTEGER","minimum":0,"maximum":3}},"required":["n"]}},
    "score":{"type":"NUMBER","minimum":-1.25,"maximum":10.5},
    "active":{"type":"BOOLEAN","nullable":false},
    "choice":{"type":"STRING","nullable":true,"anyOf":[{"type":"STRING","enum":["red"]},{"type":"STRING","minLength":"4"}]}
  },
  "required":["name","list"]
}`

const canonicalGeminiSchema = `{
  "type":"object", "title":"payload", "minProperties":1, "maxProperties":5,
  "properties":{
    "name":{"anyOf":[{"type":"string","enum":["ok","ready"],"minLength":2,"maxLength":5,"pattern":"^[a-z]+$","description":"name","default":"ok","examples":["ready"]},{"type":"null"}]},
    "list":{"type":"array","minItems":1,"maxItems":3,"items":{"type":"object","properties":{"n":{"type":"integer","minimum":0,"maximum":3}},"required":["n"]}},
    "score":{"type":"number","minimum":-1.25,"maximum":10.5},
    "active":{"type":"boolean"},
    "choice":{"anyOf":[{"type":"string","anyOf":[{"type":"string","enum":["red"]},{"type":"string","minLength":4}]},{"type":"null"}]}
  },
  "required":["name","list"]
}`

func convertedGeminiSchema(t *testing.T, schemaKey, schema, to string) (object, error) {
	t.Helper()
	body := `{"contents":[{"role":"user","parts":[{"text":"go"}]}],"generationConfig":{"maxOutputTokens":128},"tools":[{"functionDeclarations":[{"name":"f","` + schemaKey + `":` + schema + `}]}]}`
	converted, err := Request([]byte(body), Gemini, to, "m")
	if err != nil {
		return nil, err
	}
	var root object
	if err := json.Unmarshal(converted, &root); err != nil {
		t.Fatal(err)
	}
	tool := root["tools"].([]any)[0].(object)
	if to == OpenAI {
		return tool["function"].(object)["parameters"].(object), nil
	}
	return tool["input_schema"].(object), nil
}

func TestGeminiNativeParametersConvertToJSONSchema(t *testing.T) {
	var want object
	if err := json.Unmarshal([]byte(canonicalGeminiSchema), &want); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{OpenAI, Anthropic} {
		t.Run(to, func(t *testing.T) {
			got, err := convertedGeminiSchema(t, "parameters", nativeGeminiSchema, to)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				actual, _ := json.Marshal(got)
				t.Fatalf("native schema did not preserve canonical constraints:\n%s", actual)
			}
			actual, _ := json.Marshal(got)
			t.Logf("verified_target_schema=%s", actual)
		})
	}
}

func TestGeminiParametersJSONSchemaIsNotNativeDialect(t *testing.T) {
	schema := `{"type":"object","properties":{"x":{"type":["string","null"]},"nested":{"$ref":"#/$defs/n"}},"$defs":{"n":{"type":"object","additionalProperties":false}},"additionalProperties":false,"propertyOrdering":["x","nested"]}`
	var want object
	if err := json.Unmarshal([]byte(schema), &want); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{OpenAI, Anthropic} {
		got, err := convertedGeminiSchema(t, "parametersJsonSchema", schema, to)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s JSON Schema reinterpreted: got=%#v err=%v", to, got, err)
		}
	}
}

// Google's FunctionDeclaration uses a protobuf Value carrying a JSON object,
// not a string containing serialized JSON. Exercise both wire directions so
// a future encoder change cannot silently double-encode the tool schema.
func TestGeminiParametersJSONSchemaWireObjectRoundTrip(t *testing.T) {
	schema := object{
		"type": "object",
		"properties": object{
			"value":  object{"type": []any{"string", "null"}},
			"nested": object{"$ref": "#/$defs/payload"},
		},
		"$defs":                object{"payload": object{"type": "object", "additionalProperties": false}},
		"required":             []any{"value"},
		"additionalProperties": false,
	}
	for _, from := range []string{OpenAI, Anthropic} {
		t.Run(from, func(t *testing.T) {
			request := object{"model": "client-model", "messages": []any{object{"role": "user", "content": "go"}}, "max_tokens": 128}
			if from == OpenAI {
				request["tools"] = []any{object{"type": "function", "function": object{"name": "inspect", "parameters": schema}}}
			} else {
				request["tools"] = []any{object{"name": "inspect", "input_schema": schema}}
			}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := Request(body, from, Gemini, "gemini-test-model")
			if err != nil {
				t.Fatal(err)
			}
			var root object
			if err := json.Unmarshal(wire, &root); err != nil {
				t.Fatal(err)
			}
			declaration := root["tools"].([]any)[0].(object)["functionDeclarations"].([]any)[0].(object)
			if _, duplicate := declaration["parameters"]; duplicate {
				t.Fatal("Gemini parameters and parametersJsonSchema are mutually exclusive")
			}
			got, isObject := declaration["parametersJsonSchema"].(object)
			if !isObject || !reflect.DeepEqual(got, schema) {
				t.Fatalf("outgoing schema must be the unchanged JSON object, got %T: %#v", declaration["parametersJsonSchema"], got)
			}
			back, err := Request(wire, Gemini, from, "client-model")
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(back, &root); err != nil {
				t.Fatal(err)
			}
			tool := root["tools"].([]any)[0].(object)
			var returned any = tool["input_schema"]
			if from == OpenAI {
				returned = tool["function"].(object)["parameters"]
			}
			if !reflect.DeepEqual(returned, schema) {
				t.Fatalf("incoming JSON Schema constraints changed: %#v", returned)
			}
		})
	}
}

func TestGeminiParametersJSONSchemaRejectsStringAndDualFields(t *testing.T) {
	for _, declaration := range []string{
		`"parametersJsonSchema":"{\"type\":\"object\"}"`,
		`"parameters":{"type":"OBJECT"},"parametersJsonSchema":{"type":"object"}`,
	} {
		body := `{"contents":[{"role":"user","parts":[{"text":"go"}]}],"tools":[{"functionDeclarations":[{"name":"inspect",` + declaration + `}]}]}`
		for _, to := range []string{OpenAI, Anthropic} {
			if _, err := Request([]byte(body), Gemini, to, "m"); !errors.Is(err, ErrMalformed) {
				t.Fatalf("%s invalid wire declaration must be malformed, got %v", to, err)
			}
		}
	}
}

func TestGeminiNativeSchemaRefusesUnrepresentableOrMalformedFields(t *testing.T) {
	cases := []struct {
		name, schema, field string
		want                error
	}{
		{"ordering", `{"type":"OBJECT","propertyOrdering":["x"]}`, "propertyOrdering", ErrUnsupported},
		{"nested_ordering", `{"type":"OBJECT","properties":{"x":{"type":"OBJECT","propertyOrdering":[]}}}`, "propertyOrdering", ErrUnsupported},
		{"non_native_keyword", `{"type":"OBJECT","additionalProperties":false}`, "additionalProperties", ErrUnsupported},
		{"unknown_type", `{"type":"TYPE_UNSPECIFIED"}`, "type", ErrUnsupported},
		{"negative_bound", `{"type":"ARRAY","maxItems":"-1"}`, "maxItems", ErrMalformed},
		{"fractional_bound", `{"type":"ARRAY","maxItems":"1.5"}`, "maxItems", ErrMalformed},
		{"invalid_nullable", `{"type":"STRING","nullable":"true"}`, "nullable", ErrMalformed},
		{"empty_union", `{"type":"STRING","anyOf":[]}`, "anyOf", ErrMalformed},
	}
	for _, tc := range cases {
		for _, to := range []string{OpenAI, Anthropic} {
			t.Run(tc.name+"_"+to, func(t *testing.T) {
				_, err := convertedGeminiSchema(t, "parameters", tc.schema, to)
				if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("expected %v naming %s, got %v", tc.want, tc.field, err)
				}
			})
		}
	}
}
