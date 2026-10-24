package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"torouter/internal/store"
)

func catalogJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func catalogHTTPChannel(t *testing.T, s *store.Store, model string) *store.Channel {
	t.Helper()
	c := &store.Channel{Name: "catalog channel", APIKey: "credential-stays-intact", Models: []string{model}, AvailableModels: []string{model}, Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://example.com"}}}
	if err := s.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	return c
}
func catalogHTTPItem(id string, output int64) store.CatalogItem {
	return store.CatalogItem{ID: id, Source: "openrouter", Provider: "test manufacturer", Name: "Fixture", ContextLength: ptr(int64(128000)), MaxInputTokens: ptr(int64(96000)), MaxOutputTokens: &output, Modalities: store.CatalogModalities{Input: []string{"text"}, Output: []string{"text"}}, Thinking: store.NativeThinking{Supported: ptr(false)}, Defaults: &store.ModelDefaults{Temperature: ptr(0.3)}, Offerings: []store.CatalogOffering{{Key: "original", SourceKind: "original", Quotes: []store.CatalogQuote{{Key: "standard", SourceKind: "original", Table: store.PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: store.PriceRates{Input: ptr(1.0), Output: ptr(2.0)}, Rules: []store.PriceRule{}}}}}}}
}
func catalogHTTPPreview(t *testing.T, h http.Handler, req store.PrefillRequest) store.PrefillPreview {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/catalog/prefill/preview", strings.NewReader(catalogJSON(t, req))))
	if w.Code != 200 {
		t.Fatalf("preview %d: %s", w.Code, w.Body.String())
	}
	var preview store.PrefillPreview
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	return preview
}

func TestCatalogHTTPQuoteAppliesDirectlyWithoutConflict(t *testing.T) {
	s, r := newTestCatalogConsole(t)
	defer s.Close()
	item := catalogHTTPItem("apply-model", 10000)
	c := catalogHTTPChannel(t, s, item.ID)
	if err := s.SaveExternalCatalog("openrouter", []store.CatalogItem{item}); err != nil {
		t.Fatal(err)
	}
	m := store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: item.ID, Enabled: true}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	ref := &store.CatalogModelRef{Source: "openrouter", ID: item.ID}
	req := store.PrefillRequest{Targets: []store.PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: item.ID, Ref: ref}}, Groups: []string{"capabilities", "price"}}
	preview := catalogHTTPPreview(t, r, req)
	if len(preview.Patches) != 1 || preview.Updated != 1 {
		t.Fatal(preview)
	}
	item.Offerings[0].Quotes[0].Table.Rates.Output = ptr(9.0)
	if err := s.SaveCatalogItem("openrouter", item.ID, item); err != nil {
		t.Fatal(err)
	}
	code, _, _ := call(t, r, "POST", "/catalog/prefill/apply", catalogJSON(t, req))
	if code != 200 {
		t.Fatalf("expected 200 from direct apply, got %d", code)
	}
	models, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "public" {
		t.Fatalf("applied model config mismatch: %+v", models)
	}
	channels, err := s.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	if channels[0].ModelConfigs[item.ID].MaxOutputTokens == nil {
		t.Fatal("applied upstream config missing", channels)
	}
	prices, err := s.GetPrices()
	if err != nil || len(prices) == 0 {
		t.Fatal("applied prices missing", prices, err)
	}
}

