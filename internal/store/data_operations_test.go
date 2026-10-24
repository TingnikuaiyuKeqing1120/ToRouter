package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

func operationFixture(t *testing.T) (*Store, Channel, DownstreamModel) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.SaveTagGroup(TagGroup{ID: "scenario", Name: "Scenario", Type: TagGroupTypeUser}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"scenario:original", "scenario:edited"} {
		if err := s.SaveTag(Tag{ID: id, GroupID: "scenario", Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	c := Channel{Name: "upstream", APIKey: "synthetic-secret", Models: []string{"native", "other"}, Enabled: true, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.invalid"}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := DownstreamModel{ID: "old", DisplayName: "original", Enabled: true, Tags: []string{"scenario:original"}, Bindings: []Binding{{ChannelID: c.ID, Model: "native", Enabled: true}}}
	if err = s.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	return s, c, m
}

func operationSnapshot(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	result := map[string][]string{}
	for _, table := range []string{"channels", "downstream_models", "settings", "tags", "usage_logs"} {
		rows, err := s.db.Query("SELECT data FROM " + table + " ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result[table] = append(result[table], raw)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func operationPrice(model string, channelID int64) Price {
	return Price{Model: model, ChannelID: channelID, Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000, Rates: PriceRates{Input: priceNumber(0)}, Rules: []PriceRule{}}}}
}

func TestRenameModelCommitsEditAndCopiesPricesWithoutRewritingHistory(t *testing.T) {
	s, c, m := operationFixture(t)
	prices := []Price{operationPrice("old", 0), operationPrice("native", c.ID)}
	prices[0].Selection.Table.Currency = "CNY"
	if err := s.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(Usage{Model: "old", ChannelID: c.ID}); err != nil {
		t.Fatal(err)
	}
	before := operationSnapshot(t, s)
	beforePrices, _ := s.GetPrices()
	m.ID, m.DisplayName, m.SortOrder = "renamed", "edited", 17
	m.Enabled, m.Fallback = false, true
	m.Tags = []string{"scenario:edited"}
	m.Bindings[0].Model = "other"
	if err := s.RenameDownstreamModel("old", m); err != nil {
		t.Fatal(err)
	}
	models, err := s.ListDownstreamModels()
	if err != nil || len(models) != 1 || !reflect.DeepEqual(models[0], m) {
		t.Fatal("rename did not save complete edit", models, err)
	}
	after := operationSnapshot(t, s)
	if !reflect.DeepEqual(before["usage_logs"], after["usage_logs"]) || !reflect.DeepEqual(before["channels"], after["channels"]) {
		t.Fatal("rename changed historical usage or upstream")
	}
	got, err := s.GetPrices()
	if err != nil {
		t.Fatal(err)
	}
	expected := append([]Price{}, beforePrices...)
	for _, p := range beforePrices[:1] {
		p.Model = m.ID
		expected = append(expected, p)
	}
	if !reflect.DeepEqual(expected, got) {
		t.Fatal("rename failed to retain old prices and copy the official price", got)
	}
}

func TestRenameModelRejectionsAndSQLFailuresLeaveEverythingUnchanged(t *testing.T) {
	for _, stage := range []string{"model conflict", "historical price", "missing source", "invalid binding", "insert", "prices", "delete"} {
		t.Run(stage, func(t *testing.T) {
			s, _, m := operationFixture(t)
			prices := []Price{operationPrice("old", 0)}
			m.ID, m.DisplayName = "new", "edited"
			oldID := "old"
			switch stage {
			case "model conflict":
				if err := s.SaveDownstreamModel(m); err != nil {
					t.Fatal(err)
				}
			case "historical price":
				prices = append(prices, operationPrice("new", 0))
			case "missing source":
				oldID = "missing"
			case "invalid binding":
				m.Bindings[0].Model = "missing"
			}
			if err := s.SavePrices(prices); err != nil {
				t.Fatal(err)
			}
			trigger := map[string]string{
				"insert": "CREATE TRIGGER fail_insert BEFORE INSERT ON downstream_models BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END",
				"prices": "CREATE TRIGGER fail_prices BEFORE UPDATE ON settings BEGIN SELECT RAISE(ABORT, 'injected prices failure'); END",
				"delete": "CREATE TRIGGER fail_delete BEFORE DELETE ON downstream_models BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END",
			}[stage]
			if trigger != "" {
				if _, err := s.db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			before := operationSnapshot(t, s)
			err := s.RenameDownstreamModel(oldID, m)
			if err == nil {
				t.Fatal("rename unexpectedly succeeded")
			}
			if stage == "model conflict" && !errors.Is(err, ErrModelExists) {
				t.Fatal(err)
			}
			if stage == "historical price" && !errors.Is(err, ErrModelPriceConflict) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, operationSnapshot(t, s)) {
				t.Fatal("failed rename partly changed stored data")
			}
		})
	}
}

func TestDeleteTagChecksReferencesAndRollsBackEveryWrite(t *testing.T) {
	for _, stage := range []string{"unconfirmed", "channel write", "model write", "tag delete", "success"} {
		t.Run(stage, func(t *testing.T) {
			s, c, m := operationFixture(t)
			const id = "custom:tag"
			if err := s.SaveTag(Tag{ID: id, GroupID: "scenario", Name: "custom"}); err != nil {
				t.Fatal(err)
			}
			c.Tags = []string{id, "billing:payg"}
			c.ModelTags = map[string][]string{"native": {id, "privacy:zdr"}}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			m.Tags = []string{id, "scenario:original"}
			if err := s.SaveDownstreamModel(m); err != nil {
				t.Fatal(err)
			}
			trigger := map[string]string{
				"channel write": "CREATE TRIGGER fail_channel BEFORE UPDATE ON channels BEGIN SELECT RAISE(ABORT, 'injected channel failure'); END",
				"model write":   "CREATE TRIGGER fail_model BEFORE UPDATE ON downstream_models BEGIN SELECT RAISE(ABORT, 'injected model failure'); END",
				"tag delete":    "CREATE TRIGGER fail_tag BEFORE DELETE ON tags BEGIN SELECT RAISE(ABORT, 'injected tag failure'); END",
			}[stage]
			if trigger != "" {
				if _, err := s.db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			before := operationSnapshot(t, s)
			err := s.DeleteTag(id, stage != "unconfirmed")
			if stage != "success" {
				if err == nil {
					t.Fatal("expected deletion refusal/failure")
				}
				if stage == "unconfirmed" {
					var inUse *TagInUseError
					if !errors.As(err, &inUse) || len(inUse.References.ChannelNames) != 1 || len(inUse.References.DownstreamModels) != 1 {
						t.Fatal("missing reference details", err)
					}
				}
				if !reflect.DeepEqual(before, operationSnapshot(t, s)) {
					t.Fatal("failed tag deletion partly changed stored data")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			gotC, _ := s.Channel(c.ID)
			gotM, _ := s.ListDownstreamModels()
			c.Tags, c.ModelTags["native"], m.Tags = []string{"billing:payg", "scenario:unknown"}, []string{"privacy:zdr", "scenario:unknown"}, []string{"scenario:original"}
			if !reflect.DeepEqual(gotC, c) || len(gotM) != 1 || !reflect.DeepEqual(gotM[0], m) {
				t.Fatal("tag deletion changed unrelated model/channel fields")
			}
			tags, _ := s.ListTags()
			if slices.ContainsFunc(tags, func(tag Tag) bool { return tag.ID == id }) {
				t.Fatal("tag was not deleted")
			}
		})
	}
}
