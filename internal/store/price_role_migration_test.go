package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func rolePriceJSON(t *testing.T, p Price, role string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if role != "" {
		fields["role"], _ = json.Marshal(role)
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPriceRoleImportSelectsWholeEffectiveEntry(t *testing.T) {
	base := Price{Model: "public", DisplayName: "default name", Origin: "manual", Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000, Rates: PriceRates{Input: priceNumber(9)}, Rules: []PriceRule{}}}}
	ref := Price{Model: "public", DisplayName: "reference name", Origin: "auto", Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "CNY", UnitTokens: 100, Rates: PriceRates{Input: priceNumber(0)}, Rules: []PriceRule{{ID: "draft", Enabled: false, Action: PriceRuleAction{Multiplier: priceNumber(2)}}}, UncoveredConditions: []string{"preserved note"}}}}
	for _, tc := range []struct {
		name      string
		selection PriceSelection
		selected  bool
	}{
		{"explicit free", ref.Selection, true},
		{"empty reference", PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000000}}, false},
		{"disabled rule only", PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000000, Rules: []PriceRule{{Enabled: false}}}}, false},
		{"missing catalog quote", PriceSelection{Mode: "catalog", Ref: &CatalogPriceRef{Source: "missing", ID: "gone", OfferingKey: "region", QuoteKey: "lost"}}, true},
		{"invalid catalog reference", PriceSelection{Mode: "catalog", Ref: &CatalogPriceRef{ID: "gone"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := ref
			candidate.Selection = tc.selection
			for _, reversed := range []bool{false, true} {
				entries := []json.RawMessage{rolePriceJSON(t, base, "default"), rolePriceJSON(t, candidate, "reference")}
				if reversed {
					entries[0], entries[1] = entries[1], entries[0]
				}
				raw, _ := json.Marshal(entries)
				got, err := DecodePricesImport(raw)
				want := base
				if tc.selected {
					want = candidate
				}
				if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], want) {
					t.Fatal("selected entry changed", got, want, err)
				}
			}
		})
	}
	for _, role := range []string{"", "default", "reference"} {
		entries := []json.RawMessage{rolePriceJSON(t, base, role), rolePriceJSON(t, base, role)}
		raw, _ := json.Marshal(entries)
		if _, err := DecodePricesImport(raw); err == nil {
			t.Fatal("duplicate identity accepted", role)
		}
	}
	if _, err := normalizePrices([]Price{base, base}); err == nil {
		t.Fatal("runtime save accepted duplicate identity")
	}
}

func TestPriceRoleOpenMigrationBacksUpPreservesHistoryAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := Price{Model: "public", Origin: "manual", Selection: PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD", UnitTokens: 1000, Rates: PriceRates{Input: priceNumber(9)}}}}
	ref := Price{Model: "public", DisplayName: "selected", Origin: "auto", Selection: PriceSelection{Mode: "catalog", Ref: &CatalogPriceRef{Source: "missing", ID: "gone", OfferingKey: "region", QuoteKey: "lost"}}}
	upstream := base
	upstream.ChannelID = 7
	entries := []json.RawMessage{rolePriceJSON(t, base, "default"), rolePriceJSON(t, ref, "reference"), rolePriceJSON(t, upstream, "default")}
	var original string
	if err = s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&original); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal([]byte(original), &fields); err != nil {
		t.Fatal(err)
	}
	fields["prices"], _ = json.Marshal(entries)
	raw, _ := json.Marshal(fields)
	if _, err = s.db.Exec("UPDATE settings SET data=? WHERE id=1", string(raw)); err != nil {
		t.Fatal(err)
	}
	history := `{"model":"public","actual_cost":0,"currency":"USD","official_value":7,"official_currency":"CNY","official_pricing":{"method":"request","resolution":{"role":"unknown-old-role","layer":"downstream_reference","status":"resolved"}}}`
	if _, err = s.db.Exec("INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,7,'public',200,?)", time.Now().UnixMilli(), history); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err = Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetPrices()
		if err != nil || len(got) != 2 || !reflect.DeepEqual(got[0], ref) || !reflect.DeepEqual(got[1], defaultPrices([]Price{upstream})[0]) {
			t.Fatal(got, err)
		}
		var current, usage string
		if err = s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&current); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(current, `"role"`) {
			t.Fatal("retired role persisted", current)
		}
		if err = s.db.QueryRow("SELECT data FROM usage_logs").Scan(&usage); err != nil || usage != history {
			t.Fatal("migration rewrote historical JSON", usage, err)
		}
		rows, err := s.ListUsage(UsageFilter{})
		if err != nil || len(rows) != 1 || *rows[0].OfficialValue != 7 || *rows[0].ActualCost != 0 {
			t.Fatal("old unknown role changed amounts", rows, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := filepath.Glob(filepath.Join(dir, "backups", "catalog-contract-*", "router.db"))
	if err != nil || len(paths) != 1 {
		t.Fatal("migration repeated or missing backup", paths, err)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(paths[0]), "master.key")); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var saved string
	if err = db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&saved); err != nil || saved != string(raw) {
		t.Fatal("backup not original", err)
	}
}
