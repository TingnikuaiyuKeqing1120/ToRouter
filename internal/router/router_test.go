package router

import (
	"testing"

	"torouter/internal/store"
)

func channels() []store.Channel {
	return []store.Channel{
		{ID: 1, Name: "a", Enabled: true, Models: []string{"m-a"},
			Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://a.example/v1"}}},
		{ID: 2, Name: "b", Enabled: true, Models: []string{"m-b"},
			Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://b.example/v1"}}},
		{ID: 3, Name: "c", Enabled: false, Models: []string{"m-c"},
			Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: "https://c.example/v1"}}},
	}
}

const chatFormat = store.FormatOpenAIChatCompletions

func TestBindingStructurePreservesExplicitCatalogueBinding(t *testing.T) {
	c := channels()[0]
	c.AvailableModels = []string{"catalogue-only"}
	for _, id := range []string{"m-a", "catalogue-only"} {
		binding := store.Binding{ChannelID: c.ID, Model: id, Enabled: true}
		if reason := BindingStructureReason(c, binding); reason != "" {
			t.Fatalf("existing model %s rejected: %s", id, reason)
		}
		if got := Candidates([]store.Channel{c}, models(binding), "public", chatFormat); len(got) != 1 {
			t.Fatalf("existing binding lost: %s", id)
		}
	}
	if reason := BindingStructureReason(c, store.Binding{ChannelID: c.ID, Model: "missing", Enabled: true}); reason == "" {
		t.Fatal("nonexistent model admitted")
	}
}

func models(bindings ...store.Binding) []store.DownstreamModel {
	return []store.DownstreamModel{{ID: "public", Enabled: true, Fallback: true, Bindings: bindings}}
}
func TestPriorityOrderAndFallback(t *testing.T) {
	b := []store.Binding{{ChannelID: 1, Model: "m-a", Priority: 1, Enabled: true}, {ChannelID: 2, Model: "m-b", Priority: 0, Enabled: true}}
	got := Candidates(channels(), models(b...), "public", chatFormat)
	if len(got) != 2 || got[0].Channel.ID != 2 || got[1].Channel.ID != 1 {
		t.Fatalf("priority order wrong: %+v", got)
	}
	if len(Candidates(channels(), models(b...), "other", chatFormat)) != 0 {
		t.Fatal("unknown model matched")
	}
	off := []store.DownstreamModel{{ID: "public", Enabled: false, Bindings: b}}
	if len(Candidates(channels(), off, "public", chatFormat)) != 0 {
		t.Fatal("disabled model matched")
	}
}
func TestDisabledBindingAndChannelSkipped(t *testing.T) {
	b := []store.Binding{{ChannelID: 1, Model: "m-a", Priority: 0, Enabled: false}, {ChannelID: 3, Model: "m-c", Priority: 0, Enabled: true}, {ChannelID: 2, Model: "m-b", Priority: 1, Enabled: true}}
	got := Candidates(channels(), models(b...), "public", chatFormat)
	if len(got) != 1 || got[0].Channel.ID != 2 {
		t.Fatalf("disabled binding or disabled channel selected: %+v", got)
	}
}
func TestCandidatesKeepBindingOrderWithinPriority(t *testing.T) {
	b := []store.Binding{{ChannelID: 1, Model: "m-a", Priority: 0, Enabled: true}, {ChannelID: 2, Model: "m-b", Priority: 0, Enabled: true}}
	for _, bindings := range [][]store.Binding{b, {b[1], b[0]}} {
		for i := 0; i < 32; i++ {
			got := Candidates(channels(), models(bindings...), "public", chatFormat)
			if len(got) != len(bindings) {
				t.Fatalf("both bindings should be candidates, got %+v", got)
			}
			for index, binding := range bindings {
				if got[index].Channel.ID != binding.ChannelID {
					t.Fatalf("same-priority order changed on request %d: index %d got channel %d, want %d", i, index, got[index].Channel.ID, binding.ChannelID)
				}
			}
		}
	}
}

