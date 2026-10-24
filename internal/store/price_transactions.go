package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// replacePricesInTx keeps the sealed settings fields intact. It never opens a
// second connection while the SQLite single-connection transaction is held.
func replacePricesInTx(tx *sql.Tx, entries []Price, replace func(Price) bool) error {
	if entries == nil {
		return nil
	}
	var raw string
	if err := tx.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&raw); err != nil {
		return err
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return err
	}
	var current []Price
	if err := json.Unmarshal(v["prices"], &current); err != nil {
		return err
	}
	next := []Price{}
	for _, p := range current {
		if !replace(p) {
			next = append(next, p)
		}
	}
	next = append(next, entries...)
	next, err := normalizePrices(next)
	if err != nil {
		return err
	}
	v["prices"], err = json.Marshal(next)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE settings SET data=? WHERE id=1", string(data))
	return err
}
func (s *Store) ImportDownstreamModelsWithPrices(models []DownstreamModel, prices []Price, overwrite bool) error {
	return s.ImportDownstreamModelsWithPricesChecked(models, prices, overwrite, "")
}
func (s *Store) ImportDownstreamModelsWithPricesChecked(models []DownstreamModel, prices []Price, overwrite bool, expected string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	normalized, err := s.normalizeImportedDownstreamTags(models, prices)
	if err != nil {
		return err
	}
	models = normalized
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	for _, p := range prices {
		if p.ChannelID != 0 || !ids[p.Model] {
			return errors.New("import price must belong to imported downstream model")
		}
	}
	if expected != "" {
		currentModels, err := s.ListDownstreamModels()
		if err != nil {
			return err
		}
		currentPrices, err := s.GetPrices()
		if err != nil {
			return err
		}
		if EntityVersion(map[string]any{"models": currentModels, "prices": currentPrices}) != expected {
			return ErrEditConflict
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if overwrite {
		rows, err := tx.Query("SELECT id FROM downstream_models")
		if err != nil {
			return err
		}
		removed := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !ids[id] {
				removed = append(removed, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range removed {
			if err = renameClientModelReferences(tx, id, ""); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("DELETE FROM downstream_models"); err != nil {
			return err
		}
	}
	for _, m := range models {
		if err = validateAndSaveDownstreamModelInTx(tx, m); err != nil {
			return err
		}
	}
	if err = replacePricesInTx(tx, prices, func(p Price) bool { return p.ChannelID == 0 && (overwrite || ids[p.Model]) }); err != nil {
		return err
	}
	return tx.Commit()
}
