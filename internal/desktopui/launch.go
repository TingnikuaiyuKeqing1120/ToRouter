package desktopui

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"torouter/internal/app"
	"torouter/internal/desktop"
)

type launchConfig struct {
	dataDir string
	port    int
	pet     bool
}

func (c launchConfig) origin() string { return fmt.Sprintf("http://127.0.0.1:%d", c.port) }
func (c launchConfig) args() []string {
	args := []string{"-data", c.dataDir, "-port", strconv.Itoa(c.port)}
	if c.pet {
		args = append(args, "--pet")
	}
	return args
}

func parseLaunch(args []string, cwd string) (launchConfig, error) {
	cfg := launchConfig{}
	flags := flag.NewFlagSet("to-router-desktop", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.dataDir, "data", app.DefaultDir(), "data directory")
	flags.IntVar(&cfg.port, "port", 0, "HTTP listen port; omitted uses saved port")
	flags.BoolVar(&cfg.pet, "pet", false, "floating desktop companion")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected desktop argument %q", flags.Arg(0))
	}
	if cfg.port < 0 || cfg.port > 65535 {
		return cfg, fmt.Errorf("port must be between 1 and 65535")
	}
	if !filepath.IsAbs(cfg.dataDir) {
		cfg.dataDir = filepath.Join(cwd, cfg.dataDir)
	}
	absolute, err := filepath.Abs(cfg.dataDir)
	if err != nil {
		return cfg, err
	}
	cfg.dataDir = filepath.Clean(absolute)
	if resolved, err := filepath.EvalSymlinks(cfg.dataDir); err == nil {
		cfg.dataDir = resolved
	}
	return cfg, nil
}

func singletonID(dataDir string) string {
	return desktop.PetInstanceID(dataDir)
}

func (c launchConfig) windowID() string {
	if c.pet {
		return singletonID(c.dataDir)
	}
	return "to-router-gui-" + singletonID(c.dataDir)
}
