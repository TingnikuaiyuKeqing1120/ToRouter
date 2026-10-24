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

// SQL is matched exactly, so any extra query (including accounts/tokens/client
// api_keys) fails the reader tests instead of returning a convenient empty set.
type gatewayFakeResources struct {
	t            *testing.T
	files        map[string]string
	queries      map[string][][]string
	reads, calls []string
	err          error
}

func (r *gatewayFakeResources) Read(_ context.Context, file string) ([]byte, error) {
	r.reads = append(r.reads, file)
	if r.err != nil {
		return nil, r.err
	}
	if value, ok := r.files[file]; ok {
		return []byte(value), nil
	}
	return nil, ErrMissing
}

func (r *gatewayFakeResources) Query(_ context.Context, file, sql string) ([][]string, error) {
	r.calls = append(r.calls, file+":"+sql)
	if r.err != nil {
		return nil, r.err
	}
	if rows, ok := r.queries[file+":"+sql]; ok {
		return rows, nil
	}
	r.t.Errorf("unexpected query: %s", sql)
	return nil, ErrInvalid
}

func gatewayJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func gatewayHasDiagnostic(ds []Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func gatewayCCFake(t *testing.T, rows [][]string) *gatewayFakeResources {
	return &gatewayFakeResources{t: t, queries: map[string][][]string{
		"cc-switch.db:" + gatewayCCProvidersSQL: rows,
		"cc-switch.db:" + gatewayCCProxySQL:     {{"claude", "0.0.0.0", "17500"}},
	}}
}

func TestGatewayCCSwitchExcludesManagedAuthBeforeKeyExtraction(t *testing.T) {
	config := `{"env":{"ANTHROPIC_API_KEY":"synthetic-ordinary","ANTHROPIC_BASE_URL":"https://example.invalid"}}`
	rows := [][]string{
		{"1", "claude", "private source label", config, `{}`},
		{"2", "claude", "", config, `{"authBinding":{"source":"managed_account","accountId":"private"}}`},
		{"3", "claude", "", config, `{"providerType":"codex_oauth"}`},
		{"4", "claude", "", config, `{"providerType":"xai_oauth"}`},
		{"5", "claude", "", config, `{"providerType":"github_copilot"}`},
		{"6", "claude", "", config, `{"githubAccountId":"private"}`},
		{"7", "claude", "", config, `malformed private content`},
		{"8", "claude", "", config, `{"authBinding":"invalid"}`},
	}
	r := gatewayCCFake(t, rows)
	got, ds, err := readCCSwitch(context.Background(), r)
	if err != nil || len(got) != 1 || got[0].Key != "synthetic-ordinary" {
		t.Fatalf("got %d candidates, error %v", len(got), err)
	}
	if !gatewayHasDiagnostic(ds, "oauth_skipped") || !gatewayHasDiagnostic(ds, "invalid_format") {
		t.Fatalf("missing diagnostic: %+v", ds)
	}
	if len(r.calls) != 2 || len(r.reads) != 0 {
		t.Fatalf("unexpected resource access: %+v", r)
	}
	encoded := gatewayJSON(t, ds)
	if strings.Contains(encoded, "private") || strings.Contains(encoded, "synthetic") {
		t.Fatal("diagnostic leaked source data")
	}
}

func TestGatewayCCSwitchProtocolsAndActiveCodexProvider(t *testing.T) {
	codex := gatewayJSON(t, map[string]any{"auth": map[string]any{"OPENAI_API_KEY": "synthetic-codex"}, "config": `model_provider = "active"
[model_providers.inactive]
base_url = "https://wrong.invalid/v1"
wire_api = "chat"
http_headers = { X-Inactive = "unused" }
[model_providers.active]
base_url = "https://active.invalid/v1"
wire_api = "responses"
`})
	r := gatewayCCFake(t, [][]string{
		{"1", "codex", "", codex, `{}`},
		{"2", "claude", "", `{"env":{"ANTHROPIC_API_KEY":"synthetic-claude"}}`, `{"apiFormat":"openai_chat"}`},
		{"3", "gemini", "", `{"env":{"GEMINI_API_KEY":"synthetic-gemini","GOOGLE_GEMINI_BASE_URL":"https://gemini.invalid/v1beta"}}`, `{}`},
		{"4", "opencode", "", `{"npm":"@ai-sdk/anthropic","options":{"apiKey":"synthetic-open","baseURL":"https://anthropic.invalid/v1"}}`, `{}`},
	})
	got, ds, err := readCCSwitch(context.Background(), r)
	if err != nil || len(ds) != 0 || len(got) != 4 {
		t.Fatalf("got %d, diagnostics %+v, error %v", len(got), ds, err)
	}
	want := []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://active.invalid/v1"}, {Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.anthropic.com"}, {Format: store.FormatGeminiGenerateContent, BaseURL: "https://gemini.invalid/v1beta"}, {Format: store.FormatAnthropicMessages, BaseURL: "https://anthropic.invalid/v1"}}
	for i, c := range got {
		if !reflect.DeepEqual(c.Endpoints, []store.Endpoint{want[i]}) {
			t.Errorf("candidate %d endpoint = %+v", i, c.Endpoints)
		}
	}
}

func TestGatewayCCSwitchRejectsUnsupportedReferencesAndProxyOrigins(t *testing.T) {
	base := func(key, url string) string {
		return gatewayJSON(t, map[string]any{"env": map[string]any{"ANTHROPIC_API_KEY": key, "ANTHROPIC_BASE_URL": url}})
	}
	rows := [][]string{
		{"1", "claude", "", base("PROXY_MANAGED", "https://example.invalid"), `{}`},
		{"2", "claude", "", base("synthetic", "http://localhost:17500/v1"), `{}`},
		{"3", "claude", "", base("synthetic", "http://[::1]:15721"), `{}`},
		{"4", "claude", "", base("enc:private", "https://example.invalid"), `{}`},
		{"5", "claude", "", base("{env:SECRET}", "https://example.invalid"), `{}`},
		{"6", "claude", "", base("${SECRET}", "https://example.invalid"), `{}`},
		{"7", "claude", "", base("synthetic", "https://user:pass@example.invalid"), `{}`},
		{"8", "claude", "", base("synthetic", "https://example.invalid?key=secret"), `{}`},
		{"9", "claude", "", `{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic"}}`, `{}`},
		{"10", "gemini", "", `{"env":{"GEMINI_API_KEY":"ya29.synthetic"}}`, `{}`},
		{"11", "opencode", "", `{"npm":"@ai-sdk/openai-compatible","options":{"apiKey":"synthetic","baseURL":"https://example.invalid","headers":{"X-Private":"needed"}}}`, `{}`},
		{"12", "unknown-app", "", base("synthetic", "https://example.invalid"), `{}`},
		{"13", "codex", "", `{"auth":{"OPENAI_API_KEY":"stale","tokens":{"access_token":"oauth"}},"config":"model_provider='active'"}`, `{}`},
		{"14", "claude", "", base("synthetic", "https://example.invalid"), `{"localProxyRequestOverrides":{"headers":{"X-Special":"required"}}}`},
		{"15", "claude", "", base("synthetic", "http://127.0.0.1:015721/v1"), `{}`},
		{"16", "claude", "", base("synthetic", "https://example.invalid"), `{"apiFormat":false}`},
		{"17", "claude", "", `{"env":{"ANTHROPIC_API_KEY":"synthetic","ANTHROPIC_BASE_URL":42}}`, `{}`},
		{"18", "opencode", "", `{"npm":"@ai-sdk/openai-compatible","options":{"apiKey":"synthetic","baseURL":"https://example.invalid/v1"},"models":{"test":{"options":{"headers":{"X-Needed":"value"}}}}}`, `{}`},
	}
	got, ds, err := readCCSwitch(context.Background(), gatewayCCFake(t, rows))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d, error %v", len(got), err)
	}
	for _, code := range []string{"proxy_skipped", "reference_skipped", "unsupported", "oauth_skipped"} {
		if !gatewayHasDiagnostic(ds, code) {
			t.Errorf("missing %s", code)
		}
	}
}

