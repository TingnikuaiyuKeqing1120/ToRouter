package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PluginPreferences contains capability switches only; credentials and channels
// remain in their existing storage. Registration validation belongs to the host.
type PluginPreferences struct {
	Plugins map[string]map[string]bool `json:"plugins"`
}

const pluginPreferencesKey = "plugin_preferences"

var ErrInvalidPluginPreferences = errors.New("invalid plugin preferences")

func validatePluginPreferenceName(name string) error {
	if IsMarketOAuth(name) {
		return nil
	}
	if len(name) < 1 || len(name) > 64 || name[0] < 'a' || name[0] > 'z' {
		return ErrInvalidPluginPreferences
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return ErrInvalidPluginPreferences
		}
	}
	return nil
}

// Each read creates new maps. Keep unknown top-level fields when patching the
// resource, just as unknown plugin IDs and capabilities retain their settings.
func readPluginPreferences(tx *sql.Tx) (PluginPreferences, map[string]json.RawMessage, error) {
	v := PluginPreferences{Plugins: map[string]map[string]bool{}}
	fields := map[string]json.RawMessage{}
	var raw string
	err := tx.QueryRow("SELECT value FROM meta WHERE key=?", pluginPreferencesKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return v, fields, nil
	}
	if err != nil {
		return v, nil, err
	}
	if err = json.Unmarshal([]byte(raw), &fields); err != nil || fields == nil {
		return v, nil, fmt.Errorf("%w: expected a JSON object", ErrInvalidPluginPreferences)
	}
	var plugins map[string]map[string]json.RawMessage
	if err = json.Unmarshal(fields["plugins"], &plugins); err != nil || plugins == nil {
		return v, nil, fmt.Errorf("%w: plugins must be an object", ErrInvalidPluginPreferences)
	}
	for id, capabilities := range plugins {
		if validatePluginPreferenceName(id) != nil || capabilities == nil {
			return v, nil, fmt.Errorf("%w: invalid plugin entry", ErrInvalidPluginPreferences)
		}
		v.Plugins[id] = map[string]bool{}
		for capability, value := range capabilities {
			if validatePluginPreferenceName(capability) != nil || (string(value) != "true" && string(value) != "false") {
				return v, nil, fmt.Errorf("%w: invalid capability entry", ErrInvalidPluginPreferences)
			}
			v.Plugins[id][capability] = string(value) == "true"
		}
	}
	return v, fields, nil
}

func writePluginPreferences(tx *sql.Tx, v PluginPreferences, fields map[string]json.RawMessage) error {
	raw, err := json.Marshal(v.Plugins)
	if err != nil {
		return err
	}
	fields["plugins"] = raw
	raw, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", pluginPreferencesKey, string(raw))
	return err
}

// InitPluginPreferences fills only missing registered capabilities. An existing
// channel of the legacy credential type enables a missing connect switch even
// when that channel is disabled, preserving access to its management actions.
// Explicit choices, including false, always win over defaults and inheritance.
func (s *Store) InitPluginPreferences(defaults map[string]map[string]bool, legacyTypes map[string]string) (PluginPreferences, error) {
	for id, capabilities := range defaults {
		if err := validatePluginPreferenceName(id); err != nil {
			return PluginPreferences{}, err
		}
		for capability := range capabilities {
			if err := validatePluginPreferenceName(capability); err != nil {
				return PluginPreferences{}, err
			}
		}
	}
	for id := range legacyTypes {
		if err := validatePluginPreferenceName(id); err != nil {
			return PluginPreferences{}, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PluginPreferences{}, err
	}
	defer tx.Rollback()
	v, fields, err := readPluginPreferences(tx)
	if err != nil {
		return PluginPreferences{}, err
	}
	for id, capabilities := range defaults {
		if v.Plugins[id] == nil {
			v.Plugins[id] = map[string]bool{}
		}
		for capability, enabled := range capabilities {
			if _, exists := v.Plugins[id][capability]; exists {
				continue
			}
			if credentialType := legacyTypes[id]; capability == "connect" && credentialType != "" {
				var exists bool
				// Read only the credential type, never secret-bearing channel data.
				if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM channels WHERE json_extract(data,'$.credential_type')=?)", credentialType).Scan(&exists); err != nil {
					return PluginPreferences{}, err
				}
				enabled = enabled || exists
			}
			v.Plugins[id][capability] = enabled
		}
	}
	if err = writePluginPreferences(tx, v, fields); err != nil {
		return PluginPreferences{}, err
	}
	if err = tx.Commit(); err != nil {
		return PluginPreferences{}, err
	}
	return v, nil
}

func (s *Store) GetPluginPreferences() (PluginPreferences, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return PluginPreferences{}, err
	}
	defer tx.Rollback()
	v, _, err := readPluginPreferences(tx)
	if err != nil {
		return PluginPreferences{}, err
	}
	if err = tx.Commit(); err != nil {
		return PluginPreferences{}, err
	}
	return v, nil
}

// SetPluginCapability patches one exact switch in a single transaction. The
// host must verify the plugin and capability are registered before calling it.
func (s *Store) SetPluginCapability(pluginID, capability string, enabled bool) (PluginPreferences, error) {
	if err := validatePluginPreferenceName(pluginID); err != nil {
		return PluginPreferences{}, err
	}
	if err := validatePluginPreferenceName(capability); err != nil {
		return PluginPreferences{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PluginPreferences{}, err
	}
	defer tx.Rollback()
	v, fields, err := readPluginPreferences(tx)
	if err != nil {
		return PluginPreferences{}, err
	}
	if v.Plugins[pluginID] == nil {
		v.Plugins[pluginID] = map[string]bool{}
	}
	v.Plugins[pluginID][capability] = enabled
	if err = writePluginPreferences(tx, v, fields); err != nil {
		return PluginPreferences{}, err
	}
	if err = tx.Commit(); err != nil {
		return PluginPreferences{}, err
	}
	return v, nil
}
