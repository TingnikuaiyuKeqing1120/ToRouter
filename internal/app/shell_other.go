//go:build !windows

package app

import "os/exec"

// openWithShell delegates to the desktop's opener.
func openWithShell(target string) error {
	return exec.Command("xdg-open", target).Start()
}
