package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/auth"
	"torouter/internal/store"
)

type conversionObject = map[string]any

var conversionFormats = []string{store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent}

func conversionJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func conversionObjectOf(v any) conversionObject { m, _ := v.(map[string]any); return m }
func conversionArray(v any) []any               { a, _ := v.([]any); return a }

func conversionPath(format string, stream bool) string {
	switch format {
	case store.FormatOpenAIChatCompletions:
		return "/v1/chat/completions"
	case store.FormatOpenAIResponses:
		return "/v1/responses"
	case store.FormatAnthropicMessages:
		return "/v1/messages"
	default:
		if stream {
			return "/v1beta/models/test:streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/test:generateContent"
	}
}

func conversionReasoning(v any) []any {
	var parts []any
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case []any:
			for _, item := range n {
				walk(item)
			}
		case map[string]any:
			if n["type"] == "reasoning" || n["type"] == "thinking" || n["thought"] == true {
				parts = append(parts, n)
				return
			}
			for _, item := range n {
				walk(item)
			}
		}
	}
	walk(v)
	return parts
}

func conversionAssertErrorDiagnostic(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var root conversionObject
	if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	d := conversionObjectOf(conversionObjectOf(root["error"])["diagnostic"])
	requestID := w.Header().Get(requestIDHeader)
	if requestID == "" || d["request_id"] != requestID || d["code"] == nil {
		t.Fatalf("client error did not carry a structured correlated diagnostic: %s", w.Body.String())
	}
}

func conversionInitial(format string) []any {
	switch format {
	case store.FormatGeminiGenerateContent:
		return []any{conversionObject{"role": "user", "parts": []any{conversionObject{"text": "keep-question"}}}}
	default:
		return []any{conversionObject{"role": "user", "content": "keep-question"}}
	}
}

func conversionBody(format string, history []any, stream bool) []byte {
	schema := conversionObject{"type": "object", "properties": conversionObject{"x": conversionObject{"type": "integer"}}}
	b := conversionObject{"model": "test", "stream": stream}
	switch format {
	case store.FormatOpenAIChatCompletions:
		b["messages"], b["tools"] = history, []any{conversionObject{"type": "function", "function": conversionObject{"name": "f", "parameters": schema}}}
	case store.FormatOpenAIResponses:
		b["input"], b["tools"] = history, []any{conversionObject{"type": "function", "name": "f", "parameters": schema}}
	case store.FormatAnthropicMessages:
		b["messages"], b["max_tokens"], b["tools"] = history, 128, []any{conversionObject{"name": "f", "input_schema": schema}}
	case store.FormatGeminiGenerateContent:
		delete(b, "stream")
		delete(b, "model")
		b["contents"], b["tools"] = history, []any{conversionObject{"functionDeclarations": []any{conversionObject{"name": "f", "parameters": schema}}}}
	}
	return conversionJSON(b)
}

// Inspect the actual wire independently of the production converter. Tool results
// are returned in reverse order to distinguish ID pairing from positional pairing.
func conversionCalls(v any) (map[string]int, map[string]string) {
	calls, results := map[string]int{}, map[string]string{}
	var walk func(any)
	args := func(v any) int {
		if s, ok := v.(string); ok {
			_ = json.Unmarshal([]byte(s), &v)
		}
		n, _ := conversionObjectOf(v)["x"].(float64)
		return int(n)
	}
	walk = func(v any) {
		switch n := v.(type) {
		case []any:
			for _, x := range n {
				walk(x)
			}
		case map[string]any:
			switch n["type"] {
			case "function_call":
				calls[fmt.Sprint(n["call_id"])] = args(n["arguments"])
			case "function_call_output":
				results[fmt.Sprint(n["call_id"])] = string(conversionJSON(n["output"]))
			case "tool_use":
				calls[fmt.Sprint(n["id"])] = args(n["input"])
			case "tool_result":
				results[fmt.Sprint(n["tool_use_id"])] = string(conversionJSON(n["content"]))
			}
			if n["role"] == "tool" {
				results[fmt.Sprint(n["tool_call_id"])] = string(conversionJSON(n["content"]))
			}
			for _, tc := range conversionArray(n["tool_calls"]) {
				c := conversionObjectOf(tc)
				calls[fmt.Sprint(c["id"])] = args(conversionObjectOf(c["function"])["arguments"])
			}
			if c := conversionObjectOf(n["functionCall"]); c != nil {
				calls[fmt.Sprint(c["id"])] = args(c["args"])
			}
			if c := conversionObjectOf(n["functionResponse"]); c != nil {
				results[fmt.Sprint(c["id"])] = string(conversionJSON(c["response"]))
			}
			for _, x := range n {
				walk(x)
			}
		}
	}
	walk(v)
	return calls, results
}

