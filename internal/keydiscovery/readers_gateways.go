package keydiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"torouter/internal/store"
)

// These queries deliberately exclude account, token, API-client and login tables.
const (
	gatewayCCProvidersSQL      = "SELECT id,app_type,name,settings_config,meta FROM providers"
	gatewayCCProxySQL          = "SELECT app_type,listen_address,listen_port FROM proxy_config"
	gatewayManagerProvidersSQL = "SELECT a.id,a.supplier_name,a.provider_type,a.url,a.auth_type,a.auth_params_json,a.action,a.status,s.secret_value FROM aggregate_apis a JOIN aggregate_api_secrets s ON s.aggregate_api_id=a.id WHERE lower(replace(trim(a.auth_type),'-','_')) IN ('apikey','api_key','key')"
	gatewayManagerAddressSQL   = "SELECT value FROM app_settings WHERE key='app.service_addr'"
)

type gatewayOrigin struct{ host, port string }

func gatewayObject(raw string) (map[string]any, bool) {
	var value map[string]any
	err := json.Unmarshal([]byte(raw), &value)
	return value, err == nil && value != nil
}

func gatewayMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func gatewayString(v any) string      { s, _ := v.(string); return strings.TrimSpace(s) }

func gatewayDiagnostic(ds *[]Diagnostic, code string) {
	for _, d := range *ds {
		if d.Code == code {
			return
		}
	}
	*ds = append(*ds, diagnostic(code))
}

func gatewayFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrMissing) {
		return ErrMissing
	}
	return ErrInvalid
}

// No reference expansion, decryption, credential refresh or environment lookup.
func gatewayKeyCode(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "incomplete"
	}
	lower := strings.ToLower(key)
	for _, prefix := range []string{"enc:", "encrypted:", "{env:", "{file:", "${", "$env:", "env:", "file:"} {
		if strings.HasPrefix(lower, prefix) {
			return "reference_skipped"
		}
	}
	if strings.HasPrefix(key, "{") || strings.HasPrefix(key, "[") || strings.HasPrefix(lower, "ya29.") {
		return "oauth_skipped"
	}
	for _, placeholder := range []string{"proxy_managed", "copilot_placeholder", "codex_oauth_placeholder", "xai_oauth_placeholder"} {
		if lower == placeholder {
			return "proxy_skipped"
		}
	}
	if strings.ContainsAny(key, "\r\n\x00") {
		return "invalid_format"
	}
	return ""
}

func gatewayLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || host == "0.0.0.0" || host == "::" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

func gatewayURL(raw string, origins []gatewayOrigin) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "incomplete"
	}
	if strings.Contains(raw, "${") || strings.Contains(raw, "{env:") {
		return "", "reference_skipped"
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return "", "invalid_format"
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(u.Path, "\\\x00\r\n") {
		return "", "unsupported"
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", "invalid_format"
	}
	port = strconv.Itoa(p)
	for _, origin := range origins {
		if origin.port == port && (strings.EqualFold(origin.host, u.Hostname()) || gatewayLoopback(origin.host) && gatewayLoopback(u.Hostname())) {
			return "", "proxy_skipped"
		}
	}
	return strings.TrimRight(raw, "/"), ""
}

func gatewayFormat(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "chat", "chat_completions", "chat-completions", "openai_chat", "openai-chat", "openai_chat_completions":
		return store.FormatOpenAIChatCompletions
	case "responses", "openai_responses":
		return store.FormatOpenAIResponses
	case "anthropic", "anthropic_messages", "anthropic-messages", "claude", "messages":
		return store.FormatAnthropicMessages
	case "gemini", "gemini_generate_content":
		return store.FormatGeminiGenerateContent
	}
	return ""
}

func gatewayPresent(v any) bool {
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case map[string]any:
		return len(x) != 0
	case []any:
		return len(x) != 0
	case bool:
		return x
	}
	return true
}

func gatewayExtraConditions(m map[string]any) bool {
	for k, v := range m {
		switch strings.ToLower(strings.ReplaceAll(k, "_", "")) {
		case "headers", "httpheaders", "envhttpheaders", "query", "queryparams", "queryparameters", "custompath", "path", "extrabody", "body", "customuseragent", "localproxyrequestoverrides", "authheader", "insecuretls", "envkey", "bearertoken", "experimentalbearertoken":
			if gatewayPresent(v) {
				return true
			}
		}
		if child := gatewayMap(v); child != nil && gatewayExtraConditions(child) {
			return true
		}
		if list, ok := v.([]any); ok {
			for _, child := range list {
				if gatewayExtraConditions(gatewayMap(child)) {
					return true
				}
			}
		}
	}
	return false
}