func TestResponsesUsesConversionOrExactEndpoint(t *testing.T) {
	b := []store.Binding{{ChannelID: 1, Model: "m-a", Priority: 0, Enabled: true}}
	if got := Candidates(channels(), models(b...), "public", store.FormatOpenAIResponses); len(got) != 1 || got[0].Endpoint.Format != chatFormat {
		t.Fatalf("a chat-completions-only channel should convert responses: %+v", got)
	}
	enabled := channels()
	enabled[2].Enabled = true
	if got := Candidates(enabled, models(store.Binding{ChannelID: 3, Model: "m-c", Priority: 0, Enabled: true}), "public", store.FormatOpenAIResponses); len(got) != 1 || got[0].Endpoint.Format != store.FormatAnthropicMessages {
		t.Fatalf("an enabled anthropic-only channel should convert responses: %+v", got)
	}
	declared := channels()
	declared[0].Endpoints = append(declared[0].Endpoints, store.Endpoint{Format: store.FormatOpenAIResponses, BaseURL: "https://a.example/v1"})
	got := Candidates(declared, models(b...), "public", store.FormatOpenAIResponses)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatOpenAIResponses {
		t.Fatalf("declared responses endpoint not used: %+v", got)
	}
}

// Cross-protocol routing keeps working for the existing portable formats:
// an OpenAI request may be served by an Anthropic-only channel, which is the
// gateway's whole purpose.
func TestOtherFormatsConvertAcrossProtocols(t *testing.T) {
	enabled := channels()
	enabled[2].Enabled = true
	got := Candidates(enabled, models(store.Binding{ChannelID: 3, Model: "m-c", Priority: 0, Enabled: true}), "public", chatFormat)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatAnthropicMessages {
		t.Fatalf("anthropic channel should serve a chat request by conversion: %+v", got)
	}
	// An exact match wins when the channel serves several facades.
	both := channels()
	both[0].Endpoints = append(both[0].Endpoints, store.Endpoint{Format: store.FormatAnthropicMessages, BaseURL: "https://a.example/v1"})
	got = Candidates(both, models(store.Binding{ChannelID: 1, Model: "m-a", Priority: 0, Enabled: true}), "public", chatFormat)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatOpenAIChatCompletions {
		t.Fatalf("exact format match should win: %+v", got)
	}
}

// All four known formats convert pairwise. Content and continuation support is
// validated by the converter after routing, not inferred from this matrix.
func TestCanConvertMatrix(t *testing.T) {
	chat, responses := store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses
	messages, gemini := store.FormatAnthropicMessages, store.FormatGeminiGenerateContent
	matrix := map[string]map[string]bool{
		chat:      {chat: true, responses: true, messages: true, gemini: true},
		responses: {chat: true, responses: true, messages: true, gemini: true},
		messages:  {chat: true, responses: true, messages: true, gemini: true},
		gemini:    {chat: true, responses: true, messages: true, gemini: true},
	}
	for from, row := range matrix {
		for to, expect := range row {
			if got := CanConvert(from, to); got != expect {
				t.Errorf("CanConvert(%s, %s) = %v, want %v", from, to, got, expect)
			}
		}
	}
	if CanConvert("unknown_format", chat) || CanConvert(chat, "unknown_format") || CanConvert("unknown_format", "unknown_format") {
		t.Error("an unknown format must not be convertible")
	}
}

