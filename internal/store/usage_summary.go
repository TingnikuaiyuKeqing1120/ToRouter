package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
	_ "time/tzdata" // Keep IANA buckets available in the Windows single binary.
)

type UsagePricingTotals struct {
	OfficialValues         map[string]float64 `json:"official_values"`
	ActualCosts            map[string]float64 `json:"actual_costs"`
	OfficialPricedRequests int                `json:"official_priced_requests"`
	ActualPricedRequests   int                `json:"actual_priced_requests"`
	PlanRequests           int                `json:"plan_requests"`
	PlanCosts              map[string]float64 `json:"plan_costs"`
	PlanCharges            int                `json:"plan_charges"`
	UnknownPlanCharges     int                `json:"unknown_plan_charges"`
}

func newUsagePricingTotals() UsagePricingTotals {
	return UsagePricingTotals{OfficialValues: map[string]float64{"USD": 0, "CNY": 0}, ActualCosts: map[string]float64{"USD": 0, "CNY": 0}, PlanCosts: map[string]float64{"USD": 0, "CNY": 0}}
}
func (v *UsagePricingTotals) add(u Usage) {
	if u.ActualPricing != nil && u.ActualPricing.Resolution.Layer == "plan" {
		v.PlanRequests++
	}
	if u.OfficialValue != nil {
		v.OfficialPricedRequests++
		v.OfficialValues[u.OfficialCurrency] += *u.OfficialValue
	}
	if u.ActualCost != nil {
		v.ActualPricedRequests++
		v.ActualCosts[u.Currency] += *u.ActualCost
	}
}

func (v *UsagePricingTotals) addPlanCharge(c PlanCharge) {
	v.PlanCharges++
	if c.Amount == nil {
		v.UnknownPlanCharges++
		return
	}
	v.PlanCosts[c.Currency] += *c.Amount
	v.ActualCosts[c.Currency] += *c.Amount
}

type UsageMetrics struct {
	Requests             int   `json:"requests"`
	Failures             int   `json:"failures"`
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	PlainInputTokens     int64 `json:"plain_input_tokens"`
	CacheReadTokens      int64 `json:"cache_read_tokens"`
	CacheWriteTokens     int64 `json:"cache_write_tokens"`
	KnownUsageRequests   int   `json:"known_usage_requests"`
	UnknownUsageRequests int   `json:"unknown_usage_requests"`
	UsagePricingTotals
}

func newUsageMetrics() UsageMetrics { return UsageMetrics{UsagePricingTotals: newUsagePricingTotals()} }
func (v *UsageMetrics) add(u Usage) {
	v.Requests++
	if u.Status >= 400 {
		v.Failures++
	}
	v.PlainInputTokens += u.InputTokens
	v.CacheReadTokens += u.CacheReadTokens
	v.CacheWriteTokens += u.CacheWriteTokens
	v.InputTokens += u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	v.OutputTokens += u.OutputTokens
	if u.Estimated {
		v.UnknownUsageRequests++
	} else {
		v.KnownUsageRequests++
	}
	v.UsagePricingTotals.add(u)
}

type UsageInputBucket struct {
	Label      string `json:"label"`
	FromTokens int64  `json:"from_tokens"`
	ToTokens   *int64 `json:"to_tokens"`
	Requests   int    `json:"requests"`
}

func newUsageInputDistribution() []UsageInputBucket {
	bounds := []int64{0, 4000, 16000, 32000, 64000, 128000}
	labels := []string{"0–4,000", "4,000–16,000", "16,000–32,000", "32,000–64,000", "64,000–128,000", "128,000+"}
	out := make([]UsageInputBucket, len(bounds))
	for i, from := range bounds {
		out[i] = UsageInputBucket{Label: labels[i], FromTokens: from}
		if i+1 < len(bounds) {
			to := bounds[i+1]
			out[i].ToTokens = &to
		}
	}
	return out
}
func addUsageInput(distribution []UsageInputBucket, u Usage) {
	if u.Estimated {
		return
	}
	input := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	for i := range distribution {
		b := &distribution[i]
		if input >= b.FromTokens && (b.ToTokens == nil || input < *b.ToTokens) {
			b.Requests++
			return
		}
	}
}

