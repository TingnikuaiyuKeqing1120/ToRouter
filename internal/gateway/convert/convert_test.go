package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

var protocols = []string{OpenAI, Anthropic, Gemini}
var requestFixtures = map[string]string{
	OpenAI:    `{"model":"old","messages":[{"role":"system","content":"Be brief"},{"role":"user","content":"weather?"},{"role":"assistant","content":"Checking","tool_calls":[{"id":"call1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call1","content":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","description":"forecast","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"required","max_tokens":128,"temperature":0.5,"top_p":0.9,"stop":["END"],"stream":true}`,
	Anthropic: `{"model":"old","system":"Be brief","messages":[{"role":"user","content":"weather?"},{"role":"assistant","content":[{"type":"text","text":"Checking"},{"type":"tool_use","id":"call1","name":"weather","input":{"city":"Paris"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"sunny"}]}],"tools":[{"name":"weather","description":"forecast","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"any"},"max_tokens":128,"temperature":0.5,"top_p":0.9,"stop_sequences":["END"],"stream":true}`,
	Gemini:    `{"systemInstruction":{"parts":[{"text":"Be brief"}]},"contents":[{"role":"user","parts":[{"text":"weather?"}]},{"role":"model","parts":[{"text":"Checking"},{"functionCall":{"name":"weather","args":{"city":"Paris"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"output":"sunny"}}}]}],"tools":[{"functionDeclarations":[{"name":"weather","description":"forecast","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}},"required":["city"]}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"generationConfig":{"maxOutputTokens":128,"temperature":0.5,"topP":0.9,"stopSequences":["END"]}}`,
}
var responseFixtures = map[string]string{
	OpenAI:    `{"id":"r1","object":"chat.completion","model":"old","choices":[{"index":0,"message":{"role":"assistant","content":"Checking","tool_calls":[{"id":"call1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`,
	Anthropic: `{"id":"r1","type":"message","role":"assistant","model":"old","content":[{"type":"text","text":"Checking"},{"type":"tool_use","id":"call1","name":"weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":70,"output_tokens":20,"cache_read_input_tokens":30}}`,
	Gemini:    `{"responseId":"r1","modelVersion":"old","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Checking"},{"functionCall":{"name":"weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":30}}`,
}

func TestRequestAllPairs(t *testing.T) {
	for _, from := range protocols {
		for _, to := range protocols {
			if from == to {
				continue
			}
			t.Run(from+"_"+to, func(t *testing.T) {
				b, err := Request([]byte(requestFixtures[from]), from, to, "upstream")
				if err != nil {
					t.Fatal(err)
				}
				r, err := decodeRequest(b, to)
				if err != nil {
					t.Fatalf("decode target: %v\n%s", err, b)
				}
				if r.System != "Be brief" || len(r.Tools) != 1 || r.Tools[0].Name != "weather" || r.ToolChoice.Mode != TCRequired || r.MaxTokens == nil || *r.MaxTokens != 128 || r.Temperature == nil || *r.Temperature != .5 || r.TopP == nil || *r.TopP != .9 || len(r.Stop) != 1 || r.Stop[0] != "END" {
					t.Fatalf("lost request fields: %+v", r)
				}
				var call *irToolCall
				var result *irToolResult
				for _, m := range r.Messages {
					for _, p := range m.Parts {
						if p.Tool != nil {
							call = p.Tool
						}
						if p.ToolResult != nil {
							result = p.ToolResult
						}
					}
				}
				wantContent := "sunny"
				if from == Gemini || to == Gemini {
					// Gemini results are objects. Preserve its output key and the
					// wrapper introduced for textual Chat/Messages results.
					wantContent = `{"output":"sunny"}`
				}
				if call == nil || result == nil || call.ID != result.ID || call.Name != "weather" || result.Content != wantContent || result.Structured != (to == Gemini) || call.Arguments != `{"city":"Paris"}` {
					t.Fatalf("tool roundtrip: %+v %+v", call, result)
				}
				if to != Gemini && !bytes.Contains(b, []byte(`"model":"upstream"`)) {
					t.Fatalf("missing model: %s", b)
				}
			})
		}
	}
}
func TestResponseAllPairs(t *testing.T) {
	for _, from := range protocols {
		for _, to := range protocols {
			if from == to {
				continue
			}
			t.Run(from+"_"+to, func(t *testing.T) {
				b, err := Response([]byte(responseFixtures[from]), from, to, "requested")
				if err != nil {
					t.Fatal(err)
				}
				r, err := decodeResponse(b, to)
				if err != nil {
					t.Fatalf("decode target: %v\n%s", err, b)
				}
				ts := calls(r.Parts)
				if textParts(r.Parts) != "Checking" || len(ts) != 1 || ts[0].ID == "" || ts[0].Name != "weather" || ts[0].Arguments != `{"city":"Paris"}` || r.Finish != FinishToolCalls {
					t.Fatalf("lost response: %+v %s", r, b)
				}
				in, out, cr, cw, ok := ParseUsage(b, to)
				if !ok || in != 70 || out != 20 || cr != 30 || cw != 0 {
					t.Fatalf("usage %d %d %d %d %v", in, out, cr, cw, ok)
				}
				if !bytes.Contains(b, []byte(`requested`)) {
					t.Fatal("missing requested model")
				}
			})
		}
	}
}
func sse(v string) string { return "data: " + v + "\n\n" }
func sourceStream(protocol string) string {
	switch protocol {
	case OpenAI:
		return sse(`{"id":"r1","choices":[{"index":0,"delta":{"role":"assistant","content":"Checking"},"finish_reason":null}]}`) + sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`) + sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`) + sse(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`) + sse(`[DONE]`)
	case Anthropic:
		return sse(`{"type":"message_start","message":{"id":"r1","type":"message","role":"assistant","model":"old","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":70,"output_tokens":0,"cache_read_input_tokens":30}}}`) + sse(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) + sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking"}}`) + sse(`{"type":"content_block_stop","index":0}`) + sse(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call1","name":"weather","input":{}}}`) + sse(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`) + sse(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`) + sse(`{"type":"content_block_stop","index":1}`) + sse(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`) + sse(`{"type":"message_stop"}`)
	case Gemini:
		return sse(`{"responseId":"r1","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Checking"}]}}]}`) + sse(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":30}}`)
	}
	return ""
}