func gatewayManagerFake(t *testing.T, rows [][]string) *gatewayFakeResources {
	return &gatewayFakeResources{t: t, queries: map[string][][]string{
		"codexmanager.db:" + gatewayManagerProvidersSQL: rows,
		"codexmanager.db:" + gatewayManagerAddressSQL:   {{"127.0.0.1:49900"}},
	}}
}

func TestGatewayCodexManagerExplicitProtocolsAndAuth(t *testing.T) {
	rows := [][]string{
		{"1", "private", "codex", "https://responses.invalid/v1", "apikey", "", "/responses", "active", "synthetic-r"},
		{"2", "", "codex", "https://chat.invalid", "api_key", `{"location":"header","name":"Authorization","headerValueFormat":"bearer"}`, "/v1/chat/completions", "disabled", "synthetic-c"},
		{"3", "", "claude", "https://claude.invalid", "key", `{"location":"header","name":"x-api-key","headerValueFormat":"raw"}`, "/v1/messages", "active", "synthetic-a"},
		{"4", "", "gemini", "https://gemini.invalid/v1beta", "apikey", `{"location":"header","name":"x-goog-api-key","headerValueFormat":"raw"}`, "", "active", "synthetic-g"},
	}
	r := gatewayManagerFake(t, rows)
	got, ds, err := readCodexManager(context.Background(), r)
	if err != nil || len(ds) != 0 || len(got) != 4 {
		t.Fatalf("got %d, diagnostics %+v, error %v", len(got), ds, err)
	}
	if !got[1].Disabled || got[1].Endpoints[0].BaseURL != "https://chat.invalid/v1" || got[2].Endpoints[0].BaseURL != "https://claude.invalid/v1" {
		t.Fatalf("wrong URL or disabled mapping: %+v", got)
	}
	if len(r.calls) != 2 || len(r.reads) != 0 {
		t.Fatal("unexpected reads")
	}
}

