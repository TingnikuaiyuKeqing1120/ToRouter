package store

import "strconv"

type OAuthAutoOpenSetting struct {
	OAuthAutoOpen bool `json:"oauth_auto_open"`
}

// PatchOAuthAutoOpen updates the existing preference without round-tripping
// stale quota periods, sealed secrets, prices, or other settings.
func (s *Store) PatchOAuthAutoOpen(enabled bool) (OAuthAutoOpenSetting, error) {
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	if _, err := s.db.Exec("UPDATE settings SET data=json_set(data,'$.oauth_auto_open',json(?)) WHERE id=1", strconv.FormatBool(enabled)); err != nil {
		return OAuthAutoOpenSetting{}, err
	}
	return OAuthAutoOpenSetting{OAuthAutoOpen: enabled}, nil
}
