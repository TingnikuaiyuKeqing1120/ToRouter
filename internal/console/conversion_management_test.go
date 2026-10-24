package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"torouter/internal/management"
	"torouter/internal/store"
)

func TestConversionSettingsHTTPRejectsInvalidPolicyWithoutOverwriting(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := New(s, "http://127.0.0.1:17888", Hooks{})
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	get := func() map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("settings read failed: %d", w.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	initial := get()
	if initial["reasoning_policy"] != store.ReasoningPreserve {
		t.Fatal("HTTP settings omitted the resolved default policy")
	}
	put := func(value map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		delete(value, "prices")
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(string(raw))))
		return w
	}
	draft := get()
	draft["reasoning_policy"] = store.ReasoningAvailability
	if w := put(draft); w.Code != http.StatusOK {
		t.Fatalf("accepted policy failed: %d %s", w.Code, w.Body.String())
	}
	accepted := get()
	if accepted["reasoning_policy"] != store.ReasoningAvailability || accepted["gateway_key"] != initial["gateway_key"] {
		t.Fatal("HTTP update lost the accepted policy or changed the login key")
	}
	for _, invalid := range []string{"drop_all", "availability-now"} {
		draft = get()
		draft["reasoning_policy"] = invalid
		draft["max_attempts"] = accepted["max_attempts"].(float64) + 1
		if w := put(draft); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid policy returned HTTP %d", w.Code)
		}
		if after := get(); !reflect.DeepEqual(after, accepted) {
			t.Fatal("rejected HTTP policy partially overwrote settings")
		}
	}
}

func TestConversionSettingsMCPHonorsVersionAndManagePermission(t *testing.T) {
	h, _ := fullManagementHarness(t)
	reader, _ := h.client(t, management.Read)
	manager, _ := h.client(t, management.Read, management.Manage)
	initial := dataCall(t, manager, "get_settings", map[string]any{})
	safe := initial["settings"].(map[string]any)
	if safe["reasoning_policy"] != store.ReasoningPreserve {
		t.Fatal("MCP settings omitted the default policy")
	}
	if _, exposed := safe["gateway_key"]; exposed {
		t.Fatal("MCP settings exposed the login key")
	}
	dataDenied(t, reader, "update_settings", dataWrite(map[string]any{"expected_version": initial["version"], "patch": map[string]any{"reasoning_policy": store.ReasoningAvailability}}))
	if after := dataCall(t, manager, "get_settings", map[string]any{}); !reflect.DeepEqual(after, initial) {
		t.Fatal("read-only connection changed settings")
	}
	updated := dataSuccess(t, manager, "update_settings", dataWrite(map[string]any{"expected_version": initial["version"], "patch": map[string]any{"reasoning_policy": store.ReasoningAvailability}}))
	if updated["version"] == initial["version"] || updated["settings"].(map[string]any)["reasoning_policy"] != store.ReasoningAvailability {
		t.Fatal("successful MCP patch did not advance the version and policy")
	}
	accepted := dataCall(t, manager, "get_settings", map[string]any{})
	stale := dataCall(t, manager, "update_settings", dataWrite(map[string]any{"expected_version": initial["version"], "patch": map[string]any{"reasoning_policy": store.ReasoningPreserve}}))
	if stale["status"] != "failed" {
		t.Fatalf("stale update was not refused: %v", stale)
	}
	invalid := dataCall(t, manager, "update_settings", dataWrite(map[string]any{"expected_version": accepted["version"], "patch": map[string]any{"reasoning_policy": "drop_all", "max_attempts": safe["max_attempts"].(float64) + 1}}))
	if invalid["status"] != "failed" {
		t.Errorf("invalid policy was not reported as a known refusal: %v", invalid)
	}
	refusal := invalid["result"].(map[string]any)
	if refusal["error"].(map[string]any)["code"] != "invalid_arguments" {
		t.Errorf("invalid policy did not retain the standard input validation code: %v", refusal)
	}
	if _, containsDraft := refusal["result"]; containsDraft {
		t.Error("refused patch returned an unsaved settings draft")
	}
	if after := dataCall(t, manager, "get_settings", map[string]any{}); !reflect.DeepEqual(after, accepted) {
		t.Fatal("stale or invalid MCP update overwrote accepted settings or advanced its version")
	}
	// A refused write must leave the accepted version usable for the next real edit.
	dataSuccess(t, manager, "update_settings", dataWrite(map[string]any{"expected_version": accepted["version"], "patch": map[string]any{"reasoning_policy": store.ReasoningPreserve}}))
}

