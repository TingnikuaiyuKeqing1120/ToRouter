package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/auth"
	"torouter/internal/management"
	"torouter/internal/store"
)

func fullManagementHarness(t *testing.T) (*dataHarness, *atomic.Int32) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := httptest.NewUnstartedServer(nil)
	origin := "http://" + h.Listener.Addr().String()
	calls := new(atomic.Int32)
	console := New(st, origin, Hooks{Restart: func() { calls.Add(1) }, Shutdown: func() { calls.Add(1) }})
	t.Cleanup(func() { ReleaseOAuthListeners(console) })
	mux := http.NewServeMux()
	mux.Handle("/mcp", ManagementHandler(console))
	mux.Handle("/api/", auth.New(st).RequireAdmin(http.StripPrefix("/api", console)))
	h.Config.Handler = mux
	h.Start()
	t.Cleanup(h.Close)
	return &dataHarness{store: st, http: h}, calls
}
func fixtureManagementChannel(t *testing.T, s *store.Store) store.Channel {
	t.Helper()
	c := store.Channel{Name: "Example", BaseURL: "https://example.invalid/v1", Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://example.invalid/v1"}}, APIKey: "synthetic-only-key-123456", Models: []string{"model-a"}, AvailableModels: []string{"model-a"}, Enabled: true}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func fullReadChannel(t *testing.T, client *mcp.ClientSession, id int64) map[string]any {
	result := dataCall(t, client, "get_entities", map[string]any{"refs": []any{map[string]any{"kind": "channel", "channel_id": id}}})
	return result["items"].([]any)[0].(map[string]any)
}

func TestManagementCompleteRegistryAndIndependentAuthentication(t *testing.T) {
	h, _ := fullManagementHarness(t)
	fixtureManagementChannel(t, h.store)
	reader, _ := h.client(t, management.Read)
	listed, err := reader.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if strings.HasPrefix(tool.Name, "update_") || tool.Name == "read_credential_key" {
			t.Fatalf("read-only discovery exposed write/secret tool %s", tool.Name)
		}
	}
	dataCall(t, reader, "gateway_overview", map[string]any{})
	dataCall(t, reader, "search_entities", map[string]any{"kind": "channel"})
	_, token, err := h.store.CreateClientKey("inference")
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"", token} {
		req, _ := http.NewRequest("POST", h.http.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		if header != "" {
			req.Header.Set("Authorization", "Bearer "+header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("inference/empty auth status %d", resp.StatusCode)
		}
	}
	manager, _ := h.client(t, management.Read, management.Manage)
	dataDenied(t, manager, "read_credential_key", map[string]any{"channel_id": 1, "credential_id": "cred_default"})
	dataDenied(t, manager, "delete_channel", dataWrite(map[string]any{"channel_id": 1, "expected_version": "x"}))
}

func TestManagementPatchPreservesSecretsAndRejectsDestinationTransfer(t *testing.T) {
	h, _ := fullManagementHarness(t)
	c := fixtureManagementChannel(t, h.store)
	client, _ := h.client(t, management.Read, management.Manage)
	before := fullReadChannel(t, client, c.ID)
	raw, _ := json.Marshal(before)
	if strings.Contains(string(raw), c.APIKey) {
		t.Fatal("search detail leaked stored key")
	}
	denied := dataCall(t, client, "update_channel", dataWrite(map[string]any{"channel_id": c.ID, "expected_version": before["version"], "patch": map[string]any{"base_url": "https://receiver.invalid", "endpoints": []any{map[string]any{"format": store.FormatOpenAIChatCompletions, "base_url": "https://receiver.invalid", "state": ""}}}}))
	if denied["status"] != "failed" {
		t.Fatalf("destination transfer must be known refusal: %v", denied)
	}
	after, _ := h.store.Channel(c.ID)
	if after.BaseURL != c.BaseURL || after.PrimaryKey() != c.PrimaryKey() {
		t.Fatal("refused change altered stored channel")
	}
	dataSuccess(t, client, "update_channel", dataWrite(map[string]any{"channel_id": c.ID, "expected_version": before["version"], "patch": map[string]any{"name": "Renamed"}}))
	after, _ = h.store.Channel(c.ID)
	if after.Name != "Renamed" || after.PrimaryKey() != c.PrimaryKey() {
		t.Fatal("narrow patch lost key")
	}
	stale := dataCall(t, client, "update_channel", dataWrite(map[string]any{"channel_id": c.ID, "expected_version": before["version"], "patch": map[string]any{"name": "Old"}}))
	if stale["status"] != "failed" {
		t.Fatalf("old patch was not refused: %v", stale)
	}
	after, _ = h.store.Channel(c.ID)
	if after.Name != "Renamed" {
		t.Fatal("stale patch overwrote current row")
	}
	settings := dataCall(t, client, "get_settings", map[string]any{})
	dataDenied(t, client, "update_settings", dataWrite(map[string]any{"expected_version": settings["version"], "patch": map[string]any{"gateway_key": "attacker-controlled-admin-root"}}))
	dataDenied(t, client, "update_channel", dataWrite(map[string]any{"channel_id": c.ID, "expected_version": store.EntityVersion(after), "patch": map[string]any{"oauth": map[string]any{"refresh_token": "forbidden"}}}))
}

func TestManagementCredentialsModelsAndMaintenancePreview(t *testing.T) {
	h, _ := fullManagementHarness(t)
	c := fixtureManagementChannel(t, h.store)
	client, _ := h.client(t, management.Read, management.Manage, management.Maintain)
	dataSuccess(t, client, "update_credentials", dataWrite(map[string]any{"channel_id": c.ID, "expected_version": store.EntityVersion(c), "items": []any{map[string]any{"id": "secondary", "label": "备用", "api_key": "fresh-test-key", "enabled": true}}}))
	c, _ = h.store.Channel(c.ID)
	if len(c.Credentials) != 2 {
		t.Fatal("credential not added")
	}
	dataSuccess(t, client, "reorder_credentials", dataWrite(map[string]any{"channel_id": c.ID, "credential_ids": []string{"secondary", "cred_default"}}))
	c, _ = h.store.Channel(c.ID)
	if c.Credentials[0].ID != "secondary" {
		t.Fatal("ordering not applied")
	}
	m := store.DownstreamModel{ID: "public-a", DisplayName: "公开", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "model-a", Enabled: true}}}
	dataSuccess(t, client, "create_public_model", dataWrite(map[string]any{"model": m}))
	models, _ := h.store.ListDownstreamModels()
	m = models[0]
	dataSuccess(t, client, "update_public_models", dataWrite(map[string]any{"items": []any{map[string]any{"id": m.ID, "expected_version": store.EntityVersion(m), "patch": map[string]any{"display_name": "New name"}}}}))
	dataCall(t, client, "get_public_model_specifications", map[string]any{})
	c, _ = h.store.Channel(c.ID)
	args := map[string]any{"channel_id": c.ID, "expected_version": store.EntityVersion(c)}
	preview := dataSuccess(t, client, "delete_channel", dataWrite(args))
	if preview["status"] != "preview" {
		t.Fatalf("missing preview: %v", preview)
	}
	if _, err := h.store.Channel(c.ID); err != nil {
		t.Fatal("preview performed delete")
	}
	c.Name = "Web change"
	if err := h.store.SaveChannelConfig(&c); err != nil {
		t.Fatal(err)
	}
	args["preview_id"] = preview["preview_id"]
	result := dataCall(t, client, "delete_channel", dataWrite(args))
	if result["status"] != "failed" {
		t.Fatalf("changed preview not refused: %v", result)
	}
	if _, err := h.store.Channel(c.ID); err != nil {
		t.Fatal("stale preview deleted row")
	}
}

