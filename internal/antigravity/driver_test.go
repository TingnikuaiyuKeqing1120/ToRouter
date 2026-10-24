package antigravity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

// Every identity/token here is deliberately synthetic. These tests exercise
// our wire boundary, not a Google account or a live entitlement.
func fixtureCredential() store.OAuthCredential {
	return store.OAuthCredential{Provider: "antigravity", ClientID: clientID, AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Account: "fixture-user@example.invalid", Subject: "fixture-subject", ProjectID: "fixture-project", Scopes: "fixture-scope"}
}
func localDriver(t *testing.T, handler http.HandlerFunc) *Driver {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	d := NewDriver(srv.Client(), Endpoints{AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", UserInfoURL: srv.URL + "/userinfo", APIBase: srv.URL, LoadBase: srv.URL})
	d.pollInterval = time.Millisecond
	return d
}
func request(t *testing.T, ctx context.Context, stream bool, body string) *http.Request {
	t.Helper()
	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://downstream.invalid/v1beta/models/fixture-model:"+action, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("x-goog-api-key", "must-not-forward")
	r.Header.Set("Authorization", "Bearer must-not-forward")
	return r
}

func TestAuthorizationPKCEAndState(t *testing.T) {
	var hits atomic.Int32
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); t.Error("invalid callback sent HTTP") })
	a, err := d.Begin(context.Background(), "http://localhost:51121/oauth-callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(a.URL)
	q := u.Query()
	sum := sha256.Sum256([]byte(a.Verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || q.Get("code_challenge_method") != "S256" || q.Get("state") != a.State || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("client_id") != clientID || q.Get("redirect_uri") != a.RedirectURI || q.Get("scope") != strings.Join(scopes, " ") {
		t.Fatalf("invalid OAuth authorization parameters: %v", q)
	}
	b, err := d.Begin(context.Background(), a.RedirectURI)
	if err != nil || a.State == b.State || a.Verifier == b.Verifier {
		t.Fatal("flow randomness reused")
	}
	for _, cb := range []oauthbase.CallbackResult{{Code: "fixture-code", State: "wrong"}, {State: a.State, Error: "access_denied", Description: "fixture-user@example.invalid"}, {State: a.State, Code: "fixture-code", ClientID: "other-client"}} {
		c, err := d.Exchange(context.Background(), a, cb)
		if err == nil || c.AccessToken != "" || strings.Contains(err.Error(), "example.invalid") {
			t.Fatalf("bad callback accepted/leaked: %+v %v", c, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("unexpected network")
	}
	for _, redirect := range []string{"https://localhost:51121/oauth-callback", "http://remote.invalid:51121/oauth-callback", "http://localhost:51121/wrong", "http://localhost:51121/oauth-callback?x=1"} {
		if _, err := d.Begin(context.Background(), redirect); err == nil {
			t.Fatal("accepted bad redirect")
		}
	}
}

func TestExchangeObtainsProjectBeforeReturningCredential(t *testing.T) {
	var stages []string
	var verifier string
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		stages = append(stages, r.URL.Path)
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("code_verifier") != verifier || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != clientID || r.Form.Get("client_secret") != clientSecret {
				t.Error("OAuth binding/PKCE absent")
			}
			fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"returned-scope"}`)
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer synthetic-access" {
				t.Error("userinfo bearer absent")
			}
			fmt.Fprint(w, `{"email":"fixture-user@example.invalid","id":"fixture-subject"}`)
		case "/v1internal:loadCodeAssist":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["metadata"].(map[string]any)["ideType"] != "ANTIGRAVITY" {
				t.Error("load metadata absent")
			}
			fmt.Fprint(w, `{"allowedTiers":[{"id":"fixture-tier","isDefault":true}]}`)
		case "/v1internal:onboardUser":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["tier_id"] != "fixture-tier" || r.Header.Get("X-Goog-Api-Client") == "" {
				t.Error("onboarding binding absent")
			}
			fmt.Fprint(w, `{"done":true,"response":{"cloudaicompanionProject":{"id":"fixture-project"}}}`)
		default:
			t.Error("unexpected request")
		}
	})
	a, err := d.Begin(context.Background(), "http://localhost:51121/oauth-callback")
	if err != nil {
		t.Fatal(err)
	}
	verifier = a.Verifier
	c, err := d.Exchange(context.Background(), a, oauthbase.CallbackResult{Code: "fixture-code", State: a.State})
	if err != nil {
		t.Fatal(err)
	}
	if c.ProjectID != "fixture-project" || c.Subject != "fixture-subject" || c.Account != "fixture-subject" || c.AccountLabel != "Antigravity" || c.RefreshToken != "synthetic-refresh" || c.Scopes != "returned-scope" || time.Until(c.ExpiresAt) < 59*time.Minute {
		t.Fatalf("incomplete credential: %+v", c)
	}
	if strings.Join(stages, ",") != "/token,/userinfo,/v1internal:loadCodeAssist,/v1internal:onboardUser" {
		t.Fatalf("wrong flow: %v", stages)
	}
}

func TestExchangeFailureNeverReturnsPartialAccount(t *testing.T) {
	for _, test := range []struct{ name, token, load, onboard, want string }{
		{"offline", `{"access_token":"synthetic-access","expires_in":3600}`, `{}`, `{}`, "offline refresh token"},
		{"eligibility", `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`, `{"ineligibleTiers":[{"reasonCode":"AGE_RESTRICTED","reasonMessage":"fixture-user@example.invalid synthetic-access is ineligible"}]}`, `{}`, "AGE_RESTRICTED"},
		{"empty-project", `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`, `{}`, `{"done":true,"response":{}}`, "without a project"},
		{"operation-error", `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`, `{}`, `{"done":true,"error":{"status":"PERMISSION_DENIED","message":"synthetic-refresh fixture-user@example.invalid"}}`, "PERMISSION_DENIED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var onboards atomic.Int32
			d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token":
					fmt.Fprint(w, test.token)
				case "/userinfo":
					fmt.Fprint(w, `{"email":"fixture-user@example.invalid","id":"fixture-subject"}`)
				case "/v1internal:loadCodeAssist":
					fmt.Fprint(w, test.load)
				case "/v1internal:onboardUser":
					onboards.Add(1)
					fmt.Fprint(w, test.onboard)
				}
			})
			a, _ := d.Begin(context.Background(), "http://localhost:51121/oauth-callback")
			c, err := d.Exchange(context.Background(), a, oauthbase.CallbackResult{Code: "fixture-code", State: a.State})
			if err == nil || !strings.Contains(err.Error(), test.want) || c.AccessToken != "" || c.Account != "" {
				t.Fatalf("failure returned partial credential: %+v %v", c, err)
			}
			for _, secret := range []string{"synthetic-access", "synthetic-refresh", "fixture-user@example.invalid"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("failure leaked credential")
				}
			}
			if test.name == "eligibility" && onboards.Load() != 0 {
				t.Fatal("onboarded rejected account")
			}
		})
	}
}

func TestProjectPollingCancellationAndExistingProject(t *testing.T) {
	for _, raw := range []string{`"fixture-project"`, `{"id":"fixture-project"}`} {
		d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "onboard") {
				t.Error("unnecessary onboarding")
			}
			fmt.Fprintf(w, `{"cloudaicompanionProject":%s}`, raw)
		})
		id, err := d.project(context.Background(), fixtureCredential())
		if err != nil || id != "fixture-project" {
			t.Fatalf("existing project: %q %v", id, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "onboard") {
			fmt.Fprint(w, `{"done":false}`)
			cancel()
		} else {
			fmt.Fprint(w, `{}`)
		}
	})
	d.pollInterval = time.Hour
	_, err := d.project(ctx, fixtureCredential())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("poll ignored cancellation: %v", err)
	}
}

func TestRefreshRotatesOnlySuppliedMaterial(t *testing.T) {
	for _, refresh := range []string{"", "synthetic-rotated"} {
		t.Run(refresh, func(t *testing.T) {
			d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				if r.Form.Get("refresh_token") != "synthetic-refresh" || r.Form.Get("grant_type") != "refresh_token" {
					t.Error("wrong refresh form")
				}
				fmt.Fprintf(w, `{"access_token":"synthetic-new-access","refresh_token":%q,"expires_in":1800}`, refresh)
			})
			old := fixtureCredential()
			got, err := d.Refresh(context.Background(), old)
			if err != nil {
				t.Fatal(err)
			}
			wantRefresh := refresh
			if wantRefresh == "" {
				wantRefresh = old.RefreshToken
			}
			if got.RefreshToken != wantRefresh || got.ProjectID != old.ProjectID || got.Account != old.Account || got.Subject != old.Subject || got.Scopes != old.Scopes || got.AccessToken != "synthetic-new-access" || got.ExpiresAt.IsZero() {
				t.Fatalf("refresh lost material: %+v", got)
			}
		})
	}
}

func TestCatalogSendsProjectAndPreservesLogicalProtocol(t *testing.T) {
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path != "/v1internal:fetchAvailableModels" || b["project"] != "fixture-project" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("catalog request missing binding")
		}
		fmt.Fprint(w, `{"models":{"claude-fixture":{"displayName":"Fixture Claude"},"gemini-fixture":{"displayName":"Fixture Gemini"}}}`)
	})
	models, err := d.Models(context.Background(), fixtureCredential())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "claude-fixture" || models[0].Format != store.FormatGeminiGenerateContent || models[1].Name != "Fixture Gemini" {
		t.Fatalf("wrong catalog: %+v", models)
	}
	c := fixtureCredential()
	c.ProjectID = ""
	if _, err := d.Models(context.Background(), c); err == nil {
		t.Fatal("catalog accepted empty project")
	}
}

func TestJSONEnvelopePreservesToolsSignaturesAndUsage(t *testing.T) {
	logical := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"fixture","args":{"n":9007199254740993}},"thoughtSignature":"opaque-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7}}`
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:generateContent" || r.Header.Get("Authorization") != "Bearer synthetic-access" || r.Header.Get("x-goog-api-key") != "" {
			t.Error("wrong request transport")
		}
		var env map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&env)
		if string(env["project"]) != `"fixture-project"` || string(env["model"]) != `"fixture-model"` || string(env["userAgent"]) != `"antigravity"` || string(env["requestType"]) != `"agent"` || !strings.HasPrefix(string(env["requestId"]), `"agent-`) {
			t.Error("missing envelope fields")
		}
		var req map[string]json.RawMessage
		_ = json.Unmarshal(env["request"], &req)
		if string(req["sessionId"]) != `"caller-session"` || !strings.Contains(string(req["contents"]), `"thoughtSignature":"opaque-input-signature"`) || !strings.Contains(string(req["tools"]), "9007199254740993") {
			t.Error("transport mutated signatures/tools/session")
		}
		fmt.Fprint(w, `{"response":`+logical+`,"traceId":"ignored-transport-id"}`)
	})
	r := request(t, context.Background(), false, `{"sessionId":"caller-session","contents":[{"role":"model","parts":[{"text":"old","thoughtSignature":"opaque-input-signature"}]}],"tools":[{"functionDeclarations":[{"name":"fixture","parameters":{"enum":[9007199254740993]}}]}]}`)
	resp, err := d.Execute(nil, r, fixtureCredential())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != logical || resp.ContentLength != int64(len(logical)) {
		t.Fatalf("logical response changed: %s", b)
	}
}

