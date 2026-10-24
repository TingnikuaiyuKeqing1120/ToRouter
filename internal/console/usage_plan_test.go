package console

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestUsagePlanHTTPContractAndRequestFilters(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := store.Channel{Name: "plan", APIKey: "test-key", Tags: []string{"billing:plan"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "http://localhost"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	amount := 30.0
	if err := s.SavePlanPrice(store.PlanPriceConfig{ChannelID: c.ID, DefaultAmount: &amount, Currency: "CNY", FirstPaymentAt: now.Add(-time.Hour), Timezone: "UTC"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(store.Usage{ChannelID: c.ID, ChannelName: c.Name, Model: "public", Status: 200, InputTokens: 12, ActualPricing: &store.UsagePricingBasis{Resolution: store.PriceResolution{Layer: "plan"}, MissingReason: "plan_unallocated"}}); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	for _, tc := range []struct {
		query   string
		charges int
		cost    float64
	}{
		{"", 1, 30},
		{fmt.Sprintf("?channel_id=%d", c.ID), 1, 30},
		{"?model=public", 0, 0},
		{"?status=200", 0, 0},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/usage"+tc.query, nil))
		var got store.UsageSummary
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
			t.Fatal(w.Code, w.Body.String(), err)
		}
		if got.PlanRequests != 1 || got.PlanCharges != tc.charges || got.PlanCosts["CNY"] != tc.cost || got.ActualCosts["CNY"] != tc.cost || got.ActualComponents["CNY"].Plan != tc.cost || got.ActualPricedRequests != 0 || got.Requests != 1 || got.InputTokens != 12 {
			t.Fatal(got)
		}
		if len(got.ByModel) != 1 || got.ByModel[0].PlanCharges != 0 || got.ByModel[0].PlanRequests != 1 {
			t.Fatal(got.ByModel)
		}
	}
}
