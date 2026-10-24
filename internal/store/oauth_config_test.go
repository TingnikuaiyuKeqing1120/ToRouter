package store

import (
	"reflect"
	"testing"
	"time"
)

func TestSaveChannelConfigKeepsConcurrentOAuthUpdate(t *testing.T) {
	chatgpt := chatgptChannel("https://api.openai.com/v1", chatgptCredential("oaiapp_config", "urn:uuid:config", "subject-config"))
	codex := chatgptChannel("https://chatgpt.com/backend-api/codex", chatgptCredential("codex-client", "", "subject-config"))
	codex.CredentialType, codex.OAuth.Provider, codex.OAuth.Account = CredentialCodexOAuth, OAuthProviderCodex, "acct-config"
	for _, c := range []Channel{
		copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("gho-original", "old-access")), chatgpt, codex,
	} {
		t.Run(c.CredentialType, func(t *testing.T) { testSaveConfigAfterOAuthUpdate(t, c) })
	}
}

func testSaveConfigAfterOAuthUpdate(t *testing.T, c Channel) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	edit, err := s.Channel(c.ID) // management's snapshot, before a rotation
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceOAuthTokens(c.ID, OAuthTokens{AccessToken: "new-access", RefreshToken: "new-refresh", IDToken: "new-id", ExpiresAt: time.Now().Add(time.Hour).UTC(), Scopes: "new-scope"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOAuthStatus(c.ID, OAuthStatusReauthRequired, "new issuer rejection", false); err != nil {
		t.Fatal(err)
	}
	latest, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	edit.Name, edit.Enabled = "renamed", false
	if err := s.SaveChannelConfig(&edit); err != nil {
		t.Fatal(err)
	}
	got, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.OAuth, latest.OAuth) {
		t.Fatal("ordinary edit rolled back the current OAuth token, expiry, scope or status")
	}
	if got.Name != "renamed" || got.Enabled || got.Generation != latest.Generation {
		t.Fatal("configuration edit did not preserve the chosen name, enabled flag and credential generation")
	}
}
