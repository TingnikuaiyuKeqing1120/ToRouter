package store

import "errors"

const DefaultLaunchMode = "web"

func ValidLaunchMode(mode string) bool {
	return mode == "web" || mode == "desktop" || mode == "cli"
}

func normalizeDefaultLaunchMode(v *Settings) error {
	if v.DefaultLaunchMode == "" {
		v.DefaultLaunchMode = DefaultLaunchMode
	}
	if !ValidLaunchMode(v.DefaultLaunchMode) {
		return errors.New("default_launch_mode must be web, desktop or cli")
	}
	return nil
}

// SaveDefaultLaunchMode changes only the launch preference, so a tray action
// cannot overwrite a concurrently saved credential, price, or runtime setting.
func (s *Store) SaveDefaultLaunchMode(mode string) error {
	if !ValidLaunchMode(mode) {
		return errors.New("default_launch_mode must be web, desktop or cli")
	}
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	v, err := s.GetSettings()
	if err != nil {
		return err
	}
	v.DefaultLaunchMode = mode
	return s.saveSettingsLocked(v, false)
}
