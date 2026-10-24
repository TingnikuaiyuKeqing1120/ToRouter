package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// These wire fixtures model explicit provider counters, without inferring a
// cache write from a cache miss or a model name.
var chatCacheUsageCases = []struct {
	name, usage     string
	in, read, write int64
}{
	{"kimi 100k write", `{"prompt_tokens":100000,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":100000}}`, 0, 0, 100000},
	{"kimi 1M write", `{"prompt_tokens":1000000,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":1000000}}`, 0, 0, 1000000},
	{"qwen explicit", `{"prompt_tokens":100,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":5}}`, 65, 30, 5},
	// Qwen's documented cache metadata accompanies the same write count.
	{"qwen full explicit shape", `{"prompt_tokens":100,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":5},"cache_type":"ephemeral"}}`, 65, 30, 5},
	{"step hit", `{"prompt_tokens":100,"completion_tokens":0,"cached_tokens":30}`, 70, 30, 0},
	{"deepseek hit and miss", `{"prompt_tokens":100,"completion_tokens":0,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}`, 70, 30, 0},
	{"automatic cache miss remains input", `{"prompt_tokens":100000,"completion_tokens":0,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":100000}`, 100000, 0, 0},
	{"matching read and write aliases", `{"prompt_tokens":100,"completion_tokens":0,"cached_tokens":30,"prompt_cache_hit_tokens":30,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":5,"cache_creation_input_tokens":5}}`, 65, 30, 5},
}

func TestChatProviderCacheUsage(t *testing.T) {
	for _, tc := range chatCacheUsageCases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"usage":` + tc.usage + `}`)
			for _, strict := range []bool{false, true} {
				p := &parser{}
				u, ok := extractUsage(decodeRoot(body, p), OpenAIChatCompletions, p, strict)
				if p.err != nil || !ok || u.Input != tc.in || u.Output != 0 || u.CacheRead != tc.read || u.CacheWrite != tc.write {
					t.Errorf("strict=%v: usage=%+v present=%v err=%v", strict, u, ok, p.err)
				}
			}
		})
	}
}

func TestCacheUsageMalformed(t *testing.T) {
	for _, tc := range []struct{ protocol, usage string }{
		{OpenAI, `{"prompt_tokens":100,"cached_tokens":30,"prompt_tokens_details":{"cached_tokens":31}}`},
		{OpenAI, `{"prompt_tokens":100,"prompt_tokens_details":{"cache_write_tokens":5,"cache_creation_input_tokens":6}}`},
		{OpenAI, `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":96,"cache_write_tokens":5}}`},
		{OpenAIResponses, `{"input_tokens":100,"input_tokens_details":{"cached_tokens":96,"cache_write_tokens":5}}`},
		{OpenAI, `{"prompt_tokens":100,"prompt_tokens_details":{"cache_write_tokens":-1}}`},
		{OpenAI, `{"prompt_tokens":100,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":71}`},
		{OpenAI, `{"prompt_tokens":100,"prompt_tokens_details":{"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":6},"cache_type":"ephemeral"}}`},
		{OpenAI, `{"prompt_tokens":100,"prompt_tokens_details":{"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":-1},"cache_type":"ephemeral"}}`},
	} {
		for _, strict := range []bool{false, true} {
			p := &parser{}
			extractUsage(decodeRoot([]byte(`{"usage":`+tc.usage+`}`), p), tc.protocol, p, strict)
			if !errors.Is(p.err, ErrMalformed) {
				t.Errorf("strict=%v %s: got %v, want malformed", strict, tc.usage, p.err)
			}
		}
	}
}

func TestCacheUsageResponseConversion(t *testing.T) {
	for _, tc := range chatCacheUsageCases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(`{"id":"r1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + tc.usage + `}`)
			converted, err := Response(src, OpenAI, Anthropic, "requested")
			if err != nil {
				t.Fatal(err)
			}
			assertCacheUsage(t, converted, Anthropic, tc.in, 0, tc.read, tc.write)
			back, err := Response(converted, Anthropic, OpenAI, "requested")
			if err != nil {
				t.Fatal(err)
			}
			assertCacheUsage(t, back, OpenAI, tc.in, 0, tc.read, tc.write)
		})
	}
}

func TestCacheUsageStreamConversion(t *testing.T) {
	for _, tc := range chatCacheUsageCases {
		t.Run(tc.name, func(t *testing.T) {
			src := sse(`{"id":"r1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`) + sse(`{"choices":[],"usage":`+tc.usage+`}`) + sse(`[DONE]`)
			var converted bytes.Buffer
			if err := Stream(strings.NewReader(src), &converted, OpenAI, Anthropic, "requested"); err != nil {
				t.Fatal(err)
			}
			assertStreamCacheUsage(t, converted.Bytes(), Anthropic, tc.in, 0, tc.read, tc.write)
			var back bytes.Buffer
			if err := Stream(bytes.NewReader(converted.Bytes()), &back, Anthropic, OpenAI, "requested"); err != nil {
				t.Fatal(err)
			}
			assertStreamCacheUsage(t, back.Bytes(), OpenAI, tc.in, 0, tc.read, tc.write)
		})
	}
}

// Gemini has no independent cache-write counter in its native usage schema.
// Refuse that conversion instead of silently losing or relabeling the tokens.
func TestCacheWriteToGeminiRemainsUnsupported(t *testing.T) {
	src := []byte(`{"id":"r1","type":"message","role":"assistant","model":"old","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":70,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}`)
	if _, err := Response(src, Anthropic, Gemini, "requested"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("response error = %v", err)
	}
	stream := sse(`{"type":"message_start","message":{"id":"r1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":70,"output_tokens":0,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}}`) + sse(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`) + sse(`{"type":"message_stop"}`)
	var out bytes.Buffer
	if err := Stream(strings.NewReader(stream), &out, Anthropic, Gemini, "requested"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("stream error = %v", err)
	}
}

func assertCacheUsage(t *testing.T, body []byte, protocol string, input, output, read, write int64) {
	t.Helper()
	in, out, cr, cw, ok := ParseUsage(body, protocol)
	if !ok || in != input || out != output || cr != read || cw != write {
		t.Fatalf("%s: got %d/%d/%d/%d present=%v, body=%s", protocol, in, out, cr, cw, ok, body)
	}
	if protocol == OpenAI {
		var response struct {
			Usage struct {
				Prompt int64 `json:"prompt_tokens"`
				Total  int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatal(err)
		}
		if response.Usage.Prompt != input+read+write || response.Usage.Total != input+read+write+output {
			t.Fatalf("OpenAI wire totals omit or duplicate cache: %s", body)
		}
	}
}

func assertStreamCacheUsage(t *testing.T, body []byte, protocol string, input, output, read, write int64) {
	t.Helper()
	sr := newSSEReader(bytes.NewReader(body))
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
		u, ok := extractUsage(decodeRoot([]byte(data), p), protocol, p, false)
		if p.err != nil {
			t.Fatal(p.err)
		}
		if ok {
			mergeUsage(&usage, u)
		}
	}
	b, err := json.Marshal(object{"usage": usageOut(usage, Anthropic)})
	if err != nil {
		t.Fatal(err)
	}
	assertCacheUsage(t, b, Anthropic, input, output, read, write)
}
