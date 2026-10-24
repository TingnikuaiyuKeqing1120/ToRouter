package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestLoggerRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.SaveChannel(&store.Channel{Name: "up", Protocol: "openai", BaseURL: "http://localhost:1", APIKey: "sk-super-secret-upstream", Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	budget := int64(8192)
	if err = st.SaveSettings(store.Settings{GatewayKey: "tr_client_gateway-secret", RetentionDays: 30, MaxAttempts: 3, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: &budget}}); err != nil {
		t.Fatal(err)
	}
	logger, err := NewLogger(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	logger.Event("discovery failed", "detail", "upstream said bad key sk-super-secret-upstream for tr_client_gateway-secret")
	logger.Error("boom", "error", "Authorization: Bearer sk-super-secret-upstream")
	data, err := os.ReadFile(LogPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, secret := range []string{"sk-super-secret-upstream", "tr_client_gateway-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("log leaked %s:\n%s", secret, text)
		}
	}
	if !strings.Contains(text, "discovery failed") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("log missing content:\n%s", text)
	}
	if strings.Count(text, "\n") != 2 {
		t.Fatalf("expected one line per event:\n%s", text)
	}
}

func TestLoggerRotatesWhenFull(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	path := LogPath(dir)
	if err = os.WriteFile(path, []byte(strings.Repeat("x", maxLogSize+10)), 0600); err != nil {
		t.Fatal(err)
	}
	logger, err := NewLogger(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	logger.Event("after rotation")
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if len(rotated) < maxLogSize {
		t.Fatal("rotated file too small")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(current), strings.Repeat("x", 100)) {
		t.Fatal("current log still holds the rotated content")
	}
	if !strings.Contains(string(current), "after rotation") {
		t.Fatalf("current log missing the new event: %s", current)
	}
	_ = filepath.Base(path)
}

func TestLoggerRedactsHomePathsAndBareTokens(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewLogger(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger.Event(`startup at C:\Users\Example User\to-router`, "data", `/home/fixture-user/.config/to-router`, "detail", `{"path":"C:\\Users\\fixture-user\\to-router\\app.log","detail":"tr_mcp_Synthetic-private-123"}`)
	data, err := os.ReadFile(LogPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, private := range []string{"Synthetic", "Example User", "fixture-user", "private-123"} {
		if strings.Contains(text, private) {
			t.Fatalf("log leaked identity or token: %s", text)
		}
	}
	for _, tail := range []string{`[HOME]\to-router`, `[HOME]/.config/to-router`, `app.log`, "[REDACTED]"} {
		if !strings.Contains(text, tail) {
			t.Fatalf("log lost diagnostic content %q: %s", tail, text)
		}
	}
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("log entry spans multiple lines: %s", text)
	}
}
