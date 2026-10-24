package pricing

import (
	"math"
	"testing"
	"time"
	"torouter/internal/store"
)

// Exercise the embedded quotes through the same binding and calculator used by
// requests, including exclusive cache quantities and conditional whole prices.
func TestBuiltinQuotesCalculateCommonRequests(t *testing.T) {
	peak := time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, model, offering, quote string
		usage                        store.Usage
		want                         float64
	}{
		{"kimi default cache", "kimi-k3", "moonshot/international/payg", "token", store.Usage{InputTokens: 800000, OutputTokens: 200000, CacheReadTokens: 100000, CacheWriteTokens: 100000}, 5.73},
		{"kimi one hour cache", "kimi-k3", "moonshot/international/payg", "cache-1h", store.Usage{InputTokens: 800000, OutputTokens: 200000, CacheReadTokens: 100000, CacheWriteTokens: 100000}, 6.03},
		{"kimi code", "kimi-k2.7-code", "moonshot/international/payg", "token", store.Usage{InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 50000}, .1445},
		{"mistral standard original price", "mistral-large-4", "mistral/global/standard", "token", store.Usage{InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 1000}, .17794},
		{"terra cached prompt reaches long context", "gpt-5.6-terra", "", "standard-usd", store.Usage{InputTokens: 200000, OutputTokens: 10000, CacheReadTokens: 80000}, 1.012},
		{"mimo free cache write", "mimo-v2.6-pro", "xiaomi/china/realtime", "token", store.Usage{InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 1000, CacheWriteTokens: 5000}, .360025},
		{"deepseek weekday peak", "deepseek-flash", "deepseek/global/payg", "token", store.Usage{Time: peak, InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 1000}, .042006},
		{"deepseek off peak", "deepseek-flash", "deepseek/global/payg", "token", store.Usage{Time: peak.Add(10 * time.Hour), InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 1000}, .021003},
		{"minimax long context", "MiniMax-M3", "minimax/international/standard", "token", store.Usage{InputTokens: 520000, OutputTokens: 10000, CacheReadTokens: 1000}, .33612},
		{"gemini current standard despite future notice", "gemini-3.8-flash", "google/developer-api/paid-standard", "token", store.Usage{InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 1000}, .112575},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := store.CatalogPriceRef{Source: "builtin", ID: tc.model, OfferingKey: tc.offering, QuoteKey: tc.quote}
			if ref.OfferingKey == "" {
				for _, item := range store.BuiltinCatalog {
					if item.ID == tc.model {
						ref.OfferingKey = item.Offerings[0].Key
					}
				}
			}
			price := store.Price{Model: tc.model, Selection: store.PriceSelection{Mode: "catalog", Ref: &ref}}
			resolution := ResolveEntry(price, store.BuiltinCatalog, "upstream")
			got := Compute(resolution, tc.usage, "request", "")
			if got.Amount == nil || math.Abs(*got.Amount-tc.want) > 1e-10 {
				t.Fatalf("want %.8f, got %+v", tc.want, got)
			}
		})
	}
}

func TestBuiltinRegionalQuotesKeepCurrencyAndConditions(t *testing.T) {
	for _, tc := range []struct {
		model, offering, quote, currency string
		usage                            store.Usage
		want                             float64
	}{
		{"kimi-k3", "moonshot/china/payg", "cache-1h", "CNY", store.Usage{InputTokens: 800000, OutputTokens: 200000, CacheReadTokens: 100000, CacheWriteTokens: 100000}, 40.2},
		{"glm-5.3", "zhipu/china/payg", "token", "CNY", store.Usage{InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 1000}, 1.082},
		{"step-5-preview", "stepfun/china/standard", "standard-cny", "CNY", store.Usage{InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 1000}, .90035},
		{"deepseek-flash", "deepseek/global/payg", "cny-token", "CNY", store.Usage{Time: time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC), InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 1000}, .28004},
		{"MiniMax-M3", "minimax/china/standard", "token", "CNY", store.Usage{InputTokens: 520000, OutputTokens: 10000, CacheReadTokens: 1000}, 2.35284},
		{"qwen3.7-plus", "qwen/beijing/payg", "cny-explicit-cache-token", "CNY", store.Usage{InputTokens: 200000, OutputTokens: 10000, CacheReadTokens: 60000, CacheWriteTokens: 10000}, 1.551},
		{"qwen3.7-plus", "qwen/beijing/payg", "cny-implicit-cache-token", "CNY", store.Usage{InputTokens: 210000, OutputTokens: 10000, CacheReadTokens: 60000}, 1.572},
	} {
		t.Run(tc.model+"/"+tc.offering, func(t *testing.T) {
			ref := store.CatalogPriceRef{Source: "builtin", ID: tc.model, OfferingKey: tc.offering, QuoteKey: tc.quote}
			price := store.Price{Model: tc.model, Selection: store.PriceSelection{Mode: "catalog", Ref: &ref}}
			resolution := ResolveEntry(price, store.BuiltinCatalog, "upstream")
			if resolution.Table == nil || resolution.Table.Currency != tc.currency {
				t.Fatalf("wrong regional quote: %+v", resolution)
			}
			got := Compute(resolution, tc.usage, "request", "")
			if got.Amount == nil || math.Abs(*got.Amount-tc.want) > 1e-10 {
				t.Fatalf("want %.8f %s, got %+v", tc.want, tc.currency, got)
			}
		})
	}
}
