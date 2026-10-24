package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"torouter/internal/store"
)

func TestCandidateDefaultsFourProtocolsAndClientPresence(t *testing.T) {
	for _, tc := range []struct{ format, protocol, body, key string }{
		{store.FormatOpenAIChatCompletions, "openai", `{"model":"alias","messages":[],"stream":false}`, "max_completion_tokens"},
		{store.FormatOpenAIResponses, "openai", `{"model":"alias","input":"hi","stream":false}`, "max_output_tokens"},
		{store.FormatAnthropicMessages, "anthropic", `{"model":"alias","messages":[],"stream":false}`, "max_tokens"},
		{store.FormatGeminiGenerateContent, "gemini", `{"contents":[]}`, "maxOutputTokens"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			cfg := store.ModelConfig{OutputTokenField: stringValue("max_completion_tokens"), MaxOutputTokens: defaultBudget(99999)}
			defaults := &store.ModelDefaults{MaxOutputTokens: defaultBudget(123), Temperature: pricingNumber(.25)}
			in := requestInfo{format: tc.format, protocol: tc.protocol, body: []byte(tc.body), model: "alias"}
			body, _, err := candidateBody(in, tc.format, "real", cfg, defaults, false)
			if err != nil {
				t.Fatal(err)
			}
			root := rawObject(body)
			params := root
			if tc.protocol == "gemini" {
				params = rawObject(root["generationConfig"])
			}
			if string(params[tc.key]) != "123" || string(params["temperature"]) != "0.25" {
				t.Fatal(string(body))
			}
			if tc.protocol != "gemini" && string(root["stream"]) != "false" {
				t.Fatal("stream changed", string(body))
			}
			for _, explicit := range []string{"0", "null"} {
				root = rawObject(in.body)
				params = root
				if tc.protocol == "gemini" {
					params = map[string]json.RawMessage{}
				}
				params[tc.key] = json.RawMessage(explicit)
				params["temperature"] = json.RawMessage(explicit)
				if tc.protocol == "gemini" {
					root["generationConfig"] = jsonValue(params)
				}
				in.body = jsonValue(root)
				body, _, err = candidateBody(in, tc.format, "real", cfg, defaults, false)
				if err != nil {
					t.Fatal(err)
				}
				root = rawObject(body)
				params = root
				if tc.protocol == "gemini" {
					params = rawObject(root["generationConfig"])
				}
				if string(params[tc.key]) != explicit || string(params["temperature"]) != explicit {
					t.Fatal("presence overwritten", string(body))
				}
			}
		})
	}
}
func TestDefaultBudgetBeforeAnthropicConversionAndZeroDisables(t *testing.T) {
	in := requestInfo{format: store.FormatOpenAIChatCompletions, protocol: "openai", model: "alias", body: []byte(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`)}
	cfg := store.ModelConfig{}
	defaults := &store.ModelDefaults{MaxOutputTokens: defaultBudget(77)}
	body, _, err := candidateBody(in, store.FormatAnthropicMessages, "real", cfg, defaults, false)
	if err != nil || string(rawObject(body)["max_tokens"]) != "77" {
		t.Fatal(string(body), err)
	}
	defaults.MaxOutputTokens = defaultBudget(0)
	if _, _, err = candidateBody(in, store.FormatAnthropicMessages, "real", cfg, defaults, false); err == nil {
		t.Fatal("disabled budget invented max_tokens")
	}
}
func TestNativeThinkingContractAndExplicitGroups(t *testing.T) {
	high := "high"
	cfg := store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: boolValue(true), Modes: []store.NativeThinkingMode{{ID: "effort", APIFormat: store.FormatOpenAIResponses, Control: "effort", Efforts: []string{"high"}}, {ID: "budget", APIFormat: store.FormatAnthropicMessages, Control: "budget"}}}}
	defaults := &store.ModelDefaults{Thinking: &store.ThinkingDefault{Mode: "auto_native", Effort: &high, Budget: defaultBudget(512)}}
	for _, format := range []string{store.FormatOpenAIResponses, store.FormatAnthropicMessages} {
		in := requestInfo{format: format, protocol: familyOf(format), model: "alias", body: []byte(`{"model":"alias","max_tokens":12}`)}
		body, _, err := candidateBody(in, format, "real", cfg, defaults, false)
		if err != nil {
			t.Fatal(err)
		}
		root := rawObject(body)
		if format == store.FormatOpenAIResponses {
			if string(rawObject(root["reasoning"])["effort"]) != `"high"` {
				t.Fatal(string(body))
			}
		} else if string(rawObject(root["thinking"])["budget_tokens"]) != "512" {
			t.Fatal(string(body))
		}
		for _, explicit := range []string{"null", `{"enabled":false}`, `{}`} {
			root = rawObject(in.body)
			root["reasoning"] = json.RawMessage(explicit)
			in.body = jsonValue(root)
			body, _, err = candidateBody(in, format, "real", cfg, defaults, false)
			if err != nil || string(rawObject(body)["reasoning"]) != explicit || present(rawObject(body), "thinking") {
				t.Fatal("client group overwritten", string(body), err)
			}
		}
	}
	cfg.NativeThinking.Modes = append(cfg.NativeThinking.Modes, store.NativeThinkingMode{ID: "other", APIFormat: store.FormatOpenAIResponses, Control: "effort"})
	_, reason := applicableThinking(defaults.Thinking, cfg.NativeThinking, store.FormatOpenAIResponses)
	if reason != "select_native_mode" {
		t.Fatal(reason)
	}
}
func TestDeclaredNativeThinkingCarriersAndExplicitOff(t *testing.T) {
	yes, no := true, false
	for _, id := range []string{"thinking_type", "enable_thinking", "openrouter-reasoning"} {
		for _, enabled := range []*bool{&yes, &no} {
			control := "enabled"
			d := &store.ThinkingDefault{Mode: id, Enabled: enabled}
			if id == "openrouter-reasoning" {
				control = "effort"
				d.Enabled = nil
				d.Effort = stringValue("high")
			}
			cfg := store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: &yes, Modes: []store.NativeThinkingMode{{ID: id, APIFormat: store.FormatOpenAIChatCompletions, Control: control}}}}
			defaults := &store.ModelDefaults{Thinking: d}
			in := requestInfo{format: store.FormatOpenAIChatCompletions, protocol: "openai", model: "alias", body: []byte(`{"model":"alias","messages":[],"stream":false}`)}
			body, reasons, err := candidateBody(in, in.format, "real", cfg, defaults, false)
			if err != nil || len(reasons) > 0 {
				t.Fatal(string(body), reasons, err)
			}
			root := rawObject(body)
			switch id {
			case "thinking_type":
				want := `"enabled"`
				if !*enabled {
					want = `"disabled"`
				}
				if string(rawObject(root["thinking"])["type"]) != want {
					t.Fatal(string(body))
				}
			case "enable_thinking":
				if string(root["enable_thinking"]) != string(jsonValue(*enabled)) {
					t.Fatal(string(body))
				}
			case "openrouter-reasoning":
				if string(rawObject(root["reasoning"])["effort"]) != `"high"` || present(root, "reasoning_effort") {
					t.Fatal(string(body))
				}
			}
			in.body = []byte(`{"model":"alias","messages":[],"enable_thinking":false,"thinking":null}`)
			body, _, err = candidateBody(in, in.format, "real", cfg, defaults, false)
			if err != nil || string(rawObject(body)["thinking"]) != "null" || present(rawObject(body), "reasoning", "reasoning_effort") {
				t.Fatal("explicit client control changed", string(body), err)
			}
		}
	}
}

func TestSIWCDefaultsRemainInapplicable(t *testing.T) {
	cfg := store.ModelConfig{}
	defaults := &store.ModelDefaults{MaxOutputTokens: defaultBudget(99), Temperature: pricingNumber(.3)}
	in := requestInfo{format: store.FormatOpenAIResponses, protocol: "openai", model: "alias", body: []byte(`{"model":"alias","input":"hello","stream":false}`)}
	body, reasons, err := candidateBody(in, in.format, "alias", cfg, defaults, true)
	if err != nil || present(rawObject(body), "max_output_tokens", "temperature") || len(reasons) != 2 {
		t.Fatal(string(body), reasons, err)
	}
	_, inapp := specDefaults(cfg, defaults, in.format, true)
	if inapp["max_output_tokens"] != "siwc" || inapp["temperature"] != "siwc" {
		t.Fatal(inapp)
	}
}
func TestFallbackReappliesGlobalDefaultsForEachNativeContract(t *testing.T) {
	g, s := setup(t)
	var observed []map[string]json.RawMessage
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		observed = append(observed, rawObject(b))
		w.WriteHeader(429)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		observed = append(observed, rawObject(b))
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)
	}))
	defer second.Close()
	add(t, s, first.URL, "openai", 0)
	add(t, s, second.URL, "openai", 1)
	channels, _ := s.ListChannels()
	for i := range channels {
		channels[i].ModelConfigs = map[string]store.ModelConfig{"test": {OutputTokenField: stringValue("max_tokens")}}
		modelCfg := channels[i].ModelConfigs["test"]
		if i == 0 {
			modelCfg.NativeThinking = &store.NativeThinking{Supported: boolValue(true), Modes: []store.NativeThinkingMode{{ID: "native-effort", APIFormat: store.FormatOpenAIChatCompletions, Control: "effort", Efforts: []string{"high"}}}}
		} else {
			modelCfg.NativeThinking = &store.NativeThinking{Supported: boolValue(true), Modes: []store.NativeThinkingMode{{ID: "thinking_budget", APIFormat: store.FormatOpenAIChatCompletions, Control: "budget"}}}
		}
		channels[i].ModelConfigs["test"] = modelCfg
		if err := s.SaveChannel(&channels[i]); err != nil {
			t.Fatal(err)
		}
	}
	settings, _ := s.GetSettings()
	settings.MaxAttempts = 2
	settings.RetriesPerChannel = 1
	settings.RequestDefaultsEnabled = true
	settings.RequestDefaults = store.ModelDefaults{MaxOutputTokens: defaultBudget(5), Temperature: pricingNumber(.1), Thinking: &store.ThinkingDefault{Mode: "auto_native", Effort: stringValue("high"), Budget: defaultBudget(512)}}
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`)
	if w.Code != 200 || len(observed) != 2 {
		t.Fatal(w.Code, len(observed), w.Body.String())
	}
	if string(observed[0]["max_tokens"]) != "5" || string(observed[1]["max_tokens"]) != "5" || string(observed[1]["temperature"]) != "0.1" {
		t.Fatal(observed)
	}
	if string(observed[0]["reasoning_effort"]) != `"high"` || present(observed[0], "thinking_budget") || string(observed[1]["thinking_budget"]) != "512" || present(observed[1], "reasoning_effort") {
		t.Fatal("thinking defaults leaked between real fallback requests", observed)
	}
}

func TestSemanticThinkingNativePayloadsAndSpecs(t *testing.T) {
	chat, responses, messages := store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses, store.FormatAnthropicMessages
	for _, tc := range []struct {
		name, format string
		mandatory    *bool
		mode         store.NativeThinkingMode
		defaults     store.ThinkingDefault
		want         string
		reason       string
	}{
		{"optional off maps none", chat, boolValue(false), store.NativeThinkingMode{ID: "reasoning", Control: "effort", Efforts: []string{"none", "high"}}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false)}, `{"reasoning_effort":"none"}`, ""},
		{"unknown off cannot infer none", chat, nil, store.NativeThinkingMode{ID: "reasoning", Control: "effort", Efforts: []string{"none", "high"}}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false)}, `{}`, "disabled_not_supported"},
		{"mandatory off forbidden", chat, boolValue(true), store.NativeThinkingMode{ID: "reasoning", Control: "effort", Efforts: []string{"none", "high"}}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false)}, `{}`, "mandatory_thinking"},
		{"mandatory none remains effort", chat, boolValue(true), store.NativeThinkingMode{ID: "reasoning", Control: "effort", Efforts: []string{"none", "high"}}, store.ThinkingDefault{Mode: "effort", Effort: stringValue("none")}, `{"reasoning_effort":"none"}`, ""},
		{"mandatory enabled is noop", responses, boolValue(true), store.NativeThinkingMode{ID: "reasoning", Control: "effort", Efforts: []string{"high"}}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true)}, `{}`, ""},
		{"optional enabled uses declared default", chat, boolValue(false), store.NativeThinkingMode{ID: "reasoning", Control: "effort", Efforts: []string{"high"}, DefaultEffort: stringValue("high")}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true)}, `{"reasoning_effort":"high"}`, ""},
		{"optional enabled needs native default", responses, boolValue(false), store.NativeThinkingMode{ID: "reasoning", Control: "effort", Efforts: []string{"high"}}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true)}, `{}`, "enabled_requires_native_effort"},
		{"qwen budget", chat, nil, store.NativeThinkingMode{ID: "thinking_budget", Control: "budget", BudgetMin: defaultBudget(0), BudgetMax: defaultBudget(1024)}, store.ThinkingDefault{Mode: "budget", Budget: defaultBudget(0)}, `{"thinking_budget":0}`, ""},
		{"qwen budget range", chat, nil, store.NativeThinkingMode{ID: "thinking_budget", Control: "budget", BudgetMax: defaultBudget(1024)}, store.ThinkingDefault{Mode: "budget", Budget: defaultBudget(1025)}, `{}`, "budget_out_of_range"},
		{"openrouter chat budget", chat, nil, store.NativeThinkingMode{ID: "openrouter-budget", Control: "budget"}, store.ThinkingDefault{Mode: "budget", Budget: defaultBudget(512)}, `{"reasoning":{"max_tokens":512}}`, ""},
		{"openrouter responses budget", responses, nil, store.NativeThinkingMode{ID: "openrouter-budget", Control: "budget"}, store.ThinkingDefault{Mode: "budget", Budget: defaultBudget(512)}, `{"reasoning":{"max_tokens":512}}`, ""},
		{"openrouter chat off", chat, boolValue(false), store.NativeThinkingMode{ID: "openrouter-enabled", Control: "enabled"}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false)}, `{"reasoning":{"enabled":false}}`, ""},
		{"openrouter responses on", responses, boolValue(false), store.NativeThinkingMode{ID: "openrouter-enabled", Control: "enabled"}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true)}, `{"reasoning":{"enabled":true}}`, ""},
		{"mimo responses on", responses, boolValue(false), store.NativeThinkingMode{ID: "mimo-effort-switch", Control: "enabled"}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true)}, `{"reasoning":{"effort":"high"}}`, ""},
		{"mimo responses off", responses, boolValue(false), store.NativeThinkingMode{ID: "mimo-effort-switch", Control: "enabled"}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false)}, `{"reasoning":{"effort":"none"}}`, ""},
		{"minimax adaptive", chat, boolValue(true), store.NativeThinkingMode{ID: "adaptive", Control: "adaptive"}, store.ThinkingDefault{Mode: "adaptive"}, `{"thinking":{"type":"adaptive"}}`, ""},
		{"minimax enable maps adaptive", chat, boolValue(true), store.NativeThinkingMode{ID: "adaptive", Control: "adaptive"}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true)}, `{"thinking":{"type":"adaptive"}}`, ""},
		{"minimax optional off maps disabled", chat, boolValue(false), store.NativeThinkingMode{ID: "adaptive", Control: "adaptive"}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false)}, `{"thinking":{"type":"disabled"}}`, ""},
		{"minimax unknown off not inferred", chat, nil, store.NativeThinkingMode{ID: "adaptive", Control: "adaptive"}, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false)}, `{}`, "disabled_not_supported"},
		{"known empty effort enum rejects high", chat, nil, store.NativeThinkingMode{ID: "reasoning", Control: "effort", Efforts: []string{}}, store.ThinkingDefault{Mode: "effort", Effort: stringValue("high")}, `{}`, "effort_not_supported"},
		{"unknown effort enum does not restrict declared control", chat, nil, store.NativeThinkingMode{ID: "reasoning", Control: "effort"}, store.ThinkingDefault{Mode: "effort", Effort: stringValue("high")}, `{"reasoning_effort":"high"}`, ""},
		{"step messages effort", messages, boolValue(true), store.NativeThinkingMode{ID: "step-effort", Control: "effort", Efforts: []string{"high"}}, store.ThinkingDefault{Mode: "effort", Effort: stringValue("high")}, `{"output_config":{"effort":"high"}}`, ""},
		{"anthropic semantic effort maps adaptive", messages, nil, store.NativeThinkingMode{ID: "adaptive", Control: "adaptive", Efforts: []string{"high"}}, store.ThinkingDefault{Mode: "effort", Effort: stringValue("high")}, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, ""},
		{"disabled effort known allowed", messages, boolValue(false), store.NativeThinkingMode{ID: "adaptive", Control: "adaptive", Efforts: []string{"low", "high"}, DisabledEfforts: []string{"low"}}, store.ThinkingDefault{Mode: "effort", Enabled: boolValue(false), Effort: stringValue("low")}, `{"thinking":{"type":"disabled"},"output_config":{"effort":"low"}}`, ""},
		{"disabled effort known forbidden", messages, boolValue(false), store.NativeThinkingMode{ID: "adaptive", Control: "adaptive", Efforts: []string{"high"}, DisabledEfforts: []string{}}, store.ThinkingDefault{Mode: "effort", Enabled: boolValue(false), Effort: stringValue("high")}, `{}`, "disabled_effort_not_supported"},
		{"unknown chat budget carrier", chat, nil, store.NativeThinkingMode{ID: "custom-budget", Control: "budget"}, store.ThinkingDefault{Mode: "custom-budget", Budget: defaultBudget(512)}, `{}`, "unknown_mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mode.APIFormat = tc.format
			cfg := store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: boolValue(true), Mandatory: tc.mandatory, Modes: []store.NativeThinkingMode{tc.mode}}}
			defaults := &store.ModelDefaults{Thinking: &tc.defaults}
			in := requestInfo{format: tc.format, protocol: familyOf(tc.format), body: []byte(`{"model":"alias","messages":[],"input":"hello","max_tokens":12}`)}
			body, reasons, err := candidateBody(in, tc.format, "real", cfg, defaults, false)
			if err != nil {
				t.Fatal(err)
			}
			_, specReasons := specDefaults(cfg, defaults, tc.format, false)
			if tc.reason != "" {
				if len(reasons) != 1 || reasons[0] != "default_inapplicable:thinking:"+tc.reason || specReasons["thinking"] != tc.reason {
					t.Fatal(string(body), reasons, specReasons)
				}
			} else if len(reasons) != 0 || len(specReasons) != 0 {
				t.Fatal(reasons, specReasons)
			}
			root := rawObject(body)
			delete(root, "model")
			delete(root, "messages")
			delete(root, "input")
			delete(root, "max_tokens")
			if string(jsonValue(root)) != string(jsonValue(rawObject([]byte(tc.want)))) {
				t.Fatalf("payload %s, want %s", jsonValue(root), tc.want)
			}
		})
	}
}

