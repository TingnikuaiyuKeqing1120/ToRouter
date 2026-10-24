package keydiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"torouter/internal/store"
)

type fileFixtures struct {
	files    map[string]string
	failures map[string]error
	reads    []string
}

func (r *fileFixtures) Read(ctx context.Context, name string) ([]byte, error) {
	r.reads = append(r.reads, name)
	if err := r.failures[name]; err != nil {
		return nil, err
	}
	if s, ok := r.files[name]; ok {
		return []byte(s), nil
	}
	return nil, ErrMissing
}
func (r *fileFixtures) Query(context.Context, string, string) ([][]string, error) {
	panic("file readers must not query SQLite")
}

type fileReader func(context.Context, Resources) ([]Candidate, []Diagnostic, error)

func fixtureJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func hasFileCode(ds []Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}
func runFileReader(t *testing.T, reader fileReader, files map[string]string) ([]Candidate, []Diagnostic, *fileFixtures) {
	t.Helper()
	r := &fileFixtures{files: files}
	cs, ds, err := reader(context.Background(), r)
	if err != nil {
		t.Fatalf("reader failed: %v", err)
	}
	return cs, ds, r
}
func requireFileEndpoint(t *testing.T, cs []Candidate, key, base, format string) {
	t.Helper()
	if len(cs) != 1 || cs[0].Key != key || len(cs[0].Endpoints) != 1 || cs[0].Endpoints[0].BaseURL != base || cs[0].Endpoints[0].Format != format {
		t.Fatalf("unexpected synthetic candidates: %+v", cs)
	}
}

func TestReadCodexEnumeratesProvidersAndPreservesAuthBinding(t *testing.T) {
	const config = `model_provider = "selected.provider"
[model_providers."selected.provider"]
name = "Synthetic"
base_url = "https://custom.example.test/v1"
experimental_bearer_token = "synthetic-custom-key"
[model_providers.other]
base_url = "https://other.example.test/v1"
experimental_bearer_token = "synthetic-other-key"
`
	cs, ds, r := runFileReader(t, readCodex, map[string]string{"config.toml": config, "auth.json": `{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-openai-key"}`})
	if len(cs) != 2 {
		t.Fatal("Codex provider enumeration lost configured provider")
	}
	requireFileEndpoint(t, cs[1:], "synthetic-custom-key", "https://custom.example.test/v1", store.FormatOpenAIResponses)
	if len(ds) != 0 || !reflect.DeepEqual(r.reads, []string{"config.toml", "auth.json"}) {
		t.Fatalf("unexpected diagnostics/reads: %+v %+v", ds, r.reads)
	}
	cs, ds, r = runFileReader(t, readCodex, map[string]string{"config.toml": `model_provider="custom"
[model_providers.custom]
base_url="https://custom.example.test/v1"
`, "auth.json": `{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-openai-key"}`})
	if len(cs) != 0 || !hasFileCode(ds, "incomplete") || len(r.reads) != 2 {
		t.Fatal("custom provider borrowed unrelated OpenAI key")
	}
	cs, _, _ = runFileReader(t, readCodex, map[string]string{"config.toml": `model_provider="custom"
[model_providers.custom]
base_url="https://custom.example.test/v1"
requires_openai_auth=true
`, "auth.json": `{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-openai-key"}`})
	requireFileEndpoint(t, cs, "synthetic-openai-key", "https://custom.example.test/v1", store.FormatOpenAIResponses)
	cs, _, _ = runFileReader(t, readCodex, map[string]string{"auth.json": `{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-openai-key"}`})
	requireFileEndpoint(t, cs, "synthetic-openai-key", "https://api.openai.com/v1", store.FormatOpenAIResponses)
}