type UsageDay struct {
	Date string `json:"date"`
	UsageMetrics
}
type UsageGroup struct {
	Name      string `json:"name"`
	ChannelID *int64 `json:"channel_id,omitempty"`
	UsageMetrics
	Daily             []UsageDay         `json:"daily"`
	InputDistribution []UsageInputBucket `json:"input_distribution"`
	Models            []UsageGroup       `json:"models,omitempty"`
}
type UsageCostComponents struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite   float64 `json:"cache_write"`
	Unattributed float64 `json:"unattributed"`
	Plan         float64 `json:"plan"`
}
type UsageSummary struct {
	UsageMetrics
	Daily                    []UsageDay                     `json:"daily"`
	ByChannel                []UsageGroup                   `json:"by_channel"`
	ByModel                  []UsageGroup                   `json:"by_model"`
	InputDistribution        []UsageInputBucket             `json:"input_distribution"`
	Timezone                 string                         `json:"timezone"`
	ActualComponents         map[string]UsageCostComponents `json:"actual_components"`
	OfficialComponents       map[string]UsageCostComponents `json:"official_components"`
	UnconfiguredPlanChannels int                            `json:"unconfigured_plan_channels"`
}

// UsageLocation accepts IANA names only. Local is host-dependent, so it cannot
// describe the browser's chosen calendar reliably.
func UsageLocation(name string) (*time.Location, error) {
	if name == "" {
		name = "UTC"
	}
	if name == "Local" {
		return nil, fmt.Errorf("invalid timezone")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone")
	}
	return loc, nil
}

type usageGroupAccumulator struct {
	group        UsageGroup
	days         map[string]*UsageDay
	models       map[string]*usageGroupAccumulator
	latestMillis int64
	latestID     int64
}

func newUsageGroup(name string) *usageGroupAccumulator {
	return &usageGroupAccumulator{group: UsageGroup{Name: name, UsageMetrics: newUsageMetrics(), InputDistribution: newUsageInputDistribution()}, days: map[string]*UsageDay{}}
}
func addUsageDay(days map[string]*UsageDay, date string, u Usage) {
	usageDay(days, date).add(u)
}
func usageDay(days map[string]*UsageDay, date string) *UsageDay {
	day := days[date]
	if day == nil {
		day = &UsageDay{Date: date, UsageMetrics: newUsageMetrics()}
		days[date] = day
	}
	return day
}
func usageDays(days map[string]*UsageDay) []UsageDay {
	out := make([]UsageDay, 0, len(days))
	for _, day := range days {
		out = append(out, *day)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}
func (a *usageGroupAccumulator) add(date string, u Usage) {
	a.group.add(u)
	addUsageDay(a.days, date, u)
	addUsageInput(a.group.InputDistribution, u)
}
func usageGroups(groups map[string]*usageGroupAccumulator) []UsageGroup {
	out := make([]UsageGroup, 0, len(groups))
	for _, a := range groups {
		a.group.Daily = usageDays(a.days)
		if a.models != nil {
			a.group.Models = usageGroups(a.models)
		}
		out = append(out, a.group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ChannelID != nil && out[j].ChannelID != nil && *out[i].ChannelID < *out[j].ChannelID
	})
	return out
}

// Attribute only a saved amount whose own effective snapshot explains it.
// Missing or inconsistent history remains in unattributed, never repriced.
func addUsageComponents(totals map[string]UsageCostComponents, amount *float64, currency string, basis *UsagePricingBasis, u Usage) {
	if amount == nil {
		return
	}
	v := totals[currency]
	parts := [4]float64{}
	valid := basis != nil && basis.UnitTokens > 0 && basis.Currency == currency && basis.MissingReason == "" && !u.Estimated
	quantities := [4]int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens}
	if valid {
		rates := [4]*float64{basis.Rates.Input, basis.Rates.Output, basis.Rates.CacheRead, basis.Rates.CacheWrite}
		for i, q := range quantities {
			if q < 0 || q > 0 && (rates[i] == nil || *rates[i] < 0 || math.IsNaN(*rates[i]) || math.IsInf(*rates[i], 0)) {
				valid = false
				break
			}
			if q > 0 {
				parts[i] = float64(q) / float64(basis.UnitTokens) * *rates[i]
			}
		}
	}
	sum := parts[0] + parts[1] + parts[2] + parts[3]
	roundoff := 8 * (math.Nextafter(1, 2) - 1) * math.Max(math.Abs(sum), math.Abs(*amount))
	if !valid || math.IsNaN(sum) || math.IsInf(sum, 0) || math.Abs(sum-*amount) > roundoff {
		v.Unattributed += *amount
	} else {
		// Reconcile only floating-point roundoff; preserve the saved total exactly.
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] != 0 {
				parts[i] += *amount - sum
				break
			}
		}
		v.Input += parts[0]
		v.Output += parts[1]
		v.CacheRead += parts[2]
		v.CacheWrite += parts[3]
	}
	totals[currency] = v
}

