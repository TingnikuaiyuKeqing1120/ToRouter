package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

var fourProtocols = []string{OpenAI, Anthropic, Gemini, Responses}

func fourFixtureRequest(t *testing.T, format string) []byte {
	t.Helper()
	r, err := decodeRequest([]byte(requestFixtures[OpenAI]), OpenAI)
	if err != nil {
		t.Fatal(err)
	}
	r.Stop = nil
	// Two rounds, parallel same-name calls, results deliberately reversed.
	r.Messages = append(r.Messages,
		irMessage{Role: RoleAssistant, Parts: []irPart{{Tool: &irToolCall{ID: "c2", Name: "weather", Arguments: `{"city":"A"}`}}, {Tool: &irToolCall{ID: "c3", Name: "weather", Arguments: `{"city":"B"}`}}}},
		irMessage{Role: RoleTool, Parts: []irPart{{ToolResult: &irToolResult{ID: "c3", Content: "B", Name: "weather"}}, {ToolResult: &irToolResult{ID: "c2", Content: "A", Name: "weather"}}}})
	b, _, err := encodeRequest(r, format, "m")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFourFormatsTwelveRequestDirections(t *testing.T) {
	for _, from := range fourProtocols {
		for _, to := range fourProtocols {
			if from == to {
				continue
			}
			t.Run(from+"_"+to, func(t *testing.T) {
				body := fourFixtureRequest(t, from)
				out, drops, err := RequestWithOptions(body, from, to, "model", Options{})
				if err != nil || len(drops) > 0 {
					t.Fatalf("conversion: %v %v", err, drops)
				}
				again, _, err := RequestWithOptions(body, from, to, "model", Options{})
				if err != nil || !bytes.Equal(out, again) {
					t.Fatal("conversion changed stable prefix")
				}
				r, err := decodeRequest(out, to)
				if err != nil {
					t.Fatal(err)
				}
				if r.System != "Be brief" || len(r.Tools) != 1 || r.MaxTokens == nil || *r.MaxTokens != 128 || r.ToolChoice.Mode != TCRequired {
					t.Fatalf("metadata lost: %+v", r)
				}
				calls, results := map[string]string{}, map[string]string{}
				for _, m := range r.Messages {
					for _, p := range m.Parts {
						if p.Tool != nil {
							calls[p.Tool.ID] = p.Tool.Arguments
						}
						if p.ToolResult != nil {
							results[p.ToolResult.ID] = p.ToolResult.Content
						}
					}
				}
				if len(calls) != 3 || len(results) != 3 || !strings.Contains(calls["c2"], "A") || !strings.Contains(calls["c3"], "B") || !strings.Contains(results["c2"], "A") || !strings.Contains(results["c3"], "B") {
					t.Fatalf("parallel/multiround pairing lost: %s", out)
				}
			})
		}
	}
}

func TestFourFormatsTwelveResponseAndStreamDirections(t *testing.T) {
	r, err := decodeResponse([]byte(responseFixtures[OpenAI]), OpenAI)
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range fourProtocols {
		for _, to := range fourProtocols {
			if from == to {
				continue
			}
			t.Run(from+"_"+to, func(t *testing.T) {
				body, err := encodeResponse(r, from, "m")
				if err != nil {
					t.Fatal(err)
				}
				out, err := Response(body, from, to, "target")
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := decodeResponse(out, to)
				if err != nil {
					t.Fatalf("target: %v %s", err, out)
				}
				if textParts(decoded.Parts) != "Checking" || len(calls(decoded.Parts)) != 1 || decoded.Usage.Input != 70 || decoded.Usage.Output != 20 || decoded.Usage.CacheRead != 30 {
					t.Fatalf("content/tool/usage lost: %s", out)
				}
				var source bytes.Buffer
				e, _ := newEmitter(from, &source, "m")
				e.id = "r1"
				if err := e.text("Checking"); err != nil {
					t.Fatal(err)
				}
				if err := e.tool(irToolCall{ID: "call1", Name: "weather", Arguments: `{"city":"Paris"}`}); err != nil {
					t.Fatal(err)
				}
				e.use(r.Usage, true)
				if err := e.end(FinishToolCalls, ""); err != nil {
					t.Fatal(err)
				}
				var target, back bytes.Buffer
				if err := Stream(bytes.NewReader(source.Bytes()), &target, from, to, "target"); err != nil {
					t.Fatal(err)
				}
				if err := Stream(bytes.NewReader(target.Bytes()), &back, to, from, "m"); err != nil {
					t.Fatalf("target stream: %v\n%s", err, target.String())
				}
				if !strings.Contains(target.String(), "Checking") || !strings.Contains(target.String(), "Paris") {
					t.Fatal("stream lost content")
				}
				var usage irUsage
				sr := newSSEReader(bytes.NewReader(target.Bytes()))
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
					u, ok := extractUsage(decodeRoot([]byte(data), p), to, p, true)
					if p.err != nil {
						t.Fatal(p.err)
					}
					if ok {
						mergeUsage(&usage, u)
					}
				}
				if usage.Input != 70 || usage.Output != 20 || usage.CacheRead != 30 {
					t.Fatalf("stream usage: %+v", usage)
				}
			})
		}
	}
}

