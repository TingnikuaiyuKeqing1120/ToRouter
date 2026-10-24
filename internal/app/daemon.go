package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"torouter/internal/store"
	"torouter/internal/updater"
)

// ResolvePort refuses an explicit port that conflicts with a verified active
// instance, then selects explicit, verified active, saved, or default port.
// Start itself still selects explicit/saved so a restart can apply a pending port.
// Configuration initialization is serialized, including creation of master.key.
func ResolvePort(dataDir string, portFlag int) (int, error) {
	if portFlag < 0 || portFlag > 65535 {
		return 0, errors.New("port must be between 1 and 65535")
	}
	unlock, err := startupLock(dataDir, "config")
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := updater.CheckLaunch(dataDir); err != nil {
		return 0, err
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	installation, err := st.ConsoleIdentity()
	if err != nil {
		return 0, err
	}
	activePort, activeErr := activeDaemonPort(dataDir, installation)
	if portFlag != 0 {
		// Only verified live metadata has authority over an explicit port. A
		// stale descriptor, foreign service, or failed probe cannot redirect it.
		if activePort != 0 && activePort != portFlag {
			return 0, fmt.Errorf("该数据目录的后台已在端口 %d 运行，不能使用 -port %d 启动第二个后台。请连接现有服务；如需修改端口，请在系统设置中修改并重启，或使用不同的 -data 数据目录", activePort, portFlag)
		}
		return portFlag, nil
	}
	if activeErr != nil {
		return 0, activeErr
	}
	if activePort != 0 {
		return activePort, nil
	}
	port, err := st.Port()
	if err != nil {
		return 0, err
	}
	if port == 0 {
		port = DefaultPort
	}
	if port < 1 || port > 65535 {
		return 0, errors.New("saved port must be between 1 and 65535")
	}
	return port, nil
}

type daemonReady struct {
	Service      string `json:"service"`
	Version      string `json:"version"`
	Installation string `json:"installation_id"`
	Instance     string `json:"instance_id"`
	Port         int    `json:"port"`
}

func localHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func readDaemonReady(port int) (*daemonReady, error) {
	resp, err := localHTTPClient(time.Second).Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, consoleReadyPath))
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot inspect gateway on port %d: %w", port, err)
	}
	defer resp.Body.Close()
	var ready daemonReady
	err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ready)
	if resp.StatusCode != http.StatusOK || err != nil || ready.Service != "to-router" || ready.Installation == "" || ready.Instance == "" || ready.Port != port {
		return nil, fmt.Errorf("port %d is occupied by a service without valid to-router readiness", port)
	}
	return &ready, nil
}

// IsInstanceRunning checks the service structure, never a generic HTTP status.
// Call ProbeInstallation when connecting on behalf of a particular data directory.
func IsInstanceRunning(port int) bool {
	if port <= 0 {
		port = DefaultPort
	}
	ready, err := readDaemonReady(port)
	return err == nil && ready != nil
}

// ProbeInstallation prevents accidental connections to another installation.
// Public readiness identifiers are observations, not authentication credentials.
func ProbeInstallation(dataDir string, port int) (bool, error) {
	ready, err := readDaemonReady(port)
	if err != nil || ready == nil {
		return false, err
	}
	installation, err := InstallationID(dataDir)
	if err != nil {
		return false, err
	}
	if ready.Installation != installation {
		return false, fmt.Errorf("port %d belongs to a different to-router data directory", port)
	}
	return true, nil
}

// InstallationID reads the non-secret identity once for a client connection.
func InstallationID(dataDir string) (string, error) {
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
	return st.ConsoleIdentity()
}

// CheckInstallation verifies the selected installation without opening its store.
func CheckInstallation(port int, expectedID string) error {
	ready, err := readDaemonReady(port)
	if err != nil {
		return err
	}
	if ready == nil {
		return fmt.Errorf("gateway on port %d is not ready", port)
	}
	if expectedID == "" || ready.Installation != expectedID {
		return fmt.Errorf("port %d belongs to a different to-router installation", port)
	}
	return nil
}

// ObtainAdminSessionCookie requests a local session only after installation
// verification. It never reads or sends the administrator key.
func ObtainAdminSessionCookie(dataDir string, port int) *http.Cookie {
	installation, err := InstallationID(dataDir)
	if err != nil {
		return nil
	}
	return ObtainInstallationSessionCookie(port, installation)
}

// A native client has already read its selected identity during connection.
// Reuse it rather than initializing/decrypting the whole store again.
func ObtainInstallationSessionCookie(port int, installation string) *http.Cookie {
	if err := CheckInstallation(port, installation); err != nil {
		return nil
	}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/auth/local", port), nil)
	req.Header.Set("X-To-Router", "1")
	resp, err := localHTTPClient(2 * time.Second).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "to_router_session" {
			return cookie
		}
	}
	return nil
}

// AcquireDaemonStartupLock serializes Web launchers through the point at which
// Serve is reachable. The client launch lock is separate so child startup cannot
// deadlock against a parent that is waiting for readiness.
func AcquireDaemonStartupLock(dataDir string, port int) (func(), error) {
	return startupLock(dataDir, "server")
}

func EnsureDaemonRunning(dataDir string, port int) error {
	_, err := ConnectDaemon(dataDir, port)
	return err
}

// ConnectDaemon serializes normal launches per installation, resolves the active
// port inside that critical section and returns the exact attached endpoint.
func ConnectDaemon(dataDir string, portFlag int) (int, error) {
	unlock, err := startupLock(dataDir, "launch")
	if err != nil {
		return 0, err
	}
	defer unlock()
	port, err := ResolvePort(dataDir, portFlag)
	if err != nil {
		return 0, err
	}
	return ensureDaemonAtPort(dataDir, port, portFlag == 0)
}

func ensureDaemonAtPort(dataDir string, port int, attachActive bool) (int, error) {
	if ok, err := ProbeInstallation(dataDir, port); err != nil || ok {
		return port, err
	}
	executable, err := os.Executable()
	if err != nil {
		return port, err
	}
	args := []string{"-mode", "daemon", "-data", dataDir, "-port", strconv.Itoa(port), "-autostart"}
	if attachActive {
		args = append(args, "-attach")
	}
	command := exec.Command(executable, args...)
	detachRestart(command)
	if err := command.Start(); err != nil {
		return port, fmt.Errorf("cannot start Web daemon: %w", err)
	}
	// Reap the launcher asynchronously; a duplicate launcher can exit successfully
	// while the winning process is still becoming ready.
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if attachActive {
			resolved, err := ResolvePort(dataDir, 0)
			if err != nil {
				return port, err
			}
			port = resolved
		}
		if ok, err := ProbeInstallation(dataDir, port); err != nil || ok {
			return port, err
		}
		select {
		case err := <-exited:
			if err != nil {
				return port, fmt.Errorf("Web daemon exited before readiness: %w", err)
			}
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	return port, fmt.Errorf("Web daemon did not become ready for this installation on port %d", port)
}