func TestReadCodexRejectsNonEquivalentBasePaths(t *testing.T) {
	for _, path := range []string{"", "/", "/proxy", "/V1", "/v1/responses", "/proxy%2Fv1", "/proxy/../v1", "/proxy/./v1"} {
		for _, authFile := range []bool{false, true} {
			t.Run(path+"/"+map[bool]string{false: "literal", true: "auth-file"}[authFile], func(t *testing.T) {
				provider := "model_provider=\"custom\"\n[model_providers.custom]\nbase_url=\"https://custom.example.test" + path + "\"\n"
				if authFile {
					provider += "requires_openai_auth=true\n"
				} else {
					provider += "experimental_bearer_token=\"synthetic-private-key\"\n"
				}
				cs, ds, _ := runFileReader(t, readCodex, map[string]string{"config.toml": provider, "auth.json": `{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-private-key"}`})
				if len(cs) != 0 || !hasFileCode(ds, "unsupported") {
					t.Fatalf("base path would change when the gateway appends /v1: candidates=%d diagnostics=%+v", len(cs), ds)
				}
			})
		}
	}
	for _, path := range []string{"/v1", "/proxy/v1", "/proxy/v1/"} {
		config := "model_provider=\"custom\"\n[model_providers.custom]\nbase_url=\"https://custom.example.test" + path + "\"\nexperimental_bearer_token=\"synthetic-private-key\""
		cs, ds, _ := runFileReader(t, readCodex, map[string]string{"config.toml": config})
		if len(cs) != 1 || len(ds) != 0 {
			t.Fatalf("equivalent path rejected: %s %+v", path, ds)
		}
	}
}

func TestReadCodexEnvAndUnsupportedConditionsNeverFallback(t *testing.T) {
	for _, tt := range []struct{ name, extra, code string }{
		{"env wins", "env_key=\"SYNTHETIC_API_KEY\"", "reference_skipped"},
		{"command", "auth={command=\"synthetic-do-not-execute\"}", "unsupported"},
		{"gateway oauth", "gateway_oauth={issuer=\"https://issuer.example.test\"}", "unsupported"},
		{"headers", "http_headers={X-Required=\"synthetic-required\"}", "unsupported"},
		{"environment headers", "env_http_headers={X-Required=\"SYNTHETIC_HEADER\"}", "unsupported"},
		{"query", "query_params={api_version=\"synthetic\"}", "unsupported"},
		{"aws", "aws={profile=\"synthetic-profile\"}", "unsupported"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := "model_provider=\"custom\"\n[model_providers.custom]\nbase_url=\"https://custom.example.test/v1\"\nexperimental_bearer_token=\"synthetic-literal-fallback\"\n" + tt.extra
			cs, ds, r := runFileReader(t, readCodex, map[string]string{"config.toml": config, "auth.json": `{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-auth-fallback"}`})
			if len(cs) != 0 || !hasFileCode(ds, tt.code) || len(r.reads) != 2 {
				t.Fatalf("condition was lost: %+v %+v %+v", cs, ds, r.reads)
			}
		})
	}
}

func TestReadCodexAPIKeyMaterialIsNotDiscardedByUnrelatedOAuthFields(t *testing.T) {
	for _, body := range []string{
		`{"auth_mode":"chatgpt","OPENAI_API_KEY":"synthetic-key","tokens":{"access_token":"synthetic-access"}}`,
		`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-key","tokens":{"refresh_token":"synthetic-refresh"}}`,
		`{"OPENAI_API_KEY":"synthetic-key"}`,
	} {
		cs, _, _ := runFileReader(t, readCodex, map[string]string{"auth.json": body})
		if len(cs) == 0 {
			t.Fatal("explicit API Key lost")
		}
		requireFileEndpoint(t, cs[:1], "synthetic-key", "https://api.openai.com/v1", store.FormatOpenAIResponses)
		for _, candidate := range cs[1:] {
			if candidate.OAuth == nil || candidate.OAuth.OAuth.RefreshToken == "" {
				t.Fatal("additional OAuth candidate lacks refresh material")
			}
		}
	}
}