type fragmentReader struct {
	data   []byte
	random *rand.Rand
}

func (r *fragmentReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := 1 + r.random.Intn(19)
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestSSEArbitraryChunksKeepRealTerminalAndErrors(t *testing.T) {
	stream := ": heartbeat\r\n\r\nid: fixture-event\r\ndata: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"fixture\",\"args\":{}},\"thoughtSignature\":\"opaque-stream-signature\"}]}}]}}\r\n\r\ndata: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"totalTokenCount\":8}}}\n\ndata: {\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"details\":[{\"reason\":\"RATE_LIMIT_EXCEEDED\"}],\"message\":\"synthetic-access fixture-user@example.invalid\"}}\n\n"
	for seed := int64(0); seed < 20; seed++ {
		body := newEventBody(context.Background(), io.NopCloser(&fragmentReader{data: []byte(stream), random: rand.New(rand.NewSource(seed))}), []string{"synthetic-access"})
		out, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		for _, want := range []string{": heartbeat", "id: fixture-event", `"thoughtSignature":"opaque-stream-signature"`, `"functionCall"`, `"finishReason":"STOP"`, `"totalTokenCount":8`, `RATE_LIMIT_EXCEEDED`} {
			if !strings.Contains(s, want) {
				t.Fatalf("lost SSE field %s: %s", want, s)
			}
		}
		if strings.Contains(s, `"response"`) || strings.Contains(s, "[DONE]") || strings.Contains(s, "synthetic-access") || strings.Contains(s, "example.invalid") {
			t.Fatalf("SSE fabricated marker/leaked identity: %s", s)
		}
	}
	for _, source := range []string{"data: {broken}\n\n", "data: {\"response\":{}}\n", "data: {\"response\":null}\n\n"} {
		body := newEventBody(context.Background(), io.NopCloser(strings.NewReader(source)), nil)
		_, err := io.ReadAll(body)
		body.Close()
		if err == nil {
			t.Fatalf("accepted malformed/truncated SSE %q", source)
		}
	}
	// EOF after a nonterminal delta must remain nonterminal.
	body := newEventBody(context.Background(), io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[]}}\n\n")), nil)
	out, err := io.ReadAll(body)
	body.Close()
	if err != nil || strings.Contains(string(out), "STOP") || strings.Contains(string(out), "DONE") {
		t.Fatal("fabricated terminal")
	}
}

