package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/store"
)

// T018 follow-up: once the upstream has answered 2xx the request is committed —
// it was accepted and run there. Whatever fails afterwards — a truncated or
// oversized body, an answer that cannot be parsed or converted — may not be
// resent, here or on another channel, because repeating it would repeat the
// inference, the cost and any side effect. The breaker still grades the failure
// for what it says about the channel: a broken wire counts against it, a
// capability mismatch does not.

// truncatingUpstream answers 2xx and drops the connection mid-body, which is
// what a dead line looks like from the client side.
func truncatingUpstream(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server cannot hijack")
			return
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 400\r\n\r\n{\"choices\":[{\"message\":")
		buf.Flush()
	}))
	t.Cleanup(up.Close)
	return up
}

// backupServer is the second candidate: any hit on it means the committed
// request was failed over.
func backupServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(up.Close)
	return up
}

func TestCommittedTruncatedBodyIsNotResent(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 3) // retries and failover both available
	var broken, backup atomic.Int32
	up := truncatingUpstream(t, &broken)
	other := backupServer(t, &backup)
	first := store.Channel{Name: "broken-wire", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}}
	if err := s.SaveChannel(&first); err != nil {
		t.Fatal(err)
	}
	second := store.Channel{Name: "backup", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: other.URL + "/v1"}}}
	if err := s.SaveChannel(&second); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{
		{ChannelID: first.ID, Model: "test", Priority: 0, Enabled: true},
		{ChannelID: second.ID, Model: "test", Priority: 1, Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if broken.Load() != 1 {
		t.Fatalf("a committed request was resent: %d attempts on the broken channel", broken.Load())
	}
	if backup.Load() != 0 {
		t.Fatalf("a committed request failed over: %d attempts on the backup channel", backup.Load())
	}
	if !strings.Contains(w.Body.String(), "did not retry") {
		t.Fatalf("unexplained failure: %s", w.Body.String())
	}
	if code, _ := recoveryOf(t, w.Body.Bytes()); code != "upstream_may_have_processed" {
		t.Fatalf("recovery code = %q", code)
	}
	// The wire is what broke, so the breaker counts it against the channel.
	if state := healthPhase(g, first.ID); state == "" {
		t.Fatalf("a broken upstream wire must count against the channel: %v", g.Breaker.Snapshot())
	}
}

func TestCommittedUnconvertibleAnswerIsNotResent(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 3)
	var answered, backup atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answered.Add(1)
		// A matched stop sequence: Anthropic can carry it, the Chat
		// Completions schema the client speaks cannot, and the converter
		// refuses rather than dropping the information.
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"ok"}],"stop_reason":"stop_sequence","stop_sequence":"END","usage":{"input_tokens":1,"output_tokens":2}}`)
	}))
	defer up.Close()
	other := backupServer(t, &backup)
	first := store.Channel{Name: "messages-only", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: up.URL}}}
	if err := s.SaveChannel(&first); err != nil {
		t.Fatal(err)
	}
	second := store.Channel{Name: "backup", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: other.URL + "/v1"}}}
	if err := s.SaveChannel(&second); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{
		{ChannelID: first.ID, Model: "test", Priority: 0, Enabled: true},
		{ChannelID: second.ID, Model: "test", Priority: 1, Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if answered.Load() != 1 {
		t.Fatalf("a committed answer was resent: %d attempts on the answering channel", answered.Load())
	}
	if backup.Load() != 0 {
		t.Fatalf("a committed request failed over: %d attempts on the backup channel", backup.Load())
	}
	if !strings.Contains(w.Body.String(), "did not retry") {
		t.Fatalf("unexplained failure: %s", w.Body.String())
	}
	// The refusal is deterministic, not ambiguous: the same request resent
	// fails the same way, so the recovery block offers the ways forward that
	// actually change the outcome and never "wait it out".
	code, actions := recoveryOf(t, w.Body.Bytes())
	if code != "conversion_refused" {
		t.Fatalf("recovery code = %q", code)
	}
	joined := strings.Join(actions, ",")
	for _, want := range []string{"handoff", "start_fresh_session", "use_different_format"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing action %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, "retry_later") {
		t.Fatalf("a deterministic conversion refusal must not advise retrying: %s", joined)
	}
	// Converting the answer is a capability question, not a broken channel:
	// the breaker is released without counting a failure, so a channel that
	// answers correctly stays selectable.
	if state := healthPhase(g, first.ID); state != "" {
		t.Fatalf("a capability mismatch must not count against the channel: %q", state)
	}
}

// The continuation-state refusal is the same commitment: the answer arrived, it
// just cannot be continued in the client's format. Asking another channel would
// pay for a second inference to learn the same thing.
func TestCommittedContinuationStateDoesNotFailOver(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 3)
	var refused, backup atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refused.Add(1)
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"c","content":[`+
			`{"type":"thinking","thinking":"secret","signature":"sig"},`+
			`{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`)
	}))
	defer up.Close()
	other := backupServer(t, &backup)
	first := store.Channel{Name: "messages-only", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: up.URL}}}
	if err := s.SaveChannel(&first); err != nil {
		t.Fatal(err)
	}
	second := store.Channel{Name: "backup", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: other.URL + "/v1"}}}
	if err := s.SaveChannel(&second); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{
		{ChannelID: first.ID, Model: "test", Priority: 0, Enabled: true},
		{ChannelID: second.ID, Model: "test", Priority: 1, Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 400 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if refused.Load() != 1 || backup.Load() != 0 {
		t.Fatalf("a committed response must not be retried or failed over: refused %d backup %d", refused.Load(), backup.Load())
	}
	code, actions := recoveryOf(t, w.Body.Bytes())
	if code != "conversion_refused" {
		t.Fatalf("recovery code = %q", code)
	}
	joined := strings.Join(actions, ",")
	for _, want := range []string{"handoff", "start_fresh_session", "use_different_format"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing action %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, "retry_later") {
		t.Fatalf("a deterministic conversion refusal must not advise retrying: %s", joined)
	}
	// The refusal is about the state the answer carries, not about the channel's
	// health: a channel that answered correctly stays selectable.
	if state := healthPhase(g, first.ID); state != "" {
		t.Fatalf("a continuation-state refusal must not count against the channel: %q", state)
	}
}

// The streaming side of the same invariant: an upstream that answered 2xx to a
// stream request has already run it. A body that is not SSE is a broken line,
// not a reason to spend the inference again — the breaker counts it against the
// channel, and neither a resend nor a failover happens.
func TestCommittedStreamWrongContentTypeIsNotResent(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 3) // retries and failover both available
	var answered, backup atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answered.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer up.Close()
	other := backupServer(t, &backup)
	first := store.Channel{Name: "json-for-stream", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}}
	if err := s.SaveChannel(&first); err != nil {
		t.Fatal(err)
	}
	second := store.Channel{Name: "backup", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: other.URL + "/v1"}}}
	if err := s.SaveChannel(&second); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{
		{ChannelID: first.ID, Model: "test", Priority: 0, Enabled: true},
		{ChannelID: second.ID, Model: "test", Priority: 1, Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","stream":true,"messages":[]}`)
	if w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if answered.Load() != 1 {
		t.Fatalf("a committed stream was resent: %d attempts on the answering channel", answered.Load())
	}
	if backup.Load() != 0 {
		t.Fatalf("a committed stream failed over: %d attempts on the backup channel", backup.Load())
	}
	if !strings.Contains(w.Body.String(), "did not retry") {
		t.Fatalf("unexplained failure: %s", w.Body.String())
	}
	if code, _ := recoveryOf(t, w.Body.Bytes()); code != "upstream_may_have_processed" {
		t.Fatalf("recovery code = %q", code)
	}
	// Exactly one attempt was made, so exactly one usage row was written.
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != 502 {
		t.Fatalf("usage = %+v", rows)
	}
	// The line is what broke, so the breaker counts it against the channel.
	if state := healthPhase(g, first.ID); state == "" {
		t.Fatalf("a broken upstream wire must count against the channel: %v", g.Breaker.Snapshot())
	}
}

// A 2xx that is not even JSON — an upstream error page wearing a 200 — is the
// channel's wire, not a conversion question: the answer arrived and was paid
// for, so it is neither resent nor failed over, and the breaker counts it.
func TestCommittedInvalidJSONIsNotResent(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 3)
	var answered, backup atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answered.Add(1)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><body>upstream error page</body></html>`)
	}))
	defer up.Close()
	other := backupServer(t, &backup)
	first := store.Channel{Name: "html-error-page", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}}
	if err := s.SaveChannel(&first); err != nil {
		t.Fatal(err)
	}
	second := store.Channel{Name: "backup", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: other.URL + "/v1"}}}
	if err := s.SaveChannel(&second); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{
		{ChannelID: first.ID, Model: "test", Priority: 0, Enabled: true},
		{ChannelID: second.ID, Model: "test", Priority: 1, Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if answered.Load() != 1 {
		t.Fatalf("a committed answer was resent: %d attempts on the answering channel", answered.Load())
	}
	if backup.Load() != 0 {
		t.Fatalf("a committed request failed over: %d attempts on the backup channel", backup.Load())
	}
	if !strings.Contains(w.Body.String(), "did not retry") {
		t.Fatalf("unexplained failure: %s", w.Body.String())
	}
	if code, _ := recoveryOf(t, w.Body.Bytes()); code != "upstream_may_have_processed" {
		t.Fatalf("recovery code = %q", code)
	}
	// The wire is what broke, so the breaker counts it against the channel.
	if state := healthPhase(g, first.ID); state == "" {
		t.Fatalf("a broken upstream wire must count against the channel: %v", g.Breaker.Snapshot())
	}
}
