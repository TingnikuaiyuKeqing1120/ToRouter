package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	route "torouter/internal/router"
	"torouter/internal/store"
)

func TestAffinityRecordsWithoutIdleExpiration(t *testing.T) {
	a := newAffinity()
	if _, ok := a.lookup("s:x"); ok {
		t.Fatal("empty table must miss")
	}
	a.record("s:x", domain{channel: 7, model: "m", format: store.FormatOpenAIChatCompletions})
	d, ok := a.lookup("s:x")
	if !ok || d.channel != 7 || d.model != "m" || d.format != store.FormatOpenAIChatCompletions {
		t.Fatalf("lookup = %+v %v", d, ok)
	}
	// A blank domain is refused rather than stored.
	a.record("s:blank", domain{})
	if _, ok := a.lookup("s:blank"); ok {
		t.Fatal("blank domain recorded")
	}
	// So is one with no endpoint format: it can never match a request again.
	a.record("s:nofmt", domain{channel: 7, model: "m"})
	if _, ok := a.lookup("s:nofmt"); ok {
		t.Fatal("domain without a format recorded")
	}
	// No idle timer: a later request retains the same continuation domain.
	// Capacity eviction below and generation matching remain separate safeguards.
	if got, ok := a.lookup("s:x"); !ok || got != d {
		t.Fatalf("record lost: %+v %v", got, ok)
	}
}

func TestAffinityEvictsInsteadOfGrowingForever(t *testing.T) {
	a := newAffinity()
	a.max = 10
	for i := 0; i < 40; i++ {
		a.record("s:"+string(rune('a'+i%26))+string(rune('a'+i/26)), domain{channel: int64(i + 1), model: "m", format: store.FormatOpenAIChatCompletions})
	}
	a.mu.Lock()
	n := len(a.entries)
	a.mu.Unlock()
	if n > a.max {
		t.Fatalf("table grew to %d entries, max is %d", n, a.max)
	}
	if n == 0 {
		t.Fatal("table emptied entirely")
	}
}

func TestRestrictToDomainSeparatesModelsOnOneChannel(t *testing.T) {
	chat := store.FormatOpenAIChatCompletions
	// One channel, two upstream models: two continuation domains.
	candidates := []route.Attempt{
		{Channel: store.Channel{ID: 3}, Model: "model-x", Endpoint: store.Endpoint{Format: chat}},
		{Channel: store.Channel{ID: 3}, Model: "model-y", Endpoint: store.Endpoint{Format: chat}},
		{Channel: store.Channel{ID: 4}, Model: "model-x", Endpoint: store.Endpoint{Format: chat}},
	}
	kept := restrictToDomain(candidates, domain{channel: 3, model: "model-x", format: chat})
	if len(kept) != 1 || kept[0].Channel.ID != 3 || kept[0].Model != "model-x" {
		t.Fatalf("domain restriction = %+v", kept)
	}
	if len(restrictToDomain(candidates, domain{channel: 3, model: "model-y", format: chat})) != 1 {
		t.Fatal("second model on the same channel is a different domain")
	}
	if len(restrictToDomain(candidates, domain{channel: 9, model: "model-x", format: chat})) != 0 {
		t.Fatal("an unavailable domain must restrict to nothing")
	}
	// Two bindings to the same channel and model are one domain.
	same := []route.Attempt{
		{Channel: store.Channel{ID: 3}, Model: "model-x", Endpoint: store.Endpoint{Format: chat}},
		{Channel: store.Channel{ID: 3}, Model: "model-x", Endpoint: store.Endpoint{Format: chat}},
	}
	if len(restrictToDomain(same, domain{channel: 3, model: "model-x", format: chat})) != 2 {
		t.Fatal("identical domain bindings must both stay")
	}
	if distinctDomains(same) != 1 {
		t.Fatalf("distinctDomains = %d, want 1", distinctDomains(same))
	}
	if distinctDomains(candidates) != 3 {
		t.Fatalf("distinctDomains = %d, want 3", distinctDomains(candidates))
	}
}