func TestReadClaudeJSONCAndConnectionConditions(t *testing.T) {
	cs, ds, r := runFileReader(t, readClaude, map[string]string{"settings.json": "\xef\xbb\xbf" + `{ // synthetic comment
 "env":{"ANTHROPIC_API_KEY":"synthetic-key","ANTHROPIC_BASE_URL":"https://messages.example.test","ANTHROPIC_MODEL":"synthetic-model",},
}`})
	requireFileEndpoint(t, cs, "synthetic-key", "https://messages.example.test", store.FormatAnthropicMessages)
	if len(ds) != 0 || !reflect.DeepEqual(r.reads, []string{"settings.json", ".credentials.json"}) {
		t.Fatalf("unexpected read/diagnostics: %+v %+v", r.reads, ds)
	}
	cs, _, _ = runFileReader(t, readClaude, map[string]string{"settings.json": `{"env":{"ANTHROPIC_API_KEY":"synthetic-key"}}`})
	requireFileEndpoint(t, cs, "synthetic-key", "https://api.anthropic.com", store.FormatAnthropicMessages)
	for _, tt := range []struct{ body, code string }{
		{`{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic-bearer"}}`, "oauth_skipped"},
		{`{"apiKeyHelper":"synthetic-command","env":{"ANTHROPIC_API_KEY":"synthetic-key"}}`, "unsupported"},
		{`{"env":{"ANTHROPIC_API_KEY":"synthetic-key","ANTHROPIC_CUSTOM_HEADERS":"X-Required: synthetic"}}`, "unsupported"},
		{`{"env":{"ANTHROPIC_API_KEY":"synthetic-key","CLAUDE_CODE_USE_BEDROCK":"1"}}`, "unsupported"},
		{`{"env":{"ANTHROPIC_API_KEY":"synthetic-key","HTTPS_PROXY":"https://proxy.example.test"}}`, "unsupported"},
	} {
		cs, ds, r := runFileReader(t, readClaude, map[string]string{"settings.json": tt.body, ".credentials.json": "synthetic-unsupported-credentials"})
		if len(cs) != 0 || !hasFileCode(ds, tt.code) || !reflect.DeepEqual(r.reads, []string{"settings.json", ".credentials.json"}) {
			t.Fatalf("unsupported connection imported: %+v %+v", cs, ds)
		}
	}
}

func TestReadOpenCodeExplicitSDKAndFirstExistingFile(t *testing.T) {
	const primary = `{"provider":{"chat":{"npm":"@ai-sdk/openai-compatible","enabled":false,"options":{"apiKey":"synthetic-chat-key","baseURL":"https://chat.example.test/v1"}},"messages":{"npm":"@ai-sdk/anthropic","options":{"apiKey":"synthetic-messages-key","baseURL":"https://messages.example.test/v1"}}}}`
	cs, ds, r := runFileReader(t, readOpenCode, map[string]string{"opencode.jsonc": "//synthetic\n" + primary, "opencode.json": `{"provider":{}}`, "config.json": "must-not-read"})
	if len(cs) != 2 || len(ds) != 0 || !reflect.DeepEqual(r.reads, []string{"opencode.jsonc"}) {
		t.Fatalf("wrong file precedence: %+v %+v %+v", cs, ds, r.reads)
	}
	if cs[0].Key != "synthetic-chat-key" || !cs[0].Disabled || cs[0].Endpoints[0].Format != store.FormatOpenAIChatCompletions || cs[1].Endpoints[0].Format != store.FormatAnthropicMessages {
		t.Fatalf("wrong formats: %+v", cs)
	}
	cs, ds, r = runFileReader(t, readOpenCode, map[string]string{"opencode.jsonc": "{bad", "opencode.json": primary})
	if len(cs) != 0 || !hasFileCode(ds, "invalid_format") || len(r.reads) != 1 {
		t.Fatal("bad primary was bypassed")
	}
	cs, _, r = runFileReader(t, readOpenCode, map[string]string{"config.json": primary})
	if len(cs) != 2 || !reflect.DeepEqual(r.reads, []string{"opencode.jsonc", "opencode.json", "config.json"}) {
		t.Fatal("fallback file selection failed")
	}
}

