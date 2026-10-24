package pricing

import (
	"testing"
	"torouter/internal/store"
)

func TestHostedQuoteCannotBeOriginalEquivalentReference(t *testing.T) {
	rate := 1.0
	table := store.PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: store.PriceRates{Input: &rate}, Rules: []store.PriceRule{}}
	item := store.CatalogItem{Source: "openrouter", ID: "vendor/model", Offerings: []store.CatalogOffering{{Key: "provider", SourceKind: "hosted", Quotes: []store.CatalogQuote{{Key: "usd", SourceKind: "hosted", Table: table}}}}}
	entry := store.Price{Model: "alias", Selection: store.PriceSelection{Mode: "catalog", Ref: &store.CatalogPriceRef{Source: item.Source, ID: item.ID, OfferingKey: "provider", QuoteKey: "usd"}}}
	actual, official := ResolveUsagePrices([]store.Price{entry}, []store.CatalogItem{item}, 9, "upper", "alias")
	if actual.Status != "unconfigured" || official.Status != "missing" || official.MissingReason != "original_reference_required" {
		t.Fatalf("hosted was silently original or actual fell back: actual=%+v official=%+v", actual, official)
	}
	item.Offerings[0].Quotes[0].SourceKind = "original"
	_, official = ResolveUsagePrices([]store.Price{entry}, []store.CatalogItem{item}, 9, "upper", "alias")
	if official.Status != "resolved" {
		t.Fatal("verified original quote was refused", official)
	}
}
