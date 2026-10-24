package oauthtransfer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"torouter/internal/store"
)

func makeSyntheticJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body := base64.RawURLEncoding.EncodeToString(data)
	return header + "." + body + ".signature"
}

func TestParseNativeCredentials_CodexAuthJson(t *testing.T) {
	ctx := context.Background()
	jwt := makeSyntheticJWT(t, map[string]any{
		"sub":   "auth0|synthetic-user-1",
		"email": "user1@example.com",
		"exp":   time.Now().Add(2 * time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "org-workspace-1",
		},
	})

	// Format 1: Nested tokens in official auth.json
	nestedJSON := `{
		"auth_mode": "chatgpt",
		"OPENAI_API_KEY": null,
		"tokens": {
			"access_token": "` + jwt + `",
			"refresh_token": "rt-synthetic-1",
			"id_token": "` + jwt + `",
			"account_id": "org-workspace-1"
		},
		"last_refresh": "2026-10-21T00:00:00Z"
	}`

	items, err := ParseNativeCredentials(ctx, []byte(nestedJSON))
	if err != nil {
		t.Fatalf("ParseNativeCredentials failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.CredentialType != store.CredentialCodexOAuth {
		t.Errorf("expected CredentialCodexOAuth, got %s", item.CredentialType)
	}
	if item.OAuth.Subject != "auth0|synthetic-user-1" {
		t.Errorf("expected subject auth0|synthetic-user-1, got %s", item.OAuth.Subject)
	}
	if item.OAuth.Account != "org-workspace-1" {
		t.Errorf("expected account org-workspace-1, got %s", item.OAuth.Account)
	}
	if item.OAuth.RefreshToken != "rt-synthetic-1" {
		t.Errorf("expected refresh token rt-synthetic-1, got %s", item.OAuth.RefreshToken)
	}

	// Format 2: Flat tokens in auth.json
	flatJSON := `{
		"access_token": "` + jwt + `",
		"refresh_token": "rt-synthetic-2",
		"id_token": "` + jwt + `"
	}`
	items2, err := ParseNativeCredentials(ctx, []byte(flatJSON))
	if err != nil {
		t.Fatalf("ParseNativeCredentials flat failed: %v", err)
	}
	if len(items2) != 1 || items2[0].OAuth.RefreshToken != "rt-synthetic-2" {
		t.Fatalf("expected 1 flat item, got %v", items2)
	}
}

func TestParseNativeCredentials_CodexManagerExport(t *testing.T) {
	ctx := context.Background()
	jwt1 := makeSyntheticJWT(t, map[string]any{"sub": "user-cm-1", "exp": time.Now().Add(time.Hour).Unix()})
	jwt2 := makeSyntheticJWT(t, map[string]any{"sub": "user-cm-2", "exp": time.Now().Add(time.Hour).Unix()})

	managerJSON := `[
		{
			"tokens": {
				"access_token": "` + jwt1 + `",
				"id_token": "` + jwt1 + `",
				"refresh_token": "rt-cm-1",
				"account_id": "internal-uuid-do-not-use"
			},
			"meta": {
				"label": "My Personal Codex",
				"note": "Work Laptop",
				"issuer": "https://auth.openai.com",
				"status": "active",
				"workspaceId": "workspace-shared",
				"chatgptAccountId": "workspace-shared"
			}
		},
		{
			"tokens": {
				"access_token": "` + jwt2 + `",
				"id_token": "` + jwt2 + `",
				"refresh_token": "rt-cm-2",
				"account_id": "internal-uuid-2"
			},
			"meta": {
				"label": "user2@company.com",
				"note": "Backup Key",
				"issuer": "https://auth.openai.com",
				"status": "disabled",
				"workspaceId": "workspace-shared",
				"chatgptAccountId": "workspace-shared"
			}
		}
	]`

	items, err := ParseNativeCredentials(ctx, []byte(managerJSON))
	if err != nil {
		t.Fatalf("ParseNativeCredentials manager failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// First item: both label and note preserved, enabled
	if items[0].Label != "My Personal Codex (Work Laptop)" {
		t.Errorf("expected combined label 'My Personal Codex (Work Laptop)', got %q", items[0].Label)
	}
	if !items[0].Enabled {
		t.Errorf("expected enabled true for first item")
	}
	if items[0].OAuth.Account != "workspace-shared" {
		t.Errorf("expected account 'workspace-shared', got %q", items[0].OAuth.Account)
	}

	// Second item: label was email, so fallback to note 'Backup Key' without using email as label
	if items[1].Label != "Backup Key" {
		t.Errorf("expected label 'Backup Key' (fallback from email), got %q", items[1].Label)
	}
	if items[1].Enabled {
		t.Errorf("expected enabled false because status was disabled")
	}

	// Mixed invalid items should be reported rather than silently skipped
	mixedJSON := `[
		{"tokens": {"access_token": "` + jwt1 + `", "refresh_token": "rt-valid", "id_token": "` + jwt1 + `"}},
		{"tokens": {"access_token": "", "refresh_token": ""}}
	]`
	_, mixedErr := ParseNativeCredentials(ctx, []byte(mixedJSON))
	if mixedErr == nil {
		t.Fatal("expected error on mixed file with invalid entry, got nil")
	}
}

func TestPrepare_NativeImportWithoutPassword(t *testing.T) {
	st := newStore(t)
	svc := New(st)
	ctx := context.Background()

	jwt := makeSyntheticJWT(t, map[string]any{"sub": "user-native", "exp": time.Now().Add(time.Hour).Unix()})
	nativeFile := `{
		"access_token": "` + jwt + `",
		"refresh_token": "rt-native-test",
		"id_token": "` + jwt + `"
	}`

	// Native credentials are parsed directly without a transfer password.
	preview, err := svc.Prepare(ctx, "test-owner", nativeFile, "")
	if err != nil {
		t.Fatalf("Prepare native file failed: %v", err)
	}
	if len(preview.Rows) != 1 {
		t.Fatalf("expected 1 row in preview, got %d", len(preview.Rows))
	}
	if preview.Rows[0].CredentialType != store.CredentialCodexOAuth {
		t.Errorf("expected CredentialCodexOAuth, got %s", preview.Rows[0].CredentialType)
	}
}

func TestDeduplication_MultipleUsersSameWorkspace(t *testing.T) {
	st := newStore(t)
	svc := New(st)
	ctx := context.Background()

	jwt1 := makeSyntheticJWT(t, map[string]any{"sub": "user-alpha", "exp": time.Now().Add(time.Hour).Unix()})
	jwt2 := makeSyntheticJWT(t, map[string]any{"sub": "user-beta", "exp": time.Now().Add(time.Hour).Unix()})

	// Two distinct users in the same workspace should BOTH be previewed and neither skipped
	batch := `[
		{
			"tokens": {"access_token": "` + jwt1 + `", "id_token": "` + jwt1 + `", "refresh_token": "rt-alpha"},
			"meta": {"workspace_id": "shared-workspace", "chatgpt_account_id": "shared-workspace"}
		},
		{
			"tokens": {"access_token": "` + jwt2 + `", "id_token": "` + jwt2 + `", "refresh_token": "rt-beta"},
			"meta": {"workspace_id": "shared-workspace", "chatgpt_account_id": "shared-workspace"}
		}
	]`

	preview, err := svc.Prepare(ctx, "test-owner", batch, "")
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if len(preview.Rows) != 2 {
		t.Fatalf("expected 2 distinct rows for different users in same workspace, got %d", len(preview.Rows))
	}
	for _, row := range preview.Rows {
		if row.Status != "new" {
			t.Errorf("expected row status 'new', got %s", row.Status)
		}
	}
}