func TestReadOpenCodeRejectsNonEquivalentSDKBasePaths(t *testing.T) {
	for _, npm := range []string{"@ai-sdk/anthropic", "@ai-sdk/openai-compatible"} {
		for _, path := range []string{"", "/", "/proxy", "/V1", "/v1/messages", "/v1/responses", "/proxy%2Fv1", "/v1//", "/proxy/../v1"} {
			t.Run(npm+path, func(t *testing.T) {
				body := fixtureJSON(map[string]any{"provider": map[string]any{"custom": map[string]any{"npm": npm, "options": map[string]any{"apiKey": "synthetic-key", "baseURL": "https://custom.example.test" + path}}}})
				cs, ds, _ := runFileReader(t, readOpenCode, map[string]string{"opencode.json": body})
				if len(cs) != 0 || !hasFileCode(ds, "unsupported") {
					t.Fatalf("non-equivalent SDK URL imported: %d %+v", len(cs), ds)
				}
			})
		}
		for _, base := range []string{"https://custom.example.test/v1", "https://custom.example.test/proxy/v1/"} {
			body := fixtureJSON(map[string]any{"provider": map[string]any{"custom": map[string]any{"npm": npm, "options": map[string]any{"apiKey": "synthetic-key", "baseURL": base}}}})
			cs, ds, _ := runFileReader(t, readOpenCode, map[string]string{"opencode.json": body})
			if len(cs) != 1 || len(ds) != 0 {
				t.Fatalf("equivalent SDK URL rejected: %+v", ds)
			}
		}
	}
	// Anthropic's SDK explicitly normalizes this official origin to /v1.
	for _, base := range []string{"https://api.anthropic.com", "https://api.anthropic.com/"} {
		body := fixtureJSON(map[string]any{"provider": map[string]any{"official": map[string]any{"npm": "@ai-sdk/anthropic", "options": map[string]any{"apiKey": "synthetic-key", "baseURL": base}}}})
		cs, ds, _ := runFileReader(t, readOpenCode, map[string]string{"opencode.json": body})
		requireFileEndpoint(t, cs, "synthetic-key", base, store.FormatAnthropicMessages)
		if len(ds) != 0 {
			t.Fatalf("official Anthropic normalization rejected: %+v", ds)
		}
	}
	cs, ds, _ := runFileReader(t, readOpenCode, map[string]string{"opencode.json": `{"provider":{"official":{"npm":"@ai-sdk/anthropic","options":{"apiKey":"synthetic-key","baseURL":"https://api.anthropic.com//"}}}}`})
	if len(cs) != 0 || !hasFileCode(ds, "unsupported") {
		t.Fatal("AI SDK removes only one trailing slash; double slash was incorrectly treated as its official-origin exception")
	}
}

func TestReadZCodeLegacyRejectsNonEquivalentOpenAIPaths(t *testing.T) {
	for _, kind := range []string{"openai", "openai-compatible"} {
		for _, path := range []string{"", "/proxy", "/v1/responses", "/proxy%2Fv1"} {
			body := fixtureJSON(map[string]any{"provider": map[string]any{"custom": map[string]any{"source": "custom", "kind": kind, "options": map[string]any{"apiKey": "synthetic-key", "baseURL": "https://legacy.example.test" + path}}}})
			cs, ds, _ := runFileReader(t, readZCode, map[string]string{"config.json": body})
			if len(cs) != 0 || !hasFileCode(ds, "unsupported") {
				t.Fatalf("legacy OpenAI base path changed: %s%s %d %+v", kind, path, len(cs), ds)
			}
		}
	}
	// ZCode's own Anthropic kind uses baseURL + /v1/messages, unlike AI SDK.
	body := `{"provider":{"custom":{"source":"custom","kind":"anthropic","options":{"apiKey":"synthetic-key","baseURL":"https://legacy.example.test/proxy"}}}}`
	cs, ds, _ := runFileReader(t, readZCode, map[string]string{"config.json": body})
	requireFileEndpoint(t, cs, "synthetic-key", "https://legacy.example.test/proxy", store.FormatAnthropicMessages)
	if len(ds) != 0 {
		t.Fatalf("legacy Anthropic prefix rejected: %+v", ds)
	}
}

