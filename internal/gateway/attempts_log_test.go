package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"torouter/internal/store"
)

func TestGatewayAttemptsLoggedAcrossCredentialsAndChannels(t *testing.T) {
	g, s := setup(t)
	// Allow up to 3 attempts, 2 retries per channel
	if err := s.SaveSettings(store.Settings{
		GatewayKey:             "gateway-key-1234",
		RetentionDays:          30,
		MaxAttempts:            3,
		RetriesPerChannel:      2,
		RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)},
	}); err != nil {
		t.Fatal(err)
	}

	var chan1Hits atomic.Int32
	var chan2Hits atomic.Int32

	// Server 1 serves Channel 1:
	// 1st request with cred1 -> returns 429
	// 2nd request with cred2 -> returns 503
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := chan1Hits.Add(1)
		authHeader := r.Header.Get("Authorization")
		if hit == 1 {
			if authHeader != "Bearer secret-key-1111" {
				t.Errorf("expected cred1 on hit 1, got %q", authHeader)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if authHeader != "Bearer secret-key-2222" {
			t.Errorf("expected cred2 on hit 2, got %q", authHeader)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server1.Close()

	// Server 2 serves Channel 2:
	// receives request with cred3 -> returns 200
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chan2Hits.Add(1)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer secret-key-3333" {
			t.Errorf("expected cred3 on hit 1 of server2, got %q", authHeader)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"all good"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer server2.Close()

	// Setup Channel 1 with 2 credentials in pool
	c1 := store.Channel{
		Name:    "channel-primary",
		Models:  []string{"test"},
		Enabled: true,
		Endpoints: []store.Endpoint{{
			Format:  store.FormatOpenAIChatCompletions,
			BaseURL: server1.URL,
		}},
		DispatchMode: store.DispatchModeFailover,
		Credentials: []store.CredentialItem{
			{ID: "cred-alpha", Label: "Alpha", APIKey: "secret-key-1111", Enabled: true},
			{ID: "cred-beta", Label: "Beta", APIKey: "secret-key-2222", Enabled: true},
		},
	}
	if err := s.SaveChannel(&c1); err != nil {
		t.Fatal(err)
	}

	// Setup Channel 2 with 1 credential
	c2 := store.Channel{
		Name:    "channel-secondary",
		Models:  []string{"test"},
		Enabled: true,
		Endpoints: []store.Endpoint{{
			Format:  store.FormatOpenAIChatCompletions,
			BaseURL: server2.URL,
		}},
		Credentials: []store.CredentialItem{
			{ID: "cred-gamma", Label: "Gamma", APIKey: "secret-key-3333", Enabled: true},
		},
	}
	if err := s.SaveChannel(&c2); err != nil {
		t.Fatal(err)
	}

	// Configure Downstream Model binding to both channels with fallback
	m := store.DownstreamModel{
		ID:       "test",
		Enabled:  true,
		Fallback: true,
		Bindings: []store.Binding{
			{ChannelID: c1.ID, Model: "test", Priority: 0, Enabled: true},
			{ChannelID: c2.ID, Model: "test", Priority: 1, Enabled: true},
		},
	}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}

	// Invoke the request
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "all good") {
		t.Fatalf("expected response body with 'all good', got %s", w.Body.String())
	}

	if chan1Hits.Load() != 2 {
		t.Fatalf("expected 2 hits on channel 1, got %d", chan1Hits.Load())
	}
	if chan2Hits.Load() != 1 {
		t.Fatalf("expected 1 hit on channel 2, got %d", chan2Hits.Load())
	}

	// Verify usage logs
	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least 1 usage record")
	}

	// Find the final successful record (first row, ORDER BY id DESC)
	successRow := rows[0]
	if successRow.Status != 200 {
		t.Fatalf("latest row should have status 200, got %d", successRow.Status)
	}

	attempts := successRow.Attempts
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts in successful usage log, got %d: %+v", len(attempts), attempts)
	}

	// Attempt 1: Channel 1, cred-alpha, 429, cooling_switched
	att1 := attempts[0]
	if att1.AttemptIndex != 1 {
		t.Errorf("att1.AttemptIndex = %d, want 1", att1.AttemptIndex)
	}
	if att1.ChannelID != c1.ID || att1.ChannelName != "channel-primary" {
		t.Errorf("att1 channel mismatch: id=%d name=%s", att1.ChannelID, att1.ChannelName)
	}
	if att1.CredentialID != "cred-alpha" {
		t.Errorf("att1.CredentialID = %q, want cred-alpha", att1.CredentialID)
	}
	if att1.CredentialMaskKey != store.MaskKey("secret-key-1111") {
		t.Errorf("att1.CredentialMaskKey = %q, want %q", att1.CredentialMaskKey, store.MaskKey("secret-key-1111"))
	}
	if att1.StatusCode != http.StatusTooManyRequests {
		t.Errorf("att1.StatusCode = %d, want 429", att1.StatusCode)
	}
	if att1.ActionTaken != store.ActionTakenCoolingSwitched {
		t.Errorf("att1.ActionTaken = %q, want %q", att1.ActionTaken, store.ActionTakenCoolingSwitched)
	}
	if !strings.Contains(att1.ErrorReason, "429") {
		t.Errorf("att1.ErrorReason = %q, want contain 429", att1.ErrorReason)
	}

	// Attempt 2: Channel 1, cred-beta, 502, exhausted_fallback
	att2 := attempts[1]
	if att2.AttemptIndex != 2 {
		t.Errorf("att2.AttemptIndex = %d, want 2", att2.AttemptIndex)
	}
	if att2.ChannelID != c1.ID || att2.ChannelName != "channel-primary" {
		t.Errorf("att2 channel mismatch: id=%d name=%s", att2.ChannelID, att2.ChannelName)
	}
	if att2.CredentialID != "cred-beta" {
		t.Errorf("att2.CredentialID = %q, want cred-beta", att2.CredentialID)
	}
	if att2.CredentialMaskKey != store.MaskKey("secret-key-2222") {
		t.Errorf("att2.CredentialMaskKey = %q, want %q", att2.CredentialMaskKey, store.MaskKey("secret-key-2222"))
	}
	if att2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("att2.StatusCode = %d, want 503", att2.StatusCode)
	}
	if att2.ActionTaken != store.ActionTakenExhaustedFallback {
		t.Errorf("att2.ActionTaken = %q, want %q", att2.ActionTaken, store.ActionTakenExhaustedFallback)
	}
	if !strings.Contains(att2.ErrorReason, "503") {
		t.Errorf("att2.ErrorReason = %q, want contain 503", att2.ErrorReason)
	}

	// Attempt 3: Channel 2, cred-gamma, 200, success
	att3 := attempts[2]
	if att3.AttemptIndex != 3 {
		t.Errorf("att3.AttemptIndex = %d, want 3", att3.AttemptIndex)
	}
	if att3.ChannelID != c2.ID || att3.ChannelName != "channel-secondary" {
		t.Errorf("att3 channel mismatch: id=%d name=%s", att3.ChannelID, att3.ChannelName)
	}
	if att3.CredentialID != "cred-gamma" {
		t.Errorf("att3.CredentialID = %q, want cred-gamma", att3.CredentialID)
	}
	if att3.CredentialMaskKey != store.MaskKey("secret-key-3333") {
		t.Errorf("att3.CredentialMaskKey = %q, want %q", att3.CredentialMaskKey, store.MaskKey("secret-key-3333"))
	}
	if att3.StatusCode != http.StatusOK {
		t.Errorf("att3.StatusCode = %d, want 200", att3.StatusCode)
	}
	if att3.ActionTaken != store.ActionTakenSuccess {
		t.Errorf("att3.ActionTaken = %q, want %q", att3.ActionTaken, store.ActionTakenSuccess)
	}
	if att3.ErrorReason != "" {
		t.Errorf("att3.ErrorReason = %q, want empty", att3.ErrorReason)
	}

	// Verify no plaintext keys were logged in any attempts
	for i, att := range attempts {
		for _, rawKey := range []string{"secret-key-1111", "secret-key-2222", "secret-key-3333"} {
			if strings.Contains(fmt.Sprintf("%+v", att), rawKey) {
				t.Fatalf("attempt[%d] contains unmasked secret %q", i, rawKey)
			}
		}
	}
}

