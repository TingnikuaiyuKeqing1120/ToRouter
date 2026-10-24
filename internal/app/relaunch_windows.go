//go:build windows

package app

import (
	"os/exec"
	"syscall"
)

// relaunch starts a detached copy of this executable. The console restart
// button uses it so the new process inherits the same data directory and does
// not open a browser tab the user did not ask for.
func detachRestart(command *exec.Cmd) {
	// A GUI-subsystem parent has no console; keep the child from flashing one
	// and detach it so it survives this process exiting.
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
}
