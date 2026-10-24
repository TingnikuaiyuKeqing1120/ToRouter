//go:build windows

package updater

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200}
}

func configureView(cmd *exec.Cmd, mode string) {
	// STARTF_USESHOWWINDOW/SW_HIDE suppresses Wails' first native ShowWindow.
	// Only suppress a console here; visible views must control their own show.
	flags := uint32(0x08000000) // CREATE_NO_WINDOW
	if mode == "cli" {
		flags = 0x00000200
	} // CLI allocates/attaches its console itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
}

func waitProcesses(ctx context.Context, pids []int) error {
	for _, pid := range pids {
		if pid <= 0 || pid == os.Getpid() {
			return errors.New("invalid process wait target")
		}
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			continue
		}
		if err != nil {
			return errors.New("cannot safely wait for application process")
		}
		for {
			state, e := windows.WaitForSingleObject(h, 100)
			if e != nil {
				windows.CloseHandle(h)
				return e
			}
			if state == windows.WAIT_OBJECT_0 {
				break
			}
			select {
			case <-ctx.Done():
				windows.CloseHandle(h)
				return ctx.Err()
			default:
			}
		}
		windows.CloseHandle(h)
	}
	return nil
}

func processRunning(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}

func openExclusive(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func acquireGuard(dataDir string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(filepath.Join(dataDir, ".update-guard"))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), "update transaction guard"), nil
}

// Both files live on the installation volume. Never remove/rename the target
// out of the way first: even a stopped helper must leave a callable EXE path.
func atomicReplace(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func atomicPublish(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_WRITE_THROUGH)
}

func identifyProcess(pid int) (processIdentity, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return processIdentity{}, err
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return processIdentity{}, err
	}
	return processIdentity{pid, uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)}, nil
}

func identityAlive(identity processIdentity) (bool, error) {
	if identity.PID <= 0 || identity.Created == 0 {
		return false, errors.New("missing update process identity")
	}
	actual, err := identifyProcess(identity.PID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return actual == identity && processRunning(identity.PID), nil
}
