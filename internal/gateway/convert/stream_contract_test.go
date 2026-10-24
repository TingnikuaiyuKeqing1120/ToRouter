package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func streamJSON(t *testing.T, body string) ([]object, int) {
	t.Helper()
	reader := newSSEReader(strings.NewReader(body))
	var events []object
	done := 0
	for {
		_, data, err := reader.next()
		if err == io.EOF {
			return events, done
		}
		if err != nil {
			t.Fatal(err)
		}
		if data == "[DONE]" {
			done++
			continue
		}
		var event object
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

func TestAnthropicMultipleMessageDeltas(t *testing.T) {
	start := ev("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":70,"output_tokens":0,"cache_read_input_tokens":5}}}`)
	for _, tools := range []bool{false, true} {
		for _, usageFirst := range []bool{false, true} {
			name := "text"
			blocks, reason := textBlock(0, "answer"), "end_turn"
			if tools {
				name = "tool"
				blocks, reason = toolBlock(0, "t1", "f", `{\"city\":\"Paris\"}`), "tool_use"
			}
			if usageFirst {
				name += "_usage_first"
			}
			t.Run(name, func(t *testing.T) {
				finish := ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+reason+`","stop_sequence":null}}`)
				usage := ev("message_delta", `{"type":"message_delta","delta":{},"usage":{"output_tokens":3}}`)
				deltas := finish + usage
				if usageFirst {
					deltas = usage + finish
				}
				body := start + blocks + deltas + ev("message_delta", `{"type":"message_delta","delta":{},"usage":{"output_tokens":11}}`) + ev("message_stop", `{"type":"message_stop"}`)
				var output bytes.Buffer
				if _, err := StreamWithInfo(strings.NewReader(body), &output, Anthropic, OpenAI, "m"); err != nil {
					t.Fatal(err)
				}
				events, done := streamJSON(t, output.String())
				if done != 1 {
					t.Fatalf("completion markers = %d", done)
				}
				var text, finishReason string
				var gotCalls []any
				var usageEvent object
				for _, event := range events {
					if _, ok := event["usage"]; ok {
						usageEvent = event
					}
					for _, raw := range event["choices"].([]any) {
						choice := raw.(object)
						delta := choice["delta"].(object)
						if content, ok := delta["content"].(string); ok {
							text += content
						}
						if calls, ok := delta["tool_calls"].([]any); ok {
							gotCalls = append(gotCalls, calls...)
						}
						if reason, ok := choice["finish_reason"].(string); ok {
							finishReason = reason
						}
					}
				}
				wantFinish := "stop"
				if tools {
					wantFinish = "tool_calls"
					if len(gotCalls) != 1 || gotCalls[0].(object)["id"] != "t1" || gotCalls[0].(object)["function"].(object)["arguments"] != `{"city":"Paris"}` {
						t.Fatalf("buffered tools = %#v", gotCalls)
					}
				} else if text != "answer" {
					t.Fatalf("text = %q", text)
				}
				if finishReason != wantFinish {
					t.Fatalf("finish = %q", finishReason)
				}
				usageBytes, _ := json.Marshal(usageEvent)
				in, out, cache, _, ok := ParseUsage(usageBytes, OpenAI)
				if !ok || in != 70 || out != 11 || cache != 5 {
					t.Fatalf("cumulative usage = %d %d %d %v", in, out, cache, ok)
				}
			})
		}
	}
}

func TestAnthropicMessageStopRequiresFinalReason(t *testing.T) {
	body := ev("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`) + toolBlock(0, "t1", "f", `{}`) + ev("message_delta", `{"type":"message_delta","delta":{},"usage":{"output_tokens":3}}`) + ev("message_stop", `{"type":"message_stop"}`)
	var output bytes.Buffer
	if err := Stream(strings.NewReader(body), &output, Anthropic, OpenAI, "m"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("missing final reason: %v", err)
	}
	assertNoToolCall(t, output.String())
	if strings.Contains(output.String(), "[DONE]") {
		t.Fatal("incomplete message was completed")
	}
}

func TestAnthropicMultipleMessageDeltasToGemini(t *testing.T) {
	for _, tools := range []bool{false, true} {
		reason, block := "end_turn", textBlock(0, "answer")
		if tools {
			reason, block = "tool_use", toolBlock(0, "t1", "f", `{\"n\":1}`)
		}
		body := anthropicMessage(reason, block)
		old := ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+reason+`"},"usage":{"output_tokens":20}}`)
		deltas := ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":3}}`) +
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+reason+`"}}`) +
			ev("message_delta", `{"type":"message_delta","delta":{"stop_sequence":null},"usage":{"output_tokens":11}}`)
		body = strings.Replace(body, old, deltas, 1)
		var output bytes.Buffer
		if err := Stream(strings.NewReader(body), &output, Anthropic, Gemini, "m"); err != nil {
			t.Fatal(err)
		}
		events, _ := streamJSON(t, output.String())
		var text string
		var calls []object
		finishes := 0
		for _, event := range events {
			for _, raw := range event["candidates"].([]any) {
				candidate := raw.(object)
				if finish, ok := candidate["finishReason"]; ok {
					if finish != "STOP" {
						t.Fatalf("finish = %v", finish)
					}
					finishes++
					usage, _ := json.Marshal(event)
					in, out, _, _, ok := ParseUsage(usage, Gemini)
					if !ok || in != 70 || out != 11 {
						t.Fatalf("Gemini cumulative usage = %d %d %v", in, out, ok)
					}
				}
				if content, ok := candidate["content"].(object); ok {
					for _, raw := range content["parts"].([]any) {
						part := raw.(object)
						if value, ok := part["text"].(string); ok {
							text += value
						}
						if call, ok := part["functionCall"].(object); ok {
							calls = append(calls, call)
						}
					}
				}
			}
		}
		if finishes != 1 {
			t.Fatalf("finish count = %d", finishes)
		}
		if tools {
			if len(calls) != 1 || calls[0]["id"] != "t1" || calls[0]["name"] != "f" || calls[0]["args"].(object)["n"] != float64(1) {
				t.Fatalf("Gemini tool result = %#v", calls)
			}
		} else if text != "answer" {
			t.Fatalf("Gemini text = %q", text)
		}
	}
}

