//go:build windows

package launch

import (
	"os/exec"
	"syscall"
)

func configureView(command *exec.Cmd, mode string) {
	flags := uint32(0x08000000) // CREATE_NO_WINDOW; Wails creates its own window.
	if mode == "cli" {
		flags = 0x00000200
	} // CLI allocates or attaches a console itself.
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
}
