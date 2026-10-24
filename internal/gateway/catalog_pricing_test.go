package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestRequestFreezesBothCatalogPriceTables(t *testing.T) {
	g, s := setup(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	}))
	defer up.Close()
	add(t, s, up.URL, "openai", 0)
	channels, _ := s.ListChannels()
	catalog := func(actual, reference float64) []store.CatalogItem {
		return []store.CatalogItem{{ID: "quoted", Source: "openrouter", Offerings: []store.CatalogOffering{{Key: "provider", Quotes: []store.CatalogQuote{{Key: "actual", Table: *priceRow(0, "", "CNY", store.PriceRates{Input: pricingNumber(actual)}).Selection.Table}, {Key: "reference", Table: *priceRow(0, "", "USD", store.PriceRates{Input: pricingNumber(reference)}).Selection.Table}}}}}}
	}
	if err := s.SaveExternalCatalog("openrouter", catalog(1, 2)); err != nil {
		t.Fatal(err)
	}
	row := func(ch int64, key string) store.Price {
		return store.Price{ChannelID: ch, Model: "test", Selection: store.PriceSelection{Mode: "catalog", Ref: &store.CatalogPriceRef{Source: "openrouter", ID: "quoted", OfferingKey: "provider", QuoteKey: key}}}
	}
	if err := s.SavePrices([]store.Price{row(channels[0].ID, "actual"), row(0, "reference")}); err != nil {
		t.Fatal(err)
	}
	go func() { done <- invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	if err := s.SaveExternalCatalog("openrouter", catalog(10, 20)); err != nil {
		t.Fatal(err)
	}
	close(release)
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete")
	}
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	u := rows[0]
	if u.ActualCost == nil || *u.ActualCost != 1 || u.OfficialValue == nil || *u.OfficialValue != 2 || u.Currency != "CNY" || u.OfficialCurrency != "USD" {
		t.Fatal("request price changed during catalog refresh", u)
	}
	if u.ActualPricing.Resolution.Ref.QuoteKey != "actual" || u.OfficialPricing.Resolution.Ref.QuoteKey != "reference" || *u.ActualPricing.Rates.Input != 1 || *u.OfficialPricing.Rates.Input != 2 {
		t.Fatal("frozen provenance lost", u)
	}
}
