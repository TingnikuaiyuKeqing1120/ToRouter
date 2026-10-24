package gateway

import (
	"testing"
	"time"
	"torouter/internal/store"
)

func pricingNumber(v float64) *float64 { return &v }
func priceRow(ch int64, model, currency string, r store.PriceRates) store.Price {
	return store.Price{ChannelID: ch, Model: model, Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: currency, UnitTokens: 1000000, Rates: r, Rules: []store.PriceRule{}}}}
}
func TestRecordIndependentOfficialValueAndActualCost(t *testing.T) {
	g, s := setup(t)
	u := store.Usage{Model: "alias", UpstreamModel: "real", ChannelID: 2, InputTokens: 1000000, OutputTokens: 2000000, CacheReadTokens: 1000000, CacheWriteTokens: 1000000}
	rates := func(n float64) store.PriceRates {
		return store.PriceRates{Input: pricingNumber(n), Output: pricingNumber(0), CacheRead: pricingNumber(0), CacheWrite: pricingNumber(0)}
	}
	cases := []struct {
		name                             string
		prices                           []store.Price
		official, actual                 *float64
		officialCurrency, actualCurrency string
	}{
		{name: "both_unknown"},
		{name: "downstream_default_does_not_fallback_actual", prices: []store.Price{priceRow(0, "alias", "USD", rates(2))}, official: pricingNumber(2), actual: nil, officialCurrency: "USD", actualCurrency: ""},
		{name: "channel_only_no_official_fallback", prices: []store.Price{priceRow(2, "real", "CNY", store.PriceRates{Input: pricingNumber(1), Output: pricingNumber(2), CacheRead: pricingNumber(3), CacheWrite: pricingNumber(4)})}, actual: pricingNumber(12), actualCurrency: "CNY"},
		{name: "parallel_different_currencies", prices: []store.Price{priceRow(0, "alias", "USD", rates(3)), priceRow(2, "real", "CNY", rates(4))}, official: pricingNumber(3), actual: pricingNumber(4), officialCurrency: "USD", actualCurrency: "CNY"},
		{name: "known_zero_prices", prices: []store.Price{priceRow(0, "alias", "USD", rates(0)), priceRow(2, "real", "CNY", rates(0))}, official: pricingNumber(0), actual: pricingNumber(0), officialCurrency: "USD", actualCurrency: "CNY"},
		{name: "missing_output_price_unknown", prices: []store.Price{priceRow(0, "alias", "USD", store.PriceRates{Input: pricingNumber(2)})}},
		{name: "other_channel_no_fallback", prices: []store.Price{priceRow(3, "real", "USD", rates(4))}},
		{name: "official_does_not_match_upstream", prices: []store.Price{priceRow(0, "real", "USD", rates(5))}},
		{name: "actual_does_not_match_downstream", prices: []store.Price{priceRow(2, "alias", "USD", rates(5))}},
		{name: "incomplete_upstream_cannot_splice_downstream", prices: []store.Price{priceRow(2, "real", "CNY", store.PriceRates{Input: pricingNumber(1)}), priceRow(0, "alias", "USD", rates(3))}, official: pricingNumber(3), officialCurrency: "USD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g.record(u, store.Settings{Prices: tc.prices}, time.Now())
			rows, err := s.ListUsage(store.UsageFilter{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			got := rows[0]
			eq := func(a, b *float64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
			if !eq(got.OfficialValue, tc.official) || !eq(got.ActualCost, tc.actual) || got.Currency != tc.actualCurrency || got.OfficialCurrency != tc.officialCurrency {
				t.Fatalf("unexpected classified usage: %+v", got)
			}
		})
	}
}
func TestRecordAppliesSavedRulesIndependentlyToBothPrices(t *testing.T) {
	g, s := setup(t)
	threshold := int64(1000)
	official := priceRow(0, "alias", "USD", store.PriceRates{Input: pricingNumber(1)})
	official.Selection.Table.UnitTokens = 1000
	official.Selection.Table.Rules = []store.PriceRule{{ID: "context", Enabled: true, When: store.PriceRuleWhen{InputRange: &store.PriceInputRange{FromTokens: threshold + 1}}, Action: store.PriceRuleAction{Multiplier: pricingNumber(2)}}}
	actual := priceRow(2, "real", "CNY", store.PriceRates{Input: pricingNumber(3)})
	actual.Selection.Table.UnitTokens = 1000
	actual.Selection.Table.Rules = []store.PriceRule{{ID: "context", Enabled: true, When: store.PriceRuleWhen{InputRange: &store.PriceInputRange{FromTokens: threshold + 1}}, Action: store.PriceRuleAction{Multiplier: pricingNumber(0.5)}}, {ID: "time", Enabled: true, When: store.PriceRuleWhen{TimeWindow: &store.PriceTimeWindow{Start: "00:00", End: "02:00", Timezone: "Asia/Shanghai"}}, Action: store.PriceRuleAction{Multiplier: pricingNumber(0.5)}}}
	if err := s.SavePrices([]store.Price{official, actual}); err != nil {
		t.Fatal(err)
	}
	settings, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	g.record(store.Usage{Time: time.Date(2026, 10, 4, 17, 0, 0, 0, time.UTC), Model: "alias", UpstreamModel: "real", ChannelID: 2, InputTokens: 1500}, settings, time.Now())
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	u := rows[0]
	if u.OfficialValue == nil || *u.OfficialValue != 3 || u.ActualCost == nil || *u.ActualCost != 1.125 || u.OfficialCurrency != "USD" || u.Currency != "CNY" {
		t.Fatal(u)
	}
	if len(u.ActualPricing.MatchedRules) != 2 || len(u.OfficialPricing.MatchedRules) != 1 {
		t.Fatal("missing independent provenance", u)
	}
}
