package store

// Legacy structures exist only at the data loading/import boundary.
import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"
)

type legacyPriceRange struct {
	FromTokens int64       `json:"from_tokens"`
	ToTokens   *int64      `json:"to_tokens"`
	Mode       string      `json:"mode"`
	Multiplier *float64    `json:"multiplier"`
	Rates      *PriceRates `json:"rates"`
}
type legacyPriceWindow struct {
	Start      string      `json:"start"`
	End        string      `json:"end"`
	Mode       string      `json:"mode"`
	Multiplier *float64    `json:"multiplier"`
	Rates      *PriceRates `json:"rates"`
}
type legacyPrice struct {
	ChannelID      int64               `json:"channel_id"`
	Model          string              `json:"model"`
	DisplayName    string              `json:"display_name"`
	Origin         string              `json:"origin"`
	Currency       string              `json:"currency"`
	UnitTokens     int64               `json:"unit_tokens"`
	Input          *float64            `json:"input"`
	Output         *float64            `json:"output"`
	CacheRead      *float64            `json:"cache_read"`
	CacheWrite     *float64            `json:"cache_write"`
	ContextEnabled bool                `json:"context_enabled"`
	ContextRanges  []legacyPriceRange  `json:"context_ranges"`
	TimeEnabled    bool                `json:"time_enabled"`
	TimeWindows    []legacyPriceWindow `json:"time_windows"`
}

func legacyAction(mode string, n *float64, r *PriceRates) PriceRuleAction {
	if mode == "multiplier" {
		return PriceRuleAction{Multiplier: n}
	}
	if r == nil {
		return PriceRuleAction{}
	}
	return PriceRuleAction{RateChanges: map[string]*float64{"input": r.Input, "output": r.Output, "cache_read": r.CacheRead, "cache_write": r.CacheWrite}}
}
func upgradePrice(p legacyPrice) Price {
	t := PriceTable{Currency: p.Currency, UnitTokens: p.UnitTokens, Rates: PriceRates{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}, Rules: []PriceRule{}}
	addTime := func(w legacyPriceWindow, i int) {
		draft, _ := json.Marshal(w)
		if p.TimeEnabled {
			draft = nil
		}
		t.Rules = append(t.Rules, PriceRule{Draft: draft, ID: fmt.Sprintf("legacy-time-%d", i+1), Enabled: p.TimeEnabled, When: PriceRuleWhen{TimeWindow: &PriceTimeWindow{Start: w.Start, End: w.End, Timezone: "Asia/Shanghai"}}, Action: legacyAction(w.Mode, w.Multiplier, w.Rates)})
	}
	for i, w := range p.TimeWindows {
		if w.Mode != "multiplier" {
			addTime(w, i)
		}
	}
	for i, r := range p.ContextRanges {
		draft, _ := json.Marshal(r)
		if p.ContextEnabled {
			draft = nil
		}
		t.Rules = append(t.Rules, PriceRule{Draft: draft, ID: fmt.Sprintf("legacy-context-%d", i+1), Enabled: p.ContextEnabled, When: PriceRuleWhen{InputRange: &PriceInputRange{FromTokens: r.FromTokens, ToTokens: r.ToTokens}}, Action: legacyAction(r.Mode, r.Multiplier, r.Rates)})
	}
	for i, w := range p.TimeWindows {
		if w.Mode == "multiplier" {
			addTime(w, i)
		}
	}
	return defaultPrices([]Price{{ChannelID: p.ChannelID, Model: p.Model, DisplayName: p.DisplayName, Origin: p.Origin, Selection: PriceSelection{Mode: "manual", Table: &t}}})[0]
}

