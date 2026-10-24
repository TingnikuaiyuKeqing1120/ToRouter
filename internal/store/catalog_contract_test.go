package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreGlobalAndOffByDefault(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	v, e := s.GetSettings()
	if e != nil {
		t.Fatal(e)
	}
	if v.RequestDefaultsEnabled || v.RequestDefaults.MaxOutputTokens != nil || v.RequestDefaults.Temperature != nil || v.RequestDefaults.Thinking != nil {
		t.Fatal("new defaults must be empty and disabled", v.RequestDefaults)
	}
}

func TestCatalogContractUpgradeBackupPreservesValues(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	c := Channel{Name: "up", APIKey: "secret-original", Models: []string{"native"}, AvailableModels: []string{"native"}, Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.com"}}}
	if e = s.SaveChannel(&c); e != nil {
		t.Fatal(e)
	}
	m := DownstreamModel{ID: "alias", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "native", Enabled: true}}}
	if e = s.SaveDownstreamModel(m); e != nil {
		t.Fatal(e)
	}
	var raw string
	_ = s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&raw)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw), &fields)
	delete(fields, "request_defaults")
	delete(fields, "request_defaults_enabled")
	fields["max_tokens_default"] = json.RawMessage(`1234`)
	fields["config"] = json.RawMessage(`{"temperature":0,"thinking_effort":"high","thinking_budget":2048,"max_output_tokens":9999}`)
	fields["prices"] = json.RawMessage(`[{"model":"alias","currency":"USD","unit_tokens":1000,"input":1,"output":2,"time_enabled":true,"time_windows":[{"start":"23:00","end":"02:00","mode":"fixed","rates":{"input":3}},{"start":"02:00","end":"23:00","mode":"multiplier","multiplier":0.5}],"context_enabled":true,"context_ranges":[{"from_tokens":0,"mode":"multiplier","multiplier":2}]},{"model":"draft","time_windows":[{"start":"","end":"","mode":"editing"}],"context_ranges":[{"from_tokens":-1,"mode":"editing"}]}]`)
	before, _ := json.Marshal(fields)
	if _, e = s.db.Exec("UPDATE settings SET data=? WHERE id=1", string(before)); e != nil {
		t.Fatal(e)
	}
	if e = s.AddUsage(Usage{Model: "alias", ActualCost: priceNumber(0), Currency: "USD", OfficialValue: priceNumber(7), OfficialCurrency: "CNY"}); e != nil {
		t.Fatal(e)
	}
	_ = s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	v, e := s.GetSettings()
	if e != nil {
		t.Fatal(e)
	}
	d := &v.RequestDefaults
	if d == nil || *d.MaxOutputTokens != 1234 || *d.Temperature != 0 || *d.Thinking.Effort != "high" || *d.Thinking.Budget != 2048 || d.Thinking.Mode != "auto_native" || v.RequestDefaultsEnabled {
		t.Fatal("upgrade changed capabilities/defaults", v.RequestDefaults)
	}
	rules := v.Prices[0].Selection.Table.Rules
	if len(rules) != 3 || rules[0].When.TimeWindow == nil || rules[1].When.InputRange == nil || rules[2].Action.Multiplier == nil || rules[0].Action.RateChanges["output"] != nil {
		t.Fatal("legacy order/null changed", rules)
	}
	draft := v.Prices[1].Selection.Table.Rules
	if len(draft) != 2 || draft[0].Enabled || draft[1].Enabled || len(draft[0].Draft) == 0 || len(draft[1].Draft) == 0 {
		t.Fatal("disabled unfinished drafts lost", draft)
	}
	channels, _ := s.ListChannels()
	if channels[0].APIKey != "secret-original" {
		t.Fatal("credential changed")
	}
	usage, _ := s.ListUsage(UsageFilter{})
	if len(usage) != 1 || usage[0].ActualCost == nil || *usage[0].ActualCost != 0 || *usage[0].OfficialValue != 7 || usage[0].OfficialCurrency != "CNY" || usage[0].ActualPricing != nil {
		t.Fatal("old monetary evidence changed", usage)
	}
	paths, e := filepath.Glob(filepath.Join(dir, "backups", "catalog-contract-*", "router.db"))
	if e != nil || len(paths) != 1 {
		t.Fatal("missing recovery backup", paths, e)
	}
	if _, e = os.Stat(filepath.Join(filepath.Dir(paths[0]), "master.key")); e != nil {
		t.Fatal("backup cannot decrypt original credentials", e)
	}
	db, e := sql.Open("sqlite", paths[0])
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var saved string
	if e = db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&saved); e != nil || saved != string(before) {
		t.Fatal("backup not original data", e)
	}
}

