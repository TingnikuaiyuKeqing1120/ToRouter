package store

import (
	"testing"
	"time"
)

func TestChannelCredentialPoolNormalization(t *testing.T) {
	// 1. Legacy channel with only APIKey
	legacy := Channel{
		Name:      "legacy-channel",
		APIKey:    "sk-legacy-123",
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"}},
	}
	legacy.normalize()

	if legacy.DispatchMode != DispatchModeFailover {
		t.Fatalf("expected default dispatch mode %q, got %q", DispatchModeFailover, legacy.DispatchMode)
	}
	if legacy.ChainType != ChainTypeUnidirectional {
		t.Fatalf("expected default chain type %q, got %q", ChainTypeUnidirectional, legacy.ChainType)
	}
	if len(legacy.Credentials) != 1 {
		t.Fatalf("expected 1 normalized credential, got %d", len(legacy.Credentials))
	}
	cred := legacy.Credentials[0]
	if cred.ID != "cred_default" || cred.APIKey != "sk-legacy-123" || !cred.Enabled {
		t.Fatalf("unexpected normalized credential: %+v", cred)
	}

	// 2. Channel with multiple credentials specified
	multi := Channel{
		Name:         "multi-channel",
		DispatchMode: DispatchModeRoundRobin,
		ChainType:    ChainTypeCyclic,
		Endpoints:    []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"}},
		Credentials: []CredentialItem{
			{ID: "c1", Label: "Key 1", APIKey: "sk-1", Enabled: true},
			{ID: "c2", Label: "Key 2", APIKey: "sk-2", Enabled: false},
		},
	}
	multi.normalize()
	if multi.DispatchMode != DispatchModeRoundRobin {
		t.Fatalf("expected round_robin, got %q", multi.DispatchMode)
	}
	if multi.ChainType != ChainTypeCyclic {
		t.Fatalf("expected cyclic, got %q", multi.ChainType)
	}
	if multi.APIKey != "sk-1" {
		t.Fatalf("expected c.APIKey to sync to first credential sk-1, got %q", multi.APIKey)
	}
}

func TestStoreSealAndOpenMultiCredentials(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch := &Channel{
		Name:         "pool-channel",
		Enabled:      true,
		DispatchMode: DispatchModeRoundRobin,
		ChainType:    ChainTypeCyclic,
		Endpoints:    []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"}},
		Credentials: []CredentialItem{
			{ID: "cred_primary", Label: "主密钥", APIKey: "sk-primary-secret", Enabled: true},
			{ID: "cred_backup", Label: "备用密钥", APIKey: "sk-backup-secret", Enabled: true},
		},
	}
	if err := s.SaveChannel(ch); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.channel(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Credentials) != 2 {
		t.Fatalf("expected 2 credentials, got %d", len(loaded.Credentials))
	}
	if loaded.Credentials[0].APIKey != "sk-primary-secret" {
		t.Fatalf("expected decrypted primary key 'sk-primary-secret', got %q", loaded.Credentials[0].APIKey)
	}
	if loaded.Credentials[1].APIKey != "sk-backup-secret" {
		t.Fatalf("expected decrypted backup key 'sk-backup-secret', got %q", loaded.Credentials[1].APIKey)
	}
	if loaded.APIKey != "sk-primary-secret" {
		t.Fatalf("expected ch.APIKey to match primary, got %q", loaded.APIKey)
	}
}

func TestStoreReplaceOAuthTokensForCredential(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	ch := &Channel{
		Name:           "copilot-pool",
		CredentialType: CredentialCopilotOAuth,
		Enabled:        true,
		Endpoints:      []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.githubcopilot.com"}},
		Credentials: []CredentialItem{
			{
				ID:    "gh_account_1",
				Label: "Account 1",
				OAuth: &OAuthCredential{
					Provider:    OAuthProviderCopilot,
					GitHubToken: "gho_token1",
					AccessToken: "tid=init1",
					ExpiresAt:   now.Add(30 * time.Minute),
				},
				Enabled: true,
			},
			{
				ID:    "gh_account_2",
				Label: "Account 2",
				OAuth: &OAuthCredential{
					Provider:    OAuthProviderCopilot,
					GitHubToken: "gho_token2",
					AccessToken: "tid=init2",
					ExpiresAt:   now.Add(30 * time.Minute),
				},
				Enabled: true,
			},
		},
	}
	if err := s.SaveChannel(ch); err != nil {
		t.Fatal(err)
	}

	// Update account 2
	newExp := now.Add(2 * time.Hour)
	if err := s.ReplaceCopilotTokenForCredential(ch.ID, "gh_account_2", "tid=updated2", newExp); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.channel(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Credentials[1].OAuth.AccessToken != "tid=updated2" {
		t.Fatalf("expected updated access token 'tid=updated2', got %q", loaded.Credentials[1].OAuth.AccessToken)
	}
	if !loaded.Credentials[1].OAuth.ExpiresAt.Equal(newExp) {
		t.Fatalf("expected updated expires_at %v, got %v", newExp, loaded.Credentials[1].OAuth.ExpiresAt)
	}
	// Account 1 should be untouched
	if loaded.Credentials[0].OAuth.AccessToken != "tid=init1" {
		t.Fatalf("account 1 was unexpectedly modified: %q", loaded.Credentials[0].OAuth.AccessToken)
	}
}