func TestSemanticThinkingSelectionAndClientControls(t *testing.T) {
	format := store.FormatOpenAIChatCompletions
	cfg := store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: boolValue(true), Mandatory: boolValue(false), Modes: []store.NativeThinkingMode{{ID: "reasoning", APIFormat: format, Control: "effort", Efforts: []string{"high"}}, {ID: "thinking_budget", APIFormat: format, Control: "budget"}}}}
	defaults := &store.ModelDefaults{Thinking: &store.ThinkingDefault{Mode: "auto_native", Effort: stringValue("high"), Budget: defaultBudget(512)}}
	in := requestInfo{format: format, protocol: "openai", body: []byte(`{"model":"alias","messages":[]}`)}
	body, reasons, err := candidateBody(in, format, "real", cfg, defaults, false)
	if err != nil || len(reasons) != 1 || reasons[0] != "default_inapplicable:thinking:select_native_mode" || present(rawObject(body), "reasoning_effort", "thinking_budget") {
		t.Fatal(string(body), reasons, err)
	}
	for _, mode := range []string{"effort", "budget", "thinking_budget"} {
		defaults.Thinking.Mode = mode
		body, reasons, err = candidateBody(in, format, "real", cfg, defaults, false)
		if err != nil || len(reasons) != 0 {
			t.Fatal(string(body), reasons, err)
		}
		root := rawObject(body)
		if mode == "effort" {
			if string(root["reasoning_effort"]) != `"high"` || present(root, "thinking_budget") {
				t.Fatal(string(body))
			}
		} else if string(root["thinking_budget"]) != "512" || present(root, "reasoning_effort") {
			t.Fatal(string(body))
		}
	}
	for _, control := range []string{`"thinking_budget":0`, `"thinking_budget":null`, `"enable_thinking":false`, `"reasoning":null`, `"reasoning":{}`, `"thinking":false`, `"output_config":null`, `"reasoning_effort":null`} {
		in.body = []byte(`{"model":"alias","messages":[],` + control + `}`)
		body, reasons, err = candidateBody(in, format, "real", cfg, defaults, false)
		if err != nil || len(reasons) != 0 {
			t.Fatal(string(body), reasons, err)
		}
		want := rawObject(in.body)
		want["model"] = jsonValue("real")
		if string(jsonValue(rawObject(body))) != string(jsonValue(want)) {
			t.Fatal("explicit control overwritten", string(body), string(jsonValue(want)))
		}
	}
}

