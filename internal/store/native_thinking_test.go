package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeThinkingDisabledEffortsPreserveKnownEmpty(t *testing.T) {
	for _, list := range [][]string{nil, {}, {"low"}} {
		cfg := ModelConfig{NativeThinking: &NativeThinking{Modes: []NativeThinkingMode{{ID: "adaptive", APIFormat: FormatAnthropicMessages, Control: "adaptive", Efforts: []string{"low", "high"}, DisabledEfforts: list}}}}
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if list != nil && len(list) == 0 && !strings.Contains(string(b), `"disabled_efforts":[]`) {
			t.Fatal("known empty constraint omitted", string(b))
		}
		merged := ResolveConfig(cfg).Config
		got := merged.NativeThinking.Modes[0].DisabledEfforts
		if (got == nil) != (list == nil) || len(got) != len(list) {
			t.Fatalf("constraint changed: %#v -> %#v", list, got)
		}
	}
}

func TestNativeThinkingEffortsPreserveKnownEmpty(t *testing.T) {
	for _, list := range [][]string{nil, {}, {"high"}} {
		cfg := ModelConfig{NativeThinking: &NativeThinking{Modes: []NativeThinkingMode{{ID: "effort", APIFormat: FormatOpenAIChatCompletions, Control: "effort", Efforts: list}}}}
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if list != nil && len(list) == 0 && !strings.Contains(string(b), `"efforts":[]`) {
			t.Fatal("known empty effort enum omitted", string(b))
		}
		got := ResolveConfig(cfg).Config.NativeThinking.Modes[0].Efforts
		if (got == nil) != (list == nil) || len(got) != len(list) {
			t.Fatalf("effort enum changed: %#v -> %#v", list, got)
		}
	}
}

func TestNativeThinkingMetadataValidation(t *testing.T) {
	mode := NativeThinkingMode{ID: "reasoning", APIFormat: FormatOpenAIChatCompletions, Control: "effort", Efforts: []string{"none", "high"}}
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		change  func(*NativeThinkingMode)
		invalid bool
	}{
		{"declared default", func(m *NativeThinkingMode) { m.DefaultEffort = str("high") }, false},
		{"declared disabled effort", func(m *NativeThinkingMode) { m.DisabledEfforts = []string{"none"} }, false},
		{"known empty disabled efforts", func(m *NativeThinkingMode) { m.DisabledEfforts = []string{} }, false},
		{"unknown default", func(m *NativeThinkingMode) { m.DefaultEffort = str("medium") }, true},
		{"empty default", func(m *NativeThinkingMode) { m.DefaultEffort = str("") }, true},
		{"unknown disabled effort", func(m *NativeThinkingMode) { m.DisabledEfforts = []string{"medium"} }, true},
		{"duplicate disabled effort", func(m *NativeThinkingMode) { m.DisabledEfforts = []string{"none", "none"} }, true},
		{"duplicate enum", func(m *NativeThinkingMode) { m.Efforts = []string{"high", "high"} }, true},
		{"empty enum", func(m *NativeThinkingMode) { m.Efforts = []string{""} }, true},
		{"long enum", func(m *NativeThinkingMode) { m.Efforts = []string{strings.Repeat("a", 65)} }, true},
		{"long identity", func(m *NativeThinkingMode) { m.ID = strings.Repeat("a", 65) }, true},
		{"out of range budget", func(m *NativeThinkingMode) { m.BudgetMax = int64Pointer(1000000001) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mode
			tc.change(&m)
			err := ValidateModelConfig(ModelConfig{NativeThinking: &NativeThinking{Modes: []NativeThinkingMode{m}}})
			if (err != nil) != tc.invalid {
				t.Fatal(err)
			}
		})
	}
}
