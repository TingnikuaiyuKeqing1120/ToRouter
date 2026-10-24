package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCatalogueCommitGateRunsWithoutOpenSQLTransaction(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := Channel{Name: "provider", APIKey: "synthetic-key", Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.test"}}, Models: []string{"old"}, AvailableModels: []string{"old"}}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c, err = st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	called := false
	_, err = st.ReplaceCatalogueAtVersion(c.ID, []string{"fetched"}, EntityVersion(c), func() error {
		called = true
		var count int
		return st.db.QueryRowContext(ctx, "SELECT count(*) FROM management_connections").Scan(&count)
	})
	if err != nil || !called {
		t.Fatalf("commit gate could not use the shared SQL connection: %v", err)
	}
	live, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("authorization revoked")
	_, err = st.ReplaceCatalogueAtVersion(c.ID, []string{"forbidden"}, EntityVersion(live), func() error { return denied })
	if !errors.Is(err, denied) {
		t.Fatalf("commit gate ignored: %v", err)
	}
}

func TestCatalogueSourceVersionAllowsOnlyOAuthRotation(t *testing.T) {
	for _, mode := range []string{"rotation", "account", "client", "host", "api_base", "models", "enabled", "credential_id"} {
		t.Run(mode, func(t *testing.T) {
			st, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			c := Channel{Name: "codex", CredentialType: CredentialCodexOAuth, Enabled: true, Models: []string{"old"}, AvailableModels: []string{"old"}, Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}}, OAuth: &OAuthCredential{Provider: OAuthProviderCodex, AccessToken: "old-access", RefreshToken: "old-refresh", ClientID: "test-client", Account: "acct-1", ExpiresAt: time.Now().Add(-time.Minute), Status: OAuthStatusActive}}
			if err = st.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			c, err = st.Channel(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := CatalogueSourceVersion(c)
			if c.OAuth.AccessToken != "old-access" {
				t.Fatal("projection changed caller's credential")
			}
			if mode == "rotation" {
				err = st.ReplaceOAuthTokensForCredential(c.ID, c.Credentials[0].ID, OAuthTokens{AccessToken: "new-access", RefreshToken: "new-refresh", IDToken: "new-id", ExpiresAt: time.Now().Add(time.Hour)})
			} else {
				switch mode {
				case "account":
					c.Credentials[0].OAuth.Account = "another-account"
				case "client":
					c.Credentials[0].OAuth.ClientID = "another-client"
				case "host":
					c.Credentials[0].OAuth.HostID = "another-host"
				case "api_base":
					c.Credentials[0].OAuth.APIBase = "https://chatgpt.com/another"
				case "models":
					c.Models = append(c.Models, "another-model")
				case "enabled":
					c.Enabled = false
				case "credential_id":
					c.Credentials[0].ID = "different-credential"
				}
				err = st.SaveChannel(&c)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = st.ReplaceCatalogueAtVersion(c.ID, []string{"fetched"}, expected)
			if mode == "rotation" && err != nil {
				t.Fatalf("own rotation blocked catalogue: %v", err)
			}
			if mode != "rotation" && !errors.Is(err, ErrEditConflict) {
				t.Fatalf("changed %s accepted: %v", mode, err)
			}
		})
	}
}
