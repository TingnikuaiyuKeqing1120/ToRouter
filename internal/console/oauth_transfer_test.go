package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/management"
	"torouter/internal/oauthtransfer"
	"torouter/internal/store"
)

const transferTestPassword = "synthetic-file-password-19"

func transferTestChannel(t *testing.T, st *store.Store, accounts ...string) store.Channel {
	t.Helper()
	c := store.Channel{Name: "Synthetic transfer source", CredentialType: store.CredentialCodexOAuth, Enabled: true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex", State: "stateless"}},
		Models:    []string{"source-model"}, AvailableModels: []string{"source-model"}}
	for index, account := range accounts {
		c.Credentials = append(c.Credentials, store.CredentialItem{ID: "fixture-member-" + strconv.Itoa(index), Label: "Fixture member " + strconv.Itoa(index), Enabled: true,
			OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, AccessToken: "synthetic-transfer-access-" + account,
				RefreshToken: "synthetic-transfer-refresh-" + account, IDToken: "synthetic-transfer-id-" + account,
				ClientID: "synthetic-transfer-client", Account: "synthetic-transfer-account-" + account,
				Subject: "synthetic-transfer-subject-" + account, AccountLabel: "synthetic-transfer-private-label-" + account,
				APIBase: c.Endpoints[0].BaseURL, Scopes: "openid profile email offline_access", Status: store.OAuthStatusActive,
				ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}})
	}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal("synthetic OAuth channel could not be saved")
	}
	c, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func transferTestSafe(t *testing.T, value any, channels ...store.Channel) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{transferTestPassword}
	for _, channel := range channels {
		for _, member := range channel.Credentials {
			if o := member.OAuth; o != nil {
				secrets = append(secrets, o.AccessToken, o.RefreshToken, o.IDToken, o.Account, o.Subject, o.AccountLabel)
			}
		}
	}
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			t.Fatal("transfer response, receipt, or audit exposed private material")
		}
	}
}

func transferTestPreview(t *testing.T, out any, channel store.Channel, status string) oauthtransfer.Preview {
	t.Helper()
	transferTestSafe(t, out, channel)
	var preview oauthtransfer.Preview
	discoveryDecode(t, out, &preview)
	if preview.ID == "" || len(preview.Rows) != 1 || preview.Rows[0].ID == "" || preview.Rows[0].Status != status || preview.Rows[0].CredentialType != channel.CredentialType {
		t.Fatal("transfer preview lacks the expected opaque candidate or status")
	}
	return preview
}

func transferTestSelection(preview oauthtransfer.Preview) []oauthtransfer.Selection {
	return []oauthtransfer.Selection{{CandidateID: preview.Rows[0].ID, Name: "Synthetic imported OAuth"}}
}

func transferTestImported(t *testing.T, st *store.Store, id int64, source store.CredentialItem) store.Channel {
	t.Helper()
	c, err := st.Channel(id)
	if err != nil || len(c.Credentials) != 1 || !sameTransferTokens(c.Credentials[0].OAuth, source.OAuth) || c.Credentials[0].ID == source.ID {
		t.Fatal("selected complete OAuth member was not imported under a new local ID")
	}
	if len(c.Models) != 0 || len(c.AvailableModels) != 0 || c.Credentials[0].Operations != nil {
		t.Fatal("transfer automatically installed models or credential actions")
	}
	routes, err := st.ListDownstreamModels()
	if err != nil || len(routes) != 0 {
		t.Fatal("transfer automatically created routing")
	}
	return c
}

