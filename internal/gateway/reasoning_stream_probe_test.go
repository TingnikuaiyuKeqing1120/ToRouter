package gateway

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestProbeReasoningFirstEventRejection(t *testing.T) {
	for _, tc := range []struct{ name, format, stream string }{
		{"messages", store.FormatAnthropicMessages, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Invalid signature in thinking block\"}}\n\n"},
		{"chat", store.FormatOpenAIChatCompletions, "data: {\"error\":{\"code\":\"invalid_encrypted_content\"}}\n\n"},
		{"responses failed", store.FormatOpenAIResponses, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":null,\"error\":{\"code\":\"invalid_encrypted_content\"}}}\n\n"},
		{"gemini numeric code", store.FormatGeminiGenerateContent, "event: error\ndata: {\"error\":{\"code\":400,\"status\":\"INVALID_ARGUMENT\",\"message\":\"Invalid thought signature\"}}\n\n"},
		{"comments crlf multiline", store.FormatOpenAIResponses, ": keepalive\r\n\r\n: another\r\nevent: error\r\ndata: {\"error\":\r\ndata: {\"code\":\"invalid_encrypted_content\"}}\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &probeTestBody{Reader: strings.NewReader(tc.stream)}
			replay, rejection, err := probeReasoningRejection(body, tc.format)
			if err != nil || !isReasoningCompatibilityRejection(400, rejection) {
				t.Fatalf("rejection=%s err=%v", rejection, err)
			}
			got, readErr := io.ReadAll(replay)
			if readErr != nil || string(got) != tc.stream {
				t.Fatalf("replay=%q err=%v", got, readErr)
			}
			if err := replay.Close(); err != nil || body.closed != 1 {
				t.Fatalf("close=%v count=%d", err, body.closed)
			}
		})
	}
}