func TestExecuteStreamContextAndWirePath(t *testing.T) {
	started := make(chan struct{})
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
			t.Error("wrong SSE URL")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"response\":{\"candidates\":[]}}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := d.Execute(nil, request(t, ctx, true, `{"contents":[]}`), fixtureCredential())
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stream lost cancellation: %v", err)
	}
}

func TestHTTPFailuresKeepStatusRetryAfterAndReasonsWithoutSecrets(t *testing.T) {
	var hits atomic.Int32
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "17")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"synthetic-access fixture-user@example.invalid","details":[{"reason":"CREDITS_EXHAUSTED","metadata":{"refresh_token":"synthetic-refresh"}}]}}`)
	})
	resp, err := d.Execute(nil, request(t, context.Background(), false, `{"contents":[]}`), fixtureCredential())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "17" || !strings.Contains(string(b), "CREDITS_EXHAUSTED") || strings.Contains(string(b), "synthetic-access") || strings.Contains(string(b), "example.invalid") {
		t.Fatalf("lost real failure/redaction: %d %s", resp.StatusCode, b)
	}
	_, err = d.Models(context.Background(), fixtureCredential())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode() != 429 || !strings.Contains(err.Error(), "CREDITS_EXHAUSTED") || strings.Contains(err.Error(), "synthetic-refresh") {
		t.Fatalf("catalog failure lost status/redaction: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatal("inference/catalog silently retried")
	}
}

func TestEveryOperationHonorsGuardAndDoesNotFollowRedirect(t *testing.T) {
	var hits atomic.Int32
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/must-not-follow", http.StatusTemporaryRedirect)
	})
	ctx := accessguard.With(context.Background(), func() error { return errors.New("plugin disabled") })
	c := fixtureCredential()
	a := nativeoauth.Authorization{State: "fixture-state", Verifier: "fixture-verifier", RedirectURI: "http://localhost:51121/oauth-callback"}
	checks := []func() error{
		func() error { _, err := d.Refresh(ctx, c); return err },
		func() error { _, err := d.Models(ctx, c); return err },
		func() error {
			_, err := d.Exchange(ctx, a, oauthbase.CallbackResult{State: a.State, Code: "fixture-code"})
			return err
		},
		func() error { _, err := d.project(ctx, c); return err },
		func() error { _, err := d.Execute(nil, request(t, ctx, false, `{"contents":[]}`), c); return err },
	}
	for _, check := range checks {
		if err := check(); !accessguard.Denied(err) {
			t.Fatalf("guard classification lost: %v", err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("disabled operation sent network")
	}
	_, err := d.Models(context.Background(), c)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode() != 307 || hits.Load() != 1 {
		t.Fatalf("followed credential redirect: hits=%d err=%v", hits.Load(), err)
	}
}

func TestGuardRecheckedBetweenAuthorizationStages(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	var hits atomic.Int32
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		allowed.Store(false)
		fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`)
	})
	ctx := accessguard.With(context.Background(), func() error {
		if !allowed.Load() {
			return errors.New("plugin disabled")
		}
		return nil
	})
	a := nativeoauth.Authorization{State: "fixture-state", Verifier: "fixture-verifier", RedirectURI: "http://localhost:51121/oauth-callback"}
	c, err := d.Exchange(ctx, a, oauthbase.CallbackResult{State: a.State, Code: "fixture-code"})
	if !accessguard.Denied(err) || hits.Load() != 1 || c.AccessToken != "" {
		t.Fatalf("continued revoked authorization: hits=%d err=%v", hits.Load(), err)
	}
}