func TestCatalogHTTPDraftCancelAndDedicatedPriceSave(t *testing.T) {
	s, _ := newTestCatalogConsole(t)
	r := New(s, "http://127.0.0.1:17878", Hooks{})
	defer s.Close()
	item := catalogHTTPItem("draft-source", 24000)
	c := catalogHTTPChannel(t, s, item.ID)
	if err := s.SaveExternalCatalog("openrouter", []store.CatalogItem{item}); err != nil {
		t.Fatal(err)
	}
	req := store.PrefillRequest{Targets: []store.PrefillTarget{{Kind: "downstream", Model: "new-model", Draft: &store.ModelConfig{}, Ref: &store.CatalogModelRef{Source: "openrouter", ID: item.ID}}}, Groups: []string{"price"}}
	beforeChannel, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	preview := catalogHTTPPreview(t, r, req)
	if len(preview.Patches) != 1 || preview.Patches[0].Price == nil {
		t.Fatalf("incomplete draft preview: %+v", preview)
	}
	models, _ := s.ListDownstreamModels()
	prices, _ := s.GetPrices()
	if len(models) != 0 || len(prices) != 0 {
		t.Fatal("preview/cancel persisted draft", models, prices)
	}
	code, _, _ := call(t, r, "POST", "/catalog/prefill/apply", catalogJSON(t, req))
	if code != 400 {
		t.Fatal("draft must save through editor", code)
	}
	models, _ = s.ListDownstreamModels()
	prices, _ = s.GetPrices()
	if len(models) != 0 || len(prices) != 0 {
		t.Fatal("rejected draft apply persisted state", models, prices)
	}
	afterChannel, err := s.Channel(c.ID)
	if err != nil || !reflect.DeepEqual(beforeChannel.ModelConfigs, afterChannel.ModelConfigs) || !reflect.DeepEqual(beforeChannel.ModelCatalogRefs, afterChannel.ModelCatalogRefs) {
		t.Fatal("price draft modified upstream specifications", err)
	}
	patch := preview.Patches[0]
	m := store.DownstreamModel{ID: "new-model", Enabled: true, CatalogModelRef: patch.CatalogRef, Bindings: []store.Binding{{ChannelID: c.ID, Model: item.ID, Enabled: true}}}
	var payload map[string]any
	raw, _ := json.Marshal(m)
	_ = json.Unmarshal(raw, &payload)
	code, obj, _ := call(t, r, "POST", "/models", catalogJSON(t, payload))
	if code != 200 {
		t.Fatal("single editor save", code, obj)
	}
	code, obj, _ = call(t, r, "PUT", "/prices", catalogJSON(t, []store.Price{*patch.Price}))
	if code != 200 {
		t.Fatal("dedicated price save", code, obj)
	}
	models, err = s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	prices, err = s.GetPrices()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || len(prices) != 1 || models[0].ID != "new-model" || models[0].CatalogModelRef == nil || prices[0].Model != models[0].ID || prices[0].Selection.Ref == nil || prices[0].Selection.Ref.ID != item.ID {
		t.Fatal("single save omitted entity or associated price", models, prices)
	}
}

func TestCatalogHTTPSourcePreferencesPersist(t *testing.T) {
	s, r := newTestCatalogConsole(t)
	defer s.Close()
	code, obj, list := call(t, r, "PUT", "/catalog/sources", `[{"source":"builtin","prefill_enabled":false,"priority":2},{"source":"openrouter","prefill_enabled":true,"priority":0},{"source":"litellm","prefill_enabled":true,"priority":1}]`)
	if code != 200 || len(list) != 3 {
		t.Fatal(code, obj, list)
	}
	code, _, list = call(t, r, "GET", "/catalog/sources", "")
	if code != 200 || list[0].(map[string]any)["source"] != "openrouter" || list[2].(map[string]any)["prefill_enabled"] != false {
		t.Fatal("source preferences not served", list)
	}
	prefs, err := s.GetCatalogSources()
	if err != nil || prefs[0].Source != "openrouter" || prefs[1].Source != "litellm" || !prefs[1].PrefillEnabled || prefs[2].PrefillEnabled {
		t.Fatal("source preferences not stored", prefs, err)
	}
	code, _, _ = call(t, r, "PUT", "/catalog/sources", `[{"source":"openrouter","prefill_enabled":false,"priority":5},{"source":"unknown","priority":0}]`)
	if code != 400 {
		t.Fatal("invalid source accepted", code)
	}
	after, _ := s.GetCatalogSources()
	if !reflect.DeepEqual(prefs, after) {
		t.Fatal("failed preferences batch partly saved", after)
	}
}