func gatewayStringFields(m map[string]any, fields ...string) bool {
	for _, field := range fields {
		if v, exists := m[field]; exists && v != nil {
			if _, ok := v.(string); !ok {
				return false
			}
		}
	}
	return true
}

func gatewayHasOAuth(m map[string]any) bool {
	for k, v := range m {
		key := strings.ToLower(k)
		if strings.Contains(key, "oauth") || key == "tokens" || key == "access_token" || key == "refresh_token" || key == "id_token" {
			if gatewayPresent(v) {
				return true
			}
		}
		if child := gatewayMap(v); child != nil && gatewayHasOAuth(child) {
			return true
		}
	}
	return false
}

func gatewayAppend(out *[]Candidate, ds *[]Diagnostic, key, format, rawURL string, off bool, origins []gatewayOrigin) {
	if code := gatewayKeyCode(key); code != "" {
		gatewayDiagnostic(ds, code)
		return
	}
	if format == "" {
		gatewayDiagnostic(ds, "unsupported")
		return
	}
	base, code := gatewayURL(rawURL, origins)
	if code != "" {
		gatewayDiagnostic(ds, code)
		return
	}
	// A configured full operation URL would otherwise be appended twice.
	u, _ := url.Parse(base)
	for _, suffix := range []string{"/chat/completions", "/responses", "/messages"} {
		if strings.HasSuffix(u.Path, suffix) {
			gatewayDiagnostic(ds, "unsupported")
			return
		}
	}
	*out = append(*out, Candidate{Key: strings.TrimSpace(key), Endpoints: []store.Endpoint{{Format: format, BaseURL: base}}, Disabled: off})
}

