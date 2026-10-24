//go:build windows

package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestViewFirstShowFixture(t *testing.T) {
	marker := os.Getenv("TOROUTER_VIEW_FIRST_SHOW_FIXTURE")
	if marker == "" {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	user := windows.NewLazySystemDLL("user32.dll")
	defProc := user.NewProc("DefWindowProcW")
	callback := syscall.NewCallback(func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
		result, _, _ := defProc.Call(hwnd, uintptr(message), wparam, lparam)
		return result
	})
	module, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)
	class, _ := windows.UTF16PtrFromString("SyntheticUpdaterFirstShowFixture")
	type windowClass struct {
		Style                              uint32
		Proc                               uintptr
		ClassExtra, WindowExtra            int32
		Instance, Icon, Cursor, Background uintptr
		Menu, Name                         *uint16
	}
	definition := windowClass{Proc: callback, Instance: module, Name: class}
	if result, _, err := user.NewProc("RegisterClassW").Call(uintptr(unsafe.Pointer(&definition))); result == 0 {
		t.Fatal("register synthetic native class", err)
	}
	hwnd, _, err := user.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(class)), 0x00cf0000, 0, 0, 240, 120, 0, 0, module, 0)
	if hwnd == 0 {
		t.Fatal("create synthetic native window", err)
	}
	defer user.NewProc("DestroyWindow").Call(hwnd)
	user.NewProc("ShowWindow").Call(hwnd, 5) // SW_SHOW, matching Wails' win32.ShowWindow.
	visible, _, _ := user.NewProc("IsWindowVisible").Call(hwnd)
	value := "hidden"
	if visible != 0 {
		value = "visible"
	}
	if err = os.WriteFile(marker, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

// Exercise the actual Windows first-show contract rather than just asserting
// SysProcAttr fields. The detached backend configuration is a negative control.
func TestRestoredNativeViewsControlTheirFirstShow(t *testing.T) {
	for _, mode := range []string{"backend-control", "desktop", "pet"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "visibility")
			cmd := exec.Command(os.Args[0], "-test.run=^TestViewFirstShowFixture$")
			cmd.Env = append(os.Environ(), "TOROUTER_VIEW_FIRST_SHOW_FIXTURE="+marker)
			if mode == "backend-control" {
				detach(cmd)
			} else {
				configureView(cmd, mode)
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("first-show fixture: %v %s", err, output)
			}
			value, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			want := "visible"
			if mode == "backend-control" {
				want = "hidden"
			}
			if string(value) != want {
				t.Fatalf("first native ShowWindow for %s was %s; want %s", mode, value, want)
			}
		})
	}
}

func TestRestoredCLIAllowsConsoleAllocation(t *testing.T) {
	cmd := exec.Command("synthetic-cli.exe")
	configureView(cmd, "cli")
	if cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&(0x08000000|0x00000008) != 0 {
		t.Fatal("restored CLI is forbidden to show/allocate its own console")
	}
}