func TestOAuthTransferHTTPNativeExactRefsAndImportLifecycle(t *testing.T) {
	source := newDiscoveryConsoleFixture(t)
	destination := newDiscoveryConsoleFixture(t)
	c := transferTestChannel(t, source.store, "http-first", "http-selected")
	before := store.EntityVersion(c)
	refs := []store.OAuthTransferRef{{ChannelID: c.ID, CredentialID: c.Credentials[1].ID}}
	exported := source.local(t, "POST", "/oauth-transfer/export", map[string]any{"refs": refs}, 200)

	var file oauthtransfer.Export
	discoveryDecode(t, exported, &file)
	if file.Count != 1 || file.File == "" || !strings.HasSuffix(file.Filename, ".json") {
		t.Fatal("HTTP export did not return one encrypted file")
	}
	materials, err := oauthtransfer.ParseNativeCredentials(context.Background(), []byte(file.File))
	if err != nil || len(materials) != 1 || !sameTransferTokens(materials[0].OAuth, c.Credentials[1].OAuth) {
		t.Fatal("encrypted file did not contain exactly the explicitly selected pool member")
	}
	source.local(t, "POST", "/oauth-transfer/export", map[string]any{"refs": []store.OAuthTransferRef{{ChannelID: c.ID, CredentialID: ""}}}, 400)
	destination.local(t, "POST", "/oauth-transfer/preview", map[string]any{"file": `{"format":"torouter-oauth-credentials"}`}, 400)
	prepared := destination.local(t, "POST", "/oauth-transfer/preview", map[string]any{"file": file.File}, 200)
	preview := transferTestPreview(t, prepared, c, "new")
	gotten := destination.local(t, "GET", "/oauth-transfer/sessions/"+preview.ID, nil, 200)
	transferTestSafe(t, gotten, c)
	var got oauthtransfer.Preview
	discoveryDecode(t, gotten, &got)
	if !reflect.DeepEqual(got, preview) {
		t.Fatal("HTTP owner could not retrieve the same preview")
	}
	imported := destination.local(t, "POST", "/oauth-transfer/sessions/"+preview.ID+"/import", map[string]any{"items": transferTestSelection(preview)}, 200)
	transferTestSafe(t, imported, c)
	var result oauthtransfer.ImportResponse
	discoveryDecode(t, imported, &result)
	if len(result.Results) != 1 || result.Results[0].Added != 1 || result.Results[0].Status != "completed" {
		t.Fatal("HTTP OAuth import did not complete")
	}
	saved := transferTestImported(t, destination.store, result.Results[0].ChannelID, c.Credentials[1])
	duplicate := transferTestPreview(t, destination.local(t, "POST", "/oauth-transfer/preview", map[string]any{"file": file.File}, 200), c, "exists")
	destination.local(t, "POST", "/oauth-transfer/sessions/"+duplicate.ID+"/import", map[string]any{"items": transferTestSelection(duplicate)}, 400)
	if after, _ := destination.store.Channel(saved.ID); store.EntityVersion(after) != store.EntityVersion(saved) {
		t.Fatal("existing account was overwritten or rotated by duplicate import")
	}

	// The remaining exact source member can append only to the preview's
	// compatible destination and version; its model configuration stays empty.
	second := source.local(t, "POST", "/oauth-transfer/export", map[string]any{"refs": []store.OAuthTransferRef{{ChannelID: c.ID, CredentialID: c.Credentials[0].ID}}}, 200)
	var secondFile oauthtransfer.Export
	discoveryDecode(t, second, &secondFile)
	appendPreview := transferTestPreview(t, destination.local(t, "POST", "/oauth-transfer/preview", map[string]any{"file": secondFile.File, "credential_type": c.CredentialType, "target_channel_id": saved.ID}, 200), c, "new")
	if len(appendPreview.Rows[0].Targets) != 1 || appendPreview.Rows[0].Targets[0].ID != saved.ID {
		t.Fatal("matching target not offered")
	}
	target := appendPreview.Rows[0].Targets[0]
	appended := destination.local(t, "POST", "/oauth-transfer/sessions/"+appendPreview.ID+"/import", map[string]any{"items": []oauthtransfer.Selection{{CandidateID: appendPreview.Rows[0].ID, TargetChannelID: target.ID, ExpectedVersion: target.Version}}}, 200)
	transferTestSafe(t, appended, c)
	var appendedResult oauthtransfer.ImportResponse
	discoveryDecode(t, appended, &appendedResult)
	if len(appendedResult.Results) != 1 || appendedResult.Results[0].Added != 1 || appendedResult.Results[0].ChannelID != saved.ID {
		t.Fatal("explicit target append failed")
	}
	after, _ := destination.store.Channel(saved.ID)
	if len(after.Credentials) != 2 || !reflect.DeepEqual(after.Credentials[0], saved.Credentials[0]) || !sameTransferTokens(after.Credentials[1].OAuth, c.Credentials[0].OAuth) || len(after.Models) != 0 {
		t.Fatal("append changed an existing member or installed source models")
	}
	destination.local(t, "DELETE", "/oauth-transfer/sessions/"+preview.ID, nil, 200)
	destination.local(t, "GET", "/oauth-transfer/sessions/"+preview.ID, nil, 404)
	destination.local(t, "POST", "/oauth-transfer/sessions/"+preview.ID+"/import", map[string]any{"items": transferTestSelection(preview)}, 404)
	if original, _ := source.store.Channel(c.ID); store.EntityVersion(original) != before {
		t.Fatal("export changed source credential or registration")
	}
}

