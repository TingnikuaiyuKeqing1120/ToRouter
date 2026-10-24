package console

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestOrdinarySavesRejectPriceFields(t *testing.T) {
	for _, endpoint := range []string{"settings", "channels", "models"} {
		t.Run(endpoint, func(t *testing.T) {
			s, _ := newTestCatalogConsole(t)
			h := New(s, "http://127.0.0.1:17878", Hooks{})
			defer s.Close()
			c := catalogHTTPChannel(t, s, "native")
			m := store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "native", Enabled: true}}}
			if err := s.SaveDownstreamModel(m); err != nil {
				t.Fatal(err)
			}
			old := []store.Price{{Model: m.ID, Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "USD", UnitTokens: 1000, Rates: store.PriceRates{Input: ptr(11.0)}}}}}
			if err := s.SavePrices(old); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			method, path := "PUT", "/settings"
			switch endpoint {
			case "settings":
				_, payload, _ = call(t, h, "GET", path, "")
			case "channels":
				path = fmt.Sprintf("/channels/%d", c.ID)
				payload = map[string]any{"name": "changed", "models": c.Models, "endpoints": c.Endpoints, "associated_prices": []store.Price{}}
			case "models":
				path = "/models/public"
				payload = map[string]any{"id": m.ID, "display_name": "changed", "bindings": m.Bindings, "associated_prices": old}
			}
			old[0].Selection.Table.Rates.Input = ptr(99.0)
			if err := s.SavePrices(old); err != nil {
				t.Fatal(err)
			}
			beforePrices, _ := s.GetPrices()
			beforeModels, _ := s.ListDownstreamModels()
			beforeChannel, _ := s.Channel(c.ID)
			beforeSettings, _ := s.GetSettings()
			code, obj, _ := call(t, h, method, path, catalogJSON(t, payload))
			if code != 400 {
				t.Errorf("expected rejected price field, got %d: %v", code, obj)
			}
			afterPrices, _ := s.GetPrices()
			afterModels, _ := s.ListDownstreamModels()
			afterChannel, _ := s.Channel(c.ID)
			afterSettings, _ := s.GetSettings()
			if !reflect.DeepEqual(beforePrices, afterPrices) || !reflect.DeepEqual(beforeModels, afterModels) || !reflect.DeepEqual(beforeChannel, afterChannel) || !reflect.DeepEqual(beforeSettings, afterSettings) {
				t.Fatal("rejected ordinary save mutated state")
			}
		})
	}
}

func TestModelCreateAndRenameConflictsDoNotOverwrite(t *testing.T) {
	s, _ := newTestCatalogConsole(t)
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	defer s.Close()
	c := catalogHTTPChannel(t, s, "native")
	a := store.DownstreamModel{ID: "a", DisplayName: "source", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "native", Enabled: true}}}
	b := a
	b.ID, b.DisplayName = "b", "target"
	for _, m := range []store.DownstreamModel{a, b} {
		if err := s.SaveDownstreamModel(m); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.ListDownstreamModels()
	a.ID = "b"
	for _, path := range []string{"/models", "/models/a/rename"} {
		code, obj, _ := call(t, h, "POST", path, catalogJSON(t, a))
		if code != 409 {
			t.Errorf("%s expected 409, got %d: %v", path, code, obj)
		}
		after, _ := s.ListDownstreamModels()
		if !reflect.DeepEqual(before, after) {
			t.Errorf("%s overwrote existing model", path)
		}
	}
}

func TestBuiltinTagForcedDeletionLeavesReferencesIntact(t *testing.T) {
	s, _ := newTestCatalogConsole(t)
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	defer s.Close()
	c := catalogHTTPChannel(t, s, "native")
	c.Tags = []string{"billing:payg"}
	c.ModelTags = map[string][]string{"native": {"billing:payg"}}
	if err := s.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	m := store.DownstreamModel{ID: "public", Tags: c.Tags, Bindings: []store.Binding{{ChannelID: c.ID, Model: "native"}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	beforeC, _ := s.Channel(c.ID)
	beforeM, _ := s.ListDownstreamModels()
	code, obj, _ := call(t, h, "DELETE", "/tags/billing:payg?force=true", "")
	if code != 400 {
		t.Fatal(code, obj)
	}
	afterC, _ := s.Channel(c.ID)
	afterM, _ := s.ListDownstreamModels()
	if !reflect.DeepEqual(beforeC, afterC) || !reflect.DeepEqual(beforeM, afterM) {
		t.Fatal("builtin deletion rejected after detaching references")
	}
}

func TestRenameModelHTTPPreservesPriceAndRejectsHistoricalConflict(t *testing.T) {
	s, h := newTestCatalogConsole(t)
	defer s.Close()
	c := catalogHTTPChannel(t, s, "native")
	m := store.DownstreamModel{ID: "old", DisplayName: "original", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "native", Enabled: true}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	prices := []store.Price{{Model: "old", Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "USD", UnitTokens: 1000, Rates: store.PriceRates{Input: ptr(3.0)}}}}}
	prices = append(prices, prices[0])
	prices[1].Model = "historical"
	if err := s.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	m.ID = "historical"
	code, obj, _ := call(t, h, "POST", "/models/old/rename", catalogJSON(t, m))
	if code != 409 {
		t.Fatal("historical price conflict", code, obj)
	}
	m.ID, m.DisplayName = "new", "edited"
	code, obj, _ = call(t, h, "POST", "/models/old/rename", catalogJSON(t, m))
	if code != 200 || obj["id"] != "new" {
		t.Fatal("rename", code, obj)
	}
	models, _ := s.ListDownstreamModels()
	got, _ := s.GetPrices()
	if len(models) != 1 || !reflect.DeepEqual(models[0], m) || len(got) != 3 || got[0].Model != "old" || got[1].Model != "historical" || got[2].Model != "new" || *got[2].Selection.Table.Rates.Input != 3 {
		t.Fatal("rename omitted edit or price", models, got)
	}
}

