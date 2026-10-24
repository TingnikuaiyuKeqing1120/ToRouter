package gateway

import (
	"encoding/json"
	"slices"
	"strings"
	"torouter/internal/gateway/convert"
	"torouter/internal/store"
)

// candidateBody always starts with the original client request. No candidate
// can inherit parameters injected for an earlier fallback candidate.
func candidateBody(in requestInfo, to, model string, cfg store.ModelConfig, d *store.ModelDefaults, siwc bool, options ...convert.Options) ([]byte, []string, error) {
	patched := in
	var root map[string]json.RawMessage
	if err := json.Unmarshal(in.body, &root); err != nil {
		return nil, nil, err
	}
	var reasons []string
	if d != nil {
		changed := false
		containerPresent := in.format == store.FormatGeminiGenerateContent && present(root, "generationConfig") && !isRawObject(root["generationConfig"])
		params := root
		if in.format == store.FormatGeminiGenerateContent {
			params = rawObject(root["generationConfig"])
		}
		maxKey := outputKey(in.format, cfg)
		if in.format == store.FormatOpenAIChatCompletions && to != in.format {
			maxKey = "max_tokens"
		}
		if to == store.FormatOpenAIChatCompletions && outputKey(to, cfg) == "" {
			maxKey = ""
		}
		maxPresent := present(params, "max_tokens", "max_completion_tokens", "max_output_tokens", "maxOutputTokens")
		if containerPresent {
			maxPresent = true
		}
		if d.MaxOutputTokens != nil && *d.MaxOutputTokens > 0 && !maxPresent {
			if siwc {
				reasons = append(reasons, "default_inapplicable:max_output_tokens:siwc")
			} else if maxKey == "" {
				reasons = append(reasons, "default_inapplicable:max_output_tokens:unknown_field")
			} else {
				params[maxKey] = jsonValue(*d.MaxOutputTokens)
				changed = true
			}
		}
		if d.Temperature != nil && !present(params, "temperature") && !containerPresent {
			if siwc {
				reasons = append(reasons, "default_inapplicable:temperature:siwc")
			} else if to == store.FormatAnthropicMessages && *d.Temperature > 1 {
				reasons = append(reasons, "default_inapplicable:temperature:outside_native_range")
			} else {
				params["temperature"] = jsonValue(*d.Temperature)
				changed = true
			}
		}
		if changed && in.format == store.FormatGeminiGenerateContent && !present(root, "generationConfig") && len(params) > 0 {
			root["generationConfig"] = jsonValue(params)
		} else if changed && in.format == store.FormatGeminiGenerateContent && string(root["generationConfig"]) != "null" {
			root["generationConfig"] = jsonValue(params)
		}
		if changed {
			patched.body = jsonValue(root)
		}
	}
	body, adapted, err := outboundBody(patched, to, model, options...)
	if err != nil {
		return nil, reasons, err
	}
	if in.format != to && to == store.FormatOpenAIChatCompletions && outputKey(to, cfg) == "max_completion_tokens" {
		target := rawObject(body)
		if v, ok := target["max_tokens"]; ok {
			delete(target, "max_tokens")
			target["max_completion_tokens"] = v
			body = jsonValue(target)
		}
	}
	reasons = append(reasons, adapted...)
	if d == nil || d.Thinking == nil || thinkingPresent(root, in.format) {
		return body, reasons, nil
	}
	thinking, mode, why := normalizeThinking(d.Thinking, cfg.NativeThinking, to)
	if why != "" {
		return body, append(reasons, "default_inapplicable:thinking:"+why), nil
	}
	var target map[string]json.RawMessage
	_ = json.Unmarshal(body, &target)
	applyThinking(target, to, thinking, mode)
	return jsonValue(target), reasons, nil
}

