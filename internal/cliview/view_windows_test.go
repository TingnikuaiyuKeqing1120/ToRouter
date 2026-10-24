//go:build windows

package cliview

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type viewProbeResult struct {
	Existing bool
	Error    string
}

func TestCLIViewProcessHelper(t *testing.T) {
	dir := os.Getenv("TOROUTER_CLI_TEST_DIR")
	if dir == "" {
		return
	}
	release, existing, err := claimCLIView(dir)
	result := viewProbeResult{Existing: existing}
	if err != nil {
		result.Error = err.Error()
	}
	if release != nil {
		defer release()
	}
	data, _ := json.Marshal(result)
	if err := os.WriteFile(os.Getenv("TOROUTER_CLI_TEST_RESULT"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if existing || err != nil {
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(os.Getenv("TOROUTER_CLI_TEST_STOP")); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("helper never received close request")
}

func startViewProbe(t *testing.T, dir string) (*exec.Cmd, string, viewProbeResult) {
	t.Helper()
	files := t.TempDir()
	resultPath := filepath.Join(files, "result.json")
	stopPath := filepath.Join(files, "stop")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIViewProcessHelper$")
	cmd.Env = append(os.Environ(), "TOROUTER_CLI_TEST_DIR="+dir, "TOROUTER_CLI_TEST_RESULT="+resultPath, "TOROUTER_CLI_TEST_STOP="+stopPath)
	// These probes validate cross-process ownership without stealing the user's
	// foreground terminal. Actual console recall is a separate UI acceptance.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(resultPath); err == nil {
			var result viewProbeResult
			if json.Unmarshal(data, &result) == nil {
				return cmd, stopPath, result
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("CLI probe did not report its ownership")
	return nil, "", viewProbeResult{}
}

func stopViewProbe(t *testing.T, cmd *exec.Cmd, stop string) {
	t.Helper()
	if err := os.WriteFile(stop, []byte("close"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCLIViewSingleInstanceIsolationAndReopen(t *testing.T) {
	dir := t.TempDir()
	first, firstStop, firstResult := startViewProbe(t, dir)
	if firstResult.Existing || firstResult.Error != "" {
		t.Fatalf("first view: %+v", firstResult)
	}
	duplicate, _, duplicateResult := startViewProbe(t, dir)
	if !duplicateResult.Existing {
		t.Fatalf("duplicate view obtained ownership: %+v", duplicateResult)
	}
	if err := duplicate.Wait(); err != nil {
		t.Fatal(err)
	}
	other, otherStop, otherResult := startViewProbe(t, t.TempDir())
	if otherResult.Existing || otherResult.Error != "" {
		t.Fatalf("other installation blocked: %+v", otherResult)
	}
	stopViewProbe(t, first, firstStop)
	reopened, reopenedStop, reopenedResult := startViewProbe(t, dir)
	if reopenedResult.Existing || reopenedResult.Error != "" {
		t.Fatalf("closed view prevented reopening: %+v", reopenedResult)
	}
	stopViewProbe(t, reopened, reopenedStop)
	stopViewProbe(t, other, otherStop)
}

func TestCLIViewRecoversAfterProcessExitWithoutCleanup(t *testing.T) {
	dir := t.TempDir()
	first, _, result := startViewProbe(t, dir)
	if result.Existing || result.Error != "" {
		t.Fatalf("first view: %+v", result)
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	next, stop, result := startViewProbe(t, dir)
	if result.Existing || result.Error != "" {
		t.Fatalf("stale record prevented reopening: %+v", result)
	}
	stopViewProbe(t, next, stop)
}