// retirePriceRoles is used only at loading/import. Preserve the formerly selected
// complete entry, including broken catalog references; never resolve or guess a quote.
func retirePriceRoles(entries []json.RawMessage) ([]json.RawMessage, bool, error) {
	type candidate struct {
		raw    json.RawMessage
		entry  Price
		role   string
		legacy bool
	}
	groups := map[string][]candidate{}
	order := []string{}
	changed := false
	for _, raw := range entries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, false, err
		}
		var entry Price
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, false, err
		}
		roleRaw, legacy := fields["role"]
		role := "default"
		if legacy {
			if err := json.Unmarshal(roleRaw, &role); err != nil {
				return nil, false, err
			}
			if role == "" {
				role = "default"
			}
			if role != "default" && role != "reference" || entry.ChannelID != 0 && role != "default" {
				return nil, false, errors.New("invalid legacy price role")
			}
			delete(fields, "role")
			raw, _ = json.Marshal(fields)
			changed = true
		}
		key := fmt.Sprintf("%d/%s", entry.ChannelID, entry.Model)
		for _, old := range groups[key] {
			if !old.legacy || !legacy || old.role == role {
				return nil, false, errors.New("duplicate price identity")
			}
		}
		if len(groups[key]) == 0 {
			order = append(order, key)
		}
		groups[key] = append(groups[key], candidate{raw, entry, role, legacy})
	}
	out := make([]json.RawMessage, 0, len(order))
	for _, key := range order {
		group := groups[key]
		selected := group[0]
		for _, p := range group {
			if p.role == "default" {
				selected = p
			}
		}
		for _, p := range group {
			if p.role == "reference" && PriceConfigured(p.entry) {
				selected = p
			}
		}
		out = append(out, selected.raw)
	}
	return out, changed, nil
}

