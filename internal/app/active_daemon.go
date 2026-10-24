package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const activeDaemonFile = ".active-daemon.json"

// activeDaemonPort only trusts a descriptor whose installation and live instance
// both match. A stale descriptor never redirects a client to a reused port.
func activeDaemonPort(dataDir, installation string) (int, error) {
	unlock, err := startupLock(dataDir, "active")
	if err != nil {
		return 0, err
	}
	defer unlock()
	recorded, err := readActiveDaemon(dataDir)
	if err != nil {
		return 0, err
	}
	if recorded == nil || recorded.Installation != installation {
		return 0, nil
	}
	live, err := readDaemonReady(recorded.Port)
	if err != nil {
		return 0, fmt.Errorf("cannot verify recorded active daemon: %w", err)
	}
	if live == nil || *live != *recorded {
		return 0, nil
	}
	return recorded.Port, nil
}

func readActiveDaemon(dataDir string) (*daemonReady, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, activeDaemonFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ready daemonReady
	if json.Unmarshal(data, &ready) != nil || ready.Service != "to-router" || ready.Installation == "" || ready.Instance == "" || ready.Port < 1 || ready.Port > 65535 {
		return nil, nil
	}
	return &ready, nil
}

func (s *Server) publishActiveDaemon() error {
	unlock, err := startupLock(s.DataDir, "active")
	if err != nil {
		return err
	}
	defer unlock()
	file, err := os.CreateTemp(s.DataDir, ".active-daemon-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := json.NewEncoder(file).Encode(s.activeIdentity); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(s.DataDir, activeDaemonFile)); err != nil {
		return fmt.Errorf("publish active daemon: %w", err)
	}
	return nil
}

func (s *Server) clearActiveDaemon() {
	unlock, err := startupLock(s.DataDir, "active")
	if err != nil {
		return
	}
	defer unlock()
	recorded, err := readActiveDaemon(s.DataDir)
	if err == nil && recorded != nil && *recorded == s.activeIdentity {
		_ = os.Remove(filepath.Join(s.DataDir, activeDaemonFile))
	}
}
