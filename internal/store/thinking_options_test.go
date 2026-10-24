package store

import (
	"reflect"
	"testing"
)

func TestDeclaredThinkingOptionsPreserveRangeAndNativeFacts(t *testing.T) {
	mandatory := true
	field := "high"
	native := &NativeThinking{Supported: ptr(true), Mandatory: &mandatory, Modes: []NativeThinkingMode{{ID: "effort", Control: "effort", APIFormat: FormatOpenAIChatCompletions, Efforts: []string{"low", "high"}, DefaultEffort: &field}}}
	declared := []string{"adaptive", "custom"}
	for _, options := range [][]string{declared, {}} {
		got, known := ThinkingOptions(ModelConfig{NativeThinking: native, ThinkingEfforts: &options})
		if !known || got == nil || !reflect.DeepEqual(got, options) {
			t.Fatalf("declared range changed: %#v, %v", got, known)
		}
	}
	got, known := ThinkingOptions(ModelConfig{NativeThinking: native})
	if !known || !reflect.DeepEqual(got, []string{"enable", "low", "high"}) || *native.Mandatory != true || !reflect.DeepEqual(native.Modes[0].Efforts, []string{"low", "high"}) {
		t.Fatalf("native facts changed or off invented: %#v", got)
	}
	if got, known := ThinkingOptions(ModelConfig{}); known || got != nil {
		t.Fatal("unknown option range became an empty declaration")
	}
}

func TestNativeThinkingOptionsRequireKnownRange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		native NativeThinking
		known  bool
		want   []string
	}{
		{"supported only", NativeThinking{Supported: ptr(true)}, false, nil},
		{"empty modes", NativeThinking{Supported: ptr(true), Modes: []NativeThinkingMode{}}, false, nil},
		{"mandatory without modes", NativeThinking{Supported: ptr(true), Mandatory: ptr(true)}, false, nil},
		{"unknown effort enum", NativeThinking{Supported: ptr(true), Modes: []NativeThinkingMode{{Control: "effort"}}}, false, nil},
		{"default is not enum", NativeThinking{Supported: ptr(true), Modes: []NativeThinkingMode{{Control: "effort", DefaultEffort: ptr("high")}}}, false, nil},
		{"unknown alongside known", NativeThinking{Supported: ptr(true), Modes: []NativeThinkingMode{{Control: "enabled"}, {Control: "effort"}}}, false, nil},
		{"explicit empty enum", NativeThinking{Supported: ptr(true), Modes: []NativeThinkingMode{{Control: "effort", Efforts: []string{}}}}, true, []string{}},
		{"unsupported", NativeThinking{Supported: ptr(false)}, true, []string{}},
		{"explicit switch", NativeThinking{Supported: ptr(true), Mandatory: ptr(false), Modes: []NativeThinkingMode{{Control: "enabled"}}}, true, []string{"enable", "disable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := ThinkingOptions(ModelConfig{NativeThinking: &tc.native})
			if known != tc.known || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("range %#v known=%v; want %#v known=%v", got, known, tc.want, tc.known)
			}
		})
	}
}
