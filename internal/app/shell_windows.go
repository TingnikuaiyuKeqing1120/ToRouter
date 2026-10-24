package app

import "os/exec"

// openWithShell uses rundll32 so no console window flashes.
func openWithShell(target string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
}
