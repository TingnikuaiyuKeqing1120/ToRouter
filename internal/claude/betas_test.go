package claude

import (
	"strings"
	"testing"
)

func TestFeatureBetasDistinguishProtocolFieldsFromToolData(t *testing.T) {
	data := `{"model":"claude-opus-4-8","output_config":{},"tools":[{"name":"fixture","defer_loading":false,"input_schema":{"properties":{"timing":{},"effort":{},"format":{},"advisor":{},"defer_loading":{},"safeguards":{},"clear_at":{},"evict_on_complete":{}}}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"fixture-call","name":"fixture","input":{"type":"tool_addition","tool":{"definition":{}},"timing":0,"effort":"high","advisor":true,"defer_loading":true,"safeguards":{},"clear_at":1,"evict_on_complete":true}}]}]}`
	for _, beta := range []string{"timing-2026-09-09", "effort-2025-11-24", "structured-outputs-2025-12-15", "advisor-tool-2026-03-01", "advanced-tool-use-2025-11-20", "inline-tools-2026-09-15", "dangerous-tool-use-2026-09-03", "mid-conversation-system-clear-at-2026-08-21", "prompt-caching-evict-2026-05-12"} {
		if strings.Contains(requestBetas([]byte(data), ""), beta) {
			t.Errorf("business data enabled %s", beta)
		}
	}
	for _, tc := range []struct{ body, beta string }{
		{`{"output_config":{"timing":{}}}`, "timing-2026-09-09"},
		{`{"output_config":{"effort":"high"}}`, "effort-2025-11-24"},
		{`{"output_config":{"format":{"type":"json_schema"}}}`, "structured-outputs-2025-12-15"},
		{`{"tools":[{"type":"advisor_fixture"}]}`, "advisor-tool-2026-03-01"},
		{`{"tools":[{"defer_loading":true}]}`, "advanced-tool-use-2025-11-20"},
		{`{"tools":[{"type":"tool_search_tool_fixture"}]}`, "advanced-tool-use-2025-11-20"},
		{`{"tools":[{"input_examples":[]}]}`, "advanced-tool-use-2025-11-20"},
		{`{"messages":[{"content":[{"type":"tool_addition","tool":{"definition":{}}}]}]}`, "inline-tools-2026-09-15"},
		{`{"safeguards":{}}`, "dangerous-tool-use-2026-09-03"},
		{`{"messages":[{"clear_at":1}]}`, "mid-conversation-system-clear-at-2026-08-21"},
		{`{"messages":[{"content":[{"type":"text","text":"fixture","cache_control":{"evict_on_complete":true}}]}]}`, "prompt-caching-evict-2026-05-12"},
	} {
		if !strings.Contains(requestBetas([]byte(tc.body), ""), tc.beta) {
			t.Errorf("protocol field lost %s", tc.beta)
		}
	}
}

func TestFeatureBetasPreserveExplicitUnknownAndDisplayIntent(t *testing.T) {
	got := requestBetas([]byte(`{"thinking":{"display":"summarized"}}`), "fixture-extra,redact-thinking-2026-02-12,fixture-extra")
	if strings.Count(got, "fixture-extra") != 1 || strings.Contains(got, "redact-thinking") {
		t.Fatal("explicit display or caller feature lost")
	}
	if !strings.Contains(requestBetas([]byte(`{"thinking":{"display":null}}`), ""), "redact-thinking") {
		t.Fatal("null display unexpectedly changed baseline")
	}
}