// The endpoint format is the third dimension of the domain: one channel
// exposing two endpoint worlds is two continuation domains.
func TestDistinctDomainsSeparatesEndpointFormatsOnOneChannel(t *testing.T) {
	candidates := []route.Attempt{
		{Channel: store.Channel{ID: 3}, Model: "model-x", Endpoint: store.Endpoint{Format: store.FormatOpenAIResponses}},
		{Channel: store.Channel{ID: 3}, Model: "model-x", Endpoint: store.Endpoint{Format: store.FormatOpenAIChatCompletions}},
	}
	if distinctDomains(candidates) != 2 {
		t.Fatalf("distinctDomains = %d, want 2", distinctDomains(candidates))
	}
}

// The endpoint format is a dimension of the domain, so restriction honours it
// exactly like the channel and the upstream model.
func TestRestrictToDomainSeparatesEndpointFormatsOnOneChannel(t *testing.T) {
	candidates := []route.Attempt{
		{Channel: store.Channel{ID: 3}, Model: "model-x", Endpoint: store.Endpoint{Format: store.FormatOpenAIChatCompletions}},
		{Channel: store.Channel{ID: 3}, Model: "model-x", Endpoint: store.Endpoint{Format: store.FormatAnthropicMessages}},
	}
	kept := restrictToDomain(candidates, domain{channel: 3, model: "model-x", format: store.FormatOpenAIChatCompletions})
	if len(kept) != 1 || kept[0].Endpoint.Format != store.FormatOpenAIChatCompletions {
		t.Fatalf("the other endpoint world of the same channel is a different domain: %+v", kept)
	}
	// With the chat endpoint removed, the pinned domain has no candidate left.
	kept = restrictToDomain(candidates[1:], domain{channel: 3, model: "model-x", format: store.FormatOpenAIChatCompletions})
	if len(kept) != 0 {
		t.Fatalf("a domain whose endpoint format is gone must restrict to nothing: %+v", kept)
	}
}

// The pin key describes the conversation the way the client speaks it, so the
// inbound format is part of the key: one session id spoken as Chat Completions
// and as Responses is two pin keys, and the same carrier value seen in two
// endpoint worlds is two carriers. It is the key, not the stored domain, that
// keeps the worlds apart.
func TestPinKeysAreNamespacedByInboundFormat(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("x-session-id", "same-session")
	chat, kind := sessionKey(req, store.FormatOpenAIChatCompletions, false, nil, 1, "m")
	responses, _ := sessionKey(req, store.FormatOpenAIResponses, true, nil, 1, "m")
	if chat == "" || kind != "session_header" {
		t.Fatalf("session key = %q, kind = %q", chat, kind)
	}
	if chat == responses {
		t.Fatal("the same session id in two inbound formats must not share a pin")
	}
	if again, _ := sessionKey(req, store.FormatOpenAIChatCompletions, false, nil, 1, "m"); again != chat {
		t.Fatalf("same inputs must give the same key: %q vs %q", again, chat)
	}
	fChat := carrierFingerprint(1, "m", store.FormatOpenAIChatCompletions, "carrier-1")
	fResponses := carrierFingerprint(1, "m", store.FormatOpenAIResponses, "carrier-1")
	if fChat == "" || fChat == fResponses {
		t.Fatalf("carrier fingerprints must separate endpoint worlds: %q %q", fChat, fResponses)
	}
	// The client key namespaces the fingerprint the way it namespaces the
	// session key: two clients sending the same carrier get their own pins.
	if other := carrierFingerprint(2, "m", store.FormatOpenAIChatCompletions, "carrier-1"); other == fChat {
		t.Fatal("the same carrier under two client keys must not share a pin")
	}
	if again := carrierFingerprint(1, "m", store.FormatOpenAIChatCompletions, "carrier-1"); again != fChat {
		t.Fatalf("same inputs must give the same fingerprint: %q vs %q", again, fChat)
	}
}

