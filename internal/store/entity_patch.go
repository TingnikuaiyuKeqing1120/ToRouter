package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

var ErrEditConflict = errors.New("configuration changed; read the current version and retry")

// EntityVersion is an opaque optimistic concurrency token, never a secret projection.
func EntityVersion(value any) string {
	b, _ := json.Marshal(value)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// MergeFields applies only explicitly supplied fields and rejects unknown fields.
// Arrays replace arrays; objects merge; null clears only nullable fields.
func MergeFields(dst any, patch json.RawMessage, allowed ...string) (err error) {
	defer func() {
		if err != nil {
			var known *ConfigurationError
			if !errors.As(err, &known) {
				err = NewConfigurationError(err.Error())
			}
		}
	}()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(patch, &fields); err != nil || fields == nil {
		return NewConfigurationError("patch must be a JSON object")
	}
	allow := map[string]bool{}
	for _, k := range allowed {
		allow[k] = true
	}
	typ := reflect.TypeOf(dst).Elem()
	if err := checkPatchFields(fields, typ, allow); err != nil {
		return err
	}
	base, err := json.Marshal(dst)
	if err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err = json.Unmarshal(base, &object); err != nil {
		return err
	}
	mergeJSONFields(object, fields)
	b, err := json.Marshal(object)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	// Decode into a new graph. Decoding over dst reuses slice backing arrays and
	// maps, which would mutate the "before" snapshot used by destination checks.
	next := reflect.New(typ)
	if err := d.Decode(next.Interface()); err != nil {
		return err
	}
	reflect.ValueOf(dst).Elem().Set(next.Elem())
	return nil
}

func checkPatchFields(fields map[string]json.RawMessage, typ reflect.Type, allow map[string]bool) error {
	known := map[string]reflect.Type{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			known[name] = f.Type
		}
	}
	for name, raw := range fields {
		ft, ok := known[name]
		if !ok || (allow != nil && !allow[name]) {
			return fmt.Errorf("field %q cannot be changed here", name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if ft.Kind() != reflect.Pointer && ft.Kind() != reflect.Map && ft.Kind() != reflect.Slice {
				return fmt.Errorf("field %q is not nullable", name)
			}
			continue
		}
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) == nil && nested != nil {
				if err := checkPatchFields(nested, ft, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func mergeJSONFields(base, patch map[string]json.RawMessage) {
	for k, v := range patch {
		var next, old map[string]json.RawMessage
		if json.Unmarshal(v, &next) == nil && next != nil && json.Unmarshal(base[k], &old) == nil && old != nil {
			mergeJSONFields(old, next)
			base[k], _ = json.Marshal(old)
		} else {
			base[k] = v
		}
	}
}

// PatchChannelConfig holds the same lock as Web edits and token rotation.
// The callback validates credential destination authorization against the actual row.
func (s *Store) PatchChannelConfig(id int64, expected string, change func(*Channel) error) (Channel, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return c, err
	}
	if expected == "" || EntityVersion(c) != expected {
		return Channel{}, ErrEditConflict
	}
	if err = change(&c); err != nil {
		return Channel{}, err
	}
	if c.ID != id {
		return Channel{}, NewConfigurationError("channel identity cannot be changed")
	}
	// This row was read under the same lock, so no stale OAuth or model fields
	// need to be copied over the explicit credential edit.
	pruneModelEntries(&c)
	err = s.saveChannelLocked(&c, false)
	return c, err
}

type ModelEdit struct {
	ID              string          `json:"id"`
	ExpectedVersion string          `json:"expected_version"`
	Patch           json.RawMessage `json:"patch"`
}

// PatchPublicModels validates and applies the whole explicit batch in one transaction.
func (s *Store) PatchPublicModels(edits []ModelEdit) ([]DownstreamModel, error) {
	if len(edits) < 1 || len(edits) > 100 {
		return nil, NewConfigurationError("choose 1..100 models")
	}
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := []DownstreamModel{}
	seen := map[string]bool{}
	for _, edit := range edits {
		if seen[edit.ID] {
			return nil, NewConfigurationError("duplicate model target")
		}
		seen[edit.ID] = true
		var raw string
		var m DownstreamModel
		if err = tx.QueryRow("SELECT data FROM downstream_models WHERE id=?", edit.ID).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		if edit.ExpectedVersion == "" || edit.ExpectedVersion != EntityVersion(m) {
			return nil, ErrEditConflict
		}
		if err = MergeFields(&m, edit.Patch, "display_name", "enabled", "fallback", "sort_order", "catalog_model_ref", "bindings", "tags"); err != nil {
			return nil, err
		}
		if err = validateAndSaveDownstreamModelInTx(tx, m); err != nil {
			return nil, err
		}
		// Return the normalized persisted model, including its actual tag set.
		if err = tx.QueryRow("SELECT data FROM downstream_models WHERE id=?", m.ID).Scan(&raw); err != nil {
			return nil, err
		}
		m = DownstreamModel{}
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

type PriceEdit struct {
	ChannelID       int64  `json:"channel_id"`
	Model           string `json:"model"`
	ExpectedVersion string `json:"expected_version"`
	Price           *Price `json:"price,omitempty"`
	Delete          bool   `json:"delete,omitempty"`
}

func (s *Store) EditPrices(edits []PriceEdit) ([]Price, error) {
	if len(edits) < 1 || len(edits) > 100 {
		return nil, NewConfigurationError("choose 1..100 prices")
	}
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	v, err := s.GetSettings()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, edit := range edits {
		key := fmt.Sprintf("%d/%s", edit.ChannelID, edit.Model)
		if seen[key] {
			return nil, NewConfigurationError("duplicate price target")
		}
		seen[key] = true
		idx := -1
		for i, p := range v.Prices {
			if p.ChannelID == edit.ChannelID && p.Model == edit.Model {
				idx = i
				break
			}
		}
		if idx < 0 {
			if edit.ExpectedVersion != "absent" || edit.Delete {
				return nil, ErrEditConflict
			}
		} else if edit.ExpectedVersion != EntityVersion(v.Prices[idx]) {
			return nil, ErrEditConflict
		}
		if edit.Delete {
			v.Prices = append(v.Prices[:idx], v.Prices[idx+1:]...)
			continue
		}
		if edit.Price == nil || edit.Price.ChannelID != edit.ChannelID || edit.Price.Model != edit.Model {
			return nil, NewConfigurationError("price target must match its identity")
		}
		if idx < 0 {
			v.Prices = append(v.Prices, *edit.Price)
		} else {
			v.Prices[idx] = *edit.Price
		}
	}
	if err = s.saveSettingsLocked(v, true); err != nil {
		return nil, err
	}
	return s.GetPrices()
}

func (s *Store) PatchRuntimeSettings(expected string, patch json.RawMessage) (Settings, error) {
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	v, err := s.GetSettings()
	if err != nil {
		return v, err
	}
	if expected == "" || expected != EntityVersion(SafeSettings(v)) {
		return Settings{}, ErrEditConflict
	}
	if err = MergeFields(&v, patch, "retention_days", "max_attempts", "retries_per_channel", "reasoning_policy", "failure_cooldown_seconds", "request_defaults_enabled", "request_defaults", "oauth_auto_open", "quota_refresh_seconds", "default_launch_mode"); err != nil {
		return Settings{}, err
	}
	if err = normalizeDefaultLaunchMode(&v); err != nil {
		return Settings{}, NewConfigurationError(err.Error())
	}
	if err = normalizeReasoningPolicy(&v); err != nil {
		return Settings{}, NewConfigurationError(err.Error())
	}
	if err = s.saveSettingsLocked(v, false); err != nil {
		return Settings{}, err
	}
	return v, nil
}

// SafeSettings contains only business settings. Authorization roots never leave this projection.
func SafeSettings(v Settings) map[string]any {
	b, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	delete(out, "gateway_key")
	delete(out, "prices")
	return out
}

func (s *Store) PatchTagRecord(id, expected string, patch json.RawMessage, group bool) (any, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if expected == "" {
		return nil, ErrEditConflict
	}
	if group {
		var current TagGroup
		rows, err := s.ListTagGroups()
		if err != nil {
			return nil, err
		}
		found := false
		for _, v := range rows {
			if v.ID == id {
				current = v
				found = true
			}
		}
		if (found && EntityVersion(current) != expected) || (!found && expected != "absent") {
			return nil, ErrEditConflict
		}
		if !found {
			current = TagGroup{ID: id, Type: TagGroupTypeUser}
		}
		if err = MergeFields(&current, patch, "name", "description", "sort_order"); err != nil {
			return nil, err
		}
		if err = s.saveTagGroupLocked(current); err != nil {
			return nil, err
		}
		return current, nil
	}
	var current Tag
	rows, err := s.ListTags()
	if err != nil {
		return nil, err
	}
	found := false
	for _, v := range rows {
		if v.ID == id {
			current = v
			found = true
		}
	}
	if (found && EntityVersion(current) != expected) || (!found && expected != "absent") {
		return nil, ErrEditConflict
	}
	if !found {
		current = Tag{ID: id}
	}
	if err = MergeFields(&current, patch, "group_id", "name", "color", "icon", "description", "sort_order"); err != nil {
		return nil, err
	}
	if err = s.saveTagLocked(current); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *Store) PatchModelSpecification(channelID int64, model, expected string, patch json.RawMessage, ref json.RawMessage) (ModelSpec, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(channelID)
	if err != nil {
		return ModelSpec{}, err
	}
	if expected == "" || EntityVersion(c) != expected {
		return ModelSpec{}, ErrEditConflict
	}
	if !containsString(c.Models, model) && !containsString(c.AvailableModels, model) {
		return ModelSpec{}, ErrNotFound
	}
	value := c.ModelConfigs[model]
	if err = MergeFields(&value, patch, "output_token_field", "native_thinking", "max_output_tokens", "context_window", "max_input_tokens", "output_limit", "streaming", "input_modalities", "output_modalities", "thinking_efforts"); err != nil {
		return ModelSpec{}, err
	}
	if err = ValidateModelConfig(value); err != nil {
		return ModelSpec{}, err
	}
	if c.ModelConfigs == nil {
		c.ModelConfigs = map[string]ModelConfig{}
	}
	c.ModelConfigs[model] = value
	if len(ref) > 0 {
		if c.ModelCatalogRefs == nil {
			c.ModelCatalogRefs = map[string]CatalogModelRef{}
		}
		var next *CatalogModelRef
		if err = json.Unmarshal(ref, &next); err != nil {
			return ModelSpec{}, err
		}
		if next == nil {
			delete(c.ModelCatalogRefs, model)
		} else {
			if err = ValidateCatalogModelRef(*next); err != nil {
				return ModelSpec{}, err
			}
			c.ModelCatalogRefs[model] = *next
		}
	}
	if err = s.writeChannelInPlace(&c); err != nil {
		return ModelSpec{}, err
	}
	out := ModelSpec{ChannelID: channelID, ChannelName: c.Name, Model: model, Specs: value}
	if v, ok := c.ModelCatalogRefs[model]; ok {
		out.CatalogModelRef = &v
	}
	return out, nil
}

func (s *Store) DeletePublicModelsAtVersions(expected map[string]string) error {
	if len(expected) < 1 || len(expected) > 100 {
		return NewConfigurationError("choose 1..100 models")
	}
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, version := range expected {
		var raw string
		var m DownstreamModel
		if err = tx.QueryRow("SELECT data FROM downstream_models WHERE id=?", id).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		if version == "" || EntityVersion(m) != version {
			return ErrEditConflict
		}
	}
	for id := range expected {
		if err = renameClientModelReferences(tx, id, ""); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM downstream_models WHERE id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
