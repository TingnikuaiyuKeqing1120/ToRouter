package pricing

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
	"torouter/internal/store"
)

type Result struct {
	Amount        *float64                `json:"amount"`
	Basis         store.UsagePricingBasis `json:"basis"`
	MissingReason string                  `json:"missing_reason,omitempty"`
}

func cloneTable(t *store.PriceTable) *store.PriceTable {
	if t == nil {
		return nil
	}
	raw, _ := json.Marshal(t)
	var out store.PriceTable
	_ = json.Unmarshal(raw, &out)
	return &out
}
func ResolveEntry(p store.Price, items []store.CatalogItem, layer string) store.PriceResolution {
	r := store.PriceResolution{Status: "unconfigured", Layer: layer, ChannelID: p.ChannelID, Model: p.Model}
	if !store.PriceConfigured(p) {
		r.MissingReason = "price_unconfigured"
		return r
	}
	r.Status = "resolved"
	if p.Selection.Mode == "manual" {
		r.Table = cloneTable(p.Selection.Table)
		return r
	}
	if p.Selection.Ref == nil {
		r.Status = "missing"
		r.MissingReason = "catalog_reference_missing"
		return r
	}
	ref := *p.Selection.Ref
	r.Ref = &ref
	for _, it := range items {
		if it.Source != ref.Source || it.ID != ref.ID {
			continue
		}
		if it.NeedsRefresh {
			r.Status = "missing"
			r.MissingReason = "catalog_needs_refresh"
			return r
		}
		if it.FetchedAt != nil {
			fetched := *it.FetchedAt
			r.FetchedAt = &fetched
		}
		for _, o := range it.Offerings {
			if o.Key != ref.OfferingKey {
				continue
			}
			for _, q := range o.Quotes {
				if q.Key == ref.QuoteKey {
					if q.FetchedAt != nil {
						fetched := *q.FetchedAt
						r.FetchedAt = &fetched
					}
					r.SourceKind = q.SourceKind
					if r.SourceKind == "" {
						r.SourceKind = o.SourceKind
					}
					r.SourceURL = q.SourceURL
					r.VerifiedAt = q.VerifiedAt
					r.Table = cloneTable(&q.Table)
					return r
				}
			}
		}
		r.Status = "missing"
		r.MissingReason = "catalog_quote_missing"
		return r
	}
	r.Status = "missing"
	r.MissingReason = "catalog_model_missing"
	return r
}
func ResolveUsagePrices(prices []store.Price, items []store.CatalogItem, channelID int64, upstreamModel, downstreamModel string) (actual, official store.PriceResolution) {
	find := func(ch int64, model string) store.Price {
		for _, p := range prices {
			if p.ChannelID == ch && p.Model == model {
				return p
			}
		}
		return store.Price{ChannelID: ch, Model: model}
	}
	actualEntry := store.Price{ChannelID: channelID, Model: upstreamModel}
	if channelID > 0 && upstreamModel != "" {
		actualEntry = find(channelID, upstreamModel)
	}
	actual = ResolveEntry(actualEntry, items, "channel")
	official = ResolveEntry(find(0, downstreamModel), items, "official")
	if official.SourceKind == "hosted" || official.SourceKind == "third_party" || official.SourceKind == "aggregator_summary" {
		official.Status = "missing"
		official.MissingReason = "original_reference_required"
	}
	return
}

