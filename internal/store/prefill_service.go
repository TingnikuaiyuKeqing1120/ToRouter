package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

type PrefillTarget struct {
	Kind       string           `json:"kind"`
	ChannelID  int64            `json:"channel_id,omitempty"`
	Model      string           `json:"model"`
	Draft      *ModelConfig     `json:"draft,omitempty"`
	PriceDraft *PriceSelection  `json:"price_draft,omitempty"`
	Ref        *CatalogModelRef `json:"ref,omitempty"`
	PriceRef   *CatalogPriceRef `json:"price_ref,omitempty"`
}
type PrefillRequest struct {
	Targets        []PrefillTarget `json:"targets"`
	Groups         []string        `json:"groups"`
	ForceOverwrite bool            `json:"force_overwrite"`
	PriceMode      string          `json:"price_mode,omitempty"`
}
type PrefillPatch struct {
	Target     PrefillTarget    `json:"target"`
	Config     ModelConfig      `json:"config"`
	CatalogRef *CatalogModelRef `json:"catalog_model_ref,omitempty"`
	Price      *Price           `json:"price,omitempty"`
	Warnings   []string         `json:"warnings"`
}
type PrefillPreview struct {
	Request PrefillRequest `json:"request"`
	Patches []PrefillPatch `json:"patches"`
	Updated int            `json:"updated"`
	Skipped int            `json:"skipped"`
}
type CatalogMatch struct {
	Item      CatalogItem `json:"item"`
	Match     string      `json:"match"`
	Automatic bool        `json:"automatic"`
}

