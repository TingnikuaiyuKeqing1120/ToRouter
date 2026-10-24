// Package tray gives to-router a system-tray front end so the resident server
// can run without a console window. The Windows implementation is pure Go
// (golang.org/x/sys only) to keep the project free of CGO; other platforms fall
// back to running without a tray icon, and the web console stays the portable
// control surface.
package tray

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"torouter/internal/desktop"
)

//go:embed assets/tray.ico
var iconFS embed.FS

// Config is everything the tray needs to present the running server.
type Config struct {
	Title    string
	Tooltip  string
	IconPath string
	DataDir  string
	Origin   string
	LogPath  string
	// Read current settings for every default action. Direct opens and changing
	// the preference do not close any existing view.
	DefaultLaunchMode    func() string
	SetDefaultLaunchMode func(string) error
	// OnQuit is called when the user chooses Quit; it must stop the server so
	// the process can exit.
	OnQuit func()
}

// WriteIcon materialises the embedded icon next to the database so Windows can
// load it from a stable path. Operators can replace that file to rebrand the
// tray icon without rebuilding.
func WriteIcon(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := iconFS.ReadFile("assets/tray.ico")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// OpenConsole opens the web console in the default browser. It is also what the
// tray menu and the startup path use, so both go through the same handler.
func OpenConsole(origin string) error { return openWithShell(origin) }

func OpenMode(mode, origin, dataDir string) error {
	switch mode {
	case "web":
		return OpenConsole(origin)
	case "desktop":
		return desktop.OpenNativeConsole(origin, dataDir)
	case "cli":
		return desktop.OpenCLI(origin, dataDir)
	default:
		return fmt.Errorf("unknown launch mode")
	}
}

func (c Config) defaultMode() string {
	if c.DefaultLaunchMode != nil {
		switch mode := c.DefaultLaunchMode(); mode {
		case "web", "desktop", "cli":
			return mode
		}
	}
	return "web"
}
