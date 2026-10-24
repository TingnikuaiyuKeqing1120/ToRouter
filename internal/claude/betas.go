package claude

import (
	"encoding/json"
	"strings"
)

// The fixed CLI baseline and feature betas are from CPA's pinned 2.1.280
// profile. Unknown caller betas survive; this is not capability validation.
func requestBetas(body []byte, incoming string) string {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)
	var thinking map[string]json.RawMessage
	_ = json.Unmarshal(raw["thinking"], &thinking)
	features := requestFeatures(raw)
	requested := map[string]bool{}
	for _, beta := range strings.Split(incoming, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	betas := []string{"claude-code-20250219", "oauth-2025-04-20"}
	seen := map[string]bool{betas[0]: true, betas[1]: true}
	add := func(beta string, include bool) {
		if include && !seen[beta] {
			betas = append(betas, beta)
			seen[beta] = true
		}
	}
	add("context-1m-2025-08-07", requested["context-1m-2025-08-07"])
	add("interleaved-thinking-2025-05-14", true)
	displaySet := strings.TrimSpace(rawString(thinking["display"])) != ""
	add("redact-thinking-2026-02-12", !displaySet)
	for _, beta := range []string{"thinking-token-count-2026-05-13", "context-management-2025-06-27", "prompt-caching-scope-2026-01-05"} {
		add(beta, true)
	}
	model := strings.ToLower(rawString(raw["model"]))
	legacy := strings.Contains(model, "claude-3") || strings.Contains(model, "claude-haiku-4-5") || strings.Contains(model, "claude-sonnet-4-5") || strings.Contains(model, "claude-opus-4-1") || strings.Contains(model, "claude-opus-4-2025") || strings.Contains(model, "claude-sonnet-4-2025")
	add("mid-conversation-system-2026-04-07", !legacy)
	add("per-turn-control-2026-07-01", strings.Contains(model, "opus-5-5") || strings.Contains(model, "fable-5-1") || requested["per-turn-control-2026-07-01"])
	add("timing-2026-09-09", features["timing"] || requested["timing-2026-09-09"])
	add("mid-conversation-tool-changes-2026-07-01", !legacy && model != "claude-sonnet-5")
	for _, feature := range []struct{ beta, key string }{
		{"inline-tools-2026-09-15", "tool_addition"},
		{"advisor-tool-2026-03-01", "advisor"},
		{"advanced-tool-use-2025-11-20", "defer_loading"},
		{"mid-conversation-system-clear-at-2026-08-21", "clear_at"},
		{"dangerous-tool-use-2026-09-03", "safeguards"},
	} {
		add(feature.beta, requested[feature.beta] || features[feature.key])
	}
	if features["effort"] && rawString(thinking["type"]) != "disabled" {
		add("effort-2025-11-24", true)
	}
	add("server-side-fallback-2026-06-01", raw["fallbacks"] != nil || requested["server-side-fallback-2026-06-01"])
	add("fallback-credit-2026-06-01", raw["fallbacks"] != nil || raw["fallback_credit_token"] != nil || requested["fallback-credit-2026-06-01"])
	add("structured-outputs-2025-12-15", features["format"] || requested["structured-outputs-2025-12-15"])
	add("thinking-binding-controls-2026-08-01", thinking["block_binding"] != nil || requested["thinking-binding-controls-2026-08-01"])
	add("thinking-display-updates-2026-08-18", rawString(thinking["display"]) == "updates" || requested["thinking-display-updates-2026-08-18"])
	add("thinking-resumption-2026-07-17", requested["thinking-resumption-2026-07-17"])
	add("fast-mode-2026-02-01", rawString(raw["speed"]) == "fast" || requested["fast-mode-2026-02-01"])
	add("afk-mode-2026-01-31", requested["afk-mode-2026-01-31"])
	add("extended-cache-ttl-2025-04-11", true)
	add("prompt-caching-evict-2026-05-12", features["evict_on_complete"] || requested["prompt-caching-evict-2026-05-12"])
	add("cache-diagnosis-2026-04-07", raw["diagnostics"] != nil)
	for _, beta := range strings.Split(incoming, ",") {
		// Native display and redaction are mutually exclusive (CPA SOURCE.md).
		if displaySet && strings.TrimSpace(beta) == "redact-thinking-2026-02-12" {
			continue
		}
		add(strings.TrimSpace(beta), strings.TrimSpace(beta) != "")
	}
	return strings.Join(betas, ",")
}

// Only inspect protocol slots. Tool schemas, inputs and output JSON are user
// data: a business property named "timing" must not opt into a vendor beta.
func requestFeatures(raw map[string]json.RawMessage) map[string]bool {
	features := map[string]bool{"safeguards": raw["safeguards"] != nil}
	obj := func(value json.RawMessage) map[string]json.RawMessage {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(value, &fields)
		return fields
	}
	items := func(value json.RawMessage) []map[string]json.RawMessage {
		var fields []map[string]json.RawMessage
		_ = json.Unmarshal(value, &fields)
		return fields
	}
	output := obj(raw["output_config"])
	for _, name := range []string{"timing", "effort", "format"} {
		features[name] = output[name] != nil
	}
	cache := func(fields map[string]json.RawMessage) {
		if obj(fields["cache_control"])["evict_on_complete"] != nil {
			features["evict_on_complete"] = true
		}
	}
	cache(raw)
	var content func(json.RawMessage)
	content = func(value json.RawMessage) {
		for _, block := range items(value) {
			cache(block)
			switch rawString(block["type"]) {
			case "tool_addition":
				if obj(block["tool"])["definition"] != nil {
					features["tool_addition"] = true
				}
			case "tool_result":
				content(block["content"])
			}
		}
	}
	content(raw["system"])
	for _, message := range items(raw["messages"]) {
		if message["clear_at"] != nil {
			features["clear_at"] = true
		}
		if obj(message["output_config"])["timing"] != nil {
			features["timing"] = true
		}
		content(message["content"])
	}
	for _, tool := range items(raw["tools"]) {
		kind := strings.ToLower(strings.TrimSpace(rawString(tool["type"])))
		if strings.HasPrefix(kind, "advisor_") {
			features["advisor"] = true
		}
		var deferred bool
		_ = json.Unmarshal(tool["defer_loading"], &deferred)
		if deferred || strings.HasPrefix(kind, "tool_search_tool_") || tool["input_examples"] != nil || tool["allowed_callers"] != nil {
			features["defer_loading"] = true
		}
		cache(tool)
	}
	return features
}
