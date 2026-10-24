package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"torouter/internal/compatprofile"
)

func apiKeyImportTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func apiKeyImportEndpoints() []Endpoint {
	return []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.example.invalid/v1", State: "stateless"}}
}

func TestAPIKeyImportEndpointIdentityPreservesEffectiveSemantics(t *testing.T) {
	base := Channel{Endpoints: []Endpoint{
		{Format: FormatOpenAIChatCompletions, BaseURL: "HTTPS://API.Example.invalid:443/v1?a=1&b=2", State: "stateless"},
		{Format: FormatAnthropicMessages, BaseURL: "http://API.Example.invalid:80/anthropic"},
	}}
	identity := APIKeyEndpointIdentity(base)
	if identity == "" {
		t.Fatal("valid endpoint set rejected")
	}
	equivalent := Channel{BaseURL: "http://api.example.invalid/anthropic", Endpoints: []Endpoint{
		{Format: FormatAnthropicMessages},
		{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.example.invalid/v1?a=1&b=2", State: "stateless"},
	}}
	if APIKeyEndpointIdentity(equivalent) != identity {
		t.Fatal("order/case/default port/effective base mismatch")
	}
	for _, change := range []func(*Channel){
		func(c *Channel) { c.Endpoints[0].State = "" },
		func(c *Channel) { c.Endpoints[0].Format = FormatOpenAIResponses },
		func(c *Channel) { c.Endpoints[0].BaseURL = "https://api.example.invalid/v1?a=1&b=3" },
		func(c *Channel) { c.Endpoints[0].BaseURL = "https://api.example.invalid/v1?b=2&a=1" },
		func(c *Channel) { c.Endpoints[0].BaseURL = "https://api.example.invalid/V1?a=1&b=2" },
		func(c *Channel) { c.Endpoints[0].BaseURL = "https://api.example.invalid/v1/?a=1&b=2" },
		func(c *Channel) { c.Endpoints = c.Endpoints[:1] },
	} {
		changed := base
		changed.Endpoints = append([]Endpoint(nil), base.Endpoints...)
		change(&changed)
		if APIKeyEndpointIdentity(changed) == identity {
			t.Fatal("endpoint information was discarded")
		}
	}
	for _, invalid := range []string{"", "ftp://api.example.invalid", "https://user:secret@api.example.invalid/v1", "https://api.example.invalid/v1#fragment", "https://api.example.invalid:", "https://api.example.invalid:65536", "/relative"} {
		if APIKeyEndpointIdentity(Channel{Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: invalid}}}) != "" {
			t.Fatal("invalid endpoint accepted")
		}
	}
	before := EntityVersion(base)
	_ = APIKeyEndpointIdentity(base)
	if EntityVersion(base) != before {
		t.Fatal("identity comparison mutated the caller")
	}
}

func TestAPIKeyImportFullKeyDedupEncryptionAndReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	first, second := "syn-first-private-value-tail", "syn-second-private-value-tail"
	if MaskKey(first) != MaskKey(second) {
		t.Fatal("fixture does not exercise identical masks")
	}
	input := APIKeyImport{Name: "Imported channel", Endpoints: apiKeyImportEndpoints(), Keys: []string{first, first, second}}
	result, err := s.ImportAPIKeys(input, nil)
	if err != nil || result.Added != 2 || result.Skipped != 1 || result.ChannelID <= 0 {
		t.Fatalf("full key import: %+v %v", result, err)
	}
	c, err := s.Channel(result.ChannelID)
	if err != nil || len(c.Credentials) != 2 || !c.Enabled || len(c.Models) != 0 || len(c.AvailableModels) != 0 {
		t.Fatalf("new channel expanded scope: %v", err)
	}
	if c.Credentials[0].APIKey != first || c.Credentials[1].APIKey != second || c.Credentials[0].ID == c.Credentials[1].ID || !strings.HasPrefix(c.Credentials[0].ID, "cred_") {
		t.Fatal("complete keys/order/random identities not retained")
	}
	var raw string
	if err = s.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range input.Keys {
		if strings.Contains(raw, key) {
			t.Fatal("imported key stored in plaintext")
		}
	}
	public, _ := json.Marshal(result)
	if strings.Contains(string(public), first) || strings.Contains(string(public), second) {
		t.Fatal("result leaked keys")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := s.Channel(c.ID)
	if err != nil || !reflect.DeepEqual(restarted, c) {
		t.Fatalf("restart changed imported credentials: %v", err)
	}
	replayed, err := s.ImportAPIKeys(input, nil)
	if err != nil || replayed.Added != 0 || replayed.Skipped != 3 || replayed.ChannelID != c.ID {
		t.Fatalf("replay not idempotent: %+v %v", replayed, err)
	}
	channels, _ := s.ListChannels()
	models, _ := s.ListDownstreamModels()
	if len(channels) != 1 || len(models) != 0 {
		t.Fatal("replay created a channel or automatic routing")
	}
}