// bind attaches the downstream model every affinity test uses to one more
// channel, keeping the bindings it already has.
func bind(t *testing.T, s *store.Store, channelID int64, priority int) {
	t.Helper()
	m := store.DownstreamModel{ID: "test", Enabled: true, Fallback: true}
	rows, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range rows {
		if existing.ID == m.ID {
			m = existing
			break
		}
	}
	m.Bindings = append(m.Bindings, store.Binding{ChannelID: channelID, Model: "test", Priority: priority, Enabled: true})
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
}

// addBothWorlds creates a channel that exposes both of an upstream's endpoint
// worlds — Responses and Chat Completions — so a session can be pinned in one
// and asked to continue in the other.
func addBothWorlds(t *testing.T, s *store.Store, url string) {
	t.Helper()
	c := store.Channel{Name: "both-worlds", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: url}, {Format: store.FormatOpenAIChatCompletions, BaseURL: url}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, 0)
}

// addMessagesOnly creates a channel whose single endpoint is Anthropic Messages,
// so a chat request can only be served by converting it. The binding priority
// decides the order the channel is tried in: lower first.
func addMessagesOnly(t *testing.T, s *store.Store, url string, priority int) {
	t.Helper()
	c := store.Channel{Name: "messages-only", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: url}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, priority)
}

// bothWorldsServer stands up one fake upstream that answers in whichever
// endpoint world it was addressed, counting hits per world.
func bothWorldsServer(t *testing.T, resp, chat *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			resp.Add(1)
			io.WriteString(w, `{"id":"r","object":"response","status":"completed","output":[]}`)
			return
		}
		chat.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(up.Close)
	return up
}

// messagesServer stands up one fake upstream that only answers the Messages
// endpoint, counting hits.
func messagesServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("a converted request reached %s", r.URL.Path)
		}
		hits.Add(1)
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	t.Cleanup(up.Close)
	return up
}

// sessionTurn posts a chat request that names its session in a header. The
// header is how a client identifies a conversation here: metadata is a chat-body
// field the converter rejects, and this file is largely about converted traffic.
func sessionTurn(g *Gateway, session, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-session-id", session)
	w := httptest.NewRecorder()
	serveTestClient(g, w, req)
	return w
}

// Pin keys namespace inbound formats. A Responses pin cannot force a later
// Chat request onto that endpoint; carried thinking may use the current Chat
// route without proving an origin for server-side history.
func TestResponsesPinDoesNotCarryChatRequest(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var aResp, aChat, bResp, bChat atomic.Int32
	first := bothWorldsServer(t, &aResp, &aChat)
	second := bothWorldsServer(t, &bResp, &bChat)
	addBothWorlds(t, s, first.URL)
	addBothWorlds(t, s, second.URL)
	session := `"metadata":{"user_id":"user_hash_session_t021"}`
	// Turn one: a responses request pins the session in the responses world.
	w := invoke(g, "/v1/responses",
		`{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],`+session+`}`)
	if w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if aResp.Load()+bResp.Load() != 1 {
		t.Fatalf("responses turn hit %d/%d", aResp.Load(), bResp.Load())
	}
	// Turn two: the same session speaks chat and carries state. The
	// Responses-world pin must not force an endpoint. Carried thinking may use
	// the current native Chat route even without an established origin.
	w = invoke(g, "/v1/chat/completions",
		`{"model":"test",`+session+`,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"prev","reasoning_content":"earlier"}]}`)
	if w.Code != 200 {
		t.Fatalf("carried thinking must use an authorized Chat route: %d %s", w.Code, w.Body.String())
	}
	if aChat.Load()+bChat.Load() != 1 || aResp.Load()+bResp.Load() != 1 {
		t.Fatal("the native Chat route must run once without revisiting Responses")
	}
}

