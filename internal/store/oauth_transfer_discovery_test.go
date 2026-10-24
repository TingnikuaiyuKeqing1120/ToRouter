package store

import (
	"reflect"
	"testing"
)

func TestOAuthTransferDiscoveryRetainsExplicitTargetWithMultipleURLChannels(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	item := oauthTransferTestItem(t, CredentialCopilotOAuth, "incoming")
	item.Endpoints = item.Endpoints[:1]
	existing := oauthTransferTestItem(t, CredentialCopilotOAuth, "existing")
	ch := oauthTransferSource(t, st, existing)
	before, err := st.Channel(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	result, err := st.ImportOAuthCredentials(OAuthTransferImport{ChannelID: ch.ID, ExpectedVersion: EntityVersion(before), Items: []OAuthTransferItem{item}, DiscoveryURL: item.Endpoints[0].BaseURL, Enabled: &disabled}, nil)
	if err != nil || result.Added != 1 {
		t.Fatal("single same-URL target should accept native credential", err)
	}
	got, _ := st.Channel(ch.ID)
	if !reflect.DeepEqual(got.Endpoints, before.Endpoints) || got.Enabled != before.Enabled {
		t.Fatal("discovery changed target protocols or enabled state")
	}
	second := oauthTransferTestItem(t, CredentialCopilotOAuth, "other-channel")
	oauthTransferSource(t, st, second)
	latest, _ := st.Channel(ch.ID)
	item.OAuth.GitHubToken = "synthetic-another-token"
	if _, err := st.ImportOAuthCredentials(OAuthTransferImport{ChannelID: ch.ID, ExpectedVersion: EntityVersion(latest), Items: []OAuthTransferItem{item}, DiscoveryURL: item.Endpoints[0].BaseURL}, nil); err != nil {
		t.Fatal("explicit target rejected because another URL owner exists", err)
	}
}
func TestOAuthTransferDiscoveryNewDisabledAndTargetRace(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	item := oauthTransferTestItem(t, CredentialCodexOAuth, "new")
	disabled := false
	result, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{item}, DiscoveryURL: item.Endpoints[0].BaseURL, Enabled: &disabled, DisplayIcon: "codex"}, nil)
	if err != nil || result.Added != 1 {
		t.Fatal("new disabled URL import failed", err)
	}
	got, _ := st.Channel(result.ChannelID)
	if got.Enabled || !got.Credentials[0].Enabled {
		t.Fatal("URL enabled state must be independent of credential state")
	}
	other := oauthTransferTestItem(t, CredentialCodexOAuth, "newer")
	if created, err := st.ImportOAuthCredentials(OAuthTransferImport{Items: []OAuthTransferItem{other}, DiscoveryURL: other.Endpoints[0].BaseURL}, nil); err != nil || created.Added != 1 || created.ChannelID == got.ID {
		t.Fatal("global discovery did not create an independent channel", err)
	}
	got.Credentials[0].Enabled = false
	if err := st.SaveChannel(&got); err != nil {
		t.Fatal(err)
	}
	latest, _ := st.Channel(got.ID)
	duplicate, err := st.ImportOAuthCredentials(OAuthTransferImport{ChannelID: got.ID, ExpectedVersion: EntityVersion(latest), Items: []OAuthTransferItem{item}, DiscoveryURL: item.Endpoints[0].BaseURL}, nil)
	if err != nil || duplicate.Added != 0 || duplicate.Skipped != 1 {
		t.Fatal("disabled existing credential must still be skipped", err)
	}
}
