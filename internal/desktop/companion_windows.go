//go:build windows

package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var windowAPI = windows.NewLazySystemDLL("user32.dll")
var findWindow = windowAPI.NewProc("FindWindowW")
var showWindow = windowAPI.NewProc("ShowWindow")
var foregroundWindow = windowAPI.NewProc("SetForegroundWindow")
var postWindowMessage = windowAPI.NewProc("PostMessageW")

func petWindow(origin, dataDir string) uintptr {
	title, err := windows.UTF16PtrFromString(PetWindowTitle(origin, dataDir))
	if err != nil {
		return 0
	}
	hwnd, _, _ := findWindow.Call(0, uintptr(unsafe.Pointer(title)))
	return hwnd
}

func companionRunning(origin, dataDir string) bool {
	hwnd := petWindow(origin, dataDir)
	visible, _, _ := windowAPI.NewProc("IsWindowVisible").Call(hwnd)
	return hwnd != 0 && visible != 0
}

// The native pet starts hidden until its transparent tool-window style and
// WebView are prepared. A racing launcher must use native's readiness gate.
func recallCompanion(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	visible, _, _ := windowAPI.NewProc("IsWindowVisible").Call(hwnd)
	style, _, _ := windowAPI.NewProc("GetWindowLongW").Call(hwnd, ^uintptr(19))
	if visible == 0 || uint32(style)&0x80 == 0 || uint32(style)&0x40000 != 0 {
		return false
	}
	showWindow.Call(hwnd, 9)
	foregroundWindow.Call(hwnd)
	return true
}

func nativeCommand(executable, origin, dataDir string, pet bool) (*exec.Cmd, error) {
	args, err := LaunchArgs(origin, dataDir)
	if err != nil {
		return nil, err
	}
	mode := "desktop"
	if pet {
		mode = "pet"
	}
	info, err := os.Stat(executable)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("executable is a directory")
	}
	cmd := exec.Command(executable, append([]string{"-mode", mode}, args...)...)
	// Suppress an extra console without hiding the native window's first show.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	return cmd, nil
}

func openNative(origin, dataDir string, pet bool) error {
	if pet {
		if recallCompanion(petWindow(origin, dataDir)) {
			return nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd, err := nativeCommand(exe, origin, dataDir, pet)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动原生桌面窗口: %w", err)
	}
	return cmd.Process.Release()
}

func closeCompanion(origin, dataDir string) error {
	hwnd := petWindow(origin, dataDir)
	if hwnd == 0 {
		return nil
	}
	result, _, err := postWindowMessage.Call(hwnd, 0x0010, 0, 0) // WM_CLOSE; never stop the daemon.
	if result == 0 {
		return fmt.Errorf("无法关闭桌宠窗口: %w", err)
	}
	return nil
}
