package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestOpenRetiresOnlyDiscoveryDirectoryScopes(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"key_discovery_scope:source-codex", "keyXdiscoveryXscope:unrelated", "plugin_preferences_note"} {
		if _, err := s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?)", key, "synthetic-value"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var value string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", "key_discovery_scope:source-codex").Scan(&value); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retired scope still present: %v", err)
	}
	for _, key := range []string{"keyXdiscoveryXscope:unrelated", "plugin_preferences_note"} {
		if err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&value); err != nil || value != "synthetic-value" {
			t.Fatalf("unrelated metadata changed: %s %v", key, err)
		}
	}
}
