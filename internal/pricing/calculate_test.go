package pricing

import (
	"encoding/json"
	"math"
	"testing"
	"time"
	"torouter/internal/store"
)

func number(v float64) *float64 { return &v }
func bound(v int64) *int64      { return &v }
func TestCalculateContextTimeUnitAndUnknownBoundaries(t *testing.T) {
	base := legacyTestPrice{UnitTokens: 100, Input: number(1), Output: number(2), CacheRead: number(3), CacheWrite: number(4)}
	usage := store.Usage{Time: time.Date(2026, 10, 4, 15, 30, 0, 0, time.UTC), InputTokens: 150, OutputTokens: 250, CacheReadTokens: 50}
	ranges := []legacyTestRange{{FromTokens: 0, ToTokens: bound(200), Mode: "multiplier", Multiplier: number(2)}, {FromTokens: 201, Mode: "multiplier", Multiplier: number(3)}}
	cases := []struct {
		name string
		edit func(*legacyTestPrice, *store.Usage)
		want *float64
	}{
		{"simple_unit", func(p *legacyTestPrice, u *store.Usage) {}, number(8)},
		{"range_upper_inclusive_including_cache", func(p *legacyTestPrice, u *store.Usage) { p.ContextEnabled = true; p.ContextRanges = ranges }, number(16)},
		{"range_upper_plus_one_including_cache_write", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextEnabled = true
			p.ContextRanges = ranges
			u.CacheWriteTokens = 1
		}, number(24.12)},
		{"range_zero_lower_and_upper", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextEnabled = true
			p.ContextRanges = []legacyTestRange{{FromTokens: 0, ToTokens: bound(0), Mode: "multiplier", Multiplier: number(2)}}
			u.InputTokens = 0
			u.CacheReadTokens = 0
		}, number(10)},
		{"range_lower_inclusive", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextEnabled = true
			p.ContextRanges = []legacyTestRange{{FromTokens: 200, ToTokens: bound(300), Mode: "multiplier", Multiplier: number(2)}}
		}, number(16)},
		{"unmatched_range_uses_base", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextEnabled = true
			p.ContextRanges = []legacyTestRange{{FromTokens: 201, ToTokens: bound(300), Mode: "multiplier", Multiplier: number(2)}}
		}, number(8)},
		{"gap_uses_base", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextEnabled = true
			p.ContextRanges = []legacyTestRange{{FromTokens: 0, ToTokens: bound(199), Mode: "multiplier", Multiplier: number(2)}, {FromTokens: 201, Mode: "multiplier", Multiplier: number(3)}}
		}, number(8)},
		{"range_fixed_applies_to_all_token_kinds", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextEnabled = true
			p.ContextRanges = []legacyTestRange{{FromTokens: 200, Mode: "fixed", Rates: &store.PriceRates{Input: number(2), Output: number(4), CacheRead: number(6), CacheWrite: number(8)}}}
		}, number(16)},
		{"context_disabled_retains_ignored_rules", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextRanges = []legacyTestRange{{FromTokens: 0, Mode: "fixed", Rates: &store.PriceRates{}}}
		}, number(8)},
		{"time_disabled_ignores_invalid_draft", func(p *legacyTestPrice, u *store.Usage) {
			p.TimeWindows = []legacyTestWindow{{Start: "bad", End: "bad"}}
		}, number(8)},
		{"context_on_time_off", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextEnabled = true
			p.ContextRanges = ranges
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: number(0.5)}}
		}, number(16)},
		{"time_on_context_off", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextRanges = ranges
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: number(0.5)}}
		}, number(4)},
		{"unmatched_context_uses_time_fixed_base", func(p *legacyTestPrice, u *store.Usage) {
			p.ContextEnabled = true
			p.ContextRanges = []legacyTestRange{{FromTokens: 201, Mode: "multiplier", Multiplier: number(3)}}
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "fixed", Rates: &store.PriceRates{Input: number(2), Output: number(4), CacheRead: number(6)}}}
		}, number(16)},
		{"cross_midnight_multiplier", func(p *legacyTestPrice, u *store.Usage) {
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: number(0.5)}}
		}, number(4)},
		{"window_start_inclusive", func(p *legacyTestPrice, u *store.Usage) {
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: number(0.5)}}
			u.Time = time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
		}, number(4)},
		{"window_end_exclusive", func(p *legacyTestPrice, u *store.Usage) {
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: number(0.5)}}
			u.Time = time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC)
		}, number(8)},
		{"time_fixed_then_context_multiplier", func(p *legacyTestPrice, u *store.Usage) {
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "fixed", Rates: &store.PriceRates{Input: number(4)}}}
			p.ContextEnabled = true
			p.ContextRanges = []legacyTestRange{{Mode: "multiplier", Multiplier: number(2)}}
			u.OutputTokens = 0
			u.CacheReadTokens = 0
		}, number(12)},
		{"context_fixed_then_time_multiplier", func(p *legacyTestPrice, u *store.Usage) {
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: number(0.5)}}
			p.ContextEnabled = true
			p.ContextRanges = []legacyTestRange{{Mode: "fixed", Rates: &store.PriceRates{Input: number(4)}}}
			u.OutputTokens = 0
			u.CacheReadTokens = 0
		}, number(3)},
		{"fixed_does_not_inherit_null_rates", func(p *legacyTestPrice, u *store.Usage) {
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "fixed", Rates: &store.PriceRates{Input: number(4)}}}
		}, nil},
		{"positive_quantity_null_rate", func(p *legacyTestPrice, u *store.Usage) { p.Output = nil }, nil},
		{"zero_quantity_null_rate", func(p *legacyTestPrice, u *store.Usage) { p.Output = nil; u.OutputTokens = 0 }, number(3)},
		{"known_free", func(p *legacyTestPrice, u *store.Usage) {
			p.Input = number(0)
			p.Output = number(0)
			p.CacheRead = number(0)
		}, number(0)},
		{"unknown_even_with_zero_multiplier", func(p *legacyTestPrice, u *store.Usage) {
			p.Input = nil
			p.TimeEnabled = true
			p.TimeWindows = []legacyTestWindow{{Start: "23:00", End: "02:00", Mode: "multiplier", Multiplier: number(0)}}
		}, nil},
		{"missing_usage", func(p *legacyTestPrice, u *store.Usage) { u.Estimated = true }, nil},
		{"known_zero_usage", func(p *legacyTestPrice, u *store.Usage) { *u = store.Usage{}; *p = legacyTestPrice{} }, number(0)},
		{"default_million_unit", func(p *legacyTestPrice, u *store.Usage) { p.UnitTokens = 0 }, number(0.0008)},
		{"hundred_million_unit", func(p *legacyTestPrice, u *store.Usage) { p.UnitTokens = 100000000 }, number(0.000008)},
		{"overflow_returns_unknown", func(p *legacyTestPrice, u *store.Usage) { p.Input = number(math.MaxFloat64) }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, u := base, usage
			p.Model = "fixture"
			tc.edit(&p, &u)
			p.Model = "fixture"
			raw, _ := json.Marshal([]legacyTestPrice{p})
			prices, err := store.DecodePricesImport(raw)
			if err != nil {
				t.Fatal(err)
			}
			got := Calculate(*prices[0].Selection.Table, u)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want unknown", *got)
				}
				return
			}
			if got == nil || math.Abs(*got-*tc.want) > 1e-9 {
				t.Fatalf("got %v want %v", got, *tc.want)
			}
		})
	}
}

type legacyTestRange struct {
	FromTokens int64             `json:"from_tokens"`
	ToTokens   *int64            `json:"to_tokens"`
	Mode       string            `json:"mode"`
	Multiplier *float64          `json:"multiplier"`
	Rates      *store.PriceRates `json:"rates"`
}
type legacyTestWindow struct {
	Start      string            `json:"start"`
	End        string            `json:"end"`
	Mode       string            `json:"mode"`
	Multiplier *float64          `json:"multiplier"`
	Rates      *store.PriceRates `json:"rates"`
}
type legacyTestPrice struct {
	Model          string             `json:"model"`
	Currency       string             `json:"currency"`
	UnitTokens     int64              `json:"unit_tokens"`
	Input          *float64           `json:"input"`
	Output         *float64           `json:"output"`
	CacheRead      *float64           `json:"cache_read"`
	CacheWrite     *float64           `json:"cache_write"`
	ContextEnabled bool               `json:"context_enabled"`
	ContextRanges  []legacyTestRange  `json:"context_ranges"`
	TimeEnabled    bool               `json:"time_enabled"`
	TimeWindows    []legacyTestWindow `json:"time_windows"`
}
