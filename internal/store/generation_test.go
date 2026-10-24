package store

import (
	"testing"
)

// A channel's continuation-domain generation bumps only when the inputs a
// continuation domain is made of change: the credential, a base URL, or the
// endpoint declaration set. Cosmetic edits — name, models, the enabled
// flag — and an identical re-save leave it alone, so they cannot silently
// invalidate the sessions pinned to the channel.
func TestSaveChannelBumpsGenerationOnlyOnDomainInputs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := Channel{Name: "one", APIKey: "key-one", Models: []string{"m"}, Enabled: true,
		Endpoints: []Endpoint{
			{Format: FormatOpenAIChatCompletions, BaseURL: "http://localhost:1111/v1"},
			{Format: FormatOpenAIResponses, BaseURL: "http://localhost:1111/v1"},
		}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("a new channel starts at generation 1, got %d", c.Generation)
	}
	// The same configuration saved again is not a change.
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("an identical re-save bumped the generation to %d", c.Generation)
	}
	// Neither is anything that does not move the state namespace.
	c.Name, c.Models, c.Enabled = "two", []string{"m", "n"}, false
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("a cosmetic edit bumped the generation to %d", c.Generation)
	}
	c.Enabled = true
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	// A new credential is a new continuation domain.
	c.APIKey = "key-two"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 2 {
		t.Fatalf("a credential rotation must bump the generation, got %d", c.Generation)
	}
	// So is a base URL that moved.
	c.Endpoints[0].BaseURL = "http://localhost:2222/v1"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 3 {
		t.Fatalf("a base URL change must bump the generation, got %d", c.Generation)
	}
	// Reordering the endpoint array is display order (T019), not a new domain.
	c.Endpoints[0], c.Endpoints[1] = c.Endpoints[1], c.Endpoints[0]
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 3 {
		t.Fatalf("reordering endpoints bumped the generation to %d", c.Generation)
	}
	// Dropping one is a different declaration set.
	c.Endpoints = c.Endpoints[1:]
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 4 {
		t.Fatalf("dropping an endpoint must bump the generation, got %d", c.Generation)
	}
	// Declaring one back is the same question from the other side: a format the
	// row did not declare is a new endpoint of this channel.
	c.Endpoints = append(c.Endpoints, Endpoint{Format: FormatAnthropicMessages, BaseURL: "http://localhost:5555"})
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 5 {
		t.Fatalf("adding an endpoint must bump the generation, got %d", c.Generation)
	}
	// Endpoint.State is part of the declaration: whether an endpoint keeps
	// server-side conversation state decides which carriers mean anything there.
	c.Endpoints[0].State = "server_state"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 6 {
		t.Fatalf("changing an endpoint's state must bump the generation, got %d", c.Generation)
	}
	// The generation survives the round trip through the database.
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListChannels = %v %v", rows, err)
	}
	if rows[0].Generation != c.Generation {
		t.Fatalf("generation did not persist: %d vs %d", rows[0].Generation, c.Generation)
	}
	if rows[0].APIKey != c.APIKey {
		t.Fatal("credential roundtrip failed")
	}
}

// A legacy row re-saved with its endpoints spelled out for the first time keeps
// the generation it had: normalize() derives the same declarations the legacy
// fields implied, so nothing about the continuation domain changed.
func TestSaveChannelLegacyEndpointsDoNotBump(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := Channel{Name: "legacy", Protocol: "anthropic", BaseURL: "http://localhost:3333", APIKey: "k", Models: []string{"m"}, Enabled: true}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("a new channel starts at generation 1, got %d", c.Generation)
	}
	c.Endpoints = append([]Endpoint{}, c.Endpoints...)
	c.Protocol, c.BaseURL = "", ""
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("spelling out the implied endpoints bumped the generation to %d", c.Generation)
	}
	if len(c.Endpoints) != 1 || c.Endpoints[0].Format != FormatAnthropicMessages || c.Endpoints[0].BaseURL != "http://localhost:3333" {
		t.Fatalf("normalize lost the legacy endpoint: %+v", c.Endpoints)
	}
}

// Deleting a channel removes its generation with it.
func TestDeleteChannelRemovesGeneration(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := Channel{Name: "gone", APIKey: "k", Models: []string{"m"}, Enabled: true,
		Endpoints: []Endpoint{{Format: FormatGeminiGenerateContent, BaseURL: "http://localhost:4444"}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteChannel(c.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 0 {
		t.Fatalf("delete failed: %v %v", rows, err)
	}
}