func readCCSwitch(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	rows, err := r.Query(ctx, "cc-switch.db", gatewayCCProvidersSQL)
	if err != nil {
		return nil, []Diagnostic{diagnostic("invalid_format")}, gatewayFailure(ctx, err)
	}
	proxyRows, err := r.Query(ctx, "cc-switch.db", gatewayCCProxySQL)
	if err != nil {
		return nil, []Diagnostic{diagnostic("invalid_format")}, gatewayFailure(ctx, err)
	}
	origins := []gatewayOrigin{{"127.0.0.1", "15721"}}
	for _, row := range proxyRows {
		if len(row) != 3 {
			return nil, []Diagnostic{diagnostic("invalid_format")}, ErrInvalid
		}
		host := strings.TrimSpace(row[1])
		if host == "" {
			host = "127.0.0.1"
		}
		p, e := strconv.Atoi(row[2])
		if e != nil || p < 1 || p > 65535 {
			return nil, []Diagnostic{diagnostic("invalid_format")}, ErrInvalid
		}
		origins = append(origins, gatewayOrigin{host, strconv.Itoa(p)})
	}
	var out []Candidate
	var ds []Diagnostic
	for _, row := range rows {
		if len(row) != 5 {
			gatewayDiagnostic(&ds, "invalid_format")
			continue
		}
		meta := map[string]any{}
		if strings.TrimSpace(row[4]) != "" && strings.TrimSpace(row[4]) != "null" {
			var ok bool
			meta, ok = gatewayObject(row[4])
			if !ok {
				gatewayDiagnostic(&ds, "invalid_format")
				continue
			}
		}
		binding, bound := meta["authBinding"]
		if bound && binding != nil {
			b := gatewayMap(binding)
			if b == nil {
				gatewayDiagnostic(&ds, "invalid_format")
				continue
			}
			if source, present := b["source"]; present && gatewayString(source) != "provider_config" {
				gatewayDiagnostic(&ds, "oauth_skipped")
				continue
			}
		}
		switch gatewayString(meta["providerType"]) {
		case "codex_oauth", "xai_oauth", "github_copilot":
			gatewayDiagnostic(&ds, "oauth_skipped")
			continue
		}
		if gatewayPresent(meta["githubAccountId"]) {
			gatewayDiagnostic(&ds, "oauth_skipped")
			continue
		}
		if !gatewayStringFields(meta, "providerType", "apiFormat", "apiKeyField") {
			gatewayDiagnostic(&ds, "invalid_format")
			continue
		}
		settings, ok := gatewayObject(row[3])
		if !ok {
			gatewayDiagnostic(&ds, "invalid_format")
			continue
		}
		if gatewayExtraConditions(meta) || gatewayExtraConditions(settings) {
			gatewayDiagnostic(&ds, "unsupported")
			continue
		}
		var key, base, format, code string
		env := gatewayMap(settings["env"])
		if settings["env"] != nil && env == nil {
			gatewayDiagnostic(&ds, "invalid_format")
			continue
		}
		if !gatewayStringFields(env, "ANTHROPIC_BASE_URL", "GOOGLE_GEMINI_BASE_URL") {
			gatewayDiagnostic(&ds, "invalid_format")
			continue
		}
		switch strings.ToLower(row[1]) {
		case "claude":
			key = gatewayString(env["ANTHROPIC_API_KEY"])
			if gatewayPresent(env["ANTHROPIC_AUTH_TOKEN"]) || gatewayPresent(env["ANTHROPIC_CUSTOM_HEADERS"]) {
				gatewayDiagnostic(&ds, "unsupported")
				continue
			}
			base = gatewayString(env["ANTHROPIC_BASE_URL"])
			if base == "" {
				base = "https://api.anthropic.com"
			}
			format = store.FormatAnthropicMessages
			if f := gatewayString(meta["apiFormat"]); f != "" {
				format = gatewayFormat(f)
			}
		case "codex":
			auth := gatewayMap(settings["auth"])
			if settings["auth"] != nil && auth == nil {
				gatewayDiagnostic(&ds, "invalid_format")
				continue
			}
			if gatewayHasOAuth(auth) {
				gatewayDiagnostic(&ds, "oauth_skipped")
				continue
			}
			key = gatewayString(auth["OPENAI_API_KEY"])
			if key == "" {
				key = gatewayString(env["OPENAI_API_KEY"])
			}
			base, format, code = gatewayCodexConfig(gatewayString(settings["config"]))
		case "gemini":
			key = gatewayString(env["GEMINI_API_KEY"])
			base = gatewayString(env["GOOGLE_GEMINI_BASE_URL"])
			if base == "" {
				base = "https://generativelanguage.googleapis.com"
			}
			format = store.FormatGeminiGenerateContent
		case "opencode":
			options := gatewayMap(settings["options"])
			if gatewayExtraConditions(options) {
				gatewayDiagnostic(&ds, "unsupported")
				continue
			}
			key, base = gatewayString(options["apiKey"]), gatewayString(options["baseURL"])
			switch gatewayString(settings["npm"]) {
			case "@ai-sdk/anthropic":
				format = store.FormatAnthropicMessages
			case "@ai-sdk/openai-compatible":
				format = store.FormatOpenAIChatCompletions
			}
			// The AI SDK appends its operation directly to an explicit baseURL;
			// this store appends /v1 first. Only the equivalent base is supported.
			if u, err := url.Parse(base); err == nil && !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
				code = "unsupported"
			}
		default:
			gatewayDiagnostic(&ds, "unsupported")
			continue
		}
		if code != "" {
			gatewayDiagnostic(&ds, code)
			continue
		}
		gatewayAppend(&out, &ds, key, format, base, false, origins)
	}
	return out, ds, nil
}

func gatewayCodexConfig(raw string) (string, string, string) {
	if raw == "" {
		return "", "", "incomplete"
	}
	var config map[string]any
	if toml.Unmarshal([]byte(raw), &config) != nil {
		return "", "", "invalid_format"
	}
	providers := gatewayMap(config["model_providers"])
	delete(config, "model_providers") // Inactive providers do not affect this key's endpoint.
	if gatewayExtraConditions(config) {
		return "", "", "unsupported"
	}
	active := gatewayString(config["model_provider"])
	provider := gatewayMap(providers[active])
	if active == "" || provider == nil {
		return "", "", "incomplete"
	}
	if gatewayExtraConditions(provider) {
		return "", "", "unsupported"
	}
	format := gatewayFormat(gatewayString(provider["wire_api"]))
	if format != store.FormatOpenAIChatCompletions && format != store.FormatOpenAIResponses {
		return "", "", "unsupported"
	}
	base := gatewayString(provider["base_url"])
	if u, err := url.Parse(base); err == nil && u.Path != "" && u.Path != "/" && !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
		return "", "", "unsupported"
	}
	return base, format, ""
}

