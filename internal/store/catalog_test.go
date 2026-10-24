package store

import (
	"testing"
)

func catalogI(v int64) *int64     { return &v }
func catalogF(v float64) *float64 { return &v }
func catalogFixture(t *testing.T) (*Store, *Channel, CatalogItem) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &Channel{Name: "catalog fixture", Protocol: "openai", BaseURL: "https://example.com", APIKey: "test-key", Models: []string{"catalog-mini"}, Enabled: true}
	if err = s.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	supported := false
	item := CatalogItem{ID: "catalog-mini", Name: "Mini", Source: "openrouter", Provider: "test", Aliases: []string{"catalog-mini-2026-01-01"}, ContextLength: catalogI(128000), MaxOutputTokens: catalogI(16384), Modalities: CatalogModalities{Input: []string{"text"}, Output: []string{"text"}}, Thinking: NativeThinking{Supported: &supported}, Offerings: []CatalogOffering{{Key: "original", SourceKind: "original", Quotes: []CatalogQuote{{Key: "usd", SourceKind: "original", Table: PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: PriceRates{Input: catalogF(1), Output: catalogF(2)}, Rules: []PriceRule{}}}}}}}
	if err = s.SaveExternalCatalog("openrouter", []CatalogItem{item}); err != nil {
		t.Fatal(err)
	}
	return s, c, item
}
func TestUpstreamPrefillPreservesManualAndUnknownFields(t *testing.T) {
	s, c, _ := catalogFixture(t)
	c.ModelConfigs = map[string]ModelConfig{"catalog-mini": {ContextWindow: catalogI(32000), Streaming: ptr(true)}}
	if err := s.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	req := PrefillRequest{Targets: []PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: "catalog-mini"}}, Groups: []string{"capabilities"}}
	p, err := s.PreviewCatalogPrefill(req)
	if err != nil {
		t.Fatal(err)
	}
	cfg := p.Patches[0].Config
	if *cfg.ContextWindow != 32000 || cfg.Streaming == nil || !*cfg.Streaming || *cfg.MaxOutputTokens != 16384 {
		t.Fatal("manual or source-unknown value changed", cfg)
	}
	if _, err = s.ApplyCatalogPrefill(req); err != nil {
		t.Fatal(err)
	}
	req.ForceOverwrite = true
	p, err = s.PreviewCatalogPrefill(req)
	if err != nil {
		t.Fatal(err)
	}
	if *p.Patches[0].Config.ContextWindow != 128000 || !*p.Patches[0].Config.Streaming {
		t.Fatal("force must not clear source-unknown streaming")
	}
	if _, err = s.PreviewCatalogPrefill(PrefillRequest{Targets: []PrefillTarget{{Kind: "downstream", Model: "catalog-mini"}}, Groups: []string{"published_specs"}}); err == nil {
		t.Fatal("obsolete declarations accepted")
	}
}

func TestDirectoryRefreshPreservesSpecificProviderQuoteAndItsTime(t *testing.T) {
	s, _, item := catalogFixture(t)
	rows, err := s.ListCatalogItems("openrouter", "")
	if err != nil {
		t.Fatal(err)
	}
	oldTime := *rows[0].Offerings[0].Quotes[0].FetchedAt
	item.Offerings = []CatalogOffering{{Key: "directory-summary", SourceKind: "aggregator_summary", Quotes: []CatalogQuote{{Key: "usd", SourceKind: "aggregator_summary", Table: PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: PriceRates{Input: catalogF(9)}, Rules: []PriceRule{}}}}}}
	if err = s.SaveExternalCatalog("openrouter", []CatalogItem{item}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListCatalogItems("openrouter", "")
	if err != nil || len(rows[0].Offerings) != 2 {
		t.Fatalf("directory discarded bound provider quote: %+v %v", rows, err)
	}
	provider := rows[0].Offerings[1].Quotes[0]
	if *provider.Table.Rates.Input != 1 || provider.FetchedAt == nil || !provider.FetchedAt.Equal(oldTime) {
		t.Fatalf("provider price/time changed on directory pull: %+v", provider)
	}
}

