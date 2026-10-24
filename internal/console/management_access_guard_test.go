package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/management"
	"torouter/internal/store"
)

func revocableContext(allowed *atomic.Bool) context.Context {
	return accessguard.With(context.Background(), func() error {
		if !allowed.Load() {
			return errors.New("synthetic authorization revoked")
		}
		return nil
	})
}

func TestManagementProbeStopsBeforeSecondCredentialOrFormat(t *testing.T) {
	for _, mode := range []string{"credentials", "formats"} {
		t.Run(mode, func(t *testing.T) {
			_, st := probeStore(t)
			var allowed atomic.Bool
			allowed.Store(true)
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				allowed.Store(false)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
			}))
			defer upstream.Close()
			c := store.Channel{Name: "guarded", Enabled: true, Models: []string{"m"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: upstream.URL}, {Format: store.FormatAnthropicMessages, BaseURL: upstream.URL}}, Credentials: []store.CredentialItem{{ID: "cred_1", Enabled: true, APIKey: "synthetic-one"}, {ID: "cred_2", Enabled: true, APIKey: "synthetic-two"}}}
			if err := st.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			u := &upstreamTools{store: st, client: upstream.Client()}
			formats := []string{store.FormatOpenAIChatCompletions}
			ids := []string{"cred_1", "cred_2"}
			if mode == "formats" {
				formats = append(formats, store.FormatAnthropicMessages)
				ids = ids[:1]
			}
			out, err := u.Probe(revocableContext(&allowed), UpstreamManagementInput{ChannelID: c.ID, Formats: formats, CredentialIDs: ids, Model: "m", Confirmed: true})
			if !accessguard.Denied(err) || hits.Load() != 1 {
				t.Fatalf("continued after revoke: hits=%d err=%v", hits.Load(), err)
			}
			partial := out.(toolResult)
			if partial.OperationStatus() != "partial" || len(partial.Formats) != 2 || !partial.Formats[0].OK || partial.Formats[0].UpstreamStatus != 200 || partial.Formats[1].Diagnostic == nil || partial.Formats[1].Diagnostic.Code != "not_sent" {
				t.Fatalf("lost completed/unsent facts: %+v", partial)
			}
		})
	}
}

