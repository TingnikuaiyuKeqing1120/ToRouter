//go:build windows

package desktopui

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	enumWindows              = user32.NewProc("EnumWindows")
	getWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	getClassName             = user32.NewProc("GetClassNameW")
	getWindowRect            = user32.NewProc("GetWindowRect")
	monitorFromRect          = user32.NewProc("MonitorFromRect")
	monitorFromWindow        = user32.NewProc("MonitorFromWindow")
	getMonitorInfo           = user32.NewProc("GetMonitorInfoW")
	getDPIForWindow          = user32.NewProc("GetDpiForWindow")
	setWindowPos             = user32.NewProc("SetWindowPos")
	showWindowAsync          = user32.NewProc("ShowWindowAsync")
	isIconic                 = user32.NewProc("IsIconic")
	setForegroundWindow      = user32.NewProc("SetForegroundWindow")
	getWindowLong            = user32.NewProc("GetWindowLongW")
	setWindowLong            = user32.NewProc("SetWindowLongW")
	findOwnWindowCallback    = syscall.NewCallback(func(hwnd uintptr, found *uintptr) uintptr {
		var pid uint32
		getWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != uint32(os.Getpid()) {
			return 1
		}
		var class [128]uint16
		getClassName.Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
		if syscall.UTF16ToString(class[:]) != nativeWindowClass {
			return 1
		}
		*found = hwnd
		return 0
	})
)

type nativeRect struct{ Left, Top, Right, Bottom int32 }
type monitorInfo struct {
	Size          uint32
	Monitor, Work nativeRect
	Flags         uint32
}

func (r nativeRect) rect() windowRect {
	return windowRect{int(r.Left), int(r.Top), int(r.Right), int(r.Bottom)}
}

func ownWindow() uintptr {
	var hwnd uintptr
	enumWindows.Call(findOwnWindowCallback, uintptr(unsafe.Pointer(&hwnd)))
	return hwnd
}

func focusPetWindow() {
	hwnd := ownWindow()
	if hwnd == 0 {
		return
	}
	if minimized, _, _ := isIconic.Call(hwnd); minimized != 0 {
		showWindowAsync.Call(hwnd, 9) // SW_RESTORE only for minimised windows.
	}
	setForegroundWindow.Call(hwnd)
}

// WS_EX_TOOLWINDOW excludes the pet from taskbar and Alt+Tab; clear the
// explicit app-window bit installed by Wails. Keep translucency/topmost bits.
func makePetToolWindow() error {
	hwnd := ownWindow()
	if hwnd == 0 {
		return errors.New("native window unavailable")
	}
	index := ^uintptr(19) // GWL_EXSTYLE = -20
	style, _, _ := getWindowLong.Call(hwnd, index)
	wanted := (uint32(style) | 0x00000080) &^ uint32(0x00040000)
	if uint32(style) == wanted {
		return nil
	}
	setWindowLong.Call(hwnd, index, uintptr(wanted))
	actual, _, _ := getWindowLong.Call(hwnd, index)
	if uint32(actual) != wanted {
		return errors.New("cannot set companion tool-window style")
	}
	// Refresh the frame without changing position, size, z-order or focus.
	if ok, _, _ := setWindowPos.Call(hwnd, 0, 0, 0, 0, 0, 0x0001|0x0002|0x0004|0x0010|0x0020); ok == 0 {
		return errors.New("cannot refresh companion tool-window style")
	}
	return nil
}

// Unlike Wails' Windows SetPosition (monitor-relative) and Size (logical),
// these calls consistently use absolute physical desktop coordinates. The
// HTTP worker serialises requests; no UI callback waits for this mutex.
type nativePetPlacement struct {
	mu       sync.Mutex
	anchor   petAnchor
	dataDir  string
	restored bool
	last     petPosition
	saved    bool
}

func newPetPlacement(dataDir string) petPlacement { return &nativePetPlacement{dataDir: dataDir} }
func newPetResizer() func(int) error              { return newPetPlacement("").resize }

