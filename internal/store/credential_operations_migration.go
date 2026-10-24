package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Upgrade the former single daily time at Open, before typed channel readers
// can discard it. The running service accepts only daily_times afterwards.
func (s *Store) upgradeCredentialDailyTimes() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id,data FROM channels WHERE instr(data,'"daily_time"')>0`)
	if err != nil {
		return err
	}
	type row struct {
		id  int64
		raw string
	}
	var all []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.id, &r.raw); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range all {
		var channel map[string]json.RawMessage
		if err = json.Unmarshal([]byte(r.raw), &channel); err != nil {
			return err
		}
		var credentials []map[string]json.RawMessage
		if err = json.Unmarshal(channel["credentials"], &credentials); err != nil {
			return err
		}
		changed := false
		for _, cred := range credentials {
			var config map[string]json.RawMessage
			if len(cred["operations"]) == 0 || string(cred["operations"]) == "null" {
				continue
			}
			if err = json.Unmarshal(cred["operations"], &config); err != nil {
				return err
			}
			var warmup map[string]json.RawMessage
			if err = json.Unmarshal(config["warmup"], &warmup); err != nil {
				return err
			}
			old, exists := warmup["daily_time"]
			if !exists {
				continue
			}
			var at, cid string
			if err = json.Unmarshal(old, &at); err != nil {
				return err
			}
			if _, err = time.Parse("15:04", at); err != nil || len(at) != 5 {
				return fmt.Errorf("channel %d has an invalid daily warmup time", r.id)
			}
			if _, exists = warmup["daily_times"]; !exists {
				warmup["daily_times"], _ = json.Marshal([]string{at})
			}
			delete(warmup, "daily_time")
			config["warmup"], _ = json.Marshal(warmup)
			cred["operations"], _ = json.Marshal(config)
			changed = true
			if err = json.Unmarshal(cred["id"], &cid); err != nil {
				return err
			}
			// Preserve the original day's claim so an upgrade cannot replay it.
			var recordRaw string
			err = tx.QueryRow("SELECT value FROM meta WHERE key=?", credentialOperationKey(r.id, cid)).Scan(&recordRaw)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			var record credentialOperationRecord
			if err = json.Unmarshal([]byte(recordRaw), &record); err != nil {
				return err
			}
			var data map[string]json.RawMessage
			if err = json.Unmarshal(record.Data, &data); err != nil {
				return err
			}
			var date string
			if len(data["daily_date"]) > 0 {
				if err = json.Unmarshal(data["daily_date"], &date); err != nil {
					return err
				}
			}
			if _, exists = data["daily_times"]; !exists && date != "" {
				data["daily_times"], _ = json.Marshal([]string{at})
			}
			// These fields belonged to the retired exhaustion-only watcher.
			delete(data, "wake")
			delete(data, "generation")
			record.Data, _ = json.Marshal(data)
			body, _ := json.Marshal(record)
			if _, err = tx.Exec("UPDATE meta SET value=? WHERE key=?", string(body), credentialOperationKey(r.id, cid)); err != nil {
				return err
			}
		}
		if changed {
			channel["credentials"], _ = json.Marshal(credentials)
			raw, _ := json.Marshal(channel)
			if _, err = tx.Exec("UPDATE channels SET data=? WHERE id=?", string(raw), r.id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
