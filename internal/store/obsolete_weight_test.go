package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Old serialized fields are ignored by the single current schema. Removing a
// routing preference must not reset credentials, identities, user overrides or
// the binding order when a saved configuration is read and edited again.
func TestObsoleteWeightsPreserveConfiguration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	c := Channel{
		Name: "kept-name", APIKey: "synthetic-retained-key", Enabled: false,
		Models: []string{"one", "two"}, AvailableModels: []string{"one", "two", "manual"},
		ManualModels: []string{"manual"},
		Endpoints:    []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.test/v1"}},
		ModelConfigs: map[string]ModelConfig{"one": {MaxOutputTokens: ptr(int64(0))}},
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	original, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := DownstreamModel{ID: "public", Enabled: true, Fallback: false,
		Bindings: []Binding{
			{ChannelID: c.ID, Model: "two", Priority: 7, Enabled: true},
			{ChannelID: c.ID, Model: "one", Priority: 2, Enabled: false},
		},
	}
	if err := s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	var channelJSON []byte
	if err := s.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&channelJSON); err != nil {
		t.Fatal(err)
	}
	var oldChannel map[string]json.RawMessage
	if err := json.Unmarshal(channelJSON, &oldChannel); err != nil {
		t.Fatal(err)
	}
	oldChannel["weight"] = json.RawMessage(`900`)
	oldChannel["priority"] = json.RawMessage(`-99`)
	channelJSON, err = json.Marshal(oldChannel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE channels SET data=? WHERE id=?", string(channelJSON), c.ID); err != nil {
		t.Fatal(err)
	}
	modelJSON, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var oldModel map[string]json.RawMessage
	if err := json.Unmarshal(modelJSON, &oldModel); err != nil {
		t.Fatal(err)
	}
	var oldBindings []map[string]json.RawMessage
	if err := json.Unmarshal(oldModel["bindings"], &oldBindings); err != nil {
		t.Fatal(err)
	}
	oldBindings[0]["weight"] = json.RawMessage(`1`)
	oldBindings[1]["weight"] = json.RawMessage(`999`)
	oldModel["bindings"], err = json.Marshal(oldBindings)
	if err != nil {
		t.Fatal(err)
	}
	modelJSON, err = json.Marshal(oldModel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE downstream_models SET data=? WHERE id=?", string(modelJSON), m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Channel(c.ID)
	if err != nil || !reflect.DeepEqual(loaded, original) {
		t.Fatalf("existing channel configuration changed after reopen: %v", err)
	}
	models, err := s.ListDownstreamModels()
	if err != nil || len(models) != 1 || !reflect.DeepEqual(models[0], m) {
		t.Fatalf("existing bindings/configuration changed after reopen: %v", err)
	}
	if err := s.SaveChannelConfig(&loaded); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(models[0]); err != nil {
		t.Fatal(err)
	}
	resaved, err := s.Channel(c.ID)
	if err != nil || !reflect.DeepEqual(resaved, original) {
		t.Fatalf("saving the current schema lost existing channel data: %v", err)
	}
	models, err = s.ListDownstreamModels()
	if err != nil || len(models) != 1 || !reflect.DeepEqual(models[0], m) {
		t.Fatalf("saving the current schema lost existing model data: %v", err)
	}
	for name, value := range map[string]any{"channel": resaved, "binding": models[0].Bindings[0]} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["weight"]; exists {
			t.Fatalf("%s still exposes obsolete weight", name)
		}
		if name == "channel" {
			if _, exists := fields["priority"]; exists {
				t.Fatal("channel still exposes obsolete display priority")
			}
		}
	}
}
