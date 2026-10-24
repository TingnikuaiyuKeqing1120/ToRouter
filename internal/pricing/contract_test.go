package pricing

import (
	"testing"
	"time"
	"torouter/internal/store"
)

func manual(ch int64, model string, rates store.PriceRates) store.Price {
	return store.Price{ChannelID: ch, Model: model, Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "USD", UnitTokens: 1000, Rates: rates}}}
}
func TestWholeTableSelectionAndFrozenCatalog(t *testing.T) {
	prices := []store.Price{manual(0, "alias", store.PriceRates{Input: number(2), Output: number(4)}), manual(1, "native", store.PriceRates{Input: number(1)})}
	actual, official := ResolveUsagePrices(prices, nil, 1, "native", "alias")
	u := store.Usage{InputTokens: 1000, OutputTokens: 1000}
	a := Compute(actual, u, "request", "")
	o := Compute(official, u, "request", "")
	if a.Amount != nil || a.MissingReason != "rate_missing:output" || o.Amount == nil || *o.Amount != 6 {
		t.Fatalf("cross-layer price stitching: actual=%+v official=%+v", a, o)
	}
	prices[1] = manual(1, "native", store.PriceRates{})
	actual, _ = ResolveUsagePrices(prices, nil, 1, "native", "alias")
	if actual.Status == "resolved" || actual.Layer != "channel" {
		t.Fatal("expected unconfigured upstream without silent downstream fallback, got", actual)
	}
	prices[1].Origin = "auto"
	prices[1].Selection.Table.Rates.Input = number(0)
	actual, _ = ResolveUsagePrices(prices, nil, 1, "native", "alias")
	if actual.Layer != "channel" {
		t.Fatal("explicit free price lost", actual)
	}
	ref := store.CatalogPriceRef{Source: "builtin", ID: "native", OfferingKey: "official", QuoteKey: "standard"}
	prices[1].Selection = store.PriceSelection{Mode: "catalog", Ref: &ref}
	actual, _ = ResolveUsagePrices(prices, nil, 1, "native", "alias")
	if actual.Status != "missing" || actual.Layer != "channel" {
		t.Fatal("broken reference fell back", actual)
	}
	items := []store.CatalogItem{{Source: "builtin", ID: "native", Offerings: []store.CatalogOffering{{Key: "official", Quotes: []store.CatalogQuote{{Key: "standard", Table: store.PriceTable{Currency: "USD", UnitTokens: 1000, Rates: store.PriceRates{Input: number(3), Output: number(5)}}}}}}}}
	actual, _ = ResolveUsagePrices(prices, items, 1, "native", "alias")
	*items[0].Offerings[0].Quotes[0].Table.Rates.Input = 99
	a = Compute(actual, u, "request", "")
	if a.Amount == nil || *a.Amount != 8 {
		t.Fatal("in-flight table changed", a)
	}
	prices[0] = store.Price{Model: "alias", Selection: store.PriceSelection{Mode: "catalog", Ref: &ref}}
	_, official = ResolveUsagePrices(prices, nil, 1, "native", "alias")
	if official.Status != "missing" || official.Layer != "official" {
		t.Fatal("broken official reference did not remain missing", official)
	}
	prices[0] = manual(0, "alias", store.PriceRates{Input: number(9)})
	actual, official = ResolveUsagePrices(prices, nil, 0, "alias", "alias")
	if actual.Status != "unconfigured" || official.Status != "resolved" {
		t.Fatal("missing channel identity borrowed official price", actual, official)
	}
}

