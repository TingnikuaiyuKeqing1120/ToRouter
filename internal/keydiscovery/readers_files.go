package keydiscovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tidwall/jsonc"
	"torouter/internal/oauthtransfer"
	"torouter/internal/store"
)

// These readers only use declared Resources. They do not expand references,
// execute source auth hooks, refresh tokens, or contact providers.
func fileObject(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }
func fileText(v any) (string, bool)           { s, ok := v.(string); return strings.TrimSpace(s), ok }
func filePresent(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(x) != ""
	case map[string]any:
		return len(x) != 0
	case []any:
		return len(x) != 0
	default:
		return true
	}
}
func fileHas(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if filePresent(m[k]) {
			return true
		}
	}
	return false
}
func fileReference(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Contains(s, "{env:") || strings.Contains(s, "${") || strings.Contains(s, "{file:") || strings.HasPrefix(s, "enc:")
}
func fileString(v any) (string, string) {
	s, ok := fileText(v)
	if !ok {
		if v == nil {
			return "", "incomplete"
		}
		if _, object := fileObject(v); object {
			return "", "reference_skipped"
		}
		return "", "invalid_format"
	}
	if s == "" {
		return "", "incomplete"
	}
	if fileReference(s) {
		return "", "reference_skipped"
	}
	return s, ""
}
func fileJSON(b []byte) (map[string]any, bool) {
	b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
	var value map[string]any
	if json.Unmarshal(jsonc.ToJSON(b), &value) != nil || value == nil {
		return nil, false
	}
	return value, true
}
func fileRead(ctx context.Context, r Resources, name string) ([]byte, bool, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, "", err
	}
	b, err := r.Read(ctx, name)
	if err == nil {
		return b, true, "", nil
	}
	if ctx.Err() != nil {
		return nil, false, "", ctx.Err()
	}
	if errors.Is(err, ErrMissing) {
		return nil, false, "", nil
	}
	return nil, true, "invalid_format", nil
}
func fileEnabled(m map[string]any) (bool, string) {
	v, exists := m["enabled"]
	if !exists {
		return false, ""
	}
	enabled, ok := v.(bool)
	if !ok {
		return false, "invalid_format"
	}
	return !enabled, ""
}
func fileCandidate(key, base any, format string, disabled bool) (Candidate, string) {
	k, code := fileString(key)
	if code != "" {
		return Candidate{}, code
	}
	b, code := fileString(base)
	if code != "" {
		return Candidate{}, code
	}
	u, err := url.Parse(b)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Candidate{}, "invalid_format"
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Candidate{}, "unsupported"
	}
	return Candidate{Key: k, Endpoints: []store.Endpoint{{Format: format, BaseURL: b}}, Disabled: disabled}, ""
}