func jsonValue(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func isRawObject(b json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}
func rawObject(b json.RawMessage) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m
}
func present(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}
func outputKey(format string, cfg store.ModelConfig) string {
	switch format {
	case store.FormatOpenAIChatCompletions:
		if cfg.OutputTokenField != nil && (*cfg.OutputTokenField == "max_tokens" || *cfg.OutputTokenField == "max_completion_tokens") {
			return *cfg.OutputTokenField
		}
		field := ""
		if cfg.NativeThinking != nil {
			for _, m := range cfg.NativeThinking.Modes {
				if m.APIFormat != format || m.OutputTokenField == "" {
					continue
				}
				if m.OutputTokenField != "max_tokens" && m.OutputTokenField != "max_completion_tokens" {
					return ""
				}
				if field != "" && field != m.OutputTokenField {
					return ""
				}
				field = m.OutputTokenField
			}
		}
		if field != "" {
			return field
		}
		// Cross-format basic parameters use the converter's portable IR field.
		return ""
	case store.FormatOpenAIResponses:
		return "max_output_tokens"
	case store.FormatAnthropicMessages:
		return "max_tokens"
	case store.FormatGeminiGenerateContent:
		return "maxOutputTokens"
	}
	return ""
}
func thinkingPresent(root map[string]json.RawMessage, format string) bool {
	if present(root, "thinking", "thinking_budget", "reasoning", "reasoning_effort", "enable_thinking", "output_config") {
		return true
	}
	if format == store.FormatGeminiGenerateContent {
		if present(root, "generationConfig") && !isRawObject(root["generationConfig"]) {
			return true
		}
		return present(rawObject(root["generationConfig"]), "thinkingConfig")
	}
	return false
}
func applicableThinking(d *store.ThinkingDefault, n *store.NativeThinking, format string) (store.NativeThinkingMode, string) {
	_, mode, reason := normalizeThinking(d, n, format)
	return mode, reason
}

// Selection and normalization are shared by request injection and specs. Native
// enums are not translated between vendors, and missing defaults stay unknown.
func normalizeThinking(d *store.ThinkingDefault, n *store.NativeThinking, format string) (*store.ThinkingDefault, store.NativeThinkingMode, string) {
	invalid := func(reason string) (*store.ThinkingDefault, store.NativeThinkingMode, string) {
		return nil, store.NativeThinkingMode{}, reason
	}
	if d == nil {
		return invalid("missing_default")
	}
	if n == nil || n.Supported == nil {
		return invalid("unknown_contract")
	}
	if !*n.Supported {
		return invalid("unsupported")
	}
	if d.Enabled != nil && !*d.Enabled && n.Mandatory != nil && *n.Mandatory {
		return invalid("mandatory_thinking")
	}
	if len(n.Modes) == 0 && n.Mandatory != nil && *n.Mandatory && d.Enabled != nil && *d.Enabled && d.Effort == nil && d.Budget == nil && (d.Mode == "enabled" || d.Mode == "auto_native") {
		t := *d
		return &t, store.NativeThinkingMode{}, ""
	}
	var matches []store.NativeThinkingMode
	preferEnabled := d.Mode == "enabled" || d.Mode == "auto_native" && d.Enabled != nil && d.Effort == nil && d.Budget == nil
	bestPriority := 4
	for _, m := range n.Modes {
		if m.APIFormat == format && thinkingModeMatches(d, m, format) {
			if preferEnabled {
				priority := slices.Index([]string{"enabled", "adaptive", "effort", "budget"}, m.Control)
				if priority > bestPriority {
					continue
				}
				if priority < bestPriority {
					matches = nil
					bestPriority = priority
				}
			}
			matches = append(matches, m)
		}
	}
	if len(matches) != 1 {
		return invalid("select_native_mode")
	}
	m := matches[0]
	t := *d
	// A defaults group may carry values for different protocol candidates.
	// Only the selected native control consumes its matching parameter.
	if m.Control == "effort" || m.Control == "adaptive" {
		t.Budget = nil
	} else if m.Control == "budget" {
		t.Effort = nil
	} else if m.Control == "enabled" {
		t.Effort = nil
		t.Budget = nil
	}
	disabled := t.Enabled != nil && !*t.Enabled
	if m.Control == "effort" {
		if disabled {
			// An effort called none is only an off switch with an explicit
			// optional-thinking declaration. Mandatory models may still use
			// none as a minimal reasoning level.
			if n.Mandatory == nil || *n.Mandatory || !slices.Contains(m.Efforts, "none") {
				return invalid("disabled_not_supported")
			}
			if t.Effort != nil && *t.Effort != "none" {
				return invalid("disabled_effort_not_supported")
			}
			effort := "none"
			t.Effort = &effort
		} else if t.Effort == nil {
			if t.Enabled == nil || !*t.Enabled {
				return invalid("effort_required")
			}
			if m.DefaultEffort != nil && slices.Contains(m.Efforts, *m.DefaultEffort) && (*m.DefaultEffort != "none" || n.Mandatory != nil && *n.Mandatory) {
				t.Effort = m.DefaultEffort
			} else if n.Mandatory == nil || !*n.Mandatory {
				return invalid("enabled_requires_native_effort")
			}
		}
	}
	if t.Effort != nil && m.Control != "effort" && m.Control != "adaptive" {
		return invalid("effort_not_supported")
	}
	if t.Effort != nil && m.Control == "adaptive" && format == store.FormatOpenAIChatCompletions {
		return invalid("effort_not_supported")
	}
	if t.Effort != nil && m.Efforts != nil && !slices.Contains(m.Efforts, *t.Effort) {
		return invalid("effort_not_supported")
	}
	if disabled && t.Effort != nil && m.DisabledEfforts != nil && !slices.Contains(m.DisabledEfforts, *t.Effort) {
		return invalid("disabled_effort_not_supported")
	}
	if t.Budget != nil && m.Control != "budget" {
		return invalid("budget_not_supported")
	}
	if m.Control == "budget" {
		if t.Budget == nil && !disabled {
			return invalid("budget_required")
		}
		if t.Budget != nil && (m.BudgetMin != nil && *t.Budget < *m.BudgetMin || m.BudgetMax != nil && *t.Budget > *m.BudgetMax) {
			return invalid("budget_out_of_range")
		}
	}
	if m.Control == "enabled" && t.Enabled == nil {
		return invalid("enabled_required")
	}
	if !thinkingMappingKnown(m, format) {
		return invalid("unknown_mapping")
	}
	if disabled && m.Control != "effort" && format != store.FormatAnthropicMessages && m.Control != "enabled" {
		if format != store.FormatOpenAIChatCompletions || m.Control != "adaptive" || n.Mandatory == nil || *n.Mandatory {
			return invalid("disabled_not_supported")
		}
	}
	return &t, m, ""
}

