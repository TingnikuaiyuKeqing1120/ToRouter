package store

import "testing"

func TestPriceLastRowCanBeDeletedWithoutRemovingOtherModels(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Channel{Name: "test", Enabled: true, Models: []string{"a", "b"}, APIKey: "fixture-key", Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.com"}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	rate := 1.0
	table := PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: PriceRates{Input: &rate}, Rules: []PriceRule{}}
	rows := []Price{{ChannelID: c.ID, Model: "a", Selection: PriceSelection{Mode: "manual", Table: &table}}, {ChannelID: c.ID, Model: "b", Selection: PriceSelection{Mode: "manual", Table: &table}}}
	if err = s.SavePrices(rows); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePrices(rows[1:]); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPrices()
	if err != nil || len(got) != 1 || got[0].Model != "b" {
		t.Fatalf("group deletion lost other prices: %+v %v", got, err)
	}
	saved, err := s.Channel(c.ID)
	if err != nil || saved.APIKey != "fixture-key" {
		t.Fatal("price deletion changed credentials", err)
	}
}
