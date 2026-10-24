package store

import (
	"fmt"
	"math"
	"strings"
	"time"
	_ "time/tzdata"
)

const DefaultQuotaRefreshSeconds = 300

func normalizeQuotaRefresh(v *Settings) error {
	if v.QuotaRefreshSeconds == nil {
		n := DefaultQuotaRefreshSeconds
		v.QuotaRefreshSeconds = &n
	}
	n := *v.QuotaRefreshSeconds
	if n != 0 && (n < 30 || n > 86400) {
		return NewConfigurationError("quota_refresh_seconds must be 0 or 30..86400")
	}
	return nil
}
func defaultPrices(prices []Price) []Price {
	out := append([]Price{}, prices...)
	for i := range out {
		p := &out[i]
		if p.Origin == "" {
			p.Origin = "manual"
		}
		if p.Selection.Mode == "" {
			p.Selection.Mode = "manual"
		}
		if p.Selection.Mode == "manual" && p.Selection.Table == nil {
			p.Selection.Table = &PriceTable{}
		}
		if p.Selection.Table != nil {
			t := *p.Selection.Table
			if t.Currency == "" {
				t.Currency = "CNY"
			}
			if t.UnitTokens == 0 {
				t.UnitTokens = 1000000
			}
			if t.Rules == nil {
				t.Rules = []PriceRule{}
			}
			p.Selection.Table = &t
		}
	}
	return out
}
func validateRates(r PriceRates) error {
	for _, n := range []*float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
		if n != nil && (*n < 0 || math.IsNaN(*n) || math.IsInf(*n, 0)) {
			return NewConfigurationError("prices must be null or nonnegative finite numbers")
		}
	}
	return nil
}
func PriceMinute(v string) (int, error) {
	t, e := time.Parse("15:04", v)
	if e != nil || t.Format("15:04") != v {
		return 0, NewConfigurationError("time must be HH:mm in 00:00..23:59")
	}
	return t.Hour()*60 + t.Minute(), nil
}
func PriceWindowContains(a, b, m int) bool {
	if a == b {
		return false
	}
	if a < b {
		return m >= a && m < b
	}
	return m >= a || m < b
}
func ValidatePriceTable(p PriceTable) error {
	if p.Currency != "USD" && p.Currency != "CNY" {
		return NewConfigurationError("currency must be USD or CNY")
	}
	if p.UnitTokens < 1 {
		return NewConfigurationError("unit_tokens must be positive")
	}
	if e := validateRates(p.Rates); e != nil {
		return e
	}
	if len(p.Rules) > 1000 {
		return NewConfigurationError("too many price rules")
	}
	for _, r := range p.Rules {
		if !r.Enabled {
			continue
		}
		w := r.When
		if w.InputRange != nil && (w.InputRange.FromTokens < 0 || (w.InputRange.ToTokens != nil && *w.InputRange.ToTokens < w.InputRange.FromTokens)) {
			return NewConfigurationError("input range bounds must be nonnegative and ordered")
		}
		if tw := w.TimeWindow; tw != nil {
			if _, e := time.LoadLocation(tw.Timezone); e != nil || tw.Timezone == "" {
				return NewConfigurationError("time window requires a valid timezone")
			}
			if !tw.AllDay {
				a, e := PriceMinute(tw.Start)
				if e != nil {
					return e
				}
				b, e := PriceMinute(tw.End)
				if e != nil {
					return e
				}
				if a == b {
					return NewConfigurationError("time window start and end must differ")
				}
			}
		}
		if len(w.Weekdays) > 0 || len(w.DateSet) > 0 {
			if _, e := time.LoadLocation(w.Timezone); e != nil || w.Timezone == "" {
				return NewConfigurationError("weekday and date conditions require a valid timezone")
			}
		}
		seen := map[int]bool{}
		for _, d := range w.Weekdays {
			if d < 1 || d > 7 || seen[d] {
				return NewConfigurationError("weekdays must be unique ISO 1..7")
			}
			seen[d] = true
		}
		for _, d := range w.DateSet {
			t, e := time.Parse("2006-01-02", d)
			if e != nil || t.Format("2006-01-02") != d {
				return NewConfigurationError("dates must be YYYY-MM-DD")
			}
		}
		if (r.Action.Multiplier != nil) == (r.Action.RateChanges != nil) {
			return NewConfigurationError("rule requires exactly one of rate_changes or multiplier")
		}
		if n := r.Action.Multiplier; n != nil && (*n < 0 || math.IsNaN(*n) || math.IsInf(*n, 0)) {
			return NewConfigurationError("multiplier must be nonnegative and finite")
		}
		if r.Action.RateChanges != nil {
			if len(r.Action.RateChanges) == 0 {
				return NewConfigurationError("rate_changes must contain at least one key")
			}
			for k, n := range r.Action.RateChanges {
				if k != "input" && k != "output" && k != "cache_read" && k != "cache_write" {
					return NewConfigurationError("unknown rate change key")
				}
				if e := validateRates(PriceRates{Input: n}); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func ValidateCatalogModelRef(r CatalogModelRef) error {
	if strings.TrimSpace(r.Source) == "" || strings.TrimSpace(r.ID) == "" {
		return NewConfigurationError("catalog reference requires source and id")
	}
	return nil
}
func normalizePrices(prices []Price) ([]Price, error) {
	out := defaultPrices(prices)
	seen := map[string]bool{}
	for _, p := range out {
		if strings.TrimSpace(p.Model) == "" || strings.TrimSpace(p.Model) != p.Model || p.ChannelID < 0 {
			return nil, NewConfigurationError("invalid price model or channel")
		}
		k := fmt.Sprintf("%d/%s", p.ChannelID, p.Model)
		if seen[k] {
			return nil, NewConfigurationError("duplicate price identity")
		}
		seen[k] = true
		if p.Origin != "auto" && p.Origin != "manual" {
			return nil, NewConfigurationError("origin must be auto or manual")
		}
		switch p.Selection.Mode {
		case "manual":
			if p.Selection.Table == nil || p.Selection.Ref != nil {
				return nil, NewConfigurationError("manual selection requires only a table")
			}
			if e := ValidatePriceTable(*p.Selection.Table); e != nil {
				return nil, e
			}
		case "catalog":
			r := p.Selection.Ref
			if r == nil || p.Selection.Table != nil || r.OfferingKey == "" || r.QuoteKey == "" {
				return nil, NewConfigurationError("catalog selection requires only an exact quote reference")
			}
			if e := ValidateCatalogModelRef(CatalogModelRef{Source: r.Source, ID: r.ID}); e != nil {
				return nil, e
			}
		default:
			return nil, NewConfigurationError("selection mode must be manual or catalog")
		}
	}
	return out, nil
}
func PriceConfigured(p Price) bool {
	if p.Selection.Mode == "catalog" {
		return p.Selection.Ref != nil
	}
	t := p.Selection.Table
	if t == nil {
		return false
	}
	if t.Rates.Input != nil || t.Rates.Output != nil || t.Rates.CacheRead != nil || t.Rates.CacheWrite != nil {
		return true
	}
	for _, r := range t.Rules {
		if r.Enabled {
			return true
		}
	}
	return false
}
func (s *Store) GetPrices() ([]Price, error) { v, e := s.GetSettings(); return v.Prices, e }
func (s *Store) SavePrices(p []Price) error {
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	v, e := s.GetSettings()
	if e != nil {
		return e
	}
	v.Prices = p
	return s.saveSettingsLocked(v, true)
}
func normalizeUsagePricing(u *Usage) error {
	for _, n := range []*float64{u.OfficialValue, u.ActualCost} {
		if n != nil && (*n < 0 || math.IsNaN(*n) || math.IsInf(*n, 0)) {
			return NewConfigurationError("usage amounts must be nonnegative finite numbers")
		}
	}
	if u.OfficialValue == nil {
		u.OfficialCurrency = ""
	} else if u.OfficialCurrency != "USD" && u.OfficialCurrency != "CNY" {
		return NewConfigurationError("invalid official currency")
	}
	if u.ActualCost == nil {
		u.Currency = ""
	} else if u.Currency != "USD" && u.Currency != "CNY" {
		return NewConfigurationError("invalid actual currency")
	}
	return nil
}