func TestGatewayCodexManagerDoesNotGuessAuthOrCustomActions(t *testing.T) {
	rows := [][]string{}
	for _, change := range []struct {
		column int
		value  string
	}{
		{2, "compatible"}, {4, "userpass"}, {5, `{"location":"query","name":"key"}`}, {5, `{"location":"header","name":"X-Special","headerValueFormat":"raw"}`},
		{5, `{"location":"header","name":"Authorization","extra":"required"}`}, {6, "/custom/responses"}, {6, "/responses?key=private"}, {6, ""}, {3, "https://root.invalid"},
		{3, "http://[::1]:48760/v1"}, {3, "http://localhost:48761"}, {3, "http://localhost:49900/v1"}, {8, "codex_oauth_placeholder"}, {8, "enc:private"},
	} {
		row := []string{"1", "private", "codex", "https://upstream.invalid/v1", "apikey", "", "/responses", "active", "synthetic"}
		row[change.column] = change.value
		rows = append(rows, row)
	}
	got, ds, err := readCodexManager(context.Background(), gatewayManagerFake(t, rows))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d, error %v", len(got), err)
	}
	for _, code := range []string{"unsupported", "proxy_skipped", "reference_skipped"} {
		if !gatewayHasDiagnostic(ds, code) {
			t.Errorf("missing %s", code)
		}
	}
}

