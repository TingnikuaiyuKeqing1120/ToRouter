package store

import (
	"errors"
	"reflect"
	"testing"
)

func TestDefaultLaunchPreferencePreservesSettingsAndReopens(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	before, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if before.DefaultLaunchMode != "web" {
		t.Fatal("new instance must default to web")
	}
	if err = s.SaveDefaultLaunchMode("desktop"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSettings()
	want := before
	want.DefaultLaunchMode = "desktop"
	if !reflect.DeepEqual(got, want) {
		t.Fatal("tray preference overwrote unrelated settings")
	}
	// The settings form doesn't edit this preference. An omitted field must
	// preserve a newer tray selection rather than silently reverting to Web.
	got.DefaultLaunchMode = ""
	got.MaxAttempts = 4
	got.Prices = nil
	if err = s.SaveSettings(got); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultLaunchMode != "desktop" || got.MaxAttempts != 4 || got.GatewayKey != before.GatewayKey {
		t.Fatal("reopening lost launch preference or settings")
	}
	for _, mode := range []string{"", "all", "Desktop", " cli "} {
		if err = s.SaveDefaultLaunchMode(mode); err == nil {
			t.Fatalf("invalid mode accepted: %q", mode)
		}
		after, _ := s.GetSettings()
		if !reflect.DeepEqual(after, got) {
			t.Fatal("invalid launch preference changed data")
		}
	}
}

func TestDefaultLaunchPreferenceVersionedPatch(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before, _ := s.GetSettings()
	version := EntityVersion(SafeSettings(before))
	updated, err := s.PatchRuntimeSettings(version, []byte(`{"default_launch_mode":"cli"}`))
	if err != nil || updated.DefaultLaunchMode != "cli" {
		t.Fatalf("launch patch: %v", err)
	}
	if _, err = s.PatchRuntimeSettings(version, []byte(`{"default_launch_mode":"web"}`)); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("stale preference patch accepted: %v", err)
	}
	_, err = s.PatchRuntimeSettings(EntityVersion(SafeSettings(updated)), []byte(`{"default_launch_mode":"all","max_attempts":8}`))
	var invalid *ConfigurationError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected known validation refusal: %v", err)
	}
	after, _ := s.GetSettings()
	if !reflect.DeepEqual(updated, after) {
		t.Fatal("invalid patch changed existing settings")
	}
}
