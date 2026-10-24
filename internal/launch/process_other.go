//go:build !windows

package launch

import "os/exec"

func configureView(*exec.Cmd, string) {}
