//go:build windows

package desktopui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestClosingExpandedPetPersistsAndRestoresActualPosition(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hwnd, cleanup := createHiddenNativeTestWindow(t, 0)
	defer cleanup()
	dir := t.TempDir()
	c := newNativeClient(launchConfig{pet: true, dataDir: dir}, nil)
	var initial nativeRect
	getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&initial)))
	if err := c.resize(350); err != nil {
		t.Fatal(err)
	}
	opts := c.options()
	if opts.OnBeforeClose == nil {
		t.Fatal("pet close has no position persistence hook")
	}
	if opts.OnBeforeClose(context.Background()) {
		t.Fatal("position saving blocked close")
	}
	want := petPosition{int(initial.Left), int(initial.Bottom)}
	if got, ok := readPetPosition(dir); !ok || got != want {
		t.Fatalf("expanded close lost idle position: got %+v/%v want %+v", got, ok, want)
	}
	setWindowPos.Call(hwnd, 0, 40, 40, 260, 200, 0x0004|0x0010)
	reopened := newNativeClient(launchConfig{pet: true, dataDir: dir}, nil)
	if err := reopened.placement.restore(); err != nil {
		t.Fatal(err)
	}
	var restored nativeRect
	getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&restored)))
	if restored.Left != initial.Left || restored.Bottom != initial.Bottom {
		t.Fatalf("reopened window did not restore position: %+v want %+v", restored, initial)
	}
	if err := writePetPosition(dir, petPosition{1000000, 1000000}); err != nil {
		t.Fatal(err)
	}
	fallback := newNativeClient(launchConfig{pet: true, dataDir: dir}, nil)
	if err := fallback.placement.restore(); err != nil {
		t.Fatal(err)
	}
	getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&restored)))
	monitor, _, _ := monitorFromWindow.Call(hwnd, 2)
	work := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	getMonitorInfo.Call(monitor, uintptr(unsafe.Pointer(&work)))
	if restored.Left < work.Work.Left || restored.Top < work.Work.Top || restored.Right > work.Work.Right || restored.Bottom > work.Work.Bottom {
		t.Fatalf("missing monitor left window outside work area: %+v work %+v", restored, work.Work)
	}
}

func TestPetPositionSaveDoesNotBlockCloseDuringResize(t *testing.T) {
	p := newPetPlacement(t.TempDir()).(*nativePetPlacement)
	p.mu.Lock()
	defer p.mu.Unlock()
	done := make(chan struct{})
	go func() { _ = p.save(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("position persistence blocks window close during resize")
	}
}

type testWindowClass struct {
	size, style                        uint32
	procedure                          uintptr
	classExtra, windowExtra            int32
	instance, icon, cursor, background uintptr
	menu, name                         *uint16
	smallIcon                          uintptr
}

// Uses a genuine, hidden Win32 window of the class created by Wails v2.13.
// No mock resize hook or other process/window is involved.
func createHiddenNativeTestWindow(t *testing.T, extendedStyle uintptr) (uintptr, func()) {
	t.Helper()
	defProc := user32.NewProc("DefWindowProcW")
	callback := syscall.NewCallback(func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
		result, _, _ := defProc.Call(hwnd, uintptr(message), wparam, lparam)
		return result
	})
	className, _ := syscall.UTF16PtrFromString("wailsWindow")
	wc := testWindowClass{procedure: callback, name: className}
	wc.size = uint32(unsafe.Sizeof(wc))
	atom, _, err := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		t.Fatalf("register test window: %v", err)
	}
	hwnd, _, err := user32.NewProc("CreateWindowExW").Call(extendedStyle, uintptr(unsafe.Pointer(className)), 0, 0x80000000, 400, 400, 260, 200, 0, 0, 0, 0)
	if hwnd == 0 {
		user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(className)), 0)
		t.Fatalf("create hidden test window: %v", err)
	}
	return hwnd, func() {
		user32.NewProc("DestroyWindow").Call(hwnd)
		user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(className)), 0)
	}
}

func TestResizeHTTPChangesActualWailsClassWindow(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hwnd, cleanup := createHiddenNativeTestWindow(t, 0)
	defer cleanup()
	c := newNativeClient(launchConfig{pet: true}, nil)
	close(c.ready)
	for _, height := range []int{350, 200} {
		body := `{"height":350}`
		if height == 200 {
			body = `{"height":200}`
		}
		req := httptest.NewRequest(http.MethodPost, "/api/desktop/resize-pet", strings.NewReader(body))
		req.Header.Set("X-To-Router", "1")
		rec := httptest.NewRecorder()
		c.serveAPI(rec, req)
		if rec.Code != 200 {
			t.Fatalf("real native resize rejected: %d %s", rec.Code, rec.Body.String())
		}
		var rect nativeRect
		if ok, _, _ := getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect))); ok == 0 {
			t.Fatal("read resized window")
		}
		dpi, _, _ := getDPIForWindow.Call(hwnd)
		wantWidth, wantHeight := (260*int(dpi)+48)/96, (height*int(dpi)+48)/96
		if int(rect.Right-rect.Left) != wantWidth || int(rect.Bottom-rect.Top) != wantHeight {
			t.Fatalf("HTTP success without actual resize: rect=%+v want=%dx%d", rect, wantWidth, wantHeight)
		}
	}
}

func TestPetToolWindowStylesOnActualNativeWindow(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const appWindow, toolWindow, topmost, noRedirection = 0x40000, 0x80, 0x8, 0x200000
	hwnd, cleanup := createHiddenNativeTestWindow(t, appWindow|topmost|noRedirection)
	defer cleanup()
	index := ^uintptr(19)
	before, _, _ := getWindowLong.Call(hwnd, index)
	if uint32(before)&appWindow == 0 || uint32(before)&toolWindow != 0 {
		t.Fatalf("invalid normal-window fixture: %x", before)
	}
	gui := newNativeClient(launchConfig{}, nil)
	if err := gui.prepareWindow(); err != nil {
		t.Fatal(err)
	}
	unchanged, _, _ := getWindowLong.Call(hwnd, index)
	if before != unchanged {
		t.Fatal("GUI window styles changed")
	}
	pet := newNativeClient(launchConfig{pet: true}, nil)
	for i := 0; i < 2; i++ {
		if err := pet.prepareWindow(); err != nil {
			t.Fatal(err)
		}
		after, _, _ := getWindowLong.Call(hwnd, index)
		if uint32(after)&appWindow != 0 || uint32(after)&toolWindow == 0 {
			t.Fatalf("pet remains eligible for taskbar/Alt+Tab: %x", after)
		}
		if uint32(after)&(topmost|noRedirection) != (topmost | noRedirection) {
			t.Fatalf("lost topmost/translucency styles: %x", after)
		}
		if uint32(after)&^(appWindow|toolWindow) != uint32(before)&^(appWindow|toolWindow) {
			t.Fatalf("unrelated styles changed: before=%x after=%x", before, after)
		}
	}
}
