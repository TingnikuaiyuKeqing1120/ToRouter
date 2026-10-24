package app

import (
	"torouter/internal/store"
	"torouter/internal/updater"
)

// ReadDefaultLaunchMode lets a manual launcher choose its view before it owns
// a server. All views use the same saved preference in the selected data dir.
func ReadDefaultLaunchMode(dataDir string) (string, error) {
	unlock, err := startupLock(dataDir, "config")
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := updater.CheckLaunch(dataDir); err != nil {
		return "", err
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return "", err
	}
	defer st.Close()
	v, err := st.GetSettings()
	return v.DefaultLaunchMode, err
}

func (s *Server) DefaultLaunchMode() string {
	v, err := s.Store.GetSettings()
	if err != nil {
		s.log.Warn("cannot read default launch mode", "error", err)
		return store.DefaultLaunchMode
	}
	return v.DefaultLaunchMode
}

func (s *Server) SetDefaultLaunchMode(mode string) error {
	return s.Store.SaveDefaultLaunchMode(mode)
}
