package devin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

func syntheticOAuth() store.OAuthCredential {
	return store.OAuthCredential{Provider: "devin", Account: "synthetic-account", Subject: "synthetic-account", AccessToken: "synthetic-session"}
}
func readFields(t *testing.T, data []byte) []field {
	t.Helper()
	v, err := fields(data)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func value(t *testing.T, data []byte, n int) field {
	t.Helper()
	for _, f := range readFields(t, data) {
		if f.number == n {
			return f
		}
	}
	t.Fatalf("missing protobuf field %d", n)
	return field{}
}
func terminal() []byte {
	raw := frame([]byte(`{}`))
	raw[0] = 2
	return raw
}
func upstream(t *testing.T, wire []byte) (*Driver, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != chatPath || r.Header.Get("Authorization") != "Basic synthetic-session" ||
			r.Header.Get("Content-Type") != "application/connect+proto" || r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Error("wrong private RPC path or native authentication headers")
		}
		if _, _, err := readFrame(r.Body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.Write(wire)
	}))
	t.Cleanup(server.Close)
	return NewDriver(server.Client(), Endpoints{ServerURL: server.URL}), calls
}
func execute(t *testing.T, d *Driver, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest("POST", "https://example.invalid/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return d.Execute(nil, req, syntheticOAuth())
}
func TestIndependentAuthorizationAndExactModelCatalogue(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Error("missing Connect version")
		}
		switch r.URL.Path {
		case exchangePath:
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			if v["code"] != "synthetic-code" || v["code_verifier"] == "" || !strings.HasSuffix(v["redirect_uri"], "/callback") {
				t.Error("missing exchange material")
			}
			io.WriteString(w, `{"sessionToken":"synthetic-session"}`)
		case statusPath:
			data, _ := io.ReadAll(r.Body)
			if string(value(t, value(t, data, 1).data, 3).data) != "synthetic-session" {
				t.Error("status used wrong material")
			}
			w.Write(message{}.bytes(1, message{}.text(7, "private@example.invalid").text(36, "synthetic-account")))
		case modelsPath:
			w.Write(message{}.bytes(1, message{}.text(22, "model-exact-high")).bytes(1, message{}.text(22, "model-exact")).bytes(1, message{}.text(22, "model-exact")))
		default:
			t.Error("unexpected request")
		}
	}))
	defer server.Close()
	d := NewDriver(server.Client(), Endpoints{ServerURL: server.URL})
	auth, err := d.Begin(context.Background(), "http://127.0.0.1:32123/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth.URL)
	if u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("state") != auth.State || u.Query().Get("cli_pkce_marker") != "1" {
		t.Fatal("missing independent PKCE")
	}
	if _, err = d.Exchange(context.Background(), auth, oauthbase.CallbackResult{Code: "synthetic-code", State: "wrong"}); err == nil || calls.Load() != 0 {
		t.Fatal("bad state sent to vendor")
	}
	oauth, err := d.Exchange(context.Background(), auth, oauthbase.CallbackResult{Code: "synthetic-code", State: auth.State})
	if err != nil {
		t.Fatal(err)
	}
	if oauth.Account != "synthetic-account" || oauth.Subject != oauth.Account || oauth.RefreshToken != "" || !oauth.ExpiresAt.IsZero() || strings.Contains(oauth.AccountLabel, "@") {
		t.Fatalf("invented identity/renewal: %#v", oauth)
	}
	models, err := d.Models(context.Background(), oauth)
	if err != nil || len(models) != 2 || models[0].ID != "model-exact" || models[1].ID != "model-exact-high" || models[0].Format != store.FormatOpenAIResponses {
		t.Fatalf("model catalogue: %+v %v", models, err)
	}
}
func TestRequestPreservesContextAndStableSession(t *testing.T) {
	signature := signatureState{Data: []byte{0, 255, 1, 2}, Type: "vendor-signed"}
	body := jsonObject{
		"model": "model-exact", "store": false, "temperature": 0, "top_p": 0,
		"instructions": "Keep the caller's identity and instructions.",
		"input": []any{
			jsonObject{"role": "user", "content": "question"},
			jsonObject{"type": "reasoning", "summary": []any{jsonObject{"type": "summary_text", "text": "visible thought"}}, "encrypted_content": encodeSignature(signature)},
			jsonObject{"type": "function_call", "call_id": outID("tool/id:1"), "name": "original_name", "arguments": `{"x":1}`},
			jsonObject{"type": "function_call_output", "call_id": outID("tool/id:1"), "output": "actual output"},
		},
		"tools": []any{jsonObject{"type": "function", "name": "original_name", "description": "original description", "parameters": jsonObject{"type": "object", "properties": jsonObject{"x": jsonObject{"type": "integer"}}}}},
	}
	raw, _ := json.Marshal(body)
	headers := http.Header{"X-Session-Id": []string{"synthetic-session-id"}}
	parsed, err := parseRequest(raw, headers, syntheticOAuth())
	if err != nil {
		t.Fatal(err)
	}
	_, wire, err := readFrame(bytes.NewReader(parsed.Wire))
	if err != nil {
		t.Fatal(err)
	}
	if string(value(t, wire, 2).data) != body["instructions"] || string(value(t, value(t, wire, 10).data, 2).data) != "original description" {
		t.Fatal("rewrote system or tool instructions")
	}
	var prompts [][]byte
	for _, f := range readFields(t, wire) {
		if f.number == 3 {
			prompts = append(prompts, f.data)
		}
	}
	if len(prompts) != 3 || string(value(t, prompts[1], 11).data) != "visible thought" || !bytes.Equal(value(t, prompts[1], 12).data, signature.Data) || string(value(t, prompts[1], 18).data) != signature.Type {
		t.Fatal("lost reasoning state")
	}
	if string(value(t, value(t, prompts[1], 6).data, 1).data) != "tool/id:1" || string(value(t, prompts[2], 7).data) != "tool/id:1" || string(value(t, prompts[2], 3).data) != "actual output" {
		t.Fatal("lost paired tool identity")
	}
	options := value(t, wire, 8).data
	for _, n := range []int{5, 8} {
		if math.Float64frombits(binary.LittleEndian.Uint64(value(t, options, n).data)) != 0 {
			t.Fatal("zero sampling parameter changed")
		}
	}
	repeat, _ := parseRequest(raw, headers, syntheticOAuth())
	if !bytes.Equal(parsed.Wire, repeat.Wire) {
		t.Fatal("randomized stable context prefix")
	}
	other := syntheticOAuth()
	other.Account = "another-account"
	different, _ := parseRequest(raw, headers, other)
	_, second, _ := readFrame(bytes.NewReader(different.Wire))
	if bytes.Equal(value(t, wire, 16).data, value(t, second, 16).data) {
		t.Fatal("session crossed account boundary")
	}
}
func TestUnsupportedRequestsNeverReachVendor(t *testing.T) {
	d, calls := upstream(t, terminal())
	for _, body := range []string{
		`{"model":"m","input":"x","previous_response_id":"r"}`,
		`{"model":"m","input":"x","store":true}`,
		`{"model":"m","input":"x","text":{"format":{"type":"json_object"}}}`,
		`{"model":"m","input":"x","text":{"format":{"type":"text","unsupported":true}}}`,
		`{"model":"m","input":"x","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":true}]}`,
		`{"model":"m","input":"x","tool_choice":"required"}`,
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`,
		`{"model":"m","input":[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"}]}`,
		`{"model":"m","input":[{"type":"function_call_output","call_id":"a","output":"x"}]}`,
	} {
		if _, err := execute(t, d, body); !errors.Is(err, nativeoauth.ErrUnsupportedRequest) {
			t.Fatalf("not a local refusal: %s: %v", body, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unsupported request reached vendor")
	}
}
func successfulWire() []byte {
	var wire []byte
	add := func(m message) { wire = append(wire, frame(m)...) }
	// A UTF-8 code point straddles frames. Never replace its bytes with U+FFFD.
	add(message{}.bytes(9, []byte{0xe6}))
	add(message{}.bytes(9, []byte{0x83, 0xb3}).bytes(10, []byte{0, 255}).text(21, "signed"))
	add(message{}.text(3, "answer"))
	add(message{}.bytes(6, message{}.text(1, "tool/id:1").text(2, "f").text(3, `{"x":`)))
	add(message{}.bytes(6, message{}.text(3, `1}`)).bytes(10, []byte{3, 4}))
	add(message{}.integer(5, 10).bytes(7, message{}.integer(2, 10).integer(3, 4).integer(4, 2).integer(5, 6)))
	return append(wire, terminal()...)
}
func TestStreamAndJSONPreserveToolsReasoningUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			d, calls := upstream(t, successfulWire())
			body := `{"model":"m","input":"x","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
			resp, err := execute(t, d, body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			var final jsonObject
			if stream {
				sequence := 0
				completed := 0
				for _, line := range strings.Split(string(raw), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var e jsonObject
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) != nil {
						t.Fatal("invalid SSE JSON")
					}
					if int(e["sequence_number"].(float64)) != sequence {
						t.Fatal("unordered events")
					}
					sequence++
					if e["type"] == "response.completed" {
						completed++
						final = e["response"].(map[string]any)
					}
				}
				if completed != 1 || !strings.Contains(string(raw), "response.function_call_arguments.delta") || !strings.Contains(string(raw), "response.reasoning_summary_text.delta") {
					t.Fatal("missing real streamed events")
				}
			} else if json.Unmarshal(raw, &final) != nil {
				t.Fatal("invalid JSON")
			}
			output := final["output"].([]any)
			if len(output) != 3 || final["status"] != "completed" {
				t.Fatalf("bad output: %s", raw)
			}
			thought := output[0].(map[string]any)
			state, err := decodeSignature(thought["encrypted_content"].(string))
			if err != nil || !bytes.Equal(state.Data, []byte{0, 255, 3, 4}) || state.Type != "signed" || thought["summary"].([]any)[0].(map[string]any)["text"] != "想" {
				t.Fatal("thought state damaged")
			}
			call := output[2].(map[string]any)
			if call["name"] != "f" || inID(call["call_id"].(string)) != "tool/id:1" || call["arguments"] != `{"x":1}` {
				t.Fatal("tool roundtrip damaged")
			}
			usage := final["usage"].(map[string]any)
			if usage["input_tokens"] != float64(18) || usage["output_tokens"] != float64(4) || usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(6) {
				t.Fatal("usage invented or miscounted")
			}
			if calls.Load() != 1 {
				t.Fatal("inference retried")
			}
		})
	}
}
func TestTerminalsTruncationAndNoUsageEstimate(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for name, wire := range map[string][]byte{
			"missing_connect_terminal": frame(message{}.text(3, "partial").integer(5, 2)),
			"missing_model_terminal":   append(frame(message{}.text(3, "partial")), terminal()...),
			"unknown_model_terminal":   append(frame(message{}.integer(5, 99)), terminal()...),
			"malformed_text_field":     append(frame(message{}.integer(3, 9).integer(5, 2)), terminal()...),
			"invalid_tool_json":        append(frame(message{}.bytes(6, message{}.text(1, "c").text(2, "f").text(3, "{")).integer(5, 10)), terminal()...),
		} {
			t.Run(fmtBool(stream)+name, func(t *testing.T) {
				d, calls := upstream(t, wire)
				resp, err := execute(t, d, `{"model":"m","input":"x","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"stream":`+fmtBool(stream)+`}`)
				if err == nil {
					defer resp.Body.Close()
					var raw []byte
					raw, err = io.ReadAll(resp.Body)
					if bytes.Contains(raw, []byte(`"type":"response.completed"`)) {
						t.Fatal("fabricated successful terminal")
					}
				}
				if err == nil || errors.Is(err, nativeoauth.ErrUnsupportedRequest) || calls.Load() != 1 {
					t.Fatalf("wrong post-send classification: %v calls=%d", err, calls.Load())
				}
			})
		}
	}
	for _, reason := range []uint64{2, 3} {
		d, _ := upstream(t, append(frame(message{}.text(3, "text").integer(5, reason)), terminal()...))
		resp, err := execute(t, d, `{"model":"m","input":"x"}`)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out jsonObject
		json.Unmarshal(raw, &out)
		if out["usage"] != nil {
			t.Fatal("estimated usage")
		}
		if reason == 3 && out["status"] != "incomplete" {
			t.Fatal("length limit marked completed")
		}
	}
}
func fmtBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
func TestImmediateErrorAndRevokedAccess(t *testing.T) {
	end := frame([]byte(`{"error":{"code":"invalid_argument","message":"invalid reasoning signature synthetic-session"}}`))
	end[0] = 2
	d, calls := upstream(t, end)
	resp, err := execute(t, d, `{"model":"m","input":"x","stream":true}`)
	if err != nil || resp.StatusCode != 400 {
		t.Fatalf("immediate rejection lost: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "synthetic-session") || !strings.Contains(string(raw), "invalid reasoning signature") {
		t.Fatal("redaction or diagnostic lost")
	}
	req, _ := http.NewRequestWithContext(accessguard.With(context.Background(), func() error { return errors.New("disabled") }), "POST", "https://example.invalid", strings.NewReader(`{"model":"m","input":"x"}`))
	if _, err = d.Execute(nil, req, syntheticOAuth()); err == nil || calls.Load() != 1 {
		t.Fatal("revoked capability sent another request")
	}
}
func TestCloseAndCancellationUnblockRPC(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmtBool(cancelEarly), func(t *testing.T) {
			disconnected := make(chan struct{})
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(disconnected)
				w.Header().Set("Content-Type", "application/connect+proto")
				if !cancelEarly {
					w.Write(frame(message{}.text(3, "partial")))
				}
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", server.URL, strings.NewReader(`{"model":"m","input":"x","stream":true}`))
			d := NewDriver(server.Client(), Endpoints{ServerURL: server.URL})
			finished := make(chan error, 1)
			go func() {
				resp, err := d.Execute(nil, req, syntheticOAuth())
				if err == nil {
					err = resp.Body.Close()
				}
				finished <- err
			}()
			<-started
			if cancelEarly {
				cancel()
			}
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("execute/close blocked")
			}
			select {
			case <-disconnected:
			case <-time.After(2 * time.Second):
				t.Fatal("RPC remained open")
			}
		})
	}
}
func TestFrameCompressionAndMalformedWire(t *testing.T) {
	var zipped bytes.Buffer
	gz := gzip.NewWriter(&zipped)
	gz.Write([]byte{0x1a, 0x02, 'o', 'k'})
	gz.Close()
	raw := frame(zipped.Bytes())
	raw[0] = 1
	flags, data, err := readFrame(bytes.NewReader(raw))
	if err != nil || flags != 1 || !bytes.Equal(data, []byte{0x1a, 0x02, 'o', 'k'}) {
		t.Fatal("compressed frame lost")
	}
	for _, raw := range [][]byte{{0}, {0x0a, 0xff}, {0x0b}, {0x08, 0x80}} {
		if _, err := fields(raw); err == nil {
			t.Fatal("invalid protobuf accepted")
		}
	}
}