func TestConversionUsageHTTPAndMCPKeepCorrelationAndSafeDiagnostic(t *testing.T) {
	h := newDataHarness(t)
	reader, _ := h.client(t, management.Read)
	requestID := "synthetic/request+7'quoted"
	diagnostic := json.RawMessage(`{"code":"reasoning_rejected","source":"upstream","phase":"upstream_response","message":"思考状态被拒绝","facts":["未向客户端交付"],"possible_causes":["目标可能变更"],"steps":[{"action":"核对目标","expected":"比较结果"}],"actions_taken":["保留原状态"],"settings_path":"#/settings?section=conversion","request_id":"synthetic/request+7'quoted"}`)
	ids := map[int64]bool{}
	for i, id := range []string{requestID, requestID, requestID + "-other", "different-request", ""} {
		row := store.Usage{RequestID: id, ChannelID: int64(i + 1), Model: "synthetic-model", Status: 400 + i,
			Error: "private-original-error", Diagnostic: diagnostic,
			Attempts: []store.AttemptRecord{{AttemptIndex: i + 1, ErrorReason: "private-attempt-error", CredentialMaskKey: "private-mask-key", StatusCode: 400 + i}},
		}
		rowID := dataUsageID(t, h.store, row)
		if id == requestID {
			ids[rowID] = true
		}
	}
	console := New(h.store, "http://127.0.0.1:17888", Hooks{})
	t.Cleanup(func() { ReleaseOAuthListeners(console) })
	w := httptest.NewRecorder()
	console.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs?request_id="+url.QueryEscape(requestID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("correlated HTTP logs returned %d", w.Code)
	}
	var rows []store.Usage
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(ids) {
		t.Fatalf("HTTP returned %d correlated rows, want %d", len(rows), len(ids))
	}
	for _, row := range rows {
		if row.RequestID != requestID || !ids[row.ID] || len(row.Diagnostic) == 0 {
			t.Fatal("HTTP correlation included an unrelated row or lost diagnostic")
		}
	}
	page := dataCall(t, reader, "query_usage_logs", map[string]any{"filter": map[string]any{"request_id": requestID}, "limit": 10})
	items := page["items"].([]any)
	if len(items) != len(ids) {
		t.Fatalf("MCP returned %d correlated rows, want %d", len(items), len(ids))
	}
	var expected any
	if err := json.Unmarshal(diagnostic, &expected); err != nil {
		t.Fatal(err)
	}
	assertSafe := func(row map[string]any) {
		t.Helper()
		if row["request_id"] != requestID || !ids[int64(row["id"].(float64))] {
			t.Fatal("MCP selected a different logical request")
		}
		if !reflect.DeepEqual(row["diagnostic"], expected) {
			t.Fatal("MCP safe projection lost diagnostic evidence")
		}
		if _, present := row["error"]; present {
			t.Fatal("safe log exposed original error")
		}
		attempts := row["attempts"].([]any)
		if len(attempts) != 1 {
			t.Fatal("safe log lost attempt trace")
		}
		attempt := attempts[0].(map[string]any)
		if _, present := attempt["credential_mask_key"]; present {
			t.Fatal("safe attempt exposed credential mask")
		}
		if _, present := attempt["error_reason"]; present {
			t.Fatal("safe attempt exposed raw error")
		}
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "private-") {
			t.Fatal("private original material escaped safe log projection")
		}
	}
	for _, item := range items {
		row := item.(map[string]any)
		assertSafe(row)
		assertSafe(dataCall(t, reader, "get_usage_log", map[string]any{"id": row["id"]}))
	}
	empty := dataCall(t, reader, "query_usage_logs", map[string]any{"filter": map[string]any{"request_id": "absent-request"}})
	if len(empty["items"].([]any)) != 0 {
		t.Fatal("unknown request selected unrelated MCP logs")
	}
}
