package store

import (
	"errors"
	"torouter/internal/compatprofile"
)

// BuiltinChannel creates the fixed provider configuration without acquiring credentials.
func BuiltinChannel(name, credentialType string) (Channel, error) {
	c := Channel{Name: name, CredentialType: credentialType, Enabled: true, Credentials: []CredentialItem{}}
	add := func(format, base string) { c.Endpoints = append(c.Endpoints, Endpoint{Format: format, BaseURL: base}) }
	switch credentialType {
	case CredentialCodexOAuth:
		add(FormatOpenAIResponses, "https://chatgpt.com/backend-api/codex")
	case CredentialChatGPTOAuth:
		add(FormatOpenAIResponses, "https://api.openai.com/v1")
	case CredentialCopilotOAuth:
		add(FormatOpenAIChatCompletions, "https://api.githubcopilot.com")
		add(FormatAnthropicMessages, "https://api.githubcopilot.com")
	case CredentialClaudeOAuth:
		add(FormatAnthropicMessages, "https://api.anthropic.com")
	case CredentialAntigravityOAuth:
		add(FormatGeminiGenerateContent, "https://daily-cloudcode-pa.googleapis.com")
	case CredentialDevinOAuth:
		add(FormatOpenAIResponses, "https://server.codeium.com")
	case CredentialZenFree:
		add(FormatOpenAIChatCompletions, "https://opencode.ai/inference/v1")
		add(FormatOpenAIResponses, "https://opencode.ai/inference/v1")
		add(FormatAnthropicMessages, "https://opencode.ai/inference/v1")
		c.ModelFormats = map[string][]string{}
		c.Credentials = []CredentialItem{{ID: "cred_public", Label: "公共凭证", APIKey: "public", Enabled: true}}
	default:
		p, ok := compatprofile.Lookup(credentialType)
		if !ok {
			return Channel{}, errors.New("不支持的内置接入类型")
		}
		add(p.Format(), p.API)
	}
	return c, nil
}
