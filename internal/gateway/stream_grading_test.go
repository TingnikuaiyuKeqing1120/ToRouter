package gateway

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/gateway/convert"
	"torouter/internal/store"
)

// T018 follow-up: a failure inside a streamed response is graded for what it
// says about the channel, the way the non-streaming committed branch grades
// one. The upstream already ran the request, so nothing is resent either way;
// what differs is the breaker. A capability the converter lacks — or a
// continuation state it refuses to break — is deterministic and belongs to the
// request, so a channel that answered correctly stays selectable. A truncated,
// malformed or interrupted stream is the line itself and counts against it.

// sseEvent renders one upstream SSE event in the shape Anthropic documents.
func sseEvent(event, data string) string { return "event: " + event + "\ndata: " + data + "\n\n" }

// toolUseBlock is a complete Anthropic tool_use block, arguments in the initial
// input object.
func toolUseBlock(index int, id, name string) string {
	return sseEvent("content_block_start", `{"type":"content_block_start","index":`+strconv.Itoa(index)+`,"content_block":{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":{}}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":`+strconv.Itoa(index)+`}`)
}

// signedThinkingBlock is a thinking block carrying a signature: a continuation
// carrier no other protocol can hold.
func signedThinkingBlock(index int, signature string) string {
	return sseEvent("content_block_start", `{"type":"content_block_start","index":`+strconv.Itoa(index)+`,"content_block":{"type":"thinking","thinking":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":`+strconv.Itoa(index)+`,"delta":{"type":"signature_delta","signature":"`+signature+`"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":`+strconv.Itoa(index)+`}`)
}

// anthropicStream wraps content blocks into one complete Anthropic message
// lifecycle, usage included.
func anthropicStream(blocks ...string) string {
	return sseEvent("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"c","content":[],"usage":{"input_tokens":70,"output_tokens":0}}}`) +
		strings.Join(blocks, "") +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
}

// messagesUpstream stands up one Anthropic upstream answering the given SSE
// body, as the only channel of the downstream model, and counts its hits.
func messagesUpstream(t *testing.T, s *store.Store, body string) (*atomic.Int32, *store.Channel) {
	t.Helper()
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	c := store.Channel{Name: "messages", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "test", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	return &hits, &c
}

// The grading itself, as a table: a converter capability or a continuation
// state it refuses to break says nothing about the line, while everything that
// describes a broken stream does.
func TestStreamFailureIsGradedByCapability(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"continuation state", convert.ErrContinuationState, false},
		{"wrapped continuation state", fmt.Errorf("stream: %w", convert.ErrContinuationState), false},
		{"unsupported feature", fmt.Errorf("%w: matched stop sequence", convert.ErrUnsupported), false},
		{"truncated stream", convert.ErrTruncated, true},
		{"malformed payload", convert.ErrMalformed, true},
		{"oversized line", convert.ErrLineLimit, true},
		{"upstream stream error", errors.New("upstream stream error"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamFailureCounts(tc.err); got != tc.want {
				t.Fatalf("streamFailureCounts(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A signed thought after the last tool has no following tool to carry its
// attachment. Strict mode refuses without dropping it or replaying inference.
func TestStreamContinuationRefusalIsNotAChannelFailure(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	hits, c := messagesUpstream(t, s, anthropicStream(toolUseBlock(0, "t1", "f"), signedThinkingBlock(1, "sig")))
	w := invoke(g, "/v1beta/models/test:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if w.Code != 400 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reasoning_not_representable") || strings.Contains(w.Body.String(), "x-to-router-degraded") {
		t.Fatalf("strict refusal must explain the carrier without claiming a drop: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "t1") {
		t.Fatalf("an uncontinuable tool call leaked: %s", w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("a committed stream was resent: %d attempts", hits.Load())
	}
	// A capability question, not a broken line: the channel stays selectable.
	if state := healthPhase(g, c.ID); state != "" {
		t.Fatalf("a conversion refusal must not count against the channel: %q", state)
	}
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != 400 || rows[0].Degraded != "" {
		t.Fatalf("usage = %+v", rows)
	}
}

// The same refusal against a Chat Completions client: the role chunk is already
// on the wire, so the failure rides in an SSE error event on a 200 the gateway
// can no longer change, and the usage row keeps the failure status that event
// reports. The breaker still grades it as a capability, not as the line.
func TestStreamContinuationRefusalKeepsChannelSelectable(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	hits, c := messagesUpstream(t, s, anthropicStream(toolUseBlock(0, "t1", "f"), signedThinkingBlock(1, "sig")))
	w := invoke(g, "/v1/chat/completions", `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "reasoning_not_representable") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("a committed stream was resent: %d attempts", hits.Load())
	}
	if state := healthPhase(g, c.ID); state != "" {
		t.Fatalf("a conversion refusal must not count against the channel: %q", state)
	}
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != 502 {
		t.Fatalf("usage = %+v", rows)
	}
}

// The other capability class: a matched stop sequence Anthropic can carry and
// the Chat Completions schema cannot. The converter refuses instead of dropping
// it, which says nothing about the channel's health.
func TestStreamUnsupportedFeatureIsNotAChannelFailure(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	body := sseEvent("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"c","content":[],"usage":{"input_tokens":70,"output_tokens":0}}}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"END"},"usage":{"output_tokens":20}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
	hits, c := messagesUpstream(t, s, body)
	w := invoke(g, "/v1/chat/completions", `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "conversion_unsupported") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("a committed stream was resent: %d attempts", hits.Load())
	}
	if state := healthPhase(g, c.ID); state != "" {
		t.Fatalf("an unsupported feature must not count against the channel: %q", state)
	}
}

// A truncated stream is the other side of the grading: the line broke mid
// answer, so the breaker counts it against the channel.
func TestStreamTruncatedAfterCommitStillCounts(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	hits, c := messagesUpstream(t, s,
		sseEvent("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"c","content":[],"usage":{"input_tokens":70,"output_tokens":0}}}`))
	w := invoke(g, "/v1/chat/completions", `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("a committed stream was resent: %d attempts", hits.Load())
	}
	if state := healthPhase(g, c.ID); state == "" {
		t.Fatalf("a broken upstream wire must count against the channel: %v", g.Breaker.Snapshot())
	}
}