func TestUpgradeFailureRetainsOriginalAndImportKeepsUnknowns(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	var raw string
	_ = s.db.QueryRow("SELECT data FROM settings").Scan(&raw)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw), &fields)
	fields["prices"] = json.RawMessage(`[{"model":"broken","currency":"EUR","input":1}]`)
	before, _ := json.Marshal(fields)
	_, e = s.db.Exec("UPDATE settings SET data=?", string(before))
	if e != nil {
		t.Fatal(e)
	}
	_ = s.Close()
	if broken, e := Open(dir); e == nil {
		broken.Close()
		t.Fatal("invalid migration accepted")
	}
	db, e := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var after string
	_ = db.QueryRow("SELECT data FROM settings").Scan(&after)
	if after != string(before) {
		t.Fatal("failed migration altered original")
	}
	m, e := DecodeDownstreamModelImport([]byte(`{"id":"old","config":{"max_output_tokens":6000,"thinking_effort":"medium","thinking_budget":0,"streaming":false,"input_modalities":[]}}`))
	if e != nil {
		t.Fatal(e)
	}
	rawModel, _ := json.Marshal(m)
	if m.CatalogModelRef != nil || string(rawModel) == "" || strings.Contains(string(rawModel), "config") || strings.Contains(string(rawModel), "published_specs") {
		t.Fatal("import retained obsolete configuration", string(rawModel))
	}

}

func TestPriceTableValidationAndAtomicAssociatedSave(t *testing.T) {
	for _, r := range []PriceRule{{Enabled: true, When: PriceRuleWhen{TimeWindow: &PriceTimeWindow{Start: "00:00", End: "00:00", Timezone: "UTC"}}, Action: PriceRuleAction{Multiplier: priceNumber(1)}}, {Enabled: true, When: PriceRuleWhen{Weekdays: []int{0}, Timezone: "UTC"}, Action: PriceRuleAction{Multiplier: priceNumber(1)}}, {Enabled: true, When: PriceRuleWhen{DateSet: []string{"2026-02-30"}, Timezone: "UTC"}, Action: PriceRuleAction{Multiplier: priceNumber(1)}}} {
		if e := ValidatePriceTable(PriceTable{Currency: "USD", UnitTokens: 1, Rules: []PriceRule{r}}); e == nil {
			t.Fatal("invalid enabled rule accepted", r)
		}
	}
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	c := Channel{Name: "c", APIKey: "secret", Models: []string{"m"}, Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.com"}}}
	if e = s.SaveChannel(&c); e != nil {
		t.Fatal(e)
	}
	m := DownstreamModel{ID: "alias", Enabled: true, Bindings: []Binding{{ChannelID: c.ID, Model: "m", Enabled: true}}}
	p := Price{Model: "alias", Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000, Rates: PriceRates{Input: priceNumber(0)}}}}
	if e = s.ImportDownstreamModelsWithPrices([]DownstreamModel{m}, []Price{p}, false); e != nil {
		t.Fatal(e)
	}
	m.DisplayName = "must rollback"
	p.Selection.Table.UnitTokens = -1
	if e = s.ImportDownstreamModelsWithPrices([]DownstreamModel{m}, []Price{p}, false); e == nil {
		t.Fatal("invalid price accepted")
	}
	models, _ := s.ListDownstreamModels()
	if models[0].DisplayName != "" {
		t.Fatal("entity left partly written", models)
	}
	prices, _ := s.GetPrices()
	if prices[0].Selection.Table.UnitTokens != 1000 || *prices[0].Selection.Table.Rates.Input != 0 {
		t.Fatal("existing price changed", prices)
	}
}

func TestCatalogPreferencesAndExactRefreshPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCatalogSources([]CatalogSourceStatus{{Source: "openrouter", PrefillEnabled: false, Priority: 0}, {Source: "litellm", PrefillEnabled: true, Priority: 1}, {Source: "builtin", PrefillEnabled: true, Priority: 2}}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveExternalCatalog("openrouter", []CatalogItem{{ID: "a", Name: "first"}, {ID: "b", Name: "second"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCatalogItem("openrouter", "a", CatalogItem{ID: "a", Name: "refreshed", Offerings: []CatalogOffering{{Key: "provider:region", Quotes: []CatalogQuote{{Key: "standard", Table: PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: PriceRates{Input: priceNumber(0)}}}}}}}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	prefs, err := s.GetCatalogSources()
	if err != nil {
		t.Fatal(err)
	}
	if prefs[0].Source != "openrouter" || prefs[0].Priority != 0 || prefs[0].PrefillEnabled || prefs[1].Source != "litellm" || !prefs[1].PrefillEnabled {
		t.Fatal("restart overwrote source preferences", prefs)
	}
	items, err := s.ListCatalogItems("openrouter", "")
	if err != nil || len(items) != 2 || items[0].Name != "refreshed" || items[1].Name != "second" {
		t.Fatal("single refresh cleared successful source rows", items, err)
	}
}

func TestChannelConfigSaveRejectsInvalidAndRetainsCredentials(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Channel{Name: "", APIKey: "secret", Models: []string{"m"}, Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.com"}}}
	if err = s.SaveChannelConfig(&c); err == nil {
		t.Fatal("invalid channel accepted")
	}
	channels, _ := s.ListChannels()
	if len(channels) != 0 || c.ID != 0 {
		t.Fatal("failed transaction leaked channel/id", channels, c.ID)
	}
	c.Name = "new"
	if err = s.SaveChannelConfig(&c); err != nil {
		t.Fatal(err)
	}
	channels, _ = s.ListChannels()
	prices, _ := s.GetPrices()
	if len(channels) != 1 || channels[0].APIKey != "secret" || len(prices) != 0 {
		t.Fatal("config save lost credential or wrote price", channels, prices)
	}
}

func TestResponsibilityBoundaryConversionPreservesOnlyConcreteSpecs(t *testing.T) {
	raw := []byte(`{"name":"same","api_key":"sealed-value","config":{"streaming":true,"defaults":{"temperature":1}},"model_configs":{"one":{"streaming":false,"max_output_tokens":0,"input_modalities":[],"defaults":{"max_output_tokens":77}},"bad":{"output_token_field":"invalid"}}}`)
	clean, changed, err := upgradeDocument(raw, "channels")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	var c Channel
	if err = json.Unmarshal(clean, &c); err != nil {
		t.Fatal(err)
	}
	cfg := c.ModelConfigs["one"]
	if c.APIKey != "sealed-value" || cfg.Streaming == nil || *cfg.Streaming || cfg.MaxOutputTokens == nil || *cfg.MaxOutputTokens != 0 || cfg.InputModalities == nil || len(*cfg.InputModalities) != 0 {
		t.Fatal("conversion lost concrete values")
	}
	if strings.Contains(string(clean), `"defaults"`) || strings.Contains(string(clean), `"config"`) {
		t.Fatal("obsolete object configuration retained", string(clean))
	}
	if _, exists := c.ModelConfigs["bad"]; exists {
		t.Fatal("invalid unconvertible spec retained")
	}
	repeated, changed, err := upgradeDocument(clean, "channels")
	if err != nil || changed || string(repeated) != string(clean) {
		t.Fatal("migration is not idempotent", changed, err)
	}
	global, _, err := upgradeDocument([]byte(`{"config":{"defaults":{"max_output_tokens":8192}}}`), "settings")
	if err != nil {
		t.Fatal(err)
	}
	var settings Settings
	_ = json.Unmarshal(global, &settings)
	if settings.RequestDefaultsEnabled || settings.RequestDefaults.MaxOutputTokens == nil || *settings.RequestDefaults.MaxOutputTokens != 8192 {
		t.Fatal("legacy global budget not retained off", string(global))
	}
}