func TestManagementServiceControlUsesOneDurableDeferredAction(t *testing.T) {
	h, calls := fullManagementHarness(t)
	client, _ := h.client(t, management.Read, management.Maintain)
	preview := dataSuccess(t, client, "restart_gateway", dataWrite(map[string]any{}))
	args := dataWrite(map[string]any{"preview_id": preview["preview_id"]})
	dataSuccess(t, client, "restart_gateway", args)
	deadline := time.After(2 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("deferred restart not called")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	dataSuccess(t, client, "restart_gateway", args)
	if calls.Load() != 1 {
		t.Fatal("replayed restart repeated hook")
	}
}

func TestManagementExplicitNullAndFrozenTagReferences(t *testing.T) {
	h, _ := fullManagementHarness(t)
	client, _ := h.client(t, management.Read, management.Manage, management.Maintain)
	v, err := h.store.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	max := int64(1000)
	temp := 0.5
	v.RequestDefaults.MaxOutputTokens = &max
	v.RequestDefaults.Temperature = &temp
	if err = h.store.SaveSettings(v); err != nil {
		t.Fatal(err)
	}
	settings := dataCall(t, client, "get_settings", map[string]any{})
	dataSuccess(t, client, "update_settings", dataWrite(map[string]any{"expected_version": settings["version"], "patch": map[string]any{"request_defaults": map[string]any{"max_output_tokens": nil}}}))
	v, err = h.store.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if v.RequestDefaults.MaxOutputTokens != nil || v.RequestDefaults.Temperature == nil || *v.RequestDefaults.Temperature != temp {
		t.Fatal("explicit null was omitted or cleared unrelated field")
	}
	if err = h.store.SaveTagGroup(store.TagGroup{ID: "custom", Name: "Custom", Type: store.TagGroupTypeUser}); err != nil {
		t.Fatal(err)
	}
	if err = h.store.SaveTag(store.Tag{ID: "custom:one", GroupID: "custom", Name: "One", Color: "blue"}); err != nil {
		t.Fatal(err)
	}
	c := fixtureManagementChannel(t, h.store)
	args := map[string]any{"id": "custom:one", "force": true}
	preview := dataSuccess(t, client, "delete_tag", dataWrite(args))
	c.Tags = []string{"custom:one"}
	if err = h.store.SaveChannelConfig(&c); err != nil {
		t.Fatal(err)
	}
	args["preview_id"] = preview["preview_id"]
	result := dataCall(t, client, "delete_tag", dataWrite(args))
	if result["status"] != "failed" {
		t.Fatalf("new references were not protected: %v", result)
	}
	c, err = h.store.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tags) != 1 || c.Tags[0] != "custom:one" {
		t.Fatal("preview removed newly added reference")
	}
}