func conversionNativeResponse(format string, round int, reasoning bool) conversionObject {
	var numbers []int
	if round == 1 {
		numbers = []int{1, 2}
	} else if round == 2 {
		numbers = []int{3}
	}
	text, finish := fmt.Sprintf("visible-round-%d", round), "tool_calls"
	if round == 3 {
		text, finish = "finished", "stop"
	}
	var parts []any
	if reasoning && round < 3 {
		switch format {
		case store.FormatOpenAIResponses:
			parts = append(parts, conversionObject{"type": "reasoning", "id": fmt.Sprintf("rs-%d", round), "summary": []any{}, "encrypted_content": "opaque+/=bytes-\\u1234"})
		case store.FormatAnthropicMessages:
			parts = append(parts, conversionObject{"type": "thinking", "thinking": "synthetic-thought", "signature": "signature+/=bytes"})
		case store.FormatGeminiGenerateContent:
			parts = append(parts, conversionObject{"thought": true, "text": "synthetic-thought", "thoughtSignature": "signature+/=bytes"})
		}
	}
	switch format {
	case store.FormatOpenAIChatCompletions:
		msg := conversionObject{"role": "assistant", "content": text}
		var calls []any
		for _, x := range numbers {
			calls = append(calls, conversionObject{"type": "function", "id": fmt.Sprintf("c%d", x), "function": conversionObject{"name": "f", "arguments": fmt.Sprintf(`{"x":%d}`, x)}})
		}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		return conversionObject{"id": "response-native", "object": "chat.completion", "model": "test", "choices": []any{conversionObject{"index": 0, "message": msg, "finish_reason": finish}}, "usage": conversionObject{"prompt_tokens": 5, "completion_tokens": 3}}
	case store.FormatOpenAIResponses:
		parts = append(parts, conversionObject{"type": "message", "id": "msg-native", "role": "assistant", "status": "completed", "content": []any{conversionObject{"type": "output_text", "text": text, "annotations": []any{}}}})
		for _, x := range numbers {
			parts = append(parts, conversionObject{"type": "function_call", "id": fmt.Sprintf("item%d", x), "call_id": fmt.Sprintf("c%d", x), "name": "f", "arguments": fmt.Sprintf(`{"x":%d}`, x), "status": "completed"})
		}
		return conversionObject{"id": "response-native", "object": "response", "status": "completed", "model": "test", "output": parts, "usage": conversionObject{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8}}
	case store.FormatAnthropicMessages:
		parts = append(parts, conversionObject{"type": "text", "text": text})
		for _, x := range numbers {
			parts = append(parts, conversionObject{"type": "tool_use", "id": fmt.Sprintf("c%d", x), "name": "f", "input": conversionObject{"x": x}})
		}
		stop := "tool_use"
		if round == 3 {
			stop = "end_turn"
		}
		return conversionObject{"id": "response-native", "type": "message", "role": "assistant", "model": "test", "content": parts, "stop_reason": stop, "usage": conversionObject{"input_tokens": 5, "output_tokens": 3}}
	default:
		parts = append(parts, conversionObject{"text": text})
		for _, x := range numbers {
			parts = append(parts, conversionObject{"functionCall": conversionObject{"id": fmt.Sprintf("c%d", x), "name": "f", "args": conversionObject{"x": x}}})
		}
		return conversionObject{"responseId": "response-native", "candidates": []any{conversionObject{"index": 0, "content": conversionObject{"role": "model", "parts": parts}, "finishReason": "STOP"}}, "usageMetadata": conversionObject{"promptTokenCount": 5, "candidatesTokenCount": 3}}
	}
}

