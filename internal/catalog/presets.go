package catalog

import (
	_ "embed"
	"encoding/json"
	"torouter/internal/store"
)

//go:embed channel-presets.json
var channelPresetJSON []byte

type ChannelPreset struct {
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	Label             string           `json:"label"`
	Category          string           `json:"category"`
	BaseURL           string           `json:"base_url"`
	Endpoints         []store.Endpoint `json:"endpoints"`
	PreferredEndpoint string           `json:"preferred_endpoint,omitempty"`
	Hint              string           `json:"hint,omitempty"`
}

// ChannelPresets and the Web picker consume the same embedded source.
func ChannelPresets() []ChannelPreset {
	var rows []ChannelPreset
	if err := json.Unmarshal(channelPresetJSON, &rows); err != nil {
		panic(err)
	}
	return rows
}