func TestModelsHTTPBackupImportsAssociatedPricesAndDeclarations(t *testing.T) {
	s, r := newTestCatalogConsole(t)
	defer s.Close()
	c := catalogHTTPChannel(t, s, "real")
	m := store.DownstreamModel{ID: "public", DisplayName: "Original", Enabled: true, CatalogModelRef: &store.CatalogModelRef{Source: "openrouter", ID: "source-id", OfferingKey: "region"}, Bindings: []store.Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}
	prices := []store.Price{{Model: m.ID, Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "CNY", UnitTokens: 1000, Rates: store.PriceRates{Input: ptr(0.0), Output: ptr(0.0000000001)}, Rules: []store.PriceRule{}}}}}
	var payload map[string]any
	raw, _ := json.Marshal(m)
	_ = json.Unmarshal(raw, &payload)
	code, obj, _ := call(t, r, "POST", "/models", catalogJSON(t, payload))
	if code != 200 {
		t.Fatal(code, obj)
	}
	if err := s.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	code, _, list := call(t, r, "GET", "/models", "")
	if code != 200 || len(list) != 1 {
		t.Fatal(code, list)
	}
	backup := list[0].(map[string]any)
	savedPrices, ok := backup["associated_prices"].([]any)
	if !ok || len(savedPrices) != 1 || backup["published_specs"] != nil || backup["config"] != nil || backup["catalog_model_ref"] == nil {
		t.Fatal("backup projection omitted associated data", backup)
	}
	backupJSON := catalogJSON(t, map[string]any{"mode": "merge", "models": []any{backup}, "prices": savedPrices})
	m.DisplayName = "Changed"
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePrices([]store.Price{}); err != nil {
		t.Fatal(err)
	}
	code, obj, _ = call(t, r, "POST", "/models/import", backupJSON)
	if code != 200 {
		t.Fatal("restore", code, obj)
	}
	models, err := s.ListDownstreamModels()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.GetPrices()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].DisplayName != "Original" || !reflect.DeepEqual(models[0].CatalogModelRef, m.CatalogModelRef) || len(restored) != 1 || *restored[0].Selection.Table.Rates.Input != 0 || *restored[0].Selection.Table.Rates.Output != 0.0000000001 {
		t.Fatal("restore changed model/associated price precision", models, restored)
	}
	beforeModels := models
	beforePrices := restored
	backup["display_name"] = "partial"
	savedPrices[0].(map[string]any)["selection"].(map[string]any)["table"].(map[string]any)["unit_tokens"] = float64(-1)
	code, _, _ = call(t, r, "POST", "/models/import", catalogJSON(t, map[string]any{"mode": "overwrite", "models": []any{backup}, "prices": savedPrices}))
	if code != 400 {
		t.Fatal("invalid backup price accepted", code)
	}
	models, _ = s.ListDownstreamModels()
	restored, _ = s.GetPrices()
	if !reflect.DeepEqual(beforeModels, models) || !reflect.DeepEqual(beforePrices, restored) {
		t.Fatal("failed import partly replaced model or price", models, restored)
	}
	channels, _ := s.ListChannels()
	if channels[0].APIKey != "credential-stays-intact" {
		t.Fatal("restore lost upstream credential")
	}
}

func TestModelsHTTPImportRetiresLegacyPriceRoles(t *testing.T) {
	for _, mode := range []string{"merge", "overwrite"} {
		t.Run(mode, func(t *testing.T) {
			s, h := newTestCatalogConsole(t)
			defer s.Close()
			c := catalogHTTPChannel(t, s, "real")
			model := store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}
			selected := store.Price{Model: model.ID, DisplayName: "selected reference", Origin: "auto", Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "CNY", UnitTokens: 1000, Rates: store.PriceRates{Input: ptr(0.0), Output: ptr(0.0000000001)}, Rules: []store.PriceRule{}, UncoveredConditions: []string{"preserved"}}}}
			var reference map[string]any
			raw, _ := json.Marshal(selected)
			if err := json.Unmarshal(raw, &reference); err != nil {
				t.Fatal(err)
			}
			reference["role"] = "reference"
			defaultPrice := map[string]any{"model": model.ID, "role": "default", "selection": map[string]any{"mode": "manual", "table": store.PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: store.PriceRates{Input: ptr(9.0)}}}}
			code, body, _ := call(t, h, "POST", "/models/import", catalogJSON(t, map[string]any{"mode": mode, "models": []store.DownstreamModel{model}, "prices": []any{defaultPrice, reference}}))
			if code != 200 {
				t.Fatal("legacy role backup rejected", code, body)
			}
			prices, err := s.GetPrices()
			if err != nil || len(prices) != 1 || !reflect.DeepEqual(prices[0], selected) {
				t.Fatal("import changed selected complete entry", prices, err)
			}
			code, _, models := call(t, h, "GET", "/models", "")
			if code != 200 || len(models) != 1 || strings.Contains(catalogJSON(t, models), `"role"`) {
				t.Fatal("import exported retired role", models)
			}
		})
	}
}