func TestReadOpenCodeCannotDropExtraOptionsOrGuessOpenAIWire(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider map[string]any
	}{
		{"openai wire", map[string]any{"npm": "@ai-sdk/openai", "options": map[string]any{"baseURL": "https://api.example.test/v1", "apiKey": "synthetic-key"}}},
		{"headers", map[string]any{"npm": "@ai-sdk/anthropic", "headers": map[string]any{"X-Required": "synthetic"}, "options": map[string]any{"baseURL": "https://api.example.test", "apiKey": "synthetic-key"}}},
		{"extra option", map[string]any{"npm": "@ai-sdk/anthropic", "options": map[string]any{"baseURL": "https://api.example.test", "apiKey": "synthetic-key", "headers": map[string]any{"X-Required": "synthetic"}}}},
		{"explicit boolean option", map[string]any{"npm": "@ai-sdk/openai-compatible", "options": map[string]any{"baseURL": "https://api.example.test", "apiKey": "synthetic-key", "includeUsage": false}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs, ds, _ := runFileReader(t, readOpenCode, map[string]string{"opencode.json": fixtureJSON(map[string]any{"provider": map[string]any{"synthetic": tt.provider}})})
			if len(cs) != 0 || !hasFileCode(ds, "unsupported") {
				t.Fatalf("lost request condition: %+v %+v", cs, ds)
			}
		})
	}
}

func zcodeFixture(apiType string, extras map[string]any) string {
	cfg := map[string]any{"access": map[string]any{"type": "api-key", "apiKey": "synthetic-zcode-key"}, "api": map[string]any{"type": apiType, "baseUrl": "https://zcode.example.test/v1"}}
	for k, v := range extras {
		cfg[k] = v
	}
	return fixtureJSON(map[string]any{"schemaVersion": 1, "config": map[string]any{"providerConfigRules": map[string]any{"providerRules": []any{map[string]any{"providerId": "synthetic", "enabled": false, "config": cfg}}}}})
}
func TestReadZCodeCurrentProtocolsAndFixedScope(t *testing.T) {
	for kind, format := range map[string]string{"anthropic-messages": store.FormatAnthropicMessages, "openai-chat-completions": store.FormatOpenAIChatCompletions, "openai-responses": store.FormatOpenAIResponses} {
		t.Run(kind, func(t *testing.T) {
			cs, ds, r := runFileReader(t, readZCode, map[string]string{"provider_config.json": zcodeFixture(kind, nil), "setting.json": `{"dataBaseDir":"D:\\synthetic-moved"}`, "config.json": "must-not-read"})
			requireFileEndpoint(t, cs, "synthetic-zcode-key", "https://zcode.example.test/v1", format)
			if !cs[0].Disabled || !hasFileCode(ds, "redirect_scope") || !reflect.DeepEqual(r.reads, []string{"setting.json", "provider_config.json"}) {
				t.Fatalf("unexpected scope/disabled state: %+v %+v %+v", cs, ds, r.reads)
			}
		})
	}
	cs, ds, r := runFileReader(t, readZCode, map[string]string{"provider_config.json": "{bad", "config.json": `{"provider":{"custom":{"source":"custom","kind":"anthropic","options":{"baseURL":"https://legacy.example.test","apiKey":"synthetic-legacy"}}}}`})
	if len(cs) != 0 || !hasFileCode(ds, "invalid_format") || !reflect.DeepEqual(r.reads, []string{"setting.json", "provider_config.json"}) {
		t.Fatal("bad current file fell back to stale legacy")
	}
}
func TestReadZCodeLegacyOnlyExplicitCustomProvider(t *testing.T) {
	doc := map[string]any{"provider": map[string]any{
		"a":       map[string]any{"source": "custom", "kind": "openai", "options": map[string]any{"baseURL": "https://legacy.example.test/v1", "apiKey": "synthetic-legacy"}},
		"builtin": map[string]any{"kind": "anthropic", "options": map[string]any{"baseURL": "https://oauth.example.test", "apiKey": "synthetic-oauth"}},
	}}
	cs, ds, r := runFileReader(t, readZCode, map[string]string{"config.json": fixtureJSON(doc)})
	requireFileEndpoint(t, cs, "synthetic-legacy", "https://legacy.example.test/v1", store.FormatOpenAIResponses)
	if !hasFileCode(ds, "oauth_skipped") || !reflect.DeepEqual(r.reads, []string{"setting.json", "provider_config.json", "config.json"}) {
		t.Fatal("legacy custom/source boundary failed")
	}
	for _, extra := range []map[string]any{
		{"access": map[string]any{"type": "oauth", "apiKey": "synthetic-not-api-key"}},
		{"api": map[string]any{"type": "unknown", "baseUrl": "https://api.example.test"}},
		{"headers": map[string]any{"X-Required": "synthetic-required"}},
		{"api": map[string]any{"type": "openai-responses", "baseUrl": "https://api.example.test", "headers": map[string]any{"X-Required": "synthetic"}}},
	} {
		cs, ds, _ := runFileReader(t, readZCode, map[string]string{"provider_config.json": zcodeFixture("openai-responses", extra)})
		if len(cs) != 0 || !(hasFileCode(ds, "oauth_skipped") || hasFileCode(ds, "unsupported")) {
			t.Fatalf("unsupported current config imported: %+v %+v", cs, ds)
		}
	}
}