// T021: the same rule in the other direction — a session pinned on the chat
// endpoint must not be carried onto the responses endpoint. The pin key
// namespaces by inbound format, so this is a different key, not a filtered one.
func TestChatPinDoesNotCarryResponsesRequest(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var aResp, aChat, bResp, bChat atomic.Int32
	first := bothWorldsServer(t, &aResp, &aChat)
	second := bothWorldsServer(t, &bResp, &bChat)
	addBothWorlds(t, s, first.URL)
	addBothWorlds(t, s, second.URL)
	session := `"metadata":{"user_id":"user_hash_session_t021"}`
	// Turn one: a chat request pins the session in the chat world.
	w := invoke(g, "/v1/chat/completions", `{"model":"test",`+session+`,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if aChat.Load()+bChat.Load() != 1 {
		t.Fatalf("chat turn hit %d/%d", aChat.Load(), bChat.Load())
	}
	// Turn two: a Chat session pin cannot establish ownership of Responses history.
	w = invoke(g, "/v1/responses",
		`{"model":"test","previous_response_id":"resp_from_unknown_origin","input":"continue",`+session+`}`)
	if w.Code != 409 {
		t.Fatalf("expected refusal, got %d %s", w.Code, w.Body.String())
	}
	if aResp.Load()+bResp.Load() != 0 {
		t.Fatal("a pin from another endpoint world was followed")
	}
}

// T021 control: the format dimension separates worlds, it does not weaken the
// pin inside one world. Repeating a chat request on a channel that declares
// both endpoints keeps hitting the chat endpoint, state included.
func TestSameEndpointWorldStillPins(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var aResp, aChat, bResp, bChat atomic.Int32
	first := bothWorldsServer(t, &aResp, &aChat)
	second := bothWorldsServer(t, &bResp, &bChat)
	addBothWorlds(t, s, first.URL)
	addBothWorlds(t, s, second.URL)
	session := `"metadata":{"user_id":"user_hash_session_t021"}`
	w := invoke(g, "/v1/chat/completions", `{"model":"test",`+session+`,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	w = invoke(g, "/v1/chat/completions",
		`{"model":"test",`+session+`,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"prev","reasoning_content":"earlier"}]}`)
	if w.Code != 200 {
		t.Fatalf("stateful turn refused: %d %s", w.Code, w.Body.String())
	}
	// Both turns landed on the same channel's chat endpoint; the responses
	// endpoints were never involved.
	if (aChat.Load() != 2 || bChat.Load() != 0) && (bChat.Load() != 2 || aChat.Load() != 0) {
		t.Fatalf("chat turns split across channels: a=%d b=%d", aChat.Load(), bChat.Load())
	}
	if aResp.Load()+bResp.Load() != 0 {
		t.Fatal("a chat turn reached a responses endpoint")
	}
}

// A converted world pins too. A chat request served by a Messages endpoint puts
// the pin in the Messages endpoint world, and the next turn of the same session
// follows it there: turn one chooses the first eligible binding, turn two
// onward belong to the channel that already serves the conversation.
func TestConvertedWorldStillPins(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var hitsA, hitsB atomic.Int32
	addMessagesOnly(t, s, messagesServer(t, &hitsA).URL, 0)
	body := `{"model":"test","messages":[{"role":"user","content":"hi"}]}`
	if w := sessionTurn(g, "t021-converted", body); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	// A second channel that would win an unpinned pick joins only now, so turn
	// two can stay on the first channel by following the pin and by nothing else.
	addMessagesOnly(t, s, messagesServer(t, &hitsB).URL, -1)
	if w := sessionTurn(g, "t021-converted", body); w.Code != 200 {
		t.Fatalf("turn two failed: %d %s", w.Code, w.Body.String())
	}
	// Both turns landed on the same channel's Messages endpoint; the channel that
	// routing would have picked was never touched.
	if (hitsA.Load() != 2 || hitsB.Load() != 0) && (hitsB.Load() != 2 || hitsA.Load() != 0) {
		t.Fatalf("converted turns split across channels: a=%d b=%d", hitsA.Load(), hitsB.Load())
	}
}

// A pin outranks the endpoint preference. Turn one of a chat session was served
// by the channel's Messages endpoint because that is what the channel
// preferred; the owner then prefers Gemini instead. A preference is not part of
// the continuation domain — the generation stays put, correctly — but the
// conversation is still being served on the Messages endpoint, and that
// endpoint is still declared, so the pinned turn keeps using it instead of
// being refused for a domain that never moved.
func TestPinnedEndpointOutranksPreferredEndpoint(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var messages, gemini atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			messages.Add(1)
			io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
		case strings.Contains(r.URL.Path, "/v1beta/models/"):
			gemini.Add(1)
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer up.Close()
	c := store.Channel{Name: "prefers-messages", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{
			{Format: store.FormatAnthropicMessages, BaseURL: up.URL},
			{Format: store.FormatGeminiGenerateContent, BaseURL: up.URL},
		},
		PreferredEndpoint: store.FormatAnthropicMessages}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, 0)
	// Turn one: no endpoint matches a chat request exactly, so the preference
	// decides — Messages — and the pin is recorded there.
	if w := sessionTurn(g, "t021-preference", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if messages.Load() != 1 || gemini.Load() != 0 {
		t.Fatalf("turn one served from the wrong endpoint: messages=%d gemini=%d", messages.Load(), gemini.Load())
	}
	// The owner prefers Gemini from now on. What a channel prefers is not part
	// of the continuation domain, so the generation must not move.
	c.PreferredEndpoint = store.FormatGeminiGenerateContent
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("changing the endpoint preference bumped the generation to %d", c.Generation)
	}
	// A second channel that would win an unpinned pick joins only now, so turn
	// two can stay on the pinned endpoint by following the pin and by nothing
	// else.
	var backup atomic.Int32
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backup.Add(1)
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer spare.Close()
	other := store.Channel{Name: "would-win", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatGeminiGenerateContent, BaseURL: spare.URL}}}
	if err := s.SaveChannel(&other); err != nil {
		t.Fatal(err)
	}
	bind(t, s, other.ID, -1)
	// Turn two: the same session, pinned on the Messages endpoint.
	w := sessionTurn(g, "t021-preference",
		`{"model":"test","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"prev"}]}`)
	if w.Code != 200 {
		t.Fatalf("turn two failed: %d %s", w.Code, w.Body.String())
	}
	if messages.Load() != 2 || gemini.Load() != 0 {
		t.Fatalf("the pinned turn left its endpoint: messages=%d gemini=%d", messages.Load(), gemini.Load())
	}
	if backup.Load() != 0 {
		t.Fatalf("the pinned turn reached another channel: %d", backup.Load())
	}
}

