package store

import (
	"encoding/json"
	"time"
)

type PriceRates struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}
type PriceInputRange struct {
	FromTokens int64  `json:"from_tokens"`
	ToTokens   *int64 `json:"to_tokens"`
}
type PriceTimeWindow struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	AllDay   bool   `json:"all_day"`
	Timezone string `json:"timezone"`
}
type PriceRuleWhen struct {
	InputRange *PriceInputRange `json:"input_range,omitempty"`
	TimeWindow *PriceTimeWindow `json:"time_window,omitempty"`
	Weekdays   []int            `json:"weekdays,omitempty"`
	DateSet    []string         `json:"date_set,omitempty"`
	Timezone   string           `json:"timezone,omitempty"`
}
type PriceRuleAction struct {
	RateChanges map[string]*float64 `json:"rate_changes,omitempty"`
	Multiplier  *float64            `json:"multiplier,omitempty"`
}
type PriceRule struct {
	Draft   json.RawMessage `json:"draft,omitempty"`
	ID      string          `json:"id,omitempty"`
	Enabled bool            `json:"enabled"`
	When    PriceRuleWhen   `json:"when"`
	Action  PriceRuleAction `json:"action"`
}
type PriceTable struct {
	Currency            string      `json:"currency"`
	UnitTokens          int64       `json:"unit_tokens"`
	Rates               PriceRates  `json:"rates"`
	Rules               []PriceRule `json:"rules"`
	UncoveredConditions []string    `json:"uncovered_conditions,omitempty"`
}
type CatalogModelRef struct {
	Source      string `json:"source"`
	ID          string `json:"id"`
	OfferingKey string `json:"offering_key,omitempty"`
}
type CatalogPriceRef struct {
	Source      string `json:"source"`
	ID          string `json:"id"`
	OfferingKey string `json:"offering_key"`
	QuoteKey    string `json:"quote_key"`
}
type PriceSelection struct {
	Mode  string           `json:"mode"`
	Table *PriceTable      `json:"table,omitempty"`
	Ref   *CatalogPriceRef `json:"ref,omitempty"`
}
type PriceEntry struct {
	ChannelID   int64          `json:"channel_id"`
	Model       string         `json:"model"`
	DisplayName string         `json:"display_name"`
	Origin      string         `json:"origin"`
	Selection   PriceSelection `json:"selection"`
}

// Price names the sole persisted price entry contract.
type Price = PriceEntry

type PriceResolution struct {
	SourceKind    string           `json:"source_kind,omitempty"`
	SourceURL     string           `json:"source_url,omitempty"`
	VerifiedAt    string           `json:"verified_at,omitempty"`
	Status        string           `json:"status"`
	MissingReason string           `json:"missing_reason,omitempty"`
	Layer         string           `json:"layer"`
	ChannelID     int64            `json:"channel_id"`
	Model         string           `json:"model"`
	Table         *PriceTable      `json:"table,omitempty"`
	Ref           *CatalogPriceRef `json:"ref,omitempty"`
	FetchedAt     *time.Time       `json:"fetched_at,omitempty"`
}
type UsagePricingBasis struct {
	Method        string          `json:"method"`
	CalculatedAt  time.Time       `json:"calculated_at"`
	Resolution    PriceResolution `json:"resolution"`
	Rates         PriceRates      `json:"rates"`
	Currency      string          `json:"currency"`
	UnitTokens    int64           `json:"unit_tokens"`
	MatchedRules  []string        `json:"matched_rules"`
	MissingReason string          `json:"missing_reason,omitempty"`
	Note          string          `json:"note,omitempty"`
}
