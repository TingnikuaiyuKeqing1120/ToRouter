package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestMergeFieldsPreservesBeforeGraphAndFailedInput(t *testing.T) {
	c := Channel{Name: "before", Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://trusted.invalid"}}, ModelTags: map[string][]string{"model": {"privacy:zdr"}}}
	before := c
	if err := MergeFields(&c, json.RawMessage(`{"endpoints":[{"format":"openai_chat_completions","base_url":"https://other.invalid","state":""}],"model_tags":{"model":[]}}`), "endpoints", "model_tags"); err != nil {
		t.Fatal(err)
	}
	if before.Endpoints[0].BaseURL != "https://trusted.invalid" || len(before.ModelTags["model"]) != 1 {
		t.Fatal("patch mutated the before graph")
	}
	version := EntityVersion(c)
	if err := MergeFields(&c, json.RawMessage(`{"name":42}`), "name"); err == nil {
		t.Fatal("invalid typed value accepted")
	}
	if EntityVersion(c) != version {
		t.Fatal("failed decode partially changed destination")
	}
	if err := MergeFields(&c, json.RawMessage(`{"enabled":null}`), "enabled"); err == nil {
		t.Fatal("non-nullable boolean cleared")
	}
}

func TestPortAndTagDeletionCompareInsideStore(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SavePort(17878); err != nil {
		t.Fatal(err)
	}
	old := 17878
	if err = s.SavePort(19000); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePortIfCurrent(20000, &old); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("port conflict: %v", err)
	}
	port, _ := s.Port()
	if port != 19000 {
		t.Fatal("port overwritten")
	}
	if err = s.SaveTagGroup(TagGroup{ID: "custom", Name: "Custom", Type: TagGroupTypeUser}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTag(Tag{ID: "custom:one", Name: "One", GroupID: "custom", Color: "blue"}); err != nil {
		t.Fatal(err)
	}
	state, err := s.TagDeletionState("custom:one", false)
	if err != nil {
		t.Fatal(err)
	}
	c := Channel{Name: "new reference", BaseURL: "https://example.invalid", Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}, APIKey: "synthetic", Tags: []string{"custom:one"}, Enabled: true}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteTagAtVersion("custom:one", true, state.Version); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("reference conflict: %v", err)
	}
}