// The control: a preference change is not a domain change, but dropping the
// pinned endpoint is. The declaration set moved, the generation moved with it,
// and the pin's domain has no candidate left — the stateful follow-up is still
// refused as a domain that moved on rather than being converted onto another
// endpoint. (A stateless turn carries no state to lose and routes normally
// under the soft pin; this one depends on a Responses server-side history.)
func TestPinnedEndpointGoneStillReportsDomainChanged(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var responses, gemini atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses"):
			responses.Add(1)
			io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
		case strings.Contains(r.URL.Path, "/v1beta/models/"):
			gemini.Add(1)
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer up.Close()
	c := store.Channel{Name: "prefers-responses", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIResponses, BaseURL: up.URL},
			{Format: store.FormatGeminiGenerateContent, BaseURL: up.URL},
		}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, 0)
	if w := historyTurn(g, "t021-gone", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	// The operator drops the Responses endpoint; only Gemini is left. That is a
	// changed declaration set, so the channel keeps its id and loses the domain.
	c.Endpoints = []store.Endpoint{{Format: store.FormatGeminiGenerateContent, BaseURL: up.URL}}
	c.PreferredEndpoint = ""
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 2 {
		t.Fatalf("dropping an endpoint must bump the generation, got %d", c.Generation)
	}
	w := historyTurn(g, "t021-gone",
		`{"model":"test","previous_response_id":"resp_native","input":"continue"}`)
	if w.Code != 503 {
		t.Fatalf("pinned endpoint gone, want 503, got %d %s", w.Code, w.Body.String())
	}
	if responses.Load() != 1 || gemini.Load() != 0 {
		t.Fatalf("a request whose pinned domain is gone still reached an upstream: responses=%d gemini=%d", responses.Load(), gemini.Load())
	}
	var err struct {
		Error struct {
			Recovery struct {
				Code string `json:"code"`
			} `json:"recovery"`
		} `json:"error"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &err); e != nil {
		t.Fatal(e)
	}
	if err.Error.Recovery.Code != "state_domain_changed" {
		t.Fatalf("recovery = %+v", err.Error.Recovery)
	}
}

// A pin recorded on a Responses endpoint stops being servable when the
// channel loses that endpoint. The remaining Chat endpoint is a different
// continuation domain, so the pinned request is refused instead of being
// converted onto an endpoint its state never belonged to.
func TestPinnedEndpointFormatIsPartOfTheDomain(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 1)
	var respHits, chatHits atomic.Int32
	up := bothWorldsServer(t, &respHits, &chatHits)
	c := store.Channel{Name: "responses-then-chat", APIKey: "k", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bind(t, s, c.ID, 0)
	// Turn one: Responses inbound served by the Responses endpoint, pin recorded there.
	if w := historyTurn(g, "t021-domain", `{"model":"test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("turn one failed: %d %s", w.Code, w.Body.String())
	}
	if chatHits.Load() != 0 || respHits.Load() != 1 {
		t.Fatalf("turn one served from the wrong endpoint: chat=%d responses=%d", chatHits.Load(), respHits.Load())
	}
	// The operator drops the Responses endpoint; only Chat is left. That is a
	// changed endpoint set: the channel keeps its id and loses the domain.
	c.Endpoints = []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	// Turn two: the same session, now carrying state. The pin still resolves, but
	// the endpoint world it names is gone.
	w := historyTurn(g, "t021-domain",
		`{"model":"test","previous_response_id":"r","input":"continue"}`)
	if w.Code != 503 {
		t.Fatalf("pinned endpoint format gone, want 503, got %d %s", w.Code, w.Body.String())
	}
	if chatHits.Load() != 0 || respHits.Load() != 1 {
		t.Fatalf("a request whose pinned domain is gone still reached an upstream: chat=%d responses=%d", chatHits.Load(), respHits.Load())
	}
	var err struct {
		Error struct {
			Message  string `json:"message"`
			Recovery struct {
				Code    string   `json:"code"`
				Actions []string `json:"actions"`
			} `json:"recovery"`
		} `json:"error"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &err); e != nil {
		t.Fatal(e)
	}
	// The channel is still a candidate through its remaining endpoint, so this
	// is a domain that moved on, not a channel that may recover: re-declaring
	// the Responses endpoint would bump the generation again.
	if err.Error.Recovery.Code != "state_domain_changed" {
		t.Fatalf("recovery = %+v", err.Error.Recovery)
	}
	if !strings.Contains(err.Error.Message, "reconfigured") {
		t.Fatalf("message = %q", err.Error.Message)
	}
	if joined := strings.Join(err.Error.Recovery.Actions, ","); strings.Contains(joined, "retry_later") {
		t.Fatalf("a moved-on domain must not be polled: %s", joined)
	}
}