func transferTestExternalFile(t *testing.T) (string, store.Channel) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := transferTestChannel(t, st, "external")
	file, err := oauthtransfer.New(st).Export(context.Background(), []store.OAuthTransferRef{{ChannelID: c.ID, CredentialID: c.Credentials[0].ID}})
	if err != nil {
		t.Fatal("synthetic cross-instance file export failed")
	}
	return file.File, c
}

func TestOAuthTransferMCPPermissionsPrivateArtifactsAndReceiptRevocation(t *testing.T) {
	f := newDiscoveryConsoleFixture(t)
	local := transferTestChannel(t, f.store, "mcp-first", "mcp-selected")
	file, external := transferTestExternalFile(t)
	manager, _ := f.client(t, management.Read, management.Manage)
	owner, ownerID := f.client(t, management.Read, management.Manage, management.Sensitive)
	other, _ := f.client(t, management.Read, management.Manage, management.Sensitive)
	operations := map[string]management.Operation{}
	for _, operation := range f.console.(consoleHandler).management.Operations() {
		if strings.HasPrefix(operation.Capability, "oauth_transfer.") {
			operations[operation.Name] = operation
		}
	}
	if len(operations) != 5 {
		t.Fatal("OAuth transfer tool registration incomplete")
	}
	for _, name := range []string{"export_oauth_credentials", "preview_oauth_import"} {
		op := operations[name]
		properties, _ := op.InputSchema["properties"].(map[string]any)
		if !op.ReadOnly || op.Permission != management.Manage || properties["request_id"] != nil || properties["password"] != nil {
			t.Fatal("password-bearing operation can persist a durable argument hash")
		}
	}
	properties, _ := operations["import_oauth_credentials"].InputSchema["properties"].(map[string]any)
	for _, forbidden := range []string{"password", "file", "oauth", "refs"} {
		if properties[forbidden] != nil {
			t.Fatal("commit schema accepts original file or plaintext material")
		}
	}
	refs := []store.OAuthTransferRef{{ChannelID: local.ID, CredentialID: local.Credentials[1].ID}}
	exportArgs := map[string]any{"refs": refs}
	prepareArgs := map[string]any{"file": file}
	discoveryToolDenied(t, f, manager, "export_oauth_credentials", exportArgs, "forbidden")
	discoveryToolDenied(t, f, manager, "preview_oauth_import", prepareArgs, "forbidden")

	exported := dataCall(t, owner, "export_oauth_credentials", exportArgs)
	transferTestSafe(t, exported, local)
	if exported["file"] != nil || exported["count"] != float64(1) {
		t.Fatal("MCP export did not return a bounded encrypted artifact reference")
	}
	var artifact management.Artifact
	discoveryDecode(t, exported["artifact"], &artifact)
	if artifact.ID == "" || artifact.URI == "" || artifact.Size <= 0 || artifact.MIMEType != "application/json" {
		t.Fatal("encrypted export artifact metadata incomplete")
	}
	var encrypted strings.Builder
	offset := 0
	for {
		part := dataCall(t, owner, "read_export", map[string]any{"id": artifact.ID, "offset": offset, "limit": 1024})

		content, ok := part["content"].(string)
		if !ok {
			t.Fatal("encrypted artifact chunk missing")
		}
		encrypted.WriteString(content)
		next, ok := part["next_offset"].(float64)
		if !ok || int(next) <= offset {
			t.Fatal("artifact chunk did not advance")
		}
		offset = int(next)
		if part["has_more"] == false {
			break
		}
	}
	materials, err := oauthtransfer.ParseNativeCredentials(context.Background(), []byte(encrypted.String()))
	if err != nil || len(materials) != 1 || !sameTransferTokens(materials[0].OAuth, local.Credentials[1].OAuth) {
		t.Fatal("MCP artifact did not retain the exact encrypted member")
	}
	resource, err := owner.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: artifact.URI})
	if err != nil || len(resource.Contents) != 1 || resource.Contents[0].Text != encrypted.String() {
		t.Fatal("artifact URI did not return the same encrypted file")
	}
	discoveryToolDenied(t, f, other, "read_export", map[string]any{"id": artifact.ID}, "not_found")

	// Explicit request IDs are rejected before reservation, including on reads
	// carrying passwords: neither a receipt nor an argument-hash oracle exists.
	for _, probe := range []struct {
		name string
		args map[string]any
	}{{"export_oauth_credentials", map[string]any{"refs": refs, "request_id": "password-export-probe"}},
		{"preview_oauth_import", map[string]any{"file": file, "request_id": "password-prepare-probe"}}} {
		discoveryToolDenied(t, f, owner, probe.name, probe.args, "")
		if _, err := f.store.ManagementReceipt(ownerID, probe.args["request_id"].(string)); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("password-bearing call left a durable receipt or argument hash")
		}
	}
	preview := transferTestPreview(t, dataCall(t, owner, "preview_oauth_import", prepareArgs), external, "new")
	discoveryToolDenied(t, f, manager, "get_oauth_import_preview", map[string]any{"preview_id": preview.ID}, "forbidden")
	discoveryToolDenied(t, f, manager, "import_oauth_credentials", dataWrite(map[string]any{"preview_id": preview.ID, "items": transferTestSelection(preview)}), "forbidden")
	discoveryToolDenied(t, f, other, "get_oauth_import_preview", map[string]any{"preview_id": preview.ID}, "not_found")
	discoveryToolDenied(t, f, other, "import_oauth_credentials", dataWrite(map[string]any{"preview_id": preview.ID, "items": transferTestSelection(preview)}), "not_found")
	gotten := dataCall(t, owner, "get_oauth_import_preview", map[string]any{"preview_id": preview.ID})
	transferTestSafe(t, gotten, external)
	var got oauthtransfer.Preview
	discoveryDecode(t, gotten, &got)
	if !reflect.DeepEqual(got, preview) {
		t.Fatal("MCP owner did not retrieve the same preview")
	}
	importArgs := dataWrite(map[string]any{"preview_id": preview.ID, "items": transferTestSelection(preview)})
	imported := dataSuccess(t, owner, "import_oauth_credentials", importArgs)
	transferTestSafe(t, imported, local, external)
	var result oauthtransfer.ImportResponse
	discoveryDecode(t, imported, &result)
	if len(result.Results) != 1 || result.Results[0].Added != 1 || result.Results[0].Status != "completed" {
		t.Fatal("authorized MCP exact selection did not import")
	}
	saved := transferTestImported(t, f.store, result.Results[0].ChannelID, external.Credentials[0])
	receipt := dataCall(t, owner, "get_operation_receipt", map[string]any{"request_id": importArgs["request_id"]})
	transferTestSafe(t, receipt, local, external)
	transferTestSafe(t, dataSuccess(t, owner, "import_oauth_credentials", importArgs), local, external)
	durable, err := f.store.ManagementReceipt(ownerID, importArgs["request_id"].(string))
	if err != nil || durable.Operation != "import_oauth_credentials" || durable.Status != "completed" {
		t.Fatal("safe import receipt not durably recorded")
	}
	transferTestSafe(t, durable, local, external)
	audit, err := f.store.ListManagementAudit(context.Background(), ownerID, 0, 200)
	if err != nil || len(audit.Items) == 0 {
		t.Fatal("management authorization audit unavailable")
	}
	transferTestSafe(t, audit, local, external)
	for _, entry := range audit.Items {
		raw, _ := json.Marshal(entry)
		if bytes.Contains(raw, []byte(file)) || bytes.Contains(raw, []byte(encrypted.String())) {
			t.Fatal("audit retained full encrypted file arguments")
		}
	}
	permissions := []string{management.Read, management.Manage}
	if _, err := f.store.UpdateManagementConnection(ownerID, store.ManagementPatch{Permissions: &permissions}); err != nil {
		t.Fatal(err)
	}
	discoveryToolDenied(t, f, owner, "export_oauth_credentials", exportArgs, "forbidden")
	discoveryToolDenied(t, f, owner, "preview_oauth_import", prepareArgs, "forbidden")
	discoveryToolDenied(t, f, owner, "get_oauth_import_preview", map[string]any{"preview_id": preview.ID}, "forbidden")
	discoveryToolDenied(t, f, owner, "import_oauth_credentials", dataWrite(map[string]any{"preview_id": preview.ID, "items": transferTestSelection(preview)}), "forbidden")
	discoveryToolDenied(t, f, owner, "import_oauth_credentials", importArgs, "forbidden")
	discoveryToolDenied(t, f, owner, "get_operation_receipt", map[string]any{"request_id": importArgs["request_id"]}, "forbidden")
	discoveryToolDenied(t, f, owner, "read_export", map[string]any{"id": artifact.ID}, "forbidden")
	if _, err := owner.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: artifact.URI}); err == nil {
		t.Fatal("resource URI bypassed revoked Sensitive")
	}
	if after, _ := f.store.Channel(saved.ID); store.EntityVersion(after) != store.EntityVersion(saved) {
		t.Fatal("revocation or receipt replay altered imported material")
	}
	cancelled := dataSuccess(t, owner, "cancel_oauth_import_preview", dataWrite(map[string]any{"preview_id": preview.ID}))
	if cancelled["cancelled"] != true {
		t.Fatal("harmless cancellation blocked by Sensitive revocation")
	}
	// Restore the capability only to distinguish cancelled ownership from a
	// permission failure. Cancellation cannot reactivate a consumed preview.
	permissions = append(permissions, management.Sensitive)
	if _, err := f.store.UpdateManagementConnection(ownerID, store.ManagementPatch{Permissions: &permissions}); err != nil {
		t.Fatal(err)
	}
	discoveryToolDenied(t, f, owner, "get_oauth_import_preview", map[string]any{"preview_id": preview.ID}, "not_found")
}

func sameTransferTokens(a, b *store.OAuthCredential) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.IDToken == b.IDToken && a.Account == b.Account
}
func TestOAuthTransferHTTPNativeSizeAndNoPasswordSchema(t *testing.T) {
	f := newDiscoveryConsoleFixture(t)
	raw := `{"tokens":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","id_token":"synthetic-id"},"extension":"` + strings.Repeat("x", (1<<20)+32) + `"}`
	out := f.local(t, "POST", "/oauth-transfer/preview", map[string]any{"file": raw, "credential_type": store.CredentialCodexOAuth}, 200)
	var preview oauthtransfer.Preview
	discoveryDecode(t, out, &preview)
	if len(preview.Rows) != 1 {
		t.Fatal("native file above old encrypted limit rejected")
	}
	f.local(t, "POST", "/oauth-transfer/preview", map[string]any{"file": raw, "password": "obsolete"}, 400)
	f.local(t, "POST", "/oauth-transfer/preview", map[string]any{"file": raw, "files": []string{raw}}, 400)
}
