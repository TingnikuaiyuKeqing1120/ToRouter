package console

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestRetiredPriceRoleWritesAreRejected(t *testing.T) {
	s, _ := newTestCatalogConsole(t)
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", "/prices", `[{"model":"public","role":"default","selection":{"mode":"manual"}}]`},
		{"PUT", "/prices", `[{"model":"public","role":"reference","selection":{"mode":"manual"}}]`},
		{"PUT", "/prices", `[{"model":"public","selection":{"mode":"manual"}},{"model":"public","selection":{"mode":"manual"}}]`},
		{"POST", "/catalog/prefill/preview", `{"targets":[{"kind":"downstream","model":"public","price_role":"reference"}],"groups":["price"]}`},
		{"POST", "/catalog/prefill/apply", `{"targets":[{"kind":"downstream","model":"public","price_role":"default"}],"groups":["price"]}`},
	} {
		code, body, _ := call(t, h, tc.method, tc.path, tc.body)
		if code != 400 {
			t.Fatal("ambiguous retired price identity accepted", tc.path, code, body)
		}
	}
	prices, err := s.GetPrices()
	if err != nil || len(prices) != 0 {
		t.Fatal("rejected write changed prices", prices, err)
	}
}

func TestPricingSplitAPIAndNativeAggregation(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	v, _ := s.GetSettings()
	v.Prices = nil
	v.RetentionDays = 93
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PUT", "/prices", strings.NewReader(`[{"model":"real","channel_id":1,"origin":"manual","selection":{"mode":"manual","table":{"currency":"CNY","unit_tokens":1000000,"rates":{"input":2,"output":null,"cache_read":null,"cache_write":null},"rules":[]}}}]`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"currency":"CNY"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	raw, _ := json.Marshal(v)
	var body map[string]any
	json.Unmarshal(raw, &body)
	delete(body, "prices")
	raw, _ = json.Marshal(body)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PUT", "/settings", strings.NewReader(string(raw))))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	got, _ := s.GetSettings()
	if got.RetentionDays != 93 || len(got.Prices) != 1 || got.Prices[0].Selection.Table.Currency != "CNY" {
		t.Fatal(got)
	}
	amount := 2.0
	for _, currency := range []string{"USD", "CNY"} {
		if err := s.AddUsage(store.Usage{Time: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), ChannelName: "channel", Model: "alias", Currency: currency, ActualCost: &amount}); err != nil {
			t.Fatal(err)
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage", nil))
	var summary struct {
		store.UsagePricingTotals
		Daily    []store.UsageDay   `json:"daily"`
		Channels []store.UsageGroup `json:"by_channel"`
		Models   []store.UsageGroup `json:"by_model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.ActualCosts["USD"] != 2 || summary.ActualCosts["CNY"] != 2 || summary.OfficialPricedRequests != 0 || summary.ActualPricedRequests != 2 || summary.Daily[0].ActualCosts["CNY"] != 2 || summary.Channels[0].ActualCosts["CNY"] != 2 || summary.Models[0].ActualCosts["CNY"] != 2 {
		t.Fatalf("incorrect totals %s", w.Body.String())
	}
	var fields map[string]any
	json.Unmarshal(w.Body.Bytes(), &fields)
	for _, key := range []string{"cost", "costs", "legacy_costs", "legacy_cost_requests"} {
		if _, exists := fields[key]; exists {
			t.Fatalf("removed field %s exposed", key)
		}
	}
}

func TestIndependentPricingSummaryDoesNotClassifyLegacyOrUnknownAsActual(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ptr := func(v float64) *float64 { return &v }
	rows := []store.Usage{
		{}, // historical, unclassified
		{},
		{}, // known new schema, both prices missing
		{OfficialValue: ptr(0), OfficialCurrency: "USD"},
		{ActualCost: ptr(0), Currency: "CNY"},
		{OfficialValue: ptr(3), OfficialCurrency: "USD", ActualCost: ptr(5), Currency: "CNY"},
		{OfficialValue: ptr(7), OfficialCurrency: "CNY", ActualCost: ptr(2), Currency: "USD"},
	}
	for _, u := range rows {
		u.Time = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
		u.ChannelName = "one"
		u.Model = "alias"
		if err := st.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	h := New(st, "http://127.0.0.1:17878", Hooks{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage", nil))
	var got struct {
		store.UsagePricingTotals
		Daily    []store.UsageDay   `json:"daily"`
		Channels []store.UsageGroup `json:"by_channel"`
		Models   []store.UsageGroup `json:"by_model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, v := range []store.UsagePricingTotals{got.UsagePricingTotals, got.Daily[0].UsagePricingTotals, got.Channels[0].UsagePricingTotals, got.Models[0].UsagePricingTotals} {
		if v.OfficialValues["USD"] != 3 || v.OfficialValues["CNY"] != 7 || v.ActualCosts["USD"] != 2 || v.ActualCosts["CNY"] != 5 || v.OfficialPricedRequests != 3 || v.ActualPricedRequests != 3 {
			t.Fatalf("incorrect classified summary: %+v; raw %s", v, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/logs", nil))
	var logs []store.Usage
	if err := json.Unmarshal(w.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	if logs[len(logs)-1].ActualCost != nil || logs[len(logs)-1].OfficialValue != nil {
		t.Fatal("old amount was reclassified", logs)
	}
}

func TestModelDisplayNameRoundTripsWithoutChangingRoutingID(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := store.Channel{Name: "test", APIKey: "test-key", Models: []string{"real"}, Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "http://localhost:9000"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	m := store.DownstreamModel{ID: "alias", DisplayName: "Friendly model", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}
	raw, _ := json.Marshal(m)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/models", strings.NewReader(string(raw))))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/models", nil))
	var models []store.DownstreamModel
	if err := json.Unmarshal(w.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "alias" || models[0].DisplayName != "Friendly model" || models[0].Bindings[0].Model != "real" {
		t.Fatal(models)
	}
}
