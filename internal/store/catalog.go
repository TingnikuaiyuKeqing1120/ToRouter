package store

import "time"

type CatalogModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}
type CatalogThinking = NativeThinking
type CatalogQuote struct {
	Key        string     `json:"key"`
	Name       string     `json:"name,omitempty"`
	Table      PriceTable `json:"table"`
	SourceKind string     `json:"source_kind"`
	SourceURL  string     `json:"source_url,omitempty"`
	VerifiedAt string     `json:"verified_at,omitempty"`
	FetchedAt  *time.Time `json:"fetched_at,omitempty"`
	Notes      []string   `json:"notes,omitempty"`
}
type CatalogOffering struct {
	Key         string         `json:"key"`
	Provider    string         `json:"provider"`
	Region      string         `json:"region,omitempty"`
	ServiceTier string         `json:"service_tier,omitempty"`
	SourceKind  string         `json:"source_kind"`
	Quotes      []CatalogQuote `json:"quotes"`
}
type CatalogItem struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Provider         string            `json:"provider"`
	Source           string            `json:"source"`
	OriginalID       string            `json:"original_id,omitempty"`
	Aliases          []string          `json:"aliases,omitempty"`
	ContextLength    *int64            `json:"context_length"`
	MaxInputTokens   *int64            `json:"max_input_tokens"`
	MaxOutputTokens  *int64            `json:"max_output_tokens"`
	APIFormats       []string          `json:"api_formats,omitempty"`
	OutputTokenField string            `json:"output_token_field,omitempty"`
	Modalities       CatalogModalities `json:"modalities"`
	Thinking         CatalogThinking   `json:"thinking"`
	Defaults         *ModelDefaults    `json:"defaults,omitempty"`
	Offerings        []CatalogOffering `json:"offerings"`
	DocURL           string            `json:"doc_url,omitempty"`
	PricingURL       string            `json:"pricing_url,omitempty"`
	VerifiedAt       string            `json:"verified_at,omitempty"`
	FetchedAt        *time.Time        `json:"fetched_at,omitempty"`
	NeedsRefresh     bool              `json:"needs_refresh,omitempty"`
	Notes            []string          `json:"notes,omitempty"`
}
type CatalogSourceStatus struct {
	Source         string     `json:"source"`
	Name           string     `json:"name"`
	Enabled        bool       `json:"enabled"`
	PrefillEnabled bool       `json:"prefill_enabled"`
	Priority       int        `json:"priority"`
	ItemCount      int        `json:"item_count"`
	LastFetchedAt  *time.Time `json:"last_fetched_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
}
