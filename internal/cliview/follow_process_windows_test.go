//go:build windows

package cliview

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"torouter/internal/app"
)

func TestCLIFollowProcessHelper(t *testing.T) {
	dir := os.Getenv("TOROUTER_FOLLOW_TEST_DIR")
	if dir == "" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("cli-follow-helper", flag.ContinueOnError)
	os.Args = []string{os.Args[0], "-attach", "-data", dir, "-port", os.Getenv("TOROUTER_FOLLOW_TEST_PORT")}
	if err := run(os.Args[1:], flag.CommandLine); err != nil {
		t.Fatal(err)
	}
}

func TestCLIAttachedProcessExitsAndReleasesLeaseOnBackendStop(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	dir := t.TempDir()
	server, err := app.Start(dir, port, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	served := make(chan error, 1)
	go func() { served <- server.Serve(context.Background()) }()
	installation, err := app.InstallationID(dir)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for app.CheckInstallation(port, installation) != nil {
		if time.Now().After(deadline) {
			t.Fatal("real backend did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	outputPath := filepath.Join(t.TempDir(), "cli-output.txt")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIFollowProcessHelper$")
	cmd.Env = append(os.Environ(), "TOROUTER_FOLLOW_TEST_DIR="+dir, fmt.Sprintf("TOROUTER_FOLLOW_TEST_PORT=%d", port))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	deadline = time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(outputPath)
		if strings.Contains(string(data), "[CLI 视图]") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CLI did not attach: %s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The banner precedes reading the installation and beginning its watcher.
	time.Sleep(200 * time.Millisecond)
	server.Shutdown()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			data, _ := os.ReadFile(outputPath)
			t.Fatalf("CLI exit: %v, %s", err, data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CLI stayed alive after backend stop")
	}
	data, _ := os.ReadFile(outputPath)
	if !strings.Contains(string(data), "后台服务已停止，CLI 视图退出") {
		t.Fatalf("CLI did not report lifecycle stop: %s", data)
	}
	if info, err := os.Stat(filepath.Join(dir, ".cli-view.lock")); err != nil || info.Size() != 1 {
		t.Fatalf("CLI lease metadata was not released: %v", err)
	}
	reopened, reopenStop, result := startViewProbe(t, dir)
	if result.Existing || result.Error != "" {
		t.Fatalf("stopped CLI blocked reopening: %+v", result)
	}
	stopViewProbe(t, reopened, reopenStop)
}