func conversionStream(format string, root conversionObject) string {
	var out strings.Builder
	emit := func(v any) { out.WriteString("data: "); out.Write(conversionJSON(v)); out.WriteString("\n\n") }
	switch format {
	case store.FormatOpenAIResponses:
		emit(conversionObject{"type": "response.created", "response": conversionObject{"id": root["id"], "status": "in_progress", "output": []any{}}})
		for i, v := range conversionArray(root["output"]) {
			item := conversionObjectOf(v)
			initial := conversionObject{}
			for k, v := range item {
				initial[k] = v
			}
			if item["type"] == "function_call" {
				initial["arguments"] = ""
			}
			if item["type"] == "message" {
				initial["content"] = []any{}
			}
			emit(conversionObject{"type": "response.output_item.added", "output_index": i, "item": initial})
			if item["type"] == "message" {
				emit(conversionObject{"type": "response.output_text.delta", "output_index": i, "item_id": item["id"], "content_index": 0, "delta": conversionObjectOf(conversionArray(item["content"])[0])["text"]})
			}
			emit(conversionObject{"type": "response.output_item.done", "output_index": i, "item": item})
		}
		emit(conversionObject{"type": "response.completed", "response": root})
	case store.FormatOpenAIChatCompletions:
		choice := conversionObjectOf(conversionArray(root["choices"])[0])
		msg := conversionObjectOf(choice["message"])
		for i, v := range conversionArray(msg["tool_calls"]) {
			conversionObjectOf(v)["index"] = i
		}
		emit(conversionObject{"id": "response-native", "choices": []any{conversionObject{"index": 0, "delta": msg, "finish_reason": nil}}})
		emit(conversionObject{"choices": []any{conversionObject{"index": 0, "delta": conversionObject{}, "finish_reason": choice["finish_reason"]}}, "usage": root["usage"]})
		out.WriteString("data: [DONE]\n\n")
	case store.FormatAnthropicMessages:
		emit(conversionObject{"type": "message_start", "message": conversionObject{"id": "response-native", "type": "message", "role": "assistant", "model": "test", "content": []any{}, "stop_reason": nil, "usage": conversionObject{"input_tokens": 5, "output_tokens": 0}}})
		for i, v := range conversionArray(root["content"]) {
			part := conversionObjectOf(v)
			initial, delta := conversionObject{}, conversionObject{}
			if part["type"] == "text" {
				initial = conversionObject{"type": "text", "text": ""}
				delta = conversionObject{"type": "text_delta", "text": part["text"]}
			} else {
				initial = conversionObject{"type": "tool_use", "id": part["id"], "name": part["name"], "input": conversionObject{}}
				delta = conversionObject{"type": "input_json_delta", "partial_json": string(conversionJSON(part["input"]))}
			}
			emit(conversionObject{"type": "content_block_start", "index": i, "content_block": initial})
			emit(conversionObject{"type": "content_block_delta", "index": i, "delta": delta})
			emit(conversionObject{"type": "content_block_stop", "index": i})
		}
		emit(conversionObject{"type": "message_delta", "delta": conversionObject{"stop_reason": root["stop_reason"]}, "usage": conversionObject{"output_tokens": 3}})
		emit(conversionObject{"type": "message_stop"})
	}
	return out.String()
}

