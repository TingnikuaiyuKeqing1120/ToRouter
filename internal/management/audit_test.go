package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestSensitiveAuditRetainsNonStandardCredentialIdentityWithoutPlaintext(t *testing.T) {
	st, _ := testStore(t)
	c, token, err := st.CreateManagementConnection("audit", []string{Read, Sensitive})
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{ID: c.ID, Permissions: c.Permissions, store: st, token: token}
	srv := New(st, "http://127.0.0.1:12345", nil)
	op := Operation{Name: "read_credential_key", Permission: Sensitive, ReadOnly: true, Handler: func(context.Context, Principal, json.RawMessage) (any, error) {
		return map[string]string{"api_key": "synthetic-secret"}, nil
	}}
	if _, err = srv.invoke(context.Background(), p, op, json.RawMessage(`{"channel_id":3,"credential_id":"secondary"}`)); err != nil {
		t.Fatal(err)
	}
	page, err := srv.Audit(context.Background(), p, 0, 50)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("audit %+v %v", page, err)
	}
	hash := sha256.Sum256([]byte("secondary"))
	if page.Items[0].Targets["credential_id_sha256"] != hex.EncodeToString(hash[:]) {
		t.Fatalf("missing stable selector: %+v", page.Items[0].Targets)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "secondary") || strings.Contains(string(raw), "synthetic-secret") {
		t.Fatal("audit retained raw identifier or result")
	}
}

func TestSensitiveAuthorizationAuditIsPrivateBoundedAndRetained(t *testing.T) {
	st, dir := testStore(t)
	c, token, err := st.CreateManagementConnection("audit", []string{Read, Sensitive})
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{ID: c.ID, Permissions: c.Permissions, store: st, token: token}
	srv := New(st, "http://127.0.0.1:12345", nil)
	ctx := context.Background()
	op := Operation{Name: "read_credential_key", Permission: Sensitive, ReadOnly: true, Handler: func(ctx context.Context, p Principal, _ json.RawMessage) (any, error) {
		if err := p.Require(ctx, Sensitive); err != nil {
			return nil, err
		}
		return map[string]string{"api_key": "synthetic_audit_secret"}, nil
	}}
	input := json.RawMessage(`{"channel_id":42,"credential_id":"cred_1","api_key":"synthetic_audit_secret","note":"synthetic_audit_secret","fields":["synthetic_audit_secret"]}`)
	if _, err = srv.invoke(ctx, p, op, input); err != nil {
		t.Fatal(err)
	}
	permissions := []string{Read}
	if _, err = st.UpdateManagementConnection(c.ID, store.ManagementPatch{Permissions: &permissions}); err != nil {
		t.Fatal(err)
	}
	if _, err = srv.invoke(ctx, p, op, input); err == nil {
		t.Fatal("revoked sensitive read accepted")
	}
	page, err := srv.Audit(ctx, p, 0, 2)
	if err != nil || len(page.Items) != 2 || !page.HasMore || page.NextCursor == 0 {
		t.Fatalf("audit page %+v %v", page, err)
	}
	if page.Items[0].Result != "denied" || page.Items[0].Operation != "read_credential_key" || page.Items[0].Fields[0] != "api_key" {
		t.Fatalf("audit selectors %+v", page.Items[0])
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "synthetic_audit_secret") {
		t.Fatal("audit stored raw secret")
	}
	next, err := srv.Audit(ctx, p, page.NextCursor, 2)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID >= page.NextCursor {
		t.Fatalf("audit cursor %+v %v", next, err)
	}
	other, _, err := st.CreateManagementConnection("other", []string{Read})
	if err != nil {
		t.Fatal(err)
	}
	if otherPage, err := st.ListManagementAudit(ctx, other.ID, 0, 50); err != nil || len(otherPage.Items) != 0 {
		t.Fatal("audit crossed owner")
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if saved, err := reopened.ListManagementAudit(ctx, c.ID, 0, 50); err != nil || len(saved.Items) != 3 {
		t.Fatalf("audit did not survive restart %+v %v", saved, err)
	}
	if err = reopened.CleanupManagement(ctx, time.Now().AddDate(0, 0, 31)); err != nil {
		t.Fatal(err)
	}
	if expired, err := reopened.ListManagementAudit(ctx, c.ID, 0, 50); err != nil || len(expired.Items) != 0 {
		t.Fatalf("audit retention failed %+v %v", expired, err)
	}
}
