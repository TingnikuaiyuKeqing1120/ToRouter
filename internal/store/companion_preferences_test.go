package store

import (
	"encoding/json"
	"testing"
)

func TestCompanionPreferencesPatchRetainsIndependentFieldsAndExactTargets(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	initial, err := s.CompanionPreferences()
	if err != nil || initial.QuotaTargets == nil {
		t.Fatalf("default %+v %v", initial, err)
	}
	c := Channel{Name: "Example", Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}, Credentials: []CredentialItem{{ID: "one", APIKey: "synthetic-key", Enabled: true}, {ID: "two", APIKey: "synthetic-other", Enabled: true}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	light, paused := "light", true
	targets := []QuotaTarget{{c.ID, "two"}}
	if _, err = s.PatchCompanionPreferences(CompanionPreferencesPatch{Theme: &light}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PatchCompanionPreferences(CompanionPreferencesPatch{QuotaTargets: &targets}); err != nil {
		t.Fatal(err)
	}
	v, err := s.PatchCompanionPreferences(CompanionPreferencesPatch{Paused: &paused})
	if err != nil || v.Theme != "light" || !v.Paused || len(v.QuotaTargets) != 1 || v.QuotaTargets[0].CredentialID != "two" || v.Revision != 3 {
		t.Fatalf("patch %+v %v", v, err)
	}
	// Separate windows changing separate fields must serialize their row reads.
	dark, running := "dark", false
	start, results := make(chan struct{}), make(chan error, 2)
	for _, patch := range []CompanionPreferencesPatch{{Theme: &dark}, {Paused: &running}} {
		go func(p CompanionPreferencesPatch) { <-start; _, err := s.PatchCompanionPreferences(p); results <- err }(patch)
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	v, err = s.CompanionPreferences()
	if err != nil || v.Theme != "dark" || v.Paused || len(v.QuotaTargets) != 1 {
		t.Fatalf("concurrent patch %+v %v", v, err)
	}
	for _, bad := range [][]QuotaTarget{{{c.ID, "missing"}}, {{c.ID, "two"}, {c.ID, "two"}}, {{-1, "two"}}, make([]QuotaTarget, 101)} {
		if _, err := s.PatchCompanionPreferences(CompanionPreferencesPatch{QuotaTargets: &bad}); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
	if err := s.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	// A theme patch retains the explicit removed reference. It cannot silently
	// select the remaining/default credential; the read projection resolves IDs.
	v, err = s.PatchCompanionPreferences(CompanionPreferencesPatch{Theme: &dark})
	if err != nil || v.QuotaTargets[0].CredentialID != "two" {
		t.Fatalf("deleted target %+v %v", v, err)
	}
	empty := []QuotaTarget{}
	v, err = s.PatchCompanionPreferences(CompanionPreferencesPatch{QuotaTargets: &empty})
	raw, _ := json.Marshal(v)
	if err != nil || string(raw) == "" || v.QuotaTargets == nil {
		t.Fatalf("clear %+v %v", v, err)
	}
}
