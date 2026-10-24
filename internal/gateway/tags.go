package gateway

import (
	"slices"
	"strings"

	route "torouter/internal/router"
	"torouter/internal/store"
)

type EffectiveTagDetail struct {
	TagID   string   `json:"tag_id"`
	Sources []string `json:"sources"`
}

// ComputeEffectiveTags combines the public model's custom tags with modalities
// derived from its consistent, known public specifications.
func ComputeEffectiveTags(dm store.DownstreamModel, channels map[int64]store.Channel, tags []store.Tag, groups []store.TagGroup) []string {
	details := ComputeEffectiveTagsDetail(dm, channels, tags, groups)
	result := make([]string, 0, len(details))
	for _, d := range details {
		result = append(result, d.TagID)
	}
	return result
}

// ComputeEffectiveTagsDetail never inherits channel or upstream-model tags.
func ComputeEffectiveTagsDetail(dm store.DownstreamModel, channels map[int64]store.Channel, tags []store.Tag, groups []store.TagGroup) []EffectiveTagDetail {
	sourcesMap := make(map[string][]string)
	for _, tagID := range store.FilterDownstreamModelTags(dm.Tags, tags, groups) {
		sourcesMap[tagID] = []string{"公开模型显式配置"}
	}

	// Match ModelSpecs without api_format filtering: a binding participates only
	// when at least one public request format has a usable endpoint.
	var bindings []SpecBinding
	for _, b := range dm.Bindings {
		ch, ok := channels[b.ChannelID]
		if !b.Enabled || !ok || !ch.Enabled {
			continue
		}
		for _, format := range specFormats {
			if _, ok := route.PickModelEndpoint(ch, b.Model, format); ok {
				bindings = append(bindings, SpecBinding{Capabilities: capabilities(store.ResolveConfig(ch.ModelConfigs[b.Model]).Config)})
				break
			}
		}
	}
	values, _, _ := consistentSpecs(bindings)
	addModalityTags := func(field, prefix, source string) {
		if modalities, ok := values[field].([]string); ok {
			for _, modality := range modalities {
				modality = strings.ToLower(strings.TrimSpace(modality))
				if modality != "" {
					sourcesMap[prefix+modality] = []string{source}
				}
			}
		}
	}
	addModalityTags("input_modalities", "modality:input:", "公开规格一致值 (输入)")
	addModalityTags("output_modalities", "modality:output:", "公开规格一致值 (输出)")

	details := []EffectiveTagDetail{}
	for tagID, sources := range sourcesMap {
		details = append(details, EffectiveTagDetail{TagID: tagID, Sources: sources})
	}
	slices.SortFunc(details, func(a, b EffectiveTagDetail) int {
		return strings.Compare(a.TagID, b.TagID)
	})
	return details
}