func TestEnabledSemanticUsesNativeControlPriority(t *testing.T) {
	format := store.FormatOpenAIChatCompletions
	for _, tc := range []struct {
		name                    string
		modes                   []store.NativeThinkingMode
		wantControl, wantReason string
	}{
		{"qwen enabled wins over effort", []store.NativeThinkingMode{{ID: "effort", Control: "effort"}, {ID: "enable_thinking", Control: "enabled"}}, "enabled", ""},
		{"openrouter enabled wins over effort and budget", []store.NativeThinkingMode{{ID: "openrouter-reasoning", Control: "effort"}, {ID: "openrouter-budget", Control: "budget"}, {ID: "openrouter-enabled", Control: "enabled"}}, "enabled", ""},
		{"adaptive wins over effort", []store.NativeThinkingMode{{ID: "effort", Control: "effort"}, {ID: "adaptive", Control: "adaptive"}}, "adaptive", ""},
		{"same priority enabled controls stay ambiguous", []store.NativeThinkingMode{{ID: "enable_thinking", Control: "enabled"}, {ID: "thinking_type", Control: "enabled"}, {ID: "effort", Control: "effort"}}, "", "select_native_mode"},
		{"same priority adaptive controls stay ambiguous", []store.NativeThinkingMode{{ID: "first", Control: "adaptive"}, {ID: "second", Control: "adaptive"}, {ID: "effort", Control: "effort"}}, "", "select_native_mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.modes {
				tc.modes[i].APIFormat = format
			}
			for _, semantic := range []string{"enabled", "auto_native"} {
				for _, enabled := range []bool{true, false} {
					cfg := store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: boolValue(true), Mandatory: boolValue(false), Modes: tc.modes}}
					defaults := &store.ModelDefaults{Thinking: &store.ThinkingDefault{Mode: semantic, Enabled: &enabled}}
					mode, reason := applicableThinking(defaults.Thinking, cfg.NativeThinking, format)
					if reason != tc.wantReason || mode.Control != tc.wantControl {
						t.Fatal(semantic, enabled, mode, reason)
					}
					body, reasons, err := candidateBody(requestInfo{format: format, protocol: "openai", body: []byte(`{"model":"alias","messages":[]}`)}, format, "real", cfg, defaults, false)
					_, specReasons := specDefaults(cfg, defaults, format, false)
					if err != nil || specReasons["thinking"] != tc.wantReason {
						t.Fatal(string(body), reasons, specReasons, err)
					}
					if tc.wantReason != "" {
						if len(reasons) != 1 {
							t.Fatal(reasons)
						}
						continue
					}
					if len(reasons) != 0 || present(rawObject(body), "reasoning_effort", "thinking_budget") {
						t.Fatal(string(body), reasons)
					}
					root := rawObject(body)
					if mode.ID == "enable_thinking" {
						if string(root["enable_thinking"]) != string(jsonValue(enabled)) {
							t.Fatal(string(body))
						}
					} else if mode.ID == "openrouter-enabled" {
						if string(rawObject(root["reasoning"])["enabled"]) != string(jsonValue(enabled)) {
							t.Fatal(string(body))
						}
					} else {
						want := `"adaptive"`
						if !enabled {
							want = `"disabled"`
						}
						if string(rawObject(root["thinking"])["type"]) != want {
							t.Fatal(string(body))
						}
					}
				}
			}
		})
	}
}