func TestAPIKeyImportRejectsOAuthDestination(t *testing.T) {
	s := apiKeyImportTestStore(t)
	oauth := pluginFixture(t, "synthetic-import-type-account", "synthetic-import-type-access")
	channel, _, err := s.CommitPluginConsent(compatprofile.QoderCredential, oauth, PluginConsentTarget{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	channel, err = s.Channel(channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := EntityVersion(channel)
	_, err = s.ImportAPIKeys(APIKeyImport{ChannelID: channel.ID, ExpectedVersion: before, Endpoints: channel.Endpoints, Keys: []string{"synthetic-invalid-api-key"}}, nil)
	if !errors.Is(err, ErrInvalidAPIKeyImport) {
		t.Fatalf("OAuth destination accepted an API key: %v", err)
	}
	channel, err = s.Channel(channel.ID)
	if err != nil || EntityVersion(channel) != before {
		t.Fatal("rejected API key changed OAuth material")
	}
}

func TestAPIKeyImportAppendPreservesExistingConfiguration(t *testing.T) {
	s := apiKeyImportTestStore(t)
	limit := int64(1024)
	c := Channel{Name: "Retained name", Enabled: false, Endpoints: append(apiKeyImportEndpoints(), Endpoint{Format: FormatOpenAIResponses, BaseURL: "https://api.example.invalid/responses", State: "server_state"}),
		Credentials: []CredentialItem{{ID: "old-second", Label: "Private custom label", APIKey: "synthetic-second-key", Enabled: false}, {ID: "old-first", Label: "Keep label", APIKey: "synthetic-first-key", Enabled: true}},
		Models:      []string{"model-b", "model-a"}, AvailableModels: []string{"model-a", "model-b", "model-c"}, ManualModels: []string{"model-c"}, Tags: []string{"privacy:zdr", "billing:payg"},
		ModelTags: map[string][]string{"model-b": {"billing:payg"}}, ModelConfigs: map[string]ModelConfig{"model-b": {MaxOutputTokens: &limit}},
		ModelFormats:  map[string][]string{"model-a": {FormatOpenAIResponses}, "model-b": {FormatOpenAIChatCompletions}, "model-c": {FormatOpenAIResponses}},
		ModelEvidence: map[string]CapabilityEvidence{"model-a": {Source: "manual", APIFormat: FormatOpenAIResponses}},
		DispatchMode:  DispatchModeRoundRobin, ChainType: ChainTypeCyclic, PreferredEndpoint: FormatOpenAIResponses}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	route := DownstreamModel{ID: "public-fixture", DisplayName: "Retained route", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "model-b", Priority: 4, Enabled: true}}}
	if err := s.SaveDownstreamModel(route); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePrices([]Price{{ChannelID: c.ID, Model: "model-b", Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000000}}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Channel(c.ID)
	prices, _ := s.GetPrices()
	routes, _ := s.ListDownstreamModels()
	equivalent := append([]Endpoint(nil), before.Endpoints...)
	equivalent[0], equivalent[1] = equivalent[1], equivalent[0]
	equivalent[1].BaseURL = "https://API.EXAMPLE.INVALID:443/v1"
	result, err := s.ImportAPIKeys(APIKeyImport{ChannelID: c.ID, ExpectedVersion: EntityVersion(before), Name: "Ignored rename", Endpoints: equivalent, Keys: []string{"synthetic-added-key"}}, nil)
	if err != nil || result.Added != 1 || result.ChannelID != c.ID {
		t.Fatalf("append failed: %+v %v", result, err)
	}
	after, _ := s.Channel(c.ID)
	if !reflect.DeepEqual(after.Credentials[:len(before.Credentials)], before.Credentials) || after.Credentials[len(before.Credentials)].Label != "导入凭据" {
		t.Fatal("old credential identity/order/label/enablement changed")
	}
	comparison := after
	comparison.Credentials = before.Credentials
	comparison.Generation = before.Generation
	if !reflect.DeepEqual(comparison, before) {
		t.Fatal("append changed non-credential channel configuration")
	}
	afterPrices, _ := s.GetPrices()
	afterRoutes, _ := s.ListDownstreamModels()
	if !reflect.DeepEqual(prices, afterPrices) || !reflect.DeepEqual(routes, afterRoutes) {
		t.Fatal("append changed pricing or routing")
	}
}

func TestAPIKeyImportVersionRevocationAndProtocolMismatchAreAtomic(t *testing.T) {
	s := apiKeyImportTestStore(t)
	initial, err := s.ImportAPIKeys(APIKeyImport{Name: "Initial", Endpoints: apiKeyImportEndpoints(), Keys: []string{"synthetic-initial-key"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Channel(initial.ChannelID)
	input := APIKeyImport{ChannelID: before.ID, ExpectedVersion: EntityVersion(before), Endpoints: before.Endpoints, Keys: []string{"synthetic-next-key"}}
	stale := input
	stale.ExpectedVersion = "stale"
	if _, err = s.ImportAPIKeys(stale, nil); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("stale import accepted: %v", err)
	}
	changed := input
	changed.Endpoints = append(append([]Endpoint(nil), input.Endpoints...), Endpoint{Format: FormatOpenAIResponses, BaseURL: input.Endpoints[0].BaseURL})
	if _, err = s.ImportAPIKeys(changed, nil); !errors.Is(err, ErrInvalidAPIKeyImport) {
		t.Fatalf("protocol expansion accepted: %v", err)
	}
	stateChanged := input
	stateChanged.Endpoints = append([]Endpoint(nil), input.Endpoints...)
	stateChanged.Endpoints[0].State = "server_state"
	if _, err = s.ImportAPIKeys(stateChanged, nil); !errors.Is(err, ErrInvalidAPIKeyImport) {
		t.Fatalf("endpoint state mismatch accepted: %v", err)
	}
	denied := errors.New("synthetic permission revoked")
	checked := false
	if _, err = s.ImportAPIKeys(input, func() error { checked = true; return denied }); !errors.Is(err, denied) || !checked {
		t.Fatalf("permission guard bypassed: %v", err)
	}
	after, _ := s.Channel(before.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed import partially changed the channel")
	}
	if _, err = s.ImportAPIKeys(APIKeyImport{Name: "Denied create", Endpoints: input.Endpoints, Keys: input.Keys}, func() error { return denied }); !errors.Is(err, denied) {
		t.Fatal("new channel bypassed permission recheck")
	}
	channels, _ := s.ListChannels()
	if len(channels) != 1 {
		t.Fatal("denied import created an empty channel")
	}
}

func TestAPIKeyImportBatchValidationAndAmbiguousDuplicateDestinations(t *testing.T) {
	s := apiKeyImportTestStore(t)
	base := APIKeyImport{Name: "Initial", Endpoints: apiKeyImportEndpoints(), Keys: []string{"synthetic-key"}}
	for _, keys := range [][]string{nil, {"synthetic-valid-key", " "}, {strings.Repeat("x", 16*1024+1)}, make([]string, 101)} {
		invalid := base
		invalid.Keys = keys
		if _, err := s.ImportAPIKeys(invalid, nil); !errors.Is(err, ErrInvalidAPIKeyImport) {
			t.Fatalf("invalid batch accepted: %v", err)
		}
	}
	channels, _ := s.ListChannels()
	if len(channels) != 0 {
		t.Fatal("invalid batch partially wrote keys")
	}
	for _, name := range []string{"Existing one", "Existing two"} {
		c := Channel{Name: name, Endpoints: base.Endpoints, APIKey: base.Keys[0], Enabled: true}
		if err := s.SaveChannel(&c); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.ImportAPIKeys(base, nil)
	if err != nil || result.ChannelID != 0 || result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("ambiguous existing ownership invented a destination: %+v %v", result, err)
	}
	different := base
	different.Endpoints = []Endpoint{{Format: FormatAnthropicMessages, BaseURL: base.Endpoints[0].BaseURL}}
	result, err = s.ImportAPIKeys(different, nil)
	if err != nil || result.Added != 0 || result.Skipped != 1 || result.ChannelID != 0 {
		t.Fatalf("protocol difference duplicated an existing URL/key or invented ownership: %+v %v", result, err)
	}
	channels, _ = s.ListChannels()
	if len(channels) != 2 {
		t.Fatal("protocol difference created another channel")
	}
}

func TestAPIKeyImportConcurrentReplayCreatesOnlyOneDestination(t *testing.T) {
	s := apiKeyImportTestStore(t)
	input := APIKeyImport{Name: "Concurrent import", Endpoints: apiKeyImportEndpoints(), Keys: []string{"synthetic-concurrent-key"}}
	var wg sync.WaitGroup
	results := make(chan APIKeyImportResult, 2)
	errors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.ImportAPIKeys(input, nil)
			results <- result
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	added, skipped, id := 0, 0, int64(0)
	for result := range results {
		added += result.Added
		skipped += result.Skipped
		if id != 0 && id != result.ChannelID {
			t.Fatal("concurrent replay selected different destinations")
		}
		id = result.ChannelID
	}
	channels, _ := s.ListChannels()
	if added != 1 || skipped != 1 || len(channels) != 1 {
		t.Fatal("concurrent replay duplicated keys or channels")
	}
}

func TestAPIKeyDiscoveryURLTargetRecheckAndConcurrentAppend(t *testing.T) {
	s := apiKeyImportTestStore(t)
	address := "https://discovery.example.invalid/prefix/v1"
	c := Channel{Name: "Retained", Enabled: false, APIKey: "synthetic-old", Endpoints: []Endpoint{{Format: FormatAnthropicMessages, BaseURL: address}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Channel(c.ID)
	input := APIKeyImport{ChannelID: c.ID, ExpectedVersion: EntityVersion(c), DiscoveryURL: address, Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: address}}, Keys: []string{"synthetic-added"}}
	result, err := s.ImportAPIKeys(input, nil)
	if err != nil || result.Added != 1 {
		t.Fatal("same URL different protocol append failed")
	}
	after, _ := s.Channel(c.ID)
	if after.Enabled || !reflect.DeepEqual(after.Endpoints, c.Endpoints) {
		t.Fatal("append changed fixed settings")
	}
	input.ChannelID = 0
	input.ExpectedVersion = ""
	input.Name = "Independent discovered channel"
	input.Keys = []string{"synthetic-must-not-create"}
	created, err := s.ImportAPIKeys(input, nil)
	if err != nil || created.Added != 1 || created.ChannelID == after.ID {
		t.Fatal("new discovery group did not create an independent channel", err)
	}
	twin := c
	twin.ID = 0
	twin.Name = "Second URL target"
	if err = s.SaveChannel(&twin); err != nil {
		t.Fatal(err)
	}
	input.ChannelID = after.ID
	input.ExpectedVersion = EntityVersion(after)
	input.Keys = []string{"synthetic-explicit-target"}
	if appended, err := s.ImportAPIKeys(input, nil); err != nil || appended.ChannelID != after.ID || appended.Added != 1 {
		t.Fatal("explicit target rejected because URL has other owners", err)
	}
	channels, _ := s.ListChannels()
	if len(channels) != 3 {
		t.Fatal("creation or explicit append changed unrelated channels")
	}
}

func TestAPIKeyDiscoveryConcurrentNewURLNeverDuplicates(t *testing.T) {
	s := apiKeyImportTestStore(t)
	address := "https://concurrent-url.example.invalid/v1"
	input := APIKeyImport{Name: "Concurrent", DiscoveryURL: address, Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: address}}, Keys: []string{"synthetic-concurrent"}}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.ImportAPIKeys(input, nil); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			t.Fatal(err)
		}
	}
	channels, _ := s.ListChannels()
	if len(channels) != 1 || len(channels[0].Credentials) != 1 || success != 2 {
		t.Fatal("lock-time material recheck did not skip concurrent duplicate")
	}
}
