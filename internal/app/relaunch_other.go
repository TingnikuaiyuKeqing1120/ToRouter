//go:build !windows

package app

import (
	"os/exec"
	"syscall"
)

// relaunch starts a detached copy of this executable. The console restart
// button uses it so the new process inherits the same data directory and does
// not open a browser tab the user did not ask for.
func detachRestart(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