func TestDeclaredAlwaysOnWithoutControlsIsOnlyEnabledNoop(t *testing.T) {
	for _, format := range []string{store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent} {
		for _, tc := range []struct {
			name       string
			mandatory  *bool
			defaults   store.ThinkingDefault
			wantReason string
		}{
			{"semantic enabled", boolValue(true), store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true)}, ""},
			{"auto enabled", boolValue(true), store.ThinkingDefault{Mode: "auto_native", Enabled: boolValue(true)}, ""},
			{"off prohibited", boolValue(true), store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(false)}, "mandatory_thinking"},
			{"unknown mandatory", nil, store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true)}, "select_native_mode"},
			{"effort has no control", boolValue(true), store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true), Effort: stringValue("high")}, "select_native_mode"},
			{"budget has no control", boolValue(true), store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(true), Budget: defaultBudget(512)}, "select_native_mode"},
			{"custom mode cannot infer control", boolValue(true), store.ThinkingDefault{Mode: "custom", Enabled: boolValue(true)}, "select_native_mode"},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				cfg := store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: boolValue(true), Mandatory: tc.mandatory, Modes: []store.NativeThinkingMode{}}}
				defaults := &store.ModelDefaults{Thinking: &tc.defaults}
				in := requestInfo{format: format, protocol: familyOf(format), body: []byte(`{"model":"alias","messages":[],"input":"hello","max_tokens":12}`)}
				body, reasons, err := candidateBody(in, format, "real", cfg, defaults, false)
				_, specReasons := specDefaults(cfg, defaults, format, false)
				if err != nil || specReasons["thinking"] != tc.wantReason {
					t.Fatal(string(body), reasons, specReasons, err)
				}
				if tc.wantReason == "" {
					if len(reasons) != 0 {
						t.Fatal(reasons)
					}
				} else if len(reasons) != 1 || reasons[0] != "default_inapplicable:thinking:"+tc.wantReason {
					t.Fatal(reasons)
				}
				if present(rawObject(body), "reasoning", "reasoning_effort", "thinking", "thinking_budget", "enable_thinking", "output_config", "generationConfig") {
					t.Fatal("always-on invented request controls", string(body))
				}
			})
		}
	}
}
func TestHTTPDefaultsFourProtocols(t *testing.T) {
	for _, tc := range []struct{ format, path, body, response, maxKey string }{
		{store.FormatOpenAIChatCompletions, "/v1/chat/completions", `{"model":"alias","messages":[]}`, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, "max_completion_tokens"},
		{store.FormatOpenAIResponses, "/v1/responses", `{"model":"alias","input":"hi"}`, `{"id":"resp1","status":"completed","object":"response","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, "max_output_tokens"},
		{store.FormatAnthropicMessages, "/v1/messages", `{"model":"alias","messages":[]}`, `{"id":"msg1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, "max_tokens"},
		{store.FormatGeminiGenerateContent, "/v1beta/models/alias:generateContent", `{"contents":[]}`, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`, "maxOutputTokens"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			g, s := setup(t)
			hits := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				body, _ := io.ReadAll(r.Body)
				root := rawObject(body)
				params := root
				if tc.format == store.FormatGeminiGenerateContent {
					params = rawObject(root["generationConfig"])
				}
				if string(params[tc.maxKey]) != "321" || string(params["temperature"]) != "0.4" {
					t.Errorf("default fields missing from real HTTP payload: %s", body)
				}
				if present(root, "stream") && string(root["stream"]) != "false" {
					t.Errorf("stream unexpectedly enabled: %s", body)
				}
				io.WriteString(w, tc.response)
			}))
			defer up.Close()
			settings, _ := s.GetSettings()
			settings.RequestDefaultsEnabled = true
			settings.RequestDefaults = store.ModelDefaults{MaxOutputTokens: defaultBudget(321), Temperature: pricingNumber(.4)}
			if err := s.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			c := store.Channel{Name: "controlled", Enabled: true, APIKey: "controlled-key", Models: []string{"real"}, ModelConfigs: map[string]store.ModelConfig{"real": {OutputTokenField: stringValue("max_completion_tokens")}}, Endpoints: []store.Endpoint{{Format: tc.format, BaseURL: up.URL}}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "alias", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			w := invoke(g, tc.path, tc.body)
			if w.Code != 200 || hits != 1 {
				t.Fatal(w.Code, hits, w.Body.String())
			}
		})
	}
}

func TestHTTPMiMoResponsesSwitchAndSpecsPreserveExplicitEffort(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(string(jsonValue(enabled)), func(t *testing.T) {
			g, s := setup(t)
			var observed map[string]json.RawMessage
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				observed = rawObject(body)
				io.WriteString(w, `{"id":"resp1","status":"completed","object":"response","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer up.Close()
			cfg := store.ModelConfig{NativeThinking: &store.NativeThinking{Supported: boolValue(true), Mandatory: boolValue(false), Modes: []store.NativeThinkingMode{{ID: "mimo-effort-switch", APIFormat: store.FormatOpenAIResponses, Control: "enabled", OutputTokenField: "max_output_tokens"}}}}
			defaults := &store.ModelDefaults{Thinking: &store.ThinkingDefault{Mode: "enabled", Enabled: boolValue(enabled)}}
			settings, _ := s.GetSettings()
			settings.RequestDefaultsEnabled = true
			settings.RequestDefaults = *defaults
			if err := s.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			c := store.Channel{Name: "MiMo controlled", Enabled: true, APIKey: "controlled-key", Models: []string{"mimo"}, ModelConfigs: map[string]store.ModelConfig{"mimo": cfg}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: up.URL}}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "alias", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "mimo", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			status, specs, raw := specGet(t, g, "?api_format=openai_responses")
			if status != http.StatusOK || len(specs) != 1 {
				t.Fatal(status, raw)
			}
			binding := specs[0]["bindings"].([]any)[0].(map[string]any)
			thinking, ok := binding["defaults"].(map[string]any)[store.FormatOpenAIResponses].(map[string]any)["thinking"].(map[string]any)
			if !ok || thinking["mode"] != "enabled" || thinking["enabled"] != enabled || thinking["effort"] != nil {
				t.Fatal("specs lost the native semantic switch", thinking)
			}
			w := invoke(g, "/v1/responses", `{"model":"alias","input":"hello"}`)
			want := "high"
			if !enabled {
				want = "none"
			}
			reasoning := rawObject(observed["reasoning"])
			if w.Code != http.StatusOK || string(reasoning["effort"]) != string(jsonValue(want)) || present(reasoning, "enabled") {
				t.Fatal("HTTP payload differs from advertised switch", w.Code, string(jsonValue(observed)), w.Body.String())
			}
			for _, effort := range []string{"none", "low", "ultra"} {
				w = invoke(g, "/v1/responses", `{"model":"alias","input":"hello","reasoning":{"effort":"`+effort+`"}}`)
				if w.Code != http.StatusOK || string(rawObject(observed["reasoning"])["effort"]) != string(jsonValue(effort)) {
					t.Fatal("explicit client effort overwritten", effort, w.Code, string(jsonValue(observed)))
				}
			}
		})
	}
}

func TestHTTPDisabledDefaultsFourProtocols(t *testing.T) {
	for _, tc := range []struct{ format, path, body, response, maxKey string }{
		{store.FormatOpenAIChatCompletions, "/v1/chat/completions", `{"model":"alias","messages":[]}`, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, "max_completion_tokens"},
		{store.FormatOpenAIResponses, "/v1/responses", `{"model":"alias","input":"hi"}`, `{"id":"resp1","status":"completed","object":"response","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, "max_output_tokens"},
		{store.FormatAnthropicMessages, "/v1/messages", `{"model":"alias","messages":[]}`, `{"id":"msg1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, "max_tokens"},
		{store.FormatGeminiGenerateContent, "/v1beta/models/alias:generateContent", `{"contents":[]}`, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`, "maxOutputTokens"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			g, s := setup(t)
			hits := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				body, _ := io.ReadAll(r.Body)
				root := rawObject(body)
				params := root
				if tc.format == store.FormatGeminiGenerateContent {
					params = rawObject(root["generationConfig"])
				}
				if present(params, tc.maxKey, "temperature", "thinking", "reasoning", "reasoning_effort", "thinkingConfig") {
					t.Errorf("disabled defaults leaked into HTTP payload: %s", body)
				}
				if present(root, "stream") && string(root["stream"]) != "false" {
					t.Errorf("stream unexpectedly enabled: %s", body)
				}
				io.WriteString(w, tc.response)
			}))
			defer up.Close()
			settings, _ := s.GetSettings()
			settings.RequestDefaultsEnabled = false
			settings.RequestDefaults = store.ModelDefaults{MaxOutputTokens: defaultBudget(321), Temperature: pricingNumber(.4)}
			if err := s.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			c := store.Channel{Name: "controlled", Enabled: true, APIKey: "controlled-key", Models: []string{"real"}, ModelConfigs: map[string]store.ModelConfig{"real": {OutputTokenField: stringValue("max_completion_tokens")}}, Endpoints: []store.Endpoint{{Format: tc.format, BaseURL: up.URL}}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "alias", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			w := invoke(g, tc.path, tc.body)
			if w.Code != 200 || hits != 1 {
				t.Fatal(w.Code, hits, w.Body.String())
			}
		})
	}
}

func TestMissingBudgetAdviceDoesNotResendOrInventDefaults(t *testing.T) {
	for _, sameFormat := range []bool{true, false} {
		t.Run(string(jsonValue(sameFormat)), func(t *testing.T) {
			g, s := setup(t)
			settings, _ := s.GetSettings()
			settings.RequestDefaultsEnabled = false
			settings.RequestDefaults = store.ModelDefaults{MaxOutputTokens: defaultBudget(8192)}
			if err := s.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			hits := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				b, _ := io.ReadAll(r.Body)
				if present(rawObject(b), "max_tokens") {
					t.Error("invented max_tokens")
				}
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"max_tokens is required"}}`)
			}))
			defer up.Close()
			add(t, s, up.URL, "anthropic", 0)
			path, body := "/v1/messages", `{"model":"test","messages":[]}`
			if !sameFormat {
				path = "/v1/chat/completions"
				body = `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
			}
			result := invoke(g, path, body)
			expectedHits := 0
			if sameFormat {
				expectedHits = 1
			}
			if result.Code != 400 || hits != expectedHits || !strings.Contains(result.Body.String(), "Configure max_tokens explicitly") {
				t.Fatal(result.Code, hits, result.Body.String())
			}
		})
	}
}