func TestManagementDomainOutcomeClassification(t *testing.T) {
	if err := managementDomainError(&store.TagInUseError{}); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatal("tag reference refusal must be known conflict")
	}
	flow := oauthFlowResult(map[string]any{"status": "error", "code": "invalid_grant"}).(map[string]any)
	if flow["status"] != "failed" || flow["flow_status"] != "error" || flow["code"] != "invalid_grant" {
		t.Fatal(flow)
	}
	if result := upstreamActionResult(map[string]any{"ok": false, "detail": "catalog failed"}).(map[string]any); result["status"] != "failed" {
		t.Fatal(result)
	}
	for _, state := range []string{"failed", "unknown", "running", "succeeded"} {
		result := credentialActionResult(map[string]any{"last_reset": map[string]any{"state": state}}, "use_reset_card").(map[string]any)
		want := state
		if state == "running" {
			want = "unknown"
		}
		if state == "succeeded" {
			want = "completed"
		}
		if result["status"] != want {
			t.Fatalf("%s: %v", state, result)
		}
	}
	result := upstreamActionResult(map[string]any{"ok": false, "formats": []any{map[string]any{"ok": true}, map[string]any{"ok": false}}}).(map[string]any)
	if result["status"] != "partial" {
		t.Fatal(result)
	}
}

func TestManagementRejectsOAuthCopyAndMismatchedTagGroups(t *testing.T) {
	h, _ := fullManagementHarness(t)
	client, _ := h.client(t, management.Read, management.Manage, management.Sensitive)
	c := store.Channel{Name: "Synthetic OAuth", CredentialType: store.CredentialCodexOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		OAuth:     &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: "oaiapp_synthetic", Account: "synthetic-account", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Status: store.OAuthStatusActive}}
	if err := h.store.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c, _ = h.store.Channel(c.ID)
	before := store.EntityVersion(c)
	result := dataCall(t, client, "update_credentials", dataWrite(map[string]any{"channel_id": c.ID, "expected_version": before, "items": []any{map[string]any{"id": "copy", "source_channel_id": c.ID, "source_credential_id": "cred_default"}}}))
	if result["status"] != "failed" {
		t.Fatalf("OAuth copy accepted: %v", result)
	}
	after, _ := h.store.Channel(c.ID)
	if store.EntityVersion(after) != before {
		t.Fatal("refused OAuth copy changed channel")
	}
	dataDenied(t, client, "search_entities", map[string]any{"kind": "upstream_model", "tag_groups": map[string][]string{"billing": {"privacy:zdr"}}})
}

func TestManagementModelPatchReturnsPersistedTags(t *testing.T) {
	h, _ := fullManagementHarness(t)
	c := fixtureManagementChannel(t, h.store)
	m := store.DownstreamModel{ID: "patched", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "model-a", Enabled: true}}}
	if err := h.store.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	models, _ := h.store.ListDownstreamModels()
	patched, err := h.store.PatchPublicModels([]store.ModelEdit{{ID: m.ID, ExpectedVersion: store.EntityVersion(models[0]), Patch: json.RawMessage(`{"tags":["billing:plan"]}`)}})
	if err != nil {
		t.Fatal(err)
	}
	models, _ = h.store.ListDownstreamModels()
	if store.EntityVersion(patched[0]) != store.EntityVersion(models[0]) {
		t.Fatal("returned tags disagree with persisted public model")
	}
}