func TestGatewayMagpieKeepsProtocolBoundToEachKey(t *testing.T) {
	r := &gatewayFakeResources{t: t, files: map[string]string{
		"providers.json": `{"providers":[{"name":"private label","key":"synthetic-first","keyProtocol":"anthropic","chat":"https://chat.invalid/v1","responses":"https://responses.invalid/v1","anthropic":"https://anthropic.invalid","gemini":"https://gemini.invalid/v1beta","keys":[{"key":"synthetic-chat","protocol":"chat","off":true},{"key":"synthetic-all"},{"key":"synthetic-responses","protocol":"responses"}]}]}`,
		"settings.json":  `{"port":40000,"lanKey":"must-not-import"}`,
	}}
	got, ds, err := readMagpie(context.Background(), r)
	if err != nil || len(ds) != 0 || len(got) != 4 {
		t.Fatalf("got %d, diagnostics %+v, error %v", len(got), ds, err)
	}
	if got[0].Endpoints[0].Format != store.FormatAnthropicMessages || got[0].Endpoints[0].BaseURL != "https://anthropic.invalid" || len(got[0].Endpoints) != 1 {
		t.Fatalf("first key mapped to wrong protocol: %+v", got[0])
	}
	if got[1].Endpoints[0].BaseURL != "https://chat.invalid/v1" || !got[1].Disabled || len(got[2].Endpoints) != 4 || got[3].Endpoints[0].Format != store.FormatOpenAIResponses {
		t.Fatalf("key protocol mapping failed: %+v", got)
	}
	if !reflect.DeepEqual(r.reads, []string{"providers.json", "settings.json"}) || len(r.calls) != 0 {
		t.Fatalf("unexpected access: %+v", r)
	}
	r.files["providers.json"] = `{"providers":[{"key":"synthetic-disabled","chat":"https://chat.invalid/v1","off":true}]}`
	got, ds, err = readMagpie(context.Background(), r)
	if err != nil || len(ds) != 0 || len(got) != 1 || !got[0].Disabled {
		t.Fatalf("provider off was not preserved: %d %+v %v", len(got), ds, err)
	}
}

func TestGatewayMagpieRejectsManagedKeysAndExtraConditions(t *testing.T) {
	providers := []any{}
	for _, extra := range []map[string]any{
		{"preset": "remote-magpie"}, {"accessKeyID": "account", "secretAccessKey": "secret"}, {"headers": map[string]string{"X-Special": "required"}},
		{"extraBody": map[string]any{"private": "required"}}, {"authHeader": "bearer"}, {"chat": "http://[::1]:3425/v1"},
		{"chat": "http://localhost:40000/v1"}, {"key": "${SECRET}"}, {"key": "{env:SECRET}"}, {"key": "enc:private"},
		{"key": "copilot_placeholder"}, {"key": "xai_oauth_placeholder"}, {"keyProtocol": "unknown"}, {"chat": "https://example.invalid/chat/completions"},
		{"chat": "file:///private"}, {"chat": "https://example.invalid?secret=required"},
	} {
		p := map[string]any{"key": "synthetic", "chat": "https://example.invalid/v1"}
		for k, v := range extra {
			p[k] = v
		}
		providers = append(providers, p)
	}
	r := &gatewayFakeResources{t: t, files: map[string]string{"providers.json": gatewayJSON(t, map[string]any{"providers": providers}), "settings.json": `{"port":40000}`}}
	got, ds, err := readMagpie(context.Background(), r)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d, error %v", len(got), err)
	}
	for _, code := range []string{"oauth_skipped", "unsupported", "proxy_skipped", "reference_skipped", "invalid_format"} {
		if !gatewayHasDiagnostic(ds, code) {
			t.Errorf("missing %s", code)
		}
	}
}

func TestGatewayReaderErrorsAreControlled(t *testing.T) {
	for _, reader := range []func(context.Context, Resources) ([]Candidate, []Diagnostic, error){readCCSwitch, readCodexManager, readMagpie} {
		r := &gatewayFakeResources{t: t, err: errors.New("private path and secret synthetic")}
		got, ds, err := reader(context.Background(), r)
		if !errors.Is(err, ErrInvalid) || len(got) != 0 {
			t.Fatalf("unexpected result: %d %v", len(got), err)
		}
		if strings.Contains(err.Error()+gatewayJSON(t, ds), "private") {
			t.Fatal("source failure leaked")
		}
	}
	for _, raw := range []string{`{broken secret`, `{"providers":{}}`, `null`} {
		r := &gatewayFakeResources{t: t, files: map[string]string{"providers.json": raw}}
		_, ds, err := readMagpie(context.Background(), r)
		if !errors.Is(err, ErrInvalid) || !gatewayHasDiagnostic(ds, "invalid_format") {
			t.Fatalf("malformed document not rejected: %v %+v", err, ds)
		}
	}
}
