package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/store"
)

func TestConversionUnknownExecutionDiagnosticMatchesClientAndLog(t *testing.T) {
	for _, status := range []int{408, 500, 502, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			g, s := setup(t)
			settings(t, s, 3, 3)
			var hits atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"untrusted-raw-fixture"}}`)
			}))
			defer up.Close()
			add(t, s, up.URL, "openai", 0)
			w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hello"}]}`)
			if hits.Load() != 1 {
				t.Fatal("unknown execution was replayed")
			}
			var envelope struct {
				Error struct {
					Diagnostic ConversionDiagnostic `json:"diagnostic"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			d := envelope.Error.Diagnostic
			if d.Code != "execution_unconfirmed" || d.RequestID == "" || d.RequestID != w.Header().Get(requestIDHeader) || !strings.Contains(d.Message, "没有自动重发") {
				t.Fatalf("incorrect client diagnostic: %+v", d)
			}
			rows, err := s.ListUsage(store.UsageFilter{RequestID: d.RequestID})
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows=%d err=%v", len(rows), err)
			}
			var logged ConversionDiagnostic
			if err := json.Unmarshal(rows[0].Diagnostic, &logged); err != nil {
				t.Fatal(err)
			}
			if logged.Code != d.Code || logged.RequestID != d.RequestID || !strings.Contains(logged.Message, "没有自动重发") || strings.Contains(string(rows[0].Diagnostic), "untrusted-raw-fixture") {
				t.Fatalf("log diagnostic disagrees: %+v", logged)
			}
		})
	}
}

func TestConversionHistoryKeepsOriginalCredentialEvenUnderRoundRobin(t *testing.T) {
	g, s := setup(t)
	settings(t, s, 3, 2)
	var auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		io.WriteString(w, `{"id":"resp_fixture","object":"response","status":"completed","output":[]}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "fixture", Enabled: true, Models: []string{"test"}, DispatchMode: store.DispatchModeRoundRobin,
		Endpoints:   []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL}},
		Credentials: []store.CredentialItem{{ID: "one", Label: "one", APIKey: "fixture-one", Enabled: true}, {ID: "two", Label: "two", APIKey: "fixture-two", Enabled: true}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "test", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "test", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	call := func(body, session string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		if session != "" {
			r.Header.Set("x-session-id", session)
		}
		w := httptest.NewRecorder()
		serveTestClient(g, w, r)
		return w
	}
	if w := call(`{"model":"test","input":"hello"}`, "fixture-session"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call(`{"model":"test","previous_response_id":"resp_fixture","input":"continue"}`, "fixture-session"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(auths) != 2 || auths[0] != auths[1] {
		t.Fatalf("history moved accounts: %v", auths)
	}
	// A process restart cannot guess the originating account from the latest
	// generic successful path, even though only one channel is configured.
	fresh := New(s)
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"test","previous_response_id":"resp_fixture","input":"continue"}`))
	w := httptest.NewRecorder()
	serveTestClient(fresh, w, r)
	if w.Code != 409 || len(auths) != 2 || !strings.Contains(w.Body.String(), "context_unavailable") {
		t.Fatal("unknown history account was guessed", w.Code, w.Body.String())
	}
}
