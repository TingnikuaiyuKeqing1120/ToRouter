//go:build !windows

package updater

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func detach(cmd *exec.Cmd)                  { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func configureView(cmd *exec.Cmd, _ string) { detach(cmd) }
func processRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
func waitProcesses(ctx context.Context, pids []int) error {
	for _, pid := range pids {
		if pid <= 0 || pid == os.Getpid() {
			return errors.New("invalid process wait target")
		}
		for processRunning(pid) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return nil
}

// Portable support is best effort; the release target and lock guarantees are
// Windows. No claim of Unix cross-process exclusive SQLite locks is made.
func openExclusive(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDWR, 0) }

func acquireGuard(dataDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, ".update-guard"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func atomicReplace(from, to string) error { return os.Rename(from, to) }

func atomicPublish(from, to string) error {
	if err := os.Link(from, to); err != nil {
		return err
	}
	return os.Remove(from)
}

func identifyProcess(pid int) (processIdentity, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return processIdentity{}, err
	}
	end := strings.LastIndex(string(b), ")")
	if end < 0 {
		return processIdentity{}, errors.New("process identity unavailable")
	}
	fields := strings.Fields(string(b)[end+1:])
	if len(fields) < 20 {
		return processIdentity{}, errors.New("process identity unavailable")
	}
	created, err := strconv.ParseUint(fields[19], 10, 64)
	return processIdentity{pid, created}, err
}
func identityAlive(identity processIdentity) (bool, error) {
	if identity.PID <= 0 || identity.Created == 0 {
		return false, errors.New("missing update process identity")
	}
	actual, err := identifyProcess(identity.PID)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return actual == identity && processRunning(identity.PID), nil
}
