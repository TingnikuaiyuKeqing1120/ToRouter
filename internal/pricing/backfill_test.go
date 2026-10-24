package pricing

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
	"torouter/internal/store"
)

func backfillFloat(v float64) *float64 { return &v }

func TestBackfillNumberNormalizationDoesNotExpandExponents(t *testing.T) {
	for _, pair := range [][2]string{
		{"1e1000000", "10e999999"},
		{"1e-1000000", "10e-1000001"},
		{"1e-99999999999999999999999999", "10e-100000000000000000000000000"},
		{"1e99999999999999999999999999", "10e99999999999999999999999998"},
		{"0e99999999999999999999999999", "-0.00e-99999999999999999999999999"},
		{"0.0012300", "123e-5"},
		{"-1200.00", "-12e2"},
	} {
		left := normalizeBackfillNumbers(json.Number(pair[0])).(json.Number)
		right := normalizeBackfillNumbers(json.Number(pair[1])).(json.Number)
		if left != right {
			t.Fatalf("equivalent numbers differ: %s != %s", left, right)
		}
		if len(left) > len(pair[0])+3 || len(right) > len(pair[1])+3 {
			t.Fatalf("number expanded: %s", left)
		}
	}
	if normalizeBackfillNumbers(json.Number("9007199254740993")) == normalizeBackfillNumbers(json.Number("9007199254740992")) {
		t.Fatal("different large integers collapsed")
	}
}

