package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"torouter/internal/gateway/convert"
	"torouter/internal/store"
)

// Exercise the same source counters through passthrough, conversion and SSE
// observation. The saved row must agree with the usage received by the client.
func TestProviderCacheUsageHTTP(t *testing.T) {
	cases := []struct {
		name, format, usage        string
		input, output, read, write int64
	}{
		{"kimi 100k", store.FormatOpenAIChatCompletions, `{"prompt_tokens":100000,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":100000}}`, 0, 0, 0, 100000},
		{"kimi 1M", store.FormatOpenAIChatCompletions, `{"prompt_tokens":1000000,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":1000000}}`, 0, 0, 0, 1000000},
		{"qwen explicit", store.FormatOpenAIChatCompletions, `{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":5}}`, 65, 20, 30, 5},
		{"qwen full explicit shape", store.FormatOpenAIChatCompletions, `{"prompt_tokens":100,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":5},"cache_type":"ephemeral"}}`, 65, 0, 30, 5},
		{"step hit", store.FormatOpenAIChatCompletions, `{"prompt_tokens":100,"completion_tokens":20,"cached_tokens":30}`, 70, 20, 30, 0},
		{"deepseek hit and miss", store.FormatOpenAIChatCompletions, `{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}`, 70, 20, 30, 0},
		{"anthropic excludes cache", store.FormatAnthropicMessages, `{"input_tokens":70,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}`, 70, 20, 30, 5},
	}
	for _, tc := range cases {
		for _, inbound := range []string{store.FormatOpenAIChatCompletions, store.FormatAnthropicMessages} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.name, inbound, stream), func(t *testing.T) {
					g, s := setup(t)
					settings(t, s, 3, 1)
					wire := `{"id":"r1","object":"chat.completion","model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + tc.usage + `}`
					if tc.format == store.FormatAnthropicMessages {
						wire = `{"id":"r1","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":` + tc.usage + `}`
					}
					if stream {
						wire = "data: {\"id\":\"r1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" + "data: {\"choices\":[],\"usage\":" + tc.usage + "}\n\ndata: [DONE]\n\n"
						if tc.format == store.FormatAnthropicMessages {
							wire = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"r1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"test\",\"content\":[],\"stop_reason\":null,\"usage\":" + tc.usage + "}}\n\n" + fmt.Sprintf("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\ndata: {\"type\":\"message_stop\"}\n\n", tc.output)
						}
					}
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						io.WriteString(w, wire)
					}))
					defer up.Close()
					addFormat(t, s, up.URL, tc.format)
					path := "/v1/chat/completions"
					if inbound == store.FormatAnthropicMessages {
						path = "/v1/messages"
					}
					w := invoke(g, path, fmt.Sprintf(`{"model":"test","stream":%v,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`, stream))
					if w.Code != 200 {
						t.Fatalf("%d %s", w.Code, w.Body.String())
					}
					rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
					if err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 {
						t.Fatalf("rows = %+v", rows)
					}
					row := rows[0]
					if row.Status != 200 || row.Estimated || row.InputTokens != tc.input || row.OutputTokens != tc.output || row.CacheReadTokens != tc.read || row.CacheWriteTokens != tc.write {
						t.Fatalf("recorded usage = %+v", row)
					}
					if stream {
						client := store.Usage{}
						o := newObserver(strings.NewReader(w.Body.String()), inbound, &client)
						if _, err := io.ReadAll(o); err != nil {
							t.Fatal(err)
						}
						if !o.complete || !o.hasUsage || client.InputTokens != tc.input || client.OutputTokens != tc.output || client.CacheReadTokens != tc.read || client.CacheWriteTokens != tc.write {
							t.Fatalf("client usage = %+v complete=%v hasUsage=%v body=%s", client, o.complete, o.hasUsage, w.Body.String())
						}
					} else {
						in, out, cr, cw, ok := convert.ParseUsage(w.Body.Bytes(), inbound)
						if !ok || in != tc.input || out != tc.output || cr != tc.read || cw != tc.write {
							t.Fatalf("client usage=%d/%d/%d/%d present=%v body=%s", in, out, cr, cw, ok, w.Body.String())
						}
					}
				})
			}
		}
	}
}

// addFormat binds the test model to a channel serving one endpoint format,
// which is how the gateway sees a Responses-only or dual-facade provider.
func addFormat(t *testing.T, s *store.Store, url, format string) {
	t.Helper()
	c := store.Channel{Name: "test", APIKey: "upstream-secret", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: format, BaseURL: url}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "test", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
}

// T024: usage rows name the endpoint formats, not the wire family. "openai"
// said nothing about whether the attempt spoke Chat Completions or Responses,
// and the minimum unit of observability is endpoint × model × format. The same
// fix records Responses usage instead of booking it as estimated: the upstream
// reported input/output/cache tokens that the Chat Completions schema could
// not see.
func TestUsageRowNamesEndpointFormats(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/messages") {
			io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`)
			return
		}
		io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":5},"output_tokens_details":{"reasoning_tokens":7}}}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "two-facades", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: up.URL}, {Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "test", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("chat %d %s", w.Code, w.Body.String())
	}
	if w := invoke(g, "/v1/responses", `{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`); w.Code != 200 {
		t.Fatalf("responses %d %s", w.Code, w.Body.String())
	}
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("usage rows = %+v", rows)
	}
	for _, row := range rows {
		if row.Inbound == "openai" || row.Outbound == "openai" {
			t.Fatalf("family value recorded instead of a format: %+v", row)
		}
		switch row.Outbound {
		case store.FormatAnthropicMessages:
			if row.Inbound != store.FormatOpenAIChatCompletions || row.Estimated || row.InputTokens != 7 || row.OutputTokens != 3 {
				t.Fatalf("chat row = %+v", row)
			}
		case store.FormatOpenAIResponses:
			if row.Inbound != store.FormatOpenAIResponses {
				t.Fatalf("responses row inbound = %+v", row)
			}
			if row.Estimated {
				t.Fatalf("responses usage not read from the response: %+v", row)
			}
			if row.InputTokens != 65 || row.OutputTokens != 20 || row.CacheReadTokens != 30 || row.CacheWriteTokens != 5 {
				t.Fatalf("responses usage = %+v", row)
			}
		default:
			t.Fatalf("unexpected outbound %q: %+v", row.Outbound, row)
		}
	}
}