// Reconstruct only documented client events; do not use convert.Stream to feed
// its own output back as a test oracle.
func conversionCollectStream(t *testing.T, format string, data []byte) conversionObject {
	t.Helper()
	msg := conversionObject{"role": "assistant", "content": ""}
	calls := map[int]conversionObject{}
	blocks := map[int]conversionObject{}
	partial := map[int]string{}
	var output []any
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(strings.TrimPrefix(line, "data: ")) == "[DONE]" {
			continue
		}
		var e conversionObject
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatal(err)
		}
		switch format {
		case store.FormatOpenAIChatCompletions:
			for _, v := range conversionArray(e["choices"]) {
				d := conversionObjectOf(conversionObjectOf(v)["delta"])
				if s, ok := d["content"].(string); ok {
					msg["content"] = fmt.Sprint(msg["content"]) + s
				}
				if s, ok := d["reasoning_content"].(string); ok {
					old, _ := msg["reasoning_content"].(string)
					msg["reasoning_content"] = old + s
				}
				for _, v := range conversionArray(d["tool_calls"]) {
					c := conversionObjectOf(v)
					i := int(c["index"].(float64))
					if calls[i] == nil {
						calls[i] = conversionObject{"id": c["id"], "type": "function", "function": conversionObject{"name": "", "arguments": ""}}
					}
					f := conversionObjectOf(calls[i]["function"])
					for k, v := range conversionObjectOf(c["function"]) {
						if s, ok := v.(string); ok {
							f[k] = fmt.Sprint(f[k]) + s
						}
					}
				}
			}
		case store.FormatAnthropicMessages:
			i, _ := e["index"].(float64)
			idx := int(i)
			if e["type"] == "content_block_start" {
				blocks[idx] = conversionObjectOf(e["content_block"])
			}
			if e["type"] == "content_block_delta" {
				d := conversionObjectOf(e["delta"])
				for _, k := range []string{"text", "thinking", "signature"} {
					if s, ok := d[k].(string); ok {
						old, _ := blocks[idx][k].(string)
						blocks[idx][k] = old + s
					}
				}
				if s, ok := d["partial_json"].(string); ok {
					partial[idx] += s
				}
			}
		case store.FormatOpenAIResponses:
			if e["type"] == "response.output_item.done" {
				output = append(output, e["item"])
			}
		}
	}
	if format == store.FormatOpenAIResponses {
		return conversionObject{"output": output}
	}
	if format == store.FormatAnthropicMessages {
		for i := 0; i < len(blocks); i++ {
			if partial[i] != "" {
				var args any
				if err := json.Unmarshal([]byte(partial[i]), &args); err != nil {
					t.Fatal(err)
				}
				blocks[i]["input"] = args
			}
			output = append(output, blocks[i])
		}
		return conversionObject{"content": output}
	}
	for i := 0; i < len(calls); i++ {
		output = append(output, calls[i])
	}
	if len(output) > 0 {
		msg["tool_calls"] = output
	}
	return conversionObject{"choices": []any{conversionObject{"message": msg}}}
}