// ResolveRequestPrices freezes the final model billing identity alongside the
// two independent prices. Historical backfill must not call this function:
// today's tags cannot establish how an earlier request was billed.
func ResolveRequestPrices(prices []store.Price, items []store.CatalogItem, channel store.Channel, tags []store.Tag, groups []store.TagGroup, upstreamModel, downstreamModel string) (actual, official store.PriceResolution) {
	actual, official = ResolveUsagePrices(prices, items, channel.ID, upstreamModel, downstreamModel)
	for _, tag := range store.ResolveModelTags(channel, upstreamModel, tags, groups)["billing"] {
		if tag == "billing:plan" {
			actual = store.PriceResolution{Status: "resolved", Layer: "plan", ChannelID: channel.ID, Model: upstreamModel}
			break
		}
	}
	return
}
func ruleMatches(w store.PriceRuleWhen, u store.Usage, input int64) (bool, error) {
	if r := w.InputRange; r != nil && (input < r.FromTokens || (r.ToTokens != nil && input > *r.ToTokens)) {
		return false, nil
	}
	if tw := w.TimeWindow; tw != nil {
		loc, e := time.LoadLocation(tw.Timezone)
		if e != nil {
			return false, e
		}
		when := u.Time.In(loc)
		if !tw.AllDay {
			a, e := store.PriceMinute(tw.Start)
			if e != nil {
				return false, e
			}
			b, e := store.PriceMinute(tw.End)
			if e != nil {
				return false, e
			}
			if !store.PriceWindowContains(a, b, when.Hour()*60+when.Minute()) {
				return false, nil
			}
		}
	}
	if len(w.Weekdays) > 0 || len(w.DateSet) > 0 {
		loc, e := time.LoadLocation(w.Timezone)
		if e != nil {
			return false, e
		}
		when := u.Time.In(loc)
		if len(w.Weekdays) > 0 {
			day := int(when.Weekday())
			if day == 0 {
				day = 7
			}
			found := false
			for _, d := range w.Weekdays {
				found = found || d == day
			}
			if !found {
				return false, nil
			}
		}
		if len(w.DateSet) > 0 {
			found := false
			for _, d := range w.DateSet {
				found = found || d == when.Format("2006-01-02")
			}
			if !found {
				return false, nil
			}
		}
	}
	return true, nil
}
func Compute(res store.PriceResolution, u store.Usage, method, note string) Result {
	b := store.UsagePricingBasis{Method: method, CalculatedAt: time.Now().UTC(), Resolution: res, Note: note, MatchedRules: []string{}}
	fail := func(reason string) Result { b.MissingReason = reason; return Result{Basis: b, MissingReason: reason} }
	if res.Layer == "plan" {
		return fail("plan_unallocated")
	}
	if res.Status != "resolved" || res.Table == nil {
		reason := res.MissingReason
		if reason == "" {
			reason = "price_unconfigured"
		}
		return fail(reason)
	}
	t := res.Table
	b.Currency = t.Currency
	b.UnitTokens = t.UnitTokens
	if u.Estimated {
		return fail("usage_unknown")
	}
	quantities := [4]int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens}
	input := u.InputTokens
	for _, q := range quantities {
		if q < 0 {
			return fail("usage_unknown")
		}
	}
	for _, q := range []int64{u.CacheReadTokens, u.CacheWriteTokens} {
		if q > math.MaxInt64-input {
			return fail("usage_overflow")
		}
		input += q
	}
	if e := store.ValidatePriceTable(*t); e != nil {
		return fail("price_table_invalid")
	}
	rates := map[string]*float64{"input": t.Rates.Input, "output": t.Rates.Output, "cache_read": t.Rates.CacheRead, "cache_write": t.Rates.CacheWrite}
	for i, r := range t.Rules {
		if !r.Enabled {
			continue
		}
		matches, e := ruleMatches(r.When, u, input)
		if e != nil {
			return fail("price_rule_invalid")
		}
		if !matches {
			continue
		}
		id := r.ID
		if id == "" {
			id = fmt.Sprintf("rule-%d", i+1)
		}
		b.MatchedRules = append(b.MatchedRules, id)
		if r.Action.Multiplier != nil {
			for k, v := range rates {
				if v != nil {
					n := *v * *r.Action.Multiplier
					rates[k] = &n
				}
			}
		} else {
			for k, v := range r.Action.RateChanges {
				rates[k] = v
			}
		}
	}
	b.Rates = store.PriceRates{Input: rates["input"], Output: rates["output"], CacheRead: rates["cache_read"], CacheWrite: rates["cache_write"]}
	sum := 0.0
	for i, k := range []string{"input", "output", "cache_read", "cache_write"} {
		q := quantities[i]
		if q == 0 {
			continue
		}
		v := rates[k]
		if v == nil {
			return fail("rate_missing:" + k)
		}
		sum += float64(q) / float64(t.UnitTokens) * *v
	}
	if math.IsNaN(sum) || math.IsInf(sum, 0) || sum < 0 {
		return fail("amount_overflow")
	}
	return Result{Amount: &sum, Basis: b}
}
func Calculate(t store.PriceTable, u store.Usage) *float64 {
	if t.UnitTokens == 0 {
		t.UnitTokens = 1000000
	}
	if t.Currency == "" {
		t.Currency = "CNY"
	}
	return Compute(store.PriceResolution{Status: "resolved", Table: &t}, u, "request", "").Amount
}
