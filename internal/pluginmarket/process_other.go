//go:build !windows

package pluginmarket

import "os/exec"

func hideWindow(*exec.Cmd) {}