func TestCatalogBuiltinAndMatching(t *testing.T) {
	if len(BuiltinCatalog) == 0 {
		t.Fatal("expected offline curated models")
	}
	items := []CatalogItem{{ID: "gpt-4o", Source: "builtin", Provider: "OpenAI"}, {ID: "gpt-4o-mini", Source: "builtin", Provider: "OpenAI", Aliases: []string{"gpt-4o-mini-2024-07-18"}}}
	matches := catalogMatches(items, "gpt-4o-mini-2024-07-18", nil, map[string]int{"builtin": 0})
	if len(matches) != 1 || matches[0].Item.ID != "gpt-4o-mini" || !matches[0].Automatic {
		t.Fatalf("explicit dated alias must retain mini identity: %+v", matches)
	}
	if m := catalogMatches(items, "gpt-4o-unknown-date", nil, map[string]int{"builtin": 0}); len(m) != 0 {
		t.Fatalf("unverified suffix must not execute: %+v", m)
	}
	if m := catalogMatches(items, "gpt", nil, map[string]int{"builtin": 0}); len(m) != 2 || m[0].Automatic || m[1].Automatic {
		t.Fatalf("fuzzy is recommendation only: %+v", m)
	}
}
func TestCatalogStoreOperations(t *testing.T) {
	s, _, item := catalogFixture(t)
	items, err := s.ListCatalogItems("builtin", "")
	if err != nil || len(items) != len(BuiltinCatalog) {
		t.Fatalf("offline list: %d %v", len(items), err)
	}
	ds, err := s.ListCatalogItems("builtin", "deepseek")
	if err != nil || len(ds) < 2 {
		t.Fatalf("curated search: %d %v", len(ds), err)
	}
	external, err := s.ListCatalogItems("openrouter", "")
	if err != nil || len(external) != 1 || external[0].ID != item.ID {
		t.Fatalf("external save: %+v %v", external, err)
	}
	sources, err := s.GetCatalogSources()
	if err != nil || len(sources) < 3 || sources[0].Source != "builtin" {
		t.Fatalf("sources: %+v %v", sources, err)
	}
}
func TestCatalogPrefillPreservesExplicitValuesAndAppliesAtomically(t *testing.T) {
	s, c, _ := catalogFixture(t)
	empty := []string{}
	c.ModelConfigs = map[string]ModelConfig{"catalog-mini": {ContextWindow: catalogI(32000), OutputLimit: catalogI(0), InputModalities: &empty}}
	if err := s.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	m := DownstreamModel{ID: "catalog-mini", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "catalog-mini", Enabled: true}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	req := PrefillRequest{Targets: []PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: m.ID}}, Groups: []string{"capabilities", "price"}}
	preview, err := s.PreviewCatalogPrefill(req)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Updated != 1 {
		t.Fatalf("preview %+v", preview)
	}
	if _, err = s.ApplyCatalogPrefill(req); err != nil {
		t.Fatal(err)
	}
	channels, err := s.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	cfg := channels[0].ModelConfigs[m.ID]
	if cfg.ContextWindow == nil || *cfg.ContextWindow != 32000 || cfg.MaxOutputTokens == nil || *cfg.MaxOutputTokens != 16384 {
		t.Fatal("must preserve manual context and fill output", cfg)
	}
	if cfg.OutputLimit == nil || *cfg.OutputLimit != 0 || cfg.InputModalities == nil || len(*cfg.InputModalities) != 0 {
		t.Fatal("explicit zero/empty lost", cfg)
	}
	prices, err := s.GetPrices()
	if err != nil || len(prices) != 1 || prices[0].Selection.Mode != "catalog" {
		t.Fatalf("associated price: %+v %v", prices, err)
	}
}
func TestCatalogPrefillRecomputesLatestSourceAndIsolatesDraft(t *testing.T) {
	s, c, item := catalogFixture(t)
	m := DownstreamModel{ID: "catalog-mini", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "catalog-mini", Enabled: true}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	req := PrefillRequest{Targets: []PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: m.ID}}, Groups: []string{"capabilities", "price"}, PriceMode: "manual"}
	preview, err := s.PreviewCatalogPrefill(req)
	if err != nil || len(preview.Patches) != 1 || preview.Patches[0].Price == nil || *preview.Patches[0].Price.Selection.Table.Rates.Output != 2 {
		t.Fatal("initial source preview is incorrect", preview, err)
	}
	item.Offerings[0].Quotes[0].Table.Rates.Output = catalogF(9)
	if err = s.SaveExternalCatalog("openrouter", []CatalogItem{item}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyCatalogPrefill(req); err != nil {
		t.Fatalf("direct atomic apply should succeed: %v", err)
	}
	channels, _ := s.ListChannels()
	if channels[0].ModelConfigs[m.ID].MaxOutputTokens == nil {
		t.Fatal("applied batch should write config")
	}
	prices, _ := s.GetPrices()
	if len(prices) != 1 || prices[0].Selection.Table == nil || *prices[0].Selection.Table.Rates.Output != 9 {
		t.Fatal("applied batch should use the updated source price", prices)
	}
	draft := PrefillRequest{Targets: []PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: "new draft", Draft: &ModelConfig{}, Ref: &CatalogModelRef{Source: "openrouter", ID: "catalog-mini"}}}}
	plan, err := s.PreviewCatalogPrefill(draft)
	if err != nil || plan.Patches[0].Config.MaxOutputTokens == nil {
		t.Fatalf("draft preview %v %+v", err, plan)
	}
	models, _ := s.ListDownstreamModels()
	if len(models) != 1 {
		t.Fatal("preview created model")
	}
}

func TestBuiltinPrefillContractsRemainValid(t *testing.T) {
	for _, item := range BuiltinCatalog {
		cfg := sourceConfig(item)
		if err := ValidateModelConfig(cfg); err != nil {
			t.Fatalf("%s cannot enter a real config: %v", item.ID, err)
		}
	}
}

func TestPublicPrefillDefaultsToPriceAndRejectsCapabilityGroups(t *testing.T) {
	s, c, _ := catalogFixture(t)
	m := DownstreamModel{ID: "catalog-mini", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "catalog-mini", Enabled: true}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	req := PrefillRequest{Targets: []PrefillTarget{{Kind: "downstream", Model: m.ID}}}
	p, err := s.PreviewCatalogPrefill(req)
	if err != nil || p.Patches[0].Price == nil || len(configFields(p.Patches[0].Config)) != 0 {
		t.Fatal("public default prefill must only propose price", err, p)
	}
	for _, group := range []string{"capabilities", "defaults", "published_specs"} {
		req.Groups = []string{group}
		if _, err = s.PreviewCatalogPrefill(req); err == nil {
			t.Fatal("public model accepted obsolete group", group)
		}
	}
}