func TestResponsesOnlyChannelServesChatByConversion(t *testing.T) {
	only := []store.Channel{{ID: 1, Name: "resp", Enabled: true, Models: []string{"m"},
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://r.example/v1"}}}}
	b := []store.Binding{{ChannelID: 1, Model: "m", Priority: 0, Enabled: true}}
	if got := Candidates(only, models(b...), "public", chatFormat); len(got) != 1 || got[0].Endpoint.Format != store.FormatOpenAIResponses {
		t.Fatalf("a responses-only channel should convert a chat request: %+v", got)
	}
}

// T019/T020: [responses, messages] must serve a chat request from its messages
// endpoint whichever order the array is in: array order is display order only
// and never selects the endpoint.
func TestEndpointArrayOrderDoesNotChooseResponses(t *testing.T) {
	b := []store.Binding{{ChannelID: 1, Model: "m", Priority: 0, Enabled: true}}
	for _, order := range [][]string{{store.FormatOpenAIResponses, store.FormatAnthropicMessages}, {store.FormatAnthropicMessages, store.FormatOpenAIResponses}} {
		c := store.Channel{ID: 1, Name: "both", Enabled: true, Models: []string{"m"},
			Endpoints: []store.Endpoint{{Format: order[0], BaseURL: "https://a.example/v1"}, {Format: order[1], BaseURL: "https://a.example/v1"}}}
		got := Candidates([]store.Channel{c}, models(b...), "public", chatFormat)
		if len(got) != 1 || got[0].Endpoint.Format != store.FormatAnthropicMessages {
			t.Fatalf("array order %v chose %+v", order, got)
		}
	}
}

// T019: conversion is the gateway's purpose, so a channel that declares none of
// the OpenAI formats is still usable for a chat request.
func TestGeminiOnlyChannelServesChatByConversion(t *testing.T) {
	c := []store.Channel{{ID: 1, Name: "gem", Enabled: true, Models: []string{"m"},
		Endpoints: []store.Endpoint{{Format: store.FormatGeminiGenerateContent, BaseURL: "https://g.example"}}}}
	b := []store.Binding{{ChannelID: 1, Model: "m", Priority: 0, Enabled: true}}
	got := Candidates(c, models(b...), "public", chatFormat)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatGeminiGenerateContent {
		t.Fatalf("gemini-only channel should serve a chat request by conversion: %+v", got)
	}
}

func TestResponsesInboundUsesDefaultOrderUntilExactEndpointExists(t *testing.T) {
	b := []store.Binding{{ChannelID: 1, Model: "m", Priority: 0, Enabled: true}}
	c := store.Channel{ID: 1, Name: "noresp", Enabled: true, Models: []string{"m"},
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://a.example/v1"}, {Format: store.FormatAnthropicMessages, BaseURL: "https://a.example/v1"}}}
	if got := Candidates([]store.Channel{c}, models(b...), "public", store.FormatOpenAIResponses); len(got) != 1 || got[0].Endpoint.Format != store.FormatOpenAIChatCompletions {
		t.Fatalf("responses should use the default convertible endpoint: %+v", got)
	}
	declared := c
	declared.Endpoints = append(append([]store.Endpoint{}, c.Endpoints...), store.Endpoint{Format: store.FormatOpenAIResponses, BaseURL: "https://a.example/v1"})
	got := Candidates([]store.Channel{declared}, models(b...), "public", store.FormatOpenAIResponses)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatOpenAIResponses {
		t.Fatalf("declared responses endpoint not chosen: %+v", got)
	}
}

// T020: the channel's explicit preference is consulted before the fixed default
// order, so an owner can keep the portable default and still pin a channel to
// the endpoint it actually prefers.
func TestPreferredEndpointBeatsDefaultOrder(t *testing.T) {
	b := []store.Binding{{ChannelID: 1, Model: "m", Priority: 0, Enabled: true}}
	both := store.Channel{ID: 1, Name: "two", Enabled: true, Models: []string{"m"},
		Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: "https://a.example/v1"}, {Format: store.FormatGeminiGenerateContent, BaseURL: "https://a.example"}}}
	// The default order is Chat > Messages > Gemini, so messages wins unset.
	got := Candidates([]store.Channel{both}, models(b...), "public", chatFormat)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatAnthropicMessages {
		t.Fatalf("default order should pick messages: %+v", got)
	}
	pref := both
	pref.PreferredEndpoint = store.FormatGeminiGenerateContent
	got = Candidates([]store.Channel{pref}, models(b...), "public", chatFormat)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatGeminiGenerateContent {
		t.Fatalf("preferred endpoint should win: %+v", got)
	}
}

// T020: a preference that cannot serve the inbound format is not a preference
// at all: an unknown format or an undeclared endpoint cannot serve the request,
// so the fixed default order still applies.
func TestUnusablePreferredEndpointIsIgnored(t *testing.T) {
	b := []store.Binding{{ChannelID: 1, Model: "m", Priority: 0, Enabled: true}}
	c := store.Channel{ID: 1, Name: "p", Enabled: true, Models: []string{"m"},
		Endpoints:         []store.Endpoint{{Format: "unknown_format", BaseURL: "https://a.example/v1"}, {Format: store.FormatAnthropicMessages, BaseURL: "https://a.example/v1"}},
		PreferredEndpoint: "unknown_format"}
	got := Candidates([]store.Channel{c}, models(b...), "public", chatFormat)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatAnthropicMessages {
		t.Fatalf("unknown formats must not be a preferred fallback: %+v", got)
	}
	stale := store.Channel{ID: 1, Name: "s", Enabled: true, Models: []string{"m"},
		Endpoints:         []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: "https://a.example/v1"}},
		PreferredEndpoint: store.FormatGeminiGenerateContent}
	got = Candidates([]store.Channel{stale}, models(b...), "public", chatFormat)
	if len(got) != 1 || got[0].Endpoint.Format != store.FormatAnthropicMessages {
		t.Fatalf("undeclared preference must be ignored: %+v", got)
	}
}
func TestDownstreamEnabledSorted(t *testing.T) {
	rows := []store.DownstreamModel{{ID: "z", Enabled: true}, {ID: "a", Enabled: false}, {ID: "m", Enabled: true}}
	got := DownstreamEnabled(rows)
	if len(got) != 2 || got[0] != "m" || got[1] != "z" {
		t.Fatalf("got %v", got)
	}
}

