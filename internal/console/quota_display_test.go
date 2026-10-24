package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/auth"
	"torouter/internal/management"
	"torouter/internal/quota"
	"torouter/internal/store"
)

type quotaDisplayTransport func(*http.Request) (*http.Response, error)

func (f quotaDisplayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func quotaDisplayHarness(t *testing.T) (*dataHarness, store.Channel, *atomic.Int32, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := store.Channel{Name: "Balance example", Enabled: true, CredentialType: store.CredentialAPIKey,
		Endpoints:   []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.deepseek.com/v1"}},
		Credentials: []store.CredentialItem{{ID: "first", Enabled: true, APIKey: "synthetic-balance-key"}, {ID: "second", Enabled: false, APIKey: "synthetic-balance-key"}}}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	calls, observed, opened := new(atomic.Int32), new(atomic.Int32), new(atomic.Int32)
	svc := quota.New(st, nil, &http.Client{Transport: quotaDisplayTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.3400"}]}`))}, nil
	})})
	h := httptest.NewUnstartedServer(nil)
	origin := "http://" + h.Listener.Addr().String()
	console := New(st, origin, Hooks{Quotas: svc, ObserveQuota: func(_ store.Channel, _ store.CredentialItem, o quota.Observation) {
		if o.Available != nil {
			observed.Add(1)
		}
	}, OpenChannelConsole: func(id int64, cid string) error {
		if id != c.ID || cid != "first" {
			return fmt.Errorf("unexpected exact target")
		}
		opened.Add(1)
		return nil
	}})
	t.Cleanup(func() { ReleaseOAuthListeners(console) })
	mux := http.NewServeMux()
	mux.Handle("/mcp", ManagementHandler(console))
	mux.Handle("/api/", auth.New(st).RequireAdmin(http.StripPrefix("/api", console)))
	h.Config.Handler = mux
	h.Start()
	t.Cleanup(h.Close)
	return &dataHarness{store: st, http: h}, c, calls, observed, opened
}

