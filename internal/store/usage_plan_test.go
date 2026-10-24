package store

import (
	"context"
	"testing"
	"time"
)

func TestUsageSummaryMergesPlanChargesWithoutInventingRequestsOrModelCosts(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2020, 1, 1, 23, 30, 0, 0, time.UTC)
	ptr := func(n float64) *float64 { return &n }
	for _, u := range []Usage{
		{Time: at, ChannelID: 1, ChannelName: "historical", Model: "public", Status: 200, InputTokens: 10, ActualPricing: &UsagePricingBasis{Resolution: PriceResolution{Layer: "plan"}}, OfficialValue: ptr(2), OfficialCurrency: "USD"},
		{Time: at, ChannelID: 1, ChannelName: "historical", Model: "public", Status: 404, Estimated: true, ActualPricing: &UsagePricingBasis{Resolution: PriceResolution{Layer: "plan"}}},
		{Time: at, ChannelID: 1, ChannelName: "historical", Model: "payg", Status: 200, ActualCost: ptr(3), Currency: "CNY"},
		{Time: at, ChannelID: 2, ChannelName: "second", Model: "unknown", Status: 200},
		{Time: at, ChannelID: 1, Model: "blocked", NotSent: true},
	} {
		if err := s.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []PlanCharge{
		{ChannelID: 1, ChannelName: "monthly name", Month: "2020-01", Amount: ptr(20), Currency: "USD", PaidAt: at},
		{ChannelID: 2, ChannelName: "second", Month: "2020-01", Amount: ptr(0), Currency: "CNY", PaidAt: at.Add(24 * time.Hour)},
		{ChannelID: 2, ChannelName: "second", Month: "2020-02", Currency: "CNY", PaidAt: at.Add(48 * time.Hour)},
		{ChannelID: 3, ChannelName: "deleted without requests", Month: "2020-01", Amount: ptr(7), Currency: "CNY", PaidAt: at},
		{ChannelID: 1, Month: "2019-12", Amount: ptr(50), Currency: "USD", PaidAt: at.AddDate(0, -1, 0)},
		{ChannelID: 1, Month: "2099-01", Amount: ptr(100), Currency: "USD", PaidAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if _, err := s.db.Exec("INSERT INTO plan_charges(channel_id,channel_name,month,amount,currency,paid_at) VALUES(?,?,?,?,?,?)", c.ChannelID, c.ChannelName, c.Month, c.Amount, c.Currency, c.PaidAt.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	f := UsageFilter{From: at, To: at.AddDate(0, 1, 0), Timezone: "Asia/Shanghai", Limit: 1}
	got, err := s.SummarizeUsage(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 4 || got.PlanRequests != 2 || got.ActualPricedRequests != 1 || got.OfficialPricedRequests != 1 || got.UnknownUsageRequests != 1 || got.InputTokens != 10 {
		t.Fatal(got.UsageMetrics)
	}
	if got.PlanCharges != 4 || got.UnknownPlanCharges != 1 || got.PlanCosts["USD"] != 20 || got.PlanCosts["CNY"] != 7 || got.ActualCosts["CNY"] != 10 || got.ActualCosts["USD"] != 20 {
		t.Fatal(got.UsagePricingTotals)
	}
	if got.ActualComponents["CNY"].Plan != 7 || got.ActualComponents["CNY"].Unattributed != 3 || got.ActualComponents["USD"].Plan != 20 || got.OfficialComponents["USD"].Plan != 0 {
		t.Fatal(got.ActualComponents)
	}
	if len(got.Daily) != 3 || got.Daily[0].Date != "2020-01-02" || got.Daily[0].PlanCharges != 2 || got.Daily[1].Requests != 0 || got.Daily[1].UnknownPlanCharges != 0 || got.Daily[2].UnknownPlanCharges != 1 {
		t.Fatal(got.Daily)
	}
	if len(got.ByChannel) != 3 {
		t.Fatal(got.ByChannel)
	}
	assertUsageMetricsSum(t, got.UsageMetrics, dayMetrics(got.Daily))
	channelMetrics := []UsageMetrics{}
	distributed := 0
	for _, bucket := range got.InputDistribution {
		distributed += bucket.Requests
	}
	if distributed != got.KnownUsageRequests {
		t.Fatal("month fees changed request distribution", got.InputDistribution)
	}
	for _, ch := range got.ByChannel {
		channelMetrics = append(channelMetrics, ch.UsageMetrics)
		assertUsageMetricsSum(t, ch.UsageMetrics, dayMetrics(ch.Daily))
		if *ch.ChannelID == 1 && (ch.Name != "historical" || ch.PlanRequests != 2 || ch.PlanCosts["USD"] != 20) {
			t.Fatal(ch)
		}
		if *ch.ChannelID == 3 && (ch.Requests != 0 || ch.PlanCharges != 1 || len(ch.Models) != 0 || ch.ActualCosts["CNY"] != 7) {
			t.Fatal(ch)
		}
		for _, m := range ch.Models {
			if m.PlanCharges != 0 || m.PlanCosts["USD"] != 0 || m.PlanCosts["CNY"] != 0 {
				t.Fatal("monthly fee attributed to model", m)
			}
		}
	}
	assertUsageMetricsSum(t, got.UsageMetrics, channelMetrics)
	for _, m := range got.ByModel {
		if m.PlanCharges != 0 || m.PlanCosts["USD"] != 0 || m.PlanCosts["CNY"] != 0 {
			t.Fatal(m)
		}
		if m.Name == "public" && (m.PlanRequests != 2 || m.ActualPricedRequests != 0) {
			t.Fatal(m)
		}
	}
	for _, tc := range []struct {
		name              string
		filter            UsageFilter
		charges, requests int
		cny, usd          float64
	}{
		{"channel", UsageFilter{ChannelID: 3}, 1, 0, 7, 0},
		{"model", UsageFilter{Model: "public"}, 0, 2, 0, 0},
		{"status", UsageFilter{Status: 200}, 0, 3, 3, 0},
		{"future explicitly requested", UsageFilter{From: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.SummarizeUsage(context.Background(), tc.filter)
			if err != nil || got.PlanCharges != tc.charges || got.Requests != tc.requests || got.ActualCosts["CNY"] != tc.cny || got.ActualCosts["USD"] != tc.usd {
				t.Fatal(got, err)
			}
		})
	}
}

func TestUsageSummaryAccruesDueFeesAndWarnsAboutUnconfiguredChannels(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	first := now.AddDate(0, 0, -2)
	c := Channel{Name: "uncalled plan", APIKey: "test-key", Tags: []string{"billing:plan"}, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "http://localhost"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	got, err := s.SummarizeUsage(context.Background(), UsageFilter{})
	if err != nil || got.UnconfiguredPlanChannels != 1 || got.PlanCharges != 0 {
		t.Fatal(got, err)
	}
	if err := s.SavePlanPrice(PlanPriceConfig{ChannelID: c.ID, DefaultAmount: nil, Currency: "USD", FirstPaymentAt: first, Timezone: "UTC"}, now); err != nil {
		t.Fatal(err)
	}
	// Save config eagerly accrues; remove the record to exercise summary's own
	// catch-up instead of merely relying on a previous endpoint read.
	if _, err := s.db.Exec("DELETE FROM plan_charges"); err != nil {
		t.Fatal(err)
	}
	got, err = s.SummarizeUsage(context.Background(), UsageFilter{From: first.Add(-time.Hour), To: now})
	if err != nil || got.UnconfiguredPlanChannels != 0 || got.PlanCharges != 1 || got.UnknownPlanCharges != 1 || got.Requests != 0 || len(got.ByChannel) != 1 || len(got.Daily) != 1 {
		t.Fatal(got, err)
	}
	if got.ByChannel[0].UnknownPlanCharges != 1 || got.ByChannel[0].ActualPricedRequests != 0 {
		t.Fatal(got.ByChannel)
	}
	// An explicitly independent plan model without a channel declaration still
	// has an unallocated identity and a current missing-config warning.
	if err := s.AddUsage(Usage{ChannelID: 999, Model: "standalone", ActualPricing: &UsagePricingBasis{Resolution: PriceResolution{Layer: "plan"}}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.SummarizeUsage(context.Background(), UsageFilter{Model: "standalone"})
	if err != nil || got.UnconfiguredPlanChannels != 1 || got.PlanRequests != 1 || got.PlanCharges != 0 {
		t.Fatal(got, err)
	}
	got, err = s.SummarizeUsage(context.Background(), UsageFilter{ChannelID: c.ID, Model: "missing"})
	if err != nil || got.UnconfiguredPlanChannels != 0 || got.PlanCharges != 0 {
		t.Fatal(got, err)
	}
}

func TestUsageSummaryPausedPlanConfigDoesNotCoverIndependentPlanModel(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	c := Channel{Name: "mixed", APIKey: "test-key", Tags: []string{"billing:plan"}, Models: []string{"independent"}, ModelTags: map[string][]string{"independent": {"billing:plan"}}, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "http://localhost"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	amount := 10.0
	if err := s.SavePlanPrice(PlanPriceConfig{ChannelID: c.ID, DefaultAmount: &amount, Currency: "USD", FirstPaymentAt: now, Timezone: "UTC"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:payg"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(Usage{Time: now, ChannelID: c.ID, Model: "public", UpstreamModel: "independent", Status: 200, ActualPricing: &UsagePricingBasis{Resolution: PriceResolution{Layer: "plan"}}}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []UsageFilter{{}, {Model: "public"}, {Status: 200}, {ChannelID: c.ID}} {
		got, err := s.SummarizeUsage(context.Background(), f)
		if err != nil || got.UnconfiguredPlanChannels != 1 || got.PlanRequests != 1 {
			t.Fatalf("paused config hid missing declaration: summary=%+v err=%v", got, err)
		}
	}
	for _, f := range []UsageFilter{{ChannelID: c.ID + 1}, {Model: "other"}, {Status: 404}} {
		got, err := s.SummarizeUsage(context.Background(), f)
		if err != nil || got.UnconfiguredPlanChannels != 0 {
			t.Fatal(got, err)
		}
	}
	if err := s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:plan"}}, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.SummarizeUsage(context.Background(), UsageFilter{Model: "public"})
	if err != nil || got.UnconfiguredPlanChannels != 0 {
		t.Fatalf("reactivated config remains missing: summary=%+v err=%v", got, err)
	}
}