func TestUsageAcrossFramesKeepsOnlyReportedCounters(t *testing.T) {
	wire := append(frame(message{}.text(3, "answer").bytes(7, message{}.integer(2, 11).integer(5, 4))), frame(message{}.bytes(7, message{}.integer(3, 7)))...)
	wire = append(wire, frame(message{}.integer(5, 2).bytes(7, message{}.integer(6, 200)))...)
	d, _ := upstream(t, append(wire, terminal()...))
	resp, err := execute(t, d, `{"model":"m","input":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out jsonObject
	json.Unmarshal(raw, &out)
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(15) || usage["output_tokens"] != float64(7) || usage["total_tokens"] != float64(22) {
		t.Fatal("later frame erased counters")
	}
	if _, ok := usage["input_tokens_details"].(map[string]any)["cache_write_tokens"]; ok {
		t.Fatal("invented unreported counter")
	}
	failure := rpcFailure([]byte("<html>upstream failure</html>"), 503, nil)
	if failure.status != 503 || strings.Contains(failure.message, "<html>") {
		t.Fatal("HTTP failure status/privacy changed")
	}
	failure = rpcFailure([]byte(`{"error":{"code":"unauthenticated","message":"private@example.invalid session_token=synthetic-unknown"}}`), 200, nil)
	if strings.Contains(failure.message, "@") || strings.Contains(failure.message, "synthetic-unknown") {
		t.Fatal("sensitive error context exposed")
	}
}
