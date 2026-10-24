package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type ClientKeyScope struct {
	ModelMode string              `json:"model_mode"`
	ModelIDs  []string            `json:"model_ids"`
	TagRules  map[string][]string `json:"tag_rules"`
}

func DefaultClientKeyScope() ClientKeyScope {
	return ClientKeyScope{ModelMode: "all", ModelIDs: []string{}, TagRules: map[string][]string{}}
}

// AllowsModel fails closed for malformed modes. Selected empty means deny all.
func (s ClientKeyScope) AllowsModel(id string) bool {
	return s.ModelMode == "all" || s.ModelMode == "selected" && slices.Contains(s.ModelIDs, id)
}
func (s ClientKeyScope) AllowsTags(effective map[string][]string) bool {
	return (s.ModelMode == "all" || s.ModelMode == "selected") && len(s.MismatchedTagGroups(effective)) == 0
}
func (s ClientKeyScope) MismatchedTagGroups(effective map[string][]string) []string {
	mismatched := []string{}
	for group, allowed := range s.TagRules {
		matched := false
		for _, id := range effective[group] {
			if slices.Contains(allowed, id) {
				matched = true
				break
			}
		}
		if !matched {
			mismatched = append(mismatched, group)
		}
	}
	slices.Sort(mismatched)
	return mismatched
}

func canonicalScope(scope ClientKeyScope) (ClientKeyScope, error) {
	if scope.ModelMode != "all" && scope.ModelMode != "selected" {
		return scope, NewConfigurationError("scope.model_mode must be all or selected")
	}
	if scope.ModelMode == "all" && len(scope.ModelIDs) > 0 {
		return scope, NewConfigurationError("all scope cannot contain model_ids")
	}
	scope.ModelIDs = slices.Clone(scope.ModelIDs)
	if scope.ModelIDs == nil {
		scope.ModelIDs = []string{}
	}
	seen := map[string]bool{}
	for _, id := range scope.ModelIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return scope, NewConfigurationError("invalid or duplicate scope model ID")
		}
		seen[id] = true
	}
	rules := map[string][]string{}
	for group, ids := range scope.TagRules {
		if group == "" || group == "modality" {
			return scope, NewConfigurationError("invalid scope tag group")
		}
		rules[group] = []string{}
		seen = map[string]bool{}
		for _, id := range ids {
			if id == "" || seen[id] {
				return scope, NewConfigurationError("invalid or duplicate scope tag ID")
			}
			seen[id] = true
			rules[group] = append(rules[group], id)
		}
	}
	scope.TagRules = rules
	return scope, nil
}

// ValidateClientKeyScope checks references at the write boundary; runtime matching
// never interprets a missing reference as an unrestricted rule.
func (s *Store) ValidateClientKeyScope(scope ClientKeyScope) (ClientKeyScope, error) {
	return s.validateScope(scope)
}
func (s *Store) validateScope(scope ClientKeyScope) (ClientKeyScope, error) {
	scope, err := canonicalScope(scope)
	if err != nil {
		return scope, err
	}
	for _, id := range scope.ModelIDs {
		var n int
		if err = s.db.QueryRow("SELECT count(*) FROM downstream_models WHERE id=?", id).Scan(&n); err != nil {
			return scope, err
		}
		if n == 0 {
			return scope, NewConfigurationError(fmt.Sprintf("scope model %q does not exist", id))
		}
	}
	for group, ids := range scope.TagRules {
		var n int
		if err = s.db.QueryRow("SELECT count(*) FROM tag_groups WHERE id=?", group).Scan(&n); err != nil {
			return scope, err
		}
		if n == 0 {
			return scope, NewConfigurationError(fmt.Sprintf("scope group %q does not exist", group))
		}
		for _, id := range ids {
			if err = s.db.QueryRow("SELECT count(*) FROM tags WHERE id=? AND group_id=?", id, group).Scan(&n); err != nil {
				return scope, err
			}
			if n == 0 {
				return scope, NewConfigurationError(fmt.Sprintf("scope tag %q does not belong to group %q", id, group))
			}
		}
	}
	return scope, nil
}

func decodeClientScope(raw string) (ClientKeyScope, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return ClientKeyScope{}, err
	}
	for _, field := range []string{"model_mode", "model_ids", "tag_rules"} {
		if fields[field] == nil || string(fields[field]) == "null" {
			return ClientKeyScope{}, fmt.Errorf("invalid stored scope: missing %s", field)
		}
	}
	var scope ClientKeyScope
	if err := json.Unmarshal([]byte(raw), &scope); err != nil {
		return scope, err
	}
	return canonicalScope(scope)
}

func (s *Store) UpdateClientKey(id int64, name string, scope ClientKeyScope) (ClientKey, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	return s.updateClientKeyLocked(id, name, scope)
}

func (s *Store) updateClientKeyLocked(id int64, name string, scope ClientKeyScope) (ClientKey, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return ClientKey{}, NewConfigurationError("name must contain 1..100 bytes")
	}
	scope, err := s.validateScope(scope)
	if err != nil {
		return ClientKey{}, err
	}
	raw, _ := json.Marshal(scope)
	res, err := s.db.Exec("UPDATE client_keys SET name=?,scope=? WHERE id=?", name, string(raw), id)
	if err != nil {
		return ClientKey{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ClientKey{}, ErrNotFound
	}
	keys, err := s.ListClientKeys()
	if err != nil {
		return ClientKey{}, err
	}
	for _, key := range keys {
		if key.ID == id {
			return key, nil
		}
	}
	return ClientKey{}, ErrNotFound
}

func renameClientModelReferences(tx *sql.Tx, oldID, newID string) error {
	rows, err := tx.Query("SELECT id,scope FROM client_keys")
	if err != nil {
		return err
	}
	updates := map[int64]string{}
	for rows.Next() {
		var id int64
		var raw string
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		scope, e := decodeClientScope(raw)
		if e != nil {
			rows.Close()
			return e
		}
		if scope.ModelMode == "selected" {
			matched := false
			for i, model := range scope.ModelIDs {
				if model == oldID {
					scope.ModelIDs[i] = newID
					matched = true
				}
			}
			if !matched {
				continue
			}
			filtered := []string{}
			for _, model := range scope.ModelIDs {
				if model != "" && !slices.Contains(filtered, model) {
					filtered = append(filtered, model)
				}
			}
			scope.ModelIDs = filtered
			b, _ := json.Marshal(scope)
			updates[id] = string(b)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for id, raw := range updates {
		if _, err = tx.Exec("UPDATE client_keys SET scope=? WHERE id=?", raw, id); err != nil {
			return err
		}
	}
	return nil
}
