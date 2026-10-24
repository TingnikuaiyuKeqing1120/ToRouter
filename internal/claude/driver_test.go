package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

func endpointFixture(base string) Endpoints {
	return Endpoints{AuthorizeURL: base + "/authorize", TokenURL: base + "/token", RefreshURL: base + "/refresh", ProfileURL: base + "/profile", RolesURL: base + "/roles", APIBase: base}
}

func TestOAuthExchangeAndRefreshMaterials(t *testing.T) {
	var tokenPayload map[string]string
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") != "axios/1.15.2" || r.Header.Get("Accept-Encoding") != "gzip, compress, deflate, br" {
			t.Error("control profile headers missing")
		}
		switch r.URL.Path {
		case "/token":
			_ = json.NewDecoder(r.Body).Decode(&tokenPayload)
			io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expires_in":3600,"scope":"user:profile user:inference"}`)
		case "/refresh":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["refresh_token"] != "synthetic-refresh" || body["client_id"] != ClientID || body["scope"] != Scope {
				t.Error("refresh materials incomplete")
			}
			io.WriteString(w, `{"access_token":"synthetic-rotated","expires_in":7200}`)
		case "/profile":
			if r.Header.Get("Authorization") == "" {
				t.Error("profile bearer absent")
			}
			io.WriteString(w, `{"account":{"uuid":"synthetic-account","email":"hidden@example.invalid"},"organization":{"uuid":"synthetic-org"}}`)
		case "/roles":
			io.WriteString(w, `{"roles":[]}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	d := NewDriver(server.Client(), endpointFixture(server.URL))
	ctx := context.Background()
	auth, err := d.Begin(ctx, RedirectURI)
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.Begin(ctx, RedirectURI)
	if err != nil || other.State == auth.State || other.Verifier == auth.Verifier {
		t.Fatal("OAuth starts reused state or verifier")
	}
	u, _ := url.Parse(auth.URL)
	q := u.Query()
	sum := sha256.Sum256([]byte(auth.Verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || q.Get("state") != auth.State || q.Get("scope") != Scope || q.Get("redirect_uri") != RedirectURI {
		t.Fatal("invalid authorize query")
	}
	for _, callback := range []oauthbase.CallbackResult{{Code: "code", State: "wrong"}, {Code: "code#wrong", State: auth.State}, {Code: "", State: auth.State}, {Code: "code", State: auth.State, Error: "denied"}} {
		if _, err := d.Exchange(ctx, auth, callback); err == nil {
			t.Fatal("invalid callback accepted")
		}
	}
	if hits.Load() != 0 {
		t.Fatal("state rejection reached network")
	}
	credential, err := d.Exchange(ctx, auth, oauthbase.CallbackResult{Code: "synthetic-code#" + auth.State})
	if err != nil {
		t.Fatal(err)
	}
	if tokenPayload["code"] != "synthetic-code" || tokenPayload["state"] != auth.State || tokenPayload["code_verifier"] != auth.Verifier || tokenPayload["client_id"] != ClientID {
		t.Fatal("token exchange payload incorrect")
	}
	if !validDeviceID(credential.DeviceID) || credential.Account != "synthetic-account" || credential.Subject != "synthetic-account" || credential.OrganizationID != "synthetic-org" || credential.ClientID != ClientID || credential.AccountLabel != "" || !credential.ExpiresAt.After(time.Now()) {
		t.Fatal("complete private identity was not retained")
	}
	refreshed, err := d.Refresh(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "synthetic-rotated" || refreshed.RefreshToken != credential.RefreshToken || refreshed.DeviceID != credential.DeviceID || refreshed.Account != credential.Account || refreshed.OrganizationID != credential.OrganizationID || refreshed.Scopes != credential.Scopes {
		t.Fatal("refresh lost stable or omitted material")
	}
	if definition := d.Definition(); definition.ID != "claude" || definition.CredentialType != store.CredentialClaudeOAuth || definition.CallbackPath != "/callback" || definition.Endpoints[0].Format != store.FormatAnthropicMessages {
		t.Fatal("incorrect driver definition")
	}
}

func fixtureCredential() store.OAuthCredential {
	return store.OAuthCredential{Provider: "claude", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ClientID: ClientID, Account: "synthetic-account", OrganizationID: "synthetic-org", DeviceID: strings.Repeat("a", 64)}
}

const messageBody = `{"model":"claude-sonnet-4-6","max_tokens":128,"system":[{"type":"text","text":"keep system","cache_control":{"type":"ephemeral"}}],"metadata":{"extra":"keep","user_id":"{\"session_id\":\"synthetic-conversation\",\"extra\":7}"},"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"thinking","thinking":"original reasoning","signature":"original-signature"},{"type":"tool_use","id":"tool-1","name":"inspect","input":{"x":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"original result"}]}],"tools":[{"name":"inspect","input_schema":{"type":"object","properties":{"x":{"type":"integer"}}}}],"thinking":{"type":"enabled","budget_tokens":1024},"stream":true}`

func TestExecutePreservesLogicalPayloadIdentityAndUpstreamFacts(t *testing.T) {
	var bodies [][]byte
	var sessions []string
	const stream = "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		sessions = append(sessions, r.Header.Get("X-Claude-Code-Session-Id"))
		if r.Header.Get("Authorization") != "Bearer synthetic-access" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("User-Agent") != "claude-cli/2.1.280 (external, cli)" || !strings.Contains(r.Header.Get("Anthropic-Beta"), "oauth-2025-04-20") || r.Header.Get("X-Stainless-Retry-Count") != "0" {
			t.Error("missing Claude profile")
		}
		if len(bodies) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, stream)
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"synthetic upstream failure"}}`)
		}
	}))
	defer server.Close()
	d := NewDriver(server.Client(), endpointFixture(server.URL))
	credential := fixtureCredential()
	for _, status := range []int{200, 429} {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(messageBody))
		req.Header.Set("X-Api-Key", "must-not-forward")
		response, err := d.Execute(server.Client(), req, credential)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != status {
			t.Fatalf("upstream failure reclassified: %d", response.StatusCode)
		}
		if status == 200 && string(raw) != stream {
			t.Fatal("SSE error/truncation or terminal fact was rewritten")
		}
		if status == 429 && !strings.Contains(string(raw), "synthetic upstream failure") {
			t.Fatal("upstream error was replaced")
		}
	}
	if len(bodies) != 2 || sessions[0] == "" || sessions[0] != sessions[1] || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatal("stable request identity/signature changed across calls")
	}
	var original, modified map[string]json.RawMessage
	_ = json.Unmarshal([]byte(messageBody), &original)
	_ = json.Unmarshal(bodies[0], &modified)
	for _, key := range []string{"tools", "thinking", "messages", "max_tokens"} {
		var a, b any
		_ = json.Unmarshal(original[key], &a)
		_ = json.Unmarshal(modified[key], &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("logical %s changed", key)
		}
	}
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(modified["metadata"], &metadata)
	var identity map[string]json.RawMessage
	_ = json.Unmarshal([]byte(rawString(metadata["user_id"])), &identity)
	if rawString(identity["device_id"]) != credential.DeviceID || rawString(identity["account_uuid"]) != credential.Account || rawString(identity["session_id"]) != sessions[0] || string(identity["extra"]) != "7" || rawString(metadata["extra"]) != "keep" {
		t.Fatal("credential identity or metadata extra lost")
	}
	if claudeCCHFromBody(t, bodies[0]) == "00000" {
		t.Fatal("unsigned request")
	}
	rotated := credential
	rotated.AccessToken = "synthetic-new-access"
	first, _, _ := prepareBody([]byte(messageBody), http.Header{}, credential)
	second, _, _ := prepareBody([]byte(messageBody), http.Header{}, rotated)
	if !bytes.Equal(first, second) {
		t.Fatal("token rotation changed device/session identity")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGuardCancellationErrorsAndRedirects(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
		io.WriteString(w, `{"access_token":"secret-that-must-not-leak"}`)
	}))
	defer server.Close()
	d := NewDriver(server.Client(), endpointFixture(server.URL))
	ctx := accessguard.With(context.Background(), func() error { return errors.New("plugin disabled") })
	if _, err := d.Refresh(ctx, fixtureCredential()); !accessguard.Denied(err) {
		t.Fatalf("guard error classification lost: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("disabled guard hit network")
	}
	if _, err := d.Refresh(context.Background(), fixtureCredential()); err == nil || strings.Contains(err.Error(), "secret") || len(err.Error()) > 200 {
		t.Fatal("error body leaked")
	}
	blockedClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(cancelCtx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(messageBody))
	if _, err := d.Execute(blockedClient, req, fixtureCredential()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	redirectDriver := NewDriver(redirect.Client(), endpointFixture(redirect.URL))
	if _, err := redirectDriver.Refresh(context.Background(), fixtureCredential()); err == nil {
		t.Fatal("redirect accepted as token success")
	}
	if targetHits.Load() != 0 {
		t.Fatal("token material followed redirect")
	}
}

func TestRefreshKeepsIdentityWithoutProfileDependency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			io.WriteString(w, `{"access_token":"synthetic-new-access","refresh_token":"synthetic-new-refresh","expires_in":3600}`)
			return
		}
		t.Error("token refresh must not depend on a subsequent profile request")
		w.WriteHeader(403)
	}))
	defer server.Close()
	d := NewDriver(server.Client(), endpointFixture(server.URL))
	old := fixtureCredential()
	refreshed, err := d.Refresh(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "synthetic-new-access" || refreshed.RefreshToken != "synthetic-new-refresh" || refreshed.Account != old.Account || refreshed.OrganizationID != old.OrganizationID || refreshed.DeviceID != old.DeviceID {
		t.Fatal("token refresh lost rotated material or stable identity")
	}
}

func TestNativeTLSProfilesValidateCertificatesAndRunHTTP11(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Error("profile negotiated unexpected HTTP protocol")
		}
		if r.URL.Path == "/refresh" {
			io.WriteString(w, `{"access_token":"synthetic-tls","expires_in":3600}`)
		} else if r.URL.Path == "/v1/messages" {
			io.WriteString(w, `{"type":"message","content":[]}`)
		} else {
			io.WriteString(w, `{}`)
		}
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	base.TLSClientConfig.InsecureSkipVerify = false
	base.TLSClientConfig.RootCAs = roots
	client := &http.Client{Transport: base, Timeout: 2 * time.Second}
	defer base.CloseIdleConnections()
	d := NewDriver(client, endpointFixture(server.URL))
	if _, err := d.Refresh(context.Background(), fixtureCredential()); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(messageBody))
	response, err := d.Execute(client, req, fixtureCredential())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if base.DialTLSContext != nil || base.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("shared client's TLS configuration mutated")
	}
	untrusted := http.DefaultTransport.(*http.Transport).Clone()
	untrusted.Proxy = nil
	defer untrusted.CloseIdleConnections()
	bad := NewDriver(&http.Client{Transport: untrusted, Timeout: 2 * time.Second}, endpointFixture(server.URL))
	if _, err := bad.Refresh(context.Background(), fixtureCredential()); err == nil {
		t.Fatal("untrusted server certificate accepted")
	}
}

func TestModelsArePinnedAndDoNotProbeAccount(t *testing.T) {
	d := NewDriver(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("catalog performed network request")
		return nil, nil
	})}, Endpoints{})
	models, err := d.Models(context.Background(), fixtureCredential())
	if err != nil || len(models) == 0 {
		t.Fatal("missing catalog")
	}
	models[0].ID = "mutated"
	next, _ := d.Models(context.Background(), fixtureCredential())
	if next[0].ID == "mutated" {
		t.Fatal("catalog caller mutated shared state")
	}
}

func TestPreflightRejectionIsTypedAndSendsNothing(t *testing.T) {
	var hits atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { hits.Add(1); return nil, errors.New("must not execute") })}
	d := NewDriver(client, Endpoints{})
	for _, body := range []string{`{"model":"x","metadata":[]}`, `{"model":"x","metadata":{"user_id":"{\"session_id\":\"a\",\"session_id\":\"b\"}"}}`, `{"messages":[],"messages":[]}`, `{"system":42}`} {
		req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(body))
		if _, err := d.Execute(client, req, fixtureCredential()); !errors.Is(err, nativeoauth.ErrUnsupportedRequest) {
			t.Fatalf("preflight error was not typed: %v", err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid body reached inference")
	}
}

func TestProfileDecodesNegotiatedCompression(t *testing.T) {
	const raw = `{"type":"message","content":[{"type":"text","text":"synthetic"}]}`
	for _, encoding := range []string{"gzip", "br", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var compressed bytes.Buffer
			switch encoding {
			case "gzip":
				writer := gzip.NewWriter(&compressed)
				io.WriteString(writer, raw)
				writer.Close()
			case "br":
				writer := brotli.NewWriter(&compressed)
				io.WriteString(writer, raw)
				writer.Close()
			case "zstd":
				writer, _ := zstd.NewWriter(&compressed)
				io.WriteString(writer, raw)
				writer.Close()
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": []string{encoding}}, Body: io.NopCloser(bytes.NewReader(compressed.Bytes()))}, nil
			})}
			d := NewDriver(client, Endpoints{})
			req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(messageBody))
			response, err := d.Execute(client, req, fixtureCredential())
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			decoded, err := io.ReadAll(response.Body)
			if err != nil || string(decoded) != raw || response.Header.Get("Content-Encoding") != "" {
				t.Fatal("negotiated compression changed logical response")
			}
		})
	}
}

func TestCustomTLSVerifierIsPreserved(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"synthetic","expires_in":3600}`)
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	var verified atomic.Bool
	transport.TLSClientConfig.VerifyPeerCertificate = func([][]byte, [][]*x509.Certificate) error {
		verified.Store(true)
		return errors.New("policy rejected synthetic certificate")
	}
	d := NewDriver(&http.Client{Transport: transport}, endpointFixture(server.URL))
	if _, err := d.Refresh(context.Background(), fixtureCredential()); err == nil || !verified.Load() {
		t.Fatal("caller certificate verifier was silently dropped")
	}
}
