// Package desktop launches views of the unified executable.
package desktop

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"torouter/internal/launch"
)

func companionURL(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("invalid local console origin")
	}
	host := u.Hostname()
	if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return "", fmt.Errorf("desktop companion requires a loopback origin")
	}
	return strings.TrimSuffix(origin, "/") + "/#/pet", nil
}

// LaunchArgs preserves the selected daemon context for every native child.
func LaunchArgs(origin, dataDir string) ([]string, error) {
	if _, err := companionURL(origin); err != nil {
		return nil, err
	}
	u, _ := url.Parse(origin)
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid local console port")
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("missing data directory")
	}
	return []string{"-data", dataDir, "-port", strconv.Itoa(port)}, nil
}

// PetInstanceID normalizes path aliases before deriving a stable window owner.
func PetInstanceID(dataDir string) string {
	canonical, err := filepath.Abs(dataDir)
	if err != nil {
		canonical = filepath.Clean(dataDir)
	}
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}
	canonical = filepath.Clean(canonical)
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%x", sum[:16])
}

func PetWindowTitle(origin, dataDir string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return "to-router 桌面伴侣"
	}
	return "to-router 桌面伴侣 · 127.0.0.1:" + u.Port() + " · " + PetInstanceID(dataDir)
}

// OpenCompanion opens or recalls the native companion for this daemon.
func OpenCompanion(origin, dataDir string) error {
	if _, err := LaunchArgs(origin, dataDir); err != nil {
		return err
	}
	return openNative(origin, dataDir, true)
}

func OpenNativeConsole(origin, dataDir string) error {
	if _, err := LaunchArgs(origin, dataDir); err != nil {
		return err
	}
	return openNative(origin, dataDir, false)
}

func OpenCLI(origin, dataDir string) error {
	args, err := LaunchArgs(origin, dataDir)
	if err != nil {
		return err
	}
	command, err := launch.ViewCommand("cli", args...)
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// CloseCompanion closes only the companion window belonging to this origin.
func CloseCompanion(origin, dataDir string) error {
	if _, err := companionURL(origin); err != nil {
		return err
	}
	return closeCompanion(origin, dataDir)
}

// CompanionRunning reports the actual native window, not a persisted preference
// or a successful launch request. External window closure is immediately visible.
func CompanionRunning(origin, dataDir string) bool {
	if _, err := LaunchArgs(origin, dataDir); err != nil {
		return false
	}
	return companionRunning(origin, dataDir)
}