func TestResponseBoundsAndMalformedEnvelopes(t *testing.T) {
	for _, body := range []string{`{"response":null}`, `{}`, `not-json`} {
		d := localDriver(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if resp, err := d.Execute(nil, request(t, context.Background(), false, `{"contents":[]}`), fixtureCredential()); err == nil {
			resp.Body.Close()
			t.Fatal("accepted malformed response")
		}
	}
	if _, err := readBounded(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("body size cap not enforced")
	}
	if _, err := unwrap([]byte(`{"error":{"code":503,"message":"fixture failure"}}`), nil); err != nil {
		t.Fatal("upstream in-band failure discarded")
	}
}

type blockedBody struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *blockedBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.closed
	return 0, errors.New("synthetic-private-upstream-error")
}
func (b *blockedBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestSSECancelUnblocksActiveReadAndSanitizesReadFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	upstream := &blockedBody{entered: make(chan struct{}), closed: make(chan struct{})}
	body := newEventBody(ctx, upstream, nil)
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(body); done <- err }()
	<-upstream.entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost active cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled stream read remained blocked")
	}
	body.Close()
	upstream = &blockedBody{entered: make(chan struct{}), closed: make(chan struct{})}
	upstream.Close()
	body = newEventBody(context.Background(), upstream, nil)
	_, err := io.ReadAll(body)
	body.Close()
	if err == nil || strings.Contains(err.Error(), "synthetic-private-upstream-error") {
		t.Fatalf("upstream read diagnostic leaked: %v", err)
	}
}

