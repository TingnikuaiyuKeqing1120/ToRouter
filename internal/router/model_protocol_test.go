package router

import (
	"testing"
	"torouter/internal/store"
)

func TestManagedModelProtocolsConstrainAllClientFormats(t *testing.T) {
	c := channels()[0]
	c.CredentialType = store.CredentialZenFree
	c.Endpoints = append(c.Endpoints, store.Endpoint{Format: store.FormatOpenAIResponses, BaseURL: "https://example.test"})
	c.Models = []string{"chat", "response", "unknown"}
	c.ModelFormats = map[string][]string{"chat": {store.FormatOpenAIChatCompletions}, "response": {store.FormatOpenAIResponses}}
	models := []store.DownstreamModel{{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "response", Enabled: true}}}}
	for _, f := range convertibleFormats {
		attempts := Candidates([]store.Channel{c}, models, "public", f)
		if len(attempts) != 1 || attempts[0].Endpoint.Format != store.FormatOpenAIResponses {
			t.Fatalf("format %s: %#v", f, attempts)
		}
	}
	if _, ok := PickModelEndpoint(c, "unknown", store.FormatOpenAIChatCompletions); ok {
		t.Fatal("unknown model inherited channel protocol")
	}
	c.ModelFormats = map[string][]string{}
	if BindingStructureReason(c, models[0].Bindings[0]) != "没有可用端点" {
		t.Fatal("empty managed map became unrestricted")
	}
	c.CredentialType = ""
	c.ModelFormats = nil
	if ep, ok := PickModelEndpoint(c, "response", store.FormatOpenAIChatCompletions); !ok || ep.Format != store.FormatOpenAIChatCompletions {
		t.Fatal("ordinary channels changed")
	}

	// For ordinary API key channels, unlisted models inherit channel endpoints,
	// while declared overrides narrow the specific model.
	c.CredentialType = store.CredentialAPIKey
	c.ModelFormats = map[string][]string{
		"response": {store.FormatOpenAIResponses},
		"empty":    {},
	}
	if ep, ok := PickModelEndpoint(c, "unlisted", store.FormatOpenAIChatCompletions); !ok || ep.Format != store.FormatOpenAIChatCompletions {
		t.Fatal("unlisted model in API key channel did not inherit channel endpoint")
	}
	if ep, ok := PickModelEndpoint(c, "response", store.FormatOpenAIChatCompletions); !ok || ep.Format != store.FormatOpenAIResponses {
		t.Fatalf("overridden model was not constrained to its declared protocol, got format %s", ep.Format)
	}
	if ep, ok := PickModelEndpoint(c, "response", store.FormatOpenAIResponses); !ok || ep.Format != store.FormatOpenAIResponses {
		t.Fatal("overridden model could not route its declared protocol")
	}
	if _, ok := PickModelEndpoint(c, "empty", store.FormatOpenAIChatCompletions); ok {
		t.Fatal("explicitly empty format list was routable")
	}

	// For catalogue providers (ZenFree, CopilotOAuth), nil ModelFormats must NOT route unlisted models.
	c.CredentialType = store.CredentialZenFree
	c.ModelFormats = nil
	if _, ok := PickModelEndpoint(c, "unlisted", store.FormatOpenAIChatCompletions); ok {
		t.Fatal("unlisted model in nil ZenFree provider was routable")
	}
	c.CredentialType = store.CredentialCopilotOAuth
	c.ModelFormats = nil
	if _, ok := PickModelEndpoint(c, "unlisted", store.FormatOpenAIChatCompletions); ok {
		t.Fatal("unlisted model in nil Copilot provider was routable")
	}
}