func TestTagDeleteHTTPKeepsConfirmationAndCompletesCascade(t *testing.T) {
	s, _ := newTestCatalogConsole(t)
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	c := catalogHTTPChannel(t, s, "native")
	const tagID = "custom:tag"
	if err := s.SaveTag(store.Tag{ID: tagID, GroupID: "billing", Name: "custom"}); err != nil {
		t.Fatal(err)
	}
	c.Tags = []string{tagID}
	if err := s.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Channel(c.ID)
	code, obj, _ := call(t, h, "DELETE", "/tags/"+tagID, "")
	if code != 409 || obj["needs_confirm"] != true || obj["references"] == nil {
		t.Fatal("missing confirmation response", code, obj)
	}
	after, _ := s.Channel(c.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("confirmation request detached tags")
	}
	code, obj, _ = call(t, h, "DELETE", "/tags/"+tagID+"?force=true", "")
	if code != 200 {
		t.Fatal("confirmed cascade", code, obj)
	}
	refs, err := s.GetTagReferences(tagID)
	if err != nil || len(refs.ChannelNames) != 0 {
		t.Fatal(refs, err)
	}
}

func TestOrdinarySavesWithoutPricesKeepLatestPrices(t *testing.T) {
	s, _ := newTestCatalogConsole(t)
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	c := catalogHTTPChannel(t, s, "native")
	m := store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "native", Enabled: true}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	price := store.Price{Model: "public", Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "USD", UnitTokens: 1000, Rates: store.PriceRates{Input: ptr(99.0)}}}}
	if err := s.SavePrices([]store.Price{price}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetPrices()
	_, settings, _ := call(t, h, "GET", "/settings", "")
	delete(settings, "prices")
	settings["retention_days"] = 60
	for _, req := range []struct {
		path string
		body any
	}{
		{"/settings", settings},
		{fmt.Sprintf("/channels/%d", c.ID), c},
		{"/models/public", m},
	} {
		code, obj, _ := call(t, h, "PUT", req.path, catalogJSON(t, req.body))
		if code != 200 {
			t.Fatal(req.path, code, obj)
		}
		after, _ := s.GetPrices()
		if !reflect.DeepEqual(before, after) {
			t.Fatal(req.path, "changed prices")
		}
	}
}

func TestEncodedModelRenameAndTagDeletion(t *testing.T) {
	for _, source := range []struct{ id, encoded string }{
		{"vendor:source", "vendor%3Asource"},
		{"literal%3Asource", "literal%253Asource"},
	} {
		t.Run(source.id, func(t *testing.T) {
			s, _ := newTestCatalogConsole(t)
			defer s.Close()
			h := New(s, "http://127.0.0.1:17878", Hooks{})
			c := catalogHTTPChannel(t, s, "native")
			m := store.DownstreamModel{ID: source.id, Bindings: []store.Binding{{ChannelID: c.ID, Model: "native"}}}
			if err := s.SaveDownstreamModel(m); err != nil {
				t.Fatal(err)
			}
			m.DisplayName = "edited"
			code, obj, _ := call(t, h, "PUT", "/models/"+source.encoded, catalogJSON(t, m))
			if code != 200 {
				t.Errorf("encoded same-ID save: %d %v", code, obj)
			}
			m.ID = "renamed:target"
			code, obj, _ = call(t, h, "POST", "/models/"+source.encoded+"/rename", catalogJSON(t, m))
			if code != 200 {
				t.Errorf("encoded rename: %d %v", code, obj)
			}
			code, obj, _ = call(t, h, "DELETE", "/models/renamed%3Atarget", "")
			if code != 200 {
				t.Errorf("encoded model delete: %d %v", code, obj)
			}
			if err := s.SaveTag(store.Tag{ID: source.id, GroupID: "billing", Name: "custom"}); err != nil {
				t.Fatal(err)
			}
			c.Tags = []string{source.id}
			if err := s.SaveChannel(c); err != nil {
				t.Fatal(err)
			}
			code, obj, _ = call(t, h, "DELETE", "/tags/"+source.encoded+"?force=true", "")
			if code != 200 {
				t.Errorf("encoded tag deletion: %d %v", code, obj)
			}
			saved, _ := s.Channel(c.ID)
			if len(saved.Tags) != 1 || saved.Tags[0] != "billing:unknown" {
				t.Error("encoded tag deletion did not restore group unknown")
			}
		})
	}
	s, _ := newTestCatalogConsole(t)
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	code, obj, _ := call(t, h, "DELETE", "/tags/billing%3Apayg?force=true", "")
	if code != 400 || !strings.Contains(catalogJSON(t, obj), "cannot delete builtin core tag") {
		t.Fatal("encoded builtin ID did not reach builtin guard", code, obj)
	}
}
