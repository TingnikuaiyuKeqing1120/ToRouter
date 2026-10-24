package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"torouter/internal/app"
)

func resetLaunchGlobals(t *testing.T, args ...string) {
	t.Helper()
	oldArgs, oldDir := os.Args, dataDir
	oldProtocol, oldCLI, oldRestart, oldSilent := protocolMode, cliMode, restartChild, silentMode
	t.Cleanup(func() {
		os.Args, dataDir = oldArgs, oldDir
		protocolMode, cliMode, restartChild, silentMode = oldProtocol, oldCLI, oldRestart, oldSilent
	})
	os.Args = append([]string{"ToRouter.exe"}, args...)
	protocolMode, cliMode, restartChild, silentMode = false, false, false, false
}

func TestMCPDispatchRejectsArgumentsBeforeInitializingData(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	resetLaunchGlobals(t, "mcp", "-data", dir)
	if err := run(); err == nil || !strings.Contains(err.Error(), "no arguments") {
		t.Fatalf("MCP arguments: %v", err)
	}
	if !protocolMode {
		t.Fatal("MCP errors could open a GUI dialog")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("MCP dispatch initialized normal application data")
	}
}

func TestUpdatingLaunchDoesNotOpenStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".update-lock"), []byte("synthetic-update-token"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TO_ROUTER_UPDATE_TOKEN", "")
	resetLaunchGlobals(t, "-mode", "web", "-data", dir)
	if err := run(); err == nil {
		t.Fatal("normal launch ignored update lock")
	}
	for _, name := range []string{"router.db", "master.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("locked launch created %s", name)
		}
	}
}

func TestDaemonFailuresRemainSilentWithoutSkippingNormalStartupLocks(t *testing.T) {
	for _, args := range [][]string{{"-mode", "daemon"}, {"-autostart"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".update-lock"), []byte("synthetic-lock"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TO_ROUTER_UPDATE_TOKEN", "")
			resetLaunchGlobals(t, append(args, "-data", dir)...)
			if err := run(); err == nil {
				t.Fatal("locked launch unexpectedly succeeded")
			}
			if !silentMode || restartChild {
				t.Fatal("silent daemon error must suppress dialogs without impersonating restart child")
			}
		})
	}
}

func availableTestPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestExplicitPortCannotStartSecondBackendForActiveData(t *testing.T) {
	dir := t.TempDir()
	server, err := app.Start(dir, availableTestPort(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var serveErr error
	go func() { serveErr = server.Serve(context.Background()); close(done) }()
	t.Cleanup(func() { server.Shutdown(); <-done; server.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		ready, err := app.ProbeInstallation(dir, server.Port)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real backend did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	conflicting := availableTestPort(t)
	for name, resolve := range map[string]func() error{
		"port resolution":   func() error { _, err := app.ResolvePort(dir, conflicting); return err },
		"client connection": func() error { _, err := app.ConnectDaemon(dir, conflicting); return err },
		"daemon launch":     func() error { return runDaemon(launchConfig{mode: "daemon", dir: dir, port: conflicting}) },
	} {
		t.Run(name, func(t *testing.T) {
			err := resolve()
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(server.Port)) || !strings.Contains(err.Error(), "-data") || !strings.Contains(err.Error(), "设置") {
				t.Fatalf("conflicting explicit port lacks actionable error: %v", err)
			}
		})
	}
	if port, err := app.ResolvePort(dir, server.Port); err != nil || port != server.Port {
		t.Fatalf("same explicit port rejected: %d %v", port, err)
	}
	if err := runDaemon(launchConfig{mode: "daemon", dir: dir, port: server.Port}); err != nil {
		t.Fatalf("same port daemon attachment failed: %v", err)
	}
	server.Shutdown()
	<-done
	if serveErr != nil {
		t.Fatal(serveErr)
	}
	if port, err := app.ResolvePort(dir, conflicting); err != nil || port != conflicting {
		t.Fatalf("completed shutdown blocked explicit restart target: %d %v", port, err)
	}
}

func TestExplicitPortIgnoresUnverifiedActiveMetadata(t *testing.T) {
	for _, state := range []string{"closed", "reused-instance", "foreign-installation", "ordinary-http"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			identity, err := app.InstallationID(dir)
			if err != nil {
				t.Fatal(err)
			}
			var recordedPort int
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if state == "ordinary-http" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"service": "to-router", "installation_id": identity, "instance_id": "replacement", "port": recordedPort})
			}))
			defer fake.Close()
			recordedPort = fake.Listener.Addr().(*net.TCPAddr).Port
			recordedIdentity := identity
			if state == "foreign-installation" {
				recordedIdentity = "synthetic-other-installation"
			}
			record, err := json.Marshal(map[string]any{"service": "to-router", "installation_id": recordedIdentity, "instance_id": "recorded", "port": recordedPort})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".active-daemon.json"), record, 0600); err != nil {
				t.Fatal(err)
			}
			if state == "closed" {
				fake.Close()
			}
			requested := availableTestPort(t)
			port, err := app.ResolvePort(dir, requested)
			if err != nil || port != requested {
				t.Fatalf("unverified %s descriptor overrode explicit port: %d %v", state, port, err)
			}
		})
	}
}
