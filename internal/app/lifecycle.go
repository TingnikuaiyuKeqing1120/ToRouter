package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
	"torouter/internal/buildinfo"
)

// DaemonLifecycle is public loopback-only metadata, never an authentication token.
type DaemonLifecycle struct {
	Service        string `json:"service"`
	State          string `json:"state"`
	InstallationID string `json:"installation_id"`
	InstanceID     string `json:"instance_id"`
	Port           int    `json:"port"`
	TargetPort     int    `json:"target_port,omitempty"`
	Version        string `json:"version"`
}

type daemonLifecycle struct {
	mu    sync.Mutex
	value DaemonLifecycle
	done  chan struct{}
}

func newDaemonLifecycle(identity daemonReady) *daemonLifecycle {
	return &daemonLifecycle{value: DaemonLifecycle{Service: identity.Service, State: "running", InstallationID: identity.Installation, InstanceID: identity.Instance, Port: identity.Port, Version: buildinfo.Version}, done: make(chan struct{})}
}

func (l *daemonLifecycle) snapshot() DaemonLifecycle {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.value
}

func (l *daemonLifecycle) finish(state string, targetPort int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.value.State != "running" {
		return
	}
	l.value.State, l.value.TargetPort = state, targetPort
	close(l.done)
}

// WaitDaemonLifecycle waits for one lifecycle event or a 25-second running
// heartbeat. Callers repeat running heartbeats, and reconnect to TargetPort
// after restarting. Only a matching installation may stop a connected view.
func WaitDaemonLifecycle(ctx context.Context, origin, expectedInstallation string) (DaemonLifecycle, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return DaemonLifecycle{}, errors.New("lifecycle origin must be an HTTP loopback origin")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || expectedInstallation == "" {
		return DaemonLifecycle{}, errors.New("lifecycle requires a valid port and installation")
	}
	u.Path, u.RawQuery = consoleReadyPath, "watch=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return DaemonLifecycle{}, err
	}
	resp, err := localHTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return DaemonLifecycle{}, err
	}
	defer resp.Body.Close()
	var value DaemonLifecycle
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&value)
	if resp.StatusCode != http.StatusOK || decodeErr != nil || value.Service != "to-router" || value.InstanceID == "" || value.Port != port || value.InstallationID != expectedInstallation {
		return DaemonLifecycle{}, fmt.Errorf("gateway on port %d returned invalid lifecycle metadata for this installation", port)
	}
	if value.State != "running" && value.State != "stopped" && value.State != "restarting" && value.State != "updating" {
		return DaemonLifecycle{}, errors.New("gateway returned an unknown lifecycle state")
	}
	if (value.State == "restarting" || value.State == "updating") && (value.TargetPort < 1 || value.TargetPort > 65535) {
		return DaemonLifecycle{}, errors.New("gateway returned an invalid restart target")
	}
	return value, nil
}