func TestOpenAIReasoningContentPresence(t *testing.T) {
	// DeepSeek declares this response/delta field as string or null. Only a
	// nonempty string carries reasoning; a malformed type is never a loss.
	cases := []struct {
		name    string
		present bool
		value   any
		carrier bool
		invalid bool
	}{
		{name: "absent"},
		{name: "null", present: true},
		{name: "empty", present: true, value: ""},
		{name: "nonempty", present: true, value: "reason", carrier: true},
		{name: "number", present: true, value: 0, invalid: true},
		{name: "boolean", present: true, value: false, invalid: true},
		{name: "array", present: true, value: []any{}, invalid: true},
		{name: "object", present: true, value: object{}, invalid: true},
	}
	for _, tc := range cases {
		for _, tools := range []bool{false, true} {
			for _, to := range []string{Anthropic, Gemini} {
				for _, streaming := range []bool{false, true} {
					name := tc.name + "_" + to
					if tools {
						name += "_tool"
					}
					if streaming {
						name += "_stream"
					}
					t.Run(name, func(t *testing.T) {
						message := object{"role": "assistant", "content": "answer"}
						if tc.present {
							message["reasoning_content"] = tc.value
						}
						finish := "stop"
						if tools {
							finish = "tool_calls"
							call := object{"id": "t1", "type": "function", "function": object{"name": "f", "arguments": `{"n":1}`}}
							if streaming {
								call["index"] = 0
							}
							message["tool_calls"] = []any{call}
						}
						choice := object{"index": 0, "finish_reason": finish}
						var output []byte
						var dropped []string
						var err error
						if streaming {
							choice["delta"] = message
							body, _ := json.Marshal(object{"id": "r1", "choices": []any{choice}})
							var writer bytes.Buffer
							dropped, err = StreamWithInfo(strings.NewReader(sse(string(body))+sse("[DONE]")), &writer, OpenAI, to, "m")
							output = writer.Bytes()
						} else {
							choice["message"] = message
							body, _ := json.Marshal(object{"id": "r1", "choices": []any{choice}})
							output, dropped, err = ResponseWithInfo(body, OpenAI, to, "m")
						}
						if tc.invalid {
							if !errors.Is(err, ErrMalformed) {
								t.Fatalf("invalid reasoning type: %v", err)
							}
							if len(dropped) != 0 || bytes.Contains(output, []byte("degraded")) {
								t.Fatalf("malformed type was recorded as a carrier: %s %v", output, dropped)
							}
							return
						}

						if err != nil {
							t.Fatal(err)
						}
						if tc.carrier {
							if len(dropped) != 0 {
								t.Fatalf("reasoning loss = %v", dropped)
							}
						} else if len(dropped) != 0 || bytes.Contains(output, []byte("degraded")) {
							t.Fatalf("empty field degraded: %s %v", output, dropped)
						}
						if streaming {
							// Parsing the emitted protocol must reach its legitimate end.
							var back bytes.Buffer
							if err := Stream(bytes.NewReader(output), &back, to, OpenAI, "m"); err != nil {
								t.Fatalf("target stream: %v\n%s", err, output)
							}
							if !strings.HasSuffix(back.String(), sse("[DONE]")) {
								t.Fatalf("missing terminal marker: %s", back.String())
							}
							events, _ := streamJSON(t, back.String())
							callCount := 0
							for _, event := range events {
								for _, raw := range event["choices"].([]any) {
									delta := raw.(object)["delta"].(object)
									for _, call := range optionalArray(delta, "tool_calls", &parser{}) {
										callCount++
										tool := call.(object)
										function := tool["function"].(object)
										if tool["id"] != "t1" || function["name"] != "f" || function["arguments"] != `{"n":1}` {
											t.Fatalf("streamed tool changed: %#v", tool)
										}
									}
								}
							}
							wantCalls := 0
							if tools {
								wantCalls = 1
							}
							if callCount != wantCalls {
								t.Fatalf("tool count = %d want %d", callCount, wantCalls)
							}
						} else {
							response, err := decodeResponse(output, to)
							if err != nil || textParts(response.Parts) != "answer" || hasTools(response.Parts) != tools {
								t.Fatalf("target response: %v\n%s", err, output)
							}
						}
					})
				}
			}
		}
	}
}
