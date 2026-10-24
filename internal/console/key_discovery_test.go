package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/auth"
	"torouter/internal/keydiscovery"
	"torouter/internal/management"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

type discoveryConsoleFixture struct {
	*dataHarness
	handler                  http.Handler
	console                  http.Handler
	manager                  *plugins.Manager
	root, key, origin, admin string
}

// Real console registration and Store; only the source directory is synthetic.
// No OAuth start endpoint is called, so no fixed callback listener is opened.
func newDiscoveryConsoleFixture(t *testing.T) *discoveryConsoleFixture {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal("fixture store failed")
	}
	t.Cleanup(func() { st.Close() })
	manager, err := plugins.New(st)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	console := NewWithPlugins(st, origin, Hooks{}, manager)
	t.Cleanup(func() { ReleaseOAuthListeners(console) })
	mux := http.NewServeMux()
	mux.Handle("/mcp", ManagementHandler(console))
	mux.Handle("/api/", auth.New(st).RequireAdmin(http.StripPrefix("/api", console)))
	server.Config.Handler = mux
	server.Start()
	t.Cleanup(server.Close)
	settings, err := st.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := st.CreateAdminSession(settings.GatewayKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	key := "synthetic-discovery-private-middle-value-tail"
	config := "model_provider = \"fixture\"\nmodel = \"fixture-preselected-model\"\n[model_providers.fixture]\nbase_url = \"https://discovery.example.invalid/v1\"\nwire_api = \"responses\"\nexperimental_bearer_token = \"" + key + "\"\n"
	if err = os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal("synthetic source creation failed")
	}
	return &discoveryConsoleFixture{dataHarness: &dataHarness{store: st, http: server}, handler: mux, console: console, manager: manager, root: root, key: key, origin: origin, admin: admin}
}
func (f *discoveryConsoleFixture) safe(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	rootJSON, _ := json.Marshal(f.root)
	for _, private := range []string{f.key, f.root, filepath.ToSlash(f.root), string(rootJSON[1 : len(rootJSON)-1])} {
		if private != "" && bytes.Contains(raw, []byte(private)) {
			t.Fatal("discovery response exposed private key or source root")
		}
	}
}
func (f *discoveryConsoleFixture) request(t *testing.T, method, path string, input any, peer, origin string, want int) map[string]any {
	t.Helper()
	var raw []byte
	var err error
	if input != nil {
		raw, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, f.origin+"/api"+path, bytes.NewReader(raw))
	req.RemoteAddr = peer
	req.Header.Set("X-To-Router", "1")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "to_router_session", Value: f.admin})
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	var out map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("HTTP response was not JSON: status %d", w.Code)
	}
	f.safe(t, out)
	if w.Code != want {
		t.Fatalf("%s %s: HTTP %d want %d", method, path, w.Code, want)
	}
	return out
}
func (f *discoveryConsoleFixture) local(t *testing.T, method, path string, input any, want int) map[string]any {
	t.Helper()
	return f.request(t, method, path, input, "127.0.0.1:41000", f.origin, want)
}
func discoveryDecode(t *testing.T, value any, target any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}
func discoveryRow(t *testing.T, preview keydiscovery.Preview) keydiscovery.Row {
	t.Helper()
	if len(preview.Groups) != 1 || len(preview.Groups[0].Rows) != 1 {
		t.Fatal("synthetic source did not produce one candidate")
	}
	row := preview.Groups[0].Rows[0]
	if row.ID == "" || row.MaskedKey == "" || row.Status != "new" || !reflect.DeepEqual(row.Formats, []string{store.FormatOpenAIResponses}) {
		t.Fatal("candidate lost opaque reference, mask, protocol, or new status")
	}
	return row
}
func (f *discoveryConsoleFixture) imported(t *testing.T, id int64) store.Channel {
	t.Helper()
	channels, err := f.store.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatal("import did not produce exactly one channel")
	}
	c := channels[0]
	if c.ID != id || c.CredentialType != store.CredentialAPIKey || c.PrimaryKey() != f.key || !c.Enabled || len(c.Credentials) != 1 || len(c.Models) != 0 || len(c.AvailableModels) != 0 || len(c.Endpoints) != 1 || c.Endpoints[0].Format != store.FormatOpenAIResponses {
		t.Fatal("import lost complete key/protocol or automatically enabled models")
	}
	models, err := f.store.ListDownstreamModels()
	if err != nil || len(models) != 0 {
		t.Fatal("discovery import created public models or routing")
	}
	return c
}

