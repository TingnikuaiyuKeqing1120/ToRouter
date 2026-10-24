package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestQuotaRefreshPatchesOnlyExistingFieldAndSerializesSettingsWrites(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var before string
	if err := s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Preserve opaque future data and the literal representation of nested prices.
	before = strings.TrimSuffix(before, "}") + `,"future":{"escaped":"\u0061","amount":1.2300}}`
	if _, err := s.db.Exec("UPDATE settings SET data=? WHERE id=1", before); err != nil {
		t.Fatal(err)
	}
	for _, seconds := range []int{0, 30, 86400} {
		got, err := s.PatchQuotaRefresh(seconds)
		if err != nil || got.QuotaRefreshSeconds != seconds {
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
		delete(oldFields, "quota_refresh_seconds")
		delete(newFields, "quota_refresh_seconds")
		if len(oldFields) != len(newFields) {
			t.Fatal("patch changed fields")
		}
		for key, raw := range oldFields {
			if string(raw) != string(newFields[key]) {
				t.Fatalf("patch changed unrelated field %s", key)
			}
		}
		read, err := s.GetQuotaRefresh()
		if err != nil || read != got {
			t.Fatalf("projection=%+v %v", read, err)
		}
	}
	for _, seconds := range []int{-1, 1, 29, 86401} {
		if _, err := s.PatchQuotaRefresh(seconds); !errors.Is(err, ErrInvalidQuotaRefresh) {
			t.Fatalf("accepted %d: %v", seconds, err)
		}
	}
	s.settingsSaveMu.Lock()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); _, err := s.PatchQuotaRefresh(60); done <- err }()
	<-started
	select {
	case <-done:
		s.settingsSaveMu.Unlock()
		t.Fatal("patch bypassed settings lock")
	case <-time.After(30 * time.Millisecond):
	}
	s.settingsSaveMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Missing old field resolves to the same existing default and can be set.
	if _, err := s.db.Exec("UPDATE settings SET data=json_remove(data,'$.quota_refresh_seconds') WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetQuotaRefresh(); err != nil || got.QuotaRefreshSeconds != DefaultQuotaRefreshSeconds {
		t.Fatalf("default=%+v %v", got, err)
	}
	if _, err := s.PatchQuotaRefresh(0); err != nil {
		t.Fatal(err)
	}
}
