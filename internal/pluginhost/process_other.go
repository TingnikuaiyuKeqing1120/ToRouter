//go:build !windows

package pluginhost

import "os/exec"

func hideWindow(*exec.Cmd) {}