// Writes may be rejected as a durable failed receipt instead of an MCP protocol
// error. Both must carry the permission refusal, never a completed operation.
func discoveryToolDenied(t *testing.T, f *discoveryConsoleFixture, client *mcp.ClientSession, name string, args map[string]any, code string) {
	t.Helper()
	out, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		if code != "" && !strings.Contains(err.Error(), code) {
			t.Fatalf("%s failed for a reason other than %s", name, code)
		}
		return
	}
	f.safe(t, out)
	if out.IsError {
		raw, _ := json.Marshal(out)
		if code != "" && !strings.Contains(string(raw), code) {
			t.Fatalf("%s MCP error did not identify %s", name, code)
		}
		return
	}
	var value map[string]any
	discoveryDecode(t, out.StructuredContent, &value)
	if value["status"] != "failed" {
		t.Fatalf("%s was not refused", name)
	}
	body, ok := value["result"].(map[string]any)
	if !ok {
		t.Fatal("failed receipt lacks its reason")
	}
	reason, ok := body["error"].(map[string]any)
	if !ok || code != "" && reason["code"] != code {
		t.Fatalf("%s receipt does not contain expected refusal", name)
	}
}

func TestKeyDiscoveryHTTPNoScopeAndRedactedImportLifecycle(t *testing.T) {
	f := newDiscoveryConsoleFixture(t)
	sources := f.local(t, "GET", "/key-discovery/sources", nil, 200)
	encoded, _ := json.Marshal(sources)
	for _, field := range []string{"root", "files", "default_hint", "authorized"} {
		if bytes.Contains(encoded, []byte(`"`+field+`"`)) {
			t.Fatal("obsolete scope or path exposed")
		}
	}
	begun := f.local(t, "POST", "/key-discovery/sessions", map[string]any{"source_ids": []string{"source-codex"}}, 200)
	var preview keydiscovery.Preview
	discoveryDecode(t, begun, &preview)
	if preview.ID == "" || len(preview.Sources) != 1 || preview.Sources[0].Status != "pending" || len(preview.Groups) != 0 {
		t.Fatal("Begin unexpectedly scanned or lost session status")
	}
	id := preview.ID
	t.Cleanup(func() { f.local(t, "DELETE", "/key-discovery/sessions/"+id, nil, 200) })
	scanned := f.local(t, "POST", "/key-discovery/sessions/"+id+"/scan", map[string]any{"source_id": "source-codex"}, 200)
	discoveryDecode(t, scanned, &preview)
	row := discoveryRow(t, preview)
	if row.MaskedKey != store.MaskKey(f.key) || preview.Sources[0].Status != "complete" {
		t.Fatal("scan did not retain safe mask or completion")
	}
	// Explicit copy is the only route returning complete material.
	rawCopy, _ := json.Marshal(map[string]any{"candidate_ids": []string{row.ID}})
	req := httptest.NewRequest("POST", f.origin+"/api/key-discovery/sessions/"+id+"/copy", bytes.NewReader(rawCopy))
	req.Header.Set("X-To-Router", "1")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "to_router_session", Value: f.admin})
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	var copied keydiscovery.CopyResponse
	if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &copied) != nil || len(copied.Items) != 1 || copied.Items[0].APIKey != f.key {
		t.Fatal("explicit HTTP copy lost full material")
	}
	gotten := f.local(t, "GET", "/key-discovery/sessions/"+id, nil, 200)
	var getPreview keydiscovery.Preview
	discoveryDecode(t, gotten, &getPreview)
	if !reflect.DeepEqual(getPreview, preview) {
		t.Fatal("GET did not return the same preview")
	}
	channels, _ := f.store.ListChannels()
	if len(channels) != 0 {
		t.Fatal("scanning changed channel storage")
	}
	imported := f.local(t, "POST", "/key-discovery/sessions/"+id+"/import", map[string]any{"items": []keydiscovery.Selection{{CandidateID: row.ID, Name: "Synthetic imported channel"}}}, 200)
	var result keydiscovery.ImportResponse
	discoveryDecode(t, imported, &result)
	if len(result.Results) != 1 || result.Results[0].Added != 1 || result.Results[0].Status != "imported" {
		t.Fatal("import returned no completed addition")
	}
	f.imported(t, result.Results[0].ChannelID)
	cancelled := f.local(t, "DELETE", "/key-discovery/sessions/"+id, nil, 200)
	if cancelled["cancelled"] != true {
		t.Fatal("Cancel not confirmed")
	}
	t.Run("cancel invalidates preview", func(t *testing.T) { f.local(t, "GET", "/key-discovery/sessions/"+id, nil, 404) })
	f.imported(t, result.Results[0].ChannelID)
}

