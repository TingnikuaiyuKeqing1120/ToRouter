package store

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ModelOutboundFormats is the single model-to-wire constraint shared by native
// probes and routing. Channel endpoints bound every declaration. Missing API Key
// overrides inherit the channel; complete provider catalogues never guess.
func (c Channel) ModelOutboundFormats(model string) []string {
	if c.isCatalogueProvider() {
		if c.ModelFormats == nil {
			if !containsString(c.Models, model) {
				return nil
			}
			return c.Formats()
		}
		declared, exists := c.ModelFormats[model]
		if !exists {
			return nil
		}
		return c.intersectFormats(declared)
	}
	if c.ModelFormats == nil {
		return c.Formats()
	}
	declared, exists := c.ModelFormats[model]
	if !exists {
		return c.Formats()
	}
	return c.intersectFormats(declared)
}

func (c Channel) intersectFormats(declared []string) []string {
	result := []string{}
	for _, format := range c.Formats() {
		if containsString(declared, format) {
			result = append(result, format)
		}
	}
	return result
}

func (c Channel) isCatalogueProvider() bool {
	return c.CredentialType == CredentialZenFree || c.CredentialType == CredentialCopilotOAuth || IsMarketOAuth(c.CredentialType)
}

// upgradeModelFormats converts only the JSON field, before any channel decode.
// Existing ciphertext and every unrelated raw JSON value are retained. There is
// no runtime old-shape reader or second protocol field.
func (s *Store) upgradeModelFormats() error {
	rows, err := s.db.Query("SELECT id,data FROM channels")
	if err != nil {
		return err
	}
	type change struct {
		id  int64
		raw []byte
	}
	changes := []change{}
	for rows.Next() {
		var id int64
		var raw string
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var object map[string]json.RawMessage
		if err = json.Unmarshal([]byte(raw), &object); err != nil {
			rows.Close()
			return err
		}
		field := bytes.TrimSpace(object["model_formats"])
		if len(field) == 0 || bytes.Equal(field, []byte("null")) {
			continue
		}
		var formats map[string]json.RawMessage
		if err = json.Unmarshal(field, &formats); err != nil {
			rows.Close()
			return err
		}
		changed := false
		for model, value := range formats {
			if len(bytes.TrimSpace(value)) > 0 && bytes.TrimSpace(value)[0] == '"' {
				var format string
				if err = json.Unmarshal(value, &format); err != nil {
					rows.Close()
					return err
				}
				formats[model], _ = json.Marshal([]string{format})
				changed = true
			} else {
				var current []string
				if err = json.Unmarshal(value, &current); err != nil {
					rows.Close()
					return fmt.Errorf("model formats: %w", err)
				}
			}
		}
		if changed {
			object["model_formats"], _ = json.Marshal(formats)
			next, e := json.Marshal(object)
			if e != nil {
				rows.Close()
				return e
			}
			changes = append(changes, change{id, next})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(changes) == 0 {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range changes {
		if _, err = tx.Exec("UPDATE channels SET data=? WHERE id=?", string(c.raw), c.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
