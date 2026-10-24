package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConversionDiagnosticTemplates(t *testing.T) {
	for _, code := range []string{"conversion_unsupported", "reasoning_state_unavailable", "context_unavailable", "tool_context_mismatch", "reasoning_state_rejected", "upstream_rejected", "stream_interrupted", "reasoning_state_dropped", "conversion_failed"} {
		t.Run(code, func(t *testing.T) {
			d := newConversionDiagnostic(code, "request_conversion")
			if d.Code != code || d.Message == "" || d.Source == "" || len(d.Facts) == 0 || len(d.Steps) == 0 || d.Phase != "request_conversion" {
				t.Fatalf("incomplete diagnostic: %+v", d)
			}
			for _, step := range d.Steps {
				if step.Action == "" || step.Expected == "" {
					t.Fatalf("incomplete step: %+v", step)
				}
			}
			if len(d.ActionsTaken) != 0 || d.RequestID != "" {
				t.Fatal("constructor must not invent actions or correlation")
			}
		})
	}
}

func TestConversionDiagnosticDoesNotEchoUntrustedInput(t *testing.T) {
	secret := `synthetic-secret user@example.invalid C:\private\prompt encrypted-private-material`
	d := newConversionDiagnostic(secret, secret)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"synthetic-secret", "user@example.invalid", "private", "encrypted-private-material"} {
		if strings.Contains(string(b), part) {
			t.Fatalf("untrusted input copied into diagnostic: %s", b)
		}
	}
	if d.Code != "conversion_failed" || d.Phase != "" {
		t.Fatalf("unknown inputs were echoed: %+v", d)
	}
}

func TestConversionDiagnosticJSONContract(t *testing.T) {
	d := newConversionDiagnostic("reasoning_state_rejected", "upstream_request")
	d.RequestID = "synthetic-request-id"
	d.ActionsTaken = []string{"本次未剥离。"}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"code", "source", "phase", "message", "facts", "possible_causes", "steps", "actions_taken", "settings_path", "request_id"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing JSON field %s: %s", key, b)
		}
	}
	if d.SettingsPath != "#/settings?section=conversion" {
		t.Fatalf("settings path must be navigable: %q", d.SettingsPath)
	}
	if strings.Contains(string(b), "PossibleCauses") || strings.Contains(string(b), "RequestID") {
		t.Fatalf("non snake_case JSON: %s", b)
	}
	empty, _ := json.Marshal(ConversionDiagnostic{})
	if string(empty) != "{}" {
		t.Fatalf("optional fields must be omitted: %s", empty)
	}
}

func TestConversionDiagnosticKeepsHypothesesOutOfFacts(t *testing.T) {
	for _, code := range []string{"reasoning_state_unavailable", "reasoning_state_rejected", "upstream_rejected", "stream_interrupted"} {
		d := newConversionDiagnostic(code, "stream")
		for _, fact := range d.Facts {
			for _, speculation := range []string{"密钥过期", "账号切换造成", "余额耗尽", "客户端未", "重启导致", "已执行完成", "没有执行"} {
				if strings.Contains(fact, speculation) {
					t.Fatalf("speculation in facts for %s: %s", code, fact)
				}
			}
		}
	}
	for _, code := range []string{"conversion_unsupported", "context_unavailable", "tool_context_mismatch", "upstream_rejected", "stream_interrupted", "reasoning_state_unavailable"} {
		if d := newConversionDiagnostic(code, "request_conversion"); d.SettingsPath != "" {
			t.Fatalf("unrelated failure points to thinking policy: %+v", d)
		}
	}
}