func conversionContinue(format string, history []any, root conversionObject) []any {
	var assistant any
	switch format {
	case store.FormatOpenAIChatCompletions:
		assistant = conversionObjectOf(conversionArray(root["choices"])[0])["message"]
	case store.FormatAnthropicMessages:
		assistant = conversionObject{"role": "assistant", "content": root["content"]}
	case store.FormatGeminiGenerateContent:
		assistant = conversionObjectOf(conversionArray(root["candidates"])[0])["content"]
	case store.FormatOpenAIResponses:
		history = append(history, conversionArray(root["output"])...)
		assistant = root["output"]
	}
	if format != store.FormatOpenAIResponses {
		history = append(history, assistant)
	}
	calls, _ := conversionCalls(assistant)
	ids := make([]string, 0, len(calls))
	for id := range calls {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	var parts []any
	for _, id := range ids {
		result := fmt.Sprintf("result-%d", calls[id])
		switch format {
		case store.FormatOpenAIChatCompletions:
			history = append(history, conversionObject{"role": "tool", "tool_call_id": id, "content": result})
		case store.FormatOpenAIResponses:
			history = append(history, conversionObject{"type": "function_call_output", "call_id": id, "output": result})
		case store.FormatAnthropicMessages:
			parts = append(parts, conversionObject{"type": "tool_result", "tool_use_id": id, "content": result})
		case store.FormatGeminiGenerateContent:
			parts = append(parts, conversionObject{"functionResponse": conversionObject{"id": id, "name": "f", "response": conversionObject{"output": result}}})
		}
	}
	if len(parts) > 0 {
		key := "content"
		if format == store.FormatGeminiGenerateContent {
			key = "parts"
		}
		history = append(history, conversionObject{"role": "user", key: parts})
	}
	return history
}

func conversionInvoke(g *Gateway, format string, history []any, stream bool, clientID int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, conversionPath(format, stream), bytes.NewReader(conversionBody(format, history, stream)))
	r = r.WithContext(auth.WithClientContext(r.Context(), store.ClientKey{ID: clientID, Scope: store.DefaultClientKeyScope()}))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestConversionGatewayTwelveDirectionsTwoToolRounds(t *testing.T) {
	for _, client := range conversionFormats {
		for _, upstream := range conversionFormats {
			if client == upstream {
				continue
			}
			t.Run(client+"_to_"+upstream, func(t *testing.T) { conversionToolConversation(t, client, upstream, false) })
		}
	}
}

func TestConversionGatewayResponsesToolStreams(t *testing.T) {
	for _, pair := range [][2]string{{store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses}, {store.FormatAnthropicMessages, store.FormatOpenAIResponses}, {store.FormatOpenAIResponses, store.FormatOpenAIChatCompletions}, {store.FormatOpenAIResponses, store.FormatAnthropicMessages}} {
		t.Run(pair[0]+"_to_"+pair[1], func(t *testing.T) { conversionToolConversation(t, pair[0], pair[1], true) })
	}
}

func conversionToolConversation(t *testing.T, client, upstream string, stream bool) {
	t.Helper()
	g, s := setup(t)
	var count atomic.Int32
	reasoning := upstream != store.FormatOpenAIChatCompletions && !(stream && upstream == store.FormatAnthropicMessages)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round := int(count.Add(1))
		if r.Method != http.MethodPost {
			t.Error("upstream method was not POST")
		}
		var body conversionObject
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		calls, results := conversionCalls(body)
		want := 0
		if round == 2 {
			want = 2
		} else if round == 3 {
			want = 3
		}
		if len(calls) != want || len(results) != want {
			t.Errorf("round %d call/result counts=%d/%d want=%d wire=%s", round, len(calls), len(results), want, conversionJSON(body))
		}
		for i := 1; i <= want; i++ {
			id := fmt.Sprintf("c%d", i)
			if calls[id] != i || !strings.Contains(results[id], fmt.Sprintf("result-%d", i)) {
				t.Errorf("round %d tool pairing %s changed: args=%d result=%s", round, id, calls[id], results[id])
			}
		}
		if !bytes.Contains(conversionJSON(body), []byte("keep-question")) {
			t.Error("original user content lost")
		}
		if round > 1 && reasoning {
			marker := "signature+/=bytes"
			if upstream == store.FormatOpenAIResponses {
				marker = "opaque+/=bytes-\\u1234"
			}
			if !bytes.Contains(conversionJSON(body), conversionJSON(marker)) {
				t.Error("reasoning attachment bytes did not return to the original upstream")
			}
			var want []any
			for previous := 1; previous < round; previous++ {
				want = append(want, conversionReasoning(conversionNativeResponse(upstream, previous, true))...)
			}
			if !bytes.Equal(conversionJSON(conversionReasoning(body)), conversionJSON(want)) {
				t.Errorf("round %d original reasoning attachment structure changed", round)
			}
		}
		response := conversionNativeResponse(upstream, round, reasoning)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, conversionStream(upstream, response))
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Write(conversionJSON(response))
		}
	}))
	defer up.Close()
	addFormat(t, s, up.URL, upstream)
	history := conversionInitial(client)
	for round := 1; round <= 3; round++ {
		w := conversionInvoke(g, client, history, stream, 101)
		if w.Code != 200 {
			t.Fatalf("round %d HTTP %d: %s", round, w.Code, w.Body.String())
		}
		if w.Header().Get(requestIDHeader) == "" {
			t.Error("missing correlation header")
		}
		if round == 3 {
			if !strings.Contains(w.Body.String(), "finished") {
				t.Fatalf("missing final answer: %s", w.Body.String())
			}
			break
		}
		var root conversionObject
		if stream {
			root = conversionCollectStream(t, client, w.Body.Bytes())
		} else if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
			t.Fatal(err)
		}
		calls, _ := conversionCalls(root)
		want := 2
		if round == 2 {
			want = 1
		}
		if len(calls) != want {
			t.Fatalf("client tool calls=%d want=%d body=%s", len(calls), want, w.Body.String())
		}
		history = conversionContinue(client, history, root)
	}
	if count.Load() != 3 {
		t.Fatalf("upstream requests=%d want exactly 3", count.Load())
	}
}

