//go:build windows

package cliview

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"torouter/internal/desktop"
)

var consoleKernel = windows.NewLazySystemDLL("kernel32.dll")
var consoleUser = windows.NewLazySystemDLL("user32.dll")

type cliViewRecord struct {
	PID       uint32  `json:"pid"`
	Created   uint64  `json:"created"`
	Window    uintptr `json:"window"`
	WindowPID uint32  `json:"window_pid"`
}

// Byte zero is the lifetime lease. Metadata starts at byte one so another
// process can read it while Windows enforces the exclusive lock on byte zero.
// A crashed process releases the OS lock; an old record never claims ownership.
func claimCLIView(dataDir string) (func(), bool, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(filepath.Join(dataDir, ".cli-view.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			file.Close()
			return nil, false, err
		}
		var record cliViewRecord
		data := make([]byte, 1024)
		n, readErr := file.ReadAt(data, 1)
		if (readErr == nil || errors.Is(readErr, io.EOF)) && json.Unmarshal(data[:n], &record) == nil && liveCLIProcess(record) {
			file.Close()
			return nil, true, recallCLIWindow(record)
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, true, errors.New("CLI 视图正在启动，请稍后从原终端查看")
		}
		time.Sleep(25 * time.Millisecond)
	}
	// Do not leave the inherited shell title modified after Ctrl+C.
	previousTitle := consoleTitle()
	title := "to-router CLI · " + desktop.PetInstanceID(dataDir)
	setConsoleTitle(title)
	window := currentConsoleWindow()
	record := cliViewRecord{PID: uint32(os.Getpid()), Window: window, WindowPID: windowProcess(window)}
	record.Created = processCreated(windows.CurrentProcess())
	data, err := json.Marshal(record)
	if err == nil {
		err = file.Truncate(1)
	}
	if err == nil {
		_, err = file.WriteAt(data, 1)
	}
	if err != nil {
		setConsoleTitle(previousTitle)
		file.Close()
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if consoleTitle() == title {
				setConsoleTitle(previousTitle)
			}
			_ = file.Truncate(1)
			_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
			_ = file.Close()
		})
	}, false, nil
}

func processCreated(handle windows.Handle) uint64 {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
}

func liveCLIProcess(record cliViewRecord) bool {
	if record.PID == 0 || record.Created == 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, record.PID)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	wait, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && wait == uint32(windows.WAIT_TIMEOUT) && processCreated(handle) == record.Created
}

func windowProcess(window uintptr) uint32 {
	if window == 0 {
		return 0
	}
	var pid uint32
	consoleUser.NewProc("GetWindowThreadProcessId").Call(window, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func currentConsoleWindow() uintptr {
	window, _, _ := consoleKernel.NewProc("GetConsoleWindow").Call()
	visible, _, _ := consoleUser.NewProc("IsWindowVisible").Call(window)
	if window != 0 && visible != 0 {
		return window
	}
	// A pseudoconsole's GetConsoleWindow is not a visible terminal. Avoid
	// guessing an unrelated foreground window or activating the wrong tab.
	return 0
}

func recallCLIWindow(record cliViewRecord) error {
	if record.Window == 0 || record.WindowPID == 0 || windowProcess(record.Window) != record.WindowPID {
		return errors.New("CLI 视图已在运行；当前终端宿主不支持自动唤回，请切换至原终端")
	}
	visible, _, _ := consoleUser.NewProc("IsWindowVisible").Call(record.Window)
	if visible == 0 {
		return errors.New("CLI 视图已在运行；原终端窗口不可见，请从终端宿主切换至原视图")
	}
	consoleUser.NewProc("ShowWindowAsync").Call(record.Window, 9) // SW_RESTORE
	ok, _, _ := consoleUser.NewProc("SetForegroundWindow").Call(record.Window)
	if ok == 0 {
		return fmt.Errorf("CLI 视图已在运行；Windows 未允许切换前台，请切换至原终端")
	}
	return nil
}

func consoleTitle() string {
	var text [1024]uint16
	n, _, _ := consoleKernel.NewProc("GetConsoleTitleW").Call(uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(text[:])
}

func setConsoleTitle(title string) {
	text, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	consoleKernel.NewProc("SetConsoleTitleW").Call(uintptr(unsafe.Pointer(text)))
}
