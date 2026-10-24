package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestQuotaCachePersistsAndRejectsChangedOrDeletedIdentity(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := Channel{Name: "quota", CredentialType: CredentialCodexOAuth, Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}}, OAuth: &OAuthCredential{Provider: OAuthProviderCodex, ClientID: "client", Account: "account-one", AccessToken: "never-cache-access", RefreshToken: "never-cache-refresh", Status: OAuthStatusActive}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	record := QuotaCache{Snapshot: json.RawMessage(`{"captured_at":"2026-10-05T00:00:00Z","buckets":[]}`), LastAttemptAt: time.Now().UTC()}
	if err := s.SaveQuotaCache(c, c.Credentials[0].ID, record); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", quotaCacheKey(c.ID, c.Credentials[0].ID)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "never-cache") || strings.Contains(raw, "account-one") {
		t.Fatalf("cache contains credential/identity: %s", raw)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.LoadQuotaCache(c.ID, c.Credentials[0].ID)
	if err != nil || string(got.Snapshot) != string(record.Snapshot) || !got.LastAttemptAt.Equal(record.LastAttemptAt) {
		t.Fatalf("reopen lost quota: %+v %v", got, err)
	}
	if err := s.ReplaceOAuthTokens(c.ID, OAuthTokens{AccessToken: "rotated", RefreshToken: "rotated-refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadQuotaCache(c.ID, c.Credentials[0].ID)
	if err != nil || len(got.Snapshot) == 0 {
		t.Fatal("rotation discarded account snapshot")
	}
	changed, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed.OAuth.Account = "account-two"
	if err := s.SaveChannel(&changed); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadQuotaCache(c.ID, c.Credentials[0].ID)
	if err != nil || len(got.Snapshot) != 0 {
		t.Fatal("changed identity inherited old quota")
	}
	if err := s.SaveQuotaCache(c, c.Credentials[0].ID, record); err == nil {
		t.Fatal("stale writer overwrote changed identity")
	}
	if err := s.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadQuotaCache(c.ID, c.Credentials[0].ID); err != ErrNotFound {
		t.Fatalf("deleted channel cache readable: %v", err)
	}
	if err := s.SaveQuotaCache(changed, changed.Credentials[0].ID, record); err != ErrNotFound {
		t.Fatalf("deleted channel cache writable: %v", err)
	}
}

func TestQuotaCacheSeparatesPoolMembersAndRetiresChannelSnapshots(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := Channel{Name: "pool", CredentialType: CredentialCodexOAuth, Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}}, Credentials: []CredentialItem{
		{ID: "one", Enabled: true, OAuth: &OAuthCredential{Provider: OAuthProviderCodex, ClientID: "client", Account: "one", AccessToken: "private-one", RefreshToken: "private-refresh-one"}},
		{ID: "two", Enabled: true, OAuth: &OAuthCredential{Provider: OAuthProviderCodex, ClientID: "client", Account: "two", AccessToken: "private-two", RefreshToken: "private-refresh-two"}},
	}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		record := QuotaCache{Snapshot: json.RawMessage(`{"plan_type":"` + id + `"}`)}
		if err := s.SaveQuotaCache(c, id, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?)", "codex_quota_v1:1", "legacy-display"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"one", "two"} {
		got, err := s.LoadQuotaCache(c.ID, id)
		if err != nil || string(got.Snapshot) != `{"plan_type":"`+id+`"}` {
			t.Fatalf("member %s inherited wrong snapshot: %+v %v", id, got, err)
		}
	}
	var legacy int
	if err := s.db.QueryRow("SELECT count(*) FROM meta WHERE key GLOB 'codex_quota_v1:*'").Scan(&legacy); err != nil || legacy != 0 {
		t.Fatalf("legacy cache retained: %d %v", legacy, err)
	}
	if _, err := s.LoadQuotaCache(c.ID, "missing"); err == nil {
		t.Fatal("missing member fell back to primary")
	}
	// Reusing the exact pool ID for another identity invalidates old display data
	// and rejects an old request's receipt, including unnamed accounts.
	for _, anonymous := range []bool{false, true} {
		live, _ := s.Channel(c.ID)
		if anonymous {
			live.Credentials[1].OAuth.Account = ""
			live.Credentials[1].OAuth.Subject = ""
		}
		if err := s.SaveChannel(&live); err != nil {
			t.Fatal(err)
		}
		old := live
		record := QuotaCache{Snapshot: json.RawMessage(`{"plan_type":"before"}`)}
		if err := s.SaveQuotaCache(old, "two", record); err != nil {
			t.Fatal(err)
		}
		live, _ = s.Channel(c.ID)
		live.Credentials[1].OAuth.RefreshToken = "private-replacement-refresh"
		if anonymous {
			live.Credentials[1].OAuth.RefreshToken += "-anonymous"
		}
		if !anonymous {
			live.Credentials[1].OAuth.Account = "replacement"
		}
		if err := s.SaveChannel(&live); err != nil {
			t.Fatal(err)
		}
		if got, err := s.LoadQuotaCache(c.ID, "two"); err != nil || len(got.Snapshot) != 0 {
			t.Fatalf("replacement inherited quota: %+v %v", got, err)
		}
		if err := s.SaveQuotaCache(old, "two", record); err == nil {
			t.Fatal("old receipt accepted for replacement")
		}
	}
	live, _ := s.Channel(c.ID)
	old := live
	live.Credentials = live.Credentials[:1]
	if err := s.SaveChannel(&live); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadQuotaCache(c.ID, "two"); err == nil {
		t.Fatal("deleted member cache remained available")
	}
	if err := s.SaveQuotaCache(old, "two", QuotaCache{}); err == nil {
		t.Fatal("old receipt accepted after deletion")
	}
}
