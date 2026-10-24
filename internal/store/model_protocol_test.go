package store

import (
	"errors"
	"testing"
)

func TestManagedCatalogueAtomicVersionAndContinuation(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := Channel{Name: "Synthetic catalogue", Enabled: true, CredentialType: CredentialZenFree, APIKey: "public", Models: []string{"model"}, ModelFormats: map[string][]string{"model": {FormatOpenAIChatCompletions}}, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://opencode.ai/zen/v1"}, {Format: FormatOpenAIResponses, BaseURL: "https://opencode.ai/zen/v1"}}}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	generation := c.Generation
	version := CatalogueSourceVersion(c)
	formats := map[string][]string{"model": {FormatOpenAIResponses}, "new": {FormatOpenAIChatCompletions}}
	updated, err := st.ReplaceProviderCatalogueAtVersion(c.ID, formats, version)
	if err != nil || updated.Generation != generation+1 || len(updated.AvailableModels) != 2 {
		t.Fatal(updated, err)
	}
	formats["model"][0] = FormatAnthropicMessages
	loaded, _ := st.Channel(c.ID)
	if len(loaded.ModelFormats["model"]) != 1 || loaded.ModelFormats["model"][0] != FormatOpenAIResponses {
		t.Fatal("caller mutated stored map")
	}
	if _, err = st.ReplaceProviderCatalogueAtVersion(c.ID, map[string][]string{}, version); !errors.Is(err, ErrEditConflict) {
		t.Fatal("late catalogue replaced newer model protocols", err)
	}
	// A stale Web draft cannot clear provider-owned metadata.
	c.ModelFormats = nil
	c.Name = "Edited"
	if err = st.SaveChannelConfig(&c); err != nil {
		t.Fatal(err)
	}
	if len(c.ModelFormats["model"]) != 1 || c.ModelFormats["model"][0] != FormatOpenAIResponses {
		t.Fatal("ordinary edit lost provider declaration")
	}
	empty, err := st.ReplaceProviderCatalogueAtVersion(c.ID, map[string][]string{}, CatalogueSourceVersion(c))
	if err != nil {
		t.Fatal(err)
	}
	read, _ := st.Channel(c.ID)
	if read.ModelFormats == nil || len(read.ModelFormats) != 0 || empty.Generation <= generation {
		t.Fatal("empty declared catalogue lost through persistence")
	}
}