func TestGatewayAttemptsSameChannelRetryAndSuccess(t *testing.T) {
	g, s := setup(t)
	if err := s.SaveSettings(store.Settings{
		GatewayKey:             "gateway-key-1234",
		RetentionDays:          30,
		MaxAttempts:            3,
		RetriesPerChannel:      2,
		RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)},
	}); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := hits.Add(1)
		if h == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"retry passed"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer server.Close()

	add(t, s, server.URL, "openai", 0)

	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"ping"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	rows, err := s.ListUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected usage logs")
	}

	// First row (latest) is the 200 OK
	attempts := rows[0].Attempts
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d: %+v", len(attempts), attempts)
	}

	if attempts[0].ActionTaken != store.ActionTakenRetry {
		t.Errorf("attempt[0].ActionTaken = %q, want %q", attempts[0].ActionTaken, store.ActionTakenRetry)
	}
	if attempts[0].StatusCode != http.StatusTooManyRequests {
		t.Errorf("attempt[0].StatusCode = %d, want 429", attempts[0].StatusCode)
	}

	if attempts[1].ActionTaken != store.ActionTakenSuccess {
		t.Errorf("attempt[1].ActionTaken = %q, want %q", attempts[1].ActionTaken, store.ActionTakenSuccess)
	}
	if attempts[1].StatusCode != http.StatusOK {
		t.Errorf("attempt[1].StatusCode = %d, want 200", attempts[1].StatusCode)
	}
}
