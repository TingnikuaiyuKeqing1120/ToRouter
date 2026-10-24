// Command torouter provides the daemon, native desktop, CLI, pet and MCP modes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"torouter/internal/buildinfo"
	"torouter/internal/cliview"
	"torouter/internal/console"
	"torouter/internal/desktopui"
	"torouter/internal/launch"
	"torouter/internal/management"

	"torouter/internal/app"
	"torouter/internal/tray"
	"torouter/internal/updater"
)

func main() {
	if err := run(); err != nil {
		// No console exists in a GUI-subsystem binary, so the reason must be
		// shown or a double-click fails in total silence. Record it too: a
		// dismissed dialog is the only trace the user ever sees.
		if protocolMode || cliMode {
			fmt.Fprintln(os.Stderr, "to-router:", console.Redact(err.Error()))
		} else {
			app.LogStartupFailure(dataDir, err)
		}
		if !silentMode && !restartChild && !protocolMode && !cliMode {
			tray.ShowError("to-router 启动失败", err.Error())
		}
		os.Exit(1)
	}
}

// dataDir mirrors the -data flag so the failure logger writes to the same
// directory the server would have used.
var dataDir = app.DefaultDir()
var restartChild bool
var protocolMode bool
var cliMode bool
var silentMode bool

type launchConfig struct {
	mode                       string
	dir                        string
	port                       int
	attach, showKey, autostart bool
	restartGrace               int
	restartReady               string
}

func parseLaunch(args []string) (launchConfig, error) {
	cfg := launchConfig{}
	flags := flag.NewFlagSet("ToRouter", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.mode, "mode", "", "interface: web, desktop, cli or pet (daemon is internal)")
	flags.IntVar(&cfg.port, "port", 0, "HTTP listen port; omitted uses the configured port")
	flags.StringVar(&cfg.dir, "data", app.DefaultDir(), "data directory")
	flags.BoolVar(&cfg.attach, "attach", false, "prefer the active installation endpoint")
	flags.BoolVar(&cfg.showKey, "show-key", false, "show administrator login key and exit")
	flags.BoolVar(&cfg.autostart, "autostart", false, "silent login startup")
	flags.IntVar(&cfg.restartGrace, "restart-grace", 0, "internal restart handover timeout")
	flags.StringVar(&cfg.restartReady, "restart-ready", "", "internal restart readiness challenge")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if consoleErr := launch.PrepareConsole(); consoleErr != nil {
				return cfg, consoleErr
			}
			flags.SetOutput(redactedWriter{os.Stdout})
			flags.Lookup("data").DefValue = console.Redact(cfg.dir)
			flags.PrintDefaults()
		}
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	switch cfg.mode {
	case "", "web", "desktop", "cli", "pet", "daemon":
	default:
		return cfg, errors.New("mode must be web, desktop, cli or pet")
	}
	if cfg.port < 0 || cfg.port > 65535 {
		return cfg, errors.New("port must be between 1 and 65535")
	}
	return cfg, nil
}

type redactedWriter struct{ io.Writer }