func TestConversionGatewayReasoningScopeRejectsOtherClient(t *testing.T) {
	for _, client := range []string{store.FormatOpenAIChatCompletions, store.FormatAnthropicMessages} {
		t.Run(client, func(t *testing.T) {
			g, s := setup(t)
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Write(conversionJSON(conversionNativeResponse(store.FormatOpenAIResponses, 1, true)))
			}))
			defer up.Close()
			addFormat(t, s, up.URL, store.FormatOpenAIResponses)
			history := conversionInitial(client)
			first := conversionInvoke(g, client, history, false, 101)
			if first.Code != 200 {
				t.Fatalf("first response: %d %s", first.Code, first.Body.String())
			}
			var root conversionObject
			if err := json.Unmarshal(first.Body.Bytes(), &root); err != nil {
				t.Fatal(err)
			}
			history = conversionContinue(client, history, root)
			second := conversionInvoke(g, client, history, false, 202)
			if second.Code < 400 {
				t.Fatalf("different client accepted private attachment: %d", second.Code)
			}
			if calls.Load() != 1 {
				t.Fatal("cross-client attachment reached upstream")
			}
			conversionAssertErrorDiagnostic(t, second)
		})
	}
}

func TestConversionGatewayReasoningRetryBoundariesAndCorrelation(t *testing.T) {
	for _, tc := range []struct {
		name, policy  string
		status        int
		errorBody     string
		secondSuccess bool
		wantCalls     int
	}{
		{"preserve", "preserve", 400, `{"error":{"code":"invalid_encrypted_content"}}`, false, 1},
		{"availability recovers", "availability", 400, `{"error":{"code":"invalid_encrypted_content"}}`, true, 2},
		{"availability once only", "availability", 400, `{"error":{"code":"invalid_encrypted_content"}}`, false, 2},
		{"generic400", "availability", 400, `{"error":{"message":"Bad request"}}`, false, 1},
		{"generic500", "availability", 500, `{"error":{"code":"invalid_encrypted_content"}}`, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s := setup(t)
			settings, err := s.GetSettings()
			if err != nil {
				t.Fatal(err)
			}
			settings.ReasoningPolicy = tc.policy
			settings.RetriesPerChannel = 4
			if err := s.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var credential string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				if n == 1 {
					credential = r.Header.Get("Authorization")
				} else if r.Header.Get("Authorization") != credential {
					t.Error("reasoning retry switched credentials")
				}
				if !bytes.Contains(body, []byte("keep-question")) || !bytes.Contains(body, []byte("result-1")) || !bytes.Contains(body, []byte("function_call")) {
					t.Error("retry removed conversation or tool history")
				}
				hasReason := bytes.Contains(body, []byte("opaque-retry"))
				if n == 1 && !hasReason {
					t.Error("first attempt stripped reasoning")
				}
				if n > 1 && hasReason {
					t.Error("retry did not strip the rejected reasoning")
				}
				if n > 1 && tc.secondSuccess {
					w.Header().Set("Content-Type", "application/json")
					w.Write(conversionJSON(conversionNativeResponse(store.FormatOpenAIResponses, 3, false)))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.errorBody)
			}))
			defer up.Close()
			addFormat(t, s, up.URL, store.FormatOpenAIResponses)
			history := []any{conversionObject{"role": "user", "content": "keep-question"}, conversionObject{"type": "reasoning", "encrypted_content": "opaque-retry"}, conversionObject{"type": "function_call", "call_id": "c1", "name": "f", "arguments": `{"x":1}`}, conversionObject{"type": "function_call_output", "call_id": "c1", "output": "result-1"}}
			w := conversionInvoke(g, store.FormatOpenAIResponses, history, false, 101)
			if int(calls.Load()) != tc.wantCalls {
				t.Fatalf("upstream calls=%d want=%d body=%s", calls.Load(), tc.wantCalls, w.Body.String())
			}
			if tc.secondSuccess && w.Code != 200 {
				t.Fatalf("recovery failed: %d %s", w.Code, w.Body.String())
			}
			if !tc.secondSuccess && w.Code < 400 {
				t.Fatal("failed request looked successful")
			}
			requestID := w.Header().Get(requestIDHeader)
			if requestID == "" {
				t.Fatal("missing request ID")
			}
			rows, err := s.ListUsage(store.UsageFilter{RequestID: requestID})
			if err != nil || len(rows) != tc.wantCalls {
				t.Fatalf("correlated logs=%d want=%d err=%v", len(rows), tc.wantCalls, err)
			}
			for _, row := range rows {
				var d ConversionDiagnostic
				if err := json.Unmarshal(row.Diagnostic, &d); err != nil {
					t.Fatal(err)
				}
				if row.RequestID != requestID || d.RequestID != requestID {
					t.Fatal("attempt or diagnostic changed logical request ID")
				}
			}
			if !tc.secondSuccess {
				conversionAssertErrorDiagnostic(t, w)
			}
			if tc.secondSuccess && rows[0].Degraded == "" {
				t.Fatal("successful recovery lost degradation evidence")
			}
		})
	}
}

