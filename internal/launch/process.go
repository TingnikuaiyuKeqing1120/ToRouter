package launch

import (
	"os"
	"os/exec"
)

// ViewCommand opens a view from this same executable. No companion executable
// or installation directory convention is part of the distribution contract.
func ViewCommand(mode string, args ...string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, append([]string{"-mode", mode}, args...)...)
	configureView(command, mode)
	return command, nil
}