func readCodexManager(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	rows, err := r.Query(ctx, "codexmanager.db", gatewayManagerProvidersSQL)
	if err != nil {
		return nil, []Diagnostic{diagnostic("invalid_format")}, gatewayFailure(ctx, err)
	}
	addresses, err := r.Query(ctx, "codexmanager.db", gatewayManagerAddressSQL)
	if err != nil {
		return nil, []Diagnostic{diagnostic("invalid_format")}, gatewayFailure(ctx, err)
	}
	origins := []gatewayOrigin{{"localhost", "48760"}, {"localhost", "48761"}}
	for _, row := range addresses {
		if len(row) != 1 {
			return nil, []Diagnostic{diagnostic("invalid_format")}, ErrInvalid
		}
		address := strings.TrimSpace(row[0])
		if address == "" {
			continue
		}
		if !strings.Contains(address, "://") {
			address = "http://" + address
		}
		u, e := url.Parse(address)
		if e != nil || u.Hostname() == "" || u.Port() == "" {
			return nil, []Diagnostic{diagnostic("invalid_format")}, ErrInvalid
		}
		origins = append(origins, gatewayOrigin{u.Hostname(), u.Port()})
	}
	var out []Candidate
	var ds []Diagnostic
	for _, row := range rows {
		if len(row) != 9 {
			gatewayDiagnostic(&ds, "invalid_format")
			continue
		}
		auth := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(row[4]), "-", "_"))
		if auth != "apikey" && auth != "api_key" && auth != "key" {
			gatewayDiagnostic(&ds, "unsupported")
			continue
		}
		var format string
		switch strings.ToLower(row[2]) {
		case "claude":
			format = store.FormatAnthropicMessages
		case "gemini":
			format = store.FormatGeminiGenerateContent
		case "codex":
			switch strings.TrimSpace(row[6]) {
			case "/responses", "responses", "/v1/responses", "v1/responses":
				format = store.FormatOpenAIResponses
			case "/chat/completions", "chat/completions", "/v1/chat/completions", "v1/chat/completions":
				format = store.FormatOpenAIChatCompletions
			}
		}
		if format == "" || !gatewayManagerAuth(row[5], format) {
			gatewayDiagnostic(&ds, "unsupported")
			continue
		}
		base, code := gatewayManagerBase(row[3], row[6], format)
		if code != "" {
			gatewayDiagnostic(&ds, code)
			continue
		}
		off := false
		switch strings.ToLower(strings.TrimSpace(row[7])) {
		case "active", "enabled", "enable":
		case "disabled", "disable", "inactive":
			off = true
		default:
			gatewayDiagnostic(&ds, "unsupported")
			continue
		}
		gatewayAppend(&out, &ds, row[8], format, base, off, origins)
	}
	return out, ds, nil
}

func gatewayManagerAuth(raw, format string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	m, ok := gatewayObject(raw)
	if !ok {
		return false
	}
	for k := range m {
		if k != "location" && k != "name" && k != "headerValueFormat" {
			return false
		}
	}
	if !strings.EqualFold(gatewayString(m["location"]), "header") {
		return false
	}
	name, mode := strings.ToLower(gatewayString(m["name"])), strings.ToLower(gatewayString(m["headerValueFormat"]))
	if mode == "" {
		mode = "bearer"
	}
	switch format {
	case store.FormatAnthropicMessages:
		return name == "x-api-key" && mode == "raw"
	case store.FormatGeminiGenerateContent:
		return name == "x-goog-api-key" && mode == "raw"
	default:
		return name == "authorization" && mode == "bearer"
	}
}

