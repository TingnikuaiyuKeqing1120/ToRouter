package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPricePrefillProtectsEditedTables(t *testing.T) {
	for _, name := range []string{"zero", "disabled_rule", "unit", "note", "empty"} {
		t.Run(name, func(t *testing.T) {
			s, c, _ := catalogFixture(t)
			table := PriceTable{Currency: "USD", UnitTokens: 1000000, Rules: []PriceRule{}}
			switch name {
			case "zero":
				table.Rates.Input = catalogF(0)
			case "disabled_rule":
				table.Rules = []PriceRule{{ID: "draft-rule", Enabled: false, Draft: json.RawMessage(`{"input":"pending"}`)}}
			case "unit":
				table.UnitTokens = 37
			case "note":
				table.UncoveredConditions = []string{"my price note"}
			}
			price := Price{ChannelID: c.ID, Model: "catalog-mini", Origin: "manual", Selection: PriceSelection{Mode: "manual", Table: &table}}
			if err := s.SavePrices([]Price{price}); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetPrices()
			if err != nil {
				t.Fatal(err)
			}
			req := PrefillRequest{Targets: []PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: "catalog-mini"}}, Groups: []string{"price"}, PriceMode: "manual"}
			if _, err := s.ApplyCatalogPrefill(req); err != nil {
				t.Fatal(err)
			}
			after, err := s.GetPrices()
			if err != nil {
				t.Fatal(err)
			}
			if name == "empty" {
				if len(after) != 1 || after[0].Selection.Table.Rates.Input == nil || *after[0].Selection.Table.Rates.Input != 1 {
					t.Fatalf("empty placeholder was not filled: %+v", after)
				}
			} else {
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("non-force prefill replaced %s edits: before=%+v after=%+v", name, before[0].Selection.Table, after[0].Selection.Table)
				}
				req.ForceOverwrite = true
				if _, err := s.ApplyCatalogPrefill(req); err != nil {
					t.Fatal(err)
				}
				after, _ = s.GetPrices()
				if after[0].Selection.Table.Rates.Input == nil || *after[0].Selection.Table.Rates.Input != 1 {
					t.Fatal("explicit force did not replace table")
				}
			}
		})
	}
}

func TestPricePrefillUsesCurrentDraft(t *testing.T) {
	s, c, _ := catalogFixture(t)
	makeRequest := func(input any) PrefillRequest {
		t.Helper()
		raw, err := json.Marshal(map[string]any{
			"targets": []any{map[string]any{"kind": "upstream", "channel_id": c.ID, "model": "catalog-mini", "price_draft": map[string]any{
				"mode": "manual", "table": map[string]any{"currency": "USD", "unit_tokens": 1000000, "rates": map[string]any{"input": input}, "rules": []any{}},
			}}}, "groups": []string{"price"}, "price_mode": "manual",
		})
		if err != nil {
			t.Fatal(err)
		}
		var req PrefillRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		return req
	}
	for _, value := range []float64{123, 0} {
		req := makeRequest(value)
		preview, err := s.PreviewCatalogPrefill(req)
		if err != nil || len(preview.Patches) != 1 || preview.Patches[0].Price != nil {
			t.Fatalf("unsaved draft %v was not protected: %+v %v", value, preview, err)
		}
		req.ForceOverwrite = true
		preview, err = s.PreviewCatalogPrefill(req)
		if err != nil || preview.Patches[0].Price == nil || *preview.Patches[0].Price.Selection.Table.Rates.Input != 1 {
			t.Fatalf("explicit overwrite should fill draft: %+v %v", preview, err)
		}
		if _, err := s.ApplyCatalogPrefill(req); err == nil {
			t.Fatal("draft was allowed to write through batch apply")
		}
	}
	prices, _ := s.GetPrices()
	if len(prices) != 0 {
		t.Fatal("draft preview persisted prices", prices)
	}
	// An empty current draft takes precedence over an older saved price too.
	if err := s.SavePrices([]Price{{ChannelID: c.ID, Model: "catalog-mini", Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: PriceRates{Input: catalogF(9)}}}}}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewCatalogPrefill(makeRequest(nil))
	if err != nil || preview.Patches[0].Price == nil {
		t.Fatalf("saved value overrode cleared current draft: %+v %v", preview, err)
	}
}

