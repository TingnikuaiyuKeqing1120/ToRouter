package store

import (
	"math"
)

type ModelDefaults struct {
	MaxOutputTokens *int64           `json:"max_output_tokens,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	Thinking        *ThinkingDefault `json:"thinking,omitempty"`
}
type ThinkingDefault struct {
	Mode    string  `json:"mode"`
	Effort  *string `json:"effort,omitempty"`
	Budget  *int64  `json:"budget,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}
type NativeThinking struct {
	Supported *bool                `json:"supported"`
	Mandatory *bool                `json:"mandatory,omitempty"`
	Modes     []NativeThinkingMode `json:"modes"`
}
type NativeThinkingMode struct {
	ID               string   `json:"id"`
	APIFormat        string   `json:"api_format"`
	Control          string   `json:"control"`
	OutputTokenField string   `json:"output_token_field,omitempty"`
	Efforts          []string `json:"efforts"`
	DefaultEffort    *string  `json:"default_effort,omitempty"`
	// nil is unknown; an empty list explicitly permits no effort while disabled.
	DisabledEfforts []string `json:"disabled_efforts"`
	BudgetMin       *int64   `json:"budget_min,omitempty"`
	BudgetMax       *int64   `json:"budget_max,omitempty"`
}

func int64Pointer(n int64) *int64 { return &n }
func ValidateModelDefaults(d *ModelDefaults) error {
	if d == nil {
		return nil
	}
	if d.MaxOutputTokens != nil && (*d.MaxOutputTokens < 0 || *d.MaxOutputTokens > 1000000000) {
		return NewConfigurationError("default output budget must be 0..1000000000")
	}
	if d.Temperature != nil && (*d.Temperature < 0 || *d.Temperature > 2 || math.IsNaN(*d.Temperature) || math.IsInf(*d.Temperature, 0)) {
		return NewConfigurationError("default temperature must be 0..2")
	}
	if t := d.Thinking; t != nil {
		if t.Mode == "" || len(t.Mode) > 64 {
			return NewConfigurationError("thinking mode must be 1..64 characters")
		}
		if t.Effort != nil && (len(*t.Effort) == 0 || len(*t.Effort) > 64) {
			return NewConfigurationError("thinking effort must be 1..64 characters")
		}
		if t.Budget != nil && (*t.Budget < 0 || *t.Budget > 1000000000) {
			return NewConfigurationError("thinking budget must be 0..1000000000")
		}
	}
	return nil
}