func requestFromResponseParts(t *testing.T, parts []irPart, format string) []byte {
	t.Helper()
	max := int64(128)
	r := &irRequest{MaxTokens: &max, Messages: []irMessage{{Role: RoleUser, Parts: []irPart{{Text: "q"}}}, {Role: RoleAssistant, Parts: parts}}}
	var results []irPart
	for _, p := range parts {
		if p.Tool != nil {
			results = append(results, irPart{ToolResult: &irToolResult{ID: p.Tool.ID, Name: p.Tool.Name, Content: "ok"}})
		}
	}
	if len(results) > 0 {
		r.Messages = append(r.Messages, irMessage{Role: RoleTool, Parts: results})
	}
	b, _, err := encodeRequest(r, format, "m")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReasoningAttachmentsRoundTripAndAccessBoundary(t *testing.T) {
	for _, from := range fourProtocols {
		for _, to := range fourProtocols {
			if from == to {
				continue
			}
			t.Run(from+"_"+to, func(t *testing.T) {
				var reason *irReasoning
				switch from {
				case OpenAI:
					reason = &irReasoning{Format: from, Value: object{"reasoning_content": "private"}}
				case Anthropic:
					reason = &irReasoning{Format: from, Value: object{"type": "thinking", "thinking": "private", "signature": "native-sig"}}
				case Responses:
					reason = &irReasoning{Format: from, Value: object{"type": "reasoning", "id": "rs-original", "summary": []any{object{"type": "summary_text", "text": "summary"}}, "encrypted_content": "native-cipher"}}
				case Gemini:
					reason = &irReasoning{Format: from, Value: object{"text": "private", "thought": true, "thoughtSignature": "native-sig"}}
				}
				original := []irPart{{Thought: true, Reasoning: reason}, {Text: "visible"}, {Tool: &irToolCall{ID: "native-call", Name: "f", Arguments: `{"x":1}`}}}
				opts := Options{Scope: "client-a/model-a", Source: "upstream-a", SigningKey: []byte("synthetic-test-signing-key"), Bridge: NewReasoningBridge(8, time.Minute)}
				encoded, err := encodeResponse(&irResponse{ID: "r", Parts: original, Finish: FinishToolCalls}, from, "m")
				if err != nil {
					t.Fatal(err)
				}
				wire, drops, err := ResponseWithOptions(encoded, from, to, "m", opts)
				if err != nil || len(drops) > 0 {
					t.Fatalf("response: %v %v", err, drops)
				}
				decoded, err := decodeResponse(wire, to)
				if err != nil {
					t.Fatal(err)
				}
				next := requestFromResponseParts(t, decoded.Parts, to)
				opts.Source = "different-source-same-authorization"
				back, drops, err := RequestWithOptions(next, to, from, "m", opts)
				if err != nil || len(drops) > 0 {
					t.Fatalf("restore: %v %v\n%s", err, drops, next)
				}
				restored, err := decodeRequest(back, from)
				if err != nil {
					t.Fatal(err)
				}
				var got *irReasoning
				for _, m := range restored.Messages {
					for _, p := range m.Parts {
						if p.Reasoning != nil {
							got = p.Reasoning
						}
					}
				}
				if len(restored.Messages) < 2 || restored.Messages[1].Parts[0].Reasoning == nil {
					t.Fatalf("original reasoning position changed: %s", back)
				}
				wantJSON, _ := json.Marshal(reason)
				gotJSON, _ := json.Marshal(got)
				if !bytes.Equal(wantJSON, gotJSON) {
					t.Fatalf("attachment changed:\nwant %s\ngot %s", wantJSON, gotJSON)
				}
				// Plaintext is intrinsically portable; opaque transport is scoped.
				if opaqueReasoning(original[0]) {
					bad := opts
					bad.Scope = "client-b/model-a"
					if _, _, err := RequestWithOptions(next, to, from, "m", bad); err == nil {
						t.Fatal("cross-scope attachment accepted")
					}
				}
			})
		}
	}
}

func TestReasoningBridgeExpirationAndRestart(t *testing.T) {
	opts := Options{Scope: "scope", SigningKey: []byte("synthetic-test-key"), Bridge: NewReasoningBridge(1, time.Nanosecond)}
	token, err := sealAttachment([]irPart{{Thought: true, Reasoning: &irReasoning{Format: Anthropic, Value: object{"type": "redacted_thinking", "data": "opaque"}}}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	id, err := opts.Bridge.put(token, opts.Scope)
	if err != nil {
		t.Fatal(err)
	}
	entry := opts.Bridge.entries[id]
	entry.expires = time.Now().Add(-time.Second)
	opts.Bridge.entries[id] = entry
	if _, err := opts.Bridge.get(id, opts); !errors.Is(err, ErrReasoningState) {
		t.Fatalf("expired entry: %v", err)
	}
	other := opts
	other.Bridge = NewReasoningBridge(2, time.Minute)
	if _, err := other.Bridge.get(id, other); !errors.Is(err, ErrReasoningState) {
		t.Fatalf("restart entry: %v", err)
	}
	// Inline transport survives independent bridge instances with injected key.
	parts, err := openAttachment(token, other)
	if err != nil || len(parts) != 1 {
		t.Fatalf("inline restart: %v", err)
	}
	other.SigningKey = []byte("different-key")
	if _, err := openAttachment(token, other); !errors.Is(err, ErrReasoningState) {
		t.Fatalf("changed signing key: %v", err)
	}
}

// Assemble the client's actual SSE wire messages, rather than calling the
// response adapter to stand in for a streaming client that returns history.
func assembleClientStream(t *testing.T, wire []byte, format string) []irPart {
	t.Helper()
	sr := newSSEReader(bytes.NewReader(wire))
	var parts []irPart
	blocks := map[int]object{}
	args := map[int]string{}
	msg := object{"role": "assistant", "content": ""}
	var calls []any
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
		switch format {
		case Responses:
			if m["type"] == "response.completed" {
				response, _ := json.Marshal(m["response"])
				decoded, err := decodeResponse(response, Responses)
				if err != nil {
					t.Fatal(err)
				}
				parts = decoded.Parts
			}
		case Gemini:
			if a, ok := m["candidates"].([]any); ok {
				for _, v := range a {
					candidate := v.(object)
					if c, ok := candidate["content"].(object); ok {
						parts = append(parts, geminiParts(c["parts"], p, 0, false, true)...)
						if p.err != nil {
							t.Fatal(p.err)
						}
					}
				}
			}
		case Anthropic:
			i := 0
			if v, ok := m["index"]; ok {
				i = int(p.integer(v))
			}
			switch m["type"] {
			case "content_block_start":
				blocks[i] = m["content_block"].(object)
			case "content_block_delta":
				d := m["delta"].(object)
				if d["type"] == "text_delta" {
					previous, _ := blocks[i]["text"].(string)
					blocks[i]["text"] = previous + p.str(d["text"])
				} else if d["type"] == "input_json_delta" {
					args[i] += p.str(d["partial_json"])
				}
			case "content_block_stop":
				if a, ok := args[i]; ok {
					var input object
					if strictJSON([]byte(a), &input) != nil {
						t.Fatal("bad client tool arguments")
					}
					blocks[i]["input"] = input
				}
				parts = append(parts, anthropicParts([]any{blocks[i]}, p, false, true)...)
				if p.err != nil {
					t.Fatal(p.err)
				}
			}
		case OpenAI:
			if a, ok := m["choices"].([]any); ok {
				for _, v := range a {
					d := v.(object)["delta"].(object)
					if s, ok := d["content"].(string); ok {
						msg["content"] = msg["content"].(string) + s
					}
					if s, ok := d["reasoning_content"].(string); ok {
						previous, _ := msg["reasoning_content"].(string)
						msg["reasoning_content"] = previous + s
					}
					if a, ok := d["tool_calls"].([]any); ok {
						for _, raw := range a {
							call := raw.(object)
							delete(call, "index")
							calls = append(calls, call)
						}
					}
				}
			}
		}
	}
	if format == OpenAI {
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		body, _ := json.Marshal(object{"choices": []any{object{"index": 0, "message": msg, "finish_reason": "tool_calls"}}})
		decoded, err := decodeResponse(body, OpenAI)
		if err != nil {
			t.Fatal(err)
		}
		parts = decoded.Parts
	}
	return parts
}

func TestReasoningStreamClientHistoryRoundTripTwelveDirections(t *testing.T) {
	for _, from := range fourProtocols {
		for _, to := range fourProtocols {
			if from == to {
				continue
			}
			t.Run(from+"_"+to, func(t *testing.T) {
				var reason *irReasoning
				switch from {
				case OpenAI:
					reason = &irReasoning{Format: from, Value: object{"reasoning_content": "inner"}}
				case Anthropic:
					reason = &irReasoning{Format: from, Value: object{"type": "thinking", "thinking": "inner", "signature": "signature"}}
				case Gemini:
					reason = &irReasoning{Format: from, Value: object{"text": "inner", "thought": true, "thoughtSignature": "signature"}}
				case Responses:
					reason = &irReasoning{Format: from, Value: object{"type": "reasoning", "id": "reason-original", "summary": []any{object{"type": "summary_text", "text": "summary"}}, "encrypted_content": "encrypted"}}
				}
				var source bytes.Buffer
				e, _ := newEmitter(from, &source, "m")
				e.id = "response-original"
				if err := e.nativeReasoning(irPart{Thought: true, Reasoning: reason}); err != nil {
					t.Fatal(err)
				}
				e.text("visible")
				e.toolRaw(irToolCall{ID: "original-call", Name: "f", Arguments: `{"a":1}`}, "")
				e.end(FinishToolCalls, "")
				opts := Options{Scope: "scope", SigningKey: []byte("synthetic-key"), Bridge: NewReasoningBridge(8, time.Minute)}
				var wire bytes.Buffer
				drops, err := StreamWithOptions(bytes.NewReader(source.Bytes()), &wire, from, to, "m", opts)
				if err != nil || len(drops) > 0 {
					t.Fatalf("stream: %v %v", err, drops)
				}
				parts := assembleClientStream(t, wire.Bytes(), to)
				history := requestFromResponseParts(t, parts, to)
				back, drops, err := RequestWithOptions(history, to, from, "m", opts)
				if err != nil || len(drops) > 0 {
					t.Fatalf("restore client history: %v %v\n%s", err, drops, history)
				}
				restored, err := decodeRequest(back, from)
				if err != nil {
					t.Fatal(err)
				}
				assistant := restored.Messages[1]
				if assistant.Parts[0].Reasoning == nil || textParts(assistant.Parts) != "visible" || len(calls(assistant.Parts)) != 1 || calls(assistant.Parts)[0].ID != "original-call" {
					t.Fatalf("reasoning position/content/tool lost: %s", back)
				}
				want, _ := json.Marshal(reason)
				got, _ := json.Marshal(assistant.Parts[0].Reasoning)
				if !bytes.Equal(want, got) {
					t.Fatalf("streamed attachment changed: want %s got %s", want, got)
				}
			})
		}
	}
}

func TestNativeAttachmentRestorationPreservesNativeExtensions(t *testing.T) {
	opts := Options{Scope: "synthetic-scope", SigningKey: []byte("synthetic-key")}
	part := irPart{Thought: true, Reasoning: &irReasoning{Format: Anthropic, Value: object{"type": "thinking", "thinking": "original", "signature": "native-sig"}}}
	token, err := sealAttachment([]irPart{part}, opts)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(object{"model": "native", "thinking": object{"type": "enabled", "budget_tokens": 100}, "metadata": object{"user_id": "synthetic"}, "messages": []any{object{"role": "assistant", "content": []any{object{"type": "redacted_thinking", "data": token}}}, object{"role": "user", "content": []any{object{"type": "image", "source": object{"type": "base64", "media_type": "image/png", "data": "synthetic"}}}}}})
	out, drops, err := PrepareRequestWithOptions(body, AnthropicMessages, opts)
	if err != nil || len(drops) > 0 {
		t.Fatalf("native restore: %v %v", err, drops)
	}
	var native object
	json.Unmarshal(out, &native)
	if !reflect.DeepEqual(native["thinking"], object{"type": "enabled", "budget_tokens": float64(100)}) || !bytes.Contains(out, []byte(`"type":"image"`)) || !bytes.Contains(out, []byte(`"signature":"native-sig"`)) || bytes.Contains(out, []byte(attachmentPrefix)) {
		t.Fatalf("native extensions or original state changed: %s", out)
	}
	bad := opts
	bad.Scope = "other"
	if _, _, err := PrepareRequestWithOptions(body, Anthropic, bad); !errors.Is(err, ErrReasoningState) {
		t.Fatalf("native scope bypass: %v", err)
	}
	plain := []byte(" {\"messages\":[{\"role\":\"user\",\"content\":\"literal " + attachmentPrefix + "text\"}],\"unknown_native_option\":true} ")
	unchanged, _, err := PrepareRequestWithOptions(plain, Anthropic, opts)
	if err != nil || !bytes.Equal(plain, unchanged) {
		t.Fatalf("literal marker modified ordinary native body: %v %s", err, unchanged)
	}
}

func TestGeminiSignedToolRestoresWithoutDuplicateOrBypass(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"before"},{"functionCall":{"id":"native-id","name":"f","args":{"x":1}},"thoughtSignature":"original-signature"}]},"finishReason":"STOP"}]}`)
	for _, to := range []string{OpenAI, Anthropic, Responses} {
		t.Run(to, func(t *testing.T) {
			opts := Options{Scope: "scope", SigningKey: []byte("key"), Bridge: NewReasoningBridge(8, time.Minute)}
			wire, _, err := ResponseWithOptions(body, Gemini, to, "m", opts)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeResponse(wire, to)
			if err != nil {
				t.Fatal(err)
			}
			request := requestFromResponseParts(t, decoded.Parts, to)
			back, drops, err := RequestWithOptions(request, to, Gemini, "gemini-3-test", opts)
			if err != nil || len(drops) > 0 {
				t.Fatalf("signed tool restore: %v %v %s", err, drops, back)
			}
			restored, err := decodeRequest(back, Gemini)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, msg := range restored.Messages {
				for _, p := range msg.Parts {
					if p.Tool != nil {
						count++
						if p.Tool.ID != "native-id" || p.Signature != "original-signature" {
							t.Fatalf("signature/id changed: %+v", p)
						}
					}
				}
			}
			if count != 1 || bytes.Contains(back, []byte(thoughtSignatureBypass)) {
				t.Fatalf("duplicate or fabricated signature: %s", back)
			}
		})
	}
}

func TestReasoningDefaultRejectExplicitDropAndCacheHints(t *testing.T) {
	body := []byte(`{"role":"assistant","content":[{"type":"thinking","thinking":"inner","signature":"sig"},{"type":"tool_use","id":"c1","name":"f","input":{}}],"stop_reason":"tool_use"}`)
	if _, _, err := ResponseWithOptions(body, Anthropic, OpenAI, "m", Options{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("default must reject unavailable opaque state: %v", err)
	}
	out, drops, err := ResponseWithOptions(body, Anthropic, OpenAI, "m", Options{AllowReasoningDrop: true})
	if err != nil || !reflect.DeepEqual(drops, []string{"reasoning_state_dropped"}) {
		t.Fatalf("explicit drop: %v %v", err, drops)
	}
	if !bytes.Contains(out, []byte("tool_calls")) || bytes.Contains(out, []byte("inner")) {
		t.Fatalf("drop changed tool or leaked thinking: %s", out)
	}
	request := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"f","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}}]}`)
	out, drops, err = RequestWithInfo(request, Anthropic, OpenAI, "m")
	if err != nil || !reflect.DeepEqual(drops, []string{"cache_control_removed"}) || !bytes.Contains(out, []byte(`"cache_control":{"type":"string"}`)) {
		t.Fatalf("cache hint/schema handling: %v %v %s", err, drops, out)
	}
	for _, b := range []string{`{"input":"hi","store":true}`, `{"input":"hi","previous_response_id":"resp_old"}`, `{"input":"hi","conversation":"conv"}`, `{"input":[{"type":"item_reference","id":"item"}]}`, `{"input":[{"type":"compaction","encrypted_content":"x"}]}`, `{"input":"hi","tools":[{"type":"web_search"}]}`, `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`} {
		if _, _, err := RequestWithOptions([]byte(b), Responses, OpenAI, "m", Options{AllowReasoningDrop: true}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unique history/unknown content accepted: %s %v", b, err)
		}
	}
	if _, _, err := RequestWithOptions([]byte(`{"cachedContent":"cachedContents/1","contents":[{"parts":[{"text":"x"}]}]}`), Gemini, OpenAI, "m", Options{AllowReasoningDrop: true}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("cachedContent accepted: %v", err)
	}
}