func TestFileReadersReferencesEncryptedAndMissingFields(t *testing.T) {
	for _, value := range []any{"{env:SYNTHETIC_KEY}", "${SYNTHETIC_KEY}", "enc:synthetic-encrypted", "{file:synthetic-reference}", map[string]any{"encrypted": "synthetic-envelope"}} {
		doc := map[string]any{"provider": map[string]any{"synthetic": map[string]any{"npm": "@ai-sdk/anthropic", "options": map[string]any{"baseURL": "https://api.example.test", "apiKey": value}}}}
		cs, ds, _ := runFileReader(t, readOpenCode, map[string]string{"opencode.json": fixtureJSON(doc)})
		if len(cs) != 0 || !hasFileCode(ds, "reference_skipped") {
			t.Fatalf("reference imported: %+v %+v", cs, ds)
		}
	}
	for _, body := range []string{`{"provider":{"synthetic":{"npm":"@ai-sdk/anthropic","options":{"apiKey":"synthetic"}}}}`, `{"provider":{"synthetic":{"npm":"@ai-sdk/anthropic","options":{"apiKey":"synthetic","baseURL":"${SYNTHETIC_URL}"}}}}`} {
		cs, ds, _ := runFileReader(t, readOpenCode, map[string]string{"opencode.json": body})
		if len(cs) != 0 || len(ds) == 0 {
			t.Fatal("missing or referenced address imported")
		}
	}
}

func TestFileReadersBadSchemasControlledDiagnosticsAndCancellation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		reader     fileReader
		file, body string
	}{
		{"codex", readCodex, "config.toml", "model_provider = [synthetic-secret]"},
		{"codex wrong selected type", readCodex, "config.toml", "model_provider=123"},
		{"codex malformed auth flag with literal key", readCodex, "config.toml", "[model_providers.openai]\nexperimental_bearer_token=\"synthetic-key\"\nrequires_openai_auth=\"not-a-boolean\""},
		{"claude", readClaude, "settings.json", `{"env":[]}`},
		{"opencode", readOpenCode, "opencode.jsonc", `{"provider":[]}`},
		{"zcode", readZCode, "provider_config.json", `{"config":{"providerConfigRules":{"providerRules":{}}}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs, ds, _ := runFileReader(t, tt.reader, map[string]string{tt.file: tt.body})
			if len(cs) != 0 || !hasFileCode(ds, "invalid_format") {
				t.Fatalf("invalid schema imported: %+v %+v", cs, ds)
			}
			for _, d := range ds {
				if strings.Contains(d.Message, "synthetic-secret") {
					t.Fatal("parser error leaked input")
				}
			}
		})
	}
	for _, reader := range []fileReader{readCodex, readClaude, readOpenCode, readZCode} {
		r := &fileFixtures{failures: map[string]error{"config.toml": errors.New("synthetic-secret /private/path"), "settings.json": errors.New("synthetic-secret"), "opencode.jsonc": errors.New("synthetic-secret"), "setting.json": errors.New("synthetic-secret")}}
		cs, ds, err := reader(context.Background(), r)
		if err != nil || len(cs) != 0 || len(ds) != 1 || strings.Contains(ds[0].Message, "synthetic-secret") {
			t.Fatalf("raw resource error leaked: %+v %+v %v", cs, ds, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r = &fileFixtures{}
		_, _, err = reader(ctx, r)
		if !errors.Is(err, context.Canceled) || len(r.reads) != 0 {
			t.Fatalf("cancellation read source: %v %+v", err, r.reads)
		}
	}
}
