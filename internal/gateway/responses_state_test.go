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

// These server handles and replayable encrypted items are native continuation
// state. Neither an unknown owner nor an unavailable hard pin permits guessing
// another continuation domain.
var responsesStateFixtures = []struct {
	name, fields, kind, fingerprint string
}{
	{"conversation string", `"conversation":"conv_native","input":"continue"`, "openai.conversation", ""},
	{"conversation object", `"conversation":{"id":"conv_native"},"input":"continue"`, "openai.conversation", ""},
	{"item reference", `"input":[{"type":"item_reference","id":"rs_native"}]`, "openai.item_reference", ""},
	{"id only reference", `"input":[{"id":"rs_native"}]`, "openai.item_reference", ""},
	{"compaction", `"input":[{"type":"compaction","encrypted_content":"opaque-compaction"}]`, "openai.encrypted_compaction", "opaque-compaction"},
}

func addResponsesChannel(t *testing.T, s *store.Store, handler http.HandlerFunc, priority int) (int64, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	c := store.Channel{Name: "responses", APIKey: "synthetic", Models: []string{"test"}, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := store.DownstreamModel{ID: "test", Enabled: true, Fallback: true}
	rows, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range rows {
		if existing.ID == m.ID {
			m = existing
		}
	}
	m.Bindings = append(m.Bindings, store.Binding{ChannelID: c.ID, Model: "test", Priority: priority, Enabled: true})
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	return c.ID, &hits
}

func completedResponses(w http.ResponseWriter, _ *http.Request) {
	io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","output":[]}`)
}

func responsesSessionTurn(g *Gateway, session, body string) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("x-session-id", session)
	w := httptest.NewRecorder()
	serveTestClient(g, w, r)
	return w, r
}

func TestResponsesNativeStateRequiresKnownDomain(t *testing.T) {
	for _, fixture := range responsesStateFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 1)
			_, a := addResponsesChannel(t, s, completedResponses, 0)
			_, b := addResponsesChannel(t, s, completedResponses, 1)
			body := `{"model":"test",` + fixture.fields + `}`
			w := invoke(g, "/v1/responses", body)
			if w.Code != 409 || a.Load() != 0 || b.Load() != 0 {
				t.Fatalf("unknown owner must send nothing: HTTP=%d hits=%d/%d body=%s", w.Code, a.Load(), b.Load(), w.Body.String())
			}
			if code, _ := recoveryOf(t, w.Body.Bytes()); code != "state_affinity_unknown" {
				t.Fatalf("recovery = %q", code)
			}
		})
	}
}

func TestResponsesNativeStateHardPinNeverFallsBack(t *testing.T) {
	for _, fixture := range responsesStateFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 1)
			id, a := addResponsesChannel(t, s, completedResponses, 0)
			_, b := addResponsesChannel(t, s, completedResponses, 1)
			if w, _ := responsesSessionTurn(g, "hard-native", `{"model":"test","input":"hello"}`); w.Code != 200 {
				t.Fatalf("first turn: %d %s", w.Code, w.Body.String())
			}
			for i := 0; i < 1; i++ {
				lease, _ := g.Breaker.Acquire(testCombination(t, g, id))
				g.Breaker.Failure(lease)
			}
			w, _ := responsesSessionTurn(g, "hard-native", `{"model":"test",`+fixture.fields+`}`)
			if w.Code != 503 || a.Load() != 1 || b.Load() != 0 {
				t.Fatalf("hard pin must stay on the cooling domain: HTTP=%d hits=%d/%d body=%s", w.Code, a.Load(), b.Load(), w.Body.String())
			}
			if code, _ := recoveryOf(t, w.Body.Bytes()); code != "state_affinity_unavailable" {
				t.Fatalf("recovery = %q", code)
			}
		})
	}
}

func TestResponsesNativeStateSingleDomainPreservesBody(t *testing.T) {
	for _, fixture := range responsesStateFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			g, s := setup(t)
			// This test verifies byte-for-byte passthrough with defaults disabled.
			settings, err := s.GetSettings()
			if err != nil {
				t.Fatal(err)
			}
			settings.RequestDefaults = store.ModelDefaults{MaxOutputTokens: defaultBudget(0)}
			if err := s.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			body := `{"model":"test",` + fixture.fields + `}`
			_, hits := addResponsesChannel(t, s, func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != body || r.URL.Path != "/v1/responses" {
					t.Errorf("native body/path changed: %s %q %v", r.URL.Path, raw, err)
				}
				completedResponses(w, r)
			}, 0)
			w := invoke(g, "/v1/responses", body)
			if w.Code != 200 || hits.Load() != 1 {
				t.Fatalf("explicit single domain must pass: %d %s hits=%d", w.Code, w.Body.String(), hits.Load())
			}
			rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
			if err != nil || len(rows) != 1 || !rows[0].Stateful {
				t.Fatalf("stateful usage=%+v error=%v", rows, err)
			}
			state := detectState("openai", true, []byte(body))
			if len(state.kinds) != 1 || state.kinds[0] != fixture.kind || state.oldest != fixture.fingerprint {
				t.Fatalf("carrier=%+v want=%s/%s", state, fixture.kind, fixture.fingerprint)
			}
		})
	}
}
