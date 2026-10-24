//go:build windows

package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuardCrashFixture(t *testing.T) {
	dir := os.Getenv("TOROUTER_GUARD_FIXTURE")
	if dir == "" {
		return
	}
	guard, err := acquireGuard(dir)
	if err != nil {
		os.Exit(21)
	}
	defer guard.Close()
	if err = os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0600); err != nil {
		os.Exit(22)
	}
	time.Sleep(time.Hour)
}

func TestActualProcessDeathReleasesGuardAndStaleGate(t *testing.T) {
	p := trustedRecoveryPlan(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestGuardCrashFixture$")
	cmd.Env = append(os.Environ(), "TOROUTER_GUARD_FIXTURE="+p.Options.DataDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(p.Options.DataDir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guard fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	identity, err := identifyProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	setLockOwner(t, p, identity)
	if err = CheckLaunch(p.Options.DataDir); err == nil {
		t.Fatal("cleared live child guard")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err = CheckLaunch(p.Options.DataDir); err != nil {
		t.Fatal("process death left persistent launch block", err)
	}
}

func TestAtomicReplacementFailureLeavesInstalledPathUsable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ToRouter.exe")
	staged := filepath.Join(dir, "new.exe")
	if err := os.WriteFile(target, []byte("old executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new executable"), 0700); err != nil {
		t.Fatal(err)
	}
	locked, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicReplace(staged, target); err == nil {
		locked.Close()
		t.Fatal("replaced an executable held without delete sharing")
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "old executable" {
		locked.Close()
		t.Fatal("failed replacement removed or changed installed path", err)
	}
	locked.Close()
	if err = atomicReplace(staged, target); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(target)
	if err != nil || string(b) != "new executable" {
		t.Fatal("atomic replacement did not publish new executable", err)
	}
}

func TestDataDirectoryCaseDoesNotStrandStaleGate(t *testing.T) {
	p := trustedRecoveryPlan(t)
	if err := CheckLaunch(strings.ToUpper(p.Options.DataDir)); err != nil {
		t.Fatal("same Windows directory with different case did not recover", err)
	}
}