func TestImageRequestUsesObservedImageEnvelope(t *testing.T) {
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		var env map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&env)
		var id string
		_ = json.Unmarshal(env["requestId"], &id)
		if string(env["requestType"]) != `"image_gen"` || !strings.HasPrefix(id, "image_gen/") || !strings.HasSuffix(id, "/12") {
			t.Error("image envelope differs from observed transport")
		}
		fmt.Fprint(w, `{"response":{"candidates":[]}}`)
	})
	r := request(t, context.Background(), false, `{"contents":[]}`)
	r.URL.Path = "/v1beta/models/fixture-image:generateContent"
	resp, err := d.Execute(nil, r, fixtureCredential())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestMissingStableIdentityAndUnsupportedLogicalRequest(t *testing.T) {
	var hits atomic.Int32
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"email":"fixture-user@example.invalid","name":"Fixture display name"}`)
		default:
			t.Error("identity failure continued to project lookup")
		}
	})
	a := nativeoauth.Authorization{State: "fixture-state", Verifier: "fixture-verifier", RedirectURI: "http://localhost:51121/oauth-callback"}
	c, err := d.Exchange(context.Background(), a, oauthbase.CallbackResult{State: a.State, Code: "fixture-code"})
	if err == nil || c.AccessToken != "" || hits.Load() != 2 {
		t.Fatal("display name substituted for stable identity")
	}
	for _, test := range []struct{ path, body string }{{"/v1/messages", `{}`}, {"/v1beta/models/fixture:unknownAction", `{}`}, {"/v1beta/models/fixture:generateContent", `[]`}} {
		r := request(t, context.Background(), false, test.body)
		r.URL.Path = test.path
		_, err := d.Execute(nil, r, fixtureCredential())
		if !errors.Is(err, nativeoauth.ErrUnsupportedRequest) {
			t.Fatalf("unsupported logical request lost pre-send classification: %v", err)
		}
	}
	if hits.Load() != 2 {
		t.Fatal("unsupported logical request sent upstream")
	}
}
