package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestUsageSummaryEmptyCancellationAndReadFailure(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.SummarizeUsage(context.Background(), UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 0 || got.Daily == nil || got.ByChannel == nil || got.ByModel == nil || len(got.OfficialValues) != 2 || len(got.ActualCosts) != 2 {
		t.Fatalf("empty summary must retain its response shape: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SummarizeUsage(ctx, UsageFilter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
	if err := s.AddUsage(Usage{Time: time.Now(), Model: "first", InputTokens: 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,0,'broken',200,?)", time.Now().UnixMilli(), "{invalid-json}"); err != nil {
		t.Fatal(err)
	}
	got, err = s.SummarizeUsage(context.Background(), UsageFilter{})
	if err == nil || got.Requests != 0 {
		t.Fatalf("read failure returned partial statistics: summary=%+v err=%v", got, err)
	}
	// A failed/cancelled query must release the store's single connection.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		t.Fatalf("summary leaked the connection: %v", err)
	}
}

func TestUsageSummaryDetailedMetricsAndSnapshots(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ptr := func(v float64) *float64 { return &v }
	at := time.Date(2026, 10, 1, 15, 59, 59, 0, time.UTC)
	basis := func(currency string, unit int64) *UsagePricingBasis {
		return &UsagePricingBasis{Currency: currency, UnitTokens: unit, Rates: PriceRates{Input: ptr(1), Output: ptr(2), CacheRead: ptr(3), CacheWrite: ptr(4)}}
	}
	rows := []Usage{
		{Time: at, ChannelID: 1, ChannelName: "shared", Model: "alpha", Status: 503, InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 2000, CacheWriteTokens: 1000, ActualCost: ptr(12), Currency: "USD", ActualPricing: basis("USD", 1000), OfficialValue: ptr(24), OfficialCurrency: "CNY", OfficialPricing: basis("CNY", 500)},
		{Time: at.Add(time.Second), ChannelID: 2, ChannelName: "latest tied", Model: "alpha", Status: 200, ActualCost: ptr(0), Currency: "CNY", ActualPricing: basis("CNY", 1000)},
		{Time: at.Add(2 * time.Second), ChannelID: 1, ChannelName: "renamed", Model: "beta", Status: 200, InputTokens: 16000, Estimated: true, OfficialValue: ptr(7), OfficialCurrency: "USD"},
		{Time: at.Add(-time.Hour), ChannelID: 1, ChannelName: "old inserted later", Model: "beta", Status: 200, InputTokens: 128000, ActualCost: ptr(3), Currency: "USD", ActualPricing: &UsagePricingBasis{Currency: "USD", UnitTokens: 1000}},
		{Time: at.Add(2 * time.Second), ChannelID: 1, ChannelName: "latest tied", Model: "beta", Status: 200, InputTokens: 32000, ActualCost: ptr(5), Currency: "USD", ActualPricing: basis("USD", 1000)},
		{Time: at, ChannelID: 9, ChannelName: "excluded", NotSent: true, InputTokens: 999999, ActualCost: ptr(999), Currency: "USD"},
	}
	for _, u := range rows {
		if err := s.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SummarizeUsage(context.Background(), UsageFilter{Timezone: "Asia/Shanghai", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 5 || got.Failures != 1 || got.InputTokens != 180000 || got.PlainInputTokens != 177000 || got.OutputTokens != 500 || got.CacheReadTokens != 2000 || got.CacheWriteTokens != 1000 || got.KnownUsageRequests != 4 || got.UnknownUsageRequests != 1 {
		t.Fatalf("incorrect metrics: %+v", got.UsageMetrics)
	}
	if got.Timezone != "Asia/Shanghai" || len(got.Daily) != 2 || got.Daily[0].Date != "2026-10-01" || got.Daily[0].Requests != 2 || got.Daily[1].Date != "2026-10-02" || got.Daily[1].Requests != 3 {
		t.Fatalf("local day buckets: %+v", got.Daily)
	}
	if len(got.ByChannel) != 2 || len(got.ByModel) != 2 {
		t.Fatalf("groups: %+v %+v", got.ByChannel, got.ByModel)
	}
	if got.ByChannel[0].Name != got.ByChannel[1].Name || *got.ByChannel[0].ChannelID == *got.ByChannel[1].ChannelID {
		t.Fatalf("same-name channels must retain their IDs: %+v", got.ByChannel)
	}
	var channel UsageGroup
	for _, v := range got.ByChannel {
		if v.ChannelID != nil && *v.ChannelID == 1 {
			channel = v
		}
	}
	if channel.Name != "latest tied" || channel.Requests != 4 || len(channel.Models) != 2 {
		t.Fatalf("ID grouping/latest observed name: %+v", channel)
	}
	actual, official := got.ActualComponents["USD"], got.OfficialComponents["CNY"]
	if actual.Input != 1 || actual.Output != 1 || actual.CacheRead != 6 || actual.CacheWrite != 4 || actual.Unattributed != 8 || official.Input != 2 || official.Output != 2 || official.CacheRead != 12 || official.CacheWrite != 8 || official.Unattributed != 0 || got.OfficialComponents["USD"].Unattributed != 7 {
		t.Fatalf("snapshot components: actual=%+v official=%+v", got.ActualComponents, got.OfficialComponents)
	}
	if got.ActualCosts["USD"] != 20 || got.ActualCosts["CNY"] != 0 || got.ActualPricedRequests != 4 || got.OfficialPricedRequests != 2 {
		t.Fatalf("native totals/counts: %+v", got.UsagePricingTotals)
	}
	for i, want := range []int{1, 1, 0, 1, 0, 1} {
		if got.InputDistribution[i].Requests != want {
			t.Fatalf("unknown usage included or boundary incorrect: %+v", got.InputDistribution)
		}
	}
	if got.InputDistribution[5].ToTokens != nil {
		t.Fatal("unbounded bucket must be null")
	}
	assertUsageMetricsSum(t, got.UsageMetrics, dayMetrics(got.Daily))
	for _, tc := range []struct {
		groups []UsageGroup
		want   UsageMetrics
	}{{got.ByChannel, got.UsageMetrics}, {got.ByModel, got.UsageMetrics}, {channel.Models, channel.UsageMetrics}} {
		metrics := []UsageMetrics{}
		for _, v := range tc.groups {
			metrics = append(metrics, v.UsageMetrics)
			assertUsageMetricsSum(t, v.UsageMetrics, dayMetrics(v.Daily))
		}
		assertUsageMetricsSum(t, tc.want, metrics)
	}
}

func dayMetrics(days []UsageDay) []UsageMetrics {
	out := []UsageMetrics{}
	for _, v := range days {
		out = append(out, v.UsageMetrics)
	}
	return out
}
func assertUsageMetricsSum(t *testing.T, want UsageMetrics, parts []UsageMetrics) {
	t.Helper()
	got := newUsageMetrics()
	for _, v := range parts {
		got.Requests += v.Requests
		got.Failures += v.Failures
		got.InputTokens += v.InputTokens
		got.OutputTokens += v.OutputTokens
		got.PlainInputTokens += v.PlainInputTokens
		got.CacheReadTokens += v.CacheReadTokens
		got.CacheWriteTokens += v.CacheWriteTokens
		got.KnownUsageRequests += v.KnownUsageRequests
		got.UnknownUsageRequests += v.UnknownUsageRequests
		got.OfficialPricedRequests += v.OfficialPricedRequests
		got.ActualPricedRequests += v.ActualPricedRequests
		got.PlanRequests += v.PlanRequests
		got.PlanCharges += v.PlanCharges
		got.UnknownPlanCharges += v.UnknownPlanCharges
		for k, n := range v.PlanCosts {
			got.PlanCosts[k] += n
		}
		for k, n := range v.OfficialValues {
			got.OfficialValues[k] += n
		}
		for k, n := range v.ActualCosts {
			got.ActualCosts[k] += n
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("group/day sums differ:\ngot=%+v\nwant=%+v", got, want)
	}
}

func TestUsageComponentsRequireOwnCompleteConsistentBasis(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name       string
		u          Usage
		amount     float64
		basis      *UsagePricingBasis
		attributed bool
	}{
		{"no snapshot", Usage{InputTokens: 1}, 2, nil, false},
		{"missing required rate", Usage{InputTokens: 1}, 2, &UsagePricingBasis{Currency: "USD", UnitTokens: 1}, false},
		{"unused missing rates", Usage{InputTokens: 1}, 2, &UsagePricingBasis{Currency: "USD", UnitTokens: 1, Rates: PriceRates{Input: ptr(2)}}, true},
		{"zero unit", Usage{InputTokens: 1}, 2, &UsagePricingBasis{Currency: "USD", Rates: PriceRates{Input: ptr(2)}}, false},
		{"currency mismatch", Usage{InputTokens: 1}, 2, &UsagePricingBasis{Currency: "CNY", UnitTokens: 1, Rates: PriceRates{Input: ptr(2)}}, false},
		{"stored amount differs", Usage{InputTokens: 1}, 3, &UsagePricingBasis{Currency: "USD", UnitTokens: 1, Rates: PriceRates{Input: ptr(2)}}, false},
		{"small mismatch is not roundoff", Usage{InputTokens: 1}, 2.0000000001, &UsagePricingBasis{Currency: "USD", UnitTokens: 1, Rates: PriceRates{Input: ptr(2)}}, false},
		{"unknown usage", Usage{InputTokens: 1, Estimated: true}, 2, &UsagePricingBasis{Currency: "USD", UnitTokens: 1, Rates: PriceRates{Input: ptr(2)}}, false},
		{"missing reason", Usage{InputTokens: 1}, 2, &UsagePricingBasis{Currency: "USD", UnitTokens: 1, Rates: PriceRates{Input: ptr(2)}, MissingReason: "usage_unknown"}, false},
		{"zero is not tiny charge", Usage{InputTokens: 1}, 0, &UsagePricingBasis{Currency: "USD", UnitTokens: 1, Rates: PriceRates{Input: ptr(1e-20)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			totals := map[string]UsageCostComponents{}
			addUsageComponents(totals, &tc.amount, "USD", tc.basis, tc.u)
			v := totals["USD"]
			if tc.attributed {
				if v.Input != tc.amount || v.Unattributed != 0 {
					t.Fatal(v)
				}
			} else if v.Input != 0 || v.Unattributed != tc.amount {
				t.Fatal(v)
			}
		})
	}
}

func TestUsageSummaryTimezoneValidationAndDST(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"Mars/Olympus", "Local", "../UTC"} {
		if _, err := s.SummarizeUsage(context.Background(), UsageFilter{Timezone: name}); err == nil {
			t.Fatalf("accepted timezone %q", name)
		}
	}
	for _, stamp := range []string{"2026-03-08T04:59:59Z", "2026-03-08T05:00:00Z", "2026-03-09T03:59:59Z", "2026-03-09T04:00:00Z"} {
		at, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddUsage(Usage{Time: at}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SummarizeUsage(context.Background(), UsageFilter{Timezone: "America/New_York"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Daily) != 3 || got.Daily[0].Date != "2026-03-07" || got.Daily[1].Date != "2026-03-08" || got.Daily[1].Requests != 2 || got.Daily[2].Date != "2026-03-09" {
		t.Fatalf("DST calendar: %+v", got.Daily)
	}
}