func TestPriceOnlyPrefillDoesNotChangeSpecification(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "protected_price", true: "replaced_price"}[force], func(t *testing.T) {
			s, c, sourceA := catalogFixture(t)
			sourceB := sourceA
			sourceB.ID = "other-catalog-model"
			sourceB.Aliases = nil
			if err := s.SaveExternalCatalog("openrouter", []CatalogItem{sourceA, sourceB}); err != nil {
				t.Fatal(err)
			}
			refA := CatalogModelRef{Source: sourceA.Source, ID: sourceA.ID, OfferingKey: "original"}
			c.ModelCatalogRefs = map[string]CatalogModelRef{"catalog-mini": refA}
			c.ModelConfigs = map[string]ModelConfig{"catalog-mini": {ContextWindow: catalogI(32000)}}
			if err := s.SaveChannel(c); err != nil {
				t.Fatal(err)
			}
			before, _ := s.ListChannels()
			if err := s.SavePrices([]Price{{ChannelID: c.ID, Model: "catalog-mini", Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: PriceRates{Input: catalogF(7)}}}}}); err != nil {
				t.Fatal(err)
			}
			req := PrefillRequest{Targets: []PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: "catalog-mini", Ref: &CatalogModelRef{Source: sourceB.Source, ID: sourceB.ID, OfferingKey: "original"}}}, Groups: []string{"price"}, PriceMode: "manual", ForceOverwrite: force}
			plan, err := s.ApplyCatalogPrefill(req)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := s.ListChannels()
			if !reflect.DeepEqual(before[0].ModelConfigs, after[0].ModelConfigs) || !reflect.DeepEqual(before[0].ModelCatalogRefs, after[0].ModelCatalogRefs) {
				t.Fatalf("price-only apply changed specifications or their association: %+v", after[0].ModelCatalogRefs)
			}
			if !force && plan.Updated != 0 {
				t.Fatal("protected price with no capability write counted as update", plan.Updated)
			}
			prices, _ := s.GetPrices()
			want := 7.0
			if force {
				want = 1
			}
			if *prices[0].Selection.Table.Rates.Input != want {
				t.Fatal("price did not follow selected overwrite scope", prices)
			}
		})
	}
}

func TestPriceDraftPreviewDoesNotRequireLiveModel(t *testing.T) {
	for _, kind := range []string{"downstream", "upstream"} {
		t.Run(kind, func(t *testing.T) {
			s, c, source := catalogFixture(t)
			channelID := int64(0)
			if kind == "upstream" {
				channelID = c.ID
				if err := s.DeleteChannel(c.ID); err != nil {
					t.Fatal(err)
				}
			}
			selection := PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000000}}
			req := PrefillRequest{Targets: []PrefillTarget{{Kind: kind, ChannelID: channelID, Model: "standalone-price", PriceDraft: &selection,
				Ref: &CatalogModelRef{Source: source.Source, ID: source.ID, OfferingKey: "original"}}}, Groups: []string{"price"}, PriceMode: "manual"}
			preview, err := s.PreviewCatalogPrefill(req)
			if err != nil || len(preview.Patches) != 1 || preview.Patches[0].Price == nil || *preview.Patches[0].Price.Selection.Table.Rates.Input != 1 {
				t.Fatalf("independent new price cannot be previewed: %+v %v", preview, err)
			}
			// A historical row and the current form remain editable after the
			// routing entity has gone. Preview must not recreate that entity.
			selection.Table.Rates.Input = catalogF(9)
			if err := s.SavePrices([]Price{{ChannelID: channelID, Model: "standalone-price", Selection: selection}}); err != nil {
				t.Fatal(err)
			}
			before, _ := s.GetPrices()
			preview, err = s.PreviewCatalogPrefill(req)
			if err != nil || len(preview.Patches) != 1 || preview.Patches[0].Price != nil {
				t.Fatalf("historical price draft was not protected: %+v %v", preview, err)
			}
			if _, err := s.ApplyCatalogPrefill(req); err == nil {
				t.Fatal("independent draft must save through the price workbench")
			}
			after, _ := s.GetPrices()
			models, _ := s.ListDownstreamModels()
			channels, _ := s.ListChannels()
			if !reflect.DeepEqual(before, after) || len(models) != 0 || kind == "upstream" && len(channels) != 0 {
				t.Fatal("draft preview or rejected apply changed persisted state")
			}
		})
	}
}
