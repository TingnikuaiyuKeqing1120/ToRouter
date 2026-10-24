package gateway

import (
	"slices"
	"testing"

	"torouter/internal/store"
)

func TestComputeEffectiveTagsOnlyOwnCustomTags(t *testing.T) {
	groups := append(slices.Clone(store.DefaultTagGroups), store.TagGroup{ID: "scenario", Type: store.TagGroupTypeUser})
	definitions := append(slices.Clone(store.DefaultTags),
		store.Tag{ID: "scenario:daily", GroupID: "scenario"},
		store.Tag{ID: "scenario:channel", GroupID: "scenario"},
		store.Tag{ID: "scenario:upstream", GroupID: "scenario"},
		store.Tag{ID: "custom-looking-builtin", GroupID: "privacy"},
		store.Tag{ID: "billing:custom", GroupID: "scenario"},
		store.Tag{ID: "orphan", GroupID: "missing"},
	)
	// Moving a built-in modality ID into a user group must not allow a manual
	// tag to claim a specification that is still unknown.
	for i := range definitions {
		if definitions[i].ID == "modality:input:image" {
			definitions[i].GroupID = "scenario"
		}
	}
	dm := store.DownstreamModel{
		Tags:     []string{" scenario:daily ", "scenario:daily", "billing:custom", "unknown", "orphan", "custom-looking-builtin", "billing:free", "privacy:zdr", "modality:input:image"},
		Bindings: []store.Binding{{ChannelID: 1, Model: "native", Enabled: true}},
	}
	channels := map[int64]store.Channel{1: {
		Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses}},
		Tags:      []string{"scenario:channel", "billing:payg", "privacy:zdr"},
		ModelTags: map[string][]string{"native": {"scenario:upstream", "modality:input:image"}},
	}}
	want := []string{"billing:custom", "scenario:daily"}
	if got := ComputeEffectiveTags(dm, channels, definitions, groups); !slices.Equal(got, want) {
		t.Fatalf("public tags inherited or accepted invalid manual values: %v", got)
	}
	details := ComputeEffectiveTagsDetail(dm, channels, definitions, groups)
	for _, detail := range details {
		if !slices.Equal(detail.Sources, []string{"公开模型显式配置"}) {
			t.Fatalf("custom tag source = %+v", detail)
		}
	}
}

func TestComputeEffectiveTagsModalitiesUsePublicSpecifications(t *testing.T) {
	text := []string{"text"}
	image := []string{"text", "image"}
	empty := []string{}
	known := store.ModelConfig{InputModalities: &image, OutputModalities: &text}
	base := store.Channel{Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses}}, ModelConfigs: map[string]store.ModelConfig{"native": known}}
	cases := []struct {
		name           string
		second         store.Channel
		bindingEnabled bool
		present        bool
		want           []string
	}{
		{"consistent", base, true, true, []string{"modality:input:image", "modality:input:text", "modality:output:text"}},
		{"unknown input only", store.Channel{Enabled: true, Endpoints: base.Endpoints, ModelConfigs: map[string]store.ModelConfig{"native": {OutputModalities: &text}}}, true, true, []string{"modality:output:text"}},
		{"different input", store.Channel{Enabled: true, Endpoints: base.Endpoints, ModelConfigs: map[string]store.ModelConfig{"native": {InputModalities: &text, OutputModalities: &text}}}, true, true, []string{"modality:output:text"}},
		{"all unknown", store.Channel{Enabled: true, Endpoints: base.Endpoints}, true, true, []string{}},
		{"known empty differs", store.Channel{Enabled: true, Endpoints: base.Endpoints, ModelConfigs: map[string]store.ModelConfig{"native": {InputModalities: &empty, OutputModalities: &empty}}}, true, true, []string{}},
		{"unusable endpoint excluded", store.Channel{Enabled: true, Endpoints: []store.Endpoint{{Format: "unsupported"}}}, true, true, []string{"modality:input:image", "modality:input:text", "modality:output:text"}},
		{"no endpoint excluded", store.Channel{Enabled: true}, true, true, []string{"modality:input:image", "modality:input:text", "modality:output:text"}},
		{"disabled channel excluded", store.Channel{Enabled: false, Endpoints: base.Endpoints}, true, true, []string{"modality:input:image", "modality:input:text", "modality:output:text"}},
		{"disabled binding excluded", store.Channel{Enabled: true, Endpoints: base.Endpoints}, false, true, []string{"modality:input:image", "modality:input:text", "modality:output:text"}},
		{"missing channel excluded", store.Channel{}, true, false, []string{"modality:input:image", "modality:input:text", "modality:output:text"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channels := map[int64]store.Channel{1: base}
			if tc.present {
				channels[2] = tc.second
			}
			dm := store.DownstreamModel{Bindings: []store.Binding{{ChannelID: 1, Model: "native", Enabled: true}, {ChannelID: 2, Model: "native", Enabled: tc.bindingEnabled}}, Tags: []string{"modality:input:image"}}
			if got := ComputeEffectiveTags(dm, channels, store.DefaultTags, store.DefaultTagGroups); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, config := range []store.ModelConfig{{}, {InputModalities: &empty, OutputModalities: &empty}} {
		channels := map[int64]store.Channel{1: {Enabled: true, Endpoints: base.Endpoints, ModelConfigs: map[string]store.ModelConfig{"native": config}}}
		dm := store.DownstreamModel{Bindings: []store.Binding{{ChannelID: 1, Model: "native", Enabled: true}}}
		if got := ComputeEffectiveTags(dm, channels, nil, nil); len(got) != 0 {
			t.Fatalf("unknown or empty specs produced tags: %v", got)
		}
	}
}
