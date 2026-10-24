package store

import "testing"

func TestAPIKeyImportDoesNotDuplicateURLKeyForNewProtocol(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Channel{Name: "Existing", CredentialType: CredentialAPIKey, Enabled: true, APIKey: "synthetic-key",
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid/v1"}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	result, err := s.ImportAPIKeys(APIKeyImport{Name: "New protocol", Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: "https://example.invalid:443/v1"}}, Keys: []string{"synthetic-key"}}, nil)
	if err != nil || result.Added != 0 || result.Skipped != 1 || result.ChannelID != c.ID {
		t.Fatalf("%+v %v", result, err)
	}
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 || rows[0].Endpoints[0].Format != FormatOpenAIChatCompletions {
		t.Fatal("protocol widened or duplicate channel created")
	}
}

func TestAPIKeyImportDoesNotDuplicateOneURLOfMultiEndpointChannel(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	existing := Channel{Name: "Existing endpoints", CredentialType: CredentialAPIKey, Enabled: true, APIKey: "synthetic-shared-key",
		Endpoints: []Endpoint{
			{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid/a/v1"},
			{Format: FormatAnthropicMessages, BaseURL: "https://example.invalid/b/v1"},
		}}
	if err := s.SaveChannel(&existing); err != nil {
		t.Fatal(err)
	}
	before := EntityVersion(existing)
	result, err := s.ImportAPIKeys(APIKeyImport{Name: "Observed one address", Endpoints: []Endpoint{
		{Format: FormatOpenAIResponses, BaseURL: "https://example.invalid/a/v1"},
	}, Keys: []string{"synthetic-shared-key"}}, nil)
	if err != nil || result.Added != 0 || result.Skipped != 1 || result.ChannelID != existing.ID {
		t.Fatalf("single URL/key duplicated: %+v %v", result, err)
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 || EntityVersion(channels[0]) != before {
		t.Fatal("existing endpoint set changed or duplicate created")
	}
}
