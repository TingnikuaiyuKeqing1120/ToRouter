package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/store"
)

func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}
func testOperations(counter *atomic.Int32) []Operation {
	return []Operation{
		{Name: "a_read", Capability: "test.read", Permission: Read, ReadOnly: true, InputSchema: map[string]any{"type": "object", "additionalProperties": false}, Handler: func(context.Context, Principal, json.RawMessage) (any, error) {
			return map[string]any{"value": "visible"}, nil
		}},
		{Name: "z_write", Capability: "test.write", Permission: Manage, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []string{"value"}, "additionalProperties": false}, Handler: func(context.Context, Principal, json.RawMessage) (any, error) {
			return map[string]any{"writes": counter.Add(1)}, nil
		}},
	}
}
func testHTTP(t *testing.T, st *store.Store, ops []Operation) (*Server, *httptest.Server) {
	t.Helper()
	srv := New(st, "", ops)
	httpSrv := httptest.NewServer(srv.Handler())
	srv.origin = httpSrv.URL
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv
}
func testClient(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	t.Cleanup(base.CloseIdleConnections)
	client := mcp.NewClient(&mcp.Implementation{Name: "management-tests", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint + "/mcp", HTTPClient: &http.Client{Transport: &managementTransport{base: base, origin: endpoint, token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
func receipt(t *testing.T, result *mcp.CallToolResult) store.ManagementReceipt {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var r store.ManagementReceipt
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLocalIndependentAuthentication(t *testing.T) {
	st, _ := testStore(t)
	_, token, err := st.CreateManagementConnection("test", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, clientToken, err := st.CreateClientKey("client")
	if err != nil {
		t.Fatal(err)
	}
	srv, httpSrv := testHTTP(t, st, nil)
	for _, tc := range []struct {
		name, authorization, cookie, host, origin, remote string
		want                                              int
	}{
		{name: "missing", want: 401},
		{name: "model token", authorization: "Bearer " + clientToken, want: 401},
		{name: "cookie", cookie: "session=" + token, want: 401},
		{name: "host", authorization: "Bearer " + token, host: "evil.example", want: 403},
		{name: "origin", authorization: "Bearer " + token, origin: "https://evil.example", want: 403},
		{name: "null origin", authorization: "Bearer " + token, origin: "null", want: 403},
		{name: "remote", authorization: "Bearer " + token, remote: "192.0.2.1:4000", want: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", httpSrv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			r.RemoteAddr = "127.0.0.1:1234"
			if tc.host != "" {
				r.Host = tc.host
			}
			if tc.remote != "" {
				r.RemoteAddr = tc.remote
			}
			r.Header.Set("Authorization", tc.authorization)
			r.Header.Set("Cookie", tc.cookie)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d body %s", w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), clientToken) {
				t.Fatal("token leaked")
			}
		})
	}
	if list, err := st.ListManagementConnections(); err != nil || list[0].LastUsedAt != nil {
		t.Fatalf("rejected requests changed last used: %+v %v", list, err)
	}
}

func TestSDKPermissionsRevokeRotateAndSchema(t *testing.T) {
	st, _ := testStore(t)
	var count atomic.Int32
	srv, httpSrv := testHTTP(t, st, testOperations(&count))
	c, token, err := st.CreateManagementConnection("test", nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := testClient(t, httpSrv.URL, token)
	ctx := context.Background()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 2 || list.Tools[0].Name != "a_read" || list.Tools[1].Name != "z_write" {
		t.Fatalf("tools %+v", list.Tools)
	}
	invalid, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "z_write", Arguments: map[string]any{"request_id": "invalid", "value": "wrong"}})
	if err != nil || !invalid.IsError || count.Load() != 0 {
		t.Fatalf("invalid schema: %+v %v", invalid, err)
	}
	readOnly := []string{Read}
	if _, err = st.UpdateManagementConnection(c.ID, store.ManagementPatch{Permissions: &readOnly}); err != nil {
		t.Fatal(err)
	}
	list, err = cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 1 || list.Tools[0].Name != "a_read" {
		t.Fatalf("permission update: %+v %v", list, err)
	}
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "z_write", Arguments: map[string]any{"request_id": "forbidden", "value": 1}})
	if err == nil || count.Load() != 0 {
		t.Fatalf("hidden write was executable: %v", err)
	}
	p := Principal{ID: c.ID, Permissions: []string{Read, Manage}, store: st, token: token}
	if err = p.Require(ctx, Manage); err == nil {
		t.Fatal("stale principal retained manage")
	}
	_, newToken, err := st.RotateManagementConnection(c.ID)
	if err != nil || newToken == token {
		t.Fatal(err)
	}
	if err = p.Require(ctx, Read); err == nil {
		t.Fatal("rotated principal retained access")
	}
	if _, err = cs.ListTools(ctx, nil); err == nil {
		t.Fatal("old token remained valid")
	}
	newCS := testClient(t, httpSrv.URL, newToken)
	disabled := false
	if _, err = st.UpdateManagementConnection(c.ID, store.ManagementPatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err = newCS.ListTools(ctx, nil); err == nil {
		t.Fatal("disabled connection remained valid")
	}
	if len(srv.Operations()) != 2 {
		t.Fatal("registry changed with authorization")
	}
}

func TestDurableDedupUnknownAndSecrets(t *testing.T) {
	st, dir := testStore(t)
	var count atomic.Int32
	ops := testOperations(&count)
	ops = append(ops, Operation{Name: "unknown", Capability: "test.unknown", Permission: Manage, External: true, InputSchema: map[string]any{"type": "object", "additionalProperties": false}, Handler: func(context.Context, Principal, json.RawMessage) (any, error) {
		count.Add(1)
		return nil, errors.New("upstream secret synthetic_do_not_return")
	}})
	srv, httpSrv := testHTTP(t, st, ops)
	c, token, err := st.CreateManagementConnection("test", nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := testClient(t, httpSrv.URL, token)
	ctx := context.Background()
	call := func(name, id string, value any) *mcp.CallToolResult {
		t.Helper()
		args := map[string]any{"request_id": id}
		if value != nil {
			args["value"] = value
		}
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := receipt(t, call("z_write", "write-one", 1))
	second := receipt(t, call("z_write", "write-one", 1))
	if first.Status != "completed" || string(first.Result) != string(second.Result) || count.Load() != 1 {
		t.Fatalf("duplicate write %+v %+v count=%d", first, second, count.Load())
	}
	if conflict := call("z_write", "write-one", 2); !conflict.IsError || count.Load() != 1 {
		t.Fatalf("ID conflict %+v", conflict)
	}
	u1 := receipt(t, call("unknown", "uncertain", nil))
	u2 := receipt(t, call("unknown", "uncertain", nil))
	if u1.Status != "unknown" || u2.Status != "unknown" || count.Load() != 2 || bytes.Contains(u1.Result, []byte("synthetic_do_not_return")) {
		t.Fatalf("unknown replay leaked or executed twice %+v %+v", u1, u2)
	}
	_ = cs.Close()
	httpSrv.Close()
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	srv.store = reopened
	restored, err := reopened.ManagementReceipt(c.ID, "write-one")
	if err != nil || restored.Status != "completed" || string(restored.Result) != string(first.Result) {
		t.Fatalf("restart receipt %+v %v", restored, err)
	}
	_, restartedHTTP := testHTTP(t, reopened, ops)
	restartedClient := testClient(t, restartedHTTP.URL, token)
	unknownAfterRestart, err := restartedClient.CallTool(ctx, &mcp.CallToolParams{Name: "unknown", Arguments: map[string]any{"request_id": "uncertain"}})
	if err != nil || receipt(t, unknownAfterRestart).Status != "unknown" || count.Load() != 2 {
		t.Fatalf("restart re-executed unknown operation: %+v %v count=%d", unknownAfterRestart, err, count.Load())
	}
	r, claimed, err := reopened.BeginManagementReceipt(c.ID, "write-one", "z_write", "different", []string{Manage})
	if !errors.Is(err, store.ErrManagementConflict) || claimed {
		t.Fatalf("restart conflict %+v %v", r, err)
	}
	if err = reopened.CleanupManagement(ctx, time.Now().AddDate(0, 0, 31)); err != nil {
		t.Fatal(err)
	}
	expired, err := restartedClient.CallTool(ctx, &mcp.CallToolParams{Name: "z_write", Arguments: map[string]any{"request_id": "write-one", "value": 1}})
	if err != nil || receipt(t, expired).Status != "expired" || count.Load() != 2 {
		t.Fatalf("expired identity became a new write: %+v %v", expired, err)
	}
	for _, name := range []string{"router.db", "router.db-wal"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte("synthetic_do_not_return")) {
			t.Fatal("plaintext token/error persisted")
		}
	}
}

func TestPreviewOwnershipAndSensitiveReceiptRevocation(t *testing.T) {
	st, _ := testStore(t)
	c, token, err := st.CreateManagementConnection("privileged", []string{Read, Manage, Maintain, Sensitive})
	if err != nil {
		t.Fatal(err)
	}
	c2, token2, err := st.CreateManagementConnection("other", []string{Read, Maintain})
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{ID: c.ID, Permissions: c.Permissions, store: st, token: token}
	if strings.Contains(fmt.Sprintf("%+v %#v", p, p), token) {
		t.Fatal("principal formatting leaked bearer token")
	}
	p2 := Principal{ID: c2.ID, Permissions: c2.Permissions, store: st, token: token2}
	srv := New(st, "http://127.0.0.1:1234", nil)
	ctx := context.Background()
	id, err := srv.CreatePreview(ctx, p, "delete_channel", map[string]any{"targets": []int64{4}, "expected_revision": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = srv.ConsumePreview(ctx, p2, "delete_channel", id); err == nil {
		t.Fatal("other authorization consumed preview")
	}
	if _, err = srv.ConsumePreview(ctx, p, "delete_models", id); err == nil {
		t.Fatal("different operation consumed preview")
	}
	if _, err = srv.ConsumePreview(ctx, p, "delete_channel", id); err != nil {
		t.Fatal(err)
	}
	if _, err = srv.ConsumePreview(ctx, p, "delete_channel", id); err == nil {
		t.Fatal("preview reused")
	}
	op := Operation{Name: "manage_secret", Permission: Manage, Handler: func(context.Context, Principal, json.RawMessage) (any, error) {
		return map[string]any{"new_key": "synthetic_receipt_secret"}, nil
	}}
	if _, err = srv.invoke(ctx, p, op, json.RawMessage(`{"request_id":"sensitive-receipt"}`)); err != nil {
		t.Fatal(err)
	}
	perms := []string{Read, Manage, Maintain}
	if _, err = st.UpdateManagementConnection(c.ID, store.ManagementPatch{Permissions: &perms}); err != nil {
		t.Fatal(err)
	}
	if _, err = srv.Receipt(ctx, p, "sensitive-receipt"); err == nil {
		t.Fatal("sensitive result escaped via receipt")
	}
	if _, err = srv.invoke(ctx, p, op, json.RawMessage(`{"request_id":"sensitive-receipt"}`)); err == nil {
		t.Fatal("sensitive result escaped via duplicate")
	}
}

func TestAdminToolsUsesCurrentRegistryWithoutInvoking(t *testing.T) {
	st, _ := testStore(t)
	var calls atomic.Int32
	ops := testOperations(&calls)
	// Register a new tool and a shared capability, independent of the product's
	// current count; both entries must be discoverable without MCP connections.
	ops[0].Capability = "shared"
	ops[1].Capability = "shared"
	srv := New(st, "http://127.0.0.1:9876", ops)
	r := chi.NewRouter()
	srv.AdminRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/mcp/tools", nil))
	var got []struct {
		Name, Description, Capability, Permission string
		ReadOnly                                  bool `json:"read_only"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got) != len(ops) {
		t.Fatalf("unexpected catalogue: status=%d body=%s", w.Code, w.Body)
	}
	for i, op := range srv.Operations() {
		if got[i].Name != op.Name || got[i].Description != op.Description || got[i].Capability != op.Capability || got[i].Permission != op.Permission || got[i].ReadOnly != op.ReadOnly {
			t.Fatalf("catalogue differs from registered operation %q", op.Name)
		}
	}
	if calls.Load() != 0 || strings.Contains(w.Body.String(), "input_schema") || strings.Contains(w.Body.String(), "handler") {
		t.Fatal("browsing invoked an operation or included implementation material")
	}
	status := httptest.NewRecorder()
	r.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/mcp/status", nil))
	var counts struct {
		Enabled         bool `json:"enabled"`
		ToolCount       int  `json:"tool_count"`
		CapabilityCount int  `json:"capability_count"`
	}
	if json.Unmarshal(status.Body.Bytes(), &counts) != nil || counts.Enabled || counts.ToolCount != len(got) || counts.CapabilityCount != 1 {
		t.Fatalf("status/catalogue mismatch: %s", status.Body)
	}
}

func TestAdminConnectionLifecycleNoReveal(t *testing.T) {
	st, _ := testStore(t)
	srv := New(st, "http://127.0.0.1:9876", nil)
	r := chi.NewRouter()
	srv.AdminRoutes(r)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	status := request("GET", "/mcp/status", "")
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"enabled":false`) {
		t.Fatal(status.Body)
	}
	created := request("POST", "/mcp/connections", `{"name":"SDK client"}`)
	if created.Code != 201 {
		t.Fatal(created.Body)
	}
	var data struct {
		Connection store.ManagementConnection `json:"connection"`
		Token      string                     `json:"token"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	listed := request("GET", "/mcp/connections", "")
	if listed.Code != 200 || strings.Contains(listed.Body.String(), data.Token) || strings.Contains(listed.Body.String(), `"hash"`) {
		t.Fatal(listed.Body)
	}
	path := "/mcp/connections/1"
	patched := request("PATCH", path, `{"permissions":["read"],"enabled":false}`)
	if patched.Code != 200 || strings.Contains(patched.Body.String(), `"token"`) {
		t.Fatal(patched.Body)
	}
	if _, err := st.AuthenticateManagementConnection(data.Token); err == nil {
		t.Fatal("disabled")
	}
	rotated := request("POST", path+"/rotate", "")
	if rotated.Code != 200 || strings.Contains(rotated.Body.String(), data.Token) {
		t.Fatal(rotated.Body)
	}
	if deleted := request("DELETE", path, ""); deleted.Code != 204 {
		t.Fatal(deleted.Body)
	}
}

func TestSDKStdioHTTPBridge(t *testing.T) {
	st, _ := testStore(t)
	var count atomic.Int32
	_, httpSrv := testHTTP(t, st, testOperations(&count))
	_, token, err := st.CreateManagementConnection("stdio", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientTransport, bridgeTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- BridgeStdio(ctx, httpSrv.URL+"/mcp", token, bridgeTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-real-sdk", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 2 {
		t.Fatalf("bridge tools %+v %v", list, err)
	}
	for i := 0; i < 2; i++ {
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "z_write", Arguments: map[string]any{"request_id": "bridge-write", "value": 1}})
		if err != nil || result.IsError || receipt(t, result).Status != "completed" {
			t.Fatalf("bridge write %+v %v", result, err)
		}
	}
	if count.Load() != 1 {
		t.Fatal("bridge duplicated write")
	}
	_ = cs.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not stop")
	}
}

func TestArtifactsResourceAuthorizationExpiryAndEncryption(t *testing.T) {
	st, dir := testStore(t)
	c, token, err := st.CreateManagementConnection("sensitive export", []string{Read, Sensitive})
	if err != nil {
		t.Fatal(err)
	}
	other, otherToken, err := st.CreateManagementConnection("other", []string{Read, Sensitive})
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{ID: c.ID, Permissions: c.Permissions, store: st, token: token}
	p2 := Principal{ID: other.ID, Permissions: other.Permissions, store: st, token: otherToken}
	srv, httpSrv := testHTTP(t, st, nil)
	ctx := context.Background()
	a, err := srv.PutArtifact(ctx, p, "usage.csv", "text/csv", []byte("id,note\n1,synthetic_artifact_secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Size == 0 || a.URI != "torouter://exports/"+a.ID || time.Until(a.ExpiresAt) > time.Hour {
		t.Fatalf("metadata %+v", a)
	}
	if _, err = srv.ReadArtifact(ctx, p2, a.ID); err == nil {
		t.Fatal("artifact crossed authorization")
	}
	if _, err = srv.PutArtifact(ctx, p, "../../private", "text/csv", nil); err == nil {
		t.Fatal("artifact accepted path")
	}
	if _, err = srv.PutArtifact(ctx, p, "oversize.csv", "text/csv", make([]byte, store.ManagementArtifactMaxBytes+1)); err == nil {
		t.Fatal("oversized export accepted")
	}
	for i := 1; i < store.ManagementArtifactMaxCount; i++ {
		if _, err = srv.PutArtifact(ctx, p, "usage.csv", "text/csv", []byte("id\n1\n")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = srv.PutArtifact(ctx, p, "overflow.csv", "text/csv", []byte("id\n1\n")); err == nil {
		t.Fatal("active export limit bypassed")
	}
	cs := testClient(t, httpSrv.URL, token)
	resources, err := cs.ListResources(ctx, nil)
	if err != nil || len(resources.Resources) != 1 || resources.Resources[0].URI != "torouter://help" {
		t.Fatalf("help resource %+v %v", resources, err)
	}
	templates, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil || len(templates.ResourceTemplates) != 1 {
		t.Fatalf("export template %+v %v", templates, err)
	}
	result, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: a.URI})
	if err != nil || len(result.Contents) != 1 || !strings.Contains(result.Contents[0].Text, "synthetic_artifact_secret") {
		t.Fatalf("export resource %+v %v", result, err)
	}
	for _, file := range []string{"router.db", "router.db-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("synthetic_artifact_secret")) {
			t.Fatal("export persisted in plaintext")
		}
	}
	permissions := []string{Read}
	if _, err = st.UpdateManagementConnection(c.ID, store.ManagementPatch{Permissions: &permissions}); err != nil {
		t.Fatal(err)
	}
	if _, err = cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: a.URI}); err == nil {
		t.Fatal("resource bypassed revoked sensitive permission")
	}
	if err = st.CleanupManagement(ctx, time.Now().Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ReadManagementArtifact(ctx, c.ID, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired export was retained: %v", err)
	}
}

func TestStdioProtocolChild(t *testing.T) {
	if os.Getenv("TOROUTER_MCP_TEST_CHILD") != "1" {
		return
	}
	if err := RunStdioFromEnvironment(context.Background()); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRealStdioSubprocessProtocolOnly(t *testing.T) {
	st, _ := testStore(t)
	var count atomic.Int32
	_, httpSrv := testHTTP(t, st, testOperations(&count))
	_, token, err := st.CreateManagementConnection("subprocess", nil)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestStdioProtocolChild$")
	cmd.Env = append(os.Environ(), "TOROUTER_MCP_TEST_CHILD=1", "TOROUTER_MCP_URL="+httpSrv.URL+"/mcp", "TOROUTER_MCP_TOKEN="+token)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "subprocess-stdio-client", Version: "1"}, nil)
	// The race runtime itself waits one second during process exit. Keep the
	// SDK's normal grace period so this check observes exit instead of killing
	// a healthy instrumented child at that exact boundary.
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
	if err != nil {
		t.Fatalf("subprocess handshake failed: %v stderr=%s", err, stderr.String())
	}
	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 2 {
		t.Fatalf("subprocess tools %+v %v", list, err)
	}
	resources, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "torouter://help"})
	if err != nil || len(resources.Contents) != 1 {
		t.Fatalf("subprocess resource %+v %v", resources, err)
	}
	for i := 0; i < 2; i++ {
		out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "z_write", Arguments: map[string]any{"request_id": "subprocess-write", "value": 1}})
		if err != nil || out.IsError {
			t.Fatalf("subprocess write %+v %v", out, err)
		}
	}
	if count.Load() != 1 {
		t.Fatal("subprocess duplicate write")
	}
	if err = cs.Close(); err != nil {
		t.Fatalf("subprocess exit: %v stderr=%s", err, stderr.String())
	}
	if stderr.Len() != 0 || strings.Contains(stderr.String(), token) {
		t.Fatalf("unexpected child diagnostics: %s", stderr.String())
	}
}