// A Responses stream passes through untouched; the observer reads usage from
// the response.completed event, where the whole response object — usage
// included — sits below the event (T024).
func TestResponsesStreamUsageIsRecorded(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	stream := "data: {\"type\":\"response.created\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":13,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"total_tokens\":120,\"input_tokens_details\":{\"cached_tokens\":30,\"cache_write_tokens\":5},\"output_tokens_details\":{\"reasoning_tokens\":7}}}}\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	defer up.Close()
	addFormat(t, s, up.URL, store.FormatOpenAIResponses)
	w := invoke(g, "/v1/responses", `{"model":"test","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w.Body.String() != stream {
		t.Fatalf("responses stream not passed through: %s", w.Body.String())
	}
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %+v", rows)
	}
	row := rows[0]
	if row.Inbound != store.FormatOpenAIResponses || row.Outbound != store.FormatOpenAIResponses {
		t.Fatalf("row formats = %+v", row)
	}
	if row.Estimated {
		t.Fatalf("responses stream usage not read: %+v", row)
	}
	if row.InputTokens != 65 || row.OutputTokens != 20 || row.CacheReadTokens != 30 || row.CacheWriteTokens != 5 {
		t.Fatalf("responses stream usage = %+v", row)
	}
}

// The observer takes the endpoint format so it can tell the two OpenAI facades
// apart; wire family values keep their meaning, including the Gemini
// finish-reason duck.
func TestObserverReadsResponsesUsageByFormat(t *testing.T) {
	data := "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":13,\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"input_tokens_details\":{\"cached_tokens\":30,\"cache_write_tokens\":5},\"output_tokens_details\":{\"reasoning_tokens\":7}}}}\n\n"
	u := store.Usage{}
	o := newObserver(strings.NewReader(data), store.FormatOpenAIResponses, &u)
	got, err := io.ReadAll(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != data {
		t.Fatal("wire bytes changed")
	}
	if !o.complete || !o.hasUsage || u.InputTokens != 65 || u.OutputTokens != 20 || u.CacheReadTokens != 30 || u.CacheWriteTokens != 5 {
		t.Fatalf("usage %+v complete %v hasUsage %v", u, o.complete, o.hasUsage)
	}
	gemini := "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n"
	g := store.Usage{}
	og := newObserver(strings.NewReader(gemini), store.FormatGeminiGenerateContent, &g)
	if _, err := io.ReadAll(og); err != nil {
		t.Fatal(err)
	}
	if !og.complete {
		t.Fatal("gemini finish reason missed under the full format name")
	}
}

// The convert package mirrors store's format names so it can take an endpoint
// format; a silent drift would send one family's usage to the other's schema.
func TestUsageProtocolNamesMatchStoreFormats(t *testing.T) {
	for format, want := range map[string]string{
		convert.OpenAIChatCompletions: store.FormatOpenAIChatCompletions,
		convert.OpenAIResponses:       store.FormatOpenAIResponses,
		convert.AnthropicMessages:     store.FormatAnthropicMessages,
		convert.GeminiGenerateContent: store.FormatGeminiGenerateContent,
	} {
		if format != want {
			t.Errorf("convert name %q != store format %q", format, want)
		}
	}
}
