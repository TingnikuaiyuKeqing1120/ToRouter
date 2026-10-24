package store

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestPricesDefaultsValidationAndSplitSaves(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if v.QuotaRefreshSeconds == nil || *v.QuotaRefreshSeconds != 300 {
		t.Fatal("quota default", v)
	}
	prices := []legacyPrice{{Model: "official", Currency: "USD", Input: priceNumber(1)}, {Model: "cn", Currency: "CNY", Input: priceNumber(2), ChannelID: 1}}
	if err := saveLegacyPricesFixture(s, prices); err != nil {
		t.Fatal(err)
	}
	got, err := legacyPricesFixture(s)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Currency != "USD" || got[1].Currency != "CNY" {
		t.Fatal(got)
	}
	v.Prices = nil
	v.RetentionDays = 71
	if err := s.SaveSettings(v); err != nil {
		t.Fatal(err)
	}
	got, _ = legacyPricesFixture(s)
	if len(got) != 2 || got[1].Currency != "CNY" {
		t.Fatal("stale settings lost prices", got)
	}
	if err := saveLegacyPricesFixture(s, []legacyPrice{{Model: "next", Input: priceNumber(3)}}); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetSettings()
	if v.RetentionDays != 71 {
		t.Fatal("price save lost settings", v)
	}
	for _, p := range [][]legacyPrice{
		{{Model: "x", Currency: "EUR"}}, {{Model: "x", Origin: "bad"}}, {{Model: "x", ContextEnabled: true}},
		{{Model: "x"}, {Model: "x"}}, {{Model: "x", Input: priceNumber(-1)}}, {{Model: "x", UnitTokens: -1}},
	} {
		if err := saveLegacyPricesFixture(s, p); err == nil {
			t.Fatalf("accepted invalid prices: %+v", p)
		}
	}
	for _, n := range []int{-1, 1, 29, 86401} {
		v.QuotaRefreshSeconds = &n
		if err := s.SaveSettings(v); err == nil {
			t.Fatalf("accepted quota %d", n)
		}
	}
	for _, n := range []int{0, 30, 86400} {
		v.QuotaRefreshSeconds = &n
		if err := s.SaveSettings(v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPricesConcurrentSplitSaves(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, _ := s.GetSettings()
	v.Prices = nil
	v.RetentionDays = 99
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.SaveSettings(v); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := saveLegacyPricesFixture(s, []legacyPrice{{Model: "final", Currency: "CNY"}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.RetentionDays != 99 || len(got.Prices) != 1 || got.Prices[0].Selection.Table.Currency != "CNY" {
		t.Fatal(got)
	}
}

func priceNumber(v float64) *float64 { return &v }
func priceBound(v int64) *int64      { return &v }
func TestUsageAbsentCurrentAmountsStayEmptyAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,1,'old',200,'{"model":"old","cost":2.5,"pricing_version":0}')`, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(Usage{Model: "new", ActualCost: priceNumber(7.2), Currency: "CNY", OfficialValue: priceNumber(1), OfficialCurrency: "USD"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListUsage(UsageFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if rows[0].ActualCost == nil || *rows[0].ActualCost != 7.2 || rows[0].Currency != "CNY" || rows[1].ActualCost != nil || rows[1].OfficialValue != nil {
		t.Fatal(rows)
	}
	raw, _ := json.Marshal(rows[1])
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	if _, ok := fields["cost"]; ok {
		t.Fatal("removed field still exposed")
	}
	if _, ok := fields["pricing_version"]; ok {
		t.Fatal("removed version still exposed")
	}
}
func TestPriceRuleValidationAndNullableRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	valid := legacyPrice{Model: "x", DisplayName: "Friendly", Origin: "auto", Currency: "USD", UnitTokens: 1000, Input: priceNumber(0), ContextEnabled: true, ContextRanges: []legacyPriceRange{{FromTokens: 101, Mode: "fixed", Rates: &PriceRates{Input: priceNumber(2)}}, {FromTokens: 0, ToTokens: priceBound(100), Mode: "multiplier", Multiplier: priceNumber(1)}}, TimeEnabled: true, TimeWindows: []legacyPriceWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: priceNumber(0.5)}}}
	if err := saveLegacyPricesFixture(s, []legacyPrice{valid}); err != nil {
		t.Fatal(err)
	}
	got, err := legacyPricesFixture(s)
	if err != nil || got[0].Input == nil || *got[0].Input != 0 || got[0].Output != nil || !got[0].ContextEnabled || !got[0].TimeEnabled || got[0].ContextRanges[0].FromTokens != 101 || got[0].ContextRanges[0].ToTokens != nil || got[0].ContextRanges[0].Rates.Output != nil || got[0].DisplayName != "Friendly" {
		t.Fatal(got, err)
	}
	raw, _ := json.Marshal(got[0])
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	for _, key := range []string{"tier_mode", "tiers", "currency_override"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("removed field %s still exposed", key)
		}
	}
	bad := []legacyPrice{
		{Model: "x", ContextEnabled: true},
		{Model: "x", TimeEnabled: true},
		{Model: "x", ContextEnabled: true, ContextRanges: []legacyPriceRange{{FromTokens: -1, Mode: "fixed", Rates: &PriceRates{}}}},
		{Model: "x", ContextEnabled: true, ContextRanges: []legacyPriceRange{{FromTokens: 10, ToTokens: priceBound(9), Mode: "fixed", Rates: &PriceRates{}}}},
		{Model: "x", ContextEnabled: true, ContextRanges: []legacyPriceRange{{FromTokens: 0, ToTokens: priceBound(100), Mode: "fixed", Rates: &PriceRates{}}, {FromTokens: 100, ToTokens: priceBound(200), Mode: "fixed", Rates: &PriceRates{}}}},
		{Model: "x", ContextEnabled: true, ContextRanges: []legacyPriceRange{{FromTokens: 0, Mode: "fixed", Rates: &PriceRates{}}, {FromTokens: 500, ToTokens: priceBound(600), Mode: "fixed", Rates: &PriceRates{}}}},
		{Model: "x", ContextEnabled: true, ContextRanges: []legacyPriceRange{{FromTokens: 0, Mode: "multiplier"}}},
		{Model: "x", TimeEnabled: true, TimeWindows: []legacyPriceWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: priceNumber(1)}, {Start: "01:59", End: "03:00", Mode: "fixed", Rates: &PriceRates{}}}},
		{Model: "x", TimeEnabled: true, TimeWindows: []legacyPriceWindow{{Start: "01:00", End: "01:00", Mode: "multiplier", Multiplier: priceNumber(1)}}},
		{Model: "x", TimeEnabled: true, TimeWindows: []legacyPriceWindow{{Start: "24:00", End: "01:00", Mode: "multiplier", Multiplier: priceNumber(1)}}},
		{Model: "x", TimeEnabled: true, TimeWindows: []legacyPriceWindow{{Start: "01:00", End: "02:00", Mode: "multiplier"}}},
	}
	for i, p := range bad {
		if err := saveLegacyPricesFixture(s, []legacyPrice{p}); err == nil {
			t.Fatalf("accepted invalid enabled rule %d: %+v", i, p)
		}
	}
	valid.ContextRanges[1].ToTokens = priceBound(99) // An uncovered gap is valid.
	valid.TimeWindows = append(valid.TimeWindows, legacyPriceWindow{Start: "02:00", End: "23:00", Mode: "fixed", Rates: &PriceRates{Input: priceNumber(1)}})
	if err := saveLegacyPricesFixture(s, []legacyPrice{valid}); err != nil {
		t.Fatal("gapped ranges or adjacent time windows rejected", err)
	}
	draft := legacyPrice{Model: "draft", ContextRanges: []legacyPriceRange{{FromTokens: -1, Mode: "editing"}}, TimeWindows: []legacyPriceWindow{{Start: "", End: ""}}}
	if err := saveLegacyPricesFixture(s, []legacyPrice{draft}); err != nil {
		t.Fatal("disabled drafts should be retained", err)
	}
	got, err = legacyPricesFixture(s)
	if err != nil || got[0].ContextEnabled || got[0].TimeEnabled || len(got[0].ContextRanges) != 1 || got[0].ContextRanges[0].FromTokens != -1 || len(got[0].TimeWindows) != 1 {
		t.Fatal("disabled drafts lost", got, err)
	}
	draft.ContextEnabled = true
	if err := saveLegacyPricesFixture(s, []legacyPrice{draft}); err == nil {
		t.Fatal("invalid draft enabled without validation")
	}
}

// These fixtures exercise the old serialized contract through the one-time import
// boundary while retaining its original value and validation assertions.
func saveLegacyPricesFixture(s *Store, entries []legacyPrice) error {
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	prices, err := DecodePricesImport(raw)
	if err != nil {
		return err
	}
	return s.SavePrices(prices)
}
func legacyPricesFixture(s *Store) ([]legacyPrice, error) {
	prices, err := s.GetPrices()
	out := []legacyPrice{}
	for _, p := range prices {
		t := p.Selection.Table
		v := legacyPrice{ChannelID: p.ChannelID, Model: p.Model, DisplayName: p.DisplayName, Origin: p.Origin, Currency: t.Currency, UnitTokens: t.UnitTokens, Input: t.Rates.Input, Output: t.Rates.Output, CacheRead: t.Rates.CacheRead, CacheWrite: t.Rates.CacheWrite}
		for _, r := range t.Rules {
			mode := "fixed"
			if r.Action.Multiplier != nil {
				mode = "multiplier"
			}
			var rates *PriceRates
			if r.Action.RateChanges != nil {
				rates = &PriceRates{Input: r.Action.RateChanges["input"], Output: r.Action.RateChanges["output"], CacheRead: r.Action.RateChanges["cache_read"], CacheWrite: r.Action.RateChanges["cache_write"]}
			}
			if w := r.When.InputRange; w != nil {
				v.ContextEnabled = r.Enabled
				v.ContextRanges = append(v.ContextRanges, legacyPriceRange{FromTokens: w.FromTokens, ToTokens: w.ToTokens, Mode: mode, Multiplier: r.Action.Multiplier, Rates: rates})
			}
			if w := r.When.TimeWindow; w != nil {
				v.TimeEnabled = r.Enabled
				v.TimeWindows = append(v.TimeWindows, legacyPriceWindow{Start: w.Start, End: w.End, Mode: mode, Multiplier: r.Action.Multiplier, Rates: rates})
			}
		}
		out = append(out, v)
	}
	return out, err
}