func TestConversionGatewayCommittedStreamNeverStripsOrReplays(t *testing.T) {
	g, s := setup(t)
	settings, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.ReasoningPolicy = store.ReasoningAvailability
	settings.RetriesPerChannel = 4
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"partial","choices":[{"index":0,"delta":{"role":"assistant","content":"already-delivered"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, `data: {"error":{"code":"invalid_encrypted_content","message":"Invalid reasoning state"}}`+"\n\n")
	}))
	defer up.Close()
	addFormat(t, s, up.URL, store.FormatOpenAIChatCompletions)
	w := conversionInvoke(g, store.FormatOpenAIResponses, conversionInitial(store.FormatOpenAIResponses), true, 101)
	if calls.Load() != 1 {
		t.Fatal("committed stream replayed upstream generation")
	}
	if !strings.Contains(w.Body.String(), "already-delivered") {
		t.Fatalf("expected committed content: %s", w.Body.String())
	}
	rows, err := s.ListUsage(store.UsageFilter{RequestID: w.Header().Get(requestIDHeader)})
	if err != nil || len(rows) != 1 || rows[0].Status < 400 {
		t.Fatalf("stream failure not recorded: %+v %v", rows, err)
	}
	var d ConversionDiagnostic
	if err := json.Unmarshal(rows[0].Diagnostic, &d); err != nil {
		t.Fatal(err)
	}
	if d.RequestID != rows[0].RequestID || len(d.ActionsTaken) == 0 {
		t.Fatal("stream failure lost correlated handling evidence")
	}
	found := false
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event conversionObject
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "response.failed" {
			diagnostic := conversionObjectOf(conversionObjectOf(conversionObjectOf(event["response"])["error"])["diagnostic"])
			if diagnostic["request_id"] != rows[0].RequestID {
				t.Fatal("stream client diagnostic changed correlation ID")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("failed stream did not send its diagnostic terminal event")
	}
}