func catalogItemsTx(tx *sql.Tx) ([]CatalogItem, error) {
	out := append([]CatalogItem{}, BuiltinCatalog...)
	rows, err := tx.Query("SELECT data FROM catalog_cache ORDER BY source,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item CatalogItem
		if err = json.Unmarshal([]byte(raw), &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func catalogSettingsTx(tx *sql.Tx) (Settings, error) {
	var raw string
	var v Settings
	err := tx.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&raw)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}
func catalogMatches(items []CatalogItem, model string, ref *CatalogModelRef, priorities map[string]int) []CatalogMatch {
	matches := []CatalogMatch{}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, item := range items {
		if item.NeedsRefresh {
			continue
		}
		if ref != nil {
			if item.Source == ref.Source && item.ID == ref.ID {
				matches = append(matches, CatalogMatch{item, "linked", true})
			}
			continue
		}
		if _, ok := priorities[item.Source]; !ok {
			continue
		}
		exact := strings.EqualFold(item.ID, model)
		alias := false
		for _, a := range item.Aliases {
			if strings.EqualFold(a, model) {
				alias = true
			}
		}
		if exact || alias {
			kind := "alias"
			if exact {
				kind = "exact"
			}
			matches = append(matches, CatalogMatch{item, kind, true})
		} else if model != "" && (strings.Contains(strings.ToLower(item.ID), model) || strings.Contains(strings.ToLower(item.Name), model)) {
			matches = append(matches, CatalogMatch{item, "similar", false})
		}
	}
	slices.SortStableFunc(matches, func(a, b CatalogMatch) int {
		rank := func(m CatalogMatch) int {
			switch m.Match {
			case "linked":
				return 0
			case "exact":
				return 1
			case "alias":
				return 2
			default:
				return 3
			}
		}
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		if d := priorities[a.Item.Source] - priorities[b.Item.Source]; d != 0 {
			return d
		}
		return strings.Compare(a.Item.ID, b.Item.ID)
	})
	return matches
}
func catalogPrioritiesTx(tx *sql.Tx) (map[string]int, error) {
	out := map[string]int{}
	rows, err := tx.Query("SELECT source,priority FROM catalog_sources WHERE prefill_enabled=1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var source string
		var p int
		if err = rows.Scan(&source, &p); err != nil {
			return nil, err
		}
		out[source] = p
	}
	return out, rows.Err()
}
func (s *Store) CatalogMatches(model string, ref *CatalogModelRef) ([]CatalogMatch, error) {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	items, err := catalogItemsTx(tx)
	if err != nil {
		return nil, err
	}
	priorities, err := catalogPrioritiesTx(tx)
	if err != nil {
		return nil, err
	}
	return catalogMatches(items, model, ref, priorities), nil
}
func configFields(c ModelConfig) map[string]json.RawMessage {
	raw, _ := json.Marshal(c)
	out := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &out)
	return out
}
func configFromFields(fields map[string]json.RawMessage) (ModelConfig, error) {
	raw, err := json.Marshal(fields)
	var c ModelConfig
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	return c, err
}
func sourceConfig(item CatalogItem) ModelConfig {
	c := ModelConfig{ContextWindow: item.ContextLength, MaxInputTokens: item.MaxInputTokens, MaxOutputTokens: item.MaxOutputTokens}
	if item.OutputTokenField != "" {
		v := item.OutputTokenField
		c.OutputTokenField = &v
	}
	if item.Modalities.Input != nil {
		v := slices.Clone(item.Modalities.Input)
		c.InputModalities = &v
	}
	if item.Modalities.Output != nil {
		v := slices.Clone(item.Modalities.Output)
		c.OutputModalities = &v
	}
	if item.Thinking.Supported != nil || item.Thinking.Modes != nil {
		v := item.Thinking
		c.NativeThinking = &v
	}
	return c
}
func hasGroup(groups []string, name string) bool { return slices.Contains(groups, name) }

// Protect editing intent, including unfinished rules. Whether this table can
// currently calculate a bill is a separate question (PriceConfigured).
func priceHasPrefillContent(p Price) bool {
	if p.Selection.Mode == "catalog" && p.Selection.Ref != nil {
		return true
	}
	t := p.Selection.Table
	if t == nil {
		return false
	}
	if t.Rates.Input != nil || t.Rates.Output != nil || t.Rates.CacheRead != nil || t.Rates.CacheWrite != nil {
		return true
	}
	return len(t.Rules) > 0 || len(t.UncoveredConditions) > 0 || t.UnitTokens != 0 && t.UnitTokens != 1000000
}
func findPrice(prices []Price, channel int64, model string) *Price {
	for _, p := range prices {
		if p.ChannelID == channel && p.Model == model {
			v := p
			return &v
		}
	}
	return nil
}
func quoteForPrefill(item CatalogItem, requested *CatalogPriceRef, upstream bool, offeringKey string) (*CatalogPriceRef, *PriceTable) {
	var refs []CatalogPriceRef
	var tables []PriceTable
	for _, off := range item.Offerings {
		if offeringKey != "" && off.Key != offeringKey {
			continue
		}
		for _, q := range off.Quotes {
			if requested != nil && (requested.Source != item.Source || requested.ID != item.ID || requested.OfferingKey != off.Key || requested.QuoteKey != q.Key) {
				continue
			}
			if !upstream && q.SourceKind != "original" && q.SourceKind != "official" && q.SourceKind != "original_vendor" {
				continue
			}
			refs = append(refs, CatalogPriceRef{Source: item.Source, ID: item.ID, OfferingKey: off.Key, QuoteKey: q.Key})
			tables = append(tables, q.Table)
		}
	}
	if len(refs) != 1 {
		return nil, nil
	}
	return &refs[0], &tables[0]
}
func previewPrefillTx(tx *sql.Tx, req PrefillRequest) (PrefillPreview, error) {
	plan := PrefillPreview{Request: req, Patches: []PrefillPatch{}}
	if len(req.Targets) == 0 || len(req.Targets) > 1000 {
		return plan, errors.New("choose 1..1000 targets")
	}
	if len(req.Groups) == 0 {
		req.Groups = []string{"price"}
		if slices.ContainsFunc(req.Targets, func(t PrefillTarget) bool { return t.Kind == "upstream" }) {
			req.Groups = []string{"capabilities", "price"}
		}
		plan.Request.Groups = req.Groups
	}
	for _, g := range req.Groups {
		if !slices.Contains([]string{"capabilities", "price"}, g) {
			return plan, errors.New("unknown prefill group")
		}
	}
	if req.PriceMode == "" {
		req.PriceMode = "catalog"
		plan.Request.PriceMode = req.PriceMode
	}
	if req.PriceMode != "catalog" && req.PriceMode != "manual" {
		return plan, errors.New("price_mode must be catalog or manual")
	}
	items, err := catalogItemsTx(tx)
	if err != nil {
		return plan, err
	}
	pri, err := catalogPrioritiesTx(tx)
	if err != nil {
		return plan, err
	}
	settings, err := catalogSettingsTx(tx)
	if err != nil {
		return plan, err
	}
	seen := map[string]bool{}
	for _, target := range req.Targets {
		key := fmt.Sprintf("%s/%d/%s", target.Kind, target.ChannelID, target.Model)
		if seen[key] {
			return plan, errors.New("duplicate target")
		}
		seen[key] = true
		var cfg ModelConfig
		var linked *CatalogModelRef
		if target.Kind == "downstream" && hasGroup(req.Groups, "capabilities") {
			return plan, errors.New("public models support only price prefill")
		}
		if target.Kind != "downstream" && target.Kind != "upstream" {
			return plan, errors.New("target kind must be downstream or upstream")
		}
		if target.PriceDraft != nil && (target.Draft != nil || !hasGroup(req.Groups, "price") || hasGroup(req.Groups, "capabilities")) {
			return plan, errors.New("price drafts support only price preview")
		}
		if target.Draft != nil {
			cfg = *target.Draft
		} else if target.Kind == "downstream" {
			var raw string
			lookupErr := tx.QueryRow("SELECT data FROM downstream_models WHERE id=?", target.Model).Scan(&raw)
			if lookupErr != nil && !(target.PriceDraft != nil && errors.Is(lookupErr, sql.ErrNoRows)) {
				return plan, lookupErr
			}
			if lookupErr == nil {
				var m DownstreamModel
				if err = json.Unmarshal([]byte(raw), &m); err != nil {
					return plan, err
				}
				linked = m.CatalogModelRef
			}
		} else {
			var raw string
			lookupErr := tx.QueryRow("SELECT data FROM channels WHERE id=?", target.ChannelID).Scan(&raw)
			if lookupErr != nil && !(target.PriceDraft != nil && errors.Is(lookupErr, sql.ErrNoRows)) {
				return plan, lookupErr
			}
			if lookupErr == nil {
				var c Channel
				if err = json.Unmarshal([]byte(raw), &c); err != nil {
					return plan, err
				}
				if target.PriceDraft == nil && !slices.Contains(c.Models, target.Model) && !slices.Contains(c.AvailableModels, target.Model) {
					return plan, errors.New("upstream model is not configured or discovered")
				}
				cfg = c.ModelConfigs[target.Model]
				if v, ok := c.ModelCatalogRefs[target.Model]; ok {
					linked = &v
				}
			}
		}
		ref := target.Ref
		if ref == nil {
			ref = linked
		}
		matches := catalogMatches(items, target.Model, ref, pri)
		patch := PrefillPatch{Target: target, Config: cfg, Warnings: []string{}}
		if len(matches) == 0 || !matches[0].Automatic {
			patch.Warnings = append(patch.Warnings, "no exact identity; choose a catalog entry")
			plan.Patches = append(plan.Patches, patch)
			plan.Skipped++
			continue
		}
		if ref == nil && len(matches) > 1 && matches[1].Automatic && strings.EqualFold(matches[0].Item.ID, matches[1].Item.ID) && !strings.EqualFold(matches[0].Item.Provider, matches[1].Item.Provider) {
			patch.Warnings = append(patch.Warnings, "ambiguous manufacturer; choose an exact entry")
			plan.Patches = append(plan.Patches, patch)
			plan.Skipped++
			continue
		}
		item := matches[0].Item
		if ref != nil && ref.OfferingKey != "" && !slices.ContainsFunc(item.Offerings, func(off CatalogOffering) bool { return off.Key == ref.OfferingKey }) {
			return plan, errors.New("selected catalog region no longer exists; choose another entry")
		}
		source := sourceConfig(item)
		sourceFields := configFields(source)
		fields := configFields(cfg)
		configChanged := false
		for name, value := range sourceFields {
			if target.Kind != "upstream" || !hasGroup(req.Groups, "capabilities") {
				continue
			}
			if _, exists := fields[name]; exists && !req.ForceOverwrite {
				continue
			}
			if string(fields[name]) == string(value) {
				continue
			}
			configChanged = true
			fields[name] = value
		}
		patch.Config, err = configFromFields(fields)
		if err != nil {
			return plan, err
		}
		if err = ValidateModelConfig(patch.Config); err != nil {
			return plan, err
		}
		patch.CatalogRef = &CatalogModelRef{Source: item.Source, ID: item.ID}
		if ref != nil {
			patch.CatalogRef.OfferingKey = ref.OfferingKey
		}
		channel := target.ChannelID
		if target.Kind == "downstream" {
			channel = 0
		}
		oldPrice := findPrice(settings.Prices, channel, target.Model)
		if target.PriceDraft != nil {
			draft := Price{ChannelID: channel, Model: target.Model, Origin: "manual", Selection: *target.PriceDraft}
			validated, err := normalizePrices([]Price{draft})
			if err != nil {
				return plan, fmt.Errorf("invalid price draft: %w", err)
			}
			oldPrice = &validated[0]
		}
		if hasGroup(req.Groups, "price") && (oldPrice == nil || !priceHasPrefillContent(*oldPrice) || req.ForceOverwrite) {
			pref, quote := quoteForPrefill(item, target.PriceRef, target.Kind == "upstream", patch.CatalogRef.OfferingKey)
			if pref == nil {
				patch.Warnings = append(patch.Warnings, "choose an exact offering and quote to fill price")
			} else {
				p := Price{ChannelID: channel, Model: target.Model, Origin: "auto", DisplayName: item.Name, Selection: PriceSelection{Mode: req.PriceMode}}
				if req.PriceMode == "catalog" {
					p.Selection.Ref = pref
				} else {
					p.Selection.Table = quote
				}
				patch.Price = &p
			}
		}
		plan.Patches = append(plan.Patches, patch)
		if configChanged || patch.Price != nil || hasGroup(req.Groups, "capabilities") && !reflect.DeepEqual(linked, patch.CatalogRef) {
			plan.Updated++
		} else {
			plan.Skipped++
		}
	}
	return plan, nil
}
func (s *Store) PreviewCatalogPrefill(req PrefillRequest) (PrefillPreview, error) {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	tx, err := s.db.Begin()
	if err != nil {
		return PrefillPreview{}, err
	}
	defer tx.Rollback()
	return previewPrefillTx(tx, req)
}
func (s *Store) ApplyCatalogPrefill(req PrefillRequest) (PrefillPreview, error) {
	return s.applyCatalogPrefill(req, "")
}

// ApplyCatalogPrefillChecked compares the exact recomputed proposal inside the
// same transaction as its writes, so source refreshes cannot change an approved batch.
func (s *Store) ApplyCatalogPrefillChecked(req PrefillRequest, expected string) (PrefillPreview, error) {
	if expected == "" {
		return PrefillPreview{}, ErrEditConflict
	}
	return s.applyCatalogPrefill(req, expected)
}

func (s *Store) applyCatalogPrefill(req PrefillRequest, expected string) (PrefillPreview, error) {
	for _, t := range req.Targets {
		if t.Draft != nil || t.PriceDraft != nil {
			return PrefillPreview{}, errors.New("draft prefill must be saved by its editor")
		}
	}
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return PrefillPreview{}, err
	}
	defer tx.Rollback()
	plan, err := previewPrefillTx(tx, req)
	if err != nil {
		return plan, err
	}
	if expected != "" && EntityVersion(plan) != expected {
		return plan, ErrEditConflict
	}
	settings, err := catalogSettingsTx(tx)
	if err != nil {
		return plan, err
	}
	pricesChanged := false
	for _, p := range plan.Patches {
		if p.CatalogRef == nil {
			continue
		}
		if p.Target.Kind == "upstream" && hasGroup(plan.Request.Groups, "capabilities") {
			var raw string
			if err = tx.QueryRow("SELECT data FROM channels WHERE id=?", p.Target.ChannelID).Scan(&raw); err != nil {
				return plan, err
			}
			fields := map[string]json.RawMessage{}
			if err = json.Unmarshal([]byte(raw), &fields); err != nil {
				return plan, err
			}
			configs := map[string]ModelConfig{}
			_ = json.Unmarshal(fields["model_configs"], &configs)
			if configs == nil {
				configs = map[string]ModelConfig{}
			}
			configs[p.Target.Model] = p.Config
			fields["model_configs"], _ = json.Marshal(configs)
			refs := map[string]*CatalogModelRef{}
			_ = json.Unmarshal(fields["model_catalog_refs"], &refs)
			if refs == nil {
				refs = map[string]*CatalogModelRef{}
			}
			refs[p.Target.Model] = p.CatalogRef
			fields["model_catalog_refs"], _ = json.Marshal(refs)
			body, _ := json.Marshal(fields)
			if _, err = tx.Exec("UPDATE channels SET data=? WHERE id=?", string(body), p.Target.ChannelID); err != nil {
				return plan, err
			}
		}
		if p.Price != nil {
			found := false
			for i, old := range settings.Prices {
				if old.ChannelID == p.Price.ChannelID && old.Model == p.Price.Model {
					settings.Prices[i] = *p.Price
					found = true
					break
				}
			}
			if !found {
				settings.Prices = append(settings.Prices, *p.Price)
			}
			pricesChanged = true
		}
	}
	if pricesChanged {
		settings.Prices, err = normalizePrices(settings.Prices)
		if err != nil {
			return plan, err
		}
		raw, _ := json.Marshal(settings)
		if _, err = tx.Exec("UPDATE settings SET data=? WHERE id=1", string(raw)); err != nil {
			return plan, err
		}
	}
	if err = tx.Commit(); err != nil {
		return plan, err
	}
	return plan, nil
}