// SummarizeUsage scans a single full snapshot, independent of log display limits.
// Only needed scalars and effective price rates cross the SQL boundary. Memory
// grows with groups/days, rather than the number or size of saved request logs.
func (s *Store) SummarizeUsage(ctx context.Context, f UsageFilter) (UsageSummary, error) {
	loc, err := UsageLocation(f.Timezone)
	if err != nil {
		return UsageSummary{}, err
	}
	if err := ctx.Err(); err != nil {
		return UsageSummary{}, err
	}
	now := time.Now()
	if err := s.AccruePlanCharges(now); err != nil {
		return UsageSummary{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UsageSummary{}, err
	}
	defer tx.Rollback()
	f.ExcludeNotSent = true
	where, args := usageWhere(f)
	rows, err := tx.QueryContext(ctx, `SELECT id,time,channel_id,model,status,
 COALESCE(json_extract(data,'$.channel_name'),''),
 COALESCE(json_extract(data,'$.input_tokens'),0), COALESCE(json_extract(data,'$.output_tokens'),0),
 COALESCE(json_extract(data,'$.cache_read_tokens'),0), COALESCE(json_extract(data,'$.cache_write_tokens'),0),
 COALESCE(json_extract(data,'$.estimated'),0),
 json_extract(data,'$.official_value'), COALESCE(json_extract(data,'$.official_currency'),''),
 json_extract(data,'$.actual_cost'), COALESCE(json_extract(data,'$.currency'),''),
	json_object('resolution',json_object('layer',COALESCE(json_extract(data,'$.actual_pricing.resolution.layer'),'')),'rates',json_extract(data,'$.actual_pricing.rates'),'unit_tokens',json_extract(data,'$.actual_pricing.unit_tokens'),'currency',json_extract(data,'$.actual_pricing.currency'),'missing_reason',json_extract(data,'$.actual_pricing.missing_reason')),
 json_object('rates',json_extract(data,'$.official_pricing.rates'),'unit_tokens',json_extract(data,'$.official_pricing.unit_tokens'),'currency',json_extract(data,'$.official_pricing.currency'),'missing_reason',json_extract(data,'$.official_pricing.missing_reason'))
 FROM usage_logs`+where, args...)
	if err != nil {
		return UsageSummary{}, err
	}
	defer rows.Close()
	result := UsageSummary{UsageMetrics: newUsageMetrics(), InputDistribution: newUsageInputDistribution(), Timezone: loc.String(), ActualComponents: map[string]UsageCostComponents{"USD": {}, "CNY": {}}, OfficialComponents: map[string]UsageCostComponents{"USD": {}, "CNY": {}}}
	channels, models := map[string]*usageGroupAccumulator{}, map[string]*usageGroupAccumulator{}
	days := map[string]*UsageDay{}
	planChannels := map[int64]bool{}
	for rows.Next() {
		var u Usage
		var millis int64
		var actual, official string
		if err := rows.Scan(&u.ID, &millis, &u.ChannelID, &u.Model, &u.Status, &u.ChannelName, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.Estimated, &u.OfficialValue, &u.OfficialCurrency, &u.ActualCost, &u.Currency, &actual, &official); err != nil {
			return UsageSummary{}, err
		}
		var actualBasis, officialBasis UsagePricingBasis
		if err := json.Unmarshal([]byte(actual), &actualBasis); err != nil {
			return UsageSummary{}, err
		}
		if err := json.Unmarshal([]byte(official), &officialBasis); err != nil {
			return UsageSummary{}, err
		}
		u.ActualPricing = &actualBasis
		u.OfficialPricing = &officialBasis
		if actualBasis.Resolution.Layer == "plan" {
			planChannels[u.ChannelID] = true
		}
		result.add(u)
		addUsageInput(result.InputDistribution, u)
		addUsageComponents(result.ActualComponents, u.ActualCost, u.Currency, &actualBasis, u)
		addUsageComponents(result.OfficialComponents, u.OfficialValue, u.OfficialCurrency, &officialBasis, u)
		date := time.UnixMilli(millis).In(loc).Format("2006-01-02")
		addUsageDay(days, date, u)
		key := fmt.Sprint(u.ChannelID)
		channel := channels[key]
		if channel == nil {
			channel = newUsageGroup(u.ChannelName)
			id := u.ChannelID
			channel.group.ChannelID = &id
			channel.models = map[string]*usageGroupAccumulator{}
			channels[key] = channel
		}
		if channel.group.Requests == 0 || millis > channel.latestMillis || millis == channel.latestMillis && u.ID > channel.latestID {
			channel.group.Name = u.ChannelName
			channel.latestMillis = millis
			channel.latestID = u.ID
		}
		channel.add(date, u)
		if channel.models[u.Model] == nil {
			channel.models[u.Model] = newUsageGroup(u.Model)
		}
		channel.models[u.Model].add(date, u)
		if models[u.Model] == nil {
			models[u.Model] = newUsageGroup(u.Model)
		}
		models[u.Model].add(date, u)
	}
	if err := rows.Err(); err != nil {
		return UsageSummary{}, err
	}
	if err := rows.Close(); err != nil {
		return UsageSummary{}, err
	}
	charges, err := listPlanChargesInTx(ctx, tx, f, now)
	if err != nil {
		return UsageSummary{}, err
	}
	for _, charge := range charges {
		result.addPlanCharge(charge)
		date := charge.PaidAt.In(loc).Format("2006-01-02")
		usageDay(days, date).addPlanCharge(charge)
		key := fmt.Sprint(charge.ChannelID)
		channel := channels[key]
		if channel == nil {
			channel = newUsageGroup(charge.ChannelName)
			id := charge.ChannelID
			channel.group.ChannelID = &id
			channels[key] = channel
		}
		channel.group.addPlanCharge(charge)
		usageDay(channel.days, date).addPlanCharge(charge)
		if charge.Amount != nil {
			components := result.ActualComponents[charge.Currency]
			components.Plan += *charge.Amount
			result.ActualComponents[charge.Currency] = components
		}
	}
	result.UnconfiguredPlanChannels, err = unconfiguredPlanChannelsInTx(ctx, tx, f, planChannels)
	if err != nil {
		return UsageSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return UsageSummary{}, err
	}
	result.Daily = usageDays(days)
	result.ByChannel = usageGroups(channels)
	result.ByModel = usageGroups(models)
	return result, nil
}