func gatewayManagerBase(raw, action, format string) (string, string) {
	base, code := gatewayURL(raw, nil)
	if code != "" {
		return "", code
	}
	action = strings.TrimSpace(action)
	if action == "" {
		if format == store.FormatOpenAIChatCompletions || format == store.FormatOpenAIResponses {
			return "", "unsupported"
		}
		return base, ""
	}
	if format == store.FormatGeminiGenerateContent {
		return "", "unsupported"
	} // model-specific/custom actions cannot be represented.
	operation := "/messages"
	if format == store.FormatOpenAIResponses {
		operation = "/responses"
	}
	if format == store.FormatOpenAIChatCompletions {
		operation = "/chat/completions"
	}
	action = "/" + strings.TrimLeft(action, "/")
	if action != operation && action != "/v1"+operation {
		return "", "unsupported"
	}
	u, _ := url.Parse(base)
	if !strings.HasSuffix(u.Path, "/v1") {
		if action != "/v1"+operation {
			return "", "unsupported"
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/v1"
		u.RawPath = ""
	}
	return strings.TrimRight(u.String(), "/"), ""
}

func readMagpie(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	data, err := r.Read(ctx, "providers.json")
	if err != nil {
		return nil, []Diagnostic{diagnostic("not_found")}, gatewayFailure(ctx, err)
	}
	root, ok := gatewayObject(string(data))
	if !ok {
		return nil, []Diagnostic{diagnostic("invalid_format")}, ErrInvalid
	}
	providers, ok := root["providers"].([]any)
	if !ok {
		return nil, []Diagnostic{diagnostic("invalid_format")}, ErrInvalid
	}
	origins := []gatewayOrigin{{"localhost", "3425"}}
	settings, err := r.Read(ctx, "settings.json")
	if err == nil {
		var config struct {
			Port int `json:"port"`
		}
		if json.Unmarshal(settings, &config) != nil || config.Port < 0 || config.Port > 65535 {
			return nil, []Diagnostic{diagnostic("invalid_format")}, ErrInvalid
		}
		if config.Port != 0 {
			origins = append(origins, gatewayOrigin{"localhost", strconv.Itoa(config.Port)})
		}
	} else if !errors.Is(err, ErrMissing) {
		return nil, []Diagnostic{diagnostic("invalid_format")}, gatewayFailure(ctx, err)
	}
	var out []Candidate
	var ds []Diagnostic
	for _, value := range providers {
		p := gatewayMap(value)
		if p == nil {
			gatewayDiagnostic(&ds, "invalid_format")
			continue
		}
		if gatewayString(p["preset"]) == "remote-magpie" {
			gatewayDiagnostic(&ds, "proxy_skipped")
			continue
		}
		if gatewayPresent(p["accessKeyID"]) || gatewayPresent(p["secretAccessKey"]) || gatewayHasOAuth(p) {
			gatewayDiagnostic(&ds, "oauth_skipped")
			continue
		}
		if gatewayExtraConditions(p) {
			gatewayDiagnostic(&ds, "unsupported")
			continue
		}
		providerOff, valid := p["off"].(bool)
		if p["off"] != nil && !valid {
			gatewayDiagnostic(&ds, "invalid_format")
			continue
		}
		keys := []map[string]any{{"key": p["key"], "protocol": p["keyProtocol"]}}
		if other, exists := p["keys"]; exists && other != nil {
			list, valid := other.([]any)
			if !valid {
				gatewayDiagnostic(&ds, "invalid_format")
				continue
			}
			for _, value := range list {
				k := gatewayMap(value)
				if k == nil {
					gatewayDiagnostic(&ds, "invalid_format")
					continue
				}
				keys = append(keys, k)
			}
		}
		for _, k := range keys {
			key, proto := gatewayString(k["key"]), gatewayString(k["protocol"])
			if key == "" {
				continue
			}
			if proto != "" && proto != "chat" && proto != "responses" && proto != "anthropic" && proto != "gemini" {
				gatewayDiagnostic(&ds, "unsupported")
				continue
			}
			off, valid := k["off"].(bool)
			if k["off"] != nil && !valid {
				gatewayDiagnostic(&ds, "invalid_format")
				continue
			}
			off = off || providerOff
			var endpoints []store.Endpoint
			for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
				if proto != "" && proto != protocol {
					continue
				}
				base := gatewayString(p[protocol])
				if base == "" {
					continue
				}
				var single []Candidate
				gatewayAppend(&single, &ds, key, gatewayFormat(protocol), base, off, origins)
				if len(single) != 0 {
					endpoints = append(endpoints, single[0].Endpoints...)
				}
			}
			if len(endpoints) != 0 {
				out = append(out, Candidate{Key: key, Endpoints: endpoints, Disabled: off})
			} else {
				gatewayDiagnostic(&ds, "incomplete")
			}
		}
	}
	return out, ds, nil
}