func thinkingModeMatches(d *store.ThinkingDefault, m store.NativeThinkingMode, format string) bool {
	switch d.Mode {
	case "effort":
		return d.Effort != nil && (m.Control == "effort" || m.Control == "adaptive" && len(m.Efforts) > 0)
	case "budget":
		return m.Control == "budget"
	case "adaptive":
		return m.Control == "adaptive"
	case "enabled":
		return d.Enabled != nil && (m.Control == "enabled" || m.Control == "adaptive" || m.Control == "effort" || m.Control == "budget" && !*d.Enabled && format == store.FormatAnthropicMessages)
	case "auto_native":
		switch m.Control {
		case "effort":
			return d.Effort != nil || d.Enabled != nil
		case "budget":
			return d.Budget != nil || d.Enabled != nil && !*d.Enabled && format == store.FormatAnthropicMessages
		case "enabled":
			return d.Enabled != nil
		case "adaptive":
			return d.Effort != nil || d.Enabled != nil
		}
		return false
	default:
		return d.Mode == m.ID
	}
}

func thinkingMappingKnown(m store.NativeThinkingMode, format string) bool {
	switch format {
	case store.FormatOpenAIChatCompletions:
		return m.Control == "effort" || m.Control == "adaptive" || m.Control == "enabled" && slices.Contains([]string{"thinking_type", "enable_thinking", "openrouter-enabled"}, m.ID) || m.Control == "budget" && slices.Contains([]string{"thinking_budget", "openrouter-budget"}, m.ID)
	case store.FormatOpenAIResponses:
		return m.Control == "effort" || m.Control == "budget" && m.ID == "openrouter-budget" || m.Control == "enabled" && slices.Contains([]string{"openrouter-enabled", "mimo-effort-switch"}, m.ID)
	case store.FormatAnthropicMessages:
		return slices.Contains([]string{"effort", "budget", "enabled", "adaptive"}, m.Control)
	case store.FormatGeminiGenerateContent:
		return m.Control == "effort" || m.Control == "budget"
	}
	return false
}
func applyThinking(root map[string]json.RawMessage, format string, d *store.ThinkingDefault, m store.NativeThinkingMode) {
	if m.Control == "" || m.Control == "effort" && d.Effort == nil {
		return // Enabling explicitly mandatory thinking is a valid no-op.
	}
	switch format {
	case store.FormatOpenAIChatCompletions:
		switch m.Control {
		case "enabled":
			if m.ID == "enable_thinking" {
				root["enable_thinking"] = jsonValue(*d.Enabled)
			} else if m.ID == "openrouter-enabled" {
				root["reasoning"] = jsonValue(map[string]any{"enabled": *d.Enabled})
			} else {
				kind := "enabled"
				if !*d.Enabled {
					kind = "disabled"
				}
				root["thinking"] = jsonValue(map[string]any{"type": kind})
			}
		case "budget":
			if m.ID == "thinking_budget" {
				root["thinking_budget"] = jsonValue(*d.Budget)
			} else {
				root["reasoning"] = jsonValue(map[string]any{"max_tokens": *d.Budget})
			}
		case "adaptive":
			kind := "adaptive"
			if d.Enabled != nil && !*d.Enabled {
				kind = "disabled"
			}
			root["thinking"] = jsonValue(map[string]any{"type": kind})
		case "effort":
			if m.ID == "openrouter-reasoning" {
				root["reasoning"] = jsonValue(map[string]any{"effort": *d.Effort})
			} else {
				root["reasoning_effort"] = jsonValue(*d.Effort)
			}
		}
	case store.FormatOpenAIResponses:
		switch m.Control {
		case "effort":
			root["reasoning"] = jsonValue(map[string]any{"effort": *d.Effort})
		case "budget":
			root["reasoning"] = jsonValue(map[string]any{"max_tokens": *d.Budget})
		case "enabled":
			if m.ID == "mimo-effort-switch" {
				effort := "high"
				if !*d.Enabled {
					effort = "none"
				}
				root["reasoning"] = jsonValue(map[string]any{"effort": effort})
			} else {
				root["reasoning"] = jsonValue(map[string]any{"enabled": *d.Enabled})
			}
		}
	case store.FormatAnthropicMessages:
		if m.Control == "effort" {
			root["output_config"] = jsonValue(map[string]any{"effort": *d.Effort})
			return
		}
		t := map[string]any{"type": "enabled"}
		if m.Control == "adaptive" && d.Effort != nil {
			root["output_config"] = jsonValue(map[string]any{"effort": *d.Effort})
		}
		if d.Enabled != nil && !*d.Enabled {
			t["type"] = "disabled"
		} else if m.Control == "adaptive" {
			t["type"] = "adaptive"
		} else if m.Control == "budget" {
			t["budget_tokens"] = d.Budget
		}
		root["thinking"] = jsonValue(t)
	case store.FormatGeminiGenerateContent:
		gc := rawObject(root["generationConfig"])
		t := map[string]any{}
		switch m.Control {
		case "budget":
			t["thinkingBudget"] = d.Budget
		case "effort":
			t["thinkingLevel"] = d.Effort
		case "enabled":
			t["includeThoughts"] = d.Enabled
		}
		gc["thinkingConfig"] = jsonValue(t)
		root["generationConfig"] = jsonValue(gc)
	}
}

// Advice names only parameters whose absence or applicability is known. It does
// not expose the upstream error body or alter the request for another attempt.
func clientConfigurationAdvice(in requestInfo, failure string, reasons []string) string {
	lower := strings.ToLower(failure)
	missingBudget := (strings.Contains(lower, "max_tokens") || strings.Contains(lower, "max_output_tokens") || strings.Contains(lower, "maxoutputtokens")) && (strings.Contains(lower, "required") || strings.Contains(lower, "requires") || strings.Contains(lower, "missing"))
	if missingBudget {
		field := "max_tokens"
		switch in.format {
		case store.FormatOpenAIResponses:
			field = "max_output_tokens"
		case store.FormatGeminiGenerateContent:
			field = "generationConfig.maxOutputTokens"
		}
		return "Configure " + field + " explicitly in the client request; no applicable gateway output budget was supplied"
	}
	for _, reason := range reasons {
		if strings.HasPrefix(reason, "default_inapplicable:") {
			return "Configure the affected request parameter explicitly in the client using this upstream model's declared native mapping (" + reason + ")"
		}
	}
	return ""
}
