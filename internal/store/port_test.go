package store

import "testing"

func TestPortRemembersAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if port, err := s.Port(); err != nil || port != 0 {
		t.Fatalf("fresh store should remember nothing: %d %v", port, err)
	}
	if err = s.SavePort(17878); err != nil {
		t.Fatal(err)
	}
	if port, err := s.Port(); err != nil || port != 17878 {
		t.Fatalf("remembered port wrong: %d %v", port, err)
	}
	// Later saves must not be reset by unrelated settings writes.
	if err = s.SaveSettings(Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RequestDefaultsEnabled: true, RequestDefaults: ModelDefaults{MaxOutputTokens: ptr(int64(8192))}}); err != nil {
		t.Fatal(err)
	}
	if port, err := s.Port(); err != nil || port != 17878 {
		t.Fatalf("settings save reset the remembered port: %d %v", port, err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if port, err := s.Port(); err != nil || port != 17878 {
		t.Fatalf("port not persisted: %d %v", port, err)
	}
	if err = s.SavePort(99999); err == nil {
		t.Fatal("accepted out-of-range port")
	}
	if err = s.SavePort(65535); err != nil {
		t.Fatal(err)
	}
	if port, _ := s.Port(); port != 65535 {
		t.Fatalf("update failed: %d", port)
	}
}