func TestManagementRevokedUpstreamReceiptsRetainCompletedAndNotSent(t *testing.T) {
	for _, mode := range []string{"credentials", "formats", "draft", "discover", "discover_page", "http_error"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := fullManagementHarness(t)
			client, connectionID := h.client(t, management.Read, management.Manage)
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				disabled := false
				if _, err := h.store.UpdateManagementConnection(connectionID, store.ManagementPatch{Enabled: &disabled}); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if mode == "discover_page" {
						w.Write([]byte(`{"data":[{"id":"page-one"}],"has_more":true,"last_id":"page-one"}`))
					} else {
						w.Write([]byte(`{"data":[{"id":"discovered"}]}`))
					}
				} else {
					if mode == "http_error" {
						w.WriteHeader(http.StatusServiceUnavailable)
						w.Write([]byte(`{"error":{"message":"synthetic unavailable"}}`))
					} else {
						w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
					}
				}
			}))
			defer upstream.Close()
			c := store.Channel{Name: "receipt-source", Enabled: true, Models: []string{"m"}, AvailableModels: []string{"m"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: upstream.URL}, {Format: store.FormatAnthropicMessages, BaseURL: upstream.URL}}, Credentials: []store.CredentialItem{{ID: "cred_1", Enabled: true, APIKey: "synthetic-only-one"}, {ID: "cred_2", Enabled: true, APIKey: "synthetic-only-two"}}}
			if err := h.store.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"request_id": "retained-" + mode, "channel_id": c.ID, "formats": []string{store.FormatOpenAIChatCompletions}, "credential_ids": []string{"cred_1", "cred_2"}, "model": "m"}
			tool := "probe_upstream_model"
			if mode != "credentials" {
				args["formats"] = []string{store.FormatOpenAIChatCompletions, store.FormatAnthropicMessages}
				args["credential_ids"] = []string{"cred_1"}
			}
			if mode == "draft" {
				delete(args, "channel_id")
				delete(args, "credential_ids")
				args["draft"] = map[string]any{"api_key": "synthetic-draft-key", "endpoints": c.Endpoints}
			}
			if mode == "discover" || mode == "discover_page" {
				tool = "discover_upstream_models"
				args["apply"] = true
			}
			if mode == "discover_page" {
				args["formats"] = []string{store.FormatOpenAIChatCompletions}
			}
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil || !result.IsError {
				t.Fatalf("revoked caller received successful response: %+v %v", result, err)
			}
			saved, err := h.store.ManagementReceipt(connectionID, args["request_id"].(string))
			if err != nil || saved.Status != "partial" {
				t.Fatalf("actual outcome not durable: %+v %v", saved, err)
			}
			var envelope struct {
				Result toolResult `json:"result"`
				Error  struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err = json.Unmarshal(saved.Result, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != "forbidden" {
				t.Fatalf("missing safe revocation error: %s", saved.Result)
			}
			if mode == "discover_page" {
				if len(envelope.Result.Formats) != 1 || envelope.Result.Formats[0].UpstreamStatus != 200 || len(envelope.Result.Formats[0].Models) != 1 || envelope.Result.Formats[0].Models[0].ID != "page-one" || envelope.Result.Formats[0].Diagnostic == nil || envelope.Result.Formats[0].Diagnostic.Code != "access_revoked" {
					t.Fatalf("lost completed model page: %s", saved.Result)
				}
			} else {
				wantStatus := 200
				wantOK := true
				if mode == "http_error" {
					wantStatus = 503
					wantOK = false
				}
				if len(envelope.Result.Formats) != 2 || envelope.Result.Formats[0].OK != wantOK || envelope.Result.Formats[0].UpstreamStatus != wantStatus || envelope.Result.Formats[1].Diagnostic == nil || envelope.Result.Formats[1].Diagnostic.Code != "not_sent" {
					t.Fatalf("receipt lost returned response or pending work: %s", saved.Result)
				}
			}
			if mode == "discover" || mode == "discover_page" {
				live, err := h.store.Channel(c.ID)
				if err != nil || slices.Contains(live.AvailableModels, "discovered") {
					t.Fatalf("revoked catalogue saved: %+v %v", live.AvailableModels, err)
				}
			}
			enabled := true
			if _, err = h.store.UpdateManagementConnection(connectionID, store.ManagementPatch{Enabled: &enabled}); err != nil {
				t.Fatal(err)
			}
			read := dataCall(t, client, "get_operation_receipt", map[string]any{"request_id": args["request_id"]})
			if read["status"] != "partial" {
				t.Fatalf("restored authorization could not inspect receipt: %+v", read)
			}
			replay := dataCall(t, client, tool, args)
			if replay["status"] != "partial" || hits.Load() != 1 {
				t.Fatalf("replayed actual operation: %+v hits=%d", replay, hits.Load())
			}
		})
	}
}

func TestManagementDiscoveryGuardRefusalBeforeFirstSendIsNotSent(t *testing.T) {
	_, st := probeStore(t)
	var hits, checks atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte(`{"data":[]}`)) }))
	defer upstream.Close()
	u := &upstreamTools{store: st, client: upstream.Client()}
	// Permit planning, then revoke at the transport's final authorization gate.
	ctx := accessguard.With(context.Background(), func() error {
		if checks.Add(1) > 1 {
			return errors.New("authorization revoked before send")
		}
		return nil
	})
	out, err := u.Discover(ctx, UpstreamManagementInput{Draft: &UpstreamManagementDraft{APIKey: "synthetic-key", Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: upstream.URL}}}})
	result := out.(toolResult)
	if !accessguard.Denied(err) || hits.Load() != 0 || result.OperationStatus() != "failed" || len(result.Formats) != 1 || result.Formats[0].Diagnostic == nil || result.Formats[0].Diagnostic.Code != "not_sent" {
		t.Fatalf("known-not-sent became unknown: %+v err=%v hits=%d", result, err, hits.Load())
	}
}

