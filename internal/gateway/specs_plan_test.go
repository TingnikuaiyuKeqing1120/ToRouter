package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"torouter/internal/store"
)

func TestSpecsPlanDoesNotExposeRetainedTokenPriceAsCurrentPAYG(t *testing.T) {
	g, s := setup(t)
	c := store.Channel{Name: "mixed", APIKey: "test-key", Enabled: true, Models: []string{"plan-model", "payg-model"}, Tags: []string{"billing:plan"}, ModelTags: map[string][]string{"payg-model": {"billing:payg"}}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "http://localhost"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "plan-model", Enabled: true}, {ChannelID: c.ID, Model: "payg-model", Enabled: true, Priority: 1}}}); err != nil {
		t.Fatal(err)
	}
	prices := []store.Price{priceRow(c.ID, "plan-model", "CNY", store.PriceRates{Input: pricingNumber(999)}), priceRow(c.ID, "payg-model", "CNY", store.PriceRates{Input: pricingNumber(3)}), priceRow(0, "public", "USD", store.PriceRates{Input: pricingNumber(2)})}
	if err := s.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, w *httptest.ResponseRecorder, count int) {
		t.Helper()
		var got ModelSpecsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || len(got.Data) != 1 {
			t.Fatal(w.Code, w.Body.String(), err)
		}
		m := got.Data[0]
		if len(m.Bindings) != count {
			t.Fatal(m.Bindings)
		}
		for _, b := range m.Bindings {
			if b.Model == "plan-model" {
				if b.PAYG.Layer != "plan" || b.PAYG.Table != nil || b.PAYG.MissingReason != "plan_unallocated" {
					t.Fatalf("plan projected retained token price: %+v", b.PAYG)
				}
			} else if b.Model != "payg-model" || b.PAYG.Layer != "channel" || b.PAYG.Table == nil || *b.PAYG.Table.Rates.Input != 3 {
				t.Fatal(b.PAYG)
			}
		}
		official := m.Pricing["official"]
		if official.Layer != "official" || official.Table == nil || official.Table.Currency != "USD" || *official.Table.Rates.Input != 2 {
			t.Fatal(official)
		}
	}
	t.Run("management", func(t *testing.T) {
		w := httptest.NewRecorder()
		g.ModelSpecs(w, httptest.NewRequest("GET", "/models/specs", nil))
		check(t, w, 2)
	})
	_, token := scopeKey(t, s, store.DefaultClientKeyScope())
	t.Run("client unrestricted", func(t *testing.T) { check(t, scopedTurn(g, token, "GET", "/v1/models/specs", "", ""), 2) })
	scope := store.DefaultClientKeyScope()
	scope.TagRules["billing"] = []string{"billing:plan"}
	_, planToken := scopeKey(t, s, scope)
	t.Run("client filtered", func(t *testing.T) {
		w := scopedTurn(g, planToken, "GET", "/v1/models/specs", "", "")
		check(t, w, 1)
		var got ModelSpecsResponse
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got.Data[0].Bindings[0].Model != "plan-model" {
			t.Fatal("key filter changed", got.Data)
		}
	})
	saved, err := s.GetPrices()
	if err != nil || len(saved) != 3 {
		t.Fatal("projection changed saved prices", saved, err)
	}
	for _, p := range saved {
		if p.ChannelID == c.ID && p.Model == "plan-model" && (p.Selection.Table == nil || *p.Selection.Table.Rates.Input != 999) {
			t.Fatal("retained price deleted", p)
		}
	}
}
