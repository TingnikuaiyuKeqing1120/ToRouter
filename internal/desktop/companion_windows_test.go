//go:build windows

package desktop

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCompanionLaunchArgumentsDoNotPutSecretsInURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ToRouter.exe")
	if err := os.WriteFile(path, []byte("not executed"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, pet := range []bool{false, true} {
		cmd, err := nativeCommand(path, "http://127.0.0.1:19191", `C:\isolated data`, pet)
		if err != nil {
			t.Fatal(err)
		}
		mode := "desktop"
		if pet {
			mode = "pet"
		}
		want := []string{path, "-mode", mode, "-data", `C:\isolated data`, "-port", "19191"}
		if !reflect.DeepEqual(cmd.Args, want) {
			t.Fatalf("args: %v", cmd.Args)
		}
		if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&0x08000000 == 0 {
			t.Fatal("launch would create a console window")
		}
		if cmd.SysProcAttr.HideWindow {
			t.Fatal("native window would inherit a hidden initial show state")
		}
	}
}

func TestCompanionRunningTracksWindowLifetimeAndInstallation(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	dir := t.TempDir()
	origin := "http://127.0.0.1:19201"
	create := func() uintptr {
		class := windows.StringToUTF16Ptr("STATIC")
		title := windows.StringToUTF16Ptr(PetWindowTitle(origin, dir))
		window, _, err := windowAPI.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 10, 10, 0, 0, 0, 0)
		if window == 0 {
			t.Fatalf("create native window: %v", err)
		}
		return window
	}
	if CompanionRunning(origin, dir) {
		t.Fatal("missing window reported running")
	}
	window := create()
	defer func() {
		if window != 0 {
			windowAPI.NewProc("DestroyWindow").Call(window)
		}
	}()
	if CompanionRunning(origin, dir) {
		t.Fatal("hidden startup window reported ready")
	}
	if recallCompanion(window) {
		t.Fatal("hidden startup window was recalled before native preparation")
	}
	if visible, _, _ := windowAPI.NewProc("IsWindowVisible").Call(window); visible != 0 {
		t.Fatal("recall made an unprepared pet visible")
	}
	showWindow.Call(window, 4)
	if !CompanionRunning(origin, dir) {
		t.Fatal("live native window not detected")
	}
	if CompanionRunning(origin, t.TempDir()) {
		t.Fatal("different installation shares state")
	}
	windowAPI.NewProc("DestroyWindow").Call(window)
	window = 0
	if CompanionRunning(origin, dir) {
		t.Fatal("external close left stale running state")
	}
	window = create()
	showWindow.Call(window, 4)
	if !CompanionRunning(origin, dir) {
		t.Fatal("reopened window not detected")
	}
	if err := CloseCompanion(origin, dir); err != nil {
		t.Fatal(err)
	}
	// Pump the real WM_CLOSE posted by the command, rather than treating a
	// successful PostMessage as evidence that the window has closed.
	var message struct {
		Window         uintptr
		Message        uint32
		WParam, LParam uintptr
		Time           uint32
		X, Y           int32
		Private        uint32
	}
	deadline := time.Now().Add(time.Second)
	for CompanionRunning(origin, dir) && time.Now().Before(deadline) {
		got, _, _ := windowAPI.NewProc("PeekMessageW").Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, 1)
		if got != 0 {
			windowAPI.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&message)))
			windowAPI.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&message)))
		} else {
			time.Sleep(time.Millisecond)
		}
	}
	if CompanionRunning(origin, dir) {
		t.Fatal("close command did not remove native window")
	}
	window = 0
}

func TestMissingNativeReportsError(t *testing.T) {
	if _, err := nativeCommand(t.TempDir(), "http://127.0.0.1:19191", t.TempDir(), true); err == nil {
		t.Fatal("missing native executable accepted")
	}
}