func TestProbeReasoningDoesNotReplayOrdinaryOrActiveErrors(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"message":"bad request"}}`,
		`{"error":{"message":"thinking about an invalid request"}}`,
		`{"error":{"code":"invalid_encrypted_content"},"usage":{"output_tokens":0}}`,
		`{"error":{"code":"invalid_encrypted_content"},"output":[{"type":"function_call","name":"f"}]}`,
		`{"error":{"code":"invalid_encrypted_content"},"content":[{"type":"text","text":"answer"}]}`,
		`{"error":{"code":"invalid_encrypted_content"},"delta":{}}`,
		`{"error":{"code":"invalid_encrypted_content"},"tool_calls":[]}`,
		`{"error":{"code":"invalid_encrypted_content"},"tool_call":{}}`,
		`{"error":{"code":"invalid_encrypted_content"},"message":{"content":"answer"}}`,
		`{"error":{"code":"invalid_encrypted_content"},"response":{"output":[{"type":"reasoning"}]}}`,
		`{"type":"response.failed","response":{"status":"failed","output":[],"usage":{},"error":{"code":"invalid_encrypted_content"}}}`,
		`{"type":"response.failed","response":{"status":"failed","output":[{"type":"message","content":[]}],"error":{"code":"invalid_encrypted_content"}}}`,
		`{"type":"response.failed","response":{"status":"completed","error":{"code":"invalid_encrypted_content"}}}`,
		`{"type":"response.output_text.delta","delta":"answer","error":{"code":"invalid_encrypted_content"}}`,
	} {
		stream := "data: " + payload + "\n\n"
		replay, rejection, err := probeReasoningRejection(io.NopCloser(strings.NewReader(stream)), store.FormatOpenAIResponses)
		if err != nil || len(rejection) != 0 {
			t.Fatalf("active/ordinary error accepted: %s rejection=%s err=%v", payload, rejection, err)
		}
		got, readErr := io.ReadAll(replay)
		if readErr != nil || string(got) != stream {
			t.Fatalf("replay changed: %q %v", got, readErr)
		}
	}
}

func TestProbeReasoningStopsAfterCreated(t *testing.T) {
	first := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"output\":[]}}\n\n"
	second := "event: error\ndata: {\"error\":{\"code\":\"invalid_encrypted_content\"}}\n\n"
	body := &probeTestChunks{chunks: [][]byte{[]byte(first), []byte(second)}}
	replay, rejection, err := probeReasoningRejection(body, store.FormatOpenAIResponses)
	if err != nil || len(rejection) != 0 || body.reads != 1 {
		t.Fatalf("probed beyond created: rejection=%s err=%v reads=%d", rejection, err, body.reads)
	}
	got, readErr := io.ReadAll(replay)
	if readErr != nil || string(got) != first+second {
		t.Fatalf("replay changed: %q %v", got, readErr)
	}
	// Also cover both events arriving in one transport read: no duplicate bytes.
	replay, rejection, err = probeReasoningRejection(io.NopCloser(strings.NewReader(first+second)), store.FormatOpenAIResponses)
	got, readErr = io.ReadAll(replay)
	if err != nil || len(rejection) != 0 || readErr != nil || string(got) != first+second {
		t.Fatalf("read-ahead replay changed: %q %s %v %v", got, rejection, err, readErr)
	}
}

func TestProbeReasoningPrefixLimit(t *testing.T) {
	stream := ": " + strings.Repeat("x", reasoningProbePrefixLimit+30) + "\n\nevent: error\ndata: {\"error\":{\"code\":\"invalid_encrypted_content\"}}\n\n"
	body := &probeTestBody{Reader: strings.NewReader(stream)}
	replay, rejection, err := probeReasoningRejection(body, store.FormatOpenAIResponses)
	if err != nil || len(rejection) != 0 || body.bytesRead > reasoningProbePrefixLimit {
		t.Fatalf("limit exceeded: bytes=%d rejection=%s err=%v", body.bytesRead, rejection, err)
	}
	got, readErr := io.ReadAll(replay)
	if readErr != nil || string(got) != stream {
		t.Fatalf("bounded probe lost bytes: got=%d want=%d err=%v", len(got), len(stream), readErr)
	}
}

func TestProbeReasoningReadErrorsRemainObservable(t *testing.T) {
	sentinel := errors.New("synthetic stream read failure")
	for _, tc := range []struct {
		name string
		data []byte
		err  error
	}{
		{"empty EOF", nil, io.EOF},
		{"truncated EOF", []byte("event: error\ndata: {\"error\":{\"code\":\"invalid_encrypted_content\"}}\n"), io.EOF},
		{"partial failure", []byte("data: partial"), sentinel},
		{"complete event plus read failure", []byte("event: error\ndata: {\"error\":{\"code\":\"invalid_encrypted_content\"}}\n\n"), sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &probeTestChunks{chunks: [][]byte{tc.data}, lastErr: tc.err}
			replay, rejection, err := probeReasoningRejection(body, store.FormatOpenAIResponses)
			if !errors.Is(err, tc.err) || len(rejection) != 0 {
				t.Fatalf("read failure hidden: rejection=%s err=%v", rejection, err)
			}
			got, readErr := io.ReadAll(replay)
			if !bytes.Equal(got, tc.data) {
				t.Fatalf("read failure lost prefix: %q", got)
			}
			if tc.err == io.EOF {
				if readErr != nil {
					t.Fatal(readErr)
				}
			} else if !errors.Is(readErr, tc.err) {
				t.Fatalf("original read error not replayed: %v", readErr)
			}
			if err := replay.Close(); err != nil || body.closed != 1 {
				t.Fatalf("close=%v count=%d", err, body.closed)
			}
		})
	}
}

func TestProbeReasoningUnknownFormatDoesNotRead(t *testing.T) {
	body := &probeTestBody{Reader: strings.NewReader("data: anything\n\n"), closeErr: errors.New("synthetic close failure")}
	replay, rejection, err := probeReasoningRejection(body, "unknown")
	if replay != body || len(rejection) != 0 || err != nil || body.bytesRead != 0 {
		t.Fatal("unknown format was probed")
	}
	if err := replay.Close(); !errors.Is(err, body.closeErr) {
		t.Fatalf("close error hidden: %v", err)
	}
}

func TestProbeReasoningFragmentedCRLFAndCloseError(t *testing.T) {
	stream := ": ping\r\n\r\nevent: error\r\ndata: {\"error\":{\"code\":\"invalid_encrypted_content\"}}\r\n\r\n"
	chunks := make([][]byte, 0, len(stream))
	for i := range stream {
		chunks = append(chunks, []byte{stream[i]})
	}
	body := &probeTestChunks{chunks: chunks}
	replay, rejection, err := probeReasoningRejection(body, store.FormatOpenAIResponses)
	if err != nil || !isReasoningCompatibilityRejection(400, rejection) {
		t.Fatalf("fragmented event lost: %s %v", rejection, err)
	}
	got, readErr := io.ReadAll(replay)
	if readErr != nil || string(got) != stream {
		t.Fatalf("fragmented replay changed: %q %v", got, readErr)
	}
	closeErr := errors.New("synthetic close error")
	closingBody := &probeTestBody{Reader: strings.NewReader("data: {}\n\n"), closeErr: closeErr}
	replay, _, err = probeReasoningRejection(closingBody, store.FormatOpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	if err := replay.Close(); !errors.Is(err, closeErr) || closingBody.closed != 1 {
		t.Fatalf("wrapped close error lost: %v count=%d", err, closingBody.closed)
	}
}

type probeTestBody struct {
	io.Reader
	closed, bytesRead int
	closeErr          error
}

func (b *probeTestBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.bytesRead += n
	return n, err
}
func (b *probeTestBody) Close() error { b.closed++; return b.closeErr }

// The last chunk deliberately returns (n, err) to catch lost buffered errors.
type probeTestChunks struct {
	chunks        [][]byte
	lastErr       error
	reads, closed int
}

func (b *probeTestChunks) Read(p []byte) (int, error) {
	b.reads++
	if len(b.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.chunks[0])
	b.chunks[0] = b.chunks[0][n:]
	if len(b.chunks[0]) == 0 {
		b.chunks = b.chunks[1:]
	}
	if len(b.chunks) == 0 && b.lastErr != nil {
		err := b.lastErr
		b.lastErr = nil
		return n, err
	}
	return n, nil
}
func (b *probeTestChunks) Close() error { b.closed++; return nil }