type flushBuffer struct {
	bytes.Buffer
	flushes int
}

func (b *flushBuffer) Flush() { b.flushes++ }
func TestStreamAllPairs(t *testing.T) {
	for _, from := range protocols {
		for _, to := range protocols {
			if from == to {
				continue
			}
			t.Run(from+"_"+to, func(t *testing.T) {
				var w flushBuffer
				if err := Stream(strings.NewReader(sourceStream(from)), &w, from, to, "requested"); err != nil {
					t.Fatal(err)
				}
				if w.flushes < 2 || !strings.Contains(w.String(), "Checking") || !strings.Contains(w.String(), "weather") || !strings.Contains(w.String(), "requested") {
					t.Fatalf("invalid output %s", w.String())
				}
				// Roundtrip the generated stream through the source parser. This checks
				// lifecycle, tool argument validity, finish markers and JSON framing.
				var back bytes.Buffer
				if err := Stream(bytes.NewReader(w.Bytes()), &back, to, from, "requested"); err != nil {
					t.Fatalf("target rejected: %v\n%s", err, w.String())
				}
				sr := newSSEReader(bytes.NewReader(w.Bytes()))
				var usage irUsage
				for {
					_, data, err := sr.next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if data == "[DONE]" {
						continue
					}
					p := &parser{}
					m := decodeRoot([]byte(data), p)
					u, ok := extractUsage(m, to, p, false)
					if p.err != nil {
						t.Fatal(p.err)
					}
					if ok {
						mergeUsage(&usage, u)
					}
				}
				if usage.Input != 70 || usage.Output != 20 || usage.CacheRead != 30 {
					t.Fatalf("usage %+v", usage)
				}
			})
		}
	}
}
func TestUsage(t *testing.T) {
	cases := []struct {
		p, b            string
		in, out, cr, cw int64
		ok              bool
	}{
		{OpenAI, `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`, 70, 20, 30, 0, true},
		{Anthropic, `{"message":{"usage":{"input_tokens":70,"output_tokens":2,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}}`, 70, 2, 30, 5, true},
		{Anthropic, `{"type":"message_delta","usage":{"output_tokens":12}}`, 0, 12, 0, 0, true},
		{Gemini, `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":30,"thoughtsTokenCount":5}}`, 70, 25, 30, 0, true},
		{OpenAI, `{"usage":{"prompt_tokens":2,"prompt_tokens_details":{"cached_tokens":3}}}`, 0, 0, 0, 0, false},
		{OpenAI, `{"usage":{"prompt_tokens":-1}}`, 0, 0, 0, 0, false},
		{OpenAI, `{"usage":{"prompt_tokens":1.5}}`, 0, 0, 0, 0, false},
		{OpenAI, `{"usage":null}`, 0, 0, 0, 0, false},
		{OpenAI, `{"choices":[]}`, 0, 0, 0, 0, false},
		{"unknown", `{}`, 0, 0, 0, 0, false},
	}
	for _, c := range cases {
		in, out, cr, cw, ok := ParseUsage([]byte(c.b), c.p)
		if in != c.in || out != c.out || cr != c.cr || cw != c.cw || ok != c.ok {
			t.Errorf("%s %s got %d %d %d %d %v", c.p, c.b, in, out, cr, cw, ok)
		}
	}
}
func TestUnsupportedAndMalformed(t *testing.T) {
	cases := []struct{ p, b string }{
		{OpenAI, `{"messages":[],"response_format":{"type":"json_object"}}`},
		{OpenAI, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`},
		{OpenAI, `{"messages":[{"role":"assistant","content":"x","reasoning_content":{"not":"a string"}}]}`},
		{Anthropic, `{"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`},
		{Anthropic, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"x"}]}]}`},
		{Gemini, `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"x","args":{}},"thoughtSignature":"x"}]}]}`},
		{Gemini, `{"contents":[{"parts":[{"inlineData":{}}]}]}`},
		{OpenAI, `{"messages":[{"role":"tool","tool_call_id":"missing","content":"x"}]}`},
		{OpenAI, `{"messages":[{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"null"}}]}]}`},
	}
	for i, c := range cases {
		to := OpenAI
		if c.p == OpenAI {
			to = Gemini
		}
		if _, err := Request([]byte(c.b), c.p, to, "m"); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	for _, b := range []string{"null", "[]", "{", "{} {}"} {
		if _, err := Response([]byte(b), OpenAI, Gemini, "m"); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
	if _, err := Request(nil, OpenAI, OpenAI, ""); !errors.Is(err, ErrSameProtocol) {
		t.Fatal(err)
	}
	if _, err := Request(nil, "bad", OpenAI, ""); !errors.Is(err, ErrUnknownProtocol) {
		t.Fatal(err)
	}
}
func TestTruncatedStreams(t *testing.T) {
	for _, from := range protocols {
		src := sourceStream(from)
		events := strings.Split(strings.TrimSuffix(src, "\n\n"), "\n\n")
		for i := 0; i < len(events); i++ {
			input := strings.Join(events[:i], "\n\n")
			if input != "" {
				input += "\n\n"
			}
			to := OpenAI
			if from == OpenAI {
				to = Anthropic
			}
			var w bytes.Buffer
			err := Stream(strings.NewReader(input), &w, from, to, "m")
			if err == nil {
				t.Fatalf("%s prefix %d accepted", from, i)
			}
			if strings.Contains(w.String(), "[DONE]") || strings.Contains(w.String(), `"type":"message_stop"`) {
				t.Fatalf("false successful end: %s", w.String())
			}
		}
	}
	var w bytes.Buffer
	if err := Stream(strings.NewReader("data: {broken}\n\n"), &w, OpenAI, Anthropic, "m"); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
	if err := Stream(strings.NewReader("data: "+strings.Repeat("x", maxLineLen)+"\n\n"), &w, OpenAI, Anthropic, "m"); !errors.Is(err, ErrLineLimit) {
		t.Fatal(err)
	}
	if err := Stream(strings.NewReader("data: [DONE]"), &w, OpenAI, Anthropic, "m"); !errors.Is(err, ErrTruncated) {
		t.Fatal(err)
	}
}

type badWriter struct{}

func (badWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("writer failed") }
func TestWriterError(t *testing.T) {
	for _, from := range protocols {
		to := OpenAI
		if from == OpenAI {
			to = Anthropic
		}
		if err := Stream(strings.NewReader(sourceStream(from)), badWriter{}, from, to, "m"); err == nil || !strings.Contains(err.Error(), "writer failed") {
			t.Fatal(err)
		}
	}
}
func TestGeminiStableIDs(t *testing.T) {
	a, e := Response([]byte(responseFixtures[Gemini]), Gemini, OpenAI, "m")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := Response([]byte(responseFixtures[Gemini]), Gemini, OpenAI, "m")
	if !bytes.Equal(a, b) {
		t.Fatal("unstable IDs")
	}
}
func TestTextOnlyPairs(t *testing.T) {
	for _, from := range protocols {
		r, _ := decodeResponse([]byte(responseFixtures[from]), from)
		r.Parts = []irPart{{Text: "hello"}}
		r.Finish = FinishLength
		src, err := encodeResponse(r, from, "old")
		if err != nil {
			t.Fatal(err)
		}
		for _, to := range protocols {
			if from == to {
				continue
			}
			b, err := Response(src, from, to, "m")
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeResponse(b, to)
			if err != nil || textParts(got.Parts) != "hello" || got.Finish != FinishLength {
				t.Fatalf("%s %s %s %v", from, to, b, err)
			}
		}
	}
}
func FuzzRequestNoPanic(f *testing.F) {
	for _, b := range requestFixtures {
		f.Add([]byte(b))
	}
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, p := range protocols {
			to := OpenAI
			if p == OpenAI {
				to = Gemini
			}
			Request(b, p, to, "m")
		}
	})
}
func FuzzResponseAndStreamNoPanic(f *testing.F) {
	for _, b := range responseFixtures {
		f.Add([]byte(b))
	}
	f.Add([]byte(`{"candidates":[{"content":{"parts":[{"text":"x","functionCall":null}]}}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, p := range protocols {
			to := OpenAI
			if p == OpenAI {
				to = Gemini
			}
			Response(b, p, to, "m")
			Stream(strings.NewReader(sse(string(b))), io.Discard, p, to, "m")
		}
	})
}
func TestToolChoiceModes(t *testing.T) {
	for _, from := range protocols {
		for _, mode := range []string{TCNone, TCAuto, TCRequired, TCTool} {
			r, err := decodeRequest([]byte(requestFixtures[from]), from)
			if err != nil {
				t.Fatal(err)
			}
			r.ToolChoice = &irToolChoice{Mode: mode}
			if mode == TCTool {
				r.ToolChoice.Name = "weather"
			}
			src, _, err := encodeRequest(r, from, "m")
			if err != nil {
				t.Fatal(err)
			}
			for _, to := range protocols {
				if to == from {
					continue
				}
				out, err := Request(src, from, to, "m")
				if err != nil {
					t.Fatal(err)
				}
				got, err := decodeRequest(out, to)
				if err != nil || got.ToolChoice.Mode != mode || got.ToolChoice.Name != r.ToolChoice.Name {
					t.Fatalf("%s %s %s %s %v", from, to, mode, out, err)
				}
			}
		}
	}
}
func TestTextOnlyRequestsAndStreams(t *testing.T) {
	for _, from := range protocols {
		r, err := decodeRequest([]byte(requestFixtures[from]), from)
		if err != nil {
			t.Fatal(err)
		}
		r.Messages = []irMessage{{Role: RoleUser, Parts: []irPart{{Text: "hello"}}}}
		r.Tools = nil
		r.ToolChoice = nil
		b, _, err := encodeRequest(r, from, "m")
		if err != nil {
			t.Fatal(err)
		}
		var src bytes.Buffer
		e, _ := newEmitter(from, &src, "m")
		if err := e.text("hello"); err != nil {
			t.Fatal(err)
		}
		if err := e.end(FinishStop, ""); err != nil {
			t.Fatal(err)
		}
		for _, to := range protocols {
			if to == from {
				continue
			}
			out, err := Request(b, from, to, "m")
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeRequest(out, to)
			if err != nil || textParts(got.Messages[0].Parts) != "hello" {
				t.Fatalf("%s %s %v", from, to, err)
			}
			var stream bytes.Buffer
			if err := Stream(bytes.NewReader(src.Bytes()), &stream, from, to, "m"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stream.String(), "hello") {
				t.Fatal("text lost")
			}
		}
	}
}
func TestBadStreamLifecycleAndToolJSON(t *testing.T) {
	cases := []struct{ from, input string }{
		{OpenAI, strings.Replace(sourceStream(OpenAI), `\"Paris\"}`, `\"Paris\"`, 1)},
		{Anthropic, sse(`{"type":"content_block_stop","index":0}`)},
		{Anthropic, sse(`{"type":"message_stop"}`)},
		{Gemini, sse(`{"candidates":[{"content":{"parts":[{"functionCall":null}]},"finishReason":"STOP"}]}`)},
		{Gemini, sse(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":"{\\\"x\\\":"}}]},"finishReason":"STOP"}]}`)},
	}
	for i, c := range cases {
		to := OpenAI
		if c.from == OpenAI {
			to = Gemini
		}
		if err := Stream(strings.NewReader(c.input), io.Discard, c.from, to, "m"); err == nil {
			t.Fatalf("accepted invalid stream %d", i)
		}
	}
	// Both no-space data fields and CRLF framing are legal SSE.
	input := strings.ReplaceAll(strings.ReplaceAll(sourceStream(OpenAI), "data: ", "data:"), "\n", "\r\n")
	if err := Stream(strings.NewReader(input), io.Discard, OpenAI, Gemini, "m"); err != nil {
		t.Fatal(err)
	}
}
func TestJSONArguments(t *testing.T) {
	for _, v := range []string{`null`, `[]`, `1`, `"x"`, `{} {}`} {
		if jsonObject(v) == nil {
			t.Fatal(v)
		}
	}
	if jsonObject(`{"a":9007199254740993}`) != nil {
		t.Fatal("valid object rejected")
	}
	var m any
	if err := json.Unmarshal([]byte(responseFixtures[OpenAI]), &m); err != nil {
		t.Fatal(err)
	}
}
