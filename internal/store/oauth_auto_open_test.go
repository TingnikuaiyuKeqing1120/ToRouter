package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOAuthAutoOpenPatchPreservesQuotaAndOpaqueSettings(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.PatchQuotaRefresh(60); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	before = strings.TrimSuffix(before, "}") + `,"future":{"escaped":"\u0061","amount":1.2300}}`
	if _, err := s.db.Exec("UPDATE settings SET data=? WHERE id=1", before); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		got, err := s.PatchOAuthAutoOpen(enabled)
		if err != nil || got.OAuthAutoOpen != enabled {
			t.Fatalf("patch=%+v %v", got, err)
		}
		var after string
		if err := s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&after); err != nil {
			t.Fatal(err)
		}
		var oldFields, newFields map[string]json.RawMessage
		if json.Unmarshal([]byte(before), &oldFields) != nil || json.Unmarshal([]byte(after), &newFields) != nil {
			t.Fatal("invalid settings JSON")
		}
		delete(oldFields, "oauth_auto_open")
		delete(newFields, "oauth_auto_open")
		if len(oldFields) != len(newFields) {
			t.Fatal("patch changed fields")
		}
		for key, raw := range oldFields {
			if string(raw) != string(newFields[key]) {
				t.Fatalf("changed unrelated field %s", key)
			}
		}
		settings, err := s.GetSettings()
		if err != nil || settings.OAuthAutoOpen == nil || *settings.OAuthAutoOpen != enabled || *settings.QuotaRefreshSeconds != 60 {
			t.Fatalf("settings bool or quota mismatch: %v", err)
		}
	}
	s.settingsSaveMu.Lock()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); _, err := s.PatchOAuthAutoOpen(false); done <- err }()
	<-started
	select {
	case <-done:
		s.settingsSaveMu.Unlock()
		t.Fatal("auto-open bypassed settings lock")
	case <-time.After(30 * time.Millisecond):
	}
	s.settingsSaveMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
