package tray

import "testing"

func TestDefaultActionReadsCurrentPreference(t *testing.T) {
	mode := "web"
	cfg := Config{DefaultLaunchMode: func() string { return mode }}
	for _, want := range []string{"web", "desktop", "cli"} {
		mode = want
		if got := cfg.defaultMode(); got != want {
			t.Fatalf("default mode %q, want %q", got, want)
		}
	}
	mode = "invalid"
	if cfg.defaultMode() != "web" || (Config{}).defaultMode() != "web" {
		t.Fatal("missing preference must default to web")
	}
}
