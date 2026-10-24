package management

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestErrorWithPartialFactsSurvivesRevocationAndReplay(t *testing.T) {
	st, _ := testStore(t)
	connection, token, err := st.CreateManagementConnection("partial", []string{Read, Manage})
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{ID: connection.ID, Permissions: connection.Permissions, store: st, token: token}
	srv := New(st, "http://127.0.0.1:12345", nil)
	calls := 0
	op := Operation{Name: "partial_fact", Permission: Manage, Handler: func(context.Context, Principal, json.RawMessage) (any, error) {
		calls++
		disabled := false
		if _, err := st.UpdateManagementConnection(connection.ID, store.ManagementPatch{Enabled: &disabled}); err != nil {
			return nil, err
		}
		return map[string]any{"status": "partial", "items": []any{map[string]any{"status": "completed", "upstream_status": 200}, map[string]any{"status": "not_sent"}}}, NewError("forbidden", "Authorization revoked after the first result.")
	}}
	_, err = srv.invoke(context.Background(), p, op, json.RawMessage(`{"request_id":"actual-first"}`))
	var forbidden *Error
	if !errors.As(err, &forbidden) || forbidden.Code != "forbidden" {
		t.Fatalf("revoked result was exposed: %v", err)
	}
	saved, err := st.ManagementReceipt(p.ID, "actual-first")
	if err != nil || saved.Status != "partial" || !strings.Contains(string(saved.Result), `"upstream_status":200`) || !strings.Contains(string(saved.Result), `"not_sent"`) {
		t.Fatalf("facts were discarded: %+v %v", saved, err)
	}
	enabled := true
	if _, err = st.UpdateManagementConnection(connection.ID, store.ManagementPatch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	replayed, err := srv.invoke(context.Background(), p, op, json.RawMessage(`{"request_id":"actual-first"}`))
	if err != nil || replayed.(store.ManagementReceipt).Status != "partial" || calls != 1 {
		t.Fatalf("partial replay executed again: %+v %v calls=%d", replayed, err, calls)
	}
}