func TestManagementBlockedOutcomePreservesUnknownNetworkResults(t *testing.T) {
	notSent := formatResult{Diagnostic: &diagnostic{Source: "gateway", Code: "not_sent"}}
	for _, tc := range []struct {
		name string
		rows []formatResult
		want string
	}{
		{"known_http", []formatResult{{UpstreamStatus: 200, OK: true}, notSent}, "partial"},
		{"known_http_error", []formatResult{{UpstreamStatus: 503, Diagnostic: &diagnostic{Source: "upstream", Code: "upstream"}}, notSent}, "partial"},
		{"body_unknown", []formatResult{{UpstreamStatus: 200, Diagnostic: &diagnostic{Source: "network", Code: "read"}}, notSent}, "unknown"},
		{"later_request_unknown", []formatResult{{UpstreamStatus: 200, OK: true}, {Diagnostic: &diagnostic{Source: "network", Code: "connection"}}, notSent}, "unknown"},
		{"all_not_sent", []formatResult{notSent}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := toolResult{Formats: tc.rows, Diagnostic: &diagnostic{Source: "gateway", Code: "blocked"}}
			if got := result.OperationStatus(); got != tc.want {
				t.Fatalf("status=%s want %s", got, tc.want)
			}
		})
	}
}

func TestManagementDiscoverRefusesRevokedOrChangedApply(t *testing.T) {
	for _, mode := range []string{"revoked", "credential_changed", "models_changed"} {
		t.Run(mode, func(t *testing.T) {
			_, st := probeStore(t)
			var allowed atomic.Bool
			allowed.Store(true)
			var c store.Channel
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if mode == "revoked" {
					allowed.Store(false)
				} else {
					live, err := st.Channel(c.ID)
					if err != nil {
						t.Error(err)
						return
					}
					if mode == "credential_changed" {
						live.Credentials[0].APIKey = "synthetic-replacement"
						live.APIKey = live.Credentials[0].APIKey
					} else {
						live.Models = append(live.Models, "concurrent")
					}
					if err = st.SaveChannel(&live); err != nil {
						t.Error(err)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"data":[{"id":"fetched"}]}`))
			}))
			defer upstream.Close()
			c = store.Channel{Name: "guarded", Enabled: true, Models: []string{"old"}, AvailableModels: []string{"old"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: upstream.URL}}, Credentials: []store.CredentialItem{{ID: "cred_1", Enabled: true, APIKey: "synthetic-key"}}}
			if err := st.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			u := &upstreamTools{store: st, client: upstream.Client()}
			_, err := u.Discover(revocableContext(&allowed), UpstreamManagementInput{ChannelID: c.ID, Apply: true, CredentialIDs: []string{"cred_1"}})
			if mode == "revoked" && !accessguard.Denied(err) {
				t.Fatalf("revoked apply %v", err)
			}
			if mode != "revoked" && !errors.Is(err, store.ErrEditConflict) {
				t.Fatalf("stale apply %v", err)
			}
			live, readErr := st.Channel(c.ID)
			if readErr != nil || slices.Contains(live.AvailableModels, "fetched") || hits.Load() != 1 {
				t.Fatalf("stale results saved: %+v hits=%d err=%v", live.AvailableModels, hits.Load(), readErr)
			}
		})
	}
}

func TestManagementOAuthCompletionDoesNotSaveAfterRevocation(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, mode := range []string{"exchange", "catalogue"} {
		t.Run(mode, func(t *testing.T) {
			_, st := probeStore(t)
			var allowed atomic.Bool
			allowed.Store(true)
			var exchanges, models atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/start":
					json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "device-1", "user_code": "CODE", "interval": 0, "expires_in": 900})
				case "/poll":
					json.NewEncoder(w).Encode(map[string]any{"authorization_code": "code", "code_verifier": "verifier"})
				case "/token":
					exchanges.Add(1)
					if mode == "exchange" {
						allowed.Store(false)
					}
					json.NewEncoder(w).Encode(codex.TokenResponse{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", IDToken: idTokenFor(t, "acct-guard", "guard@example.test"), ExpiresIn: 3600, Scope: codex.ScopeAuthorize})
				case "/models":
					models.Add(1)
					allowed.Store(false)
					w.Write([]byte(`{"models":[{"id":"m","slug":"m"}]}`))
				default:
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			eps := codex.DefaultEndpoints()
			eps.DeviceCode = upstream.URL + "/start"
			eps.DeviceToken = upstream.URL + "/poll"
			eps.Token = upstream.URL + "/token"
			eps.Models = upstream.URL + "/models"
			eps.Backend = upstream.URL + "/backend-api/codex"
			o := &oauthTools{store: st, codex: codex.NewService(st, upstream.Client(), eps), flows: map[string]*oauthFlow{}}
			ctx := revocableContext(&allowed)
			started, err := o.ManagementStart(ctx, "codex", "device", 0)
			if err != nil {
				t.Fatal(err)
			}
			id := started.(map[string]any)["flow"].(string)
			// No real time delay or listener is needed for a device flow poll.
			o.flows[id].interval = 0
			o.flows[id].lastPoll = time.Time{}
			answer, err := o.ManagementStatus(ctx, "codex", id)
			if err != nil {
				t.Fatal(err)
			}
			out := answer.(map[string]any)
			if out["status"] != flowError || out["code"] != "access_revoked" {
				t.Fatalf("wrong revocation classification: %+v", out)
			}
			channels, err := st.ListChannels()
			if err != nil || len(channels) != 0 || exchanges.Load() != 1 {
				t.Fatalf("revoked OAuth saved account: %d channels exchanges=%d err=%v", len(channels), exchanges.Load(), err)
			}
			wantModels := int32(0)
			if mode == "catalogue" {
				wantModels = 1
			}
			if models.Load() != wantModels {
				t.Fatalf("unexpected later request: %d", models.Load())
			}
		})
	}
}

func TestManagementOAuthCatalogueAllowsOwnRotationButRejectsSourceChange(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, mode := range []string{"rotation", "account_changed", "models_changed"} {
		t.Run(mode, func(t *testing.T) {
			_, st := probeStore(t)
			var c store.Channel
			var exchanges, requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/token":
					exchanges.Add(1)
					json.NewEncoder(w).Encode(codex.TokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", IDToken: idTokenFor(t, "acct-guard", "guard@example.test"), ExpiresIn: 3600, Scope: codex.ScopeAuthorize})
				case "/models":
					requests.Add(1)
					if mode != "rotation" {
						live, err := st.Channel(c.ID)
						if err != nil {
							t.Error(err)
							return
						}
						if mode == "account_changed" {
							live.Credentials[0].OAuth.Account = "another-account"
						} else {
							live.Models = append(live.Models, "concurrent-model")
						}
						if err = st.SaveChannel(&live); err != nil {
							t.Error(err)
							return
						}
					}
					w.Write([]byte(`{"models":[{"id":"fetched","slug":"fetched"}]}`))
				default:
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			eps := codex.DefaultEndpoints()
			eps.Token = upstream.URL + "/token"
			eps.Models = upstream.URL + "/models"
			eps.Backend = upstream.URL + "/backend-api/codex"
			c = store.Channel{Name: "oauth", CredentialType: store.CredentialCodexOAuth, Enabled: true, Models: []string{"old"}, AvailableModels: []string{"old"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: eps.Backend}}, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, AccessToken: "old-access", RefreshToken: "old-refresh", IDToken: idTokenFor(t, "acct-guard", "guard@example.test"), ClientID: codex.ClientID, Account: "acct-guard", Scopes: codex.ScopeAuthorize, ExpiresAt: time.Now().Add(-time.Minute), APIBase: eps.Backend, Status: store.OAuthStatusActive}}
			if err := st.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			o := &oauthTools{store: st, codex: codex.NewService(st, upstream.Client(), eps)}
			ctx := accessguard.With(context.Background(), func() error { return nil })
			out, err := o.ManagementModels(ctx, c.ID, c.Credentials[0].ID, true)
			live, readErr := st.Channel(c.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "rotation" {
				if err != nil || !slices.Contains(live.AvailableModels, "fetched") {
					t.Fatalf("own refresh caused conflict: out=%+v err=%v", out, err)
				}
			} else if !errors.Is(err, store.ErrEditConflict) || slices.Contains(live.AvailableModels, "fetched") {
				t.Fatalf("stale %s accepted: %+v %v", mode, live.AvailableModels, err)
			}
			if exchanges.Load() != 1 || requests.Load() != 1 {
				t.Fatalf("unexpected requests: exchange=%d models=%d", exchanges.Load(), requests.Load())
			}
		})
	}
}
