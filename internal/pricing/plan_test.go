package pricing

import (
	"testing"
	"torouter/internal/store"
)

func TestPlanRequestPricesUseFinalModelBilling(t *testing.T) {
	s := backfillStore(t)
	tags, err := s.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	groups, err := s.ListTagGroups()
	if err != nil {
		t.Fatal(err)
	}
	table := backfillTable(999)
	prices := []store.Price{{ChannelID: 1, Model: "up", Selection: store.PriceSelection{Mode: "manual", Table: &table}}, {ChannelID: 0, Model: "public", Selection: store.PriceSelection{Mode: "manual", Table: &table}}}
	for _, tc := range []struct {
		name                   string
		channelTags, modelTags []string
		plan                   bool
	}{
		{"follow plan", []string{"billing:plan"}, nil, true},
		{"independent payg", []string{"billing:plan"}, []string{"billing:payg"}, false},
		{"independent plan", []string{"billing:payg"}, []string{"billing:plan"}, true},
		{"independent unknown", []string{"billing:plan"}, []string{"billing:unknown"}, false},
		{"unknown", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := store.Channel{ID: 1, Tags: tc.channelTags, ModelTags: map[string][]string{"up": tc.modelTags}}
			a, o := ResolveRequestPrices(prices, nil, c, tags, groups, "up", "public")
			u := store.Usage{InputTokens: 1}
			actual, official := Compute(a, u, "request", ""), Compute(o, u, "request", "")
			if official.Amount == nil || *official.Amount != 999 {
				t.Fatal(official)
			}
			if tc.plan {
				if a.Layer != "plan" || actual.Amount != nil || actual.MissingReason != "plan_unallocated" {
					t.Fatal(a, actual)
				}
			} else if a.Layer != "channel" || actual.Amount == nil || *actual.Amount != 999 {
				t.Fatal(a, actual)
			}
		})
	}
	if got := Compute(store.PriceResolution{Layer: "plan", Status: "resolved", Table: &table}, store.Usage{InputTokens: 1}, "request", ""); got.Amount != nil {
		t.Fatal("plan identity must suppress even a token table", got)
	}
	actual, _ := ResolveRequestPrices(nil, nil, store.Channel{ID: 1}, tags, groups, "up", "public")
	if got := Compute(actual, store.Usage{}, "request", ""); got.Amount != nil {
		t.Fatal("unknown became free", got)
	}
}

func TestPlanBackfillSkipsActualPreservesOfficialAndHistoricalIdentity(t *testing.T) {
	for _, mode := range []string{"current", "manual"} {
		t.Run(mode, func(t *testing.T) {
			s := backfillStore(t)
			channel := store.Channel{Name: "now declared plan", APIKey: "test-key", Tags: []string{"billing:plan"}, Models: []string{"up"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "http://localhost"}}}
			if err := s.SaveChannel(&channel); err != nil {
				t.Fatal(err)
			}
			table := backfillTable(7)
			if err := s.SavePrices([]store.Price{{ChannelID: channel.ID, Model: "up", Selection: store.PriceSelection{Mode: "manual", Table: &table}}, {ChannelID: 0, Model: "public", Selection: store.PriceSelection{Mode: "manual", Table: &table}}}); err != nil {
				t.Fatal(err)
			}
			plan := store.Usage{Model: "public", ChannelID: channel.ID, UpstreamModel: "up", InputTokens: 1, ActualPricing: &store.UsagePricingBasis{Resolution: store.PriceResolution{Layer: "plan"}, MissingReason: "plan_unallocated"}}
			past := plan
			past.ActualPricing = nil
			if err := s.AddUsage(plan); err != nil {
				t.Fatal(err)
			}
			if err := s.AddUsage(past); err != nil {
				t.Fatal(err)
			}
			req := BackfillRequest{Mode: mode}
			if mode == "manual" {
				req.Prices = []BackfillPrice{{Key: backfillKey(plan, "official"), Table: table}, {Key: backfillKey(past, "actual"), Table: table}}
			}
			preview, err := PreviewBackfill(s, req)
			if err != nil || preview.CalculableAmounts != 3 || preview.MissingAmounts != 0 {
				t.Fatal(preview, err)
			}
			req = preview.Request
			req.Expected = preview.Expected
			if got, err := ApplyBackfill(s, req); err != nil || got.AppliedAmounts != 3 {
				t.Fatal(got, err)
			}
			rows, err := s.ListUsage(store.UsageFilter{})
			if err != nil || len(rows) != 2 {
				t.Fatal(rows, err)
			}
			if rows[0].ActualCost == nil || *rows[0].ActualCost != 7 || rows[0].OfficialValue == nil {
				t.Fatal("unmarked history changed identity", rows[0])
			}
			if rows[1].ActualCost != nil || rows[1].ActualPricing.Resolution.Layer != "plan" || rows[1].OfficialValue == nil || *rows[1].OfficialValue != 7 {
				t.Fatal("plan actual backfilled or official lost", rows[1])
			}
		})
	}
}