func quotaAPIRequest(t *testing.T, h *dataHarness, method, path, body string, authorized bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.http.URL+"/api"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorized {
		settings, err := h.store.GetSettings()
		if err != nil {
			t.Fatal(err)
		}
		session, err := h.store.CreateAdminSession(settings.GatewayKey, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "to_router_session", Value: session})
		req.Header.Set("X-To-Router", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestQuotaDisplayHTTPReadOnlyManualRefreshAndExactSelection(t *testing.T) {
	h, c, calls, observed, opened := quotaDisplayHarness(t)
	if resp := quotaAPIRequest(t, h, "GET", "/quotas", "", false); resp.StatusCode != 401 {
		t.Fatal("unauthorized quota read")
	}
	resp := quotaAPIRequest(t, h, "GET", "/quotas", "", true)
	var listed quotaList
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || listed.RefreshSeconds != 300 || len(listed.Items) != 2 || listed.Items[1].Enabled || !listed.Items[0].Stale || calls.Load() != 0 {
		t.Fatalf("list %+v calls %d", listed, calls.Load())
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("quota response cacheable")
	}
	if _, err := h.store.PatchQuotaRefresh(0); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"channel_id":%d,"credential_id":"first","force":false}`, c.ID)
	resp = quotaAPIRequest(t, h, "POST", "/quotas/refresh", body, true)
	var snap quota.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !snap.Stale || calls.Load() != 0 {
		t.Fatalf("manual %+v", snap)
	}
	body = strings.Replace(body, `"force":false`, `"force":true`, 1)
	resp = quotaAPIRequest(t, h, "POST", "/quotas/refresh", body, true)
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || snap.Stale || len(snap.Balances) != 1 || snap.Balances[0].Total != "12.3400" || calls.Load() != 1 || observed.Load() != 1 {
		t.Fatalf("refresh %+v calls %d observed %d", snap, calls.Load(), observed.Load())
	}
	target := fmt.Sprintf(`{"channel_id":%d,"credential_id":"first"}`, c.ID)
	if resp := quotaAPIRequest(t, h, "POST", "/desktop/open-channel", target, true); resp.StatusCode != 200 || opened.Load() != 1 {
		t.Fatal("exact channel did not open")
	}
	if err := h.store.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	if resp := quotaAPIRequest(t, h, "POST", "/quotas/refresh", body, true); resp.StatusCode != 404 || calls.Load() != 1 {
		t.Fatal("removed quota target queried")
	}
	if resp := quotaAPIRequest(t, h, "POST", "/desktop/open-channel", target, true); resp.StatusCode != 404 || opened.Load() != 1 {
		t.Fatal("removed target redirected")
	}
}

func TestQuotaPreferencesHTTPValidationAndIndependentPatches(t *testing.T) {
	h, c, _, _, _ := quotaDisplayHarness(t)
	targets := fmt.Sprintf(`{"quota_targets":[{"channel_id":%d,"credential_id":"second"}]}`, c.ID)
	for _, body := range []string{`{"theme":"light"}`, targets, `{"paused":true}`} {
		if resp := quotaAPIRequest(t, h, "PUT", "/companion/preferences", body, true); resp.StatusCode != 200 {
			t.Fatalf("patch HTTP %d", resp.StatusCode)
		}
	}
	pref, err := h.store.CompanionPreferences()
	if err != nil || pref.Theme != "light" || !pref.Paused || len(pref.QuotaTargets) != 1 || pref.QuotaTargets[0].CredentialID != "second" {
		t.Fatalf("preferences %+v %v", pref, err)
	}
	for _, body := range []string{fmt.Sprintf(`{"quota_targets":[{"channel_id":%d,"credential_id":"missing"}]}`, c.ID), fmt.Sprintf(`{"quota_targets":[{"channel_id":%d,"credential_id":"first"},{"channel_id":%d,"credential_id":"first"}]}`, c.ID, c.ID)} {
		if resp := quotaAPIRequest(t, h, "PUT", "/companion/preferences", body, true); resp.StatusCode != 400 {
			t.Fatalf("invalid target HTTP %d", resp.StatusCode)
		}
	}
	c.Endpoints[0].BaseURL = "https://example.invalid"
	if err := h.store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if resp := quotaAPIRequest(t, h, "PUT", "/companion/preferences", targets, true); resp.StatusCode != 400 {
		t.Fatal("unsupported target accepted")
	}
}

func TestQuotaManagementSharesSnapshotsTargetsAndPermissions(t *testing.T) {
	h, c, calls, observed, opened := quotaDisplayHarness(t)
	reader, _ := h.client(t, management.Read)
	list := dataCall(t, reader, "get_quota_snapshots", map[string]any{})
	if len(list["items"].([]any)) != 2 || calls.Load() != 0 {
		t.Fatalf("MCP cache read %v", list)
	}
	dataCall(t, reader, "get_credential_quota", map[string]any{"channel_id": c.ID, "credential_id": "first"})
	if calls.Load() != 0 {
		t.Fatal("MCP read queried upstream")
	}
	denied, err := reader.CallTool(context.Background(), &mcp.CallToolParams{Name: "refresh_quota_snapshot", Arguments: map[string]any{"request_id": "reader-denied", "channel_id": c.ID, "credential_id": "first", "force": true}})
	if err == nil && !denied.IsError {
		t.Fatal("reader allowed refresh")
	}
	manager, _ := h.client(t, management.Read, management.Manage)
	snap := dataSuccess(t, manager, "refresh_quota_snapshot", map[string]any{"request_id": "refresh-exact", "channel_id": c.ID, "credential_id": "first", "force": true})
	if snap["credential_id"] != "first" || calls.Load() != 1 || observed.Load() != 1 {
		t.Fatalf("MCP refresh %v", snap)
	}
	dataSuccess(t, manager, "configure_companion", map[string]any{"request_id": "watch-exact", "patch": map[string]any{"quota_targets": []any{map[string]any{"channel_id": c.ID, "credential_id": "second"}}}})
	dataSuccess(t, manager, "configure_companion", map[string]any{"request_id": "theme-only", "patch": map[string]any{"theme": "light"}})
	companion := dataCall(t, reader, "get_companion", map[string]any{})
	pref := companion["preferences"].(map[string]any)
	if pref["theme"] != "light" || len(pref["quota_targets"].([]any)) != 1 {
		t.Fatalf("MCP preferences %v", pref)
	}
	dataSuccess(t, manager, "open_channel_console", map[string]any{"request_id": "open-exact", "channel_id": c.ID, "credential_id": "first"})
	if opened.Load() != 1 {
		t.Fatal("MCP exact open not invoked")
	}
	if err := h.store.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	list = dataCall(t, reader, "get_quota_snapshots", map[string]any{})
	if len(list["items"].([]any)) != 0 || calls.Load() != 1 {
		t.Fatal("removed target retained or queried")
	}
}
