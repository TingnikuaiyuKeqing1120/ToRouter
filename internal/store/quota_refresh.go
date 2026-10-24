package store

import (
	"encoding/json"
	"errors"
)

// QuotaRefresh projects only the existing global display refresh setting.
type QuotaRefresh struct {
	QuotaRefreshSeconds int `json:"quota_refresh_seconds"`
}

var ErrInvalidQuotaRefresh = errors.New("quota_refresh_seconds must be 0 or 30..86400")

func (s *Store) GetQuotaRefresh() (QuotaRefresh, error) {
	var raw string
	if err := s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&raw); err != nil {
		return QuotaRefresh{}, err
	}
	var v struct {
		Seconds *int `json:"quota_refresh_seconds"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return QuotaRefresh{}, errors.New("cannot read quota refresh setting")
	}
	seconds := DefaultQuotaRefreshSeconds
	if v.Seconds != nil {
		seconds = *v.Seconds
	}
	if seconds != 0 && (seconds < 30 || seconds > 86400) {
		return QuotaRefresh{}, ErrInvalidQuotaRefresh
	}
	return QuotaRefresh{QuotaRefreshSeconds: seconds}, nil
}

// PatchQuotaRefresh never decrypts or round-trips unrelated configuration through
// Settings. The settings lock serializes it with the other settings writers.
func (s *Store) PatchQuotaRefresh(seconds int) (QuotaRefresh, error) {
	if seconds != 0 && (seconds < 30 || seconds > 86400) {
		return QuotaRefresh{}, ErrInvalidQuotaRefresh
	}
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	if _, err := s.db.Exec("UPDATE settings SET data=json_set(data,'$.quota_refresh_seconds',?) WHERE id=1", seconds); err != nil {
		return QuotaRefresh{}, err
	}
	return QuotaRefresh{QuotaRefreshSeconds: seconds}, nil
}