func TestKeyDiscoveryMCPPermissionsOwnershipRevocationAndReceipts(t *testing.T) {
	f := newDiscoveryConsoleFixture(t)
	reader, _ := f.client(t, management.Read)
	manager, _ := f.client(t, management.Read, management.Manage)
	owner, ownerID := f.client(t, management.Read, management.Manage, management.Sensitive)
	other, _ := f.client(t, management.Read, management.Manage, management.Sensitive)
	readerTools, err := reader.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range readerTools.Tools {
		if tool.Name == "scan_api_keys" || tool.Name == "get_api_key_preview" || tool.Name == "import_api_keys" || tool.Name == "copy_credentials" {
			t.Fatal("read-only connection advertises management discovery operations")
		}
	}
	expectedTools := map[string]bool{"list_api_key_sources": true, "scan_api_keys": true, "get_api_key_preview": true, "cancel_api_key_preview": true, "import_api_keys": true, "copy_credentials": true}
	registered := map[string]bool{}
	for _, operation := range f.console.(consoleHandler).management.Operations() {
		if strings.HasPrefix(operation.Capability, "key_discovery.") {
			if !expectedTools[operation.Name] {
				t.Fatal("directory authorization unexpectedly exposed through MCP")
			}
			registered[operation.Name] = true
			properties, _ := operation.InputSchema["properties"].(map[string]any)
			if _, hasRoot := properties["root"]; hasRoot {
				t.Fatal("discovery tool can expand authorized roots")
			}
		}
	}
	if !reflect.DeepEqual(registered, expectedTools) {
		t.Fatal("discovery operation registration is incomplete")
	}
	sourceList := dataCall(t, reader, "list_api_key_sources", map[string]any{})
	f.safe(t, sourceList)
	scanArgs := dataWrite(map[string]any{"source_ids": []string{"source-codex"}})
	scanned := dataSuccess(t, owner, "scan_api_keys", scanArgs)
	f.safe(t, scanned)
	var preview keydiscovery.Preview
	discoveryDecode(t, scanned, &preview)
	row := discoveryRow(t, preview)
	t.Cleanup(func() {
		owner.CallTool(context.Background(), &mcp.CallToolParams{Name: "cancel_api_key_preview", Arguments: dataWrite(map[string]any{"preview_id": preview.ID})})
	})
	for _, client := range []*mcp.ClientSession{reader, manager} {
		code := "forbidden"
		if client == reader {
			code = ""
		}
		discoveryToolDenied(t, f, client, "scan_api_keys", dataWrite(map[string]any{"source_ids": []string{"source-codex"}}), code)
		discoveryToolDenied(t, f, client, "get_api_key_preview", map[string]any{"preview_id": preview.ID}, code)
		discoveryToolDenied(t, f, client, "import_api_keys", dataWrite(map[string]any{"preview_id": preview.ID, "items": []keydiscovery.Selection{{CandidateID: row.ID}}}), code)
	}
	for _, client := range []*mcp.ClientSession{reader, manager} {
		code := "forbidden"
		if client == reader {
			code = ""
		}
		discoveryToolDenied(t, f, client, "copy_credentials", map[string]any{"preview_id": preview.ID, "candidate_ids": []string{row.ID}}, code)
	}
	discoveryToolDenied(t, f, other, "copy_credentials", map[string]any{"preview_id": preview.ID, "candidate_ids": []string{row.ID}}, "not_found")
	copied := dataCall(t, owner, "copy_credentials", map[string]any{"preview_id": preview.ID, "candidate_ids": []string{row.ID}})
	var copiedMaterials keydiscovery.CopyResponse
	discoveryDecode(t, copied, &copiedMaterials)
	if len(copiedMaterials.Items) != 1 || copiedMaterials.Items[0].APIKey != f.key {
		t.Fatal("authorized MCP copy failed")
	}
	channels, _ := f.store.ListChannels()
	if len(channels) != 0 {
		t.Fatal("unprivileged MCP import wrote credentials")
	}
	discoveryToolDenied(t, f, other, "get_api_key_preview", map[string]any{"preview_id": preview.ID}, "not_found")
	gotten := dataCall(t, owner, "get_api_key_preview", map[string]any{"preview_id": preview.ID})
	f.safe(t, gotten)
	var got keydiscovery.Preview
	discoveryDecode(t, gotten, &got)
	if !reflect.DeepEqual(got, preview) {
		t.Fatal("owner could not read the same private session")
	}
	importArgs := dataWrite(map[string]any{"preview_id": preview.ID, "items": []keydiscovery.Selection{{CandidateID: row.ID, Name: "MCP imported synthetic channel"}}})
	imported := dataSuccess(t, owner, "import_api_keys", importArgs)
	f.safe(t, imported)
	var response keydiscovery.ImportResponse
	discoveryDecode(t, imported, &response)
	if len(response.Results) != 1 || response.Results[0].Added != 1 {
		t.Fatal("authorized MCP import failed")
	}
	saved := f.imported(t, response.Results[0].ChannelID)
	receipt := dataCall(t, owner, "get_operation_receipt", map[string]any{"request_id": importArgs["request_id"]})
	f.safe(t, receipt)
	replay := dataSuccess(t, owner, "import_api_keys", importArgs)
	f.safe(t, replay)
	f.imported(t, saved.ID)
	permissions := []string{management.Read, management.Manage}
	if _, err := f.store.UpdateManagementConnection(ownerID, store.ManagementPatch{Permissions: &permissions}); err != nil {
		t.Fatal(err)
	}
	discoveryToolDenied(t, f, owner, "get_api_key_preview", map[string]any{"preview_id": preview.ID}, "forbidden")
	discoveryToolDenied(t, f, owner, "import_api_keys", dataWrite(map[string]any{"preview_id": preview.ID, "items": []keydiscovery.Selection{{CandidateID: row.ID}}}), "forbidden")
	discoveryToolDenied(t, f, owner, "scan_api_keys", scanArgs, "forbidden")
	discoveryToolDenied(t, f, owner, "import_api_keys", importArgs, "forbidden")
	discoveryToolDenied(t, f, owner, "get_operation_receipt", map[string]any{"request_id": importArgs["request_id"]}, "forbidden")
	snapshot, err := f.manager.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.SetCapability(context.Background(), "source-codex", "discover_api_key", false, snapshot.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.SourceEpoch("source-codex"); !errors.Is(err, plugins.ErrDisabled) {
		t.Fatal("source plugin not disabled")
	}
	retained := f.imported(t, saved.ID)
	if store.EntityVersion(retained) != store.EntityVersion(saved) {
		t.Fatal("revoking read consent or source plugin changed imported credentials")
	}
	detail := fullReadChannel(t, manager, saved.ID)
	f.safe(t, detail)
	cancelled := dataSuccess(t, owner, "cancel_api_key_preview", dataWrite(map[string]any{"preview_id": preview.ID}))
	f.safe(t, cancelled)
	if cancelled["cancelled"] != true {
		t.Fatal("revoked Sensitive prevented harmless owned-preview cancellation")
	}
}
