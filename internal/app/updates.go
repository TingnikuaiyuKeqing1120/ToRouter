package app

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"torouter/internal/buildinfo"
	"torouter/internal/console"
	"torouter/internal/desktop"
	"torouter/internal/store"
	"torouter/internal/updater"
)

func (s *Server) initProgramUpdates() {
	u, _ := url.Parse(buildinfo.UpdateFeedURL)
	loopback := u != nil && u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())
	m, err := updater.New(updater.Config{Version: buildinfo.Version, FeedURL: buildinfo.UpdateFeedURL, PublicKey: buildinfo.UpdatePublicKey, AllowLoopbackHTTP: loopback})
	if err != nil {
		s.log.Warn("application update configuration unavailable", "error", updater.ErrorMessage(err))
		m, _ = updater.New(updater.Config{Version: buildinfo.Version})
		m.SetInstallError(errors.New("application update configuration unavailable"))
	}
	s.updates = m
	s.launchUpdate = updater.LaunchHelper
}

func (s *Server) programUpdateStatus() updater.Status {
	if s.updates == nil {
		return updater.Status{CurrentVersion: buildinfo.Version, State: "disabled"}
	}
	return s.updates.Status()
}

func (s *Server) checkProgramUpdate(ctx context.Context) (updater.Status, error) {
	if s.updates == nil {
		return s.programUpdateStatus(), nil
	}
	return s.updates.Check(ctx)
}

// Preparation may download, but does not stop the server. The caller activates
// only after the HTTP/MCP response and durable management receipt are written.
func (s *Server) prepareProgramUpdate(ctx context.Context, version, sha256, openMode string) (console.UpdateAccepted, func(), error) {
	if s.updates == nil || !s.updates.Status().Enabled {
		return console.UpdateAccepted{}, nil, errors.New("application updates are not configured")
	}
	if openMode != "" && !store.ValidLaunchMode(openMode) {
		return console.UpdateAccepted{}, nil, errors.New("invalid update view")
	}
	if version == "" {
		return console.UpdateAccepted{}, nil, errors.New("update version is required")
	}
	s.restartMu.Lock()
	busy := s.restartFrozen || s.restartRequested || s.updateRequested
	s.restartMu.Unlock()
	if busy {
		return console.UpdateAccepted{}, nil, errors.New("application is already stopping or updating")
	}
	prepared, err := s.updates.PrepareRelease(ctx, version, sha256)
	if err != nil {
		return console.UpdateAccepted{}, nil, err
	}
	if err = ctx.Err(); err != nil {
		return console.UpdateAccepted{}, nil, err
	}
	dataDir, err := filepath.Abs(s.DataDir)
	if err != nil {
		return console.UpdateAccepted{}, nil, err
	}
	options := updater.HelperOptions{DataDir: dataDir, Port: s.Port, ParentPID: os.Getpid(), StartArgs: []string{"-mode", "daemon"}, OpenMode: openMode, OpenPet: desktop.CompanionRunning(s.Origin(), s.DataDir)}
	var once sync.Once
	activate := func() {
		once.Do(func() {
			s.restartMu.Lock()
			if s.restartFrozen || s.restartRequested || s.updateRequested {
				s.restartMu.Unlock()
				s.updates.SetInstallError(errors.New("application is already stopping or updating"))
				return
			}
			if _, err := s.launchUpdate(prepared, options); err != nil {
				s.restartMu.Unlock()
				s.updates.SetInstallError(err)
				return
			}
			s.updateRequested = true
			s.updates.MarkInstalling()
			s.restartMu.Unlock()
			s.Shutdown()
		})
	}
	return console.UpdateAccepted{Requested: true, Port: s.Port, Version: prepared.Release.Version}, activate, nil
}