func (w redactedWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.Writer, console.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func run() error {
	args := os.Args[1:]
	// Protocol and internal helper dispatch must precede flags, store access,
	// console creation and GUI startup. MCP stdout contains protocol bytes only.
	if len(args) > 0 && args[0] == "mcp" {
		protocolMode = true
		if len(args) != 1 {
			return errors.New("mcp reads TOROUTER_MCP_URL and TOROUTER_MCP_TOKEN from the environment; no arguments are accepted")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		bridgeCtx, disconnect := app.MCPDaemonContext(ctx, os.Getenv("TOROUTER_MCP_URL"))
		defer disconnect()
		return management.RunStdioFromEnvironment(bridgeCtx)
	}
	if len(args) > 0 && args[0] == updater.HelperArgument {
		protocolMode = true // A helper never opens UI or a console on failure.
		if len(args) != 2 {
			return errors.New("internal update requires exactly one plan path")
		}
		return updater.RunHelper(context.Background(), args[1], buildinfo.UpdatePublicKey)
	}
	cfg, err := parseLaunch(args)
	silentMode = cfg.mode == "daemon" || cfg.autostart || cfg.restartReady != ""
	if cfg.mode == "cli" && !cfg.autostart && cfg.restartReady == "" {
		cliMode = true
		if consoleErr := launch.PrepareConsole(); consoleErr != nil && err == nil {
			err = consoleErr
		}
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	dataDir = cfg.dir
	restartChild = cfg.restartReady != ""
	if err := updater.CheckLaunch(cfg.dir); err != nil {
		return err
	}
	if cfg.autostart || restartChild {
		cfg.mode = "daemon"
	}
	if cfg.mode == "" {
		cfg.mode, err = app.ReadDefaultLaunchMode(cfg.dir)
		if err != nil {
			return err
		}
	}
	clientArgs := []string{"-data", cfg.dir, "-port", strconv.Itoa(cfg.port)}
	switch cfg.mode {
	case "desktop", "pet":
		if cfg.showKey {
			return errors.New("use -mode cli -show-key to print the administrator key")
		}
		if cfg.mode == "pet" {
			clientArgs = append(clientArgs, "-pet")
		}
		return desktopui.Run(clientArgs)
	case "cli":
		if !cliMode {
			cliMode = true
			if err := launch.PrepareConsole(); err != nil {
				return err
			}
		}
		if cfg.showKey {
			clientArgs = append(clientArgs, "-show-key")
		}
		return cliview.Run(clientArgs)
	}
	return runDaemon(cfg)
}

func runDaemon(cfg launchConfig) error {
	port, dir := &cfg.port, &cfg.dir
	attach, showKey := &cfg.attach, &cfg.showKey
	autostart := cfg.mode == "daemon" || cfg.autostart || cfg.restartReady != ""
	restartGrace, restartReady := &cfg.restartGrace, &cfg.restartReady
	if *port != 0 && (*port < 1 || *port > 65535) {
		return errors.New("port must be between 1 and 65535")
	}
	if *showKey {
		key, err := app.ReadAdminKey(*dir)
		if err != nil {
			return err
		}
		// A tray build has no stdout; verify the copy and fall back to showing
		// the key instead of reporting a copy that did not land.
		verified, copyErr := tray.CopyVerified(key)
		if copyErr != nil || !verified {
			tray.ShowInfo("管理员登录密钥", key)
			return nil
		}
		tray.ShowInfo("管理员登录密钥", "已复制到剪贴板，粘贴到控制台登录框即可。")
		return nil
	}
	targetPort, err := app.ResolvePort(*dir, *port)
	if err != nil {
		return err
	}
	var unlock func()
	if !restartChild {
		unlock, err = app.AcquireDaemonStartupLock(*dir, targetPort)
		if err != nil {
			return err
		}
		defer unlock()
		requestedPort := *port
		if *attach {
			requestedPort = 0
		}
		targetPort, err = app.ResolvePort(*dir, requestedPort)
		if err != nil {
			return err
		}
		running, probeErr := app.ProbeInstallation(*dir, targetPort)
		if probeErr != nil {
			return probeErr
		}
		if running {
			if !autostart {
				return tray.OpenMode("web", fmt.Sprintf("http://127.0.0.1:%d", targetPort), *dir)
			}
			return nil
		}
	}

	server, err := app.Start(*dir, targetPort, time.Duration(*restartGrace)*time.Second)
	if err != nil {
		return err
	}
	defer server.Close()
	server.SetRestartReadiness(*restartReady)

	icon := filepath.Join(server.DataDir, "tray.ico")
	if err := tray.WriteIcon(icon); err != nil {
		server.Log().Warn("tray icon unavailable", "error", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	serveDone := make(chan error, 1)
	serveFinished := make(chan struct{})
	var serveErr error
	go func() { serveErr = server.Serve(ctx); serveDone <- serveErr; close(serveFinished) }()
	if unlock != nil {
		deadline := time.Now().Add(5 * time.Second)
		for {
			ready, probeErr := app.ProbeInstallation(*dir, targetPort)
			if probeErr != nil {
				stop()
				<-serveFinished
				return probeErr
			}
			if ready {
				break
			}
			if time.Now().After(deadline) {
				stop()
				<-serveFinished
				return errors.New("Web daemon readiness timed out")
			}
			time.Sleep(25 * time.Millisecond)
		}
		unlock()
	}

	// A manual start gives no other signal that the router is up, so open the
	// console once. The login autostart passes -autostart to stay silent: a
	// browser tab on every login is noise, not feedback.
	if !autostart {
		server.Log().Event("console opened on start", "origin", server.Origin())
		go func() {
			if err := tray.OpenConsole(server.Origin()); err != nil {
				server.Log().Warn("could not open the console", "error", err)
			}
		}()
	} else {
		server.Log().Event("silent start", "reason", "autostart")
	}

	trayErr := tray.Run(tray.Config{
		Title:                "to-router",
		Tooltip:              fmt.Sprintf("to-router · %s", server.Origin()),
		IconPath:             icon,
		DataDir:              server.DataDir,
		Origin:               server.Origin(),
		LogPath:              app.LogPath(server.DataDir),
		DefaultLaunchMode:    server.DefaultLaunchMode,
		SetDefaultLaunchMode: server.SetDefaultLaunchMode,
		OnQuit: func() {
			stop()
			<-serveFinished
		},
	}, serveDone)
	stop()
	<-serveFinished
	if trayErr != nil {
		return trayErr
	}
	return serveErr
}