// Codex and the AI SDK append /responses, /chat/completions or /messages
// directly. The gateway inserts /v1 unless the base already ends with it, so
// importing another base would silently change the effective request URL.
func fileDirectPathCandidate(key, base any, format string, disabled bool) (Candidate, string) {
	candidate, code := fileCandidate(key, base, format, disabled)
	if code != "" {
		return Candidate{}, code
	}
	u, _ := url.Parse(candidate.Endpoints[0].BaseURL)
	if u.RawPath != "" || strings.Contains(u.Path, `\`) {
		// The gateway rebuilds Path and clears RawPath; encoded separators can
		// therefore change meaning even if the decoded path ends with /v1.
		return Candidate{}, "unsupported"
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return Candidate{}, "unsupported"
		}
	}
	if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
		return candidate, ""
	}
	// @ai-sdk/anthropic explicitly normalizes its official bare origin to /v1.
	// This exception does not apply to arbitrary custom Anthropic origins.
	if format == store.FormatAnthropicMessages && strings.TrimRight(candidate.Endpoints[0].BaseURL, "/") == "https://api.anthropic.com" {
		return candidate, ""
	}
	return Candidate{}, "unsupported"
}
func fileResult(c Candidate, code string) ([]Candidate, []Diagnostic, error) {
	if code != "" {
		return nil, []Diagnostic{diagnostic(code)}, nil
	}
	return []Candidate{c}, nil, nil
}
func fileProblem(code string) ([]Candidate, []Diagnostic, error) {
	return nil, []Diagnostic{diagnostic(code)}, nil
}
func fileKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func fileExtra(m map[string]any, allowed ...string) bool {
	for k, v := range m {
		known := false
		for _, a := range allowed {
			if a == k {
				known = true
				break
			}
		}
		if !known && filePresent(v) {
			return true
		}
	}
	return false
}

func readCodex(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	b, exists, code, err := fileRead(ctx, r, "config.toml")
	if err != nil {
		return nil, nil, err
	}
	if code != "" {
		return fileProblem(code)
	}
	config := map[string]any{}
	if exists && toml.Unmarshal(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}), &config) != nil {
		return fileProblem("invalid_format")
	}
	if selected, ok := config["model_provider"]; ok {
		if _, valid := fileText(selected); !valid {
			return fileProblem("invalid_format")
		}
	}
	providers := map[string]any{}
	if raw, ok := config["model_providers"]; ok {
		var valid bool
		providers, valid = fileObject(raw)
		if !valid {
			return fileProblem("invalid_format")
		}
	}
	auth, authExists, authCode, err := fileRead(ctx, r, "auth.json")
	if err != nil {
		return nil, nil, err
	}
	authObject, _ := fileJSON(auth)
	authKey := authObject["OPENAI_API_KEY"]
	var out []Candidate
	var ds []Diagnostic
	for _, id := range fileKeys(providers) {
		provider, valid := fileObject(providers[id])
		if !valid {
			ds = append(ds, diagnostic("invalid_format"))
			continue
		}
		if fileHas(provider, "auth", "gateway_oauth", "aws", "http_headers", "env_http_headers", "query_params") {
			ds = append(ds, diagnostic("unsupported"))
			continue
		}
		if value, ok := provider["requires_openai_auth"]; ok {
			if _, valid := value.(bool); !valid {
				ds = append(ds, diagnostic("invalid_format"))
				continue
			}
		}
		format := store.FormatOpenAIResponses
		if wire, ok := provider["wire_api"]; ok {
			if wire == "chat" {
				format = store.FormatOpenAIChatCompletions
			} else if wire != "responses" {
				ds = append(ds, diagnostic("unsupported"))
				continue
			}
		}
		if fileHas(provider, "env_key") {
			ds = append(ds, diagnostic("reference_skipped"))
			continue
		}
		base := provider["base_url"]
		if !filePresent(base) && id == "openai" {
			base = "https://api.openai.com/v1"
		}
		key := provider["experimental_bearer_token"]
		if !filePresent(key) {
			requires, _ := provider["requires_openai_auth"].(bool)
			if id == "openai" || requires {
				key = authKey
			}
		}
		c, why := fileDirectPathCandidate(key, base, format, false)
		if why != "" {
			ds = append(ds, diagnostic(why))
			continue
		}
		out = append(out, c)
	}
	if len(providers) == 0 && authExists && filePresent(authKey) {
		c, why := fileDirectPathCandidate(authKey, "https://api.openai.com/v1", store.FormatOpenAIResponses, false)
		if why == "" {
			out = append(out, c)
		} else {
			ds = append(ds, diagnostic(why))
		}
	}
	if authCode != "" {
		ds = append(ds, diagnostic(authCode))
	} else if authExists && (fileHas(authObject, "tokens", "access_token", "refresh_token") || !filePresent(authKey)) {
		cs, notes, e := readNativeMaterial(ctx, store.CredentialCodexOAuth, auth)
		if e != nil {
			return nil, nil, e
		}
		out = append(out, cs...)
		ds = append(ds, notes...)
	}
	if !exists && !authExists {
		ds = append(ds, diagnostic("not_found"))
	}
	return out, ds, nil
}

func readNativeMaterial(ctx context.Context, kind string, raw []byte) ([]Candidate, []Diagnostic, error) {
	items, err := oauthtransfer.ParseNativeCredentialsForType(ctx, kind, raw)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, []Diagnostic{diagnostic("oauth_skipped")}, nil
	}
	out := []Candidate{}
	for _, item := range items {
		copy := item
		out = append(out, Candidate{OAuth: &copy, Endpoints: append([]store.Endpoint(nil), item.Endpoints...), Disabled: !item.Enabled})
	}
	return out, nil, nil
}

func readClaude(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	out, ds, err := readClaudeSettings(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	raw, exists, code, err := fileRead(ctx, r, ".credentials.json")
	if err != nil {
		return nil, nil, err
	}
	if code != "" {
		ds = append(ds, diagnostic(code))
	} else if exists {
		cs, notes, e := readNativeMaterial(ctx, store.CredentialClaudeOAuth, raw)
		if e != nil {
			return nil, nil, e
		}
		out = append(out, cs...)
		ds = append(ds, notes...)
	}
	return out, ds, nil
}

func readClaudeSettings(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	b, exists, code, err := fileRead(ctx, r, "settings.json")
	if err != nil {
		return nil, nil, err
	}
	if code != "" {
		return fileProblem(code)
	}
	if !exists {
		return fileProblem("not_found")
	}
	doc, ok := fileJSON(b)
	if !ok {
		return fileProblem("invalid_format")
	}
	if fileHas(doc, "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "forceLoginMethod", "headers", "httpHeaders", "http_headers") {
		return fileProblem("unsupported")
	}
	env, ok := fileObject(doc["env"])
	if !ok {
		if doc["env"] != nil {
			return fileProblem("invalid_format")
		}
		return fileProblem("incomplete")
	}
	if fileHas(env, "ANTHROPIC_AUTH_TOKEN") && !fileHas(env, "ANTHROPIC_BASE_URL") {
		return fileProblem("oauth_skipped")
	}
	for k, v := range env {
		// Model/UI preferences do not change the connection. Authentication,
		// routing and custom headers do, and cannot be silently dropped.
		if k != "ANTHROPIC_API_KEY" && k != "ANTHROPIC_AUTH_TOKEN" && k != "ANTHROPIC_BASE_URL" && filePresent(v) && (strings.Contains(k, "HEADER") || strings.Contains(k, "PROXY") || strings.HasPrefix(k, "AWS_") || strings.HasPrefix(k, "GOOGLE_") || strings.Contains(k, "USE_BEDROCK") || strings.Contains(k, "USE_VERTEX") || strings.Contains(k, "USE_FOUNDRY") || strings.Contains(k, "AUTH_TOKEN")) {
			return fileProblem("unsupported")
		}
	}
	base := env["ANTHROPIC_BASE_URL"]
	if !filePresent(base) {
		base = "https://api.anthropic.com"
	}
	key := env["ANTHROPIC_API_KEY"]
	if fileHas(env, "ANTHROPIC_AUTH_TOKEN") {
		key = env["ANTHROPIC_AUTH_TOKEN"]
	}
	c, why := fileCandidate(key, base, store.FormatAnthropicMessages, false)
	return fileResult(c, why)
}

func readOpenCode(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	for _, name := range []string{"opencode.jsonc", "opencode.json", "config.json"} {
		b, exists, code, err := fileRead(ctx, r, name)
		if err != nil {
			return nil, nil, err
		}
		if code != "" {
			return fileProblem(code)
		}
		if !exists {
			continue
		}
		doc, ok := fileJSON(b)
		if !ok {
			return fileProblem("invalid_format")
		}
		return fileReadProviders(doc, false)
	}
	return fileProblem("not_found")
}

func fileReadProviders(doc map[string]any, legacyZCode bool) ([]Candidate, []Diagnostic, error) {
	providers, ok := fileObject(doc["provider"])
	if !ok {
		if doc["provider"] != nil {
			return fileProblem("invalid_format")
		}
		return fileProblem("incomplete")
	}
	var out []Candidate
	var ds []Diagnostic
	for _, id := range fileKeys(providers) {
		p, valid := fileObject(providers[id])
		if !valid {
			ds = append(ds, diagnostic("invalid_format"))
			continue
		}
		if legacyZCode && p["source"] != "custom" {
			ds = append(ds, diagnostic("oauth_skipped"))
			continue
		}
		if fileHas(p, "headers", "httpHeaders", "http_headers", "env_http_headers", "query_params", "auth", "gateway_oauth") {
			ds = append(ds, diagnostic("unsupported"))
			continue
		}
		disabled, code := fileEnabled(p)
		if code != "" {
			ds = append(ds, diagnostic(code))
			continue
		}
		var format string
		if legacyZCode {
			switch p["kind"] {
			case "anthropic":
				format = store.FormatAnthropicMessages
			case "openai-compatible":
				format = store.FormatOpenAIChatCompletions
			case "openai":
				format = store.FormatOpenAIResponses
			}
		} else {
			switch p["npm"] {
			case "@ai-sdk/anthropic":
				format = store.FormatAnthropicMessages
			case "@ai-sdk/openai-compatible":
				format = store.FormatOpenAIChatCompletions
			}
		}
		if format == "" {
			ds = append(ds, diagnostic("unsupported"))
			continue
		}
		opts, valid := fileObject(p["options"])
		if !valid {
			ds = append(ds, diagnostic("incomplete"))
			continue
		}
		if fileExtra(opts, "baseURL", "apiKey") {
			ds = append(ds, diagnostic("unsupported"))
			continue
		}
		// AI SDK withoutTrailingSlash removes exactly one slash, whereas the
		// gateway trims all. A repeated trailing slash is not equivalent.
		if base, ok := fileText(opts["baseURL"]); !legacyZCode && ok && strings.HasSuffix(base, "//") {
			ds = append(ds, diagnostic("unsupported"))
			continue
		}
		var c Candidate
		if legacyZCode && format == store.FormatAnthropicMessages {
			// ZCode's Anthropic kind uses /v1/messages rather than AI SDK's
			// unversioned /messages operation; its custom prefix is meaningful.
			c, code = fileCandidate(opts["apiKey"], opts["baseURL"], format, disabled)
		} else {
			c, code = fileDirectPathCandidate(opts["apiKey"], opts["baseURL"], format, disabled)
		}
		if code != "" {
			ds = append(ds, diagnostic(code))
			continue
		}
		out = append(out, c)
	}
	if len(providers) == 0 {
		ds = append(ds, diagnostic("incomplete"))
	}
	return out, ds, nil
}

func readZCode(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	var notices []Diagnostic
	b, exists, code, err := fileRead(ctx, r, "setting.json")
	if err != nil {
		return nil, nil, err
	}
	if code != "" {
		return fileProblem(code)
	}
	if exists {
		setting, valid := fileJSON(b)
		if !valid {
			return fileProblem("invalid_format")
		}
		if value, ok := setting["dataBaseDir"]; ok && filePresent(value) {
			dir, valid := fileText(value)
			if !valid {
				return fileProblem("invalid_format")
			}
			if strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, `\`) || len(dir) > 2 && dir[1] == ':' && (dir[2] == '\\' || dir[2] == '/') {
				notices = append(notices, diagnostic("redirect_scope"))
			}
		}
	}
	b, exists, code, err = fileRead(ctx, r, "provider_config.json")
	if err != nil {
		return nil, nil, err
	}
	if code != "" {
		return nil, append(notices, diagnostic(code)), nil
	}
	if !exists {
		b, exists, code, err = fileRead(ctx, r, "config.json")
		if err != nil {
			return nil, nil, err
		}
		if code != "" {
			return nil, append(notices, diagnostic(code)), nil
		}
		if !exists {
			return nil, append(notices, diagnostic("not_found")), nil
		}
		doc, valid := fileJSON(b)
		if !valid {
			return nil, append(notices, diagnostic("invalid_format")), nil
		}
		cs, ds, err := fileReadProviders(doc, true)
		return cs, append(notices, ds...), err
	}
	doc, valid := fileJSON(b)
	if !valid {
		return nil, append(notices, diagnostic("invalid_format")), nil
	}
	config, ok1 := fileObject(doc["config"])
	ruleConfig, ok2 := fileObject(config["providerConfigRules"])
	rules, ok3 := ruleConfig["providerRules"].([]any)
	if !ok1 || !ok2 || !ok3 {
		return nil, append(notices, diagnostic("invalid_format")), nil
	}
	var out []Candidate
	for _, value := range rules {
		rule, valid := fileObject(value)
		if !valid {
			notices = append(notices, diagnostic("invalid_format"))
			continue
		}
		if fileHas(rule, "headers", "httpHeaders", "http_headers", "auth") {
			notices = append(notices, diagnostic("unsupported"))
			continue
		}
		disabled, code := fileEnabled(rule)
		if code != "" {
			notices = append(notices, diagnostic(code))
			continue
		}
		cfg, valid := fileObject(rule["config"])
		if !valid {
			notices = append(notices, diagnostic("invalid_format"))
			continue
		}
		access, ok1 := fileObject(cfg["access"])
		api, ok2 := fileObject(cfg["api"])
		if !ok1 || !ok2 {
			notices = append(notices, diagnostic("incomplete"))
			continue
		}
		if access["type"] != "api-key" {
			notices = append(notices, diagnostic("oauth_skipped"))
			continue
		}
		if fileExtra(access, "type", "apiKey") || fileExtra(api, "type", "baseUrl") || fileHas(cfg, "headers", "httpHeaders", "auth", "gateway_oauth", "options") {
			notices = append(notices, diagnostic("unsupported"))
			continue
		}
		format := ""
		switch api["type"] {
		case "anthropic-messages":
			format = store.FormatAnthropicMessages
		case "openai-chat-completions":
			format = store.FormatOpenAIChatCompletions
		case "openai-responses":
			format = store.FormatOpenAIResponses
		}
		if format == "" {
			notices = append(notices, diagnostic("unsupported"))
			continue
		}
		c, code := fileCandidate(access["apiKey"], api["baseUrl"], format, disabled)
		if code != "" {
			notices = append(notices, diagnostic(code))
			continue
		}
		out = append(out, c)
	}
	if len(rules) == 0 {
		notices = append(notices, diagnostic("incomplete"))
	}
	return out, notices, nil
}