func TestBackfillResolutionSurvivesJSONRoundTrip(t *testing.T) {
	for _, mode := range []string{"current", "manual"} {
		t.Run(mode, func(t *testing.T) {
			s := backfillStore(t)
			v := backfillTable(2)
			v.Rules = []store.PriceRule{{ID: "disabled-draft", Enabled: false, Draft: json.RawMessage(`{"count":9007199254740993,"one":1.0,"thousand":1e3,"note":"keep draft"}`)}}
			u := store.Usage{Model: "public", ChannelID: 1, UpstreamModel: "upstream", InputTokens: 1}
			if err := s.SavePrices([]store.Price{{ChannelID: 1, Model: "upstream", Selection: store.PriceSelection{Mode: "manual", Table: &v}}}); err != nil {
				t.Fatal(err)
			}
			if err := s.AddUsage(u); err != nil {
				t.Fatal(err)
			}
			req := BackfillRequest{Mode: mode, Amounts: []string{"actual"}}
			if mode == "manual" {
				req.Prices = []BackfillPrice{{Key: backfillKey(u, "actual"), Table: v}}
			}
			p, err := PreviewBackfill(s, req)
			if err != nil {
				t.Fatal(err)
			}
			req = p.Request
			req.Expected = p.Expected
			// A JSON client can reformat/reorder an opaque disabled draft without
			// changing the price. Large integers must still compare exactly.
			req.Expected[0].Resolution.Table.Rules[0].Draft = json.RawMessage(`{ "note": "keep draft", "count": 9007199254740993, "one": 1, "thousand": 1000 }`)
			raw, err := json.MarshalIndent(req, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip BackfillRequest
			if err := json.Unmarshal(raw, &roundTrip); err != nil {
				t.Fatal(err)
			}
			changed := roundTrip
			changed.Expected = []BackfillExpected{{Key: roundTrip.Expected[0].Key, Resolution: roundTrip.Expected[0].Resolution}}
			changed.Expected[0].Resolution.Table = cloneTable(roundTrip.Expected[0].Resolution.Table)
			changed.Expected[0].Resolution.Table.Rules[0].Draft = json.RawMessage(`{"note":"keep draft","count":9007199254740992,"one":1,"thousand":1000}`)
			if _, err := ApplyBackfill(s, changed); !errors.Is(err, ErrPreviewConflict) {
				t.Fatalf("changed draft was accepted: %v", err)
			}
			changed.Expected[0].Resolution.Table.Rules[0].Draft = json.RawMessage(`{"note":"keep draft","count":9007199254740993,"one":"1","thousand":1000}`)
			if _, err := ApplyBackfill(s, changed); !errors.Is(err, ErrPreviewConflict) {
				t.Fatalf("number changed to string was accepted: %v", err)
			}
			changed.Expected[0].Resolution.Table = cloneTable(roundTrip.Expected[0].Resolution.Table)
			changed.Expected[0].Resolution.Table.Rates.Input = backfillFloat(9)
			if _, err := ApplyBackfill(s, changed); !errors.Is(err, ErrPreviewConflict) {
				t.Fatalf("changed rate was accepted: %v", err)
			}
			if result, err := ApplyBackfill(s, roundTrip); err != nil || result.AppliedAmounts != 1 {
				t.Fatalf("JSON formatting caused a false conflict: %+v %v", result, err)
			}
		})
	}
}
func backfillTable(rate float64) store.PriceTable {
	return store.PriceTable{Currency: "USD", UnitTokens: 1, Rates: store.PriceRates{Input: backfillFloat(rate), Output: backfillFloat(rate)}, Rules: []store.PriceRule{}}
}

func TestBackfillGroupsUseMetricIdentityAndHistoricalMetadata(t *testing.T) {
	s := backfillStore(t)
	when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	usages := []store.Usage{
		{Model: "public-a", ChannelID: 1, ChannelName: "old name", UpstreamModel: "real-upstream", Time: when, InputTokens: 1},
		{Model: "public-b", ChannelID: 1, ChannelName: "shared name", UpstreamModel: "real-upstream", Time: when.Add(time.Hour), InputTokens: 1, Estimated: true},
		{Model: "public-a", ChannelID: 2, ChannelName: "shared name", UpstreamModel: "real-upstream", Time: when.Add(2 * time.Hour), InputTokens: 1},
		{Model: "unknown-a", ChannelID: 1, Time: when, InputTokens: 1},
		{Model: "unknown-b", ChannelID: 1, Time: when, InputTokens: 1},
	}
	table := backfillTable(1)
	if err := s.SavePrices([]store.Price{{ChannelID: 1, Model: "real-upstream", Selection: store.PriceSelection{Mode: "manual", Table: &table}}}); err != nil {
		t.Fatal(err)
	}
	for _, u := range usages {
		if err := s.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	p, err := PreviewBackfill(s, BackfillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]BackfillGroup{}
	for _, g := range p.Groups {
		groups[g.Key] = g
	}
	a := groups[backfillKey(usages[0], "actual")]
	if a.Records != 2 || a.CalculableAmounts != 1 || a.MissingReasons["usage_unknown"] != 1 || a.ChannelID != 1 || a.ChannelName != "shared name" || a.Model != "real-upstream" || !reflect.DeepEqual(a.PublicModels, []string{"public-a", "public-b"}) || a.FirstTime != when.Format(time.RFC3339Nano) || a.LastTime != when.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("actual group metadata %+v", a)
	}
	b := groups[backfillKey(usages[2], "actual")]
	if b.Key == a.Key || b.Records != 1 || b.MissingReasons["price_unconfigured"] != 1 {
		t.Fatalf("same channel name merged: %+v", b)
	}
	o := groups[backfillKey(usages[0], "official")]
	if o.Records != 2 || o.ChannelID != 0 || o.ChannelName != "" || o.Model != "public-a" {
		t.Fatalf("official cross-channel group %+v", o)
	}
	if backfillKey(usages[3], "actual") == backfillKey(usages[4], "actual") || len(groups) != 8 {
		t.Fatalf("unknown identities merged or groups lost: %+v", p.Groups)
	}
	for _, u := range usages[3:] {
		if groups[backfillKey(u, "actual")].MissingReasons["upstream_identity_missing"] != 1 {
			t.Fatal("unknown upstream identity not exposed")
		}
	}
	// Keys do not depend on names, order or current price configuration.
	usages[0].ChannelName = "renamed"
	if backfillKey(usages[0], "actual") != a.Key {
		t.Fatal("historical identity key changed with channel name")
	}
}

func TestBackfillManualBatchUsesEachSelectedPriceAndPreservesRules(t *testing.T) {
	s := backfillStore(t)
	when := time.Date(2020, 1, 1, 23, 0, 0, 0, time.UTC)
	usages := []store.Usage{
		{Model: "alias-a", ChannelID: 1, UpstreamModel: "u", Time: when, InputTokens: 2},
		{Model: "alias-b", ChannelID: 1, UpstreamModel: "u", Time: when, InputTokens: 2},
		{Model: "alias-a", ChannelID: 2, UpstreamModel: "u", Time: when, InputTokens: 2},
		{Model: "unselected", ChannelID: 3, UpstreamModel: "other", Time: when, InputTokens: 2},
		{Model: "alias-a", ChannelID: 1, UpstreamModel: "u", Time: when, InputTokens: 2, ActualCost: backfillFloat(0), Currency: "USD"},
		{Model: "alias-a", ChannelID: 1, UpstreamModel: "u", Time: when, InputTokens: 2, Estimated: true},
	}
	for _, u := range usages {
		if err := s.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	a, b := backfillTable(2), backfillTable(3)
	a.Rules = []store.PriceRule{{ID: "night", Enabled: true, When: store.PriceRuleWhen{TimeWindow: &store.PriceTimeWindow{Start: "22:00", End: "06:00", Timezone: "UTC"}}, Action: store.PriceRuleAction{Multiplier: backfillFloat(2)}}}
	req := BackfillRequest{Mode: "manual", Amounts: []string{"actual"}, Prices: []BackfillPrice{{Key: backfillKey(usages[0], "actual"), Table: a}, {Key: backfillKey(usages[2], "actual"), Table: b}}}
	p, err := PreviewBackfill(s, req)
	if err != nil || p.Examined != 5 || p.ExistingAmounts != 1 || p.CalculableAmounts != 3 || p.MissingReasons["usage_unknown"] != 1 || len(p.Groups) != 2 {
		t.Fatalf("selected preview %+v %v", p, err)
	}
	req = p.Request
	req.Expected = p.Expected
	if err := s.AddUsage(usages[0]); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyBackfill(s, req)
	if err != nil || result.AppliedAmounts != 3 {
		t.Fatalf("batch apply %+v %v", result, err)
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	amounts := map[int64]*float64{}
	for _, u := range rows {
		amounts[u.ID] = u.ActualCost
		if u.OfficialValue != nil {
			t.Fatal("unselected metric was written")
		}
		if u.ActualCost != nil && *u.ActualCost == 8 && (u.ActualPricing == nil || len(u.ActualPricing.MatchedRules) != 1) {
			t.Fatal("full manual rules lost")
		}
	}
	for id, want := range map[int64]float64{1: 8, 2: 8, 3: 6, 5: 0} {
		if amounts[id] == nil || *amounts[id] != want {
			t.Fatalf("row %d got %v want %v", id, amounts[id], want)
		}
	}
	if amounts[4] != nil || amounts[6] != nil || amounts[7] != nil {
		t.Fatal("unselected, unknown or post-preview usage was priced")
	}
	again, err := ApplyBackfill(s, req)
	if err != nil || again.AppliedAmounts != 0 || again.ExistingAmounts != 4 {
		t.Fatalf("repeat apply %+v %v", again, err)
	}
}

func TestBackfillManualRejectsInvalidSelectionAtomically(t *testing.T) {
	s := backfillStore(t)
	u := store.Usage{Model: "public", ChannelID: 1, UpstreamModel: "upstream", InputTokens: 1}
	if err := s.AddUsage(u); err != nil {
		t.Fatal(err)
	}
	valid := BackfillPrice{Key: backfillKey(u, "actual"), Table: backfillTable(1)}
	preview, err := PreviewBackfill(s, BackfillRequest{Mode: "manual", Amounts: []string{"actual"}, Prices: []BackfillPrice{valid}})
	if err != nil {
		t.Fatal(err)
	}
	invalidTable := valid
	invalidTable.Table.UnitTokens = 0
	for _, tc := range []struct {
		name    string
		prices  []BackfillPrice
		filter  store.BackfillFilter
		amounts []string
		mode    string
	}{
		{name: "duplicate", prices: []BackfillPrice{valid, valid}},
		{name: "invalid table", prices: []BackfillPrice{invalidTable}},
		{name: "foreign key after valid", prices: []BackfillPrice{valid, {Key: "not-in-range", Table: backfillTable(2)}}},
		{name: "public model filter is not upstream", prices: []BackfillPrice{valid}, filter: store.BackfillFilter{Model: "upstream"}},
		{name: "channel excluded", prices: []BackfillPrice{valid}, filter: store.BackfillFilter{ChannelID: 2}},
		{name: "metric excluded", prices: []BackfillPrice{valid}, amounts: []string{"official"}},
		{name: "current prices forbidden", prices: []BackfillPrice{valid}, mode: "current"},
		{name: "current empty prices forbidden", prices: []BackfillPrice{}, mode: "current"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := preview.Request
			req.Expected = preview.Expected
			req.Prices, req.Filter = tc.prices, tc.filter
			if tc.amounts != nil {
				req.Amounts = tc.amounts
			}
			if tc.mode != "" {
				req.Mode = tc.mode
			}
			if _, err := ApplyBackfill(s, req); err == nil {
				t.Fatal("invalid selection accepted")
			}
			rows, err := s.ListUsage(store.UsageFilter{})
			if err != nil || rows[0].ActualCost != nil || rows[0].OfficialValue != nil {
				t.Fatalf("partial batch write %+v %v", rows, err)
			}
		})
	}
	req := preview.Request
	req.Expected = preview.Expected
	req.Prices[0].Table = backfillTable(2)
	if _, err := ApplyBackfill(s, req); !errors.Is(err, ErrPreviewConflict) {
		t.Fatalf("edited manual table must require fresh preview: %v", err)
	}
}

func TestBackfillManualFiltersConstrainGroupsAndMetric(t *testing.T) {
	s := backfillStore(t)
	when := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	usages := []store.Usage{
		{Model: "public", ChannelID: 1, UpstreamModel: "upstream", Time: when, InputTokens: 1},
		{Model: "public", ChannelID: 1, UpstreamModel: "upstream", Time: when.Add(-time.Hour), InputTokens: 1},
		{Model: "public", ChannelID: 1, UpstreamModel: "upstream", Time: when.Add(time.Hour), InputTokens: 1},
		{Model: "other-public", ChannelID: 1, UpstreamModel: "upstream", Time: when, InputTokens: 1},
		{Model: "public", ChannelID: 2, UpstreamModel: "upstream", Time: when, InputTokens: 1},
		{Model: "public", ChannelID: 1, UpstreamModel: "other-upstream", Time: when, InputTokens: 1},
		{Model: "public", ChannelID: 1, UpstreamModel: "upstream", Time: when, InputTokens: 1, NotSent: true},
	}
	for _, u := range usages {
		if err := s.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	for _, metric := range []string{"actual", "official"} {
		p, err := PreviewBackfill(s, BackfillRequest{Mode: "manual", Amounts: []string{metric}, Filter: store.BackfillFilter{From: when, To: when, ChannelID: 1, Model: "public", UpstreamModel: "upstream"}, Prices: []BackfillPrice{{Key: backfillKey(usages[0], metric), Table: backfillTable(1)}}})
		if err != nil || p.Examined != 1 || len(p.Groups) != 1 || p.Groups[0].Records != 1 {
			t.Fatalf("filter preview %+v %v", p, err)
		}
		req := p.Request
		req.Expected = p.Expected
		if got, err := ApplyBackfill(s, req); err != nil || got.AppliedAmounts != 1 {
			t.Fatalf("filtered apply %+v %v", got, err)
		}
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range rows {
		if u.ID == 1 {
			if u.ActualCost == nil || u.OfficialValue == nil {
				t.Fatal("matching row not priced")
			}
			continue
		}
		if u.ActualCost != nil || u.OfficialValue != nil {
			t.Fatalf("excluded history written %+v", u)
		}
	}
}
func backfillStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestBackfillUsesHistoricalIdentityTimeAndPreservesAmounts(t *testing.T) {
	s := backfillStore(t)
	table := backfillTable(1)
	table.UncoveredConditions = []string{"holiday promotion", "future price adjustment"}
	table.Rules = []store.PriceRule{{ID: "historical-night", Enabled: true, When: store.PriceRuleWhen{TimeWindow: &store.PriceTimeWindow{Start: "22:00", End: "06:00", Timezone: "UTC"}}, Action: store.PriceRuleAction{Multiplier: backfillFloat(2)}}}
	prices := []store.Price{{ChannelID: 1, Model: "A", Selection: store.PriceSelection{Mode: "manual", Table: &table}}, {ChannelID: 2, Model: "B", Selection: store.PriceSelection{Mode: "manual", Table: func() *store.PriceTable { v := backfillTable(9); return &v }()}}, {Model: "coding", Selection: store.PriceSelection{Mode: "manual", Table: func() *store.PriceTable { v := backfillTable(3); return &v }()}}}
	if err := s.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2020, 1, 1, 23, 0, 0, 0, time.UTC)
	for _, u := range []store.Usage{{Model: "coding", ChannelID: 1, UpstreamModel: "A", Time: when, InputTokens: 2, OutputTokens: 1}, {Model: "coding", ChannelID: 1, UpstreamModel: "A", Time: when, InputTokens: 2, ActualCost: backfillFloat(0), Currency: "USD"}, {Model: "coding", ChannelID: 1, UpstreamModel: "A", Time: when, InputTokens: 2, Estimated: true}} {
		if err := s.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := PreviewBackfill(s, BackfillRequest{Mode: "current"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.CalculableAmounts != 3 || preview.ExistingAmounts != 1 || preview.MissingAmounts != 2 {
		t.Fatalf("coverage %+v", preview)
	}
	before, err := s.ListUsage(store.UsageFilter{})
	if err != nil || before[2].ActualCost != nil || before[2].OfficialValue != nil {
		t.Fatalf("preview wrote history: %+v %v", before, err)
	}
	req := preview.Request
	req.Expected = preview.Expected
	applied, err := ApplyBackfill(s, req)
	if err != nil || applied.AppliedAmounts != 3 {
		t.Fatalf("apply %+v %v", applied, err)
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[2].ActualCost == nil || *rows[2].ActualCost != 6 || *rows[2].OfficialValue != 9 {
		t.Fatalf("historical A/night and lower reference: %+v", rows[2])
	}
	if *rows[1].ActualCost != 0 || rows[1].ActualPricing != nil || *rows[1].OfficialValue != 6 {
		t.Fatalf("existing zero immutable: %+v", rows[1])
	}
	if rows[0].ActualCost != nil || rows[0].OfficialValue != nil {
		t.Fatal("unknown usage was priced")
	}
	if rows[2].ActualPricing == nil || rows[2].ActualPricing.Method != "backfill_current" || len(rows[2].ActualPricing.MatchedRules) != 1 {
		t.Fatal("missing calculation basis")
	}
	if len(rows[2].ActualPricing.Resolution.Table.UncoveredConditions) != 2 {
		t.Fatal("price notes lost from persisted calculation basis")
	}
	again, err := ApplyBackfill(s, req)
	if err != nil || again.AppliedAmounts != 0 {
		t.Fatalf("idempotent %+v %v", again, err)
	}
}
func TestBackfillConflictIncludesChangedSelectionAndExcludesNewRows(t *testing.T) {
	s := backfillStore(t)
	v := backfillTable(1)
	if err := s.SavePrices([]store.Price{{Model: "m", Selection: store.PriceSelection{Mode: "manual", Table: &v}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(store.Usage{Model: "m", ChannelID: 1, UpstreamModel: "u", InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewBackfill(s, BackfillRequest{Amounts: []string{"actual"}})
	if err != nil {
		t.Fatal(err)
	}
	req := preview.Request
	req.Expected = preview.Expected
	v2 := backfillTable(2)
	if err = s.SavePrices([]store.Price{{Model: "m", Selection: store.PriceSelection{Mode: "manual", Table: &v}}, {ChannelID: 1, Model: "u", Selection: store.PriceSelection{Mode: "manual", Table: &v2}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = ApplyBackfill(s, req); !errors.Is(err, ErrPreviewConflict) {
		t.Fatalf("new upstream changes selection: %v", err)
	}
	rows, _ := s.ListUsage(store.UsageFilter{})
	if rows[0].ActualCost != nil {
		t.Fatal("conflict wrote amount")
	}
	preview, err = PreviewBackfill(s, BackfillRequest{Amounts: []string{"actual"}})
	if err != nil {
		t.Fatal(err)
	}
	req = preview.Request
	req.Expected = preview.Expected
	if err = s.AddUsage(store.Usage{Model: "m", ChannelID: 1, UpstreamModel: "u", InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = ApplyBackfill(s, req); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListUsage(store.UsageFilter{})
	if rows[0].ActualCost != nil || rows[1].ActualCost == nil {
		t.Fatal("apply range included new record")
	}
}

func TestBackfillNotesDoNotReplaceMissingRatesWithZero(t *testing.T) {
	s := backfillStore(t)
	table := backfillTable(1)
	table.Rates.Output = nil
	table.UncoveredConditions = []string{"cache TTL unknown"}
	priceDownstream := store.Price{Model: "m", Selection: store.PriceSelection{Mode: "manual", Table: &table}}
	priceUpstream := store.Price{ChannelID: 1, Model: "u", Selection: store.PriceSelection{Mode: "manual", Table: &table}}
	if err := s.SavePrices([]store.Price{priceDownstream, priceUpstream}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(store.Usage{Model: "m", ChannelID: 1, UpstreamModel: "u", InputTokens: 1, OutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewBackfill(s, BackfillRequest{Mode: "current"})
	if err != nil || preview.CalculableAmounts != 0 || preview.MissingReasons["rate_missing:output"] != 2 {
		t.Fatalf("missing used rate should remain missing: %+v %v", preview, err)
	}
	req := preview.Request
	req.Expected = preview.Expected
	if applied, err := ApplyBackfill(s, req); err != nil || applied.AppliedAmounts != 0 {
		t.Fatalf("missing rate wrote amounts: %+v %v", applied, err)
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil || rows[0].ActualCost != nil || rows[0].OfficialValue != nil {
		t.Fatalf("missing amounts must stay null: %+v %v", rows, err)
	}
	table.Rates.Output = backfillFloat(0)
	priceDownstream.Selection.Table = &table
	priceUpstream.Selection.Table = &table
	if err := s.SavePrices([]store.Price{priceDownstream, priceUpstream}); err != nil {
		t.Fatal(err)
	}
	preview, err = PreviewBackfill(s, BackfillRequest{Mode: "current"})
	if err != nil || preview.CalculableAmounts != 2 || preview.MissingAmounts != 0 {
		t.Fatalf("known free output should allow both amounts: %+v %v", preview, err)
	}
	req = preview.Request
	req.Expected = preview.Expected
	if applied, err := ApplyBackfill(s, req); err != nil || applied.AppliedAmounts != 2 {
		t.Fatalf("notes blocked free rate: %+v %v", applied, err)
	}
	rows, err = s.ListUsage(store.UsageFilter{})
	if err != nil || rows[0].ActualCost == nil || *rows[0].ActualCost != 1 || rows[0].OfficialValue == nil || *rows[0].OfficialValue != 1 {
		t.Fatalf("both saved amounts should use known prices: %+v %v", rows, err)
	}
}
func TestBackfillManualFullRangeOverListLimit(t *testing.T) {
	s := backfillStore(t)
	for i := 0; i < 10001; i++ {
		if err := s.AddUsage(store.Usage{Model: "m", ChannelID: 9, UpstreamModel: "deleted", InputTokens: 1}); err != nil {
			t.Fatal(err)
		}
	}
	v := backfillTable(0)
	preview, err := PreviewBackfill(s, BackfillRequest{Mode: "manual", Prices: []BackfillPrice{{Key: backfillKey(store.Usage{Model: "m"}, "official"), Table: v}}, Amounts: []string{"official"}, Note: "historical free reference"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Examined != 10001 || preview.CalculableAmounts != 10001 {
		t.Fatalf("range clipped: %+v", preview)
	}
	req := preview.Request
	req.Expected = preview.Expected
	got, err := ApplyBackfill(s, req)
	if err != nil || got.AppliedAmounts != 10001 {
		t.Fatalf("apply clipped: %+v %v", got, err)
	}
	rows, _ := s.ListUsage(store.UsageFilter{Limit: 1})
	if rows[0].OfficialValue == nil || *rows[0].OfficialValue != 0 || rows[0].ActualCost != nil || rows[0].OfficialPricing.Method != "backfill_manual" {
		t.Fatalf("manual independent result: %+v", rows[0])
	}
}
func TestBackfillEmptyPreviewDoesNotAdoptLaterUsage(t *testing.T) {
	s := backfillStore(t)
	p, err := PreviewBackfill(s, BackfillRequest{Mode: "current"})
	if err != nil {
		t.Fatal(err)
	}
	req := p.Request
	req.Expected = p.Expected
	if err = s.AddUsage(store.Usage{Model: "new", InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyBackfill(s, req)
	if err != nil || result.Examined != 0 {
		t.Fatalf("empty scope grew: %+v %v", result, err)
	}
}