func TestResponsesStrictTerminalAndIncremental(t *testing.T) {
	var complete bytes.Buffer
	e, _ := newEmitter(Responses, &complete, "m")
	e.id = "r"
	e.text("first")
	prefix := complete.String()
	e.end(FinishStop, "")
	for _, to := range []string{OpenAI, Anthropic, Gemini} {
		t.Run(to, func(t *testing.T) {
			r, w := io.Pipe()
			signal := make(chan struct{}, 1)
			out := &signalWriter{signal: signal}
			done := make(chan error, 1)
			go func() { done <- Stream(r, out, Responses, to, "m") }()
			if _, err := io.WriteString(w, prefix); err != nil {
				t.Fatal(err)
			}
			select {
			case <-signal:
			case <-time.After(time.Second):
				w.Close()
				t.Fatal("text waited for full upstream terminal")
			}
			w.CloseWithError(errors.New("synthetic cancellation"))
			if err := <-done; err == nil {
				t.Fatal("cancellation became successful EOF")
			}
			if strings.Contains(out.String(), "[DONE]") || strings.Contains(out.String(), `"type":"message_stop"`) {
				t.Fatalf("false terminal: %s", out.String())
			}
			for _, terminal := range []string{"response.failed", "response.incomplete", "error"} {
				body := prefix + sse(`{"type":"`+terminal+`"}`)
				var output bytes.Buffer
				if err := Stream(strings.NewReader(body), &output, Responses, to, "m"); err == nil {
					t.Fatalf("accepted %s", terminal)
				}
			}
		})
	}
}