func TestRuleConditionsAndNullChanges(t *testing.T) {
	table := store.PriceTable{Currency: "USD", UnitTokens: 100, Rates: store.PriceRates{Input: number(1), Output: number(2), CacheRead: number(1), CacheWrite: number(1)}, UncoveredConditions: []string{"cache TTL unknown", "holiday promotion"}, Rules: []store.PriceRule{
		{ID: "context-night", Enabled: true, When: store.PriceRuleWhen{InputRange: &store.PriceInputRange{FromTokens: 200, ToTokens: bound(300)}, TimeWindow: &store.PriceTimeWindow{Start: "23:00", End: "02:00", Timezone: "Asia/Shanghai"}, Weekdays: []int{1}, Timezone: "Asia/Shanghai"}, Action: store.PriceRuleAction{Multiplier: number(2)}},
		{ID: "date", Enabled: true, When: store.PriceRuleWhen{DateSet: []string{"2026-10-12"}, Timezone: "Asia/Shanghai"}, Action: store.PriceRuleAction{RateChanges: map[string]*float64{"output": number(0)}}},
	}}
	u := store.Usage{Time: time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC), InputTokens: 100, CacheReadTokens: 50, CacheWriteTokens: 50, OutputTokens: 100}
	res := store.PriceResolution{Status: "resolved", Table: &table}
	got := Compute(res, u, "request", "")
	if got.Amount == nil || *got.Amount != 4 || len(got.Basis.MatchedRules) != 2 {
		t.Fatal("AND/order/cache/date boundary", got)
	}
	u.Time = time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC) // Tuesday at end boundary: neither Monday nor the explicit date.
	got = Compute(res, u, "request", "")
	if got.Amount == nil || *got.Amount != 4 || len(got.Basis.MatchedRules) != 0 {
		t.Fatal("day/end boundary", got)
	}
	u.Time = time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC) // Tuesday midnight: date does not anchor to start of night.
	got = Compute(res, u, "request", "")
	if len(got.Basis.MatchedRules) != 0 {
		t.Fatal("cross-midnight changed date ownership", got)
	}
	table.Rules = append(table.Rules, store.PriceRule{Enabled: true, Action: store.PriceRuleAction{RateChanges: map[string]*float64{"input": nil}}})
	got = Compute(res, u, "request", "")
	if got.Amount != nil || got.MissingReason != "rate_missing:input" {
		t.Fatal("explicit null did not erase rate", got)
	}
	u.InputTokens = 0
	got = Compute(res, u, "request", "")
	if got.Amount == nil {
		t.Fatal("zero quantity should not require erased rate", got)
	}
	table.UncoveredConditions = []string{"cache TTL unknown"}
	got = Compute(res, u, "request", "")
	if got.Amount == nil || *got.Amount != 3 || got.MissingReason != "" {
		t.Fatal("supplemental note blocked known rates", got)
	}
}

func TestSupplementalNotesPreserveUsedRateRequirements(t *testing.T) {
	table := store.PriceTable{Currency: "USD", UnitTokens: 100, Rates: store.PriceRates{Input: number(1), Output: number(2), CacheRead: number(3), CacheWrite: number(4)}, UncoveredConditions: []string{"future adjustment", "cache TTL unknown"}}
	u := store.Usage{InputTokens: 100, OutputTokens: 100, CacheReadTokens: 100, CacheWriteTokens: 100}
	res := store.PriceResolution{Status: "resolved", Table: &table}
	got := Compute(res, u, "request", "")
	if got.Amount == nil || *got.Amount != 10 || got.MissingReason != "" {
		t.Fatal("known base/cache rates should calculate despite notes", got)
	}
	for _, tc := range []struct {
		name string
		rate **float64
	}{{"input", &table.Rates.Input}, {"output", &table.Rates.Output}, {"cache_read", &table.Rates.CacheRead}, {"cache_write", &table.Rates.CacheWrite}} {
		t.Run(tc.name, func(t *testing.T) {
			original := *tc.rate
			*tc.rate = nil
			missing := Compute(res, u, "request", "")
			if missing.Amount != nil || missing.MissingReason != "rate_missing:"+tc.name {
				t.Fatal("notes hid a missing used rate", missing)
			}
			*tc.rate = number(0)
			free := Compute(res, u, "request", "")
			if free.Amount == nil || *free.Amount != 10-*original {
				t.Fatal("explicit free rate should calculate", free)
			}
			*tc.rate = original
		})
	}
	table.Currency = "EUR"
	got = Compute(res, u, "request", "")
	if got.Amount != nil || got.MissingReason != "price_table_invalid" {
		t.Fatal("unsupported currency should remain invalid despite notes", got)
	}
}
