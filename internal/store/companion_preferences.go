package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

type CompanionPreferences struct {
	Theme        string        `json:"theme"`
	Paused       bool          `json:"paused"`
	Revision     int64         `json:"revision"`
	QuotaTargets []QuotaTarget `json:"quota_targets"`
}

type QuotaTarget struct {
	ChannelID    int64  `json:"channel_id"`
	CredentialID string `json:"credential_id"`
}

type CompanionPreferencesPatch struct {
	Theme        *string        `json:"theme,omitempty"`
	Paused       *bool          `json:"paused,omitempty"`
	QuotaTargets *[]QuotaTarget `json:"quota_targets,omitempty"`
}

func readCompanionPreferences(query interface{ QueryRow(string, ...any) *sql.Row }) (CompanionPreferences, error) {
	v := CompanionPreferences{Theme: "dark", QuotaTargets: []QuotaTarget{}}
	var raw string
	err := query.QueryRow("SELECT value FROM meta WHERE key='companion_preferences'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(raw), &v); err != nil {
		return v, err
	}
	if v.QuotaTargets == nil {
		v.QuotaTargets = []QuotaTarget{}
	}
	return v, nil
}

func (s *Store) CompanionPreferences() (CompanionPreferences, error) {
	return readCompanionPreferences(s.db)
}

// Patch each field against the current row within a transaction so another
// window's pause cannot be overwritten by a theme-only update.
func (s *Store) PatchCompanionPreferences(p CompanionPreferencesPatch) (CompanionPreferences, error) {
	if p.Theme != nil && *p.Theme != "dark" && *p.Theme != "light" {
		return CompanionPreferences{}, errors.New("theme must be dark or light")
	}
	// Serialize target existence checks with channel deletion/replacement and
	// serialize preference patches so independent windows retain other fields.
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if p.QuotaTargets != nil {
		if len(*p.QuotaTargets) > 100 {
			return CompanionPreferences{}, errors.New("quota_targets must contain at most 100 targets")
		}
		seen := make(map[QuotaTarget]bool)
		for _, target := range *p.QuotaTargets {
			if target.ChannelID <= 0 || target.CredentialID == "" || seen[target] {
				return CompanionPreferences{}, errors.New("quota_targets must contain unique exact credentials")
			}
			seen[target] = true
			c, err := s.channel(target.ChannelID)
			if err != nil {
				return CompanionPreferences{}, errors.New("quota target channel no longer exists")
			}
			found := false
			for _, cred := range c.Credentials {
				if cred.ID == target.CredentialID {
					found = true
					break
				}
			}
			if !found {
				return CompanionPreferences{}, errors.New("quota target credential no longer exists")
			}
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CompanionPreferences{}, err
	}
	defer tx.Rollback()
	v, err := readCompanionPreferences(tx)
	if err != nil {
		return v, err
	}
	if p.Theme != nil {
		v.Theme = *p.Theme
	}
	if p.Paused != nil {
		v.Paused = *p.Paused
	}
	if p.QuotaTargets != nil {
		v.QuotaTargets = append([]QuotaTarget{}, (*p.QuotaTargets)...)
	}
	if p.Theme != nil || p.Paused != nil || p.QuotaTargets != nil {
		v.Revision++
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	if _, err = tx.Exec("INSERT INTO meta(key,value) VALUES('companion_preferences',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
		return v, err
	}
	err = tx.Commit()
	return v, err
}
