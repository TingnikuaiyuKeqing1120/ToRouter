package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"torouter/internal/store"
)

func boolValue(v bool) *bool       { return &v }
func stringValue(v string) *string { return &v }

func TestSpecsExposeChoiceRangeAndNormalizedThinkingDefaults(t *testing.T) {
	empty := []string{}
	cfg := store.ModelConfig{ThinkingEfforts: &empty, NativeThinking: &store.NativeThinking{Supported: boolValue(true), Mandatory: boolValue(false), Modes: []store.NativeThinkingMode{{ID: "effort", APIFormat: store.FormatOpenAIChatCompletions, Control: "effort", Efforts: []string{"none", "high"}}}}}
	defaults := &store.ModelDefaults{Thinking: &store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false), Budget: defaultBudget(32)}}
	caps := capabilities(cfg)
	options, ok := caps["thinking_options"].([]string)
	if !ok || options == nil || len(options) != 0 || caps["thinking_mandatory"] != false {
		t.Fatalf("explicit empty options or native facts lost: %+v", caps)
	}
	values, reasons := specDefaults(cfg, defaults, store.FormatOpenAIChatCompletions, false)
	if len(reasons) != 0 {
		t.Fatal(reasons)
	}
	thinking := values.(map[string]any)["thinking"].(*store.ThinkingDefault)
	if thinking.Effort == nil || *thinking.Effort != "none" || thinking.Budget != nil || defaults.Thinking.Budget == nil {
		t.Fatalf("specs differ from actual mapping or mutate config: %+v", thinking)
	}
}
func specGet(t *testing.T, g *Gateway, query string) (int, []map[string]any, string) {
	t.Helper()
	w := httptest.NewRecorder()
	serveTestClient(g, w, httptest.NewRequest(http.MethodGet, "/v1/models/specs"+query, nil))
	var response struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Object != "list" {
			t.Fatal(response)
		}
	}
	return w.Code, response.Data, w.Body.String()
}
func TestModelSpecsEndpoint(t *testing.T) {
	g, s := setup(t)
	modalities := []string{"text", "image"}
	out := []string{"text"}
	c := store.Channel{Name: "OpenAI official", APIKey: "sk-private-secret", Models: []string{"gpt"}, Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://private-upstream.example"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c.ModelConfigs = map[string]store.ModelConfig{"gpt": store.ModelConfig{ContextWindow: defaultBudget(128000), MaxInputTokens: defaultBudget(64000), MaxOutputTokens: defaultBudget(4096), InputModalities: &modalities, OutputModalities: &out, NativeThinking: &store.NativeThinking{Supported: boolValue(false), Modes: []store.NativeThinkingMode{}}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := store.DownstreamModel{ID: "gpt", DisplayName: "GPT public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "gpt", Enabled: true}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "disabled", Enabled: false, Bindings: []store.Binding{{ChannelID: c.ID, Model: "gpt", Enabled: false}}}); err != nil {
		t.Fatal(err)
	}
	p := priceRow(0, "gpt", "USD", store.PriceRates{Input: pricingNumber(2.5), Output: pricingNumber(10), CacheRead: pricingNumber(0.000000001)})
	if err := s.SavePrices([]store.Price{p}); err != nil {
		t.Fatal(err)
	}
	status, data, raw := specGet(t, g, "")
	if status != 200 || len(data) != 1 {
		t.Fatal(status, data)
	}
	got := data[0]
	if got["id"] != "gpt" || got["name"] != "GPT public" || got["context_length"] != float64(128000) || got["max_input_tokens"] != float64(64000) || got["max_output_tokens"] != float64(4096) {
		t.Fatal(got)
	}
	if got["thinking"].(map[string]any)["supported"] != false {
		t.Fatal(got)
	}
	mods := got["modalities"].(map[string]any)
	if len(mods["input"].([]any)) != 2 || len(mods["output"].([]any)) != 1 {
		t.Fatal(mods)
	}
	ref := got["pricing"].(map[string]any)["official"].(map[string]any)
	table := ref["table"].(map[string]any)
	if table["currency"] != "USD" || table["rates"].(map[string]any)["input"] != 2.5 || table["rates"].(map[string]any)["output"] != float64(10) || table["rates"].(map[string]any)["cache_read"] != 0.000000001 {
		t.Fatal(ref)
	}
	if strings.Contains(raw, "sk-private") || strings.Contains(raw, "private-upstream") || strings.Contains(raw, "channel_id") || strings.Contains(raw, "base_url") {
		t.Fatal("private information leaked", raw)
	}
}
func TestSpecsMixedBindingsDeclarationsAndFormatFiltering(t *testing.T) {
	g, s := setup(t)
	a := store.Channel{Name: "large", APIKey: "secret-a", Models: []string{"up"}, Enabled: true, ModelConfigs: map[string]store.ModelConfig{"up": {ContextWindow: defaultBudget(1000000), Streaming: boolValue(false)}}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://a.invalid"}}}
	b := store.Channel{Name: "small", APIKey: "secret-b", Models: []string{"up"}, Enabled: true, ModelConfigs: map[string]store.ModelConfig{"up": {ContextWindow: defaultBudget(128000), Streaming: boolValue(false)}}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://b.invalid"}}}
	left := []string{"text", "image"}
	right := []string{"image", "text"}
	acfg := a.ModelConfigs["up"]
	acfg.InputModalities = &left
	a.ModelConfigs["up"] = acfg
	bcfg := b.ModelConfigs["up"]
	bcfg.InputModalities = &right
	b.ModelConfigs["up"] = bcfg
	disabled := a
	disabled.Name = "disabled"
	disabled.Enabled = false
	unused := a
	unused.Name = "disabled binding"
	for _, c := range []*store.Channel{&a, &b, &disabled, &unused} {
		if err := s.SaveChannel(c); err != nil {
			t.Fatal(err)
		}
	}
	m := store.DownstreamModel{ID: "mixed", Enabled: true, Bindings: []store.Binding{{ChannelID: a.ID, Model: "up", Enabled: true}, {ChannelID: b.ID, Model: "up", Enabled: true}, {ChannelID: disabled.ID, Model: "up", Enabled: true}, {ChannelID: unused.ID, Model: "up", Enabled: false}}}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	_, data, _ := specGet(t, g, "")
	got := data[0]
	if got["context_length"] != nil || got["max_input_tokens"] != nil || got["streaming"] != false || len(got["bindings"].([]any)) != 2 {
		t.Fatal(got)
	}
	if got["sources"].(map[string]any)["streaming"] != "consistent_bindings" || len(got["differences"].([]any)) == 0 {
		t.Fatal(got)
	}
	if got["sources"].(map[string]any)["input_modalities"] != "consistent_bindings" || len(got["modalities"].(map[string]any)["input"].([]any)) != 2 {
		t.Fatal("modality order created false conflict", got)
	}
	m.DisplayName = "Changed without altering upstream facts"
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	_, data, _ = specGet(t, g, "")
	got = data[0]
	if got["context_length"] != nil || got["sources"].(map[string]any)["context_length"] != "unknown" || len(got["warnings"].([]any)) == 0 {
		t.Fatal(got)
	}
	_, data, _ = specGet(t, g, "?api_format=openai_responses")
	bindings := data[0]["bindings"].([]any)
	if len(bindings) != 2 || bindings[0].(map[string]any)["name"] != "large" || bindings[1].(map[string]any)["name"] != "small" {
		t.Fatal(bindings)
	}
	_, data, _ = specGet(t, g, "?api_format=anthropic_messages")
	bindings = data[0]["bindings"].([]any)
	if len(bindings) != 2 || bindings[0].(map[string]any)["name"] != "large" || bindings[1].(map[string]any)["name"] != "small" {
		t.Fatal(bindings)
	}
	status, _, _ := specGet(t, g, "?api_format=unknown")
	if status != 400 {
		t.Fatal(status)
	}
}

func TestPublicThinkingOptionsPreserveUnknownAndDeclaredEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   store.ModelConfig
		known bool
	}{
		{"reasoning supported but enum unknown", store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: boolValue(true), Modes: []store.NativeThinkingMode{{ID: "openrouter-reasoning", Control: "effort", APIFormat: store.FormatOpenAIChatCompletions}}}}, false},
		{"reasoning supported but modes unknown", store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: boolValue(true), Modes: []store.NativeThinkingMode{}}}, false},
		{"declared empty choices", store.ModelConfig{ThinkingEfforts: ptrEmptyThinkingOptions(), NativeThinking: &store.NativeThinking{Supported: boolValue(true)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s := setup(t)
			c := store.Channel{Name: "native", Enabled: true, APIKey: "synthetic", Models: []string{"up"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}, ModelConfigs: map[string]store.ModelConfig{"up": tc.cfg}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "up", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			status, data, raw := specGet(t, g, "")
			if status != 200 || len(data) != 1 {
				t.Fatal(status, raw)
			}
			item := data[0]
			options := item["thinking"].(map[string]any)["options"]
			source := item["sources"].(map[string]any)["thinking_options"]
			if tc.known {
				values, ok := options.([]any)
				if !ok || len(values) != 0 || source != "consistent_bindings" {
					t.Fatal("declared empty options lost", raw)
				}
			} else if options != nil || source != "unknown" {
				t.Fatal("unknown options became a declaration", raw)
			}
			binding := item["bindings"].([]any)[0].(map[string]any)
			if !tc.known && binding["capabilities"].(map[string]any)["thinking_options"] != nil {
				t.Fatal("binding invented an option range", raw)
			}
		})
	}
}
func ptrEmptyThinkingOptions() *[]string { v := []string{}; return &v }