func TestFourFormatsEndpointSelection(t *testing.T) {
	formats := []string{store.FormatOpenAIChatCompletions, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent, store.FormatOpenAIResponses}
	for _, from := range formats {
		for _, to := range formats {
			t.Run(from+"_to_"+to, func(t *testing.T) {
				c := store.Channel{ID: 1, Enabled: true, Models: []string{"m"}, Endpoints: []store.Endpoint{{Format: to, BaseURL: "https://upstream.example.invalid"}}}
				got := Candidates([]store.Channel{c}, models(store.Binding{ChannelID: 1, Model: "m", Enabled: true}), "public", from)
				if len(got) != 1 || got[0].Endpoint.Format != to {
					t.Fatalf("pairwise conversion candidate missing: %+v", got)
				}
			})
		}
		c := store.Channel{PreferredEndpoint: store.FormatOpenAIResponses}
		for _, format := range formats {
			c.Endpoints = append(c.Endpoints, store.Endpoint{Format: format, BaseURL: "https://upstream.example.invalid"})
		}
		if ep, ok := PickEndpoint(c, from); !ok || ep.Format != from {
			t.Fatalf("native format must beat preference for %s: %+v %v", from, ep, ok)
		}
		// Every known format can also be an explicit cross-format preference.
		for _, preferred := range formats {
			if preferred == from {
				continue
			}
			withoutNative := store.Channel{PreferredEndpoint: preferred}
			for _, ep := range c.Endpoints {
				if ep.Format != from {
					withoutNative.Endpoints = append(withoutNative.Endpoints, ep)
				}
			}
			if ep, ok := PickEndpoint(withoutNative, from); !ok || ep.Format != preferred {
				t.Fatalf("preference %s for %s lost: %+v %v", preferred, from, ep, ok)
			}
		}
	}
}

func TestPortableEndpointOrderIncludesResponsesLast(t *testing.T) {
	order := []string{store.FormatOpenAIChatCompletions, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent, store.FormatOpenAIResponses}
	for _, inbound := range order {
		var available []string
		for _, format := range order {
			if format != inbound {
				available = append(available, format)
			}
		}
		for start := range available {
			c := store.Channel{}
			for i := len(available) - 1; i >= start; i-- {
				c.Endpoints = append(c.Endpoints, store.Endpoint{Format: available[i], BaseURL: "https://upstream.example.invalid"})
			}
			if ep, ok := PickEndpoint(c, inbound); !ok || ep.Format != available[start] {
				t.Fatalf("default for %s with %v: %+v %v", inbound, c.Endpoints, ep, ok)
			}
		}
	}
}

func TestUnknownFormatsNeverGainConversionEligibility(t *testing.T) {
	for _, inbound := range []string{"unknown_format", ""} {
		for _, endpoint := range []string{store.FormatOpenAIChatCompletions, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent, store.FormatOpenAIResponses, "unknown_format", ""} {
			c := store.Channel{PreferredEndpoint: endpoint, Endpoints: []store.Endpoint{{Format: endpoint, BaseURL: "https://upstream.example.invalid"}}}
			if ep, ok := PickEndpoint(c, inbound); ok {
				t.Fatalf("unknown inbound %q admitted via %+v", inbound, ep)
			}
			if CanConvert(inbound, endpoint) || CanConvert(endpoint, inbound) {
				t.Fatalf("unknown format declared convertible: %q %q", inbound, endpoint)
			}
		}
	}
}