func (p *nativePetPlacement) resize(height int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	hwnd := ownWindow()
	if hwnd == 0 {
		return errors.New("native window unavailable")
	}
	var current nativeRect
	if ok, _, _ := getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&current))); ok == 0 {
		return errors.New("cannot read native window bounds")
	}
	monitor, _, _ := monitorFromWindow.Call(hwnd, 2) // MONITOR_DEFAULTTONEAREST
	info := monitorInfo{}
	info.Size = uint32(unsafe.Sizeof(info))
	if ok, _, _ := getMonitorInfo.Call(monitor, uintptr(unsafe.Pointer(&info))); ok == 0 {
		return errors.New("cannot read monitor work area")
	}
	dpi := uintptr(96)
	if getDPIForWindow.Find() == nil {
		if value, _, _ := getDPIForWindow.Call(hwnd); value != 0 {
			dpi = value
		}
	}
	width := (260*int(dpi) + 48) / 96
	physicalHeight := (height*int(dpi) + 48) / 96
	next := p.anchor
	target := next.resize(current.rect(), info.Work.rect(), width, physicalHeight)
	// SWP_NOZORDER | SWP_NOACTIVATE; resizing must not steal focus.
	if ok, _, _ := setWindowPos.Call(hwnd, 0, uintptr(target.left), uintptr(target.top), uintptr(width), uintptr(physicalHeight), 0x0004|0x0010); ok == 0 {
		return errors.New("cannot resize native window")
	}
	p.anchor = next
	return nil
}

func (p *nativePetPlacement) restore() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.restored {
		return nil
	}
	p.restored = true
	value, ok := readPetPosition(p.dataDir)
	if !ok {
		return nil
	}
	hwnd := ownWindow()
	if hwnd == 0 {
		return errors.New("native window unavailable")
	}
	point := nativeRect{int32(value.Left), int32(value.Bottom - 1), int32(value.Left + 1), int32(value.Bottom)}
	monitor, _, _ := monitorFromRect.Call(uintptr(unsafe.Pointer(&point)), 2)
	info := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	if ok, _, _ := getMonitorInfo.Call(monitor, uintptr(unsafe.Pointer(&info))); ok == 0 {
		return errors.New("cannot restore monitor work area")
	}
	// Move onto the selected monitor first so GetDpiForWindow reflects it.
	var current nativeRect
	if ok, _, _ := getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&current))); ok == 0 {
		return errors.New("cannot read native window bounds")
	}
	anchor := petAnchor{valid: true, bottom: value.Bottom, lastTop: int(current.Top)}
	widthBefore, heightBefore := int(current.Right-current.Left), int(current.Bottom-current.Top)
	current.Left = int32(value.Left)
	current.Right = current.Left + int32(widthBefore)
	target := anchor.resize(current.rect(), info.Work.rect(), widthBefore, heightBefore)
	if ok, _, _ := setWindowPos.Call(hwnd, 0, uintptr(target.left), uintptr(target.top), 0, 0, 0x0001|0x0004|0x0010); ok == 0 {
		return errors.New("cannot restore native position")
	}
	dpi := uintptr(96)
	if getDPIForWindow.Find() == nil {
		if d, _, _ := getDPIForWindow.Call(hwnd); d != 0 {
			dpi = d
		}
	}
	width, height := (260*int(dpi)+48)/96, (200*int(dpi)+48)/96
	anchor = petAnchor{valid: true, bottom: value.Bottom, lastTop: target.top}
	target = anchor.resize(target, info.Work.rect(), width, height)
	if ok, _, _ := setWindowPos.Call(hwnd, 0, uintptr(target.left), uintptr(target.top), uintptr(width), uintptr(height), 0x0004|0x0010); ok == 0 {
		return errors.New("cannot restore native bounds")
	}
	// A disconnected monitor's cached point becomes its visible replacement.
	p.anchor = petAnchor{valid: true, bottom: target.bottom, lastTop: target.top}
	return nil
}

func (p *nativePetPlacement) save() error {
	// Window close can execute on the UI thread while another goroutine's
	// SetWindowPos is waiting for that thread. Never block close on its mutex.
	if !p.mu.TryLock() {
		return nil
	}
	defer p.mu.Unlock()
	hwnd := ownWindow()
	if hwnd == 0 {
		return nil
	}
	var rect nativeRect
	if ok, _, _ := getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect))); ok == 0 {
		return nil
	}
	value := p.anchor.position(rect.rect())
	if p.saved && p.last == value {
		return nil
	}
	if err := writePetPosition(p.dataDir, value); err != nil {
		return err
	}
	p.last, p.saved = value, true
	return nil
}
