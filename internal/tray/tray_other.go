//go:build !windows

package tray

import (
	"fmt"
	"os"
	"os/exec"
)

// Run has no tray on this platform: the server keeps running and the reason is
// printed to stderr. The web console remains the control surface everywhere.
func Run(cfg Config, serveDone <-chan error) error {
	fmt.Fprintf(os.Stderr, "to-router: %s (no tray support on this platform; use the web console)\n", cfg.Origin)
	err := <-serveDone
	// Same contract as the Windows build: the quit hook runs after serving has
	// finished, never on a message-pump thread.
	if cfg.OnQuit != nil {
		cfg.OnQuit()
	}
	return err
}

// CopyVerified is unavailable without a desktop session.
func CopyVerified(string) (bool, error) {
	return false, fmt.Errorf("clipboard not supported on this platform")
}

func ShowError(title, body string) { fmt.Fprintf(os.Stderr, "%s: %s\n", title, body) }

func ShowInfo(title, body string) { fmt.Fprintf(os.Stderr, "%s: %s\n", title, body) }

func AutostartEnabled() bool { return false }

func SetAutostart(bool) error { return fmt.Errorf("autostart not supported on this platform") }

func AutostartEnabledForData(string) bool { return false }

func SetAutostartForData(bool, string) error {
	return fmt.Errorf("autostart not supported on this platform")
}

func openWithShell(target string) error { return exec.Command("xdg-open", target).Start() }