func TestResponsesParallelFragmentedToolsAndTerminalAgreement(t *testing.T) {
	frame := func(v object) string { b, _ := json.Marshal(v); return sse(string(b)) }
	created := frame(object{"type": "response.created", "response": object{"id": "r", "status": "in_progress", "output": []any{}}})
	call := func(i int, args string) object {
		return object{"type": "function_call", "id": []string{"i0", "i1"}[i], "call_id": []string{"c0", "c1"}[i], "name": "f", "arguments": args, "status": "completed"}
	}
	var source strings.Builder
	source.WriteString(created)
	for i := 0; i < 2; i++ {
		source.WriteString(frame(object{"type": "response.output_item.added", "output_index": i, "item": call(i, "")}))
	}
	for _, d := range []struct {
		i int
		s string
	}{{0, `{"x":`}, {1, `{"x":`}, {1, `2}`}, {0, `1}`}} {
		source.WriteString(frame(object{"type": "response.function_call_arguments.delta", "output_index": d.i, "item_id": []string{"i0", "i1"}[d.i], "delta": d.s}))
	}
	// Parallel items may finish in another order. Their output indices remain
	// authoritative; neither completion order nor map iteration may reorder IDs.
	for _, i := range []int{1, 0} {
		args := []string{`{"x":1}`, `{"x":2}`}[i]
		source.WriteString(frame(object{"type": "response.output_item.done", "output_index": i, "item": call(i, args)}))
	}
	terminal := frame(object{"type": "response.completed", "response": object{"id": "r", "status": "completed", "output": []any{call(0, `{"x":1}`), call(1, `{"x":2}`)}, "usage": object{"input_tokens": 12, "output_tokens": 9, "input_tokens_details": object{"cached_tokens": 4}, "output_tokens_details": object{"reasoning_tokens": 3}}}})
	for _, to := range []string{OpenAI, Anthropic, Gemini} {
		t.Run(to, func(t *testing.T) {
			var out bytes.Buffer
			if err := Stream(strings.NewReader(source.String()+terminal), &out, Responses, to, "m"); err != nil {
				t.Fatal(err)
			}
			var chat bytes.Buffer
			if to == OpenAI {
				chat = out
			} else if err := Stream(bytes.NewReader(out.Bytes()), &chat, to, OpenAI, "m"); err != nil {
				t.Fatal(err)
			}
			decoded := decodeOpenAIStream(t, chat.String())
			if len(decoded.calls) != 2 || decoded.calls[0].ID != "c0" || decoded.calls[1].ID != "c1" || decoded.calls[0].Arguments != `{"x":1}` || decoded.calls[1].Arguments != `{"x":2}` || decoded.usage.Input != 8 || decoded.usage.Output != 9 || decoded.usage.CacheRead != 4 {
				t.Fatalf("fragment/parallel/usage mismatch: %+v", decoded)
			}
			bad := strings.Replace(source.String(), `"arguments":"{\"x\":2}"`, `"arguments":"{\"x\":3}"`, 1)
			var rejected bytes.Buffer
			if err := Stream(strings.NewReader(bad+terminal), &rejected, Responses, to, "m"); !errors.Is(err, ErrMalformed) {
				t.Fatalf("mismatched tool terminal accepted: %v", err)
			}
			if strings.Contains(rejected.String(), "[DONE]") || strings.Contains(rejected.String(), `"type":"message_stop"`) {
				t.Fatal("mismatched terminal became success")
			}
		})
	}
}

type signalWriter struct {
	bytes.Buffer
	signal chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, []byte("first")) {
		select {
		case w.signal <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (w *signalWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