func upgradeConfigObject(v map[string]json.RawMessage) (bool, error) {
	if v == nil {
		return false, nil
	}
	changed := false
	defaults := map[string]json.RawMessage{}
	if raw := v["defaults"]; raw != nil {
		if e := json.Unmarshal(raw, &defaults); e != nil {
			return false, e
		}
	}
	for _, key := range []string{"temperature"} {
		if value, ok := v[key]; ok {
			if _, exists := defaults[key]; !exists {
				defaults[key] = value
			}
			delete(v, key)
			changed = true
		}
	}
	thinking := map[string]json.RawMessage{}
	for old, key := range map[string]string{"thinking_effort": "effort", "thinking_budget": "budget"} {
		if value, ok := v[old]; ok {
			thinking[key] = value
			delete(v, old)
			changed = true
		}
	}
	if len(thinking) > 0 {
		if _, exists := defaults["thinking"]; !exists {
			thinking["mode"] = json.RawMessage(`"auto_native"`)
			defaults["thinking"], _ = json.Marshal(thinking)
		}
	}
	if len(defaults) > 0 {
		v["defaults"], _ = json.Marshal(defaults)
	}
	return changed, nil
}
func upgradeConfigField(v map[string]json.RawMessage, key string) (bool, error) {
	raw := v[key]
	if raw == nil {
		return false, nil
	}
	var cfg map[string]json.RawMessage
	if e := json.Unmarshal(raw, &cfg); e != nil {
		return false, e
	}
	changed, e := upgradeConfigObject(cfg)
	if changed {
		v[key], e = json.Marshal(cfg)
	}
	return changed, e
}
func upgradeDocument(raw []byte, kind string) ([]byte, bool, error) {
	var v map[string]json.RawMessage
	if e := json.Unmarshal(raw, &v); e != nil {
		return nil, false, e
	}
	changed := false
	if kind == "catalog_cache" {
		if _, old := v["pricing_usd"]; old || v["pricing_cny"] != nil || v["offerings"] == nil {
			v["needs_refresh"] = json.RawMessage(`true`)
			v["context_length"] = json.RawMessage(`null`)
			v["max_input_tokens"] = json.RawMessage(`null`)
			v["max_output_tokens"] = json.RawMessage(`null`)
			v["thinking"] = json.RawMessage(`{"supported":null,"modes":[]}`)
			delete(v, "pricing_usd")
			delete(v, "pricing_cny")
			v["offerings"] = json.RawMessage(`[]`)
			changed = true
		}
	}
	for _, key := range []string{"config", "Config"} {
		c, e := upgradeConfigField(v, key)
		if e != nil {
			return nil, false, e
		}
		changed = changed || c
	}
	if raw := v["Config"]; raw != nil {
		if v["config"] == nil {
			v["config"] = raw
		}
		delete(v, "Config")
		changed = true
	}
	if kind == "channels" && v["model_configs"] != nil {
		var configs map[string]json.RawMessage
		if e := json.Unmarshal(v["model_configs"], &configs); e != nil {
			return nil, false, e
		}
		for key, raw := range configs {
			var cfg map[string]json.RawMessage
			if e := json.Unmarshal(raw, &cfg); e != nil {
				return nil, false, e
			}
			c, e := upgradeConfigObject(cfg)
			if e != nil {
				return nil, false, e
			}
			if c {
				configs[key], _ = json.Marshal(cfg)
				changed = true
			}
		}
		v["model_configs"], _ = json.Marshal(configs)
	}
	if kind == "settings" {
		if n, ok := v["max_tokens_default"]; ok {
			var cfg map[string]json.RawMessage
			_ = json.Unmarshal(v["config"], &cfg)
			if cfg == nil {
				cfg = map[string]json.RawMessage{}
			}
			var d map[string]json.RawMessage
			_ = json.Unmarshal(cfg["defaults"], &d)
			if d == nil {
				d = map[string]json.RawMessage{}
			}
			if _, exists := d["max_output_tokens"]; !exists {
				d["max_output_tokens"] = n
			}
			cfg["defaults"], _ = json.Marshal(d)
			v["config"], _ = json.Marshal(cfg)
			delete(v, "max_tokens_default")
			changed = true
		}
		if raw := v["prices"]; raw != nil {
			var entries []json.RawMessage
			if e := json.Unmarshal(raw, &entries); e != nil {
				return nil, false, e
			}
			for i, raw := range entries {
				var fields map[string]json.RawMessage
				if e := json.Unmarshal(raw, &fields); e != nil {
					return nil, false, e
				}
				if fields["selection"] == nil {
					var old legacyPrice
					if e := json.Unmarshal(raw, &old); e != nil {
						return nil, false, e
					}
					if e := validateLegacyPrice(old); e != nil {
						return nil, false, e
					}
					p := upgradePrice(old)
					if _, e := normalizePrices([]Price{p}); e != nil {
						return nil, false, fmt.Errorf("migrate price %s: %w", p.Model, e)
					}
					entries[i], _ = json.Marshal(p)
					// Very old table shapes may also carry a role; keep it until
					// the same one-time selection below has collapsed the identity.
					if role := fields["role"]; role != nil {
						var upgraded map[string]json.RawMessage
						_ = json.Unmarshal(entries[i], &upgraded)
						upgraded["role"] = role
						entries[i], _ = json.Marshal(upgraded)
					}
					changed = true
				}
			}
			entries, rolesChanged, e := retirePriceRoles(entries)
			if e != nil {
				return nil, false, e
			}
			changed = changed || rolesChanged
			v["prices"], _ = json.Marshal(entries)
		}
	}

	// T084: collapse legacy ownership once at loading/import, never at runtime.
	if kind == "settings" || kind == "channels" || kind == "downstream_models" {
		if kind == "settings" && v["request_defaults"] == nil {
			var legacy struct {
				Defaults *ModelDefaults `json:"defaults"`
			}
			_ = json.Unmarshal(v["config"], &legacy)
			d := ModelDefaults{}
			if ValidateModelDefaults(legacy.Defaults) == nil && legacy.Defaults != nil {
				d = *legacy.Defaults
			}
			v["request_defaults"], _ = json.Marshal(d)
			v["request_defaults_enabled"] = json.RawMessage(`false`)
			changed = true
		}
		for _, key := range []string{"config", "Config", "published_specs"} {
			if _, ok := v[key]; ok {
				delete(v, key)
				changed = true
			}
		}
		if kind == "channels" && v["model_configs"] != nil {
			var configs map[string]json.RawMessage
			_ = json.Unmarshal(v["model_configs"], &configs)
			for key, raw := range configs {
				var cfg ModelConfig
				if json.Unmarshal(raw, &cfg) != nil || ValidateModelConfig(cfg) != nil {
					delete(configs, key)
					changed = true
					continue
				}
				clean, _ := json.Marshal(cfg)
				var original any
				var current any
				_ = json.Unmarshal(raw, &original)
				_ = json.Unmarshal(clean, &current)
				if !reflect.DeepEqual(original, current) {
					configs[key] = clean
					changed = true
				}
			}
			v["model_configs"], _ = json.Marshal(configs)
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, e := json.Marshal(v)
	return out, true, e
}

// DecodeDownstreamModelImport converts the old backup contract once, before validation.
func DecodeDownstreamModelImport(raw []byte) (DownstreamModel, error) {
	v, _, e := upgradeDocument(raw, "downstream_models")
	if e != nil {
		return DownstreamModel{}, e
	}
	var m DownstreamModel
	e = json.Unmarshal(v, &m)
	return m, e
}
func DecodePricesImport(raw []byte) ([]Price, error) {
	envelope, _ := json.Marshal(map[string]json.RawMessage{"prices": raw})
	v, _, e := upgradeDocument(envelope, "settings")
	if e != nil {
		return nil, e
	}
	var result struct {
		Prices []Price `json:"prices"`
	}
	e = json.Unmarshal(v, &result)
	return result.Prices, e
}
func (s *Store) upgradeCatalogContracts(dir string) error {
	type change struct {
		table string
		key   any
		data  string
	}
	var changes []change
	for _, table := range []string{"channels", "settings", "downstream_models", "catalog_cache"} {
		query := "SELECT id,data FROM " + table
		if table == "catalog_cache" {
			query = "SELECT source || char(31) || id,data FROM catalog_cache"
		}
		rows, e := s.db.Query(query)
		if e != nil {
			return e
		}
		for rows.Next() {
			var key any
			var raw string
			if e = rows.Scan(&key, &raw); e != nil {
				rows.Close()
				return e
			}
			data, changed, e := upgradeDocument([]byte(raw), table)
			if e != nil {
				rows.Close()
				return fmt.Errorf("%s migration: %w", table, e)
			}
			if changed {
				changes = append(changes, change{table, key, string(data)})
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	if len(changes) == 0 {
		return nil
	}
	backupDir := filepath.Join(dir, "backups", "catalog-contract-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if e := os.MkdirAll(backupDir, 0700); e != nil {
		return e
	}
	backup := filepath.Join(backupDir, "router.db")
	if _, e := s.db.Exec("VACUUM INTO ?", backup); e != nil {
		return fmt.Errorf("migration backup: %w", e)
	}
	key, e := os.ReadFile(filepath.Join(dir, "master.key"))
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(backupDir, "master.key"), key, 0600); e != nil {
		return e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, c := range changes {
		if c.table == "catalog_cache" {
			_, e = tx.Exec("UPDATE catalog_cache SET data=? WHERE source || char(31) || id=?", c.data, c.key)
		} else {
			_, e = tx.Exec("UPDATE "+c.table+" SET data=? WHERE id=?", c.data, c.key)
		}
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}

// Ensure this file only handles boundaries, never the pricing execution path.

func validateLegacyPrice(p legacyPrice) error {
	if p.ContextEnabled {
		if len(p.ContextRanges) == 0 {
			return errors.New("enabled context pricing requires ranges")
		}
		ranges := append([]legacyPriceRange{}, p.ContextRanges...)
		sort.Slice(ranges, func(i, j int) bool { return ranges[i].FromTokens < ranges[j].FromTokens })
		for i, r := range ranges {
			if i > 0 && (ranges[i-1].ToTokens == nil || r.FromTokens <= *ranges[i-1].ToTokens) {
				return errors.New("legacy context ranges overlap")
			}
		}
	}
	if p.TimeEnabled {
		if len(p.TimeWindows) == 0 {
			return errors.New("enabled time pricing requires windows")
		}
		occupied := [1440]bool{}
		for _, w := range p.TimeWindows {
			a, e := PriceMinute(w.Start)
			if e != nil {
				return e
			}
			b, e := PriceMinute(w.End)
			if e != nil {
				return e
			}
			if a == b {
				return errors.New("time window start and end must differ")
			}
			for m := 0; m < 1440; m++ {
				if PriceWindowContains(a, b, m) {
					if occupied[m] {
						return errors.New("legacy time windows overlap")
					}
					occupied[m] = true
				}
			}
		}
	}
	return nil
}
