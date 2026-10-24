package store

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
)

func TestCredentialDailyTimesLoadMigrationPreservesTokensAndClaims(t *testing.T) {
	s, c := operationsStoreFixture(t)
	identity, _ := CredentialOperationsIdentity(c, "two")
	cfg := DefaultCredentialOperationsConfig()
	cfg.Warmup.Model = "model"
	if err := s.PatchCredentialOperationsConfig(c.ID, "two", identity, CredentialOperationsPatch{Warmup: &cfg.Warmup}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	json.Unmarshal([]byte(raw), &old)
	credentials := old["credentials"].([]any)
	cred := credentials[1].(map[string]any)
	ciphertext := cred["oauth"].(map[string]any)["access_token"]
	warmup := cred["operations"].(map[string]any)["warmup"].(map[string]any)
	delete(warmup, "daily_times")
	warmup["daily_time"] = "08:30"
	body, _ := json.Marshal(old)
	if _, err := s.db.Exec("UPDATE channels SET data=? WHERE id=?", string(body), c.ID); err != nil {
		t.Fatal(err)
	}
	live, _ := s.Channel(c.ID)
	if _, err := s.UpdateCredentialOperationState(live, "two", func(json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"daily_date":"2026-10-18","last_reset_id":"kept","history":{"kept":{"state":"unknown"}},"wake":{"18000":1},"generation":3}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.upgradeCredentialDailyTimes(); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Channel(c.ID)
	if !reflect.DeepEqual(got.Credentials[1].Operations.Warmup.DailyTimes, []string{"08:30"}) || got.Credentials[1].OAuth.AccessToken != "private-two" {
		t.Fatal("migration lost schedule or credential")
	}
	var first string
	s.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&first)
	var after map[string]any
	json.Unmarshal([]byte(first), &after)
	afterCred := after["credentials"].([]any)[1].(map[string]any)
	if afterCred["oauth"].(map[string]any)["access_token"] != ciphertext {
		t.Fatal("migration rewrote token material")
	}
	if _, oldField := afterCred["operations"].(map[string]any)["warmup"].(map[string]any)["daily_time"]; oldField {
		t.Fatal("retired field retained")
	}
	record, err := s.LoadCredentialOperationState(c.ID, "two")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	json.Unmarshal(record, &data)
	if data["daily_date"] != "2026-10-18" || data["last_reset_id"] != "kept" || !reflect.DeepEqual(data["daily_times"], []any{"08:30"}) || data["wake"] != nil {
		t.Fatal("claim migration lost dedup or retained retired watcher")
	}
	if err := s.upgradeCredentialDailyTimes(); err != nil {
		t.Fatal(err)
	}
	var second string
	s.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&second)
	if first != second {
		t.Fatal("migration is not idempotent")
	}
}

func TestCredentialOperationPartialPatchesDoNotOverwriteSiblingSettings(t *testing.T) {
	s, c := operationsStoreFixture(t)
	identity, _ := CredentialOperationsIdentity(c, "two")
	warmup := CredentialWarmupConfig{Enabled: true, Mode: "daily", Model: "model", DailyTimes: []string{"17:30", "09:00"}}
	threshold := 20.0
	reset := CredentialAutoResetConfig{Enabled: true, Rules: []CredentialResetRule{{WindowSeconds: 604800, RemainingBelow: &threshold, Join: "and"}}}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, patch := range []CredentialOperationsPatch{{Warmup: &warmup}, {AutoReset: &reset}} {
		wg.Add(1)
		go func(patch CredentialOperationsPatch) {
			defer wg.Done()
			errs <- s.PatchCredentialOperationsConfig(c.ID, "two", identity, patch)
		}(patch)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Channel(c.ID)
	config := got.Credentials[1].Operations
	if !config.Warmup.Enabled || !reflect.DeepEqual(config.Warmup.DailyTimes, []string{"09:00", "17:30"}) || !reflect.DeepEqual(config.AutoReset, reset) {
		t.Fatal("parallel editors overwrote sibling settings")
	}
	before := *config
	model := "model"
	if err := s.PatchCredentialOperationsConfig(c.ID, "two", identity, CredentialOperationsPatch{WarmupModel: &model}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Channel(c.ID)
	if !reflect.DeepEqual(before, *got.Credentials[1].Operations) {
		t.Fatal("manual model patch changed automatic options")
	}
	if err := s.PatchCredentialOperationsConfig(c.ID, "two", identity, CredentialOperationsPatch{Warmup: &warmup, AutoReset: &reset}); err == nil {
		t.Fatal("combined patch accepted")
	}
}

func TestCredentialDailyTimesValidationAndCheckedToggle(t *testing.T) {
	s, c := operationsStoreFixture(t)
	identity, _ := CredentialOperationsIdentity(c, "two")
	for _, times := range [][]string{{}, {"8:30"}, {"24:00"}, {"08:30", "08:30"}} {
		warmup := CredentialWarmupConfig{Enabled: true, Mode: "daily", Model: "model", DailyTimes: times}
		if err := s.PatchCredentialOperationsConfig(c.ID, "two", identity, CredentialOperationsPatch{Warmup: &warmup}); err == nil {
			t.Fatalf("invalid times accepted: %v", times)
		}
	}
	old := c.Generation
	next, err := s.SetCredentialEnabledAtGeneration(c.ID, "two", false, &old)
	if err != nil || next <= old {
		t.Fatalf("checked toggle: %d %v", next, err)
	}
	if _, err = s.SetCredentialEnabledAtGeneration(c.ID, "two", true, &old); err != ErrCredentialIdentityChanged {
		t.Fatal("stale batch generation accepted")
	}
	if _, err = s.SetCredentialEnabledAtGeneration(c.ID, "two", true, &next); err != nil {
		t.Fatal(err)
	}
}
