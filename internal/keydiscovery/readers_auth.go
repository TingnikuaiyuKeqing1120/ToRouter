package keydiscovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"torouter/internal/store"
)

// OpenCode keeps credentials separately from its provider configuration.
func openCodeDataRoot() string {
	if root := os.Getenv("XDG_DATA_HOME"); root != "" {
		return filepath.Join(root, "opencode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode")
}

var openCodeAuthProviders = map[string]store.Endpoint{
	"openai":         {Format: store.FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"},
	"anthropic":      {Format: store.FormatAnthropicMessages, BaseURL: "https://api.anthropic.com"},
	"google":         {Format: store.FormatGeminiGenerateContent, BaseURL: "https://generativelanguage.googleapis.com"},
	"deepseek":       {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.deepseek.com/v1"},
	"xai":            {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.x.ai/v1"},
	"moonshotai":     {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.moonshot.ai/v1"},
	"moonshotai-cn":  {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.moonshot.cn/v1"},
	"mistral":        {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.mistral.ai/v1"},
	"groq":           {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.groq.com/openai/v1"},
	"openrouter":     {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://openrouter.ai/api/v1"},
	"togetherai":     {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.together.xyz/v1"},
	"fireworks-ai":   {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.fireworks.ai/inference/v1"},
	"siliconflow-cn": {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.siliconflow.cn/v1"},
	"nvidia":         {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://integrate.api.nvidia.com/v1"},
	"aihubmix":       {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://aihubmix.com/v1"},
	"302ai":          {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.302.ai/v1"},
	"ollama-cloud":   {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://ollama.com/v1"},
	"opencode":       {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://opencode.ai/zen/v1"},
}

func readOpenCodeAuth(ctx context.Context, r Resources, configured ...map[string][]store.Endpoint) ([]Candidate, []Diagnostic, error) {
	raw, exists, code, err := fileRead(ctx, r, "auth.json")
	if err != nil {
		return nil, nil, err
	}
	if code != "" {
		return fileProblem(code)
	}
	if !exists {
		return nil, nil, nil
	}
	entries, ok := fileJSON(raw)
	if !ok {
		return fileProblem("invalid_format")
	}
	var out []Candidate
	var ds []Diagnostic
	for _, id := range fileKeys(entries) {
		entry, valid := fileObject(entries[id])
		if !valid {
			ds = append(ds, diagnostic("invalid_format"))
			continue
		}
		if entry["type"] != "api" {
			ds = append(ds, diagnostic("oauth_skipped"))
			continue
		}
		endpoint, known := openCodeAuthProviders[id]
		endpoints := []store.Endpoint{}
		if known {
			endpoints = append(endpoints, endpoint)
		}
		if len(configured) > 0 {
			if override, present := configured[0][id]; present {
				endpoints = override
			}
		}
		if len(endpoints) == 0 {
			ds = append(ds, diagnostic("unsupported"))
			continue
		}
		for _, endpoint := range endpoints {
			candidate, why := fileCandidate(entry["key"], endpoint.BaseURL, endpoint.Format, false)
			if endpoint.Format != store.FormatGeminiGenerateContent {
				candidate, why = fileDirectPathCandidate(entry["key"], endpoint.BaseURL, endpoint.Format, false)
			}
			if why != "" {
				ds = append(ds, diagnostic(why))
				continue
			}
			out = append(out, candidate)
		}
	}
	return out, ds, nil
}

// ZCode's explicit configFolder is a configured data location, not a disk scan.
func zcodeConfigRoot(ctx context.Context, r Resources, root string) (string, error) {
	raw, exists, _, err := fileRead(ctx, r, "setting.json")
	if err != nil || !exists {
		return root, err
	}
	setting, ok := fileJSON(raw)
	if !ok {
		return root, nil
	}
	folder, ok := fileText(setting["configFolder"])
	if !ok || folder == "" {
		return root, nil
	}
	if strings.HasPrefix(folder, "~/") || strings.HasPrefix(folder, `~\`) {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", ErrInvalid
		}
		folder = filepath.Join(home, folder[2:])
	}
	if !filepath.IsAbs(folder) {
		folder = filepath.Join(root, folder)
	}
	if resourceUNC(folder) {
		return "", ErrInvalid
	}
	return filepath.Clean(folder), nil
}

// OpenCode auth.json records only provider IDs. An explicitly configured provider
// address takes precedence over its built-in vendor endpoint; unsupported custom
// conditions suppress the fallback rather than sending its key elsewhere.
func openCodeConfiguredEndpoints(ctx context.Context, r Resources) (map[string][]store.Endpoint, error) {
	out := map[string][]store.Endpoint{}
	for _, name := range []string{"opencode.jsonc", "opencode.json", "config.json"} {
		raw, exists, code, err := fileRead(ctx, r, name)
		if err != nil {
			return nil, err
		}
		if code != "" {
			return out, ErrInvalid
		}
		if !exists {
			continue
		}
		doc, ok := fileJSON(raw)
		if !ok {
			return out, ErrInvalid
		}
		providers, ok := fileObject(doc["provider"])
		if !ok {
			return out, nil
		}
		for id, value := range providers {
			provider, ok := fileObject(value)
			if !ok {
				out[id] = nil
				continue
			}
			options, ok := fileObject(provider["options"])
			if !ok {
				continue
			}
			if !fileHas(options, "baseURL") {
				continue
			}
			out[id] = nil
			if fileHas(provider, "headers", "httpHeaders", "http_headers", "env_http_headers", "query_params", "auth", "gateway_oauth") || fileExtra(options, "baseURL", "apiKey") {
				continue
			}
			endpoint, known := openCodeAuthProviders[id]
			switch provider["npm"] {
			case "@ai-sdk/anthropic":
				endpoint.Format = store.FormatAnthropicMessages
				known = true
			case "@ai-sdk/openai-compatible":
				endpoint.Format = store.FormatOpenAIChatCompletions
				known = true
			case "@ai-sdk/openai":
				endpoint.Format = store.FormatOpenAIResponses
				known = true
			case nil, "":
			default:
				known = false
			}
			if !known {
				continue
			}
			base, why := fileString(options["baseURL"])
			if why != "" {
				continue
			}
			endpoint.BaseURL = normalizeURL(base)
			if endpoint.BaseURL == "" {
				continue
			}
			out[id] = []store.Endpoint{endpoint}
		}
		return out, nil
	}
	return out, nil
}
